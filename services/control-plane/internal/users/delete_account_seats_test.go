package users

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// Erasure cascades the user's Wing seats (#2033 PR-5) and tells each survivor
// after the commit: an erased payer's active holder may lose premium, an
// erased holder's payer gets the seat back.
func TestDeleteAccountReportsErasedWingSeats(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	payer, holder, otherPayer := testdb.CreateUser(t, db), testdb.CreateUser(t, db), testdb.CreateUser(t, db)
	activeSeat := insertErasureSeat(t, db, insertErasureWing(t, db, payer), payer, holder, "active")
	pendingSeat := insertErasureSeat(t, db, insertErasureWing(t, db, otherPayer), otherPayer, payer, "pending")

	service, _ := newAccountServiceWithRail(t, db)
	var got []ErasedSeat
	service.SetErasedSeatNotifier(func(_ context.Context, seats []ErasedSeat) { got = seats })

	require.NoError(t, service.DeleteAccount(testdb.ActorContext(context.Background(), t, db, payer.String()), payer.String()))

	assert.ElementsMatch(t, []ErasedSeat{
		{SeatID: activeSeat, Survivor: holder, Holder: true},
		{SeatID: pendingSeat, Survivor: otherPayer, Holder: false},
	}, got)
	var left int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM subscription_seats`).Scan(&left))
	assert.Equal(t, 0, left, "both seats cascade with the erased user")
}

func insertErasureWing(t *testing.T, db *sql.DB, payer uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.QueryRow(`INSERT INTO subscriptions
		(user_id, tier, status, source, current_period_end, stripe_customer_id, stripe_subscription_id, plan)
		VALUES ($1, 'premium', 'active', 'stripe', $2, $3, $4, 'wing') RETURNING id`,
		payer, time.Now().Add(24*time.Hour), "cus_"+uuid.NewString(), "sub_"+uuid.NewString()).Scan(&id))
	return id
}

func insertErasureSeat(t *testing.T, db *sql.DB, sub, payer, holder uuid.UUID, state string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.QueryRow(`INSERT INTO subscription_seats
		(subscription_id, payer_user_id, holder_user_id, state, invite_expires_at, activated_at)
		VALUES ($1, $2, $3, $4::text, NOW() + INTERVAL '7 days', CASE WHEN $4::text = 'active' THEN NOW() END)
		RETURNING id`, sub, payer, holder, state).Scan(&id))
	return id
}
