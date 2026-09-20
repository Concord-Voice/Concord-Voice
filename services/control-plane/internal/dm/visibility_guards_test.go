package dm

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMVisibility_HideRejectsInvalidConversationIDBeforeDatabase(t *testing.T) {
	h := stepUpHandler(&fakeMFAVerifier{}, nil)
	c, w := testCtx()
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
	c.Set("user_id", uuid.NewString())

	h.HideConversation(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDMVisibility_HideClosedDatabaseFailsWithoutMutation(t *testing.T) {
	db := sql.OpenDB(failingConnector{})
	require.NoError(t, db.Close())
	h := stepUpHandler(&fakeMFAVerifier{}, db)
	c, w := testCtx()
	c.Params = gin.Params{{Key: "id", Value: uuid.NewString()}}
	c.Set("user_id", uuid.NewString())

	h.HideConversation(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestDMVisibility_ClearClosedDatabaseFailsWithoutMutation(t *testing.T) {
	db := sql.OpenDB(failingConnector{})
	require.NoError(t, db.Close())
	h := stepUpHandler(&fakeMFAVerifier{}, db)
	c, w := testCtx()
	c.Request = httptest.NewRequest(http.MethodPost, "/clear", strings.NewReader(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: uuid.NewString()}}
	c.Set("user_id", uuid.NewString())

	h.ClearConversation(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestDMVisibility_HideRejectsStaleCredentialEpochWithoutMutation(t *testing.T) {
	db := hiddenTestDB(t)
	convID, actor, _ := seedHiddenConv(t, db)
	oldEpoch := "epoch-before-hide"
	newEpoch := "epoch-after-hide"
	_, err := db.Exec(`UPDATE users SET credential_epoch = $1 WHERE id = $2`, newEpoch, actor)
	require.NoError(t, err)

	h := NewHandler(HandlerDeps{DB: db, Log: logger.NewWithWriter(io.Discard)})
	c, w := testCtx()
	c.Params = gin.Params{{Key: "id", Value: convID}}
	c.Set("user_id", actor)
	c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"cred_epoch": oldEpoch})
	h.HideConversation(c)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	var hiddenAt sql.NullTime
	require.NoError(t, db.QueryRow(`SELECT hidden_at FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, actor).Scan(&hiddenAt))
	assert.False(t, hiddenAt.Valid, "stale epoch must not mutate hide state")
}
