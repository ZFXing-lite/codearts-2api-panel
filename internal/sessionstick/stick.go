// Package sessionstick binds conversation session IDs to upstream account IDs.
// Same idea as CLIProxyAPI SessionAffinitySelector: keep multi-turn traffic on one
// credential so the provider's prompt/KV cache can hit.
package sessionstick

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
)

type entry struct {
	accountID string
	expiresAt time.Time
	lcpKey    string // "" when no LCP secondary index is registered
}

// Stick maps session → account with TTL and bounded capacity.
// Optional secondary lcpKey → sessionID index provides prefix-based fallback
// when clients rotate their primary session id between turns.
type Stick struct {
	mu         sync.Mutex
	items      map[string]*list.Element
	order      *list.List // front = newest
	ttl        time.Duration
	maxEntries int
	lcpIndex   map[string]string // lcpKey → sessionID (bounded by items)

	hits      atomic.Int64
	misses    atomic.Int64
	binds     atomic.Int64
	evictions atomic.Int64
	lcpHits   atomic.Int64
}

type lruItem struct {
	key   string
	value entry
}

type Stats struct {
	Enabled    bool    `json:"enabled"`
	Entries    int     `json:"entries"`
	Hits       int64   `json:"hits"`
	Misses     int64   `json:"misses"`
	Binds      int64   `json:"binds"`
	Evictions  int64   `json:"evictions"`
	HitRate    float64 `json:"hit_rate"`
	TTLSeconds int64   `json:"ttl_seconds"`
	MaxEntries int     `json:"max_entries"`
	LCPHits    int64   `json:"lcp_hits"`
	LCPEntries int     `json:"lcp_entries"`
}

func New(ttl time.Duration, maxEntries int) *Stick {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if maxEntries <= 0 {
		maxEntries = 8192
	}
	return &Stick{
		items:      make(map[string]*list.Element),
		order:      list.New(),
		ttl:        ttl,
		maxEntries: maxEntries,
		lcpIndex:   make(map[string]string),
	}
}

// Get returns the bound account ID without refreshing TTL (CLIProxyAPI Get semantics).
func (s *Stick) Get(sessionID string) (string, bool) {
	if s == nil || sessionID == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[sessionID]
	if !ok {
		s.misses.Add(1)
		return "", false
	}
	it := el.Value.(*lruItem)
	if time.Now().After(it.value.expiresAt) {
		s.removeElementLocked(el)
		s.misses.Add(1)
		return "", false
	}
	s.hits.Add(1)
	return it.value.accountID, true
}

// GetAndRefresh returns the bound account and extends TTL (used on successful reuse).
func (s *Stick) GetAndRefresh(sessionID string) (string, bool) {
	if s == nil || sessionID == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[sessionID]
	if !ok {
		s.misses.Add(1)
		return "", false
	}
	it := el.Value.(*lruItem)
	if time.Now().After(it.value.expiresAt) {
		s.removeElementLocked(el)
		s.misses.Add(1)
		return "", false
	}
	it.value.expiresAt = time.Now().Add(s.ttl)
	s.order.MoveToFront(el)
	s.hits.Add(1)
	return it.value.accountID, true
}

// Bind attaches sessionID to accountID and refreshes TTL.
func (s *Stick) Bind(sessionID, accountID string) {
	if s == nil || sessionID == "" || accountID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if el, ok := s.items[sessionID]; ok {
		it := el.Value.(*lruItem)
		it.value.accountID = accountID
		it.value.expiresAt = now.Add(s.ttl)
		s.order.MoveToFront(el)
		s.binds.Add(1)
		return
	}
	for s.order.Len() >= s.maxEntries {
		s.evictOldestLocked()
	}
	it := &lruItem{key: sessionID, value: entry{accountID: accountID, expiresAt: now.Add(s.ttl)}}
	el := s.order.PushFront(it)
	s.items[sessionID] = el
	s.binds.Add(1)
}

// Invalidate drops a session binding (e.g. sticky account failed).
func (s *Stick) Invalidate(sessionID string) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[sessionID]; ok {
		s.removeElementLocked(el)
	}
}

// InvalidateAccount drops every session bound to accountID (session-cap drain).
func (s *Stick) InvalidateAccount(accountID string) int {
	if s == nil || accountID == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for el := s.order.Front(); el != nil; {
		next := el.Next()
		it := el.Value.(*lruItem)
		if it.value.accountID == accountID {
			s.removeElementLocked(el)
			n++
		}
		el = next
	}
	return n
}

// Manual Clear is intentionally omitted: bindings expire by TTL only.

// BindWithLCP is Bind plus a secondary index entry pointing lcpKey → sessionID.
// Empty lcpKey degrades to Bind. Multiple sessions can share an lcpKey — the
// last binder wins (front-of-LRU semantics).
func (s *Stick) BindWithLCP(sessionID, accountID, lcpKey string) {
	if s == nil || sessionID == "" || accountID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if el, ok := s.items[sessionID]; ok {
		it := el.Value.(*lruItem)
		if it.value.lcpKey != "" && it.value.lcpKey != lcpKey {
			if sid, ok := s.lcpIndex[it.value.lcpKey]; ok && sid == sessionID {
				delete(s.lcpIndex, it.value.lcpKey)
			}
		}
		it.value.accountID = accountID
		it.value.expiresAt = now.Add(s.ttl)
		it.value.lcpKey = lcpKey
		s.order.MoveToFront(el)
		s.binds.Add(1)
		if lcpKey != "" {
			s.lcpIndex[lcpKey] = sessionID
		}
		return
	}
	for s.order.Len() >= s.maxEntries {
		s.evictOldestLocked()
	}
	it := &lruItem{key: sessionID, value: entry{accountID: accountID, expiresAt: now.Add(s.ttl), lcpKey: lcpKey}}
	el := s.order.PushFront(it)
	s.items[sessionID] = el
	s.binds.Add(1)
	if lcpKey != "" {
		s.lcpIndex[lcpKey] = sessionID
	}
}

// LookupLCP resolves a canonical-turn key back to a live binding. Returns the
// bound sessionID and accountID with TTL refreshed. Empty lcpKey misses.
func (s *Stick) LookupLCP(lcpKey string) (sessionID, accountID string, ok bool) {
	if s == nil || lcpKey == "" {
		return "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sid, ok := s.lcpIndex[lcpKey]
	if !ok {
		return "", "", false
	}
	el, exists := s.items[sid]
	if !exists {
		delete(s.lcpIndex, lcpKey)
		return "", "", false
	}
	it := el.Value.(*lruItem)
	if time.Now().After(it.value.expiresAt) {
		s.removeElementLocked(el)
		return "", "", false
	}
	it.value.expiresAt = time.Now().Add(s.ttl)
	s.order.MoveToFront(el)
	s.hits.Add(1)
	s.lcpHits.Add(1)
	return sid, it.value.accountID, true
}

func (s *Stick) Snapshot() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	n := s.order.Len()
	lcpN := len(s.lcpIndex)
	s.mu.Unlock()
	hits := s.hits.Load()
	misses := s.misses.Load()
	total := hits + misses
	rate := 0.0
	if total > 0 {
		rate = float64(hits) / float64(total)
	}
	return Stats{
		Enabled:    true,
		Entries:    n,
		Hits:       hits,
		Misses:     misses,
		Binds:      s.binds.Load(),
		Evictions:  s.evictions.Load(),
		HitRate:    rate,
		TTLSeconds: int64(s.ttl.Seconds()),
		MaxEntries: s.maxEntries,
		LCPHits:    s.lcpHits.Load(),
		LCPEntries: lcpN,
	}
}

func (s *Stick) removeElementLocked(el *list.Element) {
	it := el.Value.(*lruItem)
	delete(s.items, it.key)
	if it.value.lcpKey != "" {
		// Only drop the reverse mapping when it still points at THIS session.
		// Rebinding the same lcpKey to a newer sessionID first overwrites the
		// index entry, so we mustn't clobber that when evicting the older one.
		if sid, ok := s.lcpIndex[it.value.lcpKey]; ok && sid == it.key {
			delete(s.lcpIndex, it.value.lcpKey)
		}
	}
	s.order.Remove(el)
	s.evictions.Add(1)
}

func (s *Stick) evictOldestLocked() {
	el := s.order.Back()
	if el == nil {
		return
	}
	s.removeElementLocked(el)
}

// Binding is the exported view of one live session-account pair.
type Binding struct {
	SessionID string    `json:"session_id"`
	AccountID string    `json:"account_id"`
	ExpiresAt time.Time `json:"expires_at"`
	HasLCPKey bool      `json:"has_lcp_key"`
}

// SnapshotBindings returns every live binding, oldest-eviction first.
// Expired entries are lazily swept during the walk. Cap `limit` guards
// against dumping 8k entries into one HTTP body — 0 means "all".
func (s *Stick) SnapshotBindings(limit int) []Binding {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if limit <= 0 || limit > s.order.Len() {
		limit = s.order.Len()
	}
	out := make([]Binding, 0, limit)
	for el := s.order.Front(); el != nil && len(out) < limit; {
		next := el.Next()
		it := el.Value.(*lruItem)
		if now.After(it.value.expiresAt) {
			s.removeElementLocked(el)
			el = next
			continue
		}
		out = append(out, Binding{
			SessionID: it.key,
			AccountID: it.value.accountID,
			ExpiresAt: it.value.expiresAt,
			HasLCPKey: it.value.lcpKey != "",
		})
		el = next
	}
	return out
}

// CountByAccount returns live session-binding counts keyed by account ID.
func (s *Stick) CountByAccount() map[string]int {
	out := map[string]int{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for el := s.order.Front(); el != nil; {
		next := el.Next()
		it := el.Value.(*lruItem)
		if now.After(it.value.expiresAt) {
			s.removeElementLocked(el)
			el = next
			continue
		}
		out[it.value.accountID]++
		el = next
	}
	return out
}
