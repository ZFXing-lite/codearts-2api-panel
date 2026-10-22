package api

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Official InferHub / CodeArts CLI chat fields. Anything else is dropped
// after Responses/Anthropic-style input is folded into messages.
var cliChatAllow = map[string]bool{
	"model":                 true,
	"messages":              true,
	"stream":                true,
	"stream_options":        true,
	"tools":                 true,
	"tool_choice":           true,
	"max_tokens":            true,
	"max_completion_tokens": true,
	"temperature":           true,
	"top_p":                 true,
	"top_k":                 true,
	"stop":                  true,
	"n":                     true,
	"presence_penalty":      true,
	"frequency_penalty":     true,
	"thinking":              true,
	"tool_stream":           true,
	"reasoning_effort":      true,
	"user":                  true,
}

// modelAliasMap maps client model names to Huawei InferHub model IDs.
// Claude Code sends Anthropic model names; OpenCode sends OpenAI names.
// Huawei accounts have GLM-5.2, GLM-5.1, DeepSeek-V3, DeepSeek-R1, etc.
var modelAliasMap = map[string]string{
	// Anthropic Claude → GLM-5.2 (best available, supports thinking + tools)
	"claude-sonnet-4-20250514":   "GLM-5.2",
	"claude-sonnet-4":            "GLM-5.2",
	"claude-3-5-sonnet-20241022": "GLM-5.2",
	"claude-3-5-sonnet-latest":   "GLM-5.2",
	"claude-3-sonnet-20240229":   "GLM-5.2",
	"claude-3-5-haiku-20241022":  "GLM-5.1",
	"claude-3-haiku-20240307":    "GLM-5.1",
	"claude-3-opus-20240229":     "GLM-5.2",
	"claude-3-opus-latest":       "GLM-5.2",
	"claude-opus-4-20250514":     "GLM-5.2",
	"claude-opus-4":              "GLM-5.2",
	// OpenAI → Huawei equivalents
	"gpt-4o":        "GLM-5.2",
	"gpt-4o-mini":   "GLM-5.1",
	"gpt-4-turbo":   "GLM-5.2",
	"gpt-4":         "GLM-5.2",
	"gpt-3.5-turbo": "GLM-5.1",
	"o1":            "DeepSeek-R1",
	"o1-mini":       "DeepSeek-R1",
	"o1-preview":    "DeepSeek-R1",
	"o3":            "DeepSeek-R1",
	"o3-mini":       "DeepSeek-R1",
	// DeepSeek passthrough
	"deepseek-chat":     "DeepSeek-V3",
	"deepseek-reasoner": "DeepSeek-R1",
	// GLM passthrough (already correct)
	"glm-4":      "GLM-5.1",
	"glm-4-plus": "GLM-5.2",
}

// defaultHuaweiModel is used when the requested model is not in the alias map
// and doesn't look like a valid Huawei model name.
const defaultHuaweiModel = "GLM-5.2"

// rewriteModel maps client model names (Claude/OpenAI) to Huawei InferHub model IDs.
// If the model is already a valid Huawei name (GLM-*, DeepSeek-*, Qwen*), it passes through.
func rewriteModel(root map[string]any) bool {
	model, _ := root["model"].(string)
	if model == "" {
		root["model"] = defaultHuaweiModel
		return true
	}
	// Strip context-window suffixes like "[1M]", "[200K]", "(long)" that clients add.
	cleaned := stripModelSuffix(model)
	stripped := cleaned != model
	if stripped {
		model = cleaned
		root["model"] = cleaned
	}
	// Already a Huawei model? Pass through.
	if isHuaweiModel(model) {
		return stripped
	}
	// Check alias map (case-insensitive).
	lower := strings.ToLower(model)
	for k, v := range modelAliasMap {
		if strings.ToLower(k) == lower {
			if model != v {
				root["model"] = v
				log.Printf("chat model_rewrite %s -> %s", model, v)
				return true
			}
			return false
		}
	}
	// Unknown model — default to GLM-5.2.
	root["model"] = defaultHuaweiModel
	log.Printf("chat model_rewrite %s -> %s (default)", model, defaultHuaweiModel)
	return true
}

// stripModelSuffix removes context-window indicators like "[1M]", "[200K]", "(long)".
func stripModelSuffix(model string) string {
	for _, sep := range []string{"[", "(", " {"} {
		if i := strings.Index(model, sep); i > 0 {
			return strings.TrimSpace(model[:i])
		}
	}
	return model
}

// isHuaweiModel returns true for known Huawei InferHub model name patterns.
func isHuaweiModel(model string) bool {
	upper := strings.ToUpper(model)
	if strings.HasPrefix(upper, "GLM-") ||
		strings.HasPrefix(upper, "DEEPSEEK-") ||
		strings.HasPrefix(upper, "QWEN") {
		return true
	}
	return false
}

// sanitizeCLIChatBody rewrites any inbound chat body to the official CLI shape:
// Chat Completions with messages. Responses `input` and client extras are not forwarded.
func sanitizeCLIChatBody(body []byte) []byte {
	out, _ := sanitizeCLIChatBodyWithDropped(body, false)
	return out
}

func sanitizeCLIChatBodyOpt(body []byte, skipModelRewrite bool) []byte {
	out, _ := sanitizeCLIChatBodyWithDropped(body, skipModelRewrite)
	return out
}

// sanitizeCLIChatBodyWithDropped 在裁剪白名单之外，把被丢弃的字段名交回调用方。
// 静默丢弃是这里最危险的行为：客户端以为 JSON mode / 可复现 seed 生效了，
// 实际拿到的是自由文本。调用方应把这份列表回显到响应头，让"没生效"可见。
func sanitizeCLIChatBodyWithDropped(body []byte, skipModelRewrite bool) ([]byte, []string) {
	if len(body) == 0 {
		return body, nil
	}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil || root == nil {
		return body, nil
	}
	changed := false
	if !skipModelRewrite && rewriteModel(root) {
		changed = true
	}
	if _, ok := root["messages"]; !ok {
		if msgs := responsesInputToMessages(root); len(msgs) > 0 {
			root["messages"] = msgs
			changed = true
		}
	}
	if flattenClaudeSystem(root) {
		changed = true
	}
	if prependSystem(root, "system") {
		changed = true
	}
	if prependSystem(root, "instructions") {
		changed = true
	}
	if rewriteClaudeTools(root) {
		changed = true
	}
	if rewriteClaudeToolChoice(root) {
		changed = true
	}
	if rewriteStopSequences(root) {
		changed = true
	}
	if rewriteClaudeMessages(root) {
		changed = true
	}
	if normalizeGLMToolTurn(root) {
		changed = true
	}
	if scrubThinkPlaceholders(root) {
		changed = true
	}
	if stripHistoricalReasoning(root) {
		changed = true
	}
	if extractOutputConfig(root) {
		changed = true
	}
	if injectStreamUsage(root) {
		changed = true
	}
	var dropped []string
	for k := range root {
		if !cliChatAllow[k] {
			delete(root, k)
			dropped = append(dropped, k)
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body, nil
	}
	// Only fire the log when the whitelist actually dropped something: field
	// silence is the invisible-mutation the pure-forwarding rule protects
	// against, so make every drop conspicuous. Log the count first so
	// grep-friendly (`body_drop_fields n=`).
	if len(dropped) > 0 {
		sort.Strings(dropped)
		log.Printf("chat body_drop_fields n=%d fields=%v has_messages=%v", len(dropped), dropped, root["messages"] != nil)
	}
	return out, dropped
}

// normalizeGLMToolTurn repairs the official GLM tool+thinking chain.
// Z.AI docs and OpenCode #25758/#24722: assistant tool_calls must carry
// reasoning_content or InferHub hangs until APIG 60s 504.
// Hermes #12758: tool_stream=true keeps first-byte flowing during tool args.
func normalizeGLMToolTurn(root map[string]any) bool {
	if root == nil {
		return false
	}
	changed := false
	if enableToolStream(root) {
		changed = true
	}
	repaired, hasReal := repairAssistantToolReasoning(root)
	if repaired {
		changed = true
	}
	_ = hasReal
	// Force thinking when tools are present or tool_calls exist in history.
	// Z.AI/GLM contract: assistant tool_calls without reasoning_content makes
	// InferHub hang until APIG 60s 504. FORCE_THINKING=0 disables the injection
	// (trades 504 risk for lower latency on pure tool-bearing requests).
	if forceThinkingEnabled() && needsThinking(root) {
		if ensureThinkingEnabled(root) {
			changed = true
		}
	}
	return changed
}

// forceThinkingEnabled reads FORCE_THINKING env (default on). When off, thinking
// is only injected when the client explicitly requests it. Off historically caused
// InferHub 60s 504 on tool_calls without reasoning_content — keep default on.
func forceThinkingEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("FORCE_THINKING")))
	switch v {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// needsThinking returns true when the request involves tool calling,
// which requires reasoning_content to prevent InferHub 60s 504 hang.
func needsThinking(root map[string]any) bool {
	if tools, ok := root["tools"].([]any); ok && len(tools) > 0 {
		return true
	}
	msgs, _ := root["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		if m["tool_calls"] != nil {
			return true
		}
		if strings.EqualFold(asString(m["role"]), "tool") {
			return true
		}
	}
	if th, ok := root["thinking"].(map[string]any); ok && th != nil {
		if strings.EqualFold(asString(th["type"]), "enabled") {
			return true
		}
	}
	return false
}

func ensureThinkingEnabled(root map[string]any) bool {
	th, _ := root["thinking"].(map[string]any)
	if th != nil && strings.EqualFold(asString(th["type"]), "enabled") {
		if v, ok := th["clear_thinking"].(bool); ok && v {
			root["thinking"] = map[string]any{"type": "enabled", "clear_thinking": false}
			return true
		}
		return false
	}
	root["thinking"] = map[string]any{"type": "enabled", "clear_thinking": false}
	return true
}

func scrubThinkPlaceholders(root map[string]any) bool {
	msgs, _ := root["messages"].([]any)
	changed := false
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		for _, k := range []string{"reasoning_content", "reasoning", "reasoning_text"} {
			s := asString(m[k])
			if s == "" {
				continue
			}
			clean := scrubPlaceholderThink(s)
			if clean == s {
				continue
			}
			if clean == "" {
				delete(m, k)
			} else {
				m[k] = clean
			}
			changed = true
		}
	}
	return changed
}

// stripHistoricalReasoning removes reasoning_content from all historical
// assistant messages. Native Huawei CLI / OpenAI clients never replay old
// reasoning in the request — only our Claude→OpenAI conversion does,
// because Claude Code sends every turn's thinking blocks. In long agentic
// sessions this adds 50-200KB of stale reasoning that bloats context and
// dilutes attention on the actual new question. Stripping it matches CLI
// behaviour and significantly reduces context size.
// STRIP_HISTORICAL_REASONING=0 disables.
// stripHistoricalReasoning removes reasoning_content from old assistant
// messages, keeping only the last keepLast turns intact. GLM-5.2 reads all
// historical reasoning as "what I was thinking," which primes it to continue
// old tasks even when a new question is asked. Huawei CLI never sends
// reasoning_content at all — stripping ALL turns aligns our format with CLI.
// STRIP_HISTORICAL_REASONING=0 disables; KEEP_REASONING_TURNS=N overrides
// how many recent assistant messages keep their reasoning (default 0 = strip all,
// matching CLI behavior). The model doesn't need old reasoning — it already has
// the resulting content and tool_calls.
func stripHistoricalReasoning(root map[string]any) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("STRIP_HISTORICAL_REASONING")))
	if v == "0" || v == "false" || v == "off" {
		return false
	}
	msgs, _ := root["messages"].([]any)
	if len(msgs) == 0 {
		return false
	}
	keepLast := 0
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("KEEP_REASONING_TURNS"))); err == nil && n >= 0 {
		keepLast = n
	}
	// Build role-size debug info (first 6 + last 4 messages).
	var roleSizes []string
	for i, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		role := asString(m["role"])
		cLen := 0
		switch c := m["content"].(type) {
		case string:
			cLen = len(c)
		case []any:
			if b, err := json.Marshal(c); err == nil {
				cLen = len(b)
			}
		}
		rcLen := len(asString(m["reasoning_content"]))
		if i < 6 || i >= len(msgs)-4 {
			roleSizes = append(roleSizes, fmt.Sprintf("%s:%d+r%d", role, cLen, rcLen))
		} else if i == 6 {
			roleSizes = append(roleSizes, "...")
		}
	}
	// Collect indices of assistant messages (reverse order for keepLast).
	var assistantIdxs []int
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		if m == nil {
			continue
		}
		if strings.EqualFold(asString(m["role"]), "assistant") {
			assistantIdxs = append(assistantIdxs, i)
		}
	}
	changed := false
	stripped := 0
	var savedBytes int
	withReasoning := 0
	for rank, idx := range assistantIdxs {
		m, _ := msgs[idx].(map[string]any)
		if m == nil {
			continue
		}
		if messageReasoning(m) != "" {
			withReasoning++
		}
		if rank < keepLast {
			continue // preserve recent reasoning
		}
		hasToolCalls := m["tool_calls"] != nil
		for _, k := range []string{"reasoning_content", "reasoning", "reasoning_text"} {
			s := asString(m[k])
			if s == "" {
				continue
			}
			savedBytes += len(s)
			if hasToolCalls && k == "reasoning_content" {
				m[k] = ""
			} else {
				delete(m, k)
			}
			changed = true
			stripped++
		}
	}
	log.Printf("chat strip_reasoning_scan assistants=%d with_reasoning=%d stripped=%d kept=%d saved_bytes=%d msgs=%d roles=[%s]", len(assistantIdxs), withReasoning, stripped, keepLast, savedBytes, len(msgs), strings.Join(roleSizes, " "))
	return changed
}

// injectStreamUsage adds stream_options.include_usage=true to streaming
// requests so the upstream returns token counts in the final SSE frame.
// Without this, Huawei InferHub may omit usage from the stream entirely.
func injectStreamUsage(root map[string]any) bool {
	stream, _ := root["stream"].(bool)
	if !stream {
		return false
	}
	opts, _ := root["stream_options"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	if v, ok := opts["include_usage"].(bool); ok && v {
		return false
	}
	opts["include_usage"] = true
	root["stream_options"] = opts
	return true
}

func enableToolStream(root map[string]any) bool {
	tools, _ := root["tools"].([]any)
	stream, _ := root["stream"].(bool)
	if len(tools) == 0 || !stream {
		return false
	}
	if v, ok := root["tool_stream"].(bool); ok && v {
		return false
	}
	root["tool_stream"] = true
	return true
}

func repairAssistantToolReasoning(root map[string]any) (changed, hasReal bool) {
	msgs, _ := root["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil || !strings.EqualFold(asString(m["role"]), "assistant") || m["tool_calls"] == nil {
			continue
		}
		rc := messageReasoning(m)
		if rc != "" {
			hasReal = true
			if asString(m["reasoning_content"]) != rc {
				m["reasoning_content"] = rc
				changed = true
			}
			continue
		}
		if _, ok := m["reasoning_content"]; !ok {
			m["reasoning_content"] = ""
			changed = true
		}
	}
	return changed, hasReal
}

func messageReasoning(m map[string]any) string {
	if m == nil {
		return ""
	}
	for _, k := range []string{"reasoning_content", "reasoning", "reasoning_text"} {
		if s := scrubPlaceholderThink(strings.TrimSpace(asString(m[k]))); s != "" {
			return s
		}
	}
	return ""
}

// lastMessageIsToolFollowup reports whether the substantive tail of the
// conversation is a tool result. Claude Code frequently appends a trailing
// system-role message (system-reminder injection) after tool_result blocks,
// so checking only msgs[len-1] misses real tool-followup turns whenever one
// is present — observed in prod 2026-09-20: a 90-message tool chain logged
// last=[tool tool tool system] and followup=false, which routed it into the
// slow account-rotating 504 retry path (~60s/account) instead of the fast
// same-account tool_followup_504_hold path (2s). Walk back past any trailing
// system messages to find the real last turn.
func lastMessageIsToolFollowup(root map[string]any) bool {
	msgs, _ := root["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		if m == nil {
			continue
		}
		role := asString(m["role"])
		if strings.EqualFold(role, "system") {
			continue
		}
		if strings.EqualFold(role, "tool") {
			return true
		}
		parts, ok := m["content"].([]any)
		if !ok {
			return false
		}
		for _, raw := range parts {
			pm, _ := raw.(map[string]any)
			if pm != nil && strings.EqualFold(asString(pm["type"]), "tool_result") {
				return true
			}
		}
		return false
	}
	return false
}

// describeTurnShape formats a log summary from an already-parsed request body.
// Takes the pre-parsed root (not raw bytes) so callers that also need the
// parsed map for other checks (e.g. lastMessageIsToolFollowup) don't pay for
// a second full json.Unmarshal of the same body on every chat request.
func describeTurnShape(root map[string]any, bodyLen int) string {
	if root == nil {
		return fmt.Sprintf("unparsed body=%d", bodyLen)
	}
	msgs, _ := root["messages"].([]any)
	n := len(msgs)
	start := n - 4
	if start < 0 {
		start = 0
	}
	last := make([]string, 0, 4)
	hasTool := false
	hasToolCalls := false
	hasReason := false
	missingReason := false
	for i, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		role := asString(m["role"])
		if strings.EqualFold(role, "tool") {
			hasTool = true
		}
		if m["tool_calls"] != nil {
			hasToolCalls = true
			if messageReasoning(m) != "" {
				hasReason = true
			} else {
				missingReason = true
			}
		}
		if i >= start {
			last = append(last, role)
		}
	}
	th := "none"
	switch v := root["thinking"].(type) {
	case map[string]any:
		if t := asString(v["type"]); t != "" {
			th = t
		}
	case string:
		if v != "" {
			th = v
		}
	}
	lastBytes := 0
	if n > 0 {
		if b, err := json.Marshal(msgs[n-1]); err == nil {
			lastBytes = len(b)
		}
	}
	ts := false
	if v, ok := root["tool_stream"].(bool); ok {
		ts = v
	}
	return fmt.Sprintf("msgs=%d last=%v last_bytes=%d tool_role=%v tool_calls=%v reason=%v miss_reason=%v chain=%v tool_stream=%v thinking=%s body=%d", n, last, lastBytes, hasTool, hasToolCalls, hasReason, missingReason, toolChainOK(root), ts, th, bodyLen)
}

func toolChainOK(root map[string]any) bool {
	msgs, _ := root["messages"].([]any)
	ids := map[string]bool{}
	sawTool := false
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tm, _ := tc.(map[string]any)
				if id := asString(tm["id"]); id != "" {
					ids[id] = true
				}
			}
		}
	}
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil || !strings.EqualFold(asString(m["role"]), "tool") {
			continue
		}
		sawTool = true
		id := asString(m["tool_call_id"])
		if id == "" || !ids[id] {
			return false
		}
	}
	return !sawTool || len(ids) > 0
}

func flattenClaudeSystem(root map[string]any) bool {
	arr, ok := root["system"].([]any)
	if !ok {
		return false
	}
	var b strings.Builder
	for _, raw := range arr {
		switch v := raw.(type) {
		case string:
			b.WriteString(v)
		case map[string]any:
			if t := asString(v["text"]); t != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(t)
			}
		}
	}
	root["system"] = b.String()
	return true
}

func rewriteClaudeTools(root map[string]any) bool {
	tools, ok := root["tools"].([]any)
	if !ok || len(tools) == 0 {
		return false
	}
	out := make([]any, 0, len(tools))
	changed := false
	for _, raw := range tools {
		t, _ := raw.(map[string]any)
		if t == nil {
			continue
		}
		if asString(t["type"]) == "function" && t["function"] != nil {
			// Already OpenAI format — still clean the parameters.
			if fn, _ := t["function"].(map[string]any); fn != nil {
				if p, _ := fn["parameters"].(map[string]any); p != nil {
					if cleanToolParameters(p) {
						changed = true
					}
				}
			}
			out = append(out, t)
			continue
		}
		name := asString(t["name"])
		if name == "" {
			out = append(out, t)
			continue
		}
		fn := map[string]any{"name": name}
		if d := t["description"]; d != nil {
			fn["description"] = d
		}
		if p := t["input_schema"]; p != nil {
			fn["parameters"] = p
		} else if p := t["parameters"]; p != nil {
			fn["parameters"] = p
		}
		// Clean parameters schema for GLM compatibility.
		if p, _ := fn["parameters"].(map[string]any); p != nil {
			cleanToolParameters(p)
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
		changed = true
	}
	if !changed {
		return false
	}
	root["tools"] = out
	return true
}

// cleanToolParameters strips JSON Schema fields that GLM-5.2 / InferHub
// doesn't understand, causing silent tool-call failures or 400 errors.
// Pattern from new-api's CleanFunctionParameters. Recursively walks the
// schema tree (properties, items, nested definitions).
func cleanToolParameters(schema map[string]any) bool {
	stripped := false
	for _, key := range []string{"$schema", "const", "additionalProperties"} {
		if _, ok := schema[key]; ok {
			delete(schema, key)
			stripped = true
		}
	}
	// Recurse into properties.
	if props, _ := schema["properties"].(map[string]any); props != nil {
		for _, v := range props {
			if prop, _ := v.(map[string]any); prop != nil {
				if cleanToolParameters(prop) {
					stripped = true
				}
			}
		}
	}
	// Recurse into array items.
	if items, _ := schema["items"].(map[string]any); items != nil {
		if cleanToolParameters(items) {
			stripped = true
		}
	}
	// Recurse into oneOf/anyOf/allOf branches.
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		if branches, _ := schema[key].([]any); branches != nil {
			for _, b := range branches {
				if branch, _ := b.(map[string]any); branch != nil {
					if cleanToolParameters(branch) {
						stripped = true
					}
				}
			}
		}
	}
	return stripped
}

// undeclaredToolChoiceLog throttles the "undeclared tool" warning so a buggy
// client can't turn a hot inference gateway into a log-write bottleneck: one
// line per (name) per 10s window, with a suppressed-count when the window
// rolls over. The warning is informational — the actual 400 comes from
// upstream — so throttling never affects request semantics.
var (
	undeclaredToolLogMu sync.Mutex
	undeclaredToolLog   = map[string]*undeclaredToolLogState{}
)

type undeclaredToolLogState struct {
	last       time.Time
	suppressed int
}

func logUndeclaredToolChoice(name string) {
	undeclaredToolLogMu.Lock()
	defer undeclaredToolLogMu.Unlock()
	st, ok := undeclaredToolLog[name]
	if !ok {
		st = &undeclaredToolLogState{}
		undeclaredToolLog[name] = st
		// Bound the map so a client cycling through unique undeclared names
		// can't grow it without limit. 512 unique names is generous: legitimate
		// codebases have ≪100 tools.
		if len(undeclaredToolLog) > 512 {
			for k := range undeclaredToolLog {
				if k != name {
					delete(undeclaredToolLog, k)
					break
				}
			}
		}
	}
	now := time.Now()
	if now.Sub(st.last) < 10*time.Second {
		st.suppressed++
		return
	}
	if st.suppressed > 0 {
		log.Printf("chat tool_choice_undeclared name=%s forwarding to upstream (suppressed=%d in prior window)", name, st.suppressed)
	} else {
		log.Printf("chat tool_choice_undeclared name=%s forwarding to upstream", name)
	}
	st.last = now
	st.suppressed = 0
}

// rewriteClaudeToolChoice maps Claude tool_choice to OpenAI form.
// Claude: {"type":"auto"|"any"|"none"|"tool","name":"X"}  (or bare string)
// OpenAI: "auto"|"required"|"none"  or  {"type":"function","function":{"name":"X"}}
// Must run AFTER rewriteClaudeTools so declared tool names are in OpenAI shape.
func rewriteClaudeToolChoice(root map[string]any) bool {
	tc, ok := root["tool_choice"].(map[string]any)
	if !ok {
		return false // already an OpenAI string form, or absent
	}
	typ := strings.ToLower(asString(tc["type"]))
	switch typ {
	case "auto":
		root["tool_choice"] = "auto"
	case "any":
		root["tool_choice"] = "required"
	case "none":
		root["tool_choice"] = "none"
	case "tool":
		// Claude named-tool form → OpenAI named-function form. Faithfully
		// forward the name — do NOT clobber to "auto" if the tool isn't
		// declared. Log the mismatch so it's visible, and let upstream
		// return the real 400 to the client (pure-forwarding rule).
		name := asString(tc["name"])
		if name == "" {
			log.Printf("chat tool_choice_empty_name forwarding raw shape")
			return false
		}
		if !toolNameDeclared(root, name) {
			logUndeclaredToolChoice(name)
		}
		root["tool_choice"] = map[string]any{
			"type":     "function",
			"function": map[string]any{"name": name},
		}
	case "function":
		// Already OpenAI object form — always preserve, even if the named
		// tool isn't declared (upstream returns 400 for real; we don't
		// silently rewrite it to a different semantic).
		fn, _ := tc["function"].(map[string]any)
		name := asString(fn["name"])
		if name != "" && !toolNameDeclared(root, name) {
			logUndeclaredToolChoice(name)
		}
		return false
	default:
		// Unknown object form — log and forward as-is; do not rewrite.
		log.Printf("chat tool_choice_unknown_shape type=%q forwarding as-is", typ)
		return false
	}
	return true
}

// toolNameDeclared reports whether name appears in the (already OpenAI-shaped)
// tools array. Claude Code occasionally references a retired tool name.
func toolNameDeclared(root map[string]any, name string) bool {
	tools, _ := root["tools"].([]any)
	for _, raw := range tools {
		t, _ := raw.(map[string]any)
		if t == nil {
			continue
		}
		if fn, _ := t["function"].(map[string]any); fn != nil {
			if asString(fn["name"]) == name {
				return true
			}
		}
		if asString(t["name"]) == name {
			return true
		}
	}
	return false
}

// rewriteStopSequences maps Claude stop_sequences → OpenAI stop.
// Claude always sends an array; OpenAI accepts string (1 elem) or array.
// Without this, stop_sequences hits the cliChatAllow whitelist and is dropped,
// so client-requested stops silently stop working through /v1/messages.
func rewriteStopSequences(root map[string]any) bool {
	seqs, ok := root["stop_sequences"].([]any)
	delete(root, "stop_sequences")
	if !ok || len(seqs) == 0 {
		return ok // ok=true only if key existed (so caller marks changed)
	}
	if _, exists := root["stop"]; exists {
		return true // client already set OpenAI stop; don't clobber
	}
	if len(seqs) == 1 {
		root["stop"] = seqs[0]
	} else {
		root["stop"] = seqs
	}
	return true
}

func rewriteClaudeMessages(root map[string]any) bool {
	msgs, ok := root["messages"].([]any)
	if !ok {
		return false
	}
	out := make([]any, 0, len(msgs))
	changed := false
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			out = append(out, raw)
			continue
		}
		parts, ok := m["content"].([]any)
		if !ok {
			out = append(out, m)
			continue
		}
		var text strings.Builder
		var reasoning strings.Builder
		var toolCalls []any
		var toolResults []any
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if pm == nil {
				if s, ok := p.(string); ok {
					text.WriteString(s)
				}
				continue
			}
			switch strings.ToLower(asString(pm["type"])) {
			case "text", "":
				text.WriteString(asString(pm["text"]))
			case "thinking", "reasoning":
				reasoning.WriteString(asString(pm["thinking"]))
				reasoning.WriteString(asString(pm["text"]))
				changed = true
			case "tool_use":
				rawArgs, _ := json.Marshal(pm["input"])
				argStr := string(rawArgs)
				// Missing/nil input marshals to "null"; OpenAI-shaped upstreams
				// (GLM/InferHub) expect an object string. Zero-arg tools are the
				// common case, so coerce to "{}" (mirrors the Responses path at
				// the request layer and openaiCompletionToClaude on the response).
				if argStr == "null" || argStr == "" {
					argStr = "{}"
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   asString(pm["id"]),
					"type": "function",
					"function": map[string]any{
						"name":      asString(pm["name"]),
						"arguments": argStr,
					},
				})
				changed = true
			case "image", "image_url", "input_image":
				text.WriteString("[image content — vision not supported by upstream model]")
				changed = true
			case "tool_result":
				// Claude Code sends tool_result.content as an ARRAY of blocks
				// ([{type:text,text:...}], sometimes images). An OpenAI role:tool
				// message expects a plain string; json.Marshal-ing the array whole
				// leaks the {"type":"text","text":...} scaffolding into what the
				// model reads (native chat clients send a bare string here). Flatten.
				c := flattenToolResultContent(pm["content"])
				if isErr, _ := pm["is_error"].(bool); isErr {
					c = "[TOOL_ERROR] " + c
				}
				toolResults = append(toolResults, map[string]any{
					"role":         "tool",
					"tool_call_id": asString(pm["tool_use_id"]),
					"content":      c,
				})
				changed = true
			default:
				if t := asString(pm["text"]); t != "" {
					text.WriteString(t)
				}
			}
		}
		if len(toolResults) > 0 {
			out = append(out, toolResults...)
			// If this same block also carried mid-turn user text (rare — Claude
			// Code injects a system-reminder or user prompt alongside tool_result),
			// emit that text as a SEPARATE user message after the tool replies.
			// Attaching it back onto a role:tool message, or leaving it in the
			// original m["role"] slot without the tool_calls context, breaks the
			// OpenAI adjacency rule tool_calls→tool→tool→(next role) — upstream 400.
			if text.Len() == 0 && len(toolCalls) == 0 {
				continue
			}
			userRole := asString(m["role"])
			if userRole == "" {
				userRole = "user"
			}
			trailing := map[string]any{"role": userRole}
			if text.Len() > 0 {
				trailing["content"] = text.String()
			} else {
				trailing["content"] = ""
			}
			if len(toolCalls) > 0 {
				trailing["tool_calls"] = toolCalls
			}
			if reasoning.Len() > 0 {
				trailing["reasoning_content"] = reasoning.String()
			}
			out = append(out, trailing)
			changed = true
			continue
		}
		msg := map[string]any{"role": m["role"]}
		if text.Len() > 0 {
			msg["content"] = text.String()
		} else if len(toolCalls) > 0 {
			msg["content"] = ""
		} else if reasoning.Len() > 0 {
			// Thinking-only assistant history: reasoning is already lifted into
			// reasoning_content below. Falling through to normalizeCLIContent
			// would keep the original {type:thinking} blocks in `content` (they
			// hit the default case and pass through unchanged) — duplicating
			// the same reasoning on the upstream side (in content AND
			// reasoning_content) and leaking a shape GLM does not accept.
			msg["content"] = ""
		} else {
			nc := normalizeCLIContent(m["content"])
			if nc == nil {
				// nil content → OpenAI/GLM rejects assistant with content:null (400
				// or silent drop). Prefer an empty string; the caller-supplied
				// reasoning_content still carries any useful history below.
				nc = ""
			}
			msg["content"] = nc
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		if reasoning.Len() > 0 {
			msg["reasoning_content"] = reasoning.String()
		} else if rc := messageReasoning(m); rc != "" {
			msg["reasoning_content"] = rc
		}
		if id := asString(m["tool_call_id"]); id != "" {
			msg["tool_call_id"] = id
		}
		// Skip fully-empty assistant messages entirely: no content, no tool_calls,
		// no reasoning_content. Sending role:assistant with content:"" and nothing
		// else confuses tool_calls adjacency and burns a slot for zero signal.
		if strings.EqualFold(asString(msg["role"]), "assistant") &&
			asString(msg["content"]) == "" &&
			len(toolCalls) == 0 &&
			asString(msg["reasoning_content"]) == "" {
			continue
		}
		out = append(out, msg)
	}
	if !changed {
		return false
	}
	// After rewriting Claude→OpenAI, two adjacent assistant messages that each
	// carry only tool_calls (produced when a turn had parallel tool_use blocks
	// AND text) can collide with the OpenAI rule that a tool_calls assistant
	// must be immediately followed by role:tool replies. Fold them the same
	// way the Responses path does.
	out = mergeConsecutiveToolCalls(out)
	root["messages"] = out
	return true
}

func prependSystem(root map[string]any, key string) bool {
	raw, _ := root[key].(string)
	text := strings.TrimSpace(raw)
	delete(root, key)
	if text == "" {
		return raw != ""
	}
	msgs, _ := root["messages"].([]any)
	for _, item := range msgs {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		if strings.EqualFold(asString(m["role"]), "system") && messageText(m["content"]) == text {
			return true
		}
	}
	sys := map[string]any{"role": "system", "content": text}
	root["messages"] = append([]any{sys}, msgs...)
	return true
}

func responsesInputToMessages(root map[string]any) []any {
	input, ok := root["input"]
	if !ok {
		return nil
	}
	switch v := input.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []any{map[string]any{"role": "user", "content": v}}
	case []any:
		out := make([]any, 0, len(v))
		for _, raw := range v {
			if msg := responsesItemToMessage(raw); msg != nil {
				out = append(out, msg)
			}
		}
		return mergeConsecutiveToolCalls(out)
	default:
		if msg := responsesItemToMessage(v); msg != nil {
			return []any{msg}
		}
		return nil
	}
}

// mergeConsecutiveToolCalls folds adjacent assistant tool_calls messages into one.
// Responses parallel tool calls arrive as separate function_call items; converting
// them 1:1 yields [assistant(tc1), assistant(tc2), tool, tool] where assistant(tc1)
// is not immediately followed by its tool reply — OpenAI/Huawei reject that. Merging
// consecutive content-less assistant tool_calls gives [assistant(tc1,tc2), tool, tool].
func mergeConsecutiveToolCalls(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	isToolCallOnly := func(m map[string]any) bool {
		return m != nil &&
			strings.EqualFold(asString(m["role"]), "assistant") &&
			m["tool_calls"] != nil &&
			asString(m["content"]) == ""
	}
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if isToolCallOnly(m) && len(out) > 0 {
			if prev, _ := out[len(out)-1].(map[string]any); isToolCallOnly(prev) {
				pc, _ := prev["tool_calls"].([]any)
				mc, _ := m["tool_calls"].([]any)
				prev["tool_calls"] = append(pc, mc...)
				continue
			}
		}
		out = append(out, raw)
	}
	return out
}

func responsesItemToMessage(raw any) map[string]any {
	m, _ := raw.(map[string]any)
	if m == nil {
		if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
			return map[string]any{"role": "user", "content": s}
		}
		return nil
	}
	itemType := strings.ToLower(asString(m["type"]))
	// Responses-specific item types carry no `role` and must be mapped to the
	// OpenAI chat message that preserves tool-call adjacency. Getting this wrong
	// (empty user message between tool_calls and tool reply) => upstream 400.
	switch itemType {
	case "function_call":
		// {type:function_call, call_id, name, arguments} → assistant tool_calls.
		callID := asString(m["call_id"])
		if callID == "" {
			callID = asString(m["id"])
		}
		args := asString(m["arguments"])
		if args == "" {
			args = "{}"
		}
		return map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id":       callID,
				"type":     "function",
				"function": map[string]any{"name": asString(m["name"]), "arguments": args},
			}},
		}
	case "function_call_output":
		// {type:function_call_output, call_id, output} → tool reply.
		// NOTE: Responses uses `call_id`, NOT `tool_call_id`.
		callID := asString(m["call_id"])
		if callID == "" {
			callID = asString(m["tool_call_id"])
		}
		out := asString(m["output"])
		if out == "" {
			if c := normalizeCLIContent(m["content"]); c != nil {
				return map[string]any{"role": "tool", "tool_call_id": callID, "content": c}
			}
		}
		return map[string]any{"role": "tool", "tool_call_id": callID, "content": out}
	case "reasoning":
		// Reasoning items are Responses metadata; emitting a standalone message
		// breaks tool_calls→tool adjacency. Drop it (华为 chat 无对应位置).
		return nil
	}
	role := strings.ToLower(asString(m["role"]))
	if role == "" {
		role = "user"
	}
	content := normalizeCLIContent(m["content"])
	if content == nil {
		if t := asString(m["text"]); t != "" {
			content = t
		} else if t = asString(m["output"]); t != "" {
			content = t
		}
	}
	msg := map[string]any{"role": role}
	if content != nil {
		msg["content"] = content
	}
	if id := asString(m["tool_call_id"]); id != "" {
		msg["tool_call_id"] = id
	} else if id := asString(m["call_id"]); id != "" {
		msg["tool_call_id"] = id
	}
	if tc, ok := m["tool_calls"]; ok {
		msg["tool_calls"] = tc
	}
	if rc := messageReasoning(m); rc != "" {
		msg["reasoning_content"] = rc
	}
	return msg
}

// flattenToolResultContent turns a Claude tool_result content (usually an ARRAY
// of {type:text}/{type:image} blocks) into the plain-string content an OpenAI
// role:tool message expects. Native chat clients (dsh/opencode) already send a
// bare string here; Claude Code sends the wrapped array, and json.Marshal-ing it
// whole leaks the {"type":"text","text":...} scaffolding — and, for images, a
// base64 blob — into what the model reads. Extract the text; represent any
// non-text block with a short placeholder rather than raw structured JSON.
func flattenToolResultContent(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, raw := range v {
			if s, ok := raw.(string); ok {
				b.WriteString(s)
				continue
			}
			pm, _ := raw.(map[string]any)
			if pm == nil {
				continue
			}
			// Read text raw (no TrimSpace): tool output whitespace/indentation is
			// significant and must survive verbatim.
			raw, _ := pm["text"].(string)
			switch strings.ToLower(asString(pm["type"])) {
			case "text", "output_text", "input_text", "":
				b.WriteString(raw)
			case "image", "image_url", "input_image":
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString("[image omitted]")
			case "resource", "resource_link", "mcp_resource", "embedded_resource":
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				// MCP resource blocks — surface the URI so the model has SOMETHING
				// to reason about; the blob/base64 itself is dropped.
				uri := asString(pm["uri"])
				if r, ok := pm["resource"].(map[string]any); ok && uri == "" {
					uri = asString(r["uri"])
				}
				if uri != "" {
					b.WriteString("[resource: " + uri + "]")
				} else {
					b.WriteString("[resource omitted]")
				}
			default:
				if raw != "" {
					b.WriteString(raw)
				} else {
					j, _ := json.Marshal(pm)
					b.Write(j)
				}
			}
		}
		return b.String()
	default:
		j, _ := json.Marshal(v)
		return string(j)
	}
}

func normalizeCLIContent(content any) any {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := make([]any, 0, len(v))
		for _, raw := range v {
			p, _ := raw.(map[string]any)
			if p == nil {
				if s, ok := raw.(string); ok && s != "" {
					parts = append(parts, map[string]any{"type": "text", "text": s})
				}
				continue
			}
			typ := strings.ToLower(asString(p["type"]))
			switch typ {
			case "input_text", "output_text", "text", "":
				if t := asString(p["text"]); t != "" {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "input_image", "image_url", "image":
				url := asString(p["image_url"])
				if url == "" {
					if img, ok := p["image_url"].(map[string]any); ok {
						url = asString(img["url"])
					}
				}
				if url == "" {
					url = asString(p["url"])
				}
				if url != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			default:
				parts = append(parts, p)
			}
		}
		if len(parts) == 0 {
			return nil
		}
		if len(parts) == 1 {
			if p, _ := parts[0].(map[string]any); p != nil && asString(p["type"]) == "text" {
				return asString(p["text"])
			}
		}
		return parts
	default:
		return nil
	}
}

// extractOutputConfig lifts max_tokens from Anthropic's output_config wrapper
// into the top-level field that Huawei InferHub expects. Claude Code ≥2.1.260
// sends output_config instead of bare max_tokens. The client's value (typically
// 16384) is kept as-is — it acts as the only effective cap on GLM thinking,
// which ignores budget_tokens. Overriding to 65536 caused 16-minute runaway
// thinking (48K reasoning tokens for 167 chars of content).
func extractOutputConfig(root map[string]any) bool {
	oc, ok := root["output_config"].(map[string]any)
	if !ok || oc == nil {
		return false
	}
	if mt, ok := asMaxTokens(oc["max_tokens"]); ok && mt > 0 {
		if _, exists := root["max_tokens"]; !exists {
			root["max_tokens"] = mt
			log.Printf("chat output_config_lift max_tokens=%d", mt)
		}
	}
	delete(root, "output_config")
	return true
}

func asMaxTokens(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}
