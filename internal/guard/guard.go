package guard

import (
	"sync"
	"time"
)

const defaultTTL = 30 * time.Second

type Guard struct {
	mu      sync.Mutex
	entries map[string]map[string]time.Time // entity -> id -> written_at
	ttl     time.Duration
}

func New() *Guard {
	return &Guard{
		entries: make(map[string]map[string]time.Time),
		ttl:     defaultTTL,
	}
}

func (g *Guard) Mark(entity, id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.entries[entity] == nil {
		g.entries[entity] = make(map[string]time.Time)
	}
	g.entries[entity][id] = time.Now()
}

func (g *Guard) IsRecentlyWritten(entity, id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.entries[entity][id]
	if !ok {
		return false
	}
	if time.Since(t) > g.ttl {
		delete(g.entries[entity], id)
		return false
	}
	return true
}

func (g *Guard) Cleanup() {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	for entity, ids := range g.entries {
		for id, t := range ids {
			if now.Sub(t) > g.ttl {
				delete(ids, id)
			}
		}
		if len(ids) == 0 {
			delete(g.entries, entity)
		}
	}
}
