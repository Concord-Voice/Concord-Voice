package authoritycontract

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParsePoint(t *testing.T) {
	p := testPoint(t)
	_, err := ParsePoint(p)
	require.NoError(t, err)

	offCurve := slices.Clone(p)
	offCurve[96] ^= 1
	compressed := append([]byte{0x02 | (p[96] & 1)}, p[1:49]...)
	for name, b := range map[string][]byte{
		"compressed (49 bytes)": compressed,
		"off-curve":             offCurve,
		"hybrid":                hybridOf(p),
		"96 bytes":              p[:96],
		"infinity":              {0x00},
		"empty":                 {},
		"nil":                   nil,
	} {
		_, err := ParsePoint(b)
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}

func TestVerifyP384(t *testing.T) {
	k := testKey(t)
	p := pointOf(t, k)
	msg := []byte("concord")
	sig := signP1363(t, k, msg)
	require.True(t, VerifyP384(p, msg, sig))
	require.False(t, VerifyP384(p, []byte("other"), sig))
	require.False(t, VerifyP384(p, msg, sig[:95]))
	require.False(t, VerifyP384(p, msg, append(slices.Clone(sig), 0)))
	h := sha512.Sum384(msg)
	der, err := ecdsa.SignASN1(rand.Reader, k, h[:])
	require.NoError(t, err)
	require.False(t, VerifyP384(p, msg, der), "DER is never accepted")
	require.False(t, VerifyP384(hybridOf(p), msg, sig))

	// The length is exactly 96 in both directions. Each 97- or 95-byte form
	// below decodes to the valid (r, s) under some weaker length check.
	require.False(t, VerifyP384(p, msg, slices.Concat(sig[:48], []byte{0}, sig[48:])), "r || 0x00 || s (97 bytes)")
	require.False(t, VerifyP384(p, msg, slices.Concat([]byte{0}, sig)), "0x00 || r || s (97 bytes)")
	sZero, rZero := groundZeroTopBytes(t, k, msg)
	require.True(t, VerifyP384(p, msg, sZero), "control: the ground signature is valid")
	require.True(t, VerifyP384(p, msg, rZero), "control: the ground signature is valid")
	require.False(t, VerifyP384(p, msg, slices.Concat(sZero[:48], sZero[49:])), "r || s' (95 bytes): s's zero top byte dropped")
	require.False(t, VerifyP384(p, msg, slices.Concat(rZero[1:48], rZero[48:])), "r' || s (95 bytes): r's zero top byte dropped")

	require.False(t, VerifyP384(p, msg, nil), "nil signature")
	require.False(t, VerifyP384(p, msg, []byte{}), "empty signature")
	require.False(t, VerifyP384(nil, msg, sig), "nil point")
	require.False(t, VerifyP384([]byte{}, msg, sig), "empty point")
}

// groundZeroTopBytes signs msg until it has one signature whose s has a zero
// top byte and one whose r has, so dropping that byte gives a 95-byte encoding
// of the same valid (r, s). Each happens about once in 256 signatures; the
// bound fails the test rather than skipping it.
func groundZeroTopBytes(t *testing.T, k *ecdsa.PrivateKey, msg []byte) (sZero, rZero []byte) {
	t.Helper()
	const bound = 20000
	for i := 0; i < bound && (sZero == nil || rZero == nil); i++ {
		sig := signP1363(t, k, msg)
		if sZero == nil && sig[48] == 0 {
			sZero = sig
		}
		if rZero == nil && sig[0] == 0 {
			rZero = sig
		}
	}
	require.NotNil(t, sZero, "no s with a zero top byte in %d signatures", bound)
	require.NotNil(t, rZero, "no r with a zero top byte in %d signatures", bound)
	return sZero, rZero
}

// TestVerifyP384ScalarRange pins what VerifyP384 inherits from ecdsa.Verify:
// r and s must each lie in [1, n-1], and both the low and the high s verify.
// EN7 (D-85) does not require low-s, Web Crypto accepts high-s, and §3.1's
// malleated-signature vector needs n-s to verify.
func TestVerifyP384ScalarRange(t *testing.T) {
	k := testKey(t)
	p := pointOf(t, k)
	msg := []byte("concord")
	sig := signP1363(t, k, msg)
	n := elliptic.P384().Params().N
	withHalf := func(half int, v *big.Int) []byte {
		out := slices.Clone(sig)
		v.FillBytes(out[48*half : 48*half+48])
		return out
	}

	// Whichever form the signer produced, test both: n is odd, so exactly one
	// of s and n-s is above n/2.
	halfN := new(big.Int).Rsh(n, 1)
	s := new(big.Int).SetBytes(sig[48:])
	low, high := s, new(big.Int).Sub(n, s)
	if s.Cmp(halfN) > 0 {
		low, high = high, low
	}
	require.Equal(t, 1, high.Cmp(halfN), "the high form is above n/2")
	require.True(t, VerifyP384(p, msg, withHalf(1, high)), "high s verifies (EN7)")
	require.True(t, VerifyP384(p, msg, withHalf(1, low)), "low s verifies")

	for name, b := range map[string][]byte{
		"r = 0":    withHalf(0, big.NewInt(0)),
		"s = 0":    withHalf(1, big.NewInt(0)),
		"r = n":    withHalf(0, n),
		"s = n":    withHalf(1, n),
		"all zero": make([]byte, 96),
	} {
		require.False(t, VerifyP384(p, msg, b), name)
	}
}

// TestPointKind exercises kP97 and its hook checkPoint, which every b97
// position carries (§2.4), at the accept and refuse boundaries.
func TestPointKind(t *testing.T) {
	p := testPoint(t)
	require.NoError(t, checkPoint(p))
	require.NoError(t, kP97.checkBytes(p))

	offCurve := slices.Clone(p)
	offCurve[96] ^= 1
	hybrid06, hybrid07 := slices.Clone(p), slices.Clone(p)
	hybrid06[0], hybrid07[0] = 0x06, 0x07
	for name, b := range map[string][]byte{
		"hybrid 0x06": hybrid06,
		"hybrid 0x07": hybrid07,
		"off-curve":   offCurve,
		"96 bytes":    p[:96],
		"98 bytes":    append(slices.Clone(p), 0),
	} {
		require.ErrorIs(t, checkPoint(b), ErrMalformed, name)
		require.ErrorIs(t, kP97.checkBytes(b), ErrMalformed, name)
	}
	require.ErrorIs(t, kP97.checkBytes(nil), ErrMalformed, `b97 does not admit ""`)
	require.NoError(t, kP97.OrEmpty().checkBytes(nil), `b97|"" admits ""`)
	require.ErrorIs(t, kP97.OrEmpty().checkBytes(hybrid06), ErrMalformed, "OrEmpty keeps the point rule")
}

// ekStage names the first §2.4 step that refuses an EK SPKI.
type ekStage string

const (
	stageLength    ekStage = "length"
	stageParse     ekStage = "parse"
	stageNotRSA    ekStage = "not-rsa"
	stageModulus   ekStage = "modulus"
	stageExponent  ekStage = "exponent"
	stageCanonical ekStage = "canonical"
	stageAccepted  ekStage = "accepted"
)

// ekFirstStage replays §2.4's steps with the standard library alone, so it
// describes the vector rather than the code under test.
func ekFirstStage(b []byte) ekStage {
	if len(b) != 550 {
		return stageLength
	}
	k, err := x509.ParsePKIXPublicKey(b)
	if err != nil {
		return stageParse
	}
	pub, ok := k.(*rsa.PublicKey)
	if !ok {
		return stageNotRSA
	}
	if pub.N.BitLen() != 4096 {
		return stageModulus
	}
	if pub.E != 65537 {
		return stageExponent
	}
	re, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil || !bytes.Equal(re, b) {
		return stageCanonical
	}
	return stageAccepted
}

type ekNegative struct {
	name  string
	b     []byte
	stage ekStage
}

// ekNegatives is DoR §3.1's SPKI negative set plus the vectors that reach the
// later steps, each with the first §2.4 step that must refuse it.
func ekNegatives(t *testing.T) []ekNegative {
	t.Helper()
	spki := testEKSPKI(t)
	pub := &testRSA(t).PublicKey
	small, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	spki2048, err := x509.MarshalPKIXPublicKey(&small.PublicKey)
	require.NoError(t, err)
	e3, err := x509.MarshalPKIXPublicKey(&rsa.PublicKey{N: pub.N, E: 3})
	require.NoError(t, err)
	return []ekNegative{
		{"empty", nil, stageLength},
		{"2048-bit", spki2048, stageLength},
		{"e = 3", e3, stageLength},
		{"absent NULL", ekAbsentNull(t, spki), stageLength},
		{"trailing bytes", append(slices.Clone(spki), 0, 0), stageLength},
		{"549 bytes", spki[:549], stageLength},
		{"551 bytes", append(slices.Clone(spki), 0), stageLength},
		{"RSASSA-PSS OID", ekPSSOID(t, spki), stageParse},
		{"params 04 00, not NULL", ekParamsOctetString(t, spki), stageParse},
		{"BER indefinite length", ekIndefiniteLength(t, spki), stageParse},
		{"DSA SPKI, 550 bytes", ekDSA550(t), stageNotRSA},
		{"4097-bit modulus", ekModulus4097(t, spki), stageModulus},
		{"e = 65539", ekExponent65539(t, spki), stageExponent},
		{"BIT STRING with an unused bit", ekBitStringUnusedBit(t, spki), stageCanonical},
	}
}

func TestValidateEKSPKI(t *testing.T) {
	spki := testEKSPKI(t)
	require.Len(t, spki, 550)
	_, err := ValidateEKSPKI(spki)
	require.NoError(t, err)

	for _, n := range ekNegatives(t) {
		t.Run(n.name, func(t *testing.T) {
			_, err := ValidateEKSPKI(n.b)
			require.ErrorIs(t, err, ErrMalformed)
		})
	}
}

// TestEKSPKINegativesReachTheirStage pins which §2.4 step refuses each
// negative, so no vector passes TestValidateEKSPKI because an earlier step
// caught it. Only the DSA SPKI reaches the RSA type check, only the 4097-bit
// modulus the modulus check, only e = 65539 the exponent check, and only the
// unused-bit BIT STRING the re-marshal comparison.
func TestEKSPKINegativesReachTheirStage(t *testing.T) {
	require.Equal(t, stageAccepted, ekFirstStage(testEKSPKI(t)), "positive control")
	for _, n := range ekNegatives(t) {
		t.Run(n.name, func(t *testing.T) {
			require.Equal(t, n.stage, ekFirstStage(n.b))
		})
	}
}

// Go's parser is not a canonicality check: the 550-byte SPKI whose BIT STRING
// declares an unused bit parses to the same key, and only the re-marshal
// comparison refuses it. This also tests the comparison in isolation, so
// mutation M3 fails here as well as through ValidateEKSPKI.
func TestCheckCanonicalRefusesDifferentBytes(t *testing.T) {
	pub := &testRSA(t).PublicKey
	good, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	require.NoError(t, checkCanonical(good, pub))
	other := slices.Clone(good)
	other[100] ^= 1 // inside the modulus
	require.ErrorIs(t, checkCanonical(other, pub), ErrMalformed)

	unusedBit := ekBitStringUnusedBit(t, good)
	require.Len(t, unusedBit, 550)
	k, err := x509.ParsePKIXPublicKey(unusedBit)
	require.NoError(t, err)
	require.True(t, pub.Equal(k), "the parser returns the same key")
	require.ErrorIs(t, checkCanonical(unusedBit, pub), ErrMalformed)
}

// TestEKSPKIKind exercises kEKSPKI and its hook checkEKSPKI, which
// ek_public_key and legacy_spki carry, at the accept and refuse boundaries.
func TestEKSPKIKind(t *testing.T) {
	spki := testEKSPKI(t)
	require.NoError(t, checkEKSPKI(spki))
	require.NoError(t, kEKSPKI.checkBytes(spki))

	for _, n := range ekNegatives(t) {
		require.ErrorIs(t, checkEKSPKI(n.b), ErrMalformed, n.name)
		require.ErrorIs(t, kEKSPKI.checkBytes(n.b), ErrMalformed, n.name)
	}
	require.ErrorIs(t, kEKSPKI.checkBytes(testPoint(t)), ErrMalformed, "97-byte point in a b550 slot")
	require.ErrorIs(t, kEKSPKI.checkBytes(nil), ErrMalformed, `b550 does not admit ""`)
}

// The 550-byte EK SPKI is, by offset:
//
//	[0:4]     30 82 02 22  SubjectPublicKeyInfo SEQUENCE, 546 bytes
//	[4:6]     30 0d        AlgorithmIdentifier SEQUENCE
//	[6:17]    06 09 …01    OID 1.2.840.113549.1.1.1 (rsaEncryption)
//	[17:19]   05 00        NULL parameters
//	[19:23]   03 82 02 0f  BIT STRING, 527 bytes
//	[23]      00           unused bits
//	[24:28]   30 82 02 0a  RSAPublicKey SEQUENCE, 522 bytes
//	[28:32]   02 82 02 01  modulus INTEGER, 513 bytes
//	[32]      00           leading zero (the modulus's top bit is set)
//	[33:545]  modulus, 512 bytes
//	[545:550] 02 03 01 00 01  exponent 65537

// ekAbsentNull drops the NULL parameters and shortens both enclosing
// lengths. The result is 548 bytes, so the length check refuses it first.
func ekAbsentNull(t *testing.T, spki []byte) []byte {
	t.Helper()
	require.Equal(t, []byte{0x05, 0x00}, spki[17:19])
	return slices.Concat([]byte{0x30, 0x82, 0x02, 0x20, 0x30, 0x0b}, spki[6:17], spki[19:])
}

// ekParamsOctetString replaces the NULL parameters (05 00) with an empty
// OCTET STRING (04 00). It stays 550 bytes; the parser requires NULL.
func ekParamsOctetString(t *testing.T, spki []byte) []byte {
	t.Helper()
	b := slices.Clone(spki)
	require.Equal(t, []byte{0x05, 0x00}, b[17:19])
	b[17] = 0x04
	return b
}

// ekPSSOID changes the algorithm OID to 1.2.840.113549.1.1.10 (RSASSA-PSS).
func ekPSSOID(t *testing.T, spki []byte) []byte {
	t.Helper()
	b := slices.Clone(spki)
	require.Equal(t, byte(0x01), b[16]) // last byte of 1.2.840.113549.1.1.1
	b[16] = 0x0a
	return b
}

// ekExponent65539 sets e = 65539. That keeps the 3-byte exponent and the
// 550-byte length, so only the E check can refuse it (DoR's e = 3 vector dies
// at the length check).
func ekExponent65539(t *testing.T, spki []byte) []byte {
	t.Helper()
	b := slices.Clone(spki)
	require.Equal(t, []byte{0x02, 0x03, 0x01, 0x00, 0x01}, b[545:])
	b[549] = 0x03
	return b
}

// ekModulus4097 replaces the modulus's leading 0x00 with 0x01. N + 2^4096 is
// still minimal DER in 550 bytes, so only the N.BitLen() check refuses it.
func ekModulus4097(t *testing.T, spki []byte) []byte {
	t.Helper()
	b := slices.Clone(spki)
	require.Equal(t, []byte{0x02, 0x82, 0x02, 0x01, 0x00}, b[28:33])
	b[32] = 0x01
	return b
}

// ekIndefiniteLength re-encodes the outer SEQUENCE with BER's indefinite
// length (30 80 … 00 00): the same key in 550 bytes, which DER forbids.
func ekIndefiniteLength(t *testing.T, spki []byte) []byte {
	t.Helper()
	require.Equal(t, []byte{0x30, 0x82, 0x02, 0x22}, spki[:4])
	return slices.Concat([]byte{0x30, 0x80}, spki[4:], []byte{0x00, 0x00})
}

// ekBitStringUnusedBit declares one unused bit in subjectPublicKey and shifts
// bytes 24..549 left by one bit. BitString.RightAlign undoes the shift, so Go's
// parser decodes the same RSAPublicKey from different bytes.
func ekBitStringUnusedBit(t *testing.T, spki []byte) []byte {
	t.Helper()
	require.Equal(t, []byte{0x03, 0x82, 0x02, 0x0f, 0x00, 0x30}, spki[19:25])
	b := slices.Clone(spki)
	b[23] = 0x01
	for i := 24; i < len(b); i++ {
		var carry byte
		if i+1 < len(spki) {
			carry = spki[i+1] >> 7
		}
		b[i] = spki[i]<<1 | carry
	}
	return b
}

// ekDSA550 is a 550-byte DSA SubjectPublicKeyInfo (OID 1.2.840.10040.4.1).
// Go's parser returns a non-RSA key for it, so only the RSA type check refuses
// it. The integers are filler: the parser checks only that each is positive.
func ekDSA550(t *testing.T) []byte {
	t.Helper()
	filler := func(n int) *big.Int { return new(big.Int).SetBytes(bytes.Repeat([]byte{0x5a}, n)) }
	params, err := asn1.Marshal(struct{ P, Q, G *big.Int }{filler(128), filler(20), filler(128)})
	require.NoError(t, err)
	y, err := asn1.Marshal(filler(238))
	require.NoError(t, err)
	type algorithmIdentifier struct {
		Algorithm  asn1.ObjectIdentifier
		Parameters asn1.RawValue
	}
	b, err := asn1.Marshal(struct {
		Algorithm algorithmIdentifier
		PublicKey asn1.BitString
	}{
		algorithmIdentifier{asn1.ObjectIdentifier{1, 2, 840, 10040, 4, 1}, asn1.RawValue{FullBytes: params}},
		asn1.BitString{Bytes: y, BitLength: 8 * len(y)},
	})
	require.NoError(t, err)
	require.Len(t, b, 550)
	k, err := x509.ParsePKIXPublicKey(b)
	require.NoError(t, err, "the parser admits the DSA key")
	_, isRSA := k.(*rsa.PublicKey)
	require.False(t, isRSA)
	return b
}

func TestVerifyLegacyPSS(t *testing.T) {
	k := testRSA(t)
	spki := testEKSPKI(t)
	msg := []byte("legacy-ek-pop bytes")
	h := sha256.Sum256(msg)
	sig, err := rsa.SignPSS(rand.Reader, k, crypto.SHA256, h[:], &rsa.PSSOptions{SaltLength: 32})
	require.NoError(t, err)
	require.True(t, VerifyLegacyPSS(spki, msg, sig))
	require.False(t, VerifyLegacyPSS(spki, []byte("other"), sig))
	require.False(t, VerifyLegacyPSS(spki, msg, sig[:511]))
	salt20, err := rsa.SignPSS(rand.Reader, k, crypto.SHA256, h[:], &rsa.PSSOptions{SaltLength: 20})
	require.NoError(t, err)
	require.False(t, VerifyLegacyPSS(spki, msg, salt20), "salt length is exactly 32")

	// The SPKI is validated, not merely parsed. The unused-bit SPKI encodes
	// the signing key itself, so only ValidateEKSPKI stands between this valid
	// signature and acceptance.
	nonCanonical := ekBitStringUnusedBit(t, spki)
	parsed, err := x509.ParsePKIXPublicKey(nonCanonical)
	require.NoError(t, err)
	rk, ok := parsed.(*rsa.PublicKey)
	require.True(t, ok)
	require.NoError(t, rsa.VerifyPSS(rk, crypto.SHA256, h[:], sig, &rsa.PSSOptions{SaltLength: 32}),
		"control: the signature is valid for the key the SPKI encodes")
	require.False(t, VerifyLegacyPSS(nonCanonical, msg, sig), "non-canonical SPKI of the signing key")

	// A 2048-bit SPKI with its own key's valid signature. Its 256-byte
	// signature also fails the 512-byte length rule, so the unused-bit case
	// above is the one only SPKI validation refuses.
	small, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	spki2048, err := x509.MarshalPKIXPublicKey(&small.PublicKey)
	require.NoError(t, err)
	sig2048, err := rsa.SignPSS(rand.Reader, small, crypto.SHA256, h[:], &rsa.PSSOptions{SaltLength: 32})
	require.NoError(t, err)
	require.False(t, VerifyLegacyPSS(spki2048, msg, sig2048), "2048-bit SPKI")

	require.False(t, VerifyLegacyPSS(nil, msg, sig), "nil SPKI")
	require.False(t, VerifyLegacyPSS(spki, msg, nil), "nil signature")
}

func TestDigests(t *testing.T) {
	require.Len(t, Digest(nil), 48)
	h := sha512.Sum384([]byte("x"))
	require.Equal(t, h[:], EntryDigest([]byte("x")))
	require.Equal(t, h[:], KeyDigest([]byte("x")))
	require.Equal(t, h[:], CredentialEpochDigest("x", true))
	require.Nil(t, CredentialEpochDigest("x", false), "NULL epoch is encoded \"\"")
	require.Nil(t, CredentialEpochDigest("", true), "\"\" epoch is encoded \"\"")

	a, b := "0f8fad5b-d9cb-469f-a165-70867728950e", "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	da, db := digestOf("a"), digestOf("b")
	got, err := PopulationDigest([]PopulationRecord{{b, db}, {a, da}})
	require.NoError(t, err)
	want := Digest(slices.Concat([]byte(a), da, []byte(b), db))
	require.Equal(t, want, got, "84-byte records in ascending account_id order")
	_, err = PopulationDigest([]PopulationRecord{{a, da}, {a, db}})
	require.ErrorIs(t, err, ErrMalformed, "duplicate account")
	_, err = PopulationDigest([]PopulationRecord{{a, da[:47]}})
	require.ErrorIs(t, err, ErrMalformed, "short digest")
	// account_id is a canonical s36. Both of these are 36 bytes, so only the
	// UUID rule refuses them.
	for name, id := range map[string]string{
		"uppercase account_id": strings.ToUpper(a),
		"non-UUID account_id":  strings.Repeat("z", 36),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := PopulationDigest([]PopulationRecord{{id, da}})
			require.ErrorIs(t, err, ErrMalformed)
		})
	}
}

func TestTLSPin(t *testing.T) {
	p := testPoint(t)
	spki, err := P384SPKI(p)
	require.NoError(t, err)
	require.Len(t, spki, 120)
	// id-ecPublicKey, secp384r1, then a BIT STRING holding the point itself.
	prefix, err := hex.DecodeString("3076301006072a8648ce3d020106052b81040022036200")
	require.NoError(t, err)
	require.Equal(t, prefix, spki[:23])
	require.Equal(t, p, spki[23:])
	pin, err := TLSPin(p)
	require.NoError(t, err)
	require.Equal(t, Digest(spki), pin)
	_, err = TLSPin(hybridOf(p))
	require.ErrorIs(t, err, ErrMalformed)
	_, err = TLSPin(nil)
	require.ErrorIs(t, err, ErrMalformed)
}

func TestDeviceID(t *testing.T) {
	var dsk [97]byte
	copy(dsk[:], testPoint(t))
	id := DeviceID(dsk)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`), id)
	require.True(t, isUUIDv4(id))
	h := sha512.Sum384(slices.Concat([]byte("concord-device-id/v1"), []byte{0}, dsk[:]))
	require.True(t, bytes.HasPrefix([]byte(id), []byte(fmtHex(h[0:4]))), "first 4 bytes are SHA-384 bytes 0..3")
	require.Equal(t, id, DeviceID(dsk), "deterministic")

	// All 16 bytes, from the DoR's formula (§2.5): h[0:16] with only the
	// version nibble and the variant bits replaced.
	got, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	require.NoError(t, err)
	want := slices.Clone(h[:16])
	want[6] = (want[6] & 0x0f) | 0x40
	want[8] = (want[8] & 0x3f) | 0x80
	require.Equal(t, want, got)

	// Known answer: dsk = G. The expected ID was computed independently with
	// Python's hashlib and uuid modules, not with this package. G (FIPS 186-4
	// D.1.2.4) comes from the standard library rather than a hex literal.
	params := elliptic.P384().Params()
	g := slices.Concat([]byte{0x04}, params.Gx.FillBytes(make([]byte, 48)), params.Gy.FillBytes(make([]byte, 48)))
	_, err = ParsePoint(g)
	require.NoError(t, err)
	var gKey [97]byte
	copy(gKey[:], g)
	require.Equal(t, "989285f8-8de7-4727-a1e9-6ec5739ff684", DeviceID(gKey))
}

func fmtHex(b []byte) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, hexd[c>>4], hexd[c&15])
	}
	return string(out)
}
