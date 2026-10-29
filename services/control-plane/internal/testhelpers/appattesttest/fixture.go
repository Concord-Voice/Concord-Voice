// Package appattesttest builds synthetic App Attest objects for tests: a
// three-level certificate chain shaped like Apple's, attestation objects and
// assertions. Never import it from production code.
package appattesttest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"math/big"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// AppID is the default TEAMID.bundle.id the fixture signs for.
const AppID = "TEAMID1234.com.example.app"

// Now is the instant the fixture's certificates are valid at.
var Now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// AAGUIDs Apple puts in the authenticator data.
var (
	AAGUIDProduction  = append([]byte("appattest"), make([]byte, 7)...)
	AAGUIDDevelopment = []byte("appattestdevelop")
)

var nonceOID = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 8, 2}

// Fixture is one synthetic device key under its own synthetic root.
type Fixture struct {
	Roots    *x509.CertPool
	KeyID    []byte
	CredKey  *ecdsa.PrivateKey
	interDER []byte
	interKey *ecdsa.PrivateKey
	inter    *x509.Certificate
}

// MustKey generates an EC key or fails the test.
func MustKey(t testing.TB, c elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustCert(t testing.TB, tmpl, parent *x509.Certificate, pub any, signer *ecdsa.PrivateKey) ([]byte, *x509.Certificate) {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return der, c
}

func caTemplate(cn string, serial int64) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             Now.Add(-24 * time.Hour),
		NotAfter:              Now.Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
}

// New builds a root, an intermediate and a P-256 device key.
func New(t testing.TB) *Fixture {
	t.Helper()
	rootKey := MustKey(t, elliptic.P384())
	rootTmpl := caTemplate("Test App Attestation Root CA", 1)
	_, root := mustCert(t, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	interKey := MustKey(t, elliptic.P384())
	interDER, inter := mustCert(t, caTemplate("Test App Attestation CA 1", 2), root, &interKey.PublicKey, rootKey)
	roots := x509.NewCertPool()
	roots.AddCert(root)
	f := &Fixture{Roots: roots, CredKey: MustKey(t, elliptic.P256()), interDER: interDER, interKey: interKey, inter: inter}
	id := sha256.Sum256(f.PublicKey(t))
	f.KeyID = id[:]
	return f
}

// PublicKey is the device key's uncompressed point.
func (f *Fixture) PublicKey(t testing.TB) []byte {
	t.Helper()
	b, err := f.CredKey.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Options shapes one attestation object; Attestation fills sound defaults.
type Options struct {
	RPID     string
	Counter  uint32
	AAGUID   []byte
	CredID   []byte
	NonceFor []byte // client data hash the leaf nonce commits to
	CredKey  *ecdsa.PrivateKey
	Fmt      string
}

// AuthData lays out WebAuthn authenticator data. A nil aaguid omits the
// attested credential data, as in an assertion.
func AuthData(rpID string, counter uint32, aaguid, credID []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	b := append([]byte{}, h[:]...)
	b = append(b, 0x40)
	b = binary.BigEndian.AppendUint32(b, counter)
	if aaguid == nil {
		return b
	}
	b = append(b, aaguid...)
	b = binary.BigEndian.AppendUint16(b, uint16(len(credID))) //nolint:gosec // test ids are 32 bytes
	b = append(b, credID...)
	return append(b, 0xa0) // stand-in for the COSE key, which verifiers do not read
}

// Attestation returns a CBOR attestation object over clientDataHash. mod may
// change any default to produce a defective object.
func (f *Fixture) Attestation(t testing.TB, clientDataHash []byte, mod func(*Options)) []byte {
	t.Helper()
	o := Options{RPID: AppID, AAGUID: AAGUIDProduction, CredID: f.KeyID, NonceFor: clientDataHash, CredKey: f.CredKey, Fmt: "apple-appattest"}
	if mod != nil {
		mod(&o)
	}
	ad := AuthData(o.RPID, o.Counter, o.AAGUID, o.CredID)
	nonce := sha256.Sum256(append(append([]byte{}, ad...), o.NonceFor...))
	ext, err := asn1.Marshal(struct {
		Nonce []byte `asn1:"tag:1,explicit"`
	}{nonce[:]})
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber:    big.NewInt(3),
		Subject:         pkix.Name{CommonName: "credential"},
		NotBefore:       Now.Add(-time.Hour),
		NotAfter:        Now.Add(72 * time.Hour),
		ExtraExtensions: []pkix.Extension{{Id: nonceOID, Value: ext}},
	}
	leafDER, _ := mustCert(t, leafTmpl, f.inter, &o.CredKey.PublicKey, f.interKey)
	b, err := cbor.Marshal(map[string]any{
		"fmt":      o.Fmt,
		"attStmt":  map[string]any{"x5c": [][]byte{leafDER, f.interDER}, "receipt": []byte("r")},
		"authData": ad,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Assertion returns a CBOR assertion over clientDataHash signed by key.
func (f *Fixture) Assertion(t testing.TB, clientDataHash []byte, rpID string, counter uint32, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	ad := AuthData(rpID, counter, nil, nil)
	nonce := sha256.Sum256(append(append([]byte{}, ad...), clientDataHash...))
	digest := sha256.Sum256(nonce[:]) // SHA256withECDSA over the nonce, as the device does
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	b, err := cbor.Marshal(map[string]any{"signature": sig, "authenticatorData": ad})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
