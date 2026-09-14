package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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

// solveChallenge launches a short-lived headless browser to solve
// Cloudflare's JS challenge for targetURL's domain, extracts the resulting
// cookies (cf_clearance, in particular) and the User-Agent used, and then
// tears the browser down completely before returning. No browser process is
// left running between calls -- this function's entire cost (CPU, memory,
// disk) exists only for the duration of a single solve.
func solveChallenge(parentCtx context.Context, cfg Config, targetURL string) (*session, error) {
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

	var cookies []*network.Cookie
	var userAgent string

	err := chromedp.Run(browserCtx,
		network.Enable(),
		chromedp.Navigate(targetURL),
		// Give Cloudflare's JS challenge time to run and redirect/reload.
		// A fixed sleep is simpler and more robust here than polling for a
		// specific DOM element, since the challenge page's markup changes
		// over time and we only care about the resulting cookies.
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
		return nil, fmt.Errorf("browser challenge solve failed: %w", err)
	}

	var found bool
	httpCookies := make([]*http.Cookie, 0, len(cookies))
	for _, c := range cookies {
		httpCookies = append(httpCookies, &http.Cookie{Name: c.Name, Value: c.Value})
		if c.Name == "cf_clearance" {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("no cf_clearance cookie obtained for %s (challenge may require additional interaction)", targetURL)
	}

	return &session{cookies: httpCookies, userAgent: userAgent}, nil
}
