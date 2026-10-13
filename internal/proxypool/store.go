package proxypool

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("proxy not found")
	ErrEmpty    = errors.New("no usable proxy")
)

type fileData struct {
	Proxies  []Proxy   `json:"proxies"`
	Strategy Strategy  `json:"strategy"`
	Version  int       `json:"version"`
	Updated  time.Time `json:"updated_at"`
}

// Store is a file-backed proxy registry (independent of accounts.json).
type Store struct {
	mu       sync.Mutex
	path     string
	proxies  map[string]Proxy
	strategy Strategy
	rrCursor int
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "proxies.json")
	s := &Store{
		path:     path,
		proxies:  map[string]Proxy{},
		strategy: StrategyRoundRobin,
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var fd fileData
		if json.Unmarshal(data, &fd) == nil {
			for _, p := range fd.Proxies {
				p = Normalize(p)
				s.proxies[p.ID] = p
			}
			if fd.Strategy != "" {
				s.strategy = fd.Strategy
			}
		}
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	items := make([]Proxy, 0, len(s.proxies))
	for _, p := range s.proxies {
		items = append(items, p)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	fd := fileData{
		Proxies:  items,
		Strategy: s.strategy,
		Version:  1,
		Updated:  time.Now(),
	}
	raw, err := json.MarshalIndent(fd, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil { // 0600 — proxy passwords are secrets
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Strategy() Strategy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.strategy
}

func (s *Store) SetStrategy(st Strategy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch Strategy(strings.ToLower(strings.TrimSpace(string(st)))) {
	case "round-robin", "rr", StrategyRoundRobin:
		st = StrategyRoundRobin
	case StrategySticky, "sticky-only":
		st = StrategySticky
	case StrategyRandom:
		st = StrategyRandom
	default:
		return errors.New("invalid strategy")
	}
	s.strategy = st
	return s.saveLocked()
}

func (s *Store) List() []Proxy {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Proxy, 0, len(s.proxies))
	for _, p := range s.proxies {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Store) Get(id string) (Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proxies[strings.TrimSpace(id)]
	if !ok {
		return Proxy{}, ErrNotFound
	}
	return p, nil
}

func (s *Store) Upsert(p Proxy) (Proxy, error) {
	p = Normalize(p)
	if err := Validate(p); err != nil {
		return Proxy{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if existing, ok := s.proxies[p.ID]; ok {
		if p.Password == "" {
			p.Password = existing.Password
		}
		p.CreatedAt = existing.CreatedAt
		p.FailCount = existing.FailCount
		p.SuccessCount = existing.SuccessCount
		p.LastError = existing.LastError
		p.LastCheckAt = existing.LastCheckAt
		p.CooldownUntil = existing.CooldownUntil
	} else {
		if p.CreatedAt.IsZero() {
			p.CreatedAt = now
		}
	}
	p.UpdatedAt = now
	s.proxies[p.ID] = p
	if err := s.saveLocked(); err != nil {
		return Proxy{}, err
	}
	return p, nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id = strings.TrimSpace(id)
	if _, ok := s.proxies[id]; !ok {
		return ErrNotFound
	}
	delete(s.proxies, id)
	return s.saveLocked()
}

func (s *Store) SetEnabled(id string, enabled bool) (Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proxies[strings.TrimSpace(id)]
	if !ok {
		return Proxy{}, ErrNotFound
	}
	p.Enabled = enabled
	p.UpdatedAt = time.Now()
	if enabled {
		p.CooldownUntil = time.Time{}
		p.LastError = ""
	}
	s.proxies[p.ID] = p
	if err := s.saveLocked(); err != nil {
		return Proxy{}, err
	}
	return p, nil
}

func (s *Store) MarkSuccess(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proxies[strings.TrimSpace(id)]
	if !ok {
		return
	}
	p.SuccessCount++
	p.FailCount = 0
	p.LastError = ""
	p.LastCheckAt = time.Now()
	p.CooldownUntil = time.Time{}
	p.UpdatedAt = p.LastCheckAt
	s.proxies[p.ID] = p
	_ = s.saveLocked()
}

func (s *Store) MarkFailure(id, errMsg string, cooldown time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.proxies[strings.TrimSpace(id)]
	if !ok {
		return
	}
	now := time.Now()
	p.FailCount++
	p.LastError = strings.TrimSpace(errMsg)
	p.LastCheckAt = now
	if cooldown > 0 && p.FailCount >= 3 {
		p.CooldownUntil = now.Add(cooldown)
	}
	p.UpdatedAt = now
	s.proxies[p.ID] = p
	_ = s.saveLocked()
}

func (s *Store) usableLocked(now time.Time) []Proxy {
	out := make([]Proxy, 0, len(s.proxies))
	for _, p := range s.proxies {
		if p.Usable(now) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
