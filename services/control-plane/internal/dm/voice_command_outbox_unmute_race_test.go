package dm

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVoiceCommandOutbox_DMUnmuteRechecksConcurrentDeafen(t *testing.T) {
	db, convID, admin, target, handler, _ := seedMemberRemovalFixture(t, false)
	_, err := db.Exec(`
		UPDATE dm_participants SET server_muted = TRUE, server_deafened = FALSE
		WHERE conversation_id = $1 AND user_id = $2
	`, convID, target)
	require.NoError(t, err)
	hookRan := false
	handler.beforeDMVoiceEnforcementTxHook = func() {
		hookRan = true
		result, hookErr := db.Exec(`
			UPDATE dm_participants SET server_muted = TRUE, server_deafened = TRUE
			WHERE conversation_id = $1 AND user_id = $2
		`, convID, target)
		require.NoError(t, hookErr)
		changed, rowsErr := result.RowsAffected()
		require.NoError(t, rowsErr)
		require.Equal(t, int64(1), changed)
	}

	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Set("user_id", admin.String())
	ctx.Params = gin.Params{{Key: "id", Value: convID}, {Key: "userId", Value: target.String()}}
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
	handler.DMHardUnmute(ctx)

	require.True(t, hookRan, "deafen must commit after the HTTP preflight and before the unmute transaction")
	assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	var muted, deafened bool
	require.NoError(t, db.QueryRow(`
		SELECT server_muted, server_deafened FROM dm_participants
		WHERE conversation_id = $1 AND user_id = $2
	`, convID, target).Scan(&muted, &deafened))
	assert.True(t, muted)
	assert.True(t, deafened)
	var commands, allocated int64
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM voice_command_outbox`).Scan(&commands))
	require.NoError(t, db.QueryRow(`SELECT last_allocated_sequence FROM voice_command_sequence WHERE id = TRUE`).Scan(&allocated))
	assert.Zero(t, commands, "a refused unmute must not enqueue a command")
	assert.Zero(t, allocated, "a refused unmute must not consume a command position")
}
