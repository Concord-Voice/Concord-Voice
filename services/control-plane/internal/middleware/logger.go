package middleware

import (
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
)

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
		// FullPath is empty only when no route matched, i.e. a 404 — there is no
		// pattern to log and no route parameter was ever bound. The raw fallback
		// keeps 404s diagnosable. Residual, stated rather than hidden: a 404 whose
		// URL happens to contain a code-shaped string still logs it. That request
		// matched nothing and the code was never accepted, so it is not a
		// disclosure of a code in use; the trailing-slash 301 does not reach here
		// at all, because gin redirects from the tree walk before the chain runs.
		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}
		method := c.Request.Method

		c.Next()

		duration := time.Since(start)
		status := c.Writer.Status()

		fields := []any{
			"method", method,
			"path", path,
			"status", status,
			"duration", duration,
			"ip", c.ClientIP(),
		}
		if reqID, exists := c.Get(RequestIDContextKey); exists {
			fields = append(fields, "request_id", reqID)
		}

		log.Info("HTTP Request", fields...)
	}
}
