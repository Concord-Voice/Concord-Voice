package dm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test2907DMTopologyCreationAndRevalidation(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	users := seedTopologyUsers(t, db, 4)
	for _, user := range users[1:] {
		setTopologyPrivacy(t, db, user, dmPrivacyOpenToAll, false)
	}
	h := NewHandler(HandlerDeps{DB: db})
	ctx := context.Background()

	_, _, err := h.createOneOnOneConversation(ctx, "bad", users[1].String(), "")
	assert.Error(t, err)
	_, _, err = h.createOneOnOneConversation(ctx, users[0].String(), "bad", "")
	assert.Error(t, err)

	convID, created, err := h.createOneOnOneConversation(ctx, users[0].String(), users[1].String(), "")
	require.NoError(t, err)
	assert.True(t, created)
	again, created, err := h.createOneOnOneConversation(ctx, users[0].String(), users[1].String(), "")
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, convID, again)

	groupID, err := h.insertGroupConversation(ctx, nil, users[0].String(), []string{
		users[0].String(), users[1].String(), users[2].String(),
	}, "")
	require.NoError(t, err)
	assert.NotEmpty(t, groupID)
	_, err = h.insertGroupConversation(ctx, nil, users[0].String(), []string{users[0].String(), "bad"}, "")
	assert.Error(t, err)
}

func Test2907DMGroupAuthorityRevalidation(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	users := seedTopologyUsers(t, db, 3)
	var convID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, created_by) VALUES (true, $1) RETURNING id`, users[0]).Scan(&convID))
	_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id, role) VALUES ($1, $2, 'admin'), ($1, $3, 'member')`, convID, users[0], users[1])
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		user uuid.UUID
		want error
	}{
		{name: "admin", user: users[0]},
		{name: "member", user: users[1], want: errMemberRemovalStateDrifted},
		{name: "outsider", user: users[2], want: errMemberRemovalStateDrifted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			err = revalidateDeleteGroupAuthority(context.Background(), tx, convID, tc.user.String())
			require.NoError(t, tx.Rollback())
			if tc.want == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tc.want)
			}
		})
	}
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	assert.NoError(t, revalidateDeleteGroupAuthority(context.Background(), tx, convID, ""))
	assert.Error(t, revalidateDeleteGroupAuthority(context.Background(), tx, convID, "not-a-uuid"))
	assert.NoError(t, tx.Rollback())
}

func Test2907DMVoiceProofRejectsInvalidInputs(t *testing.T) {
	h := NewHandler(HandlerDeps{Cfg: &config.Config{JWTSecret: "secret"}})
	convID := uuid.New()
	callID := uuid.New()
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = request

	assert.False(t, h.validDMVoiceMediaAuthorizationProof(c, convID, callID))
	c.Request.Header.Set(dmVoiceMediaTimestampHeader, "not-a-timestamp")
	assert.False(t, h.validDMVoiceMediaAuthorizationProof(c, convID, callID))
	c.Request.Header.Set(dmVoiceMediaTimestampHeader, strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10))
	assert.False(t, h.validDMVoiceMediaAuthorizationProof(c, convID, callID))
}

func Test2907DMTopologyActorAndSetSemantics(t *testing.T) {
	actors, err := topologyActor("")
	assert.Nil(t, actors)
	assert.NoError(t, err)
	assert.Error(t, func() error {
		_, err := topologyActor("bad")
		return err
	}())
	left := []string{"b", "a"}
	right := []string{"a", "b"}
	assert.True(t, sameStringSet(left, right))
	assert.False(t, sameStringSet(left, []string{"a"}))
}
