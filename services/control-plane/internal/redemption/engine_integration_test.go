package redemption_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/redemption"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertUser creates a minimal users row so the redemption/subscription FKs are
// satisfied. Delegates to the shared testhelpers.CreateUser (which derives the
// email/username SQL-side via `$2 || ...`, so there is no Go-side string concat
// adjacent to the Exec). Returns the user id.
func insertUser(t *testing.T, db *sql.DB) uuid.UUID {
	t.Helper()
	return testhelpers.CreateUser(t, db)
}

// issueOne mints a single code via the issuer and returns its plaintext. Uses a
// no-op audit sink unless a real one is supplied.
func issueOne(t *testing.T, db *sql.DB, spec redemption.IssueSpec) string {
	t.Helper()
	if spec.Count == 0 {
		spec.Count = 1
	}
	iss := redemption.NewIssuer(db, redemption.NewCatalog(), redemption.NewDBAuditSink())
	codes, err := iss.Issue(context.Background(), spec)
	require.NoError(t, err)
	require.Len(t, codes, spec.Count)
	return codes[0].Plaintext
}

// recordingNotifier captures OnTierChange calls so tests assert the live-update
// fired with the right tiers.
type recordingNotifier struct {
	mu    sync.Mutex
	calls [][3]string // userID, old, new
}

func (n *recordingNotifier) OnTierChange(_ context.Context, userID uuid.UUID, oldTier, newTier string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, [3]string{userID.String(), oldTier, newTier})
	return nil
}

func newEngine(db *sql.DB, n redemption.Notifier) *redemption.Engine {
	return redemption.NewEngine(db, redemption.NewCatalog(), n)
}

// TestRedeem_PremiumHappyPath: a premium:subscription code grants premium,
// creates a subscriptions row, links the ledger, and fires OnTierChange.
func TestRedeem_PremiumHappyPath(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	user := insertUser(t, db)
	code := issueOne(t, db, redemption.IssueSpec{
		GrantKind:   redemption.GrantPremiumSubscription,
		GrantParams: map[string]any{"months": 12},
		Count:       1,
		SingleUse:   true,
		MaxRedeems:  intPtr(1),
		BatchID:     "test-premium",
	})

	notifier := &recordingNotifier{}
	eng := newEngine(db, notifier)

	out, err := eng.Redeem(ctx, user, code)
	require.NoError(t, err)
	assert.Contains(t, out.Description, "12 months")

	// Subscription row created, premium + source=code + active.
	var tier, status, source string
	var periodEnd sql.NullTime
	require.NoError(t, db.QueryRow(
		`SELECT tier, status, source, current_period_end FROM subscriptions WHERE user_id=$1`, user,
	).Scan(&tier, &status, &source, &periodEnd))
	assert.Equal(t, "premium", tier)
	assert.Equal(t, "active", status)
	assert.Equal(t, "code", source)
	require.True(t, periodEnd.Valid)
	assert.WithinDuration(t, time.Now().AddDate(0, 12, 0), periodEnd.Time, 24*time.Hour)

	// Ledger row links the subscription.
	var subLinked sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT resulting_subscription_id::text FROM code_redemptions WHERE user_id=$1`, user,
	).Scan(&subLinked))
	assert.True(t, subLinked.Valid, "ledger row must link the resulting subscription")

	// OnTierChange fired free→premium.
	require.Len(t, notifier.calls, 1)
	assert.Equal(t, [3]string{user.String(), "free", "premium"}, notifier.calls[0])
}

// TestRedeem_FeatureAndCosmetic: non-premium kinds redeem (ledger-of-record),
// no subscription, no tier change.
func TestRedeem_FeatureAndCosmetic(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	for _, kind := range []string{"feature:custom_themes", "cosmetic:profile_badge"} {
		user := insertUser(t, db)
		code := issueOne(t, db, redemption.IssueSpec{
			GrantKind: kind, Count: 1, SingleUse: true, MaxRedeems: intPtr(1),
		})
		notifier := &recordingNotifier{}
		out, err := newEngine(db, notifier).Redeem(ctx, user, code)
		require.NoErrorf(t, err, "kind %s", kind)
		assert.NotEmpty(t, out.Description)

		var subCount int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM subscriptions WHERE user_id=$1`, user).Scan(&subCount))
		assert.Equal(t, 0, subCount, "non-premium grant must NOT create a subscription")
		assert.Empty(t, notifier.calls, "non-premium grant must NOT fire OnTierChange")
	}
}

// TestRedeem_GenericRejection_NoOracle is the no-oracle acceptance criterion:
// invalid / not-found / revoked / expired / not-yet-valid / exhausted ALL return
// the SAME ErrCodeNotValid (the HTTP layer maps that to one generic message).
func TestRedeem_GenericRejection_NoOracle(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	eng := newEngine(db, &recordingNotifier{})

	mkUser := func() uuid.UUID { return insertUser(t, db) }

	t.Run("checksum-invalid garbage", func(t *testing.T) {
		_, err := eng.Redeem(ctx, mkUser(), "TOTALLY-BOGUS-CODE")
		assert.ErrorIs(t, err, redemption.ErrCodeNotValid)
	})

	t.Run("well-formed but not in registry", func(t *testing.T) {
		// A checksum-valid code that was never issued.
		code := issueOne(t, db, redemption.IssueSpec{
			GrantKind: redemption.GrantPremiumSubscription, Count: 1, SingleUse: true, MaxRedeems: intPtr(1),
		})
		// Delete it from the registry so the hash lookup misses.
		_, err := db.Exec(`DELETE FROM redemption_codes`)
		require.NoError(t, err)
		_, err = eng.Redeem(ctx, mkUser(), code)
		assert.ErrorIs(t, err, redemption.ErrCodeNotValid)
	})

	t.Run("revoked", func(t *testing.T) {
		code := issueOne(t, db, redemption.IssueSpec{
			GrantKind: redemption.GrantPremiumSubscription, Count: 1, SingleUse: true, MaxRedeems: intPtr(1), BatchID: "rev",
		})
		iss := redemption.NewIssuer(db, redemption.NewCatalog(), redemption.NewDBAuditSink())
		n, err := iss.RevokeBatch(ctx, "rev")
		require.NoError(t, err)
		require.Equal(t, int64(1), n)
		_, err = eng.Redeem(ctx, mkUser(), code)
		assert.ErrorIs(t, err, redemption.ErrCodeNotValid)
	})

	t.Run("expired", func(t *testing.T) {
		code := issueOne(t, db, redemption.IssueSpec{
			GrantKind: redemption.GrantPremiumSubscription, Count: 1, SingleUse: true, MaxRedeems: intPtr(1),
			ExpiresAt: timePtr(time.Now().Add(time.Hour)),
		})
		// Backdate expiry directly so it is now in the past.
		_, err := db.Exec(`UPDATE redemption_codes SET expires_at = NOW() - interval '1 hour'`)
		require.NoError(t, err)
		_, err = eng.Redeem(ctx, mkUser(), code)
		assert.ErrorIs(t, err, redemption.ErrCodeNotValid)
	})

	t.Run("not yet valid", func(t *testing.T) {
		code := issueOne(t, db, redemption.IssueSpec{
			GrantKind: redemption.GrantPremiumSubscription, Count: 1, SingleUse: true, MaxRedeems: intPtr(1),
		})
		_, err := db.Exec(`UPDATE redemption_codes SET valid_from = NOW() + interval '1 day'`)
		require.NoError(t, err)
		_, err = eng.Redeem(ctx, mkUser(), code)
		assert.ErrorIs(t, err, redemption.ErrCodeNotValid)
	})

	t.Run("exhausted (max_redemptions reached)", func(t *testing.T) {
		_, err := db.Exec(`DELETE FROM code_redemptions; DELETE FROM redemption_codes`)
		require.NoError(t, err)
		code := issueOne(t, db, redemption.IssueSpec{
			GrantKind: "feature:custom_themes", Count: 1, SingleUse: false, MaxRedeems: intPtr(1),
		})
		// First redeem consumes the single slot.
		_, err = eng.Redeem(ctx, mkUser(), code)
		require.NoError(t, err)
		// Second user → exhausted → generic rejection (NOT already-redeemed,
		// since it's a different user).
		_, err = eng.Redeem(ctx, mkUser(), code)
		assert.ErrorIs(t, err, redemption.ErrCodeNotValid)
	})
}

// TestRedeem_PerUserDedup: the same user redeeming the same promo code twice
// gets ErrAlreadyRedeemed, and redemption_count is NOT inflated by the retry.
func TestRedeem_PerUserDedup(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	eng := newEngine(db, &recordingNotifier{})

	user := insertUser(t, db)
	code := issueOne(t, db, redemption.IssueSpec{
		GrantKind: "feature:custom_themes", Count: 1, SingleUse: false,
		MaxRedeems: intPtr(5), BatchID: "dedup",
	})

	_, err := eng.Redeem(ctx, user, code)
	require.NoError(t, err)

	_, err = eng.Redeem(ctx, user, code)
	assert.ErrorIs(t, err, redemption.ErrAlreadyRedeemed)

	// redemption_count must be exactly 1 — the rolled-back retry did not inflate it.
	var count int
	require.NoError(t, db.QueryRow(`SELECT redemption_count FROM redemption_codes WHERE batch_id='dedup'`).Scan(&count))
	assert.Equal(t, 1, count, "the failed re-redeem must not inflate redemption_count")

	// Exactly one ledger row for this user+code.
	var ledger int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM code_redemptions WHERE user_id=$1`, user).Scan(&ledger))
	assert.Equal(t, 1, ledger)
}

// TestRedeem_ConcurrentDoubleSpend is the headline concurrency acceptance test:
// N goroutines simultaneously redeem a max_redemptions=1 code (each as a
// DIFFERENT user, so the per-user UNIQUE constraint does not mask the race).
// EXACTLY ONE must succeed; the rest get the generic rejection. This proves the
// atomic conditional UPDATE — not a check-then-update — is the guard.
func TestRedeem_ConcurrentDoubleSpend(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const racers = 20
	users := make([]uuid.UUID, racers)
	for i := range users {
		users[i] = insertUser(t, db)
	}

	code := issueOne(t, db, redemption.IssueSpec{
		GrantKind:  "feature:custom_themes",
		Count:      1,
		SingleUse:  true,
		MaxRedeems: intPtr(1), // exactly one redemption allowed across all users
		BatchID:    "race",
	})

	eng := newEngine(db, &recordingNotifier{})

	var (
		wg         sync.WaitGroup
		successCnt int64
		rejectCnt  int64
		start      = make(chan struct{})
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(u uuid.UUID) {
			defer wg.Done()
			<-start // release all goroutines at once for maximal contention
			_, err := eng.Redeem(ctx, u, code)
			if err == nil {
				atomic.AddInt64(&successCnt, 1)
			} else if errors.Is(err, redemption.ErrCodeNotValid) {
				atomic.AddInt64(&rejectCnt, 1)
			} else {
				t.Errorf("unexpected redeem error: %v", err)
			}
		}(users[i])
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int64(1), atomic.LoadInt64(&successCnt), "EXACTLY ONE redeem may succeed for a max_redemptions=1 code under concurrency")
	assert.Equal(t, int64(racers-1), atomic.LoadInt64(&rejectCnt), "all other racers must get the generic rejection")

	// Definitive DB check: the count is exactly 1 and there is exactly one
	// ledger row — no over-grant slipped through.
	var count, ledger int
	require.NoError(t, db.QueryRow(`SELECT redemption_count FROM redemption_codes WHERE batch_id='race'`).Scan(&count))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM code_redemptions`).Scan(&ledger))
	assert.Equal(t, 1, count, "redemption_count overran the cap — the atomic UPDATE guard failed")
	assert.Equal(t, 1, ledger, "more than one ledger row — double-spend")
}

// TestRedeem_PremiumStacks: redeeming a second premium code EXTENDS the period
// rather than truncating it.
func TestRedeem_PremiumStacks(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	eng := newEngine(db, &recordingNotifier{})

	user := insertUser(t, db)
	for i := 0; i < 2; i++ {
		code := issueOne(t, db, redemption.IssueSpec{
			GrantKind:   redemption.GrantPremiumSubscription,
			GrantParams: map[string]any{"months": 1},
			Count:       1, SingleUse: true, MaxRedeems: intPtr(1),
			BatchID: fmt.Sprintf("stack-%d", i),
		})
		_, err := eng.Redeem(ctx, user, code)
		require.NoError(t, err)
	}

	// One active subscription, period ~2 months out (stacked, not truncated).
	var rows int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM subscriptions WHERE user_id=$1 AND status IN ('active','trialing','past_due')`, user,
	).Scan(&rows))
	assert.Equal(t, 1, rows, "one active subscription (the partial unique index holds)")

	var periodEnd time.Time
	require.NoError(t, db.QueryRow(`SELECT current_period_end FROM subscriptions WHERE user_id=$1`, user).Scan(&periodEnd))
	assert.WithinDuration(t, time.Now().AddDate(0, 2, 0), periodEnd, 36*time.Hour, "two 1-month codes stack to ~2 months")
}

// TestRedeem_ConcurrentSameUserPremiumNoPriorSub is the regression lock for the
// RCI-found race: the SAME user redeems TWO DIFFERENT premium codes concurrently
// with NO prior subscription. Without the users-row FOR UPDATE serialization,
// both transactions would see "no existing sub" and race two INSERTs against the
// partial unique index — one would fail with a spurious 500. With the lock,
// exactly ONE active subscription results, stacked to ~2 months, and BOTH
// redeems succeed.
func TestRedeem_ConcurrentSameUserPremiumNoPriorSub(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	user := insertUser(t, db)
	codeA := issueOne(t, db, redemption.IssueSpec{
		GrantKind: redemption.GrantPremiumSubscription, GrantParams: map[string]any{"months": 1},
		Count: 1, SingleUse: true, MaxRedeems: intPtr(1), BatchID: "race-a",
	})
	codeB := issueOne(t, db, redemption.IssueSpec{
		GrantKind: redemption.GrantPremiumSubscription, GrantParams: map[string]any{"months": 1},
		Count: 1, SingleUse: true, MaxRedeems: intPtr(1), BatchID: "race-b",
	})

	eng := newEngine(db, &recordingNotifier{})

	var (
		wg    sync.WaitGroup
		errA  error
		errB  error
		start = make(chan struct{})
	)
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _, errA = eng.Redeem(ctx, user, codeA) }()
	go func() { defer wg.Done(); <-start; _, errB = eng.Redeem(ctx, user, codeB) }()
	close(start)
	wg.Wait()

	require.NoError(t, errA, "redeem A must not spuriously fail under same-user race")
	require.NoError(t, errB, "redeem B must not spuriously fail under same-user race")

	// Exactly ONE active subscription, stacked to ~2 months.
	var active int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM subscriptions WHERE user_id=$1 AND status IN ('active','trialing','past_due')`, user,
	).Scan(&active))
	assert.Equal(t, 1, active, "the user-row lock must prevent a second racing INSERT")

	var periodEnd time.Time
	require.NoError(t, db.QueryRow(`SELECT current_period_end FROM subscriptions WHERE user_id=$1`, user).Scan(&periodEnd))
	assert.WithinDuration(t, time.Now().AddDate(0, 2, 0), periodEnd, 36*time.Hour, "two 1-month codes stack to ~2 months even under concurrency")
}

// TestRedeem_ConcurrentSameUserPremiumCodesNoDeadlock is the regression lock for
// #1306: one user redeeming two premium codes at once used to abort one of them
// with SQLSTATE 40P01. The ledger INSERT's foreign key takes FOR KEY SHARE on the
// users row; the premium grant then asked for FOR UPDATE on the same row, so two
// transactions each held key-share and each waited for the other to release it.
// Oracle: across many rounds, both redeems succeed (codes are distinct and
// single-use, so there is nothing else for either to fail on); a 40P01 abort
// fails that assertion and names itself in the error.
func TestRedeem_ConcurrentSameUserPremiumCodesNoDeadlock(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	eng := newEngine(db, &recordingNotifier{})

	const rounds = 20
	for round := 0; round < rounds; round++ {
		user := insertUser(t, db)
		codes := [2]string{}
		for i := range codes {
			codes[i] = issueOne(t, db, redemption.IssueSpec{
				GrantKind: redemption.GrantPremiumSubscription, GrantParams: map[string]any{"months": 1},
				Count: 1, SingleUse: true, MaxRedeems: intPtr(1), BatchID: fmt.Sprintf("dl-%d-%d", round, i),
			})
		}

		var (
			wg    sync.WaitGroup
			errs  [2]error
			start = make(chan struct{})
		)
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = eng.Redeem(context.Background(), user, codes[i])
			}()
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			assert.NoErrorf(t, err, "round %d: redeem %d of two concurrent same-user premium codes must succeed "+
				"(a 40P01 here is the ledger FK key-share, then users FOR UPDATE upgrade)", round, i)
		}
	}
}

func intPtr(i int) *int              { return &i }
func timePtr(t time.Time) *time.Time { return &t }

// insertStripeRow inserts a live Stripe-sourced premium row carrying the unique
// ids subscriptions_stripe_identity_check requires, and returns its id.
func insertStripeRow(t *testing.T, db *sql.DB, user uuid.UUID, periodEnd time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.QueryRow(
		`INSERT INTO subscriptions
		     (user_id, tier, status, source, current_period_end, stripe_subscription_id, stripe_customer_id)
		 VALUES ($1, 'premium', 'active', 'stripe', $2, $3, $4)
		 RETURNING id`,
		user, periodEnd, "sub_test_"+uuid.NewString(), "cus_test_"+uuid.NewString(),
	).Scan(&id))
	return id
}

// insertCodeRow inserts a code-sourced premium row in the live status 'active'
// with the given period end, which may already be past, and returns its id.
func insertCodeRow(t *testing.T, db *sql.DB, user uuid.UUID, periodEnd time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.QueryRow(
		`INSERT INTO subscriptions (user_id, tier, status, source, current_period_end)
		 VALUES ($1, 'premium', 'active', 'code', $2)
		 RETURNING id`,
		user, periodEnd,
	).Scan(&id))
	return id
}

// subscriptionRowText renders one subscriptions row, every column including
// updated_at, as JSON text, so a test can assert the row is byte-identical.
func subscriptionRowText(t *testing.T, db *sql.DB, id uuid.UUID) string {
	t.Helper()
	var row string
	require.NoError(t, db.QueryRow(
		`SELECT row_to_json(s)::text FROM subscriptions s WHERE s.id = $1`, id,
	).Scan(&row))
	return row
}

// codeRowCount counts the user's code-sourced rows in any status.
func codeRowCount(t *testing.T, db *sql.DB, user uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM subscriptions WHERE user_id = $1 AND source = 'code'`, user,
	).Scan(&n))
	return n
}

// ledgerSubscription returns the subscription the user's one redemption
// ledger row links.
func ledgerSubscription(t *testing.T, db *sql.DB, user uuid.UUID) uuid.NullUUID {
	t.Helper()
	var linked uuid.NullUUID
	require.NoError(t, db.QueryRow(
		`SELECT resulting_subscription_id FROM code_redemptions WHERE user_id = $1`, user,
	).Scan(&linked))
	return linked
}

// redeemPremiumMonth issues one single-use, one-month premium:subscription
// code and redeems it as user.
func redeemPremiumMonth(t *testing.T, db *sql.DB, user uuid.UUID, batchID string, n redemption.Notifier) {
	t.Helper()
	code := issueOne(t, db, redemption.IssueSpec{
		GrantKind:   redemption.GrantPremiumSubscription,
		GrantParams: map[string]any{"months": 1},
		Count:       1,
		SingleUse:   true,
		MaxRedeems:  intPtr(1),
		BatchID:     batchID,
	})
	_, err := newEngine(db, n).Redeem(context.Background(), user, code)
	require.NoError(t, err)
}

// TestRedeem_PremiumNeverWritesStripeRow is the #2033 regression lock (spec
// §5): a Stripe payer's redemption leaves the Stripe row byte-identical (same
// current_period_end, same updated_at, every column) and grants the month on a
// separate code row, which the ledger links.
func TestRedeem_PremiumNeverWritesStripeRow(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := insertUser(t, db)
	stripeID := insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 1, 0).Truncate(time.Second))
	before := subscriptionRowText(t, db, stripeID)

	notifier := &recordingNotifier{}
	redeemPremiumMonth(t, db, user, "stripe-regression-lock", notifier)

	assert.Equal(t, before, subscriptionRowText(t, db, stripeID), "a redemption must never write a Stripe row")
	require.Equal(t, 1, codeRowCount(t, db, user), "the month lands on a new code row")
	var codeID uuid.UUID
	var codeEnd time.Time
	require.NoError(t, db.QueryRow(
		`SELECT id, current_period_end FROM subscriptions WHERE user_id = $1 AND source = 'code'`, user,
	).Scan(&codeID, &codeEnd))
	assert.WithinDuration(t, time.Now().AddDate(0, 1, 0), codeEnd, time.Minute)
	assert.Equal(t, uuid.NullUUID{UUID: codeID, Valid: true}, ledgerSubscription(t, db, user),
		"the ledger links the code row, not the Stripe row")

	// OldTier is "free" here although the payer is premium: the grant only
	// sees code rows. PR-2's D1 refuses this redemption (409 already_covered),
	// so only NewTier is pinned.
	require.Len(t, notifier.calls, 1)
	assert.Equal(t, "premium", notifier.calls[0][2])
}

// TestRedeem_PremiumExtendsCodeRowBesideStripeRow (Review Focus RF1): a user
// with a live code row AND a live Stripe row redeems; only the code row is
// extended and the Stripe row is untouched. The Stripe row is inserted first,
// so a select that ignored source would find it first.
func TestRedeem_PremiumExtendsCodeRowBesideStripeRow(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := insertUser(t, db)
	stripeID := insertStripeRow(t, db, user, time.Now().UTC().AddDate(0, 1, 0).Truncate(time.Second))
	codeEnd := time.Now().UTC().AddDate(0, 0, 10).Truncate(time.Second)
	codeID := insertCodeRow(t, db, user, codeEnd)
	stripeBefore := subscriptionRowText(t, db, stripeID)

	notifier := &recordingNotifier{}
	redeemPremiumMonth(t, db, user, "rf1-code-beside-stripe", notifier)

	assert.Equal(t, stripeBefore, subscriptionRowText(t, db, stripeID), "the Stripe row must be untouched")
	var extended time.Time
	require.NoError(t, db.QueryRow(
		`SELECT current_period_end FROM subscriptions WHERE id = $1`, codeID,
	).Scan(&extended))
	assert.WithinDuration(t, codeEnd.AddDate(0, 1, 0), extended, time.Second, "the code row stacks a month on its own end")
	assert.Equal(t, 1, codeRowCount(t, db, user), "the code row is extended, not duplicated")
	assert.Equal(t, uuid.NullUUID{UUID: codeID, Valid: true}, ledgerSubscription(t, db, user))

	require.Len(t, notifier.calls, 1)
	assert.Equal(t, [3]string{user.String(), "premium", "premium"}, notifier.calls[0])
}

// TestRedeem_PremiumExtendsLapsedUnsweptCodeRow (Review Focus RF2): a code row
// whose period has ended but which the expiry sweep has not flipped yet is
// still in the live status set, so idx_subscriptions_user_live_code covers it.
// The grant must find and extend that row: inserting a second code row would
// fail with 23505. This pins the select's deliberate lack of a time predicate.
func TestRedeem_PremiumExtendsLapsedUnsweptCodeRow(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := insertUser(t, db)
	codeID := insertCodeRow(t, db, user, time.Now().UTC().Add(-time.Hour).Truncate(time.Second))

	notifier := &recordingNotifier{}
	redeemPremiumMonth(t, db, user, "rf2-lapsed-unswept", notifier)

	assert.Equal(t, 1, codeRowCount(t, db, user), "the lapsed row is extended, not duplicated")
	var status string
	var extended time.Time
	require.NoError(t, db.QueryRow(
		`SELECT status, current_period_end FROM subscriptions WHERE id = $1`, codeID,
	).Scan(&status, &extended))
	assert.Equal(t, "active", status)
	assert.WithinDuration(t, time.Now().AddDate(0, 1, 0), extended, time.Minute, "a lapsed period restarts from now")
	assert.Equal(t, uuid.NullUUID{UUID: codeID, Valid: true}, ledgerSubscription(t, db, user))

	require.Len(t, notifier.calls, 1)
	assert.Equal(t, "premium", notifier.calls[0][2])
}
