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
// sessions, plus in-flight de-duplication so concurrent requests for the
// same domain trigger at most one browser solve instead of one each.
type sessionCache struct {
	mu       sync.Mutex
	sessions map[string]*session
	inflight map[string]*inflightSolve
}

// inflightSolve lets multiple callers wait on a single solve-in-progress for
// a given domain, so a burst of requests during a cold cache never spins up
// more than one browser per domain concurrently.
type inflightSolve struct {
	done chan struct{}
	sess *session
	err  error
}

func newSessionCache() *sessionCache {
	return &sessionCache{
		sessions: make(map[string]*session),
		inflight: make(map[string]*inflightSolve),
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

// solveOnce runs solveFn at most once per domain concurrently: the first
// caller for a domain executes solveFn and populates the cache; any callers
// that arrive while that solve is in flight block and receive the same
// result instead of launching their own browser.
func (c *sessionCache) solveOnce(domain string, ttl time.Duration, solveFn func() (*session, error)) (*session, error) {
	c.mu.Lock()
	if s, ok := c.sessions[domain]; ok && time.Now().Before(s.expiresAt) {
		c.mu.Unlock()
		return s, nil
	}
	if in, ok := c.inflight[domain]; ok {
		c.mu.Unlock()
		<-in.done
		return in.sess, in.err
	}
	in := &inflightSolve{done: make(chan struct{})}
	c.inflight[domain] = in
	c.mu.Unlock()

	sess, err := solveFn()

	c.mu.Lock()
	if err == nil {
		sess.expiresAt = time.Now().Add(ttl)
		c.sessions[domain] = sess
	}
	in.sess, in.err = sess, err
	delete(c.inflight, domain)
	c.mu.Unlock()

	close(in.done)
	return sess, err
}
