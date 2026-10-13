package proxypool

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ProbeResult is the outcome of a connectivity check.
type ProbeResult struct {
	OK         bool      `json:"ok"`
	ProxyID    string    `json:"proxy_id,omitempty"`
	ProxyType  string    `json:"proxy_type,omitempty"`
	StatusCode int       `json:"status_code,omitempty"`
	LatencyMS  int64     `json:"latency_ms"`
	Message    string    `json:"message"`
	CheckedAt  time.Time `json:"checked_at"`
	TargetURL  string    `json:"target_url"`
}

// DefaultProbeURL is a lightweight HTTPS endpoint used for reachability.
const DefaultProbeURL = "https://www.huaweicloud.com/"

// Probe dials through p and GETs targetURL (or DefaultProbeURL).
// 2xx–4xx count as reachable (auth walls still prove the proxy works).
func Probe(ctx context.Context, p Proxy, targetURL string) ProbeResult {
	p = Normalize(p)
	if targetURL == "" {
		targetURL = DefaultProbeURL
	}
	res := ProbeResult{
		ProxyID:   p.ID,
		ProxyType: p.Type,
		TargetURL: targetURL,
		CheckedAt: time.Now(),
	}
	if err := Validate(p); err != nil {
		res.Message = err.Error()
		return res
	}
	client, err := HTTPClient(p, 20*time.Second)
	if err != nil {
		res.Message = "build client: " + err.Error()
		return res
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		res.Message = err.Error()
		return res
	}
	req.Header.Set("User-Agent", "codearts-proxypool/1.0")
	start := time.Now()
	resp, err := client.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Message = err.Error()
		return res
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	res.StatusCode = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		res.OK = true
		res.Message = "reachable"
		return res
	}
	res.Message = fmt.Sprintf("unexpected status %d", resp.StatusCode)
	return res
}
