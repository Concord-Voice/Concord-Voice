package dm_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func credentialEpochHandler(ts *testhelpers.TestServer) *dm.Handler {
	return dm.NewHandler(dm.HandlerDeps{
		DB: ts.DB, Redis: ts.Redis, Hub: ts.Hub,
		Cfg:      &config.Config{JWTSecret: testhelpers.TestJWTSecret, MediaPlaneURL: "http://media.test"},
		EntCache: entitlements.NewCache(ts.Redis, ts.DB), Log: logger.New("test"),
	})
}

type blockingRequestBody struct {
	payload []byte
	started chan<- struct{}
	release <-chan struct{}
	done    bool
}

func (b *blockingRequestBody) Read(p []byte) (int, error) {
	if !b.done {
		close(b.started)
		<-b.release
		b.done = true
	}
	if len(b.payload) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.payload)
	b.payload = b.payload[n:]
	return n, nil
}

func (*blockingRequestBody) Close() error { return nil }

func TestStaleCredential_DMVoiceJoinDoesNotCreateReservation(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "stale_dm_voice_caller")
	callee := ts.CreateTestUser(t, "stale_dm_voice_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	staleToken := ts.SimulateStaleEpochWindow(t, caller.ID)
	w := ts.DoRequest(http.MethodPost,
		pathDMConversationsPrefix+convID+"/voice/join",
		nil, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	_, found, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, uuid.MustParse(convID))
	require.NoError(t, err)
	assert.False(t, found, "stale DM voice join must not reserve a call")
}

func TestStaleCredential_DMVoiceMediaAuthorizeDoesNotMarkLease(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "stale_dm_media_caller")
	callee := ts.CreateTestUser(t, "stale_dm_media_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	joined := ts.DoRequest(http.MethodPost,
		pathDMConversationsPrefix+convID+"/voice/join", nil,
		testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusOK, joined.Code, joined.Body.String())
	var joinBody map[string]interface{}
	testhelpers.ParseJSON(t, joined, &joinBody)
	callID := testhelpers.JSONField[string](t, joinBody, "call_id")

	staleToken := ts.SimulateStaleEpochWindow(t, caller.ID)
	w := ts.DoRequest(http.MethodPost,
		pathDMConversationsPrefix+convID+pathVoiceAuthorize,
		map[string]interface{}{"call_id": callID},
		dmVoiceMediaAuthorizationHeaders(staleToken, convID, callID))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	lease, found, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, uuid.MustParse(convID))
	require.NoError(t, err)
	require.True(t, found)
	assert.False(t, lease.MediaAuthorized, "stale DM credentials must not authorize the media handoff")
}

func TestStaleCredential_DMHardMuteDoesNotApplyEnforcement(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	admin := ts.CreateTestUser(t, "stale_dm_moderator")
	target := ts.CreateTestUser(t, "stale_dm_target")
	convID := createTestGroup(t, ts, admin, target)

	staleToken := ts.SimulateStaleEpochWindow(t, admin.ID)
	w := ts.DoRequest(http.MethodPost,
		pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+pathMute,
		nil, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	var muted bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT server_muted FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
		convID, target.ID,
	).Scan(&muted))
	assert.False(t, muted, "stale credentials must not apply DM hard mute")
}

func TestStaleCredential_DMUserMuteDoesNotPublishEnforcement(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	actor := ts.CreateTestUser(t, "stale_dm_user_mute_actor")
	target := ts.CreateTestUser(t, "stale_dm_user_mute_target")
	convID := ts.CreateDMConversation(t, actor.ID, target.ID)
	_, err := ts.DB.Exec(`INSERT INTO dm_voice_participants (conversation_id, user_id, is_muted, is_deafened, is_video_on, is_screen_sharing) VALUES ($1, $2, false, false, false, false)`, convID, target.ID)
	require.NoError(t, err)

	staleToken := ts.SimulateStaleEpochWindow(t, actor.ID)
	w := ts.DoRequest(http.MethodPost,
		pathDMConversationsPrefix+convID+pathVoiceSlash+target.ID+"/user-mute",
		nil, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

func TestStaleCredential_RingDoesNotCreatePendingCall(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)
	caller := ts.CreateTestUser(t, "stale_ring_caller")
	callee := ts.CreateTestUser(t, "stale_ring_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)

	staleToken := ts.SimulateStaleEpochWindow(t, caller.ID)
	w := ts.DoRequest(http.MethodPost,
		pathDMConversationsPrefix+convID+pathVoiceRing,
		nil, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.False(t, dm.PendingDMCallExistsForTest(uuid.MustParse(convID)), "stale credentials must not create a pending ring")
}

func TestStaleCredential_CancelDoesNotTerminatePendingCall(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)
	caller := ts.CreateTestUser(t, "stale_cancel_caller")
	callee := ts.CreateTestUser(t, "stale_cancel_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	ring := ts.DoRequest(http.MethodPost,
		pathDMConversationsPrefix+convID+pathVoiceRing,
		nil, testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusOK, ring.Code, ring.Body.String())

	staleToken := ts.SimulateStaleEpochWindow(t, caller.ID)
	w := ts.DoRequest(http.MethodPost,
		pathDMConversationsPrefix+convID+"/voice/cancel",
		nil, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.True(t, dm.PendingDMCallExistsForTest(uuid.MustParse(convID)), "stale credentials must not terminate the pending ring")
}

func TestStaleCredential_AbortDoesNotDeleteAuthorizedLease(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "stale_abort_caller")
	callee := ts.CreateTestUser(t, "stale_abort_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	authorized, callID := authorizeDMVoiceForMediaPlaneAfterJoin(t, ts, convID, caller)
	require.Equal(t, http.StatusOK, authorized.Code, authorized.Body.String())

	staleToken := ts.SimulateStaleEpochWindow(t, caller.ID)
	w := ts.DoRequest(http.MethodDelete,
		pathDMConversationsPrefix+convID+pathVoiceAuthorize,
		map[string]interface{}{"call_id": callID},
		dmVoiceMediaReleaseHeaders(staleToken, convID, callID))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	lease, found, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, uuid.MustParse(convID))
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, lease.MediaAuthorized, "stale credentials must not delete the authorized lease")
}

func TestStaleCredential_CancelRaceAfterGuardCommitDoesNotTerminateRing(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)
	caller := ts.CreateTestUser(t, "stale_cancel_race_caller")
	callee := ts.CreateTestUser(t, "stale_cancel_race_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	ring := ts.DoRequest(http.MethodPost, pathDMConversationsPrefix+convID+pathVoiceRing, nil, testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusOK, ring.Code, ring.Body.String())

	started := make(chan struct{})
	release := make(chan struct{})
	body := &blockingRequestBody{payload: []byte(`{}`), started: started, release: release}
	req := httptest.NewRequest(http.MethodPost, pathDMConversationsPrefix+convID+"/voice/cancel", body)
	req.Header = testhelpers.AuthHeaders(caller.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		ts.Router.ServeHTTP(w, req)
		response <- w
	}()
	<-started
	// resolvePendingDMRing has already committed its topology/credential guard;
	// reset before the body parser releases to model the post-guard race.
	ts.SimulateStaleEpochWindow(t, caller.ID)
	close(release)
	w := <-response
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.True(t, dm.PendingDMCallExistsForTest(uuid.MustParse(convID)), "stale cancellation must not terminate the pending ring")
}

func TestDMCancelCommitFailureLeavesRingRetryable(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)
	caller := ts.CreateTestUser(t, "cancel_commit_failure_caller")
	callee := ts.CreateTestUser(t, "cancel_commit_failure_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	ringForTest(t, ts, caller, convID)
	h := credentialEpochHandler(ts)
	dm.SetDMTopologyCommitHookForTest(h, func(*sql.Tx) error { return errors.New("commit failed") })
	c, failed := crossStoreVoiceContext(t, http.MethodPost, "/voice/cancel", caller.ID, convID, nil)
	h.CancelDMCall(c)
	assert.Equal(t, http.StatusInternalServerError, failed.Code)
	assert.True(t, dm.PendingDMCallExistsForTest(uuid.MustParse(convID)), "failed cancel must leave the ring retryable")

	dm.SetDMTopologyCommitHookForTest(h, nil)
	c, retried := crossStoreVoiceContext(t, http.MethodPost, "/voice/cancel", caller.ID, convID, nil)
	h.CancelDMCall(c)
	c.Writer.WriteHeaderNow()
	assert.Equal(t, http.StatusNoContent, retried.Code, retried.Body.String())
}

func TestDMDeclineCommitFailureRestoresDeclinedUser(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)
	caller := ts.CreateTestUser(t, "decline_commit_failure_caller")
	first := ts.CreateTestUser(t, "decline_commit_failure_first")
	second := ts.CreateTestUser(t, "decline_commit_failure_second")
	convID := ts.CreateGroupDMConversation(t, caller.ID, first.ID, second.ID)
	for _, userID := range []string{caller.ID, first.ID, second.ID} {
		ts.Hub.MarkUserOnlineForTest(uuid.MustParse(userID))
	}
	ringForTest(t, ts, caller, convID)
	h := crossStoreVoiceHandler(ts)
	dm.SetDMTopologyCommitHookForTest(h, func(*sql.Tx) error { return errors.New("commit failed") })
	c, failed := crossStoreVoiceContext(t, http.MethodPost, "/voice/decline", first.ID, convID, nil)
	h.DeclineDMCall(c)
	assert.Equal(t, http.StatusInternalServerError, failed.Code)

	dm.SetDMTopologyCommitHookForTest(h, nil)
	c, retried := crossStoreVoiceContext(t, http.MethodPost, "/voice/decline", first.ID, convID, nil)
	h.DeclineDMCall(c)
	c.Writer.WriteHeaderNow()
	assert.Equal(t, http.StatusNoContent, retried.Code, retried.Body.String())
	assert.True(t, dm.PendingDMCallExistsForTest(uuid.MustParse(convID)), "nonterminal decline should leave the other callee ringing")
}

func TestDMAbortCommitFailureRestoresAuthorizedLease(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "abort_commit_failure_caller")
	callee := ts.CreateTestUser(t, "abort_commit_failure_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	authorized, callID := authorizeDMVoiceForMediaPlaneAfterJoin(t, ts, convID, caller)
	require.Equal(t, http.StatusOK, authorized.Code, authorized.Body.String())
	h := credentialEpochHandler(ts)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	t.Cleanup(cancelRequest)
	dm.SetDMTopologyCommitHookForTest(h, func(*sql.Tx) error {
		cancelRequest()
		return errors.New("commit failed")
	})
	c, failed := crossStoreVoiceContext(t, http.MethodDelete, "/voice/authorize", caller.ID, convID,
		map[string]interface{}{"call_id": callID})
	c.Request.URL.Path = pathDMConversationsPrefix + convID + pathVoiceAuthorize
	c.Request.Header = dmVoiceMediaReleaseHeaders(caller.AccessToken, convID, callID)
	c.Request = c.Request.WithContext(requestCtx)
	h.AbortDMVoiceMediaAuthorization(c)
	assert.Equal(t, http.StatusInternalServerError, failed.Code)

	lease, found, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, uuid.MustParse(convID))
	require.NoError(t, err)
	assert.True(t, found, "a failed abort commit must restore the authorized lease")
	if found {
		assert.True(t, lease.MediaAuthorized)
	}
}
