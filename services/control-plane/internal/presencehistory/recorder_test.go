package presencehistory

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordCustomTextTransitionIntervals(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	ctx := context.Background()
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	repository := NewRepository(ts.DB, disclosure)

	t.Run("disabled records nothing", func(t *testing.T) {
		user := ts.CreateTestUser(t, "history_disabled")
		userID := uuid.MustParse(user.ID)
		require.NoError(t, recordAndCommit(t, repository, userID,
			CustomTextState{}, CustomTextState{Text: "not retained"}))
		assert.Zero(t, historyRowCount(t, ts.DB, userID))
	})

	t.Run("enable does not backfill unchanged current state", func(t *testing.T) {
		user := ts.CreateTestUser(t, "history_no_backfill")
		userID := uuid.MustParse(user.ID)
		enableHistory(t, ts.DB, userID, disclosure, 30)
		current := CustomTextState{Text: "already active", Emoji: "🕰️"}
		require.NoError(t, recordAndCommit(t, repository, userID, current, current))
		assert.Zero(t, historyRowCount(t, ts.DB, userID))
	})

	t.Run("first state opens an interval", func(t *testing.T) {
		user := ts.CreateTestUser(t, "history_first_open")
		userID := uuid.MustParse(user.ID)
		enableHistory(t, ts.DB, userID, disclosure, 30)
		require.NoError(t, recordAndCommit(t, repository, userID,
			CustomTextState{}, CustomTextState{Text: "Reviewing", Emoji: "🔍"}))

		var (
			payload               string
			startedAt, recordedAt time.Time
			endedAt               sql.NullTime
			expiresAt             time.Time
		)
		require.NoError(t, ts.DB.QueryRowContext(ctx, `
			SELECT payload::TEXT, started_at, ended_at, recorded_at, expires_at
			FROM presence_history
			WHERE sender_id = $1 AND category = 'custom_text'
		`, userID).Scan(&payload, &startedAt, &endedAt, &recordedAt, &expiresAt))
		assert.JSONEq(t, `{"text":"Reviewing","emoji":"🔍"}`, payload)
		assert.Equal(t, startedAt, recordedAt)
		assert.False(t, endedAt.Valid)
		assert.Equal(t, 30*24*time.Hour, expiresAt.Sub(recordedAt))
	})

	t.Run("semantic change closes and opens at one database timestamp", func(t *testing.T) {
		user := ts.CreateTestUser(t, "history_change")
		userID := uuid.MustParse(user.ID)
		enableHistory(t, ts.DB, userID, disclosure, 7)
		first := CustomTextState{Text: "First", Emoji: "1️⃣"}
		second := CustomTextState{Text: "Second", Emoji: "2️⃣"}
		require.NoError(t, recordAndCommit(t, repository, userID, CustomTextState{}, first))
		require.NoError(t, recordAndCommit(t, repository, userID, first, second))

		rows, err := ts.DB.QueryContext(ctx, `
			SELECT payload->>'text', started_at, ended_at, recorded_at
			FROM presence_history
			WHERE sender_id = $1
			ORDER BY recorded_at, id
		`, userID)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()

		var firstText string
		var firstStarted, firstEnded, firstRecorded time.Time
		require.True(t, rows.Next())
		require.NoError(t, rows.Scan(&firstText, &firstStarted, &firstEnded, &firstRecorded))
		var secondText string
		var secondStarted, secondRecorded time.Time
		var secondEnded sql.NullTime
		require.True(t, rows.Next())
		require.NoError(t, rows.Scan(&secondText, &secondStarted, &secondEnded, &secondRecorded))
		require.NoError(t, rows.Err())
		assert.False(t, rows.Next())
		assert.Equal(t, "First", firstText)
		assert.Equal(t, "Second", secondText)
		assert.Equal(t, firstEnded, secondStarted)
		assert.Equal(t, secondStarted, secondRecorded)
		assert.False(t, secondEnded.Valid)
	})

	t.Run("clear closes without opening", func(t *testing.T) {
		user := ts.CreateTestUser(t, "history_clear")
		userID := uuid.MustParse(user.ID)
		enableHistory(t, ts.DB, userID, disclosure, 30)
		active := CustomTextState{Text: "Temporary"}
		require.NoError(t, recordAndCommit(t, repository, userID, CustomTextState{}, active))
		require.NoError(t, recordAndCommit(t, repository, userID, active, CustomTextState{}))

		assert.Equal(t, 1, historyRowCount(t, ts.DB, userID))
		assert.Zero(t, openHistoryRowCount(t, ts.DB, userID))
	})

	t.Run("identical and emoji-empty normalized states are no-ops", func(t *testing.T) {
		user := ts.CreateTestUser(t, "history_noop")
		userID := uuid.MustParse(user.ID)
		enableHistory(t, ts.DB, userID, disclosure, 30)
		active := CustomTextState{Text: "Same"}
		require.NoError(t, recordAndCommit(t, repository, userID, CustomTextState{}, active))
		require.NoError(t, recordAndCommit(t, repository, userID, active,
			CustomTextState{Text: "Same", Emoji: ""}))
		assert.Equal(t, 1, historyRowCount(t, ts.DB, userID))
		assert.Equal(t, 1, openHistoryRowCount(t, ts.DB, userID))
	})

	t.Run("invalid typed payload is rejected before storage", func(t *testing.T) {
		user := ts.CreateTestUser(t, "history_invalid_payload")
		userID := uuid.MustParse(user.ID)
		enableHistory(t, ts.DB, userID, disclosure, 30)
		err := recordAndCommit(t, repository, userID, CustomTextState{},
			CustomTextState{Text: strings.Repeat("界", 141)})
		require.Error(t, err)
		assert.Zero(t, historyRowCount(t, ts.DB, userID))
	})

	t.Run("caller rollback removes the whole transition", func(t *testing.T) {
		user := ts.CreateTestUser(t, "history_rollback")
		userID := uuid.MustParse(user.ID)
		enableHistory(t, ts.DB, userID, disclosure, 30)
		tx, err := ts.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, repository.RecordCustomTextTransition(ctx, tx, userID,
			CustomTextState{}, CustomTextState{Text: "rolled back"}))
		require.NoError(t, tx.Rollback())
		assert.Zero(t, historyRowCount(t, ts.DB, userID))
	})

	t.Run("existing open row is evicted before replacement", func(t *testing.T) {
		user := ts.CreateTestUser(t, "history_old_open")
		userID := uuid.MustParse(user.ID)
		enableHistory(t, ts.DB, userID, disclosure, 30)
		oldTime := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
			`{"text":"stale"}`, oldTime, nil, oldTime, oldTime.Add(30*24*time.Hour))

		require.NoError(t, recordAndCommit(t, repository, userID,
			CustomTextState{Text: "stale"}, CustomTextState{Text: "fresh"}))
		assert.Equal(t, 2, historyRowCount(t, ts.DB, userID))
		assert.Equal(t, 1, openHistoryRowCount(t, ts.DB, userID))
	})
}

func TestRecordCustomTextTransitionPausesStaleOrUnavailableDisclosure(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	ctx := context.Background()
	current := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})

	for _, tc := range []struct {
		name       string
		repository *Repository
		staleHash  string
	}{
		{
			name:       "stale copy hash",
			repository: NewRepository(ts.DB, current),
			staleHash:  strings.Repeat("0", 64),
		},
		{
			name: "current disclosure unavailable",
			repository: NewRepository(ts.DB, BuildDisclosure(DisclosureOptions{
				InstanceType: "self-hosted",
			})),
			staleHash: current.RequiredConsent.CopyHash,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := ts.CreateTestUser(t, "history_pause_"+strings.ReplaceAll(tc.name, " ", "_"))
			userID := uuid.MustParse(user.ID)
			_, err := ts.DB.ExecContext(ctx, `
				INSERT INTO user_presence_settings (
					user_id,
					activity_history_enabled,
					activity_history_retention_days,
					activity_history_consent_version,
					activity_history_consent_copy_hash,
					activity_history_consented_at
				) VALUES ($1, TRUE, 30, $2, $3, clock_timestamp())
			`, userID, current.RequiredConsent.Version, tc.staleHash)
			require.NoError(t, err)
			now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
			insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
				`{"text":"preserved"}`, now, nil, now, now.Add(24*time.Hour))

			require.NoError(t, recordAndCommit(t, tc.repository, userID,
				CustomTextState{Text: "preserved"}, CustomTextState{Text: "must not record"}))

			var enabled, reconsent bool
			var version sql.NullInt64
			var hash sql.NullString
			var consentedAt sql.NullTime
			require.NoError(t, ts.DB.QueryRowContext(ctx, `
				SELECT activity_history_enabled,
				       activity_history_reconsent_required,
				       activity_history_consent_version,
				       activity_history_consent_copy_hash,
				       activity_history_consented_at
				FROM user_presence_settings
				WHERE user_id = $1
			`, userID).Scan(&enabled, &reconsent, &version, &hash, &consentedAt))
			assert.False(t, enabled)
			assert.True(t, reconsent)
			assert.False(t, version.Valid)
			assert.False(t, hash.Valid)
			assert.False(t, consentedAt.Valid)
			assert.Equal(t, 1, historyRowCount(t, ts.DB, userID), "existing history must be preserved")
			assert.Zero(t, openHistoryRowCount(t, ts.DB, userID),
				"pausing recording must close the current interval")
		})
	}
}

func TestRecordCustomTextTransitionLockDelayUsesPostLockClock(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	repository := NewRepository(ts.DB, disclosure)
	user := ts.CreateTestUser(t, "history_lock_delay")
	userID := uuid.MustParse(user.ID)
	enableHistory(t, ts.DB, userID, disclosure, 30)

	blocker, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(ctx, `
		SELECT user_id FROM user_presence_settings WHERE user_id = $1 FOR UPDATE
	`, userID)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		tx, beginErr := ts.DB.BeginTx(ctx, nil)
		if beginErr != nil {
			done <- beginErr
			return
		}
		if recordErr := repository.RecordCustomTextTransition(ctx, tx, userID,
			CustomTextState{}, CustomTextState{Text: "after lock"}); recordErr != nil {
			_ = tx.Rollback()
			done <- recordErr
			return
		}
		done <- tx.Commit()
	}()

	select {
	case early := <-done:
		require.Failf(t, "recorder did not wait for settings lock", "result: %v", early)
	case <-time.After(100 * time.Millisecond):
	}

	var releaseBoundary time.Time
	require.NoError(t, blocker.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&releaseBoundary))
	require.NoError(t, blocker.Commit())
	require.NoError(t, <-done)

	var recordedAt time.Time
	require.NoError(t, ts.DB.QueryRowContext(ctx, `
		SELECT recorded_at FROM presence_history WHERE sender_id = $1
	`, userID).Scan(&recordedAt))
	assert.False(t, recordedAt.Before(releaseBoundary),
		"recording clock must be captured only after the settings lock is acquired")
}

func recordAndCommit(
	t *testing.T,
	repository *Repository,
	userID uuid.UUID,
	before CustomTextState,
	after CustomTextState,
) error {
	t.Helper()
	tx, err := repository.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if err := repository.RecordCustomTextTransition(context.Background(), tx, userID, before, after); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func enableHistory(
	t *testing.T,
	db *sql.DB,
	userID uuid.UUID,
	disclosure DisclosureState,
	retentionDays int16,
) {
	t.Helper()
	require.True(t, disclosure.Available)
	require.NotNil(t, disclosure.RequiredConsent)
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id,
			activity_history_enabled,
			activity_history_retention_days,
			activity_history_consent_version,
			activity_history_consent_copy_hash,
			activity_history_consented_at
		) VALUES ($1, TRUE, $2, $3, $4, clock_timestamp())
	`, userID, retentionDays, disclosure.RequiredConsent.Version, disclosure.RequiredConsent.CopyHash)
	require.NoError(t, err)
}

func historyRowCount(t *testing.T, db *sql.DB, userID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM presence_history WHERE sender_id = $1`, userID).Scan(&count))
	return count
}

func openHistoryRowCount(t *testing.T, db *sql.DB, userID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM presence_history WHERE sender_id = $1 AND ended_at IS NULL
	`, userID).Scan(&count))
	return count
}

func insertHistoryRow(
	t *testing.T,
	db *sql.DB,
	userID uuid.UUID,
	category Category,
	payloadVersion int16,
	payload string,
	startedAt time.Time,
	endedAt any,
	recordedAt time.Time,
	expiresAt time.Time,
) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := db.Exec(`
		INSERT INTO presence_history (
			id, sender_id, category, payload_version, payload,
			started_at, ended_at, recorded_at, expires_at
		) VALUES ($1, $2, $3, $4, $5::JSONB, $6, $7, $8, $9)
	`, id, userID, category, payloadVersion, payload, startedAt, endedAt, recordedAt, expiresAt)
	require.NoError(t, err)
	return id
}
