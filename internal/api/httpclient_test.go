package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go-codearts/internal/config"
	"go-codearts/internal/store"
)

func TestWithHeaderTimeoutClonesTransport(t *testing.T) {
	base := defaultHTTPClient()
	got := withHeaderTimeout(base, headerWaitHeavy)
	if got == base {
		t.Fatal("must clone client")
	}
	tr, ok := got.Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport")
	}
	if tr.ResponseHeaderTimeout != headerWaitHeavy {
		t.Fatalf("timeout=%s", tr.ResponseHeaderTimeout)
	}
	orig, _ := base.Transport.(*http.Transport)
	if orig.ResponseHeaderTimeout != headerWaitDefault {
		t.Fatalf("base mutated to %s", orig.ResponseHeaderTimeout)
	}
}

func TestHttpClientForChatDepleted(t *testing.T) {
	s := &Server{chatClients: newChatClientCache()}
	dep := &store.Account{ID: "d", CallGate: store.CallGateQuotaExhausted}
	c := s.httpClientForChat(dep, false)
	tr, _ := c.Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout != headerWaitDepleted {
		t.Fatalf("depleted wait=%s want %s", tr.ResponseHeaderTimeout, headerWaitDepleted)
	}
	ready := &store.Account{ID: "r", CallGate: store.CallGateReady}
	c = s.httpClientForChat(ready, false)
	tr, _ = c.Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout != headerWaitDefault {
		t.Fatalf("ready wait=%s", tr.ResponseHeaderTimeout)
	}
}

func TestHttpClientForChatDepletedUsesConfig(t *testing.T) {
	s := &Server{
		cfg:         config.Config{HeaderWaitDepleted: 35 * time.Second},
		chatClients: newChatClientCache(),
	}
	dep := &store.Account{ID: "d", CallGate: store.CallGateQuotaExhausted}
	c := s.httpClientForChat(dep, false)
	tr, _ := c.Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout != 35*time.Second {
		t.Fatalf("depleted wait=%s want 35s", tr.ResponseHeaderTimeout)
	}
}

func TestEnsureChatClientWarmHitsUpstream(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	s := &Server{
		cfg:         config.Config{HuaweiModelBase: srv.URL},
		chatClients: newChatClientCache(),
	}
	s.ensureChatClientWarm(&store.Account{ID: "a", CallGate: store.CallGateReady})
	if hits.Load() < 1 {
		t.Fatal("warmup must issue a request to HuaweiModelBase")
	}
	s.ensureChatClientWarm(&store.Account{ID: "a", CallGate: store.CallGateReady})
	if hits.Load() != 1 {
		t.Fatalf("recently warmed must skip, hits=%d", hits.Load())
	}
}

func TestHttpClientForChatHeavy(t *testing.T) {
	s := &Server{}
	c := s.httpClientForChat(nil, true)
	tr, _ := c.Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout != headerWaitHeavy {
		t.Fatalf("heavy timeout=%s", tr.ResponseHeaderTimeout)
	}
	c = s.httpClientForChat(nil, false)
	tr, _ = c.Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout != headerWaitDefault {
		t.Fatalf("light timeout=%s", tr.ResponseHeaderTimeout)
	}
}

func TestBaseTransportFailFastHandshake(t *testing.T) {
	// Cold-connection handshake must fail fast (5s) so a bad link rotates to
	// another account instead of stalling the client for 10s. Regression guard
	// for the TLS-handshake-timeout tuning; keep in sync with huawei/models.go.
	tr := newBaseTransport(headerWaitDefault)
	if tr.TLSHandshakeTimeout != 5*time.Second {
		t.Fatalf("TLSHandshakeTimeout=%s want 5s", tr.TLSHandshakeTimeout)
	}
}

func TestStartSSEKeepaliveStopDoesNotCancel(t *testing.T) {
	rec := &sseRecorder{header: http.Header{}}
	canceled := false
	stop := startSSEKeepalive(rec, &ssePin{Model: "glm-5.2"}, func() { canceled = true })
	time.Sleep(20 * time.Millisecond)
	stop()
	time.Sleep(20 * time.Millisecond)
	if canceled {
		t.Fatal("stopping TTFT keepalive must not abort an in-flight think")
	}
}

func TestStartSSEKeepalivePinsReasoning(t *testing.T) {
	rec := &sseRecorder{header: http.Header{}}
	pin := &ssePin{Model: "glm-5.2"}
	stop := startSSEKeepalive(rec, pin, nil)
	time.Sleep(20 * time.Millisecond)
	stop()
	if pin.ID != "chatcmpl-proxy-wait" {
		t.Fatalf("pin id=%q", pin.ID)
	}
	out := rec.body.String()
	if strings.Contains(out, "思考中") {
		t.Fatalf("keepalive must not emit 思考中: %s", out)
	}
	if strings.Contains(out, "\\u200b") {
		t.Fatalf("must not use zwsp: %s", out)
	}
}

type sseRecorder struct {
	header http.Header
	body   strings.Builder
	code   int
}

func (s *sseRecorder) Header() http.Header { return s.header }
func (s *sseRecorder) Write(p []byte) (int, error) {
	return s.body.Write(p)
}
func (s *sseRecorder) WriteHeader(statusCode int) { s.code = statusCode }
func (s *sseRecorder) Flush()                     {}
