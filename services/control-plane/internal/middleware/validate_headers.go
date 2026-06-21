package middleware

import (
	"net/http"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const maxDeviceNameLen = 255

// ValidateCustomHeaders rejects requests that carry malformed
// X-Machine-Id or X-Device-Name headers before they reach handlers.
// Both headers are optional — only validated when present.
func ValidateCustomHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		if mid := c.GetHeader("X-Machine-Id"); mid != "" {
			if _, err := uuid.Parse(mid); err != nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error": "X-Machine-Id must be a valid UUID",
				})
				return
			}
		}

		if dn := c.GetHeader("X-Device-Name"); dn != "" {
			if err := validateDeviceName(dn); err != "" {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err})
				return
			}
		}

		c.Next()
	}
}

// validateDeviceName returns an error message if the device name is invalid,
// or an empty string if valid.
func validateDeviceName(dn string) string {
	if !utf8.ValidString(dn) {
		return "X-Device-Name must be valid UTF-8"
	}
	if utf8.RuneCountInString(dn) > maxDeviceNameLen {
		return "X-Device-Name must not exceed 255 characters"
	}
	for _, r := range dn {
		if unicode.IsControl(r) {
			return "X-Device-Name must not contain control characters"
		}
	}
	return ""
}
