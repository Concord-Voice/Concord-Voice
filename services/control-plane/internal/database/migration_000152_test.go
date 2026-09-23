package database_test

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000152_RefreshesExistingVoiceParticipantLease(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	up := migrationReadFile(t, "../../migrations/000152_extend_voice_lifecycle_rollout_grace.up.sql")
	owner := ts.CreateTestUser(t, "migration-136-owner")
	future := ts.CreateTestUser(t, "migration-136-future")
	server := ts.CreateTestServer(t, owner.ID, "migration-136-server")
	channel := ts.CreateVoiceChannel(t, server, "migration-136-channel")

	_, err := ts.DB.ExecContext(ctx, `
		INSERT INTO voice_participants (channel_id, user_id)
		VALUES ($1, $2), ($1, $3)`, channel, owner.ID, future.ID)
	require.NoError(t, err)
	futureObserved := time.Date(2040, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err = ts.DB.ExecContext(ctx, `
		UPDATE voice_participants SET lifecycle_observed_at = $1
		WHERE channel_id = $2 AND user_id = $3`, futureObserved, channel, future.ID)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, `
		UPDATE voice_participants SET lifecycle_observed_at = clock_timestamp() - INTERVAL '1 hour'
		WHERE channel_id = $1 AND user_id = $2`, channel, owner.ID)
	require.NoError(t, err)
	var before time.Time
	require.NoError(t, ts.DB.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&before))
	_, err = ts.DB.ExecContext(ctx, up)
	require.NoError(t, err)
	var after time.Time
	require.NoError(t, ts.DB.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&after))

	var observed time.Time
	require.NoError(t, ts.DB.QueryRowContext(ctx, `
		SELECT lifecycle_observed_at FROM voice_participants
		WHERE channel_id = $1 AND user_id = $2`, channel, owner.ID).Scan(&observed))
	assert.False(t, observed.Before(before))
	assert.False(t, observed.After(after))

	var preserved time.Time
	require.NoError(t, ts.DB.QueryRowContext(ctx, `
		SELECT lifecycle_observed_at FROM voice_participants
		WHERE channel_id = $1 AND user_id = $2`, channel, future.ID).Scan(&preserved))
	assert.Equal(t, futureObserved, preserved, "the rollout backfill must not shorten an existing future lease")
}
