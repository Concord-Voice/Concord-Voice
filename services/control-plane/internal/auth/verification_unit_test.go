package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Additional unit tests for verification.go internals ---

func TestGenerateCodeRange(t *testing.T) {
	// Verify the code is always within 000000-999999 range
	for i := 0; i < 50; i++ {
		code, err := generateCode()
		require.NoError(t, err)
		assert.Len(t, code, 6)

		// Verify all digits
		for _, ch := range code {
			assert.True(t, ch >= '0' && ch <= '9', "character %c is not a digit", ch)
		}
	}
}

func TestGenerateCodeLeadingZeros(t *testing.T) {
	// Run enough iterations to verify that codes with leading zeros are properly zero-padded.
	// The format string "%06d" should handle this, but let's verify.
	// We can't control the random output, but we can verify format is always 6 chars.
	for i := 0; i < 100; i++ {
		code, err := generateCode()
		require.NoError(t, err)
		assert.Len(t, code, 6, "code should always be 6 characters, even with leading zeros")
	}
}

func TestHashCodeConsistency(t *testing.T) {
	// Same input should always produce the same hash
	h1 := hashCode("000000")
	h2 := hashCode("000000")
	assert.Equal(t, h1, h2)
}

func TestHashCodeUniqueness(t *testing.T) {
	// Different inputs should produce different hashes
	codes := []string{"000000", "000001", "999999", "123456", "654321"}
	hashes := make(map[string]bool)
	for _, code := range codes {
		h := hashCode(code)
		assert.False(t, hashes[h], "hash collision detected for code %s", code)
		hashes[h] = true
	}
}

func TestHashCodeLength(t *testing.T) {
	// SHA-256 hex digest is always 64 characters
	h := hashCode("123456")
	assert.Len(t, h, 64)
}

func TestRedisKeyFormat(t *testing.T) {
	key := redisKey("user-abc-123")
	assert.Equal(t, "email_verify:user-abc-123", key)
}

func TestRedisKeyUUID(t *testing.T) {
	key := redisKey("550e8400-e29b-41d4-a716-446655440000")
	assert.Equal(t, "email_verify:550e8400-e29b-41d4-a716-446655440000", key)
}

func TestVerificationRecordStructure(t *testing.T) {
	// Verify the struct can be instantiated and has the expected fields
	record := verificationRecord{
		CodeHash: hashCode("123456"),
		Email:    "test@example.com",
		Attempts: 0,
	}
	assert.NotEmpty(t, record.CodeHash)
	assert.Equal(t, "test@example.com", record.Email)
	assert.Equal(t, 0, record.Attempts)
}

func TestVerifyCodeTTLConstant(t *testing.T) {
	// VerifyCodeTTLNew (from pending.go) is 2 minutes (#621).
	assert.Equal(t, 2, int(VerifyCodeTTLNew.Minutes()))
}

func TestVerifyMaxAttemptsConstant(t *testing.T) {
	// MaxCodeAttempts (from pending.go) is 4 (#621).
	assert.Equal(t, 4, MaxCodeAttempts)
}

func TestSanitizeVerificationCode(t *testing.T) {
	assert.Equal(t, "123456", sanitizeVerificationCode("123456"))
	assert.Equal(t, "123456", sanitizeVerificationCode(" 123-456 "))
	assert.Equal(t, "123456", sanitizeVerificationCode("123-456"))
	assert.Equal(t, "123456", sanitizeVerificationCode("  123456  "))
}

func TestIsValidVerificationCode(t *testing.T) {
	assert.True(t, isValidVerificationCode("123456"))
	assert.True(t, isValidVerificationCode("000000"))
	assert.False(t, isValidVerificationCode("12345"))
	assert.False(t, isValidVerificationCode("1234567"))
	assert.False(t, isValidVerificationCode("12345a"))
	assert.False(t, isValidVerificationCode("abcdef"))
	assert.False(t, isValidVerificationCode(""))
}
