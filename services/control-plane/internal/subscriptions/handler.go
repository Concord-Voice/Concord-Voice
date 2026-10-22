// Package subscriptions serves the read-only subscription-status surface the
// Settings ▸ Account subscription page (#1304) consumes. The entitlement store
// (#1297) carries only the resolved tier; the status view additionally needs
// each live row's status/source/expiry (and, for a Stripe row, the plan,
// interval and cancellation) to render "Supersonic — active · Redeemed via
// code · expires …". The GET /api/v1/subscriptions/me handler never writes the
// table. The subscriptions source of truth is the redemption engine (code rows)
// and the Stripe webhook, reconcile and drift paths in internal/billing (Stripe
// rows, #2033). The expiry sweep in this package is the one writer here: it
// flips lapsed rows to 'expired'.
package subscriptions

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
)

// Billing states reported as "billing" on GET /api/v1/subscriptions/me.
const (
	// BillingSelfHosted: a self-hosted instance. Billing does not exist there.
	BillingSelfHosted = "self_hosted"
	// BillingNotConfigured: a hosted instance with no Stripe keys.
	BillingNotConfigured = "not_configured"
	// BillingAvailable: keys are present. Whether a plan can be bought right
	// now is the plans endpoint's answer, not this field's.
	BillingAvailable = "available"
)

// statusNone is the DTO status when the user has no live subscription row — the
// free-default shape. Distinct from the DB statuses ('active'|'trialing'|…) so
// the client can render the free state without inferring it from a missing tier.
const statusNone = "none"

// sourceStripe is the subscriptions.source of a Stripe-billed row.
const sourceStripe = "stripe"

// StatusDTO is the wire shape of GET /api/v1/subscriptions/me. Snake-case JSON
// matches the sibling redemption/subscription contracts (#1303). The expiry is
// omitempty: code-sourced grants and the free-default shape may have no period
// end, so the field is simply absent rather than null. Tier, Status, Source and
// CurrentPeriodEnd describe the row that enforcement honors (see getMeQuery);
// Stripe and Code describe the first live row of each source.
type StatusDTO struct {
	Tier             string     `json:"tier"`
	Status           string     `json:"status"`
	Source           string     `json:"source,omitempty"`
	CurrentPeriodEnd *string    `json:"current_period_end,omitempty"`
	Billing          string     `json:"billing"`
	Stripe           *StripeDTO `json:"stripe,omitempty"`
	Code             *CodeDTO   `json:"code,omitempty"`
}

// StripeDTO describes the user's live Stripe-sourced row.
type StripeDTO struct {
	Plan             *string `json:"plan"`     // null on a malformed row
	Interval         *string `json:"interval"` // null on a malformed row
	Status           string  `json:"status"`
	CurrentPeriodEnd string  `json:"current_period_end"`
	Canceling        bool    `json:"canceling"`
	EndsAt           *string `json:"ends_at,omitempty"`
	FoundersRate     bool    `json:"founders_rate"`
}

// CodeDTO describes the user's live code-sourced row.
type CodeDTO struct {
	CurrentPeriodEnd *string `json:"current_period_end"` // null: never lapses
}

// freeDefault is returned when there is no live subscription row. A failed
// lookup cannot confirm this status and returns an unavailable response instead.
func (h *Handler) freeDefault() StatusDTO {
	return StatusDTO{Tier: entitlements.TierFree, Status: statusNone, Billing: h.billing}
}

// Handler serves the subscription-status read. Built with the standard
// handler-struct DI pattern (db + log). It holds no cache: the status read is
// low-frequency (page mount + post-redeem refresh) and the indexed per-user
// lookup is cheap, so a direct query avoids a second cache surface to keep in
// sync with the subscriptions table.
type Handler struct {
	db      *sql.DB
	log     *logger.Logger
	billing string
}

// NewHandler builds the subscription-status handler. billingState is one of the
// Billing* constants and is echoed as "billing" on every response.
func NewHandler(db *sql.DB, log *logger.Logger, billingState string) *Handler {
	return &Handler{db: db, log: log, billing: billingState}
}

// getMeQuery reads every live row of the user. Liveness is
// entitlements.LiveRowPredicate, the per-source rule tier enforcement uses.
// Since migration 000179 a user can hold one live code row and live Stripe rows
// at once, so the order is explicit. The first row fills the legacy top-level
// fields and must be the row enforcement honors: a premium row before any other
// tier, then a Stripe row before a code row (#2033 spec §5; PR-5 puts the seat
// between them), then the latest period end (a NULL end, which never lapses,
// sorts first), then id so the pick is deterministic. Both operands of the
// concatenation are constants.
const getMeQuery = `SELECT tier, status, source, current_period_end,
	       plan, billing_interval, founders_rate, cancel_at_period_end, cancel_at
	  FROM subscriptions
	 WHERE user_id = $1 AND ` + entitlements.LiveRowPredicate + `
	 ORDER BY CASE WHEN tier = 'premium' THEN 0 ELSE 1 END,
	          CASE WHEN source = 'stripe' THEN 0 ELSE 1 END,
	          current_period_end DESC NULLS FIRST,
	          id`

// liveRow is one scanned row of getMeQuery.
type liveRow struct {
	tier, status, source string
	periodEnd            sql.NullTime
	plan, interval       sql.NullString
	foundersRate         bool
	cancelAtPeriodEnd    bool
	cancelAt             sql.NullTime
}

func (r *liveRow) scan(rows *sql.Rows) error {
	return rows.Scan(&r.tier, &r.status, &r.source, &r.periodEnd,
		&r.plan, &r.interval, &r.foundersRate, &r.cancelAtPeriodEnd, &r.cancelAt)
}

// rfc3339UTC formats t as an RFC 3339 UTC timestamp.
func rfc3339UTC(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// nullTimeString returns a pointer to the RFC 3339 form of t, or nil when t is NULL.
func nullTimeString(t sql.NullTime) *string {
	if !t.Valid {
		return nil
	}
	s := rfc3339UTC(t.Time)
	return &s
}

// nullStringPtr returns a pointer to the string in s, or nil when s is NULL.
func nullStringPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// stripeDTO builds the stripe object from a Stripe row. canceling is
// cancel_at_period_end OR cancel_at IS NOT NULL (Stripe's flexible cancellation
// sets cancel_at and leaves the flag false). ends_at is cancel_at, else the
// period end when cancel_at_period_end, else absent (spec §8).
func (r *liveRow) stripeDTO() *StripeDTO {
	dto := &StripeDTO{
		Plan:         nullStringPtr(r.plan),
		Interval:     nullStringPtr(r.interval),
		Status:       r.status,
		Canceling:    r.cancelAtPeriodEnd || r.cancelAt.Valid,
		FoundersRate: r.foundersRate,
	}
	if r.periodEnd.Valid {
		dto.CurrentPeriodEnd = rfc3339UTC(r.periodEnd.Time)
	}
	switch {
	case r.cancelAt.Valid:
		dto.EndsAt = nullTimeString(r.cancelAt)
	case r.cancelAtPeriodEnd:
		dto.EndsAt = nullTimeString(r.periodEnd)
	}
	return dto
}

// buildStatus folds the ordered live rows into the response: the first row
// fills the legacy fields, the first Stripe row the stripe object and the first
// code row the code object. No rows yields the free default.
func (h *Handler) buildStatus(live []liveRow) StatusDTO {
	if len(live) == 0 {
		return h.freeDefault()
	}
	first := live[0]
	dto := StatusDTO{
		Tier:             first.tier,
		Status:           first.status,
		Source:           first.source,
		CurrentPeriodEnd: nullTimeString(first.periodEnd),
		Billing:          h.billing,
	}
	for i := range live {
		switch live[i].source {
		case sourceStripe:
			if dto.Stripe == nil {
				dto.Stripe = live[i].stripeDTO()
			}
		default:
			if dto.Code == nil {
				dto.Code = &CodeDTO{CurrentPeriodEnd: nullTimeString(live[i].periodEnd)}
			}
		}
	}
	return dto
}

// readLiveRows runs getMeQuery and scans every row. Any query, scan or
// iteration error is returned; the caller must not confirm a tier on error.
func (h *Handler) readLiveRows(ctx context.Context, userID string) ([]liveRow, error) {
	rows, err := h.db.QueryContext(ctx, getMeQuery, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var live []liveRow
	for rows.Next() {
		var r liveRow
		if err := r.scan(rows); err != nil {
			return nil, err
		}
		live = append(live, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return live, nil
}

// GetMe handles GET /api/v1/subscriptions/me. It returns the acting user's live
// subscription status: the legacy top-level fields describe the row enforcement
// honors (see getMeQuery), "stripe" and "code" describe the first live row of
// each source, and "billing" reports whether billing exists on this instance.
// No live row returns the free-default shape. Query, scan or iteration errors
// return 503 without confirming a tier.
func (h *Handler) GetMe(c *gin.Context) {
	userID := c.GetString("user_id")

	live, err := h.readLiveRows(c.Request.Context(), userID)
	if err != nil {
		// Report unavailability without confirming a tier. The user id passes
		// through sanitizeID (CWE-117) and no row values are logged (PII-safe).
		h.log.Error("subscriptions: status read failed", "user_id", sanitizeID(userID), "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "subscription status unavailable"})
		return
	}
	c.JSON(http.StatusOK, h.buildStatus(live))
}
