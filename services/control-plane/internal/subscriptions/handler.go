// Package subscriptions serves the read-only subscription-status surface the
// Settings ▸ Account subscription page (#1304) consumes. The entitlement store
// (#1297) carries only the resolved tier; the status view additionally needs
// the grant's status/source/expiry to render "Supersonic — active · Redeemed
// via code · expires …". This package owns that one read-only
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
// omitempty: code-sourced grants and the free-default shape may have no period
// end, so the field is simply absent rather than null.
type StatusDTO struct {
	Tier             string  `json:"tier"`
	Status           string  `json:"status"`
	Source           string  `json:"source,omitempty"`
	CurrentPeriodEnd *string `json:"current_period_end,omitempty"`
}

// freeDefault is returned when there is no live subscription row. A failed
// lookup cannot confirm this status and returns an unavailable response instead.
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

// getMeQuery reads the row whose values fill the legacy top-level status
// fields. Liveness is entitlements.LiveRowPredicate, the per-source rule tier
// enforcement uses. Since migration 000179 a user can hold one live code row
// and live Stripe rows at once, so the order is explicit: a Stripe row before a
// code row (#2033 spec §5; PR-5 puts the seat between them), then the latest
// period end (a NULL end, which never lapses, sorts first), then id so the pick
// is deterministic. Both operands of the concatenation are constants.
const getMeQuery = `SELECT tier, status, source, current_period_end
	  FROM subscriptions
	 WHERE user_id = $1 AND ` + entitlements.LiveRowPredicate + `
	 ORDER BY CASE WHEN source = 'stripe' THEN 0 ELSE 1 END,
	          current_period_end DESC NULLS FIRST,
	          id
	 LIMIT 1`

// GetMe handles GET /api/v1/subscriptions/me. It returns the acting user's live
// subscription status (tier/status/source/expiry) from the highest-precedence
// live row (see getMeQuery), or the free-default shape when no live row exists.
// Query or scan errors return 503 without confirming a tier.
func (h *Handler) GetMe(c *gin.Context) {
	userID := c.GetString("user_id")

	var (
		tier, status, source string
		periodEnd            sql.NullTime
	)
	err := h.db.QueryRowContext(c.Request.Context(), getMeQuery, userID).
		Scan(&tier, &status, &source, &periodEnd)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusOK, freeDefault())
		return
	}
	if err != nil {
		// Report unavailability without confirming a tier. The user id passes
		// through sanitizeID (CWE-117) and no row values are logged (PII-safe).
		h.log.Error("subscriptions: status read failed", "user_id", sanitizeID(userID), "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "subscription status unavailable"})
		return
	}

	dto := StatusDTO{Tier: tier, Status: status, Source: source}
	if periodEnd.Valid {
		iso := periodEnd.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		dto.CurrentPeriodEnd = &iso
	}
	c.JSON(http.StatusOK, dto)
}
