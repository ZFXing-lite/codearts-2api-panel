package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"go-codearts/internal/proxypool"
)

func (s *Server) handleListProxies(w http.ResponseWriter, r *http.Request) {
	if s.proxies == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "total": 0, "stats": map[string]any{}, "strategy": "round_robin"})
		return
	}
	items := s.proxies.List()
	out := make([]map[string]any, 0, len(items))
	for _, p := range items {
		out = append(out, p.PublicView())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    out,
		"total":    len(out),
		"stats":    s.proxies.Stats(),
		"strategy": s.proxies.Strategy(),
	})
}

func (s *Server) handleCreateProxy(w http.ResponseWriter, r *http.Request) {
	if s.proxies == nil {
		writeErr(w, http.StatusServiceUnavailable, "proxy pool not initialized")
		return
	}
	var req proxypool.Proxy
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	req.Enabled = true
	p, err := s.proxies.Upsert(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "proxy": p.PublicView()})
}

func (s *Server) handleUpdateProxy(w http.ResponseWriter, r *http.Request) {
	if s.proxies == nil {
		writeErr(w, http.StatusServiceUnavailable, "proxy pool not initialized")
		return
	}
	id := r.PathValue("id")
	existing, err := s.proxies.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body failed")
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	var req proxypool.Proxy
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	req.ID = id
	if req.Type == "" {
		req.Type = existing.Type
	}
	if req.Host == "" {
		req.Host = existing.Host
	}
	if req.Port == 0 {
		req.Port = existing.Port
	}
	if req.Name == "" {
		req.Name = existing.Name
	}
	if _, ok := raw["username"]; !ok {
		req.Username = existing.Username
	}
	if req.Password == "" {
		req.Password = existing.Password
	}
	if _, ok := raw["enabled"]; ok {
		if v, ok := raw["enabled"].(bool); ok {
			req.Enabled = v
		}
	} else {
		req.Enabled = existing.Enabled
	}
	p, err := s.proxies.Upsert(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Invalidate cached chat clients for this proxy — address/credentials
	// may have changed, old transports would route to the wrong host.
	if s.chatClients != nil {
		s.chatClients.invalidate("proxy:" + id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy": p.PublicView()})
}

func (s *Server) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	if s.proxies == nil {
		writeErr(w, http.StatusServiceUnavailable, "proxy pool not initialized")
		return
	}
	id := r.PathValue("id")
	if err := s.proxies.Delete(id); err != nil {
		if errors.Is(err, proxypool.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Invalidate cached chat clients for this proxy.
	if s.chatClients != nil {
		s.chatClients.invalidate("proxy:" + id)
	}
	for _, a := range s.db.List() {
		if a.ProxyID == id {
			a.ProxyID = ""
			_ = s.db.Upsert(a)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": "deleted"})
}

func (s *Server) handleTestProxy(w http.ResponseWriter, r *http.Request) {
	if s.proxies == nil {
		writeErr(w, http.StatusServiceUnavailable, "proxy pool not initialized")
		return
	}
	var req struct {
		ProxyID   string          `json:"proxy_id"`
		TargetURL string          `json:"target_url"`
		Proxy     proxypool.Proxy `json:"proxy"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.ProxyID != "" {
		res, err := s.proxies.ProbeAndRecord(r.Context(), req.ProxyID, req.TargetURL)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	writeJSON(w, http.StatusOK, s.proxies.ProbeInline(r.Context(), req.Proxy, req.TargetURL))
}

func (s *Server) handleProxyStrategy(w http.ResponseWriter, r *http.Request) {
	if s.proxies == nil {
		writeErr(w, http.StatusServiceUnavailable, "proxy pool not initialized")
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"strategy": s.proxies.Strategy(), "stats": s.proxies.Stats()})
		return
	}
	var req struct {
		Strategy string `json:"strategy"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := s.proxies.SetStrategy(proxypool.Strategy(req.Strategy)); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "strategy": s.proxies.Strategy()})
}

func (s *Server) handleSetAccountProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.db.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	var req struct {
		ProxyID string `json:"proxy_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	req.ProxyID = strings.TrimSpace(req.ProxyID)
	if req.ProxyID != "" {
		if s.proxies == nil {
			writeErr(w, http.StatusServiceUnavailable, "proxy pool not initialized")
			return
		}
		if _, err := s.proxies.Get(req.ProxyID); err != nil {
			writeErr(w, http.StatusBadRequest, "proxy not found")
			return
		}
	}
	a.ProxyID = req.ProxyID
	if err := s.db.Upsert(a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, redactAccount(a))
}

func (s *Server) handleEnableProxy(w http.ResponseWriter, r *http.Request) {
	if s.proxies == nil {
		writeErr(w, http.StatusServiceUnavailable, "proxy pool not initialized")
		return
	}
	id := r.PathValue("id")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	p, err := s.proxies.SetEnabled(id, req.Enabled)
	if err != nil {
		if errors.Is(err, proxypool.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Invalidate cached chat clients — enabled/disabled proxy needs fresh transport.
	if s.chatClients != nil {
		s.chatClients.invalidate("proxy:" + id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy": p.PublicView()})
}
