package api

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go-codearts/internal/proxypool"
	"go-codearts/internal/store"
)

type batchReq struct {
	Action  string   `json:"action"`
	IDs     []string `json:"ids"`
	ProxyID string   `json:"proxy_id,omitempty"`
}

type batchItemResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func normalizeIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func writeBatchResult(w http.ResponseWriter, action string, items []batchItemResult) {
	ok := 0
	for _, it := range items {
		if it.OK {
			ok++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"action":  action,
		"total":   len(items),
		"success": ok,
		"failed":  len(items) - ok,
		"items":   items,
	})
}

// POST /api/accounts/batch
// action: enable | disable | delete | refresh_quota | set_proxy
func (s *Server) handleBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req batchReq
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	ids := normalizeIDs(req.IDs)
	if action == "" {
		writeErr(w, http.StatusBadRequest, "action required")
		return
	}
	if len(ids) == 0 {
		writeErr(w, http.StatusBadRequest, "ids required")
		return
	}
	switch action {
	case "enable", "disable", "delete", "refresh_quota", "set_proxy":
	default:
		writeErr(w, http.StatusBadRequest, "unknown action: "+action)
		return
	}

	proxyID := strings.TrimSpace(req.ProxyID)
	if action == "set_proxy" && proxyID != "" {
		if s.proxies == nil {
			writeErr(w, http.StatusServiceUnavailable, "proxy pool not initialized")
			return
		}
		if _, err := s.proxies.Get(proxyID); err != nil {
			writeErr(w, http.StatusBadRequest, "proxy not found")
			return
		}
	}

	now := time.Now()
	items := make([]batchItemResult, 0, len(ids))
	for _, id := range ids {
		it := batchItemResult{ID: id}
		a, err := s.db.Get(id)
		if err != nil {
			it.Error = err.Error()
			items = append(items, it)
			continue
		}
		switch action {
		case "disable":
			a.Disabled = true
			a.DisabledReason = "manual"
			a.RecomputeCallGate(now)
			if err := s.db.Upsert(a); err != nil {
				it.Error = err.Error()
				break
			}
			it.OK = true
		case "enable":
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
				it.Error = err.Error()
				break
			}
			it.OK = true
		case "delete":
			if err := s.db.Delete(id); err != nil {
				it.Error = err.Error()
				break
			}
			it.OK = true
		case "refresh_quota":
			if a.Credentials == nil || !a.Credentials.Usable() {
				a.RecomputeCallGate(time.Now())
				_ = s.db.Upsert(a)
				it.Error = "no usable credentials"
				break
			}
			if _, err := s.refreshAccountQuota(r.Context(), a); err != nil {
				it.Error = err.Error()
				break
			}
			it.OK = true
		case "set_proxy":
			a.ProxyID = proxyID
			a.UpdatedAt = time.Now()
			if err := s.db.Upsert(a); err != nil {
				it.Error = err.Error()
				break
			}
			it.OK = true
		}
		items = append(items, it)
	}
	writeBatchResult(w, action, items)
}

// POST /api/proxies/batch
// action: enable | disable | delete | test
func (s *Server) handleBatchProxies(w http.ResponseWriter, r *http.Request) {
	if s.proxies == nil {
		writeErr(w, http.StatusServiceUnavailable, "proxy pool not initialized")
		return
	}
	var req batchReq
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	ids := normalizeIDs(req.IDs)
	if action == "" {
		writeErr(w, http.StatusBadRequest, "action required")
		return
	}
	if len(ids) == 0 {
		writeErr(w, http.StatusBadRequest, "ids required")
		return
	}
	switch action {
	case "enable", "disable", "delete", "test":
	default:
		writeErr(w, http.StatusBadRequest, "unknown action: "+action)
		return
	}

	items := make([]batchItemResult, 0, len(ids))
	for _, id := range ids {
		it := batchItemResult{ID: id}
		switch action {
		case "enable", "disable":
			_, err := s.proxies.SetEnabled(id, action == "enable")
			if err != nil {
				it.Error = err.Error()
				break
			}
			if s.chatClients != nil {
				s.chatClients.invalidate("proxy:" + id)
			}
			it.OK = true
		case "delete":
			if err := s.proxies.Delete(id); err != nil {
				it.Error = err.Error()
				break
			}
			if s.chatClients != nil {
				s.chatClients.invalidate("proxy:" + id)
			}
			for _, a := range s.db.List() {
				if a.ProxyID == id {
					a.ProxyID = ""
					_ = s.db.Upsert(a)
				}
			}
			it.OK = true
		case "test":
			res, err := s.proxies.ProbeAndRecord(r.Context(), id, "")
			if err != nil {
				it.Error = err.Error()
				break
			}
			if !res.OK {
				it.Error = res.Message
				if it.Error == "" {
					it.Error = "probe failed"
				}
				break
			}
			it.OK = true
		}
		items = append(items, it)
	}
	writeBatchResult(w, action, items)
}

// POST /api/proxies/import  { "text": "socks5://...\n..." }
func (s *Server) handleImportProxies(w http.ResponseWriter, r *http.Request) {
	if s.proxies == nil {
		writeErr(w, http.StatusServiceUnavailable, "proxy pool not initialized")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	lines := strings.Split(req.Text, "\n")
	type row struct {
		Line  string `json:"line"`
		OK    bool   `json:"ok"`
		ID    string `json:"id,omitempty"`
		Error string `json:"error,omitempty"`
	}
	out := make([]row, 0)
	okN := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		it := row{Line: line}
		px, err := parseProxyLine(line)
		if err != nil {
			it.Error = err.Error()
			out = append(out, it)
			continue
		}
		px.Enabled = true
		saved, err := s.proxies.Upsert(px)
		if err != nil {
			it.Error = err.Error()
			out = append(out, it)
			continue
		}
		it.OK = true
		it.ID = saved.ID
		okN++
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"total":   len(out),
		"success": okN,
		"failed":  len(out) - okN,
		"items":   out,
	})
}

func parseProxyLine(line string) (proxypool.Proxy, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return proxypool.Proxy{}, errors.New("empty line")
	}
	// URL form: scheme://user:pass@host:port
	if strings.Contains(line, "://") {
		u, err := url.Parse(line)
		if err != nil {
			return proxypool.Proxy{}, err
		}
		host := u.Hostname()
		portStr := u.Port()
		if host == "" || portStr == "" {
			return proxypool.Proxy{}, errors.New("need host:port")
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 {
			return proxypool.Proxy{}, errors.New("bad port")
		}
		user := ""
		pass := ""
		if u.User != nil {
			user = u.User.Username()
			pass, _ = u.User.Password()
		}
		typ := strings.ToLower(u.Scheme)
		if typ == "" {
			typ = "socks5h"
		}
		return proxypool.Proxy{
			Type:     typ,
			Host:     host,
			Port:     port,
			Username: user,
			Password: pass,
			Enabled:  true,
		}, nil
	}
	// host:port or user:pass@host:port
	user, pass, rest := "", "", line
	if at := strings.LastIndex(line, "@"); at >= 0 {
		cred := line[:at]
		rest = line[at+1:]
		if i := strings.Index(cred, ":"); i >= 0 {
			user = cred[:i]
			pass = cred[i+1:]
		} else {
			user = cred
		}
	}
	host, portStr, err := net.SplitHostPort(rest)
	if err != nil {
		return proxypool.Proxy{}, errors.New("expect host:port or scheme://user:pass@host:port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return proxypool.Proxy{}, errors.New("bad port")
	}
	return proxypool.Proxy{
		Type:     "socks5h",
		Host:     host,
		Port:     port,
		Username: user,
		Password: pass,
		Enabled:  true,
	}, nil
}

// POST /api/apikeys/batch
// action: enable | disable | delete
func (s *Server) handleBatchAPIKeys(w http.ResponseWriter, r *http.Request) {
	if s.apiKeys == nil {
		writeErr(w, http.StatusServiceUnavailable, "api key store not initialized")
		return
	}
	var req batchReq
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	ids := normalizeIDs(req.IDs)
	if action == "" {
		writeErr(w, http.StatusBadRequest, "action required")
		return
	}
	if len(ids) == 0 {
		writeErr(w, http.StatusBadRequest, "ids required")
		return
	}
	switch action {
	case "enable", "disable", "delete":
	default:
		writeErr(w, http.StatusBadRequest, "unknown action: "+action)
		return
	}

	items := make([]batchItemResult, 0, len(ids))
	for _, id := range ids {
		it := batchItemResult{ID: id}
		switch action {
		case "enable", "disable":
			_, err := s.apiKeys.SetEnabled(id, action == "enable")
			if err != nil {
				it.Error = err.Error()
				break
			}
			it.OK = true
		case "delete":
			if err := s.apiKeys.Delete(id); err != nil {
				it.Error = err.Error()
				break
			}
			it.OK = true
		}
		items = append(items, it)
	}
	writeBatchResult(w, action, items)
}

// ensure store import used when compiling unused-check edge cases
var _ = store.CallGateReady
