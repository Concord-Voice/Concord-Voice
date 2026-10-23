package billing

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v87"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// Reconcile (#2033 spec §7, plan Task 3 with refinement R-a).
//
// Invariant: for one Stripe subscription id, at most one Reconcile reads
// Stripe and writes the row at a time, and every row it commits reflects a
// fetch taken while it held that exclusivity. So commits for one id happen in
// fetch order, and a slower reconcile carrying an older fetch can never land
// after a newer one. The exclusivity is a SESSION-level advisory lock held on
// a dedicated connection from before the fetch through post-commit notifications. It
// breaks if the fetch moves before the lock, if the lock becomes
// transaction-scoped (it would end at commit, and no transaction may be open
// across the Stripe call), or if a connection that failed to unlock returns to
// the pool: session locks survive in the session and are re-entrant there, so
// such a connection would hold the key until the process exits. Hence every
// unlock failure discards the connection, which ends the session and so
// releases the lock.
//
// Lock order: advisory key → users (the payer, FOR UPDATE) → stripe_customers
// (the self-heal INSERT) → subscriptions, the order deleteAccountTx takes.
// The users lock precedes the self-heal so the INSERT's FK check (FOR KEY
// SHARE) never has to upgrade to a lock erasure is waiting on (R-a).

const (
	// metadataKeyUserID and metadataKeyEnv are the subscription metadata
	// Checkout stamps: the payer's Concord user id and the environment that
	// created it. Reconcile reads them only when the customer is unmapped.
	metadataKeyUserID = "concord_user_id"
	metadataKeyEnv    = "concord_env"

	// Advisory lock statements. The key is derived in SQL from a constant
	// domain prefix and the subscription id, so every holder of the key (and
	// the tests) computes the same value.
	advisoryLockSQL   = `SELECT pg_advisory_lock(hashtextextended('concord.stripe_sub:' || $1::text, 0))`
	advisoryUnlockSQL = `SELECT pg_advisory_unlock(hashtextextended('concord.stripe_sub:' || $1::text, 0))`

	// unlockTimeout bounds the unlock round trip. The unlock runs on a context
	// detached from the caller's cancellation, so a canceled request still
	// releases its key instead of discarding a healthy connection.
	unlockTimeout = 5 * time.Second

	// A stalled live-notification queue must not retain the subscription lock.
	tierNotificationTimeout = 10 * time.Second

	// Log attribute keys. Stripe ids appear only as values under these
	// constant keys (spec §14); objects are never dumped.
	logKeySubID      = "stripe_subscription_id"
	logKeyCustomerID = "stripe_customer_id"
	logKeyUserID     = "user_id"
	logKeyReason     = "reason"
	logKeyErrorClass = "error_class"
)

// Failure stages, the logKeyStage values Reconcile tags its errors with. Each
// names the statement or call that failed; none carries error text.
const (
	stageConnAcquire     = "conn_acquire"
	stageAdvisoryLock    = "advisory_lock"
	stageAdvisoryUnlock  = "advisory_unlock"
	stageConnClose       = "conn_close"
	stageFetchSub        = "fetch_subscription"
	stageBegin           = "begin"
	stageReadMapping     = "read_mapping"
	stageLockPayer       = "lock_payer"
	stageSelfHeal        = "self_heal"
	stageTierBefore      = "tier_before"
	stageReadStoredRow   = "read_stored_row"
	stageOwnerTierBefore = "owner_tier_before"
	stageUpsert          = "upsert_subscription"
	stageDedupeInsert    = "dedupe_insert"
	stageTierAfter       = "tier_after"
	stageOwnerTierAfter  = "owner_tier_after"
	stageCountLiveRows   = "count_live_rows"
	stageCommit          = "commit"
	stageRollback        = "rollback"
	stageTierPush        = "tier_push"
)

const (
	payerByCustomerSQL = `SELECT user_id FROM stripe_customers WHERE stripe_customer_id = $1`
	lockPayerSQL       = `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`
	selfHealMappingSQL = `INSERT INTO stripe_customers (user_id, stripe_customer_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`
	customerByUserSQL = `SELECT stripe_customer_id FROM stripe_customers WHERE user_id = $1`
	insertDedupeSQL   = `INSERT INTO stripe_webhook_events (event_id, event_type) VALUES ($1, $2)
		ON CONFLICT (event_id) DO NOTHING`

	selectStoredRowSQL = `SELECT user_id, tier, status, stripe_customer_id, current_period_end,
		cancel_at_period_end, plan, billing_interval, founders_rate, cancel_at
		FROM subscriptions WHERE stripe_subscription_id = $1 FOR UPDATE`

	// upsertRowSQL never changes user_id on conflict: a row stays with the
	// user it was first written for, whatever the mapping says later.
	// updated_at is always set, so a reconcile that changed nothing still
	// records that it ran (F27).
	upsertRowSQL = `INSERT INTO subscriptions
		(user_id, tier, status, source, stripe_customer_id, stripe_subscription_id,
		 current_period_end, cancel_at_period_end, plan, billing_interval, founders_rate, cancel_at)
		VALUES ($1, $2, $3, 'stripe', $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (stripe_subscription_id) WHERE stripe_subscription_id IS NOT NULL DO UPDATE SET
		 tier = EXCLUDED.tier,
		 status = EXCLUDED.status,
		 stripe_customer_id = EXCLUDED.stripe_customer_id,
		 current_period_end = EXCLUDED.current_period_end,
		 cancel_at_period_end = EXCLUDED.cancel_at_period_end,
		 plan = EXCLUDED.plan,
		 billing_interval = EXCLUDED.billing_interval,
		 founders_rate = EXCLUDED.founders_rate,
		 cancel_at = EXCLUDED.cancel_at,
		 updated_at = NOW()`

	// countLiveStripeRowsSQL concatenates the shared predicate as a constant.
	countLiveStripeRowsSQL = `SELECT COUNT(*) FROM subscriptions
		WHERE user_id = $1 AND source = 'stripe' AND ` + entitlements.LiveRowPredicate
)

// execer is the write method shared by *sql.DB, *sql.Conn and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// reconcileInput is one fetched subscription on its way to the database.
type reconcileInput struct {
	subID     string
	eventID   string
	eventType string
	fields    rowFields
	metadata  map[string]string
}

// reconcileOutcome is what a committed reconcile reports for the post-commit push.
type reconcileOutcome struct {
	payer        uuid.UUID
	before       string
	after        string
	changed      bool
	statusBefore string
	statusAfter  string

	// owner is the stored row's user when that is not the payer (the upsert
	// never moves a row), else uuid.Nil. The row's write moves the owner's
	// tier, not the payer's, so the owner is pushed too.
	owner       uuid.UUID
	ownerBefore string
	ownerAfter  string
}

// Reconcile fetches subID from Stripe under its advisory lock and writes the
// one subscriptions row for it. With eventID non-empty the dedupe row is
// written in the same transaction. An error wrapping ErrUnactionable means
// "record and acknowledge" (nothing here can use the object); any other error
// means "retry", and nothing was written. After the commit, OnTierChange is
// pushed for the payer when the row was inserted or changed or the tier moved,
// and by the same rule for the row's owner when that is another user; a push
// failure is logged, never returned, because the row is committed.
func (s *Service) Reconcile(ctx context.Context, subID, eventID, eventType string) (changed bool, err error) {
	conn, err := s.lockSubscription(ctx, subID)
	if err != nil {
		return false, err
	}
	defer s.unlockSubscription(ctx, conn, subID)

	out, err := s.reconcileLocked(ctx, conn, subID, eventID, eventType)
	if err != nil {
		return false, err
	}
	s.Log.Info("billing: subscription reconciled",
		logKeySubID, subID,
		"status_before", out.statusBefore,
		"status_after", out.statusAfter,
		"tier_before", out.before,
		"tier_after", out.after,
		"changed", out.changed)
	if out.changed || out.before != out.after {
		s.pushTier(ctx, subID, out.payer, out.before, out.after)
	}
	if out.owner != uuid.Nil && (out.changed || out.ownerBefore != out.ownerAfter) {
		s.pushTier(ctx, subID, out.owner, out.ownerBefore, out.ownerAfter)
	}
	return out.changed, nil
}

// pushTier tells the TierChanger that userID's tier moved. The row is already
// committed, so a failure is logged and not returned.
func (s *Service) pushTier(ctx context.Context, subID string, userID uuid.UUID, before, after string) {
	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tierNotificationTimeout)
	defer cancel()
	if err := s.Changer.OnTierChange(notifyCtx, userID, before, after); err != nil {
		s.Log.Error("billing: tier change push failed after reconcile",
			logKeySubID, subID, logKeyErrorClass, errorClass(err), logKeyStage, stageTierPush)
	}
}

// reconcileLocked fetches and commits under the advisory key Reconcile holds
// through the subsequent tier notifications.
func (s *Service) reconcileLocked(ctx context.Context, conn *sql.Conn, subID, eventID, eventType string) (reconcileOutcome, error) {
	fields, metadata, err := s.fetchSubscription(ctx, subID)
	if err != nil {
		return reconcileOutcome{}, err
	}
	return s.writeSubscription(ctx, conn, reconcileInput{
		subID: subID, eventID: eventID, eventType: eventType, fields: fields, metadata: metadata,
	})
}

// lockSubscription takes a dedicated connection and the session advisory lock
// for subID on it, waiting as long as ctx allows. On any failure the
// connection is discarded rather than pooled: a lock granted just as the wait
// was canceled would otherwise stay held by a pooled session.
func (s *Service) lockSubscription(ctx context.Context, subID string) (*sql.Conn, error) {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return nil, withStage(stageConnAcquire, fmt.Errorf("billing: reconcile: acquire connection: %w", err))
	}
	if _, err := conn.ExecContext(ctx, advisoryLockSQL, subID); err != nil {
		discardConn(conn)
		return nil, withStage(stageAdvisoryLock, fmt.Errorf("billing: reconcile: advisory lock: %w", err))
	}
	return conn, nil
}

// unlockSubscription releases subID's advisory lock and returns the
// connection to the pool, or discards the connection when the unlock fails or
// reports the key was not held, which ends the session and its locks.
func (s *Service) unlockSubscription(ctx context.Context, conn *sql.Conn, subID string) {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
	defer cancel()
	var released bool
	err := conn.QueryRowContext(uctx, advisoryUnlockSQL, subID).Scan(&released)
	switch {
	case err != nil:
		s.Log.Error("billing: advisory unlock failed; discarding the connection",
			logKeySubID, subID, logKeyErrorClass, errorClass(err), logKeyStage, stageAdvisoryUnlock)
	case !released:
		// The unlock ran, but this session did not hold the key: the
		// exclusivity the reconcile relied on was not there.
		s.Log.Error("billing: advisory lock was not held at unlock; discarding the connection",
			logKeySubID, subID, logKeyStage, stageAdvisoryUnlock)
	default:
		if err := conn.Close(); err != nil {
			s.Log.Warn("billing: closing the reconcile connection failed",
				logKeySubID, subID, logKeyErrorClass, errorClass(err), logKeyStage, stageConnClose)
		}
		return
	}
	discardConn(conn)
}

// logRollback logs the result of a billing transaction's deferred Rollback.
// After Commit it is sql.ErrTxDone, which is expected; any other failure is
// logged, because the transaction's own error is already being returned. The
// Rollback call stays in the deferred closure beside BeginTx, so cleanup
// ownership is visible where the transaction opens.
func (s *Service) logRollback(err error, msg string, attrs ...any) {
	if err != nil && !errors.Is(err, sql.ErrTxDone) {
		attrs = append(attrs, logKeyErrorClass, errorClass(err), logKeyStage, stageRollback)
		s.Log.Warn(msg, attrs...)
	}
}

// discardConn removes conn from the pool and closes its session. Returning
// driver.ErrBadConn from Raw is database/sql's signal to drop the connection.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close() // already released by Raw; ErrConnDone is expected
}

// fetchSubscription retrieves subID with its price and discounts expanded,
// never its customer, and classifies it. A subscription Stripe does not have
// (R36) or one with no usable period end (F4) is unactionable: nothing is
// written, so a stored row lapses at its stored period end. Any other Stripe
// failure is returned for a retry.
func (s *Service) fetchSubscription(ctx context.Context, subID string) (rowFields, map[string]string, error) {
	params := &stripe.SubscriptionRetrieveParams{}
	params.AddExpand("items.data.price")
	params.AddExpand("discounts")
	sub, err := s.Stripe.V1Subscriptions.Retrieve(ctx, subID, params)
	if err != nil {
		if isResourceMissing(err) {
			s.Log.Error("billing: subscription is missing in Stripe; nothing written", logKeySubID, subID)
			return rowFields{}, nil, fmt.Errorf("%w: subscription missing in Stripe", ErrUnactionable)
		}
		return rowFields{}, nil, withStage(stageFetchSub, fmt.Errorf("billing: reconcile: retrieve subscription: %w", err))
	}
	fields, err := classify(sub)
	if err != nil {
		s.Log.Error("billing: subscription has no current period end; nothing written", logKeySubID, subID)
		return rowFields{}, nil, fmt.Errorf("%w: no current period end", ErrUnactionable)
	}
	if fields.CustomerID == "" {
		s.Log.Error("billing: subscription has no customer; nothing written", logKeySubID, subID)
		return rowFields{}, nil, fmt.Errorf("%w: no customer", ErrUnactionable)
	}
	return fields, sub.Metadata, nil
}

// writeSubscription runs the one transaction: resolve and lock the payer,
// upsert the row, write the dedupe row, and read the tier on both sides of the
// write, for the payer and for a row owner who is not the payer. It makes no
// Stripe call.
func (s *Service) writeSubscription(ctx context.Context, conn *sql.Conn, in reconcileInput) (reconcileOutcome, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return reconcileOutcome{}, withStage(stageBegin, fmt.Errorf("billing: reconcile: begin: %w", err))
	}
	defer func() {
		s.logRollback(tx.Rollback(), "billing: reconcile rollback failed", logKeySubID, in.subID)
	}()

	payer, err := s.resolvePayer(ctx, tx, in)
	if err != nil {
		return reconcileOutcome{}, err
	}
	out := reconcileOutcome{payer: payer, statusAfter: in.fields.Status}
	if out.before, err = resolveTier(ctx, tx, payer, stageTierBefore); err != nil {
		return reconcileOutcome{}, err
	}
	old, found, err := loadStoredRow(ctx, tx, in.subID)
	if err != nil {
		return reconcileOutcome{}, err
	}
	if found && old.userID != payer {
		out.owner = old.userID
		if out.ownerBefore, err = resolveTier(ctx, tx, out.owner, stageOwnerTierBefore); err != nil {
			return reconcileOutcome{}, err
		}
	}
	if out.changed, out.statusBefore, err = s.upsertRow(ctx, tx, payer, in, old, found); err != nil {
		return reconcileOutcome{}, err
	}
	if err := s.finishWrite(ctx, tx, in, &out); err != nil {
		return reconcileOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return reconcileOutcome{}, withStage(stageCommit, fmt.Errorf("billing: reconcile: commit: %w", err))
	}
	return out, nil
}

// finishWrite writes the dedupe row and reads the tiers after the upsert,
// inside the reconcile's transaction.
func (s *Service) finishWrite(ctx context.Context, tx *sql.Tx, in reconcileInput, out *reconcileOutcome) error {
	if in.eventID != "" {
		if err := insertDedupe(ctx, tx, in.eventID, in.eventType); err != nil {
			return err
		}
	}
	var err error
	if out.after, err = resolveTier(ctx, tx, out.payer, stageTierAfter); err != nil {
		return err
	}
	if out.owner != uuid.Nil {
		if out.ownerAfter, err = resolveTier(ctx, tx, out.owner, stageOwnerTierAfter); err != nil {
			return err
		}
	}
	return s.checkLiveStripeRows(ctx, tx, out.payer, in.subID)
}

// resolveTier reads userID's enforced tier inside tx, tagging a failure with stage.
func resolveTier(ctx context.Context, tx *sql.Tx, userID uuid.UUID, stage string) (string, error) {
	tier, err := entitlements.ResolveTierChecked(ctx, tx, userID.String())
	if err != nil {
		return "", withStage(stage, fmt.Errorf("billing: reconcile: %s: %w", stage, err))
	}
	return tier, nil
}

// resolvePayer finds the user who pays for the subscription and locks that
// user's row FOR UPDATE before anything is written (R-a). A mapped customer's
// user is the payer. An unmapped customer's payer comes from the
// subscription's metadata, and the mapping is then self-healed. A payer whose
// row is gone (erasure committed, or a user that never existed here) is
// unactionable: no row, no mapping, no outbox row and no Stripe call (R31).
func (s *Service) resolvePayer(ctx context.Context, tx *sql.Tx, in reconcileInput) (uuid.UUID, error) {
	payer, mapped, err := s.lookupPayer(ctx, tx, in)
	if err != nil {
		return uuid.Nil, err
	}

	exists, err := lockUser(ctx, tx, payer)
	if err != nil {
		return uuid.Nil, err
	}
	if !exists {
		if mapped {
			s.Log.Warn("billing: payer was erased before reconcile; nothing written", logKeySubID, in.subID)
		} else {
			s.logUnresolvablePayer("billing: subscription metadata names no user here; nothing written", in)
		}
		return uuid.Nil, fmt.Errorf("%w: payer absent", ErrUnactionable)
	}
	if !mapped {
		if err := s.healUnlessTerminal(ctx, tx, payer, in); err != nil {
			return uuid.Nil, err
		}
	}
	return payer, nil
}

// healUnlessTerminal self-heals the mapping for an unmapped customer, except
// when the subscription has ended. An ended subscription can arrive after
// customer.deleted removed the mapping (a late .updated or .deleted event, or
// the drift worker); healing from it would point the payer at a Customer
// Stripe no longer has. The row is still written by the caller, so the end of
// the subscription propagates; only the mapping is withheld.
func (s *Service) healUnlessTerminal(ctx context.Context, tx *sql.Tx, payer uuid.UUID, in reconcileInput) error {
	if isTerminalStatus(in.fields.Status) {
		s.Log.Info("billing: subscription has ended; mapping not restored",
			logKeySubID, in.subID, logKeyCustomerID, in.fields.CustomerID)
		return nil
	}
	return s.selfHealMapping(ctx, tx, payer, in)
}

// isTerminalStatus reports whether a Stripe subscription status is final: the
// subscription cannot entitle again.
func isTerminalStatus(status string) bool {
	return status == string(stripe.SubscriptionStatusCanceled) ||
		status == string(stripe.SubscriptionStatusIncompleteExpired)
}

// lookupPayer returns the mapped user for the subscription's customer, or the
// user its metadata names when the customer is unmapped (mapped=false). It
// takes no lock: the payer's users row is locked by the caller.
func (s *Service) lookupPayer(ctx context.Context, tx *sql.Tx, in reconcileInput) (payer uuid.UUID, mapped bool, err error) {
	err = tx.QueryRowContext(ctx, payerByCustomerSQL, in.fields.CustomerID).Scan(&payer)
	if err == nil {
		return payer, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, false, withStage(stageReadMapping, fmt.Errorf("billing: reconcile: read mapping: %w", err))
	}
	payer, err = s.payerFromMetadata(in)
	return payer, false, err
}

// payerFromMetadata reads the payer of an unmapped customer from the
// subscription's metadata. Another environment's subscription (dev boxes
// share one Stripe test account, RF1) is refused with a warning; a missing
// environment or a missing or malformed user id is refused at the key mode's
// level, since Checkout stamps both on every subscription it creates.
func (s *Service) payerFromMetadata(in reconcileInput) (uuid.UUID, error) {
	env := in.metadata[metadataKeyEnv]
	if env == "" {
		s.logUnresolvablePayer("billing: unmapped subscription has no environment metadata; nothing written", in)
		return uuid.Nil, fmt.Errorf("%w: no environment in metadata", ErrUnactionable)
	}
	if env != s.Environment {
		s.Log.Warn("billing: subscription belongs to another environment; nothing written",
			logKeySubID, in.subID, logKeyCustomerID, in.fields.CustomerID)
		return uuid.Nil, fmt.Errorf("%w: environment mismatch", ErrUnactionable)
	}
	payer, err := uuid.Parse(in.metadata[metadataKeyUserID])
	if err != nil {
		s.logUnresolvablePayer("billing: unmapped subscription has no usable user id; nothing written", in)
		return uuid.Nil, fmt.Errorf("%w: no payer in metadata", ErrUnactionable)
	}
	return payer, nil
}

// logUnresolvablePayer logs an unmapped customer whose payer is not here. In
// live mode it is an error for operator review (runbook); in test mode a
// warning, because every dev box shares one test account. Ids only.
func (s *Service) logUnresolvablePayer(msg string, in reconcileInput) {
	attrs := []any{logKeySubID, in.subID, logKeyCustomerID, in.fields.CustomerID}
	if s.Billing.KeyMode == config.StripeKeyModeLive {
		s.Log.Error(msg, attrs...)
		return
	}
	s.Log.Warn(msg, attrs...)
}

// lockUser locks the user's row FOR UPDATE and reports whether it exists.
func lockUser(ctx context.Context, tx *sql.Tx, userID uuid.UUID) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, lockPayerSQL, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, withStage(stageLockPayer, fmt.Errorf("billing: reconcile: lock payer: %w", err))
	}
	return true, nil
}

// selfHealMapping records the customer for a payer found through metadata, as
// a lost checkout.session.completed would have. A user already mapped to a
// different customer keeps that mapping; the conflict is logged and the
// reconcile continues (F24). An insert that conflicts with the same mapping is
// no conflict: a sibling subscription of the same Customer healed it first
// while this reconcile waited for the payer's row.
func (s *Service) selfHealMapping(ctx context.Context, tx *sql.Tx, payer uuid.UUID, in reconcileInput) error {
	res, err := tx.ExecContext(ctx, selfHealMappingSQL, payer, in.fields.CustomerID)
	if err != nil {
		return withStage(stageSelfHeal, fmt.Errorf("billing: reconcile: self-heal mapping: %w", err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return withStage(stageSelfHeal, fmt.Errorf("billing: reconcile: self-heal mapping: %w", err))
	}
	if n != 0 {
		return nil
	}
	var stored string
	err = tx.QueryRowContext(ctx, customerByUserSQL, payer).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The insert conflicted on the customer id: another user holds it.
		s.Log.Error("billing: customer is already mapped to a different user; mapping kept",
			logKeySubID, in.subID, logKeyCustomerID, in.fields.CustomerID, logKeyUserID, payer.String())
	case err != nil:
		return withStage(stageSelfHeal, fmt.Errorf("billing: reconcile: re-read mapping: %w", err))
	case stored != in.fields.CustomerID:
		s.Log.Error("billing: payer is already mapped to a different customer; mapping kept",
			logKeySubID, in.subID, logKeyCustomerID, in.fields.CustomerID, logKeyUserID, payer.String())
	}
	return nil
}

// storedRow is the part of a subscriptions row Reconcile compares.
type storedRow struct {
	userID    uuid.UUID
	fields    rowFields
	periodEnd sql.NullTime
}

// upsertRow writes the subscription's row and reports whether it was inserted
// or any stored column differs, plus the stored status before the write ("" on
// insert). old and found are the existing row as loadStoredRow read and locked
// it, so the comparison and the write see the same version.
func (s *Service) upsertRow(ctx context.Context, tx *sql.Tx, payer uuid.UUID, in reconcileInput, old storedRow, found bool) (changed bool, statusBefore string, err error) {
	f := in.fields
	if _, err := tx.ExecContext(ctx, upsertRowSQL,
		payer, f.Tier, f.Status, f.CustomerID, in.subID,
		f.PeriodEnd, f.CancelAtPeriodEnd, f.Plan, f.Interval, f.FoundersRate, f.CancelAt,
	); err != nil {
		return false, "", withStage(stageUpsert, fmt.Errorf("billing: reconcile: upsert subscription: %w", err))
	}
	if f.Malformed != "" {
		s.Log.Error("billing: subscription is not a sold plan shape; stored at the free tier",
			logKeySubID, in.subID, logKeyReason, f.Malformed)
	}
	if !found {
		return true, "", nil
	}
	if old.userID != payer {
		s.Log.Error("billing: subscription row belongs to another user than the payer; owner kept",
			logKeySubID, in.subID, logKeyUserID, payer.String())
	}
	return !old.sameAs(f), old.fields.Status, nil
}

// loadStoredRow reads and locks the existing row for subID, if any.
func loadStoredRow(ctx context.Context, tx *sql.Tx, subID string) (storedRow, bool, error) {
	var r storedRow
	var customer sql.NullString
	err := tx.QueryRowContext(ctx, selectStoredRowSQL, subID).Scan(
		&r.userID, &r.fields.Tier, &r.fields.Status, &customer, &r.periodEnd,
		&r.fields.CancelAtPeriodEnd, &r.fields.Plan, &r.fields.Interval, &r.fields.FoundersRate, &r.fields.CancelAt)
	if errors.Is(err, sql.ErrNoRows) {
		return storedRow{}, false, nil
	}
	if err != nil {
		return storedRow{}, false, withStage(stageReadStoredRow, fmt.Errorf("billing: reconcile: read stored row: %w", err))
	}
	r.fields.CustomerID = customer.String
	return r, true, nil
}

// sameAs reports whether every column Reconcile writes already holds f's value.
func (r storedRow) sameAs(f rowFields) bool {
	o := r.fields
	return o.Tier == f.Tier &&
		o.Status == f.Status &&
		o.CustomerID == f.CustomerID &&
		r.periodEnd.Valid && r.periodEnd.Time.Equal(f.PeriodEnd) &&
		o.CancelAtPeriodEnd == f.CancelAtPeriodEnd &&
		o.Plan == f.Plan &&
		o.Interval == f.Interval &&
		o.FoundersRate == f.FoundersRate &&
		sameNullTime(o.CancelAt, f.CancelAt)
}

// sameNullTime compares two nullable instants by value, not by location.
func sameNullTime(a, b sql.NullTime) bool {
	if a.Valid != b.Valid {
		return false
	}
	return !a.Valid || a.Time.Equal(b.Time)
}

// checkLiveStripeRows logs an error when the payer now holds more than one
// live Stripe subscription. Both rows entitle (no per-user uniqueness, R1);
// the log is the operator's cue to refund the duplicate.
func (s *Service) checkLiveStripeRows(ctx context.Context, tx *sql.Tx, payer uuid.UUID, subID string) error {
	var n int
	if err := tx.QueryRowContext(ctx, countLiveStripeRowsSQL, payer).Scan(&n); err != nil {
		return withStage(stageCountLiveRows, fmt.Errorf("billing: reconcile: count live stripe rows: %w", err))
	}
	if n > 1 {
		s.Log.Error("billing: payer holds more than one live Stripe subscription",
			logKeySubID, subID, logKeyUserID, payer.String(), "live_stripe_rows", n)
	}
	return nil
}

// insertDedupe records a processed webhook event. A second insert of the same
// event id is a no-op, so the first writer's type stands.
func insertDedupe(ctx context.Context, q execer, eventID, eventType string) error {
	if _, err := q.ExecContext(ctx, insertDedupeSQL, eventID, eventType); err != nil {
		return withStage(stageDedupeInsert, fmt.Errorf("billing: insert webhook dedupe row: %w", err))
	}
	return nil
}
