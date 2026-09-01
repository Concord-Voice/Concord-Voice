package servers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// disconnectServerAudience runs post-commit, so it must never panic or block a
// response. Both early returns are real states: a handler constructed without a
// hub, and a server whose only member was the owner already removed by cascade.
func TestDisconnectServerAudienceEarlyReturns(t *testing.T) {
	t.Run("no hub wired", func(t *testing.T) {
		h := &Handler{log: logger.New("test")}
		require.NotPanics(t, func() {
			h.disconnectServerAudience(context.Background(), []uuid.UUID{uuid.New()})
		})
	})

	t.Run("empty audience", func(t *testing.T) {
		h := &Handler{log: logger.New("test")}
		require.NotPanics(t, func() {
			h.disconnectServerAudience(context.Background(), nil)
		}, "an empty captured audience is a no-op, not an error")
	})
}

func TestDeleteServerReturnsGenericErrorWhenPreflightBeginFails(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://invalid")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	h := &Handler{db: db, log: logger.New("test")}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/servers/"+uuid.NewString(), nil)
	c.Params = gin.Params{{Key: "id", Value: uuid.NewString()}}
	c.Set("user_id", uuid.NewString())

	h.DeleteServer(c)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "Failed to delete server", body["error"])
}

func TestReconcileServerDeleteAudienceOversizedInvalidatesAllAudiences(t *testing.T) {
	hub := websocket.NewHub(nil, nil)
	h := &Handler{log: logger.New("test"), hub: hub}
	before := hub.PresenceAuthzEpochForTest()

	h.reconcileServerDeleteAudience(context.Background(), nil, true)

	require.Greater(t, hub.PresenceAuthzEpochForTest(), before)
}
