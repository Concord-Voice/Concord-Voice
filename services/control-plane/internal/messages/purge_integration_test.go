package messages_test

// Integration tests for the channel/server bulk-purge endpoints (#1352).
// Backed by a real PostgreSQL + Redis via testhelpers (CI-authoritative;
// SetupTestServer fatals locally without a reachable test database).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func purgeChannelPath(channelID string) string {
	return "/api/v1/channels/" + channelID + "/messages"
}

func purgeServerPath(serverID string) string {
	return "/api/v1/servers/" + serverID + "/messages"
}

func countChannelMessages(t *testing.T, ts *testhelpers.TestServer, channelID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM messages WHERE channel_id = $1`, channelID).Scan(&n))
	return n
}

func countPurgeAudits(t *testing.T, ts *testhelpers.TestServer, contextID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM message_purges WHERE context_id = $1`, contextID).Scan(&n))
	return n
}

func TestPurgeChannel_ManageAllDeletesAllAndAudits(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "purge_owner")
	member := ts.CreateTestUser(t, "purge_member")
	serverID := ts.CreateTestServer(t, owner.ID, "purge-server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	for i := 0; i < 3; i++ {
		ts.CreateTestMessage(t, channelID, owner, fmt.Sprintf("owner-%d", i))
		ts.CreateTestMessage(t, channelID, member, fmt.Sprintf("member-%d", i))
	}
	require.Equal(t, 6, countChannelMessages(t, ts, channelID))

	w := ts.DoRequest(http.MethodDelete, purgeChannelPath(channelID),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		DeletedCount int `json:"deleted_count"`
		HiddenCount  int `json:"hidden_count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 6, resp.DeletedCount)
	assert.Equal(t, 0, resp.HiddenCount)
	assert.Equal(t, 0, countChannelMessages(t, ts, channelID))

	// Privacy-safe audit row: completed, correct count, channel context.
	var ctxType, status string
	var deleted int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT context_type, status, deleted_count FROM message_purges WHERE context_id = $1`,
		channelID).Scan(&ctxType, &status, &deleted))
	assert.Equal(t, "channel", ctxType)
	assert.Equal(t, "completed", status)
	assert.Equal(t, 6, deleted)
}

// TestPurgeChannel_ChannelLookupHonorsSynchronousTimeout proves the preflight
// query shares the bounded context used by the purge engine. Without it, a
// locked channels table can hold this HTTP request past the write deadline.
func TestPurgeChannel_ChannelLookupHonorsSynchronousTimeout(t *testing.T) {
	assertPurgeChannelPreflightTimeout(t, "channel lookup",
		`LOCK TABLE channels IN ACCESS EXCLUSIVE MODE`, http.StatusInternalServerError)
}

// TestPurgeChannel_PermissionResolutionHonorsSynchronousTimeout proves RBAC
// resolution shares the bounded context used by the purge engine.
func TestPurgeChannel_PermissionResolutionHonorsSynchronousTimeout(t *testing.T) {
	assertPurgeChannelPreflightTimeout(t, "permission resolution",
		`LOCK TABLE server_members IN ACCESS EXCLUSIVE MODE`, http.StatusInternalServerError)
}

func assertPurgeChannelPreflightTimeout(t *testing.T, preflight, lockQuery string, wantStatus int) {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "purge_timeout_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "purge-timeout-server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	lockTx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback() }()
	_, err = lockTx.Exec(lockQuery)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodDelete, purgeChannelPath(channelID),
		strings.NewReader(`{"range":"all"}`))
	req.Header = testhelpers.AuthHeaders(owner.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		ts.Router.ServeHTTP(w, req)
		responses <- w
	}()

	select {
	case w := <-responses:
		assert.Equal(t, wantStatus, w.Code, w.Body.String())
	case <-time.After(11 * time.Second):
		require.NoError(t, lockTx.Rollback())
		select {
		case <-responses:
		case <-time.After(time.Second):
			t.Fatal("purge request did not complete after releasing channel lock")
		}
		t.Fatalf("%s did not honor the synchronous purge timeout", preflight)
	}
}

func TestPurgeChannel_ManageOwnOnlyForcedToSelf(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "fs_owner")
	member := ts.CreateTestUser(t, "fs_member")
	serverID := ts.CreateTestServer(t, owner.ID, "fs-server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	ts.CreateTestMessage(t, channelID, owner, "owner-msg")
	ts.CreateTestMessage(t, channelID, member, "member-msg-1")
	ts.CreateTestMessage(t, channelID, member, "member-msg-2")

	// Plain member holds ManageOwnMessages (@all) but not ManageAllMessages:
	// the purge is forced to their own messages, even with range=all.
	w := ts.DoRequest(http.MethodDelete, purgeChannelPath(channelID),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var remaining int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM messages WHERE channel_id = $1 AND user_id = $2`,
		channelID, member.ID).Scan(&remaining))
	assert.Equal(t, 0, remaining, "member's own messages should be purged")
	assert.Equal(t, 1, countChannelMessages(t, ts, channelID), "owner's message must survive")
}

// TestPurgeChannel_ManageOwnRejectsForeignTarget locks the review finding that a
// ManageOwn-only actor asking to purge SOMEONE ELSE's messages must be DENIED — not
// silently redirected onto their own.
//
// The prior behaviour forced author=self for any target and returned 200, so a member
// POSTing target_user_id=<owner> irreversibly destroyed THEIR OWN messages while the
// response said success and the audit row named the owner (false Art.17 evidence
// against a third party). A nil target still legitimately means "my own messages" —
// that path is covered by TestPurgeChannel_ManageOwnOnlyForcedToSelf and unchanged.
func TestPurgeChannel_ManageOwnRejectsForeignTarget(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "ft_owner")
	member := ts.CreateTestUser(t, "ft_member")
	serverID := ts.CreateTestServer(t, owner.ID, "ft-server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	ts.CreateTestMessage(t, channelID, owner, "owner-msg")
	ts.CreateTestMessage(t, channelID, member, "member-msg")

	// Plain member (ManageOwn via @all, no ManageAll) explicitly targets the owner.
	w := ts.DoRequest(http.MethodDelete, purgeChannelPath(channelID),
		map[string]any{"range": "all", "target_user_id": owner.ID},
		testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	assert.Equal(t, 2, countChannelMessages(t, ts, channelID),
		"a denied foreign-target purge must delete NOTHING — least of all the actor's own messages")
	assert.Equal(t, 0, countPurgeAudits(t, ts, channelID), "denied purge must write no audit row")
}

// TestPurgeChannel_ViewDenyBlocksPurge locks the review finding that purge requires
// visibility: a channel-scoped DENY of PermViewTextChannels blocks the purge even
// when the actor holds ManageAllMessages there.
//
// Purge needs no message ID — it reaches every message in scope — so without a view
// gate an actor could irreversibly wipe a private channel they cannot even open.
func TestPurgeChannel_ViewDenyBlocksPurge(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "vd_owner")
	mod := ts.CreateTestUser(t, "vd_mod")
	serverID := ts.CreateTestServer(t, owner.ID, "vd-server")
	ts.AddMemberToServer(t, serverID, mod.ID, "member")
	modRole := ts.CreateTestRole(t, serverID, "Mods", 5, int64(rbac.ModeratorPermissions))
	ts.AssignRoleToUser(t, serverID, mod.ID, modRole)

	channelID := ts.CreateTestChannel(t, serverID, "private")
	ts.CreateTestMessage(t, channelID, owner, "unseeable-msg")

	// Deny VIEW only — the message-management bits stay granted.
	ts.CreateChannelOverride(t, channelID, "role", modRole, 0, int64(rbac.PermViewTextChannels))

	w := ts.DoRequest(http.MethodDelete, purgeChannelPath(channelID),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(mod.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	assert.Equal(t, 1, countChannelMessages(t, ts, channelID),
		"a channel the actor cannot view must not be purgeable")
	assert.Equal(t, 0, countPurgeAudits(t, ts, channelID), "denied purge must write no audit row")
}

// regression for #2344: the view gate must match the channel type. Granting text
// visibility must not authorize a destructive purge of a voice channel.
func TestPurgeChannel_VoiceViewDenyBlocksPurge(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "vvd_owner")
	mod := ts.CreateTestUser(t, "vvd_mod")
	serverID := ts.CreateTestServer(t, owner.ID, "vvd-server")
	ts.AddMemberToServer(t, serverID, mod.ID, "member")
	modRole := ts.CreateTestRole(t, serverID, "Mods", 5, int64(rbac.ModeratorPermissions))
	ts.AssignRoleToUser(t, serverID, mod.ID, modRole)

	channelID := ts.CreateVoiceChannel(t, serverID, "voice")
	ts.CreateTestMessage(t, channelID, owner, "voice-history")
	// A text-view grant must not defeat a voice-view deny on a voice channel.
	ts.CreateChannelOverride(t, channelID, "role", modRole,
		int64(rbac.PermViewTextChannels), int64(rbac.PermViewVoiceChannels))

	w := ts.DoRequest(http.MethodDelete, purgeChannelPath(channelID),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(mod.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, 1, countChannelMessages(t, ts, channelID),
		"a voice-view denial must leave the voice-channel history intact")
	assert.Equal(t, 0, countPurgeAudits(t, ts, channelID), "denied purge must write no audit row")
}

func TestPurgeChannel_NonMemberDeniedNoAudit(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "nm_owner")
	outsider := ts.CreateTestUser(t, "nm_outsider")
	serverID := ts.CreateTestServer(t, owner.ID, "nm-server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.CreateTestMessage(t, channelID, owner, "keep-me")

	w := ts.DoRequest(http.MethodDelete, purgeChannelPath(channelID),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, 1, countChannelMessages(t, ts, channelID))
	assert.Equal(t, 0, countPurgeAudits(t, ts, channelID), "denied purge must write no audit row")
}

func TestPurgeChannel_TimeRangeBoundary(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "tr_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "tr-server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	oldID := ts.CreateTestMessage(t, channelID, owner, "old-message")
	ts.CreateTestMessage(t, channelID, owner, "fresh-message")
	// Age one message beyond the 7d window (messages.created_at is TIMESTAMP,
	// migration 000006 — the per-table cast in the engine handles the type).
	_, err := ts.DB.Exec(
		`UPDATE messages SET created_at = NOW() - INTERVAL '8 days' WHERE id = $1`, oldID)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodDelete, purgeChannelPath(channelID),
		map[string]any{"range": "7d"}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var survivors int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM messages WHERE channel_id = $1 AND id = $2`,
		channelID, oldID).Scan(&survivors))
	assert.Equal(t, 1, survivors, "message older than the range must survive")
	assert.Equal(t, 1, countChannelMessages(t, ts, channelID))
}

func TestPurgeServer_ScopedToTargetServerOnly(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "sv_owner")
	serverA := ts.CreateTestServer(t, owner.ID, "server-a")
	serverB := ts.CreateTestServer(t, owner.ID, "server-b")
	chA1 := ts.CreateTestChannel(t, serverA, "a1")
	chA2 := ts.CreateTestChannel(t, serverA, "a2")
	chB1 := ts.CreateTestChannel(t, serverB, "b1")
	ts.CreateTestMessage(t, chA1, owner, "a1-msg")
	ts.CreateTestMessage(t, chA2, owner, "a2-msg")
	ts.CreateTestMessage(t, chB1, owner, "b1-msg")

	w := ts.DoRequest(http.MethodDelete, purgeServerPath(serverA),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, 0, countChannelMessages(t, ts, chA1))
	assert.Equal(t, 0, countChannelMessages(t, ts, chA2))
	assert.Equal(t, 1, countChannelMessages(t, ts, chB1), "other server must be untouched")

	var ctxType string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT context_type FROM message_purges WHERE context_id = $1`, serverA).Scan(&ctxType))
	assert.Equal(t, "server", ctxType)
}

// TestPurgeServer_ChannelDenyHonored regression-locks review finding M1: the
// whole-server purge must re-resolve authorization PER CHANNEL, so a
// channel_permission_overrides row denying ManageAllMessages on one channel
// protects that channel even when the actor holds ManageAll server-wide.
func TestPurgeServer_ChannelDenyHonored(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "cd_owner")
	mod := ts.CreateTestUser(t, "cd_mod")
	serverID := ts.CreateTestServer(t, owner.ID, "cd-server")
	ts.AddMemberToServer(t, serverID, mod.ID, "member")
	modRole := ts.CreateTestRole(t, serverID, "Mods", 5, int64(rbac.ModeratorPermissions))
	ts.AssignRoleToUser(t, serverID, mod.ID, modRole)

	chOpen := ts.CreateTestChannel(t, serverID, "open")
	chProtected := ts.CreateTestChannel(t, serverID, "protected")
	ts.CreateTestMessage(t, chOpen, owner, "open-msg")
	ts.CreateTestMessage(t, chProtected, owner, "protected-msg")

	// Channel-scoped DENY of both message-management bits for the mod's role.
	ts.CreateChannelOverride(t, chProtected, "role", modRole, 0,
		int64(rbac.PermManageAllMessages|rbac.PermManageOwnMessages))

	w := ts.DoRequest(http.MethodDelete, purgeServerPath(serverID),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(mod.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, 0, countChannelMessages(t, ts, chOpen), "permitted channel is purged")
	assert.Equal(t, 1, countChannelMessages(t, ts, chProtected),
		"channel with a scoped ManageAll deny must be SKIPPED by the server purge (M1)")
}

func TestPurgeServer_NonMemberDeniedNoAudit(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "svd_owner")
	outsider := ts.CreateTestUser(t, "svd_outsider")
	serverID := ts.CreateTestServer(t, owner.ID, "svd-server")
	chID := ts.CreateTestChannel(t, serverID, "general")
	ts.CreateTestMessage(t, chID, owner, "keep")

	w := ts.DoRequest(http.MethodDelete, purgeServerPath(serverID),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, 1, countChannelMessages(t, ts, chID))
	assert.Equal(t, 0, countPurgeAudits(t, ts, serverID))
}

// regression for #2344: a channel-less server is a valid empty scope. Its owner
// receives a completed zero-count purge with an audit row; an outsider cannot use
// that result to learn whether the server exists or has channels.
func TestPurgeServer_EmptyScopeAuthorizesBeforeCompletingAndAudits(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "empty_owner")
	outsider := ts.CreateTestUser(t, "empty_outsider")
	serverID := ts.CreateTestServer(t, owner.ID, "empty-server") // deliberately no channels

	t.Run("owner receives completed zero-count audit", func(t *testing.T) {
		w := ts.DoRequest(http.MethodDelete, purgeServerPath(serverID),
			map[string]any{"range": "all"}, testhelpers.AuthHeaders(owner.AccessToken))
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var resp struct {
			DeletedCount int `json:"deleted_count"`
			HiddenCount  int `json:"hidden_count"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Zero(t, resp.DeletedCount)
		assert.Zero(t, resp.HiddenCount)

		var status string
		var deleted int
		require.NoError(t, ts.DB.QueryRow(
			`SELECT status, deleted_count FROM message_purges WHERE context_id = $1`, serverID,
		).Scan(&status, &deleted))
		assert.Equal(t, "completed", status)
		assert.Zero(t, deleted)
	})

	t.Run("outsider receives no completion signal or audit", func(t *testing.T) {
		w := ts.DoRequest(http.MethodDelete, purgeServerPath(serverID),
			map[string]any{"range": "all"}, testhelpers.AuthHeaders(outsider.AccessToken))
		assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		assert.Equal(t, 1, countPurgeAudits(t, ts, serverID),
			"the owner's audit is the only audit row")
	})
}

func TestPurgeServer_EmptyScopeAllowsVoiceOnlyModerator(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "empty_voice_owner")
	member := ts.CreateTestUser(t, "empty_voice_member")
	serverID := ts.CreateTestServer(t, owner.ID, "empty-voice-server") // deliberately no channels
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID,
	).Scan(&allRoleID))
	_, err := ts.DB.Exec(`UPDATE roles SET permissions = 0 WHERE id = $1`, allRoleID)
	require.NoError(t, err)
	voiceModerator := ts.CreateTestRole(t, serverID, "Voice moderator", 5,
		int64(rbac.PermViewVoiceChannels|rbac.PermManageAllMessages))
	ts.AssignRoleToUser(t, serverID, member.ID, voiceModerator)

	w := ts.DoRequest(http.MethodDelete, purgeServerPath(serverID),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, countPurgeAudits(t, ts, serverID))
}

// regression for #2344: empty scopes must resolve authorization from committed
// state, not a permission bitfield cached before the actor was removed.
func TestPurgeServer_EmptyScopeRejectsRemovedMemberWithWarmCache(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "empty_cached_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "empty-cached-server") // deliberately no channels

	cache := rbac.NewPermissionCache(ts.Redis)
	require.NoError(t, cache.Set(context.Background(), serverID, owner.ID, "", rbac.OwnerPermissions))
	_, err := ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, owner.ID)
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodDelete, purgeServerPath(serverID),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Zero(t, countPurgeAudits(t, ts, serverID), "a removed member must not create a zero-count audit")
}

// --- Request-validation paths (400/404) ---

func TestPurgeChannel_RejectsInvalidInput(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "iv_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "iv-server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	hdrs := testhelpers.AuthHeaders(owner.AccessToken)

	t.Run("non-uuid channel id", func(t *testing.T) {
		w := ts.DoRequest(http.MethodDelete, "/api/v1/channels/not-a-uuid/messages",
			map[string]any{"range": "all"}, hdrs)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("missing range", func(t *testing.T) {
		w := ts.DoRequest(http.MethodDelete, purgeChannelPath(channelID), map[string]any{}, hdrs)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("unknown range value", func(t *testing.T) {
		w := ts.DoRequest(http.MethodDelete, purgeChannelPath(channelID),
			map[string]any{"range": "42y"}, hdrs)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("nonexistent channel", func(t *testing.T) {
		w := ts.DoRequest(http.MethodDelete, purgeChannelPath(uuid.NewString()),
			map[string]any{"range": "all"}, hdrs)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestPurgeServer_RejectsInvalidInput(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "ivs_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "ivs-server")
	hdrs := testhelpers.AuthHeaders(owner.AccessToken)

	t.Run("non-uuid server id", func(t *testing.T) {
		w := ts.DoRequest(http.MethodDelete, "/api/v1/servers/not-a-uuid/messages",
			map[string]any{"range": "all"}, hdrs)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("unknown range value", func(t *testing.T) {
		w := ts.DoRequest(http.MethodDelete, purgeServerPath(serverID),
			map[string]any{"range": "yesterday"}, hdrs)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

// TestPurgeChannel_LargeChannelBatched seeds 12k rows (≈3 batches at the 5000
// stride) and asserts the loop drains them all in one request — the AC6
// batched-delete verification at CI-friendly scale.
func TestPurgeChannel_LargeChannelBatched(t *testing.T) {
	if testing.Short() {
		t.Skip("large-channel purge skipped in -short mode")
	}
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "lg_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "lg-server")
	channelID := ts.CreateTestChannel(t, serverID, "big")

	_, err := ts.DB.Exec(`
		INSERT INTO messages (id, channel_id, user_id, content, created_at)
		SELECT gen_random_uuid(), $1, $2, 'bulk-' || g, NOW() - (g || ' seconds')::interval
		FROM generate_series(1, 12000) g`, channelID, owner.ID)
	require.NoError(t, err)
	require.Equal(t, 12000, countChannelMessages(t, ts, channelID))

	w := ts.DoRequest(http.MethodDelete, purgeChannelPath(channelID),
		map[string]any{"range": "all"}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		DeletedCount int `json:"deleted_count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 12000, resp.DeletedCount)
	assert.Equal(t, 0, countChannelMessages(t, ts, channelID))
}
