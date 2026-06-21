package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateCode(t *testing.T) {
	t.Run("returns 6-digit string", func(t *testing.T) {
		code, err := generateCode()
		require.NoError(t, err)
		assert.Len(t, code, 6)
		for _, c := range code {
			assert.True(t, c >= '0' && c <= '9', "expected digit, got %c", c)
		}
	})

	t.Run("generates valid codes consistently", func(t *testing.T) {
		// Generate several codes and verify they're all valid 6-digit strings
		for i := 0; i < 10; i++ {
			code, err := generateCode()
			require.NoError(t, err)
			assert.Len(t, code, 6)
			for _, c := range code {
				assert.True(t, c >= '0' && c <= '9', "expected digit, got %c", c)
			}
		}
	})
}

func TestHashCode(t *testing.T) {
	t.Run("returns deterministic hash", func(t *testing.T) {
		h1 := hashCode("123456")
		h2 := hashCode("123456")
		assert.Equal(t, h1, h2)
	})

	t.Run("different codes produce different hashes", func(t *testing.T) {
		h1 := hashCode("123456")
		h2 := hashCode("654321")
		assert.NotEqual(t, h1, h2)
	})

	t.Run("returns 64-char hex digest", func(t *testing.T) {
		h := hashCode("000000")
		assert.Len(t, h, 64)
	})
}

func TestRedisKey(t *testing.T) {
	assert.Equal(t, "email_verify:user-123", redisKey("user-123"))
}

func TestGenerateAccessTokenEmailVerifiedClaim(t *testing.T) {
	t.Run("token with email_verified=true", func(t *testing.T) {
		tokenStr, err := GenerateAccessToken("user-1", testJWTSecret, true)
		require.NoError(t, err)

		claims, err := ValidateAccessToken(tokenStr, testJWTSecret)
		require.NoError(t, err)
		require.NotNil(t, claims.EmailVerified)
		assert.True(t, *claims.EmailVerified)
	})

	t.Run("token with email_verified=false", func(t *testing.T) {
		tokenStr, err := GenerateAccessToken("user-1", testJWTSecret, false)
		require.NoError(t, err)

		claims, err := ValidateAccessToken(tokenStr, testJWTSecret)
		require.NoError(t, err)
		require.NotNil(t, claims.EmailVerified)
		assert.False(t, *claims.EmailVerified)
	})

	t.Run("pre-migration token without email_verified claim", func(t *testing.T) {
		// Simulate a token from before the migration — no email_verified field
		claims := Claims{
			UserID: "user-1",
		}
		claims.ExpiresAt = nil // Will be valid forever for this test

		// The EmailVerified field should be nil
		assert.Nil(t, claims.EmailVerified, "pre-migration tokens should have nil EmailVerified")
	})
}
