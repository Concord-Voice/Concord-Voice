package dm_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCallScopedDMVoiceHandler(t *testing.T, ts *testhelpers.TestServer) *dm.Handler {
	t.Helper()
	url := "nats://localhost:4222"
	if configured := os.Getenv("NATS_URL"); configured != "" {
		url = configured
	}
	bus, err := natsclient.Connect(url)
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping live DM enforcement test", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	return dm.NewHandler(dm.HandlerDeps{
		DB:       ts.DB,
		Log:      logger.New("test"),
		Hub:      ts.Hub,
		Cfg:      &config.Config{MediaPlaneURL: "http://media.test", JWTSecret: testhelpers.TestJWTSecret},
		NATS:     bus,
		Redis:    ts.Redis,
		EntCache: entitlements.NewCache(ts.Redis, ts.DB),
	})
}

func TestAuthorizeVoiceJoin_CommitFailureDisconnectCarriesCallID(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "call_scope_join_caller")
	callee := ts.CreateTestUser(t, "call_scope_join_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	h := newCallScopedDMVoiceHandler(t, ts)
	messages := subscribeDMVoiceDisconnect(t)
	convUUID := uuid.MustParse(convID)
	var failedCallID uuid.UUID
	dm.SetDMTopologyCommitHookForTest(h, func(*sql.Tx) error {
		lease, found, err := dm.LookupDMVoiceCallLease(context.Background(), ts.Redis, convUUID)
		require.NoError(t, err)
		require.True(t, found, "the failed direct join must have reserved a call")
		failedCallID = lease.CallID
		return errors.New("commit failed")
	})
	t.Cleanup(func() { dm.SetDMTopologyCommitHookForTest(h, nil) })

	c, response := crossStoreVoiceContext(t, http.MethodPost, "/voice/join", caller.ID, convID, nil)
	h.AuthorizeVoiceJoin(c)
	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.NotEqual(t, uuid.Nil, failedCallID, "the test must capture the call that failed to commit")

	payload := waitDMVoiceEnforcement(t, messages, caller.ID)
	assert.Equal(t, convID, payload["channelId"], "the compensation must target the DM conversation")
	assert.Equal(t, "disconnect", payload["action"], "the failed join must publish its disconnect compensation")
	assert.Equal(t, failedCallID.String(), payload["callId"], "failed join compensation must carry the failed call ID")
}

func TestAuthorizeDMVoiceForMediaPlane_CommitFailureDisconnectCarriesCallID(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "call_scope_media_caller")
	callee := ts.CreateTestUser(t, "call_scope_media_callee")
	convID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	h := newCallScopedDMVoiceHandler(t, ts)
	messages := subscribeDMVoiceDisconnect(t)
	convUUID := uuid.MustParse(convID)
	callID := uuid.New()
	require.NoError(t, dm.RefreshDMVoiceCallLease(context.Background(), ts.Redis, dm.VoiceCallLease{
		ConversationID: convUUID,
		CallID:         callID,
		CallerUserID:   uuid.MustParse(caller.ID),
	}, dm.DMVoiceCallReservationTTL, true))
	require.NoError(t, dm.RememberDMVoiceJoinAdmission(
		context.Background(), ts.Redis, convUUID, uuid.MustParse(caller.ID), callID, dm.DMVoiceCallReservationTTL,
	))
	dm.SetDMTopologyCommitHookForTest(h, func(*sql.Tx) error { return errors.New("commit failed") })
	t.Cleanup(func() { dm.SetDMTopologyCommitHookForTest(h, nil) })

	c, response := crossStoreVoiceContext(t, http.MethodPost, "/voice/authorize", caller.ID, convID,
		map[string]string{"call_id": callID.String()})
	c.Request.Header = dmVoiceMediaAuthorizationHeaders(caller.AccessToken, convID, callID.String())
	h.AuthorizeDMVoiceForMediaPlane(c)
	require.Equal(t, http.StatusInternalServerError, response.Code)

	payload := waitDMVoiceEnforcement(t, messages, caller.ID)
	assert.Equal(t, convID, payload["channelId"], "the compensation must target the DM conversation")
	assert.Equal(t, "disconnect", payload["action"], "the failed authorization must publish its disconnect compensation")
	assert.Equal(t, callID.String(), payload["callId"], "failed media authorization compensation must carry the failed call ID")
}
