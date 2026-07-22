package database_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000093_PhasedUpgradeFromBase92(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	ups := []string{
		migration000093ReadFile(t, "000093_voice_lifecycle_watermarks.up.sql"),
		migration000093ReadFile(t, "000094_backfill_voice_lifecycle_watermarks.up.sql"),
		migration000093ReadFile(t, "000095_guard_voice_lifecycle_watermarks_not_null.up.sql"),
		migration000093ReadFile(t, "000096_validate_voice_lifecycle_watermarks_not_null.up.sql"),
		migration000093ReadFile(t, "000097_enforce_voice_lifecycle_watermarks_not_null.up.sql"),
		migration000093ReadFile(t, "000098_add_activity_settings_cleanup_marker.up.sql"),
	}
	downs := []string{
		migration000093ReadFile(t, "000093_voice_lifecycle_watermarks.down.sql"),
		migration000093ReadFile(t, "000094_backfill_voice_lifecycle_watermarks.down.sql"),
		migration000093ReadFile(t, "000095_guard_voice_lifecycle_watermarks_not_null.down.sql"),
		migration000093ReadFile(t, "000096_validate_voice_lifecycle_watermarks_not_null.down.sql"),
		migration000093ReadFile(t, "000097_enforce_voice_lifecycle_watermarks_not_null.down.sql"),
		migration000093ReadFile(t, "000098_add_activity_settings_cleanup_marker.down.sql"),
	}

	phase := 6
	t.Cleanup(func() {
		for phase < len(ups) {
			if _, err := ts.DB.ExecContext(context.Background(), ups[phase]); !assert.NoError(
				t, err, "restore migration phase %d", phase+1,
			) {
				return
			}
			phase++
		}
	})

	_, err := ts.DB.ExecContext(ctx, `DELETE FROM activity_settings_pending_cleanups`)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, downs[5])
	require.NoError(t, err)
	phase = 5
	_, err = ts.DB.ExecContext(ctx, `DELETE FROM dm_voice_participants`)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, `DELETE FROM voice_participants`)
	require.NoError(t, err)
	for downPhase := 4; downPhase >= 0; downPhase-- {
		_, err = ts.DB.ExecContext(ctx, downs[downPhase])
		require.NoErrorf(t, err, "roll back migration phase %d", downPhase+1)
		phase = downPhase
	}
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseColumnMissing(t, ts.DB, table)
	}

	owner := ts.CreateTestUser(t, "voice_lifecycle_phase_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Voice lifecycle phase migration")
	channelID := ts.CreateTestChannel(t, serverID, "voice-lifecycle-phase")
	conversationID := uuid.NewString()
	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`,
		conversationID, owner.ID)
	require.NoError(t, err)
	serverJoinedAt := time.Date(2026, 7, 1, 2, 3, 4, 0, time.UTC)
	privateJoinedAt := time.Date(2026, 7, 2, 3, 4, 5, 0, time.UTC)
	_, err = ts.DB.ExecContext(ctx, `
		INSERT INTO voice_participants (channel_id, user_id, joined_at)
		VALUES ($1, $2, $3)
	`, channelID, owner.ID, serverJoinedAt)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, `
		INSERT INTO dm_voice_participants (conversation_id, user_id, joined_at)
		VALUES ($1, $2, $3)
	`, conversationID, owner.ID, privateJoinedAt)
	require.NoError(t, err)

	_, err = ts.DB.ExecContext(ctx, ups[0])
	require.NoError(t, err)
	phase = 1
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseColumn(t, ts.DB, table, "YES")
		migration000093AssertPhaseCheck(t, ts.DB, table, false, false)
	}
	migration000093AssertLifecycleTime(t, ts.DB, `
		SELECT lifecycle_event_at FROM voice_participants
		WHERE channel_id = $1 AND user_id = $2
	`, channelID, owner.ID, sql.NullTime{})
	migration000093AssertLifecycleTime(t, ts.DB, `
		SELECT lifecycle_event_at FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, owner.ID, sql.NullTime{})

	defaultUser := ts.CreateTestUser(t, "voice_lifecycle_phase_default")
	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`,
		channelID, defaultUser.ID)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`,
		conversationID, defaultUser.ID)
	require.NoError(t, err)
	for _, query := range []struct {
		sql      string
		parentID string
	}{
		{`SELECT joined_at, lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, channelID},
		{`SELECT joined_at, lifecycle_event_at FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID},
	} {
		var joinedAt, lifecycleAt time.Time
		require.NoError(t, ts.DB.QueryRowContext(
			ctx, query.sql, query.parentID, defaultUser.ID,
		).Scan(&joinedAt, &lifecycleAt))
		assert.Equal(t, joinedAt, lifecycleAt, "both defaults use the transaction timestamp")
	}
	_, err = ts.DB.ExecContext(ctx, `
		UPDATE voice_participants SET is_muted = is_muted
		WHERE channel_id = $1 AND user_id = $2
	`, channelID, owner.ID)
	require.NoError(t, err,
		"legacy replicas must update pre-backfill server voice rows during phase 000093")
	_, err = ts.DB.ExecContext(ctx, `
		UPDATE dm_voice_participants SET is_muted = is_muted
		WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, owner.ID)
	require.NoError(t, err,
		"legacy replicas must update pre-backfill private-call rows during phase 000093")

	_, err = ts.DB.ExecContext(ctx, ups[1])
	require.NoError(t, err)
	phase = 2

	migration000093AssertLifecycleTime(t, ts.DB, `
		SELECT lifecycle_event_at FROM voice_participants
		WHERE channel_id = $1 AND user_id = $2
	`, channelID, owner.ID, sql.NullTime{Time: serverJoinedAt, Valid: true})
	migration000093AssertLifecycleTime(t, ts.DB, `
		SELECT lifecycle_event_at FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, owner.ID, sql.NullTime{Time: privateJoinedAt, Valid: true})
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseColumn(t, ts.DB, table, "YES")
		migration000093AssertPhaseCheck(t, ts.DB, table, false, false)
	}

	_, err = ts.DB.ExecContext(ctx, ups[2])
	require.NoError(t, err)
	phase = 3
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseColumn(t, ts.DB, table, "YES")
		migration000093AssertPhaseCheck(t, ts.DB, table, true, false)
	}

	invalidUser := ts.CreateTestUser(t, "voice_lifecycle_phase_invalid")
	_, err = ts.DB.ExecContext(ctx, `
		INSERT INTO voice_participants (channel_id, user_id, lifecycle_event_at)
		VALUES ($1, $2, NULL)
	`, channelID, invalidUser.ID)
	require.Error(t, err, "a NOT VALID check must reject new null values")

	writerTx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	writerClosed := false
	t.Cleanup(func() {
		if !writerClosed {
			require.NoError(t, writerTx.Rollback())
		}
	})
	_, err = writerTx.ExecContext(ctx, `
		UPDATE voice_participants SET is_muted = is_muted
		WHERE channel_id = $1 AND user_id = $2
	`, channelID, defaultUser.ID)
	require.NoError(t, err)
	validationDone := make(chan error, 1)
	validationCtx, cancelValidation := context.WithTimeout(ctx, 4*time.Second)
	defer cancelValidation()
	go func() {
		_, execErr := ts.DB.ExecContext(validationCtx, ups[3])
		validationDone <- execErr
	}()
	select {
	case validationErr := <-validationDone:
		require.NoError(t, validationErr,
			"validation must coexist with a row-exclusive writer")
		phase = 4
	case <-time.After(2 * time.Second):
		require.NoError(t, writerTx.Rollback())
		writerClosed = true
		select {
		case validationErr := <-validationDone:
			if validationErr == nil {
				phase = 4
			}
			t.Fatalf("phase-000096 blocked behind a row-exclusive writer: %v", validationErr)
		case <-validationCtx.Done():
			t.Fatalf("phase-000096 remained blocked after the writer rolled back: %v",
				validationCtx.Err())
		}
	}
	require.NoError(t, writerTx.Rollback())
	writerClosed = true

	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseColumn(t, ts.DB, table, "YES")
		migration000093AssertPhaseCheck(t, ts.DB, table, true, true)
	}

	_, err = ts.DB.ExecContext(ctx, ups[4])
	require.NoError(t, err)
	phase = 5
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseColumn(t, ts.DB, table, "NO")
		migration000093AssertPhaseCheck(t, ts.DB, table, false, false)
	}
	_, err = ts.DB.ExecContext(ctx, ups[5])
	require.NoError(t, err)
	phase = 6

	_, err = ts.DB.ExecContext(ctx, downs[5])
	require.NoError(t, err)
	phase = 5
	_, err = ts.DB.ExecContext(ctx, downs[4])
	require.NoError(t, err, "enforcement rollback must preserve active participants")
	phase = 4
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseColumn(t, ts.DB, table, "YES")
		migration000093AssertPhaseCheck(t, ts.DB, table, true, false)
	}

	replayWriterTx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	replayWriterClosed := false
	_, err = replayWriterTx.ExecContext(ctx, `
		UPDATE voice_participants SET is_muted = is_muted
		WHERE channel_id = $1 AND user_id = $2
	`, channelID, defaultUser.ID)
	require.NoError(t, err)
	replayCtx, cancelReplay := context.WithTimeout(ctx, 4*time.Second)
	defer cancelReplay()
	replayConn, err := ts.DB.Conn(replayCtx)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, replayConn.Close()) })
	t.Cleanup(func() {
		if !replayWriterClosed {
			require.NoError(t, replayWriterTx.Rollback())
		}
	})
	var replayPID int
	require.NoError(t,
		replayConn.QueryRowContext(replayCtx, `SELECT pg_backend_pid()`).Scan(&replayPID))
	replayDone := make(chan error, 1)
	go func() {
		_, execErr := replayConn.ExecContext(replayCtx, ups[4])
		replayDone <- execErr
	}()
	require.Eventually(t, func() bool {
		var validationsGranted, exclusivePending int
		err := ts.DB.QueryRowContext(ctx, `
			SELECT
				COUNT(*) FILTER (
					WHERE mode = 'ShareUpdateExclusiveLock' AND granted
				),
				COUNT(*) FILTER (
					WHERE relation = to_regclass('public.voice_participants')
					  AND mode = 'AccessExclusiveLock' AND NOT granted
				)
			FROM pg_locks
			WHERE pid = $1
			  AND relation IN (
				to_regclass('public.voice_participants'),
				to_regclass('public.dm_voice_participants')
			  )
		`, replayPID).Scan(&validationsGranted, &exclusivePending)
		return err == nil && validationsGranted == 2 && exclusivePending == 1
	}, 2*time.Second, 10*time.Millisecond,
		"replay must validate both guards before waiting for ACCESS EXCLUSIVE")
	require.NoError(t, replayWriterTx.Rollback())
	replayWriterClosed = true
	select {
	case replayErr := <-replayDone:
		require.NoError(t, replayErr, "migration 000097 must reapply after its down migration")
		phase = 5
	case <-replayCtx.Done():
		t.Fatalf("migration 000097 replay did not finish: %v", replayCtx.Err())
	}
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseColumn(t, ts.DB, table, "NO")
		migration000093AssertPhaseCheck(t, ts.DB, table, false, false)
	}
	_, err = ts.DB.ExecContext(ctx, downs[4])
	require.NoError(t, err)
	phase = 4
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseColumn(t, ts.DB, table, "YES")
		migration000093AssertPhaseCheck(t, ts.DB, table, true, false)
	}

	_, err = ts.DB.ExecContext(ctx, downs[3])
	require.NoError(t, err)
	phase = 3
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseCheck(t, ts.DB, table, true, false)
	}
	_, err = ts.DB.ExecContext(ctx, downs[2])
	require.NoError(t, err)
	phase = 2
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseCheck(t, ts.DB, table, false, false)
	}

	_, err = ts.DB.ExecContext(ctx, downs[1])
	require.ErrorContains(t, err, "cannot roll back migration 000094 while voice participants are active")
	_, err = ts.DB.ExecContext(ctx, `DELETE FROM dm_voice_participants`)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, `DELETE FROM voice_participants`)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, downs[1])
	require.NoError(t, err)
	phase = 1
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseCheck(t, ts.DB, table, false, false)
	}

	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`,
		conversationID, owner.ID)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, downs[0])
	require.ErrorContains(t, err, "cannot roll back migration 000093 while voice participants are active")
	_, err = ts.DB.ExecContext(ctx, `DELETE FROM dm_voice_participants`)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, downs[0])
	require.NoError(t, err)
	phase = 0
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertPhaseColumnMissing(t, ts.DB, table)
	}

	for _, up := range ups {
		_, err = ts.DB.ExecContext(ctx, up)
		require.NoError(t, err, "all phases must reapply after a full rollback")
		phase++
	}
}

func migration000093AssertPhaseColumn(
	t *testing.T,
	db *sql.DB,
	table, wantNullable string,
) {
	t.Helper()
	var dataType, nullable, defaultExpression, comment string
	require.NoError(t, db.QueryRow(`
		SELECT c.data_type, c.is_nullable, c.column_default,
		       col_description(format('%I.%I', c.table_schema, c.table_name)::regclass, c.ordinal_position)
		FROM information_schema.columns c
		WHERE c.table_schema = 'public' AND c.table_name = $1
		  AND c.column_name = 'lifecycle_event_at'
	`, table).Scan(&dataType, &nullable, &defaultExpression, &comment))
	assert.Equal(t, "timestamp with time zone", dataType)
	assert.Equal(t, wantNullable, nullable)
	assert.Equal(t, "now()", defaultExpression)
	assert.NotEmpty(t, comment)
}

func migration000093AssertPhaseColumnMissing(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1
		  AND column_name = 'lifecycle_event_at'
	`, table).Scan(&count))
	assert.Zero(t, count)
}

func migration000093AssertPhaseCheck(
	t *testing.T,
	db *sql.DB,
	table string,
	wantExists, wantValidated bool,
) {
	t.Helper()
	var validated bool
	err := db.QueryRow(`
		SELECT convalidated FROM pg_constraint
		WHERE conrelid = $1::regclass AND conname = $2
	`, table, table+"_lifecycle_event_at_not_null").Scan(&validated)
	if !wantExists {
		require.ErrorIs(t, err, sql.ErrNoRows)
		return
	}
	require.NoError(t, err)
	assert.Equal(t, wantValidated, validated)
}

func migration000093AssertLifecycleTime(
	t *testing.T,
	db *sql.DB,
	query, parentID, userID string,
	want sql.NullTime,
) {
	t.Helper()
	var got sql.NullTime
	require.NoError(t, db.QueryRow(query, parentID, userID).Scan(&got))
	assert.Equal(t, want.Valid, got.Valid)
	if want.Valid && got.Valid {
		assert.Equal(t, want.Time, got.Time)
	}
}
