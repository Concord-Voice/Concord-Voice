package database_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000132_ServerVoiceObservedLeaseContract(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	down := migrationReadFile(t, "../../migrations/000132_add_voice_lifecycle_observed_at.down.sql")
	up := migrationReadFile(t, "../../migrations/000132_add_voice_lifecycle_observed_at.up.sql")
	owner := ts.CreateTestUser(t, "migration-132-owner")
	inserted := ts.CreateTestUser(t, "migration-132-insert")
	server := ts.CreateTestServer(t, owner.ID, "migration-132-server")
	channel := ts.CreateVoiceChannel(t, server, "migration-132-channel")

	tx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback migration 000132 transaction: %v", rollbackErr)
		}
	})
	_, err = tx.ExecContext(ctx, down)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at) VALUES ($1, $2, $3)`, channel, owner.ID, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, up)
	require.NoError(t, err)

	var observed, transactionNow time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT lifecycle_observed_at FROM voice_participants WHERE channel_id=$1 AND user_id=$2`, channel, owner.ID).Scan(&observed))
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT CURRENT_TIMESTAMP`).Scan(&transactionNow))
	assert.False(t, observed.IsZero(), "pre-existing rows receive a grace value")
	assert.Equal(t, transactionNow, observed, "pre-existing rows receive transaction-time grace")

	var typ, def, comment, nullable string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT c.data_type, c.column_default, col_description('voice_participants'::regclass, c.ordinal_position) FROM information_schema.columns c WHERE c.table_schema='public' AND c.table_name='voice_participants' AND c.column_name='lifecycle_observed_at'`).Scan(&typ, &def, &comment))
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_schema='public' AND table_name='voice_participants' AND column_name='lifecycle_observed_at'`).Scan(&nullable))
	assert.Equal(t, "timestamp with time zone", typ)
	assert.Equal(t, "NO", nullable)
	assert.Contains(t, def, "CURRENT_TIMESTAMP")
	assert.Contains(t, comment, "PostgreSQL observation time")
	var indexColumnList string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT string_agg(attribute.attname, ',' ORDER BY key.ordinality) FROM pg_index AS index JOIN pg_class AS relation ON relation.oid = index.indrelid JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace JOIN pg_class AS index_relation ON index_relation.oid = index.indexrelid CROSS JOIN LATERAL unnest(index.indkey) WITH ORDINALITY AS key(attnum, ordinality) JOIN pg_attribute AS attribute ON attribute.attrelid = relation.oid AND attribute.attnum = key.attnum WHERE namespace.nspname = 'public' AND relation.relname = 'voice_participants' AND index_relation.relname = 'idx_voice_participants_lifecycle_observed_at'`).Scan(&indexColumnList))
	assert.Equal(t, []string{"lifecycle_observed_at", "channel_id", "user_id"}, strings.Split(indexColumnList, ","))

	callerTime := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	before := txClock(t, tx)
	_, err = tx.ExecContext(ctx, `INSERT INTO voice_participants (channel_id,user_id,lifecycle_event_at,lifecycle_observed_at) VALUES ($1,$2,$3,$4)`, channel, inserted.ID, callerTime, callerTime)
	require.NoError(t, err)
	after := txClock(t, tx)
	var insertedObserved time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT lifecycle_observed_at FROM voice_participants WHERE user_id=$1`, inserted.ID).Scan(&insertedObserved))
	assertDBClockBounded(t, insertedObserved, before, after)

	changedSentinel := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	_, err = tx.ExecContext(ctx, `UPDATE voice_participants SET lifecycle_observed_at=$1 WHERE user_id=$2`, changedSentinel, owner.ID)
	require.NoError(t, err)
	changedBefore := txClock(t, tx)
	_, err = tx.ExecContext(ctx, `UPDATE voice_participants SET lifecycle_event_at=clock_timestamp() + interval '1 second' WHERE user_id=$1`, owner.ID)
	require.NoError(t, err)
	changedAfter := txClock(t, tx)
	var changed time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT lifecycle_observed_at FROM voice_participants WHERE user_id=$1`, owner.ID).Scan(&changed))
	assertDBClockBounded(t, changed, changedBefore, changedAfter)

	sameValueSentinel := time.Date(2002, 3, 4, 5, 6, 7, 0, time.UTC)
	_, err = tx.ExecContext(ctx, `UPDATE voice_participants SET lifecycle_observed_at=$1 WHERE user_id=$2`, sameValueSentinel, owner.ID)
	require.NoError(t, err)
	sameValueBefore := txClock(t, tx)
	_, err = tx.ExecContext(ctx, `UPDATE voice_participants SET lifecycle_event_at=lifecycle_event_at WHERE user_id=$1`, owner.ID)
	require.NoError(t, err)
	sameValueAfter := txClock(t, tx)
	var sameValue time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT lifecycle_observed_at FROM voice_participants WHERE user_id=$1`, owner.ID).Scan(&sameValue))
	assertDBClockBounded(t, sameValue, sameValueBefore, sameValueAfter)

	unrelatedSentinel := time.Date(2003, 4, 5, 6, 7, 8, 0, time.UTC)
	_, err = tx.ExecContext(ctx, `UPDATE voice_participants SET lifecycle_observed_at=$1 WHERE user_id=$2`, unrelatedSentinel, owner.ID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE voice_participants SET is_muted=NOT is_muted WHERE user_id=$1`, owner.ID)
	require.NoError(t, err)
	var unrelated time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT lifecycle_observed_at FROM voice_participants WHERE user_id=$1`, owner.ID).Scan(&unrelated))
	assert.Equal(t, unrelatedSentinel, unrelated)

	_, err = tx.ExecContext(ctx, down)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, up)
	require.NoError(t, err, "migration must replay after down")
}

func assertDBClockBounded(t *testing.T, got, before, after time.Time) {
	t.Helper()
	assert.False(t, got.Before(before), "trigger renewal predates its database-clock lower bound")
	assert.False(t, got.After(after), "trigger renewal exceeds its database-clock upper bound")
}

func txClock(t *testing.T, tx *sql.Tx) time.Time {
	t.Helper()
	var now time.Time
	require.NoError(t, tx.QueryRowContext(context.Background(), `SELECT clock_timestamp()`).Scan(&now))
	return now
}
