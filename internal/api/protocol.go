package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

type clientProto string

const (
	protoChat      clientProto = "chat"
	protoResponses clientProto = "responses"
	protoClaude    clientProto = "claude"
)

func detectClientProto(r *http.Request) clientProto {
	if r == nil {
		return protoChat
	}
	p := strings.ToLower(r.URL.Path)
	if strings.Contains(p, "/responses") {
		return protoResponses
	}
	if strings.Contains(p, "/messages") && !strings.Contains(p, "chat") {
		return protoClaude
	}
	if strings.TrimSpace(r.Header.Get("anthropic-version")) != "" {
		return protoClaude
	}
	return protoChat
}

func writeNamedSSE(w io.Writer, flusher http.Flusher, event, payload string) error {
	if event != "" {
		if _, err := io.WriteString(w, "event: "+event+"\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "data: "+payload+"\n\n"); err != nil {
		return err
	}
	flushSSE(flusher)
	return nil
}

func writeProtoKeepalive(w io.Writer, flusher http.Flusher, pin *ssePin) error {
	// Comments / ping only. ZWSP reasoning pings show up as an empty Thought in OpenCode.
	if pin != nil && pin.Proto == protoClaude {
		return writeNamedSSE(w, flusher, "ping", `{"type":"ping"}`)
	}
	return writeSSEComment(w, flusher)
}

// writeProtoEnvelope emits the protocol start event (message_start for Claude,
// response.created for Responses) so clients see a well-formed stream envelope
// during the TTFT wait — not just bare pings. The real Anthropic API sends
// message_start immediately, then pings. A client expecting message_start early
// may treat a stream of bare pings as malformed and disconnect.
//
// For OpenAI chat protocol this is a no-op (the first chunk IS the data).
// Sets pin.StartSent so ensureClaudeStart / ensureResponsesStart skip re-sending.
func writeClaudeMessageStart(w io.Writer, flusher http.Flusher, pin *ssePin) error {
	if pin == nil {
		pin = &ssePin{}
	}
	id := pin.ID
	if id == "" {
		id = "msg_proxy"
	}
	b, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         pin.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  pin.InputTokens,
				"output_tokens": 0,
			},
		},
	})
	if err := writeNamedSSE(w, flusher, "message_start", string(b)); err != nil {
		return err
	}
	pin.StartSent = true
	return nil
}

// estimatePromptTokens approximates the prompt token count from the raw client
// request body so message_start can carry a non-zero input_tokens. Huawei only
// reports the exact prompt_tokens in the trailing usage frame (message_delta);
// without this, message_start.usage.input_tokens is 0 and Anthropic-format
// clients that read the prompt/context size from message_start (Claude Code)
// display a 0 token count. It walks every string value in the request (system,
// messages, tool schemas) so the estimate reflects the full context, weighting
// CJK runes heavier than ASCII. The exact count still overwrites this later in
// message_delta, so a rough estimate is enough to seed the client's counter.
func estimatePromptTokens(body []byte) int64 {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return int64(len(body) / 4)
	}
	var ascii, cjk, other int
	var walk func(n any)
	walk = func(n any) {
		switch t := n.(type) {
		case string:
			for _, r := range t {
				switch {
				case r < 128:
					ascii++
				case isCJKRune(r):
					cjk++
				default:
					other++
				}
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for k, e := range t {
				switch k {
				case "model", "stream", "stream_options", "metadata",
					"temperature", "top_p", "top_k", "max_tokens":
					continue
				}
				walk(e)
			}
		}
	}
	walk(v)
	est := float64(ascii)/4.0 + float64(cjk)/1.6 + float64(other)/2.0 + 8.0
	if est < 1 {
		est = 1
	}
	return int64(est + 0.5)
}

// isCJKRune reports whether r is a CJK/Hangul/Kana rune, which tokenizers count
// far denser than Latin text (≈1–2 chars per token vs ≈4).
func isCJKRune(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF, // CJK Unified Ideographs
		r >= 0x3400 && r <= 0x4DBF, // CJK Extension A
		r >= 0x3040 && r <= 0x30FF, // Hiragana + Katakana
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0x3000 && r <= 0x303F, // CJK symbols and punctuation
		r >= 0xFF00 && r <= 0xFFEF: // Halfwidth/Fullwidth forms
		return true
	}
	return false
}

func writeResponsesCreated(w io.Writer, flusher http.Flusher, pin *ssePin) error {
	if pin == nil {
		pin = &ssePin{}
	}
	id := pin.ID
	if id == "" {
		id = "resp_proxy"
	}
	b, _ := json.Marshal(map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":     id,
			"object": "response",
			"status": "in_progress",
			"model":  pin.Model,
		},
	})
	if err := writeNamedSSE(w, flusher, "response.created", string(b)); err != nil {
		return err
	}
	pin.StartSent = true
	return nil
}

func writeProtoEnvelope(w io.Writer, flusher http.Flusher, pin *ssePin) error {
	if pin == nil {
		return nil
	}
	switch pin.Proto {
	case protoClaude:
		return writeClaudeMessageStart(w, flusher, pin)
	case protoResponses:
		return writeResponsesCreated(w, flusher, pin)
	default:
		pin.StartSent = true
		return nil
	}
}

func encodeProtoBody(proto clientProto, openaiBody []byte, pin *ssePin) []byte {
	switch proto {
	case protoClaude:
		return openaiCompletionToClaude(openaiBody, pin)
	case protoResponses:
		return openaiCompletionToResponses(openaiBody, pin)
	default:
		return openaiBody
	}
}

type protoStream struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	pin       *ssePin
	started   bool
	thinkOpen bool
	textOpen  bool
	text      strings.Builder
	think     strings.Builder
	usage     map[string]any
	// Tool-use tracking for Claude protocol.
	toolOpen  map[int]bool // index → block started (content_block_start sent)
	toolCount int          // number of tool blocks opened
	finished  bool         // terminal events already sent
	// pendingStop defers message_delta until the trailing usage-only frame
	// arrives. Huawei (with include_usage) sends finish_reason and usage in
	// separate frames; finalizing on finish_reason alone loses token counts.
	pendingStop string
	// respItemOpen tracks whether the Responses output_item.added +
	// content_part.added lifecycle events have been emitted (Codex CLI rejects
	// output_text.delta for an item it never saw "added").
	respItemOpen bool
	// textIdx is the content block index assigned to the text block when it opens,
	// so the open / delta / close events all agree even if a tool block opened
	// first (positional recomputation elsewhere would disagree).
	textIdx int
	// thinkFlushedTo tracks how many bytes of p.think have been emitted as
	// thinking_delta events to the client. Once tools open we can no longer
	// emit inline thinking (Anthropic's blocks are sequential and the pre-tool
	// thinking block is already closed), so any reasoning that keeps arriving
	// between/after tool_calls accumulates in p.think unflushed. finishClaudeFinish
	// drains the gap into a fresh trailing thinking block so the client actually
	// sees the model's mid/late reasoning instead of it being silently dropped.
	thinkFlushedTo int
	// toolBlockIdx pins the Claude content-block index assigned to each upstream
	// tool index AT OPEN TIME. blockIndexForTool recomputes from live thinkOpen/
	// textOpen/toolCount state, which DRIFTS: a tool opened before text gets
	// index N, then text opens, and a later arguments-delta recompute returns
	// N+1 — colliding with the text block. Reusing the pinned index keeps every
	// delta/close for a tool on its original block.
	toolBlockIdx map[int]int
	// Responses API tool-call tracking (parallel to toolOpen for Claude).
	respToolOpen map[int]bool
	respToolArgs map[int]*strings.Builder
	respToolMeta map[int]map[string]string // index → {id, name}
}

func newProtoStream(w http.ResponseWriter, pin *ssePin) *protoStream {
	flusher, _ := w.(http.Flusher)
	if pin == nil {
		pin = &ssePin{Proto: protoChat}
	}
	return &protoStream{
		w: w, flusher: flusher, pin: pin,
		toolOpen:     map[int]bool{},
		toolBlockIdx: map[int]int{},
		respToolOpen: map[int]bool{},
		respToolArgs: map[int]*strings.Builder{},
		respToolMeta: map[int]map[string]string{},
	}
}

func (p *protoStream) emit(payload string) error {
	if p.pin == nil || p.pin.Proto == protoChat || p.pin.Proto == "" {
		if _, err := io.WriteString(p.w, "data: "+payload+"\n\n"); err != nil {
			return err
		}
		flushSSE(p.flusher)
		return nil
	}
	if payload == "[DONE]" {
		return p.finish()
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return nil
	}
	// A Huawei in-band error (e.g. "prompt length exceeds") is shaped as
	// {"error":{...}} with no "choices" — openaiDelta below would silently
	// treat it as an empty delta, and the [DONE] that follows would then
	// close a clean, empty, stop_reason=end_turn turn. The client (Claude
	// Code CLI) sees a normal successful-looking empty response instead of
	// the real error, so a still-too-long conversation looks "stuck" on
	// every subsequent message instead of surfacing an actionable error.
	if errObj, ok := obj["error"].(map[string]any); ok {
		return p.emitError(errObj)
	}
	// Capture usage from usage-only frames for finish() to forward.
	if u, ok := obj["usage"].(map[string]any); ok {
		p.usage = u
		// The trailing usage frame is what a deferred finish was waiting for.
		if p.pin.Proto == protoClaude && p.pendingStop != "" && !p.finished {
			return p.flushClaudeStop(p.pendingStop)
		}
	}
	delta, finish := openaiDelta(obj)
	if p.pin.Proto == protoClaude {
		return p.emitClaude(delta, finish)
	}
	return p.emitResponses(delta, finish)
}

// emitError surfaces an upstream in-band error as a protocol-native terminal
// event instead of letting it vanish as an empty successful turn. Mirrors the
// real Anthropic/Responses stream error shape so Claude Code / Codex CLI show
// the actual message (e.g. prompt-too-long) instead of silently no-op'ing.
func (p *protoStream) emitError(errObj map[string]any) error {
	if p.finished {
		return nil
	}
	msg, _ := errObj["message"].(string)
	if msg == "" {
		msg = "upstream error"
	}
	switch p.pin.Proto {
	case protoClaude:
		if err := p.ensureClaudeStart(); err != nil {
			return err
		}
		p.finished = true
		b, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "invalid_request_error",
				"message": msg,
			},
		})
		return writeNamedSSE(p.w, p.flusher, "error", string(b))
	case protoResponses:
		if err := p.ensureResponsesStart(); err != nil {
			return err
		}
		p.finished = true
		id := p.pin.ID
		if id == "" {
			id = "resp_proxy"
		}
		b, _ := json.Marshal(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id":     id,
				"object": "response",
				"status": "failed",
				"error":  map[string]any{"message": msg},
			},
		})
		return writeNamedSSE(p.w, p.flusher, "response.failed", string(b))
	default:
		p.finished = true
		b, _ := json.Marshal(map[string]any{"error": errObj})
		if _, err := io.WriteString(p.w, "data: "+string(b)+"\n\n"); err != nil {
			return err
		}
		flushSSE(p.flusher)
		return nil
	}
}

func openaiDelta(obj map[string]any) (map[string]any, string) {
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return nil, ""
	}
	ch, _ := choices[0].(map[string]any)
	if ch == nil {
		return nil, ""
	}
	fr, _ := ch["finish_reason"].(string)
	delta, _ := ch["delta"].(map[string]any)
	if delta == nil {
		if msg, _ := ch["message"].(map[string]any); msg != nil {
			delta = msg
		}
	}
	return delta, fr
}

func (p *protoStream) emitClaude(delta map[string]any, finish string) error {
	if err := p.ensureClaudeStart(); err != nil {
		return err
	}
	if delta != nil {
		if s := firstThinkingString(delta); s != "" {
			// Pre-tool thinking streams live at index 0. Once tool blocks have
			// opened, thinking@0 is closed and Anthropic's sequential block
			// rule means we cannot re-open it mid-flight — so we accumulate
			// here and flush the gap as a fresh trailing thinking block in
			// finishClaudeFinish (so mid/late reasoning is preserved, not
			// silently dropped like it used to be).
			if p.toolCount == 0 {
				if err := p.openClaudeThink(); err != nil {
					return err
				}
				b, _ := json.Marshal(map[string]any{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]any{"type": "thinking_delta", "thinking": s},
				})
				if err := writeNamedSSE(p.w, p.flusher, "content_block_delta", string(b)); err != nil {
					return err
				}
				p.thinkFlushedTo += len(s)
			}
			p.think.WriteString(s)
		}
		if s := visibleText(delta["content"]); s != "" {
			// Guard against REOPENING a text block after tool_use blocks.
			// If text was already emitted (text.Len()>0) and tools opened
			// after it, reopening text would compute an index that collides
			// with a pinned tool block (base+toolCount includes the text
			// term from text.Len()>0, matching the tool's open-time base).
			// First-time text after tools is safe — the index is correct.
			reopen := p.toolCount > 0 && p.text.Len() > 0
			if !reopen {
				if err := p.openClaudeText(); err != nil {
					return err
				}
				idx := p.textIdx
				b, _ := json.Marshal(map[string]any{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]any{"type": "text_delta", "text": s},
				})
				if err := writeNamedSSE(p.w, p.flusher, "content_block_delta", string(b)); err != nil {
					return err
				}
			}
			p.text.WriteString(s)
		}
		// Tool-use: convert OpenAI tool_calls deltas to Claude tool_use blocks.
		if tcs, ok := delta["tool_calls"].([]any); ok {
			if err := p.emitClaudeToolCalls(tcs); err != nil {
				return err
			}
		}
	}
	if finish != "" {
		return p.finishClaudeFinish(finish)
	}
	return nil
}

// emitClaudeToolCalls converts OpenAI streaming tool_calls deltas into Claude
// content_block_start + input_json_delta + content_block_stop events.
func (p *protoStream) emitClaudeToolCalls(tcs []any) error {
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		toolIdx := 0
		if v := asFloat(tc["index"]); v >= 0 {
			toolIdx = int(v)
		}
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			continue
		}
		// First delta for this tool: send content_block_start.
		if !p.toolOpen[toolIdx] {
			// Anthropic protocol: content blocks are sequential; each must
			// be closed before the next opens. Close thinking and text
			// before any tool_use block. Route thinking through closeClaudeThink
			// so the signature_delta lands before content_block_stop; a bare
			// close drops thinking from the client's next-turn history and
			// starves the model of its own prior reasoning (see closeClaudeThink
			// for the full contract rationale).
			if p.thinkOpen {
				if err := p.closeClaudeThink(0, p.think.String()); err != nil {
					return err
				}
				p.thinkOpen = false
			}
			if p.textOpen {
				if err := p.closeClaudeBlock(p.textIdx); err != nil {
					return err
				}
				p.textOpen = false
			}
			p.toolOpen[toolIdx] = true
			// Dense ordinal: the Nth tool opened maps to base+N, regardless of the
			// upstream tool index (which can be sparse, e.g. 0 then 2). Claude clients
			// require contiguous block indices; base+toolIdx would leave a gap and the
			// client would mis-assemble the tool call.
			blockIdx := p.claudeBlockBase() + p.toolCount
			p.toolCount++
			p.toolBlockIdx[toolIdx] = blockIdx // pin it; deltas/close must not recompute
			toolID, _ := tc["id"].(string)
			if toolID == "" {
				toolID = fmt.Sprintf("toolu_proxy_%d", toolIdx)
			}
			toolName, _ := fn["name"].(string)
			b, _ := json.Marshal(map[string]any{
				"type":  "content_block_start",
				"index": blockIdx,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    toolID,
					"name":  toolName,
					"input": map[string]any{},
				},
			})
			if err := writeNamedSSE(p.w, p.flusher, "content_block_start", string(b)); err != nil {
				return err
			}
		}
		// Stream partial JSON arguments — reuse the index pinned at open time.
		// Some upstream shapes (non-stream synthesized deltas, GLM variants) pass
		// arguments as a parsed object/array instead of a string; marshal so the
		// Claude client always sees a `partial_json` string, otherwise the tool
		// call reaches the client with `input:{}` and no delta and it can't invoke.
		var argsStr string
		switch v := fn["arguments"].(type) {
		case string:
			argsStr = v
		case map[string]any, []any:
			if b, err := json.Marshal(v); err == nil {
				argsStr = string(b)
			}
		}
		if argsStr != "" {
			blockIdx, ok := p.toolBlockIdx[toolIdx]
			if !ok {
				blockIdx = p.blockIndexForTool(toolIdx)
			}
			b, _ := json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"index": blockIdx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": argsStr},
			})
			if err := writeNamedSSE(p.w, p.flusher, "content_block_delta", string(b)); err != nil {
				return err
			}
		}
	}
	return nil
}

// claudeBlockBase is the number of non-tool content blocks (thinking, text) that
// precede the first tool block. Tool blocks are then assigned base+ordinal in the
// order they open (see emitClaudeToolCalls) so the indices stay contiguous.
func (p *protoStream) claudeBlockBase() int {
	base := 0
	if p.thinkOpen || p.think.Len() > 0 {
		base++
	}
	if p.textOpen || p.text.Len() > 0 {
		base++
	}
	return base
}

// blockIndexForTool is a DEGENERATE fallback used only if a tool's block index
// was not pinned at open time (should never happen — emitClaudeToolCalls pins
// every tool in toolBlockIdx before any delta/close). Prefer p.toolBlockIdx.
func (p *protoStream) blockIndexForTool(toolIdx int) int {
	return p.claudeBlockBase() + toolIdx
}

// finishClaudeFinish closes all open content blocks on finish_reason, then either
// emits the terminal message_delta+message_stop immediately (usage already seen)
// or defers it until the trailing usage-only frame arrives (see pendingStop).
func (p *protoStream) finishClaudeFinish(finish string) error {
	if p.finished {
		return nil
	}
	stopReason := "end_turn"
	switch finish {
	case "tool_calls", "tool_call", "function_call":
		stopReason = "tool_use"
	case "length":
		stopReason = "max_tokens"
	case "content_filter":
		stopReason = "end_turn"
	}
	// A thinking-only finish (thinking block, no text, no tool_use) is a
	// LEGAL Anthropic Messages API shape — dsh's chat 1:1 forward works because
	// it renders reasoning_content directly. Do NOT fabricate text here. Any
	// gateway-generated visible content (placeholder OR reasoning-copy) is
	// user-facing noise; if Claude Code auto-retries on empty output, that is
	// its client-side behaviour and the fix belongs there, not here.
	if err := p.flushLateThinking(); err != nil {
		return err
	}
	if err := p.closeAllClaudeBlocks(); err != nil {
		return err
	}
	// Defer the message_delta (which carries usage) until a usage-only frame,
	// unless we already captured usage. Blocks are already closed; only the
	// terminal delta+stop waits. p.finish() ([DONE]) flushes as a backstop.
	if p.usage == nil {
		p.pendingStop = stopReason
		return nil
	}
	return p.flushClaudeStop(stopReason)
}

// flushLateThinking emits any reasoning that arrived AFTER the first tool
// block opened (which forced the primary thinking block closed) as a fresh
// trailing thinking block, so the client sees the model's late/mid reasoning
// instead of it being silently dropped. All other still-open blocks (text,
// tools) must close first — Anthropic content blocks are sequential.
// No-op when there is no gap between accumulated and already-emitted thinking.
func (p *protoStream) flushLateThinking() error {
	if p.thinkFlushedTo >= p.think.Len() {
		return nil
	}
	if p.toolCount == 0 && !p.textOpen && p.text.Len() == 0 {
		return nil
	}
	late := p.think.String()[p.thinkFlushedTo:]
	if strings.TrimSpace(late) == "" {
		p.thinkFlushedTo = p.think.Len()
		return nil
	}
	if p.textOpen {
		if err := p.closeClaudeBlock(p.textIdx); err != nil {
			return err
		}
		p.textOpen = false
	}
	type tb struct{ toolIdx, blockIdx int }
	var tbs []tb
	for toolIdx, open := range p.toolOpen {
		if !open {
			continue
		}
		bi, ok := p.toolBlockIdx[toolIdx]
		if !ok {
			bi = p.blockIndexForTool(toolIdx)
		}
		tbs = append(tbs, tb{toolIdx, bi})
	}
	sort.Slice(tbs, func(i, j int) bool { return tbs[i].blockIdx < tbs[j].blockIdx })
	for _, t := range tbs {
		if err := p.closeClaudeBlock(t.blockIdx); err != nil {
			return err
		}
		p.toolOpen[t.toolIdx] = false
	}
	lateIdx := p.claudeBlockBase() + p.toolCount
	startB, _ := json.Marshal(map[string]any{
		"type":          "content_block_start",
		"index":         lateIdx,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
	if err := writeNamedSSE(p.w, p.flusher, "content_block_start", string(startB)); err != nil {
		return err
	}
	deltaB, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": lateIdx,
		"delta": map[string]any{"type": "thinking_delta", "thinking": late},
	})
	if err := writeNamedSSE(p.w, p.flusher, "content_block_delta", string(deltaB)); err != nil {
		return err
	}
	// Route through closeClaudeThink so the late block also carries a
	// signature — otherwise the client drops it from history and any late
	// reasoning after the tool round-trip is lost from the model's view on
	// the next turn (same failure mode as the pre-tool block).
	if err := p.closeClaudeThink(lateIdx, late); err != nil {
		return err
	}
	p.thinkFlushedTo = p.think.Len()
	return nil
}

// closeAllClaudeBlocks closes thinking/text/tool blocks in ascending index
// order, matching the Anthropic API guarantee that content_block_stop events
// arrive in block-index sequence. Tool blocks are collected first, then
// sorted, so Go's random map-iteration order does not leak into the wire.
func (p *protoStream) closeAllClaudeBlocks() error {
	if p.thinkOpen {
		// thinkOpen==true here means no tool_use ever opened (otherwise
		// emitClaudeToolCalls would have closed thinking already) and no
		// text either (openClaudeText would have closed it). So the entire
		// p.think buffer is the pre-tool block's content — pass it in for
		// signature computation.
		if err := p.closeClaudeThink(0, p.think.String()); err != nil {
			return err
		}
		p.thinkOpen = false
	}
	if p.textOpen {
		if err := p.closeClaudeBlock(p.textIdx); err != nil {
			return err
		}
		p.textOpen = false
	}
	// Collect open tool block indices, sort ascending, then close in order.
	type tb struct {
		toolIdx, blockIdx int
	}
	var tbs []tb
	for toolIdx, open := range p.toolOpen {
		if !open {
			continue
		}
		blockIdx, ok := p.toolBlockIdx[toolIdx]
		if !ok {
			blockIdx = p.blockIndexForTool(toolIdx)
		}
		tbs = append(tbs, tb{toolIdx, blockIdx})
	}
	sort.Slice(tbs, func(i, j int) bool { return tbs[i].blockIdx < tbs[j].blockIdx })
	for _, t := range tbs {
		if err := p.closeClaudeBlock(t.blockIdx); err != nil {
			return err
		}
		p.toolOpen[t.toolIdx] = false
	}
	return nil
}

// flushClaudeStop emits message_delta (with whatever usage we have) + message_stop.
func (p *protoStream) flushClaudeStop(stopReason string) error {
	if p.finished {
		return nil
	}
	p.finished = true
	p.pendingStop = ""
	usage := p.claudeUsage()
	if usage == nil {
		// Some upstreams omit the trailing usage frame entirely. Emit a
		// best-effort estimate so Claude Code's UI (context bar, spend meter)
		// doesn't render 0/? — it treats a NIL usage as "no data" and the row
		// disappears, which looks like the request silently failed.
		usage = p.estimateFallbackUsage()
	}
	delta, _ := json.Marshal(map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": usage,
	})
	if err := writeNamedSSE(p.w, p.flusher, "message_delta", string(delta)); err != nil {
		return err
	}
	stop, _ := json.Marshal(map[string]any{"type": "message_stop"})
	return writeNamedSSE(p.w, p.flusher, "message_stop", string(stop))
}

// closeClaudeThink emits a deterministic signature_delta then closes a thinking
// content block.
//
// WHY signature emission is required (do NOT revert to a bare close):
// Anthropic's Messages API contract says thinking blocks WITHOUT a signature are
// dropped by the client when it echoes the assistant turn back on the next
// request. That drops the model's reasoning across turns, and manifests as
// agent-loop self-repetition — every turn the model re-derives the same
// conclusions from tool_use params + short text alone, producing a symptom
// that looks like "the model is stuck in a loop" but is really a protocol
// contract violation.
//
// The signature is deterministic per-content (sha256 of the streamed thinking
// bytes, base64-encoded). Two reasons that matters:
//   - Anthropic prompt-cache key stability: the same conversation replayed must
//     produce the same signature bytes each time, or cache misses on re-play.
//   - Idempotency: retries within a single turn must not shift the signature.
//
// The signature is NOT cryptographically valid vs Anthropic — it cannot be,
// we do not have Anthropic's signing key. That is fine: signature validation
// is server-side (Anthropic verifies its own signatures); Claude Code (and
// other clients) only echo the value back opaquely. The domain-separated
// prefix ("go-codearts-think-sig-v1:") guarantees we cannot collide with a
// real Anthropic signature by accident.
//
// Huawei/GLM ignores the signature field on the request side (cli_body.go
// rewriteClaudeMessages reads only the thinking/text fields), so the round
// trip is transparent to upstream. See think_mem.go for the legacy per-
// tool_call_id fallback that catches signature-loss edge cases.
//
// `content` is the thinking text streamed under THIS block (not the whole
// p.think buffer — the pre-tool and late trailing blocks each have their own
// slice).
func (p *protoStream) closeClaudeThink(index int, content string) error {
	sig := computeThinkSignature(content)
	b, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{"type": "signature_delta", "signature": sig},
	})
	if err := writeNamedSSE(p.w, p.flusher, "content_block_delta", string(b)); err != nil {
		return err
	}
	return p.closeClaudeBlock(index)
}

// computeThinkSignature returns a deterministic base64(sha256) signature for a
// thinking block. See closeClaudeThink for the full contract rationale. The
// domain-separated prefix is versioned so if the scheme ever changes (e.g. we
// need to include the model or index in the digest input), old cached prefixes
// remain distinguishable.
func computeThinkSignature(content string) string {
	sum := sha256.Sum256([]byte("go-codearts-think-sig-v1:" + content))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func (p *protoStream) ensureClaudeStart() error {
	if p.started {
		return nil
	}
	p.started = true
	// If the TTFT keepalive already sent message_start, skip re-sending.
	// The keepalive also covered the initial ping.
	if p.pin != nil && p.pin.StartSent {
		return nil
	}
	if err := writeClaudeMessageStart(p.w, p.flusher, p.pin); err != nil {
		return err
	}
	// Claude API sends a ping right after message_start.
	ping, _ := json.Marshal(map[string]any{"type": "ping"})
	return writeNamedSSE(p.w, p.flusher, "ping", string(ping))
}

func (p *protoStream) openClaudeThink() error {
	if p.thinkOpen {
		return nil
	}
	p.thinkOpen = true
	b, _ := json.Marshal(map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
	return writeNamedSSE(p.w, p.flusher, "content_block_start", string(b))
}

func (p *protoStream) openClaudeText() error {
	if p.textOpen {
		return nil
	}
	if p.thinkOpen {
		// Route through closeClaudeThink so signature_delta lands first;
		// bare close leaves the client unable to echo thinking back next
		// turn (see closeClaudeThink for the full contract rationale).
		if err := p.closeClaudeThink(0, p.think.String()); err != nil {
			return err
		}
		p.thinkOpen = false
	}
	p.textOpen = true
	// Match claudeBlockBase's thinking term exactly (thinkOpen OR think.Len()>0),
	// so the text block and tool blocks never compute inconsistent bases and
	// collide. (Text itself is being opened here, so its own term is excluded.)
	idx := 0
	if p.thinkOpen || p.think.Len() > 0 {
		idx = 1
	}
	// Account for any tool blocks already opened (rare: upstream emits tool_calls
	// before visible content). Store the index so delta/close events agree.
	idx += p.toolCount
	p.textIdx = idx
	b, _ := json.Marshal(map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	return writeNamedSSE(p.w, p.flusher, "content_block_start", string(b))
}

func (p *protoStream) closeClaudeBlock(index int) error {
	b, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": index})
	return writeNamedSSE(p.w, p.flusher, "content_block_stop", string(b))
}

func (p *protoStream) emitResponses(delta map[string]any, finish string) error {
	if err := p.ensureResponsesStart(); err != nil {
		return err
	}
	if delta != nil {
		if s := firstThinkingString(delta); s != "" {
			p.think.WriteString(s)
			b, _ := json.Marshal(map[string]any{
				"type":  "response.reasoning.delta",
				"delta": s,
			})
			if err := writeNamedSSE(p.w, p.flusher, "response.reasoning.delta", string(b)); err != nil {
				return err
			}
		}
		if s := visibleText(delta["content"]); s != "" {
			if err := p.ensureResponsesItemOpen(); err != nil {
				return err
			}
			p.text.WriteString(s)
			b, _ := json.Marshal(map[string]any{
				"type":          "response.output_text.delta",
				"delta":         s,
				"item_id":       p.responsesItemID(),
				"output_index":  0,
				"content_index": 0,
			})
			if err := writeNamedSSE(p.w, p.flusher, "response.output_text.delta", string(b)); err != nil {
				return err
			}
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			if err := p.emitResponsesToolCalls(tcs); err != nil {
				return err
			}
		}
	}
	if finish != "" {
		return p.finish()
	}
	return nil
}

func (p *protoStream) emitResponsesToolCalls(tcs []any) error {
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		toolIdx := 0
		if v := asFloat(tc["index"]); v >= 0 {
			toolIdx = int(v)
		}
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			continue
		}
		if !p.respToolOpen[toolIdx] {
			p.respToolOpen[toolIdx] = true
			p.respToolArgs[toolIdx] = &strings.Builder{}
			callID, _ := tc["id"].(string)
			if callID == "" {
				callID = fmt.Sprintf("call_proxy_%d", toolIdx)
			}
			toolName, _ := fn["name"].(string)
			p.respToolMeta[toolIdx] = map[string]string{"id": callID, "name": toolName}
			outIdx := toolIdx
			if p.respItemOpen {
				outIdx = toolIdx + 1
			}
			b, _ := json.Marshal(map[string]any{
				"type":         "response.output_item.added",
				"output_index": outIdx,
				"item": map[string]any{
					"id":        callID,
					"type":      "function_call",
					"call_id":   callID,
					"name":      toolName,
					"arguments": "",
					"status":    "in_progress",
				},
			})
			if err := writeNamedSSE(p.w, p.flusher, "response.output_item.added", string(b)); err != nil {
				return err
			}
		}
		if args, ok := fn["arguments"].(string); ok && args != "" {
			p.respToolArgs[toolIdx].WriteString(args)
			outIdx := toolIdx
			if p.respItemOpen {
				outIdx = toolIdx + 1
			}
			b, _ := json.Marshal(map[string]any{
				"type":         "response.function_call_arguments.delta",
				"item_id":      p.respToolMeta[toolIdx]["id"],
				"output_index": outIdx,
				"delta":        args,
			})
			if err := writeNamedSSE(p.w, p.flusher, "response.function_call_arguments.delta", string(b)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *protoStream) responsesItemID() string {
	id := p.pin.ID
	if id == "" {
		id = "resp_proxy"
	}
	return id + "_item_0"
}

// ensureResponsesItemOpen emits the output_item.added + content_part.added
// lifecycle once, before the first output_text.delta. Codex CLI / OpenAI Agents
// SDK reject a delta for an item they never saw opened.
func (p *protoStream) ensureResponsesItemOpen() error {
	if p.respItemOpen {
		return nil
	}
	p.respItemOpen = true
	itemAdded, _ := json.Marshal(map[string]any{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item": map[string]any{
			"id":      p.responsesItemID(),
			"type":    "message",
			"role":    "assistant",
			"status":  "in_progress",
			"content": []any{},
		},
	})
	if err := writeNamedSSE(p.w, p.flusher, "response.output_item.added", string(itemAdded)); err != nil {
		return err
	}
	partAdded, _ := json.Marshal(map[string]any{
		"type":          "response.content_part.added",
		"item_id":       p.responsesItemID(),
		"output_index":  0,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
	return writeNamedSSE(p.w, p.flusher, "response.content_part.added", string(partAdded))
}

func (p *protoStream) ensureResponsesStart() error {
	if p.started {
		return nil
	}
	p.started = true
	// If the TTFT keepalive already sent response.created, skip re-sending.
	if p.pin != nil && p.pin.StartSent {
		return nil
	}
	return writeResponsesCreated(p.w, p.flusher, p.pin)
}

func (p *protoStream) finish() error {
	if p.finished {
		return nil
	}
	// A deferred Claude finish (blocks already closed, waiting on usage that never
	// came) must still flush its terminal delta+stop at [DONE].
	if p.pin.Proto == protoClaude && p.pendingStop != "" {
		return p.flushClaudeStop(p.pendingStop)
	}
	p.finished = true
	switch p.pin.Proto {
	case protoClaude:
		if err := p.closeAllClaudeBlocks(); err != nil {
			return err
		}
		delta, _ := json.Marshal(map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason": "end_turn",
			},
			"usage": p.claudeUsage(),
		})
		if err := writeNamedSSE(p.w, p.flusher, "message_delta", string(delta)); err != nil {
			return err
		}
		stop, _ := json.Marshal(map[string]any{"type": "message_stop"})
		return writeNamedSSE(p.w, p.flusher, "message_stop", string(stop))
	case protoResponses:
		id := p.pin.ID
		if id == "" {
			id = "resp_proxy"
		}
		// Close the text part/item lifecycle before the terminal events.
		if p.respItemOpen {
			textDone, _ := json.Marshal(map[string]any{
				"type":          "response.output_text.done",
				"item_id":       p.responsesItemID(),
				"output_index":  0,
				"content_index": 0,
				"text":          p.text.String(),
			})
			if err := writeNamedSSE(p.w, p.flusher, "response.output_text.done", string(textDone)); err != nil {
				return err
			}
			partDone, _ := json.Marshal(map[string]any{
				"type":          "response.content_part.done",
				"item_id":       p.responsesItemID(),
				"output_index":  0,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": p.text.String(), "annotations": []any{}},
			})
			if err := writeNamedSSE(p.w, p.flusher, "response.content_part.done", string(partDone)); err != nil {
				return err
			}
		}
		// Build the output array for the terminal response.completed event.
		output := []any{}
		if p.respItemOpen {
			itemDone, _ := json.Marshal(map[string]any{
				"type":         "response.output_item.done",
				"output_index": 0,
				"item": map[string]any{
					"id":     id + "_item_0",
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []any{
						map[string]any{"type": "output_text", "text": p.text.String(), "status": "completed"},
					},
				},
			})
			if err := writeNamedSSE(p.w, p.flusher, "response.output_item.done", string(itemDone)); err != nil {
				return err
			}
			output = append(output, map[string]any{
				"id":     id + "_item_0",
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []any{
					map[string]any{"type": "output_text", "text": p.text.String(), "status": "completed"},
				},
			})
		}
		// Close any open Responses tool-call streams.
		for idx, sb := range p.respToolArgs {
			meta := p.respToolMeta[idx]
			outIdx := idx
			if p.respItemOpen {
				outIdx = idx + 1
			}
			argsDone, _ := json.Marshal(map[string]any{
				"type":         "response.function_call_arguments.done",
				"item_id":      meta["id"],
				"output_index": outIdx,
				"arguments":    sb.String(),
			})
			if err := writeNamedSSE(p.w, p.flusher, "response.function_call_arguments.done", string(argsDone)); err != nil {
				return err
			}
			toolItem := map[string]any{
				"id":        meta["id"],
				"type":      "function_call",
				"call_id":   meta["id"],
				"name":      meta["name"],
				"arguments": sb.String(),
				"status":    "completed",
			}
			toolDone, _ := json.Marshal(map[string]any{
				"type":         "response.output_item.done",
				"output_index": outIdx,
				"item":         toolItem,
			})
			if err := writeNamedSSE(p.w, p.flusher, "response.output_item.done", string(toolDone)); err != nil {
				return err
			}
			output = append(output, toolItem)
		}
		status := "completed"
		if len(p.respToolArgs) > 0 {
			status = "requires_action"
		}
		if !p.respItemOpen && len(p.respToolArgs) == 0 {
			output = append(output, map[string]any{
				"id":     id + "_item_0",
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []any{
					map[string]any{"type": "output_text", "text": p.text.String(), "status": "completed"},
				},
			})
			itemDone, _ := json.Marshal(map[string]any{
				"type":         "response.output_item.done",
				"output_index": 0,
				"item":         output[0],
			})
			if err := writeNamedSSE(p.w, p.flusher, "response.output_item.done", string(itemDone)); err != nil {
				return err
			}
		}
		b, _ := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     id,
				"object": "response",
				"status": status,
				"model":  p.pin.Model,
				"output": output,
				"usage":  p.responsesUsage(),
			},
		})
		return writeNamedSSE(p.w, p.flusher, "response.completed", string(b))
	default:
		_, err := io.WriteString(p.w, "data: [DONE]\n\n")
		flushSSE(p.flusher)
		return err
	}
}

// claudeUsage converts OpenAI usage fields to Claude message_delta usage shape.
// Also surfaces cache tokens when upstream (some Huawei variants and OpenAI-
// compat providers) reports them via prompt_tokens_details / cached_tokens —
// Claude Code renders these in the context bar, so passing zeros hides real
// cache behavior from the user.
func (p *protoStream) claudeUsage() map[string]any {
	if p.usage == nil {
		return nil
	}
	out := map[string]any{}
	if v := asFloat(p.usage["prompt_tokens"]); v > 0 {
		out["input_tokens"] = int64(v)
	}
	if v := asFloat(p.usage["completion_tokens"]); v > 0 {
		out["output_tokens"] = int64(v)
	}
	if cached := extractCachedTokensFromUsage(p.usage); cached > 0 {
		out["cache_read_input_tokens"] = cached
	}
	if created := extractCacheCreationFromUsage(p.usage); created > 0 {
		out["cache_creation_input_tokens"] = created
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractCachedTokensFromUsage reads cache_read tokens from any of the common
// upstream shapes: {cached_tokens}, {prompt_tokens_details:{cached_tokens}},
// {prompt_cache_hit_tokens}. Distinct from extractCachedTokens (which parses
// raw JSON body); this one works off the already-parsed usage map.
func extractCachedTokensFromUsage(u map[string]any) int64 {
	if v := asFloat(u["cached_tokens"]); v > 0 {
		return int64(v)
	}
	if v := asFloat(u["prompt_cache_hit_tokens"]); v > 0 {
		return int64(v)
	}
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if v := asFloat(d["cached_tokens"]); v > 0 {
			return int64(v)
		}
	}
	return 0
}

// estimateFallbackUsage builds a synthetic usage map when upstream never sent
// a usage frame. output_tokens is derived from bytes-emitted / 4 (average byte-
// per-token for English/Chinese mix); input_tokens uses the pre-flight prompt
// estimate stored on the pin. Always non-nil so downstream Claude clients see
// a well-formed message_delta.usage instead of null.
func (p *protoStream) estimateFallbackUsage() map[string]any {
	out := map[string]any{}
	if p.pin != nil && p.pin.InputTokens > 0 {
		out["input_tokens"] = p.pin.InputTokens
	} else {
		out["input_tokens"] = int64(0)
	}
	outBytes := p.text.Len() + p.think.Len()
	tok := int64(outBytes / 4)
	if tok < 1 && outBytes > 0 {
		tok = 1
	}
	out["output_tokens"] = tok
	return out
}

func extractCacheCreationFromUsage(u map[string]any) int64 {
	if v := asFloat(u["cache_creation_input_tokens"]); v > 0 {
		return int64(v)
	}
	if v := asFloat(u["prompt_cache_miss_tokens"]); v > 0 {
		return int64(v)
	}
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if v := asFloat(d["cache_creation_input_tokens"]); v > 0 {
			return int64(v)
		}
	}
	return 0
}

// responsesUsage converts OpenAI usage fields to Responses API usage shape.
func (p *protoStream) responsesUsage() map[string]any {
	if p.usage == nil {
		return nil
	}
	out := map[string]any{}
	if v := asFloat(p.usage["prompt_tokens"]); v > 0 {
		out["input_tokens"] = int64(v)
	}
	if v := asFloat(p.usage["completion_tokens"]); v > 0 {
		out["output_tokens"] = int64(v)
	}
	if v := asFloat(p.usage["total_tokens"]); v > 0 {
		out["total_tokens"] = int64(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func openaiCompletionToClaude(body []byte, pin *ssePin) []byte {
	msg := extractAssistant(body)
	id := "msg_proxy"
	model := ""
	if pin != nil {
		if pin.ID != "" {
			id = pin.ID
		}
		model = pin.Model
	}
	content := []any{}
	if msg.Reasoning != "" {
		// 必须带 signature：Anthropic 的 ThinkingBlock 校验该字段，缺了
		// 客户端会把这条 assistant 消息从历史里丢掉，或 SDK 反序列化直接报错。
		// 与流式共用同一份确定性签名，保证重放时字节一致（prompt cache 稳定）。
		content = append(content, map[string]any{
			"type":      "thinking",
			"thinking":  msg.Reasoning,
			"signature": computeThinkSignature(msg.Reasoning),
		})
	}
	if msg.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": msg.Content})
	}
	toolN := 0
	for _, raw := range msg.ToolCalls {
		tc, _ := raw.(map[string]any)
		if tc == nil {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			continue
		}
		toolID, _ := tc["id"].(string)
		if toolID == "" {
			// Match the streaming path's scheme (toolu_proxy_<tool ordinal>) so a
			// synthetic id is stable regardless of stream vs non-stream response.
			toolID = fmt.Sprintf("toolu_proxy_%d", toolN)
		}
		toolN++
		var input any
		if args, _ := fn["arguments"].(string); args != "" {
			if json.Unmarshal([]byte(args), &input) != nil {
				input = map[string]any{}
			}
		} else {
			input = map[string]any{}
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    toolID,
			"name":  asString(fn["name"]),
			"input": input,
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	stopReason := "end_turn"
	switch msg.FinishReason {
	case "tool_calls", "tool_call", "function_call":
		stopReason = "tool_use"
	case "length":
		stopReason = "max_tokens"
	}
	out, _ := json.Marshal(map[string]any{
		"id":          id,
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     content,
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":  msg.Prompt,
			"output_tokens": msg.Completion,
		},
	})
	return out
}

func openaiCompletionToResponses(body []byte, pin *ssePin) []byte {
	msg := extractAssistant(body)
	id := "resp_proxy"
	model := ""
	if pin != nil {
		if pin.ID != "" {
			id = pin.ID
		}
		model = pin.Model
	}
	output := []any{}
	if msg.Content != "" || len(msg.ToolCalls) == 0 {
		output = append(output, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": msg.Content},
			},
		})
	}
	for _, raw := range msg.ToolCalls {
		tc, _ := raw.(map[string]any)
		if tc == nil {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			continue
		}
		callID, _ := tc["id"].(string)
		if callID == "" {
			callID = fmt.Sprintf("call_proxy_%d", len(output))
		}
		args, _ := fn["arguments"].(string)
		if args == "" {
			args = "{}"
		}
		output = append(output, map[string]any{
			"type":      "function_call",
			"call_id":   callID,
			"name":      asString(fn["name"]),
			"arguments": args,
		})
	}
	status := "completed"
	if len(msg.ToolCalls) > 0 {
		status = "requires_action"
	}
	out, _ := json.Marshal(map[string]any{
		"id":     id,
		"object": "response",
		"status": status,
		"model":  model,
		"output": output,
		"usage": map[string]any{
			"input_tokens":  msg.Prompt,
			"output_tokens": msg.Completion,
		},
	})
	return out
}

type extractedAssist struct {
	Content, Reasoning string
	ToolCalls          []any
	FinishReason       string
	Prompt, Completion float64
}

func extractAssistant(body []byte) extractedAssist {
	var obj map[string]any
	out := extractedAssist{}
	if json.Unmarshal(body, &obj) != nil {
		return out
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		out.Prompt = asFloat(u["prompt_tokens"])
		out.Completion = asFloat(u["completion_tokens"])
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return out
	}
	ch, _ := choices[0].(map[string]any)
	if ch == nil {
		return out
	}
	out.FinishReason, _ = ch["finish_reason"].(string)
	msg, _ := ch["message"].(map[string]any)
	if msg == nil {
		return out
	}
	out.Content = visibleText(msg["content"])
	out.Reasoning = firstThinkingString(msg)
	if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
		out.ToolCalls = tcs
	}
	return out
}
