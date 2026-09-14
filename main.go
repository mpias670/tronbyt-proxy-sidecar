// Command sidecar is a small, generic HTTP proxy that fetches URLs on behalf
// of a Tronbyt (pixlet) app, transparently handling Cloudflare's bot
// protection so the app itself can stay a simple Starlark http.get() call.
//
// Design goals, in priority order:
//  1. Minimal steady-state memory: the process itself is a plain Go HTTP
//     server (a few MB RSS) with no browser running by default.
//  2. Short-lived, bounded memory when a challenge does need solving: a
//     headless Chromium instance is launched on demand, capped by
//     MAX_CONCURRENT_BROWSERS, given a hard timeout, and torn down
//     completely as soon as the cf_clearance cookie is extracted.
//  3. Most requests should never need step 2 at all: a solved session
//     (cf_clearance cookie + User-Agent) is cached per domain and reused
//     until it expires, and the first-attempt request uses a TLS
//     fingerprint that impersonates a real browser (via uTLS) rather than
//     Go's default, easily-fingerprinted TLS stack.
package main

import (
	"log"
	"net/http"
	"time"
)

func main() {
	reapZombies()

	cfg := loadConfig()
	initBrowserSemaphore(cfg.MaxConcurrentBrowsers)

	deps := &fetchDeps{
		cfg:       cfg,
		cache:     newSessionCache(),
		respCache: newResponseCache(),
		client:    newImpersonatingClient(cfg.UpstreamTimeout),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.Handle("/fetch", withAuth(cfg.AuthToken, http.HandlerFunc(deps.handleFetch)))

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: cfg.BrowserTimeout + cfg.UpstreamTimeout + 10*time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("sidecar listening on :%s (allowed domains: %v, browser fallback: %v, max concurrent browsers: %d, response cache ttl: %s)",
		cfg.Port, cfg.AllowedDomains, !cfg.DisableBrowserFallback, cfg.MaxConcurrentBrowsers, cfg.ResponseCacheTTL)
	log.Fatal(srv.ListenAndServe())
}

// withAuth enforces the X-Proxy-Token shared-secret header when token is
// non-empty. It's a no-op (allow everything) when token is empty, which is
// only appropriate on a trusted private network.
func withAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Proxy-Token") != token {
			httpError(w, http.StatusUnauthorized, "invalid or missing X-Proxy-Token header")
			return
		}
		next.ServeHTTP(w, r)
	})
}
