package dm_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAuthorizeVoiceJoinSerializesInjectedMemberRemoval(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "redteam_stale_authorization_caller")
	peer := ts.CreateTestUser(t, "redteam_stale_authorization_peer")
	conversationID := ts.CreateDMConversation(t, caller.ID, peer.ID)
	handler := crossStoreVoiceHandler(ts)
	dm.SetDMTopologyCommitHookForTest(handler, func(tx *sql.Tx) error {
		removed := make(chan error, 1)
		go func() {
			_, err := ts.DB.ExecContext(context.Background(),
				`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
				conversationID, caller.ID,
			)
			removed <- err
		}()
		select {
		case err := <-removed:
			t.Errorf("membership delete crossed the authorization fence: %v", err)
			return fmt.Errorf("membership delete crossed the authorization fence: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		require.NoError(t, tx.Rollback())
		select {
		case err := <-removed:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Error("membership delete did not resume after fence release")
			return errors.New("membership delete did not resume after fence release")
		}
		return errors.New("injected membership removal")
	})

	request, response := crossStoreVoiceContext(
		t, http.MethodPost, "/voice/join", caller.ID, conversationID, nil,
	)
	handler.AuthorizeVoiceJoin(request)

	require.NotEqual(t, http.StatusOK, response.Code)
	require.NotContains(t, response.Body.String(), `"allowed":true`)
	var participants int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, caller.ID,
	).Scan(&participants))
	require.Zero(t, participants, "the endpoint authorized a user removed before its fence committed")
}

// The held authorization transaction also fences legacy/direct participant
// deletion until its membership decision commits.
func TestVoiceAuthorizationFenceSerializesDirectMembershipRemoval(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	caller := ts.CreateTestUser(t, "redteam_voice_auth_caller")
	peer := ts.CreateTestUser(t, "redteam_voice_auth_peer")
	conversationID := ts.CreateDMConversation(t, caller.ID, peer.ID)

	voiceTx, err := dm.BeginDMTopologyEffectForTest(
		context.Background(),
		dm.NewHandler(dm.HandlerDeps{DB: ts.DB}),
		conversationID,
		[]uuid.UUID{uuid.MustParse(caller.ID)},
	)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := voiceTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback voice-authorization transaction: %v", rollbackErr)
		}
	}()

	removed := make(chan error, 1)
	go func() {
		tx, beginErr := ts.DB.BeginTx(context.Background(), nil)
		if beginErr != nil {
			removed <- beginErr
			return
		}
		defer func() {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				t.Errorf("rollback membership-removal transaction: %v", rollbackErr)
			}
		}()
		if _, deleteErr := tx.ExecContext(context.Background(),
			`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
			conversationID, caller.ID,
		); deleteErr != nil {
			removed <- deleteErr
			return
		}
		removed <- tx.Commit()
	}()

	select {
	case err := <-removed:
		t.Fatalf("membership removal crossed the live authorization fence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, voiceTx.Commit(), "authorization fence should release cleanly")
	select {
	case err := <-removed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("membership removal did not resume after authorization fence release")
	}

	var participants int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, caller.ID,
	).Scan(&participants))
	require.Zero(t, participants)
}
