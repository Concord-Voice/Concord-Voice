package stepup

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// requirementsResponse is the GET /mfa/step-up body. DefaultMethod is a
// pointer with no omitempty so an account with no inline factor reads
// "default_method": null — Go would otherwise encode the empty choice as ""
// (spec §2, C3).
type requirementsResponse struct {
	Methods             []string `json:"methods"`
	DefaultMethod       *string  `json:"default_method"`
	BackupCodeAvailable bool     `json:"backup_code_available"`
}

// RequirementsHandler serves GET /api/v1/mfa/step-up: the caller's own P1
// factor set, the factor to offer first, and whether a backup code is
// spendable. Read-only; it answers 200, 401 (subject gone), or 500, and never
// 404 — a 404 on this path therefore means the server predates the route
// (spec §2). The chain in front of it supplies 401/403/429/503.
//
// Nothing it returns is logged: default_method and backup availability are
// recency-derived account posture ([internal]rules/observability.md principle 7).
func RequirementsHandler(q RowQuerier, log *logger.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		f, e := InlineMFAFactors(c.Request.Context(), q, c.GetString("user_id"))
		if e != nil {
			if e.Cause != nil {
				log.Error("Failed to read step-up requirements", "error", e.Cause)
			}
			e.Write(c)
			return
		}
		resp := requirementsResponse{Methods: f.Methods, BackupCodeAvailable: f.BackupCodeAvailable}
		if d := f.Default(); d != "" {
			resp.DefaultMethod = &d
		}
		c.JSON(http.StatusOK, resp)
	}
}

// DefaultMethodReader answers the login, refresh and SSO challenges' advisory
// default_method: the default restricted to what that challenge offers. It is
// handed to auth.Handler through a setter because internal/auth cannot import
// this package (stepup imports auth). Any error, a missing user included, is
// the caller's to log and treat as "omit the field".
func DefaultMethodReader(q RowQuerier) func(ctx context.Context, userID string, offered []string) (string, error) {
	return func(ctx context.Context, userID string, offered []string) (string, error) {
		f, e := InlineMFAFactors(ctx, q, userID)
		if e != nil {
			// The cause, when there is one, is what the caller's log needs:
			// Error() on a *Error prints only its status.
			if e.Cause != nil {
				return "", e.Cause
			}
			return "", e
		}
		return f.DefaultWithin(offered), nil
	}
}
