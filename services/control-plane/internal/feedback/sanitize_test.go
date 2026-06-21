package feedback

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Each test below targets one scrub category. The test names match the
// PATTERN comments in sanitize.go so a failing test points unambiguously
// at the row in the table.

func TestSanitize_Email(t *testing.T) {
	in := "Crashed while sending to alice@example.com"
	out := Sanitize(in)
	assert.NotContains(t, out, "alice@example.com")
	assert.Contains(t, out, "<email>")
}

func TestSanitize_JWT(t *testing.T) {
	// Realistic-shape JWT (three base64url segments, eyJ prefix).
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyMSJ9.abcdefghij1234567890XYZ"
	in := "Authorization context: " + jwt
	out := Sanitize(in)
	assert.NotContains(t, out, jwt)
	assert.Contains(t, out, "<jwt>")
}

func TestSanitize_Bearer(t *testing.T) {
	in := "Header was Bearer ya29.aA1BBccDDeeFFggHHii123456"
	out := Sanitize(in)
	assert.NotContains(t, out, "ya29.aA1BBccDDeeFFggHHii123456")
	assert.Contains(t, out, "Bearer <token>")
}

func TestSanitize_POSIXPath(t *testing.T) {
	in := "ENOENT /Users/michael/.ssh/id_rsa"
	out := Sanitize(in)
	assert.NotContains(t, out, "michael")
	assert.Contains(t, out, "/Users/<user>")
}

func TestSanitize_HomePath(t *testing.T) {
	in := "ENOENT /home/sysadmin/.config/secret"
	out := Sanitize(in)
	assert.NotContains(t, out, "sysadmin")
	assert.Contains(t, out, "/home/<user>")
}

func TestSanitize_WindowsPath(t *testing.T) {
	in := `Found C:\Users\Michael\AppData\Local\Concord\logs`
	out := Sanitize(in)
	assert.NotContains(t, out, "Michael")
	assert.Contains(t, out, `C:\Users\<user>`)
}

func TestSanitize_IPv4(t *testing.T) {
	in := "Refused by 192.168.1.42"
	out := Sanitize(in)
	assert.NotContains(t, out, "192.168.1.42")
	assert.Contains(t, out, "<ip>")
}

func TestSanitize_IPv6(t *testing.T) {
	in := "Refused by 2001:db8:abcd:1234::1"
	out := Sanitize(in)
	assert.NotContains(t, out, "2001:db8")
	assert.Contains(t, out, "<ip>")
}

func TestSanitize_LongHex(t *testing.T) {
	hash := strings.Repeat("a1b2c3d4", 4) // 32 chars
	in := "Checksum: " + hash
	out := Sanitize(in)
	assert.NotContains(t, out, hash)
	assert.Contains(t, out, "<hex>")
}

func TestSanitize_Base64(t *testing.T) {
	// 48-char base64 — includes chars NOT in the hex set (`g`, `z`, `+`, `/`,
	// `=`) so it doesn't get caught by the upstream long-hex pattern. Realistic
	// shape for a PEM-style key blob.
	blob := "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA1+/="
	in := "Public key: " + blob
	out := Sanitize(in)
	assert.NotContains(t, out, blob)
	assert.Contains(t, out, "<base64>")
}

func TestSanitize_EmptyInput(t *testing.T) {
	assert.Equal(t, "", Sanitize(""))
}

func TestSanitize_NoMatches(t *testing.T) {
	in := "the quick brown fox jumps over the lazy dog"
	out := Sanitize(in)
	assert.Equal(t, in, out)
}

func TestSanitize_MultipleMatchesInOneString(t *testing.T) {
	in := "alice@example.com tried to reach 10.0.0.1"
	out := Sanitize(in)
	assert.Contains(t, out, "<email>")
	assert.Contains(t, out, "<ip>")
	assert.NotContains(t, out, "alice")
	assert.NotContains(t, out, "10.0.0.1")
}

// Regression: short numeric strings should NOT trip the IPv4 pattern.
// (e.g., "version 1.0.42" — three dot-separated numbers but only 5 total digits)
func TestSanitize_DoesNotMatchShortVersionStrings(t *testing.T) {
	in := "Running v1.2.3"
	out := Sanitize(in)
	assert.Equal(t, in, out)
}
