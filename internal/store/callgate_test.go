package store

import (
	"testing"
	"time"
)

func TestRecomputeCallGateQuotaExhaustedStillCallable(t *testing.T) {
	a := &Account{
		Credentials: &Credentials{AccessKeyID: "ak", SecretAccessKey: "sk", AuthMode: "iam_permanent"},
		Quota: &QuotaSnapshot{
			PackageStatus: "normal",
			PackageName:   "Free",
			PackageTokens: 5_000_000,
			UsedTokens:    5_000_000,
			RemainTokens:  0,
			FetchedAt:     time.Now(),
		},
	}
	a.RecomputeCallGate(time.Now())
	if a.CallGate != CallGateQuotaExhausted {
		t.Fatalf("got %s want %s", a.CallGate, CallGateQuotaExhausted)
	}
	if !a.Callable(time.Now()) {
		t.Fatal("透支中 should remain callable")
	}
	if a.PickWeight() != PickWeightDepleted {
		t.Fatalf("depleted weight got %d want %d", a.PickWeight(), PickWeightDepleted)
	}
	if CallGateLabelZH(a.CallGate) != "透支中" {
		t.Fatalf("label=%q", CallGateLabelZH(a.CallGate))
	}
}

func TestRecomputeCallGateManualDisable(t *testing.T) {
	a := &Account{
		Disabled:    true,
		Credentials: &Credentials{AccessKeyID: "ak", SecretAccessKey: "sk", AuthMode: "iam_permanent"},
		Quota: &QuotaSnapshot{
			PackageTokens: 100,
			RemainTokens:  50,
			FetchedAt:     time.Now(),
		},
	}
	a.RecomputeCallGate(time.Now())
	if a.CallGate != CallGateDisabled {
		t.Fatalf("got %s", a.CallGate)
	}
	if a.Callable(time.Now()) {
		t.Fatal("disabled must not be callable")
	}
}

func TestRecomputeCallGateReady(t *testing.T) {
	a := &Account{
		Credentials: &Credentials{AccessKeyID: "ak", SecretAccessKey: "sk", AuthMode: "iam_permanent"},
		Quota: &QuotaSnapshot{
			PackageStatus: "normal",
			PackageTokens: 5_000_000,
			UsedTokens:    10,
			RemainTokens:  4_999_990,
			FetchedAt:     time.Now(),
		},
	}
	a.RecomputeCallGate(time.Now())
	if a.CallGate != CallGateReady {
		t.Fatalf("got %s", a.CallGate)
	}
	if a.PickWeight() != PickWeightReady {
		t.Fatalf("ready weight got %d", a.PickWeight())
	}
}

func TestMarkQuotaExhaustedClearsRemain(t *testing.T) {
	a := &Account{
		Credentials: &Credentials{AccessKeyID: "ak", SecretAccessKey: "sk", AuthMode: "iam_permanent"},
		Quota:       &QuotaSnapshot{PackageTokens: 100, UsedTokens: 20, RemainTokens: 80, FetchedAt: time.Now()},
	}
	a.MarkQuotaExhausted("quota_exhausted", "test", time.Now())
	if a.CallGate != CallGateQuotaExhausted || a.Quota.RemainTokens != 0 {
		t.Fatalf("gate=%s remain=%v", a.CallGate, a.Quota.RemainTokens)
	}
	if !a.Callable(time.Now()) {
		t.Fatal("marked 透支中 should stay callable")
	}
}

func TestMarkUpstreamRejectedHardBan(t *testing.T) {
	a := &Account{
		Credentials: &Credentials{AccessKeyID: "ak", SecretAccessKey: "sk", AuthMode: "iam_permanent"},
		Quota:       &QuotaSnapshot{PackageTokens: 100, RemainTokens: 50, FetchedAt: time.Now()},
	}
	a.MarkUpstreamRejected("quota denied by upstream", time.Now())
	if !a.Disabled || a.DisabledReason != "upstream_rejected" {
		t.Fatalf("disabled=%v reason=%s", a.Disabled, a.DisabledReason)
	}
	if a.Callable(time.Now()) {
		t.Fatal("upstream reject must hard-ban")
	}
}
