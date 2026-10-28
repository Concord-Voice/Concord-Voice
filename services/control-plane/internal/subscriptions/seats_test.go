package subscriptions_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/subscriptions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wing seats on GET /subscriptions/me (#2033 PR-5).

// insertWing writes a live premium Wing row for payer and returns its id.
func insertWing(t *testing.T, db *sql.DB, payer uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.QueryRow(`INSERT INTO subscriptions
		(user_id, tier, status, source, current_period_end, stripe_customer_id, stripe_subscription_id, plan, billing_interval)
		VALUES ($1, 'premium', 'active', 'stripe', $2, $3, $4, 'wing', 'year') RETURNING id`,
		payer, time.Now().Add(30*24*time.Hour), "cus_"+uuid.NewString(), "sub_"+uuid.NewString()).Scan(&id))
	return id
}

func insertSeat(t *testing.T, db *sql.DB, sub, payer, holder uuid.UUID, state string, expiresIn time.Duration) uuid.UUID {
	t.Helper()
	var activated any
	if state == "active" {
		activated = time.Now().Add(-time.Hour)
	}
	var id uuid.UUID
	require.NoError(t, db.QueryRow(`INSERT INTO subscription_seats
		(subscription_id, payer_user_id, holder_user_id, state, invite_expires_at, activated_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		sub, payer, holder, state, time.Now().Add(expiresIn), activated).Scan(&id))
	return id
}

func getMeAs(t *testing.T, h *subscriptions.Handler, user uuid.UUID) map[string]any {
	t.Helper()
	r := gin.New()
	path := "/me/" + user.String()
	r.GET(path, authAs(user), h.GetMe)
	w := doGet(r, path)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func TestGetMe_SeatMemberAndPayer(t *testing.T) {
	h, _, db := newTestHandler(t)
	payer := testhelpers.CreateUser(t, db)
	holder := testhelpers.CreateUser(t, db)
	sub := insertWing(t, db, payer)
	seat := insertSeat(t, db, sub, payer, holder, "active", time.Hour)
	end := time.Now().Add(time.Hour)
	insertSubscription(t, db, holder, "premium", "active", "code", &end)

	member := getMeAs(t, h, holder)
	assert.Equal(t, "premium", member["tier"])
	assert.Equal(t, "seat", member["source"], "the seat comes before a code row")
	assert.NotNil(t, member["code"])
	assert.Nil(t, member["stripe"])
	assert.Equal(t, map[string]any{
		"role": "member", "seat_id": seat.String(), "state": "active", "counterpart_user_id": payer.String(),
	}, member["seat"])

	p := getMeAs(t, h, payer)
	ps, ok := p["seat"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "payer", ps["role"])
	assert.Equal(t, holder.String(), ps["counterpart_user_id"])
	assert.NotEmpty(t, ps["next_swap_at"], "a seat activated an hour ago starts the cooldown")
}

func TestGetMe_InvitesAndExpiry(t *testing.T) {
	h, _, db := newTestHandler(t)
	payer := testhelpers.CreateUser(t, db)
	holder := testhelpers.CreateUser(t, db)
	sub := insertWing(t, db, payer)
	seat := insertSeat(t, db, sub, payer, holder, "pending", 24*time.Hour)

	me := getMeAs(t, h, holder)
	assert.Equal(t, "free", me["tier"])
	invites, ok := me["invites"].([]any)
	require.True(t, ok)
	require.Len(t, invites, 1)
	inv, ok := invites[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, seat.String(), inv["invite_id"])
	assert.Nil(t, inv["blocked"])

	end := time.Now().Add(time.Hour)
	insertSubscription(t, db, holder, "premium", "active", "stripe", &end)
	me = getMeAs(t, h, holder)
	invites, ok = me["invites"].([]any)
	require.True(t, ok)
	require.Len(t, invites, 1)
	inv, ok = invites[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "own_subscription_active", inv["blocked"])

	_, err := db.Exec(`UPDATE subscription_seats SET invite_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, seat)
	require.NoError(t, err)
	assert.Nil(t, getMeAs(t, h, holder)["invites"], "an expired invite is not offered")
	ps, ok := getMeAs(t, h, payer)["seat"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "expired", ps["state"])
	assert.Nil(t, ps["next_swap_at"], "a never-activated invite starts no cooldown")
}

func TestGetMe_PausedParentSuspendsTheSeat(t *testing.T) {
	h, _, db := newTestHandler(t)
	payer := testhelpers.CreateUser(t, db)
	holder := testhelpers.CreateUser(t, db)
	sub := insertWing(t, db, payer)
	seat := insertSeat(t, db, sub, payer, holder, "active", time.Hour)
	_, err := db.Exec(`UPDATE subscriptions SET status = 'paused' WHERE id = $1`, sub)
	require.NoError(t, err)

	me := getMeAs(t, h, holder)
	assert.Equal(t, "free", me["tier"])
	assert.NotEqual(t, "seat", me["source"], "a suspended seat grants nothing")
	assert.Equal(t, map[string]any{
		"role": "member", "seat_id": seat.String(), "state": "active", "counterpart_user_id": payer.String(),
		"suspended": true,
	}, me["seat"], "the member can still see and leave a suspended seat")
	assert.Nil(t, getMeAs(t, h, payer)["seat"], "a paused Wing is not a live Wing")
}
