package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/stripe/stripe-go/v87"
)

// Erasure outbox worker (#2033 spec §10, refinements R-b and R-d).
//
// users.deleteAccountTx queues one billing_erasure_outbox row per Stripe
// Customer of an erased account; this worker deletes the Customer, which
// cancels its subscriptions at once. It is the only code that deletes a
// Customer (C11).
//
// Claiming (R-b): a short statement pushes next_attempt_at forward by a lease
// and returns the rows, then Stripe is called with no transaction open. Row
// locks held across HTTP would hold a transaction open across a network call;
// the lease gives the same exclusion without one. A worker that dies mid-row
// leaves a lease that expires, and the next pass retries it.

const (
	// outboxInterval is the worker tick.
	outboxInterval = 30 * time.Second
	// outboxBatch bounds the rows one pass claims.
	outboxBatch = 10
	// outboxLease is how far a claim pushes next_attempt_at, so no other pass
	// takes the row while Stripe is being called.
	outboxLease = 5 * time.Minute
	// outboxBaseBackoff is the delay at zero attempts; it doubles per attempt
	// up to outboxMaxBackoff.
	outboxBaseBackoff = 30 * time.Second
	outboxMaxBackoff  = time.Hour

	// claimOutboxSQL leases up to $2 due rows for $1 seconds.
	claimOutboxSQL = `UPDATE billing_erasure_outbox
		SET next_attempt_at = NOW() + make_interval(secs => $1)
		WHERE id IN (
			SELECT id FROM billing_erasure_outbox
			WHERE next_attempt_at <= NOW()
			ORDER BY next_attempt_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		RETURNING id, stripe_customer_id, attempts`

	failOutboxSQL = `UPDATE billing_erasure_outbox
		SET attempts = attempts + 1,
			next_attempt_at = NOW() + make_interval(secs => $2),
			last_error_class = $3
		WHERE id = $1`

	deleteOutboxSQL = `DELETE FROM billing_erasure_outbox WHERE id = $1`

	customerMappedSQL = `SELECT EXISTS (SELECT 1 FROM stripe_customers WHERE stripe_customer_id = $1)`

	logKeyAttempts      = "attempts"
	logKeySessionID     = "checkout_session_id"
	logKeyStripeErrCode = "stripe_error_code"

	// classCustomerUnverifiable is the stored and logged class of
	// errCustomerUnverifiable.
	classCustomerUnverifiable = "customer_unverifiable"
	// classCustomerMapped is the stored and logged class of errCustomerMapped.
	classCustomerMapped = "customer_mapped"

	stageRetrieveCustomer = "retrieve_customer"
	stageCheckMapping     = "check_mapping"
)

// errCustomerUnverifiable means Stripe answered resource_missing about a
// Customer and a retrieve could not confirm the Customer was deleted.
var errCustomerUnverifiable = errors.New("billing: stripe customer not confirmed deleted")

// errCustomerMapped means a user is still mapped to the Customer an erasure
// row names, so the worker will not delete it.
var errCustomerMapped = errors.New("billing: stripe customer is mapped to another user")

// outboxRow is one claimed erasure obligation.
type outboxRow struct {
	id         string
	customerID string
	attempts   int
}

// outboxBackoff is the delay before the next try after attempts failures:
// 30 s doubled per attempt, capped at one hour. It cannot overflow, however
// large attempts is.
func outboxBackoff(attempts int) time.Duration {
	d := outboxBaseBackoff
	for i := 0; i < attempts && d < outboxMaxBackoff; i++ {
		d *= 2
	}
	return min(d, outboxMaxBackoff)
}

// RunOutboxWorker processes the erasure and code-credit outboxes at startup
// and then every outboxInterval until ctx is canceled. It blocks; start it
// with go.
func (s *Service) RunOutboxWorker(ctx context.Context) {
	runTicker(ctx, outboxInterval, func(ctx context.Context) {
		deleted, err := s.ProcessOutbox(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			s.Log.Warn("billing: erasure outbox pass failed",
				logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
		case deleted > 0:
			s.Log.Info("billing: erased Stripe customers", "deleted", deleted)
		}
		applied, err := s.ProcessCreditOutbox(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			s.Log.Warn("billing: code credit outbox pass failed",
				logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
		case applied > 0:
			s.Log.Info("billing: applied code credits", "applied", applied)
		}
	})
}

// ProcessOutbox claims up to outboxBatch due rows and erases each Customer at
// Stripe. It returns how many rows were completed. A row that fails is kept
// with a backoff and is not an error here; an error means the claim failed or
// ctx ended.
func (s *Service) ProcessOutbox(ctx context.Context) (deleted int, err error) {
	rows, err := s.claimOutbox(ctx)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		if err := s.eraseUnlessMapped(ctx, row.customerID); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return deleted, cerr // the lease expires and the row is retried
			}
			s.recordOutboxFailure(ctx, row, err)
			continue
		}
		if _, err := s.DB.ExecContext(ctx, deleteOutboxSQL, row.id); err != nil {
			// The Customer is gone; the next pass finds resource_missing, a
			// retrieve confirms the deletion, and the row is removed.
			s.Log.Warn("billing: removing a completed erasure row failed",
				logKeyCustomerID, row.customerID, logKeyErrorClass, errorClass(err), logKeyStage, "delete_outbox_row")
			continue
		}
		deleted++
	}
	return deleted, nil
}

// claimOutbox leases the due rows and reads them completely before any Stripe
// call, so no cursor or lock outlives this function.
func (s *Service) claimOutbox(ctx context.Context) ([]outboxRow, error) {
	rs, err := s.DB.QueryContext(ctx, claimOutboxSQL, outboxLease.Seconds(), outboxBatch)
	if err != nil {
		return nil, withStage("claim_outbox", fmt.Errorf("billing: outbox: claim: %w", err))
	}
	defer func() { _ = rs.Close() }()

	var rows []outboxRow
	for rs.Next() {
		var r outboxRow
		if err := rs.Scan(&r.id, &r.customerID, &r.attempts); err != nil {
			return nil, withStage("claim_outbox", fmt.Errorf("billing: outbox: scan: %w", err))
		}
		rows = append(rows, r)
	}
	if err := rs.Err(); err != nil {
		return nil, withStage("claim_outbox", fmt.Errorf("billing: outbox: claim: %w", err))
	}
	return rows, nil
}

// eraseUnlessMapped erases cusID unless a user is still mapped to it. The
// erased user's own mapping cascaded away with their users row, so a mapping
// left here is another user's: the erased user owned a subscription row on
// that user's Customer (a row keeps the user it was first written for), or
// the Customer was mapped after the obligation was queued. Deleting it would
// cancel the other user's billing, so no Stripe call is made and the row is
// kept as customer_mapped for an operator; it is erased on a later pass once
// no user is mapped to the Customer.
func (s *Service) eraseUnlessMapped(ctx context.Context, cusID string) error {
	var mapped bool
	if err := s.DB.QueryRowContext(ctx, customerMappedSQL, cusID).Scan(&mapped); err != nil {
		return withStage(stageCheckMapping, fmt.Errorf("billing: outbox: check mapping: %w", err))
	}
	if mapped {
		return withStage(stageCheckMapping, errCustomerMapped)
	}
	return s.eraseCustomer(ctx, cusID)
}

// eraseCustomer expires cusID's open Checkout sessions, then deletes the
// Customer. A delete answered with resource_missing is success only when a
// retrieve confirms Stripe deleted the Customer (confirmCustomerDeleted).
func (s *Service) eraseCustomer(ctx context.Context, cusID string) error {
	if err := s.expireOpenSessions(ctx, cusID); err != nil {
		return err
	}
	_, err := s.Stripe.V1Customers.Delete(ctx, cusID, nil)
	switch {
	case err == nil:
		return nil
	case isResourceMissing(err):
		return s.confirmCustomerDeleted(ctx, cusID)
	default:
		return withStage("delete_customer", fmt.Errorf("billing: outbox: delete customer: %w", err))
	}
}

// confirmCustomerDeleted is asked after Stripe answered resource_missing about
// cusID. That answer alone proves nothing: a key from another Stripe account
// gets it for a Customer that is alive, holding the person's data and still
// billing in the right account. Stripe keeps a deleted Customer retrievable
// with deleted: true, so only that answer confirms the deletion. A second
// resource_missing, or a Customer that is not deleted, returns an error
// wrapping errCustomerUnverifiable; any other failure is returned for a retry.
// The outbox and the Portal both use it before they let go of a Customer.
func (s *Service) confirmCustomerDeleted(ctx context.Context, cusID string) error {
	cus, err := s.Stripe.V1Customers.Retrieve(ctx, cusID, nil)
	switch {
	case err == nil && cus.Deleted:
		return nil
	case err == nil:
		return withStage(stageRetrieveCustomer, fmt.Errorf("%w: the customer is not deleted", errCustomerUnverifiable))
	case isResourceMissing(err):
		return withStage(stageRetrieveCustomer, fmt.Errorf("%w: the customer is unknown to this key", errCustomerUnverifiable))
	default:
		return withStage(stageRetrieveCustomer, fmt.Errorf("billing: retrieve customer: %w", err))
	}
}

// expireOpenSessions expires every open Checkout session of cusID so none can
// complete against a Customer about to be deleted. resource_missing on the
// list or an expire means there is nothing left to expire. An expire Stripe
// refuses as an invalid request means the session is no longer open (it
// completed or expired first), which is also done (R-d); any other failure
// retries the row.
func (s *Service) expireOpenSessions(ctx context.Context, cusID string) error {
	ids, err := s.openSessionIDs(ctx, cusID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_, err := s.Stripe.V1CheckoutSessions.Expire(ctx, id, nil)
		switch {
		case err == nil, isResourceMissing(err):
		case isSessionNoLongerOpen(err):
			s.logSessionNoLongerOpen(id, err)
		default:
			return withStage("expire_session", fmt.Errorf("billing: expire checkout session: %w", err))
		}
	}
	return nil
}

// logSessionNoLongerOpen records an expire refusal the outbox tolerates, so a
// 400 that means something other than "already completed or expired" leaves a
// trace. Stripe's error code is logged, never its message.
func (s *Service) logSessionNoLongerOpen(sessionID string, err error) {
	var code string
	var se *stripe.Error
	if errors.As(err, &se) {
		code = boundedClass(string(se.Code), "none")
	}
	s.Log.Info("billing: Stripe refused to expire a Checkout session; treating it as no longer open",
		logKeySessionID, sessionID, logKeyStripeErrCode, code)
}

// openSessionIDs lists the ids of cusID's open Checkout sessions.
func (s *Service) openSessionIDs(ctx context.Context, cusID string) ([]string, error) {
	params := &stripe.CheckoutSessionListParams{
		Customer: stripe.String(cusID),
		Status:   stripe.String(string(stripe.CheckoutSessionStatusOpen)),
	}
	var ids []string
	for sess, err := range s.Stripe.V1CheckoutSessions.List(ctx, params).All(ctx) {
		if err != nil {
			if isResourceMissing(err) {
				return nil, nil
			}
			return nil, withStage("list_sessions", fmt.Errorf("billing: outbox: list checkout sessions: %w", err))
		}
		ids = append(ids, sess.ID)
	}
	return ids, nil
}

// isSessionNoLongerOpen reports whether Stripe refused an expire because the
// session had already completed or expired: HTTP 400 with
// invalid_request_error. A 429 also carries that type (code rate_limit) and a
// 5xx never does, so the status code is what separates "done" from "retry".
func isSessionNoLongerOpen(err error) bool {
	var se *stripe.Error
	return errors.As(err, &se) &&
		se.Type == stripe.ErrorTypeInvalidRequest &&
		se.HTTPStatusCode == http.StatusBadRequest
}

// recordOutboxFailure keeps the row, counts the attempt and schedules the next
// one. The error class, never the message, is stored and logged. A Customer
// Stripe cannot confirm was deleted is an error, not a warning: it usually
// means the key belongs to another Stripe account, and no retry fixes that.
func (s *Service) recordOutboxFailure(ctx context.Context, row outboxRow, cause error) {
	class := errorClass(cause)
	var errMsg string // set for a failure an operator must look at
	switch {
	case errors.Is(cause, errCustomerUnverifiable):
		class = classCustomerUnverifiable
		errMsg = "billing: Stripe cannot confirm the Customer was deleted; erasure row kept. Check that the Stripe key belongs to the account holding the Customer"
	case errors.Is(cause, errCustomerMapped):
		class = classCustomerMapped
		errMsg = "billing: the Stripe customer is mapped to another user; erasure row kept for operator review"
	}
	next := outboxBackoff(row.attempts + 1)
	if _, err := s.DB.ExecContext(ctx, failOutboxSQL, row.id, next.Seconds(), class); err != nil {
		s.Log.Error("billing: recording an erasure failure failed",
			logKeyCustomerID, row.customerID, logKeyErrorClass, errorClass(err), logKeyStage, "record_outbox_failure")
	}
	attrs := []any{logKeyCustomerID, row.customerID, logKeyAttempts, row.attempts + 1,
		logKeyErrorClass, class, logKeyStage, errorStage(cause)}
	if errMsg != "" {
		s.Log.Error(errMsg, attrs...)
		return
	}
	s.Log.Warn("billing: erasing a Stripe customer failed; will retry", attrs...)
}
