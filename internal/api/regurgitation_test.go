package api

import (
	"testing"
	"time"
)

func newRegurgServer() *Server {
	return &Server{
		sessionResp:  map[string]sessionRespInfo{},
		sessionBusy:  map[string]time.Time{},
		sessionDrain: map[string]time.Time{},
	}
}

func TestNoteAssistantResponseFirstIsNotDuplicate(t *testing.T) {
	s := newRegurgServer()
	dup, n := s.noteAssistantResponse("sessA", "acc1", 0xdeadbeef)
	if dup || n != 0 {
		t.Fatalf("first call must not be duplicate: dup=%v n=%d", dup, n)
	}
}

func TestNoteAssistantResponseSameFPIsDuplicate(t *testing.T) {
	s := newRegurgServer()
	s.noteAssistantResponse("sessA", "acc1", 0x1234)
	dup, n := s.noteAssistantResponse("sessA", "acc1", 0x1234)
	if !dup || n != 1 {
		t.Fatalf("repeat fp must be duplicate: dup=%v n=%d", dup, n)
	}
	dup, n = s.noteAssistantResponse("sessA", "acc1", 0x1234)
	if !dup || n != 2 {
		t.Fatalf("second consecutive repeat should count 2: dup=%v n=%d", dup, n)
	}
}

func TestNoteAssistantResponseDifferentFPResetsRun(t *testing.T) {
	s := newRegurgServer()
	s.noteAssistantResponse("sessA", "acc1", 0x1)
	s.noteAssistantResponse("sessA", "acc1", 0x1)
	dup, n := s.noteAssistantResponse("sessA", "acc1", 0x2)
	if dup || n != 0 {
		t.Fatalf("different fp must reset: dup=%v n=%d", dup, n)
	}
}

func TestNoteAssistantResponseZeroFPIsIgnored(t *testing.T) {
	s := newRegurgServer()
	dup, _ := s.noteAssistantResponse("sessA", "acc1", 0)
	if dup {
		t.Fatalf("fp=0 (no signal) must never fire")
	}
	// A subsequent real fp on the same session must not be considered a
	// duplicate of the ignored zero (because the zero was never stored).
	dup, _ = s.noteAssistantResponse("sessA", "acc1", 0x99)
	if dup {
		t.Fatalf("real fp after ignored zero must not match")
	}
}

func TestNoteAssistantResponsePerSessionIsolation(t *testing.T) {
	s := newRegurgServer()
	s.noteAssistantResponse("sessA", "acc1", 0xabc)
	dup, _ := s.noteAssistantResponse("sessB", "acc1", 0xabc)
	if dup {
		t.Fatalf("same fp on different session must not be a duplicate")
	}
}

func TestForgetSessionResponse(t *testing.T) {
	s := newRegurgServer()
	s.noteAssistantResponse("sessA", "acc1", 0xabc)
	s.forgetSessionResponse("sessA")
	dup, n := s.noteAssistantResponse("sessA", "acc1", 0xabc)
	if dup || n != 0 {
		t.Fatalf("after forget, next call must be a fresh first: dup=%v n=%d", dup, n)
	}
}

func TestAssembledResponseFingerprint(t *testing.T) {
	same := []byte(`{"choices":[{"message":{"content":"| a | b |\n| c | d |","tool_calls":[{"function":{"name":"Read","arguments":"{\"path\":\"x\"}"}}]}}]}`)
	other := []byte(`{"choices":[{"message":{"content":"different","tool_calls":[]}}]}`)
	empty := []byte(`{"choices":[{"message":{"content":"","tool_calls":[]}}]}`)
	if assembledResponseFingerprint(same) == 0 {
		t.Fatal("non-empty content must produce non-zero fingerprint")
	}
	if assembledResponseFingerprint(same) != assembledResponseFingerprint(same) {
		t.Fatal("fingerprint must be deterministic")
	}
	if assembledResponseFingerprint(same) == assembledResponseFingerprint(other) {
		t.Fatal("distinct content must produce distinct fingerprints")
	}
	if assembledResponseFingerprint(empty) != 0 {
		t.Fatal("empty content+tools must be 0 (no signal)")
	}
	if assembledResponseFingerprint([]byte("not json")) != 0 {
		t.Fatal("unparseable must be 0 (no signal)")
	}
}

// Short pure-text acknowledgements ("OK", "Done", ".") happen legitimately in
// tool-heavy chats and must NOT be treated as regurgitation signal. Only long
// verbatim replays (or any tool call, however small) count.
func TestAssembledResponseFingerprintShortContentIsNoSignal(t *testing.T) {
	shortAck := []byte(`{"choices":[{"message":{"content":"OK","tool_calls":[]}}]}`)
	if got := assembledResponseFingerprint(shortAck); got != 0 {
		t.Fatalf("short pure-text ack must be no-signal, got %x", got)
	}
	dot := []byte(`{"choices":[{"message":{"content":".","tool_calls":[]}}]}`)
	if got := assembledResponseFingerprint(dot); got != 0 {
		t.Fatalf("single-char ack must be no-signal, got %x", got)
	}
	// Above the 24-byte floor → fingerprint.
	long := []byte(`{"choices":[{"message":{"content":"this content is long enough to trip the regurgitation floor","tool_calls":[]}}]}`)
	if assembledResponseFingerprint(long) == 0 {
		t.Fatal("content above floor must produce a fingerprint")
	}
	// Short content but WITH a tool call → still a signal (repeated Bash: ls
	// is genuine regurgitation, however short the visible text is).
	shortWithTool := []byte(`{"choices":[{"message":{"content":"OK","tool_calls":[{"function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`)
	if assembledResponseFingerprint(shortWithTool) == 0 {
		t.Fatal("short content + tool call must produce a fingerprint")
	}
}

// markAccountDrain must not SHORTEN an existing drain. This was the
// regurgitation-guard bug: the 30s hold set on a duplicate turn was silently
// clobbered by the routine 400ms post-OK drain fired seconds later, so the
// scheduler kept re-picking the offending account almost immediately.
func TestMarkAccountDrainTakesMax(t *testing.T) {
	s := &Server{sessionDrain: map[string]time.Time{}}
	s.markAccountDrain("a", 30*time.Second)
	longUntil := s.sessionDrain["a"]
	s.markAccountDrain("a", 400*time.Millisecond)
	if got := s.sessionDrain["a"]; !got.Equal(longUntil) {
		t.Fatalf("shorter drain must not clobber longer one: was=%v got=%v", longUntil, got)
	}
	// A LONGER follow-up drain does extend the window (e.g. a session_cap
	// hit after a normal post-OK drain).
	newUntil := longUntil.Add(2 * time.Minute)
	_ = newUntil
	s.markAccountDrain("a", 5*time.Minute)
	if !s.sessionDrain["a"].After(longUntil) {
		t.Fatalf("longer drain must extend: was=%v now=%v", longUntil, s.sessionDrain["a"])
	}
}
