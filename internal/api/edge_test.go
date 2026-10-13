package api

import (
	"strings"
	"testing"
)

// TestSanitizeCLIChatBodyEdgeCases hits adversarial / malformed / impossible
// inputs and asserts the converter never panics and always returns valid bytes.
func TestSanitizeCLIChatBodyEdgeCases(t *testing.T) {
	cases := []string{
		``,                                     // empty
		`not json at all`,                      // garbage
		`{}`,                                   // empty object
		`[]`,                                   // array root (not object)
		`null`,                                 // null
		`{"model":123}`,                        // wrong-typed model
		`{"messages":"notarray"}`,              // wrong-typed messages
		`{"messages":[null,123,"str"]}`,        // junk message entries
		`{"input":[{"type":"function_call"}]}`, // function_call missing call_id/name
		`{"input":[{"type":"function_call_output"}]}`,                                 // output missing call_id
		`{"input":[{"type":"reasoning"},{"type":"reasoning"}]}`,                       // only reasoning items
		`{"tools":[{}],"tool_choice":{"type":"tool"}}`,                                // tool_choice tool, no name, empty tool
		`{"tool_choice":{"type":"function","function":{}}}`,                           // openai form, no name
		`{"tool_choice":"required"}`,                                                  // bare string passthrough
		`{"stop_sequences":[]}`,                                                       // empty stop
		`{"stop_sequences":[1,2,3]}`,                                                  // non-string stops
		`{"system":[{"type":"text"}]}`,                                                // system block missing text
		`{"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`,     // image with empty source
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use"}]}]}`,         // tool_use missing fields
		`{"messages":[{"role":"user","content":[{"type":"tool_result"}]}]}`,           // tool_result missing fields
		`{"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`, // pre-enabled thinking
		`{"messages":[{"role":"user","content":` + strings.Repeat("[", 50) + `}]}`,    // unbalanced deep nesting (invalid json)
		`{"model":"GLM-5.2","tools":[],"tool_choice":{"type":"any"}}`,                 // empty tools + any
	}
	for i, in := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d panicked on %q: %v", i, in, r)
				}
			}()
			out := sanitizeCLIChatBody([]byte(in))
			if out == nil {
				t.Fatalf("case %d returned nil bytes for %q", i, in)
			}
		}()
	}
}

// TestResponsesItemToMessageRobustness feeds junk to the Responses item mapper.
func TestResponsesItemToMessageRobustness(t *testing.T) {
	junk := []any{
		nil,
		123,
		"bare string",
		map[string]any{},
		map[string]any{"type": "function_call", "arguments": 123},
		map[string]any{"type": "function_call_output", "output": map[string]any{"nested": true}},
		map[string]any{"role": "assistant", "tool_calls": "notarray"},
	}
	for i, raw := range junk {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("junk %d panicked: %v", i, r)
				}
			}()
			_ = responsesItemToMessage(raw)
		}()
	}
	// mergeConsecutiveToolCalls on junk.
	_ = mergeConsecutiveToolCalls([]any{nil, 123, map[string]any{"role": "assistant", "tool_calls": []any{}}})
}
