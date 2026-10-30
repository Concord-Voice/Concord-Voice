package middleware

import (
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"
)

// RequireSessionHeaderMatchesToken refuses a request whose X-Session-ID is not
// the access token's own sid (RT-F2). Attestation tokens are stored under
// attestation:<X-Session-ID>:<X-Machine-Id>, so an unbound header would let one
// user overwrite another user's slot. Legacy tokens without a sid pass.
func RequireSessionHeaderMatchesToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		sid := TokenSessionID(c)
		if sid != "" && subtle.ConstantTimeCompare([]byte(sid), []byte(c.GetHeader("X-Session-ID"))) != 1 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "X-Session-ID does not match the access token"})
			return
		}
		c.Next()
	}
}
