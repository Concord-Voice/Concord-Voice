package dm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// The delete has committed, but receiver hide is a second guarded write. Drop
// the actor's membership at that exact boundary: own content must be gone,
// peer content must remain visible, and the audit must remain recoverable.
func TestPurgeConversationPostDeleteMembershipLossLeavesRecoveryAudit(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, _ := seedGroupCallWithParticipants(t, db, 0)
	var actorID string
	require.NoError(t, db.QueryRow(`SELECT created_by FROM dm_conversations WHERE id = $1`, convID).Scan(&actorID))
	peer := dbtest.CreateUser(t, db)
	_, err := db.Exec(`UPDATE dm_participants SET role = 'member' WHERE conversation_id = $1 AND user_id = $2`, convID, actorID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id, role) VALUES ($1, $2, 'member')`, convID, peer)
	require.NoError(t, err)
	var actorMessage, peerMessage string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_messages (conversation_id, user_id, content, type)
		VALUES ($1, $2, 'actor-delete', 'text') RETURNING id`, convID, actorID).Scan(&actorMessage))
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_messages (conversation_id, user_id, content, type)
		VALUES ($1, $2, 'peer-survives', 'text') RETURNING id`, convID, peer).Scan(&peerMessage))
	_, err = db.Exec(`
		INSERT INTO privacy_settings (user_id, require_auth_before_purge)
		VALUES ($1, FALSE) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = FALSE`, actorID)
	require.NoError(t, err)

	h, _ := newDMHandlerWithRail(t, db, convID)
	h.afterDMPurgeDeleteHook = func() {
		_, hookErr := db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, actorID)
		require.NoError(t, hookErr)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/dm/conversations/"+convID+"/messages",
		strings.NewReader(`{"range":"all"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Params = gin.Params{{Key: "id", Value: convID}}
	ctx.Set("user_id", actorID)
	h.PurgeConversation(ctx)

	require.Equal(t, http.StatusInternalServerError, recorder.Code, recorder.Body.String())
	require.Equal(t, 0, countRows(t, db, `SELECT count(*) FROM dm_messages WHERE id = $1`, actorMessage))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM dm_messages WHERE id = $1`, peerMessage))
	require.Zero(t, countRows(t, db,
		`SELECT count(*) FROM dm_message_hidden_ranges WHERE conversation_id = $1 AND user_id = $2`, convID, actorID))

	var status string
	var deleted, hidden int
	var completedAt interface{}
	require.NoError(t, db.QueryRow(`
		SELECT status, deleted_count, hidden_count, completed_at
		FROM message_purges WHERE context_id = $1`, convID).
		Scan(&status, &deleted, &hidden, &completedAt))
	require.Equal(t, "in_progress", status)
	require.Equal(t, 1, deleted)
	require.Zero(t, hidden)
	require.Nil(t, completedAt)
}
