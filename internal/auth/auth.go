package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Manager struct {
	secret   []byte
	user     string
	pass     string
	ttl      time.Duration
	basePath string

	// revoked 保存已登出的会话 jti → 过期时间。
	// Cookie 是 HttpOnly，前端删不掉，handleLogout 也只是清 cookie；
	// 没有这张表的话 token 在 24 小时 TTL 内始终有效——"退出登录"形同虚设。
	revokedMu sync.Mutex
	revoked   map[string]int64
}

type sessionClaims struct {
	User            string `json:"user"`
	Exp             int64  `json:"exp"`
	PasswordVersion string `json:"password_version"`
	Jti             string `json:"jti,omitempty"`
}

func New(user, password string) (*Manager, error) {
	return NewWithSecretFile(user, password, "")
}

// NewWithSecretFile keeps HMAC secret stable across restarts when path is set.
func NewWithSecretFile(user, password, secretFile string) (*Manager, error) {
	secret, err := loadOrCreateSecret(secretFile)
	if err != nil {
		return nil, err
	}
	return &Manager{secret: secret, user: user, pass: password, ttl: 24 * time.Hour,
		revoked: map[string]int64{}}, nil
}

func loadOrCreateSecret(path string) ([]byte, error) {
	if strings.TrimSpace(path) != "" {
		if b, err := os.ReadFile(path); err == nil {
			b = bytes.TrimSpace(b)
			if len(b) >= 16 {
				return b, nil
			}
		}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, secret, 0o600); err != nil {
			return nil, err
		}
	}
	return secret, nil
}

func (m *Manager) SetBasePath(base string) {
	m.basePath = strings.TrimRight(strings.TrimSpace(base), "/")
}

func (m *Manager) passwordVersion() string {
	sum := sha256.Sum256([]byte(m.pass))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) Login(password string) (string, error) {
	if subtle.ConstantTimeCompare([]byte(password), []byte(m.pass)) != 1 {
		return "", errors.New("invalid password")
	}
	exp := time.Now().Add(m.ttl).Unix()
	raw, _ := json.Marshal(sessionClaims{User: m.user, Exp: exp, PasswordVersion: m.passwordVersion(), Jti: newJti()})
	sig := m.sign(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (m *Manager) sign(data []byte) []byte {
	h := hmac.New(sha256.New, m.secret)
	h.Write(data)
	return h.Sum(nil)
}

func (m *Manager) Validate(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	if !hmac.Equal(m.sign(raw), sig) {
		return false
	}
	var c sessionClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return false
	}
	if time.Now().Unix() > c.Exp || c.PasswordVersion == "" || subtle.ConstantTimeCompare([]byte(c.PasswordVersion), []byte(m.passwordVersion())) != 1 {
		return false
	}
	if c.Jti != "" && m.isRevoked(c.Jti) {
		return false
	}
	return true
}

// Revoke 让一个已签发的会话立即失效（登出）。
func (m *Manager) Revoke(token string) {
	if m == nil || token == "" {
		return
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(m.sign(raw), sig) {
		return
	}
	var c sessionClaims
	if err := json.Unmarshal(raw, &c); err != nil || c.Jti == "" {
		return
	}
	m.revokedMu.Lock()
	defer m.revokedMu.Unlock()
	if m.revoked == nil {
		m.revoked = map[string]int64{}
	}
	// 记到过期时间即可：过期后的 token 本来就会被 Validate 拒掉。
	m.revoked[c.Jti] = c.Exp
	// 顺带清理已过期项，避免表无限增长。
	now := time.Now().Unix()
	for jti, exp := range m.revoked {
		if exp < now {
			delete(m.revoked, jti)
		}
	}
}

func (m *Manager) isRevoked(jti string) bool {
	m.revokedMu.Lock()
	defer m.revokedMu.Unlock()
	_, ok := m.revoked[jti]
	return ok
}

func newJti() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// Middleware guards JSON API and web pages except /api/login and /oauth/callback.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/login" || r.URL.Path == "/oauth/callback" || r.URL.Path == "/favicon.ico" ||
			r.URL.Path == "/login" || r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		token := ""
		// Prefer any valid admin_session when duplicates exist (path=/ vs /codearts).
		for _, c := range r.Cookies() {
			if c.Name == "admin_session" && m.Validate(c.Value) {
				token = c.Value
				break
			}
		}
		if token == "" {
			token = r.Header.Get("X-Admin-Token")
		}
		if token == "" || !m.Validate(token) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, m.basePath+"/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}
