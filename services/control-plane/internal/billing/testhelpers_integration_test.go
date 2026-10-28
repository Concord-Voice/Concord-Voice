package billing

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq" // registers the PostgreSQL driver used by openProbeDB
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Fixtures shared by the billing integration tests. They use the cycle-free
// testdb package, not internal/testhelpers: that package's test server imports
// the API router, which imports billing, and a billing test in package billing
// would close the loop.

const (
	// testEnvironment is the Service's Environment; a subscription whose
	// metadata concord_env differs belongs to another box on the shared Stripe
	// test account.
	testEnvironment = "test-env-a"
	// subMetaUserID and subMetaEnv are the subscription metadata keys Checkout
	// stamps and Reconcile's self-heal reads.
	subMetaUserID = "concord_user_id"
	subMetaEnv    = "concord_env"

	// testPeriodEnd is 2100-01-01 UTC: far enough ahead that a seeded row is
	// live under entitlements.LiveRowPredicate whatever the clock says.
	testPeriodEnd int64 = 4_102_444_800
)

// tierCall is one OnTierChange invocation.
type tierCall struct {
	UserID uuid.UUID
	Old    string
	New    string
}

// recordingChanger is the TierChanger the tests inject. It records every call
// and can be told to fail, to prove a notify error does not fail Reconcile.
type recordingChanger struct {
	mu    sync.Mutex
	calls []tierCall
	err   error
}

func (r *recordingChanger) OnTierChange(_ context.Context, userID uuid.UUID, oldTier, newTier string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, tierCall{UserID: userID, Old: oldTier, New: newTier})
	return r.err
}

// Calls returns a copy of the recorded calls, in arrival order.
func (r *recordingChanger) Calls() []tierCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tierCall(nil), r.calls...)
}

// syncBuffer is a log sink safe for the concurrent writers a Service has.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything logged so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testEnv is one Service wired to the test database, a Stripe fake, a
// recording TierChanger and a captured log.
type testEnv struct {
	Svc     *Service
	DB      *sql.DB
	Fake    *fakeStripe
	Changer *recordingChanger
	Logs    *syncBuffer
}

// newTestService builds a Service over the test database (migrated, truncated
// on cleanup), the fake Stripe, and a TierChanger that records. Test-mode key,
// Environment testEnvironment.
func newTestService(t *testing.T, fake *fakeStripe) *testEnv {
	t.Helper()
	db, _ := dbtest.SetupTestDB(t)
	changer := &recordingChanger{}
	logs := &syncBuffer{}
	svc := NewService(Deps{
		DB:          db,
		Stripe:      fake.client(),
		Billing:     config.BillingConfig{KeyMode: config.StripeKeyModeTest, WebhookSecret: "whsec_fake"},
		Environment: testEnvironment,
		Changer:     changer,
		Log:         logger.NewWithWriter(logs),
	})
	return &testEnv{Svc: svc, DB: db, Fake: fake, Changer: changer, Logs: logs}
}

// newPayer creates a user with a stripe_customers mapping to customerID.
func newPayer(t *testing.T, env *testEnv, customerID string) uuid.UUID {
	t.Helper()
	userID := dbtest.CreateUser(t, env.DB)
	seedMapping(t, env.DB, userID, customerID)
	return userID
}

// seedMapping inserts the user to Stripe Customer mapping.
func seedMapping(t *testing.T, db *sql.DB, userID uuid.UUID, customerID string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO stripe_customers (user_id, stripe_customer_id) VALUES ($1, $2)`,
		userID, customerID)
	require.NoError(t, err, "seed stripe_customers")
}

// seedStripeSub inserts a source='stripe' premium row for a Wing yearly plan,
// the shape Reconcile would have written for the same Stripe subscription.
func seedStripeSub(t *testing.T, db *sql.DB, userID uuid.UUID, subID, customerID, status string, periodEnd time.Time) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO subscriptions
		   (user_id, tier, status, source, stripe_customer_id, stripe_subscription_id,
		    current_period_end, plan, billing_interval, stripe_unit_amount)
		 VALUES ($1, $2, $3, 'stripe', $4, $5, $6, 'wing', 'year', 16489)`,
		userID, entitlements.TierPremium, status, customerID, subID, periodEnd)
	require.NoError(t, err, "seed stripe subscription row")
}

// seedLiveCodeSub inserts the live premium code row a redeemed gift code
// leaves: source='code', no Stripe ids, a period end that never lapses (NULL).
func seedLiveCodeSub(t *testing.T, db *sql.DB, userID uuid.UUID) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO subscriptions (user_id, tier, status, source)
		 VALUES ($1, $2, 'active', 'code')`,
		userID, entitlements.TierPremium)
	require.NoError(t, err, "seed code subscription row")
}

// subRow is every column Reconcile may write on a subscriptions row.
type subRow struct {
	ID                uuid.UUID
	UserID            uuid.UUID
	Tier              string
	Status            string
	Source            string
	CustomerID        sql.NullString
	SubscriptionID    sql.NullString
	PeriodEnd         sql.NullTime
	CancelAtPeriodEnd bool
	Plan              sql.NullString
	Interval          sql.NullString
	FoundersRate      bool
	CancelAt          sql.NullTime
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

const subRowColumns = `id, user_id, tier, status, source, stripe_customer_id, stripe_subscription_id,
	current_period_end, cancel_at_period_end, plan, billing_interval, founders_rate, cancel_at,
	created_at, updated_at`

func scanSubRow(sc interface{ Scan(dest ...any) error }) (subRow, error) {
	var r subRow
	err := sc.Scan(&r.ID, &r.UserID, &r.Tier, &r.Status, &r.Source, &r.CustomerID, &r.SubscriptionID,
		&r.PeriodEnd, &r.CancelAtPeriodEnd, &r.Plan, &r.Interval, &r.FoundersRate, &r.CancelAt,
		&r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// loadSubRow reads the one row for a Stripe subscription id. It fails the test
// when the row is absent; use countSubsBySubID to assert absence.
func loadSubRow(t *testing.T, db *sql.DB, subID string) subRow {
	t.Helper()
	row, err := scanSubRow(db.QueryRow(
		`SELECT `+subRowColumns+` FROM subscriptions WHERE stripe_subscription_id = $1`, subID))
	require.NoError(t, err, "load subscriptions row %s", subID)
	return row
}

// countSubsBySubID counts rows holding the Stripe subscription id.
func countSubsBySubID(t *testing.T, db *sql.DB, subID string) int {
	t.Helper()
	return scalarInt(t, db, `SELECT COUNT(*) FROM subscriptions WHERE stripe_subscription_id = $1`, subID)
}

// countUserSubs counts every subscriptions row the user holds.
func countUserSubs(t *testing.T, db *sql.DB, userID uuid.UUID) int {
	t.Helper()
	return scalarInt(t, db, `SELECT COUNT(*) FROM subscriptions WHERE user_id = $1`, userID)
}

// countTable counts all rows of a fixed table name (a test constant, never input).
func countTable(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	return scalarInt(t, db, `SELECT COUNT(*) FROM `+table)
}

func scalarInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n), "scalar query")
	return n
}

// mappedCustomer returns the Stripe Customer the user is mapped to, or "" if none.
func mappedCustomer(t *testing.T, db *sql.DB, userID uuid.UUID) string {
	t.Helper()
	var cus sql.NullString
	err := db.QueryRow(`SELECT stripe_customer_id FROM stripe_customers WHERE user_id = $1`, userID).Scan(&cus)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	require.NoError(t, err)
	return cus.String
}

// dedupeCount counts stripe_webhook_events rows for the event id.
func dedupeCount(t *testing.T, db *sql.DB, eventID string) int {
	t.Helper()
	return scalarInt(t, db, `SELECT COUNT(*) FROM stripe_webhook_events WHERE event_id = $1`, eventID)
}

// editFakeSub applies fn to the fake's stored subscription under the fake's
// lock, so a mutation never races the HTTP handler goroutine that renders it.
func editFakeSub(t *testing.T, fake *fakeStripe, subID string, fn func(*fakeSubscription)) {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, sub := range fake.subscriptions {
		if sub.ID == subID {
			fn(sub)
			return
		}
	}
	t.Fatalf("fake stripe has no subscription %q to edit", subID)
}

// seedFakeSub seeds a Wing yearly active subscription on customerID with the
// given metadata (nil for none), ending at testPeriodEnd.
func seedFakeSub(t *testing.T, fake *fakeStripe, subID, customerID string, metadata map[string]string) {
	t.Helper()
	fake.seedSubscription(subID, customerID, PlanWing, "year", testPeriodEnd)
	if metadata != nil {
		editFakeSub(t, fake, subID, func(s *fakeSubscription) { s.Metadata = metadata })
	}
}

// openProbeDB opens an extra pool on the test database, distinct from the
// Service's, for lock barriers and waiter probes.
func openProbeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	return db
}

// reconcileAdvisoryKey computes Reconcile's per-subscription advisory key in
// SQL, so the test asserts the production key rather than a copy of its hash.
func reconcileAdvisoryKey(t *testing.T, probe *sql.DB, subID string) int64 {
	t.Helper()
	var key int64
	require.NoError(t, probe.QueryRow(
		`SELECT hashtextextended('concord.stripe_sub:' || $1::text, 0)`, subID).Scan(&key))
	return key
}
