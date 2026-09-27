package dm_test

import (
	"database/sql"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A credential reset must not be able to commit after the ring's credential
// guard while the old request can still create and publish a new pending ring.
// This is the admitted-before/commits-after race the epoch fence is intended to
// close: once the reset commits, the old credential must have no remaining
// externally visible write boundary to cross.
func TestRedTeamRingCannotPublishAfterCredentialResetCommits(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	t.Cleanup(dm.ResetPendingDMCallsForTest)
	caller := ts.CreateTestUser(t, "redteam_ring_epoch_caller")
	callee := ts.CreateTestUser(t, "redteam_ring_epoch_callee")
	conversationID := ts.CreateDMConversation(t, caller.ID, callee.ID)
	conversationUUID := uuid.MustParse(conversationID)
	const oldEpoch = "ring-old-epoch-3141"
	const newEpoch = "ring-new-epoch-3141"
	_, err := ts.DB.Exec(`UPDATE users SET credential_epoch = $1 WHERE id = $2`, oldEpoch, caller.ID)
	require.NoError(t, err)
	calleeConn := dialDMObserver(t, ts, callee.ID, conversationID)
	handler := crossStoreVoiceHandler(ts)

	guardCommitted := make(chan struct{})
	releaseOldRequest := make(chan struct{})
	dm.SetDMTopologyCommitHookForTest(handler, func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		close(guardCommitted)
		<-releaseOldRequest
		return nil
	})
	t.Cleanup(func() { dm.SetDMTopologyCommitHookForTest(handler, nil) })

	request, recorder := crossStoreVoiceContext(
		t, http.MethodPost, "/voice/ring", caller.ID, conversationID, nil,
	)
	// This test calls the handler directly, so install the same non-empty
	// credential claim that middleware.AuthRequired would have attached.
	request.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"cred_epoch": oldEpoch})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler.RingDMCall(request)
	}()

	select {
	case <-guardCommitted:
	case <-time.After(2 * time.Second):
		close(releaseOldRequest)
		t.Fatal("ring request never reached its post-commit window")
	}

	// This DB update represents the credential reset winning after T1 commits
	// and before the fresh announcement fence can acquire the user-row lock.
	_, err = ts.DB.Exec(`UPDATE users SET credential_epoch = $1 WHERE id = $2`, newEpoch, caller.ID)
	require.NoError(t, err)
	ringExistedWhenResetCommitted := dm.PendingDMCallExistsForTest(conversationUUID)
	close(releaseOldRequest)

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("ring request did not finish after releasing the commit hook")
	}

	require.False(t, ringExistedWhenResetCommitted,
		"T1 must not expose a pending ring before the fresh announcement fence")
	require.Equal(t, http.StatusUnauthorized, recorder.Code,
		"a reset that commits before the announcement fence must reject the stale request")
	require.False(t, dm.PendingDMCallExistsForTest(conversationUUID),
		"a stale request must not retain a pending ring after credential reset")

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
		require.NotEqual(t, "dm_voice_call_invited", event.Type, "stale request sent an invite after credential reset")
		require.NotEqual(t, "dm_voice_call_canceled", event.Type, "stale request sent a cancellation without an invite")
	}
}
