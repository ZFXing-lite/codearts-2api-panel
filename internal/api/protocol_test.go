package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteProtoEnvelopeClaudeBeforePing(t *testing.T) {
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_1", Model: "GLM-5.2", Proto: protoClaude}
	stop := startSSEKeepalive(rec, pin, nil)
	stop()
	out := rec.Body.String()
	if !pin.StartSent {
		t.Fatal("StartSent")
	}
	iStart := strings.Index(out, "event: message_start")
	iPing := strings.Index(out, "event: ping")
	if iStart < 0 || iPing < 0 || iStart > iPing {
		t.Fatalf("want message_start then ping, got %q", out)
	}
	if strings.Count(out, "event: message_start") != 1 {
		t.Fatalf("duplicate envelope: %q", out)
	}
}

func TestWriteProtoEnvelopeSkippedWhenDisabled(t *testing.T) {
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_1", Model: "GLM-5.2", Proto: protoClaude, SkipEnvelope: true}
	stop := startSSEKeepalive(rec, pin, nil)
	stop()
	out := rec.Body.String()
	if pin.StartSent {
		t.Fatal("SkipEnvelope must not set StartSent")
	}
	if strings.Contains(out, "event: message_start") {
		t.Fatalf("EARLY_ENVELOPE=0 must be bare ping, got %q", out)
	}
	if !strings.Contains(out, "event: ping") {
		t.Fatalf("want ping, got %q", out)
	}
}

func TestDetectClientProto(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if detectClientProto(req) != protoClaude {
		t.Fatal("messages")
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if detectClientProto(req) != protoResponses {
		t.Fatal("responses")
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if detectClientProto(req) != protoChat {
		t.Fatal("chat")
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("anthropic-version", "2023-06-01")
	if detectClientProto(req) != protoClaude {
		t.Fatal("anthropic header")
	}
}

func TestSanitizeClaudeSystemAndTools(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"sum","input_schema":{"type":"object"}}]}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["system"]; ok {
		t.Fatalf("system leaked: %s", out)
	}
	msgs, _ := root["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("want system+user, got %s", out)
	}
	tools, _ := root["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%s", out)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Fatalf("tool=%v", tool)
	}
}

func TestEncodeProtoBodyClaude(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi","reasoning_content":"think"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	out := encodeProtoBody(protoClaude, in, &ssePin{ID: "msg_1", Model: "GLM-5.2"})
	if !strings.Contains(string(out), `"type":"message"`) || !strings.Contains(string(out), `"thinking"`) {
		t.Fatalf("%s", out)
	}
	out = encodeProtoBody(protoResponses, in, &ssePin{ID: "resp_1", Model: "GLM-5.2"})
	if !strings.Contains(string(out), `"object":"response"`) || !strings.Contains(string(out), "output_text") {
		t.Fatalf("%s", out)
	}
}

func TestVisibleTextFromParts(t *testing.T) {
	if got := visibleText([]any{map[string]any{"type": "text", "text": "hello"}}); got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestEmitErrorClaudeSurfacesUpstreamError(t *testing.T) {
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_1", Model: "GLM-5.2", Proto: protoClaude}
	p := newProtoStream(rec, pin)
	if err := p.emit(openaiErrPayload("context_length_exceeded", "Prompt length exceeds: the prompt length 308477 must less than the maximum input length 307200")); err != nil {
		t.Fatalf("emit error: %v", err)
	}
	if !p.finished {
		t.Fatal("emitError must mark stream finished")
	}
	// A trailing [DONE] (as sent by consumeSSE after an in-band error) must be
	// a no-op, not a second, contradictory "clean" termination.
	if err := p.emit("[DONE]"); err != nil {
		t.Fatalf("emit [DONE] after error: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: message_start") {
		t.Fatalf("want message_start envelope before error, got %q", out)
	}
	if !strings.Contains(out, "event: error") {
		t.Fatalf("want event: error, got %q", out)
	}
	if !strings.Contains(out, "Prompt length exceeds") {
		t.Fatalf("want upstream message surfaced, got %q", out)
	}
	if strings.Contains(out, "message_stop") {
		t.Fatalf("must not also emit a clean message_stop after an error, got %q", out)
	}
}

func TestEmitErrorResponsesSurfacesUpstreamError(t *testing.T) {
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "resp_1", Model: "GLM-5.2", Proto: protoResponses}
	p := newProtoStream(rec, pin)
	if err := p.emit(openaiErrPayload("context_length_exceeded", "Prompt length exceeds: too long")); err != nil {
		t.Fatalf("emit error: %v", err)
	}
	if err := p.emit("[DONE]"); err != nil {
		t.Fatalf("emit [DONE] after error: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "response.failed") {
		t.Fatalf("want response.failed, got %q", out)
	}
	if !strings.Contains(out, "Prompt length exceeds") {
		t.Fatalf("want upstream message surfaced, got %q", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("must not also emit response.completed after a failure, got %q", out)
	}
}

func TestEmitErrorIgnoredWhenAlreadyFinished(t *testing.T) {
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_1", Model: "GLM-5.2", Proto: protoClaude}
	p := newProtoStream(rec, pin)
	if err := p.emit(`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`); err != nil {
		t.Fatalf("emit delta: %v", err)
	}
	if err := p.emit("[DONE]"); err != nil {
		t.Fatalf("emit [DONE]: %v", err)
	}
	if !p.finished {
		t.Fatal("normal finish must set finished")
	}
	before := rec.Body.String()
	if err := p.emit(openaiErrPayload("x", "late error after finish")); err != nil {
		t.Fatalf("emit late error: %v", err)
	}
	if rec.Body.String() != before {
		t.Fatal("error arriving after a normal finish must not append more events")
	}
}

func TestOpenaiCompletionToClaudeToolCalls(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}],"reasoning_content":"thinking hard"},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	out := openaiCompletionToClaude(in, &ssePin{ID: "msg_tc", Model: "GLM-5.2"})
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp["stop_reason"] != "tool_use" {
		t.Fatalf("want stop_reason=tool_use, got %v", resp["stop_reason"])
	}
	content, _ := resp["content"].([]any)
	if len(content) < 2 {
		t.Fatalf("want thinking+tool_use, got %d blocks: %s", len(content), out)
	}
	var foundToolUse bool
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		if block == nil {
			continue
		}
		if asString(block["type"]) == "tool_use" {
			foundToolUse = true
			if asString(block["id"]) != "call_1" {
				t.Fatalf("tool id: %v", block["id"])
			}
			if asString(block["name"]) != "bash" {
				t.Fatalf("tool name: %v", block["name"])
			}
			inp, _ := block["input"].(map[string]any)
			if inp == nil || asString(inp["cmd"]) != "ls" {
				t.Fatalf("tool input: %v", block["input"])
			}
		}
	}
	if !foundToolUse {
		t.Fatalf("no tool_use block in content: %s", out)
	}
}

func TestOpenaiCompletionToClaudeStopReasons(t *testing.T) {
	cases := []struct {
		finish string
		want   string
	}{
		{"stop", "end_turn"},
		{"tool_calls", "tool_use"},
		{"length", "max_tokens"},
		{"", "end_turn"},
	}
	for _, tc := range cases {
		in, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": "hi"},
				"finish_reason": tc.finish,
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
		out := openaiCompletionToClaude(in, &ssePin{ID: "msg_sr", Model: "GLM-5.2"})
		var resp map[string]any
		json.Unmarshal(out, &resp)
		if resp["stop_reason"] != tc.want {
			t.Fatalf("finish_reason=%q: want stop_reason=%q, got %v", tc.finish, tc.want, resp["stop_reason"])
		}
	}
}

func TestOpenaiCompletionToResponsesToolCalls(t *testing.T) {
	in := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	out := openaiCompletionToResponses(in, &ssePin{ID: "resp_tc", Model: "GLM-5.2"})
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "requires_action" {
		t.Fatalf("want status=requires_action, got %v", resp["status"])
	}
	output, _ := resp["output"].([]any)
	var foundFnCall bool
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		if item["type"] == "function_call" {
			foundFnCall = true
			if item["name"] != "bash" {
				t.Fatalf("want name=bash, got %v", item["name"])
			}
			if item["call_id"] != "call_1" {
				t.Fatalf("want call_id=call_1, got %v", item["call_id"])
			}
		}
	}
	if !foundFnCall {
		t.Fatalf("no function_call in output: %s", out)
	}
}

func TestClaudeToolBlockIndexPinnedAcrossTextOpen(t *testing.T) {
	// Reproduce the drift: a tool block opens BEFORE any visible text, then text
	// opens, then a further arguments-delta arrives for the same tool. The delta
	// must land on the tool's ORIGINAL block index (0), not a recomputed index
	// (1) that would collide with the text block.
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_x", Model: "GLM-5.2", Proto: protoClaude, StartSent: true}
	p := newProtoStream(rec, pin)
	// 1) tool_calls first (opens tool_use at block index 0)
	if err := p.emit(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"bash","arguments":""}}]}}]}`); err != nil {
		t.Fatal(err)
	}
	// 2) visible text (opens text block AFTER the tool → index 1)
	if err := p.emit(`{"choices":[{"delta":{"content":"working"}}]}`); err != nil {
		t.Fatal(err)
	}
	// 3) more tool arguments for the SAME tool (must reuse block index 0)
	if err := p.emit(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}`); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	// Parse every SSE data frame as JSON and inspect the events by type/index.
	var toolStartIdx, argsDeltaIdx, textStartIdx = -1, -1, -1
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line[len("data:"):])), &ev) != nil {
			continue
		}
		idx := -1
		if f, ok := ev["index"].(float64); ok {
			idx = int(f)
		}
		switch ev["type"] {
		case "content_block_start":
			cb, _ := ev["content_block"].(map[string]any)
			if cb != nil && cb["type"] == "tool_use" {
				toolStartIdx = idx
			} else if cb != nil && cb["type"] == "text" {
				textStartIdx = idx
			}
		case "content_block_delta":
			d, _ := ev["delta"].(map[string]any)
			if d != nil && d["type"] == "input_json_delta" {
				argsDeltaIdx = idx
			}
		}
	}
	if toolStartIdx != 0 {
		t.Fatalf("tool_use block must open at index 0, got %d: %q", toolStartIdx, out)
	}
	if textStartIdx != 1 {
		t.Fatalf("text block must open at index 1 (after tool), got %d: %q", textStartIdx, out)
	}
	if argsDeltaIdx != 0 {
		t.Fatalf("arguments delta must stay pinned to tool index 0, drifted to %d (would collide with text): %q", argsDeltaIdx, out)
	}
}

func TestClaudeThinkingClosedBeforeToolBlocks(t *testing.T) {
	// Reproduce the prod pattern: reasoning=10626 content=0 tools=5. The model
	// sends reasoning_content followed by tool_calls with NO visible text in
	// between. The thinking block (index 0) must be CLOSED (content_block_stop)
	// BEFORE the first tool_use block (content_block_start) is emitted. Without
	// this, Claude Code receives overlapping blocks and mis-assembles tool args.
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_thk", Model: "GLM-5.2", Proto: protoClaude, StartSent: true}
	p := newProtoStream(rec, pin)
	// reasoning first
	if err := p.emit(`{"choices":[{"delta":{"reasoning_content":"analyzing the request"}}]}`); err != nil {
		t.Fatal(err)
	}
	// tool_calls directly after reasoning — no visible content
	if err := p.emit(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":""}}]}}]}`); err != nil {
		t.Fatal(err)
	}
	if err := p.emit(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"ls\"}"}}]}}]}`); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()

	thinkStart := strings.Index(out, `"type":"thinking"`)
	thinkStop := strings.Index(out, `"index":0,"type":"content_block_stop"`)
	toolStart := strings.Index(out, `"type":"tool_use"`)

	if thinkStart < 0 || thinkStop < 0 || toolStart < 0 {
		t.Fatalf("missing expected events: thinkStart=%d thinkStop=%d toolStart=%d\n%s",
			thinkStart, thinkStop, toolStart, out)
	}
	if thinkStop > toolStart {
		t.Fatalf("thinking block must be closed BEFORE tool_use block opens:\n"+
			"thinkStop@%d toolStart@%d\n%s", thinkStop, toolStart, out)
	}
}

func TestClaudeLateReasoningAfterToolsNotReemitted(t *testing.T) {
	// If upstream sends reasoning_content AFTER tool_calls have started,
	// the thinking block must NOT be re-opened. The late reasoning should be
	// accumulated silently (for token accounting) but not streamed.
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_lr", Model: "GLM-5.2", Proto: protoClaude, StartSent: true}
	p := newProtoStream(rec, pin)
	if err := p.emit(`{"choices":[{"delta":{"reasoning_content":"initial thought"}}]}`); err != nil {
		t.Fatal(err)
	}
	if err := p.emit(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]}}]}`); err != nil {
		t.Fatal(err)
	}
	// Late reasoning AFTER tools started
	if err := p.emit(`{"choices":[{"delta":{"reasoning_content":"late thought after tools"}}]}`); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	// Count thinking_delta events — should only contain "initial thought", not "late thought"
	thinkDeltas := strings.Count(out, "thinking_delta")
	if thinkDeltas != 1 {
		t.Fatalf("expected exactly 1 thinking_delta (before tools), got %d:\n%s", thinkDeltas, out)
	}
	// Count content_block_start with thinking — must be exactly 1
	thinkStarts := strings.Count(out, `"type":"thinking"`)
	if thinkStarts != 1 {
		t.Fatalf("expected exactly 1 thinking block start, got %d (late reopen?):\n%s", thinkStarts, out)
	}
}

func TestWriteSSEThinkLoopExhaustedClaude(t *testing.T) {
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_tl", Model: "GLM-5.2", Proto: protoClaude, StartSent: true}
	writeSSEThinkLoopExhausted(rec, pin)
	out := rec.Body.String()
	// The dangling thinking block (index 0) must be closed.
	if !strings.Contains(out, `"type":"content_block_stop","index":0`) {
		t.Fatalf("want thinking block (index 0) closed, got %q", out)
	}
	// A fresh visible text block must be opened, filled, and closed at index 1.
	if !strings.Contains(out, `"content_block":{"text":"","type":"text"}`) &&
		!strings.Contains(out, `"type":"text"`) {
		t.Fatalf("want a text content_block_start, got %q", out)
	}
	if !strings.Contains(out, "text_delta") {
		t.Fatalf("want a visible notice via text_delta, got %q", out)
	}
	// Notice must be a neutral non-empty marker, NOT the old fabricated
	// "kept generating internal reasoning" prose (that read like model output).
	if strings.Contains(out, "kept generating") {
		t.Fatalf("must not fabricate model-like text, got %q", out)
	}
	if !strings.Contains(out, `"type":"content_block_stop","index":1`) {
		t.Fatalf("want text block (index 1) closed, got %q", out)
	}
	// Terminal must be a clean, NON-empty message_stop (output_tokens>0) so the
	// client does not see an empty turn and auto-retry.
	if !strings.Contains(out, "message_delta") || !strings.Contains(out, "message_stop") {
		t.Fatalf("want message_delta+message_stop, got %q", out)
	}
	if strings.Contains(out, `"output_tokens":0`) {
		t.Fatalf("output_tokens must be > 0, got %q", out)
	}
	// Ordering: thinking close precedes text open precedes message_stop.
	iThinkClose := strings.Index(out, `"content_block_stop","index":0`)
	iTextOpen := strings.Index(out, "content_block_start")
	iStop := strings.Index(out, "message_stop")
	if !(iThinkClose >= 0 && iTextOpen > iThinkClose && iStop > iTextOpen) {
		t.Fatalf("bad event ordering: thinkClose=%d textOpen=%d stop=%d in %q", iThinkClose, iTextOpen, iStop, out)
	}
}

func TestClaudeToolBlockIndexDenseAcrossSparseUpstream(t *testing.T) {
	// Upstream emits tool indices 0 then 2 (sparse — index 1 skipped). Claude
	// clients require contiguous block indices, so the two tools must map to dense
	// blocks 0 and 1, NOT 0 and 2 (which would leave a gap the client can't fill).
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_sparse", Model: "GLM-5.2", Proto: protoClaude, StartSent: true}
	p := newProtoStream(rec, pin)
	if err := p.emit(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"a","arguments":""}}]}}]}`); err != nil {
		t.Fatal(err)
	}
	if err := p.emit(`{"choices":[{"delta":{"tool_calls":[{"index":2,"id":"c2","type":"function","function":{"name":"b","arguments":""}}]}}]}`); err != nil {
		t.Fatal(err)
	}
	// arguments for the second (sparse index 2) tool must land on dense block 1
	if err := p.emit(`{"choices":[{"delta":{"tool_calls":[{"index":2,"function":{"arguments":"{\"x\":1}"}}]}}]}`); err != nil {
		t.Fatal(err)
	}
	var starts []int
	argsIdx := -1
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line[len("data:"):])), &ev) != nil {
			continue
		}
		idx := -1
		if f, ok := ev["index"].(float64); ok {
			idx = int(f)
		}
		switch ev["type"] {
		case "content_block_start":
			if cb, _ := ev["content_block"].(map[string]any); cb != nil && cb["type"] == "tool_use" {
				starts = append(starts, idx)
			}
		case "content_block_delta":
			if d, _ := ev["delta"].(map[string]any); d != nil && d["type"] == "input_json_delta" {
				argsIdx = idx
			}
		}
	}
	if len(starts) != 2 || starts[0] != 0 || starts[1] != 1 {
		t.Fatalf("tool blocks must open at dense indices [0,1], got %v: %q", starts, rec.Body.String())
	}
	if argsIdx != 1 {
		t.Fatalf("args for sparse upstream index 2 must land on dense block 1, got %d: %q", argsIdx, rec.Body.String())
	}
}

func TestFinishClaudeThinkOnlyDoesNotFabricateText(t *testing.T) {
	// A reasoning-only turn (thinking block, no visible text, no tool_use) is a
	// LEGAL Anthropic Messages API shape. dsh's chat 1:1 forward works because
	// the chat client renders reasoning_content directly. For Claude Code, the
	// gateway MUST NOT fabricate a text block — neither a hard-coded placeholder
	// nor a copy of the reasoning tail. Any auto-retry behaviour on empty output
	// is a client-side concern.
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_mt", Model: "GLM-5.2", Proto: protoClaude, StartSent: true}
	p := newProtoStream(rec, pin)
	if err := p.emit(`{"choices":[{"delta":{"reasoning_content":"thinking and thinking"}}]}`); err != nil {
		t.Fatal(err)
	}
	if err := p.emit(`{"choices":[{"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	if strings.Contains(out, "text_delta") {
		t.Fatalf("thinking-only turn must NOT fabricate a text_delta, got %q", out)
	}
	if strings.Contains(out, "no visible output") {
		t.Fatalf("thinking-only turn must NOT inject a placeholder notice, got %q", out)
	}
	if !strings.Contains(out, `"stop_reason":"max_tokens"`) {
		t.Fatalf("want stop_reason max_tokens in terminal, got %q", out)
	}
	if !strings.Contains(out, "thinking_delta") {
		t.Fatalf("reasoning content must still stream as thinking_delta, got %q", out)
	}
}

func TestConsumeSSEArrayContentIsComplete(t *testing.T) {
	src := strings.NewReader(`data:{"id":"abc","choices":[{"delta":{"content":[{"type":"text","text":"ok"}]}}]}` + "\n\n" + `data: [DONE]` + "\n\n")
	asm, err := consumeSSE(context.Background(), src, func(string) error { return nil })
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if asm == nil || !asm.streamComplete() {
		t.Fatal("array content must count as answer")
	}
}
