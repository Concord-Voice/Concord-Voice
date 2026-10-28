package subscriptions_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/subscriptions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestHandler builds the status handler over a migrated test DB and a gin
// engine, plus a fixtures DB handle on the same database.
func newTestHandler(t *testing.T) (*subscriptions.Handler, *gin.Engine, *sql.DB) {
	t.Helper()
	return newTestHandlerWithBilling(t, subscriptions.BillingNotConfigured)
}

// newTestHandlerWithBilling is newTestHandler with an explicit billing state,
// the third NewHandler argument that /subscriptions/me echoes as "billing".
func newTestHandlerWithBilling(t *testing.T, billingState string) (*subscriptions.Handler, *gin.Engine, *sql.DB) {
	t.Helper()
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	h := subscriptions.NewHandler(db, logger.New("test"), billingState)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	return h, r, db
}

// authAs injects a user_id into the gin context, mimicking AuthRequired.
func authAs(userID uuid.UUID) gin.HandlerFunc {
	return func(c *gin.Context) { c.Set("user_id", userID.String()) }
}

func doGet(r *gin.Engine, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, bytes.NewReader(nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// insertSubscription writes a subscriptions row directly for fixture setup. A
// stripe row also gets the unique Stripe id pair that 000178's
// subscriptions_stripe_identity_check requires; its caller must pass a non-nil
// periodEnd (subscriptions_stripe_period_check).
func insertSubscription(t *testing.T, db *sql.DB, userID uuid.UUID, tier, status, source string, periodEnd *time.Time) {
	t.Helper()
	var customerID, subscriptionID sql.NullString
	if source == "stripe" {
		customerID = sql.NullString{String: "cus_test_" + uuid.NewString(), Valid: true}
		subscriptionID = sql.NullString{String: "sub_test_" + uuid.NewString(), Valid: true}
	}
	_, err := db.Exec(
		`INSERT INTO subscriptions
		     (user_id, tier, status, source, current_period_end, stripe_customer_id, stripe_subscription_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		userID, tier, status, source, periodEnd, customerID, subscriptionID,
	)
	require.NoError(t, err)
}

// TestGetMe_ActivePremiumFromCode: a live premium row created by a code
// redemption renders its tier/status/source/expiry.
func TestGetMe_ActivePremiumFromCode(t *testing.T) {
	h, r, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	expiry := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	insertSubscription(t, db, user, "premium", "active", "code", &expiry)

	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp subscriptions.StatusDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "premium", resp.Tier)
	assert.Equal(t, "active", resp.Status)
	assert.Equal(t, "code", resp.Source)
	assert.True(t, resp.TrialAvailable, "a code grant does not consume the Stripe trial")
	require.NotNil(t, resp.CurrentPeriodEnd)
	assert.Equal(t, expiry.Format("2006-01-02T15:04:05Z07:00"), *resp.CurrentPeriodEnd)
}

// TestGetMe_NoRowFreeDefault: a user with no subscription row gets the
// free-default shape (tier=free, status=none, no expiry).
func TestGetMe_NoRowFreeDefault(t *testing.T) {
	h, r, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)

	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp subscriptions.StatusDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "free", resp.Tier)
	assert.Equal(t, "none", resp.Status)
	assert.True(t, resp.TrialAvailable)
	assert.Empty(t, resp.Source)
	assert.Nil(t, resp.CurrentPeriodEnd)
}

// TestGetMe_ExpiredRowFreeDefault: a row whose current_period_end has passed is
// NOT live (the query's period predicate excludes it) → free default.
func TestGetMe_ExpiredRowFreeDefault(t *testing.T) {
	h, r, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	past := time.Now().Add(-time.Hour).UTC()
	insertSubscription(t, db, user, "premium", "active", "code", &past)

	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp subscriptions.StatusDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "free", resp.Tier)
	assert.Equal(t, "none", resp.Status)
}

// TestGetMe_CanceledRowFreeDefault: a canceled status is not in the live set →
// free default (the status predicate excludes it, even while the period end is
// still in the future).
func TestGetMe_CanceledRowFreeDefault(t *testing.T) {
	h, r, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	periodEnd := time.Now().UTC().Add(30 * 24 * time.Hour)
	insertSubscription(t, db, user, "premium", "canceled", "stripe", &periodEnd)

	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp subscriptions.StatusDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "free", resp.Tier)
	assert.Equal(t, "none", resp.Status)
	assert.False(t, resp.TrialAvailable, "canceling a trial cannot restore eligibility")
}

func TestGetMe_ActivePremiumFromStripe(t *testing.T) {
	h, r, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	periodEnd := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	insertSubscription(t, db, user, "premium", "active", "stripe", &periodEnd)

	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp subscriptions.StatusDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "premium", resp.Tier)
	assert.Equal(t, "active", resp.Status)
	assert.Equal(t, "stripe", resp.Source)
	require.NotNil(t, resp.CurrentPeriodEnd)
	assert.Equal(t, periodEnd.Format("2006-01-02T15:04:05Z07:00"), *resp.CurrentPeriodEnd)
}

// stripeRowSpec is a live Stripe subscriptions row for the /me billing-field
// tests. A nil plan or interval stores NULL, which is how Reconcile records a
// subscription it could not map (spec section 5).
type stripeRowSpec struct {
	tier              string
	plan, interval    *string
	foundersRate      bool
	cancelAtPeriodEnd bool
	cancelAt          *time.Time
	periodEnd         time.Time
}

func strPtr(s string) *string { return &s }

// insertStripeRow writes one active Stripe row from spec.
func insertStripeRow(t *testing.T, db *sql.DB, userID uuid.UUID, spec stripeRowSpec) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO subscriptions
		     (user_id, tier, status, source, current_period_end, stripe_subscription_id, stripe_customer_id,
		      plan, billing_interval, founders_rate, cancel_at_period_end, cancel_at)
		 VALUES ($1, $2, 'active', 'stripe', $3, $4, $5, $6, $7, $8, $9, $10)`,
		userID, spec.tier, spec.periodEnd, "sub_test_"+uuid.NewString(), "cus_test_"+uuid.NewString(),
		spec.plan, spec.interval, spec.foundersRate, spec.cancelAtPeriodEnd, spec.cancelAt)
	require.NoError(t, err)
}

// getMeBody serves GET /subscriptions/me for user and returns the raw 200 body,
// for assertions on JSON null versus an absent key, which the typed DTO cannot
// tell apart.
func getMeBody(t *testing.T, h *subscriptions.Handler, user uuid.UUID) string {
	t.Helper()
	r := gin.New()
	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return w.Body.String()
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"

// TestGetMe_StripeObjectFields: an active Wing yearly founders row fills every
// StripeDTO field, is not canceling, and carries no ends_at.
func TestGetMe_StripeObjectFields(t *testing.T) {
	h, _, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	periodEnd := time.Now().UTC().Add(200 * 24 * time.Hour).Truncate(time.Second)
	insertStripeRow(t, db, user, stripeRowSpec{
		tier: "premium", plan: strPtr("wing"), interval: strPtr("year"),
		foundersRate: true, periodEnd: periodEnd,
	})

	resp := getMeFor(t, h, user)
	require.NotNil(t, resp.Stripe, "a live Stripe row fills the stripe object")
	assert.Equal(t, strPtr("wing"), resp.Stripe.Plan)
	assert.Equal(t, strPtr("year"), resp.Stripe.Interval)
	assert.Equal(t, "active", resp.Stripe.Status)
	assert.Equal(t, periodEnd.Format(rfc3339), resp.Stripe.CurrentPeriodEnd)
	assert.False(t, resp.Stripe.Canceling)
	assert.Nil(t, resp.Stripe.EndsAt)
	assert.True(t, resp.Stripe.FoundersRate)
	assert.Nil(t, resp.Code, "no code row, no code object")

	body := getMeBody(t, h, user)
	assert.NotContains(t, body, `"ends_at"`, "ends_at is omitted, not null, when the subscription is not canceling")
	assert.NotContains(t, body, `"code"`)
}

func TestGetMe_StripeSubscriberPriceAndPendingChange(t *testing.T) {
	h, _, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	periodEnd := time.Now().UTC().Add(200 * 24 * time.Hour).Truncate(time.Second)
	insertStripeRow(t, db, user, stripeRowSpec{
		tier: "premium", plan: strPtr("wing"), interval: strPtr("year"),
		foundersRate: true, periodEnd: periodEnd,
	})
	_, err := db.Exec(`UPDATE subscriptions SET stripe_unit_amount=$1, stripe_schedule_id=$2, pending_plan=$3,
		pending_billing_interval=$4, pending_change_at=$5 WHERE user_id=$6 AND source='stripe'`,
		8244, "sub_sched_test", "supersonic", "month", periodEnd, user)
	require.NoError(t, err)
	resp := getMeFor(t, h, user)
	require.NotNil(t, resp.Stripe)
	require.NotNil(t, resp.Stripe.UnitAmount)
	assert.EqualValues(t, 8244, *resp.Stripe.UnitAmount)
	assert.True(t, resp.Stripe.ScheduleAttached)
	assert.Equal(t, strPtr("supersonic"), resp.Stripe.PendingPlan)
	assert.Equal(t, strPtr("month"), resp.Stripe.PendingInterval)
	assert.Equal(t, strPtr(periodEnd.Format(rfc3339)), resp.Stripe.PendingChangeAt)
}

func TestGetMe_AttachedScheduleWithoutPendingChange(t *testing.T) {
	h, _, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	periodEnd := time.Now().UTC().Add(200 * 24 * time.Hour).Truncate(time.Second)
	insertStripeRow(t, db, user, stripeRowSpec{
		tier: "premium", plan: strPtr("supersonic"), interval: strPtr("year"), periodEnd: periodEnd,
	})
	_, err := db.Exec(`UPDATE subscriptions SET stripe_schedule_id=$1
		WHERE user_id=$2 AND source='stripe'`, "sub_sched_after_transition", user)
	require.NoError(t, err)

	resp := getMeFor(t, h, user)
	require.NotNil(t, resp.Stripe)
	assert.True(t, resp.Stripe.ScheduleAttached)
	assert.Nil(t, resp.Stripe.PendingPlan)
	assert.Nil(t, resp.Stripe.PendingChangeAt)
}

// TestGetMe_CancelingFlexibleMode: Stripe's flexible cancellation sets cancel_at
// and leaves cancel_at_period_end false. The subscription is canceling and
// ends at cancel_at, not at the period end.
func TestGetMe_CancelingFlexibleMode(t *testing.T) {
	h, _, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	periodEnd := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	cancelAt := time.Now().UTC().Add(10 * 24 * time.Hour).Truncate(time.Second)
	insertStripeRow(t, db, user, stripeRowSpec{
		tier: "premium", plan: strPtr("supersonic"), interval: strPtr("month"),
		cancelAtPeriodEnd: false, cancelAt: &cancelAt, periodEnd: periodEnd,
	})

	resp := getMeFor(t, h, user)
	require.NotNil(t, resp.Stripe)
	assert.True(t, resp.Stripe.Canceling)
	require.NotNil(t, resp.Stripe.EndsAt)
	assert.Equal(t, cancelAt.Format(rfc3339), *resp.Stripe.EndsAt)
	assert.NotEqual(t, periodEnd.Format(rfc3339), *resp.Stripe.EndsAt)
}

// TestGetMe_CancelingAtPeriodEnd: cancel_at_period_end with no cancel_at ends
// the subscription at current_period_end.
func TestGetMe_CancelingAtPeriodEnd(t *testing.T) {
	h, _, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	periodEnd := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	insertStripeRow(t, db, user, stripeRowSpec{
		tier: "premium", plan: strPtr("supersonic"), interval: strPtr("month"),
		cancelAtPeriodEnd: true, periodEnd: periodEnd,
	})

	resp := getMeFor(t, h, user)
	require.NotNil(t, resp.Stripe)
	assert.True(t, resp.Stripe.Canceling)
	require.NotNil(t, resp.Stripe.EndsAt)
	assert.Equal(t, periodEnd.Format(rfc3339), *resp.Stripe.EndsAt)
}

// TestGetMe_CodeNeverLapses: a code row with a NULL period end serializes as
// "code":{"current_period_end":null}. The key is present and null, not omitted,
// so a client can tell "never lapses" from "no code row".
func TestGetMe_CodeNeverLapses(t *testing.T) {
	h, _, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	insertSubscription(t, db, user, "premium", "active", "code", nil)

	body := getMeBody(t, h, user)
	assert.Contains(t, body, `"code":{"current_period_end":null}`)
	assert.NotContains(t, body, `"stripe"`, "no Stripe row, no stripe object")

	resp := getMeFor(t, h, user)
	require.NotNil(t, resp.Code)
	assert.Nil(t, resp.Code.CurrentPeriodEnd)
	assert.Nil(t, resp.Stripe)
}

// TestGetMe_BillingState: the handler echoes the billing state it was built
// with as "billing", with the exact wire strings, on the free default and on a
// row-backed response alike.
func TestGetMe_BillingState(t *testing.T) {
	assert.Equal(t, "self_hosted", subscriptions.BillingSelfHosted)
	assert.Equal(t, "not_configured", subscriptions.BillingNotConfigured)
	assert.Equal(t, "available", subscriptions.BillingAvailable)

	for _, state := range []string{
		subscriptions.BillingSelfHosted,
		subscriptions.BillingNotConfigured,
		subscriptions.BillingAvailable,
	} {
		t.Run(state, func(t *testing.T) {
			h, _, db := newTestHandlerWithBilling(t, state)

			free := testhelpers.CreateUser(t, db)
			assert.Equal(t, state, getMeFor(t, h, free).Billing, "the free default carries billing")
			assert.Contains(t, getMeBody(t, h, free), `"billing":"`+state+`"`)

			paid := testhelpers.CreateUser(t, db)
			insertSubscription(t, db, paid, "premium", "active", "code", nil)
			assert.Equal(t, state, getMeFor(t, h, paid).Billing, "a row-backed response carries billing")
		})
	}
}
