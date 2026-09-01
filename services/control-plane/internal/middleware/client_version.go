package middleware

import (
	"net/http"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/clientversion"
	"github.com/gin-gonic/gin"
)

// ClientVersionHeader identifies the authenticated client's stable release.
const ClientVersionHeader = "X-Concord-Client-Version"

// RequireClientVersion rejects clients older than the configured minimum.
func RequireClientVersion(minimum string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !EnforceClientVersion(c, minimum, c.GetHeader(ClientVersionHeader)) {
			return
		}
		c.Next()
	}
}

// EnforceClientVersion applies the minimum-version policy and returns whether
// the request may continue.
func EnforceClientVersion(c *gin.Context, minimum, supplied string) bool {
	if minimum == "" {
		return true
	}

	minimumVersion, minimumErr := clientversion.Parse(minimum)
	suppliedVersion, suppliedErr := clientversion.Parse(supplied)
	if minimumErr == nil && suppliedErr == nil && clientversion.Compare(suppliedVersion, minimumVersion) >= 0 {
		return true
	}

	c.AbortWithStatusJSON(http.StatusForbidden, attestation.ErrorResponse{
		Error:              "Client version too old",
		Code:               attestation.ErrVersionTooOld,
		UpdateAvailable:    true,
		RequiredMinVersion: minimum,
		DownloadHelpURL:    attestation.DownloadHelpURLDefault,
	})
	return false
}
