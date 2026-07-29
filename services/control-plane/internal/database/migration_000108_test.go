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

func TestMigration000108_RejectsUnissuedRotationEpochs(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	owner := ts.CreateTestUser(t, "migration108owner")
	target := ts.CreateTestUser(t, "migration108target")
	serverID := ts.CreateTestServer(t, owner.ID, "Migration 108 Server")
	channelID := ts.CreateTestChannel(t, serverID, "migration-108")
	_, err := ts.DB.ExecContext(ctx,
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, 'epoch-one', 1)`, channelID, owner.ID,
	)
	require.NoError(t, err)

	tx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, migration000108DownSQL(t))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, 'unissued-epoch', 2)`, channelID, target.ID,
	)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, migration000108UpSQL(t))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, 'second-unissued-epoch', 2)`, channelID, owner.ID,
	)
	require.Error(t, err)
	require.NoError(t, tx.Rollback())

	tx, err = ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, 'current-epoch', 1)`, channelID, target.ID,
	)
	require.NoError(t, err)
}

func migration000108UpSQL(t *testing.T) string {
	t.Helper()
	return migration000108SQL(t, "000108_reject_unissued_rotation_epochs.up.sql")
}

func migration000108DownSQL(t *testing.T) string {
	t.Helper()
	return migration000108SQL(t, "000108_reject_unissued_rotation_epochs.down.sql")
}

func migration000108SQL(t *testing.T, name string) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve migration test path")
	path := filepath.Join(filepath.Dir(currentFile), "..", "..", "migrations", name)
	// #nosec G304 -- path is fixed relative to this test-owned source file.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
