package huawei

import (
	"encoding/json"
	"testing"
	"time"
)

func mustInfo(t *testing.T, raw string) *SubscriptionInfo {
	t.Helper()
	info := &SubscriptionInfo{}
	if err := json.Unmarshal([]byte(raw), info); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	info.Raw = []byte(raw)
	return info
}

// Upstream sometimes ships an explicit reset field — it must win over estimates.
func TestQuotaResetPrefersUpstreamField(t *testing.T) {
	raw := `{
	  "package":{"status":"normal","package_name":"CodeArts trial","expire_time":"2026-11-30T23:59:59+08:00","effective_time":"2026-09-01T00:00:00+08:00"},
	  "metrics":[{"name":"usageTokenChatMessages","usage_token_num":100,"package_token_amount":1000}],
	  "show":{"quotaResetTime":"2026-10-02T00:00:00+08:00"}
	}`
	s := mustInfo(t, raw).Summary()
	if s.ResetSource != "upstream" {
		t.Fatalf("reset source=%q note=%q (want upstream)", s.ResetSource, s.ResetNote)
	}
	want := time.Date(2026, 10, 2, 0, 0, 0, 0, quotaResetZone)
	if !s.NextResetAt.Equal(want) {
		t.Fatalf("next reset=%v want %v", s.NextResetAt, want)
	}
}

// Daily quotas: no upstream field -> estimate next 00:00 UTC+8.
func TestQuotaResetDailyEstimate(t *testing.T) {
	raw := `{
	  "package":{"status":"normal","package_name":"CodeArts daily pack","expire_time":"","effective_time":""},
	  "metrics":[{"name":"dailyTokenQuota","usage_token_num":10,"package_token_amount":100}]
	}`
	s := mustInfo(t, raw).Summary()
	if s.Period != "daily" {
		t.Fatalf("period=%q evidence=%q (want daily)", s.Period, s.PeriodEvidence)
	}
	if s.ResetSource != "estimated" {
		t.Fatalf("reset source=%q (want estimated)", s.ResetSource)
	}
	now := time.Now().In(quotaResetZone)
	want := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, quotaResetZone)
	if !want.After(time.Now()) {
		want = want.AddDate(0, 0, 1)
	}
	if !s.NextResetAt.Equal(want) {
		t.Fatalf("next reset=%v want %v", s.NextResetAt, want)
	}
}

// Package-style quota without reset fields falls back to upstream expire_time.
func TestQuotaResetFallsBackToExpireTime(t *testing.T) {
	expire := time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339)
	raw := `{
	  "package":{"status":"normal","package_name":"CodeArts 套餐","expire_time":"` + expire + `","effective_time":""},
	  "metrics":[{"name":"usageTokenChatMessages","usage_token_num":10,"package_token_amount":100}]
	}`
	s := mustInfo(t, raw).Summary()
	if s.ResetSource != "expire" {
		t.Fatalf("reset source=%q note=%q (want expire)", s.ResetSource, s.ResetNote)
	}
	if s.NextResetAt.IsZero() {
		t.Fatal("next reset must fall back to the expire time")
	}
}

// Real CodeArts trial payload: the window is start_date ~ end_date at top level.
func TestQuotaResetUsesUpstreamWindow(t *testing.T) {
	raw := `{
	  "metrics":[{"name":"usageTokenChatMessages","usage_token_num":4509495,"package_token_amount":5000000}],
	  "start_date":"2026-09-11",
	  "end_date":"2026-10-12",
	  "package":{"spec_code":"codearts.agent.trial","status":"normal","package_name_en":"Free","package_name_cn":"体验版","expire_time":"","effective_time":""},
	  "show":{"metrics":true,"package":true}
	}`
	s := mustInfo(t, raw).Summary()
	if s.StartDate != "2026-09-11" || s.EndDate != "2026-10-12" {
		t.Fatalf("window not captured: %q ~ %q", s.StartDate, s.EndDate)
	}
	if s.EffectiveTime != "2026-09-11" || s.ExpireTime != "2026-10-12" {
		t.Fatalf("window not mirrored: %q ~ %q", s.EffectiveTime, s.ExpireTime)
	}
	if s.ResetSource != "expire" {
		t.Fatalf("reset source=%q note=%q (want expire)", s.ResetSource, s.ResetNote)
	}
	if s.NextResetAt.IsZero() || s.NextResetAt.UTC().Format("2006-01-02") != "2026-10-12" {
		t.Fatalf("next reset=%v want 2026-10-12 end of day", s.NextResetAt.UTC())
	}
	if s.PackageName != "体验版" {
		t.Fatalf("package name=%q want 体验版", s.PackageName)
	}
}
