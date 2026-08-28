package middleware

import (
	"github.com/gin-gonic/gin"
)

const (
	// SessionIssuedHeader marks responses that created a new refresh session.
	SessionIssuedHeader = "X-Concord-Session-Issued"
	// SessionIDHeader identifies that exact refresh session for safe cleanup.
	SessionIDHeader = "X-Concord-Session-ID"
	// RateLimitResetHeader lets browser clients schedule a bounded retry.
	RateLimitResetHeader = "X-RateLimit-Reset"
	// RetryAfterHeader tells browser clients when a rate-limited request can retry.
	RetryAfterHeader = "Retry-After"
	// RateLimitLimitHeader names WHICH limiter answered, which a client needs in
	// order to scope its retry: this API answers 429 from both a per-user route
	// budget and per-resource budgets, and they have different blast radii.
	// Non-safelisted, so a cross-origin renderer reads null without this.
	RateLimitLimitHeader = "X-RateLimit-Limit"
	// FileMimeTypeHeader carries an attachment's original MIME type. The body is
	// opaque ciphertext, so this is the only place the real type survives.
	FileMimeTypeHeader = "X-File-Mime-Type"
	// FileKeyVersionHeader carries the CSK epoch an attachment was sealed under.
	// Without it a cross-origin renderer decrypts with the CURRENT key, which
	// fails authentication on everything older than the last rotation -- and
	// reports it as tampering.
	FileKeyVersionHeader = "X-File-Key-Version"
)

// CORS returns a middleware that handles CORS.
// CORS headers are only set when the request includes a non-empty Origin
// that matches the allowlist. Requests without an Origin header (native
// clients, curl) pass through without CORS headers — browsers will simply
// not treat them as cross-origin. The "null" string origin (sandboxed
// iframes, data: URLs) is never reflected to prevent credential leaks.
func CORS(allowedOrigins []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")

		if origin != "" && origin != "null" && isOriginAllowed(origin, allowedOrigins) {
			setCORSHeaders(c, origin)
		}

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

func isOriginAllowed(origin string, allowedOrigins []string) bool {
	for _, ao := range allowedOrigins {
		if ao == "*" || origin == ao {
			return true
		}
	}
	return false
}

func setCORSHeaders(c *gin.Context, origin string) {
	c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
	c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
	c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, X-Refresh-Token, X-Session-ID, X-Machine-Id, X-Device-Name, X-Request-ID, X-Attestation-Token")
	// Both attachment headers are exposed: a cross-origin renderer (the remote
	// SPA mode) cannot READ a response header that is not on this list, and both
	// carry metadata that only exists here because the body is ciphertext.
	c.Writer.Header().Set("Access-Control-Expose-Headers",
		SessionIssuedHeader+", "+SessionIDHeader+", "+RateLimitResetHeader+", "+
			RateLimitLimitHeader+", "+RetryAfterHeader+", "+FileMimeTypeHeader+", "+
			FileKeyVersionHeader)
	c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	c.Writer.Header().Set("Access-Control-Max-Age", "86400")
}
