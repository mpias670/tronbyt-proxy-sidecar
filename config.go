package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the sidecar, sourced from
// environment variables so the container can be configured entirely via
// docker-compose / systemd unit / k8s env, without a config file.
type Config struct {
	// Port the HTTP server listens on.
	Port string

	// AuthToken, if set, must be sent by callers in the X-Proxy-Token header.
	// Leave unset only when the sidecar is reachable exclusively on a
	// trusted private network (e.g. an isolated docker-compose network).
	AuthToken string

	// AllowedDomains restricts which upstream hosts /fetch is allowed to
	// reach. A request is allowed if its host equals an entry or is a
	// subdomain of one. This exists because /fetch is a generic proxy --
	// without an allowlist it would be an open relay for any client that
	// can reach the sidecar.
	AllowedDomains []string

	// BrowserCacheTTL bounds how long a solved Cloudflare challenge
	// (cf_clearance cookie + user agent) is reused before we solve again,
	// even if the cookie's own Expires/Max-Age suggests it lives longer.
	// Keeping this conservative avoids relying on a stale/blocked session.
	BrowserCacheTTL time.Duration

	// BrowserTimeout bounds how long a single on-demand headless-browser
	// challenge solve is allowed to run before being killed. This is the
	// primary guardrail against runaway memory: the browser process is
	// always short-lived, never a long-running sidecar component.
	BrowserTimeout time.Duration

	// MaxConcurrentBrowsers bounds how many headless browser instances may
	// run at once, capping peak memory usage regardless of request volume.
	MaxConcurrentBrowsers int

	// ChromePath optionally overrides the Chrome/Chromium executable path.
	// If empty, chromedp's default discovery is used.
	ChromePath string

	// UpstreamTimeout bounds a single upstream HTTP request (the fast path,
	// non-browser fetch).
	UpstreamTimeout time.Duration

	// DisableBrowserFallback turns off the headless-browser fallback
	// entirely, e.g. for deployments that only need the TLS-impersonation
	// fast path, or that cannot afford Chromium's disk/CPU footprint at all.
	DisableBrowserFallback bool

	// ResponseCacheTTL bounds how long a successful upstream response body
	// is cached, keyed by the full target URL. This is what actually stops
	// every pixlet app render cycle from re-hitting Surfline: surf
	// conditions change slowly, so a repeated request for the same
	// forecast/spot within this window is served from memory instead.
	// Independent of BrowserCacheTTL (which only covers the solved
	// Cloudflare session, not response bodies) and of any caching the
	// caller applies (e.g. pixlet's http.get(ttl_seconds=...)).
	ResponseCacheTTL time.Duration
}

func loadConfig() Config {
	cfg := Config{
		Port:                   getEnv("PORT", "8080"),
		AuthToken:              os.Getenv("AUTH_TOKEN"),
		AllowedDomains:         splitAndTrim(getEnv("ALLOWED_DOMAINS", "surfline.com")),
		BrowserCacheTTL:        getEnvDuration("BROWSER_CACHE_TTL_SECONDS", 25*60) * time.Second,
		BrowserTimeout:         getEnvDuration("BROWSER_TIMEOUT_SECONDS", 25) * time.Second,
		MaxConcurrentBrowsers:  getEnvInt("MAX_CONCURRENT_BROWSERS", 1),
		ChromePath:             os.Getenv("CHROME_PATH"),
		UpstreamTimeout:        getEnvDuration("UPSTREAM_TIMEOUT_SECONDS", 15) * time.Second,
		DisableBrowserFallback: getEnvBool("DISABLE_BROWSER_FALLBACK", false),
		ResponseCacheTTL:       getEnvDuration("RESPONSE_CACHE_TTL_SECONDS", 15*60) * time.Second,
	}

	if len(cfg.AllowedDomains) == 0 {
		log.Println("WARNING: ALLOWED_DOMAINS is empty -- /fetch will reject all requests. " +
			"Set ALLOWED_DOMAINS to a comma-separated list of hosts you want to allow, e.g. 'surfline.com'.")
	}
	if cfg.AuthToken == "" {
		log.Println("WARNING: AUTH_TOKEN is not set -- /fetch is unauthenticated. " +
			"Only run this way on a trusted private network.")
	}

	return cfg
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("WARNING: invalid int for %s=%q, using default %d", key, v, fallback)
	}
	return fallback
}

func getEnvDuration(key string, fallbackSeconds int) time.Duration {
	return time.Duration(getEnvInt(key, fallbackSeconds))
}

func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
		log.Printf("WARNING: invalid bool for %s=%q, using default %v", key, v, fallback)
	}
	return fallback
}

func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
