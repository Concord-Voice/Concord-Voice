package testhelpers

import (
	"crypto/rand"
	"encoding/base64"
)

// TestAuthPlaintext is the plaintext credential used for all test users.
const TestAuthPlaintext = "TestPassword123!" //nolint:gosec // test fixture, not a real credential

// TestAuthHash is a pre-computed Argon2id hash of TestAuthPlaintext.
// Pre-computing avoids the ~100ms cost of Argon2id (64MB, 3 iterations) per user creation.
const TestAuthHash = "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec // Test-only pre-computed hash, not a real credential

// TestJWTSecret is the JWT signing key used in test servers.
const TestJWTSecret = "test_secret_for_testing"

// TestUser holds the details of a user created for testing.
type TestUser struct {
	ID          string
	Email       string
	Username    string
	Password    string //nolint:gosec // Test struct field, not a credential
	AccessToken string //nolint:gosec // Test struct field, not a credential
}

// E2EETestKeys returns structurally valid but random E2EE key material (base64 encoded).
func E2EETestKeys() (publicKeyB64, wrappedPrivateKeyB64, saltB64 string) {
	pubKey := make([]byte, 550) // Approximate RSA-4096 SPKI export size
	_, _ = rand.Read(pubKey)
	publicKeyB64 = base64.StdEncoding.EncodeToString(pubKey)

	wrappedKey := make([]byte, 512) // Wrapped private key
	_, _ = rand.Read(wrappedKey)
	wrappedPrivateKeyB64 = base64.StdEncoding.EncodeToString(wrappedKey)

	derivationSalt := make([]byte, 16)
	_, _ = rand.Read(derivationSalt)
	saltB64 = base64.StdEncoding.EncodeToString(derivationSalt)

	return
}

// ValidCiphertext returns a base64-encoded byte slice that passes the
// minimum ciphertext length validation (28 bytes: 12 IV + 16 auth tag).
func ValidCiphertext() string {
	ct := make([]byte, 40)
	_, _ = rand.Read(ct)
	return base64.StdEncoding.EncodeToString(ct)
}
