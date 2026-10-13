package api

import (
	"strings"
	"testing"

	"go-codearts/internal/auth"
)

func TestJSONDepth(t *testing.T) {
	shallow := []byte(`{"a":{"b":1}}`)
	if jsonDepth(shallow) != 2 {
		t.Fatalf("shallow depth=%d", jsonDepth(shallow))
	}
	deep := []byte(strings.Repeat("[", maxJSONDepth+1) + strings.Repeat("]", maxJSONDepth+1))
	if jsonDepth(deep) <= maxJSONDepth {
		t.Fatalf("expected depth over limit")
	}
}

func TestHardenSessionID(t *testing.T) {
	configureSessionIDSalt("test-salt")
	first := hardenSessionID("short-session")
	second := hardenSessionID("short-session")
	if first == "short-session" || first != second || !strings.HasPrefix(first, "h:") {
		t.Fatalf("unexpected hardened id: %q %q", first, second)
	}
}

func TestCSRFTokenRoundTrip(t *testing.T) {
	m, err := auth.New("admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	token := m.CSRFToken("session")
	if token == "" || !m.ValidateCSRF("session", token) || m.ValidateCSRF("other", token) {
		t.Fatal("csrf token validation failed")
	}
}
