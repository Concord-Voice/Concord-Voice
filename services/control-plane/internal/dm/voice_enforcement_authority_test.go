package dm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadVoiceEnforcementSnapshotReturnsConsistentRevision(t *testing.T) {
	db, convID, _, target, _, _ := seedMemberRemovalFixture(t, false)
	_, err := db.Exec(`
		UPDATE dm_participants
		SET server_muted = true, server_deafened = true
		WHERE conversation_id = $1 AND user_id = $2`, convID, target)
	require.NoError(t, err)

	first, err := ReadVoiceEnforcementSnapshot(context.Background(), db, convID, target.String())
	require.NoError(t, err)
	second, err := ReadVoiceEnforcementSnapshot(context.Background(), db, convID, target.String())
	require.NoError(t, err)

	require.True(t, first.ServerMuted)
	require.True(t, first.ServerDeafened)
	assert.Positive(t, first.AuthorizationRevision)
	assert.Equal(t, first.ServerMuted, second.ServerMuted)
	assert.Equal(t, first.ServerDeafened, second.ServerDeafened)
	assert.Greater(t, second.AuthorizationRevision, first.AuthorizationRevision)
}

// A hard-mute request may pass the HTTP preflight and then lose its admin role
// before the guarded write begins. The transaction must reject that stale
// authority and leave the target unchanged.
func TestDMHardMuteRevalidatesAdminInsideGuardedTransaction(t *testing.T) {
	db, convID, admin, target, handler, _ := seedMemberRemovalFixture(t, false)
	handler.beforeDMVoiceEnforcementTxHook = func() {
		result, err := db.Exec(`
			UPDATE dm_participants SET role = 'member'
			WHERE conversation_id = $1 AND user_id = $2`, convID, admin)
		require.NoError(t, err)
		rows, err := result.RowsAffected()
		require.NoError(t, err)
		require.Equal(t, int64(1), rows)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("user_id", admin.String())
	ctx.Params = gin.Params{{Key: "id", Value: convID}, {Key: "userId", Value: target.String()}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	handler.DMHardMute(ctx)

	require.Equal(t, http.StatusForbidden, recorder.Code)
	var serverMuted bool
	require.NoError(t, db.QueryRow(`
		SELECT server_muted FROM dm_participants
		WHERE conversation_id = $1 AND user_id = $2`, convID, target).Scan(&serverMuted))
	require.False(t, serverMuted)
}
