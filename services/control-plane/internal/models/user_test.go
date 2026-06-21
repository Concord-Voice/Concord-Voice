package models

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testUserID    = "user-123"
	testUsername  = "Test User"
	testBio       = "Hello world"
	testAvatarURL = "https://cdn.example.com/avatar.jpg"
	testHeaderURL = "https://cdn.example.com/header.jpg"
)

func strPtr(s string) *string        { return &s }
func timePtr(t time.Time) *time.Time { return &t }

func baseUser() User {
	return User{
		ID:             testUserID,
		Email:          "test@example.com",
		Username:       "testuser",
		PasswordHash:   "hashed",
		DisplayName:    strPtr(testUsername),
		Bio:            strPtr(testBio),
		AvatarURL:      strPtr(testAvatarURL),
		HeaderImageURL: strPtr(testHeaderURL),
		ColorScheme:    strPtr("dark"),
		Links:          json.RawMessage(`[{"label":"GitHub","url":"https://github.com"}]`),
		EmailVerified:  true,
		CreatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
}

func TestPublicUserIncludesAllFields(t *testing.T) {
	u := baseUser()
	u.UsernameChangedAt = timePtr(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))

	pub := u.PublicUser()

	assert.Equal(t, testUserID, pub["id"])
	assert.Equal(t, "test@example.com", pub["email"])
	assert.Equal(t, "testuser", pub["username"])
	assert.Equal(t, true, pub["email_verified"])
	assert.Equal(t, testUsername, pub["display_name"])
	assert.Equal(t, testBio, pub["bio"])
	assert.Equal(t, testAvatarURL, pub["avatar_url"])
	assert.Equal(t, testHeaderURL, pub["header_image_url"])
	assert.Equal(t, "dark", pub["color_scheme"])
	assert.NotNil(t, pub["links"])
	assert.NotNil(t, pub["username_changed_at"])
	assert.NotNil(t, pub["username_change_eligible_at"])
	// Regression (#1648): the vestigial e2ee_preference field was removed from
	// the User model and PublicUser serialization. The profile/login responses
	// (which surface PublicUser) must never carry it again.
	_, hasE2EE := pub["e2ee_preference"]
	assert.False(t, hasE2EE, "e2ee_preference removed in #1648; must not appear in PublicUser")
}

func TestPublicUserOmitsPasswordHash(t *testing.T) {
	u := baseUser()
	pub := u.PublicUser()

	_, hasPassword := pub["password_hash"]
	assert.False(t, hasPassword, "password_hash should not be in public user")
}

func TestPublicUserNilOptionalFields(t *testing.T) {
	u := User{
		ID:       "user-456",
		Email:    "bare@example.com",
		Username: "bareuser",
	}
	pub := u.PublicUser()

	assert.Equal(t, "user-456", pub["id"])
	assert.Equal(t, "bareuser", pub["username"])

	_, hasDisplayName := pub["display_name"]
	assert.False(t, hasDisplayName)
	_, hasBio := pub["bio"]
	assert.False(t, hasBio)
	_, hasAvatar := pub["avatar_url"]
	assert.False(t, hasAvatar)
	_, hasHeader := pub["header_image_url"]
	assert.False(t, hasHeader)
	_, hasColor := pub["color_scheme"]
	assert.False(t, hasColor)
}

func TestProfileForOthersExcludesEmail(t *testing.T) {
	u := baseUser()
	prof := u.ProfileForOthers()

	assert.Equal(t, testUserID, prof["id"])
	assert.Equal(t, "testuser", prof["username"])

	_, hasEmail := prof["email"]
	assert.False(t, hasEmail, "email should not be in ProfileForOthers")
}

func TestProfileForOthersIncludesOptionalFields(t *testing.T) {
	u := baseUser()
	prof := u.ProfileForOthers()

	assert.Equal(t, testUsername, prof["display_name"])
	assert.Equal(t, testBio, prof["bio"])
	assert.Equal(t, testAvatarURL, prof["avatar_url"])
	assert.Equal(t, testHeaderURL, prof["header_image_url"])
	assert.Equal(t, "dark", prof["color_scheme"])
}

func TestProfileForOthersWithEmptyLinks(t *testing.T) {
	u := User{
		ID:       "user-789",
		Username: "minimal",
	}
	prof := u.ProfileForOthers()

	// links should default to empty array
	linksRaw, ok := prof["links"]
	require.True(t, ok)
	raw, isRaw := linksRaw.(json.RawMessage)
	require.True(t, isRaw)
	assert.Equal(t, "[]", string(raw))
}

func TestRefreshTokenSession(t *testing.T) {
	fixedTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	rt := RefreshToken{
		ID:         "session-1",
		UserID:     "user-1",
		TokenHash:  "secret-hash",
		DeviceName: "Chrome on macOS",
		IPAddress:  "192.168.1.1",
		MachineID:  "machine-abc",
		ExpiresAt:  fixedTime.Add(24 * time.Hour),
		CreatedAt:  fixedTime,
		LastUsedAt: fixedTime,
	}

	sess := rt.Session()

	assert.Equal(t, "session-1", sess["id"])
	assert.Equal(t, "Chrome on macOS", sess["device_name"])
	assert.Equal(t, "192.168.1.1", sess["ip_address"])
	assert.Equal(t, "machine-abc", sess["machine_id"])

	_, hasTokenHash := sess["token_hash"]
	assert.False(t, hasTokenHash, "token_hash should not be in session")
	_, hasUserID := sess["user_id"]
	assert.False(t, hasUserID, "user_id should not be in session")
}
