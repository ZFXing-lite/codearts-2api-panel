package api

import (
	"context"
	"sync"
	"time"
)

// dispatchLimiter paces outbound Huawei chat dispatches process-wide.
//
// Evidence (2026-09-20): a 110-concurrent single-session burst against prod
// produced 37 "InferHub.ModelArts.81101.429 — Too many requests, the rate
// limit is 50 per second" errors, all within a single 1s window. Every one of
// the 37 hit a DIFFERENT account (37 unique accounts, zero repeats), and the
// sampled accounts were not depleted/overdraft — they were normal ready
// accounts on their first-ever pick. That rules out a per-account cause:
// Huawei's 50/sec ceiling is an aggregate limit shared across the whole
// account pool (they all funnel into the same backend inference capacity),
// not a per-account quota. Spreading load across more accounts (already at
// max: 37/37 unique) cannot avoid it — only pacing the RATE of new dispatches
// can. This sits in front of the one real dispatch call site
// (s.huaweiModel.ChatCompletions in handleHuaweiChat) so it covers first
// attempts and retries, streaming and non-streaming alike.
//
// Token bucket, continuously refilled. Under normal load (well under the
// cap) tokens are always available and wait() returns immediately; it only
// queues during genuine large bursts, trading a bounded queueing delay for
// avoiding a guaranteed Huawei-side rejection + wasted retry round-trip.
type dispatchLimiter struct {
	mu       sync.Mutex
	tokens   float64
	max      float64
	perSec   float64
	lastFill time.Time
}

// newDispatchLimiter returns nil (no-op) when perSec<=0, matching this
// codebase's "0 = disabled" convention (KEY_RPM, RateLimiter, etc).
func newDispatchLimiter(perSec float64) *dispatchLimiter {
	if perSec <= 0 {
		return nil
	}
	return &dispatchLimiter{tokens: perSec, max: perSec, perSec: perSec, lastFill: time.Now()}
}

// SetRPS reconfigures the limiter's ceiling live. Callers coming from the
// hot-reload watcher pass a fresh HUAWEI_DISPATCH_RPS from .env. Non-positive
// values leave the limiter untouched — flipping between disabled and enabled
// mid-flight would need a struct swap on the Server, which isn't wired.
func (d *dispatchLimiter) SetRPS(perSec float64) {
	if d == nil || perSec <= 0 {
		return
	}
	d.mu.Lock()
	// Refill before mutating the rate so the accrued tokens under the OLD rate
	// are credited correctly. Otherwise a rate cut would over-credit and a rate
	// bump would under-credit whatever accrued during this tick.
	now := time.Now()
	elapsed := now.Sub(d.lastFill).Seconds()
	d.lastFill = now
	d.tokens += elapsed * d.perSec
	d.perSec = perSec
	d.max = perSec
	if d.tokens > d.max {
		d.tokens = d.max
	}
	d.mu.Unlock()
}

// wait blocks until a dispatch token is available or ctx is done, and
// reports how long it waited (for optional caller-side logging).
func (d *dispatchLimiter) wait(ctx context.Context) (time.Duration, error) {
	if d == nil {
		return 0, nil
	}
	started := time.Now()
	for {
		d.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(d.lastFill).Seconds()
		d.lastFill = now
		d.tokens += elapsed * d.perSec
		if d.tokens > d.max {
			d.tokens = d.max
		}
		if d.tokens >= 1 {
			d.tokens--
			d.mu.Unlock()
			return time.Since(started), nil
		}
		deficit := 1 - d.tokens
		wait := time.Duration(deficit / d.perSec * float64(time.Second))
		d.mu.Unlock()
		if wait <= 0 {
			wait = time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return time.Since(started), ctx.Err()
		case <-t.C:
			// Loop back and re-check/refill: another goroutine may have taken
			// the token that accrued while we slept, so we may need to wait again.
		}
	}
}
