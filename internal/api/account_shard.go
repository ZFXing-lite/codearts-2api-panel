package api

import (
	"sync"
	"time"
)

// accountShard is a fast-path Ready/Blocked index for picker candidacy.
//
// Motivation:
//   - pickAccountForSessionEx iterates every account in the pool (358 in prod)
//     and, for each one, takes sessionBusyMu + reads circuit.IsOpen. With hot
//     circuits this is O(N) locks per pick.
//   - shard keeps a pre-filtered "known-ready" set. picker consults it first
//     to shrink the working set before per-account lock acquisition.
//
// Authority model:
//   - circuit_breaker.go remains the source of truth for gate decisions.
//   - shard is a synchronized cache updated via CircuitBreaker hooks.
//     If shard misses a mutation the picker's own IsOpen/tryReserve checks
//     still catch it, so a lag in shard is a perf regression, not a bug.
//
// Ownership:
//   - SeedReady is called at boot with every callable account ID.
//   - MarkBlocked/MarkReady flow from CircuitBreaker.onOpen/onClose hooks.
//   - PromoteExpired flushes expired blocks back to ready on a timer.
type accountShard struct {
	mu      sync.RWMutex
	ready   map[string]struct{}
	blocked map[string]time.Time // readyAt; zero = permanent
}

func newAccountShard() *accountShard {
	return &accountShard{
		ready:   make(map[string]struct{}, 512),
		blocked: make(map[string]time.Time, 64),
	}
}

// SeedReady marks the given account IDs as ready without touching blocked
// entries. Safe to call at boot and on account list reload.
func (s *accountShard) SeedReady(ids []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, blk := s.blocked[id]; blk {
			continue
		}
		s.ready[id] = struct{}{}
	}
}

// Reset clears all state. Used only on full pool rebuild.
func (s *accountShard) Reset(ids []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = make(map[string]struct{}, len(ids)+64)
	s.blocked = make(map[string]time.Time, 64)
	for _, id := range ids {
		if id != "" {
			s.ready[id] = struct{}{}
		}
	}
}

// MarkReady moves an account into the ready set (idempotent).
func (s *accountShard) MarkReady(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	delete(s.blocked, id)
	s.ready[id] = struct{}{}
	s.mu.Unlock()
}

// MarkBlocked moves an account out of ready. readyAt zero means permanent
// (until an explicit MarkReady/PromoteExpired ignores it).
func (s *accountShard) MarkBlocked(id string, readyAt time.Time) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	delete(s.ready, id)
	s.blocked[id] = readyAt
	s.mu.Unlock()
}

// IsReady is a fast contains check. Unknown IDs return true (fail-open).
func (s *accountShard) IsReady(id string) bool {
	if s == nil || id == "" {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.ready[id]; ok {
		return true
	}
	if _, blk := s.blocked[id]; blk {
		return false
	}
	return true
}

// SnapshotReady returns a copy of the ready set. Caller owns the slice.
func (s *accountShard) SnapshotReady() map[string]struct{} {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]struct{}, len(s.ready))
	for id := range s.ready {
		out[id] = struct{}{}
	}
	return out
}

// PromoteExpired moves blocked entries whose readyAt has passed back to
// the ready set. Returns how many were promoted. Called from a timer.
func (s *accountShard) PromoteExpired(now time.Time) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, readyAt := range s.blocked {
		if readyAt.IsZero() {
			continue
		}
		if !now.Before(readyAt) {
			delete(s.blocked, id)
			s.ready[id] = struct{}{}
			n++
		}
	}
	return n
}

// Size returns the current ready/blocked counts for dashboard/metrics.
func (s *accountShard) Size() (ready, blocked int) {
	if s == nil {
		return 0, 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ready), len(s.blocked)
}
