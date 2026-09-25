package messages_test

// regression: REST send skipped the View check WebSocket send enforces (PR #3433)
//
// Oracle: A server member whose effective permissions in a text channel
// include PermSendMessages but NOT PermViewTextChannels receives 403 from
// POST /api/v1/messages, and no message row is written for that channel.
//
// REST send (SendMessage -> checkSendAccess, internal/messages/handlers.go)
// checks only rbac.PermSendMessages on the channel-scoped effective
// permissions. The WebSocket send path (authorizeMessageSend,
// internal/websocket/hub.go) requires BOTH the channel-type view bit and
// PermSendMessages. A channel_permission_overrides row that denies View
// while leaving Send granted via the base role therefore makes a channel
// silently writable over REST despite being invisible.

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// countMessagesByChannelAndUser returns how many rows in `messages` were
// written by userID in channelID -- the "no message row is written" half of
// the oracle.
func countMessagesByChannelAndUser(t *testing.T, ts *testhelpers.TestServer, channelID, userID string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE channel_id = $1 AND user_id = $2`,
		channelID, userID).Scan(&n))
	return n
}

// createTestVoiceChannel inserts a channel of type "voice", mirroring
// TestServer.CreateTestChannel's INSERT exactly (same columns: id, server_id,
// name, type -- internal/testhelpers/testserver.go:339) except for the type
// literal. No testhelpers variant that takes an explicit channel type exists
// (grepped internal/testhelpers for CreateTestChannelWithType /
// CreateTestVoiceChannel), so this is a local, test-file-only insert rather
// than a production helper change.
func createTestVoiceChannel(t *testing.T, ts *testhelpers.TestServer, serverID, name string) string {
	t.Helper()
	channelID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, 'voice')`,
		channelID, serverID, name,
	)
	require.NoError(t, err, "failed to create test voice channel")
	return channelID
}

// TestSendMessage_RESTDeniedViewTextChannel_Forbidden is the regression test.
// The member's base "member" role grants PermSendMessages (part of
// rbac.BasePermissions), but a channel-scoped override denies
// PermViewTextChannels only. REST send must refuse this exactly like the
// WebSocket path does.
func TestSendMessage_RESTDeniedViewTextChannel_Forbidden(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "viewgateowner")
	member := ts.CreateTestUser(t, "viewgatemember")
	serverID := ts.CreateTestServer(t, owner.ID, "View Gate Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "secret")

	// Deny View only; Send stays granted via the base "member" role
	// (rbac.BasePermissions includes PermSendMessages).
	ts.CreateChannelOverride(t, channelID, "user", member.ID, 0, int64(rbac.PermViewTextChannels))

	// Fixture-validity guard (vacuity mode 1): resolve the member's real
	// effective channel permissions the same way the handler does, and
	// confirm the override produced exactly the permission split the oracle
	// describes -- Send granted, View denied. Without this, a broken
	// override insert (wrong column, wrong target) would make test A "pass"
	// for the wrong reason (falling through to an unrelated 403, e.g.
	// not-a-member) rather than reproducing the bug.
	log := logger.New("test")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	effective, err := resolver.ResolveEffectivePermissionsUncached(t.Context(), serverID, member.ID, channelID)
	require.NoError(t, err, "fixture guard: resolving effective channel permissions must not error")
	require.True(t, effective.Has(rbac.PermSendMessages),
		"fixture guard: member must hold PermSendMessages for this to be a View-gate regression, not a Send denial")
	require.False(t, effective.Has(rbac.PermViewTextChannels),
		"fixture guard: member must NOT hold PermViewTextChannels for this to be a View-gate regression")

	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code,
		"member denied View Text Channels must not be able to send over REST")
	assert.Equal(t, 0, countMessagesByChannelAndUser(t, ts, channelID, member.ID),
		"member denied View Text Channels must not have a message row written for that channel")
}

// TestSendMessage_RESTViewTextChannelControl_Created is the positive
// control: identical setup with NO override row. It proves the member
// really holds Send via the base role and that the harness reaches the
// handler and inserts a row on success, so a red result in test A cannot be
// blamed on broken fixtures shared between the two tests.
func TestSendMessage_RESTViewTextChannelControl_Created(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "viewgatectrlowner")
	member := ts.CreateTestUser(t, "viewgatectrlmember")
	serverID := ts.CreateTestServer(t, owner.ID, "View Gate Control Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code,
		"member with default base permissions and no override must be able to send over REST")
	assert.Equal(t, 1, countMessagesByChannelAndUser(t, ts, channelID, member.ID),
		"a successful send must write exactly one message row for that channel")
}

// TestSendMessage_RESTDeniedSendOnly_Forbidden is an existing-behaviour
// guard: denying only PermSendMessages (View stays granted) must still be
// refused. This pins that the fix for the View gate does not regress or
// replace the existing Send check.
func TestSendMessage_RESTDeniedSendOnly_Forbidden(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "sendonlydenyowner")
	member := ts.CreateTestUser(t, "sendonlydenymember")
	serverID := ts.CreateTestServer(t, owner.ID, "Send Only Deny Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	ts.CreateChannelOverride(t, channelID, "user", member.ID, 0, int64(rbac.PermSendMessages))

	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code,
		"member denied Send Messages must not be able to send over REST")
	assert.Equal(t, 0, countMessagesByChannelAndUser(t, ts, channelID, member.ID),
		"member denied Send Messages must not have a message row written for that channel")
}

// TestSendMessage_RESTDeniedViewVoiceChannel_Forbidden covers vacuity mode 3
// (a violating branch that is never executed): the fix must select the view
// bit BY CHANNEL TYPE, mirroring internal/websocket/hub.go's
// channelContext.viewPermission() ("text"/"bulletin" -> PermViewTextChannels,
// "voice" -> PermViewVoiceChannels). A fix hard-coded to PermViewTextChannels
// would pass test A while leaving a voice channel's View gate unenforced over
// REST -- this exercises that branch specifically.
func TestSendMessage_RESTDeniedViewVoiceChannel_Forbidden(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "viewgatevoiceowner")
	member := ts.CreateTestUser(t, "viewgatevoicemember")
	serverID := ts.CreateTestServer(t, owner.ID, "View Gate Voice Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := createTestVoiceChannel(t, ts, serverID, "voice-secret")

	// Deny View Voice Channels only; Send stays granted via the base
	// "member" role.
	ts.CreateChannelOverride(t, channelID, "user", member.ID, 0, int64(rbac.PermViewVoiceChannels))

	// Fixture-validity guard, same technique as test A.
	log := logger.New("test")
	resolver := rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), log)
	effective, err := resolver.ResolveEffectivePermissionsUncached(t.Context(), serverID, member.ID, channelID)
	require.NoError(t, err, "fixture guard: resolving effective channel permissions must not error")
	require.True(t, effective.Has(rbac.PermSendMessages),
		"fixture guard: member must hold PermSendMessages for this to be a View-gate regression, not a Send denial")
	require.False(t, effective.Has(rbac.PermViewVoiceChannels),
		"fixture guard: member must NOT hold PermViewVoiceChannels for this to be a View-gate regression")

	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code,
		"member denied View Voice Channels must not be able to send over REST")
	assert.Equal(t, 0, countMessagesByChannelAndUser(t, ts, channelID, member.ID),
		"member denied View Voice Channels must not have a message row written for that channel")
}

// TestSendMessage_RESTViewVoiceChannelControl_Created is D's positive
// control: identical voice-channel setup with NO override row. It proves
// REST send accepts a text message into a voice channel at all (the
// handler applies no channel-type gate of its own), so a red result in D
// cannot be misattributed to that instead of the View-gate regression.
func TestSendMessage_RESTViewVoiceChannelControl_Created(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "viewgatevoicectrlowner")
	member := ts.CreateTestUser(t, "viewgatevoicectrlmember")
	serverID := ts.CreateTestServer(t, owner.ID, "View Gate Voice Control Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := createTestVoiceChannel(t, ts, serverID, "voice-general")

	w := ts.DoRequest("POST", "/api/v1/messages", map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code,
		"member with default base permissions and no override must be able to send into a voice channel over REST")
	assert.Equal(t, 1, countMessagesByChannelAndUser(t, ts, channelID, member.ID),
		"a successful send must write exactly one message row for that channel")
}
