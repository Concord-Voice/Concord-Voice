package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
)

// Reconcile's seat half (#2033 spec §7 steps 4 and 7, PR-5).
//
// Step 4 locks, beside the payer, every user whose seat this reconcile may
// end: the holder of this subscription's open seat, and the payer of an
// active seat the payer holds elsewhere. The first is stable once Reconcile
// holds this subscription's advisory key, because every seat op on it takes
// the same key. The second is stable once the payer's users row is locked,
// because accepting a seat locks the holder's row. Both are read before the
// lock, so the set is read again under it; a seat accepted in between makes
// the reconcile fail with a retryable error rather than touch a user it has
// not locked.
//
// Step 7 materializes the transitions the join already enforces:
//   - the subscription ended → its open seat is revoked (subscription_ended);
//   - it is a sold plan other than Wing → revoked (plan_changed);
//   - the payer now has a live own Stripe row while holding an active seat
//     elsewhere → that seat is left (holder_subscribed, F5, R33), which the
//     cooldown ignores.
// paused and unpaid suspend through the join and are not materialized.

const (
	stageSeatParties = "seat_parties"
	stageSeatReconc  = "seat_reconcile"

	seatPartiesSQL = `SELECT s.holder_user_id FROM subscription_seats s
		  JOIN subscriptions p ON p.id = s.subscription_id
		 WHERE p.stripe_subscription_id = $1 AND s.state IN ('pending', 'active')
		UNION
		SELECT payer_user_id FROM subscription_seats WHERE holder_user_id = $2 AND state = 'active'`

	ownOpenSeatSQL = `SELECT s.id, s.holder_user_id, s.state FROM subscription_seats s
		  JOIN subscriptions p ON p.id = s.subscription_id
		 WHERE p.stripe_subscription_id = $1 AND s.state IN ('pending', 'active')
		   FOR UPDATE OF s`

	// activeHolderSQL reads without a lock: before the upsert, the seat row
	// must not be locked ahead of the subscriptions row (lock order step 4).
	activeHolderSQL = `SELECT s.holder_user_id FROM subscription_seats s
		  JOIN subscriptions p ON p.id = s.subscription_id
		 WHERE p.stripe_subscription_id = $1 AND s.state = 'active'`

	heldActiveSeatSQL = `SELECT id, payer_user_id FROM subscription_seats
		WHERE holder_user_id = $1 AND state = 'active' FOR UPDATE`

	ownLiveStripeExistsSQL = `SELECT EXISTS (SELECT 1 FROM subscriptions
		WHERE user_id = $1 AND source = 'stripe' AND ` + entitlements.LiveRowPredicate + `)`
)

// errSeatPartiesChanged means a seat changed hands between Reconcile's
// unlocked read of the seat parties and its lock; the reconcile is retried.
var errSeatPartiesChanged = errors.New("billing: reconcile: seat parties changed before the lock")

// seatParties reads the users a reconcile of subID may move a seat for.
func seatParties(ctx context.Context, tx *sql.Tx, subID string, payer uuid.UUID) (ids []uuid.UUID, err error) {
	rows, err := tx.QueryContext(ctx, seatPartiesSQL, subID, payer)
	if err != nil {
		return nil, withStage(stageSeatParties, fmt.Errorf("billing: reconcile: read seat parties: %w", err))
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = withStage(stageSeatParties, fmt.Errorf("billing: reconcile: close seat parties: %w", cerr))
		}
	}()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, withStage(stageSeatParties, fmt.Errorf("billing: reconcile: scan seat party: %w", err))
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, withStage(stageSeatParties, fmt.Errorf("billing: reconcile: read seat parties: %w", err))
	}
	return ids, nil
}

// lockPayerAndSeatParties locks the payer and the seat parties ascending
// (step 4) and reports whether the payer still exists, with the set it locked.
func lockPayerAndSeatParties(ctx context.Context, tx *sql.Tx, subID string, payer uuid.UUID) (bool, map[uuid.UUID]bool, error) {
	parties, err := seatParties(ctx, tx, subID, payer)
	if err != nil {
		return false, nil, err
	}
	present, err := lockUsersAsc(ctx, tx, append(parties, payer))
	if err != nil {
		return false, nil, withStage(stageLockPayer, err)
	}
	locked := make(map[uuid.UUID]bool, len(parties)+1)
	locked[payer] = true
	for _, p := range parties {
		locked[p] = true
	}
	return present[payer], locked, nil
}

// seatHolderBefore returns the active holder of subID's seat and their tier
// before the upsert, or uuid.Nil when the seat has no active holder.
func seatHolderBefore(ctx context.Context, tx *sql.Tx, subID string) (uuid.UUID, string, error) {
	var holder uuid.UUID
	err := tx.QueryRowContext(ctx, activeHolderSQL, subID).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, "", nil
	}
	if err != nil {
		return uuid.Nil, "", withStage(stageSeatReconc, fmt.Errorf("billing: reconcile: read seat: %w", err))
	}
	before, err := seatTier(ctx, tx, holder)
	return holder, before, err
}

// reconcileSeats is step 7, after the upsert and inside its transaction.
// holder and holderBefore are seatHolderBefore's answer.
func (s *Service) reconcileSeats(ctx context.Context, tx *sql.Tx, in reconcileInput, payer uuid.UUID,
	locked map[uuid.UUID]bool, holder uuid.UUID, holderBefore string, rowChanged bool,
) (seatOutcome, error) {
	parties, err := seatParties(ctx, tx, in.subID, payer)
	if err != nil {
		return seatOutcome{}, err
	}
	for _, p := range parties {
		if !locked[p] {
			return seatOutcome{}, withStage(stageSeatParties, errSeatPartiesChanged)
		}
	}
	var out seatOutcome
	if reason := seatEndFor(in.fields); reason != "" {
		if err := s.endOwnSeat(ctx, tx, &out, in.subID, payer, reason); err != nil {
			return seatOutcome{}, err
		}
	}
	if err := s.endHeldSeatIfSubscribed(ctx, tx, &out, payer); err != nil {
		return seatOutcome{}, err
	}
	if holder != uuid.Nil {
		after, err := seatTier(ctx, tx, holder)
		if err != nil {
			return seatOutcome{}, err
		}
		// Pushed by the payer's rule: whenever the parent row changed, not
		// only when the tier moved.
		out.tiers = append(out.tiers, tierMove{user: holder, before: holderBefore, after: after, force: rowChanged})
	}
	return out, nil
}

// seatEndFor names why this subscription's open seat ends, or "" when it
// stays. A malformed row (no recognised plan) does not end the seat: the join
// suspends it while the row's tier is free, and a corrected row restores it.
func seatEndFor(f rowFields) string {
	switch {
	case isTerminalStatus(f.Status):
		return seatEndSubscriptionEnded
	case f.Plan.Valid && f.Plan.String != PlanWing:
		return seatEndPlanChanged
	}
	return ""
}

// endOwnSeat revokes subID's open seat, if any, for reason. The holder's tier
// is tracked by the caller, since the upsert alone may already have moved it.
func (s *Service) endOwnSeat(ctx context.Context, tx *sql.Tx, out *seatOutcome, subID string, payer uuid.UUID, reason string) error {
	var seatID, holder uuid.UUID
	var state string
	err := tx.QueryRowContext(ctx, ownOpenSeatSQL, subID).Scan(&seatID, &holder, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return withStage(stageSeatReconc, fmt.Errorf("billing: reconcile: read seat: %w", err))
	}
	if err := endSeat(ctx, tx, seatID, SeatStateRevoked, reason); err != nil {
		return err
	}
	out.changes = append(out.changes, seatChange{seatID: seatID, payer: payer, holder: holder, state: SeatStateRevoked})
	s.Log.Info("billing: seat ended by its subscription", logKeySubID, subID, logKeySeatID, seatID.String(), logKeyReason, reason)
	return nil
}

// endHeldSeatIfSubscribed ends the payer's active seat elsewhere when the
// payer now has a live own Stripe row (F5). The payer's own tier is already
// tracked by Reconcile.
func (s *Service) endHeldSeatIfSubscribed(ctx context.Context, tx *sql.Tx, out *seatOutcome, payer uuid.UUID) error {
	var seatID, seatPayer uuid.UUID
	err := tx.QueryRowContext(ctx, heldActiveSeatSQL, payer).Scan(&seatID, &seatPayer)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return withStage(stageSeatReconc, fmt.Errorf("billing: reconcile: read held seat: %w", err))
	}
	var subscribed bool
	if err := tx.QueryRowContext(ctx, ownLiveStripeExistsSQL, payer).Scan(&subscribed); err != nil {
		return withStage(stageSeatReconc, fmt.Errorf("billing: reconcile: read own live row: %w", err))
	}
	if !subscribed {
		return nil
	}
	if err := endSeat(ctx, tx, seatID, SeatStateLeft, seatEndHolderSubscribed); err != nil {
		return err
	}
	out.changes = append(out.changes, seatChange{seatID: seatID, payer: seatPayer, holder: payer, state: SeatStateLeft})
	s.Log.Info("billing: seat ended because its holder subscribed", logKeySeatID, seatID.String())
	return nil
}
