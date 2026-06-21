package age

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const fixturePath = "testdata/age-claim-canonical-v1.json"

type fixtureClaim struct {
	CanonicalVersion       int    `json:"canonical_version"`
	UserID                 string `json:"user_id"`
	ValidAge               bool   `json:"valid_age"`
	NSFWAuth               bool   `json:"nsfw_auth"`
	JurisdictionObligation int    `json:"jurisdiction_obligation"`
	Nonce                  string `json:"nonce"`
	Timestamp              int64  `json:"timestamp"`
	KeyVersion             int    `json:"key_version"`
	ClientVersion          string `json:"client_version"`
}

type fixture struct {
	Comment          string       `json:"_comment"`
	CanonicalVersion int          `json:"canonical_version"`
	Claim            fixtureClaim `json:"claim"`
	CanonicalUTF8    string       `json:"canonical_utf8"`
	SaltLength       string       `json:"salt_length"`
	SignatureB64     *string      `json:"signature_b64"`
	PublicKeySPKIB64 *string      `json:"public_key_spki_b64"`
}

func (f fixture) toClaim() Claim {
	return Claim{
		CanonicalVersion:       f.Claim.CanonicalVersion,
		UserID:                 f.Claim.UserID,
		ValidAge:               f.Claim.ValidAge,
		NSFWAuth:               f.Claim.NSFWAuth,
		JurisdictionObligation: f.Claim.JurisdictionObligation,
		Nonce:                  f.Claim.Nonce,
		Timestamp:              f.Claim.Timestamp,
		KeyVersion:             f.Claim.KeyVersion,
		ClientVersion:          f.Claim.ClientVersion,
	}
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	require.NoError(t, err)
	var f fixture
	require.NoError(t, json.Unmarshal(raw, &f))
	return f
}

func parseFixtureKey(t *testing.T, spkiB64 string) *rsa.PublicKey {
	t.Helper()
	der, err := base64.StdEncoding.DecodeString(spkiB64)
	require.NoError(t, err)
	pubAny, err := x509.ParsePKIXPublicKey(der)
	require.NoError(t, err)
	pub, ok := pubAny.(*rsa.PublicKey)
	require.True(t, ok)
	return pub
}

// TestBackfillFixture regenerates the cross-impl fixture's signature + public key.
// Run once with UPDATE_AGE_FIXTURE=1 to (re)populate signature_b64 + public_key_spki_b64
// with a freshly-generated test keypair; the private key is discarded (never committed).
// Child B verifies the committed signature against the committed public key for parity.
func TestBackfillFixture(t *testing.T) {
	if os.Getenv("UPDATE_AGE_FIXTURE") != "1" {
		t.Skip("set UPDATE_AGE_FIXTURE=1 to regenerate the fixture signature")
	}
	f := loadFixture(t)
	claim := f.toClaim()
	// Sanity: the fixture's canonical bytes must match its declared canonical_utf8.
	got, err := claim.CanonicalBytes()
	require.NoError(t, err)
	require.Equal(t, f.CanonicalUTF8, string(got))

	key, err := rsa.GenerateKey(rand.Reader, 4096)
	require.NoError(t, err)
	h := sha256.Sum256(got)
	sig, err := rsa.SignPSS(rand.Reader, key, crypto.SHA256, h[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	require.NoError(t, err)
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)

	sigB64 := base64.StdEncoding.EncodeToString(sig)
	spkiB64 := base64.StdEncoding.EncodeToString(spki)
	f.SignatureB64 = &sigB64
	f.PublicKeySPKIB64 = &spkiB64

	out, err := json.MarshalIndent(f, "", "  ")
	require.NoError(t, err)
	out = append(out, '\n')
	require.NoError(t, os.WriteFile(filepath.Clean(fixturePath), out, 0o600))
	t.Logf("fixture backfilled: %s", fixturePath)
}
