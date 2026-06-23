package middleware

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
)

const accessHeader = "Cf-Access-Jwt-Assertion"
const ctxAccessEmail = "cf_access_email"

var errAbsent = errors.New("absent")

// RequireCloudflareAccess rejects admin requests without a valid Cloudflare Access JWT.
func RequireCloudflareAccess(v *accessVerifier, log *slog.Logger) gin.HandlerFunc {
	if log == nil {
		log = slog.Default()
	}
	return func(c *gin.Context) {
		raw := c.GetHeader(accessHeader)
		if raw == "" {
			denyAccess(c, log, errAbsent)
			return
		}
		claims, err := v.Verify(raw)
		if err != nil {
			denyAccess(c, log, err)
			return
		}
		c.Set(ctxAccessEmail, claims.Email)
		c.Next()
	}
}

func denyAccess(c *gin.Context, log *slog.Logger, reason error) {
	reasonText := "invalid"
	if reason != nil {
		reasonText = reason.Error()
	}
	log.Warn("admin: cf-access assertion rejected", "reason", reasonText)
	c.AbortWithStatus(http.StatusForbidden)
}

// RequireCloudflareAccessFromConfig builds the Cloudflare Access gate from runtime config.
func RequireCloudflareAccessFromConfig(cfg *config.Config, log *slog.Logger) gin.HandlerFunc {
	return RequireCloudflareAccess(newAccessVerifier(cfg.CFAccessTeamDomain, cfg.CFAccessAUD), log)
}
