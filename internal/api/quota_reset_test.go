package api

import (
	"testing"
	"time"

	"go-codearts/internal/store"
)

func quotaSnap(fetched time.Time, used, remain, pkg float64) *store.QuotaSnapshot {
	return &store.QuotaSnapshot{
		FetchedAt:     fetched,
		UsedTokens:    used,
		RemainTokens:  remain,
		PackageTokens: pkg,
	}
}

func TestCarryResetObservationDetectsRollback(t *testing.T) {
	now := time.Now()
	prev := quotaSnap(now.Add(-time.Hour), 4_000_000, 0, 5_000_000)
	next := quotaSnap(now, 120_000, 4_880_000, 5_000_000)
	carryResetObservation(prev, next, now)
	if next.ResetCount != 1 || next.LastResetAt == nil {
		t.Fatalf("rollback not detected: count=%d last=%v", next.ResetCount, next.LastResetAt)
	}
}

func TestCarryResetObservationKeepsHistoryWithoutRollback(t *testing.T) {
	now := time.Now()
	last := now.Add(-72 * time.Hour)
	prev := quotaSnap(now.Add(-time.Hour), 1000, 4_999_000, 5_000_000)
	prev.ResetCount = 2
	prev.LastResetAt = &last
	prev.ResetIntervalHours = 720
	next := quotaSnap(now, 2000, 4_998_000, 5_000_000)
	carryResetObservation(prev, next, now)
	if next.ResetCount != 2 || next.LastResetAt == nil || !next.LastResetAt.Equal(last) {
		t.Fatalf("history lost: %+v", next)
	}
	if next.ResetIntervalHours != 720 {
		t.Fatalf("interval lost: %v", next.ResetIntervalHours)
	}
}

func TestCarryResetObservationIgnoresErrorsAndStale(t *testing.T) {
	now := time.Now()
	prev := quotaSnap(now.Add(-time.Hour), 4_000_000, 0, 5_000_000)
	bad := quotaSnap(now, 0, 0, 0)
	bad.Error = "upstream 500"
	carryResetObservation(prev, bad, now)
	if bad.ResetCount != 0 || bad.LastResetAt != nil {
		t.Fatal("error snapshot must not look like a reset")
	}
	stale := quotaSnap(prev.FetchedAt.Add(-time.Minute), 0, 5_000_000, 5_000_000)
	carryResetObservation(prev, stale, now)
	if stale.ResetCount != 0 {
		t.Fatal("stale snapshot must not count as rollback")
	}
}

func TestCarryResetObservationIntervalFromSecondReset(t *testing.T) {
	now := time.Now()
	prevLast := now.Add(-30 * 24 * time.Hour)
	prev := quotaSnap(now.Add(-time.Hour), 5_100_000, 0, 5_000_000)
	prev.ResetCount = 1
	prev.LastResetAt = &prevLast
	next := quotaSnap(now, 10, 4_999_990, 5_000_000)
	carryResetObservation(prev, next, now)
	if next.ResetCount != 2 {
		t.Fatalf("count=%d want 2", next.ResetCount)
	}
	if got := next.ResetIntervalHours; got < 719 || got > 721 {
		t.Fatalf("interval=%v want ~720h", got)
	}
}
