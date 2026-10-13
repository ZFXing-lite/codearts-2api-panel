package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// CSRFToken derives a per-session token without storing server-side state.
func (m *Manager) CSRFToken(session string) string {
	if m == nil || session == "" {
		return ""
	}
	h := hmac.New(sha256.New, m.secret)
	_, _ = h.Write([]byte("csrf\x00" + session))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (m *Manager) ValidateCSRF(session, token string) bool {
	expected := m.CSRFToken(strings.TrimSpace(session))
	if expected == "" || strings.TrimSpace(token) == "" {
		return false
	}
	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(token)))
}
