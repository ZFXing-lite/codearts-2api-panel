package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 未配置余额查询时必须明确拒绝并说明缺什么，不能返回 0 假装成功。
func TestCNCreditRequiresBillingConfig(t *testing.T) {
	srv, ts, db := newCNTestServer(t, "http://127.0.0.1:1", cnTestAccount("cn-1"))
	defer ts.Close()
	tok, err := srv.auth.Login("pass")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/cn/accounts/cn-1/credits", strings.NewReader("{}"))
	req.Header.Set("X-Admin-Token", tok)
	req.Header.Set("X-CSRF-Token", srv.auth.CSRFToken(tok))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "CN_BILLING") {
		t.Fatalf("error should name the missing config: %s", body)
	}
	_ = db
}

// 配置齐全时把余额写进 Metadata，池子接口能汇总。
func TestCNCreditRefreshWritesMetadata(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"签到包","CapacitySize":5000,"CapacityRemain":4321,"CycleCapacitySize":5000,"CycleCapacityRemain":4321}
		]}}}}`))
	}))
	defer up.Close()

	srv, ts, db := newCNTestServer(t, "http://127.0.0.1:1", cnTestAccount("cn-1"))
	defer ts.Close()
	srv.cfg.CNBillingBaseURL = up.URL
	srv.cfg.CNBillingProductCode = "p_test"

	tok, err := srv.auth.Login("pass")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/cn/accounts/cn-1/credits", strings.NewReader("{}"))
	req.Header.Set("X-Admin-Token", tok)
	req.Header.Set("X-CSRF-Token", srv.auth.CSRFToken(tok))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var out struct {
		Remain int64 `json:"credit_remain"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Remain != 4321 {
		t.Fatalf("remain=%d want 4321", out.Remain)
	}
	got, err := db.Get("cn-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["cn_credit_remain"] != "4321" {
		t.Fatalf("metadata=%q", got.Metadata["cn_credit_remain"])
	}
}
