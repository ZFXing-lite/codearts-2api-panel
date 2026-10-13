package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go-codearts/internal/auth"
	"go-codearts/internal/config"
	"go-codearts/internal/store"
)

func TestClaudeMessagesHTTPEnvelopeOrder(t *testing.T) {
	// Mock upstream that returns a minimal OpenAI-style SSE stream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		if f != nil {
			f.Flush()
		}
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n")
		if f != nil {
			f.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f != nil {
			f.Flush()
		}
	}))
	defer upstream.Close()

	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Upsert(&store.Account{
		ID:       "acc-dummy",
		Email:    "dummy@example.com",
		CallGate: store.CallGateReady,
		Credentials: &store.Credentials{
			AccessKeyID:     "AKIDUMMY",
			SecretAccessKey: "SKDUMMY",
			AuthMode:        "iam_permanent",
		},
	}); err != nil {
		t.Fatal(err)
	}
	authMgr, err := auth.New("admin", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s := New(config.Config{
		EarlyEnvelope:   true,
		DataDir:         dir,
		HuaweiModelBase: upstream.URL,
	}, db, authMgr, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(
		`{"model":"GLM-5.2","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	body := string(out)
	iStart := strings.Index(body, "event: message_start")
	iPing := strings.Index(body, "event: ping")
	if iStart < 0 || iPing < 0 || iStart > iPing {
		t.Fatalf("want message_start then ping, got %q", body)
	}
	if strings.Count(body, "event: message_start") != 1 {
		t.Fatalf("duplicate envelope: %q", body)
	}
}
