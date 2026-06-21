package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testJWTSecret = "test_secret_key"

func TestGenerateAccessToken(t *testing.T) {
	t.Run("returns non-empty token", func(t *testing.T) {
		token, err := GenerateAccessToken("user-123", testJWTSecret, true)
		require.NoError(t, err)
		assert.NotEmpty(t, token)
	})

	t.Run("token contains correct user_id claim", func(t *testing.T) {
		tokenStr, err := GenerateAccessToken("user-456", testJWTSecret, true)
		require.NoError(t, err)

		claims, err := ValidateAccessToken(tokenStr, testJWTSecret)
		require.NoError(t, err)
		assert.Equal(t, "user-456", claims.UserID)
	})

	t.Run("token has unique JTI", func(t *testing.T) {
		token1, _ := GenerateAccessToken("user-1", testJWTSecret, true)
		token2, _ := GenerateAccessToken("user-1", testJWTSecret, true)

		claims1, _ := ValidateAccessToken(token1, testJWTSecret)
		claims2, _ := ValidateAccessToken(token2, testJWTSecret)

		assert.NotEqual(t, claims1.ID, claims2.ID, "each token should have a unique JTI")
	})

	t.Run("token expires in 15 minutes", func(t *testing.T) {
		tokenStr, err := GenerateAccessToken("user-1", testJWTSecret, true)
		require.NoError(t, err)

		claims, err := ValidateAccessToken(tokenStr, testJWTSecret)
		require.NoError(t, err)

		expectedExpiry := time.Now().Add(15 * time.Minute)
		assert.WithinDuration(t, expectedExpiry, claims.ExpiresAt.Time, 5*time.Second)
	})

	t.Run("token has issuer set", func(t *testing.T) {
		tokenStr, _ := GenerateAccessToken("user-1", testJWTSecret, true)
		claims, _ := ValidateAccessToken(tokenStr, testJWTSecret)
		assert.Equal(t, "concordvoice-control-plane", claims.Issuer)
	})
}

func TestValidateAccessToken(t *testing.T) {
	t.Run("valid token returns claims", func(t *testing.T) {
		tokenStr, _ := GenerateAccessToken("user-123", testJWTSecret, true)
		claims, err := ValidateAccessToken(tokenStr, testJWTSecret)
		require.NoError(t, err)
		assert.Equal(t, "user-123", claims.UserID)
	})

	t.Run("wrong secret returns error", func(t *testing.T) {
		tokenStr, _ := GenerateAccessToken("user-123", testJWTSecret, true)
		_, err := ValidateAccessToken(tokenStr, "wrong_secret")
		assert.ErrorIs(t, err, ErrInvalidToken)
	})

	t.Run("malformed token returns error", func(t *testing.T) {
		_, err := ValidateAccessToken("not.a.valid.token", testJWTSecret)
		assert.ErrorIs(t, err, ErrInvalidToken)
	})

	t.Run("expired token returns ErrExpiredToken", func(t *testing.T) {
		// Create a token that's already expired
		claims := Claims{
			UserID: "user-1",
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
				IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			},
		}
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		// nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key
		tokenStr, _ := token.SignedString([]byte(testJWTSecret))

		_, err := ValidateAccessToken(tokenStr, testJWTSecret)
		assert.ErrorIs(t, err, ErrExpiredToken)
	})

	t.Run("empty string returns error", func(t *testing.T) {
		_, err := ValidateAccessToken("", testJWTSecret)
		assert.Error(t, err)
	})
}

func TestGenerateAccessToken_EmbedsTierClaim(t *testing.T) {
	tokenStr, err := GenerateAccessToken("user-1", testJWTSecret, true, "premium")
	require.NoError(t, err)

	claims, err := ValidateAccessToken(tokenStr, testJWTSecret)
	require.NoError(t, err)
	assert.Equal(t, "premium", claims.Tier)
	assert.Equal(t, "user-1", claims.UserID)
}

func TestGenerateAccessToken_OmittedTierIsEmpty(t *testing.T) {
	// Variadic tier omitted → claim absent (omitempty) → parses back to "".
	tokenStr, err := GenerateAccessToken("user-1", testJWTSecret, true)
	require.NoError(t, err)

	claims, err := ValidateAccessToken(tokenStr, testJWTSecret)
	require.NoError(t, err)
	assert.Equal(t, "", claims.Tier)
}

func TestGenerateAccessToken_EmptyTierOmittedFromClaim(t *testing.T) {
	// Explicit empty tier also yields an absent claim (omitempty), read as free.
	tokenStr, err := GenerateAccessToken("user-1", testJWTSecret, true, "")
	require.NoError(t, err)

	claims, err := ValidateAccessToken(tokenStr, testJWTSecret)
	require.NoError(t, err)
	assert.Equal(t, "", claims.Tier)
}

func TestGenerateRefreshToken(t *testing.T) {
	t.Run("returns non-empty token", func(t *testing.T) {
		token, err := GenerateRefreshToken()
		require.NoError(t, err)
		assert.NotEmpty(t, token)
	})

	t.Run("generates unique tokens", func(t *testing.T) {
		token1, _ := GenerateRefreshToken()
		token2, _ := GenerateRefreshToken()
		assert.NotEqual(t, token1, token2)
	})
}

func TestHashRefreshToken(t *testing.T) {
	t.Run("returns deterministic hash", func(t *testing.T) {
		hash1 := HashRefreshToken("test-token")
		hash2 := HashRefreshToken("test-token")
		assert.Equal(t, hash1, hash2)
	})

	t.Run("different tokens produce different hashes", func(t *testing.T) {
		hash1 := HashRefreshToken("token-a")
		hash2 := HashRefreshToken("token-b")
		assert.NotEqual(t, hash1, hash2)
	})

	t.Run("returns hex-encoded string", func(t *testing.T) {
		hash := HashRefreshToken("test")
		assert.Len(t, hash, 64, "SHA-256 hex digest should be 64 chars")
	})
}
