package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"go-codearts/internal/huawei"
	"go-codearts/internal/store"
)

func accountHasCreds(a *store.Account) bool {
	return a != nil && a.Credentials != nil && a.Credentials.AccessKeyID != "" && a.Credentials.SecretAccessKey != ""
}

func (s *Server) firstCredAccount(key *store.APIKey) *store.Account {
	for _, a := range s.callableHuaweiAccountsFor(key) {
		if accountHasCreds(a) {
			return a
		}
	}
	for _, a := range s.db.List() {
		if !accountHasCreds(a) || !key.AllowsAccount(a.ID) {
			continue
		}
		return a
	}
	return nil
}

func (s *Server) callableHuaweiAccountsFor(key *store.APIKey) []*store.Account {
	now := time.Now()
	accounts := s.db.List()
	valid := make([]*store.Account, 0, len(accounts))
	for _, a := range accounts {
		if a == nil {
			continue
		}
		a.RecomputeCallGate(now)
		// ready + 透支中(quota_exhausted) 均可调用；其余硬门禁排除
		if !a.Callable(now) {
			continue
		}
		if !key.AllowsAccount(a.ID) {
			continue
		}
		valid = append(valid, a)
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].ID < valid[j].ID })
	return valid
}

func (s *Server) accountOAuthCreds(acc *store.Account) *huawei.OAuthTokenResult {
	if acc == nil || acc.Credentials == nil {
		return nil
	}
	return &huawei.OAuthTokenResult{
		AccessKeyID:     acc.Credentials.AccessKeyID,
		SecretAccessKey: acc.Credentials.SecretAccessKey,
		SecurityToken:   acc.Credentials.SecurityToken,
		Expiration:      acc.Credentials.Expiration,
	}
}

func (s *Server) recordAccountSuccessFull(a *store.Account, usageTokens, promptTok, completionTok float64, latencyMS int64) {
	if a == nil {
		return
	}
	now := time.Now()
	a.LastUsedAt = now
	a.LastSuccessAt = now
	a.LastError = ""
	a.SuccessCount++
	if promptTok > 0 {
		a.PromptTokens += promptTok
	}
	if completionTok > 0 {
		a.CompletionTokens += completionTok
	}
	if latencyMS > 0 {
		a.LastLatencyMS = latencyMS
	}
	if usageTokens > 0 && a.Quota != nil {
		a.Quota.NormalizeRemain()
		a.Quota.UsedTokens += usageTokens
		if a.Quota.PackageTokens > 0 {
			a.Quota.RemainTokens = a.Quota.PackageTokens - a.Quota.UsedTokens
			if a.Quota.RemainTokens < 0 {
				a.Quota.RemainTokens = 0
			}
			a.Quota.UsagePercent = a.Quota.UsedTokens / a.Quota.PackageTokens * 100
		} else if a.Quota.RemainTokens > 0 {
			a.Quota.RemainTokens -= usageTokens
			if a.Quota.RemainTokens < 0 {
				a.Quota.RemainTokens = 0
			}
		}
	}
	a.RecomputeCallGate(now)
	_ = s.db.Upsert(a)
}

func isTransientRotate(errMsg string) bool {
	f := friendlyLastError(errMsg)
	switch f {
	case "upstream timeout (rotated)", "upstream stream cut (rotated)", "session busy (rotated)", "client canceled":
		return true
	default:
		low := strings.ToLower(errMsg)
		return strings.Contains(low, "timeout") ||
			strings.Contains(low, "unexpected eof") ||
			strings.Contains(low, "connection reset")
	}
}

func isTransientUpstreamErr(err error) bool {
	if err == nil {
		return false
	}
	if isUpstreamBroken(err) {
		return true
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "timeout") ||
		strings.Contains(low, "deadline") ||
		strings.Contains(low, "apig.0203") ||
		strings.Contains(low, "backend timeout")
}

func (s *Server) recordAccountRotate(a *store.Account, errMsg string) {
	if a == nil {
		return
	}
	a.LastUsedAt = time.Now()
	a.LastError = ""
	a.RecomputeCallGate(time.Now())
	_ = s.db.Upsert(a)
	log.Printf("account rotate id=%s reason=%s", a.ID, friendlyLastError(errMsg))
}

func (s *Server) recordAccountFail(a *store.Account, errMsg string) {
	if a == nil {
		return
	}
	if isTransientRotate(errMsg) {
		s.recordAccountRotate(a, errMsg)
		return
	}
	a.LastUsedAt = time.Now()
	a.FailCount++
	if errMsg != "" {
		a.LastError = friendlyLastError(errMsg)
	}
	a.RecomputeCallGate(time.Now())
	_ = s.db.Upsert(a)
}

func (s *Server) softBanQuota(a *store.Account, reason, detail string) {
	if a == nil {
		return
	}
	now := time.Now()
	// 上游明确拒识 → 硬封禁；本地 remain=0 只标「透支中」仍可调。
	if reason == "upstream_rejected" {
		a.MarkUpstreamRejected(detail, now)
		_ = s.db.Upsert(a)
		log.Printf("account hard-ban upstream_rejected id=%s email=%s detail=%s", a.ID, a.Email, truncateStr(detail, 120))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := s.refreshAccountQuota(ctx, a); err == nil {
		a.RecomputeCallGate(now)
		_ = s.db.Upsert(a)
		return
	}
	a.MarkQuotaExhausted(reason, detail, now)
	_ = s.db.Upsert(a)
}

// friendlyLastError turns Huawei/APIG payloads into a short pool status.
// Raw APIG.0203 JSON on the account row looks like the key is dead; it is not.
func friendlyLastError(errMsg string) string {
	msg := strings.TrimSpace(errMsg)
	if msg == "" {
		return ""
	}
	switch {
	case looksLikeBackendTimeout(0, []byte(msg)) || strings.Contains(msg, "APIG.0203") || strings.Contains(msg, "APIG.0201"):
		return "upstream timeout (rotated)"
	case strings.Contains(msg, "stream cut"):
		return "upstream stream cut (rotated)"
	case looksLikeSessionCap(400, []byte(msg)) || strings.Contains(msg, "00001041") || strings.Contains(msg, "并发会话"):
		return "session busy (rotated)"
	case strings.Contains(strings.ToLower(msg), "context canceled"):
		return "client canceled"
	default:
		return truncateStr(msg, 160)
	}
}

// looksLikeBackendTimeout detects Huawei APIG gateway waiting too long on LLM.
// Retriable by rotating accounts when SSE headers are not yet committed.
func looksLikeBackendTimeout(status int, body []byte) bool {
	if status == http.StatusGatewayTimeout || status == http.StatusBadGateway {
		return true
	}
	s := string(body)
	if strings.Contains(s, "APIG.0203") || strings.Contains(s, "APIG.0201") {
		return true
	}
	if strings.Contains(s, "InferHub.002002001") || strings.Contains(s, "InferHub.002002002") ||
		strings.Contains(s, "InferHub.002002006") || strings.Contains(s, "SnapInferHub.002002001") ||
		strings.Contains(s, "SnapInferHub.002002002") || strings.Contains(s, "SnapInferHub.002002006") {
		return true
	}
	low := strings.ToLower(s)
	return strings.Contains(low, "backend timeout") || strings.Contains(low, "backend timed out") ||
		strings.Contains(s, "无空闲推理") || strings.Contains(s, "推断服务超时") ||
		strings.Contains(s, "推断时间过长")
}

// looksLikeSessionCap detects Huawei concurrent-session / SSE-connection limits.
// Official: TM.00001041 并发会话上限, InferHub.001001010 SSE连接超过限制.
// Retriable by switching accounts — not a hard client error.
func looksLikeSessionCap(status int, body []byte) bool {
	s := string(body)
	low := strings.ToLower(s)
	if sessionCapPayload(s, low) {
		return true
	}
	if status < 400 || status >= 500 {
		return false
	}
	var hw struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
		ErrMsg    string `json:"err_msg"`
		Code      string `json:"code"`
		Message   string `json:"message"`
	}
	if json.Unmarshal(body, &hw) == nil {
		code := hw.ErrorCode
		if code == "" {
			code = hw.Code
		}
		if sessionCapPayload(code, strings.ToLower(code)) {
			return true
		}
		msg := hw.ErrorMsg
		if msg == "" {
			msg = hw.Message
		}
		if msg == "" {
			msg = hw.ErrMsg
		}
		if sessionCapPayload(msg, strings.ToLower(msg)) {
			return true
		}
	}
	return sessionCapPayload(s, low)
}

func sessionCapPayload(s, low string) bool {
	if strings.Contains(s, "TM.00001041") || strings.Contains(s, "00001041") {
		return true
	}
	if strings.Contains(s, "001001010") || strings.Contains(s, "SSE连接超过") {
		return true
	}
	if strings.Contains(low, "too many sse") {
		return true
	}
	keys := []string{
		"并发会话", "会话数已达上限", "并发会话数", "并发会话数已达上限",
		"session limit", "concurrent session", "too many sessions", "session count", "max session",
	}
	for _, k := range keys {
		if strings.Contains(low, strings.ToLower(k)) || strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func looksLikeQuotaExhausted(status int, body []byte) bool {
	if status == http.StatusPaymentRequired {
		return true
	}
	low := strings.ToLower(string(body))
	keys := []string{
		"insufficient", "quota", "balance", "token limit", "tokens exceeded",
		"usage limit", "package expired", "no available package", "not enough",
		"额度", "余额不足", "套餐已用尽", "套餐过期", "用量超限", "超过限额",
		"account_insufficient", "model_not_activated", "packageexpired",
	}
	for _, k := range keys {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

func extractUsageTokens(body []byte) float64 {
	p, c := extractUsageParts(body)
	total := p + c
	var payload struct {
		Usage struct {
			TotalTokens float64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Usage.TotalTokens > 0 {
		return payload.Usage.TotalTokens
	}
	return total
}

func extractUsageParts(body []byte) (prompt, completion float64) {
	var payload struct {
		Usage struct {
			PromptTokens     float64 `json:"prompt_tokens"`
			CompletionTokens float64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Usage.PromptTokens, payload.Usage.CompletionTokens
}

// estimateUsageSplit splits a total_tokens value into prompt/completion when
// the upstream (Huawei InferHub) only returns total_tokens without a breakdown.
// It estimates prompt tokens from the request body size (~4 chars/token for
// English/code, ~2 for Chinese — use 3 as a blended average).
func estimateUsageSplit(reqBody []byte, total float64) (prompt, completion float64) {
	if total <= 0 {
		return 0, 0
	}
	// Rough prompt token estimate from request body.
	// Strip JSON overhead: only count message content characters.
	var root struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(reqBody, &root)
	contentChars := 0
	for _, m := range root.Messages {
		switch v := m.Content.(type) {
		case string:
			contentChars += len(v)
		case []any:
			for _, part := range v {
				if pm, ok := part.(map[string]any); ok {
					if s, _ := pm["text"].(string); s != "" {
						contentChars += len(s)
					}
				}
			}
		}
	}
	estimatedPrompt := float64(contentChars) / 3.0
	if estimatedPrompt <= 0 {
		estimatedPrompt = total * 0.5
	}
	if estimatedPrompt >= total {
		// Prompt estimate exceeds total — upstream likely counts differently.
		// Give 80% to prompt, 20% to completion as a safe fallback.
		estimatedPrompt = total * 0.8
	}
	return estimatedPrompt, total - estimatedPrompt
}

func peekModel(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Model
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (s *Server) StartQuotaSync(ctx context.Context) {
	if s.huaweiModel != nil {
		go s.huaweiModel.WarmLoop(ctx)
	}
	interval := s.cfg.QuotaSyncInterval
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	log.Printf("quota sync worker every %s", interval)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(8 * time.Second):
			s.syncAllQuotas(ctx)
		}
	}()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.syncAllQuotas(ctx)
			}
		}
	}()
	// Idle-session sweeper: reaps sessionTurns/sessionResp entries that
	// haven't been touched for a while, so a churn of short one-turn
	// conversations doesn't grow either map without bound.
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n := s.sweepIdleSessionTurns(time.Hour)
				m := s.sweepIdleSessionResp(time.Hour)
				if n > 0 || m > 0 {
					log.Printf("session_sweep turns=%d resp=%d", n, m)
				}
			}
		}
	}()
}

func (s *Server) syncAllQuotas(parent context.Context) {
	list := s.db.List()
	ok, fail := 0, 0
	for _, a := range list {
		if a == nil || a.Credentials == nil || !a.Credentials.Usable() {
			if a != nil {
				a.RecomputeCallGate(time.Now())
			}
			continue
		}
		ctx, cancel := context.WithTimeout(parent, 45*time.Second)
		_, err := s.refreshAccountQuota(ctx, a)
		cancel()
		if err != nil {
			fail++
			continue
		}
		ok++
	}
	log.Printf("quota sync done ok=%d fail=%d", ok, fail)
}

func (s *Server) handleDisableAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	a.Disabled = true
	a.DisabledReason = "manual"
	a.RecomputeCallGate(time.Now())
	if err := s.db.Upsert(a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, redactAccount(a))
}

func (s *Server) handleEnableAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	a.Disabled = false
	if a.DisabledReason == "manual" || a.DisabledReason == "upstream_rejected" {
		a.DisabledReason = ""
		a.QuotaBlockedAt = time.Time{}
	}
	if a.Credentials != nil && a.Credentials.Usable() {
		_, _ = s.refreshAccountQuota(r.Context(), a)
	}
	a.RecomputeCallGate(time.Now())
	if err := s.db.Upsert(a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, redactAccount(a))
}

func (s *Server) handleRefreshAllQuotas(w http.ResponseWriter, r *http.Request) {
	list := s.db.List()
	type item struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		OK    bool   `json:"ok"`
		Gate  string `json:"call_gate"`
		Error string `json:"error,omitempty"`
	}
	out := make([]item, 0, len(list))
	for _, a := range list {
		it := item{ID: a.ID, Email: a.Email}
		if a.Credentials == nil || !a.Credentials.Usable() {
			a.RecomputeCallGate(time.Now())
			it.Gate = a.CallGate
			it.Error = "no usable credentials"
			out = append(out, it)
			continue
		}
		if _, err := s.refreshAccountQuota(r.Context(), a); err != nil {
			it.Error = err.Error()
			it.Gate = a.CallGate
			out = append(out, it)
			continue
		}
		it.OK = true
		it.Gate = a.CallGate
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "total": len(out)})
}

// sessionSoftCapDefault: Huawei TM.00001041 hard-limit is 3 including
// leftover slots after [DONE]. Two live + one leftover = 3. Stay at 1.
const sessionSoftCapDefault = 1

// depletedSoftCap: same single-open as ready.
const depletedSoftCap = 1

// accountDrainAfterOK: beat after we FIN the chat TCP so APIG can
// drop the session. Idle-pool reuse was the real leftover; chat
// requests now set Request.Close. 3s wait is no longer needed.
const accountDrainAfterOK = 400 * time.Millisecond

// depletedDrainAfterOK spaces consecutive requests to an overdraft account so
// Huawei's per-account rate limiter recovers between calls (stability > latency).
const depletedDrainAfterOK = 2 * time.Second

// thinkLoopRetryDrainStep scales the per-attempt drain inside a single
// request's thinkLoop retry chain: attempt N gets N*step (sub2api-style
// growing same-account backoff; neither sub2api nor one-api/new-api/GPT Load
// has a reasoning-only-loop detector to match against directly — verified
// 2026-09-19 via source review, see gateway-scheduler-comparison research).
const thinkLoopRetryDrainStep = 5 * time.Second

// thinkLoopExhaustedDrain is the cross-REQUEST cooldown once all in-request
// thinkLoop retries (thinkLoops < maxThinkLoopRetries in api.go) are burned on
// one account. pickAccountForSession's candidate filter already checks
// sessionDrain for every caller, sticky or not — the old flat 1s drain just
// expired before it could matter, so the very next unrelated request could land
// right back on an account that just proved itself stuck reasoning-looping in a
// row. Raising the value alone closes that gap; no new tracking state needed.
const thinkLoopExhaustedDrain = 2 * time.Minute

// maxThinkLoopRetries caps NON-streaming think-stall retries. Streaming
// think-stalls no longer retry at all (a retry would open a second thinking
// block on the already-committed client stream — malformed). Was effectively 6
// (thinkLoops < 6); each attempt could burn a full reasoningOnlyTimeout, so 6
// stacked to ~9 minutes of client-visible ping-only silence — exactly the
// "will retry in 9m" magnitude Claude Code reported. With the timeout now 240s
// and streaming excluded, 2 bounds the rare non-stream case tightly.
const maxThinkLoopRetries = 2

// pickRecencyCap bounds the "coldest first" spread bonus (seconds). An account
// not picked for >= this long is treated as fully cold; a just-picked account
// scores 0. Bounded so recency only orders similar-load candidates.
const pickRecencyCap = 30

// accountDrainAfterCap: leftover slots after TM.00001041 still
// reject the same AK 12s later. With sessionSoftCap=2, keep 12s drain
// to account for 2 concurrent sessions + leftover slot.
const accountDrainAfterCap = 12 * time.Second

// sessionLeaseTTL: client cancel leaves a Huawei zombie. Use the
// longer window; abort is how slots stacked to 3.
const sessionLeaseTTL = accountDrainAfterCap

// accountSSERotateAfter: legacy default when ROTATE_BY_GATE=0.
const accountSSERotateAfter = 2

// Ready accounts keep sticky longer (prefix reuse). Sequential turns on one
// AK do not trip Huawei's 3-live-session cap if leftover drain holds.
const accountSSERotateAfterReady = 5

// Depleted accounts still rotate early — they are slow and leftover-prone.
const accountSSERotateAfterDepleted = 2

// maxChatRetries: hard cap on REAL upstream attempts per request.
// Inspired by new-api common.RetryTimes (default 3) and one-api config.RetryTimes.
// Selection skips (circuit-open / rate-limited / no slot) do NOT count against
// this — see upstreamTries in handleHuaweiChat.
const maxChatRetries = 6

// maxStreamingRetries: for STREAMING requests, fail fast to the client after
// this many real upstream attempts. Claude Code has its own retry logic — let
// it handle further retries rather than the gateway silently stacking 6×90s
// header-waits behind a committed SSE stream. Typical: first attempt succeeds;
// one failover covers transient single-account failures.
// Non-streaming requests still use the full maxChatRetries budget.
const maxStreamingRetries = 2

// streamingWallLimit: generous safety-net ceiling on total wall-clock time for
// a streaming request. The primary fast-fail mechanism is maxStreamingRetries;
// this only fires if something truly hangs (e.g., a single upstream attempt
// wedged past all header timeouts). 6 minutes covers a legitimate 240s thinking
// turn + relay overhead with room to spare.
// Override: STREAMING_WALL_LIMIT (seconds, e.g. "300").
var streamingWallLimit = 6 * time.Minute

func init() {
	if s := os.Getenv("STREAMING_WALL_LIMIT"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			streamingWallLimit = time.Duration(v) * time.Second
		}
	}
}

// maxEarlyModelErrRetries: separate, tighter budget for Huawei inline model
// errors (parseHuaweiModelErr) that arrive before any byte reached the
// client. These are silently retried on another account instead of failing
// the request, but a genuinely bad request (bad model name, policy reject)
// errors identically on every account — without this cap it would burn the
// whole maxChatRetries budget one account at a time before the client sees
// anything. Smaller than maxChatRetries so it fails fast and leaves the rest
// of the budget for transient infra causes (timeouts, 5xx, stream cuts).
const maxEarlyModelErrRetries = 3

// maxLoopIterations bounds total loop passes (upstream tries + selection skips)
// as a safety net against an infinite skip spin on a fully throttled pool.
// Normal termination is the lease-wait time cap; this only catches bugs.
const maxLoopIterations = 400

// leaseWaitMax: bounded wait for an account slot. No infinite queue, no ticket protocol.
const leaseWaitMax = 45 * time.Second

var errLeaseWait = errors.New("no free account slot")

// heavyRequestBytes: metrics only — never used to drop/rotate context or sticky.
const heavyRequestBytes = 48 << 10

func requestIsHeavy(body []byte) bool {
	return len(body) >= heavyRequestBytes
}

func (s *Server) softCap() int {
	if s != nil && s.sessionSoftCap > 0 {
		return s.sessionSoftCap
	}
	return sessionSoftCapDefault
}

func (s *Server) accountCap(a *store.Account) int {
	base := s.softCap()
	if a != nil && a.Depleted() {
		base = depletedSoftCap
	}
	if a == nil {
		return base
	}
	if obs := s.observedCap(a.ID); obs > 0 && obs < base {
		return obs
	}
	return base
}

const observedCapTTL = 15 * time.Minute

func (s *Server) observedCap(accID string) int {
	if s == nil || accID == "" {
		return 0
	}
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	if s.sessionObservedCap == nil {
		return 0
	}
	n := s.sessionObservedCap[accID]
	if n <= 0 {
		return 0
	}
	// Fast invalidation via generation watermark: if any SSE success ran AFTER
	// this cap was recorded, the cap is stale — the account demonstrated live
	// capacity, so we shouldn't keep starving new sticks against it. Falls
	// through to the 15min TTL when no post-cap success exists (still the
	// backstop for accounts that never recover).
	if s.lastSuccessGen != nil && s.sessionObservedGen != nil {
		if last, capGen := s.lastSuccessGen[accID], s.sessionObservedGen[accID]; last > capGen && capGen > 0 {
			delete(s.sessionObservedCap, accID)
			delete(s.sessionObservedAt, accID)
			delete(s.sessionObservedGen, accID)
			return 0
		}
	}
	if at, ok := s.sessionObservedAt[accID]; ok && time.Since(at) > observedCapTTL {
		delete(s.sessionObservedCap, accID)
		delete(s.sessionObservedAt, accID)
		delete(s.sessionObservedGen, accID)
		return 0
	}
	return n
}

func (s *Server) noteObservedCap(accID string, inFlightAtReject int) {
	if accID == "" {
		return
	}
	// in_flight=1 + SSE cap is a leftover Huawei slot, not a hard cap of 1.
	// Recording cap=1 makes the whole pool unusable for the next 15 minutes.
	if inFlightAtReject <= 1 {
		log.Printf("chat session_cap zombie account=%s in_flight=%d (keep soft cap)", accID, inFlightAtReject)
		return
	}
	hard := inFlightAtReject - 1
	if hard < 1 {
		hard = 1
	}
	// Closure keeps the critical section defer-Unlock safe (a panic inside the
	// map access can't leak sessionBusyMu, which sits under every session-affinity
	// path). Logging stays OUTSIDE the section — log.Print has its own mutex and
	// blocking on it while holding sessionBusyMu would stack contention across
	// every concurrent session_cap reject.
	changed := func() bool {
		s.sessionBusyMu.Lock()
		defer s.sessionBusyMu.Unlock()
		if s.sessionObservedCap == nil {
			s.sessionObservedCap = map[string]int{}
		}
		if s.sessionObservedAt == nil {
			s.sessionObservedAt = map[string]time.Time{}
		}
		if s.sessionObservedGen == nil {
			s.sessionObservedGen = map[string]uint64{}
		}
		prev := s.sessionObservedCap[accID]
		ch := prev == 0 || hard < prev
		if ch {
			s.sessionObservedCap[accID] = hard
		}
		s.sessionObservedAt[accID] = time.Now()
		// Snapshot the current success seq as this cap's generation. Any success
		// with a higher seq after this point invalidates the cap in observedCap().
		s.sessionObservedGen[accID] = s.observedCapSeq.Load()
		return ch
	}()
	if changed {
		log.Printf("chat observed_cap account=%s cap=%d in_flight=%d", accID, hard, inFlightAtReject)
	}
}

func accountRotateBusy(a *store.Account) time.Duration {
	if a != nil && a.Depleted() {
		return 10 * time.Second
	}
	return 5 * time.Second
}

func poolHasReady(valid []*store.Account, tried map[string]bool) bool {
	for _, a := range valid {
		if a != nil && !a.Depleted() && (tried == nil || !tried[a.ID]) {
			return true
		}
	}
	return false
}

func (s *Server) stickCount(accID string) int {
	if accID == "" || s.sessionStick == nil {
		return 0
	}
	m := s.sessionStick.CountByAccount()
	if m == nil {
		return 0
	}
	return m[accID]
}

func pruneLeasesLocked(leases []time.Time, now time.Time) []time.Time {
	if len(leases) == 0 {
		return nil
	}
	out := leases[:0]
	for _, exp := range leases {
		if exp.After(now) {
			out = append(out, exp)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// accountLoadLocked returns estimated upstream pressure (leases + in-flight).
// Caller must hold sessionBusyMu.
func (s *Server) accountLoadLocked(accID string, now time.Time) int {
	if s.sessionLeases != nil {
		s.sessionLeases[accID] = pruneLeasesLocked(s.sessionLeases[accID], now)
		if len(s.sessionLeases[accID]) == 0 {
			delete(s.sessionLeases, accID)
		}
	}
	n := 0
	if s.sessionLeases != nil {
		n += len(s.sessionLeases[accID])
	}
	if s.sessionInFlight != nil {
		n += s.sessionInFlight[accID]
	}
	return n
}

// tryReserveAccount atomically reserves in-flight + a lingering upstream lease.
// enforceCap=true rejects when estimated load already at soft-cap.
func (s *Server) tryReserveAccount(accID string, capN int, enforceCap, ignoreLeases bool) bool {
	if accID == "" {
		return false
	}
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	now := time.Now()
	if until, ok := s.sessionBusy[accID]; ok && now.Before(until) {
		return false
	}
	if until, ok := s.sessionDrain[accID]; ok && now.Before(until) {
		return false
	}
	if s.sessionInFlight == nil {
		s.sessionInFlight = map[string]int{}
	}
	if s.sessionLeases == nil {
		s.sessionLeases = map[string][]time.Time{}
	}
	load := s.sessionInFlight[accID]
	if !ignoreLeases {
		load = s.accountLoadLocked(accID, now)
	}
	if enforceCap && capN > 0 && load >= capN {
		return false
	}
	s.sessionInFlight[accID]++
	if s.lastPickedAt == nil {
		s.lastPickedAt = map[string]time.Time{}
	}
	s.lastPickedAt[accID] = now
	return true
}

func (s *Server) endAccountInFlight(accID string) {
	s.endAccountInFlightLease(accID, 0)
}

// endAccountInFlightLease frees in-flight and optionally records an upstream lease.
// Use a short lease on success; long lease after client cancel so sticky won't reuse zombies.
func (s *Server) endAccountInFlightLease(accID string, lease time.Duration) {
	if accID == "" {
		return
	}
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	now := time.Now()
	if s.sessionInFlight != nil {
		if s.sessionInFlight[accID] <= 1 {
			delete(s.sessionInFlight, accID)
		} else {
			s.sessionInFlight[accID]--
		}
	}
	if lease <= 0 {
		return
	}
	if s.sessionLeases == nil {
		s.sessionLeases = map[string][]time.Time{}
	}
	s.sessionLeases[accID] = pruneLeasesLocked(s.sessionLeases[accID], now)
	s.sessionLeases[accID] = append(s.sessionLeases[accID], now.Add(lease))
}

// markAccountDrain schedules accID to sit out until now+d. Take-MAX semantics:
// if a longer drain is already in force, keep it — no caller has a legitimate
// reason to SHORTEN a drain (e.g. a 30s regurgitation hold must not be clobbered
// by a routine 400ms post-OK drain that fires later in the same success path).
func (s *Server) markAccountDrain(accID string, d time.Duration) {
	if accID == "" || d <= 0 {
		return
	}
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	if s.sessionDrain == nil {
		s.sessionDrain = map[string]time.Time{}
	}
	until := time.Now().Add(d)
	if prev, ok := s.sessionDrain[accID]; ok && prev.After(until) {
		return
	}
	s.sessionDrain[accID] = until
}

func (s *Server) accountDraining(accID string) bool {
	if accID == "" {
		return false
	}
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	until, ok := s.sessionDrain[accID]
	return ok && time.Now().Before(until)
}

func (s *Server) releaseAccountAfterOK(accID string, depleted bool) {
	// Keep a leftover lease so load stays 1 until Huawei drops the SSE slot.
	// Overdraft (depleted) accounts are rate-limited upstream; space consecutive
	// requests with a longer drain so the next call doesn't immediately trip a
	// 429/backend-timeout on the same throttled account.
	drain := accountDrainAfterOK
	if depleted {
		drain = depletedDrainAfterOK
	}
	s.endAccountInFlightLease(accID, drain)
	s.markAccountDrain(accID, drain)
}

// sessionTurnsKey scopes the consecutive-success counter to (sessionID, accID).
// Was `accID` alone, which conflated turns across independent sessions: session
// A used account X for 4 turns and finished; session B then bound to X and was
// rotated on its FIRST turn because the counter had already crossed the rotate
// threshold. Composite key isolates the counters per session. Empty sessionID
// (rare — non-sticky path) degrades gracefully by scoping only on account.
func sessionTurnsKey(sessionID, accID string) string {
	if sessionID == "" {
		return "@" + accID
	}
	return sessionID + "\x00" + accID
}

func (s *Server) noteAccountSSESuccess(sessionID, accID string) int {
	if accID == "" {
		return 0
	}
	// Bump success seq + pin it against this account. Done inline (no extra
	// lock cycle) since we already hold sessionBusyMu below and lastSuccessGen
	// lives under the same mutex.
	seq := s.observedCapSeq.Add(1)
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	if s.sessionTurns == nil {
		s.sessionTurns = map[string]int{}
	}
	if s.sessionTurnsAt == nil {
		s.sessionTurnsAt = map[string]time.Time{}
	}
	if s.lastSuccessGen == nil {
		s.lastSuccessGen = map[string]uint64{}
	}
	s.lastSuccessGen[accID] = seq
	k := sessionTurnsKey(sessionID, accID)
	s.sessionTurns[k]++
	s.sessionTurnsAt[k] = time.Now()
	return s.sessionTurns[k]
}

func (s *Server) resetAccountSSESuccess(sessionID, accID string) {
	if accID == "" {
		return
	}
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	k := sessionTurnsKey(sessionID, accID)
	delete(s.sessionTurns, k)
	delete(s.sessionTurnsAt, k)
}

// resetAllSessionSuccessForAccount clears counters across every session for one
// account — used when the account itself changes state (drain / circuit open)
// and any surviving per-session counter would be stale.
func (s *Server) resetAllSessionSuccessForAccount(accID string) {
	if accID == "" {
		return
	}
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	suffix := "\x00" + accID
	altSuffix := "@" + accID
	for k := range s.sessionTurns {
		if strings.HasSuffix(k, suffix) || k == altSuffix {
			delete(s.sessionTurns, k)
			delete(s.sessionTurnsAt, k)
		}
	}
}

// sweepIdleSessionTurns drops sessionTurns/sessionTurnsAt entries idle for
// longer than idleTTL. Called periodically from the quota-sync worker so
// short-lived conversations that never trip the 5-turn rotation don't leak
// into an unbounded map under the hottest lock. Returns the number of
// entries reaped for observability.
func (s *Server) sweepIdleSessionTurns(idleTTL time.Duration) int {
	if idleTTL <= 0 {
		idleTTL = time.Hour
	}
	cutoff := time.Now().Add(-idleTTL)
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	reaped := 0
	for k, at := range s.sessionTurnsAt {
		if at.Before(cutoff) {
			delete(s.sessionTurns, k)
			delete(s.sessionTurnsAt, k)
			reaped++
		}
	}
	return reaped
}

// sweepIdleSessionResp drops sessionResp entries idle for longer than idleTTL.
// Called periodically from the quota-sync worker so a firehose of spoofed
// session-ids can't grow this map without bound (regurgitation detection only
// needs recent state — anything older than idleTTL was going to be dropped
// on the next write anyway). Runs under sessionRespMu to stay off the hot
// sessionBusyMu path.
func (s *Server) sweepIdleSessionResp(idleTTL time.Duration) int {
	if idleTTL <= 0 {
		idleTTL = time.Hour
	}
	cutoff := time.Now().Add(-idleTTL)
	s.sessionRespMu.Lock()
	defer s.sessionRespMu.Unlock()
	reaped := 0
	for k, v := range s.sessionResp {
		if v.updatedAt.Before(cutoff) {
			delete(s.sessionResp, k)
			reaped++
		}
	}
	return reaped
}

func (s *Server) rotateAfterFor(acc *store.Account) int {
	if s == nil || !s.cfg.RotateByGate {
		return accountSSERotateAfter
	}
	ready, dep := accountSSERotateAfterReady, accountSSERotateAfterDepleted
	if s.cfg.RotateAfterReady > 0 {
		ready = s.cfg.RotateAfterReady
	}
	if s.cfg.RotateAfterDepleted > 0 {
		dep = s.cfg.RotateAfterDepleted
	}
	if acc != nil && acc.Depleted() {
		return dep
	}
	return ready
}

func (s *Server) rotateStickyAfterTurns(sessionID string, acc *store.Account) {
	if acc == nil {
		return
	}
	n := s.noteAccountSSESuccess(sessionID, acc.ID)
	limit := s.rotateAfterFor(acc)
	if n < limit {
		return
	}
	s.resetAccountSSESuccess(sessionID, acc.ID)
	s.markAccountDrain(acc.ID, accountDrainAfterCap)
	if sessionID != "" && s.sessionStick != nil {
		s.sessionStick.Invalidate(sessionID)
	}
	log.Printf("chat sticky_rotate_after_turns account=%s n=%d limit=%d depleted=%v session=%s", acc.ID, n, limit, acc.Depleted(), sessionID)
}

// rotateOffSessionCap leaves an AK that already reported TM.00001041.
// A 5s hold + same-AK retry is a 400 every time and burns the open SSE.
func (s *Server) rotateOffSessionCap(acc *store.Account, sessionID string) {
	if acc == nil {
		return
	}
	if s.reqStats != nil {
		s.reqStats.NoteEvent("session_cap")
	}
	s.markAccountDrain(acc.ID, accountDrainAfterCap)
	s.markAccountSessionBusy(acc.ID, accountDrainAfterCap)
	s.resetAccountSSESuccess(sessionID, acc.ID)
	s.resetAllSessionSuccessForAccount(acc.ID)
	if sessionID != "" && s.sessionStick != nil {
		s.sessionStick.Invalidate(sessionID)
	}
}

// sessionRespInfo tracks the last successful assistant response for a session
// so we can detect regurgitation loops (same account emitting the SAME visible
// content on consecutive turns, which — with heavy tool loops carrying prior
// output in-context — means we're just paying tokens to replay the previous
// answer). See noteAssistantResponse.
type sessionRespInfo struct {
	fp        uint64
	dupCount  int // consecutive duplicates on same session
	updatedAt time.Time
}

const (
	// sessionRespTTL: gate stops firing once the session has been quiet this
	// long. Longer than any plausible tool loop; shorter than a full editing
	// session so old fingerprints don't false-match after context reset.
	sessionRespTTL = 10 * time.Minute
	// regurgitationDrain: how long to keep the offending account out of the
	// pool once it emits an identical response. Short: the account is fine,
	// it's the (input, model) combo that's degenerate; the drain just breaks
	// the sticky affinity so the next turn tries a fresh account.
	regurgitationDrain = 30 * time.Second
	// sessionRespReapThreshold: opportunistic TTL reap on write once the map
	// crosses this size.
	sessionRespReapThreshold = 1024
	// sessionRespHardCap: absolute ceiling on map size. When exceeded, drop
	// the oldest ~25% of entries by updatedAt. Prevents session-id spoofing
	// from turning this map into an unbounded lock-contended pool.
	sessionRespHardCap = 4096
)

// noteAssistantResponse records a response fingerprint for the session and
// returns (isDuplicate, consecutiveDups). isDuplicate means the same non-empty
// fingerprint was just recorded — the caller should treat the account as
// producing a regurgitation loop (invalidate sticky, drain briefly).
func (s *Server) noteAssistantResponse(sessionID, accID string, fp uint64) (bool, int) {
	_ = accID // reserved for future account-aware heuristics
	if sessionID == "" || fp == 0 {
		return false, 0
	}
	s.sessionRespMu.Lock()
	defer s.sessionRespMu.Unlock()
	if s.sessionResp == nil {
		s.sessionResp = map[string]sessionRespInfo{}
	}
	// Reap expired entries opportunistically — bounded, and it keeps the map
	// from growing unbounded on servers with many short-lived sessions.
	now := time.Now()
	if len(s.sessionResp) > sessionRespReapThreshold {
		for k, v := range s.sessionResp {
			if now.Sub(v.updatedAt) > sessionRespTTL {
				delete(s.sessionResp, k)
			}
		}
	}
	// Hard cap: after TTL reap, if we're still above the ceiling (session-id
	// spoofing flood), evict oldest by updatedAt down to ~75% capacity. O(N)
	// scan but only triggers past a large threshold, so amortized cost stays
	// bounded and the map size is guaranteed finite.
	if len(s.sessionResp) > sessionRespHardCap {
		target := sessionRespHardCap * 3 / 4
		type kt struct {
			k string
			t time.Time
		}
		items := make([]kt, 0, len(s.sessionResp))
		for k, v := range s.sessionResp {
			items = append(items, kt{k, v.updatedAt})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].t.Before(items[j].t) })
		toDrop := len(s.sessionResp) - target
		if toDrop > len(items) {
			toDrop = len(items)
		}
		for i := 0; i < toDrop; i++ {
			delete(s.sessionResp, items[i].k)
		}
	}
	prev, ok := s.sessionResp[sessionID]
	if ok && now.Sub(prev.updatedAt) > sessionRespTTL {
		ok = false
	}
	dup := ok && prev.fp == fp
	info := sessionRespInfo{fp: fp, updatedAt: now}
	if dup {
		info.dupCount = prev.dupCount + 1
	}
	s.sessionResp[sessionID] = info
	return dup, info.dupCount
}

func (s *Server) forgetSessionResponse(sessionID string) {
	if sessionID == "" {
		return
	}
	s.sessionRespMu.Lock()
	defer s.sessionRespMu.Unlock()
	delete(s.sessionResp, sessionID)
}

func (s *Server) clearAccountLeases(accID string) {
	if accID == "" {
		return
	}
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	if s.sessionLeases != nil {
		delete(s.sessionLeases, accID)
	}
}

// onSessionCap cools only this account. Do NOT wipe inFlight — other users may
// still be streaming on the same AK; the caller decrements its own slot.
func (s *Server) onSessionCap(accID, sessionID string) {
	if accID == "" {
		return
	}
	if s.reqStats != nil {
		s.reqStats.NoteEvent("session_cap")
	}
	load := 0
	s.sessionBusyMu.Lock()
	if s.sessionInFlight != nil {
		load = s.sessionInFlight[accID]
	}
	s.sessionBusyMu.Unlock()
	if load > 1 {
		s.noteObservedCap(accID, load)
	} else {
		log.Printf("chat session_cap zombie account=%s in_flight=%d session=%s", accID, load, sessionID)
	}
	n := 0
	if s.sessionStick != nil {
		n = s.sessionStick.InvalidateAccount(accID)
	}
	s.clearAccountLeases(accID)
	s.markAccountSessionBusy(accID, 6*time.Second)
	log.Printf("chat session_cap account=%s drained_stick=%d cooldown=6s in_flight=%d session=%s", accID, n, load, sessionID)
}

func (s *Server) isAccountSessionBusy(accID string) bool {
	if accID == "" {
		return false
	}
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	until, ok := s.sessionBusy[accID]
	return ok && time.Now().Before(until)
}

// pickReason disambiguates why pickAccountForSession returned (nil, false).
// The caller uses it to decide whether the next attempt is a short wait
// (sticky is briefly draining and we want to preserve prompt cache) vs. a
// pool-exhaustion condition (every candidate is at cap / reserve-race lost).
// Cases:
//   - pickReasonOK: a candidate was picked, this is the normal path.
//   - pickReasonStickyDraining: the sticky account is in a short post-success
//     drain; a fresh pick would break prompt-cache affinity, so caller waits.
//   - pickReasonNoCandidate: no ready or depleted candidate was even eligible
//     (all tried / circuit-open / busy / at-cap). Caller waits + retries fresh.
//   - pickReasonReserveRace: candidates existed but all lost the reserve race
//     to concurrent callers. Caller can retry immediately.
const (
	pickReasonOK = iota
	pickReasonStickyDraining
	pickReasonNoCandidate
	pickReasonReserveRace
)

// pickAccountForSession prefers sticky for the same conversation, then spreads
// new sessions across the least-loaded accounts. Multi-user relay: never pin
// everyone onto the lowest account ID. The bool return preserves the older
// (acc, stickyHit) shape for existing tests; api.go uses pickAccountForSessionEx
// to get the disambiguated reason.
func (s *Server) pickAccountForSession(valid []*store.Account, sessionID string, tried map[string]bool, skipSticky bool) (*store.Account, bool) {
	acc, sticky, _ := s.pickAccountForSessionEx(valid, sessionID, tried, skipSticky)
	return acc, sticky
}

// pickAccountForSessionEx is the disambiguated form. See pickReason* consts.
func (s *Server) pickAccountForSessionEx(valid []*store.Account, sessionID string, tried map[string]bool, skipSticky bool) (*store.Account, bool, int) {
	if !skipSticky && sessionID != "" && s.sessionStick != nil {
		if aid, ok := s.sessionStick.GetAndRefresh(sessionID); ok {
			var stickyAcc *store.Account
			for _, a := range valid {
				if a != nil && a.ID == aid {
					stickyAcc = a
					break
				}
			}
			if stickyAcc == nil {
				s.sessionStick.Invalidate(sessionID)
			} else if !tried[aid] {
				// 透支粘滞会把后续轮次拖进限速断流；池里有 ready 就松绑。
				if stickyAcc.Depleted() && poolHasReady(valid, tried) {
					s.sessionStick.Invalidate(sessionID)
				} else if s.circuit != nil && s.circuit.IsOpen(aid) {
					// Sticky account tripped its circuit — drop the binding and fall
					// through to fresh selection instead of picking a doomed account
					// that the post-pick check would just skip (burning a retry).
					s.sessionStick.Invalidate(sessionID)
					tried[aid] = true
				} else if s.isAccountSessionBusy(aid) {
					log.Printf("chat sticky_cap_failover session=%s account=%s", sessionID, aid)
					s.sessionStick.Invalidate(sessionID)
					tried[aid] = true
				} else if s.accountDraining(aid) {
					// Sticky account is in a SHORT post-success drain (a leftover lease
					// held until Huawei drops the SSE slot; depleted accounts get a
					// longer drain to space out throttled calls). Keep the original
					// wait-for-warm-account behavior: the drain clears fast and
					// preserves this session's prompt cache. This is deliberately
					// distinct from the at-cap case below — a drain is a brief
					// self-clearing cooldown, whereas an at-cap account can stay
					// saturated for the whole fan-out and must NOT be waited on.
					return nil, false, pickReasonStickyDraining
				} else if s.tryReserveAccount(aid, s.accountCap(stickyAcc), true, false) {
					return stickyAcc, true, pickReasonOK
				} else {
					// Reserve failed but the account is neither busy nor draining, so
					// it is at its Huawei session cap — every slot is held by THIS
					// session's own concurrent fan-out (Claude Code parallel tool calls
					// / subagents share one X-Claude-Code-Session-Id and all bind here).
					// The old `return nil, false` here sent api.go into a 150ms
					// lease-wait spin bounded only by leaseWaitMax=45s (observed
					// 19–32s, retry≈128) pinned to this one account while ~226 idle
					// accounts sat unused — a single session head-of-line-blocks its
					// own tail requests. Deliberately DON'T return: fall through to the
					// candidate scorer WITHOUT marking aid tried and WITHOUT
					// Invalidating. The scorer's own `load >= cap` check excludes the
					// saturated account from selection yet still counts it toward
					// hasReadyInPool, so an overflow request spills to a free READY
					// account immediately, still WAITS (returns nil) rather than dumping
					// onto a depleted account when every ready account is itself at cap,
					// and keeps the binding so the session's next non-concurrent turn
					// returns to the warm, prompt-cached account.
				}
			}
		}
	}

	type cand struct {
		a        *store.Account
		load     int
		stick    int
		recency  int // seconds since last pick (capped) — higher = colder = preferred
		priority int
	}
	var readyC, depC []cand
	// Track the strongest (lowest-numbered) priority tier seen for each pool
	// so we only consider higher-tier candidates when their tier is empty.
	readyMinPri := 0
	depMinPri := 0
	readyHavePri := false
	depHavePri := false
	hasReadyInPool := false
	now := time.Now()
	// Lazy-promote any expired blocked entries so the fast-path below sees
	// freshly-eligible accounts without waiting for a background sweeper.
	if s.accountShard != nil {
		s.accountShard.PromoteExpired(now)
	}
	for _, a := range valid {
		if a == nil || tried[a.ID] {
			continue
		}
		// Fast-path: shard lookup is a single RLock read and short-circuits the
		// per-account sessionBusyMu + circuit.Mutex acquisitions below. Shard
		// is a hook-synchronized cache; IsOpen below still catches drift.
		if s.accountShard != nil && !s.accountShard.IsReady(a.ID) {
			continue
		}
		// Skip open-circuit accounts at selection time (pure IsOpen read; Allow
		// would consume a half-open probe). Without this, a tripped account is
		// idle (load=0) so the scorer ranks it FIRST, and the post-pick circuit
		// check then burns a retry via continue — enough tripped accounts exhaust
		// maxChatRetries and 503 before any healthy account is tried. Half-open
		// stays selectable so it still receives probe traffic.
		if s.circuit != nil && s.circuit.IsOpen(a.ID) {
			continue
		}
		if !a.Depleted() {
			hasReadyInPool = true
		}
		s.sessionBusyMu.Lock()
		busy := false
		if until, ok := s.sessionBusy[a.ID]; ok && now.Before(until) {
			busy = true
		}
		draining := false
		if until, ok := s.sessionDrain[a.ID]; ok && now.Before(until) {
			draining = true
		}
		load := 0
		if !busy && !draining {
			load = s.accountLoadLocked(a.ID, now)
		}
		recency := pickRecencyCap
		if t, ok := s.lastPickedAt[a.ID]; ok {
			if secs := int(now.Sub(t).Seconds()); secs < pickRecencyCap {
				recency = secs
			}
		}
		s.sessionBusyMu.Unlock()
		if busy || draining || load >= s.accountCap(a) {
			continue
		}
		c := cand{a: a, load: load, stick: s.stickCount(a.ID), recency: recency, priority: a.Priority}
		if a.Depleted() {
			if !depHavePri || c.priority < depMinPri {
				depMinPri = c.priority
				depHavePri = true
			}
			depC = append(depC, c)
		} else {
			if !readyHavePri || c.priority < readyMinPri {
				readyMinPri = c.priority
				readyHavePri = true
			}
			readyC = append(readyC, c)
		}
	}
	// Priority tier gate: only candidates in the strongest tier compete. Lower
	// tiers exist purely as fallback when the top tier is empty (all busy /
	// tried) and hasReadyInPool has already forced spillover.
	if readyHavePri {
		filtered := readyC[:0]
		for _, c := range readyC {
			if c.priority == readyMinPri {
				filtered = append(filtered, c)
			}
		}
		readyC = filtered
	}
	if depHavePri {
		filtered := depC[:0]
		for _, c := range depC {
			if c.priority == depMinPri {
				filtered = append(filtered, c)
			}
		}
		depC = filtered
	}
	// 有额度先用。额度号只是打满并发时等，不立刻甩给透支（透支慢、易断）。
	under := readyC
	if len(under) == 0 {
		if hasReadyInPool {
			return nil, false, pickReasonNoCandidate
		}
		under = depC
	}
	if len(under) == 0 {
		return nil, false, pickReasonNoCandidate
	}
	scoreOf := func(c cand) int {
		pen := c.load*1000 + c.stick*10
		if rem := accountRemainTokens(c.a); rem > 0 {
			bonus := int(rem / 10_000)
			if bonus > 500 {
				bonus = 500
			}
			pen -= bonus
		}
		// Prefer the coldest account (longest since last pick). Bounded so it only
		// orders otherwise-similar accounts — the key lever for spreading load
		// across a throttled/rate-limited depleted pool so no single account is hit
		// often enough to trip its per-account rate limit.
		pen -= c.recency
		return pen
	}
	best := scoreOf(under[0])
	for _, c := range under[1:] {
		if sc := scoreOf(c); sc < best {
			best = sc
		}
	}
	var tied []cand
	for _, c := range under {
		if scoreOf(c) == best {
			tied = append(tied, c)
		}
	}
	start := 0
	if s != nil && len(tied) > 0 {
		start = int(s.pickSeq.Add(1) % uint64(len(tied)))
	}
	for i := 0; i < len(tied); i++ {
		c := tied[(start+i)%len(tied)]
		if s.tryReserveAccount(c.a.ID, s.accountCap(c.a), true, false) {
			return c.a, false, pickReasonOK
		}
	}
	return nil, false, pickReasonReserveRace
}

func accountRemainTokens(a *store.Account) float64 {
	if a == nil || a.Quota == nil {
		return 0
	}
	a.Quota.NormalizeRemain()
	if a.Quota.RemainTokens > 0 {
		return a.Quota.RemainTokens
	}
	return 0
}

func (s *Server) bindSessionAccount(sessionID string, acc *store.Account) {
	s.bindSessionAccountLCP(sessionID, "", acc)
}

// bindSessionAccountLCP binds sessionID → acc.ID and records lcpKey as a
// secondary index so a client that later rotates its primary session id
// but keeps the same conversation prefix still lands on the same account.
// Depleted accounts skip binding — they should not become the anchor for
// future turns of a healthy session.
func (s *Server) bindSessionAccountLCP(sessionID, lcpKey string, acc *store.Account) {
	if sessionID == "" || acc == nil || s.sessionStick == nil {
		return
	}
	if acc.Depleted() {
		return
	}
	if lcpKey == "" {
		s.sessionStick.Bind(sessionID, acc.ID)
		return
	}
	s.sessionStick.BindWithLCP(sessionID, acc.ID, lcpKey)
}

// tryLCPPromote is called before the picker when primary sticky is empty.
// If the client's conversation prefix (lcpKey) is already bound to an account
// under a different primary session id, promote the current primary id to
// that same account so the picker's sticky path lands immediately.
// Returns true when a promotion happened.
func (s *Server) tryLCPPromote(sessionID, lcpKey string) bool {
	if s == nil || s.sessionStick == nil {
		return false
	}
	if sessionID == "" || lcpKey == "" {
		return false
	}
	if _, ok := s.sessionStick.Get(sessionID); ok {
		return false // primary already binds; nothing to promote
	}
	_, accID, ok := s.sessionStick.LookupLCP(lcpKey)
	if !ok || accID == "" {
		return false
	}
	s.sessionStick.BindWithLCP(sessionID, accID, lcpKey)
	return true
}

func extractCachedTokens(body []byte) float64 {
	var payload struct {
		Usage struct {
			CachedTokens        float64 `json:"cached_tokens"`
			PromptTokensDetails struct {
				CachedTokens float64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			PromptCacheHitTokens float64 `json:"prompt_cache_hit_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return 0
	}
	if payload.Usage.PromptTokensDetails.CachedTokens > 0 {
		return payload.Usage.PromptTokensDetails.CachedTokens
	}
	if payload.Usage.CachedTokens > 0 {
		return payload.Usage.CachedTokens
	}
	return payload.Usage.PromptCacheHitTokens
}

func (s *Server) noteAccountCache(a *store.Account, stickyHit bool, cachedTokens float64) {
	if a == nil {
		return
	}
	changed := false
	if stickyHit {
		a.StickHits++
		changed = true
	}
	if cachedTokens > 0 {
		a.CachedTokens += cachedTokens
		changed = true
	}
	if !changed {
		return
	}
	_ = s.db.Upsert(a)
}

// totalInFlight sums live in-flight chat requests across all accounts, plus the
// count of accounts currently serving traffic (for the dashboard live panel).
func (s *Server) totalInFlight() (requests, activeAccounts int) {
	if s == nil {
		return 0, 0
	}
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	for _, n := range s.sessionInFlight {
		if n > 0 {
			requests += n
			activeAccounts++
		}
	}
	return requests, activeAccounts
}

func (s *Server) markAccountSessionBusy(accID string, d time.Duration) {
	if s == nil || accID == "" {
		return
	}
	if d <= 0 {
		d = 45 * time.Second
	}
	s.sessionBusyMu.Lock()
	defer s.sessionBusyMu.Unlock()
	if s.sessionBusy == nil {
		s.sessionBusy = map[string]time.Time{}
	}
	s.sessionBusy[accID] = time.Now().Add(d)
}
