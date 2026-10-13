package proxypool

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Strategy controls how Next() picks a proxy.
type Strategy string

const (
	StrategySticky     Strategy = "sticky"      // require explicit ID
	StrategyRoundRobin Strategy = "round_robin" // default
	StrategyRandom     Strategy = "random"
)

// Proxy is one outbound proxy entry (ds2api-style registry + pool extras).
type Proxy struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Type     string `json:"type,omitempty"` // http | https | socks5 | socks5h
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Enabled  bool   `json:"enabled"`
	Weight   int    `json:"weight,omitempty"` // reserved; currently unused

	// Runtime health (persisted lightly so restarts keep cooldown memory).
	FailCount     int       `json:"fail_count,omitempty"`
	SuccessCount  int       `json:"success_count,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	LastCheckAt   time.Time `json:"last_check_at,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	CreatedAt     time.Time `json:"created_at,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
}

// PublicView redacts password for API responses.
func (p Proxy) PublicView() map[string]any {
	return map[string]any{
		"id":             p.ID,
		"name":           p.Name,
		"type":           p.Type,
		"host":           p.Host,
		"port":           p.Port,
		"username":       p.Username,
		"has_password":   strings.TrimSpace(p.Password) != "",
		"enabled":        p.Enabled,
		"fail_count":     p.FailCount,
		"success_count":  p.SuccessCount,
		"last_error":     p.LastError,
		"last_check_at":  p.LastCheckAt,
		"cooldown_until": p.CooldownUntil,
		"created_at":     p.CreatedAt,
		"updated_at":     p.UpdatedAt,
		"address":        p.Address(),
	}
}

func (p Proxy) Address() string {
	if p.Host == "" || p.Port <= 0 {
		return ""
	}
	return net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

func (p Proxy) URLString() (string, error) {
	p = Normalize(p)
	if err := Validate(p); err != nil {
		return "", err
	}
	user := p.Username
	pass := p.Password
	auth := ""
	if user != "" || pass != "" {
		auth = urlUserInfo(user, pass) + "@"
	}
	scheme := p.Type
	if scheme == "socks5h" {
		scheme = "socks5"
	}
	return fmt.Sprintf("%s://%s%s", scheme, auth, p.Address()), nil
}

func urlUserInfo(user, pass string) string {
	// Escape is handled by net/url when building URL; keep raw for socks Auth.
	if pass == "" {
		return user
	}
	return user + ":" + pass
}

func Normalize(p Proxy) Proxy {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.Type = strings.ToLower(strings.TrimSpace(p.Type))
	p.Host = strings.TrimSpace(p.Host)
	p.Username = strings.TrimSpace(p.Username)
	// password may intentionally have spaces — only trim ends
	p.Password = strings.TrimSpace(p.Password)
	if p.Type == "" {
		p.Type = "socks5h"
	}
	switch p.Type {
	case "socks5http", "socks", "socks5h":
		p.Type = "socks5h"
	}
	if p.ID == "" {
		p.ID = StableID(p)
	}
	if p.Name == "" && p.Host != "" && p.Port > 0 {
		p.Name = p.Address()
	}
	if p.Weight <= 0 {
		p.Weight = 1
	}
	return p
}

func StableID(p Proxy) string {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "%s|%s|%d|%s",
		strings.ToLower(strings.TrimSpace(p.Type)),
		strings.ToLower(strings.TrimSpace(p.Host)),
		p.Port,
		strings.TrimSpace(p.Username),
	)
	sum := h.Sum(nil)
	return "proxy_" + hex.EncodeToString(sum[:6])
}

func Validate(p Proxy) error {
	p = Normalize(p)
	switch p.Type {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("unsupported proxy type %q (http|https|socks5|socks5h)", p.Type)
	}
	if p.Host == "" {
		return fmt.Errorf("proxy host required")
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("proxy port out of range")
	}
	return nil
}

func (p Proxy) Usable(now time.Time) bool {
	if !p.Enabled {
		return false
	}
	if !p.CooldownUntil.IsZero() && p.CooldownUntil.After(now) {
		return false
	}
	return Validate(p) == nil
}
