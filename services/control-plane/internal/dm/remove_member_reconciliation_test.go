package dm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func invokeRemoveMember(t *testing.T, h *Handler, convID, callerID, targetID string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("user_id", callerID)
	c.Params = gin.Params{{Key: "id", Value: convID}, {Key: "userId", Value: targetID}}
	c.Request = httptest.NewRequest("DELETE", "/", nil)
	h.RemoveMember(c)
	return recorder
}

func seedMemberRemovalFixture(t *testing.T, voice bool) (*sql.DB, string, uuid.UUID, uuid.UUID, *Handler, *recordingDeliverer) {
	t.Helper()
	db, _ := dbtest.SetupTestDB(t)
	convID, participants := seedGroupCallWithParticipants(t, db, 1)
	// The helper's creator is the first participant; recover it from the row.
	var creatorID string
	require.NoError(t, db.QueryRow(`SELECT created_by FROM dm_conversations WHERE id = $1`, convID).Scan(&creatorID))
	_, err := db.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, convID, creatorID)
	require.NoError(t, err)
	target := participants[0]
	if !voice {
		_, err := db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target)
		require.NoError(t, err)
	}
	h, deliverer := newDMHandlerWithRail(t, db, convID)
	h.hub = websocket.NewHub(db, nil)
	return db, convID, uuid.MustParse(creatorID), target, h, deliverer
}

func TestRemoveMemberCapturesPlanAndDeletesVoiceEvidence(t *testing.T) {
	db, convID, creator, target, h, deliverer := seedMemberRemovalFixture(t, true)
	r := invokeRemoveMember(t, h, convID, creator.String(), target.String())
	require.Equal(t, 200, r.Code)
	require.Equal(t, []uuid.UUID{target}, deliverer.subjectsCleared(),
		"active removal must deliver one clear for the removed target")
	require.Len(t, deliverer.clears, 1)
	require.Equal(t, presence.CategoryPrivateCall, deliverer.clears[0].category)
	require.Equal(t, 1, deliverer.clears[0].plansVisible,
		"the conservative plan must be committed before delivery")
	require.Equal(t, 1, deliverer.clears[0].conversationsLeft,
		"member removal must retain the conversation")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID))
}

func TestRemoveMemberWithoutActiveCallCapturesNothing(t *testing.T) {
	db, convID, creator, target, h, deliverer := seedMemberRemovalFixture(t, false)
	r := invokeRemoveMember(t, h, convID, creator.String(), target.String())
	require.Equal(t, 200, r.Code)
	require.Empty(t, deliverer.subjectsCleared())
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM presence_active_pending_plans`))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID))
}

func TestRemoveMemberLastCreatorReturnsConflict(t *testing.T) {
	db, convID, creator, successor, h, _ := seedMemberRemovalFixture(t, true)
	_, err := db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, convID, successor)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, successor)
	require.NoError(t, err)

	r := invokeRemoveMember(t, h, convID, creator.String(), creator.String())
	require.Equal(t, http.StatusConflict, r.Code,
		"removing the last participant must return retryable state drift, not an internal error")
	require.JSONEq(t, `{"error":"Failed to remove member"}`, r.Body.String())
	require.Equal(t, 1, countRows(t, db,
		`SELECT count(*) FROM dm_conversations WHERE id = $1`, convID))
	require.Equal(t, 1, countRows(t, db,
		`SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, creator))
}

type failingRemovalRail struct {
	db *sql.DB
	t  *testing.T
}

func (r failingRemovalRail) WithGatedTx(ctx context.Context, _ []uuid.UUID, work func(*sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			r.t.Errorf("rollback failing rail transaction: %v", rollbackErr)
		}
	}()
	return work(tx)
}
func (failingRemovalRail) CapturePlansTx(context.Context, *sql.Tx, []activepresence.Plan) error {
	return errors.New("capture failed")
}
func (failingRemovalRail) CompleteAlreadyGated(context.Context, *sql.Tx, []activepresence.PlanKey) error {
	return nil
}

func TestRemoveMemberCaptureFailureRollsBackBothRows(t *testing.T) {
	db, convID, creator, target, h, _ := seedMemberRemovalFixture(t, true)
	_, err := db.Exec(`INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`, convID, creator, []byte("key"))
	require.NoError(t, err)
	h.activePlans = failingRemovalRail{db: db, t: t}
	r := invokeRemoveMember(t, h, convID, creator.String(), target.String())
	require.Equal(t, 500, r.Code)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID))
	var createdBy uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT created_by FROM dm_conversations WHERE id = $1`, convID).Scan(&createdBy))
	require.Equal(t, creator, createdBy)
	require.Equal(t, 0, countRows(t, db, `SELECT count(*) FROM dm_key_revocations WHERE conversation_id = $1 AND reason = 'member_removed'`, convID))
}

func TestRemoveMemberCandidateGrowthReturnsConflict(t *testing.T) {
	db, convID, creator, target, h, _ := seedMemberRemovalFixture(t, false)
	// The hook is the deterministic window between the initial candidate read and
	// the row lock. The fixed handler inserts here; the pre-fix handler never
	// reads candidates and therefore removes the member instead.
	hookRan := false
	h.afterCandidateReadHook = func() {
		hookRan = true
		_, err := db.Exec(`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, target)
		require.NoError(t, err)
	}
	r := invokeRemoveMember(t, h, convID, creator.String(), target.String())
	require.True(t, hookRan, "candidate growth fixture must run after the initial candidate read")
	require.Equal(t, 409, r.Code)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target))
}

func TestRemoveMemberRevalidatesRevokedAdminInsideTransaction(t *testing.T) {
	db, convID, creator, target, h, _ := seedMemberRemovalFixture(t, false)
	h.afterCandidateReadHook = func() {
		_, err := db.Exec(`
			UPDATE dm_participants SET role = 'member'
			WHERE conversation_id = $1 AND user_id = $2`, convID, creator)
		require.NoError(t, err)
	}

	r := invokeRemoveMember(t, h, convID, creator.String(), target.String())
	require.Equal(t, http.StatusConflict, r.Code,
		"a role revoked after preflight must not authorize destructive work")
	require.Equal(t, 1, countRows(t, db,
		`SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
		convID, target))
}

type deliveryFailureRail struct {
	db *sql.DB
	t  *testing.T
}

func (r deliveryFailureRail) WithGatedTx(ctx context.Context, _ []uuid.UUID, work func(*sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			r.t.Errorf("rollback delivery rail transaction: %v", rollbackErr)
		}
	}()
	return work(tx)
}
func (deliveryFailureRail) CapturePlansTx(ctx context.Context, tx *sql.Tx, plans []activepresence.Plan) error {
	for _, p := range plans {
		if err := activepresence.InsertPlanTx(ctx, tx, p); err != nil {
			return err
		}
	}
	return nil
}
func (deliveryFailureRail) CompleteAlreadyGated(context.Context, *sql.Tx, []activepresence.PlanKey) error {
	return activepresence.ErrDeliveryIncomplete
}

func signedRemoveMemberWSToken(t *testing.T, userID uuid.UUID, jwtSecret string) string {
	t.Helper()
	signed, err := auth.GenerateAccessToken(userID.String(), jwtSecret, true, "", "")
	require.NoError(t, err)
	return signed
}

func TestRemoveMemberDeliveryFailureStillEnforcesAndBroadcasts(t *testing.T) {
	db, convID, creator, successor, h, _ := seedMemberRemovalFixture(t, true)
	target := creator
	_, err := db.Exec(`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, target)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`, convID, target, []byte("key"))
	require.NoError(t, err)

	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}
	bus, err := natsclient.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(bus.Close)
	enforcement := make(chan map[string]interface{}, 8)
	subscription, err := bus.Subscribe("voice.enforce.disconnect", func(data []byte) {
		var payload map[string]interface{}
		if decodeErr := json.Unmarshal(data, &payload); decodeErr != nil {
			t.Errorf("decode DM voice enforcement: %v", decodeErr)
			return
		}
		enforcement <- payload
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, subscription.Unsubscribe()) })
	require.NoError(t, bus.Flush())

	wsRedis := redistest.Client(t)
	hub := websocket.NewHub(db, wsRedis)
	go hub.Run()
	h.hub = hub
	h.nats = bus
	h.activePlans = deliveryFailureRail{db: db, t: t}
	jwtSecret := uuid.NewString()
	wsHandler := websocket.NewHandler(hub, db, wsRedis, jwtSecret, []string{"*"}, nil, nil)
	router := gin.New()
	router.GET("/ws", wsHandler.HandleWebSocket)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	t.Cleanup(hub.Shutdown)
	wsURL := "ws" + server.URL[len("http"):] + "/ws?token=" + signedRemoveMemberWSToken(t, successor, jwtSecret)
	conn, response, err := gorillaWS.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	require.Eventually(t, func() bool {
		return hub.GetUserClientCount(successor) == 1
	}, time.Second, 10*time.Millisecond)

	r := invokeRemoveMember(t, h, convID, creator.String(), target.String())
	require.Equal(t, 503, r.Code)
	disconnectTimer := time.NewTimer(3 * time.Second)
	defer disconnectTimer.Stop()
	disconnectObserved := false
	for !disconnectObserved {
		select {
		case payload := <-enforcement:
			if payload["userId"] != target.String() {
				continue
			}
			require.Equal(t, convID, payload["channelId"])
			require.Equal(t, "disconnect", payload["action"])
			disconnectObserved = true
		case <-disconnectTimer.C:
			t.Fatal("missing unconditional voice disconnect for removed member")
		}
	}

	seen := map[string]map[string]interface{}{}
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	for len(seen) < 3 {
		_, payload, readErr := conn.ReadMessage()
		require.NoError(t, readErr)
		var event struct {
			Type string                 `json:"type"`
			Data map[string]interface{} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(payload, &event))
		if event.Type == "dm_participant_removed" || event.Type == "dm_role_changed" || event.Type == "key_rotation" {
			seen[event.Type] = event.Data
		}
	}
	require.Equal(t, target.String(), seen["dm_participant_removed"]["user_id"])
	require.Equal(t, successor.String(), seen["dm_role_changed"]["user_id"])
	require.Equal(t, "admin", seen["dm_role_changed"]["role"])
	require.Equal(t, float64(2), seen["key_rotation"]["new_key_version"])
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID))
	var newCreator uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT created_by FROM dm_conversations WHERE id = $1`, convID).Scan(&newCreator))
	require.Equal(t, successor, newCreator)
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM presence_active_pending_plans WHERE user_id = $1 AND category = $2 AND resolution = 'conservative'`, target, string(presence.CategoryPrivateCall)))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_key_revocations WHERE conversation_id = $1 AND reason = 'member_removed'`, convID))
}

func TestRemoveMemberLocksUserBeforeConversation(t *testing.T) {
	db, convID, creator, target, h, _ := seedMemberRemovalFixture(t, true)
	observed := false
	h.afterUsersLockHook = func(tx *sql.Tx) {
		observed = true
		relations := sampleRelationLocks(t, db, backendPID(t, tx))
		require.Contains(t, relations, "users")
		require.NotContains(t, relations, "dm_conversations")
	}
	require.Equal(t, 200, invokeRemoveMember(t, h, convID, creator.String(), target.String()).Code)
	require.True(t, observed, "removal must expose the users-before-conversation lock point")
}

func TestRemoveMemberDoesNotDeadlockAgainstDMMessageEdit(t *testing.T) {
	db, convID, creator, target, h, _ := seedMemberRemovalFixture(t, true)
	ctx := context.Background()
	editTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := editTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback editor transaction: %v", rollbackErr)
		}
	}()
	var editorTxID int64
	require.NoError(t, editTx.QueryRow(`SELECT txid_current()`).Scan(&editorTxID))
	var lockedUser uuid.UUID
	require.NoError(t, editTx.QueryRow(`SELECT id FROM users WHERE id = $1 FOR SHARE`, target).Scan(&lockedUser))
	done := make(chan int, 1)
	go func() { done <- invokeRemoveMember(t, h, convID, creator.String(), target.String()).Code }()
	dbtest.WaitForRowLockWaiter(t, db, editorTxID)
	var waitingPID int
	require.NoError(t, db.QueryRow(`
		SELECT pid FROM pg_locks
		WHERE locktype = 'transactionid' AND NOT granted
		  AND transactionid::text::bigint = $1
		LIMIT 1`, dbtest.TransactionIDForLockProbe(editorTxID)).Scan(&waitingPID))
	require.NotContains(t, sampleRelationLocks(t, db, waitingPID), "dm_conversations")
	var lockedConversation string
	require.NoError(t, editTx.QueryRow(`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, convID).Scan(&lockedConversation))
	require.NoError(t, editTx.Commit())
	select {
	case code := <-done:
		require.Equal(t, http.StatusOK, code)
	case <-time.After(3 * time.Second):
		t.Fatal("member removal/editor contention exceeded bound")
	}
}

func TestRemoveMemberDoesNotDeadlockAgainstVoiceIngress(t *testing.T) {
	db, convID, creator, target, h, _ := seedMemberRemovalFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	voiceTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := voiceTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback voice transaction: %v", rollbackErr)
		}
	}()
	conversationID := uuid.MustParse(convID)
	require.NoError(t, LockDMVoiceParticipantSetTx(ctx, voiceTx, conversationID))
	require.NoError(t, lockVoiceCandidates(ctx, voiceTx, []uuid.UUID{target}))
	lockKey, err := VoiceParticipantSetAdvisoryKey(conversationID)
	require.NoError(t, err)
	var lockedTarget uuid.UUID
	require.NoError(t, voiceTx.QueryRowContext(ctx, `
		SELECT user_id FROM dm_participants
		WHERE conversation_id = $1 AND user_id = $2 FOR KEY SHARE`, convID, target).Scan(&lockedTarget))

	removeDone := make(chan error, 1)
	go func() {
		_, _, removeErr := h.removeMemberTx(ctx, convID, target.String(), creator.String())
		removeDone <- removeErr
	}()

	dbtest.WaitForAdvisoryLockWaiter(t, db, lockKey)
	_, joinErr := voiceTx.ExecContext(ctx, `
		INSERT INTO dm_voice_participants (conversation_id, user_id, lifecycle_event_at)
		VALUES ($1, $2, now())`, convID, creator)
	require.NoError(t, joinErr, "voice ingress must not deadlock with member removal")
	require.NoError(t, voiceTx.Commit())
	removeErr := <-removeDone

	require.NoError(t, removeErr, "member removal must not deadlock with voice ingress")
}

func TestInactiveCreatorRemovalSerializesBeforeErasureLocks(t *testing.T) {
	db, convID, creator, successor, h, _ := seedMemberRemovalFixture(t, true)
	_, err := db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, convID, creator)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	erasureTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := erasureTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback erasure transaction: %v", rollbackErr)
		}
	}()
	require.NoError(t, LockPrivateVoiceScopesTx(ctx, erasureTx, []uuid.UUID{creator, successor}))
	var lockedUser uuid.UUID
	require.NoError(t, erasureTx.QueryRowContext(ctx, `
		SELECT id FROM users WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`, creator, successor).Scan(&lockedUser))

	removalDone := make(chan error, 1)
	go func() {
		_, _, removeErr := h.removeMemberTx(ctx, convID, creator.String(), creator.String())
		removalDone <- removeErr
	}()

	successorLock, err := PrivateVoiceScopeAdvisoryKey(successor)
	require.NoError(t, err)
	dbtest.WaitForAdvisoryLockWaiter(t, db, successorLock)

	_, err = erasureTx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, creator)
	require.NoError(t, err)
	require.NoError(t, erasureTx.Commit())

	select {
	case removeErr := <-removalDone:
		require.Error(t, removeErr, "removal must fail closed after erasure removes its conversation")
		require.NotContains(t, removeErr.Error(), "40P01")
	case <-time.After(3 * time.Second):
		t.Fatal("inactive creator removal/erasure contention exceeded bound")
	}
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM dm_conversations WHERE id = $1`, convID))
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM users WHERE id = $1`, creator))
}

// The account-erasure transaction locks its user row before deleting it. A
// non-creator admin removal reaches the revocation ledger with a FOR KEY SHARE
// check on that same user row, after it has locked the conversation and caller
// membership. Staging the two waits makes the pre-fix cycle deterministic.
func TestAdminRemovalAgainstCallerErasureCompletesWithoutDeadlock(t *testing.T) {
	db, convID, creator, target, h, _ := seedMemberRemovalFixture(t, true)
	caller := dbtest.CreateUser(t, db)
	_, err := db.Exec(`
		INSERT INTO dm_participants (conversation_id, user_id, role)
		VALUES ($1, $2, 'admin')`, convID, caller)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, convID, target)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version)
		VALUES ($1, $2, $3, 1)`, convID, creator, []byte("key"))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	erasureTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := erasureTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback erasure transaction: %v", rollbackErr)
		}
	}()
	var erasureTxID int64
	require.NoError(t, erasureTx.QueryRowContext(ctx, `SELECT txid_current()`).Scan(&erasureTxID))
	var lockedUser uuid.UUID
	require.NoError(t, erasureTx.QueryRowContext(ctx, `
		SELECT id FROM users WHERE id = $1 FOR UPDATE`, caller).Scan(&lockedUser))

	removalDone := make(chan error, 1)
	go func() {
		_, _, removeErr := h.removeMemberTx(ctx, convID, target.String(), caller.String())
		removalDone <- removeErr
	}()

	// This waiter plus the relation-lock sample pins the fixed order: removal
	// waits on the caller user before acquiring the conversation. On the
	// baseline, removal reaches the late revocation FK check after locking the
	// conversation.
	dbtest.WaitForRowLockWaiter(t, db, erasureTxID)
	var waitingPID int
	require.NoError(t, db.QueryRow(`
		SELECT pid FROM pg_locks
		WHERE locktype = 'transactionid' AND NOT granted
		  AND transactionid::text::bigint = $1
		LIMIT 1`, dbtest.TransactionIDForLockProbe(erasureTxID)).Scan(&waitingPID))
	require.NotContains(t, sampleRelationLocks(t, db, waitingPID), "dm_conversations",
		"removal must wait on the caller user before acquiring the conversation")

	erasureDone := make(chan error, 1)
	go func() {
		_, erasureErr := erasureTx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, caller)
		if erasureErr == nil {
			erasureErr = erasureTx.Commit()
		}
		erasureDone <- erasureErr
	}()

	var removalErr, erasureErr error
	removalFinished, erasureFinished := false, false
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for !removalFinished || !erasureFinished {
		select {
		case removalErr = <-removalDone:
			removalFinished = true
		case erasureErr = <-erasureDone:
			erasureFinished = true
		case <-deadline.C:
			t.Fatal("admin removal against caller erasure exceeded liveness bound; neither transaction may return PostgreSQL 40P01")
		}
	}
	require.NoError(t, erasureErr,
		"caller account erasure must complete without a deadlock victim")
	require.ErrorIs(t, removalErr, errMemberRemovalStateDrifted,
		"admin removal must fail closed with state drift after caller erasure, without a deadlock victim")
}
