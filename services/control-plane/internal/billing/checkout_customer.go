package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/stripe/stripe-go/v87"
)

// The caller's Stripe Customer for Checkout (#2033 spec §8 Checkout step 4,
// plan Task 4). Stripe is called with no transaction open; each statement
// below autocommits.

const (
	// customerTouchSQL stamps last_checkout_at (the drift worker's discovery
	// pass reads it) and returns the mapped Customer, so it is also the
	// re-select after the INSERT.
	customerTouchSQL = `UPDATE stripe_customers SET last_checkout_at = NOW()
		WHERE user_id = $1 RETURNING stripe_customer_id`

	// customerInsertSQL never maps a Customer the erasure outbox still holds:
	// the outbox keeps its row until Stripe confirms the deletion, so while
	// the row exists the Customer may be deleted under the mapping.
	customerInsertSQL = `INSERT INTO stripe_customers (user_id, stripe_customer_id)
		SELECT $1, $2 WHERE NOT EXISTS (
			SELECT 1 FROM billing_erasure_outbox WHERE stripe_customer_id = $2)
		ON CONFLICT DO NOTHING`

	customerPendingErasureSQL = `SELECT EXISTS (
		SELECT 1 FROM billing_erasure_outbox WHERE stripe_customer_id = $1)`

	erasureEnqueueSQL = `INSERT INTO billing_erasure_outbox (stripe_customer_id)
		VALUES ($1) ON CONFLICT DO NOTHING`

	// customerIdempotencyPrefix keys the Customer create, so a double click or
	// a retry gets the Customer the first request created.
	customerIdempotencyPrefix = "concord-customer-v1:"

	stageEnsureCustomer = "ensure_customer"

	// customerCreateTimeout bounds the detached Customer create, so a
	// Stripe stall cannot hold an abandoned Checkout open.
	customerCreateTimeout = 30 * time.Second

	// userLockedSQL waits out an erasure holding the user's row, then reports
	// whether the user still exists.
	userLockedSQL = `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 FOR KEY SHARE)`

	// mappingHeldSQL reports whether userID still maps cus.
	mappingHeldSQL = `SELECT EXISTS (SELECT 1 FROM stripe_customers
		WHERE user_id = $1 AND stripe_customer_id = $2)`
)

// mapTimeout bounds each database step after a Customer exists in Stripe.
// They run detached from the request, so a client that disconnects
// mid-Checkout cannot leave the Customer unmapped. A var so tests can shorten
// it.
var mapTimeout = 5 * time.Second

// errUserErased means the caller's account was erased while Checkout ran: the
// mapping INSERT failed its foreign key (R31). The new Customer is already in
// the erasure outbox.
var errUserErased = errors.New("billing: checkout: user erased mid-request")

// errErasureNotQueued joins errUserErased when the new Customer could not be
// put in the erasure outbox; the enqueue failure is logged with its id.
var errErasureNotQueued = errors.New("billing: checkout: unmapped customer not queued for deletion")

// errMappingChanged means the mapping vanished between its INSERT and the
// re-select (an erasure or a concurrent self-heal). The caller retries later.
var errMappingChanged = errors.New("billing: checkout: customer mapping changed")

// ensureCustomer returns the caller's Stripe Customer, creating it on first
// use, and stamps last_checkout_at.
func (s *Service) ensureCustomer(ctx context.Context, userID uuid.UUID) (string, error) {
	cus, err := s.touchMapping(ctx, userID)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return cus, err
	}
	return s.createAndMap(ctx, userID, customerIdempotencyPrefix+userID.String())
}

// replaceStaleCustomer drops the mapping of a Customer Stripe confirmed
// deleted and maps a new one (spec §8 step 6, F18). The idempotency key names
// the stale Customer, so a retry of this replacement reuses its Customer while
// a later replacement of the new one does not.
func (s *Service) replaceStaleCustomer(ctx context.Context, userID uuid.UUID, stale string) (string, error) {
	if _, err := s.DB.ExecContext(ctx, portalDropStaleSQL, stale); err != nil {
		return "", withStage(stageEnsureCustomer, fmt.Errorf("billing: checkout: drop stale mapping: %w", err))
	}
	s.Log.Warn("billing: Stripe no longer has the mapped Customer; replacing it for Checkout", logKeyCustomerID, stale)
	return s.createAndMap(ctx, userID, customerIdempotencyPrefix+userID.String()+":after:"+stale)
}

// touchMapping stamps and returns the existing mapping; sql.ErrNoRows when the
// caller has none.
func (s *Service) touchMapping(ctx context.Context, userID uuid.UUID) (string, error) {
	var cus string
	err := s.DB.QueryRowContext(ctx, customerTouchSQL, userID).Scan(&cus)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", withStage(stageEnsureCustomer, fmt.Errorf("billing: checkout: read mapping: %w", err))
	}
	return cus, err
}

// createAndMap creates a Customer under key and maps it to userID. The
// Customer carries no email or name: Checkout collects what Stripe needs.
// It runs detached from the request: a client that disconnects mid-create
// must not leave a Customer that nothing maps.
func (s *Service) createAndMap(ctx context.Context, userID uuid.UUID, key string) (string, error) {
	return s.createAndMapDetached(context.WithoutCancel(ctx), userID, key, true)
}

func (s *Service) createAndMapDetached(ctx context.Context, userID uuid.UUID, key string, mayReplace bool) (string, error) {
	params := &stripe.CustomerCreateParams{}
	params.AddMetadata(metadataKeyUserID, userID.String())
	params.AddMetadata(metadataKeyEnv, s.Environment)
	params.SetIdempotencyKey(key)
	createCtx, cancelCreate := context.WithTimeout(ctx, customerCreateTimeout)
	created, err := s.Stripe.V1Customers.Create(createCtx, params)
	cancelCreate()
	if err != nil {
		return "", withStage("create_customer", fmt.Errorf("billing: checkout: create customer: %w", err))
	}
	mapCtx, cancel := context.WithTimeout(ctx, mapTimeout)
	defer cancel()
	if _, err := s.DB.ExecContext(mapCtx, customerInsertSQL, userID, created.ID); err != nil {
		var pe *pq.Error
		if errors.As(err, &pe) && pe.Code == pqerror.ForeignKeyViolation {
			if !s.enqueueErasure(mapCtx, created.ID) {
				return "", errors.Join(errUserErased, errErasureNotQueued)
			}
			return "", errUserErased
		}
		// A timeout here can be an erasure holding the user's row: the FK
		// check never got to answer 23503. Wait it out on a fresh deadline.
		if !s.userStillExists(ctx, userID) {
			if !s.enqueueErasure(ctx, created.ID) {
				return "", errors.Join(errUserErased, errErasureNotQueued)
			}
			return "", errUserErased
		}
		// Not queued: the next Checkout replays the idempotency key, gets
		// this Customer back and maps it. The id is logged for the case
		// where no retry comes.
		s.Log.Error("billing: checkout created a Customer but could not map it; the next Checkout reuses it",
			logKeyCustomerID, created.ID, logKeyErrorClass, errorClass(err), logKeyStage, stageEnsureCustomer)
		return "", withStage(stageEnsureCustomer, fmt.Errorf("billing: checkout: insert mapping: %w", err))
	}
	cus, err := s.touchMapping(mapCtx, userID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if mayReplace && s.pendingErasure(mapCtx, created.ID) {
			// The key replayed a Customer the outbox is deleting (it lost an
			// earlier mapping race), so the insert refused it. Create a new
			// one under a key that names it; this happens at most once.
			return s.createAndMapDetached(ctx, userID, key+":after:"+created.ID, false)
		}
		// The mapping is gone already. If it was an erasure the user row is
		// gone too, and the Customer just created would be left behind.
		s.enqueueErasure(mapCtx, created.ID)
		return "", withStage(stageEnsureCustomer, errMappingChanged)
	case err != nil:
		return "", err
	case cus != created.ID:
		// A concurrent request mapped a different Customer first (only a
		// self-heal racing a first purchase can). Keep theirs; the outbox
		// worker deletes ours, because nothing maps it, and customerInsertSQL
		// refuses to map it again while it is queued.
		s.enqueueErasure(mapCtx, created.ID)
	}
	return cus, nil
}

// userStillExists waits, on its own deadline, for any erasure holding the
// user's row and reports whether the user survived. An error counts as yes:
// the caller then logs the Customer for an operator.
func (s *Service) userStillExists(ctx context.Context, userID uuid.UUID) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mapTimeout)
	defer cancel()
	var exists bool
	if err := s.DB.QueryRowContext(ctx, userLockedSQL, userID).Scan(&exists); err != nil {
		return true
	}
	return exists
}

// mappingHeld reports, on its own deadline, whether userID still maps cus.
// A read error counts as no, so a session is never returned unverified.
func (s *Service) mappingHeld(ctx context.Context, userID uuid.UUID, cus string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mapTimeout)
	defer cancel()
	var held bool
	if err := s.DB.QueryRowContext(ctx, mappingHeldSQL, userID, cus).Scan(&held); err != nil {
		s.Log.Error("billing: checkout could not re-read the Customer mapping",
			logKeyErrorClass, errorClass(err), logKeyStage, stageEnsureCustomer)
		return false
	}
	return held
}

// pendingErasure reports whether the erasure outbox still holds cusID. A read
// error counts as no: the caller then reports the mapping as changed.
func (s *Service) pendingErasure(ctx context.Context, cusID string) bool {
	var pending bool
	if err := s.DB.QueryRowContext(ctx, customerPendingErasureSQL, cusID).Scan(&pending); err != nil {
		s.Log.Error("billing: checkout could not read the erasure outbox",
			logKeyErrorClass, errorClass(err), logKeyStage, stageEnsureCustomer)
		return false
	}
	return pending
}

// enqueueErasure puts an unmapped Customer in the erasure outbox and reports
// whether it did. A failure is logged with the Customer id: the drift worker's
// discovery cannot see an unmapped Customer, so the log line is what an
// operator has.
func (s *Service) enqueueErasure(ctx context.Context, cusID string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mapTimeout)
	defer cancel()
	if _, err := s.DB.ExecContext(ctx, erasureEnqueueSQL, cusID); err != nil {
		s.Log.Error("billing: could not queue an unmapped Customer for deletion; delete it in the Dashboard",
			logKeyCustomerID, cusID, logKeyErrorClass, errorClass(err), logKeyStage, "enqueue_erasure")
		return false
	}
	return true
}
