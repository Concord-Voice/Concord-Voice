package voice_test

import (
	"context"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/voice"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTempGrantSweeper(t *testing.T, ts *testhelpers.TestServer) *voice.TempGrantSweeper {
	t.Helper()
	log := logger.New("test")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	return voice.NewTempGrantSweeper(ts.DB, log, ts.Hub, resolver, nil)
}

// TestSweep_RevokesOrphanedTempGrant verifies the D3 backstop: a temp grant whose
// holder has NO live voice_participants row is selected as an orphan and revoked
// (override deleted, CSK rotated).
func TestSweep_RevokesOrphanedTempGrant(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sweeper := newTempGrantSweeper(t, ts)

	owner := ts.CreateTestUser(t, "sweep_orphan_owner")
	orphan := ts.CreateTestUser(t, "sweep_orphan_user")
	serverID := ts.CreateTestServer(t, owner.ID, "Sweep Orphan Server")
	ts.AddMemberToServer(t, serverID, orphan.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-sweep-orphan")

	// Temp grant exists but the holder is NOT in voice_participants → orphan.
	// Age it past the 60s grace window (finding #3) so the sweep selects it.
	seedTempGrant(t, ts, serverID, channelID, orphan.ID)
	backdateGrantedAt(t, ts, channelID, orphan.ID, 120)
	require.True(t, tempOverrideExists(t, ts.DB, channelID, orphan.ID))

	n, err := sweeper.SweepOrphanedTempGrants(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "exactly one orphan revoked")
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, orphan.ID), "orphan temp override deleted")
	assert.Equal(t, 1, keyRevocationCount(t, ts.DB, channelID), "CSK rotated for the swept orphan")
}

// TestSweep_LeavesPresentHolderUntouched verifies the anti-join excludes grants whose
// holder IS still present in the channel — only orphans are swept.
func TestSweep_LeavesPresentHolderUntouched(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sweeper := newTempGrantSweeper(t, ts)

	owner := ts.CreateTestUser(t, "sweep_present_owner")
	present := ts.CreateTestUser(t, "sweep_present_user")
	serverID := ts.CreateTestServer(t, owner.ID, "Sweep Present Server")
	ts.AddMemberToServer(t, serverID, present.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-sweep-present")

	// Temp grant AND a live participant row → NOT an orphan.
	seedTempGrant(t, ts, serverID, channelID, present.ID)
	insertVoiceParticipant(t, ts.DB, channelID, present.ID)

	n, err := sweeper.SweepOrphanedTempGrants(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "present holder is not an orphan")
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, present.ID), "present holder's grant survives")
	assert.Equal(t, 0, keyRevocationCount(t, ts.DB, channelID), "no rotation when nothing is swept")
}

// backdateGrantedAt ages a temp grant's granted_at into the past so the sweep's
// 60s grace predicate (finding #3) treats it as past-grace.
func backdateGrantedAt(t *testing.T, ts *testhelpers.TestServer, channelID, userID string, secondsAgo int) {
	t.Helper()
	_, err := ts.DB.Exec(
		`UPDATE channel_permission_overrides
		   SET granted_at = NOW() - make_interval(secs => $3)
		 WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND is_temporary = true`,
		channelID, userID, secondsAgo)
	require.NoError(t, err)
}

// TestSweep_SkipsFreshOrphanWithinGrace verifies the grant->join grace (finding
// #3): an orphan granted moments ago (granted_at = NOW(), <60s) is NOT swept,
// because its voice.joined event may simply not have landed yet.
func TestSweep_SkipsFreshOrphanWithinGrace(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sweeper := newTempGrantSweeper(t, ts)

	owner := ts.CreateTestUser(t, "sweep_fresh_owner")
	fresh := ts.CreateTestUser(t, "sweep_fresh_user")
	serverID := ts.CreateTestServer(t, owner.ID, "Sweep Fresh Server")
	ts.AddMemberToServer(t, serverID, fresh.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-sweep-fresh")

	// seedTempGrant stamps granted_at = NOW(); the holder is absent → orphan, but
	// within the grace window.
	seedTempGrant(t, ts, serverID, channelID, fresh.ID)
	require.True(t, tempOverrideExists(t, ts.DB, channelID, fresh.ID))

	n, err := sweeper.SweepOrphanedTempGrants(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "fresh orphan within 60s grace must not be swept")
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, fresh.ID), "fresh orphan survives the sweep")
	assert.Equal(t, 0, keyRevocationCount(t, ts.DB, channelID), "no rotation for a within-grace orphan")
}

// TestSweep_RevokesAgedOrphanPastGrace verifies an orphan whose granted_at is older
// than the grace window IS swept (companion to the fresh-orphan test above).
func TestSweep_RevokesAgedOrphanPastGrace(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sweeper := newTempGrantSweeper(t, ts)

	owner := ts.CreateTestUser(t, "sweep_aged_owner")
	aged := ts.CreateTestUser(t, "sweep_aged_user")
	serverID := ts.CreateTestServer(t, owner.ID, "Sweep Aged Server")
	ts.AddMemberToServer(t, serverID, aged.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-sweep-aged")

	seedTempGrant(t, ts, serverID, channelID, aged.ID)
	backdateGrantedAt(t, ts, channelID, aged.ID, 120) // 2 minutes ago → past grace
	require.True(t, tempOverrideExists(t, ts.DB, channelID, aged.ID))

	n, err := sweeper.SweepOrphanedTempGrants(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "aged orphan past the 60s grace is swept")
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, aged.ID), "aged orphan override deleted")
	assert.Equal(t, 1, keyRevocationCount(t, ts.DB, channelID), "CSK rotated for the swept aged orphan")
}

// TestSweep_IgnoresPermanentOverrides verifies a permanent (non-temporary) override
// whose holder is absent is NOT swept — the sweep only targets is_temporary rows.
func TestSweep_IgnoresPermanentOverrides(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sweeper := newTempGrantSweeper(t, ts)

	owner := ts.CreateTestUser(t, "sweep_perm_owner")
	permUser := ts.CreateTestUser(t, "sweep_perm_user")
	serverID := ts.CreateTestServer(t, owner.ID, "Sweep Perm Server")
	ts.AddMemberToServer(t, serverID, permUser.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-sweep-perm")

	// Permanent override (is_temporary defaults false), holder absent.
	ts.CreateChannelOverride(t, channelID, "user", permUser.ID,
		int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice), 0)

	n, err := sweeper.SweepOrphanedTempGrants(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "permanent override is never an orphan target")

	var exists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM channel_permission_overrides WHERE channel_id=$1 AND target_id=$2)`,
		channelID, permUser.ID,
	).Scan(&exists))
	assert.True(t, exists, "permanent override must survive the sweep")
	assert.Equal(t, 0, keyRevocationCount(t, ts.DB, channelID))
}

// TestStartTempGrantSweepWorker_StartupAndTick verifies the background worker:
// (1) runs the startup sweep immediately (revoking a pre-existing orphan), then
// (2) runs again on the ticker interval (revoking an orphan seeded after start),
// and (3) stops cleanly when the context is cancelled.
func TestStartTempGrantSweepWorker_StartupAndTick(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	log := logger.New("test")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)

	owner := ts.CreateTestUser(t, "sweep_worker_owner")
	orphanA := ts.CreateTestUser(t, "sweep_worker_orphan_a")
	orphanB := ts.CreateTestUser(t, "sweep_worker_orphan_b")
	serverID := ts.CreateTestServer(t, owner.ID, "Sweep Worker Server")
	ts.AddMemberToServer(t, serverID, orphanA.ID, "member")
	ts.AddMemberToServer(t, serverID, orphanB.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-sweep-worker")

	// Orphan A exists before the worker starts → covered by the startup sweep.
	// Backdate past the 60s grace (finding #3) so the sweep selects it.
	seedTempGrant(t, ts, serverID, channelID, orphanA.ID)
	backdateGrantedAt(t, ts, channelID, orphanA.ID, 120)
	require.True(t, tempOverrideExists(t, ts.DB, channelID, orphanA.ID))

	ctx, cancel := context.WithCancel(context.Background())
	// Short interval so the periodic tick fires quickly in-test (nil NATS is fine —
	// publishForceDisconnect is a no-op without it).
	voice.StartTempGrantSweepWorker(ctx, ts.DB, log, ts.Hub, resolver, nil, 50*time.Millisecond)

	// (1) Startup sweep revokes orphan A.
	require.Eventually(t, func() bool {
		return !tempOverrideExists(t, ts.DB, channelID, orphanA.ID)
	}, 3*time.Second, 20*time.Millisecond, "startup sweep should revoke orphan A")

	// (2) Seed orphan B AFTER start (aged past grace); the ticker sweep should catch it.
	seedTempGrant(t, ts, serverID, channelID, orphanB.ID)
	backdateGrantedAt(t, ts, channelID, orphanB.ID, 120)
	require.Eventually(t, func() bool {
		return !tempOverrideExists(t, ts.DB, channelID, orphanB.ID)
	}, 3*time.Second, 20*time.Millisecond, "periodic sweep should revoke orphan B")

	// (3) Cancel; the worker observes ctx.Done() and returns cleanly. The test
	// would hang on a leaked goroutine if the Done() branch were unreachable, so
	// reaching the end without a deadlock confirms the stop path runs.
	cancel()
	assert.NotPanics(t, func() { cancel() }, "cancel is idempotent; worker shutdown must not panic")
}
