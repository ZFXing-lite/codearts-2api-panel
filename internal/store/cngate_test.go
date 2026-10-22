package store

import (
	"testing"
	"time"
)

// CN 渠道只用 OAuth access_token，不需要 IAM AK/SK。
func TestCNCallGateUsesAccessToken(t *testing.T) {
	now := time.Now()
	base := func() *Account {
		return &Account{ID: "cn-1", Channel: ChannelCN, Email: "u@cn"}
	}

	// 1) 只有 access_token → ready
	a := base()
	a.AccessToken = "tok"
	a.RecomputeCallGate(now)
	if a.CallGate != CallGateReady {
		t.Fatalf("cn with access_token: want ready, got %s", a.CallGate)
	}

	// 2) 无任何 token → no_cred
	a = base()
	a.RecomputeCallGate(now)
	if a.CallGate != CallGateNoCred {
		t.Fatalf("cn without token: want no_cred, got %s", a.CallGate)
	}

	// 3) token 过期但有 refresh_token → 仍 ready（chat 侧 inline 续期）
	a = base()
	a.AccessToken = "tok"
	a.RefreshToken = "rt"
	a.TokenExpires = now.Add(-time.Hour)
	a.RecomputeCallGate(now)
	if a.CallGate != CallGateReady {
		t.Fatalf("cn expired+refresh: want ready, got %s", a.CallGate)
	}

	// 4) token 过期且无 refresh_token → token_expired
	a = base()
	a.AccessToken = "tok"
	a.TokenExpires = now.Add(-time.Hour)
	a.RecomputeCallGate(now)
	if a.CallGate != CallGateTokenExpired {
		t.Fatalf("cn expired w/o refresh: want token_expired, got %s", a.CallGate)
	}

	// 5) 国际渠道仍要求 AK/SK：无 credentials → no_cred
	a = &Account{ID: "intl-1", Channel: ChannelIntl, Email: "u@intl"}
	a.AccessToken = "tok"
	a.RecomputeCallGate(now)
	if a.CallGate != CallGateNoCred {
		t.Fatalf("intl without aksk: want no_cred, got %s", a.CallGate)
	}

	// 6) 手动禁用优先于一切
	a = base()
	a.AccessToken = "tok"
	a.Disabled = true
	a.RecomputeCallGate(now)
	if a.CallGate != CallGateDisabled {
		t.Fatalf("disabled: want disabled, got %s", a.CallGate)
	}
}
