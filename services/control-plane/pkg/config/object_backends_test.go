package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Attachment-backend table coverage (ADR-0038 / #2759).
//
// This file had no tests. Two things needed pinning: the credential gate that
// decides whether a configured backend becomes live, and the String()
// redaction, since ObjectBackend carries an access key and a secret and is the
// kind of struct that ends up in a %v somewhere.

// TestAttachmentBackends_CredentialGate uses the full four-case matrix, not
// just the two interesting rows. Both half-credentialled cases are what make
// the `||` falsifiable: with `&&` the key-only and secret-only rows would go
// live carrying an unusable credential, and a two-row test would not notice.
func TestAttachmentBackends_CredentialGate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		key      string
		secret   string
		wantLive bool
	}{
		{"both credentials present", "AKIAEXAMPLE", "s3cr3t", true},
		{"key only", "AKIAEXAMPLE", "", false},
		{"secret only", "", "s3cr3t", false},
		{"neither — the dormant EU/Asia bucket state", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{CloudflareR2: CloudflareR2Config{
				Endpoint:        "https://accountid.r2.cloudflarestorage.com",
				Region:          "auto",
				Bucket:          "concord-voice-r2-us-east",
				AccessKeyID:     tc.key,
				SecretAccessKey: tc.secret,
			}}

			got := cfg.AttachmentBackends()
			if !tc.wantLive {
				assert.Empty(t, got, "a backend missing either credential must not go live")
				return
			}
			require.Len(t, got, 1)
			assert.Equal(t, AttachmentBackendR2USEast, got[0].ID)
			assert.Equal(t, "concord-voice-r2-us-east", got[0].Bucket)
			assert.Equal(t, "auto", got[0].Region)
		})
	}
}

// TestAttachmentBackends_NilConfig — the registry calls this on a possibly-nil
// Config, so a nil receiver must yield no backends rather than panic at boot.
func TestAttachmentBackends_NilConfig(t *testing.T) {
	var cfg *Config
	assert.Nil(t, cfg.AttachmentBackends())
}

// TestObjectBackendString_RedactsBothCredentials — the whole point of the
// custom String(). Asserting the length marker is present is not enough; the
// test must assert the secret VALUES are absent, which is what actually fails
// if a field is added to the format string verbatim.
func TestObjectBackendString_RedactsBothCredentials(t *testing.T) {
	backend := ObjectBackend{
		ID:              AttachmentBackendR2USEast,
		Endpoint:        "https://accountid.r2.cloudflarestorage.com",
		Region:          "auto",
		Bucket:          "concord-voice-r2-us-east",
		AccessKeyID:     "AKIAEXAMPLEKEYID",
		SecretAccessKey: "super-secret-value",
	}

	rendered := backend.String()

	assert.NotContains(t, rendered, "AKIAEXAMPLEKEYID")
	assert.NotContains(t, rendered, "super-secret-value")
	assert.Contains(t, rendered, "REDACTED")

	// The non-secret destination fields SHOULD survive — a redaction that hides
	// the endpoint too would make a misconfiguration undiagnosable.
	assert.Contains(t, rendered, "concord-voice-r2-us-east")
	assert.Contains(t, rendered, "accountid.r2.cloudflarestorage.com")

	// Length markers are present and correct, so an operator can tell "unset"
	// from "set to the wrong thing" without seeing either value.
	assert.Contains(t, rendered, "16 bytes")
	assert.Contains(t, rendered, "18 bytes")
}

// TestObjectBackendString_EmptyCredentialsRenderZeroLength — an unset
// credential must be visibly zero-length rather than indistinguishable from a
// populated one.
func TestObjectBackendString_EmptyCredentialsRenderZeroLength(t *testing.T) {
	rendered := ObjectBackend{ID: "r2-eu"}.String()
	assert.Equal(t, 2, strings.Count(rendered, "0 bytes"))
}
