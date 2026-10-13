package api

// Behavioral concurrency + long-task simulations for the fan-out spill fix and
// sessionSoftCap=1. The race detector needs cgo, which this build host lacks, so
// these exercise the real scheduler code paths (tryReserveAccount under
// sessionBusyMu, pickAccountForSession, rotation/drain) with many goroutines and
// many sequential turns to surface deadlocks, over-cap double-hands, stalls, and
// in-flight leaks without -race. Run -race against these in the Linux docker build.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"go-codearts/internal/sessionstick"
	"go-codearts/internal/store"
)

// TestSpillConcurrentFanoutDistinct models Claude Code firing N parallel
// tool-calls under ONE X-Claude-Code-Session-Id (all bound to a0). With N ready
// accounts, the bound account serves one caller and the fan-out SPILLS the rest
// across the idle accounts — every caller served, no account handed to two
// callers (the cap=1 invariant), no head-of-line stall.
func TestSpillConcurrentFanoutDistinct(t *testing.T) {
	const M = 8 // ready accounts in the pool
	const N = 8 // concurrent picks from one session
	s := newPickServer()
	s.sessionStick = sessionstick.New(time.Minute, 64)
	pool := make([]*store.Account, M)
	for i := range pool {
		pool[i] = &store.Account{ID: fmt.Sprintf("a%d", i), CallGate: store.CallGateReady}
	}
	s.sessionStick.Bind("sess", "a0") // fan-out shares the session → all try a0 first

	results := make([]*store.Account, N)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // release all goroutines at once to maximize contention
			acc, _ := s.pickAccountForSession(pool, "sess", map[string]bool{}, false)
			results[idx] = acc
		}(i)
	}
	close(start)
	wg.Wait()

	seen := map[string]int{}
	served := 0
	for _, acc := range results {
		if acc == nil {
			continue
		}
		served++
		seen[acc.ID]++
	}
	for id, c := range seen {
		if c > 1 {
			t.Fatalf("account %s handed to %d concurrent callers — over cap=1", id, c)
		}
	}
	if served != N {
		t.Fatalf("want all %d fan-out callers served via spill, got %d (seen=%v)", N, served, seen)
	}

	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	total := 0
	for id, n := range s.sessionInFlight {
		if n != 1 {
			t.Fatalf("account %s inflight=%d, want exactly 1", id, n)
		}
		total += n
	}
	if total != N {
		t.Fatalf("reserved slots=%d, want %d (one per served caller)", total, N)
	}
}

// TestSpillConcurrentOversubscribedWaits proves the spill still RESPECTS the cap:
// with more concurrent callers than ready slots, exactly M are served (distinct)
// and the surplus WAITS (nil) rather than double-committing an account or dumping
// onto a throttled one.
func TestSpillConcurrentOversubscribedWaits(t *testing.T) {
	const M = 4  // ready slots
	const N = 10 // concurrent callers
	s := newPickServer()
	s.sessionStick = sessionstick.New(time.Minute, 64)
	pool := make([]*store.Account, M)
	for i := range pool {
		pool[i] = &store.Account{ID: fmt.Sprintf("a%d", i), CallGate: store.CallGateReady}
	}
	s.sessionStick.Bind("sess", "a0")

	results := make([]*store.Account, N)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			acc, _ := s.pickAccountForSession(pool, "sess", map[string]bool{}, false)
			results[idx] = acc
		}(i)
	}
	close(start)
	wg.Wait()

	seen := map[string]int{}
	served := 0
	for _, acc := range results {
		if acc == nil {
			continue
		}
		served++
		seen[acc.ID]++
	}
	for id, c := range seen {
		if c > 1 {
			t.Fatalf("account %s over cap=1: used by %d callers", id, c)
		}
	}
	if served != M {
		t.Fatalf("only %d ready slots exist; want served=%d with the surplus waiting, got %d (seen=%v)", M, M, served, seen)
	}
}

// TestLongTaskManyTurnsStable simulates a long coding session: 300 sequential
// turns of pick → bind → successful SSE → rotate → release on a single session
// over a modest ready pool. The stability guarantees under test: every turn is
// served (no stall) and no in-flight slot leaks after the run. Rotation/spread
// are asserted loosely here (exact cadence is pinned by TestRotateReadyKeepsStickyLonger).
func TestLongTaskManyTurnsStable(t *testing.T) {
	const M = 6
	const turns = 300
	s := newPickServer()
	s.cfg.RotateByGate = true
	s.cfg.RotateAfterReady = 5 // production ROTATE_AFTER_READY
	s.sessionStick = sessionstick.New(time.Hour, 64)
	pool := make([]*store.Account, M)
	for i := range pool {
		pool[i] = &store.Account{ID: fmt.Sprintf("a%d", i), CallGate: store.CallGateReady}
	}

	usage := map[string]int{}
	rotations := 0
	for turn := 0; turn < turns; turn++ {
		// Simulate seconds/minutes of real inter-turn latency: any <=12s
		// post-rotation drain from an earlier turn has long expired before the
		// next user turn, so the frozen test clock does not falsely starve the
		// pool. Drain-wait behavior itself is covered by
		// TestPickStickyDrainWaitsInsteadOfFailover.
		s.sessionBusyMu.Lock()
		s.sessionDrain = map[string]time.Time{}
		s.sessionBusyMu.Unlock()

		acc, _ := s.pickAccountForSession(pool, "sess", map[string]bool{}, false)
		if acc == nil {
			t.Fatalf("turn %d: no account served from a pool of %d ready accounts on a single session", turn, M)
		}
		usage[acc.ID]++
		s.bindSessionAccount("sess", acc) // gateway pins the picked account
		s.rotateStickyAfterTurns("sess", acc)
		s.endAccountInFlight(acc.ID)
		if _, ok := s.sessionStick.Get("sess"); !ok {
			rotations++ // rotation invalidated the binding this turn
		}
	}

	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	for id, n := range s.sessionInFlight {
		if n != 0 {
			t.Fatalf("in-flight leak: account %s = %d after %d turns", id, n, turns)
		}
	}
	if rotations == 0 {
		t.Fatalf("rotation never fired across %d turns — sticky pinned forever", turns)
	}
	if len(usage) < 2 {
		t.Fatalf("all %d turns pinned to a single account (%v) — sticky starvation regression", turns, usage)
	}
}
