package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"go-codearts/internal/sessionstick"
)

// extractSessionID mirrors CLIProxyAPI session affinity key priority:
// explicit client headers → body session fields → stable hash of conversation root.
// BUG-15 fix: external session IDs are hashed with a per-installation salt to prevent
// spoofing attacks where a client pins themselves to a specific account.
// extractSessionIDs is extractSessionID plus a content-derived LCP key used
// as a secondary sticky fallback when clients rotate their primary id.
// Both returns are pre-hardened where applicable (primary via hardenSessionID;
// lcp is a hashed digest already so no salting is applied — an attacker
// cannot craft two different prompts that collide on sha256).
func extractSessionIDs(h http.Header, body []byte) (primary, lcp string) {
	primary = extractSessionID(h, body)
	lcp = lcpKeyFromBody(body)
	return
}

func lcpKeyFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	turns := sessionstick.ExtractCanonicalTurns(body, 2)
	if len(turns) == 0 {
		return ""
	}
	return sessionstick.LCPSessionKey(turns)
}

func extractSessionID(h http.Header, body []byte) string {
	if sid := firstHeader(h,
		"X-Claude-Code-Session-Id",
		"Session-Id",
		"Session_Id",
		"X-Http-Session-Id",
		"X-Session-Id",
		"X-Session-Affinity",
		"X-Slot-Session-Id",
		"X-Conversation-Id",
		"X-Thread-Id",
		"X-Client-Request-Id",
	); sid != "" {
		return hardenSessionID(sid)
	}

	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}

	for _, key := range []string{
		"prompt_cache_key", "promptCacheKey",
		"session_id", "sessionId",
		"conversation_id", "conversationId", "chat_id", "chatId",
	} {
		if sid := normalizeSessionID(asString(root[key])); sid != "" {
			return hardenSessionID(sid)
		}
	}
	if conv, ok := root["conversation"].(map[string]any); ok {
		if sid := normalizeSessionID(asString(conv["id"])); sid != "" {
			return hardenSessionID("conv:" + sid)
		}
	}
	if meta, ok := root["metadata"].(map[string]any); ok {
		for _, key := range []string{"session_id", "sessionId", "user_id", "userId", "conversation_id"} {
			if sid := normalizeSessionID(asString(meta[key])); sid != "" {
				return hardenSessionID("meta:" + sid)
			}
		}
	}

	// Fallback: hash model + system + first user message so multi-turn sticks
	// even when clients omit explicit session headers (common for OpenAI SDKs).
	return derivedSessionID(root)
}

func derivedSessionID(root map[string]any) string {
	model := asString(root["model"])
	var system, firstUser string
	msgs, _ := root["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		role := strings.ToLower(asString(m["role"]))
		text := messageText(m["content"])
		if role == "system" && system == "" {
			system = text
			continue
		}
		if role == "user" {
			firstUser = text
			break
		}
	}
	if system == "" && firstUser == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("go-codearts:sess:v1\x00" + model + "\x00" + system + "\x00" + firstUser))
	return "root:" + hex.EncodeToString(sum[:16])
}

func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			switch p := part.(type) {
			case string:
				b.WriteString(p)
			case map[string]any:
				if asString(p["type"]) == "text" {
					b.WriteString(asString(p["text"]))
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

func firstHeader(h http.Header, names ...string) string {
	for _, name := range names {
		if sid := normalizeSessionID(h.Get(name)); sid != "" {
			return sid
		}
	}
	return ""
}

func normalizeSessionID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 {
		return ""
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return raw
}

var sessionIDHardenSalt = "go-codearts:sess-harden:v1"

// configureSessionIDSalt loads or creates a per-installation salt. On first
// write, fsync + read-back verifies the salt actually landed on disk (a
// truncated / partially-written salt file would make hardenSessionID rotate
// on every boot, invalidating every client's session affinity). The read-back
// guards against an fs where WriteFile succeeded but a subsequent crash left
// the file empty; we prefer to fall back to the in-memory salt for this boot
// rather than pin a doomed persisted salt.
func configureSessionIDSalt(dataDir string) {
	if strings.TrimSpace(dataDir) == "" {
		return
	}
	path := filepath.Join(dataDir, ".session-salt")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 16 {
		sessionIDHardenSalt = string(b)
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return
	}
	// Read-back verify: only adopt as the persisted salt if we can prove the
	// bytes match what we intended to write.
	if got, err := os.ReadFile(path); err == nil && len(got) == len(b) && string(got) == string(b) {
		sessionIDHardenSalt = string(b)
	}
}

// hardenSessionID hashes external session IDs with a per-installation salt.
// This prevents clients from pinning themselves to a specific account by
// guessing or reusing session IDs (BUG-15 fix).
// The hash is deterministic so the same client always gets the same session binding.
//
// The earlier "long IDs pass through" branch was a bypass: an attacker who
// knew a target account's affinity key length could craft a matching ≥32-char
// id and pin themselves to that account, defeating the salt entirely. Every
// id — regardless of length or entropy claim — must run through the salted
// hash. Output is fixed-width so cache keys stay stable across id sizes.
func hardenSessionID(raw string) string {
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionIDHardenSalt + "\x00" + raw))
	return "h:" + hex.EncodeToString(sum[:16])
}

func asString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
