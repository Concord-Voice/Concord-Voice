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

const migration106Fingerprint = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func TestMigration000106_BindsRotationCSKAndInitialCreator(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "migration106owner")
	distributor := ts.CreateTestUser(t, "migration106distributor")
	otherHolder := ts.CreateTestUser(t, "migration106otherholder")
	target := ts.CreateTestUser(t, "migration106target")
	serverID := ts.CreateTestServer(t, owner.ID, "Migration 106 Server")
	channelID := ts.CreateTestChannel(t, serverID, "migration-106")
	_, err := ts.DB.ExecContext(ctx,
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, 'epoch-one', 1)`, channelID, owner.ID,
	)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by
		) VALUES ($1, 1, 2, 'member_removal', $2)`,
		channelID, owner.ID,
	)
	require.NoError(t, err)
	var defaultClaimed bool
	require.NoError(t, ts.DB.QueryRowContext(ctx,
		`SELECT rotation_distributor_claimed FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 1`, channelID,
	).Scan(&defaultClaimed))
	require.False(t, defaultClaimed)

	t.Run("fences an old handler from an active initial distribution", func(t *testing.T) {
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO channel_initial_key_distributions (channel_id, creator_id, key_version)
			 VALUES ($1, $2, 2)`, channelID, owner.ID,
		)
		require.NoError(t, err)
		_, err = ts.DB.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'old-handler', 2)`, channelID, target.ID,
		)
		require.Error(t, err)

		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `SELECT set_config('concord.rotation_distributor_id', $1, TRUE)`, owner.ID)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'creator-without-fingerprint', 2)`, channelID, target.ID,
		)
		require.Error(t, err)
		require.NoError(t, tx.Rollback())

		tx, err = ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`UPDATE key_revocations
			 SET rotation_distributor_id = $2, rotation_distributor_claimed = TRUE,
			     rotation_key_fingerprint = $3
			 WHERE channel_id = $1 AND revoked_epoch = 1`,
			channelID, owner.ID, migration106Fingerprint,
		)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `SELECT set_config('concord.rotation_distributor_id', $1, TRUE)`, owner.ID)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`, migration106Fingerprint)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'creator-handler', 2)`, channelID, target.ID,
		)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
	})

	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_claimed
		) VALUES ($1, 2, 3, 'member_removal', $2, FALSE)`,
		channelID, owner.ID,
	)
	require.NoError(t, err)

	t.Run("requires the established CSK fingerprint", func(t *testing.T) {
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'old-handler', 3)`, channelID, target.ID,
		)
		require.Error(t, err)

		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`UPDATE key_revocations
			 SET rotation_distributor_id = $2, rotation_distributor_claimed = TRUE,
			     rotation_key_fingerprint = $3
			 WHERE channel_id = $1 AND revoked_epoch = 2`,
			channelID, distributor.ID, migration106Fingerprint,
		)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`, migration106Fingerprint)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'established-key', 3)`, channelID, target.ID,
		)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
	})

	t.Run("allows another holder to rewrap only that CSK", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`, "//////////////////////////////////////////8=")
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'different-key', 3)`, channelID, otherHolder.ID,
		)
		require.Error(t, err)
		_ = tx.Rollback()

		tx, err = ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`, migration106Fingerprint)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'same-established-key', 3)`, channelID, otherHolder.ID,
		)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
	})

	t.Run("refuses to discard an established fingerprint", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, migration000106DownSQL(t))
		require.Error(t, err)
	})

	t.Run("restores the 000105 guard before dropping an unused fingerprint column", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx,
			`UPDATE key_revocations
			 SET rotation_key_fingerprint = NULL
			 WHERE channel_id = $1`, channelID,
		)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, migration000106DownSQL(t))
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO key_revocations (
				channel_id, revoked_epoch, successor_epoch, reason, revoked_by
			) VALUES ($1, 3, 4, 'member_removal', $2)`,
			channelID, owner.ID,
		)
		require.NoError(t, err)
		var revertedClaimed *bool
		require.NoError(t, tx.QueryRowContext(ctx,
			`SELECT rotation_distributor_claimed FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 3`, channelID,
		).Scan(&revertedClaimed))
		require.Nil(t, revertedClaimed)
		_, err = tx.ExecContext(ctx,
			`UPDATE key_revocations
			 SET rotation_distributor_id = $2, rotation_distributor_claimed = TRUE
			 WHERE channel_id = $1 AND revoked_epoch = 3`, channelID, distributor.ID,
		)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `SELECT set_config('concord.rotation_distributor_id', $1, TRUE)`, distributor.ID)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, '000105-guard', 4)`, channelID, owner.ID,
		)
		require.NoError(t, err)
	})
}

func migration000106DownSQL(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve migration test path")
	path := filepath.Join(filepath.Dir(currentFile), "..", "..", "migrations", "000106_bind_rotation_key_fingerprint.down.sql")
	// #nosec G304 -- path is fixed relative to this test-owned source file.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
