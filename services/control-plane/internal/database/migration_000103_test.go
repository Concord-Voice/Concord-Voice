package database_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestMigration000103_DownPreservesRotationDistributorClaim(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	down := migration000103DownSQL(t)

	t.Run("allows rollback without a claim", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })

		_, err = tx.ExecContext(ctx, down)
		require.NoError(t, err)
	})

	owner := ts.CreateTestUser(t, "migration103owner")
	distributor := ts.CreateTestUser(t, "migration103distributor")
	serverID := ts.CreateTestServer(t, owner.ID, "Migration 103 Server")
	channelID := ts.CreateTestChannel(t, serverID, "migration-103")
	_, err := ts.DB.ExecContext(ctx,
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by,
			rotation_distributor_id, rotation_distributor_claimed
		) VALUES ($1, 1, 2, 'member_removal', $2, $3, TRUE)`,
		channelID, owner.ID, distributor.ID,
	)
	require.NoError(t, err)

	t.Run("refuses rollback with a live distributor claim", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })

		_, err = tx.ExecContext(ctx, down)
		require.Error(t, err)
	})
}

func migration000103DownSQL(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "migrations", "000103_bind_rotation_distributor.down.sql")
	// #nosec G304 -- path is fixed relative to the package directory.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
