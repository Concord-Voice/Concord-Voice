package middleware

import (
	"net/http"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/clientversion"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/gin-gonic/gin"
)

// ClientVersionHeader identifies the authenticated client's version.
const ClientVersionHeader = "X-Concord-Client-Version"

// ClientPlatformHeader names a mobile client's platform ("ios" or "android").
// Desktop sends none, so an absent value selects the desktop minimum.
const ClientPlatformHeader = "X-Concord-Client-Platform"

// ClientMinimums holds the per-platform minimum client versions. An empty
// value disables the check for that platform.
type ClientMinimums struct {
	Desktop string
	IOS     string
	Android string
}

// ClientMinimumsFromConfig reads the three minimums from the server config.
func ClientMinimumsFromConfig(cfg *config.Config) ClientMinimums {
	return ClientMinimums{Desktop: cfg.ClientMinVersion, IOS: cfg.ClientMinVersionIOS, Android: cfg.ClientMinVersionAndroid}
}

// forPlatform returns the minimum for a platform, and false for an unknown one.
func (m ClientMinimums) forPlatform(platform string) (string, bool) {
	switch platform {
	case "":
		return m.Desktop, true
	case "ios":
		return m.IOS, true
	case "android":
		return m.Android, true
	default:
		return "", false
	}
}

// RequireClientVersion rejects clients older than their platform's minimum.
func RequireClientVersion(minimums ClientMinimums) gin.HandlerFunc {
	return func(c *gin.Context) {
		// An authenticated media-plane hop is exempt: it is a service
		// re-validating on behalf of a user whose own client already passed
		// this gate on the same operation. See MediaPlaneServiceHop.
		if IsMediaPlaneServiceHop(c) {
			c.Next()
			return
		}
		if !EnforceClientVersion(c, minimums, c.GetHeader(ClientPlatformHeader), c.GetHeader(ClientVersionHeader)) {
			return
		}
		c.Next()
	}
}

// EnforceClientVersion applies the platform's minimum-version policy and
// returns whether the request may continue. An unknown platform is refused
// even when no minimum is set.
func EnforceClientVersion(c *gin.Context, minimums ClientMinimums, platform, supplied string) bool {
	minimum, known := minimums.forPlatform(platform)
	if !known {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Unsupported client platform"})
		return false
	}
	if minimum == "" {
		return true
	}

	minimumVersion, minimumErr := clientversion.Parse(minimum)
	suppliedVersion, suppliedErr := clientversion.ParseClient(supplied)
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
