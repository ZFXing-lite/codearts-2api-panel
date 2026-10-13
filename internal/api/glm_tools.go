package api

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	glmToolCallRe = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>`)
	glmArgPairRe  = regexp.MustCompile(`(?s)<arg_key>(.*?)</arg_key>\s*<arg_value>(.*?)</arg_value>`)
	thinkBlockRe  = regexp.MustCompile(`(?s)<think>(.*?)</think>`)
)

func parseGLMToolXML(text string) []map[string]any {
	if !strings.Contains(text, "<tool_call>") {
		return nil
	}
	matches := glmToolCallRe.FindAllStringSubmatch(text, -1)
	out := make([]map[string]any, 0, len(matches))
	for i, m := range matches {
		inner := strings.TrimSpace(m[1])
		if inner == "" {
			continue
		}
		name, argsJSON := splitGLMToolInner(inner)
		if name == "" {
			continue
		}
		out = append(out, map[string]any{
			"index": i,
			"id":    fmt.Sprintf("call_glm_%d", i),
			"type":  "function",
			"function": map[string]any{
				"name":      name,
				"arguments": argsJSON,
			},
		})
	}
	return out
}

func splitGLMToolInner(inner string) (name, argsJSON string) {
	inner = strings.TrimSpace(inner)
	if strings.HasPrefix(inner, "{") && json.Valid([]byte(inner)) {
		var obj map[string]any
		if json.Unmarshal([]byte(inner), &obj) == nil {
			name = asString(obj["name"])
			if name == "" {
				return "", ""
			}
			if raw, err := json.Marshal(obj["arguments"]); err == nil && string(raw) != "null" {
				return name, string(raw)
			}
			return name, "{}"
		}
	}
	rest := inner
	if i := strings.Index(inner, "<arg_key>"); i >= 0 {
		name = strings.TrimSpace(inner[:i])
		rest = inner[i:]
	} else {
		name = strings.TrimSpace(strings.SplitN(inner, "\n", 2)[0])
		rest = ""
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ""
	}
	pairs := glmArgPairRe.FindAllStringSubmatch(rest, -1)
	if len(pairs) == 0 {
		return name, "{}"
	}
	args := map[string]any{}
	for _, p := range pairs {
		k := strings.TrimSpace(p[1])
		v := strings.TrimSpace(p[2])
		if k == "" {
			continue
		}
		var asJSON any
		if json.Unmarshal([]byte(v), &asJSON) == nil {
			args[k] = asJSON
		} else {
			args[k] = v
		}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return name, "{}"
	}
	return name, string(b)
}

func stripGLMToolXML(s string) string {
	if s == "" || !strings.Contains(s, "<tool_call>") {
		return s
	}
	return strings.TrimSpace(glmToolCallRe.ReplaceAllString(s, ""))
}

func (a *sseAssembly) rescueToolsFromText() {
	if a == nil || a.hasToolCall() {
		return
	}
	blob := a.reasoning.String() + "\n" + a.content.String()
	tools := parseGLMToolXML(blob)
	if len(tools) == 0 {
		return
	}
	a.tools = tools
	a.toolCalls = len(a.readyTools())
	if a.lastFinish == "" || a.lastFinish == "stop" {
		a.lastFinish = "tool_calls"
		a.sawFinish = true
	}
	if cleaned := stripGLMToolXML(a.reasoning.String()); cleaned != a.reasoning.String() {
		a.reasoning.Reset()
		a.reasoning.WriteString(cleaned)
	}
	if cleaned := stripGLMToolXML(a.content.String()); cleaned != a.content.String() {
		a.content.Reset()
		a.content.WriteString(cleaned)
	}
}

func (a *sseAssembly) splitThinkTags() {
	if a == nil {
		return
	}
	s := a.content.String()
	if !strings.Contains(s, "<think>") {
		return
	}
	for _, m := range thinkBlockRe.FindAllStringSubmatch(s, -1) {
		a.noteThinkText(m[1])
	}
	a.content.Reset()
	a.content.WriteString(strings.TrimSpace(thinkBlockRe.ReplaceAllString(s, "")))
}
