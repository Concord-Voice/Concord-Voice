package voice_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tgActorSystem is the actor string passed by system-triggered temp-SBAC cleanup
// (presence/heartbeat/sweep). It is the EMPTY string — there is no human actor — so
// the resulting key_revocations row stores revoked_by as SQL NULL (the column is
// nullable, REFERENCES users(id) ON DELETE SET NULL). Passing a non-empty,
// non-existent UUID here would trip key_revocations_revoked_by_fkey.
const tgActorSystem = ""

type ambiguousGrantPlan struct{}

func (ambiguousGrantPlan) HasWork() bool { return true }

type ambiguousGrantRecheck struct{ abandoned int }

func (r *ambiguousGrantRecheck) PrepareCapture(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	return ambiguousGrantPlan{}, nil
}
func (r *ambiguousGrantRecheck) CaptureVisibility(context.Context, *sql.Tx, rbac.PresenceRecheckPlan) error {
	return errors.New("forced capture failure")
}
func (*ambiguousGrantRecheck) Execute(rbac.PresenceRecheckPlan)           {}
func (r *ambiguousGrantRecheck) Abandon(rbac.PresenceRecheckPlan, string) { r.abandoned++ }

// newTempGrantManager builds a tempGrantManager backed by the test DB/Redis/Hub.
func newTempGrantManager(t *testing.T, ts *testhelpers.TestServer) *voice.TestTempGrantManager {
	t.Helper()
	log := logger.New("test")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	return voice.NewTestTempGrantManager(ts.DB, log, ts.Hub, resolver, nil)
}

// tgOverride reads the (allow, deny, is_temporary, temporary_reason) of a user
// override row, plus whether the row exists.
func tgOverride(t *testing.T, db *sql.DB, channelID, userID string) (exists bool, allow, deny int64, isTemp bool, reason sql.NullString) {
	t.Helper()
	err := db.QueryRow(
		`SELECT allow, deny, is_temporary, temporary_reason
		 FROM channel_permission_overrides
		 WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`,
		channelID, userID,
	).Scan(&allow, &deny, &isTemp, &reason)
	if err == sql.ErrNoRows {
		return false, 0, 0, false, sql.NullString{}
	}
	require.NoError(t, err)
	return true, allow, deny, isTemp, reason
}

func tgSeedChannelKey(t *testing.T, db *sql.DB, channelID, userID string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, 'wk', 1)`,
		channelID, userID,
	)
	require.NoError(t, err)
}

func tgSeedPendingKeyRequest(t *testing.T, db *sql.DB, channelID, userID string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO pending_key_requests (channel_id, user_id) VALUES ($1, $2)`,
		channelID, userID,
	)
	require.NoError(t, err)
}

func tgChannelKeyExists(t *testing.T, db *sql.DB, channelID, userID string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM channel_keys WHERE channel_id = $1 AND user_id = $2)`,
		channelID, userID,
	).Scan(&exists))
	return exists
}

func tgPendingKeyRequestExists(t *testing.T, db *sql.DB, channelID, userID string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2)`,
		channelID, userID,
	).Scan(&exists))
	return exists
}

func tgKeyRevocationCount(t *testing.T, db *sql.DB, channelID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1`, channelID).Scan(&n))
	return n
}

// tgLatestRevokedBy returns the revoked_by of the most recent key_revocations row
// for the channel (NullString.Valid == false means SQL NULL).
func tgLatestRevokedBy(t *testing.T, db *sql.DB, channelID string) sql.NullString {
	t.Helper()
	var revokedBy sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT revoked_by FROM key_revocations WHERE channel_id = $1 ORDER BY revoked_epoch DESC LIMIT 1`,
		channelID,
	).Scan(&revokedBy))
	return revokedBy
}

// --- Grant tests (#487 Scope C grant / T5) ---

func TestGrantTemporaryChannelAccess_RejectsInvalidUserBeforeDatabaseAccess(t *testing.T) {
	// Validation is deliberately first: malformed caller input must not reach a
	// transaction or dereference the manager's database dependencies.
	mgr := voice.NewTestTempGrantManager(nil, nil, nil, nil, nil)

	err := mgr.Grant(context.Background(), "server", "channel", "not-a-uuid")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid temporary grant user")
}

func TestGrantTemporaryChannelAccess_BeginFailureIsReturned(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://invalid-host.invalid/concord")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	mgr := voice.NewTestTempGrantManager(db, logger.New("test"), nil, nil, nil)

	err = mgr.Grant(context.Background(), uuid.NewString(), uuid.NewString(), uuid.NewString())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "temp grant begin")
}

func TestGrantTemporaryChannelAccess_InsertsTempOverride(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_grant_owner")
	mover := ts.CreateTestUser(t, "tg_grant_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Insert")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-grant")

	err := mgr.Grant(context.Background(), serverID, channelID, mover.ID)
	require.NoError(t, err)

	exists, allow, deny, isTemp, reason := tgOverride(t, ts.DB, channelID, mover.ID)
	require.True(t, exists, "a temp override row should be inserted")
	assert.Equal(t, int64(voice.TempGrantAllow), allow, "allow mask must be exactly VIEW|CONNECT|SPEAK")
	assert.Equal(t, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice|rbac.PermSpeak), allow, "allow mask must be the exact three bits")
	assert.Equal(t, int64(0), deny, "deny must be 0")
	assert.True(t, isTemp, "is_temporary must be true")
	assert.True(t, reason.Valid)
	assert.Equal(t, "move_granted", reason.String)
}

func TestGrantTemporaryChannelAccess_AllowMaskExcludesManagementAndMessaging(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_mask_owner")
	mover := ts.CreateTestUser(t, "tg_mask_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Mask")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-mask")

	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))

	_, allow, _, _, _ := tgOverride(t, ts.DB, channelID, mover.ID)
	// SEND_MESSAGES and every management/moderation bit must be absent.
	assert.Equal(t, int64(0), allow&int64(rbac.PermSendMessages), "must NOT grant SEND_MESSAGES")
	assert.Equal(t, int64(0), allow&int64(rbac.PermMoveMembers), "must NOT grant MOVE_MEMBERS")
	assert.Equal(t, int64(0), allow&int64(rbac.PermMuteMembers), "must NOT grant MUTE_MEMBERS")
	assert.Equal(t, int64(0), allow&int64(rbac.PermManageRoles), "must NOT grant MANAGE_ROLES")
	assert.Equal(t, int64(0), allow&int64(rbac.PermAdministrator), "must NOT grant ADMINISTRATOR")
}

func TestGrantTemporaryChannelAccess_DoesNotDowngradePermanent(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_perm_owner")
	mover := ts.CreateTestUser(t, "tg_perm_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Permanent")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-perm")

	// Pre-existing PERMANENT override (is_temporary defaults to false) with a wider mask.
	permAllow := int64(rbac.PermViewVoiceChannels | rbac.PermJoinVoice | rbac.PermSpeak | rbac.PermSendMessages)
	ts.CreateChannelOverride(t, channelID, "user", mover.ID, permAllow, 0)

	err := mgr.Grant(context.Background(), serverID, channelID, mover.ID)
	require.NoError(t, err)

	exists, allow, _, isTemp, reason := tgOverride(t, ts.DB, channelID, mover.ID)
	require.True(t, exists)
	assert.False(t, isTemp, "permanent grant must NOT be flipped to temporary")
	assert.Equal(t, permAllow, allow, "permanent allow mask must be untouched (not downgraded)")
	assert.False(t, reason.Valid, "temporary_reason must remain NULL on the permanent row")
}

func TestGrantTemporaryChannelAccess_IdempotentOnExistingTemp(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_idem_owner")
	mover := ts.CreateTestUser(t, "tg_idem_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Idempotent")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-idem")

	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`,
		channelID, mover.ID,
	).Scan(&count))
	assert.Equal(t, 1, count, "repeat grant must remain a single row (idempotent)")

	_, allow, _, isTemp, _ := tgOverride(t, ts.DB, channelID, mover.ID)
	assert.Equal(t, int64(voice.TempGrantAllow), allow)
	assert.True(t, isTemp)
}

func TestReconcileAmbiguousTemporaryGrant_ExactFenceRevokesCommittedUnsignaledGrant(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_ambiguous_owner")
	mover := ts.CreateTestUser(t, "tg_ambiguous_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Ambiguous")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-ambiguous")

	ackLoss := errors.New("temporary grant commit acknowledgement lost")
	mgr.SetGrantCommitForTest(func(tx *sql.Tx) error {
		require.NoError(t, tx.Commit()) // The durable grant is real.
		return ackLoss                  // Only its acknowledgement is lost.
	})

	err := mgr.Grant(context.Background(), serverID, channelID, mover.ID)
	require.ErrorIs(t, err, ackLoss)
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, mover.ID))
	assert.Equal(t, 1, tgKeyRevocationCount(t, ts.DB, channelID))
}

func TestGrantTemporaryChannelAccess_RollbackCommitErrorDoesNotMutate(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_ambiguous_rollback_owner")
	mover := ts.CreateTestUser(t, "tg_ambiguous_rollback_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Ambiguous Rollback")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-ambiguous-rollback")

	commitRejected := errors.New("temporary grant commit rejected")
	mgr.SetGrantCommitForTest(func(*sql.Tx) error { return commitRejected })
	err := mgr.Grant(context.Background(), serverID, channelID, mover.ID)
	require.ErrorIs(t, err, commitRejected)
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, mover.ID))
	assert.Zero(t, tgKeyRevocationCount(t, ts.DB, channelID))
}

func TestGrantTemporaryChannelAccess_RollbackErrorInvalidatesAuthority(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_rollback_error_owner")
	mover := ts.CreateTestUser(t, "tg_rollback_error_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Rollback Error")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-rollback-error")

	cacheKey := "perm:" + serverID + ":" + mover.ID + ":" + channelID
	require.NoError(t, ts.Redis.Set(context.Background(), cacheKey, int64(rbac.PermJoinVoice), time.Minute).Err())
	commitErr := errors.New("temporary grant commit acknowledgement lost")
	rollbackErr := errors.New("temporary grant rollback outcome unresolved")
	mgr.SetGrantCommitForTest(func(*sql.Tx) error { return commitErr })
	mgr.SetGrantRollbackForTest(func(tx *sql.Tx) error {
		if err := tx.Rollback(); err != nil {
			return err
		}
		return rollbackErr
	})

	err := mgr.Grant(context.Background(), serverID, channelID, mover.ID)
	require.ErrorIs(t, err, commitErr)
	require.ErrorIs(t, err, rollbackErr)
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, mover.ID))
	assert.Zero(t, ts.Redis.Exists(context.Background(), cacheKey).Val(), "unresolved rollback must fail closed by invalidating cached authority")
}

func TestGrantTemporaryChannelAccess_ConfirmedCommitSurvivesCallerCancellation(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg-confirmed-cancel-owner")
	mover := ts.CreateTestUser(t, "tg-confirmed-cancel-mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Confirmed Cancellation")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-confirmed-cancel")

	ctx, cancel := context.WithCancel(context.Background())
	mgr.SetGrantCommitForTest(func(tx *sql.Tx) error {
		err := tx.Commit()
		cancel() // Models client cancellation immediately after the durable commit.
		return err
	})
	t.Cleanup(cancel)

	require.NoError(t, mgr.Grant(ctx, serverID, channelID, mover.ID))
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, mover.ID), "post-commit cache delivery is detached best effort, never a false failed move")
}

func TestReconcileAmbiguousTemporaryGrant_ExpiredCallerDeadlineUsesFreshContext(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg-expired-deadline-owner")
	mover := ts.CreateTestUser(t, "tg-expired-deadline-mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Expired Deadline")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-expired-deadline")

	ackLoss := errors.New("temporary grant commit acknowledgement lost after deadline")
	ctx, cancel := context.WithCancel(context.Background())
	mgr.SetGrantCommitForTest(func(tx *sql.Tx) error {
		require.NoError(t, tx.Commit())
		cancel() // The caller deadline/cancellation is observed after durable commit.
		return ackLoss
	})
	t.Cleanup(cancel)

	err := mgr.Grant(ctx, serverID, channelID, mover.ID)
	require.ErrorIs(t, err, ackLoss)
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, mover.ID),
		"ambiguous compensation must reconcile even after the caller context expires")
	assert.Equal(t, 1, tgKeyRevocationCount(t, ts.DB, channelID))
}

func TestGrantTemporaryChannelAccess_GrantAgeStartsAfterVisibilityLockWait(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg-grace-lock-owner")
	mover := ts.CreateTestUser(t, "tg-grace-lock-mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Grace Lock")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-grace-lock")

	visibilityBlocker, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = visibilityBlocker.Rollback() })
	require.NoError(t, rbac.LockServerVisibilityCapture(context.Background(), visibilityBlocker, serverID))
	lockKey, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)
	lifecycleBlocker, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycleBlocker.Rollback() })
	userID := uuid.MustParse(mover.ID)
	lifecycleKey, err := voice.ServerVoiceLifecycleAdvisoryKeyForTest(userID)
	require.NoError(t, err)
	require.NoError(t, voice.LockServerVoiceLifecycleTx(context.Background(), lifecycleBlocker, userID))
	done := make(chan error, 1)
	go func() { done <- mgr.Grant(context.Background(), serverID, channelID, mover.ID) }()
	dbtest.WaitForAdvisoryLockWaiter(t, ts.DB, lockKey)
	require.NoError(t, visibilityBlocker.Commit())
	dbtest.WaitForAdvisoryLockWaiter(t, ts.DB, lifecycleKey)
	// Use the database clock at the final lock boundary. A timestamp captured
	// before either lock must be earlier than this value; production writes it
	// after the lifecycle lock, so no Go-clock skew or timing slack is involved.
	var releaseBoundary time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT clock_timestamp()`).Scan(&releaseBoundary))
	require.NoError(t, lifecycleBlocker.Commit())
	require.NoError(t, <-done)

	var grantedAt time.Time
	require.NoError(t, ts.DB.QueryRow(`
		SELECT granted_at
		FROM channel_permission_overrides
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`, channelID, mover.ID,
	).Scan(&grantedAt))
	assert.False(t, grantedAt.Before(releaseBoundary),
		"grant age must begin at the post-lock write, not transaction start")
}

func TestReconcileAmbiguousTemporaryGrant_CaptureFailureAbandonsPreparedPresencePlan(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg-ambiguous-abandon-owner")
	mover := ts.CreateTestUser(t, "tg-ambiguous-abandon-mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Ambiguous Abandon")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-ambiguous-abandon")
	recheck := &ambiguousGrantRecheck{}
	mgr.SetPresenceRecheckForTest(recheck)
	mgr.SetGrantCommitForTest(func(tx *sql.Tx) error {
		require.NoError(t, tx.Commit())
		return errors.New("commit acknowledgement lost")
	})

	require.Error(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	assert.Equal(t, 1, recheck.abandoned, "a prepared plan is fail-closed when ambiguity compensation cannot capture it")
}

func TestReconcileAmbiguousTemporaryGrant_NewerTupleWithSameGrantedAtSurvives(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_ambiguous_newer_owner")
	mover := ts.CreateTestUser(t, "tg_ambiguous_newer_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Ambiguous Newer")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-ambiguous-newer")

	ackLoss := errors.New("temporary grant commit acknowledgement lost after same-time refresh")
	mgr.SetGrantCommitForTest(func(tx *sql.Tx) error {
		require.NoError(t, tx.Commit())
		// NOW() is transaction-start time and therefore not an operation fence:
		// write a newer tuple while preserving granted_at exactly. Compensation
		// must use the returned xmin, not the audit timestamp or stable row id.
		_, refreshErr := ts.DB.Exec(`
			UPDATE channel_permission_overrides
			SET allow = allow
			WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2
			  AND is_temporary = TRUE`, channelID, mover.ID)
		require.NoError(t, refreshErr)
		return ackLoss
	})
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)
	tgSeedPendingKeyRequest(t, ts.DB, channelID, mover.ID)

	err := mgr.Grant(context.Background(), serverID, channelID, mover.ID)
	require.ErrorIs(t, err, ackLoss)
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, mover.ID), "an old xmin fence must not erase a newer tuple with the same granted_at")
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID))
	assert.True(t, tgPendingKeyRequestExists(t, ts.DB, channelID, mover.ID))
	assert.Zero(t, tgKeyRevocationCount(t, ts.DB, channelID))
}

func TestReconcileAmbiguousTemporaryGrant_PermanentOverrideSurvives(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_ambiguous_permanent_owner")
	mover := ts.CreateTestUser(t, "tg_ambiguous_permanent_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Ambiguous Permanent")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-ambiguous-permanent")

	permanentAllow := int64(rbac.PermViewVoiceChannels | rbac.PermJoinVoice | rbac.PermSpeak | rbac.PermSendMessages)
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)
	ackLoss := errors.New("temporary grant commit acknowledgement lost after promotion")
	mgr.SetGrantCommitForTest(func(tx *sql.Tx) error {
		require.NoError(t, tx.Commit())
		_, err := ts.DB.Exec(
			`UPDATE channel_permission_overrides
			 SET allow = $3, is_temporary = false, temporary_reason = NULL
			 WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`,
			channelID, mover.ID, permanentAllow,
		)
		require.NoError(t, err)
		return ackLoss
	})

	err := mgr.Grant(context.Background(), serverID, channelID, mover.ID)
	require.ErrorIs(t, err, ackLoss)
	exists, allow, _, isTemp, reason := tgOverride(t, ts.DB, channelID, mover.ID)
	assert.True(t, exists)
	assert.False(t, isTemp, "the exact temporary fence must not erase a permanent override")
	assert.Equal(t, permanentAllow, allow)
	assert.False(t, reason.Valid)
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID))
	assert.Zero(t, tgKeyRevocationCount(t, ts.DB, channelID))
}

// TestGrantTemporaryChannelAccess_InsertError verifies the INSERT-failure branch:
// granting against a channel_id that does not exist violates the
// channel_permission_overrides FK (channel_id REFERENCES channels(id)). The grant
// must surface a wrapped "temp grant insert" error and write no override row.
func TestGrantTemporaryChannelAccess_InsertError(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_inserr_owner")
	mover := ts.CreateTestUser(t, "tg_inserr_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant InsertErr")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)

	// A well-formed UUID that is NOT a real channel → FK violation on INSERT.
	orphanChannel := "33333333-3333-3333-3333-333333333333"
	err := mgr.Grant(context.Background(), serverID, orphanChannel, mover.ID)
	require.Error(t, err, "grant against a non-existent channel must fail on the FK")
	assert.Contains(t, err.Error(), "temp grant insert")

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1 AND target_id = $2`,
		orphanChannel, mover.ID,
	).Scan(&count))
	assert.Equal(t, 0, count, "no override row should persist when the INSERT fails")
}

// --- Revoke tests (#487 P1 / T6) ---

func TestRevokeTemporaryChannelAccess_DeletesTempAndPurges(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_rev_owner")
	mover := ts.CreateTestUser(t, "tg_rev_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempRevoke Purge")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-rev")

	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)
	tgSeedPendingKeyRequest(t, ts.DB, channelID, mover.ID)

	err := mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem)
	require.NoError(t, err)

	exists, _, _, _, _ := tgOverride(t, ts.DB, channelID, mover.ID)
	assert.False(t, exists, "temp override must be deleted")
	assert.False(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID), "channel_keys must be purged")
	assert.False(t, tgPendingKeyRequestExists(t, ts.DB, channelID, mover.ID), "pending_key_requests must be purged")
	assert.Equal(t, 1, tgKeyRevocationCount(t, ts.DB, channelID), "CSK must be rotated (one key_revocations row)")

	// System-triggered revoke (actorID == "") must store revoked_by as SQL NULL,
	// not the literal empty string — otherwise the FK to users(id) is violated.
	revokedBy := tgLatestRevokedBy(t, ts.DB, channelID)
	assert.False(t, revokedBy.Valid, "actorless system revoke must store revoked_by as NULL")
}

func TestRevokeTemporaryChannelAccess_ChannelLockDeadlineRollsBackAtomically(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_atomic_lock_owner")
	mover := ts.CreateTestUser(t, "tg_atomic_lock_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempRevoke Atomic Lock")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-atomic-lock")
	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)
	tgSeedPendingKeyRequest(t, ts.DB, channelID, mover.ID)

	blocker, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	var transactionID int64
	require.NoError(t, blocker.QueryRow(`SELECT txid_current()`).Scan(&transactionID))
	_, err = blocker.Exec(`SELECT id FROM channels WHERE id = $1 FOR UPDATE`, channelID)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	type revokeResult struct{ err error }
	done := make(chan revokeResult, 1)
	go func() { done <- revokeResult{mgr.Revoke(ctx, serverID, channelID, mover.ID, tgActorSystem)} }()
	dbtest.WaitForRowLockWaiter(t, ts.DB, transactionID)
	select {
	case result := <-done:
		require.Error(t, result.err)
	case <-time.After(2 * time.Second):
		t.Fatal("temporary-grant revoke did not exit after the caller deadline")
	}
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, mover.ID))
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID))
	assert.True(t, tgPendingKeyRequestExists(t, ts.DB, channelID, mover.ID))
	assert.Zero(t, tgKeyRevocationCount(t, ts.DB, channelID))
	require.NoError(t, blocker.Rollback())

	require.NoError(t, mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem))
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, mover.ID))
	assert.False(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID))
	assert.False(t, tgPendingKeyRequestExists(t, ts.DB, channelID, mover.ID))
	assert.Equal(t, 1, tgKeyRevocationCount(t, ts.DB, channelID))
}

func TestRevokeTemporaryChannelAccess_KeyPurgeFailureRollsBackAndRetries(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_atomic_purge_owner")
	mover := ts.CreateTestUser(t, "tg_atomic_purge_mover")
	serverID := ts.CreateTestServer(t, owner.ID, "TempRevoke Atomic Purge")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-atomic-purge")
	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)
	tgSeedPendingKeyRequest(t, ts.DB, channelID, mover.ID)
	_, err := ts.DB.Exec(`
		CREATE FUNCTION test_fail_temp_revoke_key_purge() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'forced temp revoke key purge failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_fail_temp_revoke_key_purge BEFORE DELETE ON channel_keys
		FOR EACH ROW EXECUTE FUNCTION test_fail_temp_revoke_key_purge()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := ts.DB.Exec(`
			DROP TRIGGER IF EXISTS test_fail_temp_revoke_key_purge ON channel_keys;
			DROP FUNCTION IF EXISTS test_fail_temp_revoke_key_purge()`)
		if cleanupErr != nil {
			t.Errorf("drop temp revoke key-purge failure trigger: %v", cleanupErr)
		}
	})

	err = mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem)
	require.Error(t, err)
	assert.True(t, tempOverrideExists(t, ts.DB, channelID, mover.ID))
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID))
	assert.True(t, tgPendingKeyRequestExists(t, ts.DB, channelID, mover.ID))
	assert.Zero(t, tgKeyRevocationCount(t, ts.DB, channelID))
	require.NoError(t, func() error {
		_, dropErr := ts.DB.Exec(`DROP TRIGGER test_fail_temp_revoke_key_purge ON channel_keys; DROP FUNCTION test_fail_temp_revoke_key_purge()`)
		return dropErr
	}())

	require.NoError(t, mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem))
	assert.False(t, tempOverrideExists(t, ts.DB, channelID, mover.ID))
	assert.False(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID))
	assert.False(t, tgPendingKeyRequestExists(t, ts.DB, channelID, mover.ID))
	assert.Equal(t, 1, tgKeyRevocationCount(t, ts.DB, channelID))
}

func TestRevokeTemporaryChannelAccess_DeletesOnlyTemporary(t *testing.T) {
	// SECURITY-CRITICAL: revoke must NOT touch a permanent override for the same
	// (channel, user). With ONLY a permanent grant present, revoke is a total NO-OP.
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_revperm_owner")
	mover := ts.CreateTestUser(t, "tg_revperm_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempRevoke Permanent")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-revperm")

	permAllow := int64(rbac.PermViewVoiceChannels | rbac.PermJoinVoice | rbac.PermSpeak)
	ts.CreateChannelOverride(t, channelID, "user", mover.ID, permAllow, 0)
	// Seed key material that must SURVIVE because no temp grant exists.
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)
	tgSeedPendingKeyRequest(t, ts.DB, channelID, mover.ID)

	err := mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem)
	require.NoError(t, err)

	exists, allow, _, isTemp, _ := tgOverride(t, ts.DB, channelID, mover.ID)
	require.True(t, exists, "permanent override must NOT be deleted")
	assert.False(t, isTemp)
	assert.Equal(t, permAllow, allow)
	// No purge, no rotation when only a permanent grant exists.
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID), "channel_keys must NOT be purged on no-op")
	assert.True(t, tgPendingKeyRequestExists(t, ts.DB, channelID, mover.ID), "pending_key_requests must NOT be purged on no-op")
	assert.Equal(t, 0, tgKeyRevocationCount(t, ts.DB, channelID), "no CSK rotation on a permanent-only no-op")
}

func TestRevokeTemporaryChannelAccess_NoGrantIsNoOp(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_revnone_owner")
	mover := ts.CreateTestUser(t, "tg_revnone_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempRevoke None")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-revnone")
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)

	err := mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem)
	require.NoError(t, err)

	// No temp grant present → no purge, no rotation.
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID), "channel_keys must survive when there is no temp grant")
	assert.Equal(t, 0, tgKeyRevocationCount(t, ts.DB, channelID))
}

// TestRevokeTemporaryChannelAccess_PermanentSupersedesTemp verifies the
// "permanent grant supersedes" lifecycle row: if a temp grant was flipped to
// permanent (is_temporary cleared), the revoke's is_temporary guard finds nothing
// to delete → total no-op (no purge, no rotation). Correct by construction.
func TestRevokeTemporaryChannelAccess_PermanentSupersedesTemp(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_super_owner")
	mover := ts.CreateTestUser(t, "tg_super_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempRevoke Supersede")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-super")

	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	// Simulate the temp grant being promoted to permanent.
	_, err := ts.DB.Exec(
		`UPDATE channel_permission_overrides SET is_temporary = false, temporary_reason = NULL
		 WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`,
		channelID, mover.ID)
	require.NoError(t, err)
	tgSeedChannelKey(t, ts.DB, channelID, mover.ID)

	require.NoError(t, mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem))

	exists, _, _, isTemp, _ := tgOverride(t, ts.DB, channelID, mover.ID)
	require.True(t, exists, "promoted permanent override must survive")
	assert.False(t, isTemp)
	assert.True(t, tgChannelKeyExists(t, ts.DB, channelID, mover.ID), "no purge on promoted-permanent no-op")
	assert.Equal(t, 0, tgKeyRevocationCount(t, ts.DB, channelID))
}

func TestHasTemporaryGrant(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	owner := ts.CreateTestUser(t, "tg_has_owner")
	mover := ts.CreateTestUser(t, "tg_has_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Has")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-tg-has")

	has, err := mgr.HasTemporaryGrant(context.Background(), channelID, mover.ID)
	require.NoError(t, err)
	assert.False(t, has, "no grant yet")

	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	has, err = mgr.HasTemporaryGrant(context.Background(), channelID, mover.ID)
	require.NoError(t, err)
	assert.True(t, has, "temp grant should be detected")

	// A permanent override is not a temporary grant.
	other := ts.CreateTestUser(t, "tg_has_perm")
	ts.AddMemberToServer(t, serverID, other.ID, roleMember)
	ts.CreateChannelOverride(t, channelID, "user", other.ID, int64(rbac.PermViewVoiceChannels), 0)
	has, err = mgr.HasTemporaryGrant(context.Background(), channelID, other.ID)
	require.NoError(t, err)
	assert.False(t, has, "permanent override must NOT count as a temporary grant")
}

// TestRevokeThenVisible verifies the integration with GetVisibleChannelIDs (T7
// premise): a temp-granted hidden voice channel becomes visible, and after revoke
// it disappears again.
func TestGrantMakesHiddenVoiceChannelVisible(t *testing.T) {
	ts := setupTS(t)
	mgr := newTempGrantManager(t, ts)
	log := logger.New("test")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)

	owner := ts.CreateTestUser(t, "tg_vis_owner")
	mover := ts.CreateTestUser(t, "tg_vis_target")
	serverID := ts.CreateTestServer(t, owner.ID, "TempGrant Visible")
	ts.AddMemberToServer(t, serverID, mover.ID, roleMember)

	// Hidden voice channel: deny VIEW_VOICE for the @all role so the member can't see it.
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-hidden")
	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID))
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))

	visible, err := resolver.GetVisibleChannelIDs(context.Background(), serverID, mover.ID)
	require.NoError(t, err)
	assert.NotContains(t, visible, channelID, "channel should be hidden before grant")

	require.NoError(t, mgr.Grant(context.Background(), serverID, channelID, mover.ID))
	visible, err = resolver.GetVisibleChannelIDs(context.Background(), serverID, mover.ID)
	require.NoError(t, err)
	assert.Contains(t, visible, channelID, "temp grant (user-allow VIEW_VOICE) must surface the hidden channel")

	require.NoError(t, mgr.Revoke(context.Background(), serverID, channelID, mover.ID, tgActorSystem))
	visible, err = resolver.GetVisibleChannelIDs(context.Background(), serverID, mover.ID)
	require.NoError(t, err)
	assert.NotContains(t, visible, channelID, "channel should be hidden again after revoke")
}
