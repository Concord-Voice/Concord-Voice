package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// Logger returns a gin.HandlerFunc that logs requests
func Logger(log *logger.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
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
