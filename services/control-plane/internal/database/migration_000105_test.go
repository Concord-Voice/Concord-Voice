package database_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestMigration000105_EnforcesRotationDistributorWriter(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	down := migration000105DownSQL(t)

	owner := ts.CreateTestUser(t, "migration105owner")
	distributor := ts.CreateTestUser(t, "migration105distributor")
	target := ts.CreateTestUser(t, "migration105target")
	serverID := ts.CreateTestServer(t, owner.ID, "Migration 105 Server")
	channelID := ts.CreateTestChannel(t, serverID, "migration-105")
	_, err := ts.DB.ExecContext(ctx,
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, 'epoch-one', 1)`, channelID, owner.ID,
	)
	require.NoError(t, err)

	t.Run("restores the sealed state before removing the writer guard", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })

		_, err = tx.ExecContext(ctx, down)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO key_revocations (
				channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_claimed
			) VALUES ($1, 1, 2, 'member_removal', $2, FALSE)`,
			channelID, owner.ID,
		)
		require.Error(t, err)
	})

	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_claimed
		) VALUES ($1, 1, 2, 'member_removal', $2, FALSE)`,
		channelID, owner.ID,
	)
	require.NoError(t, err)

	t.Run("rejects an unclaimed or old-handler write", func(t *testing.T) {
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'epoch-two', 2)`, channelID, target.ID,
		)
		require.Error(t, err)
	})

	t.Run("allows the claimed current handler", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		_, err = tx.ExecContext(ctx,
			`UPDATE key_revocations
			 SET rotation_distributor_id = $2, rotation_distributor_claimed = TRUE,
			     rotation_key_fingerprint = $3
			 WHERE channel_id = $1 AND revoked_epoch = 1`, channelID, distributor.ID,
			migration106Fingerprint,
		)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`, migration106Fingerprint)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'epoch-two', 2)`, channelID, target.ID,
		)
		require.NoError(t, err)
	})

	t.Run("keeps creator-only initial distribution working", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		_, err = tx.ExecContext(ctx,
			`UPDATE key_revocations
			 SET rotation_distributor_id = $2, rotation_distributor_claimed = TRUE,
			     rotation_key_fingerprint = $3
			 WHERE channel_id = $1 AND revoked_epoch = 1`, channelID, owner.ID, migration106Fingerprint,
		)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO channel_initial_key_distributions (channel_id, creator_id, key_version)
			 VALUES ($1, $2, 2)`, channelID, owner.ID,
		)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `SELECT set_config('concord.rotation_distributor_id', $1, TRUE)`, owner.ID)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`, migration106Fingerprint)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'epoch-two', 2)`, channelID, target.ID,
		)
		require.NoError(t, err)
	})

}

func migration000105DownSQL(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve migration test path")
	path := filepath.Join(filepath.Dir(currentFile), "..", "..", "migrations", "000105_enforce_rotation_distributor_writer.down.sql")
	// #nosec G304 -- path is fixed relative to this test-owned source file.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
