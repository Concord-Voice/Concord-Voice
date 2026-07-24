package channels

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// #2420 recipientKeyFresh unit coverage (unexported → package-internal test).
// The FOR SHARE lock in recipientKeyFresh is exercised for serialization in the
// HTTP-level tests; here we cover the version-compare and missing-row branches.
func TestRecipientKeyFresh(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	insertPK := func(userID string, version int) {
		_, err := db.Exec(
			`INSERT INTO public_keys (user_id, public_key, key_version) VALUES ($1, $2, $3)`,
			userID, []byte("test-public-key"), version)
		require.NoError(t, err)
	}

	t.Run("matching version is fresh", func(t *testing.T) {
		u := dbtest.CreateUser(t, db).String()
		insertPK(u, 5)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		fresh, err := recipientKeyFresh(ctx, tx, u, 5)
		require.NoError(t, err)
		assert.True(t, fresh, "current version 5 == wrapped 5 is fresh")
	})

	t.Run("rotated version is stale", func(t *testing.T) {
		u := dbtest.CreateUser(t, db).String()
		insertPK(u, 6) // recipient rotated past the version the CSK was wrapped against
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		fresh, err := recipientKeyFresh(ctx, tx, u, 5)
		require.NoError(t, err)
		assert.False(t, fresh, "current version 6 != wrapped 5 is stale")
	})

	t.Run("missing public key is not fresh", func(t *testing.T) {
		u := dbtest.CreateUser(t, db).String() // no public_keys row
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		fresh, err := recipientKeyFresh(ctx, tx, u, 5)
		require.NoError(t, err)
		assert.False(t, fresh, "no key to wrap to => not fresh (fail-closed)")
	})
}
