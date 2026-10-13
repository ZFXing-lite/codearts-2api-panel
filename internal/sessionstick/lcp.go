package sessionstick

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// LCP (Longest Common Prefix) session identification.
//
// Motivation:
//   - Primary session ID is derived from client headers or body keys. Some
//     clients regenerate that ID between turns (OpenAI SDK without an explicit
//     session_id header, tool sub-loops with fresh conversation_ids, and so
//     on). When that happens sticky affinity breaks and every turn spills to
//     a cold account, losing the upstream prompt-cache.
//   - LCP builds a content-derived key from a sparse fingerprint of the
//     canonical turns (system + first N user messages). Two requests whose
//     conversation prefix is identical produce the same key even when the
//     primary session ID drifted.
//
// The key is a secondary lookup: primary sticky wins if bound; LCP only fires
// when primary Get misses. Stored keys are sha256-hex so map lookups stay
// constant-time regardless of prompt size.

// CanonicalPart is a single addressable unit of a canonical turn (usually a
// text or image_url block). Order matters — LCP hashes are sequence-sensitive.
type CanonicalPart struct {
	Type  string // "text", "image", "tool_result"
	Value string // text content or resource identifier
}

// CanonicalTurn is one message reduced to its stable fingerprint inputs.
type CanonicalTurn struct {
	Role  string
	Parts []CanonicalPart
}

// ExtractCanonicalTurns pulls the system + first `maxUser` user turns from an
// OpenAI/Anthropic/Codex-shaped body. Everything else (tool results, later
// user turns, assistant history) is ignored because they mutate every turn
// and would destabilize the LCP key. Returns nil if the body is not a
// recognized shape.
func ExtractCanonicalTurns(body []byte, maxUser int) []CanonicalTurn {
	if len(body) == 0 {
		return nil
	}
	if maxUser <= 0 {
		maxUser = 2
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	var out []CanonicalTurn
	// Anthropic-style "system" top-level field (string or content-blocks array).
	if sys, ok := root["system"]; ok {
		if t := canonicalTurnFromContent("system", sys); t != nil {
			out = append(out, *t)
		}
	}
	msgs, _ := root["messages"].([]any)
	users := 0
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		role := strings.ToLower(asString(m["role"]))
		switch role {
		case "system":
			if t := canonicalTurnFromContent("system", m["content"]); t != nil {
				out = append(out, *t)
			}
		case "user":
			if users >= maxUser {
				return out
			}
			if t := canonicalTurnFromContent("user", m["content"]); t != nil {
				out = append(out, *t)
				users++
			}
		}
	}
	return out
}

func canonicalTurnFromContent(role string, content any) *CanonicalTurn {
	switch v := content.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return &CanonicalTurn{Role: role, Parts: []CanonicalPart{{Type: "text", Value: v}}}
	case []any:
		parts := make([]CanonicalPart, 0, len(v))
		for _, part := range v {
			switch p := part.(type) {
			case string:
				if strings.TrimSpace(p) != "" {
					parts = append(parts, CanonicalPart{Type: "text", Value: p})
				}
			case map[string]any:
				pt := strings.ToLower(asString(p["type"]))
				switch pt {
				case "text", "input_text", "output_text", "":
					if txt := asString(p["text"]); txt != "" {
						parts = append(parts, CanonicalPart{Type: "text", Value: txt})
					}
				case "image", "image_url", "input_image":
					if src := imagePartValue(p); src != "" {
						parts = append(parts, CanonicalPart{Type: "image", Value: src})
					}
				case "tool_result":
					// tool_result content is skipped: it changes every turn and
					// would poison the fingerprint. Anything else derived from
					// tool state also intentionally excluded.
				}
			}
		}
		if len(parts) == 0 {
			return nil
		}
		return &CanonicalTurn{Role: role, Parts: parts}
	default:
		return nil
	}
}

func imagePartValue(p map[string]any) string {
	if src, ok := p["image_url"].(map[string]any); ok {
		if u := asString(src["url"]); u != "" {
			return u
		}
	}
	if u := asString(p["image_url"]); u != "" {
		return u
	}
	if src, ok := p["source"].(map[string]any); ok {
		return asString(src["data"])
	}
	return ""
}

// fingerprintPartLimit sets the byte budget used per canonical part when its
// content is longer than 3 × chunkSize. Head/mid/tail sampling keeps two
// prompts differing only in the middle from collapsing to the same key.
const (
	fpChunkSize  = 4096
	fpMinInline  = 3 * fpChunkSize // parts smaller than this hash in full
	fpMinTailGap = 512             // avoid overlap between mid and tail slices
)

// FastTurnFingerprint returns a 16-byte hex digest for a single canonical
// turn. Large text parts are sparse-sampled (head/mid/tail 4 KiB each) so a
// giant single-message prompt still hashes in bounded time.
func FastTurnFingerprint(t CanonicalTurn) string {
	h := sha256.New()
	h.Write([]byte(t.Role))
	h.Write([]byte{0})
	for _, part := range t.Parts {
		h.Write([]byte(part.Type))
		h.Write([]byte{0})
		writeSparse(h, part.Value)
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

func writeSparse(h interface{ Write([]byte) (int, error) }, s string) {
	if len(s) <= fpMinInline {
		_, _ = h.Write([]byte(s))
		return
	}
	head := s[:fpChunkSize]
	tail := s[len(s)-fpChunkSize:]
	midStart := (len(s) - fpChunkSize) / 2
	if midStart < fpChunkSize+fpMinTailGap {
		midStart = fpChunkSize + fpMinTailGap
	}
	midEnd := midStart + fpChunkSize
	if midEnd > len(s)-fpChunkSize-fpMinTailGap {
		midEnd = len(s) - fpChunkSize - fpMinTailGap
	}
	if midEnd <= midStart {
		midStart = fpChunkSize
		midEnd = midStart + fpChunkSize
	}
	_, _ = h.Write([]byte(head))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(s[midStart:midEnd]))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(tail))
}

// LCPSessionKey returns "lcp:<hex16>" derived from a list of canonical turns.
// Returns "" when there is not enough signal to identify a stable prefix
// (empty conversation, images only with no text, etc.).
func LCPSessionKey(turns []CanonicalTurn) string {
	if len(turns) == 0 {
		return ""
	}
	textBytes := 0
	for _, t := range turns {
		for _, p := range t.Parts {
			if p.Type == "text" {
				textBytes += len(p.Value)
			}
		}
	}
	// Guard against short conversations that would collide across users
	// (a single "hi" would pin every fresh chat to the same account).
	if textBytes < 64 {
		return ""
	}
	h := sha256.New()
	for _, t := range turns {
		fp := FastTurnFingerprint(t)
		h.Write([]byte(fp))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return "lcp:" + hex.EncodeToString(sum[:16])
}

func asString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
