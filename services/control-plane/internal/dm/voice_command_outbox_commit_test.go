package dm

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func routeDMOutboxDeafen(t *testing.T, handler *Handler, actorID, convID, targetID string) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.Use(func(ctx *gin.Context) {
		ctx.Set("user_id", actorID)
		withActor(t, handler.db, ctx, actorID)
		ctx.Next()
	})
	router.POST("/conversations/:id/voice/:userId/deafen", handler.DMHardDeafen)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost,
		"/conversations/"+convID+"/voice/"+targetID+"/deafen", nil))
	return response
}

func TestVoiceCommandOutbox_DMCommitAckLossRetainsSavedPolicy(t *testing.T) {
	db, convID, admin, target, handler, _ := seedMemberRemovalFixture(t, false)
	handler.commitDMTopologyTx = func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return errors.New("injected outbox commit acknowledgement lost")
	}
	response := routeDMOutboxDeafen(t, handler, admin.String(), convID, target.String())
	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	var muted, deafened bool
	require.NoError(t, db.QueryRow(`SELECT server_muted, server_deafened FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
		convID, target).Scan(&muted, &deafened))
	assert.True(t, muted)
	assert.True(t, deafened)
	var retained int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM voice_command_outbox
		WHERE scope = 'dm' AND kind = 'server_moderation' AND action = 'deafen'
			AND channel_id = $1 AND user_id = $2 AND server_muted AND server_deafened
			AND authorization_revision > 0 AND published_at IS NULL AND broker_sequence IS NULL
	`, convID, target).Scan(&retained))
	assert.Equal(t, 1, retained, "the saved policy and command survive an acknowledgement lost after COMMIT")
}

func TestVoiceCommandOutbox_DMSnapshotNilNATSRetainsCommand(t *testing.T) {
	db, convID, admin, target, handler, _ := seedMemberRemovalFixture(t, false)
	require.Nil(t, handler.nats)
	response := routeDMOutboxDeafen(t, handler, admin.String(), convID, target.String())
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var body struct {
		EnforcementStatus string `json:"enforcement_status"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, "queued", body.EnforcementStatus)
	var retained int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM voice_command_outbox WHERE scope = 'dm' AND kind = 'server_moderation'
			AND action = 'deafen' AND channel_id = $1 AND user_id = $2
			AND server_muted AND server_deafened AND authorization_revision > 0
			AND session_generation IS NULL AND node_boot_id IS NULL AND socket_id IS NULL AND credential_epoch IS NULL
			AND published_at IS NULL AND broker_sequence IS NULL
	`, convID, target).Scan(&retained))
	assert.Equal(t, 1, retained)
}
