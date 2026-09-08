package database_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000133_PreservesObservedLeaseAcrossLifecycleReplays(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	down := migrationReadFile(t, "../../migrations/000133_preserve_voice_lifecycle_replay_lease.down.sql")
	up := migrationReadFile(t, "../../migrations/000133_preserve_voice_lifecycle_replay_lease.up.sql")
	owner := ts.CreateTestUser(t, "migration-133-owner")
	inserted := ts.CreateTestUser(t, "migration-133-insert")
	server := ts.CreateTestServer(t, owner.ID, "migration-133-server")
	channel := ts.CreateVoiceChannel(t, server, "migration-133-channel")

	tx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback migration 000133 transaction: %v", rollbackErr)
		}
	})
	_, err = tx.ExecContext(ctx, down)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at) VALUES ($1, $2, $3)`, channel, owner.ID, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	require.NoError(t, err)

	oldSentinel := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	_, err = tx.ExecContext(ctx, `UPDATE voice_participants SET lifecycle_observed_at=$1 WHERE user_id=$2`, oldSentinel, owner.ID)
	require.NoError(t, err)
	oldBefore := txClock(t, tx)
	_, err = tx.ExecContext(ctx, `UPDATE voice_participants SET lifecycle_event_at=lifecycle_event_at WHERE user_id=$1`, owner.ID)
	require.NoError(t, err)
	oldAfter := txClock(t, tx)
	var oldObserved time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT lifecycle_observed_at FROM voice_participants WHERE user_id=$1`, owner.ID).Scan(&oldObserved))
	assertDBClockBounded(t, oldObserved, oldBefore, oldAfter)

	_, err = tx.ExecContext(ctx, up)
	require.NoError(t, err)
	insertBefore := txClock(t, tx)
	_, err = tx.ExecContext(ctx, `INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at, lifecycle_observed_at) VALUES ($1, $2, $3, $4)`, channel, inserted.ID, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), oldSentinel)
	require.NoError(t, err)
	var insertedObserved time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT lifecycle_observed_at FROM voice_participants WHERE user_id=$1`, inserted.ID).Scan(&insertedObserved))
	insertAfter := txClock(t, tx)
	assertDBClockBounded(t, insertedObserved, insertBefore, insertAfter)

	equalSentinel := time.Date(2002, 3, 4, 5, 6, 7, 0, time.UTC)
	_, err = tx.ExecContext(ctx, `UPDATE voice_participants SET lifecycle_observed_at=$1 WHERE user_id=$2`, equalSentinel, owner.ID)
	require.NoError(t, err)
	spoofedObserved := time.Date(2004, 5, 6, 7, 8, 9, 0, time.UTC)
	equalBefore := txClock(t, tx)
	_, err = tx.ExecContext(ctx, `UPDATE voice_participants SET lifecycle_event_at=lifecycle_event_at, lifecycle_observed_at=$1 WHERE user_id=$2`, spoofedObserved, owner.ID)
	require.NoError(t, err)
	equalAfter := txClock(t, tx)
	var equalObserved time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT lifecycle_observed_at FROM voice_participants WHERE user_id=$1`, owner.ID).Scan(&equalObserved))
	assert.Equal(t, equalSentinel, equalObserved)
	assert.True(t, equalObserved.Before(equalBefore) && equalObserved.Before(equalAfter))

	distinctBefore := txClock(t, tx)
	_, err = tx.ExecContext(ctx, `UPDATE voice_participants SET lifecycle_event_at=clock_timestamp() + interval '1 second' WHERE user_id=$1`, owner.ID)
	require.NoError(t, err)
	distinctAfter := txClock(t, tx)
	var distinctObserved time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT lifecycle_observed_at FROM voice_participants WHERE user_id=$1`, owner.ID).Scan(&distinctObserved))
	assertDBClockBounded(t, distinctObserved, distinctBefore, distinctAfter)

	_, err = tx.ExecContext(ctx, down)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, up)
	require.NoError(t, err, "migration must replay after down")
}
