package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v87"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/redemption"
)

// Code credit (#2033 spec §0 D1): a premium code redeemed on a live, renewing
// subscription becomes Stripe customer-balance credit, spent on the next
// renewals before the card is charged.
//
// The redemption commits the code claim and one billing_credit_outbox row in a
// single transaction, with no Stripe call inside it. The credit is applied
// from that row afterwards: at once by ApplyCredit, and by the outbox worker
// for any row still due. Claiming follows the erasure outbox (R-b): a short
// statement leases the row, then Stripe is called with no transaction open.

const (
	// balanceIdempotencyPrefix starts the credit's Idempotency-Key, shaped
	// like customerIdempotencyPrefix. The key is the redemption id, so every
	// retry of one credit replays it and no two credits share a key.
	balanceIdempotencyPrefix = "concord-code-credit-v2:"

	// creditTimeout bounds one Stripe balance call.
	creditTimeout = 15 * time.Second

	// creditReplayWindow is how old a pending credit may be before a retry
	// first checks Stripe for it. Stripe may prune an Idempotency-Key after 24
	// hours and then treats it as new, so an older row whose earlier attempt
	// was accepted (but whose row delete failed) must not be posted again.
	creditReplayWindow = 23 * time.Hour

	// creditAlertAttempts is the failure count at which a pending credit is
	// logged as an error rather than a warning.
	creditAlertAttempts = 5

	metadataKeyCodeID       = "concord_code_id"
	metadataKeyRedemptionID = "concord_redemption_id"

	logKeyRedemptionID = "redemption_id"

	// claimCreditSQL leases up to $2 due rows for $1 seconds; with $3 set, only
	// that redemption's row. A row is past the replay window ($4 seconds) by
	// the database's clock alone, the clock that stamped created_at.
	claimCreditSQL = `UPDATE billing_credit_outbox o
		SET next_attempt_at = NOW() + make_interval(secs => $1)
		FROM code_redemptions r
		WHERE r.id = o.redemption_id
		  AND o.id IN (
			SELECT id FROM billing_credit_outbox
			WHERE next_attempt_at <= NOW()
			  AND ($3::uuid IS NULL OR redemption_id = $3)
			ORDER BY next_attempt_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		RETURNING o.id, o.redemption_id, o.stripe_customer_id, o.amount, o.currency,
		          o.description, o.attempts, o.created_at <= NOW() - make_interval(secs => $4),
		          r.user_id, r.code_id`

	failCreditSQL = `UPDATE billing_credit_outbox
		SET attempts = attempts + 1,
			next_attempt_at = NOW() + make_interval(secs => $2),
			last_error_class = $3
		WHERE id = $1`

	deleteCreditSQL = `DELETE FROM billing_credit_outbox WHERE id = $1`
)

var _ redemption.Crediter = (*Service)(nil)

// creditRow is one leased pending credit.
type creditRow struct {
	id           uuid.UUID
	redemptionID uuid.UUID
	customerID   string
	amount       int64
	currency     string
	description  string
	attempts     int
	pastReplay   bool // older than creditReplayWindow
	userID       uuid.UUID
	codeID       uuid.UUID
}

// codeCreditAmount is what a code is worth as credit, in cents.
//
// Same plan: the code covers whole renewals, so its months are priced at what
// the subscription renews for (founders rate included): months × the monthly
// renewal, or months/12 of the yearly one. A partial year rounds UP to the
// cent, in the user's favour. Another plan: face value, months × that plan's
// monthly list price, which the user has confirmed.
func codeCreditAmount(req redemption.CreditRequest) (amount int64, matched bool, err error) {
	months := int64(req.Months)
	if req.CodePlan == req.SubscribedPlan {
		o, ok := findOffer(req.SubscribedPlan, req.Interval)
		if !ok {
			return 0, true, errUnknownPlan
		}
		unit := o.unitAmount
		if req.FoundersRate {
			unit = foundersUnitAmount(unit)
		}
		if o.interval == stripe.PriceRecurringIntervalYear {
			return (months*unit + 11) / 12, true, nil
		}
		return months * unit, true, nil
	}
	o, ok := findOffer(req.CodePlan, string(stripe.PriceRecurringIntervalMonth))
	if !ok {
		return 0, false, errUnknownPlan
	}
	return months * o.unitAmount, false, nil
}

// QuoteCredit prices the code as credit. It calls nothing. A code for another
// plan returns *redemption.CreditConfirmationError until the user confirms.
func (s *Service) QuoteCredit(req redemption.CreditRequest) (redemption.CreditResult, error) {
	amount, matched, err := codeCreditAmount(req)
	if err != nil {
		return redemption.CreditResult{}, err
	}
	if !matched && !req.Confirmed {
		return redemption.CreditResult{}, &redemption.CreditConfirmationError{
			Amount: amount, Currency: catalogCurrency, CodePlan: req.CodePlan, SubscribedPlan: req.SubscribedPlan,
		}
	}
	return redemption.CreditResult{
		Amount: amount, Currency: catalogCurrency, Matched: matched,
		StripeDescription: fmt.Sprintf("Concord code credit: %d month(s) of %s", req.Months, req.CodePlan),
	}, nil
}

// ApplyCredit applies the committed credit of one redemption now, so the user
// usually sees it at once. It is detached from the caller: a client that
// disconnects must not cut the call short. Any failure leaves the row to the
// outbox worker, which retries it.
func (s *Service) ApplyCredit(ctx context.Context, redemptionID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), creditTimeout)
	defer cancel()
	rows, err := s.claimCredits(ctx, 1, uuid.NullUUID{UUID: redemptionID, Valid: true})
	if err != nil {
		s.Log.Warn("billing: claiming a code credit failed; the outbox worker will apply it",
			logKeyRedemptionID, redemptionID.String(), logKeyErrorClass, errorClass(err), logKeyStage, "claim_credit")
		return
	}
	for _, row := range rows {
		s.applyCreditRow(ctx, row)
	}
}

// ProcessCreditOutbox applies up to outboxBatch due credits and returns how
// many Stripe accepted. A credit that fails is kept with a backoff and is not
// an error here; an error means the claim failed or ctx ended.
func (s *Service) ProcessCreditOutbox(ctx context.Context) (applied int, err error) {
	rows, err := s.claimCredits(ctx, outboxBatch, uuid.NullUUID{})
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return applied, err // the lease expires and the row is retried
		}
		if s.applyCreditRow(ctx, row) {
			applied++
		}
	}
	return applied, nil
}

// claimCredits leases due rows and reads them completely before any Stripe
// call, so no cursor or lock outlives this function.
func (s *Service) claimCredits(ctx context.Context, limit int, only uuid.NullUUID) ([]creditRow, error) {
	rs, err := s.DB.QueryContext(ctx, claimCreditSQL, outboxLease.Seconds(), limit, only, creditReplayWindow.Seconds())
	if err != nil {
		return nil, withStage("claim_credit", fmt.Errorf("billing: claim code credits: %w", err))
	}
	defer func() { _ = rs.Close() }() // read-only cursor; rs.Err below reports a read failure
	var rows []creditRow
	for rs.Next() {
		var r creditRow
		if err := rs.Scan(&r.id, &r.redemptionID, &r.customerID, &r.amount, &r.currency,
			&r.description, &r.attempts, &r.pastReplay, &r.userID, &r.codeID); err != nil {
			return nil, withStage("claim_credit", fmt.Errorf("billing: scan code credit: %w", err))
		}
		rows = append(rows, r)
	}
	if err := rs.Err(); err != nil {
		return nil, withStage("claim_credit", fmt.Errorf("billing: read code credits: %w", err))
	}
	return rows, nil
}

// applyCreditRow posts one balance transaction and removes the row once Stripe
// holds the credit. It reports whether Stripe holds it.
func (s *Service) applyCreditRow(ctx context.Context, row creditRow) bool {
	if row.pastReplay {
		held, err := s.creditAlreadyApplied(ctx, row)
		if err != nil {
			s.creditMissedCustomer(ctx, row, err)
			return false
		}
		if held {
			s.deleteCreditRow(ctx, row)
			return true
		}
	}

	params := &stripe.CustomerBalanceTransactionCreateParams{
		Customer:    stripe.String(row.customerID),
		Amount:      stripe.Int64(-row.amount), // negative is credit
		Currency:    stripe.String(row.currency),
		Description: stripe.String(row.description),
	}
	params.AddMetadata(metadataKeyUserID, row.userID.String())
	params.AddMetadata(metadataKeyCodeID, row.codeID.String())
	params.AddMetadata(metadataKeyRedemptionID, row.redemptionID.String())
	params.AddMetadata(metadataKeyEnv, s.Environment)
	params.SetIdempotencyKey(balanceIdempotencyPrefix + row.redemptionID.String())

	if _, err := s.Stripe.V1CustomerBalanceTransactions.Create(ctx, params); err != nil {
		s.creditMissedCustomer(ctx, row, err)
		return false
	}
	s.deleteCreditRow(ctx, row)
	return true
}

// creditMissedCustomer handles a failed list or post. resource_missing alone
// is not proof: a key for another account sees no Customer either, so the
// credit is dropped only once deletion is confirmed; anything else is retried.
func (s *Service) creditMissedCustomer(ctx context.Context, row creditRow, err error) {
	if !isCustomerMissing(err) {
		s.creditFailed(ctx, row, err)
		return
	}
	if err := s.confirmCustomerDeleted(ctx, row.customerID); err != nil {
		s.creditFailed(ctx, row, err)
		return
	}
	s.Log.Warn("billing: dropping a code credit whose Stripe customer was deleted",
		logKeyCustomerID, row.customerID, logKeyRedemptionID, row.redemptionID.String())
	s.deleteCreditRow(ctx, row)
}

// creditAlreadyApplied reports whether the Customer already holds this
// redemption's credit, found by its metadata. It reads the whole history, not
// a created range, so clock skew between the database and Stripe cannot hide
// the credit.
// ponytail: full scan; a Customer's balance transactions are few. Page by
// created range with a wide margin if that ever stops holding.
func (s *Service) creditAlreadyApplied(ctx context.Context, row creditRow) (bool, error) {
	params := &stripe.CustomerBalanceTransactionListParams{Customer: stripe.String(row.customerID)}
	for txn, err := range s.Stripe.V1CustomerBalanceTransactions.List(ctx, params).All(ctx) {
		if err != nil {
			return false, withStage("list_credits", fmt.Errorf("billing: list balance transactions: %w", err))
		}
		if txn.Metadata[metadataKeyRedemptionID] == row.redemptionID.String() {
			return true, nil
		}
	}
	return false, nil
}

// creditFailed records a failed attempt unless ctx ended, in which case the
// lease expires and the row is retried.
func (s *Service) creditFailed(ctx context.Context, row creditRow, err error) {
	if ctx.Err() == nil {
		s.recordCreditFailure(ctx, row, err)
	}
}

func (s *Service) deleteCreditRow(ctx context.Context, row creditRow) {
	if _, err := s.DB.ExecContext(ctx, deleteCreditSQL, row.id); err != nil {
		// The next pass replays the same Idempotency-Key (or, past
		// creditReplayWindow, finds the credit by its metadata) and removes
		// the row then.
		s.Log.Warn("billing: removing a completed code credit row failed",
			logKeyRedemptionID, row.redemptionID.String(), logKeyErrorClass, errorClass(err), logKeyStage, "delete_credit_row")
	}
}

// recordCreditFailure keeps the row, counts the attempt and schedules the next
// one. The error class, never the message, is stored and logged.
func (s *Service) recordCreditFailure(ctx context.Context, row creditRow, cause error) {
	class := errorClass(cause)
	if errors.Is(cause, errCustomerUnverifiable) {
		class = classCustomerUnverifiable // usually a key for another Stripe account
	}
	next := outboxBackoff(row.attempts + 1)
	if _, err := s.DB.ExecContext(ctx, failCreditSQL, row.id, next.Seconds(), class); err != nil {
		s.Log.Error("billing: recording a code credit failure failed",
			logKeyRedemptionID, row.redemptionID.String(), logKeyErrorClass, errorClass(err), logKeyStage, "record_credit_failure")
	}
	attrs := []any{logKeyCustomerID, row.customerID, logKeyRedemptionID, row.redemptionID.String(),
		logKeyAttempts, row.attempts + 1, logKeyErrorClass, class, logKeyStage, errorStage(cause)}
	if row.attempts+1 >= creditAlertAttempts {
		s.Log.Error("billing: a code credit keeps failing at Stripe; the user has not received it", attrs...)
		return
	}
	s.Log.Warn("billing: applying a code credit failed; will retry", attrs...)
}
