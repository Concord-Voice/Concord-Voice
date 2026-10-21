package database_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000178 (epic #2033 PR-1, Refs #1305): every Stripe subscription
// status, the plan / billing_interval / founders_rate / cancel_at columns, and
// the two CHECKs that pin a Stripe row's shape.
//
// SetupTestDB already migrates to head. Every round trip and refusal below runs
// the .down.sql / .up.sql contents inside a transaction that is always rolled
// back (the TestMigration000157 shape), so the schema shared by the rest of this
// package's test binary is never left downgraded.
//
// The generic helpers in this file are shared by the 000179-000182 tests.

// migrationExecer is the Exec surface shared by *sql.DB and *sql.Tx.
type migrationExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// migrationRollback rolls tx back, failing the test on any error except
// sql.ErrTxDone, which is the expected answer once the test committed tx.
func migrationRollback(t *testing.T, tx *sql.Tx) {
	t.Helper()
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("rollback: %v", err)
	}
}

// migrationSchemaSnapshot describes table's columns, constraints and indexes as
// tx sees them, its own uncommitted DDL included, one sorted line each, so a
// down-then-up round trip can be compared exactly. An absent table yields an
// empty slice.
func migrationSchemaSnapshot(t *testing.T, tx *sql.Tx, table string) []string {
	t.Helper()
	rows, err := tx.Query(`
		SELECT 'column ' || column_name || ' ' || data_type || ' '
		       || COALESCE(character_maximum_length::text, '-') || ' '
		       || is_nullable || ' ' || COALESCE(column_default, '-')
		  FROM information_schema.columns
		 WHERE table_schema = current_schema() AND table_name = $1::text
		UNION ALL
		SELECT 'constraint ' || conname || ' ' || pg_get_constraintdef(oid)
		  FROM pg_constraint
		 WHERE conrelid = to_regclass(quote_ident(current_schema()) || '.' || quote_ident($1::text))
		UNION ALL
		SELECT 'index ' || indexname || ' ' || indexdef
		  FROM pg_indexes
		 WHERE schemaname = current_schema() AND tablename = $1::text
		 ORDER BY 1`, table)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var out []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		out = append(out, line)
	}
	require.NoError(t, rows.Err())
	return out
}

// migrationSnapshotLine returns the first snapshot line that starts with
// prefix, or "" when none does.
func migrationSnapshotLine(snapshot []string, prefix string) string {
	for _, line := range snapshot {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// migrationRequirePQError asserts err is a PostgreSQL error with SQLSTATE code
// and, when constraint is non-empty, naming that constraint or unique index.
func migrationRequirePQError(t *testing.T, err error, code, constraint string) {
	t.Helper()
	require.Error(t, err)
	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr), "expected a PostgreSQL error, got %T: %v", err, err)
	assert.Equal(t, code, string(pqErr.Code), pqErr.Message)
	if constraint != "" {
		assert.Equal(t, constraint, pqErr.Constraint)
	}
}

// migrationRequireGuardRunsUnderLock proves a guarded down takes its ACCESS
// EXCLUSIVE lock on table BEFORE it looks for blocking rows. insertBlocker
// writes a blocking row in a transaction that is still open when the down
// starts; the down parks on its LOCK TABLE, the row commits, and the down must
// then refuse with wantMessage. A down that checked first would find nothing,
// wait on the lock, and then drop.
func migrationRequireGuardRunsUnderLock(t *testing.T, db *sql.DB, down, table, wantMessage string, insertBlocker func(tx *sql.Tx) error) {
	t.Helper()
	probe, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, probe.Close()) })

	// downTx's rollback is deferred first so it runs last: if this helper fails
	// while the down is parked, the blocker's rollback releases the down before
	// downTx.Rollback waits for the in-flight Exec.
	downTx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, downTx)

	blocker, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, blocker)
	require.NoError(t, insertBlocker(blocker))

	result := make(chan error, 1)
	go func() {
		_, execErr := downTx.Exec(down)
		result <- execErr
	}()

	testdb.WaitForRelationLockWaiter(t, probe, table, "AccessExclusiveLock")
	require.NoError(t, blocker.Commit())

	err = <-result
	require.Error(t, err, "the down must refuse a row that committed while it waited for its lock")
	migrationRequirePQError(t, err, "23001", "")
	assert.Contains(t, err.Error(), wantMessage)
}

func migration000178UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000178_subscriptions_stripe_billing.up.sql")
}

func migration000178DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000178_subscriptions_stripe_billing.down.sql")
}

// migration000178InsertStripe writes a well-formed Stripe row for userID: both
// Stripe ids, unique per call, and a period end 30 days out.
func migration000178InsertStripe(q migrationExecer, userID uuid.UUID, status string) error {
	_, err := q.Exec(`
		INSERT INTO subscriptions
		    (user_id, tier, status, source, stripe_customer_id, stripe_subscription_id, current_period_end)
		VALUES ($1, 'premium', $2, 'stripe', $3, $4, NOW() + INTERVAL '30 days')`,
		userID, status, "cus_test_"+uuid.NewString(), "sub_test_"+uuid.NewString())
	return err
}

// migration000178InsertCode writes a code-sourced row for userID.
func migration000178InsertCode(q migrationExecer, userID uuid.UUID, status string) error {
	_, err := q.Exec(`
		INSERT INTO subscriptions (user_id, tier, status, source)
		VALUES ($1, 'premium', $2, 'code')`, userID, status)
	return err
}

func TestMigration000178_UpDownUpRoundTrip(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)

	applied := migrationSchemaSnapshot(t, tx, "subscriptions")
	for _, prefix := range []string{
		"column plan character varying 16 YES -",
		"column billing_interval character varying 8 YES -",
		"column founders_rate boolean - NO false",
		"column cancel_at timestamp with time zone - YES -",
		"constraint subscriptions_plan_check ",
		"constraint subscriptions_billing_interval_check ",
		"constraint subscriptions_stripe_identity_check ",
		"constraint subscriptions_stripe_period_check ",
	} {
		require.NotEmpty(t, migrationSnapshotLine(applied, prefix), "missing after up: %s", prefix)
	}
	require.Contains(t, migrationSnapshotLine(applied, "constraint subscriptions_status_check "), "'incomplete_expired'")

	_, err = tx.Exec(migration000178DownSQL(t))
	require.NoError(t, err, "the down must succeed with no Stripe row and no new-status row")

	reverted := migrationSchemaSnapshot(t, tx, "subscriptions")
	for _, prefix := range []string{
		"column plan ", "column billing_interval ", "column founders_rate ", "column cancel_at ",
		"constraint subscriptions_plan_check ", "constraint subscriptions_billing_interval_check ",
		"constraint subscriptions_stripe_identity_check ", "constraint subscriptions_stripe_period_check ",
	} {
		assert.Empty(t, migrationSnapshotLine(reverted, prefix), "left behind by the down: %s", prefix)
	}
	status := migrationSnapshotLine(reverted, "constraint subscriptions_status_check ")
	assert.Contains(t, status, "'expired'", "the down must restore the 000083 status set")
	for _, added := range []string{"'incomplete_expired'", "'unpaid'", "'paused'"} {
		assert.NotContains(t, status, added)
	}

	_, err = tx.Exec(migration000178UpSQL(t))
	require.NoError(t, err, "the up must re-apply after its down")
	assert.Equal(t, applied, migrationSchemaSnapshot(t, tx, "subscriptions"),
		"down then up must rebuild exactly the schema the migration chain built")
}

func TestMigration000178_DownRefusesUnderLock(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	down := migration000178DownSQL(t)
	user := testhelpers.CreateUser(t, db)

	for _, tc := range []struct {
		name, wantMessage string
		seed              func(tx *sql.Tx) error
	}{
		{
			name:        "a code row in a status 000178 added",
			wantMessage: "cannot roll back 000178 while a subscriptions row has status incomplete_expired, unpaid or paused",
			seed:        func(tx *sql.Tx) error { return migration000178InsertCode(tx, user, "unpaid") },
		},
		{
			name:        "a Stripe row in a pre-000178 status",
			wantMessage: "cannot roll back 000178 while a subscriptions row is Stripe-sourced",
			seed:        func(tx *sql.Tx) error { return migration000178InsertStripe(tx, user, "canceled") },
		},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			tx, err := db.Begin()
			require.NoError(t, err)
			defer migrationRollback(t, tx)
			require.NoError(t, tc.seed(tx))

			_, err = tx.Exec(down)
			migrationRequirePQError(t, err, "23001", "")
			assert.Contains(t, err.Error(), tc.wantMessage)
			// The failed statement poisons tx (25P02); the deferred rollback
			// discards the seed row and nothing of the down committed.
		})
	}

	t.Run("succeeds with only code rows in pre-000178 statuses", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer migrationRollback(t, tx)
		require.NoError(t, migration000178InsertCode(tx, user, "expired"))
		require.NoError(t, migration000178InsertCode(tx, user, "active"))

		_, err = tx.Exec(down)
		require.NoError(t, err)
		var rows int
		require.NoError(t, tx.QueryRow(`SELECT count(*) FROM subscriptions WHERE user_id = $1`, user).Scan(&rows))
		assert.Equal(t, 2, rows, "the down must neither delete nor rewrite code rows")
	})

	t.Run("checks for a Stripe row only after it holds the lock", func(t *testing.T) {
		migrationRequireGuardRunsUnderLock(t, db, down, "subscriptions",
			"cannot roll back 000178 while a subscriptions row is Stripe-sourced",
			func(tx *sql.Tx) error { return migration000178InsertStripe(tx, user, "active") })
	})

	t.Run("checks for a new-status row only after it holds the lock", func(t *testing.T) {
		// The case above committed a Stripe row, which the down's first check
		// would refuse on; clear it so only the second check can answer.
		_, err := db.Exec(`DELETE FROM subscriptions`)
		require.NoError(t, err)
		migrationRequireGuardRunsUnderLock(t, db, down, "subscriptions",
			"cannot roll back 000178 while a subscriptions row has status incomplete_expired, unpaid or paused",
			func(tx *sql.Tx) error { return migration000178InsertCode(tx, user, "paused") })
	})

	var planColumns int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = current_schema() AND table_name = 'subscriptions' AND column_name = 'plan'`,
	).Scan(&planColumns))
	assert.Equal(t, 1, planColumns, "a refused or rolled-back down must leave 000178 applied")
}

func TestMigration000178_Checks(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	// Each insert uses its own user, so the live-row unique index (000070's,
	// or 000179's per-source pair) can never be what accepts or rejects it.

	// Spec section 5 defines the identity CHECK as (source='stripe') =
	// (both ids NOT NULL): a non-Stripe row may carry one id, never both. These
	// accepts pin that a tightened CHECK (any id on a code row refused) is a
	// reviewed change, not a silent one.
	t.Run("accepts a non-Stripe row carrying exactly one Stripe id", func(t *testing.T) {
		_, err := db.Exec(`
			INSERT INTO subscriptions (user_id, tier, status, source, stripe_customer_id)
			VALUES ($1, 'premium', 'active', 'code', $2)`,
			testhelpers.CreateUser(t, db), "cus_test_"+uuid.NewString())
		require.NoError(t, err, "a code row with only a customer id")
		_, err = db.Exec(`
			INSERT INTO subscriptions (user_id, tier, status, source, stripe_subscription_id)
			VALUES ($1, 'premium', 'active', 'code', $2)`,
			testhelpers.CreateUser(t, db), "sub_test_"+uuid.NewString())
		require.NoError(t, err, "a code row with only a subscription id")
	})

	t.Run("accepts well-formed rows in every new status and column value", func(t *testing.T) {
		for _, status := range []string{"active", "incomplete_expired", "unpaid", "paused"} {
			require.NoError(t, migration000178InsertStripe(db, testhelpers.CreateUser(t, db), status), status)
		}

		codeUser := testhelpers.CreateUser(t, db)
		require.NoError(t, migration000178InsertCode(db, codeUser, "paused"))
		var foundersRate bool
		require.NoError(t, db.QueryRow(
			`SELECT founders_rate FROM subscriptions WHERE user_id = $1`, codeUser).Scan(&foundersRate))
		assert.False(t, foundersRate, "founders_rate must default to FALSE")

		for _, plan := range []string{"supersonic", "wing"} {
			for _, interval := range []string{"month", "year"} {
				_, err := db.Exec(`
					INSERT INTO subscriptions
					    (user_id, tier, status, source, stripe_customer_id, stripe_subscription_id,
					     current_period_end, plan, billing_interval, founders_rate, cancel_at)
					VALUES ($1, 'premium', 'active', 'stripe', $2, $3,
					        NOW() + INTERVAL '30 days', $4, $5, TRUE, NOW() + INTERVAL '30 days')`,
					testhelpers.CreateUser(t, db), "cus_test_"+uuid.NewString(), "sub_test_"+uuid.NewString(),
					plan, interval)
				require.NoError(t, err, "%s/%s", plan, interval)
			}
		}
	})

	for _, tc := range []struct{ name, constraint, query string }{
		{
			name:       "a Stripe row without Stripe ids",
			constraint: "subscriptions_stripe_identity_check",
			query: `INSERT INTO subscriptions (user_id, tier, status, source, current_period_end)
			        VALUES ($1, 'premium', 'active', 'stripe', NOW() + INTERVAL '30 days')`,
		},
		{
			name:       "a Stripe row with only a customer id",
			constraint: "subscriptions_stripe_identity_check",
			query: `INSERT INTO subscriptions (user_id, tier, status, source, stripe_customer_id, current_period_end)
			        VALUES ($1, 'premium', 'active', 'stripe', 'cus_test_identity_half', NOW() + INTERVAL '30 days')`,
		},
		{
			// Without this case a CHECK that dropped the customer-id clause,
			// (source='stripe') = (stripe_subscription_id IS NOT NULL), would
			// still reject every other identity case here.
			name:       "a Stripe row with only a subscription id",
			constraint: "subscriptions_stripe_identity_check",
			query: `INSERT INTO subscriptions (user_id, tier, status, source, stripe_subscription_id, current_period_end)
			        VALUES ($1, 'premium', 'active', 'stripe', 'sub_test_identity_half', NOW() + INTERVAL '30 days')`,
		},
		{
			name:       "a code row carrying both Stripe ids",
			constraint: "subscriptions_stripe_identity_check",
			query: `INSERT INTO subscriptions (user_id, tier, status, source, stripe_customer_id, stripe_subscription_id)
			        VALUES ($1, 'premium', 'active', 'code', 'cus_test_identity_code', 'sub_test_identity_code')`,
		},
		{
			name:       "a Stripe row with a NULL period end",
			constraint: "subscriptions_stripe_period_check",
			query: `INSERT INTO subscriptions (user_id, tier, status, source, stripe_customer_id, stripe_subscription_id)
			        VALUES ($1, 'premium', 'active', 'stripe', 'cus_test_period_null', 'sub_test_period_null')`,
		},
		{
			name:       "plan mach",
			constraint: "subscriptions_plan_check",
			query: `INSERT INTO subscriptions (user_id, tier, status, source, plan)
			        VALUES ($1, 'premium', 'active', 'code', 'mach')`,
		},
		{
			name:       "billing_interval week",
			constraint: "subscriptions_billing_interval_check",
			query: `INSERT INTO subscriptions (user_id, tier, status, source, billing_interval)
			        VALUES ($1, 'premium', 'active', 'code', 'week')`,
		},
		{
			// Already rejected by 000083's CHECK; this case pins that the
			// widened CHECK still refuses a status outside the Stripe set.
			name:       "a status outside the Stripe set",
			constraint: "subscriptions_status_check",
			query: `INSERT INTO subscriptions (user_id, tier, status, source)
			        VALUES ($1, 'premium', 'past_due_forever', 'code')`,
		},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := db.Exec(tc.query, testhelpers.CreateUser(t, db))
			migrationRequirePQError(t, err, "23514", tc.constraint)
		})
	}
}
