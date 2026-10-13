package huawei

import (
	"net/http"
	"testing"
	"time"
)

func TestKeepAliveClientFailFastHandshake(t *testing.T) {
	// The fallback / warm-loop client must also fail fast on a bad cold
	// handshake (5s), aligned with api/httpclient.go newBaseTransport. Guards
	// against the inherited 30s default dial / 10s handshake creeping back.
	c := newKeepAliveClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport")
	}
	if tr.TLSHandshakeTimeout != 5*time.Second {
		t.Fatalf("TLSHandshakeTimeout=%s want 5s", tr.TLSHandshakeTimeout)
	}
	if tr.DialContext == nil {
		t.Fatal("DialContext must be overridden to bound the cold dial")
	}
}
