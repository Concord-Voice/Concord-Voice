package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Seats the friendship no longer supports (follow-up to #4133). An unfriend or
// a block ends a pending invite; a block by either side ends an active seat,
// and an unfriend alone leaves it running (Mark, 2026-10-29). Every such end
// carries the one reason seatEndPairIneligible, so the row never says which.
//
// None of this runs inside the friendship transaction. friends.BlockUser locks
// users before anything else, and a seat op locks the parent's advisory key
// before users, so a seat write inside the block would close a cycle. Instead
// friends calls OnPairChanged after its commit, and each seat ends in its own
// runSeatTx that re-reads the pair under the seat locks. The drift pass is the
// durable backstop: the friendship row is the record, so a hook lost to a crash
// is caught on the next pass.

const (
	seatEndPairIneligible = "pair_ineligible"

	// pairOpenSeatsSQL lists the open seats between $1 and $2, either way round.
	pairOpenSeatsSQL = `SELECT s.id, s.payer_user_id, s.holder_user_id, p.stripe_subscription_id
		FROM subscription_seats s JOIN subscriptions p ON p.id = s.subscription_id
		WHERE s.state IN ('pending', 'active')
		  AND ((s.payer_user_id = $1 AND s.holder_user_id = $2) OR (s.payer_user_id = $2 AND s.holder_user_id = $1))`

	// pairBlockedSQL: a blocked friendship row in either direction.
	pairBlockedSQL = `SELECT EXISTS (SELECT 1 FROM friendships WHERE status = 'blocked'
		AND ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)))`

	// unsupportedSeatsSQL is the drift pass's candidate list, the same rule
	// endUnsupportedSeat applies: a pending seat whose pair has no accepted
	// friendship, has a block, or has a DM-block reconciliation pending, and
	// an active seat whose pair has a block. It must not admit a seat the
	// decision keeps (an active seat after an unfriend): ORDER BY id LIMIT
	// would return those same no-ops every pass and starve the seats behind
	// them. It is a pooled pre-read; each seat is decided again under the
	// seat locks. $1 is the batch size.
	unsupportedSeatsSQL = `SELECT s.id, s.payer_user_id, s.holder_user_id, p.stripe_subscription_id
		FROM subscription_seats s JOIN subscriptions p ON p.id = s.subscription_id
		WHERE (s.state IN ('pending', 'active') AND EXISTS (SELECT 1 FROM friendships f WHERE f.status = 'blocked'
				AND ((f.requester_id = s.payer_user_id AND f.addressee_id = s.holder_user_id)
				  OR (f.requester_id = s.holder_user_id AND f.addressee_id = s.payer_user_id))))
		   OR (s.state = 'pending' AND (
			NOT EXISTS (SELECT 1 FROM friendships f WHERE f.status = 'accepted'
				AND ((f.requester_id = s.payer_user_id AND f.addressee_id = s.holder_user_id)
				  OR (f.requester_id = s.holder_user_id AND f.addressee_id = s.payer_user_id)))
			OR EXISTS (SELECT 1 FROM dm_block_reconciliations r
				WHERE r.user_a_id = LEAST(s.payer_user_id, s.holder_user_id)
				  AND r.user_b_id = GREATEST(s.payer_user_id, s.holder_user_id))))
		ORDER BY s.id LIMIT $1`
)

// pairSeat is one open seat between a pair, as a pooled read saw it.
type pairSeat struct {
	seatID, payer, holder uuid.UUID
	stripeSubID           string
}

// OnPairChanged ends the open seats between a and b that their friendship no
// longer supports. friends calls it after an unfriend (blocker uuid.Nil) or a
// block (blocker is the user who blocked) commits. The friendship change has
// already succeeded, so a failure is logged, not returned; the drift pass
// retries it.
func (s *Service) OnPairChanged(ctx context.Context, a, b, blocker uuid.UUID) {
	if err := s.endPairSeats(ctx, a, b, blocker); err != nil {
		s.Log.Warn("billing: ending seats after a friendship change failed",
			logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
	}
}

func (s *Service) endPairSeats(ctx context.Context, a, b, blocker uuid.UUID) error {
	seats, err := s.querySeats(ctx, pairOpenSeatsSQL, a, b)
	if err != nil {
		return err
	}
	var errs []error
	for _, seat := range seats {
		if _, err := s.endUnsupportedSeat(ctx, seat, blocker); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// endUnsupportedSeats is the drift stage: it ends a batch of open seats the
// pair no longer supports. The sweep cannot tell who blocked (a blocked row
// keeps the friendship's original requester), so an active seat it ends is the
// payer's revoke.
func (s *Service) endUnsupportedSeats(ctx context.Context, report *DriftReport) error {
	seats, err := s.querySeats(ctx, unsupportedSeatsSQL, driftBatch)
	if err != nil {
		return withStage("end_unsupported_seats", err)
	}
	var errs []error
	for _, seat := range seats {
		ended, err := s.endUnsupportedSeat(ctx, seat, uuid.Nil)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ended {
			report.PairEnded++
		}
	}
	return errors.Join(errs...)
}

// endUnsupportedSeat decides one seat under the seat lock order and ends it
// when the pair, read under those locks, no longer supports it. ended is false
// when the seat moved first or the pair supports it again.
func (s *Service) endUnsupportedSeat(ctx context.Context, seat pairSeat, blocker uuid.UUID) (ended bool, err error) {
	out, err := s.runSeatTx(ctx, seat.stripeSubID, []uuid.UUID{seat.payer, seat.holder},
		func(tx *sql.Tx, _ map[uuid.UUID]bool) (seatOutcome, error) {
			var eligible, blocked bool
			if err := tx.QueryRowContext(ctx, eligibleInviteeSQL, seat.payer, seat.holder).Scan(&eligible); err != nil {
				return seatOutcome{}, withStage(stageSeatRead, err)
			}
			if err := tx.QueryRowContext(ctx, pairBlockedSQL, seat.payer, seat.holder).Scan(&blocked); err != nil {
				return seatOutcome{}, withStage(stageSeatRead, err)
			}
			return s.endOpenSeatLocked(ctx, tx, seat.seatID, seat.payer, seat.holder, func(state string) (string, string, bool) {
				switch {
				case state == SeatStatePending && !eligible:
					return SeatStateRevoked, seatEndPairIneligible, true
				case state == SeatStateActive && blocked && blocker == seat.holder:
					return SeatStateLeft, seatEndPairIneligible, true
				case state == SeatStateActive && blocked:
					return SeatStateRevoked, seatEndPairIneligible, true
				}
				return "", "", false
			})
		})
	var refused *errSeatRefused
	if errors.As(err, &refused) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.afterSeatCommit(ctx, out)
	return true, nil
}

// querySeats runs a pooled read of pairSeat rows.
func (s *Service) querySeats(ctx context.Context, query string, args ...any) (seats []pairSeat, err error) {
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, withStage(stageSeatRead, fmt.Errorf("billing: seat: read pair seats: %w", err))
	}
	defer func() {
		err = errors.Join(err, rows.Close())
	}()
	for rows.Next() {
		var p pairSeat
		if err := rows.Scan(&p.seatID, &p.payer, &p.holder, &p.stripeSubID); err != nil {
			return nil, withStage(stageSeatRead, fmt.Errorf("billing: seat: scan pair seat: %w", err))
		}
		seats = append(seats, p)
	}
	if err := rows.Err(); err != nil {
		return nil, withStage(stageSeatRead, fmt.Errorf("billing: seat: read pair seats: %w", err))
	}
	return seats, nil
}
