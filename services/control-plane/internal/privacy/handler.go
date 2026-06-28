// Package privacy provides a GDPR Article 17 account-erasure handler.
package privacy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// Handler exposes the privacy erasure endpoints.
type Handler struct {
	account users.AccountDeleter
	redis   *redis.Client
	log     *logger.Logger
}

// NewHandler builds a Handler wired to the given account-deletion service.
// Panics if account is nil — every method dereferences it unconditionally;
// a nil value would surface as an opaque nil-pointer panic on the first
// request. Panicking at construction makes mis-wiring obvious at startup.
//
// A nil redis client IS allowed in direct unit tests that do not exercise
// current-token revocation; production callers must always pass a non-nil
// client. A nil logger IS allowed in tests that do not exercise the failure
// path; production callers must always pass a non-nil logger. The log field is
// only dereferenced behind a nil-check on the error path.
func NewHandler(account users.AccountDeleter, redisClient *redis.Client, log *logger.Logger) *Handler {
	if account == nil {
		panic("privacy.NewHandler: account AccountDeleter must not be nil")
	}
	return &Handler{account: account, redis: redisClient, log: log}
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

	if err := h.revokeAccessTokens(c, userID); err != nil {
		if h.log != nil {
			h.log.Error("erase-account: access token revocation failed",
				"user_id", userID,
				"error", err,
			)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "access token revocation failed"})
		return
	}

	c.Status(http.StatusNoContent)
}

func (h *Handler) revokeAccessTokens(c *gin.Context, userID string) error {
	if h.redis == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	if err := h.redis.Set(ctx, middleware.UserDisabledKey(userID), "1", auth.AccessTokenTTL).Err(); err != nil {
		return fmt.Errorf("denylist erased user: %w", err)
	}

	rawClaims, ok := c.Get(middleware.JWTClaimsContextKey)
	if !ok {
		return nil
	}
	claims, ok := rawClaims.(jwt.MapClaims)
	if !ok {
		return errors.New("jwt claims have unexpected type")
	}

	jti, ok := claims["jti"].(string)
	if !ok || jti == "" {
		return errors.New("jwt claims missing jti")
	}
	expiresAt, err := claims.GetExpirationTime()
	if err != nil {
		return fmt.Errorf("read jwt expiration: %w", err)
	}
	if expiresAt == nil {
		return errors.New("jwt claims missing expiration")
	}

	if err := middleware.BlacklistToken(ctx, h.redis, jti, time.Until(expiresAt.Time)); err != nil {
		return fmt.Errorf("blacklist current access token: %w", err)
	}
	return nil
}
