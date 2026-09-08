package middleware

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
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
	// All verifier sentinels intentionally collapse to one client and event
	// decision. Unknown errors are also denied, so malformed infrastructure
	// details never become part of the application event schema.
	log.Warn("admin: cf-access assertion rejected", "reason", cloudflareAccessFailure(reason))
	MarkNightwatchVerdict(c, NightwatchVerdict{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, Reason: securityevent.ReasonCloudflareAccessDenied})
	c.AbortWithStatus(http.StatusForbidden)
}

func cloudflareAccessFailure(_ error) string {
	return "cloudflare_access_denied"
}

// RequireCloudflareAccessFromConfig builds the Cloudflare Access gate from runtime config.
func RequireCloudflareAccessFromConfig(cfg *config.Config, log *slog.Logger) gin.HandlerFunc {
	return RequireCloudflareAccess(newAccessVerifier(cfg.CFAccessTeamDomain, cfg.CFAccessAUD), log)
}
