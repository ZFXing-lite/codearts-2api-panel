package respcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// Entry is one cached non-stream chat completion response.
type Entry struct {
	Key       string
	Model     string
	Body      []byte
	Status    int
	Tokens    float64
	CreatedAt time.Time
	ExpiresAt time.Time
	Hits      int64
}

type Stats struct {
	Enabled     bool    `json:"enabled"`
	Entries     int     `json:"entries"`
	Bytes       int64   `json:"bytes"`
	Hits        int64   `json:"hits"`
	Misses      int64   `json:"misses"`
	Stores      int64   `json:"stores"`
	Evictions   int64   `json:"evictions"`
	HitRate     float64 `json:"hit_rate"`
	SavedTokens float64 `json:"saved_tokens"`
	TTLSeconds  int64   `json:"ttl_seconds"`
	MaxEntries  int     `json:"max_entries"`
	MaxBytes    int64   `json:"max_bytes"`
}

// Cache is an in-memory TTL response cache for identical chat requests.
type Cache struct {
	mu         sync.Mutex
	items      map[string]*Entry
	ttl        time.Duration
	maxEntries int
	maxBytes   int64
	bytes      int64

	hits        atomic.Int64
	misses      atomic.Int64
	stores      atomic.Int64
	evictions   atomic.Int64
	savedTokens atomic.Uint64 // store as float bits via math? keep int tokens * 1000
	savedTok    atomic.Int64
}

func New(ttl time.Duration, maxEntries int, maxBytes int64) *Cache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	if maxEntries <= 0 {
		maxEntries = 512
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	return &Cache{
		items:      make(map[string]*Entry),
		ttl:        ttl,
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
	}
}

// KeyFromRequest builds a stable hash from cache-relevant request fields.
func KeyFromRequest(raw []byte) (key, model string) {
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:]), ""
	}
	// Drop fields that must not affect semantic equality.
	delete(req, "stream")
	delete(req, "user_id")
	delete(req, "stream_options")
	if m, ok := req["model"].(string); ok {
		model = m
	}
	norm, err := json.Marshal(req)
	if err != nil {
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:]), model
	}
	sum := sha256.Sum256(norm)
	return hex.EncodeToString(sum[:]), model
}

func (c *Cache) Get(key string) (*Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	if time.Now().After(e.ExpiresAt) {
		c.bytes -= int64(len(e.Body))
		delete(c.items, key)
		c.evictions.Add(1)
		c.misses.Add(1)
		return nil, false
	}
	e.Hits++
	c.hits.Add(1)
	c.savedTok.Add(int64(e.Tokens))
	out := *e
	out.Body = append([]byte(nil), e.Body...)
	return &out, true
}

func (c *Cache) Put(key, model string, status int, body []byte, tokens float64) {
	if key == "" || status < 200 || status >= 300 || len(body) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if old, ok := c.items[key]; ok {
		c.bytes -= int64(len(old.Body))
		delete(c.items, key)
	}
	for (len(c.items) >= c.maxEntries || c.bytes+int64(len(body)) > c.maxBytes) && len(c.items) > 0 {
		c.evictOldestLocked()
	}
	cp := append([]byte(nil), body...)
	c.items[key] = &Entry{
		Key:       key,
		Model:     model,
		Body:      cp,
		Status:    status,
		Tokens:    tokens,
		CreatedAt: now,
		ExpiresAt: now.Add(c.ttl),
	}
	c.bytes += int64(len(cp))
	c.stores.Add(1)
}

func (c *Cache) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, e := range c.items {
		if oldestKey == "" || e.CreatedAt.Before(oldest) {
			oldestKey = k
			oldest = e.CreatedAt
		}
	}
	if oldestKey == "" {
		return
	}
	e := c.items[oldestKey]
	c.bytes -= int64(len(e.Body))
	delete(c.items, oldestKey)
	c.evictions.Add(1)
}

// Manual Clear is intentionally omitted: entries expire by TTL only.

func (c *Cache) Snapshot() Stats {
	c.mu.Lock()
	entries := len(c.items)
	bytes := c.bytes
	c.mu.Unlock()
	hits := c.hits.Load()
	misses := c.misses.Load()
	total := hits + misses
	rate := 0.0
	if total > 0 {
		rate = float64(hits) / float64(total)
	}
	return Stats{
		Enabled:     true,
		Entries:     entries,
		Bytes:       bytes,
		Hits:        hits,
		Misses:      misses,
		Stores:      c.stores.Load(),
		Evictions:   c.evictions.Load(),
		HitRate:     rate,
		SavedTokens: float64(c.savedTok.Load()),
		TTLSeconds:  int64(c.ttl.Seconds()),
		MaxEntries:  c.maxEntries,
		MaxBytes:    c.maxBytes,
	}
}
