package friends

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestFriendRequestReceivedData_ShapeAndFields pins the friend_request_received WS
// payload shape — the WS↔REST shape contract that regressed in #981 (the client
// silently dropped broadcasts missing to_user_id/to_username). Both emit sites
// (notifyFriendRequestReceived and notifyFriendCodeClaimed) route their payload
// through friendRequestReceivedData, so asserting the builder's output guards both
// against from_*/to_* drift without needing a DB or WS hub.
func TestFriendRequestReceivedData_ShapeAndFields(t *testing.T) {
	sp := func(s string) *string { return &s }

	from := userProfile{username: "alice", displayName: sp("Alice"), avatarURL: sp("https://example/a.png")}
	to := userProfile{username: "bob", displayName: sp("Bob"), avatarURL: nil}

	data := friendRequestReceivedData("fr-1", "user-from", from, "user-to", to, "2026-06-01T00:00:00Z")

	// to_* are the fields the client gates on (#981) — presence + value is the
	// load-bearing assertion this test exists to protect.
	assert.Equal(t, "user-to", data["to_user_id"])
	assert.Equal(t, "bob", data["to_username"])
	assert.Equal(t, sp("Bob"), data["to_display_name"])
	assert.Nil(t, data["to_avatar_url"])

	// from_* describe the sender.
	assert.Equal(t, "user-from", data["from_user_id"])
	assert.Equal(t, "alice", data["from_username"])
	assert.Equal(t, sp("Alice"), data["from_display_name"])
	assert.Equal(t, sp("https://example/a.png"), data["from_avatar_url"])

	assert.Equal(t, "fr-1", data["id"])
	assert.Equal(t, "2026-06-01T00:00:00Z", data["created_at"])

	// Full key set — catches accidental addition or removal of a wire field.
	expectedKeys := []string{
		"id",
		"from_user_id", "from_username", "from_display_name", "from_avatar_url",
		"to_user_id", "to_username", "to_display_name", "to_avatar_url",
		"created_at",
	}
	assert.Len(t, data, len(expectedKeys))
	for _, k := range expectedKeys {
		assert.Contains(t, data, k, "missing wire field %q", k)
	}
}

// TestFriendRequestReceivedData_NilProfileFields confirms nil display_name/avatar_url
// pointers pass through as nil (serialized to JSON null) rather than being coerced —
// mirroring the from_* nilable pattern the frontend schema normalizes (null→undefined).
func TestFriendRequestReceivedData_NilProfileFields(t *testing.T) {
	from := userProfile{username: "carol"}
	to := userProfile{username: "dave"}

	data := friendRequestReceivedData("fr-2", "uf", from, "ut", to, "2026-06-01T00:00:00Z")

	assert.Nil(t, data["from_display_name"])
	assert.Nil(t, data["from_avatar_url"])
	assert.Nil(t, data["to_display_name"])
	assert.Nil(t, data["to_avatar_url"])
	// Required string fields are still present even when profile pointers are nil.
	assert.Equal(t, "carol", data["from_username"])
	assert.Equal(t, "dave", data["to_username"])
}
