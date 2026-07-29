package database_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const migration109Fingerprint = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func requireMigration109RevokedEpochError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr))
	assert.Equal(t, "CV001", string(pqErr.Code))
}

// Regression for #2534.
func TestMigration000109_RejectsAlreadyRevokedRotationEpochs(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	owner := ts.CreateTestUser(t, "migration109owner")
	epochOneTarget := ts.CreateTestUser(t, "migration109epochone")
	epochTwoTarget := ts.CreateTestUser(t, "migration109epochtwo")
	activeTarget := ts.CreateTestUser(t, "migration109active")
	serverID := ts.CreateTestServer(t, owner.ID, "Migration 109 Server")
	channelID := ts.CreateTestChannel(t, serverID, "migration-109")

	_, err := ts.DB.ExecContext(ctx,
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by,
			rotation_distributor_id, rotation_distributor_claimed, rotation_key_fingerprint
		) VALUES
			($1, 1, 2, 'member_removal', $2, $2, TRUE, $3),
			($1, 2, 3, 'member_removal', $2, $2, TRUE, $3)`,
		channelID, owner.ID, migration109Fingerprint,
	)
	require.NoError(t, err)

	t.Run("allows active successor epoch", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`,
			migration109Fingerprint,
		)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'active-epoch', 3)`, channelID, activeTarget.ID,
		)
		assert.NoError(t, err)
		require.NoError(t, tx.Commit())
	})

	t.Run("rejects revoked base epoch", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'revoked-epoch-one', 1)`, channelID, epochOneTarget.ID,
		)
		requireMigration109RevokedEpochError(t, err)
		require.NoError(t, tx.Rollback())
	})

	t.Run("rejects revoked successor epoch", func(t *testing.T) {
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`,
			migration109Fingerprint,
		)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'revoked-epoch-two', 2)`, channelID, epochTwoTarget.ID,
		)
		requireMigration109RevokedEpochError(t, err)
		require.NoError(t, tx.Rollback())
	})
}

func TestMigration000109_SerializesConcurrentRevocationAndDistribution(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	owner := ts.CreateTestUser(t, "migration109raceowner")
	target := ts.CreateTestUser(t, "migration109racetarget")
	serverID := ts.CreateTestServer(t, owner.ID, "Migration 109 Race Server")
	channelID := ts.CreateTestChannel(t, serverID, "migration-109-race")

	revocationTx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = revocationTx.Rollback() })
	_, err = revocationTx.ExecContext(ctx,
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by,
			rotation_distributor_id, rotation_distributor_claimed, rotation_key_fingerprint
		) VALUES ($1, 1, 2, 'member_removal', $2, $2, TRUE, $3)`,
		channelID, owner.ID, migration109Fingerprint,
	)
	require.NoError(t, err)
	distributionConn, err := ts.DB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		_ = distributionConn.Close()
	})
	var distributionPID int
	require.NoError(t, distributionConn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&distributionPID))

	distributionDone := make(chan error, 1)
	go func() {
		_, insertErr := distributionConn.ExecContext(ctx,
			`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
			 VALUES ($1, $2, 'concurrent-revoked-epoch', 1)`, channelID, target.ID,
		)
		distributionDone <- insertErr
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		err := ts.DB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = $1 AND NOT granted)`, distributionPID,
		).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond, "distribution backend must wait on the channel lock")

	require.NoError(t, revocationTx.Commit())

	select {
	case insertErr := <-distributionDone:
		requireMigration109RevokedEpochError(t, insertErr)
	case <-time.After(5 * time.Second):
		t.Fatal("distribution remained blocked after the concurrent revocation committed")
	}

	var inserted int
	require.NoError(t, ts.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM channel_keys
		 WHERE channel_id = $1 AND user_id = $2 AND key_version = 1`, channelID, target.ID,
	).Scan(&inserted))
	assert.Zero(t, inserted)
}
