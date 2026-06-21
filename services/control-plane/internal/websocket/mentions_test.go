package websocket

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
)

// notAUUID is declared in hub_test.go (same package)

// --- MentionAddendum unit tests ---

func TestMentionAddendum_Wipe(t *testing.T) {
	addendum := &MentionAddendum{
		Users:    []string{uuid.New().String(), uuid.New().String()},
		Roles:    []string{uuid.New().String()},
		Everyone: true,
		Here:     true,
	}

	addendum.Wipe()

	assert.Nil(t, addendum.Users)
	assert.Nil(t, addendum.Roles)
	assert.False(t, addendum.Everyone)
	assert.False(t, addendum.Here)
}

func TestMentionAddendum_Wipe_AlreadyEmpty(t *testing.T) {
	addendum := &MentionAddendum{}
	addendum.Wipe()
	assert.True(t, addendum.IsEmpty())
}

func TestMentionAddendum_IsEmpty(t *testing.T) {
	tests := []struct {
		name     string
		addendum MentionAddendum
		want     bool
	}{
		{
			name:     "completely empty",
			addendum: MentionAddendum{},
			want:     true,
		},
		{
			name:     "nil slices, false bools",
			addendum: MentionAddendum{Users: nil, Roles: nil, Everyone: false, Here: false},
			want:     true,
		},
		{
			name:     "empty slices",
			addendum: MentionAddendum{Users: []string{}, Roles: []string{}},
			want:     true,
		},
		{
			name:     "has users",
			addendum: MentionAddendum{Users: []string{uuid.New().String()}},
			want:     false,
		},
		{
			name:     "has roles",
			addendum: MentionAddendum{Roles: []string{uuid.New().String()}},
			want:     false,
		},
		{
			name:     "everyone true",
			addendum: MentionAddendum{Everyone: true},
			want:     false,
		},
		{
			name:     "here true",
			addendum: MentionAddendum{Here: true},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.addendum.IsEmpty())
		})
	}
}

// --- decodeMentionMeta tests ---

// encodeMentionMeta is a test helper that msgpack-encodes and base64-encodes a MentionAddendum.
func encodeMentionMeta(t *testing.T, addendum MentionAddendum) string {
	t.Helper()
	data, err := msgpack.Marshal(&addendum)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(data)
}

func TestDecodeMentionMeta_EmptyString(t *testing.T) {
	assert.Nil(t, decodeMentionMeta(""))
}

func TestDecodeMentionMeta_ValidUsers(t *testing.T) {
	uid1 := uuid.New().String()
	uid2 := uuid.New().String()

	raw := encodeMentionMeta(t, MentionAddendum{Users: []string{uid1, uid2}})
	result := decodeMentionMeta(raw)

	require.NotNil(t, result)
	assert.Equal(t, []string{uid1, uid2}, result.Users)
	assert.False(t, result.Everyone)
	assert.False(t, result.Here)
}

func TestDecodeMentionMeta_ValidRoles(t *testing.T) {
	rid := uuid.New().String()

	raw := encodeMentionMeta(t, MentionAddendum{Roles: []string{rid}})
	result := decodeMentionMeta(raw)

	require.NotNil(t, result)
	assert.Equal(t, []string{rid}, result.Roles)
}

func TestDecodeMentionMeta_Everyone(t *testing.T) {
	raw := encodeMentionMeta(t, MentionAddendum{Everyone: true})
	result := decodeMentionMeta(raw)

	require.NotNil(t, result)
	assert.True(t, result.Everyone)
}

func TestDecodeMentionMeta_Here(t *testing.T) {
	raw := encodeMentionMeta(t, MentionAddendum{Here: true})
	result := decodeMentionMeta(raw)

	require.NotNil(t, result)
	assert.True(t, result.Here)
}

func TestDecodeMentionMeta_Combined(t *testing.T) {
	uid := uuid.New().String()
	rid := uuid.New().String()

	raw := encodeMentionMeta(t, MentionAddendum{
		Users:    []string{uid},
		Roles:    []string{rid},
		Everyone: true,
		Here:     true,
	})
	result := decodeMentionMeta(raw)

	require.NotNil(t, result)
	assert.Equal(t, []string{uid}, result.Users)
	assert.Equal(t, []string{rid}, result.Roles)
	assert.True(t, result.Everyone)
	assert.True(t, result.Here)
}

func TestDecodeMentionMeta_EmptyAddendum(t *testing.T) {
	// An addendum with no targets should return nil
	raw := encodeMentionMeta(t, MentionAddendum{})
	assert.Nil(t, decodeMentionMeta(raw))
}

func TestDecodeMentionMeta_OversizedInput(t *testing.T) {
	// Create a string longer than maxMentionMetaBytes
	oversized := strings.Repeat("A", maxMentionMetaBytes+1)
	assert.Nil(t, decodeMentionMeta(oversized))
}

func TestDecodeMentionMeta_InvalidBase64(t *testing.T) {
	assert.Nil(t, decodeMentionMeta("not-valid-base64!!!"))
}

func TestDecodeMentionMeta_InvalidMsgpack(t *testing.T) {
	// Valid base64, but invalid msgpack content
	raw := base64.StdEncoding.EncodeToString([]byte("not msgpack"))
	assert.Nil(t, decodeMentionMeta(raw))
}

func TestDecodeMentionMeta_TruncatesExcessUsers(t *testing.T) {
	users := make([]string, maxMentionUsers+10)
	for i := range users {
		users[i] = uuid.New().String()
	}

	raw := encodeMentionMeta(t, MentionAddendum{Users: users})
	result := decodeMentionMeta(raw)

	require.NotNil(t, result)
	assert.Len(t, result.Users, maxMentionUsers)
}

func TestDecodeMentionMeta_TruncatesExcessRoles(t *testing.T) {
	roles := make([]string, maxMentionRoles+5)
	for i := range roles {
		roles[i] = uuid.New().String()
	}

	raw := encodeMentionMeta(t, MentionAddendum{Roles: roles})
	result := decodeMentionMeta(raw)

	require.NotNil(t, result)
	assert.Len(t, result.Roles, maxMentionRoles)
}

func TestDecodeMentionMeta_FiltersInvalidUUIDs(t *testing.T) {
	validUID := uuid.New().String()

	raw := encodeMentionMeta(t, MentionAddendum{
		Users: []string{validUID, notAUUID, "also-bad", ""},
	})
	result := decodeMentionMeta(raw)

	require.NotNil(t, result)
	assert.Equal(t, []string{validUID}, result.Users)
}

func TestDecodeMentionMeta_AllInvalidUUIDs_ReturnsNil(t *testing.T) {
	raw := encodeMentionMeta(t, MentionAddendum{
		Users: []string{"bad1", "bad2"},
	})
	assert.Nil(t, decodeMentionMeta(raw))
}

func TestDecodeMentionMeta_MixedValidAndInvalidRoles(t *testing.T) {
	validRID := uuid.New().String()

	raw := encodeMentionMeta(t, MentionAddendum{
		Roles: []string{validRID, notAUUID},
	})
	result := decodeMentionMeta(raw)

	require.NotNil(t, result)
	assert.Equal(t, []string{validRID}, result.Roles)
}

// --- filterValidUUIDs tests ---

func TestFilterValidUUIDs(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  int // expected count of valid UUIDs
	}{
		{"nil input", nil, 0},
		{"empty input", []string{}, 0},
		{"all valid", []string{uuid.New().String(), uuid.New().String()}, 2},
		{"all invalid", []string{"bad", "also-bad", ""}, 0},
		{"mixed", []string{uuid.New().String(), "bad", uuid.New().String()}, 2},
		{"empty strings", []string{"", "", ""}, 0},
		{"sql injection attempt", []string{"'; DROP TABLE users; --"}, 0},
		{"partial uuid", []string{"550e8400-e29b-41d4-a716"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterValidUUIDs(tt.input)
			if tt.want == 0 {
				assert.Nil(t, result)
			} else {
				assert.Len(t, result, tt.want)
			}
		})
	}
}

// --- uuidArrayParam tests ---

func TestUuidArrayParam(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  string
	}{
		{"empty", []string{}, "{}"},
		{"single", []string{"abc"}, "{abc}"},
		{"multiple", []string{"a", "b", "c"}, "{a,b,c}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, uuidArrayParam(tt.input))
		})
	}
}

// --- marshalOutgoing tests ---

func TestMarshalOutgoing(t *testing.T) {
	msg := OutgoingMessage{
		Type: "test",
		Data: map[string]interface{}{"key": "value"},
	}

	data, err := marshalOutgoing(msg)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Equal(t, "test", parsed["type"])
}

// --- Mention permission enforcement tests (using mock checker) ---

// mockMentionChecker is a test double for MentionPermissionChecker.
type mockMentionChecker struct {
	// permissions maps (serverID+userID+channelID+permBit) -> (allowed, error)
	permissions map[int64]bool
	err         error
}

func (m *mockMentionChecker) HasMentionPermission(_ context.Context, _, _, _ string, permBit int64) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	return m.permissions[permBit], nil
}

// newTestHubWithChecker creates a minimal hub with a mock mention checker for unit testing.
func newTestHubWithChecker(checker MentionPermissionChecker) *Hub {
	hub := &Hub{
		clients:              make(map[uuid.UUID]*Client),
		userClients:          make(map[uuid.UUID]map[uuid.UUID]bool),
		channelSubscriptions: make(map[uuid.UUID]map[uuid.UUID]bool),
		usernames:            make(map[uuid.UUID]string),
		serverSubscriptions:  make(map[uuid.UUID]map[uuid.UUID]bool),
		dmSubscriptions:      make(map[uuid.UUID]map[uuid.UUID]bool),
		onlineCountPending:   make(map[uuid.UUID]bool),
	}
	hub.mentionChecker = checker
	return hub
}

func TestEnforceMentionPermissions_NilAddendum(_ *testing.T) {
	hub := newTestHubWithChecker(&mockMentionChecker{})
	// Should not panic
	hub.enforceMentionPermissions("server", "user", "channel", nil)
}

func TestEnforceMentionPermissions_NilChecker(t *testing.T) {
	hub := newTestHubWithChecker(nil)
	addendum := &MentionAddendum{Everyone: true}
	// Should not panic, addendum should remain unchanged
	hub.enforceMentionPermissions("server", "user", "channel", addendum)
	assert.True(t, addendum.Everyone)
}

func TestEnforceMentionPermissions_EveryoneAllowed(t *testing.T) {
	checker := &mockMentionChecker{
		permissions: map[int64]bool{permMentionEveryone: true},
	}
	hub := newTestHubWithChecker(checker)

	addendum := &MentionAddendum{Everyone: true, Here: true}
	hub.enforceMentionPermissions("s", "u", "c", addendum)

	assert.True(t, addendum.Everyone)
	assert.True(t, addendum.Here)
}

func TestEnforceMentionPermissions_EveryoneDenied(t *testing.T) {
	checker := &mockMentionChecker{
		permissions: map[int64]bool{permMentionEveryone: false},
	}
	hub := newTestHubWithChecker(checker)

	addendum := &MentionAddendum{Everyone: true, Here: true}
	hub.enforceMentionPermissions("s", "u", "c", addendum)

	assert.False(t, addendum.Everyone)
	assert.False(t, addendum.Here)
}

func TestEnforceMentionPermissions_UserMentionAllowed(t *testing.T) {
	uid := uuid.New().String()
	checker := &mockMentionChecker{
		permissions: map[int64]bool{permMentionUsers: true},
	}
	hub := newTestHubWithChecker(checker)

	addendum := &MentionAddendum{Users: []string{uid}}
	hub.enforceMentionPermissions("s", "u", "c", addendum)

	assert.Equal(t, []string{uid}, addendum.Users)
}

func TestEnforceMentionPermissions_UserMentionDenied(t *testing.T) {
	uid := uuid.New().String()
	checker := &mockMentionChecker{
		permissions: map[int64]bool{permMentionUsers: false},
	}
	hub := newTestHubWithChecker(checker)

	addendum := &MentionAddendum{Users: []string{uid}}
	hub.enforceMentionPermissions("s", "u", "c", addendum)

	assert.Nil(t, addendum.Users)
}

func TestEnforceMentionPermissions_RoleMentionDenied(t *testing.T) {
	rid := uuid.New().String()
	checker := &mockMentionChecker{
		permissions: map[int64]bool{permMentionRoles: false},
	}
	hub := newTestHubWithChecker(checker)

	addendum := &MentionAddendum{Roles: []string{rid}}
	hub.enforceMentionPermissions("s", "u", "c", addendum)

	assert.Nil(t, addendum.Roles)
}

func TestEnforceMentionPermissions_ErrorStripsMentions(t *testing.T) {
	checker := &mockMentionChecker{
		err: assert.AnError,
	}
	hub := newTestHubWithChecker(checker)

	addendum := &MentionAddendum{
		Users:    []string{uuid.New().String()},
		Roles:    []string{uuid.New().String()},
		Everyone: true,
		Here:     true,
	}
	hub.enforceMentionPermissions("s", "u", "c", addendum)

	assert.Nil(t, addendum.Users)
	assert.Nil(t, addendum.Roles)
	assert.False(t, addendum.Everyone)
	assert.False(t, addendum.Here)
}

func TestEnforceMentionPermissions_SkipsEmptyFields(_ *testing.T) {
	// If the addendum has no @everyone/@here and no users/roles,
	// the individual enforcers should be no-ops without calling the checker.
	checker := &mockMentionChecker{
		permissions: map[int64]bool{},
	}
	hub := newTestHubWithChecker(checker)

	addendum := &MentionAddendum{}
	hub.enforceMentionPermissions("s", "u", "c", addendum)
	// No panic, no error
}

// --- collectMentionedUsers tests ---

// mockRows implements the row-scanner interface for collectMentionedUsers.
type mockRows struct {
	data    []string
	index   int
	scanErr error
	iterErr error
}

func (m *mockRows) Next() bool {
	if m.index < len(m.data) {
		m.index++
		return true
	}
	return false
}

func (m *mockRows) Scan(dest ...interface{}) error {
	if m.scanErr != nil {
		return m.scanErr
	}
	if p, ok := dest[0].(*string); ok {
		*p = m.data[m.index-1]
	}
	return nil
}

func (m *mockRows) Err() error   { return m.iterErr }
func (m *mockRows) Close() error { return nil }

func TestCollectMentionedUsers_Basic(t *testing.T) {
	uid1 := uuid.New()
	uid2 := uuid.New()
	sender := uuid.New()

	rows := &mockRows{data: []string{uid1.String(), uid2.String(), sender.String()}}
	result := make(map[uuid.UUID]bool)

	collectMentionedUsers(rows, sender, result, "test")

	assert.True(t, result[uid1])
	assert.True(t, result[uid2])
	assert.False(t, result[sender], "sender should be excluded")
}

func TestCollectMentionedUsers_InvalidUUIDs(t *testing.T) {
	validUID := uuid.New()
	sender := uuid.New()

	rows := &mockRows{data: []string{validUID.String(), notAUUID}}
	result := make(map[uuid.UUID]bool)

	collectMentionedUsers(rows, sender, result, "test")

	assert.True(t, result[validUID])
	assert.Len(t, result, 1)
}

func TestCollectMentionedUsers_ScanError(t *testing.T) {
	rows := &mockRows{data: []string{"anything"}, scanErr: assert.AnError}
	result := make(map[uuid.UUID]bool)

	collectMentionedUsers(rows, uuid.New(), result, "test")

	assert.Empty(t, result)
}

func TestCollectMentionedUsers_EmptyRows(t *testing.T) {
	rows := &mockRows{data: []string{}}
	result := make(map[uuid.UUID]bool)

	collectMentionedUsers(rows, uuid.New(), result, "test")

	assert.Empty(t, result)
}

// --- sendToUnsubscribedClients tests ---

func TestSendToUnsubscribedClients_SendsToUnsubscribed(t *testing.T) {
	hub := newTestHubWithChecker(nil)

	userID := uuid.New()
	clientID1 := uuid.New()
	clientID2 := uuid.New()
	client1 := &Client{ID: clientID1, UserID: userID, Send: make(chan []byte, 10)}
	client2 := &Client{ID: clientID2, UserID: userID, Send: make(chan []byte, 10)}

	hub.clients[clientID1] = client1
	hub.clients[clientID2] = client2
	hub.userClients[userID] = map[uuid.UUID]bool{clientID1: true, clientID2: true}

	// client1 is subscribed, client2 is not
	subscribedClients := map[uuid.UUID]bool{clientID1: true}

	msg := []byte(`{"type":"test"}`)
	hub.sendToUnsubscribedClients(userID, subscribedClients, msg)

	// client2 should receive the message, client1 should not
	assert.Len(t, client2.Send, 1)
	assert.Len(t, client1.Send, 0)
}

func TestSendToUnsubscribedClients_UserNotConnected(_ *testing.T) {
	hub := newTestHubWithChecker(nil)
	// No panic when user is not connected
	hub.sendToUnsubscribedClients(uuid.New(), nil, []byte(`{}`))
}

func TestSendToUnsubscribedClients_NilSubscribedClients(t *testing.T) {
	hub := newTestHubWithChecker(nil)

	userID := uuid.New()
	clientID := uuid.New()
	client := &Client{ID: clientID, UserID: userID, Send: make(chan []byte, 10)}

	hub.clients[clientID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{clientID: true}

	msg := []byte(`{"type":"test"}`)
	hub.sendToUnsubscribedClients(userID, nil, msg)

	// With nil subscribedClients, all clients should receive
	assert.Len(t, client.Send, 1)
}

func TestSendToUnsubscribedClients_FullSendChannel(_ *testing.T) {
	hub := newTestHubWithChecker(nil)

	userID := uuid.New()
	clientID := uuid.New()
	// Buffer of 0: send will be non-blocking and drop
	client := &Client{ID: clientID, UserID: userID, Send: make(chan []byte)}

	hub.clients[clientID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{clientID: true}

	// Should not block
	hub.sendToUnsubscribedClients(userID, nil, []byte(`{}`))
}

// --- resolveDMMentionTargets tests ---

func TestResolveDMMentionTargets_DirectMentions(t *testing.T) {
	hub := newTestHubWithChecker(nil)

	sender := uuid.New()
	participant1 := uuid.New()
	participant2 := uuid.New()
	nonParticipant := uuid.New()

	participants := map[uuid.UUID]bool{
		sender:       true,
		participant1: true,
		participant2: true,
	}

	addendum := &MentionAddendum{
		Users: []string{participant1.String(), nonParticipant.String()},
	}

	result := hub.resolveDMMentionTargets(sender, addendum, participants)

	assert.True(t, result[participant1], "participant should be included")
	assert.False(t, result[nonParticipant], "non-participant should be excluded")
	assert.False(t, result[sender], "sender should be excluded")
}

func TestResolveDMMentionTargets_Here(t *testing.T) {
	hub := newTestHubWithChecker(nil)

	sender := uuid.New()
	participant1 := uuid.New()
	participant2 := uuid.New()

	participants := map[uuid.UUID]bool{
		sender:       true,
		participant1: true,
		participant2: true,
	}

	addendum := &MentionAddendum{Here: true}

	result := hub.resolveDMMentionTargets(sender, addendum, participants)

	assert.True(t, result[participant1])
	assert.True(t, result[participant2])
	assert.False(t, result[sender], "sender should be excluded from @here")
}

func TestResolveDMMentionTargets_InvalidUUID(t *testing.T) {
	hub := newTestHubWithChecker(nil)

	sender := uuid.New()
	participants := map[uuid.UUID]bool{sender: true}

	addendum := &MentionAddendum{
		Users: []string{notAUUID},
	}

	result := hub.resolveDMMentionTargets(sender, addendum, participants)
	assert.Empty(t, result)
}

func TestResolveDMMentionTargets_SenderExcludedFromDirectMention(t *testing.T) {
	hub := newTestHubWithChecker(nil)

	sender := uuid.New()
	participants := map[uuid.UUID]bool{sender: true}

	addendum := &MentionAddendum{
		Users: []string{sender.String()},
	}

	result := hub.resolveDMMentionTargets(sender, addendum, participants)
	assert.Empty(t, result)
}

// --- sendMentionNotify tests ---

func TestSendMentionNotify_SendsToMentionedUsers(t *testing.T) {
	hub := newTestHubWithChecker(nil)

	serverID := uuid.New()
	channelID := uuid.New()
	mentionedUserID := uuid.New()
	clientID := uuid.New()
	client := &Client{ID: clientID, UserID: mentionedUserID, Send: make(chan []byte, 10)}

	hub.clients[clientID] = client
	hub.userClients[mentionedUserID] = map[uuid.UUID]bool{clientID: true}
	// The user is NOT subscribed to the channel
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{}

	mentionedUsers := map[uuid.UUID]bool{mentionedUserID: true}

	hub.sendMentionNotify(serverID, channelID, mentionedUsers, true, false)

	require.Len(t, client.Send, 1)
	data := <-client.Send
	var msg map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &msg))
	assert.Equal(t, "unread_notify", msg["type"])
	msgData := msg["data"].(map[string]interface{})
	assert.Equal(t, true, msgData["mentioned"])
	assert.Equal(t, true, msgData["mention_everyone"])
	assert.Equal(t, false, msgData["mention_here"])
}

func TestSendMentionNotify_SkipsSubscribedClients(t *testing.T) {
	hub := newTestHubWithChecker(nil)

	serverID := uuid.New()
	channelID := uuid.New()
	mentionedUserID := uuid.New()
	clientID := uuid.New()
	client := &Client{ID: clientID, UserID: mentionedUserID, Send: make(chan []byte, 10)}

	hub.clients[clientID] = client
	hub.userClients[mentionedUserID] = map[uuid.UUID]bool{clientID: true}
	// The user IS subscribed to the channel
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{clientID: true}

	mentionedUsers := map[uuid.UUID]bool{mentionedUserID: true}

	hub.sendMentionNotify(serverID, channelID, mentionedUsers, false, false)

	assert.Len(t, client.Send, 0, "subscribed client should not receive mention notify")
}

// --- sendDMMentionNotify tests ---

func TestSendDMMentionNotify_SendsToMentionedUsers(t *testing.T) {
	hub := newTestHubWithChecker(nil)

	convID := uuid.New()
	mentionedUserID := uuid.New()
	clientID := uuid.New()
	client := &Client{ID: clientID, UserID: mentionedUserID, Send: make(chan []byte, 10)}

	hub.clients[clientID] = client
	hub.userClients[mentionedUserID] = map[uuid.UUID]bool{clientID: true}
	// Not subscribed to the DM conversation
	hub.dmSubscriptions[convID] = map[uuid.UUID]bool{}

	mentionedUsers := map[uuid.UUID]bool{mentionedUserID: true}

	hub.sendDMMentionNotify(convID, mentionedUsers, true)

	require.Len(t, client.Send, 1)
	data := <-client.Send
	var msg map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &msg))
	assert.Equal(t, "dm_unread_notify", msg["type"])
	msgData := msg["data"].(map[string]interface{})
	assert.Equal(t, true, msgData["mentioned"])
	assert.Equal(t, true, msgData["mention_here"])
}

// --- routeMentionNotifications edge cases ---

func TestRouteMentionNotifications_NilAddendum(_ *testing.T) {
	hub := newTestHubWithChecker(nil)
	// Should not panic
	hub.routeMentionNotifications(uuid.New(), uuid.New(), uuid.New(), nil)
}

func TestRouteMentionNotifications_EmptyAddendum(_ *testing.T) {
	hub := newTestHubWithChecker(nil)
	addendum := &MentionAddendum{}
	// Should not panic
	hub.routeMentionNotifications(uuid.New(), uuid.New(), uuid.New(), addendum)
}

// --- routeDMMentionNotifications edge cases ---

func TestRouteDMMentionNotifications_NilAddendum(_ *testing.T) {
	hub := newTestHubWithChecker(nil)
	// Should not panic
	hub.routeDMMentionNotifications(uuid.New(), uuid.New(), nil)
}

func TestRouteDMMentionNotifications_EmptyAddendum(_ *testing.T) {
	hub := newTestHubWithChecker(nil)
	addendum := &MentionAddendum{}
	// Should not panic
	hub.routeDMMentionNotifications(uuid.New(), uuid.New(), addendum)
}
