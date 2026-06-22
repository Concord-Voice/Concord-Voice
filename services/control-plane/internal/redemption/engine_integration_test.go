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

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/redemption"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
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

	for _, kind := range []string{"feature:custom_themes", "cosmetic:founder_badge"} {
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

func intPtr(i int) *int              { return &i }
func timePtr(t time.Time) *time.Time { return &t }
