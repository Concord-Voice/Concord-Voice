// Package privacy provides a GDPR Article 17 account-erasure handler.
package privacy

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// Handler exposes the privacy erasure endpoints.
type Handler struct {
	account users.AccountDeleter
	log     *logger.Logger
}

// NewHandler builds a Handler wired to the given account-deletion service.
// Panics if account is nil — every method dereferences it unconditionally;
// a nil value would surface as an opaque nil-pointer panic on the first
// request. Panicking at construction makes mis-wiring obvious at startup.
//
// A nil logger IS allowed in tests that do not exercise the failure path;
// production callers must always pass a non-nil logger. The log field is
// only dereferenced behind a nil-check on the error path.
func NewHandler(account users.AccountDeleter, log *logger.Logger) *Handler {
	if account == nil {
		panic("privacy.NewHandler: account AccountDeleter must not be nil")
	}
	return &Handler{account: account, log: log}
}

// eraseAccountRequest is intentionally empty. Unknown fields in the
// request body (e.g. legacy "clientId" sent by old desktop builds) are
// silently ignored — Gin's ShouldBindJSON does not reject unknown fields
// by default. This unblocks #757 (desktop strip): the desktop client may
// continue sending {"clientId":"..."} during the rollout window without
// server-side errors.
type eraseAccountRequest struct{}

// EraseAccount handles POST /api/v1/privacy/erase-account.
// Performs the atomic DB erasure via the AccountDeleter, which cascades
// through every user_id-FK table configured with ON DELETE CASCADE and
// inserts a privacy-safe audit row.
func (h *Handler) EraseAccount(c *gin.Context) {
	userID := c.GetString("user_id")
	// Defense-in-depth: auth middleware is expected to have populated user_id
	// before this handler runs. If a future routing change exposed this
	// endpoint behind optional auth, DeleteAccount would attempt a
	// DELETE WHERE id = '' and the audit row would be written without a
	// meaningful subject. Fail closed instead.
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var req eraseAccountRequest
	// An empty body (io.EOF from the JSON decoder) is acceptable — the
	// request struct has no required fields. Other JSON-shape errors are
	// rejected as 400.
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	if err := h.account.DeleteAccount(c.Request.Context(), userID); err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
			return
		}
		if h.log != nil {
			h.log.Error("erase-account: account deletion failed",
				"user_id", userID,
				"error", err,
			)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "account deletion failed"})
		return
	}

	c.Status(http.StatusNoContent)
}
