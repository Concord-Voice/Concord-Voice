package billing

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// Wing seat integration tests (#2033 spec §9, §15 "Seats (PR-5)"). The real
// handlers and Reconcile run against the test database and the Stripe fake.

type seatEvent struct {
	User  uuid.UUID
	Event string
	Data  map[string]any
}

type recordingSeats struct {
	mu     sync.Mutex
	events []seatEvent
}

func (r *recordingSeats) NotifySeat(_ context.Context, userID uuid.UUID, event string, data map[string]any) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, seatEvent{User: userID, Event: event, Data: data})
	return true
}

func (r *recordingSeats) For(userID uuid.UUID, event string) []seatEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []seatEvent
	for _, e := range r.events {
		if e.User == userID && e.Event == event {
			out = append(out, e)
		}
	}
	return out
}

type seatEnv struct {
	*testEnv
	Seats *recordingSeats
}

func newSeatEnv(t *testing.T) *seatEnv {
	t.Helper()
	env := newTestService(t, newFakeStripe(t))
	seats := &recordingSeats{}
	env.Svc.Seats = seats
	return &seatEnv{testEnv: env, Seats: seats}
}

// wingPayer creates a user paying for a live Wing yearly (row and fake).
func (e *seatEnv) wingPayer(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	subID := "sub_wing_" + uuid.NewString()[:8]
	cus := "cus_" + uuid.NewString()[:8]
	payer := newPayer(t, e.testEnv, cus)
	seedStripeSub(t, e.DB, payer, subID, cus, "active", time.Unix(testPeriodEnd, 0))
	seedFakeSub(t, e.Fake, subID, cus, nil)
	return payer, subID
}

func befriend(t *testing.T, db *sql.DB, a, b uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)
}

func (e *seatEnv) friend(t *testing.T, payer uuid.UUID) uuid.UUID {
	t.Helper()
	u := dbtest.CreateUser(t, e.DB)
	befriend(t, e.DB, payer, u)
	return u
}

func seatCall(svc *Service, handler func(*Service, *gin.Context), method, path, id string, userID uuid.UUID, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	if id != "" {
		c.Params = gin.Params{{Key: "id", Value: id}}
	}
	c.Set("user_id", userID.String())
	handler(svc, c)
	return w
}

func invite(svc *Service, payer, invitee uuid.UUID, replace bool) *httptest.ResponseRecorder {
	body := `{"user_id":"` + invitee.String() + `"}`
	if replace {
		body = `{"user_id":"` + invitee.String() + `","replace":true}`
	}
	return seatCall(svc, (*Service).SeatInvite, http.MethodPost, "/billing/seat/invite", "", payer, body)
}

func accept(svc *Service, holder uuid.UUID, seatID string) *httptest.ResponseRecorder {
	return seatCall(svc, (*Service).SeatAccept, http.MethodPost, "/billing/seat-invites/x/accept", seatID, holder, "")
}

func decline(svc *Service, holder uuid.UUID, seatID string) *httptest.ResponseRecorder {
	return seatCall(svc, (*Service).SeatDecline, http.MethodPost, "/billing/seat-invites/x/decline", seatID, holder, "")
}

func revoke(svc *Service, payer uuid.UUID) *httptest.ResponseRecorder {
	return seatCall(svc, (*Service).SeatRevoke, http.MethodDelete, "/billing/seat", "", payer, "")
}

func leave(svc *Service, holder uuid.UUID) *httptest.ResponseRecorder {
	return seatCall(svc, (*Service).SeatLeave, http.MethodPost, "/billing/seat/leave", "", holder, "")
}

// invitedSeat invites invitee and returns the new seat id.
func invitedSeat(t *testing.T, svc *Service, payer, invitee uuid.UUID) string {
	t.Helper()
	w := invite(svc, payer, invitee, false)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var resp struct{ Seat SeatDTO }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Seat.SeatID
}

// seatedFriend invites and accepts, returning the holder and seat id.
func (e *seatEnv) seatedFriend(t *testing.T, payer uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	holder := e.friend(t, payer)
	seatID := invitedSeat(t, e.Svc, payer, holder)
	w := accept(e.Svc, holder, seatID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return holder, seatID
}

func seatState(t *testing.T, db *sql.DB, seatID string) (state string, reason sql.NullString) {
	t.Helper()
	require.NoError(t, db.QueryRow(`SELECT state, end_reason FROM subscription_seats WHERE id = $1`, seatID).Scan(&state, &reason))
	return state, reason
}

// backdateActivation moves a seat's activation into the past by d.
func backdateActivation(t *testing.T, db *sql.DB, seatID string, d time.Duration) {
	t.Helper()
	_, err := db.Exec(`UPDATE subscription_seats SET activated_at = NOW() - make_interval(secs => $2),
		invited_at = NOW() - make_interval(secs => $2) WHERE id = $1`, seatID, d.Seconds())
	require.NoError(t, err)
}

func tierOf(t *testing.T, db *sql.DB, user uuid.UUID) string {
	t.Helper()
	tier, err := entitlements.ResolveTierChecked(context.Background(), db, user.String())
	require.NoError(t, err)
	return tier
}

func TestSeat_InviteAcceptGrantsPremiumAndPushesBothSides(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)
	holder := env.friend(t, payer)

	seatID := invitedSeat(t, env.Svc, payer, holder)
	invites := env.Seats.For(holder, EventSeatInvite)
	require.Len(t, invites, 1)
	assert.Equal(t, seatID, invites[0].Data["invite_id"])
	assert.Equal(t, entitlements.TierFree, tierOf(t, env.DB, holder), "a pending invite grants nothing")

	w := accept(env.Svc, holder, seatID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, entitlements.TierPremium, tierOf(t, env.DB, holder))
	assert.Contains(t, env.Changer.Calls(), tierCall{UserID: holder, Old: entitlements.TierFree, New: entitlements.TierPremium})
	assert.Len(t, env.Seats.For(payer, EventSeatUpdated), 1)
	assert.Len(t, env.Seats.For(holder, EventSeatUpdated), 1)
}

func TestSeat_InviteRefusalsShareOneBody(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)

	stranger := dbtest.CreateUser(t, env.DB)
	blocked := env.friend(t, payer)
	a, b := payer, blocked
	if b.String() < a.String() {
		a, b = b, a
	}
	_, err := env.DB.Exec(`INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a)
		VALUES ($1, $2, $3, TRUE)`, a, b, uuid.New())
	require.NoError(t, err)

	cases := map[string]uuid.UUID{
		"stranger":    stranger,
		"nonexistent": uuid.New(),
		"self":        payer,
		"blocked":     blocked,
	}
	bodies := make([]string, 0, len(cases))
	for name, target := range cases {
		w := invite(env.Svc, payer, target, false)
		require.Equal(t, http.StatusNotFound, w.Code, name)
		bodies = append(bodies, w.Body.String())
	}
	for _, body := range bodies {
		assert.Equal(t, `{"error":"invitee_not_eligible"}`, body)
	}
	assert.Zero(t, countTable(t, env.DB, "subscription_seats"))
}

func TestSeat_InviteNeedsALiveWing(t *testing.T) {
	env := newSeatEnv(t)
	payer := newPayer(t, env.testEnv, "cus_ss")
	_, err := env.DB.Exec(`INSERT INTO subscriptions (user_id, tier, status, source, stripe_customer_id,
		stripe_subscription_id, current_period_end, plan, billing_interval)
		VALUES ($1, 'premium', 'active', 'stripe', 'cus_ss', 'sub_ss', $2, 'supersonic', 'month')`,
		payer, time.Unix(testPeriodEnd, 0))
	require.NoError(t, err)
	friend := env.friend(t, payer)
	requireError(t, invite(env.Svc, payer, friend, false), http.StatusConflict, seatErrNotPayer)
}

func TestSeat_AcceptAndDeclineAreCallerScoped(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)
	holder := env.friend(t, payer)
	third := dbtest.CreateUser(t, env.DB)
	seatID := invitedSeat(t, env.Svc, payer, holder)

	missing := accept(env.Svc, holder, uuid.NewString())
	require.Equal(t, http.StatusNotFound, missing.Code)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"payer accepts":     accept(env.Svc, payer, seatID),
		"third accepts":     accept(env.Svc, third, seatID),
		"third declines":    decline(env.Svc, third, seatID),
		"payer declines":    decline(env.Svc, payer, seatID),
		"malformed accepts": accept(env.Svc, holder, "not-a-uuid"),
	} {
		assert.Equal(t, http.StatusNotFound, w.Code, name)
		assert.Equal(t, missing.Body.String(), w.Body.String(), name)
	}
	state, _ := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStatePending, state)

	w := decline(env.Svc, holder, seatID)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	state, reason := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStateDeclined, state)
	assert.Equal(t, seatEndDeclined, reason.String)
	assert.Equal(t, http.StatusNotFound, accept(env.Svc, holder, seatID).Code, "a declined invite is gone")
}

func TestSeat_CooldownBoundaryAndSameHolderExemption(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)
	first, firstSeat := env.seatedFriend(t, payer)
	next := env.friend(t, payer)

	backdateActivation(t, env.DB, firstSeat, 30*24*time.Hour-time.Hour)
	w := invite(env.Svc, payer, next, true)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"error":"seat_cooldown"`)
	assert.Contains(t, w.Body.String(), `"available_at"`)
	state, _ := seatState(t, env.DB, firstSeat)
	assert.Equal(t, SeatStateActive, state, "a refused swap leaves the old seat intact")
	assert.Equal(t, entitlements.TierPremium, tierOf(t, env.DB, first))

	backdateActivation(t, env.DB, firstSeat, 30*24*time.Hour+time.Minute)
	w = invite(env.Svc, payer, next, true)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	state, reason := seatState(t, env.DB, firstSeat)
	assert.Equal(t, SeatStateRevoked, state)
	assert.Equal(t, seatEndPayerSwapped, reason.String)
	assert.Equal(t, entitlements.TierFree, tierOf(t, env.DB, first))
	assert.Contains(t, env.Changer.Calls(), tierCall{UserID: first, Old: entitlements.TierPremium, New: entitlements.TierFree})

	// next was never activated, so nothing blocks first's return once the
	// pending invite is withdrawn.
	require.Equal(t, http.StatusNoContent, revoke(env.Svc, payer).Code)
	w = invite(env.Svc, payer, first, false)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

func TestSeat_SwapLocksTheDisplacedHolder(t *testing.T) {
	env := newSeatEnv(t)
	payer, subID := env.wingPayer(t)
	first, firstSeat := env.seatedFriend(t, payer)
	backdateActivation(t, env.DB, firstSeat, 31*24*time.Hour)
	next := env.friend(t, payer)

	// Hold the displaced holder's row, as a concurrent accept would.
	tx, err := env.DB.Begin()
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT id FROM users WHERE id = $1 FOR UPDATE`, first)
	require.NoError(t, err)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- invite(env.Svc, payer, next, true) }()
	select {
	case w := <-done:
		t.Fatalf("swap finished without the displaced holder's lock: %d %s", w.Code, w.Body.String())
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	w := <-done
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	// A holder read before locking that no longer matches is refused intact.
	var subRowID uuid.UUID
	require.NoError(t, env.DB.QueryRow(`SELECT id FROM subscriptions WHERE stripe_subscription_id = $1`, subID).Scan(&subRowID))
	other := env.friend(t, payer)
	ctx := context.Background()
	_, err = env.Svc.runSeatTx(ctx, subID, []uuid.UUID{payer, other, first},
		func(tx *sql.Tx, present map[uuid.UUID]bool) (seatOutcome, error) {
			return env.Svc.inviteLocked(ctx, tx, present, subRowID, payer, other, first, true)
		})
	var refused *errSeatRefused
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, seatErrOccupied, refused.class)
}

func TestSeat_DelayedPushSendsTheCurrentTier(t *testing.T) {
	env := newSeatEnv(t)
	holder := dbtest.CreateUser(t, env.DB)
	// An accept's push arriving after a later revoke committed: the outcome
	// says premium, the database now says free.
	env.Svc.afterSeatCommit(context.Background(), seatOutcome{
		tiers: []tierMove{{user: holder, before: entitlements.TierFree, after: entitlements.TierPremium}},
	})
	assert.Contains(t, env.Changer.Calls(), tierCall{UserID: holder, Old: entitlements.TierFree, New: entitlements.TierFree})
	assert.NotContains(t, env.Changer.Calls(), tierCall{UserID: holder, Old: entitlements.TierFree, New: entitlements.TierPremium})
}

func TestSeat_LeaveCountsTowardCooldown(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)
	holder, seatID := env.seatedFriend(t, payer)

	require.Equal(t, http.StatusNoContent, leave(env.Svc, holder).Code)
	state, reason := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStateLeft, state)
	assert.Equal(t, seatEndHolderLeft, reason.String)
	assert.Equal(t, entitlements.TierFree, tierOf(t, env.DB, holder))

	other := env.friend(t, payer)
	w := invite(env.Svc, payer, other, false)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	w = invite(env.Svc, payer, holder, false)
	assert.Equal(t, http.StatusCreated, w.Code, "re-inviting the same holder is exempt: %s", w.Body.String())
	requireError(t, leave(env.Svc, other), http.StatusNotFound, seatErrNoSeat)
}

func TestSeat_OccupiedAndRevoke(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)
	holder, seatID := env.seatedFriend(t, payer)
	other := env.friend(t, payer)

	requireError(t, invite(env.Svc, payer, other, false), http.StatusConflict, seatErrOccupied)
	require.Equal(t, http.StatusNoContent, revoke(env.Svc, payer).Code)
	state, reason := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStateRevoked, state)
	assert.Equal(t, seatEndPayerRevoked, reason.String)
	assert.Equal(t, entitlements.TierFree, tierOf(t, env.DB, holder))
	requireError(t, revoke(env.Svc, payer), http.StatusNotFound, seatErrNoSeat)
}

func TestSeat_AcceptRefusesADoublePay(t *testing.T) {
	for _, canceling := range []bool{false, true} {
		env := newSeatEnv(t)
		payer, _ := env.wingPayer(t)
		holder := env.friend(t, payer)
		seatID := invitedSeat(t, env.Svc, payer, holder)
		cus := "cus_holder_" + uuid.NewString()[:8]
		seedMapping(t, env.DB, holder, cus)
		_, err := env.DB.Exec(`INSERT INTO subscriptions (user_id, tier, status, source, stripe_customer_id,
			stripe_subscription_id, current_period_end, plan, billing_interval, cancel_at_period_end)
			VALUES ($1, 'premium', 'active', 'stripe', $2, $3, $4, 'supersonic', 'month', $5)`,
			holder, cus, "sub_"+cus, time.Unix(testPeriodEnd, 0), canceling)
		require.NoError(t, err)

		w := accept(env.Svc, holder, seatID)
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), `"error":"own_subscription_active"`)
		assert.Contains(t, w.Body.String(), `"current_period_end":"2100-01-01T00:00:00Z"`)
	}
}

func TestSeat_ExpiredInviteIsGoneAndDriftMaterializesIt(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)
	holder := env.friend(t, payer)
	seatID := invitedSeat(t, env.Svc, payer, holder)
	_, err := env.DB.Exec(`UPDATE subscription_seats SET invite_expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, seatID)
	require.NoError(t, err)

	requireError(t, accept(env.Svc, holder, seatID), http.StatusGone, seatErrInviteExpired)
	report, err := env.Svc.RunDrift(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, report.Expired)
	state, reason := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStateExpired, state)
	assert.Equal(t, "invite_expired", reason.String)
	assert.Len(t, env.Seats.For(payer, EventSeatUpdated), 1)
}

func TestSeat_ConcurrentAcceptsSeatOnce(t *testing.T) {
	env := newSeatEnv(t)
	payerA, _ := env.wingPayer(t)
	payerB, _ := env.wingPayer(t)
	holder := env.friend(t, payerA)
	befriend(t, env.DB, payerB, holder)
	seatA := invitedSeat(t, env.Svc, payerA, holder)
	seatB := invitedSeat(t, env.Svc, payerB, holder)

	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, id := range []string{seatA, seatB} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			codes <- accept(env.Svc, holder, id).Code
		}(id)
	}
	wg.Wait()
	close(codes)
	var got []int
	for c := range codes {
		got = append(got, c)
	}
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, got)
	assert.Equal(t, 1, scalarInt(t, env.DB, `SELECT COUNT(*) FROM subscription_seats WHERE holder_user_id = $1 AND state = 'active'`, holder))
}

func TestSeat_CheckoutRefusesASeatMember(t *testing.T) {
	env := newCheckoutEnv(t, foundersConfig(config.BillingPurchaseModeOn))
	seats := &seatEnv{testEnv: env.testEnv, Seats: &recordingSeats{}}
	payer, _ := seats.wingPayer(t)
	holder, _ := seats.seatedFriend(t, payer)
	requireError(t, postCheckout(env.Svc, holder, bodyWingYearly), http.StatusConflict, checkoutErrSeatMember)
}

func TestReconcile_HolderSubscribedEndsTheSeatWithoutCooldown(t *testing.T) {
	env := newSeatEnv(t)
	payer, _ := env.wingPayer(t)
	holder, seatID := env.seatedFriend(t, payer)

	seedMapping(t, env.DB, holder, "cus_holder")
	env.Fake.seedSubscription("sub_holder_own", "cus_holder", PlanSupersonic, "month", testPeriodEnd)
	_, err := env.Svc.Reconcile(context.Background(), "sub_holder_own", "", "")
	require.NoError(t, err)

	state, reason := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStateLeft, state)
	assert.Equal(t, seatEndHolderSubscribed, reason.String)
	assert.Equal(t, entitlements.TierPremium, tierOf(t, env.DB, holder), "own plan now entitles")
	assert.NotEmpty(t, env.Seats.For(payer, EventSeatUpdated))
	assert.NotEmpty(t, env.Seats.For(holder, EventSeatUpdated))

	other := env.friend(t, payer)
	w := invite(env.Svc, payer, other, false)
	assert.Equal(t, http.StatusCreated, w.Code, "holder_subscribed does not count toward the cooldown: %s", w.Body.String())
}

func TestReconcile_EndsTheSeatWithItsParent(t *testing.T) {
	cases := map[string]struct {
		edit   func(*fakeSubscription)
		reason string
	}{
		"canceled": {func(s *fakeSubscription) { s.Status = "canceled" }, seatEndSubscriptionEnded},
		"plan changed": {func(s *fakeSubscription) {
			s.Items.Data[0].Price.Metadata[PlanMetadataKey] = PlanSupersonic
		}, seatEndPlanChanged},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newSeatEnv(t)
			payer, subID := env.wingPayer(t)
			holder, seatID := env.seatedFriend(t, payer)
			editFakeSub(t, env.Fake, subID, tc.edit)

			_, err := env.Svc.Reconcile(context.Background(), subID, "", "")
			require.NoError(t, err)
			state, reason := seatState(t, env.DB, seatID)
			assert.Equal(t, SeatStateRevoked, state)
			assert.Equal(t, tc.reason, reason.String)
			assert.Equal(t, entitlements.TierFree, tierOf(t, env.DB, holder))
			assert.Contains(t, env.Changer.Calls(), tierCall{UserID: holder, Old: entitlements.TierPremium, New: entitlements.TierFree})
		})
	}
}

func TestReconcile_LapsedParentStillPushesTheHolder(t *testing.T) {
	env := newSeatEnv(t)
	payer, subID := env.wingPayer(t)
	holder, seatID := env.seatedFriend(t, payer)
	// The stored period ended long ago, so the holder already resolves free
	// before the late terminal event arrives.
	_, err := env.DB.Exec(`UPDATE subscriptions SET current_period_end = NOW() - INTERVAL '7 days'
		WHERE stripe_subscription_id = $1`, subID)
	require.NoError(t, err)
	require.Equal(t, entitlements.TierFree, tierOf(t, env.DB, holder))
	editFakeSub(t, env.Fake, subID, func(s *fakeSubscription) { s.Status = "canceled" })

	_, err = env.Svc.Reconcile(context.Background(), subID, "", "")
	require.NoError(t, err)
	state, _ := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStateRevoked, state)
	assert.Contains(t, env.Changer.Calls(), tierCall{UserID: holder, Old: entitlements.TierFree, New: entitlements.TierFree})
}

func TestReconcile_PausedParentSuspendsWithoutEndingTheSeat(t *testing.T) {
	env := newSeatEnv(t)
	payer, subID := env.wingPayer(t)
	holder, seatID := env.seatedFriend(t, payer)
	editFakeSub(t, env.Fake, subID, func(s *fakeSubscription) { s.Status = "paused" })

	_, err := env.Svc.Reconcile(context.Background(), subID, "", "")
	require.NoError(t, err)
	state, _ := seatState(t, env.DB, seatID)
	assert.Equal(t, SeatStateActive, state)
	assert.Equal(t, entitlements.TierFree, tierOf(t, env.DB, holder), "the join suspends the seat")

	editFakeSub(t, env.Fake, subID, func(s *fakeSubscription) { s.Status = "active" })
	_, err = env.Svc.Reconcile(context.Background(), subID, "", "")
	require.NoError(t, err)
	assert.Equal(t, entitlements.TierPremium, tierOf(t, env.DB, holder))
}

func TestSeat_OnSeatsErasedPushesSurvivors(t *testing.T) {
	env := newSeatEnv(t)
	holder := dbtest.CreateUser(t, env.DB)
	payer := dbtest.CreateUser(t, env.DB)
	seat := uuid.New()
	env.Svc.OnSeatsErased(context.Background(), []ErasedSeat{
		{SeatID: seat, Survivor: holder, Holder: true},
		{SeatID: seat, Survivor: payer, Holder: false},
	})
	assert.Contains(t, env.Changer.Calls(), tierCall{UserID: holder, Old: entitlements.TierPremium, New: entitlements.TierFree})
	require.Len(t, env.Seats.For(payer, EventSeatUpdated), 1)
	assert.Equal(t, SeatStateLeft, env.Seats.For(payer, EventSeatUpdated)[0].Data["state"])
	assert.Equal(t, SeatStateRevoked, env.Seats.For(holder, EventSeatUpdated)[0].Data["state"])
}
