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

// E2EETestPublicKey is a valid RSA-4096 SPKI public key for request fixtures.
const E2EETestPublicKey = "MIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEA2qM/seYPo49k6tRZvNCVl0A7jKbVTGWwsPYSfX6MWd7/VU4ocUh6v/au02rvDIXoCyHHHAP/YCN+SLgZBJnSd77KqzWBoXczEd3uZhr/rHIfPAewHKYICj2tKXKAr6KduK42I0guEODiHXwWT4vHfzUEk7dJRALhNnKc6utjBjD9fyeasC/m12hw9b007NoyA9xUIeVT0/n+Yy+BmJSmhlgEXywQ0NCXQtJnW3Vj6iZiDhORt7udauPYlzdf1N7YCM0rMs/BtdgNE8m+/mj4OazKasRn8hOCPDOuHfprbDhW6yaACugsjtX3chE7TeXzg+q41+zARuC/YgkPZy7FgNuUsOONSvkHefi0b646+CzUcXE8I4oJQ7MIpjGb7n+h52TH4VO9GWdLwHAOd1A19XyniI7+NeH/D5pJNW6HJqbq5CTAhYAWXvEymnow2nX4MBNtin5fNovmIsh3Z9mGhvl2e3D3kBfgtMO+n6PW4c0k1g6qBhGwMwrx+f0nLKDii/tXN2GBLM3URPxjob55O4YJk+6FyGUNekfw1IFcf4mp0klytHaIEHQFBxpnYf+/0rTO1a59m98nVxY8LBjBFuzFAhFNB1nCSL0P1T5G1AkLbIE2myKOIsksDVo7WNyqvbwaqk4B7zKXkOUUXIhJLgX8oLduD74OgMqGXhZDTXUCAwEAAQ==" // pragma: allowlist secret -- public RSA-4096 SPKI test fixture

// TestUser holds the details of a user created for testing.
type TestUser struct {
	ID          string
	Email       string
	Username    string
	Password    string //nolint:gosec // Test struct field, not a credential
	AccessToken string //nolint:gosec // Test struct field, not a credential
}

// E2EETestKeys returns valid RSA-4096 SPKI public key material with random private-key wrapping data.
func E2EETestKeys() (publicKeyB64, wrappedPrivateKeyB64, saltB64 string) {
	publicKeyB64 = E2EETestPublicKey

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
