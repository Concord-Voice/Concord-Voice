package users

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type replayActivitySettingsSuppressor struct {
	mu           sync.Mutex
	calls        int
	err          error
	hook         func()
	accountCalls int
	accountErr   error
	accountHook  func()
}

func (s *replayActivitySettingsSuppressor) SuppressAllActivityAlreadyGated(
	_ context.Context,
	_ uuid.UUID,
) error {
	if s.accountHook != nil {
		s.accountHook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountCalls++
	return s.accountErr
}

func (s *replayActivitySettingsSuppressor) ApplySettingsSuppressionAlreadyGated(
	context.Context,
	uuid.UUID,
	presence.ActivityPolicySettings,
	presence.ActivityPolicySettings,
) error {
	if s.hook != nil {
		s.hook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.err
}

func (s *replayActivitySettingsSuppressor) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *replayActivitySettingsSuppressor) accountCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accountCalls
}

func TestActivityTierFromPersisted(t *testing.T) {
	for value, want := range map[int]presence.Tier{
		0: presence.TierOff,
		1: presence.TierFriends,
		2: presence.TierServers,
	} {
		tier, err := activityTierFromPersisted(value)
		require.NoError(t, err)
		assert.Equal(t, want, tier)
	}
	for _, value := range []int{-1, 3} {
		_, err := activityTierFromPersisted(value)
		require.Error(t, err)
	}
}

func TestActivitySettingsCleanupAdvisoryKeyIsNamespacedAndDeterministic(t *testing.T) {
	userID := uuid.New()
	key, err := activitySettingsCleanupAdvisoryKey(userID)
	require.NoError(t, err)
	repeated, err := activitySettingsCleanupAdvisoryKey(userID)
	require.NoError(t, err)
	other, err := activitySettingsCleanupAdvisoryKey(uuid.New())
	require.NoError(t, err)

	assert.Equal(t, key, repeated)
	assert.NotEqual(t, key, other)
	_, err = activitySettingsCleanupAdvisoryKey(uuid.Nil)
	assert.Error(t, err)
}

func TestActivitySettingsCleanupEvidenceRejectsMalformedRecords(t *testing.T) {
	for _, raw := range []string{
		`not-json`,
		`{"version":2,"before":{},"after":{}}`,
		`{"version":1,"before":{"server_voice_tier":3},"after":{}}`,
		`{"version":1,"before":{},"after":{},"unexpected":true}`,
		`{"version":1,"before":{},"after":{}}`,
	} {
		_, _, _, err := decodeActivitySettingsCleanupEvidence([]byte(raw))
		assert.ErrorIs(t, err, errInvalidActivityCleanupEvidence, raw)
	}
}

func TestResumePendingActivitySettingsCleanupClassifiesDatabaseResourceErrorsAsRetryable(t *testing.T) {
	db, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, db.Close())
	handler := &Handler{db: db}

	_, err = handler.resumePendingActivitySettingsCleanup(context.Background(), uuid.New())

	var writerErr *presenceWriterFailure
	require.ErrorAs(t, err, &writerErr)
	assert.Equal(t, 503, writerErr.status)
	assert.Equal(t, "activity_cleanup", writerErr.class)
	assert.False(t, errors.Is(err, errInvalidActivityCleanupEvidence))
}

func TestJoinActivitySettingsCleanupRollbackPreservesWriterFailureAndRollbackCause(t *testing.T) {
	primaryCause := errors.New("cleanup primary failure")
	rollbackCause := errors.New("cleanup rollback failure")
	returnErr := error(&presenceWriterFailure{
		status: 503, class: "activity_cleanup", cause: primaryCause,
	})

	joinActivitySettingsCleanupRollback(func() error { return rollbackCause }, &returnErr)

	assert.ErrorIs(t, returnErr, primaryCause)
	assert.ErrorIs(t, returnErr, rollbackCause)
	var writerErr *presenceWriterFailure
	require.ErrorAs(t, returnErr, &writerErr)
	assert.Equal(t, 503, writerErr.status)
	assert.Equal(t, "activity_cleanup", writerErr.class)

	previous := returnErr
	joinActivitySettingsCleanupRollback(func() error { return sql.ErrTxDone }, &returnErr)
	assert.Equal(t, previous, returnErr, "an already completed transaction is not a rollback failure")
}

func TestDeleteActivitySettingsCleanupCannotDeleteSuccessorMarker(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	successorID := uuid.New()
	before := presence.ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: presence.TierFriends,
	}
	after := presence.ActivityPolicySettings{
		MasterEnabled: false, ServerVoiceTier: presence.TierOff,
	}
	evidence, err := encodeActivitySettingsCleanupEvidence(before, after)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO activity_settings_pending_cleanups (
			user_id, operation_id, evidence
		) VALUES ($1, $2, $3)
	`, userID, successorID, string(evidence))
	require.NoError(t, err)

	require.NoError(t, deleteActivitySettingsCleanup(
		context.Background(), db, userID, uuid.New(),
	))

	var retained uuid.UUID
	require.NoError(t, db.QueryRow(`
		SELECT operation_id FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, userID).Scan(&retained))
	assert.Equal(t, successorID, retained)
}

func TestResumePendingActivitySettingsCleanupReplaysFinalizationWithoutSuppression(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	before := presence.ActivityPolicySettings{
		MasterEnabled: true, ServerVoiceTier: presence.TierFriends,
	}
	after := presence.ActivityPolicySettings{
		MasterEnabled: false, ServerVoiceTier: presence.TierOff,
	}
	evidence, err := encodeActivitySettingsCleanupEvidence(before, after)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO activity_settings_pending_cleanups (
			user_id, operation_id, evidence
		) VALUES ($1, $2, $3)
	`, userID, uuid.New(), string(evidence))
	require.NoError(t, err)
	require.NoError(t, execInternalActivityCleanupSQL(db, `
		CREATE OR REPLACE FUNCTION reject_internal_activity_cleanup_delete_for_test()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'forced activity cleanup finalization rollback';
		END
		$$
	`))
	require.NoError(t, execInternalActivityCleanupSQL(db, `
		CREATE TRIGGER reject_internal_activity_cleanup_delete_for_test
		BEFORE DELETE ON activity_settings_pending_cleanups
		FOR EACH ROW EXECUTE FUNCTION reject_internal_activity_cleanup_delete_for_test()
	`))
	t.Cleanup(func() {
		assert.NoError(t, execInternalActivityCleanupSQL(db, `
			DROP TRIGGER IF EXISTS reject_internal_activity_cleanup_delete_for_test
			ON activity_settings_pending_cleanups
		`))
		assert.NoError(t, execInternalActivityCleanupSQL(db,
			`DROP FUNCTION IF EXISTS reject_internal_activity_cleanup_delete_for_test()`))
	})

	suppressor := &replayActivitySettingsSuppressor{}
	handler := &Handler{db: db, activitySuppressor: suppressor}
	resumed, err := handler.resumePendingActivitySettingsCleanup(
		context.Background(), userID,
	)
	require.True(t, resumed)
	require.Error(t, err)
	require.Equal(t, 1, suppressor.callCount())

	var suppressionCompleted bool
	require.NoError(t, db.QueryRow(`
		SELECT COALESCE((evidence ->> 'suppressed')::boolean, false)
		FROM activity_settings_pending_cleanups
		WHERE user_id = $1
	`, userID).Scan(&suppressionCompleted))
	assert.True(t, suppressionCompleted)

	require.NoError(t, execInternalActivityCleanupSQL(db, `
		DROP TRIGGER reject_internal_activity_cleanup_delete_for_test
		ON activity_settings_pending_cleanups
	`))
	suppressor.mu.Lock()
	suppressor.err = errors.New("settings evidence unavailable after prior suppression")
	suppressor.mu.Unlock()
	resumed, err = handler.resumePendingActivitySettingsCleanup(
		context.Background(), userID,
	)

	require.NoError(t, err)
	assert.True(t, resumed)
	assert.Equal(t, 1, suppressor.callCount())
	var pending int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, userID).Scan(&pending))
	assert.Zero(t, pending)
}

func execInternalActivityCleanupSQL(db *sql.DB, statement string) error {
	_, err := db.Exec(statement)
	return err
}
