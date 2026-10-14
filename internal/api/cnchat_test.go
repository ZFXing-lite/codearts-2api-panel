package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go-codearts/internal/auth"
	"go-codearts/internal/cnup"
	"go-codearts/internal/config"
	"go-codearts/internal/store"
)

func fakeCNUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		for _, line := range []string{
			`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"你好"}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		} {
			fmt.Fprint(w, line+"\n\n")
			if f != nil {
				f.Flush()
			}
		}
	}))
}

func newCNTestServer(t *testing.T, upstreamURL string, acc *store.Account) (*Server, *httptest.Server, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if acc != nil {
		if err := db.Upsert(acc); err != nil {
			t.Fatal(err)
		}
	}
	authMgr, err := auth.New("admin", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s := New(config.Config{CNEnabled: true, CNUpstreamTimeout: 5 * time.Second, DataDir: dir}, db, authMgr, nil)
	// 注入指向假上游的 cnup 客户端（生产路径走 cnUpstream 懒构造）。
	s.cnOnce.Do(func() {
		s.cnClient = cnup.NewWithChatEndpoint(5*time.Second, upstreamURL)
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, db
}

func cnTestAccount(id string) *store.Account {
	return &store.Account{
		ID:          id,
		Channel:     store.ChannelCN,
		Email:       "cn-user@example.com",
		UserID:      "uid-1",
		AccessToken: "tok-dummy",
		ChatID:      "chat-sticky-1",
		Credentials: &store.Credentials{
			AccessKeyID:     "AKDUMMY",
			SecretAccessKey: "SKDUMMY",
			SecurityToken:   "tok-dummy",
			AuthMode:        "iam_permanent",
		},
	}
}

// CN 账号选中后走 cnup 假上游，非流式聚合返回 OpenAI completion。
func TestCNChatDispatchNonStream(t *testing.T) {
	up := fakeCNUpstream(t)
	defer up.Close()
	_, ts, db := newCNTestServer(t, up.URL, cnTestAccount("cn-1"))

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/v1/chat/completions", strings.NewReader(
			`{"model":"snap-chat","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var payload struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Object != "chat.completion" || len(payload.Choices) == 0 ||
		!strings.Contains(payload.Choices[0].Message.Content, "你好") {
		t.Fatalf("bad completion: %+v", payload)
	}
	// chat 活动应回写 cn_last_active（watchdog 保活门控依赖）。
	saved, err := db.Get("cn-1")
	if err != nil || saved == nil {
		t.Fatalf("account missing: %v", err)
	}
	if saved.Metadata["cn_last_active"] == "" {
		t.Fatal("cn_last_active not recorded")
	}
}

// ChatID 粘性：账号已有 chat_id 且未显式指定时沿用；显式 header 优先。
func TestCNChatIDResolution(t *testing.T) {
	acc := cnTestAccount("cn-2")
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := cnChatID(r, []byte(`{}`), acc); got != "chat-sticky-1" {
		t.Fatalf("sticky chat_id lost: %q", got)
	}
	r.Header.Set("X-Codearts-Chat-Id", "chat-explicit")
	if got := cnChatID(r, []byte(`{"conversation_id":"chat-body"}`), acc); got != "chat-explicit" {
		t.Fatalf("header should win: %q", got)
	}
	r.Header.Del("X-Codearts-Chat-Id")
	if got := cnChatID(r, []byte(`{"conversation_id":"chat-body"}`), acc); got != "chat-body" {
		t.Fatalf("body conversation_id should win over sticky: %q", got)
	}
}

// /v1/models 把已检出的 CN 模型目录以 cn: 前缀合流。
func TestCNModelsMergedWithPrefix(t *testing.T) {
	up := fakeCNUpstream(t)
	defer up.Close()
	s, ts, _ := newCNTestServer(t, up.URL, cnTestAccount("cn-5"))
	cnup.SetAccountModels("uid-1", []cnup.ModelInfo{{ID: "Qwen3-VL-235B"}})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	_ = s
	found := map[string]bool{}
	for _, m := range payload.Data {
		found[m.ID] = true
	}
	if !found["cn:Qwen3-VL-235B"] {
		t.Fatal("missing cn:Qwen3-VL-235B in /v1/models")
	}
	// 注意：裸名 Qwen3-VL-235B 可能已存在于国际 fallback 目录，此处只断言
	// cn: 前缀条目必须出现（渠道可分辨）。
}

// 渠道前缀解析（workbuddy2api realm 前缀协议）。
func TestCNSplitChannelModel(t *testing.T) {
	cases := []struct{ in, realm, bare string }{
		{"cn:snap-chat", "cn", "snap-chat"},
		{"cn:qwen3-vl-235b", "cn", "qwen3-vl-235b"},
		{"intl:GLM-5.2", "intl", "GLM-5.2"},
		{"global:GLM-5.2", "intl", "GLM-5.2"},
		{"snap-chat", "", "snap-chat"},
		{"", "", ""},
		{"weird:name", "", "weird:name"},
	}
	for _, c := range cases {
		if r, b := cnSplitChannelModel(c.in); r != c.realm || b != c.bare {
			t.Errorf("cnSplitChannelModel(%q)=(%q,%q), want (%q,%q)", c.in, r, b, c.realm, c.bare)
		}
	}
}

// cn: 前缀强制 CN 渠道，且模型走 CN canonical 管线（不被 intl
// rewriteModel 改写；qwen3-vl-235b 应交上游 Qwen3-VL-235B 而非 GLM-5.2）。
func TestCNPrefixForcesCNChannelAndModel(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if m, _ := body["model"].(string); m != "" {
			gotModel = m
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()

	// 池里同放 intl 账号：前缀必须保证不选它。
	_, ts, db := newCNTestServer(t, up.URL, cnTestAccount("cn-4"))
	intlAcc := &store.Account{
		ID:    "intl-1",
		Email: "intl@example.com",
		Credentials: &store.Credentials{
			AccessKeyID: "AKINTL", SecretAccessKey: "SKINTL", AuthMode: "iam_permanent",
		},
	}
	if err := db.Upsert(intlAcc); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/v1/chat/completions", strings.NewReader(
			`{"model":"cn:qwen3-vl-235b","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if gotModel != "Qwen3-VL-235B" {
		t.Fatalf("upstream model=%q (intl rewrite or wrong dispatch leaked)", gotModel)
	}
}
func TestCNChatNeedsReloginSkipped(t *testing.T) {
	up := fakeCNUpstream(t)
	defer up.Close()
	acc := cnTestAccount("cn-3")
	acc.Metadata = map[string]string{"cn_needs_relogin": "true"}
	s, _, _ := newCNTestServer(t, up.URL, acc)

	rec := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"/v1/chat/completions", strings.NewReader(
			`{"model":"snap-chat","messages":[{"role":"user","content":"hi"}]}`))
	committed, retry, _, _ := s.handleCNChat(context.Background(), rec, req,
		[]byte(`{"model":"snap-chat","messages":[{"role":"user","content":"hi"}]}`),
		acc, false, protoChat, "snap-chat")
	if committed || !retry {
		t.Fatalf("needs_relogin account must rotate without writing: committed=%v retry=%v", committed, retry)
	}
	if rec.Code != http.StatusOK { // recorder 默认 200 = 没写过任何响应
		t.Fatalf("response must stay untouched, got %d", rec.Code)
	}
}
