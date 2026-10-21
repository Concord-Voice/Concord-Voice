package entitlements_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"regexp"
	"sort"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveRowExecer is the write surface shared by *sql.DB and *sql.Tx, so a
// fixture row can be written on the pool or inside a test transaction.
type liveRowExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertLiveRow inserts one subscriptions row. periodOffset is a PostgreSQL
// interval added to the database clock (for example "-5 hours -59 minutes");
// "" stores a NULL current_period_end. Computing the period end in SQL puts
// every boundary case on the same clock as the predicate's NOW(). A 'stripe'
// row carries the unique stripe_subscription_id and stripe_customer_id that
// subscriptions_stripe_identity_check requires.
func insertLiveRow(t *testing.T, q liveRowExecer, userID uuid.UUID, tier, status, source, periodOffset string) {
	t.Helper()
	var stripeSubID, stripeCusID any
	if source == "stripe" {
		stripeSubID = "sub_test_" + uuid.NewString()
		stripeCusID = "cus_test_" + uuid.NewString()
	}
	_, err := q.ExecContext(context.Background(),
		`INSERT INTO subscriptions
		     (user_id, tier, status, source, current_period_end, stripe_subscription_id, stripe_customer_id)
		 VALUES ($1, $2, $3, $4, NOW() + NULLIF($5::text, '')::interval, $6, $7)`,
		userID, tier, status, source, periodOffset, stripeSubID, stripeCusID)
	require.NoError(t, err)
}

// TestLiveRowPredicate_Boundaries pins the per-source liveness rule through
// ResolveTierQ: a Stripe row keeps a 6-hour renewal buffer past its period
// end, a code row has none, and only the live status set entitles.
func TestLiveRowPredicate_Boundaries(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	cases := []struct {
		name, status, source, offset, want string
	}{
		{"stripe 5h59m past period end entitles (renewal buffer)", "active", "stripe", "-5 hours -59 minutes", entitlements.TierPremium},
		{"stripe 6h01m past period end does not entitle", "active", "stripe", "-6 hours -1 minutes", entitlements.TierFree},
		{"stripe past_due inside its period entitles", "past_due", "stripe", "3 days", entitlements.TierPremium},
		{"stripe unpaid inside its period does not entitle", "unpaid", "stripe", "3 days", entitlements.TierFree},
		{"stripe paused inside its period does not entitle", "paused", "stripe", "3 days", entitlements.TierFree},
		{"stripe incomplete_expired inside its period does not entitle", "incomplete_expired", "stripe", "3 days", entitlements.TierFree},
		{"stripe canceled inside its period does not entitle", "canceled", "stripe", "3 days", entitlements.TierFree},
		{"code 1m past period end does not entitle (no buffer)", "active", "code", "-1 minutes", entitlements.TierFree},
		{"code with a NULL period end entitles", "active", "code", "", entitlements.TierPremium},
		{"code with a future period end entitles", "trialing", "code", "1 hour", entitlements.TierPremium},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user := testhelpers.CreateUser(t, db)
			insertLiveRow(t, db, user, entitlements.TierPremium, tc.status, tc.source, tc.offset)
			assert.Equal(t, tc.want, entitlements.ResolveTierQ(ctx, db, user.String()))
		})
	}
}

// predicateMatches evaluates entitlements.LiveRowPredicate over a one-row
// VALUES subselect that carries only the three columns the predicate reads, so
// a row the subscriptions CHECK constraints would refuse can still be judged.
// periodOffset is a PostgreSQL interval added to the database clock; "" is a
// NULL current_period_end.
func predicateMatches(t *testing.T, db *sql.DB, status, source, periodOffset string) bool {
	t.Helper()
	var matches bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS (
		     SELECT 1
		       FROM (VALUES ($1::text, $2::text, NOW() + NULLIF($3::text, '')::interval))
		            AS subscriptions (status, source, current_period_end)
		      WHERE `+entitlements.LiveRowPredicate+`)`,
		status, source, periodOffset).Scan(&matches))
	return matches
}

// TestLiveRowPredicate_NullPeriodEnd pins the fail-closed arm: a Stripe row
// with no period end is not live (subscriptions_stripe_period_check forbids
// that row, so only the predicate itself can be asked), while a code row with
// none is a grant that never lapses. The control row proves the VALUES harness
// can answer true, so the Stripe result is not an artifact of it.
func TestLiveRowPredicate_NullPeriodEnd(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	assert.True(t, predicateMatches(t, db, "active", "code", ""), "control: a code row with a NULL period end is live")
	assert.True(t, predicateMatches(t, db, "active", "stripe", "1 hour"), "control: a Stripe row with a future period end is live")
	assert.False(t, predicateMatches(t, db, "active", "stripe", ""), "a Stripe row with a NULL period end must not be live")
	assert.False(t, predicateMatches(t, db, "past_due", "stripe", ""), "the live status does not rescue a NULL Stripe period end")
}

// TestLiveRowPredicate_StatusSetOnBothSources pins the status IN-list on every
// source: only active, trialing and past_due entitle, and a period end in the
// future never rescues a status outside the set.
func TestLiveRowPredicate_StatusSetOnBothSources(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	for _, source := range []string{"code", "stripe"} {
		for _, tc := range []struct {
			status string
			want   string
		}{
			{"active", entitlements.TierPremium},
			{"trialing", entitlements.TierPremium},
			{"past_due", entitlements.TierPremium},
			{"incomplete", entitlements.TierFree},
			{"expired", entitlements.TierFree},
			{"canceled", entitlements.TierFree},
		} {
			t.Run(source+" "+tc.status, func(t *testing.T) {
				user := testhelpers.CreateUser(t, db)
				insertLiveRow(t, db, user, entitlements.TierPremium, tc.status, source, "3 days")
				assert.Equal(t, tc.want, entitlements.ResolveTierQ(ctx, db, user.String()))
			})
		}
	}
}

// TestLiveRowPredicate_StripeBufferBoundary pins the 6-hour renewal buffer on
// the predicate itself (the ResolveTierQ cases in TestLiveRowPredicate_Boundaries
// pin the same edge through the resolver), including its exact bounds.
func TestLiveRowPredicate_StripeBufferBoundary(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	assert.True(t, predicateMatches(t, db, "active", "stripe", "-5 hours -59 minutes"))
	assert.False(t, predicateMatches(t, db, "active", "stripe", "-6 hours -1 minutes"))
	assert.False(t, predicateMatches(t, db, "active", "code", "-1 minutes"), "a code row has no buffer")
}

var (
	predicateStatusList = regexp.MustCompile(`status IN \(([^)]*)\)`)
	statusLiteral       = regexp.MustCompile(`'([a-z_]+)'`)
)

// quotedNames returns the sorted, distinct single-quoted lowercase names in s.
func quotedNames(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range statusLiteral.FindAllStringSubmatch(s, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// TestLiveRowIndexPredicates_MatchLiveRowPredicate is the drift guard between
// migration 000179's partial indexes and LiveRowPredicate: each index serves
// the predicate only while its WHERE carries the same live status set, so a
// status added to one side and not the other would silently stop the index from
// matching (a plan change, not an error). It reads the stored predicate, not an
// EXPLAIN plan, because the planner's choice is not stable now that a full
// user_id index exists.
func TestLiveRowIndexPredicates_MatchLiveRowPredicate(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	statusList := predicateStatusList.FindStringSubmatch(entitlements.LiveRowPredicate)
	require.NotNil(t, statusList, "LiveRowPredicate must keep a status IN (...) list")
	want := quotedNames(statusList[1])
	require.Equal(t, []string{"active", "past_due", "trialing"}, want, "the live status set itself changed; review every reader and 000179")

	for _, index := range []string{
		"idx_subscriptions_user_live_code",
		"idx_subscriptions_user_live_stripe",
		"idx_subscriptions_stripe_live_period_end",
	} {
		t.Run(index, func(t *testing.T) {
			var pred sql.NullString
			require.NoError(t, db.QueryRow(`
				SELECT pg_get_expr(i.indpred, i.indrelid)
				  FROM pg_index i
				  JOIN pg_class c ON c.oid = i.indexrelid
				 WHERE c.relname = $1 AND c.relnamespace = current_schema()::regnamespace`,
				index).Scan(&pred))
			require.True(t, pred.Valid, "%s must be a partial index", index)
			// Only the status half of the predicate is compared: the source
			// literal is the per-index scope and differs by design.
			arr := regexp.MustCompile(`ARRAY\[([^\]]*)\]`).FindStringSubmatch(pred.String)
			require.NotNil(t, arr, "no status array in %s predicate: %s", index, pred.String)
			assert.Equal(t, want, quotedNames(arr[1]),
				"%s status set must equal LiveRowPredicate's live set", index)
		})
	}
}

// TestResolveTierQ_CodeAndStripeRowsTogether (Review Focus RF1): since 000179 a
// user may hold a live code row and a live Stripe row at once. This test pins
// that the pair can be inserted (no 23505) and resolves to premium. That every
// live row counts toward the result is pinned by
// TestResolveTierQ_MaxRankAcrossLiveRows, whose rows differ in tier.
func TestResolveTierQ_CodeAndStripeRowsTogether(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	user := testhelpers.CreateUser(t, db)
	insertLiveRow(t, db, user, entitlements.TierPremium, "active", "code", "")
	insertLiveRow(t, db, user, entitlements.TierPremium, "active", "stripe", "30 days")
	assert.Equal(t, entitlements.TierPremium, entitlements.ResolveTierQ(context.Background(), db, user.String()))
}

// TestResolveTierQ_MaxRankAcrossLiveRows: the result is the highest-ranked
// tier over every live row. Both insertion orders run, so an implementation
// that kept the first or the last row fails one of them.
func TestResolveTierQ_MaxRankAcrossLiveRows(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	t.Run("free code row first, premium stripe row second", func(t *testing.T) {
		user := testhelpers.CreateUser(t, db)
		insertLiveRow(t, db, user, entitlements.TierFree, "active", "code", "")
		insertLiveRow(t, db, user, entitlements.TierPremium, "active", "stripe", "30 days")
		assert.Equal(t, entitlements.TierPremium, entitlements.ResolveTierQ(ctx, db, user.String()))
	})
	t.Run("premium code row first, free stripe row second", func(t *testing.T) {
		user := testhelpers.CreateUser(t, db)
		insertLiveRow(t, db, user, entitlements.TierPremium, "active", "code", "")
		insertLiveRow(t, db, user, entitlements.TierFree, "active", "stripe", "30 days")
		assert.Equal(t, entitlements.TierPremium, entitlements.ResolveTierQ(ctx, db, user.String()))
	})
	t.Run("a premium stripe row past its buffer beside a live free code row", func(t *testing.T) {
		user := testhelpers.CreateUser(t, db)
		insertLiveRow(t, db, user, entitlements.TierPremium, "active", "stripe", "-7 hours")
		insertLiveRow(t, db, user, entitlements.TierFree, "active", "code", "")
		assert.Equal(t, entitlements.TierFree, entitlements.ResolveTierQ(ctx, db, user.String()))
	})
}

// TestResolveTierQ_SeesUncommittedWritesInItsTx: on a *sql.Tx the read runs in
// that transaction, so it sees the transaction's own uncommitted row while the
// pool does not. Reconcile, seat ops and erasure rely on this to compute a
// before/after tier on their own transaction.
func TestResolveTierQ_SeesUncommittedWritesInItsTx(t *testing.T) {
	// SetupTestDB registers its own t.Cleanup; that cleanup empties every table
	// and must run after the rollback below, so it is not deferred here.
	db, _ := testhelpers.SetupTestDB(t)
	ctx := context.Background()
	user := testhelpers.CreateUser(t, db)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			t.Errorf("rollback: %v", rbErr)
		}
	}()

	assert.Equal(t, entitlements.TierFree, entitlements.ResolveTierQ(ctx, tx, user.String()), "no row yet")
	insertLiveRow(t, tx, user, entitlements.TierPremium, "active", "code", "")
	assert.Equal(t, entitlements.TierPremium, entitlements.ResolveTierQ(ctx, tx, user.String()), "the tx sees its own write")
	assert.Equal(t, entitlements.TierFree, entitlements.ResolveTierQ(ctx, db, user.String()), "the pool does not see an uncommitted row")
}

// TestResolveTierQ_ErrorsFailClosedToFree: every error path returns TierFree.
// Each fake serves a premium row first, so only the error branch can produce
// free; the control case proves the fake reaches the max-Rank path.
func TestResolveTierQ_ErrorsFailClosedToFree(t *testing.T) {
	ctx := context.Background()
	premium := []driver.Value{entitlements.TierPremium}
	userID := uuid.NewString()

	t.Run("control: the fake reaches the max-Rank path", func(t *testing.T) {
		db := sql.OpenDB(liveRowConnector{rows: [][]driver.Value{{entitlements.TierFree}, premium}})
		t.Cleanup(func() { assert.NoError(t, db.Close()) })
		assert.Equal(t, entitlements.TierPremium, entitlements.ResolveTierQ(ctx, db, userID))
	})
	t.Run("closed pool", func(t *testing.T) {
		db := sql.OpenDB(liveRowConnector{rows: [][]driver.Value{premium}})
		require.NoError(t, db.Close())
		assert.Equal(t, entitlements.TierFree, entitlements.ResolveTierQ(ctx, db, userID))
	})
	t.Run("canceled context", func(t *testing.T) {
		db := sql.OpenDB(liveRowConnector{rows: [][]driver.Value{premium}})
		t.Cleanup(func() { assert.NoError(t, db.Close()) })
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		assert.Equal(t, entitlements.TierFree, entitlements.ResolveTierQ(canceled, db, userID))
	})
	t.Run("scan error after a premium row", func(t *testing.T) {
		db := sql.OpenDB(liveRowConnector{rows: [][]driver.Value{premium, {nil}}})
		t.Cleanup(func() { assert.NoError(t, db.Close()) })
		assert.Equal(t, entitlements.TierFree, entitlements.ResolveTierQ(ctx, db, userID))
	})
	t.Run("iteration error after a premium row", func(t *testing.T) {
		db := sql.OpenDB(liveRowConnector{rows: [][]driver.Value{premium}, nextErr: errors.New("fake: connection reset")})
		t.Cleanup(func() { assert.NoError(t, db.Close()) })
		assert.Equal(t, entitlements.TierFree, entitlements.ResolveTierQ(ctx, db, userID))
	})
}

// liveRowConnector serves canned one-column rows to any query, so the error
// branches of ResolveTierQ run without Postgres. A nil element is SQL NULL.
// After the rows, Next returns nextErr, or io.EOF when nextErr is nil.
type liveRowConnector struct {
	rows    [][]driver.Value
	nextErr error
}

func (c liveRowConnector) Connect(context.Context) (driver.Conn, error) {
	return liveRowConn(c), nil
}
func (liveRowConnector) Driver() driver.Driver { return liveRowDriver{} }

type liveRowDriver struct{}

func (liveRowDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use the test connector")
}

type liveRowConn liveRowConnector

func (liveRowConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("test connection does not prepare statements")
}
func (liveRowConn) Close() error { return nil }
func (liveRowConn) Begin() (driver.Tx, error) {
	return nil, errors.New("test connection does not begin transactions")
}
func (c liveRowConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &liveRows{rows: c.rows, nextErr: c.nextErr}, nil
}

type liveRows struct {
	rows    [][]driver.Value
	nextErr error
}

func (*liveRows) Columns() []string { return []string{"tier"} }
func (*liveRows) Close() error      { return nil }
func (r *liveRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		if r.nextErr != nil {
			return r.nextErr
		}
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}
