package testhelpers

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/invites"
)

// FriendCodeSeed describes a single friend_codes row to insert, together with
// the owning user's identity fields. Zero values produce a live, unlimited code
// owned by a freshly-created verified user.
//
// MaxUses is written verbatim: 0 means "unlimited" to the shared validity
// predicate (friends/handlers.go), which is why it is not defaulted to the
// column's DEFAULT 1.
type FriendCodeSeed struct {
	Username    string
	DisplayName string
	AvatarURL   string
	Expired     bool
	Revoked     bool
	MaxUses     int
	UseCount    int
}

// SeededFriendCode is the result of SeedFriendCode: the generated code plus the
// user that owns it, so tests can assert on the owner's identity fields.
type SeededFriendCode struct {
	Code  string
	Owner TestUser
}

// SeedFriendCode creates a user and a friend_codes row for it, returning the
// generated code. The code is produced by invites.GenerateCode so it always
// satisfies invites.IsValidCode — the charset gate under test must never be the
// reason a "valid" fixture is rejected.
func (ts *TestServer) SeedFriendCode(t *testing.T, seed FriendCodeSeed) SeededFriendCode {
	t.Helper()

	owner := ts.CreateTestUser(t, seed.Username)

	if seed.DisplayName != "" || seed.AvatarURL != "" {
		var displayName, avatarURL *string
		if seed.DisplayName != "" {
			displayName = &seed.DisplayName
		}
		if seed.AvatarURL != "" {
			avatarURL = &seed.AvatarURL
		}
		if _, err := ts.DB.Exec(
			`UPDATE users SET display_name = $1, avatar_url = $2 WHERE id = $3`,
			displayName, avatarURL, owner.ID,
		); err != nil {
			t.Fatalf("testhelpers: failed to set friend-code owner identity fields: %v", err)
		}
	}

	code, err := invites.GenerateCode()
	if err != nil {
		t.Fatalf("testhelpers: failed to generate friend code: %v", err)
	}

	expiresAt := "NULL"
	if seed.Expired {
		expiresAt = "NOW() - INTERVAL '1 hour'"
	}

	// expiresAt is one of two package-private literals, never caller input, so
	// this interpolation cannot carry untrusted data. Every value that can is
	// bound as a parameter.
	if _, err := ts.DB.Exec(
		`INSERT INTO friend_codes (user_id, code, max_uses, use_count, is_revoked, expires_at)
		 VALUES ($1, $2, $3, $4, $5, `+expiresAt+`)`,
		owner.ID, code, seed.MaxUses, seed.UseCount, seed.Revoked,
	); err != nil {
		t.Fatalf("testhelpers: failed to seed friend code: %v", err)
	}

	return SeededFriendCode{Code: code, Owner: owner}
}
