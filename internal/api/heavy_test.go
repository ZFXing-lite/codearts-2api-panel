package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"go-codearts/internal/sessionstick"
	"go-codearts/internal/store"
)

func newPickServer() *Server {
	return &Server{
		sessionBusy:     map[string]time.Time{},
		sessionInFlight: map[string]int{},
		sessionLeases:   map[string][]time.Time{},
		sessionDrain:    map[string]time.Time{},
	}
}

func TestPickReadyBeforeDepleted(t *testing.T) {
	s := newPickServer()
	ready := &store.Account{ID: "ready", CallGate: store.CallGateReady}
	dep := &store.Account{ID: "over", CallGate: store.CallGateQuotaExhausted}
	acc, sticky := s.pickAccountForSession([]*store.Account{dep, ready}, "sess", map[string]bool{}, true)
	if sticky {
		t.Fatal("expected non-sticky")
	}
	if acc == nil || acc.ID != "ready" {
		t.Fatalf("want ready, got %+v", acc)
	}
}

func TestPickWaitsWhenReadyAtCap(t *testing.T) {
	s := newPickServer()
	s.sessionInFlight["ready"] = sessionSoftCapDefault
	ready := &store.Account{ID: "ready", CallGate: store.CallGateReady}
	dep := &store.Account{ID: "over", CallGate: store.CallGateQuotaExhausted}
	acc, _ := s.pickAccountForSession([]*store.Account{dep, ready}, "sess", map[string]bool{}, true)
	if acc != nil {
		t.Fatalf("must wait for ready, not dump onto depleted, got %s", acc.ID)
	}
}

func TestPickDepletedWhenNoReady(t *testing.T) {
	s := newPickServer()
	dep := &store.Account{ID: "over", CallGate: store.CallGateQuotaExhausted}
	acc, _ := s.pickAccountForSession([]*store.Account{dep}, "sess", map[string]bool{}, true)
	if acc == nil || acc.ID != "over" {
		t.Fatalf("want depleted fallback, got %+v", acc)
	}
}

func TestPickPrefersColdestDepleted(t *testing.T) {
	// Depleted-primary spread: among equal-load depleted accounts, the one used
	// least recently must win so no single throttled account is hammered.
	s := newPickServer()
	s.lastPickedAt = map[string]time.Time{"hot": time.Now()} // just picked
	hot := &store.Account{ID: "hot", CallGate: store.CallGateQuotaExhausted}
	cold := &store.Account{ID: "cold", CallGate: store.CallGateQuotaExhausted} // never picked
	acc, _ := s.pickAccountForSession([]*store.Account{hot, cold}, "", map[string]bool{}, true)
	if acc == nil || acc.ID != "cold" {
		t.Fatalf("recency spread: want coldest 'cold', got %+v", acc)
	}
}

func TestPickDepletedSoftCapOne(t *testing.T) {
	s := newPickServer()
	dep := &store.Account{ID: "over", CallGate: store.CallGateQuotaExhausted}
	acc, _ := s.pickAccountForSession([]*store.Account{dep}, "sess", map[string]bool{}, true)
	if acc == nil || acc.ID != "over" {
		t.Fatalf("first depleted slot, got %+v", acc)
	}
	s.sessionInFlight["over"] = 1
	acc, _ = s.pickAccountForSession([]*store.Account{dep}, "sess", map[string]bool{}, true)
	if acc != nil {
		t.Fatalf("depleted cap is 1, got %s", acc.ID)
	}
}

func TestPickStickyAtCapDoesNotDumpToDepleted(t *testing.T) {
	s := newPickServer()
	s.sessionStick = sessionstick.New(time.Minute, 8)
	s.sessionStick.Bind("sess", "ready")
	s.sessionInFlight["ready"] = sessionSoftCapDefault
	ready := &store.Account{ID: "ready", CallGate: store.CallGateReady}
	dep := &store.Account{ID: "over", CallGate: store.CallGateQuotaExhausted}
	acc, _ := s.pickAccountForSession([]*store.Account{dep, ready}, "sess", map[string]bool{}, false)
	if acc != nil {
		t.Fatalf("must wait for ready slot, got %s", acc.ID)
	}
	aid, ok := s.sessionStick.Get("sess")
	if !ok || aid != "ready" {
		t.Fatalf("sticky must stay on ready, got %q ok=%v", aid, ok)
	}
}

func TestPickStickyAtCapSpillsToReady(t *testing.T) {
	// Sticky "a" is at its Huawei session cap (all slots held by this session's
	// own concurrent fan-out). A ready account "b" is free, so the overflow
	// request must SPILL to "b" immediately instead of lease-wait-spinning
	// ~19–45s for "a" to free a slot. The binding stays on "a" so the next
	// non-concurrent turn returns to the warm, prompt-cached account.
	s := newPickServer()
	s.sessionStick = sessionstick.New(time.Minute, 8)
	s.sessionStick.Bind("sess", "a")
	s.sessionInFlight["a"] = sessionSoftCapDefault
	a := &store.Account{ID: "a", CallGate: store.CallGateReady}
	b := &store.Account{ID: "b", CallGate: store.CallGateReady}
	acc, sticky := s.pickAccountForSession([]*store.Account{a, b}, "sess", map[string]bool{}, false)
	if acc == nil || acc.ID != "b" {
		t.Fatalf("overflow must spill to ready 'b', got %+v", acc)
	}
	if sticky {
		t.Fatal("spill is a MISS, not a sticky hit")
	}
	aid, ok := s.sessionStick.Get("sess")
	if !ok || aid != "a" {
		t.Fatalf("binding must stay on 'a', got %q ok=%v", aid, ok)
	}
}

func TestPickStickyDepletedDropsWhenReady(t *testing.T) {
	s := newPickServer()
	s.sessionStick = sessionstick.New(time.Minute, 8)
	s.sessionStick.Bind("sess", "over")
	ready := &store.Account{ID: "ready", CallGate: store.CallGateReady}
	dep := &store.Account{ID: "over", CallGate: store.CallGateQuotaExhausted}
	acc, sticky := s.pickAccountForSession([]*store.Account{dep, ready}, "sess", map[string]bool{}, false)
	if sticky {
		t.Fatal("must not stick on depleted when ready exists")
	}
	if acc == nil || acc.ID != "ready" {
		t.Fatalf("want ready, got %+v", acc)
	}
	if _, ok := s.sessionStick.Get("sess"); ok {
		t.Fatal("depleted stick must be dropped")
	}
}

func TestPickStickyDrainWaitsInsteadOfFailover(t *testing.T) {
	s := newPickServer()
	s.sessionStick = sessionstick.New(time.Minute, 8)
	s.sessionStick.Bind("sess", "a")
	s.markAccountDrain("a", time.Minute)
	a := &store.Account{ID: "a", CallGate: store.CallGateReady}
	b := &store.Account{ID: "b", CallGate: store.CallGateReady}
	acc, _ := s.pickAccountForSession([]*store.Account{a, b}, "sess", map[string]bool{}, false)
	if acc != nil {
		t.Fatalf("must wait for sticky drain, got %s", acc.ID)
	}
	aid, ok := s.sessionStick.Get("sess")
	if !ok || aid != "a" {
		t.Fatalf("keep original stick, got %q ok=%v", aid, ok)
	}
}

func TestLooksLikeSSEConnCap(t *testing.T) {
	body := []byte(`{"error_code":"InferHub.001001010","error_msg":"SSE连接超过限制"}`)
	if !looksLikeSessionCap(http.StatusOK, body) {
		t.Fatal("001001010 on 200 must rotate")
	}
	if !looksLikeSessionCap(http.StatusBadRequest, body) {
		t.Fatal("001001010 on 400 must rotate")
	}
}

func TestNoteObservedCapIgnoresSingleInFlight(t *testing.T) {
	s := newPickServer()
	ready := &store.Account{ID: "ready", CallGate: store.CallGateReady}
	s.noteObservedCap("ready", 1)
	if s.accountCap(ready) != sessionSoftCapDefault {
		t.Fatalf("in_flight=1 must not lower cap, got %d", s.accountCap(ready))
	}
}

func TestObservedCapLowersReady(t *testing.T) {
	s := newPickServer()
	s.sessionSoftCap = 3
	ready := &store.Account{ID: "ready", CallGate: store.CallGateReady}
	s.noteObservedCap("ready", 3)
	if s.accountCap(ready) != 2 {
		t.Fatalf("observed hard 2, got %d", s.accountCap(ready))
	}
}

func TestAccountCapAndRotateBusy(t *testing.T) {
	s := newPickServer()
	ready := &store.Account{ID: "ready", CallGate: store.CallGateReady}
	dep := &store.Account{ID: "over", CallGate: store.CallGateQuotaExhausted}
	if s.accountCap(ready) != sessionSoftCapDefault {
		t.Fatalf("ready cap=%d", s.accountCap(ready))
	}
	if s.accountCap(dep) != depletedSoftCap {
		t.Fatalf("depleted cap=%d", s.accountCap(dep))
	}
	if accountRotateBusy(dep) <= accountRotateBusy(ready) {
		t.Fatal("depleted cooldown must be longer")
	}
}

func TestLooksLikeIdleInferTimeout(t *testing.T) {
	body := []byte(`{"error_code":"InferHub.002002002","error_msg":"无空闲推理服务"}`)
	if !looksLikeBackendTimeout(http.StatusOK, body) {
		t.Fatal("002002002 should rotate as timeout")
	}
}

func TestRequestIsHeavy(t *testing.T) {
	if requestIsHeavy(make([]byte, heavyRequestBytes-1)) {
		t.Fatal("under threshold should be light")
	}
	if !requestIsHeavy(make([]byte, heavyRequestBytes)) {
		t.Fatal("at threshold should be heavy")
	}
}

func TestFriendlyLastError(t *testing.T) {
	raw := `{"error_msg":"Backend timeout","error_code":"APIG.0203","request_id":"af98250a"}`
	if got := friendlyLastError(raw); got != "upstream timeout (rotated)" {
		t.Fatalf("timeout: %q", got)
	}
	if got := friendlyLastError(`{"error_code":"TM.00001041","error_msg":"并发会话数已达上限"}`); got != "session busy (rotated)" {
		t.Fatalf("session: %q", got)
	}
}

func TestHTTPClientUnboundIsDirect(t *testing.T) {
	s := &Server{}
	direct := s.sharedHTTPClient()
	if s.httpClientForAccount(&store.Account{ID: "a"}) != direct {
		t.Fatal("unbound account must not use proxy pool")
	}
	if s.httpClientForAccount(&store.Account{ID: "a", ProxyID: "  "}) != direct {
		t.Fatal("blank proxy_id must be direct")
	}
}

func TestParseHuaweiPromptTooLong(t *testing.T) {
	raw := []byte(`{"text":"[DONE]","error_code":"InferHub.ModelArts.81027.400","error_msg":"Prompt length exceeds: the prompt length 307782 must less than the maximum input length 307200"}`)
	code, msg, ok := parseHuaweiModelErr(raw)
	if !ok || code != "InferHub.ModelArts.81027.400" || !strings.Contains(msg, "307782") {
		t.Fatalf("parse: ok=%v code=%q msg=%q", ok, code, msg)
	}
	if !looksLikePromptTooLong(raw) {
		t.Fatal("expected prompt-too-long")
	}
}

func TestLooksLikeBackendTimeout(t *testing.T) {
	if !looksLikeBackendTimeout(http.StatusGatewayTimeout, nil) {
		t.Fatal("504 should match")
	}
	body := []byte(`{"error_msg":"Backend timeout","error_code":"APIG.0203"}`)
	if !looksLikeBackendTimeout(http.StatusOK, body) {
		t.Fatal("APIG.0203 body should match")
	}
	if looksLikeBackendTimeout(http.StatusBadRequest, []byte(`{"error":"nope"}`)) {
		t.Fatal("unrelated 400 should not match")
	}
}

func TestSuccessDrainIsSubSecond(t *testing.T) {
	if accountDrainAfterOK >= time.Second {
		t.Fatalf("success drain=%s; chat TCP close should keep this under 1s", accountDrainAfterOK)
	}
	if accountDrainAfterCap < 12*time.Second {
		t.Fatalf("cap recovery drain=%s want >=12s", accountDrainAfterCap)
	}
}

func TestRotateStickyAfterTwoTurns(t *testing.T) {
	s := newPickServer()
	s.sessionStick = sessionstick.New(time.Hour, 16)
	s.sessionStick.Bind("sess", "a")
	dep := &store.Account{ID: "a", CallGate: store.CallGateQuotaExhausted}
	s.rotateStickyAfterTurns("sess", dep)
	if _, ok := s.sessionStick.Get("sess"); !ok {
		t.Fatal("first success must keep sticky")
	}
	s.rotateStickyAfterTurns("sess", dep)
	if _, ok := s.sessionStick.Get("sess"); ok {
		t.Fatal("depleted account rotates after 2 successes")
	}
	if !s.accountDraining("a") {
		t.Fatal("rotated account must drain leftover Huawei slots")
	}
}

func TestRotateReadyKeepsStickyLonger(t *testing.T) {
	s := newPickServer()
	s.cfg.RotateByGate = true
	s.cfg.RotateAfterReady = 5
	s.sessionStick = sessionstick.New(time.Hour, 16)
	s.sessionStick.Bind("sess", "ready")
	ready := &store.Account{ID: "ready", CallGate: store.CallGateReady}
	for i := 0; i < 4; i++ {
		s.rotateStickyAfterTurns("sess", ready)
	}
	if _, ok := s.sessionStick.Get("sess"); !ok {
		t.Fatal("ready account must stay sticky through 4 turns")
	}
	s.rotateStickyAfterTurns("sess", ready)
	if _, ok := s.sessionStick.Get("sess"); ok {
		t.Fatal("ready account rotates on turn 5")
	}
}

func TestRotateOffSessionCapSkipsSameAK(t *testing.T) {
	s := newPickServer()
	s.sessionStick = sessionstick.New(time.Hour, 16)
	s.sessionStick.Bind("sess", "a")
	a := &store.Account{ID: "a", CallGate: store.CallGateReady}
	b := &store.Account{ID: "b", CallGate: store.CallGateReady}
	s.rotateOffSessionCap(a, "sess")
	if _, ok := s.sessionStick.Get("sess"); ok {
		t.Fatal("session cap must drop sticky")
	}
	acc, _ := s.pickAccountForSession([]*store.Account{a, b}, "sess", map[string]bool{"a": true}, false)
	if acc == nil || acc.ID != "b" {
		t.Fatalf("after session cap want b, got %+v", acc)
	}
}

func TestReleaseAfterOKBlocksPick(t *testing.T) {
	s := newPickServer()
	s.sessionInFlight["a"] = 1
	s.releaseAccountAfterOK("a", false)
	a := &store.Account{ID: "a", CallGate: store.CallGateReady}
	acc, _ := s.pickAccountForSession([]*store.Account{a}, "sess", map[string]bool{}, true)
	if acc != nil {
		t.Fatal("must wait for leftover Huawei slot")
	}
	s.sessionBusyMu.Lock()
	s.sessionDrain["a"] = time.Now().Add(-time.Second)
	delete(s.sessionLeases, "a")
	s.sessionBusyMu.Unlock()
	acc, _ = s.pickAccountForSession([]*store.Account{a}, "sess", map[string]bool{}, true)
	if acc == nil || acc.ID != "a" {
		t.Fatalf("after leftover expired want a, got %+v", acc)
	}
}
