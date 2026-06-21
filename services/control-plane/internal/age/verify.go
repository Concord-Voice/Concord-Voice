package age

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"errors"
)

// Signature/key-lookup sentinel errors. ErrInvalidSignature is intentionally the
// single failure mode of the verify path — decode, canonical-build, and PSS-verify
// failures all collapse to it so the wire response is not an oracle distinguishing
// "bad base64" from "tampered claim" from "wrong key".
var (
	ErrNoSigningKey     = errors.New("no signing key")
	ErrStaleKeyVersion  = errors.New("stale key_version")
	ErrInvalidSignature = errors.New("invalid signature")
)

// minRSABits is the project-minimum RSA modulus (e2ee.md). Enforced on the modulus
// bit length (pub.N.BitLen()), NOT pub.Size() — Size() returns bytes (512 for a
// 4096-bit key), so the equivalent `Size() < minRSABits` check would compare
// 512 < 4096 and erroneously REJECT every realistic key (an availability bug, not a
// weak-key acceptance). BitLen() is the correct test.
const minRSABits = 4096

// VerifySignature checks the RSA-PSS signature (salt length = hash length = 32) over
// the claim's canonical bytes. Pure: no DB, no I/O. Returns ErrInvalidSignature on any
// failure (nil/undersized key, bad base64, un-buildable claim, PSS mismatch).
//
// The salt-length pin (PSSSaltLengthEqualsHash) is a hard contract term: the child-B
// signer MUST use salt length 32; a PSSSaltLengthAuto (max-salt) signature is rejected.
func VerifySignature(pub *rsa.PublicKey, c Claim, sigB64 string) error {
	if pub == nil || pub.N == nil || pub.N.BitLen() < minRSABits {
		return ErrInvalidSignature
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return ErrInvalidSignature
	}
	msg, err := c.CanonicalBytes()
	if err != nil {
		return ErrInvalidSignature
	}
	h := sha256.Sum256(msg)
	if err := rsa.VerifyPSS(pub, crypto.SHA256, h[:], sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}); err != nil {
		return ErrInvalidSignature
	}
	return nil
}

// LoadCurrentKey fetches the user's current public key + version, matching the existing
// public_keys read pattern (ORDER BY key_version DESC LIMIT 1). It does NOT select by
// (user_id, key_version) — the table has no unique constraint on that pair, and a key
// rotation (including RecoveryResetAccount, which bumps key_version) must invalidate
// outstanding claims signed under the old version.
//
//   - no row            → ErrNoSigningKey   (SSO-only / pending accounts may lack a key)
//   - stored != wanted  → ErrStaleKeyVersion (client must re-sign under the current version)
//   - corrupt SPKI/non-RSA → ErrInvalidSignature (the stored key can verify nothing)
func LoadCurrentKey(ctx context.Context, db *sql.DB, userID string, wantVersion int) (*rsa.PublicKey, error) {
	var spki []byte
	var storedVer int
	err := db.QueryRowContext(ctx,
		`SELECT public_key, key_version FROM public_keys WHERE user_id = $1 ORDER BY key_version DESC LIMIT 1`,
		userID).Scan(&spki, &storedVer)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSigningKey
	}
	if err != nil {
		return nil, err
	}
	if storedVer != wantVersion {
		return nil, ErrStaleKeyVersion
	}
	pubAny, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return nil, ErrInvalidSignature
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		return nil, ErrInvalidSignature
	}
	return pub, nil
}
