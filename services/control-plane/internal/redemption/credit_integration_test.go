package redemption_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/redemption"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCrediter stands in for billing.Service: it asks for confirmation on a
// plan mismatch, as billing does, and otherwise returns err or a fixed credit.
// ApplyCredit, which the engine runs in a goroutine, records each redemption and
// whether its outbox row was already committed when it was called.
type fakeCrediter struct {
	db        *sql.DB
	reqs      []redemption.CreditRequest
	err       error
	mu        sync.Mutex
	applied   []uuid.UUID
	committed []bool
}

func (f *fakeCrediter) QuoteCredit(req redemption.CreditRequest) (redemption.CreditResult, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return redemption.CreditResult{}, f.err
	}
	if req.CodePlan != req.SubscribedPlan && !req.Confirmed {
		return redemption.CreditResult{}, &redemption.CreditConfirmationError{Amount: 4497, Currency: "usd", CodePlan: req.CodePlan, SubscribedPlan: req.SubscribedPlan}
	}
	return redemption.CreditResult{Amount: 2697, Currency: "usd", Matched: req.CodePlan == req.SubscribedPlan, StripeDescription: "test credit"}, nil
}

func (f *fakeCrediter) ApplyCredit(_ context.Context, redemptionID uuid.UUID) {
	var visible bool
	if f.db != nil {
		if err := f.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM billing_credit_outbox WHERE redemption_id = $1)`, redemptionID).Scan(&visible); err != nil {
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, redemptionID)
	f.committed = append(f.committed, visible)
}

// waitApplied waits for n ApplyCredit calls and returns what they recorded.
func (f *fakeCrediter) waitApplied(t *testing.T, n int) (applied []uuid.UUID, committed []bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.applied) >= n
	}, 5*time.Second, 10*time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.applied), slices.Clone(f.committed)
}

// queuedCredits returns the user's pending credits as (redemption id, customer, amount).
func queuedCredits(t *testing.T, db *sql.DB, user uuid.UUID) (ids []uuid.UUID, customers []string, amounts []int64) {
	t.Helper()
	rows, err := db.Query(`
		SELECT o.redemption_id, o.stripe_customer_id, o.amount
		  FROM billing_credit_outbox o JOIN code_redemptions r ON r.id = o.redemption_id
		 WHERE r.user_id = $1`, user)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var id uuid.UUID
		var cus string
		var amount int64
		require.NoError(t, rows.Scan(&id, &cus, &amount))
		ids, customers, amounts = append(ids, id), append(customers, cus), append(amounts, amount)
	}
	require.NoError(t, rows.Err())
	return ids, customers, amounts
}

func issuePremium(t *testing.T, db *sql.DB, params map[string]any) string {
	t.Helper()
	return issueOne(t, db, redemption.IssueSpec{
		GrantKind: redemption.GrantPremiumSubscription, GrantParams: params,
		SingleUse: true, MaxRedeems: intPtr(1), BatchID: "code-credit",
	})
}

// assertUnredeemed checks a refused redemption consumed nothing.
func assertUnredeemed(t *testing.T, db *sql.DB, user uuid.UUID) {
	t.Helper()
	var claimed, ledger int
	require.NoError(t, db.QueryRow(`SELECT COALESCE(SUM(redemption_count), 0) FROM redemption_codes WHERE batch_id = 'code-credit'`).Scan(&claimed))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM code_redemptions WHERE user_id = $1`, user).Scan(&ledger))
	assert.Zero(t, claimed, "the code claim rolled back")
	assert.Zero(t, ledger, "no ledger row")
	assert.Zero(t, codeRowCount(t, db, user), "no code grant")
	ids, _, _ := queuedCredits(t, db, user)
	assert.Empty(t, ids, "no credit queued")
}

// A renewing subscriber's same-plan code becomes credit: no code row, the
// Stripe row is byte-identical, the ledger links the Stripe row, and the tier
// does not change.
func TestRedeem_CreditOnRenewingSubscription(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := insertUser(t, db)
	stripeID := insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 1, 0).Truncate(time.Second), false)
	before := subscriptionRowText(t, db, stripeID)
	code := issuePremium(t, db, map[string]any{"months": 3})

	notifier := &recordingNotifier{}
	crediter := &fakeCrediter{db: db}
	eng := newEngine(db, notifier)
	eng.SetCrediter(crediter)
	out, err := eng.Redeem(context.Background(), user, code)
	require.NoError(t, err)

	assert.Contains(t, out.Description, "$26.97")
	require.Len(t, crediter.reqs, 1)
	req := crediter.reqs[0]
	assert.Equal(t, "supersonic", req.SubscribedPlan)
	assert.Equal(t, "month", req.Interval)
	assert.Equal(t, "supersonic", req.CodePlan, "a code without a plan is a Supersonic code")
	assert.Equal(t, 3, req.Months)

	ids, customers, amounts := queuedCredits(t, db, user)
	require.Len(t, ids, 1, "the credit is queued in the redemption transaction")
	assert.Equal(t, []int64{2697}, amounts)
	var stripeCustomer string
	require.NoError(t, db.QueryRow(`SELECT stripe_customer_id FROM subscriptions WHERE id = $1`, stripeID).Scan(&stripeCustomer))
	assert.Equal(t, []string{stripeCustomer}, customers)
	applied, committed := crediter.waitApplied(t, 1)
	assert.Equal(t, ids, applied, "applied once, for that redemption")
	assert.Equal(t, []bool{true}, committed, "Stripe is reached only after the commit")

	assert.Equal(t, before, subscriptionRowText(t, db, stripeID), "credit never writes the Stripe row")
	assert.Zero(t, codeRowCount(t, db, user))
	assert.Equal(t, uuid.NullUUID{UUID: stripeID, Valid: true}, ledgerSubscription(t, db, user))
	assert.Empty(t, notifier.calls, "credit does not change the tier")
}

// Another plan's code needs confirmation: unconfirmed it redeems nothing and
// returns the quote; confirmed it becomes credit.
func TestRedeem_CreditOtherPlanNeedsConfirmation(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := insertUser(t, db)
	insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 1, 0), false)
	code := issuePremium(t, db, map[string]any{"months": 3, "plan": "wing"})

	crediter := &fakeCrediter{}
	eng := newEngine(db, nil)
	eng.SetCrediter(crediter)

	_, err := eng.Redeem(context.Background(), user, code)
	var confirm *redemption.CreditConfirmationError
	require.ErrorAs(t, err, &confirm)
	assert.Equal(t, redemption.CreditConfirmationError{Amount: 4497, Currency: "usd", CodePlan: "wing", SubscribedPlan: "supersonic"}, *confirm)
	assertUnredeemed(t, db, user)

	_, err = eng.RedeemConfirmed(context.Background(), user, code, true)
	require.NoError(t, err)
	require.Len(t, crediter.reqs, 2)
	assert.True(t, crediter.reqs[1].Confirmed)
	applied, _ := crediter.waitApplied(t, 1)
	assert.Len(t, applied, 1, "only the confirmed redemption is applied")
}

// Credit that cannot be applied (no crediter, or Stripe fails) redeems nothing.
func TestRedeem_CreditUnavailableRedeemsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		crediter *fakeCrediter
		noPlan   bool
	}{
		"no crediter":        {},
		"stripe failed":      {crediter: &fakeCrediter{err: errors.New("stripe down")}},
		"row without a plan": {crediter: &fakeCrediter{}, noPlan: true},
	} {
		crediter := tc.crediter
		t.Run(name, func(t *testing.T) {
			db, cleanup := testhelpers.SetupTestDB(t)
			defer cleanup()
			user := insertUser(t, db)
			stripeID := insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 1, 0), false)
			if tc.noPlan {
				_, err := db.Exec(`UPDATE subscriptions SET plan = NULL, billing_interval = NULL WHERE id = $1`, stripeID)
				require.NoError(t, err)
			}
			code := issuePremium(t, db, map[string]any{"months": 1})

			eng := newEngine(db, nil)
			if crediter != nil {
				eng.SetCrediter(crediter)
			}
			_, err := eng.Redeem(context.Background(), user, code)
			require.ErrorIs(t, err, redemption.ErrCreditUnavailable)
			assertUnredeemed(t, db, user)
			if tc.noPlan {
				assert.Empty(t, crediter.reqs, "Stripe is not called for a row it cannot price")
			}
		})
	}
}

// A premium code naming no plan Concord sells (the issuer refuses one, so it is
// a row written outside it) is not valid on the credit path.
func TestRedeem_CreditUnknownPlanIsNotValid(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := insertUser(t, db)
	insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 1, 0), false)
	code := issuePremium(t, db, map[string]any{"months": 1})
	_, err := db.Exec(`UPDATE redemption_codes SET grant_params = '{"months": 1, "plan": "platinum"}' WHERE batch_id = 'code-credit'`)
	require.NoError(t, err)

	eng := newEngine(db, nil)
	eng.SetCrediter(&fakeCrediter{})
	_, err = eng.Redeem(context.Background(), user, code)
	require.ErrorIs(t, err, redemption.ErrCodeNotValid)
	assertUnredeemed(t, db, user)
}

// A trialing subscription is live and renews, so a code redeemed during the
// trial becomes credit that pays the first charge after it (#2033 D6 trial).
func TestRedeem_CreditDuringTrial(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := insertUser(t, db)
	stripeID := insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 0, 14), false)
	_, err := db.Exec(`UPDATE subscriptions SET status = 'trialing' WHERE id = $1`, stripeID)
	require.NoError(t, err)
	code := issuePremium(t, db, map[string]any{"months": 1})

	crediter := &fakeCrediter{}
	eng := newEngine(db, nil)
	eng.SetCrediter(crediter)
	_, err = eng.Redeem(context.Background(), user, code)
	require.NoError(t, err)
	require.Len(t, crediter.reqs, 1)
	assert.Zero(t, codeRowCount(t, db, user), "the trial is not extended by a parallel code grant")
}

// With a renewing and a canceling live row, the renewing one takes the credit
// even when the canceling one ends later.
func TestRedeem_CreditPrefersRenewingRow(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := insertUser(t, db)
	renewing := insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 1, 0), false)
	insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 6, 0), true)
	code := issuePremium(t, db, map[string]any{"months": 1})

	crediter := &fakeCrediter{}
	eng := newEngine(db, nil)
	eng.SetCrediter(crediter)
	_, err := eng.Redeem(context.Background(), user, code)
	require.NoError(t, err)
	require.Len(t, crediter.reqs, 1)
	assert.Equal(t, uuid.NullUUID{UUID: renewing, Valid: true}, ledgerSubscription(t, db, user))
}

// Each subscriber who redeems a multi-use code gets their own queued credit,
// keyed by their own redemption.
func TestRedeem_CreditPerRedemption(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	code := issueOne(t, db, redemption.IssueSpec{
		GrantKind: redemption.GrantPremiumSubscription, GrantParams: map[string]any{"months": 1},
		MaxRedeems: intPtr(2), BatchID: "code-credit",
	})
	crediter := &fakeCrediter{}
	eng := newEngine(db, nil)
	eng.SetCrediter(crediter)
	for range 2 {
		user := insertUser(t, db)
		insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 1, 0), false)
		_, err := eng.Redeem(context.Background(), user, code)
		require.NoError(t, err)
		ids, _, _ := queuedCredits(t, db, user)
		require.Len(t, ids, 1)
	}
	applied, _ := crediter.waitApplied(t, 2)
	require.Len(t, applied, 2)
	assert.NotEqual(t, applied[0], applied[1])
}

// A subscription ending at cancel_at (not at period end) takes the code grant,
// starting where it ends; Stripe is not called.
func TestRedeem_CancelAtTakesTheGrant(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := insertUser(t, db)
	cancelAt := time.Now().UTC().AddDate(0, 0, 10).Truncate(time.Second)
	stripeID := insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 1, 0), false)
	_, err := db.Exec(`UPDATE subscriptions SET cancel_at = $2 WHERE id = $1`, stripeID, cancelAt)
	require.NoError(t, err)
	code := issuePremium(t, db, map[string]any{"months": 1})

	crediter := &fakeCrediter{}
	eng := newEngine(db, nil)
	eng.SetCrediter(crediter)
	_, err = eng.Redeem(context.Background(), user, code)
	require.NoError(t, err)
	assert.Empty(t, crediter.reqs)

	var end time.Time
	require.NoError(t, db.QueryRow(`SELECT current_period_end FROM subscriptions WHERE user_id = $1 AND source = 'code'`, user).Scan(&end))
	assert.WithinDuration(t, cancelAt.AddDate(0, 1, 0), end, 24*time.Hour, "the grant's month starts at cancel_at")
}

// A non-premium code on a renewing subscription applies normally: only
// premium codes become credit.
func TestRedeem_CosmeticOnRenewingSubscriptionIsNotCredit(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := insertUser(t, db)
	insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 1, 0), false)
	code := issueOne(t, db, redemption.IssueSpec{GrantKind: "cosmetic:profile_badge", SingleUse: true, MaxRedeems: intPtr(1)})

	crediter := &fakeCrediter{}
	eng := newEngine(db, nil)
	eng.SetCrediter(crediter)
	_, err := eng.Redeem(context.Background(), user, code)
	require.NoError(t, err)
	assert.Empty(t, crediter.reqs)
}

// confirm_credit in the /redeem body reaches the crediter, so the desktop's
// confirmed retry is not answered with the same 409 again.
func TestHandler_RedeemConfirmCreditReachesCrediter(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := insertUser(t, db)
	insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 1, 0), false)
	code := issuePremium(t, db, map[string]any{"months": 1, "plan": "wing"})

	crediter := &fakeCrediter{}
	eng := newEngine(db, nil)
	eng.SetCrediter(crediter)
	h := redemption.NewHandler(eng, nil, "", logger.New("test"))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/redeem", authAs(user), h.Redeem)

	w := doJSON(r, http.MethodPost, "/redeem", []byte(`{"code":"`+code+`"}`))
	require.Equal(t, http.StatusConflict, w.Code)
	w = doJSON(r, http.MethodPost, "/redeem", []byte(`{"code":"`+code+`","confirm_credit":true}`))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, crediter.reqs, 2)
	assert.False(t, crediter.reqs[0].Confirmed)
	assert.True(t, crediter.reqs[1].Confirmed)
}
