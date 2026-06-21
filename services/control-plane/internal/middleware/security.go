// Package middleware provides HTTP middleware for authentication, rate limiting, and CORS.
package middleware

import "github.com/gin-gonic/gin"

// SecurityHeaders adds standard security headers to every response.
// When env is "production", HSTS is included to enforce HTTPS.
func SecurityHeaders(env string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Content-Security-Policy", "default-src 'none'")
		if env == "production" {
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		c.Next()
	}
}
