package age

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func signClaim(t *testing.T, k *rsa.PrivateKey, c Claim, salt int) string {
	t.Helper()
	msg, err := c.CanonicalBytes()
	require.NoError(t, err)
	h := sha256.Sum256(msg)
	sig, err := rsa.SignPSS(rand.Reader, k, crypto.SHA256, h[:], &rsa.PSSOptions{SaltLength: salt})
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(sig)
}

func TestVerifySignature_AcceptsValid(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 4096)
	require.NoError(t, err)
	c := validClaim()
	sig := signClaim(t, k, c, rsa.PSSSaltLengthEqualsHash)
	assert.NoError(t, VerifySignature(&k.PublicKey, c, sig))
}

func TestVerifySignature_RejectsTamperWrongKeyAndBadSalt(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 4096)
	require.NoError(t, err)
	c := validClaim()
	good := signClaim(t, k, c, rsa.PSSSaltLengthEqualsHash)

	// Tampered claim: a single flipped boolean must fail (the signed bytes differ).
	tampered := c
	tampered.ValidAge = false
	assert.ErrorIs(t, VerifySignature(&k.PublicKey, tampered, good), ErrInvalidSignature)

	// Wrong key.
	other, err := rsa.GenerateKey(rand.Reader, 4096)
	require.NoError(t, err)
	assert.ErrorIs(t, VerifySignature(&other.PublicKey, c, good), ErrInvalidSignature)

	// Max-salt-length signature must be REJECTED — locks the salt-length=hash pin
	// (PSSSaltLengthAuto = max salt on a 4096-bit key; child B MUST use salt=32).
	maxSalt := signClaim(t, k, c, rsa.PSSSaltLengthAuto)
	assert.ErrorIs(t, VerifySignature(&k.PublicKey, c, maxSalt), ErrInvalidSignature)
}

func TestVerifySignature_RejectsSmallKey(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	c := validClaim()
	sig := signClaim(t, small, c, rsa.PSSSaltLengthEqualsHash)
	// Even a valid signature under a sub-4096 key must be rejected (BitLen guard).
	assert.ErrorIs(t, VerifySignature(&small.PublicKey, c, sig), ErrInvalidSignature)
}

func TestVerifySignature_RejectsMalformedInputs(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 4096)
	require.NoError(t, err)
	c := validClaim()

	// nil public key.
	assert.ErrorIs(t, VerifySignature(nil, c, "AAAA"), ErrInvalidSignature)
	// non-base64 signature.
	assert.ErrorIs(t, VerifySignature(&k.PublicKey, c, "!!!not base64!!!"), ErrInvalidSignature)
	// well-formed base64 but garbage signature bytes.
	assert.ErrorIs(t, VerifySignature(&k.PublicKey, c, base64.StdEncoding.EncodeToString([]byte("garbage"))), ErrInvalidSignature)
	// claim that cannot produce canonical bytes (bad canonical_version) → reject, no panic.
	bad := c
	bad.CanonicalVersion = 99
	assert.ErrorIs(t, VerifySignature(&k.PublicKey, bad, "AAAA"), ErrInvalidSignature)
}

// TestFixtureByteParity locks the committed cross-impl fixture: the Go builder must
// reproduce the fixture's canonical_utf8 exactly, and (once backfilled) the fixture's
// signature must verify against the fixture's public key. This is the child-B contract.
func TestFixtureByteParity(t *testing.T) {
	fx := loadFixture(t)
	got, err := fx.toClaim().CanonicalBytes()
	require.NoError(t, err)
	assert.Equal(t, fx.CanonicalUTF8, string(got), "Go canonical bytes must match fixture canonical_utf8")

	if fx.SignatureB64 == nil || fx.PublicKeySPKIB64 == nil {
		t.Skip("fixture signature not yet backfilled")
	}
	pub := parseFixtureKey(t, *fx.PublicKeySPKIB64)
	assert.NoError(t, VerifySignature(pub, fx.toClaim(), *fx.SignatureB64),
		"fixture signature must verify against fixture key (child-B byte+sig parity)")
}
