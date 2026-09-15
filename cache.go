package main

import (
	"net/http"
	"sync"
	"time"
)

// session holds a solved Cloudflare challenge for one domain: the cookies
// obtained (most importantly cf_clearance) and the User-Agent that was used
// to solve the challenge, since cf_clearance is bound to the requesting
// User-Agent/TLS fingerprint and won't validate if reused with a different one.
type session struct {
	cookies   []*http.Cookie
	userAgent string
	expiresAt time.Time
}

// sessionCache is a small in-memory, per-domain cache of solved Cloudflare
// sessions (cookies + User-Agent), opportunistically populated as a
// best-effort optimization for the cheap fast path.
type sessionCache struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func newSessionCache() *sessionCache {
	return &sessionCache{
		sessions: make(map[string]*session),
	}
}

// get returns a cached, non-expired session for domain, if any.
func (c *sessionCache) get(domain string) *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[domain]
	if !ok || time.Now().After(s.expiresAt) {
		return nil
	}
	return s
}

// invalidate drops any cached session for domain, e.g. because a request
// made with it was still rejected.
func (c *sessionCache) invalidate(domain string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, domain)
}

// set stores a session for domain, e.g. one opportunistically observed
// while performing a full browser-fetch for a different purpose. This is a
// best-effort optimization only: for a domain under strict Cloudflare
// Enterprise Bot Management, a replayed cookie won't actually unblock a
// non-browser client, but for less strictly protected domains it can let
// the cheap fast path succeed on subsequent requests.
func (c *sessionCache) set(domain string, sess *session, ttl time.Duration) {
	if sess == nil || ttl <= 0 {
		return
	}
	sess.expiresAt = time.Now().Add(ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[domain] = sess
}
