package dm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// PurgeConversation resolves the actor before opening the purge transaction.
// Hold the users-before-parent fence, remove the participant after that
// preflight, and release it: the real purge Guard closure must reject the
// stale authority without deleting the message; its recovery audit remains
// in progress for the normal retry path.
func TestPurgeConversationRejectsParticipantRemovalAtGuard(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	convID, _ := seedGroupCallWithParticipants(t, db, 0)
	var actorID string
	require.NoError(t, db.QueryRow(`SELECT created_by FROM dm_conversations WHERE id = $1`, convID).Scan(&actorID))
	var messageID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_messages (conversation_id, user_id, content, type)
		VALUES ($1, $2, 'must survive guard drift', 'text') RETURNING id`, convID, actorID).Scan(&messageID))

	_, err := db.Exec(`
		INSERT INTO privacy_settings (user_id, require_auth_before_purge)
		VALUES ($1, FALSE) ON CONFLICT (user_id) DO UPDATE SET require_auth_before_purge = FALSE`, actorID)
	require.NoError(t, err)

	barrier, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = barrier.Rollback() }()
	var barrierXID int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&barrierXID))
	var lockedID string
	require.NoError(t, barrier.QueryRow(
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, actorID).Scan(&lockedID))

	h, _ := newDMHandlerWithRail(t, db, convID)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		gin.SetMode(gin.TestMode)
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodDelete, "/dm/conversations/"+convID+"/messages",
			strings.NewReader(`{"range":"all"}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		ctx.Params = gin.Params{{Key: "id", Value: convID}}
		ctx.Set("user_id", actorID)
		h.PurgeConversation(ctx)
		result <- recorder
	}()
	dbtest.WaitForRowLockWaiter(t, db, barrierXID)
	_, err = db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, actorID)
	require.NoError(t, err)
	require.NoError(t, barrier.Commit())

	select {
	case response := <-result:
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		require.JSONEq(t, `{"error":"dm_unavailable"}`, response.Body.String())
	case <-time.After(time.Second):
		t.Fatal("DM purge did not resume after releasing the authority fence")
	}

	var messageCount, auditCount int
	var auditStatus string
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_messages WHERE id = $1`, messageID).Scan(&messageCount))
	require.NoError(t, db.QueryRow(`SELECT count(*), COALESCE(max(status), '') FROM message_purges WHERE context_id = $1`, convID).Scan(&auditCount, &auditStatus))
	require.Equal(t, 1, messageCount, "participant removal at the guard must preserve the message")
	require.Equal(t, 1, auditCount, "the failed purge retains its recovery audit")
	require.NotEqual(t, "completed", auditStatus, "a rejected purge must not be marked completed")
}
