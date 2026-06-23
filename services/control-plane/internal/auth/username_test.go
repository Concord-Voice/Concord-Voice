package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateUsername(t *testing.T) {
	valid := []struct {
		name     string
		username string
	}{
		{"simple lowercase", "testuser"},
		{"with numbers", "user123"},
		{"with underscore", "test_user"},
		{"with hyphen", "test-user"},
		{"with period", "test.user"},
		{"multiple periods", "a.b.c"},
		{"period and underscore", "test.user_name"},
		{"period and hyphen", "test.user-name"},
		{"minimum length (3)", "abc"},
		{"32 chars max", "abcdefghijklmnopqrstuvwxyz012345"},
		{"mixed case", "TestUser"},
		{"starts/ends with number", "1test1"},
	}

	for _, tt := range valid {
		t.Run("valid: "+tt.name, func(t *testing.T) {
			assert.NoError(t, ValidateUsername(tt.username))
		})
	}

	invalid := []struct {
		name     string
		username string
	}{
		{"single char too short", "a"},
		{"two chars too short", "ab"},
		{"too long (33 chars)", "abcdefghijklmnopqrstuvwxyz0123456"},
		{"empty", ""},
		{"starts with underscore", "_test"},
		{"starts with hyphen", "-test"},
		{"starts with period", ".test"},
		{"ends with underscore", "test_"},
		{"ends with hyphen", "test-"},
		{"ends with period", "test."},
		{"consecutive underscores", "test__user"},
		{"consecutive hyphens", "test--user"},
		{"consecutive periods", "test..user"},
		{"mixed consecutive specials", "test_-user"},
		{"period-underscore consecutive", "test._user"},
		{"underscore-period consecutive", "test_.user"},
		{"period-hyphen consecutive", "test.-user"},
		{"contains space", "test user"},
		{"contains @", "test@user"},
		{"reserved word admin", "admin"},
		{"reserved word as substring", "admin123"},
		{"reserved word root", "root"},
		{"reserved word moderator", "moderator"},
		{"reserved word system", "system"},
		{"reserved word support", "support"},
		{"reserved word concord", "concord"},
		{"reserved word via periods", "a.d" + "m.i.n"}, // "a.dm.i.n" stripped to "admin"
		{"profanity exact match", "fuck"},
		// Injection prevention — regex whitelist blocks all non-allowed characters
		{"script injection", "user<script>"},
		{"sql injection", "user';DROP"},
		{"path traversal", "user/../x"},
		{"null byte", "user\x00name"},
	}

	for _, tt := range invalid {
		t.Run("invalid: "+tt.name, func(t *testing.T) {
			assert.Error(t, ValidateUsername(tt.username))
		})
	}
}

func TestValidateUsernameLeetspeak(t *testing.T) {
	// Leetspeak variations of reserved words should be caught
	tests := []string{
		"4dmin",  // a->4 for "admin"
		"r00t",   // o->0 for "root"
		"$y$t3m", // s->$, e->3 for "system"
	}
	for _, username := range tests {
		t.Run(username, func(t *testing.T) {
			assert.Error(t, ValidateUsername(username))
		})
	}
}

func TestZAPSeedUsernameIsStableValidFixture(t *testing.T) {
	assert.Error(t, ValidateUsername("zapscan28037908070"))
	assert.NoError(t, ValidateUsername("zapci"))
}

func TestNormalizeUsername(t *testing.T) {
	assert.Equal(t, "testuser", NormalizeUsername("TestUser"))
	assert.Equal(t, "testuser", NormalizeUsername("TESTUSER"))
	assert.Equal(t, "testuser", NormalizeUsername("testuser"))
}

func TestDeLeetSpeak(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"h3ll0", "hello"},
		{"4dm1n", "admin"},
		{"$y$t3m", "system"},
		{"normal", "normal"},
		{"1337", "ieet"},
		{"@$$", "ass"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, deLeetSpeak(tt.input))
		})
	}
}
