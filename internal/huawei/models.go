package huawei

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go-codearts/internal/kit"
)

// HuaweiModelClient calls the Huawei Cloud CodeArts internal model endpoint.
// Model IDs are not hardcoded; they are fetched from HUAWEI_MODELS_URL or from
// the upstream host candidates when possible.
type HuaweiModelClient struct {
	BaseURL   string
	ModelsURL string
	HTTP      *http.Client

	mu        sync.Mutex
	modelIDs  []string
	lastFetch time.Time
}

func NewHuaweiModelClient(baseURL, modelsURL string) *HuaweiModelClient {
	if baseURL == "" {
		baseURL = "https://snap-access.ap-southeast-1.myhuaweicloud.com"
	}
	return &HuaweiModelClient{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		ModelsURL: strings.TrimSpace(modelsURL),
		// No overall Timeout: streaming chat must not die at 30s.
		// Response headers still bounded by transport.ResponseHeaderTimeout.
		HTTP: newKeepAliveClient(),
	}
}

func newKeepAliveClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Override the inherited 30s default dial timeout: a cold dial to InferHub
	// is <200ms; fail fast (5s) so a bad link rotates instead of stalling.
	tr.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.MaxIdleConns = 9
	tr.MaxIdleConnsPerHost = 3
	tr.IdleConnTimeout = 15 * time.Second
	tr.TLSHandshakeTimeout = 5 * time.Second // was 10s — see api/httpclient.go
	tr.ResponseHeaderTimeout = 55 * time.Second
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(authority string, c *tls.Conn) http.RoundTripper{}
	tlsCfg := tr.TLSClientConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	} else {
		tlsCfg = tlsCfg.Clone()
	}
	tlsCfg.NextProtos = []string{"http/1.1"}
	tr.TLSClientConfig = tlsCfg
	return &http.Client{Transport: tr}
}

type httpClientCtxKey struct{}

// WithHTTPClient attaches an outbound *http.Client to ctx (proxy pool).
func WithHTTPClient(ctx context.Context, client *http.Client) context.Context {
	if client == nil {
		return ctx
	}
	return context.WithValue(ctx, httpClientCtxKey{}, client)
}

func ClientFromContext(ctx context.Context) *http.Client {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(httpClientCtxKey{}).(*http.Client); ok && v != nil {
		return v
	}
	return nil
}

func (c *HuaweiModelClient) clientFrom(ctx context.Context) *http.Client {
	if cl := ClientFromContext(ctx); cl != nil {
		return cl
	}
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return newKeepAliveClient()
}

// WarmLoop keeps a TLS connection warm to the InferHub host so the first
// chat request after idle does not pay a full dial+handshake tax.
func (c *HuaweiModelClient) WarmLoop(ctx context.Context) {
	if c == nil || c.BaseURL == "" {
		return
	}
	tick := time.NewTicker(8 * time.Second) // was 12s — keep TLS warmer under burst
	defer tick.Stop()
	warm := func() {
		client := c.HTTP
		if client == nil {
			client = newKeepAliveClient()
		}
		u := strings.TrimRight(c.BaseURL, "/") + "/"
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
		if err != nil {
			return
		}
		t0 := time.Now()
		resp, err := client.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
		}
		// 404/405 still prove the TCP/TLS session is hot.
		log.Printf("upstream warm base=%s err=%v status=%v took=%s", c.BaseURL, err, statusOf(resp), time.Since(t0).Round(time.Millisecond))
	}
	warm()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			warm()
		}
	}
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// FallbackModelIDs are CodeArts built-ins used when agent-center catalog
// cannot be fetched (no OAuth AK/SK yet, or agent not provisioned).
// Official docs: GLM-5.1 / GLM-5.2; CLI also references Qwen3-VL family.
var FallbackModelIDs = []string{
	"GLM-5.1",
	"GLM-5.2",
	"DeepSeek-V3",
	"DeepSeek-R1",
	"Qwen2.5-72B-Instruct",
	"Qwen3-VL-235B",
	"Qwen3.5-397B-A17B-VL",
	"Qwen3.6-27B-VL",
}

// RefreshModels loads the real catalog the same way CodeArts CLI does:
// AK/SK-signed GET agent-center agents → detail → gpts.models[].model_id.
// Plain GET /v1/models on snap-access is NOT published (APIG.0101) — do not rely on it.
func (c *HuaweiModelClient) RefreshModels(ctx context.Context, creds *OAuthTokenResult) error {
	c.mu.Lock()
	if time.Since(c.lastFetch) < time.Minute && len(c.modelIDs) > 0 {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	var lastErr error

	if c.ModelsURL != "" {
		ids, err := c.fetchOpenAIModels(ctx, c.ModelsURL, creds)
		if err == nil && len(ids) > 0 {
			c.setModels(ids)
			return nil
		}
		if err != nil {
			lastErr = err
		}
	}

	if creds != nil && creds.AccessKeyID != "" && creds.SecretAccessKey != "" {
		ids, err := c.fetchAgentCenterModels(ctx, creds)
		if err == nil && len(ids) > 0 {
			c.setModels(ids)
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = &UpstreamError{Status: 200, Body: "agent-center returned no models"}
		}
	} else if lastErr == nil {
		lastErr = &UpstreamError{Status: 401, Body: "no OAuth AK/SK on any account; agent-center catalog requires CodeArts OAuth first"}
	}

	return lastErr
}

func (c *HuaweiModelClient) setModels(ids []string) {
	c.mu.Lock()
	c.modelIDs = ids
	c.lastFetch = time.Now()
	c.mu.Unlock()
}

func (c *HuaweiModelClient) signedGET(ctx context.Context, rawURL string, creds *OAuthTokenResult, extra map[string]string) ([]byte, int, error) {
	headers := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
		"Agent-Type":   "AgentCenter",
		"X-Language":   "zh-cn",
	}
	for k, v := range extra {
		headers[k] = v
	}
	if creds != nil && creds.AccessKeyID != "" && creds.SecretAccessKey != "" {
		signed, _, err := AKSKSign(creds.AccessKeyID, creds.SecretAccessKey, creds.SecurityToken, http.MethodGet, rawURL, nil, headers)
		if err != nil {
			return nil, 0, err
		}
		headers = signed
	} else if creds != nil && creds.SecurityToken != "" {
		headers["X-Security-Token"] = creds.SecurityToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.clientFrom(ctx).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	return body, resp.StatusCode, nil
}

func (c *HuaweiModelClient) fetchOpenAIModels(ctx context.Context, u string, creds *OAuthTokenResult) ([]string, error) {
	body, status, err := c.signedGET(ctx, u, creds, map[string]string{
		"x-client-type": "kernel",
		"app-id":        "CodeAgent3.0",
	})
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, &UpstreamError{Status: status, Body: kit.Snippet(string(body), 300)}
	}
	ids := parseModelIDs(body)
	if len(ids) == 0 {
		return nil, &UpstreamError{Status: status, Body: "no model ids in response"}
	}
	return ids, nil
}

func (c *HuaweiModelClient) fetchAgentCenterModels(ctx context.Context, creds *OAuthTokenResult) ([]string, error) {
	agentsURL := c.BaseURL + "/v1/agent-center/agents/useragents?offset=0&limit=100"
	body, status, err := c.signedGET(ctx, agentsURL, creds, nil)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, &UpstreamError{Status: status, Body: kit.Snippet(string(body), 300)}
	}
	agentID := parseDefaultAgentID(body, "CodeAgent")
	if agentID == "" {
		return nil, &UpstreamError{Status: status, Body: "no CodeAgent in agent-center useragents"}
	}
	detailURL := c.BaseURL + "/v1/agent-center/agents/detail?agent_id=" + agentID
	body, status, err = c.signedGET(ctx, detailURL, creds, nil)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, &UpstreamError{Status: status, Body: kit.Snippet(string(body), 300)}
	}
	ids := parseAgentCenterModelIDs(body)
	if len(ids) == 0 {
		return nil, &UpstreamError{Status: status, Body: "agent detail has no gpts.models"}
	}
	return ids, nil
}

func parseDefaultAgentID(data []byte, wantName string) string {
	var wrap struct {
		Agents []struct {
			AgentID   string `json:"agent_id"`
			AgentName string `json:"agent_name"`
		} `json:"agents"`
		Data struct {
			Agents []struct {
				AgentID   string `json:"agent_id"`
				AgentName string `json:"agent_name"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return ""
	}
	agents := wrap.Agents
	if len(agents) == 0 {
		agents = wrap.Data.Agents
	}
	for _, a := range agents {
		if a.AgentName == wantName && a.AgentID != "" {
			return a.AgentID
		}
	}
	if len(agents) > 0 {
		return agents[0].AgentID
	}
	return ""
}

func parseAgentCenterModelIDs(data []byte) []string {
	var wrap struct {
		Gpts struct {
			Models []struct {
				ModelName       string `json:"model_name"`
				ModelParameters struct {
					ModelID string `json:"model_id"`
				} `json:"model_parameters"`
			} `json:"models"`
		} `json:"gpts"`
		Data struct {
			Gpts struct {
				Models []struct {
					ModelName       string `json:"model_name"`
					ModelParameters struct {
						ModelID string `json:"model_id"`
					} `json:"model_parameters"`
				} `json:"models"`
			} `json:"gpts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil
	}
	models := wrap.Gpts.Models
	if len(models) == 0 {
		models = wrap.Data.Gpts.Models
	}
	seen := map[string]struct{}{}
	ids := []string{}
	for _, m := range models {
		id := m.ModelParameters.ModelID
		if id == "" {
			id = m.ModelName
		}
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func parseModelIDs(data []byte) []string {
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models map[string]any `json:"models"`
	}
	if err := json.Unmarshal(data, &list); err == nil {
		ids := []string{}
		for _, m := range list.Data {
			if m.ID != "" {
				ids = append(ids, m.ID)
			}
		}
		if len(ids) > 0 {
			return ids
		}
		for id := range list.Models {
			if id != "" {
				ids = append(ids, id)
			}
		}
		return ids
	}
	return nil
}

func (c *HuaweiModelClient) ModelIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.modelIDs))
	copy(out, c.modelIDs)
	return out
}

// OpenAIModelsList is the shape Cursor/OpenAI SDKs accept. `created` is required;
// extra keys like `warning` make some clients drop the whole catalog.
func OpenAIModelsList(ids []string) map[string]any {
	created := time.Now().Unix()
	seen := map[string]struct{}{}
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  created,
			"owned_by": "huawei-codearts",
		})
	}
	return map[string]any{"object": "list", "data": data}
}

// ModelsPayload returns an OpenAI compatible /v1/models response.
func (c *HuaweiModelClient) ModelsPayload() map[string]any {
	return OpenAIModelsList(c.ModelIDs())
}

// ChatCompletions sends a request to Huawei CodeArts with the token obtained
// from the account. When ak/sk are present the request is AKSK signed before
// sending; otherwise it falls back to the CLI classifier style x-auth-token.
//
// CLI InferHub base is `{snap-access}/api/v2/` and OpenAI-compatible chat posts
// to `{base}/chat/completions` => `/api/v2/chat/completions` (NOT `/v1/...`).
func (c *HuaweiModelClient) ChatCompletions(ctx context.Context, creds *OAuthTokenResult, userToken string, body []byte) (*http.Response, error) {
	endpoint := strings.TrimRight(c.BaseURL, "/") + PathChatCompletions
	// Official CLI always signs Accept: application/json. stream=true in the
	// body is what switches InferHub to SSE — do not change Accept.
	headers := map[string]string{
		"Accept":          "application/json",
		"is_confidential": "false",
		"app-id":          "CodeAgent3.0",
		"x-client-type":   "kernel",
	}
	if creds != nil {
		if creds.AccessKeyID != "" && creds.SecretAccessKey != "" {
			signed, _, err := AKSKSign(creds.AccessKeyID, creds.SecretAccessKey, creds.SecurityToken, http.MethodPost, endpoint, body, headers)
			if err != nil {
				return nil, err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			for k, v := range signed {
				req.Header.Set(k, v)
			}
			req.Header.Set("Content-Type", "application/json")
			return c.clientFrom(ctx).Do(req)
		}
		if creds.SecurityToken != "" {
			headers["x-auth-token"] = creds.SecurityToken
		}
	}
	if userToken != "" {
		headers["x-auth-token"] = userToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.clientFrom(ctx).Do(req)
}

// markChatConnClose is retained as a no-op for compatibility.
// Previously it set req.Close=true to prevent TCP reuse, but this forced
// a full TLS handshake on every request (~200-500ms overhead). With
// MaxIdleConnsPerHost=2 and IdleConnTimeout=15s the idle pool stays small
// enough to avoid tripping Huawei TM.00001041 while allowing connection reuse.
func markChatConnClose(req *http.Request) {
	_ = req
}

// UpstreamError is a simple upstream failure.
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	return "upstream " + strconv.Itoa(e.Status) + ": " + e.Body
}

// ResponseText is a convenience helper for tests.
func ResponseText(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	resp.Body.Close()
	return string(b)
}

// parseRetryAfter helps multi-account cooldown logic.
func parseRetryAfter(s string) time.Duration {
	if s == "" {
		return 0
	}
	if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(s); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}
