package api

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseGLMToolXML(t *testing.T) {
	text := `I'll read it.<tool_call>Read<arg_key>path</arg_key><arg_value>a.go</arg_value></tool_call>`
	tools := parseGLMToolXML(text)
	if len(tools) != 1 {
		t.Fatalf("tools=%d", len(tools))
	}
	fn, _ := tools[0]["function"].(map[string]any)
	if asString(fn["name"]) != "Read" {
		t.Fatalf("name=%v", fn["name"])
	}
	if !strings.Contains(asString(fn["arguments"]), "a.go") {
		t.Fatalf("args=%v", fn["arguments"])
	}
}

func TestConsumeSSERescuesToolXMLFromThink(t *testing.T) {
	src := strings.NewReader(
		`data:{"id":"abc","choices":[{"delta":{"reasoning_content":"<tool_call>Read<arg_key>path</arg_key><arg_value>a.go</arg_value></tool_call>"}}]}` + "\n\n" +
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
		t.Fatal("xml tool_call in think must be rescued")
	}
	if !strings.Contains(out.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish: %s", out.String())
	}
	if strings.Contains(asm.reasoning.String(), "<tool_call>") {
		t.Fatalf("xml leaked into think: %q", asm.reasoning.String())
	}
}

func TestThinkMemRestoresExactBlock(t *testing.T) {
	s := &Server{thinkMem: newThinkMem()}
	comp := []byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"need file","tool_calls":[{"id":"c9","function":{"name":"Read","arguments":"{}"}}]}}]}`)
	s.rememberTurnThink(comp)
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"assistant","tool_calls":[{"id":"c9","type":"function","function":{"name":"Read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c9","content":"ok"}],"stream":true}`)
	out := s.restoreThink(sanitizeCLIChatBody(in))
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	msgs, _ := root["messages"].([]any)
	asst, _ := msgs[0].(map[string]any)
	if asString(asst["reasoning_content"]) != "need file" {
		t.Fatalf("restore failed: %s", out)
	}
}

func TestSanitizeKeepsReasoningEffort(t *testing.T) {
	in := []byte(`{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","stream":true}`)
	out := sanitizeCLIChatBody(in)
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if asString(root["reasoning_effort"]) != "high" {
		t.Fatalf("reasoning_effort dropped: %s", out)
	}
}
