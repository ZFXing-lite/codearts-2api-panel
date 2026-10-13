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
	s := New(config.Config{CNUpstreamTimeout: 5 * time.Second, DataDir: dir}, db, authMgr, nil)
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

// needs_relogin 打标的 CN 账号：handleCNChat 直接返回 rotate，不写响应。
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
	committed, retry := s.handleCNChat(context.Background(), rec, req,
		[]byte(`{"model":"snap-chat","messages":[{"role":"user","content":"hi"}]}`),
		acc, false, protoChat, "snap-chat")
	if committed || !retry {
		t.Fatalf("needs_relogin account must rotate without writing: committed=%v retry=%v", committed, retry)
	}
	if rec.Code != http.StatusOK { // recorder 默认 200 = 没写过任何响应
		t.Fatalf("response must stay untouched, got %d", rec.Code)
	}
}
