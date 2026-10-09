package authoritycontract

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"math/big"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Digest is SHA-384 over the exact bytes (§2.4); every named digest uses it.
func Digest(b []byte) []byte { h := sha512.Sum384(b); return h[:] }

// EntryDigest is entry_digest: the exact bytes of the result message (D-133).
func EntryDigest(resultBytes []byte) []byte { return Digest(resultBytes) }

// KeyDigest is root_digest, signer_digest, legacy_ek_digest or key_digest:
// SHA-384 of a 97-byte point or of SPKI DER.
func KeyDigest(key []byte) []byte { return Digest(key) }

// CredentialEpochDigest maps a NULL (!valid) or "" epoch to "" (nil): a NULL
// epoch admits any token, so the same-epoch rule must never fire on it.
func CredentialEpochDigest(epoch string, valid bool) []byte {
	if !valid || epoch == "" {
		return nil
	}
	return Digest([]byte(epoch))
}

// PopulationRecord is one 84-byte population_digest record.
type PopulationRecord struct {
	AccountID      string
	LegacyEKDigest []byte
}

// PopulationDigest concatenates ascii(account_id)(36) ‖ legacy_ek_digest(48)
// in ascending account_id order and hashes the result.
func PopulationDigest(recs []PopulationRecord) ([]byte, error) {
	sorted := slices.Clone(recs)
	slices.SortFunc(sorted, func(a, b PopulationRecord) int { return strings.Compare(a.AccountID, b.AccountID) })
	buf := make([]byte, 0, 84*len(sorted))
	for i, r := range sorted {
		if _, ok := canonicalUUID(r.AccountID); !ok || len(r.LegacyEKDigest) != 48 {
			return nil, malformed("population record")
		}
		if i > 0 && sorted[i-1].AccountID == r.AccountID {
			return nil, malformed("duplicate account")
		}
		buf = append(buf, r.AccountID...)
		buf = append(buf, r.LegacyEKDigest...)
	}
	return Digest(buf), nil
}

// ParsePoint accepts only a 97-byte uncompressed P-384 point on the curve.
// The prefix check mirrors the Web Crypto rule (§2.4), although
// ParseUncompressedPublicKey refuses compressed, hybrid, off-curve and
// infinity encodings by itself.
func ParsePoint(b []byte) (*ecdsa.PublicKey, error) {
	if len(b) != 97 || b[0] != 0x04 {
		return nil, malformed("point form")
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P384(), b)
	if err != nil {
		return nil, malformed("point")
	}
	return pub, nil
}

// VerifyP384 verifies a 96-byte P1363 signature over SHA-384(msg). No DER.
func VerifyP384(point, msg, sig []byte) bool {
	pub, err := ParsePoint(point)
	if err != nil || len(sig) != 96 {
		return false
	}
	h := sha512.Sum384(msg)
	r := new(big.Int).SetBytes(sig[:48])
	s := new(big.Int).SetBytes(sig[48:])
	return ecdsa.Verify(pub, h[:], r, s)
}

// P384SPKI is the 120-byte SubjectPublicKeyInfo of a point (§2.4 TLS pin row).
func P384SPKI(point []byte) ([]byte, error) {
	pub, err := ParsePoint(point)
	if err != nil {
		return nil, err
	}
	return x509.MarshalPKIXPublicKey(pub)
}

// TLSPin is SHA-384 of a P-384 TLS key's SPKI DER (cp_tls_pins, LD-19).
func TLSPin(point []byte) ([]byte, error) {
	spki, err := P384SPKI(point)
	if err != nil {
		return nil, err
	}
	return Digest(spki), nil
}

// ValidateEKSPKI applies §2.4's three steps: length, parse, canonical.
func ValidateEKSPKI(spki []byte) (*rsa.PublicKey, error) {
	if len(spki) != 550 {
		return nil, malformed("ek spki length")
	}
	k, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return nil, malformed("ek spki parse")
	}
	pub, ok := k.(*rsa.PublicKey)
	if !ok {
		return nil, malformed("ek spki not rsa")
	}
	if err := checkEKParams(pub); err != nil {
		return nil, err
	}
	if err := checkCanonical(spki, pub); err != nil {
		return nil, err
	}
	return pub, nil
}

func checkEKParams(pub *rsa.PublicKey) error {
	if pub.N.BitLen() != 4096 {
		return malformed("ek modulus")
	}
	if pub.E != 65537 {
		return malformed("ek exponent")
	}
	return nil
}

// checkCanonical forces rsaEncryption, NULL params and minimal DER. Go's
// parser is not a canonicality check: a 550-byte SPKI whose BIT STRING
// declares an unused bit parses to the same key, and only this comparison
// refuses it.
func checkCanonical(spki []byte, pub *rsa.PublicKey) error {
	re, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil || !bytes.Equal(re, spki) {
		return malformed("ek spki not canonical")
	}
	return nil
}

// VerifyLegacyPSS verifies a legacy-key RSA-PSS signature (legacy-ek-pop, and
// query on a no-root head). VerifyPSS ignores opts.Hash; SaltLength is exact.
func VerifyLegacyPSS(spki, msg, sig []byte) bool {
	pub, err := ValidateEKSPKI(spki)
	if err != nil || len(sig) != 512 {
		return false
	}
	h := sha256.Sum256(msg)
	return rsa.VerifyPSS(pub, crypto.SHA256, h[:], sig, &rsa.PSSOptions{SaltLength: 32}) == nil
}

// DeviceID is D-237: SHA-384("concord-device-id/v1" ‖ 0x00 ‖ dsk), first 16
// bytes, version 4 and RFC 4122 variant bits set, canonical lowercase.
// It hashes any 97 bytes, so the caller MUST have validated dsk with
// ParsePoint. The ID keeps 122 hash bits: two distinct keys collide only by
// chance, which the birthday bound puts near 2^61 keys.
func DeviceID(dsk [97]byte) string {
	h := sha512.New384()
	h.Write([]byte("concord-device-id/v1"))
	h.Write([]byte{0})
	h.Write(dsk[:])
	var id uuid.UUID
	copy(id[:], h.Sum(nil)[:16])
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return id.String()
}

func checkPoint(b []byte) error  { _, err := ParsePoint(b); return err }
func checkEKSPKI(b []byte) error { _, err := ValidateEKSPKI(b); return err }

// kP97 is every b97 position: all of them are P-384 points (§2.4).
// kEKSPKI is every b550 position: ek_public_key and legacy_spki.
var (
	kP97    = kB(97).withBin(checkPoint)
	kEKSPKI = kB(550).withBin(checkEKSPKI)
)
