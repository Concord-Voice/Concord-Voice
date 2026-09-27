package messages

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func authorityFenceContext() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	return c, w
}

func TestMessageAuthorityFencesFailClosedOnMissingRowsAndDatabaseErrors(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	h := NewHandler(db, logger.New("test"), nil, rbac.NewResolver(db, nil, logger.New("test")), nil, nil)
	missing := uuid.NewString()

	t.Run("missing channel is forbidden", func(t *testing.T) {
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		c, w := authorityFenceContext()
		_, ok := h.lockMessageChannelTx(context.Background(), c, tx, messageChannelAuthorization{
			serverID: missing, channelID: missing, userID: missing, genericMsg: "send failed",
		})
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("channel query failure is internal error", func(t *testing.T) {
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c, w := authorityFenceContext()
		_, ok := h.lockMessageChannelTx(ctx, c, tx, messageChannelAuthorization{
			serverID: missing, channelID: missing, userID: missing, genericMsg: "send failed",
		})
		assert.False(t, ok)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})

	t.Run("permission lookup failure is internal error", func(t *testing.T) {
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c, w := authorityFenceContext()
		ok := h.authorizeMessageChannelTx(ctx, c, tx, messageChannelAuthorization{
			serverID: missing, channelID: missing, userID: missing, genericMsg: "send failed",
		}, "text")
		assert.False(t, ok)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})

	t.Run("non-member permission lookup is forbidden", func(t *testing.T) {
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		c, w := authorityFenceContext()
		ok := h.authorizeMessageChannelTx(context.Background(), c, tx, messageChannelAuthorization{
			serverID: missing, channelID: missing, userID: missing, genericMsg: "send failed",
		}, "text")
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("missing membership is forbidden", func(t *testing.T) {
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		c, w := authorityFenceContext()
		ok := h.lockCurrentMessageMembership(context.Background(), c, tx, missing, missing, "send failed")
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("membership query failure is internal error", func(t *testing.T) {
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c, w := authorityFenceContext()
		ok := h.lockCurrentMessageMembership(ctx, c, tx, missing, missing, "send failed")
		assert.False(t, ok)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})

	t.Run("timed out membership is forbidden", func(t *testing.T) {
		owner := dbtest.CreateUser(t, db)
		member := dbtest.CreateUser(t, db)
		serverID := uuid.NewString()
		channelID := uuid.NewString()
		_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'fence server', $2)`, serverID, owner)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role, timed_out_until) VALUES ($1, $2, 'member', NOW() + INTERVAL '1 minute')`, serverID, member)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'fence channel', 'text')`, channelID, serverID)
		require.NoError(t, err)

		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		c, w := authorityFenceContext()
		ok := h.lockCurrentMessageMembership(context.Background(), c, tx, serverID, member.String(), "send failed")
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("member without channel permission is forbidden", func(t *testing.T) {
		owner := dbtest.CreateUser(t, db)
		member := dbtest.CreateUser(t, db)
		serverID := uuid.NewString()
		channelID := uuid.NewString()
		_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'permission server', $2)`, serverID, owner)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, serverID, owner, member)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'permission channel', 'text')`, channelID, serverID)
		require.NoError(t, err)

		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		c, w := authorityFenceContext()
		ok := h.authorizeMessageChannelTx(context.Background(), c, tx, messageChannelAuthorization{
			serverID: serverID, channelID: channelID, userID: member.String(), required: rbac.PermSendMessages, genericMsg: "send failed",
		}, "text")
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// Regression: two message mutations used to both take FOR SHARE before their
// later UPDATE, creating a lock-upgrade deadlock. The second common fence must
// wait at the message row before either caller can start its write.
func TestChannelMessageMutationFenceTakesMessageWriteLock(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	owner := dbtest.CreateUser(t, db)
	serverID := uuid.NewString()
	channelID := uuid.NewString()
	messageID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'message lock server', $2)`, serverID, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner')`, serverID, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'message lock channel', 'text')`, channelID, serverID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, created_at, updated_at) VALUES ($1, $2, $3, 'content', 1, FALSE, NOW(), NOW())`, messageID, channelID, owner)
	require.NoError(t, err)

	h := NewHandler(db, logger.New("test"), nil, rbac.NewResolver(db, nil, logger.New("test")), nil, nil)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	c, _ := authorityFenceContext()
	c.Set("user_id", owner.String())
	_, _, _, allowed := h.lockChannelMessageMutationTx(c, tx, messageID, owner.String(), rbac.PermPinMessages, "pin failed")
	require.True(t, allowed)

	var txID int64
	require.NoError(t, tx.QueryRow(`SELECT txid_current()`).Scan(&txID))
	probe, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, probe.Close()) })

	type lockResult struct {
		allowed bool
		code    int
		err     error
	}
	second := make(chan lockResult, 1)
	go func() {
		tx2, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			second <- lockResult{err: err}
			return
		}
		defer func() { _ = tx2.Rollback() }()
		c2, w2 := authorityFenceContext()
		c2.Set("user_id", owner.String())
		_, _, _, ok := h.lockChannelMessageMutationTx(c2, tx2, messageID, owner.String(), rbac.PermPinMessages, "pin failed")
		if ok {
			err = tx2.Commit()
		}
		second <- lockResult{allowed: ok, code: w2.Code, err: err}
	}()

	dbtest.WaitForRowLockWaiter(t, probe, txID)
	require.NoError(t, tx.Commit())
	result := <-second
	require.NoError(t, result.err)
	assert.True(t, result.allowed, "the second writer may proceed once the first transaction releases its lock")
	assert.Equal(t, http.StatusOK, result.code)
}

func TestPreflightMessageDeleteFailClosedPaths(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	log := logger.New("test")
	owner := dbtest.CreateUser(t, db)
	actor := dbtest.CreateUser(t, db)
	serverID := uuid.NewString()
	channelID := uuid.NewString()
	messageID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'delete preflight server', $2)`, serverID, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, serverID, owner, actor)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'delete preflight channel', 'text')`, channelID, serverID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, created_at, updated_at) VALUES ($1, $2, $3, 'content', 1, FALSE, NOW(), NOW())`, messageID, channelID, owner)
	require.NoError(t, err)

	redisClient := redistest.Client(t)
	h := NewHandler(db, log, nil, rbac.NewResolver(db, rbac.NewPermissionCache(redisClient), log), nil, nil)

	t.Run("missing message returns not found", func(t *testing.T) {
		c, w := authorityFenceContext()
		_, ok := h.preflightMessageDelete(c, uuid.NewString(), actor.String())
		assert.False(t, ok)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("preflight database failure returns internal error", func(t *testing.T) {
		// A separate closed handle preserves the fixture for the remaining cases.
		failedDB, err := sql.Open("postgres", "postgres://localhost:5432/unused?sslmode=disable")
		require.NoError(t, err)
		require.NoError(t, failedDB.Close())
		failed := NewHandler(failedDB, log, nil, h.resolver, nil, nil)
		c, w := authorityFenceContext()
		_, ok := failed.preflightMessageDelete(c, messageID, actor.String())
		assert.False(t, ok)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})

	for _, tc := range []struct {
		name string
		user string
	}{
		{"author permission lookup failure", owner.String()},
		{"manage-all permission lookup failure", actor.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			c, w := authorityFenceContext()
			c.Request = c.Request.WithContext(ctx)
			_, ok := h.preflightMessageDelete(c, messageID, tc.user)
			assert.False(t, ok)
			assert.Equal(t, http.StatusInternalServerError, w.Code)
		})
	}

	t.Run("member without delete permission is forbidden", func(t *testing.T) {
		secondMessageID := uuid.NewString()
		_, err := db.Exec(`INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, created_at, updated_at) VALUES ($1, $2, $3, 'member content', 1, FALSE, NOW(), NOW())`, secondMessageID, channelID, actor)
		require.NoError(t, err)
		c, w := authorityFenceContext()
		_, ok := h.preflightMessageDelete(c, secondMessageID, actor.String())
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("authorized foreign author cannot update", func(t *testing.T) {
		roleID := uuid.NewString()
		_, err := db.Exec(`INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed) VALUES ($1, $2, 'message editor', 1, $3, FALSE, FALSE)`, roleID, serverID, int64(rbac.PermManageOwnMessages|rbac.PermViewTextChannels))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`, serverID, actor, roleID)
		require.NoError(t, err)
		c, w := authorityFenceContext()
		_, ok := h.updateMessageCiphertext(c, messageID, channelID, actor.String(), UpdateMessageRequest{Content: "ciphertext", KeyVersion: 1})
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}
