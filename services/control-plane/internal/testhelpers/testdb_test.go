package testhelpers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetupTestDBReturnsFunctionalDB(t *testing.T) {
	db, cleanup := SetupTestDB(t)
	defer cleanup()

	// Verify the connection is alive and migrations have run.
	var tableExists bool
	err := db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_name = 'users'
	)`).Scan(&tableExists)
	require.NoError(t, err)
	assert.True(t, tableExists, "users table should exist after migrations")
}

func TestSetupTestDBMultipleCallsWork(t *testing.T) {
	// Two sequential calls should both return working databases.
	db1, cleanup1 := SetupTestDB(t)
	defer cleanup1()

	db2, cleanup2 := SetupTestDB(t)
	defer cleanup2()

	// Both connections should be functional.
	require.NoError(t, db1.Ping())
	require.NoError(t, db2.Ping())

	// Both should see the migrated schema.
	var count int
	require.NoError(t, db1.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&count))
	assert.Greater(t, count, 0)

	require.NoError(t, db2.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&count))
	assert.Greater(t, count, 0)
}

func TestTruncateAllTablesClearsData(t *testing.T) {
	db, cleanup := SetupTestDB(t)
	defer cleanup()

	// Insert one row per table. tier1_erasure_delete_obligations,
	// stripe_webhook_events and billing_erasure_outbox have no foreign key, so
	// only their explicit entries in truncateAllTablesOnce clear them;
	// stripe_customers is not listed and is cleared through CASCADE from users.
	// The FK-less billing rows use unique keys and an at-least-one count: a red
	// run from before a table was listed leaves its row behind in the scratch
	// database, and the next reset must clear that row too.
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES (gen_random_uuid(), 'trunctest@test.concord.chat', 'trunctest',
		'$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY',
		true, true)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key)
		VALUES ('avatars/trunctest')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO stripe_customers (user_id, stripe_customer_id)
		SELECT id, 'cus_test_trunctest' FROM users WHERE username = 'trunctest'`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO stripe_webhook_events (event_id, event_type)
		VALUES ('evt_trunctest_' || gen_random_uuid(), 'invoice.paid')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO billing_erasure_outbox (stripe_customer_id)
		VALUES ('cus_test_trunctest_' || gen_random_uuid())`)
	require.NoError(t, err)

	var before int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&before))
	assert.Equal(t, 1, before)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tier1_erasure_delete_obligations`).Scan(&before))
	assert.Equal(t, 1, before)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM stripe_customers`).Scan(&before))
	assert.Equal(t, 1, before)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM stripe_webhook_events`).Scan(&before))
	assert.GreaterOrEqual(t, before, 1)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM billing_erasure_outbox`).Scan(&before))
	assert.GreaterOrEqual(t, before, 1)

	// Truncate and verify.
	require.NoError(t, TruncateAllTables(db))

	var after int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&after))
	assert.Equal(t, 0, after)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tier1_erasure_delete_obligations`).Scan(&after))
	assert.Equal(t, 0, after)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM stripe_customers`).Scan(&after))
	assert.Equal(t, 0, after)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM stripe_webhook_events`).Scan(&after))
	assert.Equal(t, 0, after)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM billing_erasure_outbox`).Scan(&after))
	assert.Equal(t, 0, after)
}

func TestSetupTestDBCleanupTruncates(t *testing.T) {
	db, cleanup := SetupTestDB(t)

	// Insert a row, then run the cleanup.
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES (gen_random_uuid(), 'cleanuptest@test.concord.chat', 'cleanuptest',
		'$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY',
		true, true)`)
	require.NoError(t, err)

	cleanup()

	// Open a fresh connection to verify truncation happened.
	db2, cleanup2 := SetupTestDB(t)
	defer cleanup2()

	var count int
	require.NoError(t, db2.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count))
	assert.Equal(t, 0, count)
}
