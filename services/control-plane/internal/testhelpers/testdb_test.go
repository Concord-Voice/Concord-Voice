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

	// Insert a row.
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES (gen_random_uuid(), 'trunctest@test.concord.chat', 'trunctest',
		'$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY',
		true, true)`)
	require.NoError(t, err)

	var before int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&before))
	assert.Equal(t, 1, before)

	// Truncate and verify.
	require.NoError(t, TruncateAllTables(db))

	var after int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&after))
	assert.Equal(t, 0, after)
}

func TestSetupTestDBCleanupTruncates(t *testing.T) {
	db, cleanup := SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() }) // safety net if test exits early

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
