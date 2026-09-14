package main

import (
	"sync"
	"time"
)

// responseEntry is a single cached upstream response, keyed by full target
// URL. Caching here (in addition to whatever HTTP-level cache the caller,
// e.g. pixlet's http.get(ttl_seconds=...), applies) means repeated requests
// for the same forecast/spot never re-hit Surfline within the TTL, even if
// the caller's own cache is disabled, misconfigured, or simply not shared
// across renders (e.g. multiple tronbyt-server processes, or a render
// pipeline that doesn't persist the pixlet HTTP cache between invocations).
type responseEntry struct {
	status      int
	contentType string
	body        []byte
	expiresAt   time.Time
}

// responseCache is a small in-memory cache of successful upstream responses.
// Only successful (2xx) responses are cached -- errors and challenge pages
// should never be cached, so a transient failure doesn't get "stuck" for the
// whole TTL.
type responseCache struct {
	mu      sync.Mutex
	entries map[string]responseEntry
}

func newResponseCache() *responseCache {
	return &responseCache{entries: make(map[string]responseEntry)}
}

func (c *responseCache) get(key string) (responseEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expiresAt) {
		return responseEntry{}, false
	}
	return e, true
}

func (c *responseCache) set(key string, e responseEntry, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	e.expiresAt = time.Now().Add(ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = e
}
