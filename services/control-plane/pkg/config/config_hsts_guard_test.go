package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateProductionHSTSHeaderValueGuard covers the #2318 override guard:
// a set-but-malformed HSTS_HEADER_VALUE would silently weaken HSTS, so it is
// fatal in production. Empty (use the middleware default) and any
// "max-age="-prefixed policy — including the RFC 6797 policy-clear
// "max-age=0" — are allowed. Inert outside production.
func TestValidateProductionHSTSHeaderValueGuard(t *testing.T) {
	t.Run("production + malformed value is fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "includeSubDomains; preload"
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HSTS_HEADER_VALUE")
	})

	// Browsers silently ignore an invalid STS policy, so a value that merely
	// STARTS with max-age= must still be rejected when the policy is not
	// structurally valid (#2318 review, Codex P2).
	t.Run("production + non-numeric max-age is fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=abc"
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HSTS_HEADER_VALUE")
	})

	t.Run("production + control characters are fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000; include\r\nSubDomains"
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HSTS_HEADER_VALUE")
	})

	t.Run("production + trailing garbage directive is fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000; includeSubDomains; pre load!"
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HSTS_HEADER_VALUE")
	})

	t.Run("production + full default policy shape is allowed", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000; includeSubDomains; preload"
		assert.NoError(t, cfg.validate())
	})

	// RFC 6797 §6.1: a UA ignores an STS field with ANY duplicated directive,
	// so a duplicate would silently disable HSTS (#2318 review, Codex P2 #2).
	t.Run("production + duplicate max-age is fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000; max-age=0"
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HSTS_HEADER_VALUE")
	})

	t.Run("production + duplicate directive (case-insensitive) is fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000; includeSubDomains; INCLUDESUBDOMAINS"
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HSTS_HEADER_VALUE")
	})

	// RFC 6797 permits any directive order and case-insensitive names — a
	// browser-valid policy must not abort startup (#2318 review, Gitar edge).
	t.Run("production + max-age not first is allowed", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "includeSubDomains; max-age=63072000"
		assert.NoError(t, cfg.validate())
	})

	t.Run("production + case-variant Max-Age is allowed", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "Max-Age=63072000; preload"
		assert.NoError(t, cfg.validate())
	})

	// RFC 6797: includeSubDomains and preload are VALUELESS directives; a UA
	// drops the malformed directive but keeps max-age, silently leaving
	// subdomains uncovered (#2318 review, Codex P2 #3).
	t.Run("production + valued includeSubDomains is fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000; includeSubDomains=true"
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HSTS_HEADER_VALUE")
	})

	t.Run("production + valued preload is fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000; preload=yes"
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HSTS_HEADER_VALUE")
	})

	// RFC-valid shapes browsers accept must not abort startup (#2318 review):
	// empty directive slots (trailing semicolon) and quoted-string extension
	// directive values.
	t.Run("production + trailing semicolon is allowed", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000; includeSubDomains;"
		assert.NoError(t, cfg.validate())
	})

	t.Run("production + quoted extension directive value is allowed", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=31536000; vendor-ext=\"foo bar\""
		assert.NoError(t, cfg.validate())
	})

	// A trailing backslash escapes the apparent closing quote — an invalid
	// quoted-string a UA may discard (#2318 review); escapes are unsupported.
	t.Run("production + backslash in quoted value is fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000; vendor-ext=\"foo\\\""
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HSTS_HEADER_VALUE")
	})

	// RFC delta-seconds is unbounded 1*DIGIT; UAs clamp huge values, so an
	// 11-digit max-age must not abort startup (#2318 review).
	t.Run("production + max-age beyond ten digits is allowed", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=10000000000"
		assert.NoError(t, cfg.validate())
	})

	// DEL is not permitted in an RFC quoted-string, and Go's HTTP/2 writer
	// silently OMITS headers containing it — HSTS would vanish despite a
	// started service (#2318 review).
	t.Run("production + DEL in quoted value is fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000; vendor-ext=\"foo\x7fbar\""
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HSTS_HEADER_VALUE")
	})

	// The printable-ASCII whole-value gate: Unicode whitespace (NBSP) would
	// survive into the emitted header while TrimSpace hid it from validation
	// (#2318 review). Tab is legitimate RFC OWS and stays allowed.
	t.Run("production + non-breaking space is fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000;\u00a0includeSubDomains"
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HSTS_HEADER_VALUE")
	})

	t.Run("production + tab as OWS is allowed", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000;\tincludeSubDomains"
		assert.NoError(t, cfg.validate())
	})

	t.Run("production + empty value is allowed (middleware default)", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = ""
		assert.NoError(t, cfg.validate())
	})

	t.Run("production + max-age policy is allowed", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=63072000; includeSubDomains"
		assert.NoError(t, cfg.validate())
	})

	t.Run("production + max-age=0 policy-clear is allowed", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.HSTSHeaderValue = "max-age=0"
		assert.NoError(t, cfg.validate())
	})

	t.Run("development + malformed value is inert", func(t *testing.T) {
		cfg := &Config{Environment: "development", HSTSHeaderValue: "garbage"}
		assert.NoError(t, cfg.validate())
	})
}
