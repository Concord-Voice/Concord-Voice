package subscriptions_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/subscriptions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTierChange is a subscriptions.TierChanger that captures OnTierChange
// calls so a test can assert the downgrade fired with the right (old, new)
// tiers, without a live Redis/WS.
type recordingTierChange struct {
	mu    sync.Mutex
	calls [][3]string // userID, old, new
}

func (r *recordingTierChange) OnTierChange(_ context.Context, userID uuid.UUID, oldTier, newTier string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, [3]string{userID.String(), oldTier, newTier})
	return nil
}

// newTestSweeper builds an ExpirySweeper wired to a recording notifier (no Redis).
func newTestSweeper(db *sql.DB, rec *recordingTierChange) *subscriptions.ExpirySweeper {
	return subscriptions.NewExpirySweeperWith(db, logger.New("test"), rec)
}

// insertSub inserts one premium code-sourced subscription with the given status
// and period end (parameterized — the timestamp is computed Go-side and bound as
// $3, never concatenated into SQL), returning its id.
func insertSub(t *testing.T, db *sql.DB, userID uuid.UUID, status string, periodEnd time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.QueryRow(`
		INSERT INTO subscriptions (user_id, tier, status, source, current_period_end)
		VALUES ($1, 'premium', $2, 'code', $3)
		RETURNING id`, userID, status, periodEnd).Scan(&id))
	return id
}

func statusOf(t *testing.T, db *sql.DB, id uuid.UUID) string {
	t.Helper()
	var s string
	require.NoError(t, db.QueryRow(`SELECT status FROM subscriptions WHERE id = $1`, id).Scan(&s))
	return s
}

// TestSweep_ExpiresLapsedPremium: an active premium sub past its period end is
// flipped to 'expired' and fires a premium->free downgrade notification.
func TestSweep_ExpiresLapsedPremium(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	user := testhelpers.CreateUser(t, db)
	subID := insertSub(t, db, user, "active", time.Now().Add(-time.Hour))

	rec := &recordingTierChange{}
	n, err := newTestSweeper(db, rec).SweepExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	assert.Equal(t, "expired", statusOf(t, db, subID), "lapsed sub must be flipped to expired")
	require.Len(t, rec.calls, 1)
	assert.Equal(t, [3]string{user.String(), "premium", "free"}, rec.calls[0],
		"expiry must notify premium->free (a downgrade)")
}

// TestSweep_LeavesValidPremium: a premium sub whose period end is in the future
// is NOT swept (no status change, no notification).
func TestSweep_LeavesValidPremium(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	user := testhelpers.CreateUser(t, db)
	subID := insertSub(t, db, user, "active", time.Now().Add(30*24*time.Hour))

	rec := &recordingTierChange{}
	n, err := newTestSweeper(db, rec).SweepExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, "active", statusOf(t, db, subID), "still-valid sub must stay active")
	assert.Empty(t, rec.calls)
}

// TestSweep_LeavesStripeSourced: a lapsed premium sub whose source is 'stripe'
// is NOT swept. Stripe subscription lifecycle is owned by the Stripe webhook
// path (#1306), whose renewal can lag the locally-stored current_period_end;
// expiring it here would fire a premium->free downgrade/disconnect before Stripe
// declared the subscription terminal. Only code-owned grants are the sweeper's
// responsibility.
func TestSweep_LeavesStripeSourced(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	user := testhelpers.CreateUser(t, db)
	var subID uuid.UUID
	require.NoError(t, db.QueryRow(`
		INSERT INTO subscriptions (user_id, tier, status, source, current_period_end)
		VALUES ($1, 'premium', 'active', 'stripe', $2)
		RETURNING id`, user, time.Now().Add(-time.Hour)).Scan(&subID))

	rec := &recordingTierChange{}
	n, err := newTestSweeper(db, rec).SweepExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a stripe-sourced row is owned by the Stripe webhook path, not the sweep")
	assert.Equal(t, "active", statusOf(t, db, subID), "stripe row must stay live")
	assert.Empty(t, rec.calls, "no downgrade may be fired for a stripe-sourced row")
}

// TestSweep_IdempotentAcrossRuns: re-running the sweep (e.g. after a restart)
// does not re-expire or re-notify an already-expired subscription.
func TestSweep_IdempotentAcrossRuns(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	user := testhelpers.CreateUser(t, db)
	insertSub(t, db, user, "active", time.Now().Add(-time.Hour))

	rec := &recordingTierChange{}
	sweeper := newTestSweeper(db, rec)

	first, err := sweeper.SweepExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, first)

	second, err := sweeper.SweepExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, second, "second sweep must find nothing new")
	assert.Len(t, rec.calls, 1, "an already-expired sub must not be re-notified")
}

// fakeSessionNotifier is an entitlements.SessionNotifier that records the
// downgrade disconnect + entitlements_changed push, exercising the production
// NewExpirySweeper -> tierChangeAdapter -> entitlements.OnTierChange path against
// a real Redis-backed Cache (no live WS hub needed).
type fakeSessionNotifier struct {
	mu           sync.Mutex
	disconnected []uuid.UUID
	broadcasts   int
}

func (f *fakeSessionNotifier) DisconnectUser(id uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnected = append(f.disconnected, id)
}

func (f *fakeSessionNotifier) BroadcastEntitlements(_ uuid.UUID, _ entitlements.EntitlementDTO) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcasts++
}

// TestSweep_ProductionPath_RealCacheAndNotifier drives the production
// constructor (NewExpirySweeper + tierChangeAdapter) through a real
// entitlements.OnTierChange over a real Redis Cache, asserting the downgrade
// both pushes and disconnects.
func TestSweep_ProductionPath_RealCacheAndNotifier(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	user := ts.CreateTestUser(t, "expiry_prod_path")
	uid := uuid.MustParse(user.ID)
	subID := insertSub(t, ts.DB, uid, "active", time.Now().Add(-time.Hour))

	cache := entitlements.NewCache(ts.Redis, ts.DB)
	notifier := &fakeSessionNotifier{}
	sweeper := subscriptions.NewExpirySweeper(ts.DB, logger.New("test"), cache, notifier)

	n, err := sweeper.SweepExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "expired", statusOf(t, ts.DB, subID))

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	assert.Equal(t, 1, notifier.broadcasts, "downgrade must push entitlements_changed")
	assert.Equal(t, []uuid.UUID{uid}, notifier.disconnected, "premium->free must force-disconnect")
}

// TestSweep_SelfHosted_NoSpuriousDowngrade is the SaaS production-path test's
// self-hosted twin: on a self-hosted instance every user is premium, so the
// post-flip tier resolves to premium (premium->premium). The row is still
// honestly expired and entitlements are still pushed, but the change is NOT a
// downgrade, so the user must NOT be force-disconnected (#2158 self-hosted). A
// DB-only resolve would have read free and wrongly kicked a premium user.
func TestSweep_SelfHosted_NoSpuriousDowngrade(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	user := ts.CreateTestUser(t, "expiry_selfhosted")
	uid := uuid.MustParse(user.ID)
	subID := insertSub(t, ts.DB, uid, "active", time.Now().Add(-time.Hour))

	cache := entitlements.NewCacheForInstance(ts.Redis, ts.DB, config.InstanceTypeSelfHosted)
	notifier := &fakeSessionNotifier{}
	sweeper := subscriptions.NewExpirySweeper(ts.DB, logger.New("test"), cache, notifier)

	n, err := sweeper.SweepExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "expired", statusOf(t, ts.DB, subID), "the row is still honestly expired")

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	assert.Equal(t, 1, notifier.broadcasts, "entitlements are still pushed on the change")
	assert.Empty(t, notifier.disconnected, "self-hosted stays premium: expiry must NOT force-disconnect")
}

// TestExpirySweepWorker_StartupSweepAndStop covers StartExpirySweepWorker: the
// once-at-startup sweep flips a pre-existing lapsed sub, and the worker stops
// cleanly on context cancellation.
func TestExpirySweepWorker_StartupSweepAndStop(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	user := ts.CreateTestUser(t, "expiry_worker")
	subID := insertSub(t, ts.DB, uuid.MustParse(user.ID), "active", time.Now().Add(-time.Hour))

	cache := entitlements.NewCache(ts.Redis, ts.DB)
	notifier := &fakeSessionNotifier{}

	// Short interval so the ticker loop is reachable; the startup sweep runs first.
	subscriptions.StartExpirySweepWorker(ctx, ts.DB, logger.New("test"), cache, notifier, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		return statusOf(t, ts.DB, subID) == "expired"
	}, 3*time.Second, 25*time.Millisecond, "startup sweep should expire the lapsed sub")

	notifier.mu.Lock()
	assert.GreaterOrEqual(t, notifier.broadcasts, 1)
	notifier.mu.Unlock()

	cancel() // worker returns on ctx.Done
}

// erroringTierChange always fails the notification, exercising expireOne's
// notify-error branch + SweepExpired's log-and-continue path.
type erroringTierChange struct{}

func (erroringTierChange) OnTierChange(_ context.Context, _ uuid.UUID, _, _ string) error {
	return errors.New("notify boom")
}

// TestSweep_NotifyErrorRollsBackForRetry: when the tier-change notification
// fails, the flip is ROLLED BACK (at-least-once) so the row stays live and a
// later sweep retries — the notification can never be permanently lost. The
// per-row error is logged + skipped and the row is not counted as swept.
func TestSweep_NotifyErrorRollsBackForRetry(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	user := testhelpers.CreateUser(t, db)
	subID := insertSub(t, db, user, "active", time.Now().Add(-time.Hour))

	sweeper := subscriptions.NewExpirySweeperWith(db, logger.New("test"), erroringTierChange{})
	n, err := sweeper.SweepExpired(context.Background())
	require.NoError(t, err, "a per-row notify error is logged + skipped, not fatal to the sweep")
	assert.Equal(t, 0, n, "a row whose notify failed is not counted as swept")
	assert.Equal(t, "active", statusOf(t, db, subID),
		"a failed notify rolls back the flip so the row is retried (at-least-once)")

	// A subsequent sweep with a working notifier converges the row to expired.
	rec := &recordingTierChange{}
	n2, err := subscriptions.NewExpirySweeperWith(db, logger.New("test"), rec).SweepExpired(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n2, "the retry expires the still-live row")
	assert.Equal(t, "expired", statusOf(t, db, subID))
	require.Len(t, rec.calls, 1, "the downgrade is delivered on retry")
}

// TestSweep_QueryErrorPropagates: a DB-level failure in the select surfaces as a
// sweep error (not a silent success), exercising the error-return branches.
func TestSweep_QueryErrorPropagates(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	cleanup() // close the pool so the sweep's SELECT fails deterministically

	sweeper := subscriptions.NewExpirySweeperWith(db, logger.New("test"), &recordingTierChange{})
	_, err := sweeper.SweepExpired(context.Background())
	require.Error(t, err, "a select failure must propagate, not be swallowed")
}
