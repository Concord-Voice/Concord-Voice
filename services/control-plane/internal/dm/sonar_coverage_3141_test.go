package dm

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test3141DMVoiceAuthorityAndSnapshotCoverage(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	users := seedTopologyUsers(t, db, 3)
	var groupID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, created_by) VALUES (true, $1)
		RETURNING id`, users[0]).Scan(&groupID))
	_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id, role)
		VALUES ($1, $2, 'admin'), ($1, $3, 'member')`, groupID, users[0], users[1])
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		id   string
		want error
	}{
		{name: "admin is authorized", id: users[0].String()},
		{name: "member is unavailable", id: users[1].String(), want: dmblock.ErrUnavailable},
		{name: "outsider is unavailable", id: users[2].String(), want: dmblock.ErrUnavailable},
		{name: "missing participant is unavailable", id: "00000000-0000-0000-0000-000000000000", want: dmblock.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			err = revalidateDMVoiceEnforcementAuthority(context.Background(), tx, groupID, tc.id)
			require.NoError(t, tx.Rollback())
			if tc.want == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tc.want)
			}
		})
	}

	t.Run("snapshot and locked participants", func(t *testing.T) {
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { require.NoError(t, tx.Rollback()) }()
		snapshot, err := dmVoiceEnforcementSnapshotTx(context.Background(), tx, groupID, users[1].String())
		require.NoError(t, err)
		assert.False(t, snapshot.ServerMuted)
		assert.False(t, snapshot.ServerDeafened)
		assert.Positive(t, snapshot.AuthorizationRevision)
		ids, err := fetchParticipantIDsTx(context.Background(), tx, groupID)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{users[0].String(), users[1].String()}, ids)
		_, err = dmVoiceEnforcementSnapshotTx(context.Background(), tx, groupID, users[2].String())
		assert.Error(t, err)
	})

	t.Run("public snapshot", func(t *testing.T) {
		// The public reader takes the same users-before-conversation fence and
		// allocates the revision only after the participant snapshot is locked.
		publicSnapshot, err := ReadVoiceEnforcementSnapshot(context.Background(), db, groupID, users[1].String())
		require.NoError(t, err)
		assert.Positive(t, publicSnapshot.AuthorizationRevision)
	})
}

func Test3141ReadVoiceEnforcementSnapshotRejectsInvalidTopology(t *testing.T) {
	db, convID, _, target, _, _ := seedMemberRemovalFixture(t, false)

	_, err := ReadVoiceEnforcementSnapshot(context.Background(), db, convID, "not-a-uuid")
	assert.Error(t, err)

	_, err = ReadVoiceEnforcementSnapshot(
		context.Background(), db, convID, "00000000-0000-0000-0000-000000000099",
	)
	assert.Error(t, err, "a user outside the conversation must not receive an enforcement snapshot")

	_, err = ReadVoiceEnforcementSnapshot(
		context.Background(), db, "00000000-0000-0000-0000-000000000098", target.String(),
	)
	assert.Error(t, err, "a missing conversation must fail closed")
}

func Test3141DMVoiceTopologyErrorResponses(t *testing.T) {
	h := NewHandler(HandlerDeps{Log: logger.New("test")})
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "credential epoch", err: credepoch.ErrEpochMismatch, want: 401},
		{name: "membership unavailable", err: dmblock.ErrUnavailable, want: 403},
		{name: "unexpected database error", err: sql.ErrConnDone, want: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			h.respondDMVoiceTopologyEffectError(c, tc.err, "test failure", "test response")
			assert.Equal(t, tc.want, c.Writer.Status())
		})
	}
}

// A stale authorization must not activate a missing accepted lease or restore
// a missing exact tombstone. Both cases would otherwise resurrect authority.
func Test3141DMVoiceLeaseExactStateRequired(t *testing.T) {
	ctx := context.Background()
	conversationID := uuid.New()
	userID := uuid.New()
	callID := uuid.New()
	client := redistest.Client(t)
	require.NoError(t, redistest.Reset(ctx, client))

	require.ErrorIs(t,
		ActivateAcceptedDMVoiceCallLease(ctx, client, conversationID, callID, time.Minute),
		ErrDMVoiceCallLeaseConflict,
	)
	require.ErrorIs(t,
		RestoreAbortedDMVoiceCallReservation(ctx, client, VoiceCallLease{
			ConversationID:  conversationID,
			CallerUserID:    userID,
			CallID:          callID,
			MediaAuthorized: true,
		}),
		ErrDMVoiceCallLeaseConflict,
	)
}

func Test3141ValidateAddMemberTargetRejectsMissingUser(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(HandlerDeps{DB: db, Log: logger.New("test")})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	assert.False(t, h.validateAddMemberTarget(c, uuid.NewString(), uuid.NewString()))
	assert.Equal(t, http.StatusBadRequest, c.Writer.Status())
}

func Test3141DMVoiceValidationPaths(t *testing.T) {
	h := NewHandler(HandlerDeps{Cfg: &config.Config{JWTSecret: "test-secret"}, Log: logger.New("test")})
	validConversation := "00000000-0000-0000-0000-000000000001"
	validUser := "00000000-0000-0000-0000-000000000002"
	validTarget := "00000000-0000-0000-0000-000000000003"

	t.Run("enforcement rejects malformed actor", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set("user_id", "not-a-uuid")
		h.applyDMVoiceEnforcement(c, validConversation, validTarget, dmVoiceEnforcementChange{})
		assert.Equal(t, http.StatusForbidden, c.Writer.Status())
	})
	t.Run("enforcement rejects malformed target", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set("user_id", validUser)
		h.applyDMVoiceEnforcement(c, validConversation, "not-a-uuid", dmVoiceEnforcementChange{})
		assert.Equal(t, http.StatusForbidden, c.Writer.Status())
	})

	for _, tc := range []struct {
		name, conversation, target, actor string
		want                              int
	}{
		{name: "invalid conversation", conversation: "bad", target: validTarget, actor: validUser, want: http.StatusBadRequest},
		{name: "invalid target", conversation: validConversation, target: "bad", actor: validUser, want: http.StatusBadRequest},
		{name: "self target", conversation: validConversation, target: validUser, actor: validUser, want: http.StatusBadRequest},
	} {
		t.Run("user mute "+tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set("user_id", tc.actor)
			c.Params = gin.Params{{Key: "id", Value: tc.conversation}, {Key: "userId", Value: tc.target}}
			h.DMUserMute(c)
			assert.Equal(t, tc.want, c.Writer.Status())
		})
	}

	for _, tc := range []struct {
		name, conversation string
		want               int
	}{
		{name: "invalid IDs", conversation: "bad", want: http.StatusBadRequest},
		{name: "missing call ID", conversation: validConversation, want: http.StatusBadRequest},
	} {
		t.Run("abort "+tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set("user_id", validUser)
			c.Params = gin.Params{{Key: "id", Value: tc.conversation}}
			c.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
			h.AbortDMVoiceMediaAuthorization(c)
			assert.Equal(t, tc.want, c.Writer.Status())
		})
	}
}
