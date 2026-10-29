package billing

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
)

// Seats the friendship no longer supports (follow-up to #4133). An unfriend or
// a block ends a pending invite, and a block by either side ends an active
// seat; an unfriend alone leaves an active seat running (Mark, 2026-10-29).
// Before this, a stale invite lingered until day 7 and an active seat kept
// billing the payer until they removed it by hand.
//
// Oracle: the drift pass, the durable backstop, ends each such seat with
// pair_ineligible, the holder of an ended active seat drops to free, and both
// parties get the seat update.

func TestDrift_EndsSeatsThePairNoLongerSupports(t *testing.T) {
	cases := map[string]struct {
		active bool
		change func(t *testing.T, env *seatEnv, payer, holder uuid.UUID)
	}{
		"pending, unfriended": {
			change: func(t *testing.T, env *seatEnv, payer, holder uuid.UUID) { unfriend(t, env.DB, payer, holder) },
		},
		"pending, blocked by the payer": {
			change: func(t *testing.T, env *seatEnv, payer, holder uuid.UUID) { blockPair(t, env.DB, payer, holder) },
		},
		"pending, blocked by the holder": {
			change: func(t *testing.T, env *seatEnv, payer, holder uuid.UUID) { blockPair(t, env.DB, holder, payer) },
		},
		"active, blocked by the payer": {
			active: true,
			change: func(t *testing.T, env *seatEnv, payer, holder uuid.UUID) { blockPair(t, env.DB, payer, holder) },
		},
		"active, blocked by the holder": {
			active: true,
			change: func(t *testing.T, env *seatEnv, payer, holder uuid.UUID) { blockPair(t, env.DB, holder, payer) },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newSeatEnv(t)
			payer, _ := env.wingPayer(t)
			var holder uuid.UUID
			var seatID string
			if tc.active {
				holder, seatID = env.seatedFriend(t, payer)
			} else {
				holder = env.friend(t, payer)
				seatID = invitedSeat(t, env.Svc, payer, holder)
			}
			pushed := len(env.Seats.For(payer, EventSeatUpdated))

			tc.change(t, env, payer, holder)
			report, err := env.Svc.RunDrift(context.Background())
			require.NoError(t, err)
			assert.Equal(t, 1, report.PairEnded)

			state, reason := seatState(t, env.DB, seatID)
			// The sweep cannot tell who blocked (a blocked friendship row keeps
			// its original requester), so it ends as the payer's revoke; the
			// post-commit hook knows the blocker and ends a holder's block as left.
			assert.Equal(t, SeatStateRevoked, state)
			assert.Equal(t, "pair_ineligible", reason.String)
			assert.Equal(t, entitlements.TierFree, tierOf(t, env.DB, holder))
			assert.Len(t, env.Seats.For(payer, EventSeatUpdated), pushed+1, "the payer is told")
		})
	}
}

// The controls. An unfriend alone leaves an active seat running, and a pair
// that is friends again keeps its invite: the sweep reads the pair as it is
// now, not as it ever was.
func TestDrift_LeavesSeatsThePairStillSupports(t *testing.T) {
	t.Run("active, unfriended", func(t *testing.T) {
		env := newSeatEnv(t)
		payer, _ := env.wingPayer(t)
		holder, seatID := env.seatedFriend(t, payer)
		unfriend(t, env.DB, payer, holder)

		_, err := env.Svc.RunDrift(context.Background())
		require.NoError(t, err)
		state, _ := seatState(t, env.DB, seatID)
		assert.Equal(t, SeatStateActive, state)
		assert.Equal(t, entitlements.TierPremium, tierOf(t, env.DB, holder))
	})
	t.Run("pending, friends again", func(t *testing.T) {
		env := newSeatEnv(t)
		payer, _ := env.wingPayer(t)
		holder := env.friend(t, payer)
		seatID := invitedSeat(t, env.Svc, payer, holder)
		unfriend(t, env.DB, payer, holder)
		befriend(t, env.DB, payer, holder)

		_, err := env.Svc.RunDrift(context.Background())
		require.NoError(t, err)
		state, _ := seatState(t, env.DB, seatID)
		assert.Equal(t, SeatStatePending, state)
	})
}

// The candidate list carries only seats the decision would end. An active seat
// whose pair merely unfriended stays, so it must not be a candidate: a batch of
// them sorted first would fill every pass and starve a blocked seat behind it.
func TestDrift_KeptSeatsDoNotStarveTheBatch(t *testing.T) {
	env := newSeatEnv(t)
	for range driftBatch {
		payer, _ := env.wingPayer(t)
		holder, _ := env.seatedFriend(t, payer)
		unfriend(t, env.DB, payer, holder)
	}
	payer, _ := env.wingPayer(t)
	holder, _ := env.seatedFriend(t, payer)
	blockPair(t, env.DB, payer, holder)
	// Sort the blocked seat after every kept one.
	seatID := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	_, err := env.DB.Exec(`UPDATE subscription_seats SET id = $1 WHERE holder_user_id = $2`, seatID, holder)
	require.NoError(t, err)

	report, err := env.Svc.RunDrift(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, report.PairEnded)
	state, _ := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStateRevoked, state)
}

// The post-commit hook, which friends calls with the blocker. It knows who
// blocked, so a holder's block ends the seat as their leave and a payer's as
// their revoke.
func TestOnPairChanged_EndsTheSeatAsTheBlockerWould(t *testing.T) {
	cases := map[string]struct {
		active    bool
		byHolder  bool
		unfriend  bool
		wantState string
	}{
		"pending, unfriended":        {unfriend: true, wantState: SeatStateRevoked},
		"pending, blocked by holder": {byHolder: true, wantState: SeatStateRevoked},
		"active, blocked by payer":   {active: true, wantState: SeatStateRevoked},
		"active, blocked by holder":  {active: true, byHolder: true, wantState: SeatStateLeft},
		"active, unfriended (stays)": {active: true, unfriend: true, wantState: SeatStateActive},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newSeatEnv(t)
			payer, _ := env.wingPayer(t)
			var holder uuid.UUID
			var seatID string
			if tc.active {
				holder, seatID = env.seatedFriend(t, payer)
			} else {
				holder = env.friend(t, payer)
				seatID = invitedSeat(t, env.Svc, payer, holder)
			}
			actor, other, blocker := payer, holder, payer
			if tc.byHolder {
				actor, other, blocker = holder, payer, holder
			}
			if tc.unfriend {
				unfriend(t, env.DB, actor, other)
				blocker = uuid.Nil
			} else {
				blockPair(t, env.DB, actor, other)
			}

			env.Svc.OnPairChanged(context.Background(), actor, other, blocker)

			state, reason := seatState(t, env.DB, seatID)
			assert.Equal(t, tc.wantState, state)
			if tc.wantState == SeatStateActive {
				assert.False(t, reason.Valid)
				assert.Equal(t, entitlements.TierPremium, tierOf(t, env.DB, holder))
				return
			}
			assert.Equal(t, seatEndPairIneligible, reason.String)
			assert.Equal(t, entitlements.TierFree, tierOf(t, env.DB, holder))
		})
	}
}

// An ended active seat counts toward the payer's 30-day cooldown like any
// other end, so block and unblock cannot rotate seats past it.
func TestOnPairChanged_BlockEndCountsTowardCooldown(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)
	holder, _ := env.seatedFriend(t, payer)
	blockPair(t, env.DB, payer, holder)
	env.Svc.OnPairChanged(context.Background(), payer, holder, payer)

	other := env.friend(t, payer)
	w := invite(env.Svc, payer, other, false)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), `"error":"`+seatErrCooldown+`"`)
}

// The decision is read under the seat locks. An in-flight block holds the
// users rows until it commits; the hook's seat transaction waits on them and
// then reads the block. A read taken before those locks would see the pair as
// friends and leave the invite open.
func TestOnPairChanged_ReadsThePairUnderTheSeatLocks(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)
	holder := env.friend(t, payer)
	seatID := invitedSeat(t, env.Svc, payer, holder)

	tx, err := env.DB.Begin()
	require.NoError(t, err)
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback: %v", err)
		}
	}()
	require.NoError(t, dmblock.LockBlockSubjectsTx(context.Background(), tx, []uuid.UUID{payer, holder}))
	blockPairTx(t, tx, payer, holder)

	done := make(chan error, 1)
	go func() { done <- env.Svc.endPairSeats(context.Background(), payer, holder, payer) }()
	waitForUsersLockWaiter(t, env.DB)

	require.NoError(t, tx.Commit())
	require.NoError(t, <-done)
	state, reason := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStateRevoked, state)
	assert.Equal(t, seatEndPairIneligible, reason.String)
}

// A failed read is an error with its stage, and OnPairChanged only logs it:
// the friendship change already committed.
func TestOnPairChanged_FailedReadIsReportedNotRaised(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)
	holder := env.friend(t, payer)
	seatID := invitedSeat(t, env.Svc, payer, holder)
	unfriend(t, env.DB, payer, holder)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := env.Svc.endPairSeats(ctx, payer, holder, uuid.Nil)
	require.Error(t, err)
	assert.Equal(t, stageSeatRead, errorStage(err))
	env.Svc.OnPairChanged(ctx, payer, holder, uuid.Nil)

	state, _ := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStatePending, state, "nothing was written")
}
