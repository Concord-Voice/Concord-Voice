package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
)

// Wing seats (#2033 spec §9, PR-5). A live Wing subscription carries one seat
// for a friend the payer picks. The seat row never grants on its own:
// entitlements.ResolveTierQ reads it through LiveSeatParentPredicate, so a
// parent that lapses, pauses or stops being a Wing suspends the seat with no
// write here. The writes below are the state machine and its bookkeeping.
//
// Lock order for every seat op, and for Reconcile's seat transitions:
//  1. the parent's advisory key (Reconcile holds it as a session lock on its
//     own connection; seat ops take the xact form of the same key);
//  2. users, ascending, FOR UPDATE;
//  3. the parent subscriptions row (FOR SHARE in a seat op);
//  4. the seats.
// It is the order deleteAccountTx and the redemption engine use for users →
// subscriptions, so no seat op can close a cycle with them.

// Seat states (migration 000188).
const (
	SeatStatePending  = "pending"
	SeatStateActive   = "active"
	SeatStateDeclined = "declined"
	SeatStateExpired  = "expired"
	SeatStateRevoked  = "revoked"
	SeatStateLeft     = "left"
)

// Seat end reasons (migration 000188).
const (
	seatEndPayerWithdrew     = "payer_withdrew"
	seatEndPayerRevoked      = "payer_revoked"
	seatEndPayerSwapped      = "payer_swapped"
	seatEndHolderLeft        = "holder_left"
	seatEndHolderSubscribed  = "holder_subscribed"
	seatEndPlanChanged       = "plan_changed"
	seatEndSubscriptionEnded = "subscription_ended"
	seatEndDeclined          = "declined"
)

// Seat WebSocket events (spec §9). Payloads carry ids only; the renderer
// refetches /subscriptions/me and takes names from its friend list.
const (
	EventSeatInvite  = "subscription_seat_invite"
	EventSeatUpdated = "subscription_seat_updated"
)

// Seat error classes, the {"error": ...} values of spec §8.
const (
	seatErrNotEligible    = "invitee_not_eligible"
	seatErrNotPayer       = "not_wing_payer"
	seatErrOccupied       = "seat_occupied"
	seatErrCooldown       = "seat_cooldown"
	seatErrNoSeat         = "no_seat"
	seatErrInviteNotFound = "invite_not_found"
	seatErrInviteExpired  = "invite_expired"
	seatErrOwnSub         = "own_subscription_active"
	seatErrAlreadyMember  = "already_seat_member"
	checkoutErrSeatMember = "seat_member"
)

const (
	logKeySeatID = "seat_id"

	stageSeatBegin    = "seat_begin"
	stageSeatLock     = "seat_lock"
	stageSeatRead     = "seat_read"
	stageSeatWrite    = "seat_write"
	stageSeatTier     = "seat_tier"
	stageSeatCommit   = "seat_commit"
	stageSeatPreClose = "seat_preexpire_checkout"

	// seatPushTimeout bounds one post-commit WebSocket enqueue.
	seatPushTimeout = 5 * time.Second
	// seatPreCloseTimeout bounds accept's best-effort Checkout pre-expiry.
	seatPreCloseTimeout = 10 * time.Second
)

const (
	// The parent's reconcile key, transaction-scoped: it waits for a running
	// Reconcile of the same subscription and ends at this op's commit.
	seatAdvisoryLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('concord.stripe_sub:' || $1::text, 0))`

	lockUsersAscSQL = `SELECT id FROM users WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`

	// payerWingSQL finds the caller's live premium Wing; with several, the
	// most recent. It takes no lock: the op re-checks under its locks.
	payerWingSQL = `SELECT id, stripe_subscription_id FROM subscriptions
		WHERE user_id = $1 AND source = 'stripe' AND plan = 'wing' AND tier = 'premium'
		  AND ` + entitlements.LiveRowPredicate + `
		ORDER BY current_period_end DESC, created_at DESC, id
		LIMIT 1`

	// parentLiveSQL locks the parent FOR SHARE and reports whether it is
	// still the payer's live premium Wing.
	parentLiveSQL = `SELECT (user_id = $2 AND source = 'stripe' AND plan = 'wing' AND tier = 'premium'
		AND ` + entitlements.LiveRowPredicate + `)
		FROM subscriptions WHERE id = $1 FOR SHARE`

	expireStaleInvitesSQL = `UPDATE subscription_seats
		SET state = 'expired', end_reason = 'invite_expired', ended_at = NOW()
		WHERE subscription_id = $1 AND state = 'pending' AND invite_expires_at <= NOW()
		RETURNING id, payer_user_id, holder_user_id`

	// eligibleInviteeSQL: an accepted friendship in either direction, no
	// blocked friendship in either direction, and no DM-block reconciliation
	// pending for the pair (dm_block_reconciliations stores user_a < user_b).
	eligibleInviteeSQL = `SELECT
		EXISTS (SELECT 1 FROM friendships WHERE status = 'accepted'
		        AND ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)))
		AND NOT EXISTS (SELECT 1 FROM friendships WHERE status = 'blocked'
		        AND ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)))
		AND NOT EXISTS (SELECT 1 FROM dm_block_reconciliations
		        WHERE user_a_id = LEAST($1::uuid, $2::uuid) AND user_b_id = GREATEST($1::uuid, $2::uuid))`

	openSeatHolderSQL = `SELECT holder_user_id FROM subscription_seats
		WHERE subscription_id = $1 AND state IN ('pending', 'active')`

	openSeatForUpdateSQL = `SELECT id, holder_user_id, state FROM subscription_seats
		WHERE subscription_id = $1 AND state IN ('pending', 'active') FOR UPDATE`

	// seatCooldownSQL returns when the payer may seat someone other than $2,
	// or NULL when they may now (D5): any seat this payer activated for
	// another holder in the last 30 days blocks, except one that ended because
	// its holder bought their own plan. A leave counts; an erased party's row
	// is gone, so erasure never counts.
	seatCooldownSQL = `SELECT MAX(activated_at) + INTERVAL '30 days' FROM subscription_seats
		WHERE payer_user_id = $1 AND holder_user_id <> $2
		  AND activated_at > NOW() - INTERVAL '30 days'
		  AND end_reason IS DISTINCT FROM 'holder_subscribed'`

	endSeatSQL = `UPDATE subscription_seats SET state = $2, end_reason = $3, ended_at = NOW() WHERE id = $1`

	insertSeatSQL = `INSERT INTO subscription_seats
		(subscription_id, payer_user_id, holder_user_id, state, invite_expires_at)
		VALUES ($1, $2, $3, 'pending', NOW() + INTERVAL '7 days')
		RETURNING id, invite_expires_at`

	// seatByHolderSQL is the caller-scoped load of accept and decline (F1):
	// a wrong caller, a nonexistent id and a non-pending seat all miss alike.
	seatByHolderSQL = `SELECT s.payer_user_id, p.stripe_subscription_id
		FROM subscription_seats s JOIN subscriptions p ON p.id = s.subscription_id
		WHERE s.id = $1 AND s.holder_user_id = $2 AND s.state = 'pending'`

	relockPendingSQL = `SELECT subscription_id, invite_expires_at <= NOW() FROM subscription_seats
		WHERE id = $1 AND holder_user_id = $2 AND state = 'pending' FOR UPDATE`

	ownLiveStripeEndSQL = `SELECT current_period_end FROM subscriptions
		WHERE user_id = $1 AND source = 'stripe' AND ` + entitlements.LiveRowPredicate + `
		ORDER BY current_period_end DESC LIMIT 1`

	activateSeatSQL = `UPDATE subscription_seats SET state = 'active', activated_at = NOW()
		WHERE id = $1 RETURNING invite_expires_at`

	payerOpenSeatSQL = `SELECT s.id, s.holder_user_id, p.stripe_subscription_id
		FROM subscription_seats s JOIN subscriptions p ON p.id = s.subscription_id
		WHERE s.payer_user_id = $1 AND s.state IN ('pending', 'active')
		ORDER BY s.invited_at DESC, s.id LIMIT 1`

	holderActiveSeatSQL = `SELECT s.id, s.payer_user_id, p.stripe_subscription_id
		FROM subscription_seats s JOIN subscriptions p ON p.id = s.subscription_id
		WHERE s.holder_user_id = $1 AND s.state = 'active'`

	relockSeatSQL = `SELECT state FROM subscription_seats WHERE id = $1 FOR UPDATE`

	// heldSeatSQL reports whether the user holds an active seat whose parent
	// entitles (Checkout's seat_member).
	heldSeatSQL = `SELECT EXISTS (SELECT 1 FROM subscription_seats s
		JOIN subscriptions ON subscriptions.id = s.subscription_id
		WHERE s.holder_user_id = $1 AND ` + entitlements.LiveSeatParentPredicate + `)`
)

// errSeatRefused carries a refusal the handler writes as-is.
type errSeatRefused struct {
	status int
	class  string
	body   map[string]any // extra fields beside "error", or nil
}

func (e *errSeatRefused) Error() string { return "billing: seat refused: " + e.class }

func refuse(status int, class string) error { return &errSeatRefused{status: status, class: class} }

// SeatNotifier pushes a seat event to every live session of one user. It is
// an interface so billing does not import the WebSocket hub (internal/api
// adapts it), and tests record what was sent.
type SeatNotifier interface {
	NotifySeat(ctx context.Context, userID uuid.UUID, event string, data map[string]any) bool
}

// seatChange is one seat whose state a committed transaction changed.
type seatChange struct {
	seatID, payer, holder uuid.UUID
	state                 string
}

// tierMove is one user's tier on both sides of a committed transaction.
type tierMove struct {
	user          uuid.UUID
	before, after string
	// force pushes even when before == after: the parent row changed, so the
	// holder's cached entitlement may be stale either way (a lapsed parent
	// resolves free on both sides of its terminal reconcile).
	force bool
}

// seatOutcome is what a committed seat transaction owes its parties.
type seatOutcome struct {
	changes []seatChange
	tiers   []tierMove
	invite  *seatInvite
	// expiresAt is the accepted seat's invite expiry, for the response only.
	expiresAt time.Time
}

// seatInvite is a new pending seat, pushed to its invitee.
type seatInvite struct {
	seatID, holder uuid.UUID
	expiresAt      time.Time
}

// SeatDTO is the seat a seat endpoint returns.
type SeatDTO struct {
	SeatID          string `json:"seat_id"`
	State           string `json:"state"`
	PayerUserID     string `json:"payer_user_id"`
	HolderUserID    string `json:"holder_user_id"`
	InviteExpiresAt string `json:"invite_expires_at"`
}

// runSeatTx runs fn in one transaction under the seat lock order's first two
// steps: the parent's advisory key, then the named users ascending. fn gets
// the set of those users that still exist. Nothing is committed when fn
// returns an error.
func (s *Service) runSeatTx(ctx context.Context, stripeSubID string, users []uuid.UUID,
	fn func(tx *sql.Tx, present map[uuid.UUID]bool) (seatOutcome, error),
) (seatOutcome, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return seatOutcome{}, withStage(stageSeatBegin, fmt.Errorf("billing: seat: begin: %w", err))
	}
	defer func() {
		s.logRollback(tx.Rollback(), "billing: seat rollback failed")
	}()
	if _, err := tx.ExecContext(ctx, seatAdvisoryLockSQL, stripeSubID); err != nil {
		return seatOutcome{}, withStage(stageSeatLock, fmt.Errorf("billing: seat: advisory lock: %w", err))
	}
	present, err := lockUsersAsc(ctx, tx, users)
	if err != nil {
		return seatOutcome{}, err
	}
	out, err := fn(tx, present)
	if err != nil {
		return seatOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return seatOutcome{}, withStage(stageSeatCommit, fmt.Errorf("billing: seat: commit: %w", err))
	}
	return out, nil
}

// lockUsersAsc locks the users' rows FOR UPDATE in ascending id order and
// returns the ones that exist.
func lockUsersAsc(ctx context.Context, tx *sql.Tx, users []uuid.UUID) (present map[uuid.UUID]bool, err error) {
	ids := make([]string, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.String())
	}
	sort.Strings(ids) // the query orders too; sorting keeps the array stable for logs and tests
	rows, err := tx.QueryContext(ctx, lockUsersAscSQL, pq.Array(ids))
	if err != nil {
		return nil, withStage(stageSeatLock, fmt.Errorf("billing: seat: lock users: %w", err))
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = withStage(stageSeatLock, fmt.Errorf("billing: seat: close locked users: %w", cerr))
		}
	}()
	present = make(map[uuid.UUID]bool, len(users))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, withStage(stageSeatLock, fmt.Errorf("billing: seat: scan locked user: %w", err))
		}
		present[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, withStage(stageSeatLock, fmt.Errorf("billing: seat: lock users: %w", err))
	}
	return present, nil
}

// seatTier reads userID's enforced tier inside tx.
func seatTier(ctx context.Context, tx *sql.Tx, userID uuid.UUID) (string, error) {
	tier, err := entitlements.ResolveTierChecked(ctx, tx, userID.String())
	if err != nil {
		return "", withStage(stageSeatTier, fmt.Errorf("billing: seat: resolve tier: %w", err))
	}
	return tier, nil
}

// endSeat moves one open seat to a terminal state.
func endSeat(ctx context.Context, tx *sql.Tx, seatID uuid.UUID, state, reason string) error {
	if _, err := tx.ExecContext(ctx, endSeatSQL, seatID, state, reason); err != nil {
		return withStage(stageSeatWrite, fmt.Errorf("billing: seat: end seat: %w", err))
	}
	return nil
}

// cooldownUntil returns when payer may seat invitee, or a zero time when now.
func cooldownUntil(ctx context.Context, tx *sql.Tx, payer, invitee uuid.UUID) (time.Time, error) {
	var until sql.NullTime
	if err := tx.QueryRowContext(ctx, seatCooldownSQL, payer, invitee).Scan(&until); err != nil {
		return time.Time{}, withStage(stageSeatRead, fmt.Errorf("billing: seat: read cooldown: %w", err))
	}
	if !until.Valid {
		return time.Time{}, nil
	}
	return until.Time, nil
}

// cooldownRefusal is the 409 seat_cooldown with its available_at.
func cooldownRefusal(until time.Time) error {
	return &errSeatRefused{status: 409, class: seatErrCooldown,
		body: map[string]any{"available_at": until.UTC().Format(time.RFC3339)}}
}

// parentLive locks the parent row FOR SHARE and reports whether it is still
// payer's live premium Wing. A missing row is not live.
func parentLive(ctx context.Context, tx *sql.Tx, subRowID, payer uuid.UUID) (bool, error) {
	var live bool
	err := tx.QueryRowContext(ctx, parentLiveSQL, subRowID, payer).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, withStage(stageSeatRead, fmt.Errorf("billing: seat: lock parent: %w", err))
	}
	return live, nil
}

// expireStaleInvites materializes the parent's lapsed pending invite, if any.
func expireStaleInvites(ctx context.Context, tx *sql.Tx, subRowID uuid.UUID) (changes []seatChange, err error) {
	rows, err := tx.QueryContext(ctx, expireStaleInvitesSQL, subRowID)
	if err != nil {
		return nil, withStage(stageSeatWrite, fmt.Errorf("billing: seat: expire invites: %w", err))
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = withStage(stageSeatWrite, fmt.Errorf("billing: seat: close expired invites: %w", cerr))
		}
	}()
	for rows.Next() {
		c := seatChange{state: SeatStateExpired}
		if err := rows.Scan(&c.seatID, &c.payer, &c.holder); err != nil {
			return nil, withStage(stageSeatWrite, fmt.Errorf("billing: seat: scan expired invite: %w", err))
		}
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, withStage(stageSeatWrite, fmt.Errorf("billing: seat: expire invites: %w", err))
	}
	return changes, nil
}

// afterSeatCommit tells each party what a committed seat transaction did:
// OnTierChange for every tier that moved (or is forced), then subscription_seat_updated to
// both sides of each changed seat, then the invite to a new invitee. Every
// failure is logged and never returned; the transaction has committed.
func (s *Service) afterSeatCommit(ctx context.Context, out seatOutcome) {
	for _, m := range out.tiers {
		if m.before == m.after && !m.force {
			continue
		}
		// The locks are gone by now, so a later seat op may already have
		// committed and pushed. Push the tier as it stands, not as this
		// transaction left it, so a delayed push cannot restore a stale one.
		// ponytail: read-then-push still leaves a sub-millisecond window; a
		// per-user push sequence would close it if that ever matters.
		notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tierNotificationTimeout)
		if cur, err := entitlements.ResolveTierChecked(notifyCtx, s.DB, m.user.String()); err == nil {
			m.after = cur
		} else {
			s.Log.Warn("billing: tier re-read before a seat push failed; pushing the committed tier",
				logKeyErrorClass, errorClass(err))
		}
		if err := s.Changer.OnTierChange(notifyCtx, m.user, m.before, m.after); err != nil {
			s.Log.Error("billing: tier change push failed after a seat change",
				logKeyErrorClass, errorClass(err), logKeyStage, stageTierPush)
		}
		cancel()
	}
	for _, c := range out.changes {
		data := map[string]any{"seat_id": c.seatID.String(), "state": c.state}
		s.notifySeat(ctx, c.payer, EventSeatUpdated, data)
		s.notifySeat(ctx, c.holder, EventSeatUpdated, data)
	}
	if out.invite != nil {
		s.notifySeat(ctx, out.invite.holder, EventSeatInvite, map[string]any{
			"invite_id":  out.invite.seatID.String(),
			"expires_at": out.invite.expiresAt.UTC().Format(time.RFC3339),
		})
	}
}

// notifySeat sends one seat event, if a notifier is wired.
func (s *Service) notifySeat(ctx context.Context, userID uuid.UUID, event string, data map[string]any) {
	if s.Seats == nil {
		return
	}
	pushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), seatPushTimeout)
	defer cancel()
	if !s.Seats.NotifySeat(pushCtx, userID, event, data) {
		s.Log.Warn("billing: seat event push was not enqueued", "event", event)
	}
}

// holdsLiveSeat reports whether userID holds an active seat on an entitling
// parent. It takes no lock (Checkout's advisory 409).
func (s *Service) holdsLiveSeat(ctx context.Context, userID uuid.UUID) (bool, error) {
	var held bool
	if err := s.DB.QueryRowContext(ctx, heldSeatSQL, userID).Scan(&held); err != nil {
		return false, withStage(stageSeatRead, fmt.Errorf("billing: seat: read held seat: %w", err))
	}
	return held, nil
}

// ErasedSeat is one seat an account erasure's cascade removed, with the
// surviving party (spec §10 step 2). Holder is true when the survivor held an
// active seat the erased user paid for.
type ErasedSeat struct {
	SeatID, Survivor uuid.UUID
	Holder           bool
}

// OnSeatsErased tells the survivors of seats an erasure removed, after its
// commit: a holder whose tier fell gets OnTierChange(premium, after), and
// every survivor gets subscription_seat_updated. The seat rows are gone, so
// the state sent is the one the survivor now sees: revoked for a holder, left
// for a payer. Best effort; failures are logged.
func (s *Service) OnSeatsErased(ctx context.Context, seats []ErasedSeat) {
	for _, seat := range seats {
		state := SeatStateLeft
		if seat.Holder {
			state = SeatStateRevoked
			after, err := entitlements.ResolveTierChecked(ctx, s.DB, seat.Survivor.String())
			if err != nil {
				s.Log.Error("billing: could not read a seat holder's tier after an erasure",
					logKeyErrorClass, errorClass(err), logKeyStage, stageSeatTier)
			} else {
				s.afterSeatCommit(ctx, seatOutcome{tiers: []tierMove{
					{user: seat.Survivor, before: entitlements.TierPremium, after: after},
				}})
			}
		}
		s.notifySeat(ctx, seat.Survivor, EventSeatUpdated,
			map[string]any{"seat_id": seat.SeatID.String(), "state": state})
	}
}
