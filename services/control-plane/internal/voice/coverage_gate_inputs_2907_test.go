package voice_test

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test2907VoiceInputBoundsFailClosed(t *testing.T) {
	_, err := voice.ServerVoiceLifecycleAdvisoryKeyForTest(uuid.Nil)
	assert.Error(t, err)
}

func Test2907VoiceMissingScopesAndParticipantsStayEmpty(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)
	var before, after int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM voice_participants`).Scan(&before))
	sub.HandleRoomEmpty([]byte(`{"callId":"not-a-uuid","timestamp":"not-a-time"}`))
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM voice_participants`).Scan(&after))
	assert.Equal(t, before, after, "invalid room-empty input must not mutate participants")
}

func Test2907VoiceReconcileEmptyBatchAtCapIsNoOp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := newTestSubscriber(ts)

	owner := ts.CreateTestUser(t, "coverage-cap-owner")
	serverID := ts.CreateTestServer(t, owner.ID, "coverage-cap-server")
	channelID := ts.CreateVoiceChannel(t, serverID, "coverage-cap-channel")
	prefix := "coverage_cap_" + uuid.NewString()[:8]
	_, err := ts.DB.Exec(`
		INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		SELECT gen_random_uuid(), $1 || '_' || series || '@test.concord.chat', $1 || '_' || series,
		       'coverage-cap-hash', TRUE, TRUE
		FROM generate_series(1, 1001) AS series`, prefix)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO voice_participants (channel_id, user_id, joined_at, lifecycle_observed_at)
		SELECT $1, id, clock_timestamp(), clock_timestamp()
		FROM users WHERE LEFT(username, LENGTH($2)) = $2`, channelID, prefix)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		UPDATE voice_participants
		SET lifecycle_observed_at = clock_timestamp() - interval '91 seconds'
		WHERE channel_id = $1 AND user_id IN (
			SELECT id FROM users WHERE LEFT(username, LENGTH($2)) = $2
		)`, channelID, prefix)
	require.NoError(t, err)
	require.Equal(t, 1001, countVoiceParticipants(t, ts.DB, channelID))

	// The public entry point clamps the requested batch to the production cap.
	_, err = sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1)
	require.NoError(t, err)
	sub.CompleteServerVoiceCleanupGraceForTest()
	revoked, err := sub.ReconcileStaleServerVoiceParticipants(context.Background(), 1001)
	require.NoError(t, err)
	assert.Equal(t, 1000, revoked)
	assert.Equal(t, 1, countVoiceParticipants(t, ts.DB, channelID), "the cap must leave one stale participant for the next pass")
}

func Test2907VoiceOrphanAndRevokeInputsFailClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "coverage-input-owner")
	serverID := ts.CreateTestServer(t, owner.ID, "coverage-input-server")
	channelID := ts.CreateVoiceChannel(t, serverID, "coverage-input-channel")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	mgr := voice.NewTestTempGrantManager(ts.DB, logger.New("test"), ts.Hub, resolver, nil)

	err := mgr.Revoke(context.Background(), serverID, channelID, "not-a-uuid", "")
	require.Error(t, err)

	validUser := uuid.NewString()
	err = mgr.Revoke(context.Background(), serverID, channelID, validUser, "")
	require.NoError(t, err)
	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM channel_permission_overrides WHERE channel_id = $1 AND target_id = $2`,
		channelID, validUser,
	).Scan(&count))
	assert.Zero(t, count, "missing grant cleanup must not create durable rows")
}
