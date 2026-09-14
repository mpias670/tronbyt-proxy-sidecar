package main

import "strings"

// isDomainAllowed reports whether host is exactly one of allowed, or a
// subdomain of one of them. Comparison is case-insensitive; a trailing dot
// on host (fully-qualified domain name notation) is ignored.
func isDomainAllowed(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, domain := range allowed {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}
