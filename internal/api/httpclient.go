package api

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"go-codearts/internal/store"
)

const (
	// headerWaitDefault caps first-byte wait for ready accounts.
	// Was 120s — too long; Claude Code client gives up at ~60s. 60s lets the
	// gateway rotate to another account before the client disconnects.
	headerWaitDefault = 60 * time.Second
	// headerWaitHeavy was 5m — same client give-up (~60s to first stall
	// warning, connection actually dropped a few minutes later) applies
	// regardless of body size, so 5m just let the gateway sit blocked on one
	// slow/stuck account for the client's entire patience window instead of
	// rotating. 90s keeps some extra prefill slack for large contexts over
	// headerWaitDefault while still leaving room for several account
	// rotations inside the client's real-world patience budget.
	headerWaitHeavy = 90 * time.Second
	// headerWaitDepleted: 透支号被华为限速，60s+ 首字节会先被 Claude Code 掐断。
	// 更短的 header timeout 让网关在客户端放弃前换号。
	headerWaitDepleted = 40 * time.Second
	// nonStreamTimeout caps total time for non-streaming requests.
	// Streaming requests have no overall timeout (SSE can run long).
	nonStreamTimeout         = 5 * time.Minute
	nonStreamTimeoutDepleted = 45 * time.Second
)

// defaultHTTPClient is used for chat/upstream — no overall Timeout so SSE can run long.
// First-byte wait is capped by ResponseHeaderTimeout on the transport.
// Connection pool tuned for Huawei TM.00001041: small idle pool, short timeout.
func defaultHTTPClient() *http.Client {
	tr := newBaseTransport(headerWaitDefault)
	return &http.Client{Transport: tr}
}

// newBaseTransport creates a fresh transport with the given header timeout.
// Shared by all chat clients so connection pool settings are consistent.
func newBaseTransport(headerTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			// 5s (was 10s): a cold TCP dial to InferHub normally completes in
			// <200ms; a stall means the link is already bad, so fail fast and
			// rotate to another account instead of making the client wait 10s.
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// HTTP/2 disabled: Huawei APIG multiplexes streams on a single H2
		// connection, preventing clean teardown of individual requests. This
		// causes zombie sessions occupying account slots. With H1.1, each
		// request has its own TCP connection that closes on completion,
		// enabling clean account rotation. Pattern from workbuddy2api/sub2api.
		ForceAttemptHTTP2: false,
		TLSClientConfig:   &tls.Config{NextProtos: []string{"http/1.1"}},
		TLSNextProto:      make(map[string]func(string, *tls.Conn) http.RoundTripper),
		// MaxIdleConnsPerHost 20→200, MaxIdleConns 100→256: chatClientKey caches
		// one shared transport per proxy (almost every account has no ProxyID,
		// so nearly all chat traffic funnels through the single "direct" entry
		// into ONE Huawei host). MaxConnsPerHost=0 already lets 100+ concurrent
		// requests dial in parallel, but only 20 could stay in the idle pool
		// afterward — the rest were closed and paid a fresh cross-border TCP+TLS
		// handshake on the next burst. A single conversation can now fan out
		// 100+ concurrent tool-call requests, so size the idle pool for that.
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 200,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,
		// 5s (was 10s): normal TLS handshake to InferHub is <500ms even
		// cross-border; a 10s stall only wastes the client's time before we
		// rotate. Fail fast so a bad cold connection rotates to a fresh account.
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		WriteBufferSize:       64 << 10,
		ReadBufferSize:        64 << 10,
	}
}

// chatClientCache holds pre-built HTTP clients for each (account, heavy) combo.
// This avoids cloning the transport on every request, which defeated connection
// pooling (BUG-7 fix). Each client has its own persistent connection pool so
// TCP/TLS handshakes are reused across requests — major latency win under load.
type chatClientCache struct {
	mu sync.Mutex
	// key = "direct" or proxyID; value has normal/heavy variants.
	entries map[string]*chatClientEntry
}

type chatClientEntry struct {
	// Stream clients (no overall timeout — SSE can run long).
	normal   *http.Client // headerWaitDefault
	heavy    *http.Client // headerWaitHeavy
	depleted *http.Client // headerWaitDepleted
	// Non-stream clients (overall timeout caps total request time).
	normalNonStream   *http.Client
	heavyNonStream    *http.Client
	depletedNonStream *http.Client
	lastWarm          time.Time
}

func newChatClientCache() *chatClientCache {
	return &chatClientCache{entries: map[string]*chatClientEntry{}}
}

func (cc *chatClientCache) get(key string, build func() *chatClientEntry) *chatClientEntry {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	e, ok := cc.entries[key]
	if !ok {
		e = build()
		cc.entries[key] = e
	}
	return e
}

func (cc *chatClientCache) invalidate(key string) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	delete(cc.entries, key)
}

func (cc *chatClientCache) recentlyWarm(key string, d time.Duration) bool {
	if cc == nil {
		return false
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	e := cc.entries[key]
	return e != nil && !e.lastWarm.IsZero() && time.Since(e.lastWarm) < d
}

func (cc *chatClientCache) markWarm(key string) {
	if cc == nil {
		return
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if e := cc.entries[key]; e != nil {
		e.lastWarm = time.Now()
	}
}

func buildEntryWithWait(waitNorm, waitHeavy, waitDep, nsNorm, nsDep time.Duration, base *http.Transport) *chatClientEntry {
	mk := func(d time.Duration) *http.Transport {
		if base != nil {
			tr := base.Clone()
			tr.ResponseHeaderTimeout = d
			return tr
		}
		return newBaseTransport(d)
	}
	trNormal := mk(waitNorm)
	trHeavy := mk(waitHeavy)
	trDep := mk(waitDep)
	return &chatClientEntry{
		normal:            &http.Client{Transport: trNormal},
		heavy:             &http.Client{Transport: trHeavy},
		depleted:          &http.Client{Transport: trDep},
		normalNonStream:   &http.Client{Transport: trNormal, Timeout: nsNorm},
		heavyNonStream:    &http.Client{Transport: trHeavy, Timeout: 5 * time.Minute},
		depletedNonStream: &http.Client{Transport: trDep, Timeout: nsDep},
	}
}

// buildProxyEntry creates chat clients that reuse a proxy's transport.
// We clone the proxy transport twice (normal/heavy header timeout) but only
// once per proxy — not per request.
func buildProxyEntry(proxyClient *http.Client, waitDep, nsDep time.Duration) *chatClientEntry {
	if waitDep <= 0 {
		waitDep = headerWaitDepleted
	}
	if nsDep <= 0 {
		nsDep = nonStreamTimeoutDepleted
	}
	baseTr, ok := proxyClient.Transport.(*http.Transport)
	if !ok || baseTr == nil {
		return buildEntryWithWait(headerWaitDefault, headerWaitHeavy, waitDep, nonStreamTimeout, nsDep, nil)
	}
	return buildEntryWithWait(headerWaitDefault, headerWaitHeavy, waitDep, nonStreamTimeout, nsDep, baseTr)
}

func (s *Server) sharedHTTPClient() *http.Client {
	if s == nil {
		return defaultHTTPClient()
	}
	s.httpDefaultMu.Lock()
	defer s.httpDefaultMu.Unlock()
	if s.httpDefault == nil {
		s.httpDefault = defaultHTTPClient()
	}
	return s.httpDefault
}

// httpClientForAccount returns the base proxy client for an account (if bound).
func (s *Server) httpClientForAccount(a *store.Account) *http.Client {
	if s == nil {
		return defaultHTTPClient()
	}
	if a != nil && s.proxies != nil {
		if id := strings.TrimSpace(a.ProxyID); id != "" {
			c, _, err := s.proxies.ClientFor(id)
			if err == nil && c != nil {
				return c
			}
		}
	}
	return s.sharedHTTPClient()
}

// chatClientKey returns the cache key for an account's chat clients.
func chatClientKey(a *store.Account) string {
	if a != nil {
		if id := strings.TrimSpace(a.ProxyID); id != "" {
			return "proxy:" + id
		}
	}
	return "direct"
}

// httpClientForChat returns a cached chat client with the right header timeout.
// Connection pool is reused across requests — no per-request transport clone
// (BUG-7 fix). This restores CLI-style keep-alive connection reuse.
func (s *Server) depletedHeaderWait() time.Duration {
	if s != nil && s.cfg.HeaderWaitDepleted > 0 {
		return s.cfg.HeaderWaitDepleted
	}
	return headerWaitDepleted
}

func (s *Server) depletedNonStreamTimeout() time.Duration {
	if s != nil && s.cfg.NonStreamTimeoutDepleted > 0 {
		return s.cfg.NonStreamTimeoutDepleted
	}
	return nonStreamTimeoutDepleted
}

func (s *Server) chatEntryFor(a *store.Account) *chatClientEntry {
	key := chatClientKey(a)
	waitDep := s.depletedHeaderWait()
	nsDep := s.depletedNonStreamTimeout()
	return s.chatClients.get(key, func() *chatClientEntry {
		if key == "direct" {
			return buildEntryWithWait(headerWaitDefault, headerWaitHeavy, waitDep, nonStreamTimeout, nsDep, nil)
		}
		return buildProxyEntry(s.httpClientForAccount(a), waitDep, nsDep)
	})
}

func (s *Server) httpClientForChat(a *store.Account, heavy bool) *http.Client {
	if s == nil {
		return defaultHTTPClient()
	}
	if a != nil && a.Depleted() && s.chatClients == nil {
		return withHeaderTimeout(s.httpClientForAccount(a), s.depletedHeaderWait())
	}
	if s.chatClients == nil {
		return withHeaderTimeout(s.httpClientForAccount(a), ternary(heavy, headerWaitHeavy, headerWaitDefault))
	}
	entry := s.chatEntryFor(a)
	if a != nil && a.Depleted() && entry.depleted != nil {
		return entry.depleted
	}
	if heavy {
		return entry.heavy
	}
	return entry.normal
}

// ensureChatClientWarm puts TLS for this account's transport into the idle pool.
// Cheap GET to HuaweiModelBase; no-op if the base URL is empty or recently warmed.
func (s *Server) ensureChatClientWarm(a *store.Account) {
	if s == nil || a == nil {
		return
	}
	c := s.httpClientForChat(a, false)
	base := ""
	if s.cfg.HuaweiModelBase != "" {
		base = strings.TrimSpace(s.cfg.HuaweiModelBase)
	}
	if c == nil || base == "" {
		return
	}
	key := chatClientKey(a)
	if s.chatClients != nil && s.chatClients.recentlyWarm(key, 45*time.Second) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, strings.TrimRight(base, "/")+"/", nil)
	if err != nil {
		return
	}
	resp, err := c.Do(req)
	if err != nil {
		req2, err2 := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/", nil)
		if err2 != nil {
			return
		}
		resp, err = c.Do(req2)
	}
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		resp.Body.Close()
	}
	if err == nil && s.chatClients != nil {
		s.chatClients.markWarm(key)
	}
}

// httpClientForChatNonStream returns a cached client with an overall timeout.
func (s *Server) httpClientForChatNonStream(a *store.Account, heavy bool) *http.Client {
	if s == nil {
		c := defaultHTTPClient()
		out := *c
		out.Timeout = nonStreamTimeout
		return &out
	}
	if s.chatClients == nil {
		c := s.httpClientForChat(a, heavy)
		out := *c
		if a != nil && a.Depleted() {
			out.Timeout = s.depletedNonStreamTimeout()
		} else {
			out.Timeout = ternary(heavy, 5*time.Minute, nonStreamTimeout)
		}
		return &out
	}
	entry := s.chatEntryFor(a)
	if a != nil && a.Depleted() {
		return entry.depletedNonStream
	}
	if heavy {
		return entry.heavyNonStream
	}
	return entry.normalNonStream
}

// withHeaderTimeout is retained for fallback when chatClients cache is nil.
func withHeaderTimeout(c *http.Client, d time.Duration) *http.Client {
	if c == nil {
		c = defaultHTTPClient()
	}
	if d <= 0 {
		d = headerWaitDefault
	}
	out := *c
	if tr, ok := c.Transport.(*http.Transport); ok && tr != nil {
		cl := tr.Clone()
		cl.ResponseHeaderTimeout = d
		out.Transport = cl
	}
	return &out
}

func ternary(b bool, a, c time.Duration) time.Duration {
	if b {
		return a
	}
	return c
}

// chatSemCap computes the concurrent chat request semaphore capacity from the
// account pool size. 24 concurrent requests per account (3 SSE slots × 8 retry
// headroom), clamped to [32, 384]. More accounts → more parallelism.
// Was n*16 — too conservative under multi-user load; new-api/sub2api allow
// per-channel semaphores that sum higher.
func chatSemCap(db *store.DB) int {
	if db == nil {
		return 32
	}
	n := len(db.List())
	cap := n * 24
	if cap < 32 {
		cap = 32
	}
	// Ceiling raised 384->1024 (2026-09-20): 384 was calibrated when the
	// pool was much smaller and predates the per-key KEY_MAX_INFLIGHT gate
	// (see .env) whose "share" (chatSemCap/tenant count) is capped by this
	// value too. At 358 accounts, n*24 is already >8000, so this ceiling
	// is the sole binding constraint; 384 forced every key's share to
	// well under 100, defeating a single conversation's 100+-concurrent
	// tool-call fan-out even after raising KEY_MAX_INFLIGHT itself. 1024
	// keeps a real outer bound (well below the theoretical n*24) while
	// giving that headroom back.
	if cap > 1024 {
		cap = 1024
	}
	return cap
}
