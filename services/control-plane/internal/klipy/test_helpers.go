package klipy

import (
	"net/http"
	"net/netip"
	"regexp"
)

// SetAPIBaseForTest swaps the upstream KLIPY API base URL for testing.
// Returns the previous value so callers can restore it. Production code never
// invokes this — it exists solely so handlers_test.go can route requests to
// an httptest server.
func SetAPIBaseForTest(newBase string) string {
	old := klipyAPIBase
	klipyAPIBase = newBase
	return old
}

// GetAPIBaseForTest returns the current upstream API base URL.
// Production code never invokes this — see SetAPIBaseForTest.
func GetAPIBaseForTest() string {
	return klipyAPIBase
}

// SetMediaHostsForTest swaps the media-host whitelist regex for testing.
// Returns the previous value. Production code never invokes this — see
// SetAPIBaseForTest.
func SetMediaHostsForTest(newPattern *regexp.Regexp) *regexp.Regexp {
	old := allowedMediaHosts
	allowedMediaHosts = newPattern
	return old
}

// SetHTTPClientForTest replaces a Handler's internal http.Client with the
// supplied client. Used by tests that need to skip TLS verification when
// hitting an httptest.NewTLSServer (which serves a self-signed cert).
// Production code never invokes this.
func SetHTTPClientForTest(h *Handler, client *http.Client) {
	h.client = client
}

// SetMediaClientForTest replaces a Handler's internal mediaClient with the
// supplied client. Needed by media-proxy tests so the long-timeout media
// client also routes to the httptest server.
// Production code never invokes this.
func SetMediaClientForTest(h *Handler, client *http.Client) {
	h.mediaClient = client
}

// SetEgressGuardForTest swaps the dial-time egress predicate, returning a
// restore func. Tests use it to dial httptest servers (which bind 127.0.0.1,
// otherwise denied by isDeniedEgressIP). Production code never invokes this.
func SetEgressGuardForTest(fn func(netip.Addr) bool) func() {
	old := egressGuard
	egressGuard = fn
	return func() { egressGuard = old }
}
