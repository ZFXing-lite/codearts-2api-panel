package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeCLIChatBodyStripsIntercept(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"ses_abc","stream_options":{"include_usage":true},"tools":[{"type":"function"}]}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["prompt_cache_key"]; ok {
		t.Fatal("must not send CLI intercept prompt_cache_key")
	}
	if _, ok := root["stream_options"]; !ok {
		t.Fatal("stream_options must be preserved for usage tracking")
	}
	if root["model"] != "GLM-5.2" {
		t.Fatalf("model lost: %v", root["model"])
	}
	msgs, _ := root["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages mutated/lost: %v", root["messages"])
	}
	if _, ok := root["tools"]; !ok {
		t.Fatal("tools must pass through")
	}
}

func TestSanitizeCLIChatBodyKeepsCleanBody(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":32}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	// Simple request without tools — thinking should NOT be forced.
	if th, _ := root["thinking"].(map[string]any); th != nil && asString(th["type"]) == "enabled" {
		t.Fatalf("must not force thinking for simple chat: %s", out)
	}
	if root["model"] != "GLM-5.2" || root["stream"] != true {
		t.Fatalf("clean fields lost: %s", out)
	}
}

func TestSanitizeCLIChatBodyInjectsThinkingForTools(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true,"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	th, _ := root["thinking"].(map[string]any)
	if asString(th["type"]) != "enabled" {
		t.Fatalf("must inject thinking when tools present: %s", out)
	}
}

func TestSanitizeMapsClaudeToolChoice(t *testing.T) {
	cases := map[string]any{
		`{"model":"GLM-5.2","messages":[],"tool_choice":{"type":"any"}}`:  "required",
		`{"model":"GLM-5.2","messages":[],"tool_choice":{"type":"auto"}}`: "auto",
		`{"model":"GLM-5.2","messages":[],"tool_choice":{"type":"none"}}`: "none",
	}
	for in, want := range cases {
		out := sanitizeCLIChatBody([]byte(in))
		var root map[string]any
		if err := json.Unmarshal(out, &root); err != nil {
			t.Fatal(err)
		}
		if root["tool_choice"] != want {
			t.Fatalf("tool_choice=%v want %v for %s", root["tool_choice"], want, in)
		}
	}
	// {type:tool,name} → {type:function,function:{name}} when the tool is declared.
	out := sanitizeCLIChatBody([]byte(`{"model":"GLM-5.2","messages":[],"tools":[{"name":"Read","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"Read"}}`))
	var root map[string]any
	_ = json.Unmarshal(out, &root)
	tc, _ := root["tool_choice"].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if asString(tc["type"]) != "function" || asString(fn["name"]) != "Read" {
		t.Fatalf("tool_choice tool form=%v", root["tool_choice"])
	}
	// Undeclared named tool is FORWARDED (no silent auto-rewrite). Pure
	// forwarding rule: log the mismatch, let upstream decide. The type is
	// still mapped Claude→OpenAI so the upstream sees a valid schema.
	out = sanitizeCLIChatBody([]byte(`{"model":"GLM-5.2","messages":[],"tools":[{"name":"Read","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"Ghost"}}`))
	root = map[string]any{}
	_ = json.Unmarshal(out, &root)
	tc, _ = root["tool_choice"].(map[string]any)
	fn, _ = tc["function"].(map[string]any)
	if asString(tc["type"]) != "function" || asString(fn["name"]) != "Ghost" {
		t.Fatalf("undeclared tool_choice must be forwarded verbatim, got %v", root["tool_choice"])
	}
	// Already-OpenAI function form must be preserved, not clobbered to auto.
	out = sanitizeCLIChatBody([]byte(`{"model":"GLM-5.2","messages":[],"tools":[{"type":"function","function":{"name":"Read","parameters":{}}}],"tool_choice":{"type":"function","function":{"name":"Read"}}}`))
	root = map[string]any{}
	_ = json.Unmarshal(out, &root)
	tc, _ = root["tool_choice"].(map[string]any)
	fn, _ = tc["function"].(map[string]any)
	if asString(fn["name"]) != "Read" {
		t.Fatalf("OpenAI-form tool_choice must be preserved, got %v", root["tool_choice"])
	}
}

func TestSanitizeResponsesFunctionCallChain(t *testing.T) {
	// Codex-style Responses input: function_call + reasoning + function_call_output.
	// reasoning must NOT become a message; function_call → assistant tool_calls;
	// function_call_output(call_id) → tool reply with matching tool_call_id.
	in := []byte(`{"model":"GLM-5.2","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"p\":\"a\"}"},` +
		`{"type":"reasoning","summary":[]},` +
		`{"type":"function_call_output","call_id":"call_1","output":"file body"}` +
		`]}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	msgs, _ := root["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 msgs (user, assistant tool_calls, tool), got %d: %s", len(msgs), out)
	}
	asst, _ := msgs[1].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if asString(asst["role"]) != "assistant" || len(tcs) != 1 {
		t.Fatalf("function_call must become assistant tool_calls: %s", out)
	}
	tc0, _ := tcs[0].(map[string]any)
	if asString(tc0["id"]) != "call_1" {
		t.Fatalf("tool_call id lost: %v", tc0)
	}
	tool, _ := msgs[2].(map[string]any)
	if asString(tool["role"]) != "tool" || asString(tool["tool_call_id"]) != "call_1" {
		t.Fatalf("function_call_output must be tool reply with call_id: %s", out)
	}
	if asString(tool["content"]) != "file body" {
		t.Fatalf("tool output lost: %v", tool)
	}
}

func TestSanitizeResponsesParallelToolCallsMerge(t *testing.T) {
	// Parallel Responses tool calls: two function_call items then two outputs.
	// Must merge into ONE assistant with 2 tool_calls, else tool replies aren't
	// adjacent to their assistant tool_calls and upstream 400s.
	in := []byte(`{"model":"GLM-5.2","input":[` +
		`{"type":"function_call","call_id":"a","name":"Read","arguments":"{}"},` +
		`{"type":"function_call","call_id":"b","name":"Grep","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"a","output":"ra"},` +
		`{"type":"function_call_output","call_id":"b","output":"rb"}` +
		`]}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	msgs, _ := root["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want assistant(2 tc)+2 tool, got %d: %s", len(msgs), out)
	}
	asst, _ := msgs[0].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if asString(asst["role"]) != "assistant" || len(tcs) != 2 {
		t.Fatalf("parallel tool_calls must merge into one assistant: %s", out)
	}
}

func TestSanitizeMapsStopSequences(t *testing.T) {
	// Single element → string.
	out := sanitizeCLIChatBody([]byte(`{"model":"GLM-5.2","messages":[],"stop_sequences":["END"]}`))
	var root map[string]any
	_ = json.Unmarshal(out, &root)
	if _, ok := root["stop_sequences"]; ok {
		t.Fatalf("stop_sequences must be renamed: %s", out)
	}
	if root["stop"] != "END" {
		t.Fatalf("single stop=%v want END", root["stop"])
	}
	// Multiple → array preserved.
	out = sanitizeCLIChatBody([]byte(`{"model":"GLM-5.2","messages":[],"stop_sequences":["A","B"]}`))
	root = map[string]any{}
	_ = json.Unmarshal(out, &root)
	arr, ok := root["stop"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("multi stop=%v", root["stop"])
	}
}

func TestSanitizeCLIChatBodyDoesNotInjectCacheKey(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}]}`)
	out := sanitizeCLIChatBody(in)
	if strings.Contains(string(out), "prompt_cache_key") {
		t.Fatalf("injected intercept field: %s", out)
	}
}

func TestSanitizeCLIChatBodyDropsUnknownExtras(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","verbosity":"low","parallel_tool_calls":true}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"verbosity", "parallel_tool_calls"} {
		if _, ok := root[k]; ok {
			t.Fatalf("extra %s leaked: %s", k, out)
		}
	}
	if asString(root["reasoning_effort"]) != "high" {
		t.Fatalf("reasoning_effort must pass through: %s", out)
	}
	if root["model"] != "GLM-5.2" {
		t.Fatalf("model=%v", root["model"])
	}
}

func TestSanitizeCLIChatBodyConvertsResponsesInput(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","instructions":"be brief","input":"hello","stream":true}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["input"]; ok {
		t.Fatalf("input leaked: %s", out)
	}
	if _, ok := root["instructions"]; ok {
		t.Fatalf("instructions leaked: %s", out)
	}
	msgs, _ := root["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", root["messages"])
	}
	sys, _ := msgs[0].(map[string]any)
	user, _ := msgs[1].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be brief" {
		t.Fatalf("system=%v", sys)
	}
	if user["role"] != "user" || user["content"] != "hello" {
		t.Fatalf("user=%v", user)
	}
}

func TestSanitizeCLIChatBodyConvertsInputParts(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	msgs, _ := root["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%v", root["messages"])
	}
	user, _ := msgs[0].(map[string]any)
	if user["content"] != "hi" {
		t.Fatalf("content=%v", user["content"])
	}
}

func TestSanitizeDisablesThinkingOnToolFollowup(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"assistant","tool_calls":[{"id":"c1"}]},{"role":"tool","tool_call_id":"c1","content":"ok"}],"stream":true}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	th, _ := root["thinking"].(map[string]any)
	if asString(th["type"]) != "enabled" {
		t.Fatalf("thinking must stay enabled: %s", out)
	}
	msgs, _ := root["messages"].([]any)
	asst, _ := msgs[0].(map[string]any)
	if _, ok := asst["reasoning_content"]; !ok {
		t.Fatalf("assistant tool_calls must keep reasoning_content: %s", out)
	}
	shape := describeTurnShape(root, len(out))
	if !strings.Contains(shape, "tool_role=true") || !strings.Contains(shape, "thinking=enabled") || !strings.Contains(shape, "chain=true") {
		t.Fatalf("shape=%s", shape)
	}
}

func TestSanitizePreservesReasoningOnToolFollowup(t *testing.T) {
	t.Setenv("KEEP_REASONING_TURNS", "0")
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"assistant","content":"","reasoning":"need file","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"}],"tools":[{"type":"function","function":{"name":"Read"}}],"stream":true}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	th, _ := root["thinking"].(map[string]any)
	if asString(th["type"]) != "enabled" {
		t.Fatalf("real reasoning must keep thinking: %s", out)
	}
	if th["clear_thinking"] != false {
		t.Fatalf("clear_thinking must be false: %s", out)
	}
	if root["tool_stream"] != true {
		t.Fatalf("tool_stream must be true: %s", out)
	}
	msgs, _ := root["messages"].([]any)
	asst, _ := msgs[0].(map[string]any)
	// stripHistoricalReasoning clears old reasoning to match CLI behaviour;
	// tool_calls messages keep the key as "" (InferHub requires it present).
	if asString(asst["reasoning_content"]) != "" {
		t.Fatalf("historical reasoning should be stripped: %s", out)
	}
}

func TestRewriteClaudeThinkingToReasoning(t *testing.T) {
	t.Setenv("KEEP_REASONING_TURNS", "0")
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"step"},{"type":"tool_use","id":"c1","name":"Read","input":{"path":"a"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}],"stream":true}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	msgs, _ := root["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("msgs=%s", out)
	}
	asst, _ := msgs[0].(map[string]any)
	// stripHistoricalReasoning clears old reasoning to match native CLI
	// behaviour; tool_calls messages keep the key as "" (InferHub requirement).
	if asString(asst["reasoning_content"]) != "" {
		t.Fatalf("historical reasoning should be stripped: %s", out)
	}
	th, _ := root["thinking"].(map[string]any)
	if asString(th["type"]) != "enabled" {
		t.Fatalf("repaired chain must enable thinking: %s", out)
	}
}

func TestSanitizeKeepsThinkingOnFirstUserTurn(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"}}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	th, _ := root["thinking"].(map[string]any)
	if asString(th["type"]) != "enabled" {
		t.Fatalf("first turn must keep thinking: %s", out)
	}
}

func TestSanitizeForcesThinkingWhenMissing(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true,"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	th, _ := root["thinking"].(map[string]any)
	if asString(th["type"]) != "enabled" || th["clear_thinking"] != false {
		t.Fatalf("missing thinking must be enabled when tools present: %s", out)
	}
}

func TestSanitizeCarriesReasoningToLaterToolCall(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"assistant","content":"","reasoning_content":"need file","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"},{"role":"assistant","tool_calls":[{"id":"c2","type":"function","function":{"name":"Read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c2","content":"ok2"}],"stream":true,"tools":[{"type":"function"}]}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	msgs, _ := root["messages"].([]any)
	asst, _ := msgs[2].(map[string]any)
	if asString(asst["reasoning_content"]) == "need file" {
		t.Fatalf("must not copy another turn's reasoning: %s", out)
	}
}

func TestSanitizeScrubsPlaceholderThink(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"assistant","content":"","reasoning_content":"思考中思考中思考中思考中","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},{"role":"tool","content":"ok","tool_call_id":"c1"}],"stream":true,"tools":[{"type":"function"}]}`)
	out := sanitizeCLIChatBody(in)
	if strings.Contains(string(out), "思考中") {
		t.Fatalf("placeholder must be scrubbed: %s", out)
	}
}

// 被白名单丢掉的字段必须让客户端看得见：静默丢弃会让客户端
// 以为 JSON mode / seed 生效了，实际拿到自由文本。
func TestDroppedFieldsReported(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"verbosity":"low","response_format":{"type":"json_object"}}`)
	out, dropped := sanitizeCLIChatBodyWithDropped(in, false)
	if strings.Contains(string(out), "response_format") {
		t.Fatalf("response_format should be dropped: %s", out)
	}
	found := false
	for _, k := range dropped {
		if k == "response_format" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dropped list must name response_format: %v", dropped)
	}
}
