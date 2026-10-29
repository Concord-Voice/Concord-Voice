package billing

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
)

// Regression for the accept-time eligibility gap in Wing +1 seats (follow-up to
// #4102). Eligibility (an accepted friendship, no block, no pending DM-block
// reconciliation) was read only when the invite was created, so a payer who
// unfriended or blocked the invitee while the invite was pending still paid for
// the invitee's seat the moment they accepted.
//
// Oracle: once the pair stops being eligible, accept writes the same 404
// invite_not_found a missing invite gets, the seat stays pending, and the
// invitee gains no premium.

// blockPairTx writes a block the way friends.BlockUser does: the friendship
// becomes blocked in place (or a blocked row is inserted when there is none) and
// the DM reconciliation evidence the 000153 trigger demands is recorded in the
// same transaction.
func blockPairTx(t *testing.T, tx *sql.Tx, blocker, blocked uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	res, err := tx.ExecContext(ctx, `UPDATE friendships SET status = 'blocked', updated_at = NOW()
		WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)`, blocker, blocked)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	if n == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'blocked')`, blocker, blocked)
		require.NoError(t, err)
	}
	require.NoError(t, dmblock.RecordBlockTx(ctx, tx, blocker.String(), blocked.String(), uuid.NewString()))
}

func blockPair(t *testing.T, db *sql.DB, blocker, blocked uuid.UUID) {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	blockPairTx(t, tx, blocker, blocked)
	require.NoError(t, tx.Commit())
}

func unfriend(t *testing.T, db *sql.DB, a, b uuid.UUID) {
	t.Helper()
	res, err := db.Exec(`DELETE FROM friendships
		WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)`, a, b)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "the fixture must delete the friendship, not a no-op")
}

func pairState(t *testing.T, db *sql.DB, a, b uuid.UUID) (accepted, blocked, reconciling int) {
	t.Helper()
	q := func(query string) int { return scalarInt(t, db, query, a, b) }
	return q(`SELECT COUNT(*) FROM friendships WHERE status = 'accepted'
			AND ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))`),
		q(`SELECT COUNT(*) FROM friendships WHERE status = 'blocked'
			AND ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))`),
		q(`SELECT COUNT(*) FROM dm_block_reconciliations
			WHERE user_a_id = LEAST($1::uuid, $2::uuid) AND user_b_id = GREATEST($1::uuid, $2::uuid)`)
}

func TestSeatAccept_InviteeIneligibleSinceInvite_IsRefusedLikeAMissingInvite(t *testing.T) {
	type fixture struct {
		change func(t *testing.T, env *seatEnv, payer, holder uuid.UUID)
		// accepted, blocked, reconciling: what the fixture must leave behind, so
		// each case can only be refused by the arm of eligibleInviteeSQL it names.
		accepted, blocked, reconciling int
	}
	cases := map[string]fixture{
		"unfriended": {
			change:   func(t *testing.T, env *seatEnv, payer, holder uuid.UUID) { unfriend(t, env.DB, payer, holder) },
			accepted: 0, blocked: 0, reconciling: 0,
		},
		"blocked by the payer": {
			change:   func(t *testing.T, env *seatEnv, payer, holder uuid.UUID) { blockPair(t, env.DB, payer, holder) },
			accepted: 0, blocked: 1, reconciling: 1,
		},
		"blocked by the holder": {
			change:   func(t *testing.T, env *seatEnv, payer, holder uuid.UUID) { blockPair(t, env.DB, holder, payer) },
			accepted: 0, blocked: 1, reconciling: 1,
		},
		// Once the reconciler drains its obligation only the blocked friendship
		// row is left to say so.
		"blocked, reconciliation drained": {
			change: func(t *testing.T, env *seatEnv, payer, holder uuid.UUID) {
				blockPair(t, env.DB, payer, holder)
				_, err := env.DB.Exec(`DELETE FROM dm_block_reconciliations
					WHERE user_a_id = LEAST($1::uuid, $2::uuid) AND user_b_id = GREATEST($1::uuid, $2::uuid)`, payer, holder)
				require.NoError(t, err)
			},
			accepted: 0, blocked: 1, reconciling: 0,
		},
		// Only the third arm of eligibleInviteeSQL: the friendship is still
		// accepted and no friendship row is blocked.
		"dm block reconciliation pending": {
			change: func(t *testing.T, env *seatEnv, payer, holder uuid.UUID) {
				a, b := payer, holder
				if b.String() < a.String() {
					a, b = b, a
				}
				_, err := env.DB.Exec(`INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a)
					VALUES ($1, $2, $3, TRUE)`, a, b, uuid.New())
				require.NoError(t, err)
			},
			accepted: 1, blocked: 0, reconciling: 1,
		},
	}
	for name, fx := range cases {
		t.Run(name, func(t *testing.T) {
			env := newSeatEnv(t)
			payer, _ := env.wingPayer(t)
			holder := env.friend(t, payer)
			seatID := invitedSeat(t, env.Svc, payer, holder)

			fx.change(t, env, payer, holder)
			accepted, blocked, reconciling := pairState(t, env.DB, payer, holder)
			require.Equal(t, []int{fx.accepted, fx.blocked, fx.reconciling}, []int{accepted, blocked, reconciling},
				"the fixture left the pair in a different state than the case names")

			missing := accept(env.Svc, holder, uuid.NewString())
			require.Equal(t, http.StatusNotFound, missing.Code)

			w := accept(env.Svc, holder, seatID)
			requireError(t, w, http.StatusNotFound, seatErrInviteNotFound)
			assert.Equal(t, missing.Body.String(), w.Body.String(), "an ineligible accept reads like a missing invite")

			state, reason := seatState(t, env.DB, seatID)
			assert.Equal(t, SeatStatePending, state, "accept refused without activating the seat")
			assert.False(t, reason.Valid)
			assert.Equal(t, 0, scalarInt(t, env.DB, `SELECT COUNT(*) FROM subscription_seats WHERE state = 'active'`))
			assert.Equal(t, entitlements.TierFree, tierOf(t, env.DB, holder), "the invitee gains no premium")
			assert.Empty(t, env.Seats.For(payer, EventSeatUpdated), "nothing committed, so nothing is pushed")
			assert.NotContains(t, env.Changer.Calls(), tierCall{UserID: holder, Old: entitlements.TierFree, New: entitlements.TierPremium})
		})
	}
}

// The control for the cases above: eligibility is read at accept time, so a
// pair that is eligible again accepts. A fix that refused every invite whose
// friendship was ever touched would pass the refusals and fail here.
func TestSeatAccept_InviteeEligibleAgainAtAccept_Succeeds(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)
	holder := env.friend(t, payer)
	seatID := invitedSeat(t, env.Svc, payer, holder)

	unfriend(t, env.DB, payer, holder)
	requireError(t, accept(env.Svc, holder, seatID), http.StatusNotFound, seatErrInviteNotFound)

	befriend(t, env.DB, payer, holder)
	w := accept(env.Svc, holder, seatID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, entitlements.TierPremium, tierOf(t, env.DB, holder))
}

// The re-check must read under the users locks accept already holds. A block
// holds the users rows FOR NO KEY UPDATE until it commits; accept takes them
// FOR UPDATE, so it waits, and what it reads afterwards is the block. A check
// made before those locks would read the friendship as it stood before the
// block and activate the seat.
func TestSeatAccept_EligibilityIsReadUnderTheUserLocks(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)
	holder := env.friend(t, payer)
	seatID := invitedSeat(t, env.Svc, payer, holder)

	// An in-flight block, locked as friends.BlockUser locks it, not committed.
	tx, err := env.DB.Begin()
	require.NoError(t, err)
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback: %v", err)
		}
	}()
	require.NoError(t, dmblock.LockBlockSubjectsTx(context.Background(), tx, []uuid.UUID{payer, holder}))
	blockPairTx(t, tx, payer, holder)

	done := make(chan int, 1)
	go func() { done <- accept(env.Svc, holder, seatID).Code }()
	waitForUsersLockWaiter(t, env.DB)

	require.NoError(t, tx.Commit())
	assert.Equal(t, http.StatusNotFound, <-done, "accept read the committed block, not the pre-block friendship")
	state, _ := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStatePending, state)
}

// A failed eligibility read ends the accept in an error the handler writes as a
// 503, never as a seat and never as a 4xx the invitee could read as a verdict.
func TestSeatAccept_EligibilityReadFailureFailsClosed(t *testing.T) {
	env := newSeatEnv(t)
	payer, subID := env.wingPayer(t)
	holder := env.friend(t, payer)
	seatID := invitedSeat(t, env.Svc, payer, holder)

	ctx := context.Background()
	_, err := env.Svc.runSeatTx(ctx, subID, []uuid.UUID{payer, holder},
		func(tx *sql.Tx, present map[uuid.UUID]bool) (seatOutcome, error) {
			// Break only the eligibility read, which names this table. The rename
			// happens in the seat transaction, so the error rolls it back.
			_, execErr := tx.ExecContext(ctx, `ALTER TABLE dm_block_reconciliations RENAME TO dm_block_reconciliations_gone`)
			require.NoError(t, execErr)
			return env.Svc.acceptLocked(ctx, tx, present, uuid.MustParse(seatID), payer, holder)
		})

	require.Error(t, err)
	var refused *errSeatRefused
	assert.False(t, errors.As(err, &refused), "a failed read is not a refusal")
	assert.Equal(t, stageSeatRead, errorStage(err))
	state, _ := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStatePending, state)
	assert.Equal(t, 1, scalarInt(t, env.DB,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'dm_block_reconciliations'`),
		"the rename rolled back with the failed accept")
}

// waitForUsersLockWaiter returns once a backend is blocked on the users-row
// lock accept takes, so the caller releases a lock accept is provably waiting
// on rather than one it has not reached yet.
func waitForUsersLockWaiter(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n := scalarInt(t, db, `SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND query LIKE '%FROM users WHERE id = ANY%FOR UPDATE%'`)
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("accept never waited on the users-row lock")
}
