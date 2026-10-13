package api

// Anthropic Messages API contract: thinking blocks WITHOUT a signature are
// dropped from history when the client echoes the assistant turn back on
// subsequent requests. That drops the model's reasoning across turns and
// manifests as agent-loop self-repetition (the model re-derives the same
// conclusions from tool_use params + short text every turn). These tests
// pin the signature emission invariants so a well-meaning "safe choice"
// doesn't revert to a bare close.

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComputeThinkSignatureDeterministic(t *testing.T) {
	// Same content -> same signature. Prompt-cache stability depends on this;
	// a shifting signature would produce cache misses on every replay.
	a := computeThinkSignature("the model was thinking about foo")
	b := computeThinkSignature("the model was thinking about foo")
	if a != b {
		t.Fatalf("signature must be deterministic per-content: %q != %q", a, b)
	}
	if a == "" {
		t.Fatal("signature must be non-empty (empty is client-dropped)")
	}
}

func TestComputeThinkSignatureContentSensitive(t *testing.T) {
	// Different content -> different signature. Otherwise two distinct
	// thinking blocks in the same turn would share a signature — cache key
	// collision on replay and semantic ambiguity on the wire.
	a := computeThinkSignature("thought one")
	b := computeThinkSignature("thought two")
	if a == b {
		t.Fatalf("distinct thinking text must produce distinct signatures: %q == %q", a, b)
	}
}

func TestComputeThinkSignatureEmpty(t *testing.T) {
	// Empty thinking still gets a stable non-empty signature — an empty
	// signature field is functionally equivalent to no signature (client
	// drops the block). We want empty-thinking blocks to survive round-trip.
	s := computeThinkSignature("")
	if s == "" {
		t.Fatal("empty thinking must still get a non-empty signature placeholder")
	}
}

// TestClaudeThinkingBlockCarriesSignatureBeforeStop verifies that a thinking
// block being closed to make way for a tool_use block emits signature_delta
// against index 0 BEFORE the content_block_stop for that block. Order
// matters: clients treat the signature as an attribute sealed at close.
func TestClaudeThinkingBlockCarriesSignatureBeforeStop(t *testing.T) {
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_sig", Model: "GLM-5.2", Proto: protoClaude, StartSent: true}
	p := newProtoStream(rec, pin)
	if err := p.emit(mustJSONReasoning("analysing input")); err != nil {
		t.Fatal(err)
	}
	if err := p.emit(mustJSONToolCallOpen("c1", "Bash", "")); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()

	sigLine, sigIdx := findSSEDataLineContaining(out, `"type":"signature_delta"`)
	if sigIdx < 0 {
		t.Fatalf("signature_delta must be emitted for thinking block, missing:\n%s", out)
	}
	if !strings.Contains(sigLine, `"index":0`) {
		t.Fatalf("signature_delta must target thinking block index 0, got: %s", sigLine)
	}
	if !strings.Contains(sigLine, `"signature":"`) {
		t.Fatalf("signature_delta payload missing signature field: %s", sigLine)
	}
	// Order check: signature_delta BEFORE content_block_stop for index 0.
	stopIdx := findContentBlockStop(out, 0)
	if stopIdx < 0 {
		t.Fatalf("content_block_stop for index 0 missing:\n%s", out)
	}
	if sigIdx > stopIdx {
		t.Fatalf("signature_delta must precede content_block_stop, got sig@%d stop@%d:\n%s",
			sigIdx, stopIdx, out)
	}
}

// TestClaudeLateThinkingBlockCarriesSignature verifies that the trailing
// thinking block emitted by flushLateThinking (for reasoning that arrived
// after tool_calls opened) also carries a signature — same failure mode
// applies if it doesn't.
func TestClaudeLateThinkingBlockCarriesSignature(t *testing.T) {
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_late", Model: "GLM-5.2", Proto: protoClaude, StartSent: true}
	p := newProtoStream(rec, pin)
	if err := p.emit(mustJSONReasoning("initial")); err != nil {
		t.Fatal(err)
	}
	if err := p.emit(mustJSONToolCallFull("c1", "Bash", `{"command":"ls"}`)); err != nil {
		t.Fatal(err)
	}
	if err := p.emit(mustJSONReasoning("late reasoning")); err != nil {
		t.Fatal(err)
	}
	if err := p.emit(mustJSONFinish("stop")); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()

	// Two signature_delta events expected: one for pre-tool thinking@0,
	// one for the late trailing thinking block.
	sigCount := strings.Count(out, `"type":"signature_delta"`)
	if sigCount < 2 {
		t.Fatalf("expected >=2 signature_delta (pre-tool + late), got %d:\n%s", sigCount, out)
	}
	// The late thinking block emits a fresh content_block_start whose
	// content_block object has type=thinking. Match with a closing brace so
	// we don't accidentally pick up "thinking_delta" occurrences.
	thinkStarts := strings.Count(out, `"type":"thinking"}`)
	if thinkStarts < 2 {
		t.Fatalf("expected >=2 thinking block starts (pre-tool + late), got %d:\n%s", thinkStarts, out)
	}
}

// TestClaudeThinkingSignatureStableAcrossRuns verifies the signature is
// identical for the same input across independent stream instances. This
// is what makes Anthropic prompt-cache replay hits possible.
func TestClaudeThinkingSignatureStableAcrossRuns(t *testing.T) {
	run := func() string {
		rec := httptest.NewRecorder()
		pin := &ssePin{ID: "msg_stable", Model: "GLM-5.2", Proto: protoClaude, StartSent: true}
		p := newProtoStream(rec, pin)
		if err := p.emit(mustJSONReasoning("stable thought")); err != nil {
			t.Fatal(err)
		}
		if err := p.emit(mustJSONToolCallOpen("c1", "Bash", "")); err != nil {
			t.Fatal(err)
		}
		return rec.Body.String()
	}
	sigA := extractSignatureField(run())
	sigB := extractSignatureField(run())
	if sigA == "" || sigB == "" {
		t.Fatalf("signature missing: A=%q B=%q", sigA, sigB)
	}
	if sigA != sigB {
		t.Fatalf("signature must be stable across replays for prompt-cache; got %q vs %q", sigA, sigB)
	}
}

// TestClaudeThinkingOnlyTurnCarriesSignature verifies that a thinking-only
// turn (reasoning + no text + no tool_use) still emits signature_delta.
// Close path here is closeAllClaudeBlocks from the finish sequence.
func TestClaudeThinkingOnlyTurnCarriesSignature(t *testing.T) {
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_tonly", Model: "GLM-5.2", Proto: protoClaude, StartSent: true}
	p := newProtoStream(rec, pin)
	if err := p.emit(mustJSONReasoning("reasoning only, no text")); err != nil {
		t.Fatal(err)
	}
	if err := p.emit(mustJSONFinish("stop")); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()

	sigIdx := strings.Index(out, `"type":"signature_delta"`)
	if sigIdx < 0 {
		t.Fatalf("thinking-only turn must still emit signature_delta:\n%s", out)
	}
	stopIdx := findContentBlockStop(out, 0)
	if stopIdx < 0 {
		t.Fatalf("content_block_stop for index 0 missing:\n%s", out)
	}
	if sigIdx > stopIdx {
		t.Fatalf("signature must precede content_block_stop:\n%s", out)
	}
}

// TestClaudeTextAfterThinkingClosesThinkWithSignature verifies openClaudeText
// path — when text arrives after reasoning (no tools involved), the thinking
// close must still route through the signature-emitting helper.
func TestClaudeTextAfterThinkingClosesThinkWithSignature(t *testing.T) {
	rec := httptest.NewRecorder()
	pin := &ssePin{ID: "msg_text", Model: "GLM-5.2", Proto: protoClaude, StartSent: true}
	p := newProtoStream(rec, pin)
	if err := p.emit(mustJSONReasoning("thinking before text")); err != nil {
		t.Fatal(err)
	}
	if err := p.emit(`{"choices":[{"delta":{"content":"hello world"}}]}`); err != nil {
		t.Fatal(err)
	}
	if err := p.emit(mustJSONFinish("stop")); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()

	sigIdx := strings.Index(out, `"type":"signature_delta"`)
	if sigIdx < 0 {
		t.Fatalf("signature_delta must be emitted before text opens:\n%s", out)
	}
	// signature_delta must be for thinking block (index 0) and land before
	// the text_delta (which opens index 1).
	textDeltaIdx := strings.Index(out, `"type":"text_delta"`)
	if textDeltaIdx < 0 {
		t.Fatalf("text_delta missing:\n%s", out)
	}
	if sigIdx > textDeltaIdx {
		t.Fatalf("signature_delta must land before text_delta (thinking closed first):\n"+
			"sig@%d text@%d\n%s", sigIdx, textDeltaIdx, out)
	}
}

// --- helpers ---------------------------------------------------------------

func mustJSONReasoning(text string) string {
	return `{"choices":[{"delta":{"reasoning_content":` + jsonQuoteForTest(text) + `}}]}`
}

func mustJSONToolCallOpen(id, name, args string) string {
	return `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":` +
		jsonQuoteForTest(id) + `,"type":"function","function":{"name":` +
		jsonQuoteForTest(name) + `,"arguments":` + jsonQuoteForTest(args) + `}}]}}]}`
}

func mustJSONToolCallFull(id, name, args string) string {
	// Same as Open — GLM often sends the full tool call in one delta.
	return mustJSONToolCallOpen(id, name, args)
}

func mustJSONFinish(reason string) string {
	return `{"choices":[{"delta":{},"finish_reason":` + jsonQuoteForTest(reason) +
		`}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
}

// jsonQuoteForTest quotes a string as a JSON string literal. Adequate for
// test inputs; not a general-purpose encoder.
func jsonQuoteForTest(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// findContentBlockStop returns the byte offset of a content_block_stop event
// targeting the given index, regardless of field order in the JSON payload
// (Go map encoding is deterministic per-instance but the two possible orders
// are both allowed by clients).
func findContentBlockStop(out string, idx int) int {
	// Try both orderings that json.Marshal might produce.
	needleA := `"type":"content_block_stop","index":` + itoaTest(idx)
	needleB := `"index":` + itoaTest(idx) + `,"type":"content_block_stop"`
	a := strings.Index(out, needleA)
	b := strings.Index(out, needleB)
	switch {
	case a < 0:
		return b
	case b < 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// findSSEDataLineContaining scans the captured SSE body for the first
// `data: ...` line that contains the needle, returns (fullLineWithoutPrefix,
// startOffsetOfLineInBody). Returns ("", -1) when not found. Used because
// field order inside the JSON payload is alphabetized by Go's encoder, so a
// substring search for a specific field can land past adjacent fields we
// also want to assert on — anchoring at the line lets us search within the
// full JSON.
func findSSEDataLineContaining(body, needle string) (string, int) {
	for i := 0; i < len(body); {
		lineEnd := strings.Index(body[i:], "\n")
		var line string
		if lineEnd < 0 {
			line = body[i:]
		} else {
			line = body[i : i+lineEnd]
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, needle) {
			return line, i
		}
		if lineEnd < 0 {
			break
		}
		i += lineEnd + 1
	}
	return "", -1
}

// extractSignatureField pulls the first signature value out of a captured
// SSE stream body. Returns "" when no signature is present.
func extractSignatureField(body string) string {
	needle := `"signature":"`
	i := strings.Index(body, needle)
	if i < 0 {
		return ""
	}
	body = body[i+len(needle):]
	j := strings.Index(body, `"`)
	if j < 0 {
		return ""
	}
	return body[:j]
}
