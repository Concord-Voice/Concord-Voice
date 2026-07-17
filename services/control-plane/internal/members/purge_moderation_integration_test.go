package members_test

// Integration tests for the #1353 purge-on-ban/kick option. Backed by a real
// PostgreSQL + Redis via testhelpers (CI-authoritative; SetupTestServer fatals
// locally without a reachable test database). setupTS, banPath, and memberPath
// are defined in the sibling handlers_(integration_)test.go files.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func countChannelMessages(t *testing.T, ts *testhelpers.TestServer, channelID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE channel_id = $1`, channelID).Scan(&n))
	return n
}

func countPurgeAuditRows(t *testing.T, ts *testhelpers.TestServer, serverID, targetUserID, reason string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM message_purges WHERE server_id = $1 AND target_user_id = $2 AND reason = $3`,
		serverID, targetUserID, reason).Scan(&n))
	return n
}

func countAuthorMessages(t *testing.T, ts *testhelpers.TestServer, channelID, userID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE channel_id = $1 AND user_id = $2`, channelID, userID).Scan(&n))
	return n
}

type purgeRespBody struct {
	Message string `json:"message"`
	Purge   *struct {
		Requested   bool   `json:"requested"`
		Status      string `json:"status"`
		PurgedCount int    `json:"purged_count"`
	} `json:"purge"`
}

// ── Ban + purge (#1353) ───────────────────────────────────────────────────────

// Default path: purge_messages omitted -> no purge object, messages untouched.
func TestBanMember_PurgeDefaultOff(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "banp_owner1")
	victim := ts.CreateTestUser(t, "banp_victim1")
	serverID := ts.CreateTestServer(t, owner.ID, "S")
	ch := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, victim.ID, "member")
	ts.CreateTestMessage(t, ch, victim, "m1")
	ts.CreateTestMessage(t, ch, victim, "m2")

	w := ts.DoRequest(http.MethodPost, banPath(serverID, victim.ID),
		map[string]interface{}{}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body purgeRespBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Nil(t, body.Purge, "no purge object on the default path")
	assert.Equal(t, 2, countChannelMessages(t, ts, ch), "messages untouched")
}

// Authorized (owner) purge -> completed; victim's messages gone, a BYSTANDER's survive.
func TestBanMember_PurgeAuthorizedCompletes(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "banp_owner2")
	victim := ts.CreateTestUser(t, "banp_victim2")
	bystander := ts.CreateTestUser(t, "banp_bystander2")
	serverID := ts.CreateTestServer(t, owner.ID, "S")
	ch := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, victim.ID, "member")
	ts.AddMemberToServer(t, serverID, bystander.ID, "member")
	ts.CreateTestMessage(t, ch, victim, "m1")
	ts.CreateTestMessage(t, ch, victim, "m2")
	ts.CreateTestMessage(t, ch, bystander, "keep-me") // must survive the purge

	w := ts.DoRequest(http.MethodPost, banPath(serverID, victim.ID),
		map[string]interface{}{"purge_messages": true}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body purgeRespBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotNil(t, body.Purge)
	assert.True(t, body.Purge.Requested)
	assert.Equal(t, "completed", body.Purge.Status)
	assert.Equal(t, 2, body.Purge.PurgedCount, "only the victim's 2 messages counted")
	assert.Equal(t, 0, countAuthorMessages(t, ts, ch, victim.ID), "victim's messages purged")
	assert.Equal(t, 1, countAuthorMessages(t, ts, ch, bystander.ID), "bystander's message survives (target-scoped)")
	assert.Equal(t, 1, countChannelMessages(t, ts, ch), "only the bystander's message remains")
	assert.Equal(t, 1, countPurgeAuditRows(t, ts, serverID, victim.ID, "ban"))
}

// Actor holds Ban but not ManageAll -> ban succeeds, purge skipped, messages remain, no audit.
func TestBanMember_PurgeSkippedWhenUnauthorized(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "banp_owner3")
	mod := ts.CreateTestUser(t, "banp_mod3")
	victim := ts.CreateTestUser(t, "banp_victim3")
	serverID := ts.CreateTestServer(t, owner.ID, "S")
	ch := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, mod.ID, "member")
	ts.AddMemberToServer(t, serverID, victim.ID, "member")
	// Mod can ban (role position 5 > victim) but has NO ManageAllMessages.
	modRole := ts.CreateTestRole(t, serverID, "Bouncers", 5, int64(rbac.BasePermissions|rbac.PermBan))
	ts.AssignRoleToUser(t, serverID, mod.ID, modRole)
	ts.CreateTestMessage(t, ch, victim, "m1")
	ts.CreateTestMessage(t, ch, victim, "m2")

	w := ts.DoRequest(http.MethodPost, banPath(serverID, victim.ID),
		map[string]interface{}{"purge_messages": true}, testhelpers.AuthHeaders(mod.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code, "ban still succeeds (additive)")
	var body purgeRespBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotNil(t, body.Purge)
	assert.Equal(t, "skipped_unauthorized", body.Purge.Status)
	assert.Equal(t, 2, countChannelMessages(t, ts, ch), "messages NOT purged")
	assert.Equal(t, 0, countPurgeAuditRows(t, ts, serverID, victim.ID, "ban"), "no audit row when skipped")
}

// Fail-CLOSED moderation-purge rate limit (#1353 review): an exhausted per-actor budget skips
// the purge, but the ban still succeeds (additive). Exercises the real router-wired redis path.
func TestBanMember_PurgeRateLimited(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "banp_owner_rl")
	victim := ts.CreateTestUser(t, "banp_victim_rl")
	serverID := ts.CreateTestServer(t, owner.ID, "S")
	ch := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, victim.ID, "member")
	ts.CreateTestMessage(t, ch, victim, "m1")

	// Exhaust the actor's fail-closed moderation-purge budget (key mirrors applyPurgeOnModeration).
	rlKey := fmt.Sprintf("ratelimit:user:%s:purge-on-moderation", owner.ID)
	require.NoError(t, ts.Redis.Set(context.Background(), rlKey, "999", time.Hour).Err())

	w := ts.DoRequest(http.MethodPost, banPath(serverID, victim.ID),
		map[string]interface{}{"purge_messages": true}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code, "ban still succeeds (additive)")
	var body purgeRespBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotNil(t, body.Purge)
	assert.Equal(t, "skipped_rate_limited", body.Purge.Status)
	assert.Equal(t, 1, countChannelMessages(t, ts, ch), "messages NOT purged when rate-limited")
	assert.Equal(t, 0, countPurgeAuditRows(t, ts, serverID, victim.ID, "ban"), "no audit row when skipped")
}

// ── Kick + purge (#1353) ──────────────────────────────────────────────────────

// Moderator removal (owner kicks victim) with purge -> completed, messages gone, audit row.
func TestRemoveMember_PurgeAuthorizedCompletes(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "kickp_owner1")
	victim := ts.CreateTestUser(t, "kickp_victim1")
	serverID := ts.CreateTestServer(t, owner.ID, "S")
	ch := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, victim.ID, "member")
	ts.CreateTestMessage(t, ch, victim, "m1")

	w := ts.DoRequest(http.MethodDelete, memberPath(serverID, victim.ID),
		map[string]interface{}{"purge_messages": true}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body purgeRespBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotNil(t, body.Purge)
	assert.Equal(t, "completed", body.Purge.Status)
	assert.Equal(t, 0, countChannelMessages(t, ts, ch))
	assert.Equal(t, 1, countPurgeAuditRows(t, ts, serverID, victim.ID, "kick"))
}

// Self-leave with purge_messages=true -> flag ignored: no purge object, own messages remain.
func TestRemoveMember_SelfRemovalIgnoresPurge(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "kickp_owner2")
	member := ts.CreateTestUser(t, "kickp_self2")
	serverID := ts.CreateTestServer(t, owner.ID, "S")
	ch := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ts.CreateTestMessage(t, ch, member, "m1")

	w := ts.DoRequest(http.MethodDelete, memberPath(serverID, member.ID),
		map[string]interface{}{"purge_messages": true}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body purgeRespBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Nil(t, body.Purge, "self-leave must not trigger purge")
	assert.Equal(t, 1, countChannelMessages(t, ts, ch), "own messages remain")
}
