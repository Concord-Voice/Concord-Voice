package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashPassword(t *testing.T) {
	t.Run("returns a valid argon2id hash", func(t *testing.T) {
		hash, err := HashPassword("TestPassword123!")
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(hash, "$argon2id$v=19$"))
	})

	t.Run("produces different hashes for same password", func(t *testing.T) {
		hash1, err := HashPassword("TestPassword123!")
		require.NoError(t, err)
		hash2, err := HashPassword("TestPassword123!")
		require.NoError(t, err)
		assert.NotEqual(t, hash1, hash2, "salts should differ")
	})

	t.Run("hash format has 6 dollar-delimited parts", func(t *testing.T) {
		hash, err := HashPassword("TestPassword123!")
		require.NoError(t, err)
		parts := strings.Split(hash, "$")
		assert.Len(t, parts, 6) // empty + argon2id + version + params + salt + hash
	})
}

func TestHashPasswordWithParams(t *testing.T) {
	t.Run("respects custom parameters", func(t *testing.T) {
		params := &Argon2Params{
			Memory:      32 * 1024,
			Iterations:  1,
			Parallelism: 1,
			SaltLength:  16,
			KeyLength:   32,
		}
		hash, err := HashPasswordWithParams("password", params)
		require.NoError(t, err)
		assert.Contains(t, hash, "m=32768,t=1,p=1")
	})
}

func TestVerifyPassword(t *testing.T) {
	hash, err := HashPassword("TestPassword123!")
	require.NoError(t, err)

	t.Run("correct password returns true", func(t *testing.T) {
		ok, err := VerifyPassword("TestPassword123!", hash)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("wrong password returns false", func(t *testing.T) {
		ok, err := VerifyPassword("WrongPassword123!", hash)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("malformed hash returns error", func(t *testing.T) {
		_, err := VerifyPassword("test", "not-a-hash")
		assert.ErrorIs(t, err, ErrInvalidHash)
	})

	t.Run("empty hash returns error", func(t *testing.T) {
		_, err := VerifyPassword("test", "")
		assert.ErrorIs(t, err, ErrInvalidHash)
	})

	t.Run("wrong algorithm returns error", func(t *testing.T) {
		_, err := VerifyPassword("test", "$bcrypt$v=19$m=65536,t=3,p=4$salt$hash")
		assert.ErrorIs(t, err, ErrInvalidHash)
	})
}

func TestDefaultParams(t *testing.T) {
	p := DefaultParams()
	assert.Equal(t, uint32(64*1024), p.Memory)
	assert.Equal(t, uint32(3), p.Iterations)
	assert.Equal(t, uint8(4), p.Parallelism)
	assert.Equal(t, uint32(16), p.SaltLength)
	assert.Equal(t, uint32(32), p.KeyLength)
}

func TestValidatePasswordStrength(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"valid password", "TestPassword123!", false},
		{"valid with three types", "TestPassword123", false},
		{"too short", "Test1!", true},
		{"11 chars is too short", "TestPass12!", true},
		{"12 chars minimum", "TestPass123!", false},
		{"too long (129 chars)", strings.Repeat("a", 129), true},
		{"128 chars max", "Aa1!" + strings.Repeat("x", 124), false},
		{"only lowercase", "abcdefghijklmn", true},
		{"only uppercase", "ABCDEFGHIJKLMN", true},
		{"only numbers", "123456789012", true},
		{"lower + upper only (2 types)", "abcdefGHIJKL", true},
		{"lower + upper + number (3 types)", "abcdefGHIJ12", false},
		{"lower + upper + special (3 types)", "abcdefGHIJ!@", false},
		{"lower + number + special (3 types)", "abcdef1234!@", false},
		{"upper + number + special (3 types)", "ABCDEF1234!@", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePasswordStrength(tt.password)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
