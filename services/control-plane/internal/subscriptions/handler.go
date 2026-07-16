// Package subscriptions serves the read-only subscription-status surface the
// Settings ▸ Account subscription page (#1304) consumes. The entitlement store
// (#1297) carries only the resolved tier; the status view additionally needs
// the grant's status/source/expiry to render "Supersonic — active · Redeemed
// via Kickstarter code · expires …". This package owns that one read-only
// GET /api/v1/subscriptions/me handler.
//
// It does NOT own the subscriptions source of truth (that is the redemption
// engine + the future Stripe webhook, #2033) and never writes the table.
package subscriptions

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
)

// statusNone is the DTO status when the user has no live subscription row — the
// free-default shape. Distinct from the DB statuses ('active'|'trialing'|…) so
// the client can render the free state without inferring it from a missing tier.
const statusNone = "none"

// StatusDTO is the wire shape of GET /api/v1/subscriptions/me. Snake-case JSON
// matches the sibling redemption/subscription contracts (#1303). The expiry is
// omitempty: kickstarter/code-sourced Beta grants and the free-default shape
// have no period end, so the field is simply absent rather than null.
type StatusDTO struct {
	Tier             string  `json:"tier"`
	Status           string  `json:"status"`
	Source           string  `json:"source,omitempty"`
	CurrentPeriodEnd *string `json:"current_period_end,omitempty"`
}

// freeDefault is the fail-closed shape returned when there is no live row OR any
// DB error occurs — never a fabricated premium (least privilege; mirrors
// entitlements.ResolveTier's degrade-don't-fail-open posture).
func freeDefault() StatusDTO {
	return StatusDTO{Tier: entitlements.TierFree, Status: statusNone}
}

// Handler serves the subscription-status read. Built with the standard
// handler-struct DI pattern (db + log). It holds no cache: the status read is
// low-frequency (page mount + post-redeem refresh) and the single-row indexed
// lookup is cheap, so a direct query avoids a second cache surface to keep in
// sync with the subscriptions table.
type Handler struct {
	db  *sql.DB
	log *logger.Logger
}

// NewHandler builds the subscription-status handler.
func NewHandler(db *sql.DB, log *logger.Logger) *Handler {
	return &Handler{db: db, log: log}
}

// GetMe handles GET /api/v1/subscriptions/me. It returns the acting user's live
// subscription status (tier/status/source/expiry) or the free-default shape when
// no live row exists. It always returns 200 with a valid shape — a DB error
// degrades to the free default (never a 500 that would leave the page unable to
// render, and never a fabricated premium).
//
// The query mirrors entitlements.ResolveTier's live-row predicate exactly, so
// the status view and the tier the enforcement path resolves can never disagree.
// The partial unique index idx_subscriptions_user_active guarantees at most one
// matching row, so LIMIT 1 is exact.
func (h *Handler) GetMe(c *gin.Context) {
	userID := c.GetString("user_id")

	const q = `SELECT tier, status, source, current_period_end FROM subscriptions
	           WHERE user_id = $1
	             AND status IN ('active', 'trialing', 'past_due')
	             AND (current_period_end IS NULL OR current_period_end > NOW())
	           LIMIT 1`

	var (
		tier, status, source string
		periodEnd            sql.NullTime
	)
	err := h.db.QueryRowContext(c.Request.Context(), q, userID).
		Scan(&tier, &status, &source, &periodEnd)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusOK, freeDefault())
		return
	}
	if err != nil {
		// Degrade, don't fail open. %q quoting is injection-safe for the userID
		// (observability.md: %q-escaped values need no further sanitization). No
		// row values are logged — PII-safe.
		h.log.Error("subscriptions: status read failed", "user_id", sanitizeID(userID), "error", err.Error())
		c.JSON(http.StatusOK, freeDefault())
		return
	}

	dto := StatusDTO{Tier: tier, Status: status, Source: source}
	if periodEnd.Valid {
		iso := periodEnd.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		dto.CurrentPeriodEnd = &iso
	}
	c.JSON(http.StatusOK, dto)
}
