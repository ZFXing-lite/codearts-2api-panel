package api

import (
	"time"

	"go-codearts/internal/store"
)

// Reset timing is owned by upstream. CodeArts free/trial packages ship no
// reset/expiry fields today, so instead of inventing a schedule we observe
// counter rollbacks on every quota refresh and remember when one happened.
// The panel then shows "上次重置 / 观测周期" (or an explicit "上游未下发").
const quotaResetTokenSlack = 1000 // tolerate small counter jitter

// carryResetObservation copies the observed reset history onto the refreshed
// snapshot and records a new reset when upstream counters rolled back.
func carryResetObservation(prev, next *store.QuotaSnapshot, now time.Time) {
	if next == nil || prev == nil {
		return
	}
	next.LastResetAt = prev.LastResetAt
	next.ResetCount = prev.ResetCount
	next.ResetIntervalHours = prev.ResetIntervalHours
	if !quotaRolledBack(prev, next) {
		return
	}
	next.ResetCount = prev.ResetCount + 1
	if prev.LastResetAt != nil && now.After(*prev.LastResetAt) {
		if h := now.Sub(*prev.LastResetAt).Hours(); h > 0 {
			next.ResetIntervalHours = h
		}
	}
	t := now
	next.LastResetAt = &t
}

// quotaRolledBack reports whether upstream counters moved backwards between two
// successful snapshots (new window / refilled budget / bigger package).
func quotaRolledBack(prev, next *store.QuotaSnapshot) bool {
	if prev == nil || next == nil || prev.Error != "" || next.Error != "" {
		return false
	}
	if prev.FetchedAt.IsZero() || !next.FetchedAt.After(prev.FetchedAt) {
		return false
	}
	switch {
	case next.UsedTokens+quotaResetTokenSlack < prev.UsedTokens:
		return true
	case prev.RemainTokens <= 0 && next.RemainTokens > prev.RemainTokens+quotaResetTokenSlack:
		return true
	case prev.PackageTokens > 0 && next.PackageTokens > prev.PackageTokens+quotaResetTokenSlack:
		return true
	}
	return false
}
