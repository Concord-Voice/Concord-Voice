package feedback

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- SHA1 is required by the TURN REST API credential spec (RFC 5389); used only to reproduce a production-shaped test fixture
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// Server raw-UUID strip backstop (#2074). This is NOT parity-matched to a
// client PATTERN — the honest client already replaces UUIDs with <id:N>
// ordinals via pseudonymizeLogUuids; this fires only on raw UUIDs from a
// compromised client that bypassed that pass.
func TestSanitize_StripsRawUUID(t *testing.T) {
	in := "channel 550e8400-e29b-41d4-a716-446655440000 ok"
	assert.Equal(t, "channel <uuid> ok", Sanitize(in))
}

// The client ordinal pseudonyms (<id:N>) must pass through untouched — the
// backstop targets raw UUIDs only, not the honest client's placeholders.
func TestSanitize_LeavesPseudonymTokensUntouched(t *testing.T) {
	assert.Equal(t, "channel <id:1> ok", Sanitize("channel <id:1> ok"))
}

// ─── TURN relay credentials (#3104) ──────────────────────────────────────
//
// Parity mirror of the TURN entries added to logBufferService.PATTERNS. The
// client scrubs at capture time; this is the last pass before a bug report
// reaches a PUBLIC repo, so it must catch what a compromised or outdated
// client did not.
//
// The fixture is minted the way pkg/config/turn.go mints it — HMAC-SHA1 over
// `<expiry>:<userID>`, standard base64 — rather than hand-written, because the
// defect is a LENGTH defect: 20 digest bytes encode to exactly 28 characters,
// which is under the 40-char floor of the long-base64 pattern. A literal of
// the wrong length makes every assertion below vacuous.
func mintTURNCredentials(t *testing.T) (username, credential string) {
	t.Helper()
	username = "1774000000:550e8400-e29b-41d4-a716-446655440000"
	// #nosec G505 -- SHA1 is required by the TURN REST API credential spec
	// (RFC 5389); this mirrors pkg/config/turn.go so the fixture has the
	// production shape.
	mac := hmac.New(sha1.New, []byte("turn-secret-fixture-not-a-real-one")) // pragma: allowlist secret -- synthetic HMAC key for a test fixture
	mac.Write([]byte(username))
	credential = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	require.Len(t, credential, 28, "fixture must have the production 27-chars-plus-pad shape")
	require.True(t, strings.HasSuffix(credential, "="))
	return username, credential
}

func TestSanitize_TURNCredential(t *testing.T) {
	_, credential := mintTURNCredentials(t)
	out := Sanitize(`{"urls":"turn:t.example:3478","credential":"` + credential + `"}`)
	assert.NotContains(t, out, credential)
}

func TestSanitize_TURNUsername(t *testing.T) {
	username, _ := mintTURNCredentials(t)
	out := Sanitize("[ice] username=" + username)
	assert.NotContains(t, out, username)
	// The pre-existing raw-UUID backstop rewrites the userID half; the expiry
	// half survived it, which is why the username needs its own pattern.
	assert.NotContains(t, out, "1774000000")
	assert.Contains(t, out, "<turn-username>")
}

func TestSanitize_CredentialByKey(t *testing.T) {
	assert.NotContains(t, Sanitize(`{"credential":"whatever-shape-this-is"}`), "whatever-shape-this-is")
	assert.NotContains(t, Sanitize(`{"turnPassword":"hunter2"}`), "hunter2")
	assert.NotContains(t, Sanitize(`RTCConfiguration { credential: 'zzz' }`), "zzz")
}

// False-positive containment: the backstop deliberately over-redacts relative
// to the client (RE2 has no lookaround), but it must not swallow ordinary
// diagnostic text.
func TestSanitize_TURNPatternsLeaveOrdinaryTextAlone(t *testing.T) {
	for _, line := range []string{
		"voice join ok in 412ms, 3 servers, policy=all, attempt 2 of 5",
		"GET /api/v1/channels?limit=50&before=abc -> 200",
		"consumer AbCdEfGhIjKlMnOpQrStUvWxYz01 resumed",
	} {
		assert.Equal(t, line, Sanitize(line), "line: %s", line)
	}
}

// ─── #3117 gap 1: a credential whose own first character is '+' or '/' ───
//
// `\b` needs a word/non-word transition. Every realistic delimiter (`"`,
// space, `:`, `=`, `,`) is non-word — and so are `+` and `/` — so when the
// token STARTS with one of those two there is no boundary between the
// delimiter and the token and the match failed. Two of the 64 base64
// characters, so roughly 3% of minted credentials walked through the last
// pass before a PUBLIC GitHub issue.
//
// The credential is built rather than written down: the leading base64
// character encodes the top 6 bits of byte 0, so 0xF8 yields index 62 ('+')
// and 0xFC yields index 63 ('/'). 20 digest bytes still encode to exactly 27
// characters plus one '=' pad, which is the length the defect depends on.
func turnCredentialStartingWith(t *testing.T, first byte) string {
	t.Helper()
	digest := append([]byte{first}, bytes.Repeat([]byte{0x42}, 19)...)
	credential := base64.StdEncoding.EncodeToString(digest)
	require.Len(t, credential, 28, "fixture must have the production 27-chars-plus-pad shape")
	return credential
}

func TestSanitize_TURNCredentialStartingWithNonWordBase64Char(t *testing.T) {
	cases := []struct {
		name      string
		firstByte byte
		firstChar string
	}{
		{"plus", 0xF8, "+"},
		{"slash", 0xFC, "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credential := turnCredentialStartingWith(t, tc.firstByte)
			require.True(t, strings.HasPrefix(credential, tc.firstChar),
				"fixture must start with %q, got %q", tc.firstChar, credential)
			// Deliberately NOT keyed as credential/password — those forms are
			// caught by the key-shaped entries. This exercises the narrow band
			// on its own, which is the entry with the defect.
			for _, line := range []string{
				"[ice] relay cred " + credential + " ok",
				`{"opaque":"` + credential + `"}`,
				"cred=" + credential,
			} {
				assert.NotContains(t, Sanitize(line), credential, "line: %s", line)
			}
		})
	}
}

// ─── #3117 gap 2: a bare key with a double-quoted value ──────────────────
//
// The JSON entry requires a QUOTED key; the util.inspect entry required a
// SINGLE-quoted value. A bare key with a double-quoted value satisfied
// neither, so it was redacted by nothing.
func TestSanitize_CredentialByKey_BareKeyDoubleQuotedValue(t *testing.T) {
	for _, line := range []string{
		`credential: "hunter2"`,
		`turnCredential: "hunter2"`,
		`{ password: "hunter2" }`,
		`{credential:"hunter2"}`,
	} {
		out := Sanitize(line)
		assert.NotContains(t, out, "hunter2", "line: %s", line)
		// It must be the KEY-shaped entry that ate it, not an incidental
		// shape match somewhere else in the table — and the key must survive
		// so a triager can still see WHAT was redacted.
		assert.Contains(t, out, "<redacted>", "line: %s", line)
	}
}
