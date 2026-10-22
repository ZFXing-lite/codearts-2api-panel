package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"go-codearts/internal/auth"
	"go-codearts/internal/config"
	"go-codearts/internal/store"
)

func newCNLoginServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	authMgr, err := auth.New("admin", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s := New(config.Config{
		CNEnabled: true, DataDir: dir, CNAuthDir: dir + "/auths", Port: ":29137",
	}, db, authMgr, nil)
	return s, db
}

func TestPortHelpers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{":29137", 29137}, {"0.0.0.0:8080", 8080}, {"29137", 29137}, {"", 0}, {"abc", 0},
	} {
		if got := portFromListen(tc.in); got != tc.want {
			t.Errorf("portFromListen(%q)=%d want %d", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"https://panel.example.com:443/cn", 443},
		{"https://panel.example.com", 0},
		{"example.com:8443", 8443},
		{"", 0},
	} {
		if got := portOfCallbackHost(tc.in); got != tc.want {
			t.Errorf("portOfCallbackHost(%q)=%d want %d", tc.in, got, tc.want)
		}
	}
	u := rewriteAuthURLPort("https://p/authorize?port=1&x=2", 9999)
	if !strings.Contains(u, "port=9999") || strings.Contains(u, "port=1&") {
		t.Errorf("rewriteAuthURLPort=%s", u)
	}
}

// start 必须给出可用的授权链接，并登记会话。
func TestCNLoginStart(t *testing.T) {
	s, _ := newCNLoginServer(t)
	body := strings.NewReader(`{"port":39217}`)
	req := httptest.NewRequest(http.MethodPost, "/api/cn/login/start", body)
	w := httptest.NewRecorder()
	s.handleCNLoginStart(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var d struct {
		SessionID string `json:"session_id"`
		AuthURL   string `json:"auth_url"`
		Port      int    `json:"port"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.SessionID == "" || d.AuthURL == "" {
		t.Fatalf("empty: %+v", d)
	}
	// 面板上报的端口优先（容器端口映射场景）
	if d.Port != 39217 {
		t.Fatalf("port=%d want 39217", d.Port)
	}
	for _, want := range []string{"client_id=", "ticket_id=", "code_challenge=", "port=39217"} {
		if !strings.Contains(d.AuthURL, want) {
			t.Errorf("auth_url missing %q: %s", want, d.AuthURL)
		}
	}
	u, _ := url.Parse(d.AuthURL)
	if u.Query().Get("ticket_id") == "" || u.Query().Get("code_challenge_method") != "SHA-256" {
		t.Errorf("bad authorize query: %s", d.AuthURL)
	}
	// 会话可查，未回环回调前只能是 pending
	req2 := httptest.NewRequest(http.MethodGet, "/api/cn/login/status?id="+d.SessionID, nil)
	w2 := httptest.NewRecorder()
	s.handleCNLoginStatus(w2, req2)
	var st struct {
		Status string `json:"status"`
	}
	json.Unmarshal(w2.Body.Bytes(), &st)
	if st.Status != "pending" {
		t.Fatalf("status=%q want pending", st.Status)
	}
}

// 粘贴回调：校验回环 host、端口一致，并只接受 portal 下发的 secret。
func TestCNLoginPasteCallback(t *testing.T) {
	s, _ := newCNLoginServer(t)
	w := httptest.NewRecorder()
	s.handleCNLoginStart(w, httptest.NewRequest(http.MethodPost, "/api/cn/login/start",
		strings.NewReader(`{"port":8910}`)))
	var start struct {
		SessionID string `json:"session_id"`
		AuthURL   string `json:"auth_url"`
	}
	json.Unmarshal(w.Body.Bytes(), &start)
	au, _ := url.Parse(start.AuthURL)
	ticket := au.Query().Get("ticket_id")
	sess := s.cnLogins.get(start.SessionID)
	if sess == nil {
		t.Fatal("session not stored")
	}

	post := func(cb string) (int, map[string]any) {
		// 会话 ID 走 query（面板同款），body 只放回调地址
		body, _ := json.Marshal(map[string]string{"callback_url": cb})
		rec := httptest.NewRecorder()
		s.handleCNLoginCallbackPaste(rec, httptest.NewRequest(http.MethodPost,
			"/api/cn/login/callback?id="+start.SessionID, strings.NewReader(string(body))))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// 非回环地址直接拒绝
	if code, _ := post("http://evil.example.com/oauth/callback?code=x"); code != http.StatusBadRequest {
		t.Fatalf("non-loopback accepted: %d", code)
	}
	// 端口不一致拒绝
	if code, _ := post("http://127.0.0.1:9999/oauth/callback?code=x"); code != http.StatusBadRequest {
		t.Fatalf("port mismatch accepted: %d", code)
	}
	// secret 与 redirect 不匹配当前 ticket 时拒绝
	bad := "http://127.0.0.1:8910/oauth/callback?secret=s1&redirect=" +
		url.QueryEscape("https://codearts.huaweicloud.com/portal/x?ticket_id=other")
	if code, _ := post(bad); code != http.StatusBadRequest {
		t.Fatalf("mismatched ticket accepted: %d", code)
	}
	// 合法 secret + redirect：接收并开启 ticket 轮询通道
	good := "http://127.0.0.1:8910/oauth/callback?secret=s2&redirect=" +
		url.QueryEscape("https://codearts.huaweicloud.com/portal/x?ticket_id="+ticket)
	code, out := post(good)
	if code != http.StatusOK {
		t.Fatalf("valid callback rejected: %d %+v", code, out)
	}
	if out["status"] != "continue" {
		t.Fatalf("status=%v want continue", out["status"])
	}
	if secret, fallback := sess.pollSecret(); !fallback || secret != "s2" {
		t.Fatalf("fallback=%v secret=%q", fallback, secret)
	}
}

// 回环回调路由：无匹配会话时退回国际版提示页，不 500。
func TestCNOAuthCallbackFallbackPage(t *testing.T) {
	s, _ := newCNLoginServer(t)
	rec := httptest.NewRecorder()
	s.handleCNOAuthCallback(rec, httptest.NewRequest(http.MethodGet, "/oauth/callback", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Errorf("placeholder page unexpected: %s", rec.Body.String())
	}
}

// 端口上千位数的回调地址也不能被误判（回归：strconv 忘判错会放行）。
func TestCNLoginPortParseStrict(t *testing.T) {
	if got := portFromListen(":" + strconv.Itoa(0)); got != 0 {
		t.Fatalf("port 0 should be rejected, got %d", got)
	}
}
