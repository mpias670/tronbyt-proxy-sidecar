package main

import (
	"context"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// browserSemaphore bounds how many headless-browser instances may run at
// once, regardless of how many requests are in flight. This is the hard cap
// on peak memory usage from the browser fallback: with the default of 1,
// there is never more than one Chromium process alive at any moment, and it
// only exists for the few seconds it takes to solve a challenge.
var browserSemaphore chan struct{}

func initBrowserSemaphore(max int) {
	if max < 1 {
		max = 1
	}
	browserSemaphore = make(chan struct{}, max)
}

// browserFetchResult is the outcome of fetching targetURL through a real,
// live headless-browser navigation: the actual HTTP response Cloudflare
// served to that page load (status, headers, body), plus the resulting
// session (cookies + User-Agent) observed along the way, purely as a cheap
// opportunistic optimization for other, less strictly protected domains --
// it is NOT relied upon to unblock this same request again.
type browserFetchResult struct {
	status  int
	header  http.Header
	body    []byte
	session *session
}

// browserFetch launches a short-lived headless browser and navigates it
// directly to targetURL, waiting for Cloudflare's protection (JS challenge
// and/or bot-management scoring) to run its course and letting the browser
// itself receive the final response -- rather than solving a challenge once
// and then replaying the resulting cf_clearance cookie through a separate,
// non-browser HTTP client.
//
// This distinction matters: Surfline (and any Cloudflare Enterprise
// Bot Management site) scores every request's live TLS/HTTP2 fingerprint,
// header order, and JS-runtime behavior, not just whether a valid
// cf_clearance cookie is attached. A cookie extracted from a real browser
// session does not transfer that score to a different client (even one
// using uTLS to impersonate the same browser's TLS ClientHello) -- so the
// previous solve-once-then-replay design could solve the challenge
// correctly and still get blocked on the very next request. Performing the
// actual data fetch inside the browser sidesteps that entirely: the
// response Cloudflare serves to targetURL is captured directly from the
// live page load, so there's nothing to fingerprint-mismatch.
//
// The browser is torn down completely before returning -- no browser
// process is left running between calls.
func browserFetch(parentCtx context.Context, cfg Config, targetURL string) (*browserFetchResult, error) {
	browserSemaphore <- struct{}{}
	defer func() { <-browserSemaphore }()

	timeoutCtx, cancelTimeout := context.WithTimeout(parentCtx, cfg.BrowserTimeout)
	defer cancelTimeout()

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", "new"),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true), // avoid /dev/shm exhaustion in small containers
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("no-sandbox", true), // required when running as root in a minimal container
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.WindowSize(1280, 800),
	)
	if cfg.ChromePath != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(cfg.ChromePath))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(timeoutCtx, allocOpts...)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	var (
		mu        sync.Mutex
		reqID     network.RequestID
		respFound bool
		status    int64
		hdrs      map[string]interface{}
		mimeType  string
	)

	// Track every response seen for the exact target URL and keep
	// overwriting -- Cloudflare's JS challenge, when present, works by
	// having the browser reload/re-request the same URL after solving, so
	// the LAST response received for targetURL (not the first, which may
	// be the challenge interstitial itself) is the one we want.
	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		e, ok := ev.(*network.EventResponseReceived)
		if !ok || e.Response == nil || e.Response.URL != targetURL {
			return
		}
		mu.Lock()
		reqID = e.RequestID
		respFound = true
		status = e.Response.Status
		hdrs = e.Response.Headers
		mimeType = e.Response.MimeType
		mu.Unlock()
	})

	var cookies []*network.Cookie
	var userAgent string

	err := chromedp.Run(browserCtx,
		network.Enable(),
		chromedp.Navigate(targetURL),
		// Give Cloudflare's JS challenge (if one fires) time to run and
		// redirect/reload to the final response. A fixed sleep is simpler
		// and more robust here than polling for a specific DOM element,
		// since the challenge page's markup changes over time.
		chromedp.Sleep(6*time.Second),
		chromedp.ActionFunc(func(ctx context.Context) error {
			u, err := url.Parse(targetURL)
			if err != nil {
				return err
			}
			cks, err := network.GetCookies().WithUrls([]string{u.Scheme + "://" + u.Host}).Do(ctx)
			if err != nil {
				return err
			}
			cookies = cks
			return nil
		}),
		chromedp.Evaluate(`navigator.userAgent`, &userAgent),
	)
	if err != nil {
		return nil, fmt.Errorf("browser fetch failed: %w", err)
	}

	mu.Lock()
	finalReqID, finalOK, finalStatus, finalHeaders, finalMime := reqID, respFound, status, hdrs, mimeType
	mu.Unlock()

	if !finalOK {
		return nil, fmt.Errorf("browser fetch: no network response observed for %s", targetURL)
	}

	var bodyStr string
	err = chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		b, err := network.GetResponseBody(finalReqID).Do(ctx)
		if err != nil {
			return err
		}
		bodyStr = string(b)
		return nil
	}))
	if err != nil {
		return nil, fmt.Errorf("browser fetch: reading response body for %s: %w", targetURL, err)
	}

	header := http.Header{}
	for k, v := range finalHeaders {
		if s, ok := v.(string); ok {
			header.Set(textproto.CanonicalMIMEHeaderKey(k), s)
		}
	}
	if header.Get("Content-Type") == "" && finalMime != "" {
		header.Set("Content-Type", finalMime)
	}

	httpCookies := make([]*http.Cookie, 0, len(cookies))
	for _, c := range cookies {
		httpCookies = append(httpCookies, &http.Cookie{Name: c.Name, Value: c.Value})
	}

	return &browserFetchResult{
		status:  int(finalStatus),
		header:  header,
		body:    []byte(bodyStr),
		session: &session{cookies: httpCookies, userAgent: userAgent},
	}, nil
}
