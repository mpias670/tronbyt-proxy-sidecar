package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
)

// browserUserAgent is sent alongside the uTLS ClientHello fingerprint below
// so both layers of fingerprinting (TLS handshake + HTTP headers) agree on
// "recent desktop Chrome", which is what most Cloudflare bot-management
// heuristics actually check.
const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// newImpersonatingClient returns an http.Client whose TLS handshake mimics a
// real Chrome browser (via uTLS's ClientHelloID fingerprint database)
// instead of Go's easily-fingerprinted default `crypto/tls` handshake. This
// alone defeats a meaningful fraction of Cloudflare's bot-management
// heuristics without needing a browser at all, which is why it's always
// tried before falling back to the (expensive) headless-browser solve.
//
// Limitation: this dials plain HTTP/1.1 over the uTLS connection. Servers
// that hard-require HTTP/2 would need a fuller implementation layering
// golang.org/x/net/http2 on top of the uTLS conn.
func newImpersonatingClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}

	transport := &http.Transport{
		DialContext: dialer.DialContext,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			rawConn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}

			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}

			// NextProtos is pinned to HTTP/1.1 only: the uTLS ClientHello
			// fingerprint (JA3) still matches Chrome, but this avoids the
			// server negotiating an h2 ALPN that our plain http.Transport
			// (layered directly on this conn, with no HTTP/2 framing
			// support) can't actually speak.
			uConn := utls.UClient(rawConn, &utls.Config{ServerName: host}, utls.HelloChrome_Auto)

			// HelloChrome_Auto's built-in ALPN extension advertises h2, but
			// this Transport speaks HTTP/1.1 framing only. Build the
			// ClientHello first, then pin the ALPN extension down to
			// http/1.1 so the server won't negotiate h2 out from under us.
			// The JA3 fingerprint (cipher suites/extension order) is
			// unaffected -- only the advertised protocol list changes.
			if err := uConn.BuildHandshakeState(); err != nil {
				rawConn.Close()
				return nil, err
			}
			for _, ext := range uConn.Extensions {
				if alpn, ok := ext.(*utls.ALPNExtension); ok {
					alpn.AlpnProtocols = []string{"http/1.1"}
				}
			}
			if err := uConn.HandshakeContext(ctx); err != nil {
				rawConn.Close()
				return nil, err
			}
			return uConn, nil
		},
		ForceAttemptHTTP2:   false,
		MaxIdleConnsPerHost: 4,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// setBrowserHeaders sets a realistic Chrome header set on req, in the order
// and casing a real browser tends to send them. Cloudflare's heuristics
// look at header presence/order as well as the TLS fingerprint, so an
// otherwise-perfect uTLS handshake with a bare-bones header set (Go's
// default, or a hand-picked partial set) can still get flagged.
func setBrowserHeaders(req *http.Request, userAgent string) {
	if userAgent == "" {
		userAgent = browserUserAgent
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Sec-Ch-Ua", `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
}

// tlsVersionName is unused today but kept handy for debug logging while
// tuning the fingerprint against a real deployment.
func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS 1.3"
	case tls.VersionTLS12:
		return "TLS 1.2"
	default:
		return "unknown"
	}
}
