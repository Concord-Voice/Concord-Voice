package redemption

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/google/uuid"
)

// Code credit (#2033 spec §0 D1). A premium code redeemed by a user whose Stripe
// subscription is live and renewing does not open a parallel code grant: it
// becomes Stripe customer-balance credit, which Stripe spends on the next
// renewals before charging the card. A code for the subscribed plan covers whole
// renewals; a code for another plan becomes face-value credit once the user
// confirms. A subscription that is set to end takes the ordinary code grant
// instead, starting when the paid period ends (grantPremiumSubscription), so the
// code is never stranded as credit nothing will spend.

// Plans a premium code can name in grant_params {"plan": ...}. They mirror
// billing.PlanSupersonic and billing.PlanWing; billing imports this package,
// so it cannot be imported back.
const (
	PlanSupersonic = "supersonic"
	PlanWing       = "wing"
)

// ErrCreditUnavailable means the code would become credit but cannot (billing
// is not configured, or the subscription has no plan or interval to price it
// by). The redemption rolls back and the code stays unredeemed.
var ErrCreditUnavailable = errors.New("redemption: code credit unavailable")

// CreditConfirmationError is returned when the code names a plan other than the
// subscribed one and the user has not confirmed. It carries the quote the client
// shows. The redemption rolls back, so the code stays unredeemed.
type CreditConfirmationError struct {
	Amount         int64  `json:"amount"`
	Currency       string `json:"currency"`
	CodePlan       string `json:"code_plan"`
	SubscribedPlan string `json:"subscribed_plan"`
}

func (e *CreditConfirmationError) Error() string {
	return "redemption: code credit needs confirmation"
}

// CreditRequest is one code to price as credit for one live subscription.
type CreditRequest struct {
	SubscribedPlan string
	Interval       string // "month" or "year"
	FoundersRate   bool
	CodePlan       string
	Months         int
	Confirmed      bool
}

// CreditResult is a priced credit, in the smallest currency unit.
type CreditResult struct {
	Amount            int64
	Currency          string
	Matched           bool   // the code's plan is the subscribed plan
	StripeDescription string // the balance transaction's description
}

// Crediter prices and applies code credit. billing.Service implements it.
// QuoteCredit runs inside the redemption transaction and calls nothing outside
// the process. The engine then commits the claim with one
// billing_credit_outbox row, and calls ApplyCredit after the commit; a credit
// ApplyCredit cannot apply stays in the outbox for billing's worker. No Stripe
// call ever runs inside the redemption transaction, so a rollback cannot leave
// credit behind.
type Crediter interface {
	QuoteCredit(req CreditRequest) (CreditResult, error)
	ApplyCredit(ctx context.Context, redemptionID uuid.UUID)
}

// SetCrediter wires code credit. Without it, a redemption on a live renewing
// Stripe subscription fails with ErrCreditUnavailable rather than opening a
// parallel code grant.
func (e *Engine) SetCrediter(c Crediter) {
	e.crediter = c
}

// stripeSub is the user's live Stripe row, as the credit path needs it.
type stripeSub struct {
	id           uuid.UUID
	customerID   string
	plan         sql.NullString
	interval     sql.NullString
	foundersRate bool
	canceling    bool
}

// liveStripeSub reads the user's live Stripe row, if any, locking it. A
// renewing row wins over a canceling one, so a user with both gets credit. The
// engine already holds the users row, so this follows the lock order
// users → subscriptions.
func liveStripeSub(ctx context.Context, tx *sql.Tx, userID uuid.UUID) (stripeSub, bool, error) {
	var s stripeSub
	err := tx.QueryRowContext(ctx, `
		SELECT id, stripe_customer_id, plan, billing_interval, founders_rate,
		       (cancel_at_period_end OR cancel_at IS NOT NULL)
		  FROM subscriptions
		 WHERE user_id = $1
		   AND source = 'stripe'
		   AND `+entitlements.LiveRowPredicate+`
		 ORDER BY (cancel_at_period_end OR cancel_at IS NOT NULL), current_period_end DESC
		 LIMIT 1
		   FOR UPDATE`, userID).
		Scan(&s.id, &s.customerID, &s.plan, &s.interval, &s.foundersRate, &s.canceling)
	if errors.Is(err, sql.ErrNoRows) {
		return stripeSub{}, false, nil
	}
	if err != nil {
		return stripeSub{}, false, fmt.Errorf("redemption: read stripe subscription: %w", err)
	}
	return s, true, nil
}

// codePlanFromParams reads {"plan": ...}; a code without one is a Supersonic
// code, which is what every premium code issued before D1 grants. The issuer
// refuses any other plan, so a false here is a row written outside it.
func codePlanFromParams(params map[string]any) (string, bool) {
	v, ok := params["plan"]
	if !ok {
		return PlanSupersonic, true
	}
	p, _ := v.(string)
	if p != PlanSupersonic && p != PlanWing {
		return "", false
	}
	return p, true
}

// creditGrant prices the claimed premium code as credit on sub and queues it
// in billing_credit_outbox, in the redemption transaction.
func (e *Engine) creditGrant(ctx context.Context, tx *sql.Tx, sub stripeSub, claim claimResult, redemptionID uuid.UUID, confirmed bool) (GrantResult, error) {
	switch {
	case e.crediter == nil:
		return GrantResult{}, fmt.Errorf("%w: billing not configured", ErrCreditUnavailable)
	case !sub.plan.Valid || !sub.interval.Valid:
		return GrantResult{}, fmt.Errorf("%w: stripe row %s has no plan or interval", ErrCreditUnavailable, sub.id)
	}
	params, err := decodeParams(claim.grantBytes)
	if err != nil {
		return GrantResult{}, fmt.Errorf("redemption: decode grant_params: %w", err)
	}
	codePlan, ok := codePlanFromParams(params)
	if !ok {
		return GrantResult{}, ErrCodeNotValid
	}
	res, err := e.crediter.QuoteCredit(CreditRequest{
		SubscribedPlan: sub.plan.String,
		Interval:       sub.interval.String,
		FoundersRate:   sub.foundersRate,
		CodePlan:       codePlan,
		Months:         monthsFromParams(params),
		Confirmed:      confirmed,
	})
	if err != nil {
		var confirm *CreditConfirmationError
		if errors.As(err, &confirm) {
			return GrantResult{}, err
		}
		return GrantResult{}, fmt.Errorf("%w: %w", ErrCreditUnavailable, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing_credit_outbox (redemption_id, stripe_customer_id, amount, currency, description)
		VALUES ($1, $2, $3, $4, $5)`,
		redemptionID, sub.customerID, res.Amount, res.Currency, res.StripeDescription); err != nil {
		return GrantResult{}, fmt.Errorf("redemption: queue code credit: %w", err)
	}
	return GrantResult{
		Description:    creditDescription(res),
		SubscriptionID: uuid.NullUUID{UUID: sub.id, Valid: true},
		creditQueued:   true,
	}, nil
}

// creditDescription is the success line. Only USD is sold (spec P3-C).
func creditDescription(r CreditResult) string {
	return fmt.Sprintf("Added $%d.%02d of credit to your subscription. Your next renewals use it before your card is charged.",
		r.Amount/100, r.Amount%100)
}

// heldSeatSQL matches an active Wing seat whose parent entitles, the same
// join entitlements.ResolveTierQ reads.
const heldSeatSQL = `SELECT EXISTS (SELECT 1 FROM subscription_seats s
	JOIN subscriptions ON subscriptions.id = s.subscription_id
	WHERE s.holder_user_id = $1 AND ` + entitlements.LiveSeatParentPredicate + `)`

// refuseSeatMember returns ErrSeatMember when the user holds an entitling
// Wing seat. It takes no seat lock: the users row is already held, and a seat
// accepted after this read leaves the code grant running beside the seat, the
// "code first, then seat" order the spec allows.
func refuseSeatMember(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error {
	var seated bool
	if err := tx.QueryRowContext(ctx, heldSeatSQL, userID).Scan(&seated); err != nil {
		return fmt.Errorf("redemption: read wing seat: %w", err)
	}
	if seated {
		return ErrSeatMember
	}
	return nil
}
