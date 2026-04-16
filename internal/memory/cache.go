package memory

import (
	"sync"
	"time"
)

type cacheEntry struct {
	timestamp time.Time
	data      []map[string]interface{}
}

type Cache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
	ttl     time.Duration
}

func NewCache(ttl time.Duration) *Cache {
	return &Cache{
		entries: make(map[string]cacheEntry),
		ttl:     ttl,
	}
}

func (c *Cache) Get(key string) ([]map[string]interface{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok || time.Since(entry.timestamp) > c.ttl {
		return nil, false
	}
	return entry.data, true
}

func (c *Cache) Set(key string, data []map[string]interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry{
		timestamp: time.Now(),
		data:      data,
	}
}

func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cacheEntry)
}
