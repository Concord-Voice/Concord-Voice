package dm_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func crossStoreVoiceContext(t *testing.T, method, path, userID, convID string, body any) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		c.Request.Header.Set("Content-Type", "application/json")
	}
	c.Params = gin.Params{{Key: "id", Value: convID}}
	c.Set("user_id", userID)
	return c, w
}

func crossStoreVoiceHandler(ts *testhelpers.TestServer) *dm.Handler {
	return dm.NewHandler(dm.HandlerDeps{
		DB:       ts.DB,
		Log:      logger.New("test"),
		Hub:      ts.Hub,
		Cfg:      &config.Config{MediaPlaneURL: "http://media.test"},
		Redis:    ts.Redis,
		EntCache: entitlements.NewCache(ts.Redis, ts.DB),
	})
}

func TestOpenConversation_PostCommitFetchFailureReturnsError(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "open_dm_fetch_failure_caller")
	target := ts.CreateTestUser(t, "open_dm_fetch_failure_target")
	ts.CreateFriendship(t, caller.ID, target.ID, statusAccepted)
	convID := ts.CreateDMConversation(t, caller.ID, target.ID)
	h := crossStoreVoiceHandler(ts)
	dm.SetDMTopologyCommitHookForTest(h, func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		_, err := ts.DB.Exec(`DELETE FROM dm_conversations WHERE id = $1`, convID)
		return err
	})
	t.Cleanup(func() { dm.SetDMTopologyCommitHookForTest(h, nil) })

	c, w := crossStoreVoiceContext(t, http.MethodPost, "/dm/conversations", caller.ID, "", map[string]string{
		"user_id": target.ID,
	})
	h.OpenConversation(c)

	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, "Failed to open conversation", body["error"])
	assert.NotContains(t, body, "conversation", "fetch failure must not return a conversation field")
}

func TestAuthorizeVoiceJoin_CommitFailureCompensatesLease(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "cross_store_join_caller")
	callee := ts.CreateTestUser(t, "cross_store_join_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	h := crossStoreVoiceHandler(ts)
	dm.SetDMTopologyCommitHookForTest(h, func(*sql.Tx) error { return errors.New("commit failed") })

	c, w := crossStoreVoiceContext(t, http.MethodPost, "/voice/join", caller.ID, convID, nil)
	h.AuthorizeVoiceJoin(c)
	require.Equal(t, http.StatusInternalServerError, w.Code)

	_, found, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, uuid.MustParse(convID))
	require.NoError(t, err)
	require.False(t, found, "a failed fence commit must retract the direct-call lease")
}

func TestAuthorizeVoiceJoin_AcceptCommitFailureLeavesRingRetryable(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)
	caller := ts.CreateTestUser(t, "accept_commit_failure_caller")
	callee := ts.CreateTestUser(t, "accept_commit_failure_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	ringID := ringForTest(t, ts, caller, convID)
	convUUID := uuid.MustParse(convID)
	h := crossStoreVoiceHandler(ts)
	dm.SetDMTopologyCommitHookForTest(h, func(*sql.Tx) error { return errors.New("commit failed") })

	c, failed := crossStoreVoiceContext(t, http.MethodPost, "/voice/join", callee.ID, convID,
		map[string]string{"ring_id": ringID})
	h.AuthorizeVoiceJoin(c)
	require.Equal(t, http.StatusInternalServerError, failed.Code)
	require.True(t, dm.PendingDMCallExistsForTest(convUUID), "failed accept must retain its ring")
	_, found, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, convUUID)
	require.NoError(t, err)
	require.False(t, found, "failed accept must retract its provisional lease")
	_, admitted, err := dm.LookupDMVoiceJoinAdmission(context.Background(), ts.Redis, convUUID, uuid.MustParse(callee.ID))
	require.NoError(t, err)
	require.False(t, admitted, "failed accept must retract its provisional admission")

	dm.SetDMTopologyCommitHookForTest(h, nil)
	c, retried := crossStoreVoiceContext(t, http.MethodPost, "/voice/join", callee.ID, convID,
		map[string]string{"ring_id": ringID})
	h.AuthorizeVoiceJoin(c)
	require.Equal(t, http.StatusOK, retried.Code, retried.Body.String())
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(retried.Body.Bytes(), &body))
	require.Equal(t, ringID, body["call_id"])
	require.False(t, dm.PendingDMCallExistsForTest(convUUID))
}

func TestAuthorizeVoiceJoin_AcceptCanceledRequestRetractsAndDeniesMedia(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)
	caller := ts.CreateTestUser(t, "accept_cancel_caller")
	callee := ts.CreateTestUser(t, "accept_cancel_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	ringID := ringForTest(t, ts, caller, convID)
	convUUID := uuid.MustParse(convID)
	h := crossStoreVoiceHandler(ts)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	t.Cleanup(cancelRequest)
	dm.SetDMTopologyCommitHookForTest(h, func(*sql.Tx) error {
		cancelRequest()
		return errors.New("commit canceled")
	})

	c, failed := crossStoreVoiceContext(t, http.MethodPost, "/voice/join", callee.ID, convID,
		map[string]string{"ring_id": ringID})
	c.Request = c.Request.WithContext(requestCtx)
	h.AuthorizeVoiceJoin(c)
	assert.Equal(t, http.StatusInternalServerError, failed.Code)
	assert.True(t, dm.PendingDMCallExistsForTest(convUUID), "canceled accept must retain its ring")
	_, found, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, convUUID)
	require.NoError(t, err)
	assert.False(t, found, "compensation must outlive the canceled request")
	_, admitted, err := dm.LookupDMVoiceJoinAdmission(context.Background(), ts.Redis, convUUID, uuid.MustParse(callee.ID))
	require.NoError(t, err)
	assert.False(t, admitted, "canceled request must not retain a media admission")

	media, denied := crossStoreVoiceContext(t, http.MethodPost, "/voice/authorize", callee.ID, convID,
		map[string]string{"call_id": ringID})
	media.Request.Header = dmVoiceMediaAuthorizationHeaders(callee.AccessToken, convID, ringID)
	h.AuthorizeDMVoiceForMediaPlane(media)
	assert.Equal(t, http.StatusConflict, denied.Code, denied.Body.String())
}

func TestAuthorizeVoiceJoin_AcceptActivationAcknowledgementLossConfirmsExactLease(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)
	caller := ts.CreateTestUser(t, "activation_ack_loss_caller")
	callee := ts.CreateTestUser(t, "activation_ack_loss_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	ringID := ringForTest(t, ts, caller, convID)
	convUUID := uuid.MustParse(convID)
	h := crossStoreVoiceHandler(ts)
	dm.SetDMTopologyCommitHookForTest(h, func(tx *sql.Tx) error { return tx.Commit() })
	dm.SetDMVoiceActivationHookForTest(h, func(ctx context.Context, conversationID, callID uuid.UUID, ttl time.Duration) error {
		if err := dm.ActivateAcceptedDMVoiceCallLease(ctx, ts.Redis, conversationID, callID, ttl); err != nil {
			return err
		}
		return errors.New("activation acknowledgement lost")
	})

	c, accepted := crossStoreVoiceContext(t, http.MethodPost, "/voice/join", callee.ID, convID,
		map[string]string{"ring_id": ringID})
	h.AuthorizeVoiceJoin(c)
	require.Equal(t, http.StatusOK, accepted.Code, accepted.Body.String())
	assert.False(t, dm.PendingDMCallExistsForTest(convUUID))
	lease, found, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, convUUID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, uuid.MustParse(ringID), lease.CallID)
	assert.Equal(t, uuid.MustParse(ringID), lease.RingID)
	assert.True(t, lease.Promoted)
}

func TestAuthorizeDMVoiceForMediaPlane_RejectsProvisionalAcceptedRing(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "provisional_ring_caller")
	callee := ts.CreateTestUser(t, "provisional_ring_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	convUUID := uuid.MustParse(convID)
	callID := uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(context.Background(), ts.Redis, dm.VoiceCallLease{
		ConversationID: convUUID,
		CallID:         callID,
		RingID:         callID,
		CallerUserID:   uuid.MustParse(caller.ID),
	}, dm.DMVoiceCallReservationTTL, true))
	require.NoError(t, dm.RememberDMVoiceJoinAdmission(
		context.Background(), ts.Redis, convUUID, uuid.MustParse(caller.ID), callID, dm.DMVoiceCallReservationTTL,
	))

	h := crossStoreVoiceHandler(ts)
	media, denied := crossStoreVoiceContext(t, http.MethodPost, "/voice/authorize", caller.ID, convID,
		map[string]string{"call_id": callID.String()})
	media.Request.Header = dmVoiceMediaAuthorizationHeaders(caller.AccessToken, convID, callID.String())
	h.AuthorizeDMVoiceForMediaPlane(media)
	require.Equal(t, http.StatusConflict, denied.Code, denied.Body.String())
}

func TestRingDMCall_CommitFailureCancelsPendingRing(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)
	caller := ts.CreateTestUser(t, "cross_store_ring_caller")
	callee := ts.CreateTestUser(t, "cross_store_ring_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	calleeConn := dialDMObserver(t, ts, callee.ID, convID)
	h := crossStoreVoiceHandler(ts)
	dm.SetDMTopologyCommitHookForTest(h, func(*sql.Tx) error { return errors.New("commit failed") })

	c, w := crossStoreVoiceContext(t, http.MethodPost, "/voice/ring", caller.ID, convID, nil)
	h.RingDMCall(c)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.False(t, dm.PendingDMCallExistsForTest(uuid.MustParse(convID)), "a failed fence commit must retract the pending ring")
	deadline := time.Now().Add(1 * time.Second)
	for {
		require.NoError(t, calleeConn.SetReadDeadline(deadline))
		var event struct {
			Type string `json:"type"`
		}
		err := calleeConn.ReadJSON(&event)
		if err != nil {
			netErr, timedOut := err.(net.Error)
			require.True(t, timedOut && netErr.Timeout(), "observer connection failed before read deadline: %v", err)
			break
		}
		if event.Type == "dm_voice_call_invited" || event.Type == "dm_voice_call_canceled" {
			t.Fatalf("uncommitted ring sent unexpected %q event", event.Type)
		}
	}
}

func TestRingDMCall_RefreshesRecipientsUnderAnnouncementFence(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)
	caller := ts.CreateTestUser(t, "cross_store_refresh_caller")
	removed := ts.CreateTestUser(t, "cross_store_refresh_removed")
	remaining := ts.CreateTestUser(t, "cross_store_refresh_remaining")
	convID := ts.CreateGroupDMConversation(t, caller.ID, removed.ID, remaining.ID)
	ts.Hub.MarkUserOnlineForTest(uuid.MustParse(remaining.ID))
	removedConn := dialDMObserver(t, ts, removed.ID, convID)
	h := crossStoreVoiceHandler(ts)

	committed := make(chan struct{})
	release := make(chan struct{})
	dm.SetDMTopologyCommitHookForTest(h, func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		close(committed)
		<-release
		return nil
	})
	t.Cleanup(func() { dm.SetDMTopologyCommitHookForTest(h, nil) })

	c, w := crossStoreVoiceContext(t, http.MethodPost, "/voice/ring", caller.ID, convID, nil)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		h.RingDMCall(c)
	}()

	select {
	case <-committed:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("ring request did not commit T1")
	}
	_, err := ts.DB.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, removed.ID)
	require.NoError(t, err)
	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("ring request did not finish after T1 release")
	}
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		RingingUserIDs []string `json:"ringing_user_ids"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.NotContains(t, body.RingingUserIDs, removed.ID,
		"a member removed between T1 and announcement must be absent from the response ring")
	assert.Contains(t, body.RingingUserIDs, remaining.ID,
		"the remaining online member must stay in the response ring")

	deadline := time.Now().Add(1 * time.Second)
	for {
		require.NoError(t, removedConn.SetReadDeadline(deadline))
		var event struct {
			Type string `json:"type"`
		}
		err := removedConn.ReadJSON(&event)
		if err != nil {
			netErr, timedOut := err.(net.Error)
			require.True(t, timedOut && netErr.Timeout(), "observer connection failed before read deadline: %v", err)
			break
		}
		if event.Type == "dm_voice_call_invited" || event.Type == "dm_voice_call_canceled" {
			t.Fatalf("removed member received unexpected %q event", event.Type)
		}
	}
}

func TestDMVoiceTopologyFenceBlocksBlockUntilEffectReleases(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	left := ts.CreateTestUser(t, "cross_store_block_left")
	right := ts.CreateTestUser(t, "cross_store_block_right")
	convID := ts.CreateDMConversation(t, left.ID, right.ID)
	h := crossStoreVoiceHandler(ts)

	voiceTx, err := dm.BeginDMTopologyEffectForTest(context.Background(), h, convID, []uuid.UUID{uuid.MustParse(left.ID)})
	require.NoError(t, err)
	defer func() {
		if rollbackErr := voiceTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback voice-effect transaction: %v", rollbackErr)
		}
	}()

	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		blockTx, beginErr := ts.DB.BeginTx(context.Background(), nil)
		if beginErr != nil {
			finished <- beginErr
			return
		}
		close(started)
		recordErr := dmblock.RecordBlockTx(context.Background(), blockTx, left.ID, right.ID, uuid.NewString())
		if rollbackErr := blockTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			recordErr = errors.Join(recordErr, fmt.Errorf("rollback block transaction: %w", rollbackErr))
		}
		finished <- recordErr
	}()
	<-started
	select {
	case err := <-finished:
		t.Fatalf("Block crossed the held voice-effect fence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, voiceTx.Rollback())
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Block did not proceed after the voice-effect fence released")
	}
}
