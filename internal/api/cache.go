// internal/api/cache.go
//
// Lightweight TTL cache for API responses.

package api

import (
	"sync"
	"time"
)

type cacheItem struct {
	value     []byte
	expiresAt time.Time
}

// Cache stores small response payloads with TTL.
type Cache struct {
	mu   sync.RWMutex
	item map[string]cacheItem
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{item: make(map[string]cacheItem)}
}

// Get returns cached bytes when present and not expired.
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	it, ok := c.item[key]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(it.expiresAt) {
		c.mu.Lock()
		delete(c.item, key)
		c.mu.Unlock()
		return nil, false
	}
	return it.value, true
}

// Set stores bytes with TTL.
func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	c.item[key] = cacheItem{value: value, expiresAt: time.Now().Add(ttl)}
	c.mu.Unlock()
}
