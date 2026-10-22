package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go-codearts/internal/auth"
	"go-codearts/internal/cnup"
	"go-codearts/internal/config"
	"go-codearts/internal/huawei"
	"go-codearts/internal/proxypool"
	"go-codearts/internal/reqstats"
	"go-codearts/internal/respcache"
	"go-codearts/internal/sessionstick"
	"go-codearts/internal/store"
	"go-codearts/internal/webui"
)

type pendingOAuth struct {
	Verifier    string
	TicketID    string
	AccountID   string
	CallbackURL string
	LocalPort   string
	LoginEmail  string
	LoginPass   string
	Status      string // pending|success|failed
	Error       string
	StartedAt   time.Time
	cb          *huawei.LocalCallback
}

type Server struct {
	cfg            config.Config
	db             *store.DB
	auth           *auth.Manager
	proxies        *proxypool.Pool
	apiKeys        *store.APIKeyStore
	pendingOAuthMu sync.Mutex
	pendingOAuth   map[string]pendingOAuth

	huaweiModel *huawei.HuaweiModelClient

	respCache          *respcache.Cache
	reqStats           *reqstats.Tracker
	sessionStick       *sessionstick.Stick
	sessionBusyMu      sync.Mutex
	sessionBusy        map[string]time.Time   // accountID -> busy-until (TM.00001041 cooldown)
	sessionInFlight    map[string]int         // accountID -> in-flight chat requests
	sessionLeases      map[string][]time.Time // accountID -> upstream session lease expiries
	sessionDrain       map[string]time.Time   // accountID -> wait before reuse (Huawei SSE drain)
	sessionTurns       map[string]int         // sessionTurnsKey(sessionID,accID) -> consecutive successful SSE (per-session scoped)
	sessionTurnsAt     map[string]time.Time   // parallel to sessionTurns: last-touch, for idle sweep (BUG-24 fix)
	sessionObservedCap map[string]int         // accountID -> last Huawei-hard cap we hit
	sessionObservedAt  map[string]time.Time
	// sessionObservedGen: generation counter snapshot AT TIME the cap was
	// recorded. Compared against lastSuccessGen to fast-invalidate stale caps
	// once any successful SSE proves the account has capacity again — avoids
	// the 15min TTL wait when a Huawei session_cap event was a transient burst.
	sessionObservedGen map[string]uint64
	lastSuccessGen     map[string]uint64
	observedCapSeq     atomic.Uint64
	// sessionResp tracks the last assistant-response fingerprint per session.
	// Same fingerprint on two consecutive turns => model is regurgitating
	// (usually because the prior turn's output is now in-context and the
	// upstream just replays it verbatim). Detecting this here lets us break
	// the loop by rotating sticky and briefly draining the offending account
	// instead of burning heavy-context tokens on a doomed 3rd turn.
	sessionResp    map[string]sessionRespInfo // sessionID -> last response info
	sessionRespMu  sync.Mutex
	lastPickedAt   map[string]time.Time // accountID -> last time it was reserved (recency spread)
	sessionSoftCap int                  // max estimated upstream sessions per account (Huawei hard≈3)
	pickSeq        atomic.Uint64        // spread new sessions across equal-load accounts
	thinkMem       *thinkMem

	// Stability: circuit breaker + rate limiter per account
	circuit   *CircuitBreaker
	rateLimit *RateLimiter
	keyGate   *KeyGate

	// Ready/Blocked shard index — fast-path candidacy filter to shrink the
	// per-pick O(N) scan. Authority is still circuit_breaker.go; shard is a
	// hook-synchronized cache. See account_shard.go for details.
	accountShard *accountShard

	// Concurrent chat request semaphore — prevents account pool exhaustion
	// under burst load. Pattern from LiteLLM scheduler FlowItem.
	// Dynamically sized in New() based on account count for throughput.
	chatSem chan struct{}

	// dispatchLimiter paces new Huawei dispatches process-wide to stay under
	// Huawei's aggregate "50 per second" ceiling — see dispatch_limiter.go.
	dispatchLimiter *dispatchLimiter

	httpDefault   *http.Client // shared direct client — CLI-style connection reuse
	httpDefaultMu sync.Mutex

	// CN channel (国内码道) upstream plumbing — see cnchat.go.
	cnOnce   sync.Once
	cnClient *cnup.Client
	cnSemaMu sync.Mutex
	cnSema   map[string]chan struct{} // accountID -> per-account in-flight slots

	// Cached chat HTTP clients per (direct/proxy) key — avoids per-request
	// transport clone that defeated connection pooling (BUG-7 fix).
	chatClients *chatClientCache
}

func New(cfg config.Config, db *store.DB, authMgr *auth.Manager, proxies *proxypool.Pool) *Server {
	configureSessionIDSalt(cfg.DataDir)
	keys, err := store.OpenAPIKeys(cfg.DataDir)
	if err != nil {
		panic("open api keys: " + err.Error())
	}
	authMgr.SetBasePath(cfg.BasePath)
	sharedHTTP := defaultHTTPClient()
	hm := huawei.NewHuaweiModelClient(cfg.HuaweiModelBase, cfg.HuaweiModelsURL)
	hm.HTTP = sharedHTTP // WarmLoop + chat share one keep-alive pool (CLI-like)
	// Local response cache: opt-in via RESP_CACHE_TTL. Only serves NON-STREAM
	// identical-body requests (streaming clients never hit it). Default disabled.
	var respCache *respcache.Cache
	if cfg.RespCacheTTL > 0 {
		respCache = respcache.New(cfg.RespCacheTTL, 512, 64<<20)
	}
	srv := &Server{
		cfg:                cfg,
		db:                 db,
		auth:               authMgr,
		proxies:            proxies,
		apiKeys:            keys,
		pendingOAuth:       map[string]pendingOAuth{},
		huaweiModel:        hm,
		httpDefault:        sharedHTTP,
		respCache:          respCache,
		reqStats:           reqstats.New(),
		sessionBusy:        map[string]time.Time{},
		sessionInFlight:    map[string]int{},
		sessionLeases:      map[string][]time.Time{},
		sessionDrain:       map[string]time.Time{},
		sessionTurns:       map[string]int{},
		sessionObservedCap: map[string]int{},
		sessionObservedAt:  map[string]time.Time{},
		sessionObservedGen: map[string]uint64{},
		lastSuccessGen:     map[string]uint64{},
		sessionResp:        map[string]sessionRespInfo{},
		lastPickedAt:       map[string]time.Time{},
		thinkMem:           newThinkMem(),
		// sessionSoftCap=1 — one live SSE per account. Huawei counts sessions that outlive
		// our release: markChatConnClose is a no-op and the chat transport is HTTP/2 (so
		// Request.Close cannot force a teardown), so a completed request's server-side
		// session lingers past our 400ms drain. At 2, two just-released zombies + one live
		// hit Huawei's hard cap of 3 → spurious TM.00001041 (the 55 session_cap events). At
		// 1, live(1) + zombies(≤2) ≤ 3 by construction. The at-cap spill fix spreads a
		// session's fan-out across idle accounts, so per-account cap=1 costs ~0 aggregate
		// throughput (observed peak ~6 concurrent vs callable=226). Restores the default.
		sessionSoftCap:  1,
		sessionStick:    sessionstick.New(30*time.Minute, 8192),
		circuit:         newCircuitBreaker(),
		rateLimit:       newRateLimiter(0), // 0 = disabled by default
		keyGate:         newKeyGate(cfg.KeyRPM, cfg.KeyMaxInFlight),
		chatSem:         make(chan struct{}, chatSemCap(db)), // dynamic: 24 per account, min32 max1024 (see chatSemCap); fixed at boot, does not grow if accounts are added later
		chatClients:     newChatClientCache(),                // cached transport variants — connection pool reuse (BUG-7 fix)
		dispatchLimiter: newDispatchLimiter(float64(cfg.HuaweiDispatchRPS)),
		accountShard:    newAccountShard(),
	}
	// Seed shard with current callable account IDs so the picker fast-path is
	// warm at boot. hot-reload paths must call SyncShard again after mutating db.
	srv.syncShardFromDB()
	srv.circuit.SetHooks(
		func(id string, readyAt time.Time) { srv.accountShard.MarkBlocked(id, readyAt) },
		func(id string) { srv.accountShard.MarkReady(id) },
	)
	return srv
}

// ApplyReloadable swaps the hot-reloadable subset of config into the live
// Server. Called by the fsnotify/mtime watcher after config.ReloadEnv() +
// config.Get() picked up new .env values. Fields NOT in this whitelist need
// a full process restart (Port, DataDir, APIKey, admin creds, base path,
// captcha URL, HuaweiModel* URLs, MaxRequestBytes — all read once at boot).
//
// Held under sessionBusyMu so a picker reading s.cfg.RotateAfterReady mid-
// swap doesn't observe a torn struct. dispatchLimiter's rate is refreshed
// separately since it holds its own atomic.
func (s *Server) ApplyReloadable(cfg config.Config) {
	if s == nil {
		return
	}
	s.sessionBusyMu.Lock()
	s.cfg.RotateByGate = cfg.RotateByGate
	s.cfg.RotateAfterReady = cfg.RotateAfterReady
	s.cfg.RotateAfterDepleted = cfg.RotateAfterDepleted
	s.cfg.HeaderWaitDepleted = cfg.HeaderWaitDepleted
	s.cfg.NonStreamTimeoutDepleted = cfg.NonStreamTimeoutDepleted
	s.cfg.EarlyEnvelope = cfg.EarlyEnvelope
	s.cfg.ForceThinking = cfg.ForceThinking
	s.cfg.SSEIdleTimeout = cfg.SSEIdleTimeout
	s.cfg.RespCacheTTL = cfg.RespCacheTTL
	s.cfg.HuaweiDispatchRPS = cfg.HuaweiDispatchRPS
	s.cfg.KeyRPM = cfg.KeyRPM
	s.cfg.KeyMaxInFlight = cfg.KeyMaxInFlight
	s.sessionBusyMu.Unlock()
	// KeyGate 是独立 mutex，不受 sessionBusyMu 保护；单独同步
	if s.keyGate != nil {
		s.keyGate.SetLimits(cfg.KeyRPM, cfg.KeyMaxInFlight)
	}
	if s.dispatchLimiter != nil {
		s.dispatchLimiter.SetRPS(float64(cfg.HuaweiDispatchRPS))
	}
}

// SyncShardFromDB is the exported reload wrapper. Watcher wires this on
// accounts.json change AFTER db.ReloadFromDisk() succeeds.
func (s *Server) SyncShardFromDB() { s.syncShardFromDB() }

// syncShardFromDB refreshes the ready set from the current account snapshot.
// Safe to call after account CRUD or hot-reload.
func (s *Server) syncShardFromDB() {
	if s == nil || s.db == nil || s.accountShard == nil {
		return
	}
	now := time.Now()
	var ids []string
	for _, a := range s.db.SnapshotList() {
		if a != nil && a.Callable(now) {
			ids = append(ids, a.ID)
		}
	}
	s.accountShard.SeedReady(ids)
}

func (s *Server) Handler() http.Handler {
	inner := s.buildHandler()
	// Wrap with request ID middleware for tracing
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			reqID = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", reqID)
		r = r.WithContext(context.WithValue(r.Context(), ctxReqID, reqID))
		inner.ServeHTTP(w, r)
	})
	base := s.cfg.BasePath
	if base == "" {
		return wrapped
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path != base && !strings.HasPrefix(path, base+"/") {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(path, base)
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
		}
		wrapped.ServeHTTP(w, r2)
	})
}

// handleHealth returns 200 if the server is ready to accept requests.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	accounts := s.db.SnapshotList()
	callable := 0
	openCircuits := 0
	for _, a := range accounts {
		if a != nil && a.Callable(time.Now()) {
			callable++
			if s.circuit != nil && s.circuit.IsOpen(a.ID) {
				openCircuits++
			}
		}
	}
	status := http.StatusOK
	if callable == 0 {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"status":        map[string]any{"ok": status == http.StatusOK},
		"accounts":      len(accounts),
		"callable":      callable,
		"open_circuits": openCircuits,
	})
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleHealth)
	mux.HandleFunc("GET /", s.serveIndex)
	mux.HandleFunc("GET /login", s.serveIndex)
	mux.Handle("GET /static/", webui.StaticHandler())

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/csrf", s.handleCSRF)
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)
	mux.HandleFunc("GET /api/accounts", s.handleAccounts)
	mux.HandleFunc("POST /api/accounts", s.handleCreateAccount)
	mux.HandleFunc("POST /api/accounts/import", s.handleImportAccounts)
	mux.HandleFunc("POST /api/accounts/batch", s.handleBatchAccounts)
	mux.HandleFunc("PUT /api/accounts/{id}", s.handleUpdateAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	mux.HandleFunc("POST /api/accounts/{id}/token/refresh", s.handleRefreshToken)
	mux.HandleFunc("POST /api/accounts/{id}/iam-aksk", s.handleSetIAMAkSk)
	mux.HandleFunc("GET /api/accounts/{id}/quota", s.handleAccountQuota)
	mux.HandleFunc("POST /api/accounts/{id}/quota/refresh", s.handleAccountQuota)
	mux.HandleFunc("GET /api/accounts/{id}/models", s.handleAccountModels)
	mux.HandleFunc("POST /api/accounts/{id}/models/refresh", s.handleAccountModels)
	mux.HandleFunc("POST /api/accounts/{id}/insight/refresh", s.handleAccountInsightRefresh)
	mux.HandleFunc("PUT /api/accounts/{id}/disable", s.handleDisableAccount)
	mux.HandleFunc("PATCH /api/accounts/{id}/priority", s.handlePatchAccountPriority)
	mux.HandleFunc("PUT /api/accounts/{id}/enable", s.handleEnableAccount)
	mux.HandleFunc("POST /api/accounts/quota/refresh-all", s.handleRefreshAllQuotas)
	mux.HandleFunc("GET /api/oauth/start", s.handleOAuthStart)
	mux.HandleFunc("GET /api/oauth/status", s.handleOAuthStatus)
	mux.HandleFunc("GET /oauth/callback", s.handleOAuthCallback)

	mux.HandleFunc("GET /api/proxies", s.handleListProxies)
	mux.HandleFunc("POST /api/proxies", s.handleCreateProxy)
	mux.HandleFunc("POST /api/proxies/batch", s.handleBatchProxies)
	mux.HandleFunc("POST /api/proxies/import", s.handleImportProxies)
	mux.HandleFunc("POST /api/proxies/test", s.handleTestProxy)
	mux.HandleFunc("PUT /api/proxies/{id}", s.handleUpdateProxy)
	mux.HandleFunc("DELETE /api/proxies/{id}", s.handleDeleteProxy)
	mux.HandleFunc("POST /api/proxies/{id}/enable", s.handleEnableProxy)
	mux.HandleFunc("GET /api/proxies/strategy", s.handleProxyStrategy)
	mux.HandleFunc("PUT /api/proxies/strategy", s.handleProxyStrategy)
	mux.HandleFunc("PUT /api/accounts/{id}/proxy", s.handleSetAccountProxy)

	// 国内码道（CN 渠道）面板 API — 见 cnpanel.go
	mux.HandleFunc("GET /api/models/catalog", s.handleModelsCatalog)
	mux.HandleFunc("GET /api/cn/dashboard", s.handleCNDashboard)
	mux.HandleFunc("POST /api/cn/import", s.handleCNImport)
	mux.HandleFunc("POST /api/cn/accounts/{id}/refresh", s.handleCNRefresh)
	mux.HandleFunc("POST /api/cn/accounts/{id}/checkin", s.handleCNCheckin)
	mux.HandleFunc("POST /api/cn/accounts/{id}/relogin-clear", s.handleCNReloginClear)
	mux.HandleFunc("GET /api/cn/models", s.handleCNModels)

	mux.HandleFunc("GET /api/apikeys", s.handleListAPIKeys)
	mux.HandleFunc("POST /api/apikeys", s.handleCreateAPIKey)
	mux.HandleFunc("POST /api/apikeys/batch", s.handleBatchAPIKeys)
	mux.HandleFunc("PUT /api/apikeys/{id}", s.handleUpdateAPIKey)
	mux.HandleFunc("DELETE /api/apikeys/{id}", s.handleDeleteAPIKey)

	// Introspection endpoints — read-only visibility into scheduler internals.
	// Paired with the recent shard/LCP/observedCap-gen/hot-reload work so ops
	// can verify each subsystem without tailing logs.
	mux.HandleFunc("GET /api/circuits", s.handleGetCircuits)
	mux.HandleFunc("GET /api/sessions", s.handleGetSessions)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)

	adminHandler := s.auth.Middleware(s.csrfMiddleware(mux))

	modelMux := http.NewServeMux()
	modelMux.HandleFunc("GET /v1/models", s.handleHuaweiModels)
	modelMux.HandleFunc("HEAD /v1/models", s.handleHuaweiModels)
	modelMux.HandleFunc("GET /v1/models/", s.handleHuaweiModels)
	modelMux.HandleFunc("HEAD /v1/models/", s.handleHuaweiModels)
	modelMux.HandleFunc("GET /models", s.handleHuaweiModels)
	modelMux.HandleFunc("HEAD /models", s.handleHuaweiModels)
	modelMux.HandleFunc("GET /models/", s.handleHuaweiModels)
	modelMux.HandleFunc("HEAD /models/", s.handleHuaweiModels)
	modelMux.HandleFunc("POST /v1/chat/completions", s.handleHuaweiChat)
	modelMux.HandleFunc("POST /chat/completions", s.handleHuaweiChat)
	modelMux.HandleFunc("POST /v1/responses", s.handleHuaweiChat)
	modelMux.HandleFunc("POST /responses", s.handleHuaweiChat)
	modelMux.HandleFunc("POST /v1/messages", s.handleHuaweiChat)
	modelMux.HandleFunc("POST /messages", s.handleHuaweiChat)

	root := http.NewServeMux()
	// Health/readiness must be public for reverse proxies and container probes.
	root.HandleFunc("GET /health", s.handleHealth)
	root.HandleFunc("GET /ready", s.handleHealth)
	root.Handle("/", adminHandler)
	modelsAuth := s.apiKeyMiddleware(modelMux)
	for _, p := range []string{
		"/v1/models", "/v1/models/", "/models", "/models/",
		"/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses",
		"/v1/messages", "/messages",
	} {
		root.Handle(p, modelsAuth)
	}
	return root
}

func (s *Server) extractBearer(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if k := strings.TrimSpace(r.Header.Get("X-API-Key")); k != "" {
		return k
	}
	return ""
}

type ctxKey int

const (
	ctxAPIKey ctxKey = 1
	ctxReqID  ctxKey = 2
)

func apiKeyFromRequest(r *http.Request) *store.APIKey {
	if r == nil {
		return nil
	}
	k, _ := r.Context().Value(ctxAPIKey).(*store.APIKey)
	return k
}

// reqIDFromRequest returns the per-request trace ID assigned by the
// X-Request-ID middleware in Handler(). Empty if called outside that path
// (e.g. in tests that construct requests directly).
func reqIDFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	id, _ := r.Context().Value(ctxReqID).(string)
	return id
}

func (s *Server) noteAPIKeyUsage(key *store.APIKey, prompt, completion float64) {
	if s == nil || s.apiKeys == nil || key == nil || key.ID == "" {
		return
	}
	if err := s.apiKeys.AddUsage(key.ID, prompt, completion); err != nil {
		log.Printf("apikey usage id=%s err=%v", key.ID, err)
	}
}

func (s *Server) handleCSRF(w http.ResponseWriter, r *http.Request) {
	session := ""
	if cookie, err := r.Cookie("admin_session"); err == nil && s.auth.Validate(cookie.Value) {
		session = cookie.Value
	} else if token := r.Header.Get("X-Admin-Token"); s.auth.Validate(token) {
		session = token
	}
	if session == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": s.auth.CSRFToken(session)})
}

func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions || r.URL.Path == "/api/login" {
			next.ServeHTTP(w, r)
			return
		}
		session := ""
		if cookie, err := r.Cookie("admin_session"); err == nil && s.auth.Validate(cookie.Value) {
			session = cookie.Value
		} else if token := r.Header.Get("X-Admin-Token"); s.auth.Validate(token) {
			session = token
		}
		if session == "" || !s.auth.ValidateCSRF(session, r.Header.Get("X-CSRF-Token")) {
			writeErr(w, http.StatusForbidden, "csrf validation failed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) apiKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envKey := strings.TrimSpace(s.cfg.APIKey)
		hasManaged := s.apiKeys != nil && s.apiKeys.CountEnabled() > 0
		if envKey == "" && !hasManaged {
			next.ServeHTTP(w, r)
			return
		}
		token := s.extractBearer(r)
		if token != "" {
			gateID := ""
			if envKey != "" && token == envKey {
				gateID = "env"
			} else if hasManaged {
				if rec := s.apiKeys.Lookup(token); rec != nil {
					r = r.WithContext(context.WithValue(r.Context(), ctxAPIKey, rec))
					gateID = rec.ID
				}
			}
			if gateID == "" {
				writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
				return
			}
			if !s.acquireKeyGate(w, gateID) {
				return
			}
			defer s.releaseKeyGate(gateID)
			next.ServeHTTP(w, r)
			return
		}
		// Admin UI is same-origin and already logged in — allow session cookie
		// so /v1/models in the panel works without pasting a Bearer key.
		if c, err := r.Cookie("admin_session"); err == nil && s.auth.Validate(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
	})
}

func (s *Server) acquireKeyGate(w http.ResponseWriter, id string) bool {
	if s == nil || s.keyGate == nil || id == "" {
		return true
	}
	if s.keyGate.Acquire(id, s.keyInFlightShare()) {
		return true
	}
	writeErr(w, http.StatusTooManyRequests, "key concurrency or RPM limit reached")
	return false
}

func (s *Server) keyTenantCount() int {
	n := 0
	if s != nil && s.apiKeys != nil {
		n = s.apiKeys.CountEnabled()
	}
	if s != nil && strings.TrimSpace(s.cfg.APIKey) != "" {
		n++
	}
	if n < 1 {
		return 1
	}
	return n
}

// keyInFlightShare carves per-key in-flight from the global chatSem (Track 3b).
func (s *Server) keyInFlightShare() int {
	semCap := 32
	if s != nil && s.chatSem != nil {
		semCap = cap(s.chatSem)
	}
	share := semCap / s.keyTenantCount()
	if share < 1 {
		return 1
	}
	return share
}

func (s *Server) releaseKeyGate(id string) {
	if s != nil && s.keyGate != nil {
		s.keyGate.Release(id)
	}
}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	items := s.apiKeys.List()
	for _, k := range items {
		if s.keyGate != nil {
			k.InFlight = int64(s.keyGate.InFlight(k.ID))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":         items,
		"env_key_set":   strings.TrimSpace(s.cfg.APIKey) != "",
		"auth_required": strings.TrimSpace(s.cfg.APIKey) != "" || s.apiKeys.CountEnabled() > 0,
		"enabled_count": s.apiKeys.CountEnabled(),
	})
}

func (s *Server) sanitizeKeyAccountIDs(ids []string) ([]string, error) {
	ids = store.NormalizeAccountIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, err := s.db.Get(id); err != nil {
			continue
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, errors.New("no valid account_ids")
	}
	return out, nil
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string   `json:"name"`
		AccountIDs []string `json:"account_ids"`
	}
	_ = decodeJSONBody(r, &req)
	ids, err := s.sanitizeKeyAccountIDs(req.AccountIDs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, raw, err := s.apiKeys.Create(req.Name, ids)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":     rec,
		"secret":  raw,
		"warning": "明文密钥仅显示一次，请立即复制保存",
	})
}

func (s *Server) handleUpdateAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Enabled    *bool     `json:"enabled"`
		AccountIDs *[]string `json:"account_ids"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Enabled == nil && req.AccountIDs == nil {
		writeErr(w, http.StatusBadRequest, "enabled or account_ids required")
		return
	}
	var ids *[]string
	if req.AccountIDs != nil {
		clean, err := s.sanitizeKeyAccountIDs(*req.AccountIDs)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ids = &clean
	}
	rec, err := s.apiKeys.Update(id, req.Enabled, ids)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.apiKeys.Delete(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "deleted"})
}

func (s *Server) collectedModelIDs() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 16)
	add := func(ids []string) {
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	if s.huaweiModel != nil {
		add(s.huaweiModel.ModelIDs())
	}
	if s.db != nil {
		for _, a := range s.db.List() {
			if a != nil {
				add(a.Models)
			}
		}
	}
	add(huawei.FallbackModelIDs)
	// CN 渠道模型目录以 "cn:" 前缀合流（workbuddy2api 的 realm 前缀协议）：
	// 检出目录按账号缓存（cnwatch/登录时拉取），同 ID 与国际侧不冲突——
	// 客户端用前缀显式选渠道。
	if s.db != nil {
		for _, a := range s.db.List() {
			if a == nil || store.NormalizeChannel(a.Channel) != store.ChannelCN || a.UserID == "" {
				continue
			}
			infos, ok := cnup.AccountModels(a.UserID)
			if !ok {
				continue
			}
			ids := make([]string, 0, len(infos))
			for _, m := range infos {
				if m.ID != "" {
					ids = append(ids, store.ChannelCN+":"+m.ID)
				}
			}
			add(ids)
		}
	}
	return out
}

func (s *Server) warmModels(pick *store.Account) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var creds *huawei.OAuthTokenResult
	if pick != nil && pick.Credentials != nil {
		creds = &huawei.OAuthTokenResult{
			AccessKeyID:     pick.Credentials.AccessKeyID,
			SecretAccessKey: pick.Credentials.SecretAccessKey,
			SecurityToken:   pick.Credentials.SecurityToken,
		}
	}
	ctx = huawei.WithHTTPClient(ctx, s.httpClientForAccount(pick))
	if err := s.huaweiModel.RefreshModels(ctx, creds); err != nil {
		log.Printf("models refresh: %v", err)
	}
}

func (s *Server) handleHuaweiModels(w http.ResponseWriter, r *http.Request) {
	// Cursor times out if this waits on agent-center. Always return a valid
	// OpenAI list immediately; refresh catalog in the background.
	pick := s.firstCredAccount(apiKeyFromRequest(r))
	ids := s.collectedModelIDs()
	writeJSON(w, http.StatusOK, huawei.OpenAIModelsList(ids))
	go s.warmModels(pick)
}

func (s *Server) handleHuaweiChat(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	// Panic safety net for the account in-flight slot. All ~5 explicit
	// release call sites below clear leasedAccID to nil right after they
	// release; if a panic fires between pickAccountForSession success and
	// any release, this defer catches it and endAccountInFlight prevents
	// the slot from pinning until process restart.
	var leasedAccID string
	defer func() {
		if rec := recover(); rec != nil {
			if leasedAccID != "" {
				s.endAccountInFlight(leasedAccID)
			}
			log.Printf("chat panic rid=%s account=%q err=%v", reqIDFromRequest(r), leasedAccID, rec)
			if w.Header().Get("Content-Type") == "" {
				writeErr(w, http.StatusInternalServerError, "internal server error")
			}
		}
	}()
	// endLease / releaseLease are the ONLY release paths in this handler.
	// They clear leasedAccID so the panic-recovery defer above knows the
	// slot has already been released — preventing double-release and
	// keeping panics between iterations of the retry loop from leaking a
	// slot on the last-picked account.
	endLease := func(id string) {
		s.endAccountInFlight(id)
		// Return any half-open probe slot Allow() consumed but the caller
		// never resolved with RecordSuccess/RecordFailure — otherwise probes
		// leak until halfOpenMax and the account is permanently gated. A
		// no-op if the account is closed or already recorded (probe count 0).
		if s.circuit != nil {
			s.circuit.ReleaseProbe(id)
		}
		if leasedAccID == id {
			leasedAccID = ""
		}
	}
	releaseLease := func(id string, depleted bool) {
		s.releaseAccountAfterOK(id, depleted)
		if leasedAccID == id {
			leasedAccID = ""
		}
	}
	// Reject oversized requests early via Content-Length before reading body (BUG-23 fix).
	if cl := r.ContentLength; cl > 16<<20 {
		writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Reject non-JSON bodies fast with 400 instead of forwarding garbage upstream
	// (which burns account retries and returns a confusing 502).
	if len(body) == 0 || !json.Valid(body) {
		writeErr(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	// Reject deeply-nested JSON up-front. The hot path repeatedly walks the
	// body via json.Unmarshal → recursive map[string]any, which can blow the
	// goroutine stack on adversarial inputs. maxJSONDepth is generous
	// (100 levels) — real Claude messages nest at most ~10 deep.
	if depth := jsonDepth(body); depth > maxJSONDepth {
		writeErr(w, http.StatusBadRequest, "request json nesting depth exceeds limit")
		return
	}
	// Concurrent request semaphore — prevents account pool exhaustion under burst.
	// Pattern from LiteLLM scheduler: bounded parallel requests with short wait.
	// NewTimer + defer Stop instead of time.After: time.After leaks the underlying
	// timer for the full duration when the acquire wins the race, which under a
	// burst of thousands of concurrent chats piles up leaked timers in the runtime.
	semWait := time.NewTimer(500 * time.Millisecond)
	select {
	case s.chatSem <- struct{}{}:
		semWait.Stop()
		defer func() { <-s.chatSem }()
	case <-semWait.C:
		// Waited 500ms, still full — reject.
		writeErr(w, http.StatusServiceUnavailable, "gateway at capacity, please retry shortly")
		return
	case <-r.Context().Done():
		semWait.Stop()
		// Client disconnected while waiting.
		writeErr(w, http.StatusServiceUnavailable, "client disconnected")
		return
	}

	wantStream := requestWantsStream(body)
	heavy := requestIsHeavy(body) // metrics only — never strip context / break sticky

	sessionID, lcpKey := extractSessionIDs(r.Header, body)
	// If the client rotated its primary session id but the conversation prefix
	// is identical to a live binding, promote the new primary id onto the same
	// account so multi-turn prompt caches keep hitting.
	if sessionID != "" && lcpKey != "" {
		s.tryLCPPromote(sessionID, lcpKey)
	}
	rid := reqIDFromRequest(r)
	// Channel realm routing (borrowed the realm-prefix protocol from
	// workbuddy2api): "cn:<model>" forces CN-channel accounts,
	// "intl:<model>" forces international accounts, bare names keep the
	// dual-realm default. The prefix is stripped before sanitize so the
	// intl alias rewrite never mangles CN-canonical model names
	// (e.g. snap-chat must NOT become GLM-5.2 on the CN channel).
	cnRealm, cnBareModel := cnSplitChannelModel(peekModel(body))
	if cnRealm != "" {
		if body = rewriteBodyModel(body, cnBareModel); cnRealm == store.ChannelCN {
			body = sanitizeCLIChatBodyOpt(body, true)
		} else {
			body = sanitizeCLIChatBody(body)
		}
	} else {
		body = sanitizeCLIChatBody(body)
	}
	if cnRealm == store.ChannelCN && !s.cfg.CNEnabled {
		writeErr(w, http.StatusServiceUnavailable, "cn channel disabled (CN_ENABLED=false)")
		return
	}
	body = s.restoreThink(body)
	// Never rewrite the body to force it under Huawei's 307200-token limit —
	// the client owns conversation history and its own compaction (manual or
	// automatic); the gateway only warns when getting close, see
	// warnIfContextLarge in context_warn.go.
	estTokens := estimatePromptTokens(body)
	warnIfContextLarge(w, sessionID, estTokens)
	// Parsed once and reused for both the log-shape summary and the
	// tool-followup check below — this body previously got a second,
	// fully redundant json.Unmarshal pass here on every single chat
	// request (describeTurnShape used to parse its own copy internally).
	var shapeRoot map[string]any
	_ = json.Unmarshal(body, &shapeRoot)
	shape := ""
	if len(body) <= 200_000 {
		shape = describeTurnShape(shapeRoot, len(body))
	} else {
		shape = fmt.Sprintf("large body=%d", len(body))
	}
	toolFollowup := lastMessageIsToolFollowup(shapeRoot)
	log.Printf("chat turn_shape rid=%s session=%s followup=%v %s", rid, sessionID, toolFollowup, shape)

	reqKey := apiKeyFromRequest(r)
	// Protocol label for per-protocol stats: chat / messages (Claude) / responses.
	proto := detectClientProto(r)
	protoLabel := "chat"
	switch proto {
	case protoClaude:
		protoLabel = "messages"
	case protoResponses:
		protoLabel = "responses"
	}
	recordStats := func(stream, ok bool, prompt, completion, cached float64) {
		s.reqStats.Record(stream, false, ok, prompt, completion, cached, time.Since(started).Milliseconds(), protoLabel)
		s.noteAPIKeyUsage(reqKey, prompt, completion)
	}
	// Local response cache (opt-in). Identical sanitized body → cached OpenAI
	// completion, re-encoded to the client protocol on hit. Covers both streaming
	// (SSE replay) and non-streaming (JSON) requests.
	var cacheKey, cacheModel string
	if s.respCache != nil {
		cacheKey, cacheModel = respcache.KeyFromRequest(body)
		if ent, ok := s.respCache.Get(cacheKey); ok {
			log.Printf("chat cache_hit rid=%s key=%s model=%s stream=%v proto=%v", rid, cacheKey[:12], cacheModel, wantStream, proto)
			recordStats(wantStream, true, 0, 0, 0)
			cachePin := &ssePin{Proto: proto, Model: cacheModel}
			if wantStream {
				serveCachedAsProtoSSE(w, ent.Body, cachePin)
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(ent.Status)
				_, _ = w.Write(encodeProtoBody(proto, ent.Body, cachePin))
			}
			return
		}
	}
	valid := filterAccountsByRealm(s.callableHuaweiAccountsFor(reqKey), cnRealm)
	if len(valid) == 0 {
		recordStats(wantStream, false, 0, 0, 0)
		if reqKey != nil && len(reqKey.AccountIDs) > 0 {
			writeErr(w, http.StatusServiceUnavailable, "no callable account in this key's allowlist")
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "no callable account (all exhausted or disabled)")
		return
	}

	var lastErr error
	var lastStatus int
	gatewayTimeouts := 0
	sameAccountTimeouts := 0
	sameAccountPartials := 0
	thinkLoops := 0
	earlyModelErrRetries := 0
	// upstreamTries counts REAL Huawei calls (the retry budget). Selection skips
	// (circuit-open / rate-limited / no free slot) do NOT consume it — essential
	// for a throttled depleted pool where we must skip past many limited accounts
	// to reach a healthy one without prematurely 503-ing a long/multi-tool task.
	upstreamTries := 0
	tried := map[string]bool{}

	// Hint affinity before any wait so early SSE WriteHeader still carries them.
	if sessionID != "" && s.sessionStick != nil {
		if aid, ok := s.sessionStick.Get(sessionID); ok && aid != "" {
			w.Header().Set("X-Session-Affinity", "HIT")
			w.Header().Set("X-Account-Id", aid)
		}
	}

	relayCtx, relayCancel := context.WithCancel(r.Context())
	defer relayCancel()

	sseCommitted := false
	waitID := "chatcmpl-proxy-wait"
	if proto == protoClaude {
		waitID = "msg_proxy_wait"
	} else if proto == protoResponses {
		waitID = "resp_proxy_wait"
	}
	pin := &ssePin{ID: waitID, Model: peekModel(body), Proto: proto, SkipEnvelope: !s.cfg.EarlyEnvelope, InputTokens: estTokens}
	var stopTTFT func()
	// SSE commitment is deferred until a successful upstream response.
	// Early commitment (writing 200 + keepalive pings before upstream answers)
	// causes Claude Code to show "will retry in Xm · check your network" while
	// the gateway silently retries behind the scenes. By deferring, failed
	// attempts return a clean HTTP error and Claude Code retries itself — fast.
	isEarly := func() bool { return sseCommitted }

	for i := 0; ; i++ {
		// Hard wall-clock ceiling for streaming requests: prevents the client from
		// sitting in "will retry in Xm · check your network" for 7–9 minutes while
		// the gateway burns through its retry budget. The ceiling covers the entire
		// request lifetime (all attempts + backoff + header waits).
		if wantStream && time.Since(started) > streamingWallLimit {
			log.Printf("chat streaming_wall_limit rid=%s wall=%s committed=%v tries=%d",
				rid, time.Since(started).Round(time.Second), sseCommitted, upstreamTries)
			recordStats(wantStream, false, 0, 0, 0)
			if sseCommitted {
				writeSSEStopDone(w, pin)
			} else {
				writeErr(w, http.StatusGatewayTimeout, "streaming request exceeded wall-clock limit")
			}
			return
		}
		// Streaming fast-fail: return a clean error after maxStreamingRetries so
		// Claude Code can retry the whole request with its own (faster) backoff,
		// instead of sitting behind our silent multi-retry spiral.
		if wantStream && !sseCommitted && upstreamTries >= maxStreamingRetries {
			lastErr = fmt.Errorf("streaming fast-fail after %d upstream attempts", upstreamTries)
			goto pickExhausted
		}
		// Retry budget is REAL upstream attempts, not loop iterations. Skipping a
		// rate-limited/circuit-open account must not count as a retry.
		if upstreamTries > maxChatRetries {
			lastErr = fmt.Errorf("max retries (%d) exceeded", maxChatRetries)
			goto pickExhausted
		}
		// Safety bound on total iterations (skips included) to prevent an infinite
		// spin; the lease-wait time cap below is the normal termination.
		if i > maxLoopIterations {
			lastErr = errLeaseWait
			goto pickExhausted
		}
		// Exponential backoff with ±25% jitter after a real upstream failure.
		// Jitter prevents synchronized retry storms when many requests fail
		// at the same time (pattern from workbuddy2api/sub2api/GPT-Load).
		if upstreamTries > 0 && upstreamTries <= 6 {
			base := time.Duration(1<<uint(upstreamTries-1)) * 100 * time.Millisecond // 100ms..3.2s
			if base > 3*time.Second {
				base = 3 * time.Second
			}
			// ±25% jitter: multiply by 0.75..1.25
			var jitterBuf [1]byte
			rand.Read(jitterBuf[:])
			jitter := 0.75 + float64(jitterBuf[0])/512.0 // [0.75, 1.25)
			backoff := time.Duration(float64(base) * jitter)
			select {
			case <-relayCtx.Done():
				lastErr = relayCtx.Err()
				goto pickExhausted
			case <-time.After(backoff):
			}
		}

		acc, stickyHit, pickReason := s.pickAccountForSessionEx(valid, sessionID, tried, false)
		if acc != nil {
			leasedAccID = acc.ID
		}
		if err := relayCtx.Err(); err != nil {
			if acc != nil {
				endLease(acc.ID)
			}
			lastErr = err
			goto pickExhausted
		}
		if acc == nil {
			if time.Since(started) >= leaseWaitMax {
				lastErr = errLeaseWait
				goto pickExhausted
			}
			// Reserve-race lost = candidates existed, we just tied and lost. Retry
			// immediately with a tiny jitter so we don't burn 150ms while the pool
			// is actually healthy. Sticky-draining and no-candidate are actual pool
			// pressure — keep the 150ms cool-down.
			wait := 150 * time.Millisecond
			if pickReason == pickReasonReserveRace {
				wait = 10 * time.Millisecond
			}
			select {
			case <-relayCtx.Done():
				lastErr = relayCtx.Err()
				goto pickExhausted
			case <-time.After(wait):
			}
			valid = filterAccountsByRealm(s.callableHuaweiAccountsFor(reqKey), cnRealm)
			if i > 0 && i%12 == 0 {
				tried = map[string]bool{}
			}
			if i > 0 && i%40 == 0 {
				log.Printf("chat lease_wait rid=%s session=%s waited=%s callable=%d reason=%d", rid, sessionID, time.Since(started).Round(time.Millisecond), len(valid), pickReason)
			}
			continue
		}
		tried[acc.ID] = true
		// Circuit breaker: skip accounts with open circuits
		if s.circuit != nil && !s.circuit.Allow(acc.ID) {
			log.Printf("chat circuit_open rid=%s account=%s session=%s skip", rid, acc.ID, sessionID)
			endLease(acc.ID)
			continue
		}
		// Rate limiter: skip accounts over RPM limit
		if s.rateLimit != nil && !s.rateLimit.Allow(acc.ID) {
			log.Printf("chat rate_limited rid=%s account=%s session=%s skip", rid, acc.ID, sessionID)
			endLease(acc.ID)
			continue
		}
		if stickyHit {
			w.Header().Set("X-Session-Affinity", "HIT")
		} else if sessionID != "" {
			w.Header().Set("X-Session-Affinity", "MISS")
		} else {
			w.Header().Set("X-Session-Affinity", "NONE")
		}
		if heavy {
			w.Header().Set("X-Request-Heavy", "1")
		}
		w.Header().Set("X-Account-Id", acc.ID)
		if stickyHit {
			s.ensureChatClientWarm(acc)
		}
		// Sampled log — don't log every pick under load (BUG-18 fix).
		if i == 0 || i%8 == 0 {
			log.Printf("chat pick rid=%s session=%s account=%s sticky=%v depleted=%v heavy=%v body=%dB %s retry=%d", rid, sessionID, acc.ID, stickyHit, acc.Depleted(), heavy, len(body), shape, i)
		}
		// Reliability metric: any retry attempt that reaches an upstream call is a rotation.
		if i > 0 {
			s.reqStats.NoteEvent("rotation")
		}
		// CN channel (国内码道) — self-contained branch, see cnchat.go.
		if store.NormalizeChannel(acc.Channel) == store.ChannelCN {
			committed, cnRetry, cnOK, slotFull := s.handleCNChat(relayCtx, w, r, body, acc, wantStream, proto, peekModel(body))
			if committed {
				releaseLease(acc.ID, acc.Depleted())
				// 失败响应也走 committed 但必须按 ok=false 记账，别把 502 记成成功。
				recordStats(wantStream, cnOK, 0, 0, 0)
				return
			}
			endLease(acc.ID)
			if cnRetry {
				// slotFull = 本地并发槽满，换账号不算真实上游上游尝试，不消耗重试预算
				if !slotFull {
					upstreamTries++
				}
				continue
			}
			recordStats(wantStream, cnOK, 0, 0, 0)
			return
		}
		creds := s.accountOAuthCreds(acc)
		userToken := ""
		if acc.Metadata != nil {
			userToken = acc.Metadata["user_token"]
		}
		// Use non-stream timeout client for non-streaming requests to prevent indefinite hangs
		chatHTTP := s.httpClientForChat(acc, heavy)
		if !wantStream {
			chatHTTP = s.httpClientForChatNonStream(acc, heavy)
		}
		ctx := huawei.WithHTTPClient(relayCtx, chatHTTP)
		// Pace the actual dispatch against Huawei's aggregate rate ceiling (see
		// dispatch_limiter.go). A wait failure only happens on relayCtx dying
		// (client gone / deadline) — no real upstream call was attempted, so
		// this must NOT consume the account slot or the retry budget.
		throttled, werr := s.dispatchLimiter.wait(relayCtx)
		if werr != nil {
			endLease(acc.ID)
			lastErr = werr
			goto pickExhausted
		}
		if throttled > 20*time.Millisecond {
			log.Printf("chat dispatch_throttled rid=%s account=%s session=%s waited=%s", rid, acc.ID, sessionID, throttled.Round(time.Millisecond))
		}
		tUpstream := time.Now()
		upstreamTries++ // this is a real upstream call — counts against the retry budget
		resp, err := s.huaweiModel.ChatCompletions(ctx, creds, userToken, body)
		headerWait := time.Since(tUpstream)
		early := isEarly()
		if err != nil {
			lastErr = err
			if s.circuit != nil {
				// Classify error type for cooldown duration (uni-api CooldownPolicy pattern).
				errType := classifyError(0, err.Error())
				s.circuit.RecordFailureWithType(acc.ID, errType, 0)
			}
			if relayCtx.Err() != nil || isCancelErr(err) {
				s.reqStats.NoteEvent("client_abort")
				log.Printf("chat upstream_abort rid=%s account=%s reason=client_gone wait=%s", rid, acc.ID, time.Since(started).Round(time.Millisecond))
				recordStats(wantStream, false, 0, 0, 0)
				s.markAccountSessionBusy(acc.ID, 4*time.Second)
				s.endAccountInFlightLease(acc.ID, sessionLeaseTTL)
				if stopTTFT != nil {
					stopTTFT()
				}
				return
			}
			if isTransientUpstreamErr(err) {
				s.recordAccountRotate(acc, "upstream timeout (rotated)")
			} else {
				s.recordAccountFail(acc, err.Error())
			}
			if stickyHit {
				s.sessionStick.Invalidate(sessionID)
			}
			s.markAccountSessionBusy(acc.ID, accountRotateBusy(acc))
			log.Printf("chat upstream_fail rid=%s account=%s depleted=%v wait=%s body=%dB heavy=%v early=%v err=%v", rid, acc.ID, acc.Depleted(), headerWait.Round(time.Millisecond), len(body), heavy, early, err)
			endLease(acc.ID)
			continue
		}
		s.reqStats.RecordHeaderWait(headerWait.Milliseconds())
		log.Printf("chat upstream_headers rid=%s account=%s stream=%v wait=%s status=%d heavy=%v sticky=%v depleted=%v early=%v", rid, acc.ID, wantStream, headerWait.Round(time.Millisecond), resp.StatusCode, heavy, stickyHit, acc.Depleted(), early)

		// Streaming: pipe immediately so first token is not blocked by full-body buffer.
		if wantStream || isEventStream(resp) {
			if resp.StatusCode >= 400 {
				respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				closeHuaweiResp(resp)
				failClient := func(msg string) {
					if stopTTFT != nil {
						stopTTFT()
					}
					recordStats(true, false, 0, 0, 0)
					if isEarly() {
						// Stream already 200 + SSE. one-api closes with [DONE],
						// not an error event that OpenCode treats as interrupt.
						log.Printf("chat stream_close_done rid=%s reason=%s", rid, msg)
						writeSSEStopDone(w, pin)
					} else {
						writeUpstreamClientErr(w, resp.StatusCode, respBody)
					}
				}
				if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests ||
					resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable ||
					resp.StatusCode == http.StatusGatewayTimeout || resp.StatusCode >= 500 ||
					looksLikeBackendTimeout(resp.StatusCode, respBody) {
					lastStatus = resp.StatusCode
					endLease(acc.ID)
					if toolFollowup && looksLikeBackendTimeout(resp.StatusCode, respBody) {
						sameAccountTimeouts++
						delete(tried, acc.ID)
						s.markAccountDrain(acc.ID, 2*time.Second)
						log.Printf("chat tool_followup_504_hold rid=%s session=%s account=%s n=%d early=%v", rid, sessionID, acc.ID, sameAccountTimeouts, early)
						continue
					}
					s.recordAccountRotate(acc, "upstream timeout (rotated)")
					s.markAccountSessionBusy(acc.ID, accountRotateBusy(acc))
					// Record classified failure for circuit breaker (uni-api pattern).
					if s.circuit != nil {
						retryAfter := extractRetryAfter(resp)
						errType := classifyError(resp.StatusCode, string(respBody))
						s.circuit.RecordFailureWithType(acc.ID, errType, retryAfter)
					}
					if stickyHit {
						s.sessionStick.Invalidate(sessionID)
					}
					if looksLikeBackendTimeout(resp.StatusCode, respBody) {
						gatewayTimeouts++
						log.Printf("chat upstream_retry rid=%s status=%d account=%s depleted=%v early=%v gateway_timeouts=%d", rid, resp.StatusCode, acc.ID, acc.Depleted(), early, gatewayTimeouts)
					} else {
						log.Printf("chat upstream_retry rid=%s status=%d account=%s depleted=%v early=%v", rid, resp.StatusCode, acc.ID, acc.Depleted(), early)
					}
					continue
				}
				if looksLikeQuotaExhausted(resp.StatusCode, respBody) {
					lastStatus = resp.StatusCode
					s.softBanQuota(acc, "upstream_rejected", truncateStr(string(respBody), 300))
					if stickyHit {
						s.sessionStick.Invalidate(sessionID)
					}
					valid = filterAccountsByRealm(s.callableHuaweiAccountsFor(reqKey), cnRealm)
					endLease(acc.ID)
					log.Printf("chat quota_rotate rid=%s account=%s early=%v", rid, acc.ID, early)
					continue
				}
				if looksLikeSessionCap(resp.StatusCode, respBody) {
					lastStatus = resp.StatusCode
					lastErr = fmt.Errorf("session cap")
					log.Printf("chat session_cap rid=%s body=%s account=%s wait=%s early=%v", rid, truncateStr(string(respBody), 180), acc.ID, headerWait.Round(time.Millisecond), early)
					endLease(acc.ID)
					s.rotateOffSessionCap(acc, sessionID)
					log.Printf("chat session_cap rotate rid=%s early=%v account=%s -> next", rid, early, acc.ID)
					continue
				}
				// Upstream safety refusal text — return as normal assistant content.
				if looksLikeSensitiveContentReject(respBody) {
					text := extractUpstreamAssistText(respBody)
					if text == "" {
						text = strings.TrimSpace(string(respBody))
					}
					if stopTTFT != nil {
						stopTTFT()
					}
					log.Printf("chat upstream_safe_http rid=%s status=%d account=%s bytes=%d", rid, resp.StatusCode, acc.ID, len(text))
					s.bindSessionAccountLCP(sessionID, lcpKey, acc)
					recordStats(true, true, 0, 0, 0)
					releaseLease(acc.ID, acc.Depleted())
					s.rotateStickyAfterTurns(sessionID, acc)
					writeTextAsChatSSE(w, isEarly(), text, peekModel(body), "")
					return
				}
				lastStatus = resp.StatusCode
				s.recordAccountFail(acc, truncateStr(string(respBody), 300))
				if stickyHit {
					s.sessionStick.Invalidate(sessionID)
				}
				endLease(acc.ID)
				if _, msg, ok := parseHuaweiModelErr(respBody); ok {
					failClient(msg)
					return
				}
				failClient("upstream temporarily unavailable, please retry")
				return
			}
			if stopTTFT != nil {
				stopTTFT()
			}
			var cachedBody []byte
			var usage float64
			var relayErr error
			streamCtx := relayCtx
			if acc.Depleted() {
				streamCtx = withSSEIdle(relayCtx, sseUpstreamIdleDepleted)
			}
			// Shared by both the streaming pipe and the non-stream JSON assembler
			// below — either path may see the same pre-first-byte Huawei inline
			// model error, and both are equally safe to retry on another account.
			streamCtx = withEarlyModelErrBudget(streamCtx, maxEarlyModelErrRetries-earlyModelErrRetries)
			if wantStream {
				alreadySSE := sseCommitted
				sseCommitted = true
				if !alreadySSE {
					copyResponseHeaders(w, resp)
					w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
					w.Header().Set("X-Accel-Buffering", "no")
					w.Header().Set("Cache-Control", "no-cache, no-transform")
					w.Header().Set("Connection", "keep-alive")
					w.Header().Set("X-Cache", "BYPASS")
					w.Header().Del("Content-Length")
					w.Header().Del("Content-Encoding")
					if heavy {
						w.Header().Set("X-Request-Heavy", "1")
					}
					w.WriteHeader(resp.StatusCode)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					cachedBody, usage, relayErr = relayNormalizedSSEOpt(streamCtx, w, resp.Body, false, pin)
				} else {
					cachedBody, usage, relayErr = relayNormalizedSSEOpt(streamCtx, w, resp.Body, true, pin)
				}
			} else {
				cachedBody, usage, relayErr = readSSEAsCompletionJSON(streamCtx, resp.Body)
				if relayErr == nil {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Cache", "MISS")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(encodeProtoBody(pin.Proto, cachedBody, pin))
				}
			}
			closeHuaweiResp(resp)
			if relayErr != nil {
				log.Printf("chat sse assemble err rid=%s account=%s stream=%v: %v", rid, acc.ID, wantStream, relayErr)
				wired := errors.Is(relayErr, errSSEWired)
				canceled := isCancelErr(relayErr) || relayCtx.Err() != nil
				if wired {
					// Thinking/content already flushed. Retry = second generation = cut.
					if canceled {
						s.reqStats.NoteEvent("client_abort")
						// Client cancelling a wired stream mid-flight is the
						// strongest tell that the upstream got stuck — a happy
						// path finishes in seconds and the client gladly keeps
						// it. Break sticky so the SAME session doesn't come
						// back on the SAME account next turn only to loop
						// again on the same bad (input, KV-cache) combo.
						if sessionID != "" && s.sessionStick != nil {
							s.sessionStick.Invalidate(sessionID)
						}
						// Forget the last-response fingerprint too: the client
						// aborted mid-stream so the response is incomplete,
						// don't let a partial hash false-match future turns.
						s.forgetSessionResponse(sessionID)
					}
					log.Printf("chat sse_wired_hold rid=%s account=%s canceled=%v err=%v", rid, acc.ID, canceled, relayErr)
					recordStats(wantStream, false, 0, 0, 0)
					s.endAccountInFlightLease(acc.ID, sessionLeaseTTL)
					if !canceled && sseCommitted {
						writeSSEStopDone(w, pin)
					}
					return
				}
				// Same trigger as errHuaweiClient (parseHuaweiModelErr) but only ever
				// returned while firstByteAt was still zero — nothing reached the
				// client yet on either path (stream or assembled-JSON), so silently
				// rotate instead of failing the request. Bounded by
				// maxEarlyModelErrRetries, separate from the general upstreamTries
				// budget, so a deterministically-bad request (errors identically on
				// every account) still fails fast instead of burning the whole pool.
				if errors.Is(relayErr, errSSEEarlyModelErr) {
					if looksLikeSessionCap(400, []byte(relayErr.Error())) {
						lastErr = relayErr
						s.recordAccountFail(acc, "session busy (rotated)")
						s.onSessionCap(acc.ID, sessionID)
						endLease(acc.ID)
						log.Printf("chat sse_early_model_err_session_cap rid=%s account=%s depleted=%v -> next", rid, acc.ID, acc.Depleted())
						continue
					}
					earlyModelErrRetries++
					lastErr = relayErr
					tried[acc.ID] = true
					s.recordAccountRotate(acc, truncateStr(relayErr.Error(), 160))
					s.markAccountSessionBusy(acc.ID, accountRotateBusy(acc))
					endLease(acc.ID)
					log.Printf("chat sse_early_model_err_retry rid=%s account=%s session=%s stream=%v n=%d err=%v", rid, acc.ID, sessionID, wantStream, earlyModelErrRetries, relayErr)
					continue
				}
				if errors.Is(relayErr, errHuaweiClient) {
					if looksLikeSessionCap(400, []byte(relayErr.Error())) {
						lastErr = relayErr
						s.recordAccountFail(acc, "session busy (rotated)")
						s.onSessionCap(acc.ID, sessionID)
						endLease(acc.ID)
						log.Printf("chat sse_session_cap rid=%s account=%s depleted=%v -> next", rid, acc.ID, acc.Depleted())
						continue
					}
					s.recordAccountRotate(acc, truncateStr(relayErr.Error(), 160))
					recordStats(wantStream, false, 0, 0, 0)
					endLease(acc.ID)
					if !wantStream && !sseCommitted {
						writeUpstreamClientErr(w, http.StatusBadRequest, []byte(relayErr.Error()))
					}
					return
				}
				idled := errors.Is(relayErr, errSSEIdleTimeout) || strings.Contains(strings.ToLower(relayErr.Error()), "idle timeout")
				partial := errors.Is(relayErr, errSSEPartialAbort)
				thinkLoop := errors.Is(relayErr, errSSEThinkLoop)
				cut := errors.Is(relayErr, errSSEUpstreamCut)
				if canceled {
					s.reqStats.NoteEvent("client_abort")
					log.Printf("chat upstream_abort rid=%s account=%s reason=client_gone wait=%s", rid, acc.ID, time.Since(started).Round(time.Millisecond))
					recordStats(wantStream, false, 0, 0, 0)
					s.markAccountSessionBusy(acc.ID, 4*time.Second)
					s.endAccountInFlightLease(acc.ID, sessionLeaseTTL)
					// Same reasoning as the wired-hold branch: mid-flight
					// cancel is the client saying "this is going nowhere".
					// Break sticky so the retry doesn't re-bind the same
					// account and loop on the same bad state.
					if sessionID != "" && s.sessionStick != nil {
						s.sessionStick.Invalidate(sessionID)
					}
					s.forgetSessionResponse(sessionID)
					return
				}
				// Empty-stream cut can rotate. 透支号断流后再开一轮只会再限速扣额度。
				if cut && relayCtx.Err() == nil {
					lastErr = relayErr
					tried[acc.ID] = true
					s.recordAccountRotate(acc, "upstream stream cut (rotated)")
					s.markAccountSessionBusy(acc.ID, accountRotateBusy(acc))
					if stickyHit {
						s.sessionStick.Invalidate(sessionID)
					}
					endLease(acc.ID)
					if acc.Depleted() && !poolHasReady(valid, tried) {
						log.Printf("chat sse_cut_depleted_stop rid=%s account=%s committed=%v", rid, acc.ID, sseCommitted)
						recordStats(wantStream, false, 0, 0, 0)
						if sseCommitted {
							writeSSEStopDone(w, pin)
						}
						return
					}
					log.Printf("chat sse_cut_retry rid=%s account=%s depleted=%v committed=%v -> next", rid, acc.ID, acc.Depleted(), sseCommitted)
					continue
				}
				// Only NON-streaming think-stalls may retry: nothing has been written
				// to a client stream yet, so re-relaying on another account is clean.
				// A STREAMING think-stall (sseCommitted) must NOT retry — the thinking
				// block is already open on the committed client stream, and a second
				// attempt would open a second, dangling one (malformed protocol). It
				// falls through to a graceful in-place terminal below.
				if thinkLoop && relayCtx.Err() == nil && !sseCommitted && thinkLoops < maxThinkLoopRetries {
					thinkLoops++
					lastErr = relayErr
					s.markAccountDrain(acc.ID, time.Duration(thinkLoops)*thinkLoopRetryDrainStep)
					endLease(acc.ID)
					delete(tried, acc.ID)
					log.Printf("chat think_loop_retry rid=%s account=%s session=%s n=%d", rid, acc.ID, sessionID, thinkLoops)
					continue
				}
				if partial && relayCtx.Err() == nil && sameAccountPartials < 2 {
					sameAccountPartials++
					lastErr = relayErr
					delete(tried, acc.ID)
					s.markAccountDrain(acc.ID, 2*time.Second)
					endLease(acc.ID)
					log.Printf("chat sse_partial_retry rid=%s account=%s session=%s n=%d", rid, acc.ID, sessionID, sameAccountPartials)
					continue
				}
				recordStats(wantStream, false, 0, 0, 0)
				if idled || partial {
					s.markAccountSessionBusy(acc.ID, accountRotateBusy(acc))
					s.endAccountInFlightLease(acc.ID, sessionLeaseTTL)
				} else if thinkLoop {
					// Think-stall exhausted on this account (streaming never retries;
					// non-streaming burned maxThinkLoopRetries) — treat as a
					// cross-request signal, not just this request's problem. See
					// thinkLoopExhaustedDrain.
					s.markAccountDrain(acc.ID, thinkLoopExhaustedDrain)
					s.endAccountInFlightLease(acc.ID, sessionLeaseTTL)
				} else {
					s.endAccountInFlightLease(acc.ID, 0)
				}
				if sseCommitted {
					if thinkLoop {
						// Close the dangling thinking block + emit a visible notice so
						// the client gets a well-formed, non-empty turn instead of an
						// empty/malformed stop that triggers its auto-retry loop.
						writeSSEThinkLoopExhausted(w, pin)
					} else {
						// one-api / new-api: close with [DONE], never an error event.
						// Error events make OpenCode abort the agent on a normal tool miss.
						writeSSEStopDone(w, pin)
					}
				}
				return
			}
			// Always bind sticky on success — stream usage may be missing from SSE.
			promptTok, completionTok := extractUsageParts(cachedBody)
			// 华为 SSE 可能只回 total_tokens 或 prompt_tokens，缺 completion_tokens。
			// 有 total 但缺 completion → completion = total - prompt。
			// 全缺 → 用请求体估算拆分。
			if promptTok == 0 && completionTok == 0 && usage > 0 {
				promptTok, completionTok = estimateUsageSplit(body, usage)
				log.Printf("chat token_split_estimated rid=%s total=%.0f prompt=%.0f completion=%.0f", rid, usage, promptTok, completionTok)
			} else if completionTok == 0 && usage > 0 && promptTok > 0 {
				completionTok = usage - promptTok
				if completionTok < 0 {
					completionTok = 0
				}
				log.Printf("chat token_split_computed rid=%s total=%.0f prompt=%.0f completion=%.0f", rid, usage, promptTok, completionTok)
			}
			// 始终记录成功——即使 SSE 没有 usage 也要记耗时和成功次数。
			s.recordAccountSuccessFull(acc, usage, promptTok, completionTok, time.Since(started).Milliseconds())
			if m := peekModel(body); m != "" {
				acc.LastModel = m
			}
			if s.circuit != nil {
				s.circuit.RecordSuccess(acc.ID)
			}
			// Regurgitation guard: if the model just produced the SAME visible
			// output as the previous turn in this session, sticking on the same
			// account will loop — Claude Code sends the full history each turn,
			// including that offending output, and the upstream will replay it
			// verbatim. Break the loop BEFORE binding sticky, so the next turn
			// picks a fresh account and (with luck) the different KV-cache state
			// nudges the model off the regurgitation path. See sessionRespInfo.
			respFP := assembledResponseFingerprint(cachedBody)
			dupResp, dupN := s.noteAssistantResponse(sessionID, acc.ID, respFP)
			if dupResp {
				s.markAccountDrain(acc.ID, regurgitationDrain)
				if s.sessionStick != nil {
					s.sessionStick.Invalidate(sessionID)
				}
				s.resetAllSessionSuccessForAccount(acc.ID)
				log.Printf("chat regurgitation rid=%s session=%s account=%s dup=%d fp=%x — invalidated sticky + drained %s", rid, sessionID, acc.ID, dupN, respFP, regurgitationDrain)
			} else {
				// CLI-style: pin session→account even for long context (prefix reuse).
				s.bindSessionAccountLCP(sessionID, lcpKey, acc)
			}
			upCached := extractCachedTokens(cachedBody)
			s.noteAccountCache(acc, stickyHit, upCached)
			recordStats(wantStream, true, promptTok, completionTok, upCached)
			releaseLease(acc.ID, acc.Depleted())
			if !dupResp {
				s.rotateStickyAfterTurns(sessionID, acc)
			}
			s.rememberTurnThink(cachedBody)
			if s.respCache != nil && cacheKey != "" && len(cachedBody) > 0 {
				storeCachedChatResponse(s, cacheKey, cacheModel, http.StatusOK, cachedBody, usage)
			}
			reasonN, contentN, toolsN, finish := assembledStreamShape(cachedBody)
			// drain here must reflect the actual drain releaseLease applied,
			// which depends on whether the account was depleted. Reporting the
			// unconditional accountDrainAfterOK is misleading when the release
			// path used depletedDrainAfterOK.
			actualDrain := accountDrainAfterOK
			if acc.Depleted() {
				actualDrain = depletedDrainAfterOK
			}
			log.Printf("chat sse done rid=%s session=%s account=%s stream=%v took=%s usage=%.0f cached=%.0f sticky=%v heavy=%v drain=%s reasoning=%d content=%d tools=%d finish=%s tool=%s", rid, sessionID, acc.ID, wantStream, time.Since(started).Round(time.Millisecond), usage, upCached, stickyHit, heavy, actualDrain, reasonN, contentN, toolsN, finish, assembledToolInfo(cachedBody))
			return
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		closeHuaweiResp(resp)
		if len(respBody) >= 16<<20 {
			log.Printf("chat nonstream response at size cap rid=%s account=%s status=%d — may be truncated", rid, acc.ID, resp.StatusCode)
		}

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == http.StatusGatewayTimeout || resp.StatusCode >= 500 ||
			looksLikeBackendTimeout(resp.StatusCode, respBody) {
			lastStatus = resp.StatusCode
			endLease(acc.ID)
			if toolFollowup && looksLikeBackendTimeout(resp.StatusCode, respBody) {
				sameAccountTimeouts++
				delete(tried, acc.ID)
				s.markAccountDrain(acc.ID, 2*time.Second)
				log.Printf("chat tool_followup_504_hold_nonstream rid=%s session=%s account=%s n=%d", rid, sessionID, acc.ID, sameAccountTimeouts)
				continue
			}
			s.recordAccountFail(acc, "upstream timeout (rotated)")
			s.markAccountSessionBusy(acc.ID, accountRotateBusy(acc))
			if stickyHit {
				s.sessionStick.Invalidate(sessionID)
			}
			if looksLikeBackendTimeout(resp.StatusCode, respBody) {
				gatewayTimeouts++
				log.Printf("chat upstream_retry_nonstream rid=%s status=%d account=%s gateway_timeouts=%d", rid, resp.StatusCode, acc.ID, gatewayTimeouts)
			}
			continue
		}
		if looksLikeQuotaExhausted(resp.StatusCode, respBody) {
			lastStatus = resp.StatusCode
			s.softBanQuota(acc, "upstream_rejected", truncateStr(string(respBody), 300))
			if stickyHit {
				s.sessionStick.Invalidate(sessionID)
			}
			valid = filterAccountsByRealm(s.callableHuaweiAccountsFor(reqKey), cnRealm)
			endLease(acc.ID)
			continue
		}
		if looksLikeSessionCap(resp.StatusCode, respBody) {
			lastStatus = resp.StatusCode
			lastErr = fmt.Errorf("session cap")
			log.Printf("chat session_cap_nonstream rid=%s body=%s account=%s", rid, truncateStr(string(respBody), 180), acc.ID)
			endLease(acc.ID)
			s.rotateOffSessionCap(acc, sessionID)
			continue
		}
		if resp.StatusCode >= 400 && looksLikeSensitiveContentReject(respBody) {
			text := extractUpstreamAssistText(respBody)
			if text == "" {
				text = strings.TrimSpace(string(respBody))
			}
			log.Printf("chat upstream_safe_http_nonstream rid=%s status=%d account=%s bytes=%d", rid, resp.StatusCode, acc.ID, len(text))
			s.bindSessionAccountLCP(sessionID, lcpKey, acc)
			recordStats(false, true, 0, 0, 0)
			releaseLease(acc.ID, acc.Depleted())
			s.rotateStickyAfterTurns(sessionID, acc)
			out := map[string]any{
				"id":      "chatcmpl-upstream-safe",
				"object":  "chat.completion",
				"created": time.Now().Unix(),
				"model":   peekModel(body),
				"choices": []any{map[string]any{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": text,
					},
					"finish_reason": "stop",
				}},
			}
			b, _ := json.Marshal(out)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "BYPASS")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(encodeProtoBody(pin.Proto, b, pin))
			return
		}
		if resp.StatusCode >= 400 {
			lastStatus = resp.StatusCode
			s.recordAccountFail(acc, truncateStr(string(respBody), 300))
			if stickyHit {
				s.sessionStick.Invalidate(sessionID)
			}
			recordStats(false, false, 0, 0, 0)
			endLease(acc.ID)
			writeUpstreamClientErr(w, resp.StatusCode, respBody)
			return
		}
		if _, _, ok := parseHuaweiModelErr(respBody); ok {
			lastStatus = http.StatusBadRequest
			s.recordAccountRotate(acc, truncateStr(string(respBody), 160))
			recordStats(false, false, 0, 0, 0)
			endLease(acc.ID)
			writeUpstreamClientErr(w, http.StatusBadRequest, respBody)
			return
		}

		usage := extractUsageTokens(respBody)
		promptTok, completionTok := extractUsageParts(respBody)
		// 华为非流式也可能只有 total_tokens 或 prompt_tokens，缺 completion_tokens。
		if promptTok == 0 && completionTok == 0 && usage > 0 {
			promptTok, completionTok = estimateUsageSplit(body, usage)
		} else if completionTok == 0 && usage > 0 && promptTok > 0 {
			completionTok = usage - promptTok
			if completionTok < 0 {
				completionTok = 0
			}
		}
		s.recordAccountSuccessFull(acc, usage, promptTok, completionTok, time.Since(started).Milliseconds())
		if m := peekModel(body); m != "" {
			acc.LastModel = m
		}
		if s.circuit != nil {
			s.circuit.RecordSuccess(acc.ID)
		}
		s.bindSessionAccountLCP(sessionID, lcpKey, acc)
		upCached := extractCachedTokens(respBody)
		s.noteAccountCache(acc, stickyHit, upCached)
		recordStats(false, true, promptTok, completionTok, upCached)
		if sanitized, ok := sanitizeFilterFinish(respBody); ok {
			respBody = sanitized
			// Non-stream clients expect a terminal finish_reason.
			var obj map[string]any
			if json.Unmarshal(respBody, &obj) == nil {
				if choices, ok := obj["choices"].([]any); ok {
					for _, raw := range choices {
						if ch, _ := raw.(map[string]any); ch != nil {
							if ch["finish_reason"] == nil {
								ch["finish_reason"] = "stop"
							}
						}
					}
				}
				if b, err := json.Marshal(obj); err == nil {
					respBody = b
				}
			}
			log.Printf("chat content_filter_rewritten nonstream rid=%s account=%s", rid, acc.ID)
		}
		copyResponseHeaders(w, resp)
		w.Header().Set("X-Cache", "BYPASS")
		if heavy {
			w.Header().Set("X-Request-Heavy", "1")
		}
		// Store the OpenAI-form body for identical-body replay (non-stream, opt-in).
		if s.respCache != nil && cacheKey != "" && resp.StatusCode == http.StatusOK {
			storeCachedChatResponse(s, cacheKey, cacheModel, resp.StatusCode, respBody, usage)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(encodeProtoBody(pin.Proto, respBody, pin))
		releaseLease(acc.ID, acc.Depleted())
		s.rotateStickyAfterTurns(sessionID, acc)
		return
	}
pickExhausted:
	log.Printf("chat pick_exhausted rid=%s session=%s lastStatus=%d lastErr=%v took=%s", rid, sessionID, lastStatus, lastErr, time.Since(started).Round(time.Millisecond))
	recordStats(wantStream, false, 0, 0, 0)
	failMsg := "upstream temporarily unavailable, please retry"
	if errors.Is(lastErr, errLeaseWait) {
		failMsg = "all account slots busy, please retry shortly"
	}
	if errors.Is(lastErr, context.Canceled) {
		if stopTTFT != nil {
			stopTTFT()
		}
		return
	}
	if sseCommitted {
		if stopTTFT != nil {
			stopTTFT()
		}
		writeSSEStopDone(w, pin)
		return
	}
	writeErr(w, http.StatusBadGateway, failMsg)
}

func requestWantsStream(body []byte) bool {
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)
	return peek.Stream
}

func isEventStream(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.Contains(ct, "text/event-stream")
}

var hopByHopHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailers": true,
	"transfer-encoding": true, "upgrade": true,
	"content-length": true, "content-encoding": true,
}

func closeHuaweiResp(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	// Fully drain and close to prevent connection pool corruption.
	// Limiting to 256KB could leave unread data on pooled connections.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for k, vv := range resp.Header {
		if hopByHopHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	html := string(webui.IndexHTML())
	base := s.cfg.BasePath
	ver := webui.AssetVersion()
	inject := `<script>window.__BASE__=` + strconv.Quote(base) + `;window.__ASSET_V__=` + strconv.Quote(ver) + `;</script>`
	if strings.Contains(html, "<!--BASE-->") {
		html = strings.Replace(html, "<!--BASE-->", inject, 1)
	} else {
		html = strings.Replace(html, "<head>", "<head>\n"+inject, 1)
	}
	staticPrefix := "/static/"
	if base != "" {
		staticPrefix = base + "/static/"
		html = strings.ReplaceAll(html, `href="/static/`, `href="`+staticPrefix)
		html = strings.ReplaceAll(html, `src="/static/`, `src="`+staticPrefix)
	}
	// Bust browser/CDN cache on every binary embed change.
	html = strings.ReplaceAll(html, staticPrefix+`style.css"`, staticPrefix+`style.css?v=`+ver+`"`)
	html = strings.ReplaceAll(html, staticPrefix+`app.js"`, staticPrefix+`app.js?v=`+ver+`"`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	_, _ = w.Write([]byte(html))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeUpstreamClientErr rewrites Huawei InferHub/TM error payloads into OpenAI-compatible
// {"error":{"message":"...","type":"invalid_request_error","code":"..."}} so clients like
// OpenCode can parse them instead of failing schema validation.
func writeUpstreamClientErr(w http.ResponseWriter, status int, body []byte) {
	log.Printf("client_upstream_err status=%d body=%q", status, truncateStr(string(body), 240))
	// Session-cap must never reach the client as 400 / TM.* text.
	if looksLikeSessionCap(status, body) {
		writeErr(w, http.StatusServiceUnavailable, "upstream temporarily unavailable, please retry")
		return
	}
	msg := strings.TrimSpace(string(body))
	code := ""
	var hw struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
		Error     any    `json:"error"`
		Message   string `json:"message"`
	}
	if json.Unmarshal(body, &hw) == nil {
		if hw.ErrorMsg != "" {
			msg = hw.ErrorMsg
		} else if hw.Message != "" {
			msg = hw.Message
		}
		code = hw.ErrorCode
	}
	if strings.Contains(msg, "并发会话") || (strings.Contains(strings.ToLower(msg), "session") && strings.Contains(msg, "上限")) {
		writeErr(w, http.StatusServiceUnavailable, "upstream temporarily unavailable, please retry")
		return
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	if status <= 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func randID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// ---------- auth ----------

// loginRateLimit prevents brute-force attacks on the admin password.
// Tracks failed attempts per IP, blocks after 5 failures for 5 minutes.
var (
	loginMu       sync.Mutex
	loginAttempts = map[string][]time.Time{}
)

const (
	loginMaxFailures = 5
	loginBlockTime   = 5 * time.Minute
)

func loginBlocked(ip string) bool {
	loginMu.Lock()
	defer loginMu.Unlock()
	now := time.Now()
	attempts := loginAttempts[ip]
	cutoff := now.Add(-loginBlockTime)
	count := 0
	for _, t := range attempts {
		if t.After(cutoff) {
			count++
		}
	}
	return count >= loginMaxFailures
}

func recordLoginFailure(ip string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	now := time.Now()
	loginAttempts[ip] = append(loginAttempts[ip], now)
	// prune old
	cutoff := now.Add(-loginBlockTime)
	fresh := loginAttempts[ip][:0]
	for _, t := range loginAttempts[ip] {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	// Delete empty entries to prevent unbounded map growth (BUG-4 fix).
	if len(fresh) == 0 {
		delete(loginAttempts, ip)
	} else {
		loginAttempts[ip] = fresh
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := r.RemoteAddr
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		ip = strings.SplitN(forwarded, ",", 2)[0]
	}
	if loginBlocked(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many login attempts, try again later")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	token, err := s.auth.Login(req.Password)
	if err != nil {
		recordLoginFailure(ip)
		writeErr(w, http.StatusUnauthorized, "invalid password")
		return
	}
	s.clearAdminCookies(w)
	s.setAdminCookie(w, token)
	// Don't return token in body — prevents token leakage via proxy logs.
	writeJSON(w, http.StatusOK, map[string]string{"ok": "login"})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearAdminCookies(w)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "logout"})
}

func (s *Server) adminCookiePaths() []string {
	paths := []string{"/"}
	if s.cfg.BasePath != "" {
		paths = append(paths, s.cfg.BasePath, s.cfg.BasePath+"/")
	}
	return paths
}

func (s *Server) setAdminCookie(w http.ResponseWriter, token string) {
	path := "/"
	if s.cfg.BasePath != "" {
		path = s.cfg.BasePath
	}
	secure := strings.HasPrefix(strings.ToLower(s.cfg.PublicURL), "https://")
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    token,
		Path:     path,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode, // BUG-5 fix: Strict prevents CSRF
		MaxAge:   86400,
	})
}

func (s *Server) clearAdminCookies(w http.ResponseWriter) {
	secure := strings.HasPrefix(strings.ToLower(s.cfg.PublicURL), "https://")
	for _, path := range s.adminCookiePaths() {
		http.SetCookie(w, &http.Cookie{
			Name:     "admin_session",
			Value:    "",
			Path:     path,
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode, // BUG-5 fix: Strict prevents CSRF
			MaxAge:   -1,
		})
	}
}

// ---------- dashboard ----------

func (s *Server) dashboardKeyUsage() []map[string]any {
	if s == nil || s.apiKeys == nil {
		return []map[string]any{}
	}
	items := s.apiKeys.List()
	out := make([]map[string]any, 0, len(items))
	for _, k := range items {
		inflight := 0
		if s.keyGate != nil {
			inflight = s.keyGate.InFlight(k.ID)
		}
		out = append(out, map[string]any{
			"id":                k.ID,
			"name":              k.Name,
			"enabled":           k.Enabled,
			"in_flight":         inflight,
			"request_count":     k.RequestCount,
			"prompt_tokens":     k.PromptTokens,
			"completion_tokens": k.CompletionTokens,
		})
	}
	return out
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	accounts := s.db.SnapshotList() // deep copies — no race with chat goroutines (BUG-6 fix)
	now := time.Now()
	tokenCount := 0
	validToken := 0
	callable := 0
	exhausted := 0
	disabled := 0
	modelSet := map[string]struct{}{}
	var totalRemain, totalPackage, totalUsed float64
	quotaAccounts := 0
	periodCounts := map[string]int{}
	gateCounts := map[string]int{}
	var nextExpire string
	var nextExpireAt time.Time
	circuitSnap := map[string]int{}
	if s.circuit != nil {
		circuitSnap = s.circuit.Snapshot()
	}
	accountViews := make([]map[string]any, 0, len(accounts))
	// 双渠道分解：总览页要能一眼区分国际版 / 国内码道各自的池况。
	type chanStat struct {
		Accounts int `json:"accounts"`
		Callable int `json:"callable"`
		Disabled int `json:"disabled"`
		Ready    int `json:"ready"`
		Depleted int `json:"depleted"`
	}
	chanStats := map[string]*chanStat{"intl": {}, "cn": {}}
	statFor := func(a *store.Account) *chanStat {
		if store.NormalizeChannel(a.Channel) == store.ChannelCN {
			return chanStats["cn"]
		}
		return chanStats["intl"]
	}
	for _, a := range accounts {
		a.RecomputeCallGate(now)
		gateCounts[a.CallGate]++
		cs := statFor(a)
		cs.Accounts++
		if (a.CallGate == store.CallGateReady || a.CallGate == store.CallGateQuotaExhausted) && !a.Disabled {
			cs.Callable++
			callable++
		}
		if a.Disabled {
			cs.Disabled++
		}
		if a.CallGate == store.CallGateReady {
			cs.Ready++
		}
		if a.CallGate == store.CallGateQuotaExhausted {
			cs.Depleted++
			exhausted++
		}
		if a.Disabled || a.CallGate == store.CallGateDisabled {
			disabled++
		}
		if a.Credentials != nil && a.Credentials.AccessKeyID != "" {
			tokenCount++
		}
		if a.Credentials != nil && a.Credentials.Usable() {
			validToken++
		}
		for _, m := range a.Models {
			if m != "" {
				modelSet[m] = struct{}{}
			}
		}
		if a.Quota != nil && a.Quota.Error == "" {
			a.Quota.NormalizeRemain()
			quotaAccounts++
			totalRemain += a.Quota.RemainTokens
			totalPackage += a.Quota.PackageTokens
			totalUsed += a.Quota.UsedTokens
			if a.Quota.Period != "" {
				periodCounts[a.Quota.Period]++
			}
			if t := store.ParseFlexibleTime(a.Quota.ExpireTime); !t.IsZero() {
				if nextExpireAt.IsZero() || t.Before(nextExpireAt) {
					nextExpireAt = t
					nextExpire = a.Quota.ExpireTime
				}
			}
		}
		accountViews = append(accountViews, map[string]any{
			"id":                a.ID,
			"channel":           store.NormalizeChannel(a.Channel),
			"user_id":           a.UserID,
			"email":             a.Email,
			"status":            a.Status,
			"call_gate":         a.CallGate,
			"call_gate_label":   store.CallGateLabelZH(a.CallGate),
			"circuit":           circuitSnap[a.ID],
			"disabled":          a.Disabled,
			"models":            a.Models,
			"models_error":      a.ModelsError,
			"quota":             a.Quota,
			"last_used_at":      a.LastUsedAt,
			"last_error":        friendlyLastError(a.LastError),
			"prompt_tokens":     a.PromptTokens,
			"completion_tokens": a.CompletionTokens,
			"last_latency_ms":   a.LastLatencyMS,
			"last_model":        a.LastModel,
			"success_count":     a.SuccessCount,
			"fail_count":        a.FailCount,
		})
	}
	models := make([]string, 0, len(modelSet))
	for m := range modelSet {
		models = append(models, m)
	}
	sort.Strings(models)
	cacheSnap := respcache.Stats{}
	if s.respCache != nil {
		cacheSnap = s.respCache.Snapshot()
	}
	reqSnap := reqstats.Snapshot{}
	if s.reqStats != nil {
		reqSnap = s.reqStats.Snapshot()
	}
	var upstreamCached float64
	var stickHitsAcc int64
	for _, a := range accounts {
		if a == nil {
			continue
		}
		upstreamCached += a.CachedTokens
		stickHitsAcc += a.StickHits
	}
	stickSnap := sessionstick.Stats{}
	if s.sessionStick != nil {
		stickSnap = s.sessionStick.Snapshot()
	}
	// Pool health: explicit ready vs depleted vs disabled split.
	// "callable" above conflates ready+depleted; this separates them so the
	// dashboard can surface "all accounts in overdraft" at a glance.
	readyCount := gateCounts[store.CallGateReady]
	circuitOpen := 0
	circuitHalf := 0
	for _, st := range circuitSnap {
		if st == 1 { // cbOpen
			circuitOpen++
		} else if st == 2 { // cbHalfOpen
			circuitHalf++
		}
	}
	// Reliability: client disconnect rate, rotations, session-cap hits, first-byte p95.
	// These are the signals that diagnose "Connection lost mid-response" incidents.
	todayReq := reqSnap.Today.Requests
	if todayReq == 0 {
		todayReq = 1
	}
	totalReq := reqSnap.Total.Requests
	if totalReq == 0 {
		totalReq = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        len(accounts),
		"tokens":          tokenCount,
		"valid_tokens":    validToken,
		"callable":        callable,
		"quota_exhausted": exhausted,
		"disabled":        disabled,
		"models":          models,
		"model_count":     len(models),
		"quota_accounts":  quotaAccounts,
		"remain_tokens":   totalRemain,
		"used_tokens":     totalUsed,
		"package_tokens":  totalPackage,
		"period_counts":   periodCounts,
		"gate_counts":     gateCounts,
		"channel_stats":   chanStats,
		"next_expire":     nextExpire,
		"account_views":   accountViews,
		"cache":           cacheSnap,
		"session_stick":   stickSnap,
		"live": func() map[string]any {
			reqs, accts := s.totalInFlight()
			return map[string]any{
				"in_flight":       reqs,
				"active_accounts": accts,
				"stick_entries":   stickSnap.Entries,
				"resp_cache":      s.respCache != nil,
			}
		}(),
		"pool_health": map[string]any{
			"ready":          readyCount,
			"depleted":       gateCounts[store.CallGateQuotaExhausted],
			"disabled":       gateCounts[store.CallGateDisabled],
			"total":          len(accounts),
			"circuit_open":   circuitOpen,
			"circuit_half":   circuitHalf,
			"circuit_states": circuitSnap,
		},
		"reliability": map[string]any{
			"today_client_aborts":   reqSnap.Today.ClientAborts,
			"today_rotations":       reqSnap.Today.Rotations,
			"today_session_caps":    reqSnap.Today.SessionCapHits,
			"today_abort_rate":      float64(reqSnap.Today.ClientAborts) / float64(todayReq),
			"today_header_wait_p95": reqSnap.Today.HeaderWaitP95Approx,
			"total_client_aborts":   reqSnap.Total.ClientAborts,
			"total_rotations":       reqSnap.Total.Rotations,
			"total_session_caps":    reqSnap.Total.SessionCapHits,
			"total_abort_rate":      float64(reqSnap.Total.ClientAborts) / float64(totalReq),
			"total_header_wait_p95": reqSnap.Total.HeaderWaitP95Approx,
		},
		"circuits": circuitSnap,
		"scheduler": map[string]any{
			"rotate_by_gate":             s.cfg.RotateByGate,
			"rotate_after_ready":         s.rotateAfterFor(&store.Account{CallGate: store.CallGateReady}),
			"rotate_after_depleted":      s.rotateAfterFor(&store.Account{CallGate: store.CallGateQuotaExhausted}),
			"header_wait_depleted_ms":    s.depletedHeaderWait().Milliseconds(),
			"nonstream_timeout_depleted": s.depletedNonStreamTimeout().String(),
			"early_envelope":             s.cfg.EarlyEnvelope,
		},
		"key_limits": func() any {
			if s.keyGate != nil {
				return s.keyGate.Snapshot()
			}
			return []any{}
		}(),
		"key_usage": s.dashboardKeyUsage(),
		"prompt_cache": map[string]any{
			"upstream_supported":     false,
			"upstream_note":          "InferHub/CodeArts 无官方 prefix-cache 提示头；cached=0 是上游行为",
			"upstream_cached_tokens": upstreamCached,
			"today_upstream_cached":  reqSnap.Today.UpstreamCachedTokens,
			"stick_hit_rate":         stickSnap.HitRate,
			"stick_hits":             stickSnap.Hits,
			"stick_misses":           stickSnap.Misses,
			"stick_binds":            stickSnap.Binds,
			"stick_entries":          stickSnap.Entries,
			"account_stick_hits":     stickHitsAcc,
			"local_hit_rate":         cacheSnap.HitRate,
			"local_hits":             cacheSnap.Hits,
			"local_misses":           cacheSnap.Misses,
			"local_stores":           cacheSnap.Stores,
			"saved_tokens":           upstreamCached + cacheSnap.SavedTokens,
		},
		"request_stats": reqSnap,
		"time":          time.Now(),
	})
}

// ---------- accounts ----------

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	// Compute call gates from live accounts before redaction; clearing secrets first
	// would make every account appear no_cred in the dashboard.
	list := s.db.SnapshotList() // deep copies — safe to mutate display fields (BUG-6 fix)
	// 渠道过滤：面板「国际版账户池」只拉 intl；CN 走 /api/cn/dashboard。
	// 默认 all 保持向后兼容（API Key 选号等需要全池）。
	if want := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("channel"))); want == "intl" || want == "cn" {
		filtered := list[:0:0]
		for _, a := range list {
			if a != nil && store.NormalizeChannel(a.Channel) == want {
				filtered = append(filtered, a)
			}
		}
		list = filtered
	}
	now := time.Now()
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	stickCounts := map[string]int{}
	if s.sessionStick != nil {
		stickCounts = s.sessionStick.CountByAccount()
	}
	for _, a := range list {
		if a.Quota != nil {
			a.Quota.NormalizeRemain()
			// Stale snapshots may carry a zero reset time — drop it from the view.
			if a.Quota.NextResetAt != nil && a.Quota.NextResetAt.IsZero() {
				a.Quota.NextResetAt = nil
			}
		}
		a.RecomputeCallGate(now)
		// 空 channel 是历史存量（等价 intl）；归一化后前端不必做 null 兜底。
		a.Channel = store.NormalizeChannel(a.Channel)
		a.StickSessions = stickCounts[a.ID]
		if s.circuit != nil {
			a.Circuit = s.circuit.State(a.ID)
		}
		// Clean transient error labels in-memory only — don't write on GET.
		// Writing on every list read was an I/O bug (handleAccounts is GET /api/accounts).
		cleaned := friendlyLastError(a.LastError)
		if isTransientRotate(cleaned) || isTransientRotate(a.LastError) {
			a.LastError = ""
		} else if cleaned != a.LastError {
			a.LastError = cleaned
		}
		switch {
		case a.StickSessions > 0:
			a.CacheStatus = "sticky"
			a.CacheStatusLabel = "粘滞中"
		case a.StickHits > 0 || a.CachedTokens > 0:
			a.CacheStatus = "warm"
			a.CacheStatusLabel = "有缓存史"
		default:
			a.CacheStatus = "none"
			a.CacheStatusLabel = "无粘滞"
		}
	}
	writeJSON(w, http.StatusOK, redactAccounts(list))
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email        string             `json:"email"`
		Password     string             `json:"password"`
		CountryCode  string             `json:"country_code"`
		Status       string             `json:"status"`
		RefreshToken string             `json:"refresh_token"`
		UserToken    string             `json:"user_token"`
		Credentials  *store.Credentials `json:"credentials"`
		Metadata     map[string]string  `json:"metadata"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Email == "" {
		writeErr(w, http.StatusBadRequest, "email required")
		return
	}
	// Validate email format to prevent garbage/injection.
	if !strings.Contains(req.Email, "@") || len(req.Email) > 256 {
		writeErr(w, http.StatusBadRequest, "invalid email format")
		return
	}
	// Check for duplicate email.
	if existing := s.db.FindByEmail(req.Email); existing != nil {
		writeErr(w, http.StatusConflict, "account with this email already exists")
		return
	}
	cc := req.CountryCode
	if cc == "" {
		cc = s.cfg.RegisterAreaCode
	}
	status := req.Status
	if status == "" {
		status = "manual"
	}
	meta := req.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	if req.UserToken != "" {
		meta["user_token"] = req.UserToken
	}
	a := &store.Account{
		ID:           randID("acc"),
		Email:        req.Email,
		Password:     req.Password,
		CountryCode:  strings.ToUpper(cc),
		Status:       status,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		Credentials:  req.Credentials,
		RefreshToken: req.RefreshToken,
		Metadata:     meta,
	}
	if a.Channel == "" {
		a.Channel = store.ChannelIntl
	}
	if a.Credentials != nil && !a.Credentials.Expiration.IsZero() {
		a.TokenExpires = a.Credentials.Expiration
	}
	if err := s.db.Upsert(a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, redactAccount(a))
}

func (s *Server) handleImportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text         string `json:"text"`
		CountryCode  string `json:"country_code"`
		SkipNoAKSK   bool   `json:"skip_no_aksk"`
		RefreshQuota bool   `json:"refresh_quota"`
	}
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") || strings.Contains(ct, "+json") {
		if err := decodeJSONBody(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad json")
			return
		}
	} else {
		b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "read body failed")
			return
		}
		req.Text = string(b)
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, "text required (email----password----ak----sk)")
		return
	}
	cc := strings.ToUpper(strings.TrimSpace(req.CountryCode))
	if cc == "" {
		cc = s.cfg.RegisterAreaCode
	}

	type rowResult struct {
		Email  string `json:"email"`
		Action string `json:"action"` // created|updated|skipped|error
		ID     string `json:"id,omitempty"`
		Error  string `json:"error,omitempty"`
		Line   string `json:"line,omitempty"`
	}
	var (
		results            []rowResult
		created, updated   int
		skipped, parseFail int
		duplicates         int
	)
	// Dedup is always on: only brand-new accounts get created. Reject any line
	// whose email or AK already exists in the pool OR repeats earlier in this batch.
	existingEmails := map[string]bool{}
	existingAKs := map[string]bool{}
	for _, a := range s.db.List() {
		if a == nil {
			continue
		}
		if e := strings.ToLower(strings.TrimSpace(a.Email)); e != "" {
			existingEmails[e] = true
		}
		if a.Credentials != nil {
			if ak := strings.TrimSpace(a.Credentials.AccessKeyID); ak != "" {
				existingAKs[ak] = true
			}
		}
	}
	seenEmails := map[string]bool{}
	seenAKs := map[string]bool{}

	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		line, ok := store.ParseCredentialLine(raw)
		if !ok {
			parseFail++
			results = append(results, rowResult{Action: "error", Error: "unrecognized line", Line: raw})
			continue
		}
		if req.SkipNoAKSK && (line.AK == "" || line.SK == "") {
			skipped++
			results = append(results, rowResult{Email: line.Email, Action: "skipped", Error: "no ak/sk"})
			continue
		}
		emailKey := strings.ToLower(strings.TrimSpace(line.Email))
		akKey := strings.TrimSpace(line.AK)
		var dupReason string
		switch {
		case emailKey != "" && (existingEmails[emailKey] || seenEmails[emailKey]):
			dupReason = "duplicate email"
		case akKey != "" && (existingAKs[akKey] || seenAKs[akKey]):
			dupReason = "duplicate ak"
		}
		if dupReason != "" {
			duplicates++
			results = append(results, rowResult{Email: line.Email, Action: "duplicate", Error: dupReason})
			continue
		}
		if emailKey != "" {
			seenEmails[emailKey] = true
		}
		if akKey != "" {
			seenAKs[akKey] = true
		}

		existing := s.db.FindByEmail(line.Email)
		var a *store.Account
		action := "created"
		if existing != nil {
			a = existing
			action = "updated"
		} else {
			a = &store.Account{
				ID:          randID("acc"),
				Email:       line.Email,
				CountryCode: cc,
				CreatedAt:   time.Now(),
			}
		}
		if a.Channel == "" {
			a.Channel = store.ChannelIntl
		}
		if line.Password != "" {
			a.Password = line.Password
		}
		if a.CountryCode == "" {
			a.CountryCode = cc
		}
		if line.Opened != nil {
			a.Opened = *line.Opened
		}
		if line.AK != "" && line.SK != "" {
			a.Credentials = &store.Credentials{
				AccessKeyID:     line.AK,
				SecretAccessKey: line.SK,
				SecurityToken:   "",
				Expiration:      time.Time{},
				AuthMode:        "iam_permanent",
			}
			a.Status = "token_ok"
			a.Opened = true
			if a.Metadata == nil {
				a.Metadata = map[string]string{}
			}
			a.Metadata["auth_mode"] = "iam_permanent"
			a.Metadata["imported_at"] = time.Now().Format(time.RFC3339)
		} else if a.Status == "" {
			a.Status = "imported"
		}
		a.UpdatedAt = time.Now()
		if err := s.db.Upsert(a); err != nil {
			results = append(results, rowResult{Email: line.Email, Action: "error", Error: err.Error(), Line: raw})
			continue
		}
		if action == "created" {
			created++
		} else {
			updated++
		}
		results = append(results, rowResult{Email: a.Email, Action: action, ID: a.ID})

		if req.RefreshQuota && a.Credentials != nil && a.Credentials.Usable() {
			_, _ = s.refreshAccountQuota(r.Context(), a)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"created":    created,
		"updated":    updated,
		"skipped":    skipped,
		"duplicates": duplicates,
		"parse_fail": parseFail,
		"total":      created + updated + skipped + duplicates + parseFail,
		"results":    results,
	})
}

func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	var req struct {
		Email        string             `json:"email"`
		Password     string             `json:"password"`
		CountryCode  string             `json:"country_code"`
		Status       string             `json:"status"`
		RefreshToken string             `json:"refresh_token"`
		UserToken    string             `json:"user_token"`
		Credentials  *store.Credentials `json:"credentials"`
		Metadata     map[string]string  `json:"metadata"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Email != "" {
		a.Email = req.Email
	}
	if req.Password != "" {
		a.Password = req.Password
	}
	if req.CountryCode != "" {
		a.CountryCode = strings.ToUpper(req.CountryCode)
	}
	if req.Status != "" {
		a.Status = req.Status
	}
	if req.RefreshToken != "" {
		a.RefreshToken = req.RefreshToken
	}
	if req.Credentials != nil {
		a.Credentials = req.Credentials
		if !req.Credentials.Expiration.IsZero() {
			a.TokenExpires = req.Credentials.Expiration
		}
	}
	if req.Metadata != nil {
		if a.Metadata == nil {
			a.Metadata = map[string]string{}
		}
		for k, v := range req.Metadata {
			a.Metadata[k] = v
		}
	}
	if req.UserToken != "" {
		if a.Metadata == nil {
			a.Metadata = map[string]string{}
		}
		a.Metadata["user_token"] = req.UserToken
	}
	a.UpdatedAt = time.Now()
	if err := s.db.Upsert(a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, redactAccount(a))
}

// handlePatchAccountPriority sets Account.Priority. Lower = higher priority.
// Ties within a tier fall through to the existing load/stick/recency scorer.
func (s *Server) handlePatchAccountPriority(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	var req struct {
		Priority *int `json:"priority"`
	}
	if err := decodeJSONBody(r, &req); err != nil || req.Priority == nil {
		writeErr(w, http.StatusBadRequest, "bad json: expected {\"priority\": int}")
		return
	}
	// Guard against runaway values that would make sorting logic surprising.
	// Anything past ±1000 is almost certainly a client bug.
	if *req.Priority < -1000 || *req.Priority > 1000 {
		writeErr(w, http.StatusBadRequest, "priority must be in [-1000, 1000]")
		return
	}
	a.Priority = *req.Priority
	a.UpdatedAt = time.Now()
	if err := s.db.Upsert(a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account_id": a.ID, "priority": a.Priority})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.db.Delete(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "deleted"})
}

func (s *Server) handleAccountQuota(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	force := r.Method == http.MethodPost || r.URL.Query().Get("refresh") == "1"
	// 国内码道账号无国际 quota；返回 token 视图，避免面板"刷新额度"报错。
	if store.NormalizeChannel(a.Channel) == store.ChannelCN {
		writeJSON(w, http.StatusOK, map[string]any{
			"account_id":    a.ID,
			"email":         a.Email,
			"channel":       "cn",
			"token_expires": a.TokenExpires,
			"message":       "国内码道渠道不适用国际额度查询（额度走每日签到/积分）",
		})
		return
	}
	if !force && a.Quota != nil && a.Quota.Error == "" && time.Since(a.Quota.FetchedAt) < 5*time.Minute {
		writeJSON(w, http.StatusOK, map[string]any{
			"account_id": a.ID,
			"email":      a.Email,
			"quota":      a.Quota,
			"cached":     true,
		})
		return
	}
	sum, err := s.refreshAccountQuota(r.Context(), a)
	if err != nil {
		if a.Quota != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"account_id": a.ID,
				"email":      a.Email,
				"quota":      a.Quota,
				"cached":     true,
				"warning":    err.Error(),
			})
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": a.ID,
		"email":      a.Email,
		"quota":      sum,
		"cached":     false,
	})
}

func (s *Server) handleAccountModels(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	force := r.Method == http.MethodPost || r.URL.Query().Get("refresh") == "1"
	if !force && len(a.Models) > 0 && time.Since(a.ModelsFetchedAt) < 10*time.Minute {
		writeJSON(w, http.StatusOK, map[string]any{
			"account_id": a.ID,
			"email":      a.Email,
			"models":     a.Models,
			"cached":     true,
			"fetched_at": a.ModelsFetchedAt,
			"error":      a.ModelsError,
		})
		return
	}
	models, err := s.refreshAccountModels(r.Context(), a)
	if err != nil {
		if len(a.Models) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{
				"account_id": a.ID,
				"email":      a.Email,
				"models":     a.Models,
				"cached":     true,
				"fetched_at": a.ModelsFetchedAt,
				"warning":    err.Error(),
			})
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": a.ID,
		"email":      a.Email,
		"models":     models,
		"cached":     false,
		"fetched_at": a.ModelsFetchedAt,
	})
}

func (s *Server) handleAccountInsightRefresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	out := map[string]any{"account_id": a.ID, "email": a.Email}
	if q, err := s.refreshAccountQuota(r.Context(), a); err != nil {
		out["quota_error"] = err.Error()
		out["quota"] = a.Quota
	} else {
		out["quota"] = q
	}
	if models, err := s.refreshAccountModels(r.Context(), a); err != nil {
		out["models_error"] = err.Error()
		out["models"] = a.Models
	} else {
		out["models"] = models
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) accountCreds(a *store.Account) (*huawei.OAuthTokenResult, error) {
	if a == nil || a.Credentials == nil || a.Credentials.AccessKeyID == "" || a.Credentials.SecretAccessKey == "" {
		return nil, fmt.Errorf("oauth not completed for this account (no AK/SK)")
	}
	return &huawei.OAuthTokenResult{
		AccessKeyID:     a.Credentials.AccessKeyID,
		SecretAccessKey: a.Credentials.SecretAccessKey,
		SecurityToken:   a.Credentials.SecurityToken,
		Expiration:      a.Credentials.Expiration,
	}, nil
}

func (s *Server) refreshAccountQuota(ctx context.Context, a *store.Account) (*store.QuotaSnapshot, error) {
	creds, err := s.accountCreds(a)
	if err != nil {
		return nil, err
	}
	info, err := huawei.FetchSubscription(ctx, s.cfg.HuaweiModelBase, creds, s.httpClientForAccount(a))
	if err != nil {
		snap := a.Quota
		if snap == nil {
			snap = &store.QuotaSnapshot{}
		}
		snap.Error = err.Error()
		snap.FetchedAt = time.Now()
		a.Quota = snap
		_ = s.db.Upsert(a)
		return nil, err
	}
	sum := info.Summary()
	// Zero time means "upstream gave nothing" — keep it out of the stored/JSON form.
	var resetAt *time.Time
	if !sum.NextResetAt.IsZero() {
		t := sum.NextResetAt
		resetAt = &t
	}
	snap := &store.QuotaSnapshot{
		PackageStatus:  sum.PackageStatus,
		PackageName:    sum.PackageName,
		ExpireTime:     sum.ExpireTime,
		EffectiveTime:  sum.EffectiveTime,
		StartDate:      sum.StartDate,
		EndDate:        sum.EndDate,
		UsedTokens:     sum.UsedTokens,
		PackageTokens:  sum.PackageTokens,
		RemainTokens:   sum.RemainTokens,
		UsagePercent:   sum.UsagePercent,
		MetricName:     sum.MetricName,
		Period:         sum.Period,
		PeriodLabel:    sum.PeriodLabel,
		PeriodEvidence: sum.PeriodEvidence,
		NextResetAt:    resetAt,
		ResetSource:    sum.ResetSource,
		ResetNote:      sum.ResetNote,
		FetchedAt:      sum.FetchedAt,
	}
	snap.NormalizeRemain()
	carryResetObservation(a.Quota, snap, time.Now())
	a.Quota = snap
	a.Opened = sum.PackageStatus == "normal" || sum.PackageTokens > 0 || sum.RemainTokens > 0
	a.RecomputeCallGate(time.Now())
	if err := s.db.Upsert(a); err != nil {
		return snap, err
	}
	return snap, nil
}

func (s *Server) refreshAccountModels(ctx context.Context, a *store.Account) ([]string, error) {
	creds, err := s.accountCreds(a)
	if err != nil {
		return nil, err
	}
	// Force a fresh catalog for this account's credentials.
	client := huawei.NewHuaweiModelClient(s.cfg.HuaweiModelBase, s.cfg.HuaweiModelsURL)
	ctx = huawei.WithHTTPClient(ctx, s.httpClientForAccount(a))
	if err := client.RefreshModels(ctx, creds); err != nil {
		a.ModelsError = err.Error()
		a.ModelsFetchedAt = time.Now()
		_ = s.db.Upsert(a)
		return nil, err
	}
	models := client.ModelIDs()
	a.Models = models
	a.ModelsError = ""
	a.ModelsFetchedAt = time.Now()
	if err := s.db.Upsert(a); err != nil {
		return models, err
	}
	return models, nil
}

func (s *Server) handleSetIAMAkSk(w http.ResponseWriter, r *http.Request) {
	// 官方 CLI授权指导：IAM 永久 AK/SK → CODEARTS_CLI_AK / CODEARTS_CLI_SK
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	var req struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		AK              string `json:"ak"`
		SK              string `json:"sk"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	ak := strings.TrimSpace(req.AccessKeyID)
	if ak == "" {
		ak = strings.TrimSpace(req.AK)
	}
	sk := strings.TrimSpace(req.SecretAccessKey)
	if sk == "" {
		sk = strings.TrimSpace(req.SK)
	}
	if ak == "" || sk == "" {
		writeErr(w, http.StatusBadRequest, "access_key_id and secret_access_key required")
		return
	}
	a.Credentials = &store.Credentials{
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		SecurityToken:   "",
		Expiration:      time.Time{},
		AuthMode:        "iam_permanent",
	}
	if a.Metadata == nil {
		a.Metadata = map[string]string{}
	}
	a.Metadata["auth_mode"] = "iam_permanent"
	a.Metadata["cli_env"] = "CODEARTS_CLI_AK / CODEARTS_CLI_SK"
	a.Status = "token_ok"
	a.TokenExpires = time.Time{}
	a.Opened = true
	if err := s.db.Upsert(a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 立刻按官方永久 AK/SK 拉模型+额度
	out := map[string]any{"account": redactAccount(a), "auth_mode": "iam_permanent"}
	if q, err := s.refreshAccountQuota(r.Context(), a); err != nil {
		out["quota_error"] = err.Error()
	} else {
		out["quota"] = q
	}
	if models, err := s.refreshAccountModels(r.Context(), a); err != nil {
		out["models_error"] = err.Error()
	} else {
		out["models"] = models
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRefreshToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if a.Credentials != nil && a.Credentials.IsPermanent() {
		writeJSON(w, http.StatusOK, map[string]any{
			"account": redactAccount(a),
			"note":    "iam_permanent AK/SK (official CLI path); no OAuth refresh needed",
		})
		return
	}
	if a.RefreshToken == "" || a.Credentials == nil || a.Credentials.DpopKeyPair == nil {
		writeErr(w, http.StatusBadRequest, "no refresh token available")
		return
	}
	pair := &huawei.DpopKeyPair{
		PrivateKeyJwk: a.Credentials.DpopKeyPair.PrivateKeyJwk,
		PublicKeyJwk:  a.Credentials.DpopKeyPair.PublicKeyJwk,
	}
	res, err := huawei.RefreshOAuthToken(r.Context(), a.RefreshToken, stringMap(a.UserInfo, "domain_id"), pair)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.Credentials = &store.Credentials{
		AccessKeyID:     res.AccessKeyID,
		SecretAccessKey: res.SecretAccessKey,
		SecurityToken:   res.SecurityToken,
		Expiration:      res.Expiration,
		DpopKeyPair: &store.DpopKeyPair{
			PrivateKeyJwk: res.DpopKeyPair.PrivateKeyJwk,
			PublicKeyJwk:  res.DpopKeyPair.PublicKeyJwk,
		},
	}
	if res.RefreshToken != "" {
		a.RefreshToken = res.RefreshToken
	}
	a.TokenExpires = res.Expiration
	a.Status = "token_ok"
	if err := s.db.Upsert(a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, redactAccount(a))
}

func stringMap(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// ---------- oauth ----------

func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account_id")
	loginEmail := ""
	loginPassword := ""
	if accountID != "" {
		if acc, err := s.db.Get(accountID); err == nil && acc != nil {
			loginEmail = acc.Email
			loginPassword = acc.Password
		}
	}

	verifier, challenge, err := huawei.GeneratePKCE()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ticketID := randID("ticket")

	// Official CodeArts CLI: local callback on 127.0.0.1:<port>/oauth/callback.
	cb, err := huawei.StartLocalCallback(ticketID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "local callback: "+err.Error())
		return
	}

	// NOW it's safe to wipe credentials — all setup succeeded.
	// MarkOAuthPending is atomic under db.mu so chat goroutines skip this
	// account before we nil out credentials (BUG-3 fix, race-safe).
	if accountID != "" {
		s.db.MarkOAuthPending(accountID)
	}

	// Drop previous pending for this account and close their listeners.
	s.pendingOAuthMu.Lock()
	for k, p := range s.pendingOAuth {
		if accountID != "" && p.AccountID == accountID {
			if p.cb != nil {
				_ = p.cb.Close()
			}
			delete(s.pendingOAuth, k)
		}
	}
	pending := pendingOAuth{
		Verifier:    verifier,
		TicketID:    ticketID,
		AccountID:   accountID,
		CallbackURL: cb.URL(),
		LocalPort:   cb.PortStr(),
		LoginEmail:  loginEmail,
		LoginPass:   loginPassword,
		Status:      "pending",
		StartedAt:   time.Now(),
		cb:          cb,
	}
	s.pendingOAuth[ticketID] = pending
	s.pendingOAuthMu.Unlock()

	authURL := huawei.BuildAuthorizeURL(cb.PortStr(), ticketID, challenge)
	// Cline-style: open Edge InPrivate / Chrome Incognito so the host SSO cookie
	// of the operator's personal Huawei account is never reused.
	browserOpened := huawei.OpenAuthBrowser(authURL)
	go s.waitLocalOAuth(ticketID)

	writeJSON(w, http.StatusOK, map[string]any{
		"authorize_url":   authURL,
		"account_id":      accountID,
		"ticket_id":       ticketID,
		"login_email":     loginEmail,
		"login_line":      loginEmail + "----", // password never exposed in API response
		"fresh":           true,
		"browser_opened":  browserOpened,
		"browser_private": browserOpened,
		"callback_url":    cb.URL(),
		"local_port":      cb.PortStr(),
		"hint":            "1) 已尝试用无痕窗口打开授权页（与 Cline GitHub OAuth 同款，避免复用本机已登录账号）2) 在无痕窗口用目标华为云账号登录并确认授权 3) 若未自动弹出，手动打开 authorize_url 4) 回调: " + cb.URL(),
	})
}

func (s *Server) waitLocalOAuth(ticketID string) {
	s.pendingOAuthMu.Lock()
	pending, ok := s.pendingOAuth[ticketID]
	s.pendingOAuthMu.Unlock()
	if !ok || pending.cb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := pending.cb.Wait(ctx)
	_ = pending.cb.Close()

	// cleanupOAuthEntry removes the pending entry after a short grace period
	// so handleOAuthStatus can still read the final state. Entries that are
	// never polled (abandoned OAuth) are reaped by the background goroutine
	// in handleOAuthStart (BUG-5 fix).
	cleanupOAuthEntry := func() {
		go func() {
			time.Sleep(2 * time.Minute)
			s.pendingOAuthMu.Lock()
			delete(s.pendingOAuth, ticketID)
			s.pendingOAuthMu.Unlock()
		}()
	}

	s.pendingOAuthMu.Lock()
	cur, ok := s.pendingOAuth[ticketID]
	if !ok {
		s.pendingOAuthMu.Unlock()
		return
	}
	if err != nil {
		cur.Status = "failed"
		cur.Error = err.Error()
		s.pendingOAuth[ticketID] = cur
		s.pendingOAuthMu.Unlock()
		cleanupOAuthEntry()
		return
	}
	s.pendingOAuthMu.Unlock()

	if res == nil || subtle.ConstantTimeCompare([]byte(res.State), []byte(cur.TicketID)) != 1 {
		s.pendingOAuthMu.Lock()
		cur.Status = "failed"
		cur.Error = "oauth state mismatch"
		s.pendingOAuth[ticketID] = cur
		s.pendingOAuthMu.Unlock()
		cleanupOAuthEntry()
		return
	}

	var token *huawei.OAuthTokenResult
	if res.Code != "" {
		token, err = huawei.ExchangeCodeForToken(context.Background(), res.Code, cur.Verifier, cur.CallbackURL)
	} else {
		token, err = huawei.ExchangeSecretForToken(context.Background(), cur.TicketID, res.Secret)
	}
	if err != nil {
		s.pendingOAuthMu.Lock()
		cur.Status = "failed"
		cur.Error = err.Error()
		s.pendingOAuth[ticketID] = cur
		s.pendingOAuthMu.Unlock()
		cleanupOAuthEntry()
		return
	}
	if err := s.bindOAuthToken(cur, token); err != nil {
		s.pendingOAuthMu.Lock()
		cur.Status = "failed"
		cur.Error = err.Error()
		s.pendingOAuth[ticketID] = cur
		s.pendingOAuthMu.Unlock()
		cleanupOAuthEntry()
		return
	}
	s.pendingOAuthMu.Lock()
	cur.Status = "success"
	cur.Error = ""
	s.pendingOAuth[ticketID] = cur
	s.pendingOAuthMu.Unlock()
	cleanupOAuthEntry()
}

func (s *Server) bindOAuthToken(pending pendingOAuth, res *huawei.OAuthTokenResult) error {
	userInfo, _ := huawei.GetUserInfo(context.Background(), res)
	var dpopStore *store.DpopKeyPair
	if res.DpopKeyPair != nil {
		dpopStore = &store.DpopKeyPair{PrivateKeyJwk: res.DpopKeyPair.PrivateKeyJwk, PublicKeyJwk: res.DpopKeyPair.PublicKeyJwk}
	}
	creds := &store.Credentials{
		AccessKeyID:     res.AccessKeyID,
		SecretAccessKey: res.SecretAccessKey,
		SecurityToken:   res.SecurityToken,
		Expiration:      res.Expiration,
		DpopKeyPair:     dpopStore,
	}
	var acc *store.Account
	if pending.AccountID != "" {
		acc, _ = s.db.Get(pending.AccountID)
	}
	if acc == nil {
		acc = &store.Account{ID: randID("acc"), CountryCode: s.cfg.RegisterAreaCode, CreatedAt: time.Now()}
	}
	if acc.Channel == "" {
		acc.Channel = store.ChannelIntl
	}
	if acc.Email == "" {
		acc.Email = pending.LoginEmail
	}
	if acc.Email == "" {
		acc.Email = stringMap(userInfo, "user_name")
	}
	if acc.Email == "" {
		acc.Email = "oauth-user"
	}
	if acc.Password == "" && pending.LoginPass != "" {
		acc.Password = pending.LoginPass
	}
	acc.Status = "token_ok"
	acc.UpdatedAt = time.Now()
	acc.Credentials = creds
	acc.RefreshToken = res.RefreshToken
	acc.TokenExpires = res.Expiration
	acc.UserInfo = userInfo
	if acc.Metadata == nil {
		acc.Metadata = map[string]string{}
	}
	acc.Metadata["oauth_fresh_at"] = time.Now().Format(time.RFC3339)
	acc.Metadata["oauth_callback"] = pending.CallbackURL
	return s.db.Upsert(acc)
}

func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	ticketID := r.URL.Query().Get("ticket_id")
	if ticketID == "" {
		writeErr(w, http.StatusBadRequest, "ticket_id required")
		return
	}
	s.pendingOAuthMu.Lock()
	pending, ok := s.pendingOAuth[ticketID]
	s.pendingOAuthMu.Unlock()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "unknown", "ticket_id": ticketID})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      pending.Status,
		"error":       pending.Error,
		"ticket_id":   pending.TicketID,
		"account_id":  pending.AccountID,
		"login_email": pending.LoginEmail,
		"login_line":  pending.LoginEmail + "----", // password never exposed in status API
		"started_at":  pending.StartedAt,
	})
}

// handleOAuthCallback keeps a no-op for old PUBLIC_URL redirects; real flow uses local callback.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	// BUG-14: This is a fallback HTML page only. The real OAuth callback happens
	// on 127.0.0.1 via huawei.StartLocalCallback(). The state parameter is the
	// ticketID embedded in the authorize URL, verified against s.pendingOAuth[ticketID]
	// in waitLocalOAuth(). CSRF is inherently prevented because:
	// 1) The callback is on localhost, not the server
	// 2) The ticketID is random and only known to the admin session that started OAuth
	// 3) The PKCE verifier prevents code injection
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><html><body style="font-family:system-ui;padding:40px">
<h2>Please use local callback to complete authorization</h2>
<p>CodeArts OAuth must callback to 127.0.0.1. Please re-click "New OAuth" from the panel.</p>
</body></html>`)
}
