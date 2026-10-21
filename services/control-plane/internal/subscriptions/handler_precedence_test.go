package subscriptions_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/subscriptions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertPrecedenceRow inserts one live-status premium row whose
// current_period_end is the database clock plus periodOffset (a PostgreSQL
// interval; "" stores NULL), so boundary cases share the predicate's clock. A
// 'stripe' row carries the unique Stripe ids that
// subscriptions_stripe_identity_check requires.
func insertPrecedenceRow(t *testing.T, db *sql.DB, userID uuid.UUID, source, periodOffset string) {
	t.Helper()
	var stripeSubID, stripeCusID any
	if source == "stripe" {
		stripeSubID = "sub_test_" + uuid.NewString()
		stripeCusID = "cus_test_" + uuid.NewString()
	}
	_, err := db.Exec(
		`INSERT INTO subscriptions
		     (user_id, tier, status, source, current_period_end, stripe_subscription_id, stripe_customer_id)
		 VALUES ($1, 'premium', 'active', $2, NOW() + NULLIF($3::text, '')::interval, $4, $5)`,
		userID, source, periodOffset, stripeSubID, stripeCusID)
	require.NoError(t, err)
}

// getMeFor serves GET /subscriptions/me for user on a fresh engine and decodes
// the 200 body.
func getMeFor(t *testing.T, h *subscriptions.Handler, user uuid.UUID) subscriptions.StatusDTO {
	t.Helper()
	r := gin.New()
	r.GET("/subscriptions/me", authAs(user), h.GetMe)
	w := doGet(r, "/subscriptions/me")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp subscriptions.StatusDTO
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

// TestGetMe_StripeRowReportedBeforeCodeRow (Review Focus RF1): with a live
// code row and a live Stripe row, legal since 000179, the legacy top-level
// fields describe the Stripe row. The code row is inserted first, so a query
// that relied on heap order would report it.
func TestGetMe_StripeRowReportedBeforeCodeRow(t *testing.T) {
	h, _, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	insertPrecedenceRow(t, db, user, "code", "")
	insertPrecedenceRow(t, db, user, "stripe", "30 days")

	resp := getMeFor(t, h, user)
	assert.Equal(t, "premium", resp.Tier)
	assert.Equal(t, "active", resp.Status)
	assert.Equal(t, "stripe", resp.Source)
	assert.NotNil(t, resp.CurrentPeriodEnd, "a Stripe row always carries a period end")
}

// TestGetMe_PerSourceLiveness: GetMe applies entitlements.LiveRowPredicate, so
// a Stripe row inside its 6-hour renewal buffer is still reported, one past it
// is not, and a code row has no buffer.
func TestGetMe_PerSourceLiveness(t *testing.T) {
	h, _, db := newTestHandler(t)
	cases := []struct {
		name       string
		rows       [][2]string // {source, periodOffset}, inserted in order
		wantSource string      // "" means the free default
	}{
		{"stripe in the renewal gap beside a code row", [][2]string{{"code", ""}, {"stripe", "-1 hours"}}, "stripe"},
		{"stripe past the buffer falls back to the code row", [][2]string{{"stripe", "-7 hours"}, {"code", ""}}, "code"},
		{"stripe alone 5h59m past period end", [][2]string{{"stripe", "-5 hours -59 minutes"}}, "stripe"},
		{"stripe alone 6h01m past period end", [][2]string{{"stripe", "-6 hours -1 minutes"}}, ""},
		{"code alone 1m past period end", [][2]string{{"code", "-1 minutes"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user := testhelpers.CreateUser(t, db)
			for _, row := range tc.rows {
				insertPrecedenceRow(t, db, user, row[0], row[1])
			}
			resp := getMeFor(t, h, user)
			if tc.wantSource == "" {
				assert.Equal(t, "free", resp.Tier)
				assert.Equal(t, "none", resp.Status)
				assert.Empty(t, resp.Source)
				return
			}
			assert.Equal(t, "premium", resp.Tier)
			assert.Equal(t, tc.wantSource, resp.Source)
		})
	}
}

// TestGetMe_LatestStripePeriodEndFirst: idx_subscriptions_user_live_stripe is
// not unique, so a user may hold several live Stripe rows, and GetMe reports the
// one with the latest period end. Both insertion orders run, so a query that
// kept heap order fails one subtest and one that sorted ascending fails both.
// (NULLS FIRST and the id tie-break are not tested: no two live rows in one
// precedence bucket can differ only by a NULL end, since a user holds at most
// one live code row, and StatusDTO does not expose id.)
func TestGetMe_LatestStripePeriodEndFirst(t *testing.T) {
	h, _, db := newTestHandler(t)
	for _, order := range [][2]string{{"10 days", "30 days"}, {"30 days", "10 days"}} {
		t.Run(order[0]+" then "+order[1], func(t *testing.T) {
			user := testhelpers.CreateUser(t, db)
			insertPrecedenceRow(t, db, user, "stripe", order[0])
			insertPrecedenceRow(t, db, user, "stripe", order[1])

			resp := getMeFor(t, h, user)
			assert.Equal(t, "stripe", resp.Source)
			require.NotNil(t, resp.CurrentPeriodEnd)
			end, err := time.Parse(time.RFC3339, *resp.CurrentPeriodEnd)
			require.NoError(t, err)
			assert.WithinDuration(t, time.Now().AddDate(0, 0, 30), end, 24*time.Hour,
				"the later Stripe period end is reported")
		})
	}
}

// TestGetMe_FreeStripeRowShownWhileEnforcementStaysPremium pins the spec
// section 5 edge (design spec line 190): Reconcile writes tier='free' on a live
// Stripe row when it cannot map the subscription (an unknown concord_plan, an
// item count other than 1, a quantity other than 1). Beside a live premium code
// row, GetMe shows that free Stripe row (the Stripe-first order keeps the
// charged subscription and its manage link visible) while
// entitlements.ResolveTierQ, which takes the maximum rank over live rows,
// enforces premium. A change that made GetMe skip the free row, or made the
// resolver prefer Stripe, breaks one half of this test.
func TestGetMe_FreeStripeRowShownWhileEnforcementStaysPremium(t *testing.T) {
	h, _, db := newTestHandler(t)
	user := testhelpers.CreateUser(t, db)
	insertPrecedenceRow(t, db, user, "code", "")
	_, err := db.Exec(
		`INSERT INTO subscriptions
		     (user_id, tier, status, source, current_period_end, stripe_subscription_id, stripe_customer_id)
		 VALUES ($1, 'free', 'active', 'stripe', NOW() + INTERVAL '30 days', $2, $3)`,
		user, "sub_test_"+uuid.NewString(), "cus_test_"+uuid.NewString())
	require.NoError(t, err)

	resp := getMeFor(t, h, user)
	assert.Equal(t, "free", resp.Tier, "GetMe describes the live Stripe row, even when it is free")
	assert.Equal(t, "stripe", resp.Source)
	assert.Equal(t, "active", resp.Status)

	assert.Equal(t, entitlements.TierPremium, entitlements.ResolveTierQ(t.Context(), db, user.String()),
		"enforcement takes the maximum rank over live rows: the premium code row still counts")
}
