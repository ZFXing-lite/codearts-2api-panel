package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type sseAssembly struct {
	id, model           string
	content             strings.Builder
	reasoning           strings.Builder
	sentContent         strings.Builder
	sentReasoning       strings.Builder
	usagePrompt         float64
	usageCompl          float64
	usageTotal          float64
	usageCached         float64
	usageCachedDetails  float64
	usagePromptCacheHit float64
	sawDone             bool
	sawFinish           bool
	sawFilter           bool
	toolCalls           int
	tools               []map[string]any
	placeholderN        int
	emittedTools        bool
	emittedFinish       bool
	emittedVisible      bool
	loggedCap           bool // one-shot log flag when assembly cap is hit
	lastFinish          string
	filterMsg           string // upstream safety text, if any
}

// appendContent adds s to the retained content buffer, bounded by
// sseAssemblyCapBytes. Downstream bytes are already emitted by the caller
// verbatim — this only guards the retained copy.
func (a *sseAssembly) appendContent(s string) {
	if a == nil || s == "" {
		return
	}
	if _, hit := appendBounded(&a.content, s, sseAssemblyCapBytes()); hit && !a.loggedCap {
		a.loggedCap = true
		log.Printf("chat sse_assembly_cap_hit which=content cap=%d", sseAssemblyCapBytes())
	}
}

// appendReasoning is appendContent for the reasoning buffer; shares the
// same cap and one-shot log flag.
func (a *sseAssembly) appendReasoning(s string) {
	if a == nil || s == "" {
		return
	}
	if _, hit := appendBounded(&a.reasoning, s, sseAssemblyCapBytes()); hit && !a.loggedCap {
		a.loggedCap = true
		log.Printf("chat sse_assembly_cap_hit which=reasoning cap=%d", sseAssemblyCapBytes())
	}
}

// isProviderFilterFinish matches OpenAI/Anthropic/Huawei safety stop reasons that
// OpenCode maps to ContentFilterError ("blocked by the provider's content filter").
func isProviderFilterFinish(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return false
	}
	switch s {
	case "content_filter", "content-filter", "contentfilter",
		"refusal", "refuse", "blocked", "moderation":
		return true
	}
	// Do NOT match bare "sensitive"/"safety" — those appear inside normal Huawei
	// reply text ("involves sensitive information") and are not finish reasons.
	return strings.Contains(s, "content_filter") || strings.Contains(s, "content-filter") ||
		strings.Contains(s, "refus") || strings.Contains(s, "moderat")
}

func maybeFilterPayload(payload []byte) bool {
	lower := bytes.ToLower(payload)
	return bytes.Contains(lower, []byte("content_filter")) ||
		bytes.Contains(lower, []byte("content-filter")) ||
		bytes.Contains(lower, []byte(`"refusal"`)) ||
		bytes.Contains(lower, []byte(`"finish_reason":"refuse`)) ||
		bytes.Contains(lower, []byte("moderation"))
}

// looksLikeSensitiveContentReject detects Huawei/OpenAI-style safety refusals
// that should be shown to the client as normal assistant text (not proxy errors).
func looksLikeSensitiveContentReject(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	s := string(body)
	low := strings.ToLower(s)
	keys := []string{
		"sensitive information",
		"try a new topic",
		"involves sensitive",
		"content policy",
		"content_filter",
		"敏感信息",
		"敏感内容",
		"换个话题",
		"更换话题",
		"请更换",
		"无法回答",
	}
	for _, k := range keys {
		if strings.Contains(low, strings.ToLower(k)) || strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// extractUpstreamAssistText pulls the human-readable refusal/content text from
// Huawei/OpenAI error or chat payloads.
func extractUpstreamAssistText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil && obj != nil {
		if msg := firstString(obj, "error_msg", "err_msg", "message", "msg", "error_message"); msg != "" {
			if !strings.HasPrefix(strings.TrimSpace(msg), "{") {
				return strings.TrimSpace(msg)
			}
		}
		if errObj, ok := obj["error"].(map[string]any); ok {
			if msg := firstString(errObj, "message", "msg", "error_msg"); msg != "" {
				return strings.TrimSpace(msg)
			}
		}
		if choices, ok := obj["choices"].([]any); ok {
			for _, raw := range choices {
				ch, _ := raw.(map[string]any)
				if ch == nil {
					continue
				}
				if msg, _ := ch["message"].(map[string]any); msg != nil {
					if s, _ := msg["content"].(string); strings.TrimSpace(s) != "" {
						return strings.TrimSpace(s)
					}
				}
				if delta, _ := ch["delta"].(map[string]any); delta != nil {
					if s, _ := delta["content"].(string); strings.TrimSpace(s) != "" {
						return strings.TrimSpace(s)
					}
				}
				if s, _ := ch["text"].(string); strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	t := strings.TrimSpace(string(body))
	if t != "" && !strings.HasPrefix(t, "{") && !strings.HasPrefix(t, "[") {
		return t
	}
	return ""
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

var thinkingFieldNames = []string{
	"reasoning_content", "reasoning", "reasoning_text", "reasoningContent",
	"thinking", "thinking_content", "thinking_delta", "thinkingContent",
}

func firstThinkingString(m map[string]any) string {
	return firstString(m, thinkingFieldNames...)
}

// visibleText pulls assistant text from a string or OpenAI/Claude content parts.
func visibleText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, raw := range t {
			if s, ok := raw.(string); ok {
				b.WriteString(s)
				continue
			}
			pm, _ := raw.(map[string]any)
			if pm == nil {
				continue
			}
			typ := strings.ToLower(fmt.Sprint(pm["type"]))
			if strings.Contains(typ, "think") || strings.Contains(typ, "reason") {
				continue
			}
			if s := firstString(pm, "text", "content"); s != "" {
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return ""
	}
}

// normalizeThinkingPayload copies Huawei/GLM thinking aliases onto
// reasoning_content so OpenCode actually renders the think panel.
func thinkingFromContentParts(v any) string {
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, raw := range arr {
		pm, _ := raw.(map[string]any)
		if pm == nil {
			continue
		}
		typ := strings.ToLower(fmt.Sprint(pm["type"]))
		if !strings.Contains(typ, "think") && !strings.Contains(typ, "reason") {
			continue
		}
		if s := firstString(pm, "text", "thinking", "reasoning_content", "content", "reasoning"); s != "" {
			b.WriteString(s)
		}
	}
	return b.String()
}

func flattenThinkingMap(m map[string]any) bool {
	if m == nil {
		return false
	}
	if s, _ := m["reasoning_content"].(string); strings.TrimSpace(s) != "" {
		return false
	}
	if s := firstThinkingString(m); s != "" {
		m["reasoning_content"] = s
		return true
	}
	if s := thinkingFromContentParts(m["content"]); s != "" {
		m["reasoning_content"] = s
		return true
	}
	return false
}

func normalizeThinkingPayload(payload []byte) []byte {
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil || obj == nil {
		return payload
	}
	choices, _ := obj["choices"].([]any)
	changed := false
	for _, raw := range choices {
		ch, _ := raw.(map[string]any)
		if ch == nil {
			continue
		}
		for _, bucket := range []string{"delta", "message"} {
			m, _ := ch[bucket].(map[string]any)
			if flattenThinkingMap(m) {
				changed = true
			}
		}
	}
	if !changed {
		return payload
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return b
}

func wrapThinkChunk(text, id, model string) []byte {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	chunk := map[string]any{
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"reasoning_content": text,
			},
			"finish_reason": nil,
		}},
	}
	if id != "" {
		chunk["id"] = id
	}
	if model != "" {
		chunk["model"] = model
	}
	b, _ := json.Marshal(chunk)
	return b
}

// coerceOpenAIChunk makes every Huawei/ModelArts frame OpenAI-shaped.
// A choices-less object (e.g. {"text":"..."}) makes OpenCode Zod-abort and
// the rest of the thinking stream never arrives.
func coerceOpenAIChunk(payload []byte, asm *sseAssembly) []byte {
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil || obj == nil {
		return nil
	}
	if _, _, ok := parseHuaweiModelErr(payload); ok {
		return payload
	}
	if _, hasChoices := obj["choices"]; hasChoices {
		return normalizeThinkingPayload(payload)
	}
	if _, hasErr := obj["error"]; hasErr {
		return payload
	}
	// Usage-only frame (no choices, no text, no thinking) — pass through
	// so ingestPayload can extract token counts. Without this, the final
	// SSE usage frame is silently dropped and token tracking stays at 0.
	if _, hasUsage := obj["usage"]; hasUsage {
		return payload
	}
	text := firstThinkingString(obj)
	if text == "" {
		if s, _ := obj["text"].(string); s != "" && s != "[DONE]" {
			text = s
		}
	}
	if text == "" {
		return nil
	}
	id, model := "", ""
	if asm != nil {
		id, model = asm.id, asm.model
	}
	return wrapThinkChunk(text, id, model)
}

func sseDataPayload(line []byte) (payload []byte, ok bool) {
	trimmed := bytes.TrimSpace(bytes.TrimRight(line, "\r\n"))
	if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte(":")) {
		return nil, false
	}
	low := bytes.ToLower(trimmed)
	if bytes.HasPrefix(low, []byte("event:")) || bytes.HasPrefix(low, []byte("id:")) || bytes.HasPrefix(low, []byte("retry:")) {
		return nil, false
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:"))), true
	}
	if trimmed[0] == '{' {
		return trimmed, true
	}
	return nil, false
}

func salvageSSEPayload(leftover []byte) []byte {
	payload, ok := sseDataPayload(leftover)
	if !ok || len(payload) == 0 {
		return nil
	}
	if bytes.Equal(payload, []byte("[DONE]")) || json.Valid(payload) {
		return payload
	}
	return nil
}

// writeTextAsChatSSE emits a normal assistant stream so clients display upstream
// safety text instead of a proxy/transport error.
func writeTextAsChatSSE(w http.ResponseWriter, headersAlreadySent bool, text, model, id string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if model == "" {
		model = "unknown"
	}
	if id == "" {
		id = "chatcmpl-upstream-safe"
	}
	flusher, _ := w.(http.Flusher)
	if !headersAlreadySent {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Cache", "BYPASS")
		w.WriteHeader(http.StatusOK)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// Detect Claude protocol from the id prefix to send the right format.
	if strings.HasPrefix(id, "msg_proxy") {
		// Claude Messages API format.
		startMsg, _ := json.Marshal(map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            id,
				"type":          "message",
				"role":          "assistant",
				"model":         model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
		_, _ = io.WriteString(w, "event: message_start\ndata: "+string(startMsg)+"\n\n")
		blockStart, _ := json.Marshal(map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		_, _ = io.WriteString(w, "event: content_block_start\ndata: "+string(blockStart)+"\n\n")
		textDelta, _ := json.Marshal(map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: "+string(textDelta)+"\n\n")
		blockStop, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": 0})
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: "+string(blockStop)+"\n\n")
		msgDelta, _ := json.Marshal(map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		})
		_, _ = io.WriteString(w, "event: message_delta\ndata: "+string(msgDelta)+"\n\n")
		msgStop, _ := json.Marshal(map[string]any{"type": "message_stop"})
		_, _ = io.WriteString(w, "event: message_stop\ndata: "+string(msgStop)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	write := func(payload any) {
		b, _ := json.Marshal(payload)
		_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	write(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": text},
			"finish_reason": nil,
		}},
	})
	write(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// sanitizeFilterFinish only rewrites finish_reason content_filter → null so
// OpenCode won't abort; upstream message content is never modified.
// one-api StreamHandler / new-api OaiStreamHandler forward finish_reason as-is.
func payloadHasFinishReason(payload []byte) bool {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil || obj == nil {
		return false
	}
	choices, _ := obj["choices"].([]any)
	for _, raw := range choices {
		ch, _ := raw.(map[string]any)
		if ch == nil {
			continue
		}
		if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
			return true
		}
	}
	return false
}

func sanitizeFilterFinish(payload []byte) ([]byte, bool) {
	if !maybeFilterPayload(payload) {
		return payload, false
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload, false
	}
	changed := false
	if choices, ok := obj["choices"].([]any); ok {
		for _, raw := range choices {
			ch, _ := raw.(map[string]any)
			if ch == nil {
				continue
			}
			fr, _ := ch["finish_reason"].(string)
			if isProviderFilterFinish(fr) {
				ch["finish_reason"] = nil
				changed = true
			}
		}
	}
	if !changed {
		return payload, false
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return payload, false
	}
	return b, true
}

func (a *sseAssembly) ingestPayload(payload []byte) {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return
	}
	if id, _ := obj["id"].(string); id != "" {
		a.id = id
	}
	if model, _ := obj["model"].(string); model != "" {
		a.model = model
	}
	// Keep upstream safety/refusal text for passthrough when deltas are empty.
	if msg := extractUpstreamAssistText(payload); msg != "" {
		a.filterMsg = msg
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		log.Printf("chat sse_usage_raw fields=%v", u)
		if v := asFloat(u["prompt_tokens"]); v > 0 {
			a.usagePrompt = v
		}
		if v := asFloat(u["completion_tokens"]); v > 0 {
			a.usageCompl = v
		}
		if v := asFloat(u["total_tokens"]); v > 0 {
			a.usageTotal = v
		}
		if v := asFloat(u["cached_tokens"]); v > 0 {
			a.usageCached = v
		}
		if v := asFloat(u["prompt_cache_hit_tokens"]); v > 0 {
			a.usagePromptCacheHit = v
		}
		if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
			if v := asFloat(details["cached_tokens"]); v > 0 {
				a.usageCachedDetails = v
			}
		}
	}
	if choices, ok := obj["choices"].([]any); ok {
		for _, raw := range choices {
			ch, _ := raw.(map[string]any)
			if ch == nil {
				continue
			}
			if fr, exists := ch["finish_reason"]; exists && fr != nil {
				if s, ok := fr.(string); ok && s != "" {
					a.sawFinish = true
					a.lastFinish = s
					if isProviderFilterFinish(s) {
						a.sawFilter = true
					}
				}
			}
			delta, _ := ch["delta"].(map[string]any)
			if delta == nil {
				if msg, _ := ch["message"].(map[string]any); msg != nil {
					if s := visibleText(msg["content"]); s != "" {
						if trimmed := trimDuplicateDelta(&a.content, s); trimmed != "" {
							a.appendContent(trimmed)
						}
					}
					if s := firstThinkingString(msg); s != "" {
						a.noteThinkText(s)
					}
					a.noteToolCalls(msg["tool_calls"])
					a.noteToolCalls(msg["function_call"])
				}
				continue
			}
			if s := visibleText(delta["content"]); s != "" {
				if trimmed := trimDuplicateDelta(&a.content, s); trimmed != "" {
					a.appendContent(trimmed)
				}
			}
			a.noteToolCalls(delta["tool_calls"])
			a.noteToolCalls(delta["function_call"])
			if s := firstThinkingString(delta); s != "" {
				a.noteThinkText(s)
			}
		}
	}
}

func (a *sseAssembly) completionJSON() ([]byte, float64, error) {
	msg := map[string]any{"role": "assistant", "content": a.content.String()}
	if a.reasoning.Len() > 0 {
		msg["reasoning_content"] = a.reasoning.String()
	}
	if ready := a.readyTools(); len(ready) > 0 {
		out := make([]any, len(ready))
		for i, t := range ready {
			out[i] = t
		}
		msg["tool_calls"] = out
	}
	finish := a.lastFinish
	if a.hasToolCall() {
		finish = "tool_calls"
	} else if finish == "" {
		finish = "stop"
	}
	completion := map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   a.model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msg,
				"finish_reason": finish,
			},
		},
	}
	if a.usageTotal > 0 || a.usagePrompt > 0 || a.usageCompl > 0 || a.usageCached > 0 || a.usageCachedDetails > 0 || a.usagePromptCacheHit > 0 {
		usage := map[string]any{
			"prompt_tokens":     a.usagePrompt,
			"completion_tokens": a.usageCompl,
			"total_tokens":      a.usageTotal,
		}
		if a.usageCached > 0 {
			usage["cached_tokens"] = a.usageCached
		}
		if a.usagePromptCacheHit > 0 {
			usage["prompt_cache_hit_tokens"] = a.usagePromptCacheHit
		}
		if a.usageCachedDetails > 0 {
			usage["prompt_tokens_details"] = map[string]any{
				"cached_tokens": a.usageCachedDetails,
			}
		}
		completion["usage"] = usage
	}
	if completion["id"] == "" {
		completion["id"] = "chatcmpl-proxy-cache"
	}
	if completion["model"] == "" {
		completion["model"] = "unknown"
	}
	body, err := json.Marshal(completion)
	if err != nil {
		return nil, 0, err
	}
	tok := a.usageTotal
	if tok <= 0 {
		tok = a.usagePrompt + a.usageCompl
	}
	return body, tok, nil
}

func readSSEAsCompletionJSON(ctx context.Context, src io.Reader) ([]byte, float64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	asm, err := consumeSSE(ctx, src, nil)
	if err != nil {
		return nil, 0, err
	}
	return asm.completionJSON()
}

const sseIdleZWSP = "\u200b"

func flushSSE(flusher http.Flusher) {
	if flusher != nil {
		flusher.Flush()
	}
}

func writeSSEComment(w io.Writer, flusher http.Flusher) error {
	if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
		return err
	}
	flushSSE(flusher)
	return nil
}

func payloadHasToolDelta(payload []byte) bool {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return false
	}
	choices, _ := obj["choices"].([]any)
	for _, raw := range choices {
		ch, _ := raw.(map[string]any)
		if ch == nil {
			continue
		}
		if delta, _ := ch["delta"].(map[string]any); delta != nil {
			if delta["tool_calls"] != nil || delta["function_call"] != nil {
				return true
			}
		}
		if msg, _ := ch["message"].(map[string]any); msg != nil {
			if msg["tool_calls"] != nil || msg["function_call"] != nil {
				return true
			}
		}
	}
	return false
}

func assembledToolInfo(body []byte) string {
	var obj struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &obj) != nil || len(obj.Choices) == 0 {
		return ""
	}
	var parts []string
	for _, tc := range obj.Choices[0].Message.ToolCalls {
		parts = append(parts, fmt.Sprintf("%s:%d", tc.Function.Name, len(tc.Function.Arguments)))
	}
	return strings.Join(parts, ",")
}

// regurgitationMinContentBytes: a text-only turn must exceed this length before
// its fingerprint is treated as a regurgitation signal. Two consecutive "OK"s
// or "." acknowledgements are a false positive we do NOT want to punish — they
// happen legitimately in tool-heavy chats where the model briefly acks. Real
// regurgitation loops (the ones that eat token budget) are long verbatim
// replays, well above this floor. Tool calls of any size bypass the floor
// because a repeated `Bash: ls .` IS a genuine loop.
const regurgitationMinContentBytes = 24

// assembledResponseFingerprint hashes the model's VISIBLE output (content +
// tool call arguments) so callers can detect turn-over-turn regurgitation.
// Reasoning content is deliberately excluded: it varies naturally even when
// the model is looping (different internal wording), and clients don't see it
// so it can't be what drives the tool loop's decision.
//
// Returns 0 for empty/unparseable/trivially-short bodies — callers must treat
// 0 as "no signal" (never trigger regurgitation logic on 0), matching the
// guard in noteAssistantResponse.
func assembledResponseFingerprint(body []byte) uint64 {
	var obj struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &obj) != nil || len(obj.Choices) == 0 {
		return 0
	}
	msg := obj.Choices[0].Message
	// False-positive floor: pure short acknowledgements ("OK", ".", "Done")
	// with no tool calls are not real regurgitation — never fingerprint them.
	if len(msg.ToolCalls) == 0 && len(strings.TrimSpace(msg.Content)) < regurgitationMinContentBytes {
		return 0
	}
	h := fnv.New64a()
	// Content — the visible answer text.
	h.Write([]byte(msg.Content))
	// Tool calls — including args, so parallel "Read on same file" degeneracy
	// is caught the same way as visible-content degeneracy.
	for _, tc := range msg.ToolCalls {
		h.Write([]byte{0})
		h.Write([]byte(tc.Function.Name))
		h.Write([]byte{0})
		h.Write([]byte(tc.Function.Arguments))
	}
	// Guard against the degenerate empty case ending up as 0 (which we use as
	// the "no signal" sentinel): if content+tools were all empty, force 0.
	if msg.Content == "" && len(msg.ToolCalls) == 0 {
		return 0
	}
	sum := h.Sum64()
	if sum == 0 {
		sum = 1 // vanishingly unlikely, but keep 0 reserved for "no signal"
	}
	return sum
}

func assembledStreamShape(body []byte) (reason, content, tools int, finish string) {
	var obj struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
				ToolCalls []any  `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &obj) != nil || len(obj.Choices) == 0 {
		return 0, 0, 0, ""
	}
	ch := obj.Choices[0]
	return len(ch.Message.Reasoning), len(ch.Message.Content), len(ch.Message.ToolCalls), ch.FinishReason
}

// writeSSEReasoningPing emits a thinking delta on the SAME completion id.
// A different id looks like a second stream and clients abort mid-think.
func writeSSEReasoningPing(w io.Writer, flusher http.Flusher, id, model string) error {
	if id == "" {
		return writeSSEComment(w, flusher)
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{
					"reasoning_content": sseIdleZWSP,
					"reasoning":         sseIdleZWSP,
					"reasoning_text":    sseIdleZWSP,
				},
				"finish_reason": nil,
			},
		},
	}
	if model != "" {
		chunk["model"] = model
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, ": keepalive\n\ndata: "+string(payload)+"\n\n"); err != nil {
		return err
	}
	flushSSE(flusher)
	return nil
}

func peekSSEIdentity(payload string) (id, model string) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return "", ""
	}
	id, _ = obj["id"].(string)
	model, _ = obj["model"].(string)
	return id, model
}

func payloadHasVisibleDelta(payload string) bool {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return false
	}
	choices, _ := obj["choices"].([]any)
	for _, raw := range choices {
		ch, _ := raw.(map[string]any)
		if ch == nil {
			continue
		}
		delta, _ := ch["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if delta["tool_calls"] != nil || delta["function_call"] != nil {
			return true
		}
		for _, key := range []string{"content", "reasoning_content", "reasoning", "reasoning_text"} {
			if s, _ := delta[key].(string); s != "" && !isPlaceholderThink(s) {
				return true
			}
		}
	}
	return false
}

func isPlaceholderThink(s string) bool {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\u200b", ""))
	if s == "" {
		return true
	}
	rest := strings.ReplaceAll(s, "思考中", "")
	return strings.TrimSpace(rest) == ""
}

func mostlyPlaceholderThink(s string) bool {
	compact := strings.ReplaceAll(strings.ReplaceAll(s, "\u200b", ""), " ", "")
	compact = strings.TrimSpace(compact)
	if compact == "" {
		return false
	}
	if strings.Count(compact, "思考中") < 2 {
		return false
	}
	rest := strings.ReplaceAll(compact, "思考中", "")
	return len(rest)*5 < len(compact)
}

func scrubPlaceholderThink(s string) string {
	if s == "" || isPlaceholderThink(s) || mostlyPlaceholderThink(s) {
		return ""
	}
	for strings.HasPrefix(s, "思考中") {
		s = strings.TrimPrefix(s, "思考中")
	}
	return strings.TrimSpace(s)
}

func (a *sseAssembly) noteThinkText(s string) {
	if a == nil || s == "" {
		return
	}
	if isPlaceholderThink(s) {
		n := strings.Count(s, "思考中")
		if n < 1 {
			n = 1
		}
		a.placeholderN += n
		return
	}
	if trimmed := trimDuplicateDelta(&a.reasoning, s); trimmed != s {
		if trimmed == "" {
			return
		}
		s = trimmed
	}
	a.appendReasoning(s)
	if mostlyPlaceholderThink(a.reasoning.String()) {
		a.placeholderN += strings.Count(s, "思考中")
		if a.placeholderN < 2 {
			a.placeholderN = 2
		}
		a.reasoning.Reset()
	}
}

func (a *sseAssembly) thinkStuck() bool {
	if a == nil || a.hasAnswer() {
		return false
	}
	return a.placeholderN >= 8
}

// dedupMinLen bounds trimDuplicateDelta to sizeable chunks only. GLM streams
// token-by-token in normal operation (a few bytes per SSE frame), so a
// legitimate delta almost never reaches this length — only an anomalous
// re-send/overlap of already-emitted text does, which keeps short, naturally
// repeated words/punctuation (e.g. "的的", list bullets) untouched.
const dedupMinLen = 32

// sseDedupEnabled reports whether the SSE mid-stream duplicate-delta guard
// is turned on. OFF by default (pure-forwarding rule); operator opts in via
// SSE_DEDUP=1 when upstream is known to double-splice paragraphs.
func sseDedupEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SSE_DEDUP"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// sseAssemblyCapBytes bounds the retained per-request assembly buffer for
// content + reasoning. The downstream client always gets every byte
// forwarded verbatim; this cap only limits the in-memory copy kept for
// fingerprinting, cached-body reuse, and post-stream inspection. Prevents
// RSS blowup at chatSemCap=1024 when a giant response streams through.
// Configurable via SSE_ASSEMBLY_MAX (bytes); default 16 MiB.
func sseAssemblyCapBytes() int {
	if v := strings.TrimSpace(os.Getenv("SSE_ASSEMBLY_MAX")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 16 * 1024 * 1024
}

// appendBounded appends s to b, but writes at most (cap-b.Len()) bytes.
// Returns the number of bytes actually written and whether the cap was hit
// during this call (for one-shot log observability at the point of clipping).
func appendBounded(b *strings.Builder, s string, cap int) (int, bool) {
	if b == nil || s == "" {
		return 0, false
	}
	if cap <= 0 || b.Len() >= cap {
		return 0, cap > 0 && b.Len() >= cap
	}
	room := cap - b.Len()
	if len(s) <= room {
		b.WriteString(s)
		return len(s), false
	}
	b.WriteString(s[:room])
	return room, true
}

// dedupWindow caps how far back into already-sent text we look for an
// overlap, so the check stays O(1)-ish per delta regardless of total
// response length.
const dedupWindow = 4000

// trimDuplicateDelta guards against upstream re-sending SSE delta bytes that
// overlap with (or exactly repeat) text already accumulated in acc. Observed
// live: a client rendering the same paragraph 2-3x with a small character
// range silently missing at the seam — consistent with a raw stream splice,
// not the gateway's own retry loop (that path is separately guarded by
// errSSEWired/sse_drop_post_finish, both of which only engage AFTER a
// finish_reason/[DONE] boundary; this fires mid-generation, before any
// boundary, which the retry-loop guard structurally cannot reach).
// Returns the suffix of s that is genuinely new (unseen), or "" if s is
// wholly a repeat of the recent tail of acc.
//
// Env-gated: off by default (pure-forwarding rule). Both ingest-side
// assembly appends and the outgoing dedupOutgoingPayload feed through this
// function, so a single gate here keeps cachedBody / fingerprint /
// downstream bytes all byte-faithful when SSE_DEDUP != 1.
func trimDuplicateDelta(acc *strings.Builder, s string) string {
	if !sseDedupEnabled() {
		return s
	}
	if len(s) < dedupMinLen || acc.Len() == 0 {
		return s
	}
	accStr := acc.String()
	winStart := 0
	if len(accStr) > dedupWindow {
		winStart = len(accStr) - dedupWindow
	}
	tail := accStr[winStart:]
	if strings.Contains(tail, s) {
		return ""
	}
	maxK := len(s)
	if len(tail) < maxK {
		maxK = len(tail)
	}
	for k := maxK; k >= dedupMinLen; k-- {
		if strings.HasSuffix(tail, s[:k]) {
			return s[k:]
		}
	}
	return s
}

// dedupOutgoingPayload applies trimDuplicateDelta to the content/reasoning
// deltas of a single OpenAI-shaped SSE chunk right before it is relayed to
// the client, using asm.sentContent/asm.sentReasoning as the record of what
// has already reached the wire. finish_reason/tool_calls/usage are left
// untouched and always keep the frame alive even if its text is fully
// deduped away.
//
// DEFAULT OFF (pure-forwarding rule): the gateway does not rewrite upstream
// response bytes unless the operator explicitly opts in via SSE_DEDUP=1.
// This function no-ops otherwise, and the client receives whatever upstream
// actually emitted — duplicate seams included.
func dedupOutgoingPayload(payload []byte, asm *sseAssembly) ([]byte, bool) {
	if asm == nil || !sseDedupEnabled() {
		return payload, false
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil || obj == nil {
		return payload, false
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return payload, false
	}
	changed := false
	hasRemainingWork := false
	if usage, ok := obj["usage"]; ok && usage != nil {
		hasRemainingWork = true
	}
	for _, raw := range choices {
		ch, _ := raw.(map[string]any)
		if ch == nil {
			continue
		}
		if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
			hasRemainingWork = true
		}
		delta, _ := ch["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if delta["tool_calls"] != nil || delta["function_call"] != nil {
			hasRemainingWork = true
		}
		if s, ok := delta["content"].(string); ok && s != "" {
			trimmed := trimDuplicateDelta(&asm.sentContent, s)
			if trimmed != s {
				changed = true
				if trimmed == "" {
					delete(delta, "content")
					log.Printf("chat sse_dup_delta_drop kind=content id=%s bytes=%d", asm.id, len(s))
				} else {
					delta["content"] = trimmed
					log.Printf("chat sse_dup_delta_trim kind=content id=%s dropped=%d kept=%d", asm.id, len(s)-len(trimmed), len(trimmed))
				}
			}
			if trimmed != "" {
				asm.sentContent.WriteString(trimmed)
				hasRemainingWork = true
			}
		}
		for _, key := range []string{"reasoning_content", "reasoning", "reasoning_text"} {
			s, ok := delta[key].(string)
			if !ok || s == "" {
				continue
			}
			trimmed := trimDuplicateDelta(&asm.sentReasoning, s)
			if trimmed != s {
				changed = true
				if trimmed == "" {
					delete(delta, key)
					log.Printf("chat sse_dup_delta_drop kind=reasoning id=%s bytes=%d", asm.id, len(s))
				} else {
					delta[key] = trimmed
					log.Printf("chat sse_dup_delta_trim kind=reasoning id=%s dropped=%d kept=%d", asm.id, len(s)-len(trimmed), len(trimmed))
				}
			}
			if trimmed != "" {
				asm.sentReasoning.WriteString(trimmed)
				hasRemainingWork = true
			}
		}
	}
	if !changed {
		return payload, false
	}
	if !hasRemainingWork {
		return nil, true
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return payload, false
	}
	return b, true
}

func stripPlaceholderThinkPayload(payload []byte) ([]byte, bool) {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil || obj == nil {
		return payload, false
	}
	changed := false
	clearThink := func(m map[string]any) {
		if m == nil {
			return
		}
		for _, k := range []string{"reasoning_content", "reasoning", "reasoning_text", "thinking"} {
			if s, ok := m[k].(string); ok && (isPlaceholderThink(s) || mostlyPlaceholderThink(s)) {
				delete(m, k)
				changed = true
			}
		}
	}
	clearThink(obj)
	if choices, ok := obj["choices"].([]any); ok {
		for _, raw := range choices {
			ch, _ := raw.(map[string]any)
			if ch == nil {
				continue
			}
			if delta, _ := ch["delta"].(map[string]any); delta != nil {
				clearThink(delta)
			}
			if msg, _ := ch["message"].(map[string]any); msg != nil {
				clearThink(msg)
			}
		}
	}
	out := payload
	if changed {
		if b, err := json.Marshal(obj); err == nil {
			out = b
		}
	}
	return out, !payloadHasRealWork(out)
}

func payloadHasRealWork(payload []byte) bool {
	if payloadHasVisibleDelta(string(payload)) {
		return true
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return false
	}
	choices, _ := obj["choices"].([]any)
	for _, raw := range choices {
		ch, _ := raw.(map[string]any)
		if ch == nil {
			continue
		}
		delta, _ := ch["delta"].(map[string]any)
		if delta != nil && (delta["tool_calls"] != nil || delta["function_call"] != nil) {
			return true
		}
		if msg, _ := ch["message"].(map[string]any); msg != nil {
			if msg["tool_calls"] != nil || msg["function_call"] != nil {
				return true
			}
			if visibleText(msg["content"]) != "" {
				return true
			}
		}
	}
	return false
}

// startSSEKeepalive opens text/event-stream (status 200) and pings the client
// during lease wait and upstream TTFT. stop before writing real SSE data or an error event.
// onFail runs if a keepalive write fails (client gone) so the caller can abort Huawei.
func startSSEKeepalive(w http.ResponseWriter, pin *ssePin, onFail func()) (stop func()) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	// ExtendWriteDeadline: a stalled client must not wedge the keepalive write
	// and block stop()'s wg.Wait() on the request's critical path.
	rc := http.NewResponseController(w)
	if pin != nil && pin.ID == "" {
		pin.ID = "chatcmpl-proxy-wait"
	}
	var writeMu sync.Mutex
	stopCh := make(chan struct{})
	var stopOnce, failOnce sync.Once
	var wg sync.WaitGroup
	stopped := func() bool {
		select {
		case <-stopCh:
			return true
		default:
			return false
		}
	}
	signalFail := func() {
		if stopped() {
			return
		}
		failOnce.Do(func() {
			if onFail != nil {
				onFail()
			}
		})
	}
	writeKA := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if stopped() {
			return nil
		}
		_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		// First write: emit the protocol envelope (message_start / response.created)
		// before the first keepalive ping, so Claude/Responses clients see a
		// well-formed stream during the TTFT wait — not just bare pings.
		if pin != nil && !pin.StartSent && !pin.SkipEnvelope {
			if err := writeProtoEnvelope(w, flusher, pin); err != nil {
				return err
			}
		}
		return writeProtoKeepalive(w, flusher, pin)
	}
	if err := writeKA(); err != nil {
		signalFail()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
				if stopped() {
					return
				}
				if err := writeKA(); err != nil {
					signalFail()
					return
				}
			}
		}
	}()
	return func() {
		stopOnce.Do(func() { close(stopCh) })
		wg.Wait()
	}
}

func writeSSEStopDone(w http.ResponseWriter, pin *ssePin) {
	// one-api StreamHandler: if the stream is already open, only append [DONE].
	// A fake finish=stop after tool_calls makes OpenCode abort the agent.
	// Send a finish_reason=stop chunk before [DONE] so clients don't see silent truncation.
	if pin != nil && pin.Proto == protoClaude {
		// Claude protocol: send message_delta + message_stop, NOT OpenAI chunks.
		flusher, _ := w.(http.Flusher)
		delta, _ := json.Marshal(map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   "end_turn",
				"stop_sequence": nil,
			},
		})
		_, _ = io.WriteString(w, "event: message_delta\ndata: "+string(delta)+"\n\n")
		stop, _ := json.Marshal(map[string]any{"type": "message_stop"})
		_, _ = io.WriteString(w, "event: message_stop\ndata: "+string(stop)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	if pin != nil && pin.Proto == protoResponses {
		// Responses API: send response.completed.
		flusher, _ := w.(http.Flusher)
		b, _ := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     pin.ID,
				"object": "response",
				"status": "completed",
				"model":  pin.Model,
			},
		})
		_, _ = io.WriteString(w, "event: response.completed\ndata: "+string(b)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	if pin != nil && pin.ID != "" {
		finishChunk, _ := json.Marshal(map[string]any{
			"id":      pin.ID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   pin.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}},
		})
		_, _ = io.WriteString(w, "data: "+string(finishChunk)+"\n\n")
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// writeSSEThinkLoopExhausted terminates an ALREADY-COMMITTED stream after a
// think-stall (errSSEThinkLoop) on the streaming path, where retrying is not an
// option (a retry re-opens a second thinking block on a stream that already has
// one open — dangling and unclosed — corrupting the protocol). Unlike
// writeSSEStopDone, which slaps message_stop on top of the still-open thinking
// block and emits ZERO visible content (output_tokens=0), this:
//   - closes the thinking block the failed attempt left open at index 0, and
//   - emits a visible text block explaining the stall.
//
// The result is a well-formed, NON-EMPTY terminal, so Claude Code accepts the
// turn instead of seeing an empty/malformed response and entering its
// "check your network / will retry" auto-retry loop (the same output_tokens=0
// failure think_only_guard prevents on the normal finish path — this covers the
// abort path that guard never reaches).
func writeSSEThinkLoopExhausted(w http.ResponseWriter, pin *ssePin) {
	flusher, _ := w.(http.Flusher)
	writeEv := func(event, data string) {
		_, _ = io.WriteString(w, "event: "+event+"\ndata: "+data+"\n\n")
	}
	// The prior notice text ("kept generating internal reasoning...") was a
	// leaked internal diagnostic that the model didn't produce — fabricated
	// gateway text pretending to be assistant output. Emit a short neutral
	// marker instead: still non-empty (Claude Code auto-retries on an empty
	// terminal, so we can't drop the text entirely), but plainly not model-
	// authored, so the user isn't misled into thinking the model gave up.
	const notice = "…"
	if pin != nil && pin.Proto == protoClaude {
		// Think-loop detection fires ONLY on a reasoning-only stream, and the
		// runtime path that leads here always has an open index-0 thinking
		// block (writeSSEReasoningPing / think-loop watchdog only arms after
		// the first reasoning delta, which opens index 0). Always close it —
		// then advance to text at index 1. ThinkOpen mirrors that state so a
		// future callsite that closes thinking earlier can suppress the stop
		// by setting ThinkOpen=false first; today no such path exists but the
		// state field is kept for that hook.
		writeEv("content_block_stop", `{"type":"content_block_stop","index":0}`)
		pin.ThinkOpen = false
		textIdx := 1
		start, _ := json.Marshal(map[string]any{
			"type":          "content_block_start",
			"index":         textIdx,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		writeEv("content_block_start", string(start))
		delta, _ := json.Marshal(map[string]any{
			"type":  "content_block_delta",
			"index": textIdx,
			"delta": map[string]any{"type": "text_delta", "text": notice},
		})
		writeEv("content_block_delta", string(delta))
		writeEv("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, textIdx))
		md, _ := json.Marshal(map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 1},
		})
		writeEv("message_delta", string(md))
		writeEv("message_stop", `{"type":"message_stop"}`)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	if pin != nil && pin.Proto == protoResponses {
		// Responses: emit a visible text item so output isn't empty, then complete.
		item, _ := json.Marshal(map[string]any{
			"type":         "response.output_item.done",
			"output_index": 0,
			"item": map[string]any{
				"type":    "message",
				"role":    "assistant",
				"status":  "completed",
				"content": []any{map[string]any{"type": "output_text", "text": notice}},
			},
		})
		writeEv("response.output_item.done", string(item))
		done, _ := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": pin.ID, "object": "response", "status": "completed", "model": pin.Model,
			},
		})
		writeEv("response.completed", string(done))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	// chat: a plain finish chunk carrying the notice text.
	if pin != nil && pin.ID != "" {
		chunk, _ := json.Marshal(map[string]any{
			"id": pin.ID, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": pin.Model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": notice}, "finish_reason": "stop",
			}},
		})
		_, _ = io.WriteString(w, "data: "+string(chunk)+"\n\n")
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// relayNormalizedSSE forwards upstream SSE to the client while assembling JSON for cache.
// SSE comment keepalives prevent OpenCode from looking frozen during long thinking gaps.
// headersAlreadySent: caller already opened the SSE response (TTFT keepalive).
type ssePin struct {
	ID, Model string
	Proto     clientProto
	// StartSent is set when the protocol envelope (message_start / response.created)
	// has already been emitted by the TTFT keepalive, so ensureClaudeStart /
	// ensureResponsesStart skip re-sending it when the first real delta arrives.
	StartSent bool
	// SkipEnvelope is the EARLY_ENVELOPE=0 rollback: keepalive emits bare pings only.
	SkipEnvelope bool
	// InputTokens is an estimate of the prompt token count, computed from the raw
	// request body before generation starts. Huawei only reports the exact
	// prompt_tokens in the trailing usage frame (which lands in message_delta),
	// so message_start would otherwise carry input_tokens:0 and Anthropic-format
	// clients (Claude Code reads the prompt/context size from message_start) show
	// a 0 token count. The exact count still overwrites this in message_delta.
	InputTokens int64
	// ThinkOpen mirrors whether an index-0 thinking block is currently open on
	// this stream. writeSSEThinkLoopExhausted uses it to avoid emitting a
	// content_block_stop for a block that was never opened (edge: retry that
	// already saw thinking close before the abort).
	ThinkOpen bool
}

func rewriteSSEIdentity(payload, id, model string) string {
	if payload == "[DONE]" || (id == "" && model == "") {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil || obj == nil {
		return payload
	}
	if id != "" {
		obj["id"] = id
	}
	if model != "" && obj["model"] == nil {
		obj["model"] = model
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return string(b)
}

func relayNormalizedSSEOpt(ctx context.Context, w http.ResponseWriter, src io.Reader, headersAlreadySent bool, pin *ssePin) ([]byte, float64, error) {
	if pin == nil {
		pin = &ssePin{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	flusher, _ := w.(http.Flusher)
	// ExtendWriteDeadline (new-api pattern): push the write deadline before each
	// write so a slow/stalled client does not kill the keepalive goroutine and
	// wedge wg.Wait(). http.NewResponseController is nil-safe on net/http.
	rc := http.NewResponseController(w)
	extendWrite := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	}
	var writeMu sync.Mutex
	stopKA := make(chan struct{})
	defer close(stopKA)
	writeKA := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if stopped := func() bool {
			select {
			case <-stopKA:
				return true
			default:
				return false
			}
		}(); stopped {
			return nil
		}
		extendWrite()
		// Protocol-aware keepalive: Claude needs ping events, not SSE comments.
		err := writeProtoKeepalive(w, flusher, pin)
		if err != nil {
			cancel()
		}
		return err
	}
	if !headersAlreadySent {
		if pin != nil && !pin.StartSent && !pin.SkipEnvelope {
			extendWrite()
			_ = writeProtoEnvelope(w, flusher, pin)
		}
		_ = writeKA()
	}
	go func() {
		t := time.NewTicker(3 * time.Second) // was 5s — 3s keeps clients from timing out during thinking gaps and reduces perceived latency
		defer t.Stop()
		for {
			select {
			case <-stopKA:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := writeKA(); err != nil {
					return
				}
			}
		}
	}()
	stream := newProtoStream(w, pin)
	emit := func(payload string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		// Push the write deadline before the data write too, not just keepalive.
		// Otherwise a stalled keepalive goroutine leaves a stale 30s deadline that
		// silently kills a stream that main.go otherwise runs with no WriteTimeout.
		extendWrite()
		if payload != "[DONE]" {
			if id, model := peekSSEIdentity(payload); id != "" || model != "" {
				if pin.ID == "" && id != "" {
					pin.ID = id
				}
				if pin.Model == "" && model != "" {
					pin.Model = model
				}
			}
			payload = rewriteSSEIdentity(payload, pin.ID, pin.Model)
		}
		if err := stream.emit(payload); err != nil {
			cancel()
			return err
		}
		return nil
	}
	asm, err := consumeSSE(ctx, src, emit)
	if err != nil {
		return nil, 0, wireSSEErr(asm, err)
	}
	return asm.completionJSON()
}

var errSSEIdleTimeout = errors.New("upstream sse idle timeout")
var errSSEUpstreamCut = errors.New("upstream sse cut mid-think")
var errSSEPartialAbort = errors.New("upstream aborted after partial stream")
var errSSEThinkLoop = errors.New("upstream thinking loop")
var errSSERepetitionLoop = errors.New("model repetition loop detected")
var errSSEToolFlood = errors.New("model tool call flood detected")

// maxToolCallsPerTurn caps how many tool calls a single model turn can produce.
// GLM-5.2 degenerates into generating 100-200+ near-identical Bash/Read calls
// in a loop, burning 30K+ tokens over 12+ minutes. Claude Code normally uses
// 1-10 tool calls per turn. 20 is generous enough for legitimate parallel
// tool use while stopping runaway generation early.
const maxToolCallsPerTurn = 20

// reasoningOnlyTimeout bounds how long GLM can emit reasoning_content with
// ZERO visible content/tool_calls before the gateway treats the stream as a
// genuine think-stall (errSSEThinkLoop). This is a LAST-RESORT time backstop,
// NOT the primary loop detector — real degenerate loops repeat text and are
// caught much earlier and more precisely by the repetition guard (repGuard).
// The pure-time cutoff only sees NON-repeating reasoning, which on heavy
// Claude Code contexts is overwhelmingly LEGITIMATE deep thinking, not a loop.
//
// Was 90s → 240s → 900s. 240s clipped legitimate deep-thinking turns on
// heavy contexts (users configuring CLAUDE_STREAM_IDLE_TIMEOUT_MS=307200 =
// 5min proves the client is willing to wait far longer than 4min); the
// 240s wall was ours, not the client's. repGuard already catches repeating
// degenerate loops precisely — the pure-time backstop only needs to bound
// truly stalled streams that keep emitting non-repeating reasoning forever,
// which 15min covers with room to spare. Overridable via THINK_LOOP_TIMEOUT.
var reasoningOnlyTimeout = 900 * time.Second

func reasoningOnlyTimeoutValue() time.Duration {
	if v := strings.TrimSpace(os.Getenv("THINK_LOOP_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return reasoningOnlyTimeout
}

var errSSEWired = errors.New("tokens already on the wire")
var errHuaweiClient = errors.New("huawei client error")

// errSSEEarlyModelErr is errHuaweiClient's retryable twin: same trigger
// (parseHuaweiModelErr), but only returned while firstByteAt is still zero
// AND the caller opted into a budget via withEarlyModelErrBudget. Nothing
// has reached the client at that point, so the caller may rotate accounts
// and retry instead of surfacing a terminal error. Callers that never call
// withEarlyModelErrBudget see unchanged behavior — dispatch falls straight
// through to errHuaweiClient, exactly as before this existed.
var errSSEEarlyModelErr = errors.New("huawei model error before first byte")

func (a *sseAssembly) clientWired() bool {
	return a != nil && a.emittedVisible
}

func wireSSEErr(asm *sseAssembly, err error) error {
	if err == nil || !asm.clientWired() {
		return err
	}
	// errSSEThinkLoop fires with content=0 and toolCalls=0 (checked at the
	// call site in consumeSSE) — only reasoning_content was ever streamed.
	// payloadHasVisibleDelta() treats reasoning_content as "visible" so
	// emittedVisible/clientWired() is already true here, but re-generating
	// costs nothing a real answer wasn't already going to cost: no answer
	// text or tool call has been sent to overwrite. Do not let it get
	// promoted to errSSEWired, or the retry never fires and this degrades
	// back into the exact silent hang errSSEThinkLoop exists to prevent.
	if errors.Is(err, errSSEThinkLoop) {
		return err
	}
	return fmt.Errorf("%w: %w", errSSEWired, err)
}

// sseUpstreamIdle is the max gap between upstream bytes on a ready account.
// Was 180s — too tight for multi-hour tasks with long thinking gaps (new-api
// uses no idle cap, sub2api relies on upstream flow). 600s survives GLM/DeepSeek
// extended reasoning + tool-execution silence. Overridable via SSE_IDLE_TIMEOUT.
var sseUpstreamIdle = 600 * time.Second

// sseUpstreamIdleDepleted: 透支号限速后思考空窗更长。
var sseUpstreamIdleDepleted = 900 * time.Second

type sseIdleCtxKey struct{}

func withSSEIdle(ctx context.Context, d time.Duration) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, sseIdleCtxKey{}, d)
}

func sseIdleFrom(ctx context.Context) time.Duration {
	if ctx != nil {
		if d, ok := ctx.Value(sseIdleCtxKey{}).(time.Duration); ok && d > 0 {
			return d
		}
	}
	if v := strings.TrimSpace(os.Getenv("SSE_IDLE_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return sseUpstreamIdle
}

type earlyModelErrBudgetCtxKey struct{}

// withEarlyModelErrBudget caps how many times consumeSSE may surface a
// pre-first-byte Huawei inline model error as the retryable
// errSSEEarlyModelErr instead of the terminal errHuaweiClient. n is the
// number of retries still allowed for the current request across the whole
// account-rotation loop, decremented by the caller (api.go) between
// attempts — consumeSSE only reads it, it never decrements it itself.
// Unset (zero value, ctx never wrapped) means "no budget", identical to
// today's behavior.
func withEarlyModelErrBudget(ctx context.Context, n int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, earlyModelErrBudgetCtxKey{}, n)
}

func earlyModelErrBudgetFrom(ctx context.Context) int {
	if ctx != nil {
		if n, ok := ctx.Value(earlyModelErrBudgetCtxKey{}).(int); ok {
			return n
		}
	}
	return 0
}

// parseHuaweiModelErr detects InferHub/ModelArts inline errors such as
// {"text":"[DONE]","error_code":"InferHub.ModelArts.81027.400","error_msg":"Prompt length exceeds..."}.
// These are not OpenAI chat objects — forwarding them makes OpenCode Zod fail.
func parseHuaweiModelErr(body []byte) (code, msg string, ok bool) {
	if len(body) == 0 || !bytes.Contains(body, []byte("error_code")) {
		return "", "", false
	}
	var hw struct {
		ErrorCode string          `json:"error_code"`
		ErrorMsg  string          `json:"error_msg"`
		Choices   json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(body, &hw) != nil {
		return "", "", false
	}
	if hw.ErrorCode == "" || strings.TrimSpace(hw.ErrorMsg) == "" {
		return "", "", false
	}
	if len(bytes.TrimSpace(hw.Choices)) > 2 {
		return "", "", false
	}
	return hw.ErrorCode, strings.TrimSpace(hw.ErrorMsg), true
}

func looksLikePromptTooLong(body []byte) bool {
	code, msg, ok := parseHuaweiModelErr(body)
	if !ok {
		low := strings.ToLower(string(body))
		return strings.Contains(low, "81027") || strings.Contains(low, "prompt length exceeds")
	}
	return strings.Contains(code, "81027") || strings.Contains(strings.ToLower(msg), "prompt length exceeds")
}

func openaiErrPayload(code, msg string) string {
	if msg == "" {
		msg = "invalid request"
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
	return string(b)
}

func isCancelErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "cancel") && !strings.Contains(low, "unexpected")
}

func isUpstreamBroken(err error) bool {
	if err == nil || isCancelErr(err) {
		return false
	}
	if errors.Is(err, errSSEUpstreamCut) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "unexpected eof") ||
		strings.Contains(low, "connection reset") ||
		strings.Contains(low, "broken pipe") ||
		strings.Contains(low, "server closed idle")
}

func (a *sseAssembly) noteToolCalls(v any) {
	if a == nil || v == nil {
		return
	}
	var arr []any
	switch t := v.(type) {
	case []any:
		arr = t
	case map[string]any:
		if t["function"] != nil || t["id"] != nil || t["name"] != nil {
			arr = []any{t}
		} else {
			for _, raw := range t {
				arr = append(arr, raw)
			}
		}
	default:
		return
	}
	if a.tools == nil {
		a.tools = []map[string]any{}
	}
	for _, raw := range arr {
		tc, _ := raw.(map[string]any)
		if tc == nil {
			continue
		}
		idx := 0
		if f, ok := tc["index"].(float64); ok {
			idx = int(f)
		}
		for len(a.tools) <= idx {
			a.tools = append(a.tools, map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "", "arguments": ""},
			})
		}
		dst := a.tools[idx]
		if id := asString(tc["id"]); id != "" {
			dst["id"] = id
		}
		if typ := asString(tc["type"]); typ != "" {
			dst["type"] = typ
		}
		fn, _ := tc["function"].(map[string]any)
		if fn == nil && asString(tc["name"]) != "" {
			fn = map[string]any{"name": tc["name"], "arguments": tc["arguments"]}
		}
		dfn, _ := dst["function"].(map[string]any)
		if dfn == nil {
			dfn = map[string]any{"name": "", "arguments": ""}
			dst["function"] = dfn
		}
		if fn != nil {
			if n := asString(fn["name"]); n != "" {
				dfn["name"] = n
			}
			if args := asString(fn["arguments"]); args != "" {
				dfn["arguments"] = asString(dfn["arguments"]) + args
			}
		}
	}
	a.toolCalls = len(a.readyTools())
}

func (a *sseAssembly) readyTools() []map[string]any {
	if a == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(a.tools))
	for _, t := range a.tools {
		if toolCallUsable(t) {
			out = append(out, t)
		}
	}
	return out
}

func toolCallUsable(t map[string]any) bool {
	if t == nil {
		return false
	}
	fn, _ := t["function"].(map[string]any)
	if asString(fn["name"]) == "" {
		return false
	}
	args := strings.TrimSpace(asString(fn["arguments"]))
	if args == "" {
		return true
	}
	return json.Valid([]byte(args))
}

func (a *sseAssembly) hasToolCall() bool {
	return a != nil && len(a.readyTools()) > 0
}

func (a *sseAssembly) hasVisible() bool {
	return a != nil && (a.reasoning.Len() > 0 || a.content.Len() > 0 || a.hasToolCall())
}

func (a *sseAssembly) hasAnswer() bool {
	return a != nil && (a.content.Len() > 0 || a.hasToolCall())
}

func (a *sseAssembly) streamComplete() bool {
	if a == nil {
		return false
	}
	// Huawei CLI / one-api: upstream [DONE] or finish_reason ends the turn.
	// Do not invent a client error just because the model only thought.
	return a.hasAnswer() || a.sawDone || a.sawFinish
}

func shouldRetrySSECut(asm *sseAssembly, unexpected bool) bool {
	if asm != nil && (asm.sawDone || asm.sawFinish || asm.hasAnswer() || asm.hasVisible()) {
		return false
	}
	// Placeholder-only EOF is incomplete think, not a transport cut.
	if asm != nil && asm.placeholderN > 0 && !unexpected {
		return false
	}
	return unexpected || asm == nil || !asm.hasAnswer()
}

type idleTimeoutReader struct {
	r    io.Reader
	idle time.Duration
}

func newIdleTimeoutReader(r io.Reader, idle time.Duration) *idleTimeoutReader {
	if idle <= 0 {
		idle = 2 * time.Hour
	}
	return &idleTimeoutReader{r: r, idle: idle}
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	// Prefer deadline on the underlying conn — no per-Read goroutine tax.
	if d, ok := r.r.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(time.Now().Add(r.idle))
		n, err := r.r.Read(p)
		if n > 0 {
			_ = d.SetReadDeadline(time.Time{})
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return n, errSSEIdleTimeout
			}
		}
		return n, err
	}
	// Fallback: plain read (client cancel / upstream EOF still terminate).
	return r.r.Read(p)
}

func consumeSSE(ctx context.Context, src io.Reader, emit func(payload string) error) (*sseAssembly, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c, ok := src.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = c.Close() })
		defer stop()
	}
	br := bufio.NewReaderSize(newIdleTimeoutReader(src, sseIdleFrom(ctx)), 64<<10)
	asm := &sseAssembly{}
	repGuard := newRepetitionGuard()
	var firstByteAt time.Time
	dispatch := func(payload []byte) error {
		if len(payload) == 0 {
			return nil
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			asm.splitThinkTags()
			asm.rescueToolsFromText()
			asm.sawDone = true
			return nil
		}
		if code, msg, ok := parseHuaweiModelErr(payload); ok {
			// Nothing real has been ingested yet (firstByteAt still zero) — safe
			// to rotate accounts instead of surfacing a terminal error, as long
			// as the caller left retries in the budget. Applies identically to
			// streaming (emit != nil) and non-streaming (emit == nil) callers:
			// in both cases no bytes have reached the client at this point.
			if firstByteAt.IsZero() && earlyModelErrBudgetFrom(ctx) > 0 {
				return fmt.Errorf("%w: %s (code=%s)", errSSEEarlyModelErr, msg, code)
			}
			if emit != nil {
				if err := emit(openaiErrPayload(code, msg)); err != nil {
					return err
				}
				if err := emit("[DONE]"); err != nil {
					return err
				}
			}
			return fmt.Errorf("%w: %s", errHuaweiClient, msg)
		}
		out := coerceOpenAIChunk(payload, asm)
		if len(out) == 0 {
			log.Printf("chat sse skip non-openai frame bytes=%d", len(payload))
			return nil
		}
		if firstByteAt.IsZero() {
			firstByteAt = time.Now()
		}
		asm.ingestPayload(out)
		if repGuard.check(asm) {
			log.Printf("chat sse_repetition_loop id=%s kind=%s period=%d reps=%d content=%d reasoning=%d",
				asm.id, repGuard.TriggerKind, repGuard.TriggerPeriod, repGuard.TriggerReps,
				asm.content.Len(), asm.reasoning.Len())
			return errSSERepetitionLoop
		}
		if asm.toolCalls > maxToolCallsPerTurn {
			log.Printf("chat sse_tool_flood id=%s tools=%d limit=%d reasoning=%d content=%d",
				asm.id, asm.toolCalls, maxToolCallsPerTurn, asm.reasoning.Len(), asm.content.Len())
			return errSSEToolFlood
		}
		if asm.content.Len() == 0 && asm.toolCalls == 0 && asm.reasoning.Len() > 0 &&
			time.Since(firstByteAt) > reasoningOnlyTimeoutValue() {
			log.Printf("chat sse_think_loop id=%s elapsed=%s reasoning=%d",
				asm.id, time.Since(firstByteAt).Round(time.Second), asm.reasoning.Len())
			return errSSEThinkLoop
		}
		if asm.thinkStuck() {
			log.Printf("chat sse_think_loop_skip id=%s n=%d", asm.id, asm.placeholderN)
		}
		if emit != nil {
			// Drop a SECOND generation's visible content that arrives AFTER a
			// terminal boundary (finish_reason or [DONE]) was already emitted.
			// Huawei/GLM occasionally concatenates multiple generations into ONE
			// response body; without this guard consumeSSE faithfully relays all
			// of them into the single client SSE connection and the client renders
			// the same answer/thinking 2-6x (emittedVisible is already true, so no
			// gateway retry fires — invisible to retry-signature log analysis).
			// Usage-only trailing frames carry no visible delta and still pass, so
			// deferred token counts are unaffected. Keyed on sawFinish (set in
			// ingestPayload the moment a finish_reason is seen) rather than
			// emittedFinish, because GLM sends finish in an empty-delta frame that
			// is stripped as "no real work" before emittedFinish would be set. A
			// content_filter recovery resets sawFinish=false below, so legitimate
			// post-filter content is never dropped.
			if (asm.sawFinish || asm.sawDone) && (payloadHasVisibleDelta(string(out)) || payloadHasToolDelta(out)) {
				log.Printf("chat sse_drop_post_finish id=%s bytes=%d reasoning=%d content=%d", asm.id, len(out), asm.reasoning.Len(), asm.content.Len())
				return nil
			}
			if rewritten, ok := sanitizeFilterFinish(out); ok {
				out = rewritten
				asm.sawFilter = true
				asm.sawFinish = false
				log.Printf("chat content_filter_stripped finish=%q", asm.lastFinish)
			}
			out, skip := stripPlaceholderThinkPayload(out)
			if skip && !bytes.Contains(out, []byte(`"usage"`)) {
				return nil
			}
			if deduped, changed := dedupOutgoingPayload(out, asm); changed {
				if len(deduped) == 0 {
					return nil
				}
				out = deduped
			}
			// A trailing usage-only frame has no visible "work" but must still be
			// forwarded so the protocol emitter (Claude/Responses) can attach token
			// counts to its terminal event — otherwise input/output_tokens are 0.
			if payloadHasToolDelta(out) {
				asm.emittedTools = true
			}
			if payloadHasFinishReason(out) {
				asm.emittedFinish = true
			}
			if err := emit(string(out)); err != nil {
				return err
			}
			if payloadHasVisibleDelta(string(out)) || payloadHasToolDelta(out) {
				asm.emittedVisible = true
			}
			return nil
		}
		return nil
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if errors.Is(err, io.ErrUnexpectedEOF) || err == io.EOF {
				if salvaged := salvageSSEPayload(line); len(salvaged) > 0 {
					if de := dispatch(salvaged); de != nil {
						if errors.Is(de, errHuaweiClient) || errors.Is(de, errSSEThinkLoop) || errors.Is(de, errSSEEarlyModelErr) {
							return asm, de
						}
						if errors.Is(de, errSSERepetitionLoop) || errors.Is(de, errSSEToolFlood) {
							asm.sawFinish = true
							asm.lastFinish = "stop"
							asm.sawDone = true
							break
						}
						return nil, de
					}
				} else {
					log.Printf("chat sse drop incomplete frame bytes=%d", len(line))
				}
			} else if payload, ok := sseDataPayload(line); ok {
				if de := dispatch(payload); de != nil {
					if errors.Is(de, errHuaweiClient) || errors.Is(de, errSSEThinkLoop) || errors.Is(de, errSSEEarlyModelErr) {
						return asm, de
					}
					if errors.Is(de, errSSERepetitionLoop) || errors.Is(de, errSSEToolFlood) {
						asm.sawFinish = true
						asm.lastFinish = "stop"
						asm.sawDone = true
						break
					}
					return nil, de
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil || isCancelErr(err) {
				log.Printf("chat sse_abort client_gone id=%s reasoning=%d content=%d err=%v", asm.id, asm.reasoning.Len(), asm.content.Len(), err)
				if ctx.Err() != nil {
					return asm, ctx.Err()
				}
				return asm, err
			}
			if errors.Is(err, errSSEIdleTimeout) {
				log.Printf("chat sse_idle id=%s reasoning=%d content=%d", asm.id, asm.reasoning.Len(), asm.content.Len())
				return asm, errSSEIdleTimeout
			}
			unexpected := errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(strings.ToLower(err.Error()), "unexpected eof")
			if err == io.EOF || unexpected {
				if shouldRetrySSECut(asm, unexpected) {
					log.Printf("chat sse_cut err=%v id=%s reasoning=%d content=%d", err, asm.id, asm.reasoning.Len(), asm.content.Len())
					return asm, errSSEUpstreamCut
				}
				if !asm.streamComplete() {
					log.Printf("chat sse_partial_abort id=%s reasoning=%d content=%d done=%v finish=%q", asm.id, asm.reasoning.Len(), asm.content.Len(), asm.sawDone, asm.lastFinish)
					return asm, errSSEPartialAbort
				}
				break
			}
			if shouldRetrySSECut(asm, true) {
				log.Printf("chat sse_cut err=%v id=%s reasoning=%d content=%d", err, asm.id, asm.reasoning.Len(), asm.content.Len())
				return asm, errSSEUpstreamCut
			}
			if !asm.streamComplete() {
				return asm, errSSEPartialAbort
			}
			return asm, err
		}
	}

	// If upstream finished with content_filter but already sent text (e.g. Huawei
	// "involves sensitive information"), keep that text — never invent a pad.
	// Only when content is empty, promote upstream error/refusal text into a
	// normal assistant delta so the client sees the original message.
	if emit != nil && asm.sawFilter && asm.content.Len() == 0 {
		text := strings.TrimSpace(asm.filterMsg)
		if text != "" {
			pad := map[string]any{
				"id":      asm.id,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   asm.model,
				"choices": []any{
					map[string]any{
						"index": 0,
						"delta": map[string]any{
							"role":    "assistant",
							"content": text,
						},
						"finish_reason": nil,
					},
				},
			}
			if pad["id"] == "" {
				pad["id"] = "chatcmpl-upstream-safe"
			}
			if pad["model"] == "" {
				pad["model"] = "unknown"
			}
			b, _ := json.Marshal(pad)
			asm.appendContent(text)
			log.Printf("chat upstream_safe_passthrough bytes=%d", len(text))
			if err := emit(string(b)); err != nil {
				return nil, err
			}
		}
	}

	asm.splitThinkTags()
	asm.rescueToolsFromText()

	if emit != nil && !asm.streamComplete() {
		log.Printf("chat sse_incomplete_stop id=%s reasoning=%d content=%d done=%v finish=%q", asm.id, asm.reasoning.Len(), asm.content.Len(), asm.sawDone, asm.lastFinish)
		return asm, errSSEPartialAbort
	}

	if emit != nil {
		if ready := asm.readyTools(); len(ready) > 0 && !asm.emittedTools {
			tools := make([]any, len(ready))
			for i, t := range ready {
				item := map[string]any{}
				for k, v := range t {
					item[k] = v
				}
				item["index"] = i
				tools[i] = item
			}
			chunk := map[string]any{
				"id":      asm.id,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   asm.model,
				"choices": []any{
					map[string]any{
						"index": 0,
						"delta": map[string]any{
							"tool_calls": tools,
						},
						"finish_reason": nil,
					},
				},
			}
			if chunk["id"] == "" {
				chunk["id"] = "chatcmpl-proxy-tools"
			}
			if chunk["model"] == "" {
				chunk["model"] = "unknown"
			}
			b, _ := json.Marshal(chunk)
			if err := emit(string(b)); err != nil {
				return nil, err
			}
			asm.emittedTools = true
		}
		if !asm.emittedFinish {
			reason := asm.lastFinish
			if asm.hasToolCall() {
				reason = "tool_calls"
			} else if reason == "" {
				reason = "stop"
			}
			stop := map[string]any{
				"id":      asm.id,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   asm.model,
				"choices": []any{
					map[string]any{
						"index":         0,
						"delta":         map[string]any{},
						"finish_reason": reason,
					},
				},
			}
			if stop["id"] == "" {
				stop["id"] = "chatcmpl-proxy-stop"
			}
			if stop["model"] == "" {
				stop["model"] = "unknown"
			}
			b, _ := json.Marshal(stop)
			if err := emit(string(b)); err != nil {
				return nil, err
			}
			asm.emittedFinish = true
		}
		if err := emit("[DONE]"); err != nil {
			return nil, err
		}
	}
	return asm, nil
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

// serveCachedAsProtoSSE replays a cached OpenAI completion as a streaming
// response, translated to the client's protocol by protoStream.
func serveCachedAsProtoSSE(w http.ResponseWriter, body []byte, pin *ssePin) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return
	}
	id, _ := obj["id"].(string)
	model, _ := obj["model"].(string)
	created := time.Now().Unix()
	if c, ok := obj["created"].(float64); ok && c > 0 {
		created = int64(c)
	}
	if pin.ID == "" && id != "" {
		pin.ID = id
	}
	if pin.Model == "" && model != "" {
		pin.Model = model
	}

	content := ""
	reasoning := ""
	var toolCalls []any
	finishReason := "stop"
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		if ch, _ := choices[0].(map[string]any); ch != nil {
			if msg, _ := ch["message"].(map[string]any); msg != nil {
				content, _ = msg["content"].(string)
				reasoning, _ = msg["reasoning_content"].(string)
				toolCalls, _ = msg["tool_calls"].([]any)
			}
			if fr, _ := ch["finish_reason"].(string); fr != "" {
				finishReason = fr
			}
		}
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Cache", "HIT")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	stream := newProtoStream(w, pin)
	mkChunk := func(delta map[string]any, finish string) string {
		ch := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			ch["finish_reason"] = finish
		} else {
			ch["finish_reason"] = nil
		}
		c := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{ch},
		}
		b, _ := json.Marshal(c)
		return string(b)
	}

	_ = stream.emit(mkChunk(map[string]any{"role": "assistant"}, ""))

	if reasoning != "" {
		_ = stream.emit(mkChunk(map[string]any{"reasoning_content": reasoning}, ""))
	}
	if content != "" {
		_ = stream.emit(mkChunk(map[string]any{"content": content}, ""))
	}
	if len(toolCalls) > 0 {
		indexed := make([]any, len(toolCalls))
		for i, tc := range toolCalls {
			m, _ := tc.(map[string]any)
			if m == nil {
				continue
			}
			item := map[string]any{}
			for k, v := range m {
				item[k] = v
			}
			item["index"] = i
			indexed[i] = item
		}
		_ = stream.emit(mkChunk(map[string]any{"tool_calls": indexed}, ""))
	}

	usageChunk := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}},
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		usageChunk["usage"] = u
	}
	ub, _ := json.Marshal(usageChunk)
	_ = stream.emit(string(ub))
	_ = stream.emit("[DONE]")
}

func storeCachedChatResponse(s *Server, cacheKey, cacheModel string, status int, body []byte, usage float64) {
	if s.respCache == nil || len(body) == 0 {
		return
	}
	model := cacheModel
	if model == "" {
		model = peekModel(body)
	}
	s.respCache.Put(cacheKey, model, status, body, usage)
}
