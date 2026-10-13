package api

import (
	"encoding/json"
	"strings"
	"sync"
)

// thinkMem stores the exact reasoning_content that we streamed for a tool_call id.
// Z.AI preserved-thinking requires the unmodified original block, not a copy from
// a later/earlier assistant turn (that degrades quality and prefix cache).
type thinkMem struct {
	mu   sync.Mutex
	byID map[string]string
}

func newThinkMem() *thinkMem {
	return &thinkMem{byID: map[string]string{}}
}

func (m *thinkMem) remember(reasoning string, toolIDs []string) {
	reasoning = strings.TrimSpace(reasoning)
	if m == nil || reasoning == "" || isPlaceholderThink(reasoning) || mostlyPlaceholderThink(reasoning) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byID == nil {
		m.byID = map[string]string{}
	}
	if len(m.byID) > 8192 {
		m.byID = map[string]string{}
	}
	for _, id := range toolIDs {
		if id == "" {
			continue
		}
		m.byID[id] = reasoning
	}
}

func (m *thinkMem) lookup(toolID string) string {
	if m == nil || toolID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byID[toolID]
}

func (s *Server) rememberTurnThink(body []byte) {
	if s == nil || s.thinkMem == nil || len(body) == 0 {
		return
	}
	var obj struct {
		Choices []struct {
			Message struct {
				Reasoning string `json:"reasoning_content"`
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &obj) != nil || len(obj.Choices) == 0 {
		return
	}
	msg := obj.Choices[0].Message
	ids := make([]string, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		ids = append(ids, tc.ID)
	}
	s.thinkMem.remember(msg.Reasoning, ids)
}

func (s *Server) restoreThink(body []byte) []byte {
	if s == nil || s.thinkMem == nil || len(body) == 0 {
		return body
	}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil || root == nil {
		return body
	}
	msgs, _ := root["messages"].([]any)
	changed := false
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil || !strings.EqualFold(asString(m["role"]), "assistant") || m["tool_calls"] == nil {
			continue
		}
		if messageReasoning(m) != "" {
			continue
		}
		id := firstToolCallID(m["tool_calls"])
		rc := s.thinkMem.lookup(id)
		if rc == "" {
			continue
		}
		m["reasoning_content"] = rc
		changed = true
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func firstToolCallID(v any) string {
	arr, _ := v.([]any)
	if len(arr) == 0 {
		return ""
	}
	m, _ := arr[0].(map[string]any)
	return asString(m["id"])
}
