// Package middleware provides HTTP middleware for authentication, rate limiting, and CORS.
package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// DefaultHSTSHeaderValue is the Strict-Transport-Security policy emitted when
// HSTS_HEADER_VALUE is unset. Consolidates on the stronger pre-#2318 nginx-side
// intent (the Go/nginx values used to diverge, and per RFC 6797 §8.1 only the
// FIRST STS header is processed — the weaker Go value silently won).
// NOTE: `preload` here only advertises eligibility; submitting the domain to
// the browser preload list is a separate, deliberate operator action.
const DefaultHSTSHeaderValue = "max-age=63072000; includeSubDomains; preload"

// SecurityHeaderSet builds the full security-header set as an http.Header.
// It exists as a separate seam because gorilla/websocket's Upgrader.Upgrade
// hijacks the connection and writes the 101 handshake from its explicit
// responseHeader argument — headers already set on the gin writer are NOT
// serialized onto the 101 (#2318 review). The WebSocket handler passes this
// set into Upgrade so handshake responses carry the same headers as every
// other Go-owned response.
func SecurityHeaderSet(env, hstsValue string) http.Header {
	if hstsValue == "" {
		hstsValue = DefaultHSTSHeaderValue
	}
	h := http.Header{}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	// Explicit 0: the XSS auditor is removed from modern engines and
	// "1; mode=block" is an XS-leak vector in legacy ones (OWASP guidance).
	h.Set("X-XSS-Protection", "0")
	// camera/microphone allowed for the app's own origin so getUserMedia()
	// works for voice/video calls if a document surface is ever served;
	// other capabilities remain denied — Concord does not currently use them.
	h.Set("Permissions-Policy", "camera=(self), microphone=(self), geolocation=(), payment=(), usb=(), magnetometer=(), gyroscope=(), accelerometer=()")
	if env == "production" {
		h.Set("Strict-Transport-Security", hstsValue)
	}
	return h
}

// SecurityHeaders adds the full security-header set to every response.
// Post-#2318 this middleware (plus the SecurityHeaderSet seam above for
// WebSocket 101 handshakes) is the SINGLE owner of these headers for the API
// surface across all deployment modes (SaaS, self-hosted, docker failover,
// bare-Go dev) — do not re-add duplicates to an nginx layer in front.
// When env is "production", HSTS is included to enforce HTTPS; hstsValue
// overrides the default policy (empty = default).
func SecurityHeaders(env, hstsValue string) gin.HandlerFunc {
	set := SecurityHeaderSet(env, hstsValue)
	return func(c *gin.Context) {
		for k := range set {
			c.Header(k, set.Get(k))
		}
		c.Next()
	}
}
