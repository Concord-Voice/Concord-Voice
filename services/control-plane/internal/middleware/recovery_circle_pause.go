package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// RecoveryCirclePaused stops public social recovery while preserving stored configuration.
func RecoveryCirclePaused(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"error":      "Recovery Circle recovery is temporarily paused. Use another recovery method.",
		"error_code": "recovery_circle_paused",
	})
}
