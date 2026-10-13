package api

import (
	"net/http"
	"strconv"
	"time"
)

// handleGetCircuits returns per-account CircuitBreaker state.
// Only non-closed entries appear (closed = healthy). Response shape:
//
//	{
//	  "counts":   {"open": N, "half_open": N},
//	  "circuits": [ {account_id, state, failures, opened_at, ready_at, error_type, cooldown_ms}, ... ]
//	}
func (s *Server) handleGetCircuits(w http.ResponseWriter, r *http.Request) {
	details := s.circuit.SnapshotDetailed()
	counts := map[string]int{"open": 0, "half_open": 0}
	for _, d := range details {
		counts[d.State]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"counts":   counts,
		"circuits": details,
	})
}

// handleGetSessions returns live sticky bindings + LCP stats.
// Query params:
//
//	?limit=N   cap the bindings list (default 500, max 8192)
//	?bindings=0 skip the bindings array (stats-only mode for cheap polling)
func (s *Server) handleGetSessions(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 8192 {
			limit = n
		}
	}
	stats := s.sessionStick.Snapshot()
	byAcct := s.sessionStick.CountByAccount()
	resp := map[string]any{
		"stats":      stats,
		"by_account": byAcct,
	}
	if r.URL.Query().Get("bindings") != "0" {
		resp["bindings"] = s.sessionStick.SnapshotBindings(limit)
		resp["bindings_limit"] = limit
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetConfig returns the live server config with secrets redacted.
// Split into runtime (needs restart to change) vs hot_reloadable (fsnotify picks up)
// so operators can tell at a glance what's editable-live vs boot-locked.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	// Snapshot under sessionBusyMu to match the write side (ApplyReloadable
	// holds it while swapping the hot subset).
	s.sessionBusyMu.Lock()
	cfg := s.cfg
	s.sessionBusyMu.Unlock()

	runtime := map[string]any{
		"port":              cfg.Port,
		"base_path":         cfg.BasePath,
		"data_dir":          cfg.DataDir,
		"public_url":        cfg.PublicURL,
		"admin_user":        cfg.AdminUser,
		"admin_password":    redactPresence(cfg.AdminPassword),
		"api_key":           redactPresence(cfg.APIKey),
		"captcha_solver":    redactPresence(cfg.CaptchaSolverURL),
		"outbound_proxy":    redactPresence(cfg.OutboundProxy),
		"huawei_model_base": cfg.HuaweiModelBase,
		"huawei_models_url": cfg.HuaweiModelsURL,
		"register_area":     cfg.RegisterAreaCode,
		"max_request_bytes": cfg.MaxRequestBytes,
	}
	hot := map[string]any{
		"rotate_by_gate":             cfg.RotateByGate,
		"rotate_after_ready":         cfg.RotateAfterReady,
		"rotate_after_depleted":      cfg.RotateAfterDepleted,
		"header_wait_depleted":       cfg.HeaderWaitDepleted.String(),
		"nonstream_timeout_depleted": cfg.NonStreamTimeoutDepleted.String(),
		"early_envelope":             cfg.EarlyEnvelope,
		"force_thinking":             cfg.ForceThinking,
		"sse_idle_timeout":           cfg.SSEIdleTimeout.String(),
		"resp_cache_ttl":             cfg.RespCacheTTL.String(),
		"huawei_dispatch_rps":        cfg.HuaweiDispatchRPS,
		"key_rpm":                    cfg.KeyRPM,
		"key_max_inflight":           cfg.KeyMaxInFlight,
		"quota_sync_interval":        cfg.QuotaSyncInterval.String(),
	}
	// Non-config runtime values that are useful for verifying the process
	// booted with the intended pool size + concurrency ceiling.
	var callable int
	if s.db != nil {
		now := time.Now()
		for _, a := range s.db.SnapshotList() {
			if a != nil && a.Callable(now) {
				callable++
			}
		}
	}
	build := map[string]any{
		"session_soft_cap":  s.sessionSoftCap,
		"chat_sem_cap":      cap(s.chatSem),
		"callable_accounts": callable,
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runtime":        runtime,
		"hot_reloadable": hot,
		"build_info":     build,
	})
}

// redactPresence collapses any set value to "***set***" and empty to "unset".
// Prevents leaking secret values via GET /api/config while still letting
// operators verify presence.
func redactPresence(v string) string {
	if v == "" {
		return "unset"
	}
	return "***set***"
}
