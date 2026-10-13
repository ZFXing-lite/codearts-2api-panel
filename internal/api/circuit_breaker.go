package api

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CircuitBreaker implements a per-account circuit breaker with three states:
// closed (normal), open (rejecting), half-open (probing).
//
// Patterns synthesized from:
//   - new-api: ShouldDisableChannel + auto-ban on error keywords
//   - LiteLLM: CooldownCache with per-deployment cooldown timers
//   - uni-api: CooldownPolicy with error-type-aware cooldown durations
//   - one-api: monitor.ShouldDisableChannel + DisableChannel
type CircuitBreaker struct {
	mu               sync.Mutex
	failureThreshold int
	recoveryInterval time.Duration
	halfOpenMax      int

	state map[string]cbState

	// onOpen fires the first time an account transitions closed→open. readyAt
	// is when the circuit becomes probe-eligible (halfOpen). Hook is invoked
	// while cb.mu is held — hook implementations MUST NOT re-enter CircuitBreaker.
	onOpen func(accID string, readyAt time.Time)
	// onClose fires on RecordSuccess (open→closed or half-open→closed).
	// Same locking constraint as onOpen.
	onClose func(accID string)
}

type cbState struct {
	consecutiveFailures int
	openedAt            time.Time
	halfOpenProbes      int
	halfOpenSuccesses   int
	// customRecovery overrides recoveryInterval when set (e.g. from retry-after header).
	// Pattern from uni-api: rate_limit_cooling_time from upstream response.
	customRecovery time.Duration
	// errorType tracks the last error category for diagnostics.
	errorType string
}

const (
	cbClosed   = 0
	cbOpen     = 1
	cbHalfOpen = 2
)

// Error type classifications for cooldown duration.
// Pattern from uni-api CooldownPolicy + ProviderErrorClassifier.
const (
	errTypeNone           = ""
	errTypeRateLimit      = "rate_limit"      // 429 — use retry-after or short cooldown
	errTypeQuotaExhausted = "quota_exhausted" // quota hit — long cooldown (6h in uni-api)
	errTypeAuthError      = "auth_error"      // 401/403 — cred expired, long cooldown
	errTypeServerError    = "server_error"    // 5xx — short cooldown
	errTypeTimeout        = "timeout"         // transport timeout — short cooldown
	errTypeTransport      = "transport"       // connection reset/broken pipe — very short
)

func newCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		failureThreshold: 5,
		recoveryInterval: 30 * time.Second,
		halfOpenMax:      3,
		state:            map[string]cbState{},
	}
}

// SetHooks installs open/close observers. Pass nil to detach a side.
func (cb *CircuitBreaker) SetHooks(onOpen func(string, time.Time), onClose func(string)) {
	if cb == nil {
		return
	}
	cb.mu.Lock()
	cb.onOpen = onOpen
	cb.onClose = onClose
	cb.mu.Unlock()
}

// Allow checks if the account can be used. Returns false if circuit is open.
// In half-open state, allows a limited number of probe requests.
func (cb *CircuitBreaker) Allow(accID string) bool {
	if cb == nil || accID == "" {
		return true
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	s := cb.state[accID]
	now := time.Now()
	recovery := cb.recoveryInterval
	if s.customRecovery > 0 {
		recovery = s.customRecovery
	}
	switch s.currentState(now, recovery) {
	case cbClosed:
		return true
	case cbOpen:
		return false
	case cbHalfOpen:
		// Defence-in-depth: if we've been sitting in half-open at max probes
		// for >3x the recovery interval, assume the outstanding probes were
		// abandoned (e.g. panic before ReleaseProbe / long-running requests
		// that never completed cleanly) and let a fresh probe through. This
		// keeps a healthy account from being permanently gated by a leaked
		// probe count if ReleaseProbe was missed anywhere.
		if s.halfOpenProbes >= cb.halfOpenMax && now.Sub(s.openedAt) > 3*recovery {
			s.halfOpenProbes = 0
		}
		if s.halfOpenProbes < cb.halfOpenMax {
			s.halfOpenProbes++
			cb.state[accID] = s
			return true
		}
		return false
	}
	return true
}

// RecordSuccess resets the failure counter and closes the circuit.
func (cb *CircuitBreaker) RecordSuccess(accID string) {
	if cb == nil || accID == "" {
		return
	}
	cb.mu.Lock()
	_, wasTracked := cb.state[accID]
	delete(cb.state, accID)
	hook := cb.onClose
	cb.mu.Unlock()
	if wasTracked && hook != nil {
		hook(accID)
	}
}

// ReleaseProbe returns a half-open probe slot without recording success
// or failure. Callers use it when Allow() granted a slot but the account
// was skipped before running the upstream request (client abort, quota
// rotation, session_cap, transport pre-check failure). Without this the
// probe counter monotonically climbs to halfOpenMax and Allow returns
// false forever, permanently gating a healthy account after cooldown.
func (cb *CircuitBreaker) ReleaseProbe(accID string) {
	if cb == nil || accID == "" {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	s, ok := cb.state[accID]
	if !ok {
		return
	}
	if s.halfOpenProbes > 0 {
		s.halfOpenProbes--
		cb.state[accID] = s
	}
}

// RecordFailure increments the failure counter. Opens the circuit when threshold is reached.
func (cb *CircuitBreaker) RecordFailure(accID string) {
	cb.RecordFailureWithType(accID, errTypeServerError, 0)
}

// RecordFailureWithType records a failure with error classification.
// cooldownOverride (from retry-after header or policy) overrides the default recovery interval.
// Pattern from uni-api maybe_cool_provider_api_key + CooldownPolicy.
func (cb *CircuitBreaker) RecordFailureWithType(accID string, errType string, cooldownOverride time.Duration) {
	if cb == nil || accID == "" {
		return
	}
	cb.mu.Lock()
	s := cb.state[accID]
	priorOpenedAt := s.openedAt
	s.consecutiveFailures++
	s.errorType = errType

	if cooldownOverride > 0 {
		s.customRecovery = cooldownOverride
	} else {
		s.customRecovery = cooldownForErrorType(errType)
	}

	openedNow := false
	if s.consecutiveFailures >= cb.failureThreshold || !s.openedAt.IsZero() {
		s.openedAt = time.Now()
		s.halfOpenProbes = 0
		s.halfOpenSuccesses = 0
		// wasOpen == this account was already inside the open window; a re-open
		// (probe failure) or a threshold trip both flip into open, but only the
		// closed→open edge is a real state change worth notifying observers.
		recovery := s.customRecovery
		if recovery <= 0 {
			recovery = cb.recoveryInterval
		}
		wasOpen := !priorOpenedAt.IsZero() && time.Since(priorOpenedAt) < recovery
		openedNow = !wasOpen
	}
	cb.state[accID] = s
	hook := cb.onOpen
	readyAt := s.openedAt.Add(s.customRecovery)
	cb.mu.Unlock()
	if openedNow && hook != nil {
		hook(accID, readyAt)
	}
}

// cooldownForErrorType returns the appropriate cooldown duration based on error type.
// Pattern from uni-api CooldownPolicy:
//   - quota_exhausted → 6 hours (upstream quota hit, won't recover soon)
//   - auth_error → 10 minutes (cred expired, needs refresh)
//   - rate_limit → 60 seconds (retry-after or default)
//   - server_error → 30 seconds (transient, might recover)
//   - timeout → 15 seconds (transient network issue)
//   - transport → 5 seconds (connection reset, quick retry)
func cooldownForErrorType(errType string) time.Duration {
	switch errType {
	case errTypeQuotaExhausted:
		return 6 * time.Hour
	case errTypeAuthError:
		return 10 * time.Minute
	case errTypeRateLimit:
		// Shorter than the classic 60s: with a depleted-primary pool we want a
		// rate-limited account back in rotation quickly (Huawei's per-account
		// window is short). A Retry-After header, when present, overrides this.
		return 20 * time.Second
	case errTypeServerError:
		return 30 * time.Second
	case errTypeTimeout:
		return 15 * time.Second
	case errTypeTransport:
		return 5 * time.Second
	default:
		return 30 * time.Second
	}
}

// classifyError determines the error type from HTTP status code and error message.
// Pattern from uni-api ProviderErrorClassifier.
func classifyError(statusCode int, errMsg string) string {
	low := strings.ToLower(errMsg)
	switch {
	case statusCode == 429:
		return errTypeRateLimit
	case statusCode == 401 || statusCode == 403:
		return errTypeAuthError
	case statusCode >= 500:
		return errTypeServerError
	case strings.Contains(low, "quota") || strings.Contains(low, "exhausted") || strings.Contains(low, "额度"):
		return errTypeQuotaExhausted
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline"):
		return errTypeTimeout
	case strings.Contains(low, "connection reset") || strings.Contains(low, "broken pipe") || strings.Contains(low, "unexpected eof"):
		return errTypeTransport
	default:
		return errTypeServerError
	}
}

// extractRetryAfter parses the Retry-After header from upstream 429 responses.
// Pattern from uni-api _extract_retry_after_seconds.
// Returns 0 if header is absent or unparseable.
func extractRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	val := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if val == "" {
		return 0
	}
	// Try integer seconds first.
	if secs, err := strconv.Atoi(val); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	// Try HTTP-date format.
	if t, err := http.ParseTime(val); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

// IsOpen reports whether the circuit is currently open (for logging/metrics).
func (cb *CircuitBreaker) IsOpen(accID string) bool {
	if cb == nil || accID == "" {
		return false
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	s := cb.state[accID]
	recovery := cb.recoveryInterval
	if s.customRecovery > 0 {
		recovery = s.customRecovery
	}
	return s.currentState(time.Now(), recovery) == cbOpen
}

// State returns 0=closed, 1=open, 2=half-open for a given account.
func (cb *CircuitBreaker) State(accID string) int {
	if cb == nil || accID == "" {
		return cbClosed
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	s := cb.state[accID]
	recovery := cb.recoveryInterval
	if s.customRecovery > 0 {
		recovery = s.customRecovery
	}
	return s.currentState(time.Now(), recovery)
}

func (s cbState) currentState(now time.Time, recovery time.Duration) int {
	if s.consecutiveFailures == 0 {
		return cbClosed
	}
	if s.openedAt.IsZero() {
		return cbClosed
	}
	if now.Sub(s.openedAt) >= recovery {
		return cbHalfOpen
	}
	return cbOpen
}

// Snapshot returns open circuit count for dashboard.
func (cb *CircuitBreaker) Snapshot() map[string]int {
	if cb == nil {
		return nil
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	now := time.Now()
	out := map[string]int{}
	for id, s := range cb.state {
		recovery := cb.recoveryInterval
		if s.customRecovery > 0 {
			recovery = s.customRecovery
		}
		st := s.currentState(now, recovery)
		if st != cbClosed {
			out[id] = st
		}
	}
	return out
}

// CircuitDetail is a per-account snapshot for the introspection endpoint.
type CircuitDetail struct {
	AccountID  string    `json:"account_id"`
	State      string    `json:"state"` // "closed" | "open" | "half_open"
	Failures   int       `json:"failures"`
	OpenedAt   time.Time `json:"opened_at,omitempty"`
	ReadyAt    time.Time `json:"ready_at,omitempty"`
	ErrorType  string    `json:"error_type,omitempty"`
	HalfOpenIn int       `json:"half_open_probes,omitempty"`
	CooldownMS int64     `json:"cooldown_ms,omitempty"`
}

// SnapshotDetailed returns full per-account state for the admin introspection
// endpoint. Only non-closed entries are included (closed = healthy = nothing
// interesting to report). Unlike Snapshot(), this exposes error_type and
// cooldown remaining so operators can distinguish "will recover in 20s" from
// "quota exhausted for 6h".
func (cb *CircuitBreaker) SnapshotDetailed() []CircuitDetail {
	if cb == nil {
		return nil
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	now := time.Now()
	out := make([]CircuitDetail, 0, len(cb.state))
	for id, s := range cb.state {
		recovery := cb.recoveryInterval
		if s.customRecovery > 0 {
			recovery = s.customRecovery
		}
		st := s.currentState(now, recovery)
		if st == cbClosed {
			continue
		}
		d := CircuitDetail{
			AccountID:  id,
			Failures:   s.consecutiveFailures,
			OpenedAt:   s.openedAt,
			ErrorType:  s.errorType,
			HalfOpenIn: s.halfOpenProbes,
			CooldownMS: recovery.Milliseconds(),
		}
		switch st {
		case cbOpen:
			d.State = "open"
		case cbHalfOpen:
			d.State = "half_open"
		}
		if !s.openedAt.IsZero() {
			d.ReadyAt = s.openedAt.Add(recovery)
		}
		out = append(out, d)
	}
	return out
}
