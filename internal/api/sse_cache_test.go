package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type unexpectedEOFReader struct{}

func (unexpectedEOFReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestWriteSSEReasoningPingKeepsStreamID(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSSEReasoningPing(&buf, nil, "abc", "glm-5.2"); err != nil {
		t.Fatal(err)
	}
	raw := buf.String()
	if !strings.Contains(raw, ": keepalive") {
		t.Fatalf("missing comment: %q", raw)
	}
	var payload map[string]any
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("ping json: %v body=%q", err, body)
		}
	}
	if payload == nil {
		t.Fatal("missing data frame")
	}
	if payload["id"] != "abc" {
		t.Fatalf("id=%v want abc", payload["id"])
	}
	if payload["model"] != "glm-5.2" {
		t.Fatalf("model=%v", payload["model"])
	}
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("choices=%v", payload)
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	for _, field := range []string{"reasoning_content", "reasoning", "reasoning_text"} {
		val, _ := delta[field].(string)
		if val == "" {
			t.Fatalf("empty %s in %v", field, delta)
		}
	}
}

func TestConsumeSSEThinkDoneCompletes(t *testing.T) {
	src := strings.NewReader(`data:{"id":"abc","choices":[{"delta":{"reasoning_content":"step 1"}}]}` + "\n\n" + `data: [DONE]` + "\n\n")
	var out bytes.Buffer
	asm, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload + "|")
		return nil
	})
	if err != nil {
		t.Fatalf("huawei [DONE] must complete, err=%v", err)
	}
	if asm == nil || !asm.streamComplete() {
		t.Fatal("think + [DONE] is a finished upstream turn")
	}
	if !strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("must forward [DONE]: %s", out.String())
	}
	if !strings.Contains(out.String(), `"finish_reason":"stop"`) {
		t.Fatalf("must emit finish after think-only [DONE]: %s", out.String())
	}
}

func TestConsumeSSEToolCallsIsComplete(t *testing.T) {
	src := strings.NewReader(
		`data:{"id":"abc","choices":[{"delta":{"reasoning_content":"need file"}}]}` + "\n\n" +
			`data:{"id":"abc","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]}}]}` + "\n\n" +
			`data:{"id":"abc","choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	)
	var out bytes.Buffer
	asm, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload + "|")
		return nil
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if asm == nil || !asm.streamComplete() {
		t.Fatal("tool_calls must complete")
	}
	if !strings.Contains(out.String(), "[DONE]") || !strings.Contains(out.String(), "tool_calls") {
		t.Fatalf("must forward tool_calls: %s", out.String())
	}
}

func TestConsumeSSEToolFinishWithoutDeltaCompletes(t *testing.T) {
	src := strings.NewReader(
		`data:{"id":"abc","choices":[{"delta":{"reasoning_content":"need file"}}]}` + "\n\n" +
			`data:{"id":"abc","choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	)
	asm, err := consumeSSE(context.Background(), src, func(string) error { return nil })
	if err != nil {
		t.Fatalf("upstream finish+DONE must pass through: %v", err)
	}
	if asm == nil || !asm.sawDone || !asm.sawFinish {
		t.Fatal("must accept Huawei finish_reason + [DONE]")
	}
}

func TestConsumeSSEThinkFinishStopCompletes(t *testing.T) {
	src := strings.NewReader(`data:{"id":"abc","choices":[{"delta":{"reasoning_content":"step"}}]}` + "\n\n" + `data:{"id":"abc","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	var out bytes.Buffer
	asm, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload + "|")
		return nil
	})
	if err != nil {
		t.Fatalf("CLI/one-api forward finish=stop, err=%v", err)
	}
	if asm == nil || !asm.sawFinish {
		t.Fatal("finish_reason=stop must complete")
	}
	if !strings.Contains(out.String(), `"finish_reason":"stop"`) || !strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("must forward stop and [DONE]: %s", out.String())
	}
}

func TestCompletionJSONKeepsToolCalls(t *testing.T) {
	src := strings.NewReader(
		`data:{"id":"abc","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]}}]}` + "\n\n" +
			`data:{"id":"abc","choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	)
	asm, err := consumeSSE(context.Background(), src, func(string) error { return nil })
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	body, _, err := asm.completionJSON()
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	r, c, tools, finish := assembledStreamShape(body)
	if tools == 0 || finish != "tool_calls" || r != 0 || c != 0 {
		t.Fatalf("shape reason=%d content=%d tools=%d finish=%s body=%s", r, c, tools, finish, body)
	}
}

func TestConsumeSSEMidThinkEOFRetries(t *testing.T) {
	src := strings.NewReader(`data:{"id":"abc","choices":[{"delta":{"reasoning_content":"step 1"}}]}` + "\n\n")
	var out bytes.Buffer
	_, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload + "|")
		return nil
	})
	if err != errSSEPartialAbort {
		t.Fatalf("err=%v want partial abort (no second generation)", err)
	}
	if strings.Contains(out.String(), `"finish_reason":"stop"`) {
		t.Fatalf("must not fake stop mid-think: %s", out.String())
	}
}

func TestNormalizeThinkingAliases(t *testing.T) {
	in := []byte(`{"id":"x","choices":[{"delta":{"thinking":"逐步分析"}}]}`)
	out := normalizeThinkingPayload(in)
	if !strings.Contains(string(out), `"reasoning_content":"逐步分析"`) {
		t.Fatalf("want reasoning_content, got %s", out)
	}
}

func TestConsumeSSERewritesPromptTooLong(t *testing.T) {
	raw := `data: {"text":"[DONE]","error_code":"InferHub.ModelArts.81027.400","error_msg":"Prompt length exceeds: the prompt length 307782 must less than the maximum input length 307200"}` + "\n\n"
	var out []string
	_, err := consumeSSE(context.Background(), strings.NewReader(raw), func(payload string) error {
		out = append(out, payload)
		return nil
	})
	if !errors.Is(err, errHuaweiClient) {
		t.Fatalf("err=%v want huawei client", err)
	}
	if len(out) < 1 {
		t.Fatal("expected rewritten error frame")
	}
	var obj map[string]any
	if json.Unmarshal([]byte(out[0]), &obj) != nil {
		t.Fatalf("not json: %q", out[0])
	}
	errObj, _ := obj["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("want openai error object, got %v", obj)
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "Prompt length exceeds") {
		t.Fatalf("message=%q", msg)
	}
	if _, ok := obj["choices"]; ok {
		t.Fatalf("must not leak choices-less huawei frame: %v", obj)
	}
}

func TestConsumeSSEEarlyModelErrRetriesWithinBudget(t *testing.T) {
	raw := `data: {"text":"[DONE]","error_code":"InferHub.ModelArts.81027.400","error_msg":"Prompt length exceeds: the prompt length 307782 must less than the maximum input length 307200"}` + "\n\n"
	var out []string
	ctx := withEarlyModelErrBudget(context.Background(), 1)
	_, err := consumeSSE(ctx, strings.NewReader(raw), func(payload string) error {
		out = append(out, payload)
		return nil
	})
	if !errors.Is(err, errSSEEarlyModelErr) {
		t.Fatalf("err=%v want errSSEEarlyModelErr", err)
	}
	if errors.Is(err, errHuaweiClient) {
		t.Fatalf("err=%v must not also match errHuaweiClient (callers switch on that too)", err)
	}
	if len(out) != 0 {
		t.Fatalf("must not emit anything to the client while a retry is still possible: out=%v", out)
	}
}

func TestConsumeSSEEarlyModelErrZeroBudgetFallsBackToHuaweiClient(t *testing.T) {
	raw := `data: {"text":"[DONE]","error_code":"InferHub.ModelArts.81027.400","error_msg":"boom"}` + "\n\n"
	ctx := withEarlyModelErrBudget(context.Background(), 0)
	_, err := consumeSSE(ctx, strings.NewReader(raw), func(string) error { return nil })
	if !errors.Is(err, errHuaweiClient) {
		t.Fatalf("err=%v want errHuaweiClient once the retry budget is exhausted", err)
	}
	if errors.Is(err, errSSEEarlyModelErr) {
		t.Fatalf("err=%v must not retry once the budget is exhausted", err)
	}
}

func TestConsumeSSEEarlyModelErrNotFiredAfterFirstByte(t *testing.T) {
	raw := `data: {"id":"abc","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"Hi"}}]}` + "\n\n" +
		`data: {"text":"[DONE]","error_code":"InferHub.ModelArts.81027.400","error_msg":"boom"}` + "\n\n"
	ctx := withEarlyModelErrBudget(context.Background(), 3)
	var out []string
	_, err := consumeSSE(ctx, strings.NewReader(raw), func(payload string) error {
		out = append(out, payload)
		return nil
	})
	if !errors.Is(err, errHuaweiClient) {
		t.Fatalf("err=%v want errHuaweiClient once content already reached the client", err)
	}
	if errors.Is(err, errSSEEarlyModelErr) {
		t.Fatalf("err=%v must not silently retry after content was already emitted", err)
	}
	if len(out) == 0 {
		t.Fatal("the earlier content frame must still have been emitted before the error hit")
	}
}

func TestReadSSEAsCompletionJSONEarlyModelErrWithBudget(t *testing.T) {
	raw := `data:{"text":"[DONE]","error_code":"InferHub.ModelArts.81027.400","error_msg":"boom"}` + "\n\n"
	ctx := withEarlyModelErrBudget(context.Background(), 1)
	_, _, err := readSSEAsCompletionJSON(ctx, strings.NewReader(raw))
	if !errors.Is(err, errSSEEarlyModelErr) {
		t.Fatalf("err=%v want errSSEEarlyModelErr on the non-streaming path too", err)
	}
}

func TestConsumeSSEContentEOFIsComplete(t *testing.T) {
	src := strings.NewReader(`data:{"id":"abc","choices":[{"delta":{"content":"hi"}}]}` + "\n\n")
	_, err := consumeSSE(context.Background(), src, func(string) error { return nil })
	if err != nil {
		t.Fatalf("content eof should complete: %v", err)
	}
}

func TestConsumeSSEUnexpectedEOFRetriesEvenWithContent(t *testing.T) {
	src := io.MultiReader(
		strings.NewReader(`data:{"id":"abc","choices":[{"delta":{"content":"hi"}}]}`+"\n\n"),
		unexpectedEOFReader{},
	)
	_, err := consumeSSE(context.Background(), src, func(string) error { return nil })
	if err != nil {
		t.Fatalf("content already sent — do not retry: %v", err)
	}
}

func TestConsumeSSESalvagesCompleteTailWithoutNL(t *testing.T) {
	src := io.MultiReader(
		strings.NewReader(`data:{"id":"abc","choices":[{"delta":{"reasoning_content":"keep-me"}}]}`),
		unexpectedEOFReader{},
	)
	var out bytes.Buffer
	_, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload)
		return nil
	})
	if err != errSSEPartialAbort {
		t.Fatalf("err=%v want partial abort", err)
	}
	if !strings.Contains(out.String(), "keep-me") {
		t.Fatalf("last complete think frame must be forwarded: %s", out.String())
	}
}

func TestCoerceHuaweiTextThinkFrame(t *testing.T) {
	out := coerceOpenAIChunk([]byte(`{"text":"逐步拆解问题"}`), &sseAssembly{id: "abc"})
	if !strings.Contains(string(out), `"reasoning_content":"逐步拆解问题"`) {
		t.Fatalf("text frame must become reasoning: %s", out)
	}
	if !strings.Contains(string(out), `"choices"`) {
		t.Fatalf("must have choices: %s", out)
	}
}

func TestConsumeSSESalvagesLastThinkFrame(t *testing.T) {
	// Upstream often closes after the last think chunk with no trailing newline.
	src := io.MultiReader(
		strings.NewReader(`data: {"id":"abc","choices":[{"delta":{"reasoning_content":"step1"}}]}`+"\n\n"),
		strings.NewReader(`data: {"id":"abc","choices":[{"delta":{"reasoning_content":"step2"}}]}`),
		unexpectedEOFReader{},
	)
	var out bytes.Buffer
	_, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload)
		return nil
	})
	if err != errSSEPartialAbort {
		t.Fatalf("err=%v want partial abort", err)
	}
	if !strings.Contains(out.String(), "step2") {
		t.Fatalf("last think frame must be forwarded: %s", out.String())
	}
	if strings.Contains(out.String(), `"finish_reason":"stop"`) {
		t.Fatalf("must not fake stop: %s", out.String())
	}
}

func TestConsumeSSEDropsTornFrame(t *testing.T) {
	src := io.MultiReader(
		strings.NewReader(`data:{"id":"abc","choices":[{"delta":{"reasoning_content":"think"}}]}`+"\n\n"+`data:{"broken`),
		unexpectedEOFReader{},
	)
	var out bytes.Buffer
	_, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload)
		return nil
	})
	if err != errSSEPartialAbort {
		t.Fatalf("err=%v want partial abort", err)
	}
	if strings.Contains(out.String(), "broken") {
		t.Fatalf("torn frame leaked: %s", out.String())
	}
	if strings.Contains(out.String(), `"finish_reason":"stop"`) {
		t.Fatalf("must not fake stop on unexpected eof: %s", out.String())
	}
}

func TestRewriteSSEIdentityPinsID(t *testing.T) {
	got := rewriteSSEIdentity(`{"id":"new","choices":[{"delta":{"content":"x"}}]}`, "old", "glm-5.2")
	if !strings.Contains(got, `"id":"old"`) {
		t.Fatalf("pin id lost: %s", got)
	}
}

func TestPayloadHasVisibleDelta(t *testing.T) {
	if payloadHasVisibleDelta(`{"id":"abc","choices":[{"delta":{"role":"assistant"}}]}`) {
		t.Fatal("role-only must not start thinking pings")
	}
	if !payloadHasVisibleDelta(`{"id":"abc","choices":[{"delta":{"reasoning_content":"hmm"}}]}`) {
		t.Fatal("reasoning should count")
	}
}

func TestWriteSSECommentHasNoDataFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSSEComment(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "data:") {
		t.Fatalf("ttft ping must not invent thinking: %q", buf.String())
	}
}

func TestReadSSEAsCompletionJSON(t *testing.T) {
	src := strings.NewReader("" +
		"data:{\"id\":\"abc\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data:{\"id\":\"abc\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
		"data:{\"id\":\"abc\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
		"data:[DONE]\n\n")
	body, usage, err := readSSEAsCompletionJSON(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if usage != 5 {
		t.Fatalf("usage=%v want 5", usage)
	}
	if !bytes.Contains(body, []byte(`"content":"Hi"`)) {
		t.Fatalf("body=%s", body)
	}
}

func TestReadSSEPreservesCachedTokens(t *testing.T) {
	src := strings.NewReader("" +
		"data:{\"id\":\"abc\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
		"data:{\"id\":\"abc\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":2,\"total_tokens\":102,\"prompt_tokens_details\":{\"cached_tokens\":80},\"cached_tokens\":80}}\n\n" +
		"data:[DONE]\n\n")
	body, _, err := readSSEAsCompletionJSON(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	got := extractCachedTokens(body)
	if got != 80 {
		t.Fatalf("cached=%v want 80 body=%s", got, body)
	}
}

func TestConsumeSSEEmitsNormalizedFrames(t *testing.T) {
	src := strings.NewReader("data:{\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	var out bytes.Buffer
	_, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload + "|")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Fatal("expected emitted frames")
	}
}

// TestClaudeDeferredUsageReachesMessageDelta verifies the Gap-4 fix: Huawei sends
// finish_reason and the usage tally in separate frames (include_usage=true). The
// Claude message_delta must carry the trailing usage, not 0.
func TestClaudeDeferredUsageReachesMessageDelta(t *testing.T) {
	src := strings.NewReader(
		`data:{"id":"m1","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			`data:{"id":"m1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n" +
			`data:[DONE]` + "\n\n")
	rec := httptest.NewRecorder()
	_, _, _ = relayNormalizedSSEOpt(context.Background(), rec, src, true, &ssePin{ID: "m1", Model: "glm-5.2", Proto: protoClaude})
	body := rec.Body.String()
	i := strings.Index(body, "event: message_delta")
	if i < 0 {
		t.Fatalf("no message_delta: %s", body)
	}
	seg := body[i:]
	if !strings.Contains(seg, `"input_tokens":10`) || !strings.Contains(seg, `"output_tokens":5`) {
		t.Fatalf("message_delta must carry deferred usage, got: %s", seg)
	}
	if strings.Count(body, "event: message_stop") != 1 {
		t.Fatalf("want exactly one message_stop: %s", body)
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	// A real request yields a positive, ballpark-correct estimate (Huawei
	// reported prompt_tokens=18 for this exact prompt in production).
	body := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"say hi in 3 words"}]}`)
	if got := estimatePromptTokens(body); got < 8 || got > 40 {
		t.Fatalf("english estimate out of range, got %d", got)
	}
	// Base overhead keeps even an empty body non-zero.
	if got := estimatePromptTokens([]byte(`{}`)); got < 1 {
		t.Fatalf("base estimate must be >=1, got %d", got)
	}
	// CJK content is counted (heavier than ASCII) — must exceed base overhead.
	if got := estimatePromptTokens([]byte(`{"messages":[{"role":"user","content":"你好世界请给我写一首关于秋天的诗"}]}`)); got <= 8 {
		t.Fatalf("CJK content must add tokens above base, got %d", got)
	}
	// Nested content blocks + tool schemas (Claude Messages shape) are walked.
	rich := []byte(`{"system":[{"type":"text","text":"You are helpful."}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hello there friend"}]}],` +
		`"tools":[{"name":"get_weather","description":"Return the weather for a city"}]}`)
	if got := estimatePromptTokens(rich); got < 12 {
		t.Fatalf("rich body must count system+content-blocks+tools, got %d", got)
	}
	// Malformed JSON falls back to a byte-length heuristic, still positive.
	if got := estimatePromptTokens([]byte(`not json at all, but long enough`)); got <= 0 {
		t.Fatalf("fallback estimate must be > 0, got %d", got)
	}
}

// TestClaudeMessageStartCarriesInputTokens verifies the message_start envelope
// reports the estimated prompt tokens instead of a hardcoded 0, so Anthropic
// clients that read input/context size from message_start (Claude Code) see a
// real count. The exact upstream count still lands in message_delta.
func TestClaudeMessageStartCarriesInputTokens(t *testing.T) {
	src := strings.NewReader(
		`data:{"id":"m1","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			`data:{"id":"m1","choices":[],"usage":{"prompt_tokens":18,"completion_tokens":5,"total_tokens":23}}` + "\n\n" +
			`data:[DONE]` + "\n\n")
	rec := httptest.NewRecorder()
	// headersAlreadySent=true → ensureClaudeStart emits message_start from the pin.
	_, _, _ = relayNormalizedSSEOpt(context.Background(), rec, src, true,
		&ssePin{ID: "m1", Model: "glm-5.2", Proto: protoClaude, InputTokens: 123})
	body := rec.Body.String()
	i := strings.Index(body, "event: message_start")
	if i < 0 {
		t.Fatalf("no message_start: %s", body)
	}
	seg := body[i:]
	if j := strings.Index(seg[len("event: message_start"):], "event: "); j >= 0 {
		seg = seg[:len("event: message_start")+j]
	}
	if !strings.Contains(seg, `"input_tokens":123`) {
		t.Fatalf("message_start must carry estimated input_tokens=123, got: %s", seg)
	}
	// The exact upstream count still reaches message_delta (overwrites the estimate).
	if d := strings.Index(body, "event: message_delta"); d < 0 || !strings.Contains(body[d:], `"input_tokens":18`) {
		t.Fatalf("message_delta must carry exact input_tokens=18, got: %s", body)
	}
}

// TestConsumeSSEDropsPostFinishSecondGeneration verifies the terminal-stop guard:
// Huawei/GLM occasionally concatenates two generations (each with its own
// thinking) into ONE response body. Only the first may reach the client; any
// visible content after the first finish_reason must be dropped, or the client
// renders the same answer/thinking twice.
func TestConsumeSSEDropsPostFinishSecondGeneration(t *testing.T) {
	src := strings.NewReader(
		`data:{"id":"m1","choices":[{"delta":{"reasoning_content":"THINK-ONE"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{"content":"ANSWER-ONE"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{"reasoning_content":"THINK-TWO"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{"content":"ANSWER-TWO"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			`data:[DONE]` + "\n\n")
	var out bytes.Buffer
	_, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload + "|")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "ANSWER-ONE") || !strings.Contains(s, "THINK-ONE") {
		t.Fatalf("first generation must be emitted: %s", s)
	}
	if strings.Contains(s, "ANSWER-TWO") || strings.Contains(s, "THINK-TWO") {
		t.Fatalf("second generation after finish_reason must be dropped: %s", s)
	}
}

// TestConsumeSSEDropsMidStreamExactDuplicateChunk covers the gap the
// post-finish guard cannot reach: Huawei re-sending an already-emitted delta
// chunk verbatim with NO finish_reason/[DONE] boundary in between (observed
// live as a client rendering the same paragraph 2-3x within one turn).
func TestConsumeSSEDropsMidStreamExactDuplicateChunk(t *testing.T) {
	t.Setenv("SSE_DEDUP", "1")
	chunk := strings.Repeat("A", 40)
	src := strings.NewReader(
		`data:{"id":"m1","choices":[{"delta":{"content":"` + chunk + `"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{"content":"` + chunk + `"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			`data:[DONE]` + "\n\n")
	var out bytes.Buffer
	_, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload + "|")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), chunk); got != 1 {
		t.Fatalf("exact duplicate chunk must reach the client exactly once, got %d in: %s", got, out.String())
	}
}

// TestConsumeSSETrimsOverlappingDuplicateChunk covers a partial re-send: the
// second frame repeats the previous frame's full text as a prefix and tacks
// on genuinely new text — matches the observed pattern of a growing restated
// block. Only the new suffix should reach the client.
func TestConsumeSSETrimsOverlappingDuplicateChunk(t *testing.T) {
	t.Setenv("SSE_DEDUP", "1")
	head := strings.Repeat("X", 40)
	tail := "NEWTAIL123456"
	src := strings.NewReader(
		`data:{"id":"m1","choices":[{"delta":{"content":"` + head + `"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{"content":"` + head + tail + `"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			`data:[DONE]` + "\n\n")
	var out bytes.Buffer
	_, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload + "|")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if got := strings.Count(s, head); got != 1 {
		t.Fatalf("overlapping prefix must not be duplicated, got %d occurrences: %s", got, s)
	}
	if !strings.Contains(s, tail) {
		t.Fatalf("genuinely new suffix must still reach the client: %s", s)
	}
}

// TestConsumeSSEKeepsShortLegitimateRepeats proves the dedup guard never
// touches ordinary token-sized deltas, so normal streaming (a few bytes per
// SSE frame) and short natural repeats (e.g. "the the") are unaffected.
func TestConsumeSSEKeepsShortLegitimateRepeats(t *testing.T) {
	src := strings.NewReader(
		`data:{"id":"m1","choices":[{"delta":{"content":"the the "}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{"content":"the the "}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			`data:[DONE]` + "\n\n")
	var out bytes.Buffer
	_, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload + "|")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), `"the the "`); got != 2 {
		t.Fatalf("short repeated tokens below the dedup floor must both pass through, got %d: %s", got, out.String())
	}
}

// TestDedupKeepsFinishReasonWhenContentFullyDuplicated covers the unusual
// (not GLM's normal empty-delta-finish shape) case of a duplicate content
// chunk riding on the same frame as finish_reason. In practice the pre-existing
// sse_drop_post_finish guard fires first here (sawFinish flips true inside
// ingestPayload before this frame's own emit check runs) and drops the whole
// frame outright; the end-of-stream emittedFinish fallback then synthesizes
// the terminal finish_reason. Either mechanism is acceptable — what must
// always hold is the end-to-end guarantee this test checks: the duplicated
// content never reaches the client twice, and finish_reason still arrives.
func TestDedupKeepsFinishReasonWhenContentFullyDuplicated(t *testing.T) {
	t.Setenv("SSE_DEDUP", "1")
	chunk := strings.Repeat("B", 40)
	src := strings.NewReader(
		`data:{"id":"m1","choices":[{"delta":{"content":"` + chunk + `"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{"content":"` + chunk + `"},"finish_reason":"stop"}]}` + "\n\n" +
			`data:[DONE]` + "\n\n")
	var out bytes.Buffer
	_, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload + "|")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if got := strings.Count(s, chunk); got != 1 {
		t.Fatalf("duplicated content must still be dropped even when finish_reason rides along, got %d: %s", got, s)
	}
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason must still reach the client even though content was fully deduped: %s", s)
	}
}

// TestPostFinishGuardKeepsDeferredUsage proves the terminal-stop guard drops a
// second generation's content yet still lets a trailing usage-only frame reach
// the Claude message_delta (usage frames carry no visible delta, so they pass).
func TestPostFinishGuardKeepsDeferredUsage(t *testing.T) {
	src := strings.NewReader(
		`data:{"id":"m1","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			`data:{"id":"m1","choices":[{"delta":{"content":"AGAIN"}}]}` + "\n\n" +
			`data:{"id":"m1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n" +
			`data:[DONE]` + "\n\n")
	rec := httptest.NewRecorder()
	_, _, _ = relayNormalizedSSEOpt(context.Background(), rec, src, true, &ssePin{ID: "m1", Model: "glm-5.2", Proto: protoClaude})
	body := rec.Body.String()
	if !strings.Contains(body, "hi") {
		t.Fatalf("first generation content must reach client: %s", body)
	}
	if strings.Contains(body, "AGAIN") {
		t.Fatalf("second generation after finish must be dropped: %s", body)
	}
	i := strings.Index(body, "event: message_delta")
	if i < 0 || !strings.Contains(body[i:], `"output_tokens":5`) {
		t.Fatalf("deferred usage must still reach message_delta after a dropped dup: %s", body)
	}
}

func TestRelayKeepsMidThinkOnTheWire(t *testing.T) {
	src := strings.NewReader(`data:{"id":"abc","choices":[{"delta":{"reasoning_content":"step 1"}}]}` + "\n\n")
	rec := httptest.NewRecorder()
	_, _, err := relayNormalizedSSEOpt(context.Background(), rec, src, true, &ssePin{ID: "abc", Model: "glm-5.2"})
	if !errors.Is(err, errSSEWired) {
		t.Fatalf("err=%v want wired", err)
	}
	if !strings.Contains(rec.Body.String(), "step 1") {
		t.Fatalf("thinking must stay on the client stream: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("must not fake stop mid-think: %s", rec.Body.String())
	}
}

func TestWireSSEErrBlocksRetryAfterThink(t *testing.T) {
	asm := &sseAssembly{emittedVisible: true}
	err := wireSSEErr(asm, errSSEPartialAbort)
	if !errors.Is(err, errSSEWired) || !errors.Is(err, errSSEPartialAbort) {
		t.Fatalf("err=%v", err)
	}
	if wireSSEErr(&sseAssembly{}, errSSEPartialAbort) != errSSEPartialAbort {
		t.Fatal("silent stream must stay retryable")
	}
}

func TestShouldRetrySSECutOnlyWhenSilent(t *testing.T) {
	if !shouldRetrySSECut(&sseAssembly{}, true) {
		t.Fatal("empty unexpected eof should retry")
	}
	think := &sseAssembly{}
	think.reasoning.WriteString("step")
	if shouldRetrySSECut(think, true) {
		t.Fatal("thinking already sent must not start a second generation")
	}
	ans := &sseAssembly{}
	ans.content.WriteString("hi")
	if shouldRetrySSECut(ans, true) {
		t.Fatal("content already sent must not retry")
	}
}

func TestConsumeSSEEmptyUnexpectedEOFRetries(t *testing.T) {
	_, err := consumeSSE(context.Background(), unexpectedEOFReader{}, nil)
	if err != errSSEUpstreamCut {
		t.Fatalf("err=%v want cut", err)
	}
}

func TestConsumeSSECancelClosesUpstream(t *testing.T) {
	r, w := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := consumeSSE(ctx, r, func(string) error { return nil })
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !isCancelErr(err) {
			t.Fatalf("err=%v want cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumeSSE did not abort after client cancel")
	}
	_ = w.Close()
}

func TestConsumeSSEThinkLoopWithoutDoneIsIncomplete(t *testing.T) {
	var src strings.Builder
	for i := 0; i < 8; i++ {
		src.WriteString(`data:{"id":"abc","choices":[{"delta":{"reasoning_content":"思考中"}}]}` + "\n\n")
	}
	var out bytes.Buffer
	_, err := consumeSSE(context.Background(), strings.NewReader(src.String()), func(payload string) error {
		out.WriteString(payload)
		return nil
	})
	if err != errSSEPartialAbort {
		t.Fatalf("placeholder EOF without [DONE] is a cut, err=%v", err)
	}
	if strings.Contains(out.String(), "思考中") {
		t.Fatalf("must not forward placeholder: %s", out.String())
	}
}

func TestConsumeSSEDropsSinglePlaceholder(t *testing.T) {
	src := strings.NewReader(
		`data:{"id":"abc","choices":[{"delta":{"reasoning_content":"思考中"}}]}` + "\n\n" +
			`data:{"id":"abc","choices":[{"delta":{"reasoning_content":"先看文件"}}]}` + "\n\n" +
			`data:{"id":"abc","choices":[{"delta":{"content":"ok"}}]}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	)
	var out bytes.Buffer
	asm, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload + "|")
		return nil
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(out.String(), "思考中") {
		t.Fatalf("placeholder leaked: %s", out.String())
	}
	if !strings.Contains(out.String(), "先看文件") {
		t.Fatalf("real think dropped: %s", out.String())
	}
	if asm.reasoning.String() != "先看文件" {
		t.Fatalf("assembly=%q", asm.reasoning.String())
	}
}

func TestConsumeSSETruncatedToolArgsWithDoneCompletes(t *testing.T) {
	src := strings.NewReader(
		`data:{"id":"abc","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Read","arguments":"{\"path\""}}]}}]}` + "\n\n" +
			`data:{"id":"abc","choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	)
	_, err := consumeSSE(context.Background(), src, func(string) error { return nil })
	if err != nil {
		t.Fatalf("Huawei [DONE] must pass through even if args are torn: %v", err)
	}
}

func TestConsumeSSEStopWithToolsEmitsToolFinish(t *testing.T) {
	src := strings.NewReader(
		`data:{"id":"abc","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]}}]}` + "\n\n" +
			`data:{"id":"abc","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	)
	var out bytes.Buffer
	asm, err := consumeSSE(context.Background(), src, func(payload string) error {
		out.WriteString(payload + "|")
		return nil
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !asm.hasToolCall() {
		t.Fatal("tools lost")
	}
	if !strings.Contains(out.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("must rewrite stop to tool_calls: %s", out.String())
	}
}
