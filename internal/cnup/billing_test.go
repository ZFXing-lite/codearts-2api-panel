package cnup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 余额求和口径：套餐有周期额度时按周期剩余算，否则按总剩余算；负值夹 0。
func TestFetchCreditBalanceAggregates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != defaultMeterPath {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("request not signed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"签到包","CapacitySize":2000,"CapacityRemain":1200,"CycleCapacitySize":2000,"CycleCapacityRemain":900},
			{"PackageName":"体验包","CapacitySize":1000,"CapacityRemain":300,"CycleCapacitySize":0,"CycleCapacityRemain":0},
			{"PackageName":"透支包","CapacitySize":100,"CapacityRemain":-50,"CycleCapacitySize":0,"CycleCapacityRemain":0}
		]}}}}`))
	}))
	defer srv.Close()

	c := New(5e9)
	cfg := BillingConfig{BaseURL: srv.URL, ProductCode: "p_test"}
	bal, err := c.FetchCreditBalance(context.Background(), cfg, SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	// 900（周期优先） + 300（无周期用总量） + 0（负值夹零）
	if bal.Remain != 1200 {
		t.Fatalf("remain=%d want 1200", bal.Remain)
	}
	if len(bal.Packages) != 3 {
		t.Fatalf("packages=%d", len(bal.Packages))
	}
}

// 未配置时必须明确报错，而不是返回 0 假装"余额为 0"。
func TestFetchCreditBalanceNotConfigured(t *testing.T) {
	c := New(5e9)
	_, err := c.FetchCreditBalance(context.Background(), BillingConfig{}, SignCredential{})
	if !errors.Is(err, ErrBillingNotConfigured) {
		t.Fatalf("err=%v want ErrBillingNotConfigured", err)
	}
}

// 上游业务码非 0 要带回消息，便于区分「配置错」与「账号无额度」。
func TestFetchCreditBalanceBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":401,"message":"product code invalid"}`))
	}))
	defer srv.Close()
	c := New(5e9)
	_, err := c.FetchCreditBalance(context.Background(),
		BillingConfig{BaseURL: srv.URL, ProductCode: "p_bad"}, SignCredential{AccessKeyID: "a", SecretAccessKey: "b"})
	if err == nil || !contains(err.Error(), "product code invalid") {
		t.Fatalf("err=%v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
