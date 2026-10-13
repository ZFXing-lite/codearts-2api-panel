package api

import (
	"encoding/json"
	"testing"
)

func TestFlattenToolResultContent(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"plain string", "hello", "hello"},
		{"nil", nil, ""},
		{
			"single text block",
			[]any{map[string]any{"type": "text", "text": "file output"}},
			"file output",
		},
		{
			"multi text blocks",
			[]any{
				map[string]any{"type": "text", "text": "line1\n"},
				map[string]any{"type": "text", "text": "line2"},
			},
			"line1\nline2",
		},
		{
			"image block becomes placeholder, not base64",
			[]any{map[string]any{"type": "image", "source": map[string]any{"data": "AAAABBBBCCCC"}}},
			"[image omitted]",
		},
		{
			"text plus image",
			[]any{
				map[string]any{"type": "text", "text": "see below"},
				map[string]any{"type": "image", "source": map[string]any{"data": "xxxx"}},
			},
			"see below\n[image omitted]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := flattenToolResultContent(tc.in); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestRewriteClaudeToolResultArrayFlattened(t *testing.T) {
	// A Claude Code turn: assistant tool_use, then user tool_result with ARRAY
	// content. The role:tool message content must be the clean text, NOT the
	// JSON-scaffolded array, and tool_use with no input must send "{}".
	in := []byte(`{"model":"GLM-5.2","messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"ls","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"a.go\nb.go"}]}]}
	]}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	msgs, _ := root["messages"].([]any)
	var sawTool, sawAssistant bool
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		switch m["role"] {
		case "tool":
			sawTool = true
			if c, _ := m["content"].(string); c != "a.go\nb.go" {
				t.Fatalf("tool content not flattened: %q", m["content"])
			}
			if id, _ := m["tool_call_id"].(string); id != "call_1" {
				t.Fatalf("tool_call_id: %v", m["tool_call_id"])
			}
		case "assistant":
			sawAssistant = true
			tcs, _ := m["tool_calls"].([]any)
			if len(tcs) != 1 {
				t.Fatalf("want 1 tool_call, got %v", m["tool_calls"])
			}
			tc, _ := tcs[0].(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			if args, _ := fn["arguments"].(string); args != "{}" {
				t.Fatalf("empty input must serialize to {}, got %q", args)
			}
		}
	}
	if !sawTool || !sawAssistant {
		t.Fatalf("missing converted messages: tool=%v assistant=%v out=%s", sawTool, sawAssistant, out)
	}
}

func TestLastMessageIsToolFollowup(t *testing.T) {
	cases := []struct {
		name string
		root map[string]any
		want bool
	}{
		{
			name: "empty messages",
			root: map[string]any{"messages": []any{}},
			want: false,
		},
		{
			name: "trailing tool-role message",
			root: map[string]any{"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"role": "assistant", "content": "ok"},
				map[string]any{"role": "tool", "content": "result"},
			}},
			want: true,
		},
		{
			name: "trailing anthropic tool_result content part",
			root: map[string]any{"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "content": "result"},
				}},
			}},
			want: true,
		},
		{
			name: "plain user message, no tool anywhere",
			root: map[string]any{"messages": []any{
				map[string]any{"role": "user", "content": "hello"},
			}},
			want: false,
		},
		{
			name: "trailing system-reminder after tool results (prod bug 2026-09-20)",
			root: map[string]any{"messages": []any{
				map[string]any{"role": "assistant", "content": "calling tools"},
				map[string]any{"role": "tool", "content": "result 1"},
				map[string]any{"role": "tool", "content": "result 2"},
				map[string]any{"role": "tool", "content": "result 3"},
				map[string]any{"role": "system", "content": "<system-reminder>...</system-reminder>"},
			}},
			want: true,
		},
		{
			name: "trailing system-reminder after anthropic tool_result part",
			root: map[string]any{"messages": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "content": "result"},
				}},
				map[string]any{"role": "system", "content": "<system-reminder>...</system-reminder>"},
			}},
			want: true,
		},
		{
			name: "trailing system message after a plain user message stays non-followup",
			root: map[string]any{"messages": []any{
				map[string]any{"role": "user", "content": "hello"},
				map[string]any{"role": "system", "content": "<system-reminder>...</system-reminder>"},
			}},
			want: false,
		},
		{
			name: "multiple trailing system messages after tool result",
			root: map[string]any{"messages": []any{
				map[string]any{"role": "tool", "content": "result"},
				map[string]any{"role": "system", "content": "reminder 1"},
				map[string]any{"role": "system", "content": "reminder 2"},
			}},
			want: true,
		},
		{
			name: "all-system messages",
			root: map[string]any{"messages": []any{
				map[string]any{"role": "system", "content": "reminder 1"},
			}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lastMessageIsToolFollowup(tc.root)
			if got != tc.want {
				t.Fatalf("lastMessageIsToolFollowup() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRewriteClaudeMessagesToolResultIsError(t *testing.T) {
	root := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_1",
						"content":     "command failed: exit 1",
						"is_error":    true,
					},
				},
			},
		},
	}
	changed := rewriteClaudeMessages(root)
	if !changed {
		t.Fatal("expected changed=true")
	}
	msgs, _ := root["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	toolMsg, _ := msgs[0].(map[string]any)
	if toolMsg == nil || toolMsg["role"] != "tool" {
		t.Fatalf("expected tool message, got %v", toolMsg)
	}
	content, _ := toolMsg["content"].(string)
	if content == "" {
		b, _ := json.Marshal(toolMsg["content"])
		content = string(b)
	}
	if len(content) < 12 || content[:12] != "[TOOL_ERROR]" {
		t.Fatalf("expected [TOOL_ERROR] prefix, got %q", content)
	}
}

func TestRewriteClaudeMessagesToolResultNoError(t *testing.T) {
	root := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_1",
						"content":     "success",
					},
				},
			},
		},
	}
	rewriteClaudeMessages(root)
	msgs, _ := root["messages"].([]any)
	toolMsg, _ := msgs[0].(map[string]any)
	content, _ := toolMsg["content"].(string)
	if content != "success" {
		t.Fatalf("expected plain content, got %q", content)
	}
}

func TestCleanToolParameters(t *testing.T) {
	schema := map[string]any{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type":    "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":                 "string",
				"const":                "/tmp",
				"additionalProperties": false,
			},
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":  "string",
					"const": "fixed",
				},
			},
		},
		"additionalProperties": false,
	}
	if !cleanToolParameters(schema) {
		t.Fatal("should return true when stripping fields")
	}
	if _, ok := schema["$schema"]; ok {
		t.Fatal("$schema should be stripped")
	}
	if _, ok := schema["additionalProperties"]; ok {
		t.Fatal("top-level additionalProperties should be stripped")
	}
	props := schema["properties"].(map[string]any)
	pathProp := props["path"].(map[string]any)
	if _, ok := pathProp["const"]; ok {
		t.Fatal("nested const should be stripped")
	}
	if pathProp["type"] != "string" {
		t.Fatal("type should be preserved")
	}
	itemsProp := props["items"].(map[string]any)
	nested := itemsProp["items"].(map[string]any)
	if _, ok := nested["const"]; ok {
		t.Fatal("deep nested const should be stripped")
	}
}

func TestCleanToolParametersNoOp(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
	}
	if cleanToolParameters(schema) {
		t.Fatal("clean schema should return false")
	}
}

func TestRewriteModel(t *testing.T) {
	cases := []struct {
		name    string
		model   string
		want    string
		changed bool
	}{
		{"empty defaults", "", "GLM-5.2", true},
		{"huawei passthrough", "GLM-5.2", "GLM-5.2", false},
		{"strip [1M] suffix", "GLM-5.2[1M]", "GLM-5.2", true},
		{"strip [200K] suffix", "DeepSeek-R1[200K]", "DeepSeek-R1", true},
		{"claude alias", "claude-sonnet-4-20250514", "GLM-5.2", true},
		{"haiku alias", "claude-3-5-haiku-20241022", "GLM-5.1", true},
		{"unknown defaults", "some-random-model-xyz", "GLM-5.2", true},
		{"deepseek passthrough", "DeepSeek-V3", "DeepSeek-V3", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := map[string]any{"model": tc.model}
			if tc.model == "" {
				delete(root, "model")
			}
			got := rewriteModel(root)
			if got != tc.changed {
				t.Fatalf("changed=%v want %v", got, tc.changed)
			}
			if m := root["model"].(string); m != tc.want {
				t.Fatalf("model=%q want %q", m, tc.want)
			}
		})
	}
}

func TestStripHistoricalReasoningKeepLast(t *testing.T) {
	t.Setenv("STRIP_HISTORICAL_REASONING", "")
	t.Setenv("KEEP_REASONING_TURNS", "2")

	root := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "q1"},
			map[string]any{"role": "assistant", "reasoning_content": "old-reason-1", "content": "a1"},
			map[string]any{"role": "user", "content": "q2"},
			map[string]any{"role": "assistant", "reasoning_content": "old-reason-2", "content": "a2",
				"tool_calls": []any{map[string]any{"id": "c1"}}},
			map[string]any{"role": "tool", "content": "r1", "tool_call_id": "c1"},
			map[string]any{"role": "assistant", "reasoning_content": "recent-reason-3", "content": "a3"},
			map[string]any{"role": "user", "content": "q3"},
			map[string]any{"role": "assistant", "reasoning_content": "recent-reason-4", "content": "a4"},
			map[string]any{"role": "user", "content": "q4"},
		},
	}
	changed := stripHistoricalReasoning(root)
	if !changed {
		t.Fatal("expected changed=true")
	}
	msgs, _ := root["messages"].([]any)
	// msg[1]: oldest assistant — reasoning should be stripped (deleted, no tool_calls)
	m1, _ := msgs[1].(map[string]any)
	if _, ok := m1["reasoning_content"]; ok {
		t.Fatalf("msg[1] reasoning should be deleted, got %v", m1["reasoning_content"])
	}
	// msg[3]: second oldest assistant with tool_calls — reasoning set to ""
	m3, _ := msgs[3].(map[string]any)
	if rc, _ := m3["reasoning_content"].(string); rc != "" {
		t.Fatalf("msg[3] reasoning should be empty string, got %q", rc)
	}
	// msg[5]: 2nd-newest assistant — kept (rank 1 < keepLast 2)
	m5, _ := msgs[5].(map[string]any)
	if rc, _ := m5["reasoning_content"].(string); rc != "recent-reason-3" {
		t.Fatalf("msg[5] reasoning should be preserved, got %q", rc)
	}
	// msg[7]: newest assistant — kept (rank 0 < keepLast 2)
	m7, _ := msgs[7].(map[string]any)
	if rc, _ := m7["reasoning_content"].(string); rc != "recent-reason-4" {
		t.Fatalf("msg[7] reasoning should be preserved, got %q", rc)
	}
}

func TestStripHistoricalReasoningDisabled(t *testing.T) {
	t.Setenv("STRIP_HISTORICAL_REASONING", "0")
	root := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "reasoning_content": "keep-me", "content": "a"},
		},
	}
	if stripHistoricalReasoning(root) {
		t.Fatal("should be disabled by env=0")
	}
	m, _ := root["messages"].([]any)[0].(map[string]any)
	if m["reasoning_content"] != "keep-me" {
		t.Fatal("reasoning was stripped when disabled")
	}
}

func TestStripHistoricalReasoning(t *testing.T) {
	t.Setenv("KEEP_REASONING_TURNS", "0")
	root := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "you are helpful"},
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{
				"role":              "assistant",
				"content":           "thinking about this...",
				"reasoning_content": "long internal reasoning turn 1 " + string(make([]byte, 5000)),
			},
			map[string]any{"role": "user", "content": "now do tools"},
			map[string]any{
				"role":              "assistant",
				"content":           "",
				"reasoning_content": "long reasoning before tool call " + string(make([]byte, 3000)),
				"tool_calls":        []any{map[string]any{"id": "call_1"}},
			},
			map[string]any{"role": "tool", "content": "result", "tool_call_id": "call_1"},
			map[string]any{"role": "user", "content": "new question"},
		},
	}

	changed := stripHistoricalReasoning(root)
	if !changed {
		t.Fatal("expected changed=true")
	}

	msgs, _ := root["messages"].([]any)
	// Plain assistant (no tool_calls): reasoning_content deleted entirely
	m2, _ := msgs[2].(map[string]any)
	if _, exists := m2["reasoning_content"]; exists {
		t.Fatalf("plain assistant should have reasoning_content deleted, got %v", m2["reasoning_content"])
	}
	if m2["content"] != "thinking about this..." {
		t.Fatalf("text content should be preserved, got %v", m2["content"])
	}

	// Assistant with tool_calls: reasoning_content set to empty, not deleted
	m4, _ := msgs[4].(map[string]any)
	rc, exists := m4["reasoning_content"]
	if !exists {
		t.Fatal("tool_calls assistant should keep reasoning_content key (empty)")
	}
	if rc != "" {
		t.Fatalf("tool_calls assistant reasoning_content should be empty, got %q", rc)
	}
	if m4["tool_calls"] == nil {
		t.Fatal("tool_calls should be preserved")
	}

	// Non-assistant messages untouched
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" {
		t.Fatalf("system message mutated: %v", m0)
	}
}

func TestStripHistoricalReasoningNoOp(t *testing.T) {
	root := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "hello"},
		},
	}
	if stripHistoricalReasoning(root) {
		t.Fatal("no reasoning to strip, should return false")
	}
}
