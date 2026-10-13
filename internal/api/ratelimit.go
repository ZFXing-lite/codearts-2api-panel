package api

import (
	"sync"
	"time"
)

// RateLimiter implements a sliding-window RPM limiter per account.
// Inspired by LiteLLM's rpm/tpm limiting and new-api's channel rate limiting.
type RateLimiter struct {
	mu       sync.Mutex
	rpmLimit int
	window   time.Duration
	hits     map[string][]time.Time
}

func newRateLimiter(rpm int) *RateLimiter {
	if rpm <= 0 {
		rpm = 0 // disabled
	}
	return &RateLimiter{
		rpmLimit: rpm,
		window:   time.Minute,
		hits:     map[string][]time.Time{},
	}
}

// Allow checks if the account is within its RPM limit. Returns true if allowed.
// When rpmLimit is 0, always allows (disabled).
func (rl *RateLimiter) Allow(accID string) bool {
	if rl == nil || accID == "" || rl.rpmLimit <= 0 {
		return true
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-rl.window)
	hits := rl.hits[accID]
	// prune old hits
	idx := 0
	for ; idx < len(hits); idx++ {
		if hits[idx].After(cutoff) {
			break
		}
	}
	if idx > 0 {
		hits = hits[idx:]
	}
	if len(hits) >= rl.rpmLimit {
		rl.hits[accID] = hits
		return false
	}
	rl.hits[accID] = append(hits, now)
	return true
}

// KeyGate is a per-API-key sliding-window RPM + in-flight limiter.
type KeyGate struct {
	mu          sync.Mutex
	rpmLimit    int
	maxInFlight int
	window      time.Duration
	hits        map[string][]time.Time
	inflight    map[string]int
}

func newKeyGate(rpm, maxInFlight int) *KeyGate {
	if rpm < 0 {
		rpm = 0
	}
	if maxInFlight < 0 {
		maxInFlight = 0
	}
	return &KeyGate{
		rpmLimit:    rpm,
		maxInFlight: maxInFlight,
		window:      time.Minute,
		hits:        map[string][]time.Time{},
		inflight:    map[string]int{},
	}
}

func keyInflightLimit(maxInFlight, share int) int {
	limit := maxInFlight
	if share > 0 && (limit <= 0 || share < limit) {
		limit = share
	}
	if limit < 0 {
		return 0
	}
	return limit
}

// Acquire reserves one in-flight slot and one RPM token. Caller must Release.
// share is this key's slice of the global chatSem (Track 3b); 0 = no carve.
func (g *KeyGate) Acquire(keyID string, share int) bool {
	if g == nil || keyID == "" {
		return true
	}
	limit := keyInflightLimit(g.maxInFlight, share)
	if limit <= 0 && g.rpmLimit <= 0 {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if limit > 0 && g.inflight[keyID] >= limit {
		return false
	}
	now := time.Now()
	if g.rpmLimit > 0 {
		cutoff := now.Add(-g.window)
		hits := g.hits[keyID]
		idx := 0
		for ; idx < len(hits); idx++ {
			if hits[idx].After(cutoff) {
				break
			}
		}
		if idx > 0 {
			hits = hits[idx:]
		}
		if len(hits) >= g.rpmLimit {
			g.hits[keyID] = hits
			return false
		}
		g.hits[keyID] = append(hits, now)
	}
	g.inflight[keyID]++
	return true
}

func (g *KeyGate) Release(keyID string) {
	if g == nil || keyID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight[keyID] <= 1 {
		delete(g.inflight, keyID)
		return
	}
	g.inflight[keyID]--
}

func (g *KeyGate) InFlight(keyID string) int {
	if g == nil || keyID == "" {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight[keyID]
}

type keyGateSnap struct {
	ID       string `json:"id"`
	InFlight int    `json:"in_flight"`
	RPMUsed  int    `json:"rpm_used"`
	RPMLimit int    `json:"rpm_limit"`
	MaxIn    int    `json:"max_in_flight"`
}

func (g *KeyGate) Snapshot() []keyGateSnap {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-g.window)
	seen := map[string]struct{}{}
	for id := range g.inflight {
		seen[id] = struct{}{}
	}
	for id := range g.hits {
		seen[id] = struct{}{}
	}
	out := make([]keyGateSnap, 0, len(seen))
	for id := range seen {
		used := 0
		for _, h := range g.hits[id] {
			if h.After(cutoff) {
				used++
			}
		}
		out = append(out, keyGateSnap{
			ID:       id,
			InFlight: g.inflight[id],
			RPMUsed:  used,
			RPMLimit: g.rpmLimit,
			MaxIn:    g.maxInFlight,
		})
	}
	return out
}
