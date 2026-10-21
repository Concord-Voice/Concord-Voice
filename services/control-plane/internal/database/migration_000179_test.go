package database_test

import (
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000179 (epic #2033 PR-1, Refs #1305): 000070's one-live-row-per-user
// unique index becomes per source. A user may hold one live code row beside any
// number of live Stripe rows; a second live code row is still refused; and
// stripe_subscription_id becomes the Reconcile upsert key. Round trips and
// refusals run in rolled-back transactions, as in the 000178 tests.

const migration000179Refusal = "cannot restore idx_subscriptions_user_active while a user holds more than one live subscriptions row"

func migration000179UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000179_subscriptions_per_source_live_indexes.up.sql")
}

func migration000179DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000179_subscriptions_per_source_live_indexes.down.sql")
}

func TestMigration000179_UpDownUpRoundTrip(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)

	newIndexes := []string{
		"index idx_subscriptions_user_live_code ",
		"index idx_subscriptions_stripe_subscription_id ",
		"index idx_subscriptions_user_live_stripe ",
		"index idx_subscriptions_stripe_live_period_end ",
		"index idx_subscriptions_user_id ",
	}
	applied := migrationSchemaSnapshot(t, tx, "subscriptions")
	for _, prefix := range newIndexes {
		require.NotEmpty(t, migrationSnapshotLine(applied, prefix), "missing after up: %s", prefix)
	}
	require.Empty(t, migrationSnapshotLine(applied, "index idx_subscriptions_user_active "))
	assert.Contains(t, migrationSnapshotLine(applied, "index idx_subscriptions_user_live_code "), "CREATE UNIQUE INDEX")
	assert.Contains(t, migrationSnapshotLine(applied, "index idx_subscriptions_stripe_subscription_id "), "CREATE UNIQUE INDEX")
	assert.NotContains(t, migrationSnapshotLine(applied, "index idx_subscriptions_user_live_stripe "), "UNIQUE")
	assert.NotContains(t, migrationSnapshotLine(applied, "index idx_subscriptions_stripe_live_period_end "), "UNIQUE")

	// The full user_id index serves the users ON DELETE CASCADE and history
	// reads, which no partial index can: it must be non-unique (a user holds
	// many rows) and carry no predicate (it must cover every row).
	userIndex := migrationSnapshotLine(applied, "index idx_subscriptions_user_id ")
	assert.Contains(t, userIndex, "USING btree (user_id)")
	assert.NotContains(t, userIndex, "UNIQUE", "a user holds many subscriptions rows")
	assert.NotContains(t, userIndex, " WHERE ", "the cascade index must cover every row, so it carries no predicate")

	// Pin each live-set predicate's text. The two live-Stripe indexes are not
	// unique, so no insert can fail on them: only their definition shows a
	// dropped source filter or a widened status list, and the drift worker's
	// lapsed bucket relies on both. PostgreSQL deparses the predicate as
	// ((source)::text = 'stripe'::text) AND ((status)::text = ANY (ARRAY[...])).
	for _, ix := range []struct{ prefix, column, source string }{
		{"index idx_subscriptions_user_live_code ", "(user_id)", "'code'"},
		{"index idx_subscriptions_user_live_stripe ", "(user_id)", "'stripe'"},
		{"index idx_subscriptions_stripe_live_period_end ", "(current_period_end)", "'stripe'"},
	} {
		def := migrationSnapshotLine(applied, ix.prefix)
		assert.Contains(t, def, "USING btree "+ix.column+" WHERE ", ix.prefix)
		assert.Contains(t, def, "(source)::text = "+ix.source+"::text", ix.prefix)
		for _, live := range []string{"'active'", "'trialing'", "'past_due'"} {
			assert.Contains(t, def, live, "%s must include %s", ix.prefix, live)
		}
		for _, notLive := range []string{"'canceled'", "'incomplete'", "'expired'", "'incomplete_expired'", "'unpaid'", "'paused'"} {
			assert.NotContains(t, def, notLive, "%s must exclude %s", ix.prefix, notLive)
		}
	}

	_, err = tx.Exec(migration000179DownSQL(t))
	require.NoError(t, err, "the down must succeed while no user holds two live rows")

	reverted := migrationSchemaSnapshot(t, tx, "subscriptions")
	for _, prefix := range newIndexes {
		assert.Empty(t, migrationSnapshotLine(reverted, prefix), "left behind by the down: %s", prefix)
	}
	assert.Contains(t, migrationSnapshotLine(reverted, "index idx_subscriptions_user_active "), "CREATE UNIQUE INDEX",
		"the down must restore 000070's one-live-row-per-user index")

	_, err = tx.Exec(migration000179UpSQL(t))
	require.NoError(t, err, "the up must re-apply after its down")
	assert.Equal(t, applied, migrationSchemaSnapshot(t, tx, "subscriptions"),
		"down then up must rebuild exactly the indexes the migration chain built")
}

func TestMigration000179_DownRefusesUnderLock(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	down := migration000179DownSQL(t)

	for _, tc := range []struct {
		name string
		seed func(tx *sql.Tx, user uuid.UUID) error
	}{
		{
			name: "a live code row beside a live Stripe row",
			seed: func(tx *sql.Tx, user uuid.UUID) error {
				if err := migration000178InsertCode(tx, user, "active"); err != nil {
					return err
				}
				return migration000178InsertStripe(tx, user, "past_due")
			},
		},
		{
			name: "two live Stripe rows",
			seed: func(tx *sql.Tx, user uuid.UUID) error {
				if err := migration000178InsertStripe(tx, user, "active"); err != nil {
					return err
				}
				return migration000178InsertStripe(tx, user, "trialing")
			},
		},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			user := testhelpers.CreateUser(t, db)
			tx, err := db.Begin()
			require.NoError(t, err)
			defer migrationRollback(t, tx)
			require.NoError(t, tc.seed(tx, user))

			_, err = tx.Exec(down)
			migrationRequirePQError(t, err, "23001", "")
			assert.Contains(t, err.Error(), migration000179Refusal)
		})
	}

	t.Run("succeeds while every user has at most one live row", func(t *testing.T) {
		mixed := testhelpers.CreateUser(t, db)
		stripeOnly := testhelpers.CreateUser(t, db)
		tx, err := db.Begin()
		require.NoError(t, err)
		defer migrationRollback(t, tx)
		require.NoError(t, migration000178InsertCode(tx, mixed, "active"))
		// Every non-live status beside the one live row, including the three
		// 000178 added: a down whose live list grew to take in any of them
		// would refuse here.
		nonLive := []string{"canceled", "incomplete", "expired", "incomplete_expired", "unpaid", "paused"}
		for _, status := range nonLive {
			require.NoError(t, migration000178InsertStripe(tx, mixed, status), status)
		}
		require.NoError(t, migration000178InsertStripe(tx, stripeOnly, "active"))

		_, err = tx.Exec(down)
		require.NoError(t, err)
		var rows int
		require.NoError(t, tx.QueryRow(`SELECT count(*) FROM subscriptions`).Scan(&rows))
		assert.Equal(t, 2+len(nonLive), rows, "the down must not delete rows")
	})

	t.Run("checks for a second live row only after it holds the lock", func(t *testing.T) {
		user := testhelpers.CreateUser(t, db)
		require.NoError(t, migration000178InsertCode(db, user, "active"))
		migrationRequireGuardRunsUnderLock(t, db, down, "subscriptions", migration000179Refusal,
			func(tx *sql.Tx) error { return migration000178InsertStripe(tx, user, "active") })
	})

	var codeIndex int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM pg_indexes
		 WHERE schemaname = current_schema() AND indexname = 'idx_subscriptions_user_live_code'`,
	).Scan(&codeIndex))
	assert.Equal(t, 1, codeIndex, "a refused or rolled-back down must leave 000179 applied")
}

func TestMigration000179_LiveRowIndexes(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	t.Run("a live code row and live Stripe rows coexist for one user", func(t *testing.T) {
		user := testhelpers.CreateUser(t, db)
		require.NoError(t, migration000178InsertCode(db, user, "active"))
		require.NoError(t, migration000178InsertStripe(db, user, "active"))
		require.NoError(t, migration000178InsertStripe(db, user, "past_due"),
			"two live Stripe rows for one user are legal; Reconcile and the reader set handle them")
	})

	t.Run("a second live code row is refused", func(t *testing.T) {
		user := testhelpers.CreateUser(t, db)
		require.NoError(t, migration000178InsertCode(db, user, "active"))
		migrationRequirePQError(t, migration000178InsertCode(db, user, "trialing"),
			"23505", "idx_subscriptions_user_live_code")
	})

	t.Run("a code row outside the live set does not hold the code slot", func(t *testing.T) {
		// Every non-live status, including the three 000178 added (spec: they
		// entitle nobody). An index predicate that took in any of them would
		// let, say, a paused code row block the user's next redemption.
		user := testhelpers.CreateUser(t, db)
		for _, status := range []string{"canceled", "incomplete", "expired", "incomplete_expired", "unpaid", "paused"} {
			require.NoError(t, migration000178InsertCode(db, user, status), status)
		}
		require.NoError(t, migration000178InsertCode(db, user, "active"),
			"no non-live code row may hold idx_subscriptions_user_live_code")
	})

	t.Run("stripe_subscription_id is unique across rows and statuses", func(t *testing.T) {
		subID := "sub_test_" + uuid.NewString()
		insert := func(user uuid.UUID, status string) error {
			_, err := db.Exec(`
				INSERT INTO subscriptions
				    (user_id, tier, status, source, stripe_customer_id, stripe_subscription_id, current_period_end)
				VALUES ($1, 'premium', $2, 'stripe', $3, $4, NOW() + INTERVAL '30 days')`,
				user, status, "cus_test_"+uuid.NewString(), subID)
			return err
		}
		require.NoError(t, insert(testhelpers.CreateUser(t, db), "canceled"))
		migrationRequirePQError(t, insert(testhelpers.CreateUser(t, db), "active"),
			"23505", "idx_subscriptions_stripe_subscription_id")
	})
}
