package rbac_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type supersessionVoiceEnforcer struct{ rechecked int }

func (e *supersessionVoiceEnforcer) RecheckUser(string, string)    {}
func (e *supersessionVoiceEnforcer) RecheckServer(string)          {}
func (e *supersessionVoiceEnforcer) DisconnectUser(string, string) {}
func (e *supersessionVoiceEnforcer) RecheckChannel(string, string) { e.rechecked++ }

type categoryCaptureScopePlan struct{}

func (categoryCaptureScopePlan) HasWork() bool { return false }

type categoryCaptureScopeRecorder struct{ prepared [][]string }

func (r *categoryCaptureScopeRecorder) PrepareCapture(_ context.Context, _ string, channelIDs []string, _ *string) (rbac.PresenceRecheckPlan, error) {
	r.prepared = append(r.prepared, append([]string{}, channelIDs...))
	return categoryCaptureScopePlan{}, nil
}

func (*categoryCaptureScopeRecorder) CaptureVisibility(context.Context, *sql.Tx, rbac.PresenceRecheckPlan) error {
	return nil
}

func (*categoryCaptureScopeRecorder) Execute(rbac.PresenceRecheckPlan)         {}
func (*categoryCaptureScopeRecorder) Abandon(rbac.PresenceRecheckPlan, string) {}

func newAcknowledgementLostRoleHandler(t *testing.T, ts *testhelpers.TestServer, cancelRequest context.CancelFunc) *rbac.Handler {
	t.Helper()
	cache := rbac.NewPermissionCache(ts.Redis)
	h := rbac.NewHandler(ts.DB, logger.New("test"), ts.Redis, nil,
		rbac.NewResolver(ts.DB, cache, logger.New("test")), cache, nil)
	rbac.SetAuthorityCommitForTest(h, func(tx *sql.Tx) error {
		require.NoError(t, tx.Commit())
		cancelRequest()
		return errors.New("authority commit acknowledgement lost")
	})
	return h
}

func ambiguousRoleMutationRequest(ctx context.Context, t *testing.T, handler func(*gin.Context), method string, params gin.Params, actorID string, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = params
	c.Set("user_id", actorID)
	c.Request = httptest.NewRequest(method, "/", bytes.NewBufferString(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	return w
}

// An acknowledgement-lost role mutation returns before the normal post-commit
// cache invalidation. The mutation may have committed, so the stale Redis grant
// must be removed on this fail-closed branch as well.
func TestRoleMutation_AmbiguousCommitInvalidatesPermissionCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		invoke func(t *testing.T, ts *testhelpers.TestServer, ctx context.Context, h *rbac.Handler, ownerID, memberID, serverID string)
	}{
		{
			name: "update role invalidates server",
			invoke: func(t *testing.T, ts *testhelpers.TestServer, ctx context.Context, h *rbac.Handler, ownerID, memberID, serverID string) {
				roleID := ts.CreateTestRole(t, serverID, "ambiguous-update", 1, int64(rbac.PermViewVoiceChannels))
				ts.AssignRoleToUser(t, serverID, memberID, roleID)
				w := ambiguousRoleMutationRequest(ctx, t, h.UpdateRole, http.MethodPatch,
					gin.Params{{Key: "id", Value: serverID}, {Key: "role_id", Value: roleID}}, ownerID, `{"permissions":"0"}`)
				require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
			},
		},
		{
			name: "delete role invalidates server",
			invoke: func(t *testing.T, ts *testhelpers.TestServer, ctx context.Context, h *rbac.Handler, ownerID, memberID, serverID string) {
				roleID := ts.CreateTestRole(t, serverID, "ambiguous-delete", 1, int64(rbac.PermViewVoiceChannels))
				ts.AssignRoleToUser(t, serverID, memberID, roleID)
				w := ambiguousRoleMutationRequest(ctx, t, h.DeleteRole, http.MethodDelete,
					gin.Params{{Key: "id", Value: serverID}, {Key: "role_id", Value: roleID}}, ownerID, "")
				require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
			},
		},
		{
			name: "assign role invalidates target",
			invoke: func(t *testing.T, ts *testhelpers.TestServer, ctx context.Context, h *rbac.Handler, ownerID, memberID, serverID string) {
				roleID := ts.CreateTestRole(t, serverID, "ambiguous-assign", 1, int64(rbac.PermViewVoiceChannels))
				w := ambiguousRoleMutationRequest(ctx, t, h.AssignRole, http.MethodPost,
					gin.Params{{Key: "id", Value: serverID}, {Key: "user_id", Value: memberID}}, ownerID, `{"role_id":"`+roleID+`"}`)
				require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
			},
		},
		{
			name: "unassign role invalidates target",
			invoke: func(t *testing.T, ts *testhelpers.TestServer, ctx context.Context, h *rbac.Handler, ownerID, memberID, serverID string) {
				roleID := ts.CreateTestRole(t, serverID, "ambiguous-unassign", 1, int64(rbac.PermViewVoiceChannels))
				ts.AssignRoleToUser(t, serverID, memberID, roleID)
				w := ambiguousRoleMutationRequest(ctx, t, h.UnassignRole, http.MethodDelete,
					gin.Params{{Key: "id", Value: serverID}, {Key: "user_id", Value: memberID}, {Key: "role_id", Value: roleID}}, ownerID, "")
				require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, owner, member, serverID := setupOwnerAndMember(t)
			requestCtx, cancelRequest := context.WithCancel(ctx)
			t.Cleanup(cancelRequest)
			cacheKey := "perm:" + serverID + ":" + member.ID
			testhelpers.PublishPermissionCache(t, ts.Redis, serverID, member.ID, "", rbac.PermManageRoles)

			tc.invoke(t, ts, requestCtx, newAcknowledgementLostRoleHandler(t, ts, cancelRequest), owner.ID, member.ID, serverID)

			exists, err := ts.Redis.Exists(ctx, cacheKey).Result()
			require.NoError(t, err)
			assert.Zero(t, exists, "ambiguous commit must invalidate the stale permission grant")
		})
	}
}

func TestUpsertChannelOverride_AmbiguousRetainedViewRechecksAuthority(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "supersession-owner")
	member := ts.CreateTestUser(t, "supersession-member")
	serverID := ts.CreateTestServer(t, owner.ID, "Supersession ambiguity")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "ambiguous-retained-view")

	_, err := ts.DB.Exec(`
		INSERT INTO channel_permission_overrides
			(id, channel_id, target_type, target_id, allow, deny, is_temporary, temporary_reason, granted_at)
		VALUES ($1, $2, 'user', $3, $4, 0, TRUE, 'move_granted', NOW())`,
		uuid.NewString(), channelID, member.ID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice),
	)
	require.NoError(t, err)

	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, logger.New("test"))
	// A read publishes only once both generations exist (#3453 seed-and-skip);
	// without them nothing would be cached and the invalidation asserted below
	// would pass whether or not it ran.
	testhelpers.SeedPermissionGenerations(t, ts.Redis, serverID, member.ID)
	before, err := resolver.GetEffectivePermissions(context.Background(), serverID, member.ID, channelID)
	require.NoError(t, err)
	require.True(t, before.Has(rbac.PermJoinVoice), "the pre-commit cache must contain the temporary grant")
	cachedBefore, cached, _ := cache.Get(context.Background(), serverID, member.ID, channelID)
	require.True(t, cached, "the pre-commit cache must contain the temporary grant")
	require.True(t, cachedBefore.Has(rbac.PermJoinVoice))

	enforcer := &supersessionVoiceEnforcer{}
	h := rbac.NewHandler(ts.DB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)
	h.SetVoiceEnforcer(enforcer)
	ackLost := errors.New("authority commit acknowledgement lost")
	rbac.SetAuthorityCommitForTest(h, func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return ackLost
	})

	body, err := json.Marshal(rbac.UpsertOverrideRequest{
		TargetType: "user", TargetID: member.ID,
		Allow: int64(rbac.PermViewVoiceChannels), Deny: int64(rbac.PermJoinVoice),
	})
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: channelID}}
	c.Set("user_id", owner.ID)
	c.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.UpsertChannelOverride(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, 1, enforcer.rechecked, "ambiguous retained-VIEW supersession must recheck voice authority")
	after, err := resolver.GetEffectivePermissions(context.Background(), serverID, member.ID, channelID)
	require.NoError(t, err)
	assert.True(t, after.Has(rbac.PermViewVoiceChannels))
	assert.False(t, after.Has(rbac.PermJoinVoice), "safe cache invalidation must discard the temporary JOIN grant")

	var isTemporary bool
	var revocationCount int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT is_temporary FROM channel_permission_overrides
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`, channelID, member.ID,
	).Scan(&isTemporary))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1`, channelID,
	).Scan(&revocationCount))
	assert.False(t, isTemporary)
	assert.Zero(t, revocationCount, "retained VIEW must not rotate an unconfirmed local epoch")
}

func TestDeleteChannelOverride_AmbiguousCommitFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "ambiguous-delete-owner")
	member := ts.CreateTestUser(t, "ambiguous-delete-member")
	serverID := ts.CreateTestServer(t, owner.ID, "Ambiguous delete")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "ambiguous-delete-channel")
	overrideID := uuid.NewString()
	_, err := ts.DB.Exec(`
		INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		VALUES ($1, $2, 'user', $3, $4, 0)`, overrideID, channelID, member.ID, int64(rbac.PermViewVoiceChannels))
	require.NoError(t, err)

	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, logger.New("test"))
	_, err = resolver.GetEffectivePermissions(context.Background(), serverID, member.ID, channelID)
	require.NoError(t, err)
	enforcer := &supersessionVoiceEnforcer{}
	h := rbac.NewHandler(ts.DB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)
	h.SetVoiceEnforcer(enforcer)
	rbac.SetAuthorityCommitForTest(h, func(tx *sql.Tx) error {
		require.NoError(t, tx.Commit())
		return errors.New("authority commit acknowledgement lost")
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: channelID}, {Key: "override_id", Value: overrideID}}
	c.Set("user_id", owner.ID)
	c.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
	h.DeleteChannelOverride(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, 1, enforcer.rechecked, "ambiguous delete must recover voice authority")
	var remaining int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_permission_overrides WHERE id = $1`, overrideID).Scan(&remaining))
	assert.Zero(t, remaining, "the committed delete must remain durable despite lost acknowledgement")
}

func TestCategorySyncMutations_AmbiguousCommitUseOnlyFailClosedRecovery(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "category-ambiguity-owner")
	member := ts.CreateTestUser(t, "category-ambiguity-member")
	serverID := ts.CreateTestServer(t, owner.ID, "Category ambiguity")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	categoryID := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'ambiguous-category', 0)`, categoryID, serverID)
	require.NoError(t, err)
	syncedChannelID := ts.CreateVoiceChannel(t, serverID, "ambiguous-category-synced")
	_, err = ts.DB.Exec(`UPDATE channels SET group_id = $1, sync_permissions = TRUE WHERE id = $2`, categoryID, syncedChannelID)
	require.NoError(t, err)

	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, logger.New("test"))
	enforcer := &supersessionVoiceEnforcer{}
	h := rbac.NewHandler(ts.DB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)
	h.SetVoiceEnforcer(enforcer)
	rbac.SetAuthorityCommitForTest(h, func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return errors.New("authority commit acknowledgement lost")
	})

	call := func(method string, params gin.Params, body interface{}, handler func(*gin.Context)) *httptest.ResponseRecorder {
		payload, marshalErr := json.Marshal(body)
		require.NoError(t, marshalErr)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = params
		c.Set("user_id", owner.ID)
		c.Request = httptest.NewRequest(method, "/", bytes.NewReader(payload))
		c.Request.Header.Set("Content-Type", "application/json")
		handler(c)
		return w
	}

	gin.SetMode(gin.TestMode)
	w := call(http.MethodPut, gin.Params{{Key: "id", Value: categoryID}}, rbac.UpsertOverrideRequest{
		TargetType: "user", TargetID: member.ID, Deny: int64(rbac.PermJoinVoice),
	}, h.UpsertCategoryOverride)
	require.Equal(t, http.StatusInternalServerError, w.Code)

	var overrideID string
	require.NoError(t, ts.DB.QueryRow(`SELECT id FROM category_permission_overrides WHERE category_id = $1`, categoryID).Scan(&overrideID))
	w = call(http.MethodDelete, gin.Params{{Key: "id", Value: categoryID}, {Key: "override_id", Value: overrideID}}, map[string]string{}, h.DeleteCategoryOverride)
	require.Equal(t, http.StatusInternalServerError, w.Code)

	unsyncedChannelID := ts.CreateVoiceChannel(t, serverID, "ambiguous-category-unsynced")
	_, err = ts.DB.Exec(`UPDATE channels SET group_id = $1, sync_permissions = FALSE WHERE id = $2`, categoryID, unsyncedChannelID)
	require.NoError(t, err)
	w = call(http.MethodPut, gin.Params{{Key: "id", Value: unsyncedChannelID}}, map[string]bool{"sync_permissions": true}, h.SetChannelPermissionSync)
	require.Equal(t, http.StatusInternalServerError, w.Code)

	var syncEnabled bool
	require.NoError(t, ts.DB.QueryRow(`SELECT sync_permissions FROM channels WHERE id = $1`, unsyncedChannelID).Scan(&syncEnabled))
	assert.True(t, syncEnabled, "the ambiguous enable commit may have won, so recovery must recheck rather than claim failure")
	assert.Equal(t, 3, enforcer.rechecked, "upsert, delete, and enable each run fail-closed channel recovery after acknowledgement loss")
}

func TestUpsertCategoryOverride_RetriesWhenLockedSyncedSetChanged(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "category-race-owner")
	member := ts.CreateTestUser(t, "category-race-member")
	serverID := ts.CreateTestServer(t, owner.ID, "Category set race")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	categoryID := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'race-category', 0)`, categoryID, serverID)
	require.NoError(t, err)
	channelID := ts.CreateVoiceChannel(t, serverID, "race-synced-channel")
	_, err = ts.DB.Exec(`UPDATE channels SET group_id = $1, sync_permissions = TRUE WHERE id = $2`, categoryID, channelID)
	require.NoError(t, err)

	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, logger.New("test"))
	h := rbac.NewHandler(ts.DB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)
	preflightReached := make(chan struct{})
	var preflightOnce sync.Once
	rbac.SetSyncedCategoryPreflightForTest(h, func() { preflightOnce.Do(func() { close(preflightReached) }) })

	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, rbac.LockServerVisibilityCapture(context.Background(), tx, serverID))
	_, err = tx.Exec(`UPDATE channels SET group_id = NULL WHERE id = $1`, channelID)
	require.NoError(t, err)

	body, err := json.Marshal(rbac.UpsertOverrideRequest{
		TargetType: "user", TargetID: member.ID, Deny: int64(rbac.PermJoinVoice),
	})
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: categoryID}}
		c.Set("user_id", owner.ID)
		c.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpsertCategoryOverride(c)
		completed <- w
	}()

	<-preflightReached
	require.NoError(t, tx.Commit(), "the handler has preflighted the old set and is blocked on its visibility lock")
	w := <-completed
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var childRows int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1`, channelID).Scan(&childRows))
	assert.Zero(t, childRows, "the retry must write only the locked replacement set, not the stale preflight child")
}

func TestUpsertCategoryOverride_RetriesWhenLockedVoiceSubsetChanged(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "category-type-race-owner")
	member := ts.CreateTestUser(t, "category-type-race-member")
	serverID := ts.CreateTestServer(t, owner.ID, "Category voice subset race")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	categoryID := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'voice-subset-race-category', 0)`, categoryID, serverID)
	require.NoError(t, err)
	channelID := ts.CreateTestChannel(t, serverID, "voice-subset-race-channel")
	_, err = ts.DB.Exec(`UPDATE channels SET group_id = $1, sync_permissions = TRUE WHERE id = $2`, categoryID, channelID)
	require.NoError(t, err)

	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, logger.New("test"))
	h := rbac.NewHandler(ts.DB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)
	capture := &categoryCaptureScopeRecorder{}
	h.SetPresenceRecheck(capture)
	preflightReached := make(chan struct{})
	var preflightOnce sync.Once
	rbac.SetSyncedCategoryPreflightForTest(h, func() { preflightOnce.Do(func() { close(preflightReached) }) })

	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, rbac.LockServerVisibilityCapture(context.Background(), tx, serverID))
	_, err = tx.Exec(`UPDATE channels SET type = 'voice' WHERE id = $1`, channelID)
	require.NoError(t, err)

	body, err := json.Marshal(rbac.UpsertOverrideRequest{
		TargetType: "user", TargetID: member.ID, Deny: int64(rbac.PermJoinVoice),
	})
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: categoryID}}
		c.Set("user_id", owner.ID)
		c.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpsertCategoryOverride(c)
		completed <- w
	}()

	<-preflightReached
	require.NoError(t, tx.Commit(), "the handler has preflighted the text-only set and is blocked on its visibility lock")
	w := <-completed
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, [][]string{{}, {channelID}}, capture.prepared,
		"the type change must force a retry whose capture scope includes the new voice child")
}
