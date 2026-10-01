package middleware

import (
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
)

// unmatchedRoutePath stands in for the path of a request that matched no route.
const unmatchedRoutePath = "<unmatched>"

// Logger returns a gin.HandlerFunc that logs requests
func Logger(log *logger.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		// The CONCRETE path is the credential on every bearer-code route
		// (/api/v1/invites/:code/preview, /api/v1/friends/codes/:code/preview and
		// /avatar): the code IS the bearer material, so logging URL.Path writes it
		// to stdout on 100% of requests (CWE-532). FullPath is the matched route
		// PATTERN — ":code" stays literal — which keeps every operational use of
		// this field (routing, latency-by-endpoint, status distribution) while
		// dropping the secret.
		//
		//
		// FullPath is empty when no route matched, and that is NOT only a refused
		// 404: CORS answers every preflight 204 before routing matters, and the
		// desktop client preflights each invite and friend-code call, so an
		// unmatched path carries a code in use (PR #3541, @red-team PoC). Log a
		// constant instead; a 404 is still visible by method and status.
		path := c.FullPath()
		if path == "" {
			path = unmatchedRoutePath
		}
		method := c.Request.Method

		c.Next()

		duration := time.Since(start)
		status := c.Writer.Status()

		// Constant keys at the call, never a spread []any: the log-injection
		// query can prove a value safe only when it sees the key beside it.
		// RequestID is mounted on the engine and sets the key before any handler
		// returns, and this reads it after c.Next(), so request_id is always set.
		log.Info("HTTP Request",
			"method", method,
			"path", path,
			"status", status,
			"duration", duration,
			"ip", c.ClientIP(), // kept on purpose: abuse forensics and per-IP ban correlation need it (PR #3541)
			"request_id", c.GetString(RequestIDContextKey),
		)
	}
}
