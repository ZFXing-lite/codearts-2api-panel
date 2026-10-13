package api

import (
	"context"
	"testing"
	"time"
)

func TestDispatchLimiterDisabledWhenZero(t *testing.T) {
	d := newDispatchLimiter(0)
	if d != nil {
		t.Fatalf("expected nil limiter for perSec<=0, got %+v", d)
	}
	waited, err := d.wait(context.Background())
	if err != nil || waited != 0 {
		t.Fatalf("nil limiter wait() should be an immediate no-op, got waited=%v err=%v", waited, err)
	}
}

func TestDispatchLimiterAllowsInitialBurstUpToCapacity(t *testing.T) {
	d := newDispatchLimiter(50)
	ctx := context.Background()
	// The bucket starts full (tokens=max=perSec), so the first `perSec` calls
	// must be admitted immediately without any queueing.
	for i := 0; i < 50; i++ {
		waited, err := d.wait(ctx)
		if err != nil {
			t.Fatalf("call %d: unexpected error %v", i, err)
		}
		if waited > 20*time.Millisecond {
			t.Fatalf("call %d: expected near-instant admission from initial burst capacity, waited %v", i, waited)
		}
	}
}

func TestDispatchLimiterThrottlesBeyondCapacity(t *testing.T) {
	// Small perSec keeps the test fast while still exercising real queueing.
	d := newDispatchLimiter(10)
	ctx := context.Background()

	// Drain the initial full bucket (10 tokens) immediately.
	for i := 0; i < 10; i++ {
		if _, err := d.wait(ctx); err != nil {
			t.Fatalf("drain call %d: unexpected error %v", i, err)
		}
	}

	// The 11th call must now wait for a refill: at 10/sec, one token takes
	// ~100ms to accrue.
	start := time.Now()
	waited, err := d.wait(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if waited < 50*time.Millisecond {
		t.Fatalf("expected a real wait (~100ms) once the burst is exhausted, got %v", waited)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("wall-clock elapsed should reflect the wait, got %v", elapsed)
	}
}

func TestDispatchLimiterRespectsContextCancellation(t *testing.T) {
	d := newDispatchLimiter(1)
	ctx := context.Background()
	// Drain the single initial token.
	if _, err := d.wait(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.wait(cctx)
	if err == nil {
		t.Fatalf("expected context deadline error while waiting for a token at 1/sec, got nil")
	}
}

func TestDispatchLimiterConcurrentCallersEachGetExactlyOneToken(t *testing.T) {
	d := newDispatchLimiter(20)
	ctx := context.Background()
	n := 20
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := d.wait(ctx)
			done <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent call %d failed: %v", i, err)
		}
	}
	d.mu.Lock()
	tokens := d.tokens
	d.mu.Unlock()
	if tokens > 1 {
		t.Fatalf("expected bucket to be ~drained after %d concurrent callers against a %d-capacity bucket, tokens=%v", n, n, tokens)
	}
}
