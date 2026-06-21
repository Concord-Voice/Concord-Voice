package middleware

import (
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RequestIDHeader is the HTTP header name used for request ID propagation.
const RequestIDHeader = "X-Request-ID"

// RequestIDContextKey is the Gin context key where the request ID is stored.
const RequestIDContextKey = "request_id"

// requestIDMaxLen is the maximum accepted length for a client-supplied request ID.
// nginx's $request_id is 32 hex chars; UUID is 36. 128 covers any reasonable format.
const requestIDMaxLen = 128

// RequestID returns middleware that ensures every request carries an X-Request-ID.
// If the client or upstream proxy (e.g. nginx) already set the header and it passes
// validation (≤128 chars, no control characters), it is reused. Otherwise a new
// UUID v4 is generated. The ID is stored in the Gin context and echoed back in the
// response header for client-side correlation.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if !isValidRequestID(id) {
			id = uuid.New().String()
		}

		c.Set(RequestIDContextKey, id)
		c.Header(RequestIDHeader, id)
		c.Next()
	}
}

// isValidRequestID checks that a client-supplied request ID is non-empty,
// within the max length, and contains no control characters.
func isValidRequestID(id string) bool {
	if id == "" || len(id) > requestIDMaxLen {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
