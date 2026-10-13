package proxypool

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Pool is the public facade: registry + selection + client cache + probe.
type Pool struct {
	store    *Store
	mu       sync.Mutex
	clients  map[string]*http.Client
	cooldown time.Duration
	timeout  time.Duration
}

func OpenPool(dataDir string) (*Pool, error) {
	st, err := Open(dataDir)
	if err != nil {
		return nil, err
	}
	return &Pool{
		store:    st,
		clients:  map[string]*http.Client{},
		cooldown: 2 * time.Minute,
		timeout:  0, // chat/SSE must not use a hard client deadline
	}, nil
}

func (p *Pool) Store() *Store { return p.store }

func (p *Pool) List() []Proxy { return p.store.List() }

func (p *Pool) Get(id string) (Proxy, error) { return p.store.Get(id) }

func (p *Pool) Upsert(px Proxy) (Proxy, error) {
	px = Normalize(px)
	out, err := p.store.Upsert(px)
	if err == nil {
		p.invalidate(out.ID)
	}
	return out, err
}

func (p *Pool) Delete(id string) error {
	if err := p.store.Delete(id); err != nil {
		return err
	}
	p.invalidate(id)
	return nil
}

func (p *Pool) SetEnabled(id string, enabled bool) (Proxy, error) {
	out, err := p.store.SetEnabled(id, enabled)
	if err == nil {
		p.invalidate(id)
	}
	return out, err
}

func (p *Pool) SetStrategy(st Strategy) error { return p.store.SetStrategy(st) }

func (p *Pool) Strategy() Strategy { return p.store.Strategy() }

func (p *Pool) invalidate(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.clients, id)
}

// ClientFor returns a cached HTTP client for a sticky proxy id.
func (p *Pool) ClientFor(id string) (*http.Client, Proxy, error) {
	px, err := p.store.Get(id)
	if err != nil {
		return nil, Proxy{}, err
	}
	if !px.Usable(time.Now()) {
		return nil, px, ErrEmpty
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[id]; ok {
		return c, px, nil
	}
	c, err := HTTPClient(px, p.timeout)
	if err != nil {
		return nil, px, err
	}
	p.clients[id] = c
	return c, px, nil
}

// ClientNext picks by strategy (or sticky id) and returns an HTTP client.
func (p *Pool) ClientNext(stickyID string) (*http.Client, Proxy, error) {
	px, err := p.store.Next(stickyID)
	if err != nil {
		return nil, Proxy{}, err
	}
	return p.ClientFor(px.ID)
}

// ProbeAndRecord tests a proxy and updates success/failure counters.
func (p *Pool) ProbeAndRecord(ctx context.Context, id string, target string) (ProbeResult, error) {
	px, err := p.store.Get(id)
	if err != nil {
		return ProbeResult{}, err
	}
	res := Probe(ctx, px, target)
	if res.OK {
		p.store.MarkSuccess(id)
	} else {
		p.store.MarkFailure(id, res.Message, p.cooldown)
	}
	return res, nil
}

// ProbeInline tests a not-yet-saved proxy definition.
func (p *Pool) ProbeInline(ctx context.Context, px Proxy, target string) ProbeResult {
	return Probe(ctx, Normalize(px), target)
}

// Stats returns aggregate counters for the dashboard.
func (p *Pool) Stats() map[string]any {
	list := p.store.List()
	now := time.Now()
	total, enabled, cooling, disabled := 0, 0, 0, 0
	byType := map[string]int{}
	usable := 0
	for _, px := range list {
		total++
		byType[px.Type]++
		if px.Enabled {
			enabled++
		} else {
			disabled++
		}
		if !px.CooldownUntil.IsZero() && px.CooldownUntil.After(now) {
			cooling++
		}
		if px.Usable(now) {
			usable++
		}
	}
	return map[string]any{
		"total":    total,
		"enabled":  enabled,
		"disabled": disabled,
		"cooling":  cooling,
		"usable":   usable,
		"by_type":  byType,
		"strategy": p.store.Strategy(),
	}
}
