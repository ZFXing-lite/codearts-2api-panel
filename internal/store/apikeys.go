package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// APIKey is an OpenAI-compatible Bearer token for /v1/* endpoints.
type APIKey struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"`
	KeyHash    string    `json:"key_hash"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	// AccountIDs restricts this key to these pool accounts.
	// Empty / omitted = all callable accounts.
	AccountIDs       []string `json:"account_ids,omitempty"`
	RequestCount     int64    `json:"request_count,omitempty"`
	PromptTokens     float64  `json:"prompt_tokens,omitempty"`
	CompletionTokens float64  `json:"completion_tokens,omitempty"`
	InFlight         int64    `json:"in_flight,omitempty"` // runtime only, not persisted
}

type apiKeyFile struct {
	Keys    map[string]*APIKey `json:"keys"`
	Version int                `json:"version"`
}

// APIKeyStore persists API keys under dataDir/apikeys.json.
type APIKeyStore struct {
	mu   sync.Mutex
	path string
	keys map[string]*APIKey
}

func OpenAPIKeys(dataDir string) (*APIKeyStore, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "apikeys.json")
	s := &APIKeyStore{path: path, keys: map[string]*APIKey{}}
	data, err := os.ReadFile(path)
	if err == nil {
		var f apiKeyFile
		// 解析失败必须报错：当成空库继续跑的话，下一次 Create 会用空 map
		// 覆盖 apikeys.json，所有已签发的 key 一起消失。
		if uerr := json.Unmarshal(data, &f); uerr != nil {
			return nil, fmt.Errorf("apikeys.json 解析失败（请检查/恢复备份）: %w", uerr)
		}
		if f.Keys != nil {
			s.keys = f.Keys
		}
	}
	return s, nil
}

func (s *APIKeyStore) saveLocked() error {
	cleaned := make(map[string]*APIKey, len(s.keys))
	for id, k := range s.keys {
		if k == nil {
			continue
		}
		cp := *k
		cp.InFlight = 0
		cleaned[id] = &cp
	}
	f := apiKeyFile{Keys: cleaned, Version: 1}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func hashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func generateAPIKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-hw-" + hex.EncodeToString(b), nil
}

func cloneAPIKey(k *APIKey) *APIKey {
	if k == nil {
		return nil
	}
	cp := *k
	cp.KeyHash = ""
	cp.InFlight = 0
	if k.AccountIDs != nil {
		cp.AccountIDs = append([]string(nil), k.AccountIDs...)
	}
	return &cp
}

func NormalizeAccountIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
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
	if len(out) == 0 {
		return nil
	}
	return out
}

// AllowsAccount reports whether this key may use the given account.
// nil key or empty allowlist means the full pool.
func (k *APIKey) AllowsAccount(accountID string) bool {
	if k == nil || len(k.AccountIDs) == 0 {
		return true
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false
	}
	for _, id := range k.AccountIDs {
		if id == accountID {
			return true
		}
	}
	return false
}

func (s *APIKeyStore) List() []*APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*APIKey, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, cloneAPIKey(k))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func (s *APIKeyStore) CountEnabled() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, k := range s.keys {
		if k.Enabled {
			n++
		}
	}
	return n
}

// Create returns the plaintext key once; only the hash is stored.
// accountIDs empty = this key may use every callable account.
func (s *APIKeyStore) Create(name string, accountIDs []string) (*APIKey, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	raw, err := generateAPIKey()
	if err != nil {
		return nil, "", err
	}
	idBytes := make([]byte, 6)
	_, _ = rand.Read(idBytes)
	rec := &APIKey{
		ID:         "key-" + hex.EncodeToString(idBytes),
		Name:       name,
		Prefix:     raw[:12] + "...",
		KeyHash:    hashAPIKey(raw),
		Enabled:    true,
		CreatedAt:  time.Now(),
		AccountIDs: NormalizeAccountIDs(accountIDs),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[rec.ID] = rec
	if err := s.saveLocked(); err != nil {
		delete(s.keys, rec.ID)
		return nil, "", err
	}
	return cloneAPIKey(rec), raw, nil
}

func (s *APIKeyStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[id]; !ok {
		return errors.New("api key not found")
	}
	delete(s.keys, id)
	return s.saveLocked()
}

func (s *APIKeyStore) SetEnabled(id string, enabled bool) (*APIKey, error) {
	return s.Update(id, &enabled, nil)
}

// Update changes enabled and/or account allowlist. Nil pointer = leave unchanged.
func (s *APIKeyStore) Update(id string, enabled *bool, accountIDs *[]string) (*APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return nil, errors.New("api key not found")
	}
	if enabled != nil {
		k.Enabled = *enabled
	}
	if accountIDs != nil {
		k.AccountIDs = NormalizeAccountIDs(*accountIDs)
	}
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return cloneAPIKey(k), nil
}

// Lookup returns a copy of the enabled key matching raw and updates last_used in memory.
func (s *APIKeyStore) Lookup(raw string) *APIKey {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	h := hashAPIKey(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if !k.Enabled {
			continue
		}
		if k.KeyHash == h {
			k.LastUsedAt = time.Now()
			return cloneAPIKey(k)
		}
	}
	return nil
}

// Validate returns true if raw matches an enabled key (and touches last_used).
func (s *APIKeyStore) Validate(raw string) bool {
	return s.Lookup(raw) != nil
}

// AddUsage increments persisted request/token counters for a managed key.
func (s *APIKeyStore) AddUsage(id string, prompt, completion float64) error {
	id = strings.TrimSpace(id)
	if s == nil || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return errors.New("api key not found")
	}
	k.RequestCount++
	if prompt > 0 {
		k.PromptTokens += prompt
	}
	if completion > 0 {
		k.CompletionTokens += completion
	}
	k.LastUsedAt = time.Now()
	return s.saveLocked()
}
