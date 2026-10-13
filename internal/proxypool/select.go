package proxypool

import (
	"math/rand"
	"time"
)

// Next returns the next usable proxy according to strategy.
// If id is non-empty, sticky lookup by id is used regardless of strategy.
func (s *Store) Next(id string) (Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	if id != "" {
		p, ok := s.proxies[id]
		if !ok {
			return Proxy{}, ErrNotFound
		}
		if !p.Usable(now) {
			return Proxy{}, ErrEmpty
		}
		return p, nil
	}

	usable := s.usableLocked(now)
	if len(usable) == 0 {
		return Proxy{}, ErrEmpty
	}

	switch s.strategy {
	case StrategyRandom:
		return usable[rand.Intn(len(usable))], nil
	case StrategySticky:
		return Proxy{}, ErrEmpty
	default:
		s.rrCursor++
		return usable[s.rrCursor%len(usable)], nil
	}
}
