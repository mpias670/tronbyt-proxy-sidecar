package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
)

// maxUpstreamBodyBytes caps how much of an upstream response we buffer, to
// protect the sidecar's own memory from an unexpectedly huge or malicious
// upstream response.
const maxUpstreamBodyBytes = 10 * 1024 * 1024 // 10MB

// cloudflareChallengeMarkers are strings that reliably appear in Cloudflare's
// interstitial "Just a moment..." / managed-challenge HTML, used to detect
// that a response is a challenge page rather than the real upstream payload.
// Note: Cloudflare injects some of these (e.g. the cv/challenge-platform
// analytics snippet) into many of its own error pages site-wide, not just
// active bot challenges -- so a match here doesn't guarantee a headless
// browser solve will succeed, only that the response came from Cloudflare's
// edge rather than the real upstream payload and is worth a browser retry.
var cloudflareChallengeMarkers = []string{
	"Just a moment",
	"cf-chl",
	"cf_chl_opt",
	"cf$cv$params",
	"challenge-platform",
	"Enable JavaScript and cookies to continue",
	"challenges.cloudflare.com",
	"Attention Required! | Cloudflare",
	"cdn-cgi/challenge-platform",
	// Cloudflare also serves a plain, decoy "502 Bad Gateway / nginx" page
	// (with a distinctive trailing block of HTML-comment padding) to
	// requests it's already confident are bots -- deliberately omitting the
	// usual JS-challenge markers above so the block can't be fingerprinted
	// and retried by naive scrapers. Observed paired with a real HTTP status
	// of 403 despite the body's claimed "502", which a genuine origin error
	// would not do.
	"a padding to disable msie and chrome friendly error page",
}

func isChallengeResponse(status int, header http.Header, body []byte) bool {
	if status == http.StatusForbidden || status == 503 {
		if header.Get("cf-mitigated") == "challenge" {
			return true
		}
		// Any non-2xx from a server that identifies itself as Cloudflare is
		// worth a browser-solve attempt: Cloudflare fronts the entire
		// allowlisted domain, so a 403/503 direct from its edge (as opposed
		// to the real upstream) is far more likely to be a bot block than a
		// genuine application-level error, even when the body doesn't match
		// any of the known challenge-page markers below (see the decoy
		// "502 Bad Gateway" page noted above).
		if strings.Contains(strings.ToLower(header.Get("Server")), "cloudflare") {
			return true
		}
	}
	lower := bytes.ToLower(body)
	for _, marker := range cloudflareChallengeMarkers {
		if bytes.Contains(lower, bytes.ToLower([]byte(marker))) {
			return true
		}
	}
	return false
}

type fetchDeps struct {
	cfg       Config
	cache     *sessionCache
	respCache *responseCache
	client    *http.Client
}

func (d *fetchDeps) handleFetch(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		httpError(w, http.StatusBadRequest, "missing required query parameter: url")
		return
	}

	target, err := url.Parse(rawURL)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		httpError(w, http.StatusBadRequest, "invalid url parameter")
		return
	}

	if !isDomainAllowed(target.Hostname(), d.cfg.AllowedDomains) {
		httpError(w, http.StatusForbidden, fmt.Sprintf("domain not allowed: %s", target.Hostname()))
		return
	}

	domain := strings.ToLower(target.Hostname())
	cacheKey := target.String()

	if entry, ok := d.respCache.get(cacheKey); ok {
		if entry.contentType != "" {
			w.Header().Set("Content-Type", entry.contentType)
		}
		w.Header().Set("X-Sidecar-Cache", "hit")
		w.WriteHeader(entry.status)
		w.Write(entry.body)
		return
	}

	status, contentType, body, err := d.fetchWithFallback(r.Context(), domain, target.String())
	if err != nil {
		log.Printf("fetch error for %s: %v", target.String(), err)
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}

	if status >= 200 && status < 300 {
		d.respCache.set(cacheKey, responseEntry{
			status:      status,
			contentType: contentType,
			body:        body,
		}, d.cfg.ResponseCacheTTL)
	} else {
		// Non-2xx responses are passed through as-is (they're not cached),
		// but log them so a real upstream outage or a stale challenge-marker
		// list is diagnosable from `docker logs` without needing to curl the
		// sidecar directly. Deliberately status+URL only, not the body --
		// keeps log volume low and avoids ever logging upstream response
		// content.
		log.Printf("fetch passthrough non-2xx for %s: status=%d", target.String(), status)
	}

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("X-Sidecar-Cache", "miss")
	w.WriteHeader(status)
	w.Write(body)
}

// maxTransientRetries is how many times to retry a request that failed with
// a status that looks like a transient edge/rate-limit hiccup (not a real
// "not found", and not confidently a solvable Cloudflare challenge either).
// Cloudflare's edge has been observed to intermittently 403 a request that
// succeeds moments later with the exact same URL and session -- in bursty
// periods this has been measured at roughly a 30% single-request success
// rate, so a low retry budget isn't enough headroom. 4 retries (5 attempts
// total) pushes the odds of at least one success well above 80% in that
// regime while keeping the added worst-case latency (with the backoff below)
// under ~2s, still far cheaper than escalating to a headless-browser solve.
const maxTransientRetries = 4

var transientRetryBackoff = 400 * time.Millisecond

// isTransientStatus reports whether status is worth a quick retry rather
// than treating it as a definitive result. 404 is intentionally excluded --
// a genuinely missing resource won't start existing after a short sleep.
func isTransientStatus(status int) bool {
	switch status {
	case http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// fetchWithFallback performs the fast-path (TLS-impersonated) request first,
// using any cached solved session for domain. Cloudflare's challenge-page
// markers (see cloudflareChallengeMarkers) are known to also appear on some
// merely-transient origin error pages, not only genuine bot challenges --
// so this exhausts the cheap transient-status retries FIRST, regardless of
// whether the response looks like a challenge, and only escalates to the
// much more expensive headless-browser solve once retries are exhausted AND
// the last response still looks like a challenge. This avoids wasting the
// browser-solve budget (and risking a caller-side timeout while it runs) on
// what was actually just Cloudflare edge flakiness that a plain retry would
// have cleared.
func (d *fetchDeps) fetchWithFallback(ctx context.Context, domain, targetURL string) (int, string, []byte, error) {
	sess := d.cache.get(domain)

	var status int
	var header http.Header
	var body []byte
	var err error

	for attempt := 0; ; attempt++ {
		status, header, body, err = d.doRequest(ctx, targetURL, sess)
		if err != nil {
			return 0, "", nil, err
		}

		if status >= 200 && status < 300 {
			return status, header.Get("Content-Type"), body, nil
		}

		if !isTransientStatus(status) || attempt >= maxTransientRetries {
			// Retries exhausted (or this status isn't worth retrying, e.g.
			// a real 404) -- fall through to the challenge check below.
			break
		}

		select {
		case <-ctx.Done():
			return 0, "", nil, ctx.Err()
		case <-time.After(transientRetryBackoff):
		}
	}

	if !isChallengeResponse(status, header, body) {
		return status, header.Get("Content-Type"), body, nil
	}

	if d.cfg.DisableBrowserFallback {
		return 0, "", nil, fmt.Errorf("upstream returned a Cloudflare challenge and browser fallback is disabled")
	}

	d.cache.invalidate(domain)

	newSess, err := d.cache.solveOnce(domain, d.cfg.BrowserCacheTTL, func() (*session, error) {
		return solveChallenge(ctx, d.cfg, targetURL)
	})
	if err != nil {
		return 0, "", nil, fmt.Errorf("challenge solve failed: %w", err)
	}

	status, header, body, err = d.doRequest(ctx, targetURL, newSess)
	if err != nil {
		return 0, "", nil, err
	}
	if isChallengeResponse(status, header, body) {
		d.cache.invalidate(domain)
		return 0, "", nil, fmt.Errorf("still received a Cloudflare challenge after solving")
	}

	return status, header.Get("Content-Type"), body, nil
}

func (d *fetchDeps) doRequest(ctx context.Context, targetURL string, sess *session) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return 0, nil, nil, err
	}

	userAgent := ""
	if sess != nil {
		userAgent = sess.userAgent
		for _, c := range sess.cookies {
			req.AddCookie(c)
		}
	}
	setBrowserHeaders(req, userAgent)

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBodyBytes))
	if err != nil {
		return 0, nil, nil, err
	}

	// Since we set our own Accept-Encoding header (to look like a real
	// browser), Go's http.Transport does not auto-decompress the response
	// -- it only does that when it added the header itself. Do it manually
	// so callers always get a plain, already-decoded body.
	body, err = decompress(resp.Header.Get("Content-Encoding"), body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("decompressing response: %w", err)
	}

	return resp.StatusCode, resp.Header, body, nil
}


// decompress decodes body according to the upstream Content-Encoding header.
// Returns body unchanged for encodings it doesn't recognize (e.g. empty/identity).
func decompress(encoding string, body []byte) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(io.LimitReader(r, maxUpstreamBodyBytes))
	case "deflate":
		r := flate.NewReader(bytes.NewReader(body))
		defer r.Close()
		return io.ReadAll(io.LimitReader(r, maxUpstreamBodyBytes))
	case "br":
		r := brotli.NewReader(bytes.NewReader(body))
		return io.ReadAll(io.LimitReader(r, maxUpstreamBodyBytes))
	default:
		return body, nil
	}
}

func httpError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}
