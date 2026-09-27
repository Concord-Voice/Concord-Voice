package voice

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errVoiceEffectCommitAcknowledgementLost = errors.New("injected voice effect commit acknowledgement lost")

func commitEffectNATSURL() string {
	if url := os.Getenv("NATS_URL"); url != "" {
		return url
	}
	return "nats://localhost:4222"
}

func newCommitFailureVoiceEffectHandler(
	t *testing.T, db *sql.DB, redisClient *redis.Client, effects *atomic.Int32,
) *Handler {
	t.Helper()
	log := logger.New("test")
	h := NewHandler(HandlerDeps{
		DB:       db,
		Hub:      websocket.NewHub(db, redisClient),
		Log:      log,
		Resolver: rbac.NewResolver(db, rbac.NewPermissionCache(redisClient), log),
	})
	// Model a successful database commit whose acknowledgement is lost. The
	// handler must treat this exactly like any other commit error and must not
	// emit an irreversible NATS/WebSocket effect.
	h.commitVoiceEffectTxForTest = func(tx *sql.Tx) error {
		require.NoError(t, tx.Commit())
		return errVoiceEffectCommitAcknowledgementLost
	}
	h.beforeVoiceEffectForTest = func() { effects.Add(1) }
	return h
}

func invokeCommitFailureVoiceEffect(
	t *testing.T, h *Handler, actorID, method, path string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", actorID)
		c.Next()
	})
	router.POST("/servers/:id/voice/:userId/disconnect", h.ServerDisconnect)
	router.POST("/servers/:id/voice/:userId/move", h.ServerMove)

	var payload *bytes.Reader
	if body == nil {
		payload = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		payload = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, payload)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func insertCommitFailureVoiceParticipant(t *testing.T, db *sql.DB, channelID, userID uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`, channelID, userID)
	require.NoError(t, err)
}

func commitFailureVoiceEffectFixture(t *testing.T) (db *sql.DB, redisClient *redis.Client, ownerID, memberID, serverID, fromChannelID, toChannelID uuid.UUID) {
	t.Helper()
	db, _ = dbtest.SetupTestDB(t)
	redisClient = redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), redisClient))
	ownerID = dbtest.CreateUser(t, db)
	memberID = dbtest.CreateUser(t, db)
	serverID = uuid.New()
	fromChannelID = uuid.New()
	everyoneRoleID := uuid.New()
	require.NoError(t, insertPendingAdmissionJoinFixture(db, ownerID, memberID, serverID, fromChannelID, everyoneRoleID))
	toChannelID = uuid.New()
	_, err := db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'destination', 'voice')`, toChannelID, serverID)
	require.NoError(t, err)
	return db, redisClient, ownerID, memberID, serverID, fromChannelID, toChannelID
}

func TestVoiceEffects_CommitAcknowledgementFailureSuppressesDispatch(t *testing.T) {
	t.Run("disconnect NATS command", func(t *testing.T) {
		db, redisClient, ownerID, memberID, serverID, fromChannelID, _ := commitFailureVoiceEffectFixture(t)
		insertCommitFailureVoiceParticipant(t, db, fromChannelID, memberID)

		var effects atomic.Int32
		h := newCommitFailureVoiceEffectHandler(t, db, redisClient, &effects)
		response := invokeCommitFailureVoiceEffect(
			t, h, ownerID.String(), http.MethodPost,
			"/servers/"+serverID.String()+"/voice/"+memberID.String()+"/disconnect", nil,
		)

		assert.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
		assert.Zero(t, effects.Load(), "failed commit acknowledgement must not dispatch NATS disconnect")
	})

	t.Run("ordinary move WebSocket signal", func(t *testing.T) {
		db, redisClient, ownerID, _, serverID, fromChannelID, toChannelID := commitFailureVoiceEffectFixture(t)
		insertCommitFailureVoiceParticipant(t, db, fromChannelID, ownerID)

		var effects atomic.Int32
		h := newCommitFailureVoiceEffectHandler(t, db, redisClient, &effects)
		response := invokeCommitFailureVoiceEffect(
			t, h, ownerID.String(), http.MethodPost,
			"/servers/"+serverID.String()+"/voice/"+ownerID.String()+"/move",
			map[string]string{"target_channel_id": toChannelID.String()},
		)

		assert.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
		assert.Zero(t, effects.Load(), "failed commit acknowledgement must not dispatch WebSocket move")
	})

	t.Run("temporary-grant move reset before grant lock", func(t *testing.T) {
		db, redisClient, ownerID, memberID, serverID, fromChannelID, toChannelID := commitFailureVoiceEffectFixture(t)
		insertCommitFailureVoiceParticipant(t, db, fromChannelID, memberID)
		var everyoneRoleID uuid.UUID
		require.NoError(t, db.QueryRow(
			`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID,
		).Scan(&everyoneRoleID))
		_, err := db.Exec(`
			INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
			VALUES ($1, $2, 'role', $3, 0, $4)
		`, uuid.New(), toChannelID, everyoneRoleID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice))
		require.NoError(t, err)

		var effects atomic.Int32
		h := newCommitFailureVoiceEffectHandler(t, db, redisClient, &effects)
		h.beforeTemporaryGrantForTest = func() {
			_, err := db.Exec(`UPDATE users SET credential_epoch = 'rotated-before-grant' WHERE id = $1`, ownerID)
			require.NoError(t, err)
		}
		response := invokeCommitFailureVoiceEffect(
			t, h, ownerID.String(), http.MethodPost,
			"/servers/"+serverID.String()+"/voice/"+memberID.String()+"/move",
			map[string]string{"target_channel_id": toChannelID.String()},
		)

		assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
		assert.Zero(t, effects.Load(), "a reset before the temporary-grant lock must not dispatch WebSocket move")
		var temporaryGrantExists bool
		require.NoError(t, db.QueryRow(
			`SELECT EXISTS(
				SELECT 1 FROM channel_permission_overrides
				WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND is_temporary
			)`, toChannelID, memberID,
		).Scan(&temporaryGrantExists))
		assert.False(t, temporaryGrantExists, "a reset before the temporary-grant lock must not create access")
	})
}

func TestServerMove_AmbiguousTemporaryGrantCommitSuppressesSignal(t *testing.T) {
	db, redisClient, ownerID, memberID, serverID, fromChannelID, toChannelID := commitFailureVoiceEffectFixture(t)
	insertCommitFailureVoiceParticipant(t, db, fromChannelID, memberID)
	var everyoneRoleID uuid.UUID
	require.NoError(t, db.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID,
	).Scan(&everyoneRoleID))
	_, err := db.Exec(`
		INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, 'role', $3, 0, $4)
	`, uuid.New(), toChannelID, everyoneRoleID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice))
	require.NoError(t, err)

	var effects atomic.Int32
	h := newCommitFailureVoiceEffectHandler(t, db, redisClient, &effects)
	publisher, err := natsclient.Connect(commitEffectNATSURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping exact-xmin successor delivery assertion", err)
	}
	t.Cleanup(func() {
		if closeErr := publisher.Close(); closeErr != nil {
			t.Errorf("close NATS publisher: %v", closeErr)
		}
	})
	observer, err := natsclient.Connect(commitEffectNATSURL())
	require.NoError(t, err)
	t.Cleanup(func() {
		if closeErr := observer.Close(); closeErr != nil {
			t.Errorf("close NATS observer: %v", closeErr)
		}
	})
	disconnects := make(chan struct{}, 1)
	subscription, err := observer.Subscribe(natsSubjectEnforceDisconnect, func([]byte) { disconnects <- struct{}{} })
	require.NoError(t, err)
	t.Cleanup(func() {
		if unsubscribeErr := subscription.Unsubscribe(); unsubscribeErr != nil {
			t.Errorf("unsubscribe NATS disconnect observer: %v", unsubscribeErr)
		}
	})
	require.NoError(t, observer.Flush())
	h.tempGrant.nats = publisher
	h.tempGrant.grantCommit = func(tx *sql.Tx) error {
		require.NoError(t, tx.Commit())
		_, refreshErr := db.Exec(`
			UPDATE channel_permission_overrides SET allow = allow
			WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2
			  AND is_temporary = TRUE`, toChannelID, memberID)
		require.NoError(t, refreshErr)
		return errVoiceEffectCommitAcknowledgementLost
	}
	response := invokeCommitFailureVoiceEffect(
		t, h, ownerID.String(), http.MethodPost,
		"/servers/"+serverID.String()+"/voice/"+memberID.String()+"/move",
		map[string]string{"target_channel_id": toChannelID.String()},
	)

	assert.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	assert.Zero(t, effects.Load(), "an ambiguous temporary-grant commit must not dispatch WebSocket move")
	var temporaryGrantExists bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS(
			SELECT 1 FROM channel_permission_overrides
			WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND is_temporary
		)`, toChannelID, memberID,
	).Scan(&temporaryGrantExists))
	assert.True(t, temporaryGrantExists, "an old xmin must not erase a newer temporary grant")
	select {
	case <-disconnects:
		t.Fatal("an exact-xmin miss must not force-disconnect a newer grant holder")
	case <-time.After(250 * time.Millisecond):
	}
}

func TestServerMove_TemporaryGrantCommitIsFinalDecision(t *testing.T) {
	t.Run("credential reset after committed grant still signals", func(t *testing.T) {
		db, redisClient, ownerID, memberID, serverID, fromChannelID, toChannelID := commitFailureVoiceEffectFixture(t)
		insertCommitFailureVoiceParticipant(t, db, fromChannelID, memberID)

		var effects atomic.Int32
		h := newCommitFailureVoiceEffectHandler(t, db, redisClient, &effects)
		h.commitVoiceEffectTxForTest = nil
		h.tempGrant.grantCommit = func(tx *sql.Tx) error {
			require.NoError(t, tx.Commit())
			_, err := db.Exec(`UPDATE users SET credential_epoch = 'rotated-after-grant' WHERE id = $1`, ownerID)
			return err
		}

		response := invokeCommitFailureVoiceEffect(
			t, h, ownerID.String(), http.MethodPost,
			"/servers/"+serverID.String()+"/voice/"+memberID.String()+"/move",
			map[string]string{"target_channel_id": toChannelID.String()},
		)

		assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Equal(t, int32(1), effects.Load(), "a reset after the committed grant cannot retroactively cancel its move")
	})

	t.Run("permanent deny remains fail closed without signal", func(t *testing.T) {
		db, redisClient, ownerID, memberID, serverID, fromChannelID, toChannelID := commitFailureVoiceEffectFixture(t)
		insertCommitFailureVoiceParticipant(t, db, fromChannelID, memberID)

		_, err := db.Exec(`
			INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
			VALUES ($1, $2, 'user', $3, 0, $4)
		`, uuid.New(), toChannelID, memberID, int64(rbac.PermViewVoiceChannels))
		require.NoError(t, err)

		var effects atomic.Int32
		h := newCommitFailureVoiceEffectHandler(t, db, redisClient, &effects)
		h.commitVoiceEffectTxForTest = nil
		response := invokeCommitFailureVoiceEffect(
			t, h, ownerID.String(), http.MethodPost,
			"/servers/"+serverID.String()+"/voice/"+memberID.String()+"/move",
			map[string]string{"target_channel_id": toChannelID.String()},
		)

		assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
		assert.Zero(t, effects.Load(), "a permanent deny must not signal a move")
	})

	t.Run("target leaving before grant lock receives no access or signal", func(t *testing.T) {
		db, redisClient, ownerID, memberID, serverID, fromChannelID, toChannelID := commitFailureVoiceEffectFixture(t)
		insertCommitFailureVoiceParticipant(t, db, fromChannelID, memberID)
		var everyoneRoleID uuid.UUID
		require.NoError(t, db.QueryRow(
			`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID,
		).Scan(&everyoneRoleID))
		_, err := db.Exec(`
			INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
			VALUES ($1, $2, 'role', $3, 0, $4)
		`, uuid.New(), toChannelID, everyoneRoleID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice))
		require.NoError(t, err)

		var effects atomic.Int32
		h := newCommitFailureVoiceEffectHandler(t, db, redisClient, &effects)
		h.beforeTemporaryGrantForTest = func() {
			_, err := db.Exec(`DELETE FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, fromChannelID, memberID)
			require.NoError(t, err)
		}
		response := invokeCommitFailureVoiceEffect(
			t, h, ownerID.String(), http.MethodPost,
			"/servers/"+serverID.String()+"/voice/"+memberID.String()+"/move",
			map[string]string{"target_channel_id": toChannelID.String()},
		)

		assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
		assert.Zero(t, effects.Load(), "a target that left before the grant lock must not receive a move signal")
		var temporaryGrantExists bool
		require.NoError(t, db.QueryRow(
			`SELECT EXISTS(
				SELECT 1 FROM channel_permission_overrides
				WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND is_temporary
			)`, toChannelID, memberID,
		).Scan(&temporaryGrantExists))
		assert.False(t, temporaryGrantExists, "a target that left before the grant lock must not receive temporary access")
	})
}
