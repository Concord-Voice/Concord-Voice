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

func TestMigration000102_DownPreservesIncompleteDistributionFence(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	down := migration000102DownSQL(t)

	t.Run("allows rollback without an incomplete distribution", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })

		_, err = tx.ExecContext(ctx, down)
		require.NoError(t, err)
	})

	owner := ts.CreateTestUser(t, "migration102owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Migration 102 Server")
	channelID := ts.CreateTestChannel(t, serverID, "migration-102")
	_, err := ts.DB.ExecContext(ctx,
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, channelID, owner.ID)
	require.NoError(t, err)

	t.Run("refuses rollback with an incomplete distribution", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })

		_, err = tx.ExecContext(ctx, down)
		require.Error(t, err)
	})
}

func migration000102DownSQL(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve migration test path")
	path := filepath.Join(filepath.Dir(currentFile), "..", "..", "migrations", "000102_add_channel_initial_key_distributions.down.sql")
	// #nosec G304 -- path is fixed relative to this test-owned source file.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
