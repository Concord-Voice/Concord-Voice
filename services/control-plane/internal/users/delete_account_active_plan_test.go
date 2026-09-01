// services/control-plane/internal/users/delete_account_active_plan_test.go

package users

import (
	"context"
	"database/sql"
	"errors"
	"go/build"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// gatedErasureBound bounds DeleteAccount when the rail is wired. It is the ONLY
// assertion that can see a gate re-entry: presencehistory's sender gate is a
// buffered-1 channel with no timeout and no detector, so a drain that reached
// Rail.WithGatedTx instead of Rail.DrainAlreadyGated would block forever rather
// than fail. `go test -race` cannot see that class -- wall clock can.
const gatedErasureBound = 15 * time.Second

// Without a drain the FK RESTRICT turns an erasure into an opaque 23503. This
// test is what keeps the RESTRICT meaningful rather than merely obstructive: if
// it ever stops failing, migration 000111 has been softened to CASCADE and the
// drain below has quietly become decoration.
func TestDeleteUserWithoutDrainRaisesForeignKeyViolation(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "private_call")

	_, err := db.Exec(`DELETE FROM users WHERE id = $1`, userID)

	require.Error(t, err)
	require.Contains(t, err.Error(), "presence_active_pending_plans")
}

// The erasure succeeds, the row is drained, and the obligation is transferred to
// exactly one clear frame per drained category.
func TestDeleteAccountDrainsPlansAndTransfersTheObligation(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "private_call")
	insertPlanRow(t, db, userID, "server_voice")
	service, deliverer := newAccountServiceWithRail(t, db)

	require.NoError(t, service.DeleteAccount(context.Background(), userID.String()))

	assert.Zero(t, countUsers(t, db, userID))
	assert.Zero(t, countPlansForUser(t, db, userID))
	assert.ElementsMatch(t,
		[]presence.Category{presence.CategoryPrivateCall, presence.CategoryServerVoice},
		deliverer.categoriesFor(userID))
}

// A presence delivery failure must NEVER fail the erasure. The account is gone;
// only the frame did not settle. The seam enforces this structurally --
// ClearDrained returns nothing -- so this test pins the SHAPE: an implementer
// who widens it to return an error and propagates that error fails here.
func TestDeleteAccountSucceedsWhenTheClearFails(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "private_call")
	service, deliverer := newAccountServiceWithRail(t, db)
	deliverer.failEveryClear = true

	require.NoError(t, service.DeleteAccount(context.Background(), userID.String()))

	assert.Zero(t, countUsers(t, db, userID))
	assert.Equal(t, 1, deliverer.failureCount())
}

// The drain runs BEFORE DELETE FROM users, so a drain failure fails the erasure
// with a diagnosable error instead of Postgres's opaque 23503 -- migration
// 000110's fail-closed-with-a-diagnosable-error precedent. Move the drain after
// the DELETE and the surfaced error becomes the constraint violation.
func TestDeleteAccountDrainFailureRetainsUserWithDiagnosableError(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "private_call")
	drainErr := errors.New("forced active-category drain failure")
	drain := &stubActivePlanDrain{drainErr: drainErr}
	service := newAccountServiceWithDrain(t, db, drain)

	err := service.DeleteAccount(context.Background(), userID.String())

	require.ErrorIs(t, err, drainErr)
	require.Contains(t, err.Error(), "drain presence plans")
	require.NotContains(t, err.Error(), "23503",
		"an erasure must not fail on a raw foreign-key violation")
	assert.Equal(t, 1, countUsers(t, db, userID))
	assert.Equal(t, 1, countPlansForUser(t, db, userID))
	assert.Zero(t, drain.clearCount(),
		"a rolled-back erasure owes no clear frame")
}

// The transfer is POST-commit. The probe runs inside the clear frame and asks
// the database, on its own connection, whether the user is already gone. Hoist
// the transfer into the transaction and the probe either sees the user alive or
// blocks on the FOR UPDATE lock the erasure still holds -- both fail here, and
// the bounded probe context keeps the second one a failure rather than a hang.
func TestDeleteAccountTransfersTheObligationAfterCommit(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "server_voice")
	service, deliverer := newAccountServiceWithRail(t, db)
	var probed int
	var probeErr error
	deliverer.onClear = func(subject uuid.UUID, category presence.Category) {
		if subject != userID || category != presence.CategoryServerVoice {
			return
		}
		ctx, cancelProbe := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelProbe()
		probeErr = db.QueryRowContext(ctx,
			`SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&probed)
	}

	require.NoError(t, service.DeleteAccount(context.Background(), userID.String()))

	require.NoError(t, probeErr, "the clear frame ran while the erasure still held its locks")
	assert.Zero(t, probed, "the clear frame ran before the erasure committed")
}

// The drain runs inside a sender gate the caller ALREADY holds, and the rail
// shares that exact gate. Re-entering it deadlocks with no timeout and no
// detector, so the bound is the assertion.
func TestDeleteAccountWithRailCompletesInsideTheHeldSenderGate(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	insertPlanRow(t, db, userID, "private_call")
	service, _ := newAccountServiceWithRail(t, db)

	done := make(chan error, 1)
	go func() { done <- service.DeleteAccount(context.Background(), userID.String()) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(gatedErasureBound):
		t.Fatalf("erasure did not complete within %s: the drain re-entered the sender gate",
			gatedErasureBound)
	}
}

// This is the mandatory pre-fix oracle for #2901. The creator clear is a
// positive control: it proves the rail, creator drain, commit, and observer
// path all ran. The named survivor assertion is the defect: the surviving
// caller has no committed private-call plan/clear before the fix.
func TestDeleteAccountCapturesSurvivorPlansBeforeCreatorCascade(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator, survivor := testdb.CreateUser(t, db), testdb.CreateUser(t, db)
	conv := seedCreatorConversation(t, db, creator, survivor, true)
	insertPlanRow(t, db, creator, "private_call")
	service, deliverer := newAccountServiceWithRail(t, db)
	var survivorPlan, creatorRows, conversationRows int
	var probeErr error
	deliverer.onClear = func(subject uuid.UUID, category presence.Category) {
		if subject != survivor || category != presence.CategoryPrivateCall {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		probeErr = db.QueryRowContext(ctx, `SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1 AND category = 'private_call'`, survivor).Scan(&survivorPlan)
		if probeErr == nil {
			probeErr = db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE id = $1`, creator).Scan(&creatorRows)
		}
		if probeErr == nil {
			probeErr = db.QueryRowContext(ctx, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conv).Scan(&conversationRows)
		}
	}

	require.NoError(t, service.DeleteAccount(context.Background(), creator.String()))
	assert.Zero(t, countUsers(t, db, creator), "positive control: creator erasure committed")
	assert.Zero(t, countConversation(t, db, conv), "positive control: creator cascade committed")
	assert.Contains(t, deliverer.categoriesFor(creator), presence.CategoryPrivateCall,
		"positive control: creator clear/delivery observer ran")

	assert.Contains(t, deliverer.categoriesFor(survivor), presence.CategoryPrivateCall,
		"DEFECT #2901: survivor private-call plan/clear was not committed before creator cascade")
	require.NoError(t, probeErr)
	assert.Equal(t, 1, survivorPlan, "survivor plan must be committed before its clear delivery")
	assert.Zero(t, creatorRows, "creator must be absent before survivor clear delivery")
	assert.Zero(t, conversationRows, "creator conversation must be absent before survivor clear delivery")
}

func TestDeleteAccountCandidateShrinkProceeds(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator, survivor := testdb.CreateUser(t, db), testdb.CreateUser(t, db)
	conv := seedCreatorConversation(t, db, creator, survivor, true)
	hookRan := false
	suppressor := &replayActivitySettingsSuppressor{accountHook: func() {
		hookRan = true
		_, err := db.ExecContext(context.Background(), `DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, conv, survivor)
		require.NoError(t, err)
	}}
	drain := &stubActivePlanDrain{}
	service := configuredAccountServiceWithDrain(t, db, presencehistory.NewService(db, presencehistory.DisclosureState{}, false), drain)
	service.activityCleanup.activitySuppressor = suppressor
	err := service.DeleteAccount(context.Background(), creator.String())
	assert.True(t, hookRan, "candidate-shrink hook must run")
	assert.Empty(t, drain.capturedSubjects(), "shrink must not capture a removed active-call survivor")
	assert.Empty(t, drain.completedSubjects(), "shrink must not complete a removed active-call survivor")
	require.NoError(t, err, "candidate shrink must not be treated as drift")
	assert.Zero(t, countUsers(t, db, creator))
	assert.Zero(t, countConversation(t, db, conv))
}

func TestDeleteAccountCapturesCreatorOwnedOneToOneConversation(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator, survivor := testdb.CreateUser(t, db), testdb.CreateUser(t, db)
	conv := seedCreatorConversation(t, db, creator, survivor, false)
	service, deliverer := newAccountServiceWithRail(t, db)
	require.NoError(t, service.DeleteAccount(context.Background(), creator.String()))
	assert.Zero(t, countConversation(t, db, conv), "creator-owned 1:1 conversation must cascade")
	assert.Contains(t, deliverer.categoriesFor(survivor), presence.CategoryPrivateCall,
		"DEFECT #2901: 1:1 survivor plan/clear evidence is absent")
}

func TestDeleteAccountCandidateGrowthFailsClosed(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator, survivor := testdb.CreateUser(t, db), testdb.CreateUser(t, db)
	conv := seedCreatorConversation(t, db, creator, survivor, true)
	suppressor := &replayActivitySettingsSuppressor{accountHook: func() {
		late := testdb.CreateUser(t, db)
		_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conv, late)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`, conv, late)
		require.NoError(t, err)
	}}
	service := configuredAccountServiceWithDrain(t, db, presencehistory.NewService(db, presencehistory.DisclosureState{}, false), &stubActivePlanDrain{})
	service.activityCleanup.activitySuppressor = suppressor
	err := service.DeleteAccount(context.Background(), creator.String())
	require.ErrorIs(t, err, ErrErasureCandidateSetDrifted, "candidate growth must fail closed with the real sentinel")
	assert.Equal(t, 1, countUsers(t, db, creator), "creator must remain after candidate growth")
	assert.Equal(t, 1, countConversation(t, db, conv), "conversation must remain after candidate growth")
}

func TestDeleteAccountCandidateGrowthBeyondBoundFailsClosed(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator := testdb.CreateUser(t, db)
	for i := 0; i < 16; i++ {
		survivor := testdb.CreateUser(t, db)
		seedCreatorConversation(t, db, creator, survivor, true)
	}
	suppressor := &replayActivitySettingsSuppressor{accountHook: func() {
		late := testdb.CreateUser(t, db)
		_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id)
			SELECT id, $2 FROM dm_conversations WHERE created_by = $1 LIMIT 1`, creator, late)
		require.NoError(t, err)
		var conversationID uuid.UUID
		require.NoError(t, db.QueryRow(`SELECT id FROM dm_conversations WHERE created_by = $1 LIMIT 1`, creator).Scan(&conversationID))
		_, err = db.Exec(`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`, conversationID, late)
		require.NoError(t, err)
	}}
	service := configuredAccountServiceWithDrain(t, db, presencehistory.NewService(db, presencehistory.DisclosureState{}, false), &stubActivePlanDrain{})
	service.activityCleanup.activitySuppressor = suppressor
	err := service.DeleteAccount(context.Background(), creator.String())
	require.ErrorIs(t, err, ErrErasureCandidateSetDrifted)
	assert.Equal(t, 1, countUsers(t, db, creator), "overflow drift must not erase creator")
	var conversationCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_conversations WHERE created_by = $1`, creator).Scan(&conversationCount))
	assert.Equal(t, 16, conversationCount, "overflow drift must not cascade conversations")
}

func TestDeleteAccountKeepsAudienceFenceOpenThroughPlanCompletion(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator, survivor := testdb.CreateUser(t, db), testdb.CreateUser(t, db)
	seedCreatorConversation(t, db, creator, survivor, true)
	hub := websocket.NewHub(nil, nil)
	var openDuringCompletion uint64
	drain := &stubActivePlanDrain{completionHook: func() {
		openDuringCompletion = hub.PresenceAuthzOpenForTest()
	}}
	service := newAccountServiceWithDrain(t, db, drain)
	service.SetAudienceFence(hub)

	require.NoError(t, service.DeleteAccount(context.Background(), creator.String()))
	assert.NotZero(t, openDuringCompletion, "audience revocation must remain open through plan completion")
	assert.Zero(t, hub.PresenceAuthzOpenForTest(), "audience revocation must close after erasure")
}

func TestDeleteAccountRejectsSeventeenSurvivorSubjects(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator := testdb.CreateUser(t, db)
	for i := 0; i < 17; i++ {
		survivor := testdb.CreateUser(t, db)
		seedCreatorConversation(t, db, creator, survivor, true)
	}
	service, _ := newAccountServiceWithRail(t, db)
	err := service.DeleteAccount(context.Background(), creator.String())
	assert.Error(t, err, "seventeen survivors must exceed the bounded fan-out")
	assert.Equal(t, 1, countUsers(t, db, creator), "bound rejection must not mutate creator")
}

func TestDeleteAccountCaptureFailureRetainsCreatorAndConversations(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator, survivor := testdb.CreateUser(t, db), testdb.CreateUser(t, db)
	conv := seedCreatorConversation(t, db, creator, survivor, true)
	drain := &stubActivePlanDrain{captureErr: errors.New("capture sentinel")}
	service := newAccountServiceWithDrain(t, db, drain)
	var reclaimed bool
	service.SetErasedMediaReclaimer(func(context.Context, []media.BlobRef, []media.BlobRef) { reclaimed = true })
	var auditBefore, auditAfter int
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT count(*) FROM account_deletions`).Scan(&auditBefore))
	err := service.DeleteAccount(context.Background(), creator.String())
	assert.ErrorIs(t, err, drain.captureErr, "capture failure must be returned")
	assert.Equal(t, 1, countUsers(t, db, creator))
	assert.Equal(t, 1, countConversation(t, db, conv))
	assert.Zero(t, countPlansForUser(t, db, survivor), "capture failure must commit no plan")
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT count(*) FROM account_deletions`).Scan(&auditAfter))
	assert.Equal(t, auditBefore, auditAfter, "capture failure must write no audit row")
	assert.False(t, reclaimed, "capture failure must not run media cleanup")
}

func TestDeleteAccountSurvivorCompletionFailureStillRunsPostCommitObligations(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator, survivor := testdb.CreateUser(t, db), testdb.CreateUser(t, db)
	seedCreatorConversation(t, db, creator, survivor, true)
	drain := &stubActivePlanDrain{
		completionErr:     errors.New("completion sentinel"),
		drainedCategories: []presence.Category{presence.CategoryPrivateCall},
	}
	service := newAccountServiceWithDrain(t, db, drain)
	err := service.DeleteAccount(context.Background(), creator.String())
	assert.ErrorIs(t, err, drain.completionErr, "post-commit completion failure must surface")
	assert.Zero(t, countUsers(t, db, creator), "completion failure occurs after commit")
	assert.Equal(t, 1, drain.clearCount(), "existing post-commit drain obligation must run after completion error")
}

func TestDeleteAccountLocksSurvivorUsersBeforeCreatorConversations(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator, survivor := testdb.CreateUser(t, db), testdb.CreateUser(t, db)
	seedCreatorConversation(t, db, creator, survivor, true)
	drain := &stubActivePlanDrain{captureHook: func(ctx context.Context, tx *sql.Tx, _ []uuid.UUID) error {
		var usersLock, conversationsLock bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = pg_backend_pid() AND locktype = 'relation' AND relation = 'users'::regclass AND mode IN ('RowShareLock', 'RowExclusiveLock'))`).Scan(&usersLock)
		assert.NoError(t, err)
		err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = pg_backend_pid() AND locktype = 'relation' AND relation = 'dm_conversations'::regclass AND mode IN ('RowShareLock', 'RowExclusiveLock'))`).Scan(&conversationsLock)
		assert.NoError(t, err)
		assert.True(t, usersLock, "survivor users lock must be held at capture")
		assert.True(t, conversationsLock, "creator conversation lock must be held at capture")
		return nil
	}}
	service := newAccountServiceWithDrain(t, db, drain)
	assert.NoError(t, service.DeleteAccount(context.Background(), creator.String()))
	assert.True(t, drain.captureCalled, "capture seam must observe lock order")
}

func TestDeleteAccountDoesNotDeadlockAgainstDMMessageEditOrder(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator, survivor := testdb.CreateUser(t, db), testdb.CreateUser(t, db)
	conv := seedCreatorConversation(t, db, creator, survivor, true)
	drain := &stubActivePlanDrain{}
	service := newAccountServiceWithDrain(t, db, drain)
	started := make(chan struct{})
	suppressor := service.activityCleanup.activitySuppressor.(*replayActivitySettingsSuppressor)
	suppressor.accountHook = func() { close(started) }
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	editor, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := editor.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("editor rollback: %v", rollbackErr)
		}
	}()
	var locked uuid.UUID
	require.NoError(t, editor.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR SHARE`, survivor).Scan(&locked))
	var editorTxID int64
	require.NoError(t, editor.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&editorTxID))
	probe, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() {
		if closeErr := probe.Close(); closeErr != nil {
			t.Errorf("close lock probe: %v", closeErr)
		}
	})
	done := make(chan error, 1)
	go func() { done <- service.DeleteAccount(ctx, creator.String()) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("erasure did not reach the staged contention point")
	}
	testdb.WaitForRowLockWaiter(t, probe, editorTxID)
	assert.False(t, drain.captureWasCalled(), "capture must not run while erasure waits on survivor user")
	var waitingPID int64
	require.NoError(t, probe.QueryRowContext(ctx, `SELECT pid FROM pg_locks WHERE NOT granted AND locktype = 'transactionid' AND transactionid::text::bigint = $1 LIMIT 1`, testdb.TransactionIDForLockProbe(editorTxID)).Scan(&waitingPID))
	var conversationLocks int
	require.NoError(t, probe.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE pid = $1 AND relation = 'dm_conversations'::regclass AND mode IN ('RowShareLock', 'RowExclusiveLock')`, waitingPID).Scan(&conversationLocks))
	assert.Zero(t, conversationLocks, "erasure must wait on survivor users before taking creator conversation lock")
	require.NoError(t, editor.QueryRowContext(ctx, `SELECT id FROM dm_conversations WHERE id = $1 FOR UPDATE`, conv).Scan(&conv))
	require.NoError(t, editor.Commit())
	select {
	case err := <-done:
		if err != nil {
			assert.NotContains(t, err.Error(), "40P01", "erasure must not deadlock with DM edit order")
		}
	case <-time.After(gatedErasureBound):
		t.Fatal("erasure exceeded bounded liveness window")
	}
	assert.True(t, drain.captureWasCalled(), "capture must run after survivor user lock is released")
}

// The drain seam is declared at the consumer so this package depends on the two
// methods it uses, not on the rail. Without that the erasure path would carry an
// internal/activepresence import purely for a type name.
func TestUsersPackageGainsNoActivePresenceImport(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	require.NoError(t, err)

	for _, imported := range pkg.Imports {
		require.False(t,
			strings.HasSuffix(imported, "/internal/activepresence"),
			"internal/users must reach the rail through its own ActivePlanDrain interface")
	}
}

// --- fixtures -------------------------------------------------------------

func insertPlanRow(t *testing.T, db *sql.DB, userID uuid.UUID, category string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO presence_active_pending_plans (
			user_id, category, operation_id, resolution, scope_event_at
		) VALUES ($1, $2, $3, 'conservative', now())`,
		userID, category, uuid.New())
	require.NoError(t, err)
}

func countUsers(t *testing.T, db *sql.DB, userID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&count))
	return count
}

// Scoped to one subject on purpose: concurrent sessions share this database, so
// a global count would make the assertion someone else's flake.
func countPlansForUser(t *testing.T, db *sql.DB, userID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1`,
		userID).Scan(&count))
	return count
}

func countConversation(t *testing.T, db *sql.DB, conversationID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID).Scan(&count))
	return count
}

func TestDeleteAccountDoesNotDeadlockAgainstFreshPrivateVoiceIngressFKOrder(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	creator, survivor := testdb.CreateUser(t, db), testdb.CreateUser(t, db)
	conv := seedCreatorConversation(t, db, creator, survivor, true)
	service, _ := newAccountServiceWithRail(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), gatedErasureBound)
	defer cancel()
	ingressTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := ingressTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback ingress transaction: %v", rollbackErr)
		}
	}()
	require.NoError(t, dm.LockPrivateVoiceScopesTx(ctx, ingressTx, []uuid.UUID{survivor}))
	require.NoError(t, dm.LockDMVoiceParticipantSetTx(ctx, ingressTx, conv))
	var lockedMember uuid.UUID
	require.NoError(t, ingressTx.QueryRowContext(ctx, `
		SELECT user_id FROM dm_participants
		WHERE conversation_id = $1 AND user_id = $2 FOR KEY SHARE`, conv, survivor).Scan(&lockedMember))
	lockKey, err := dm.PrivateVoiceScopeAdvisoryKey(survivor)
	require.NoError(t, err)

	erasureDone := make(chan error, 1)
	go func() { erasureDone <- service.DeleteAccount(ctx, creator.String()) }()
	testdb.WaitForAdvisoryLockWaiter(t, db, lockKey)

	_, insertErr := ingressTx.ExecContext(ctx, `
		INSERT INTO dm_voice_participants (conversation_id, user_id, lifecycle_event_at)
		VALUES ($1, $2, now())`, conv, creator)
	require.NoError(t, insertErr, "fresh voice ingress must not deadlock against account erasure")
	require.NoError(t, ingressTx.Commit())

	select {
	case erasureErr := <-erasureDone:
		require.NoError(t, erasureErr)
	case <-time.After(gatedErasureBound):
		t.Fatalf("account erasure did not complete within %s", gatedErasureBound)
	}
}

func seedCreatorConversation(t *testing.T, db *sql.DB, creator, survivor uuid.UUID, group bool) uuid.UUID {
	t.Helper()
	conversationID := uuid.New()
	_, err := db.ExecContext(context.Background(), `INSERT INTO dm_conversations (id, is_group, created_by) VALUES ($1, $2, $3)`, conversationID, group, creator)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversationID, creator, survivor)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`, conversationID, survivor)
	require.NoError(t, err)
	return conversationID
}

// newAccountServiceWithRail wires a REAL *activepresence.Rail over the SAME
// presencehistory coordinator that gates DeleteAccount. Sharing the gate is what
// makes the wall-clock bound above meaningful: a rail with its own coordinator
// would take a different gate array and never reproduce the deadlock.
func newAccountServiceWithRail(
	t *testing.T,
	db *sql.DB,
) (*AccountService, *recordingActiveDeliverer) {
	t.Helper()
	coordinator := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	deliverer := &recordingActiveDeliverer{}
	rail := activepresence.NewRail(db, coordinator,
		activepresence.NewReconciler(db, coordinator, absentStateReader{}, noopGenerationDeleter{}, deliverer, nil), nil)
	return configuredAccountServiceWithDrain(t, db, coordinator, rail), deliverer
}

func newAccountServiceWithDrain(t *testing.T, db *sql.DB, drain ActivePlanDrain) *AccountService {
	t.Helper()
	coordinator := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	return configuredAccountServiceWithDrain(t, db, coordinator, drain)
}

func configuredAccountServiceWithDrain(
	t *testing.T,
	db *sql.DB,
	coordinator *presencehistory.Service,
	drain ActivePlanDrain,
) *AccountService {
	t.Helper()
	handler := &Handler{
		db:                 db,
		presenceHistory:    coordinator,
		activitySuppressor: &replayActivitySettingsSuppressor{},
	}
	service := NewAccountService(db, logger.New("test"))
	service.SetActivitySettingsCleanupHandler(handler)
	require.False(t, service.HasActivePlanDrain(),
		"an unwired service must report the drain missing, or the boot guard is a tautology")
	service.SetActivePlanRail(drain)
	require.True(t, service.HasActivePlanDrain())
	return service
}

// --- doubles --------------------------------------------------------------

// Keep the real rail's resolver on its ordinary clear arm: no live state is
// present and the captured plan is young, so delivery owes one clear frame.
type absentStateReader struct{}

func (absentStateReader) GetWithLease(context.Context, uuid.UUID, presence.Category) (presence.ActivityState, bool, error) {
	return presence.ActivityState{}, false, nil
}

type noopGenerationDeleter struct{}

func (noopGenerationDeleter) CompareAndDelete(context.Context, uuid.UUID, presence.Category, uuid.UUID, int64) (bool, error) {
	return true, nil
}

type clearedFrame struct {
	subject  uuid.UUID
	category presence.Category
}

// recordingActiveDeliverer is the rail's terminal. failEveryClear models a frame
// that never settled -- the terminal returns nothing, so a dropped frame is
// invisible to the erasure by construction, which is the property under test.
type recordingActiveDeliverer struct {
	mu             sync.Mutex
	cleared        []clearedFrame
	failures       int
	failEveryClear bool
	onClear        func(uuid.UUID, presence.Category)
}

func (d *recordingActiveDeliverer) ClearSenderActiveCategory(
	subject uuid.UUID,
	category presence.Category,
) {
	if d.onClear != nil {
		d.onClear(subject, category)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failEveryClear {
		d.failures++
		return
	}
	d.cleared = append(d.cleared, clearedFrame{subject: subject, category: category})
}

func (d *recordingActiveDeliverer) DisconnectAllRichPresenceClients(context.Context) error {
	return errors.New("the erasure drain must never reach the fleet-wide disconnect")
}

func (d *recordingActiveDeliverer) categoriesFor(subject uuid.UUID) []presence.Category {
	d.mu.Lock()
	defer d.mu.Unlock()
	categories := make([]presence.Category, 0, len(d.cleared))
	for _, frame := range d.cleared {
		if frame.subject == subject {
			categories = append(categories, frame.category)
		}
	}
	return categories
}

func (d *recordingActiveDeliverer) failureCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.failures
}

// stubActivePlanDrain injects a drain fault the real rail cannot be made to
// produce on demand.
type stubActivePlanDrain struct {
	mu                sync.Mutex
	drainErr          error
	drainedCategories []presence.Category
	captureErr        error
	completionErr     error
	completionHook    func()
	captureHook       func(context.Context, *sql.Tx, []uuid.UUID) error
	captured          []uuid.UUID
	completed         []uuid.UUID
	captureCalled     bool
	cleared           int
}

func (s *stubActivePlanDrain) DrainAlreadyGated(
	context.Context, *sql.Tx, uuid.UUID,
) ([]presence.Category, error) {
	return s.drainedCategories, s.drainErr
}

func (s *stubActivePlanDrain) ClearDrained(context.Context, uuid.UUID, []presence.Category) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleared++
}

func (s *stubActivePlanDrain) CapturePrivateCallPlansAlreadyGated(ctx context.Context, tx *sql.Tx, subjects []uuid.UUID) error {
	s.mu.Lock()
	s.captureCalled = true
	s.captured = append([]uuid.UUID(nil), subjects...)
	s.mu.Unlock()
	if s.captureHook != nil {
		if err := s.captureHook(ctx, tx, subjects); err != nil {
			return err
		}
	}
	return s.captureErr
}

func (s *stubActivePlanDrain) CompletePrivateCallPlansAlreadyGated(_ context.Context, subjects []uuid.UUID) error {
	s.mu.Lock()
	s.completed = append([]uuid.UUID(nil), subjects...)
	s.mu.Unlock()
	if s.completionHook != nil {
		s.completionHook()
	}
	return s.completionErr
}

func (s *stubActivePlanDrain) clearCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleared
}

func (s *stubActivePlanDrain) captureWasCalled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captureCalled
}

func (s *stubActivePlanDrain) capturedSubjects() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uuid.UUID(nil), s.captured...)
}

func (s *stubActivePlanDrain) completedSubjects() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uuid.UUID(nil), s.completed...)
}
