package database_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000107_DefaultsFutureRotationClaimsUnclaimed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	owner := ts.CreateTestUser(t, "migration107owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Migration 107 Server")
	channelID := ts.CreateTestChannel(t, serverID, "migration-107")

	tx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `ALTER TABLE key_revocations ALTER COLUMN rotation_distributor_claimed DROP DEFAULT`)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, migration000107UpSQL(t))
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by)
		 VALUES ($1, 1, 2, 'member_removal', $2)`, channelID, owner.ID,
	)
	require.NoError(t, err)
	var defaultClaimed bool
	require.NoError(t, tx.QueryRowContext(ctx,
		`SELECT rotation_distributor_claimed FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 1`, channelID,
	).Scan(&defaultClaimed))
	assert.False(t, defaultClaimed)

	_, err = tx.ExecContext(ctx, migration000107DownSQL(t))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by)
		 VALUES ($1, 2, 3, 'member_removal', $2)`, channelID, owner.ID,
	)
	require.NoError(t, err)
	var revertedClaimed bool
	require.NoError(t, tx.QueryRowContext(ctx,
		`SELECT rotation_distributor_claimed FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 2`, channelID,
	).Scan(&revertedClaimed))
	assert.False(t, revertedClaimed)
}

func migration000107UpSQL(t *testing.T) string {
	t.Helper()
	return migration000107SQL(t, "000107_recover_unwritten_rotation_claims.up.sql")
}

func migration000107DownSQL(t *testing.T) string {
	t.Helper()
	return migration000107SQL(t, "000107_recover_unwritten_rotation_claims.down.sql")
}

func migration000107SQL(t *testing.T, name string) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve migration test path")
	path := filepath.Join(filepath.Dir(currentFile), "..", "..", "migrations", name)
	// #nosec G304 -- path is fixed relative to this test-owned source file.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
