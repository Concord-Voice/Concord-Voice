package middleware

import (
	"github.com/gin-gonic/gin"
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
	c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	c.Writer.Header().Set("Access-Control-Max-Age", "86400")
}
