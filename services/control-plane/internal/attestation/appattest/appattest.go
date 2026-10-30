// Package appattest verifies Apple App Attest attestations and assertions for
// the iOS client (ADR-0010 D17). It is pure: no I/O, no storage. Callers own
// challenge issuance, key persistence and the sign counter.
//
// Validation follows Apple's "Validating apps that connect to your server":
// https://developer.apple.com/documentation/devicecheck/validating-apps-that-connect-to-your-server
package appattest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	_ "embed" // Apple's root CA is pinned in the binary.
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/fxamacker/cbor/v2"
)

//go:embed apple_app_attestation_root_ca.pem
var appleRootPEM []byte

// Environment is the App Attest environment a key was certified in.
type Environment string

// App Attest environments, taken from the authenticator data AAGUID.
const (
	Production  Environment = "production"
	Development Environment = "development"
)

var (
	aaguidProduction  = append([]byte("appattest"), make([]byte, 7)...)
	aaguidDevelopment = []byte("appattestdevelop")
	// nonceOID is Apple's credential certificate extension carrying the nonce.
	nonceOID = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 8, 2}
)

// Rejection reasons. Every failure wraps ErrRejected so callers can map any of
// them to one client-facing code without leaking which check failed.
var (
	// ErrRejected is wrapped by every rejection below.
	ErrRejected       = errors.New("appattest: rejected")
	ErrFormat         = fmt.Errorf("%w: malformed object", ErrRejected)
	ErrChain          = fmt.Errorf("%w: certificate chain", ErrRejected)
	ErrNonce          = fmt.Errorf("%w: nonce", ErrRejected)
	ErrKeyID          = fmt.Errorf("%w: key id", ErrRejected)
	ErrAppID          = fmt.Errorf("%w: app id", ErrRejected)
	ErrEnvironment    = fmt.Errorf("%w: environment", ErrRejected)
	ErrCounter        = fmt.Errorf("%w: counter", ErrRejected)
	ErrSignature      = fmt.Errorf("%w: signature", ErrRejected)
	ErrClientDataHash = fmt.Errorf("%w: client data hash must be 32 bytes", ErrRejected)
)

// Verifier checks App Attest objects against a pinned root and an allowlist
// of app ids (TEAMID.bundle.id).
type Verifier struct {
	roots      *x509.CertPool
	rpIDHashes [][32]byte
	allowDev   bool
	now        func() time.Time
}

// New returns a Verifier pinned to Apple's App Attestation root CA.
func New(appIDs []string, allowDevelopment bool) (*Verifier, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(appleRootPEM) {
		return nil, errors.New("appattest: embedded Apple root CA did not parse")
	}
	return NewWithRoots(roots, appIDs, allowDevelopment, time.Now)
}

// NewWithRoots is New with an injectable root pool and clock, for tests.
func NewWithRoots(roots *x509.CertPool, appIDs []string, allowDevelopment bool, now func() time.Time) (*Verifier, error) {
	if len(appIDs) == 0 {
		return nil, errors.New("appattest: at least one app id is required")
	}
	v := &Verifier{roots: roots, allowDev: allowDevelopment, now: now}
	for _, id := range appIDs {
		v.rpIDHashes = append(v.rpIDHashes, sha256.Sum256([]byte(id)))
	}
	return v, nil
}

// AllowsDevelopment reports whether development-environment keys are accepted.
func (v *Verifier) AllowsDevelopment() bool { return v.allowDev }

// AttestedKey is what a successful attestation yields for storage.
type AttestedKey struct {
	// PublicKey is the uncompressed P-256 point (65 bytes).
	PublicKey   []byte
	Environment Environment
}

// maxX5C bounds the chain: Apple sends leaf + one intermediate (RT-F3).
const maxX5C = 4

type attestationObject struct {
	Fmt     string `cbor:"fmt"`
	AttStmt struct {
		X5C [][]byte `cbor:"x5c"`
	} `cbor:"attStmt"`
	AuthData []byte `cbor:"authData"`
}

type authData struct {
	rpIDHash [32]byte
	counter  uint32
	aaguid   []byte
	credID   []byte
}

// parseAuthData reads the WebAuthn authenticator data layout. When attested is
// true the attested credential data (AAGUID, credential id) must be present.
func parseAuthData(b []byte, attested bool) (authData, error) {
	var a authData
	if len(b) < 37 {
		return a, ErrFormat
	}
	copy(a.rpIDHash[:], b[:32])
	a.counter = binary.BigEndian.Uint32(b[33:37])
	if !attested {
		return a, nil
	}
	if len(b) < 55 {
		return a, ErrFormat
	}
	a.aaguid = b[37:53]
	n := int(binary.BigEndian.Uint16(b[53:55]))
	if len(b) < 55+n {
		return a, ErrFormat
	}
	a.credID = b[55 : 55+n]
	return a, nil
}

func (v *Verifier) appIDAllowed(h [32]byte) bool {
	ok := 0
	for _, want := range v.rpIDHashes {
		ok |= subtle.ConstantTimeCompare(h[:], want[:])
	}
	return ok == 1
}

// VerifyAttestation checks a one-time key attestation. keyID is the raw
// (decoded) key identifier and clientDataHash is SHA-256 of the server
// challenge, exactly as the app passed it to attestKey.
func (v *Verifier) VerifyAttestation(keyID, attestation, clientDataHash []byte) (AttestedKey, error) {
	if len(clientDataHash) != sha256.Size {
		return AttestedKey{}, ErrClientDataHash
	}
	var obj attestationObject
	if err := cbor.Unmarshal(attestation, &obj); err != nil || obj.Fmt != "apple-appattest" || len(obj.AttStmt.X5C) < 2 || len(obj.AttStmt.X5C) > maxX5C {
		return AttestedKey{}, ErrFormat
	}

	// 1. The credential certificate chains to the pinned Apple root.
	leaf, err := x509.ParseCertificate(obj.AttStmt.X5C[0])
	if err != nil {
		return AttestedKey{}, ErrFormat
	}
	inter := x509.NewCertPool()
	for _, der := range obj.AttStmt.X5C[1:] {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return AttestedKey{}, ErrFormat
		}
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         v.roots,
		Intermediates: inter,
		CurrentTime:   v.now(),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return AttestedKey{}, ErrChain
	}

	// 2-4. nonce = SHA-256(authData || clientDataHash) is the leaf's extension.
	nonce := sha256.Sum256(append(append([]byte{}, obj.AuthData...), clientDataHash...))
	certNonce, err := leafNonce(leaf)
	if err != nil {
		return AttestedKey{}, err
	}
	if subtle.ConstantTimeCompare(nonce[:], certNonce) != 1 {
		return AttestedKey{}, ErrNonce
	}

	// 5. SHA-256 of the leaf public key is the key id.
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return AttestedKey{}, ErrFormat
	}
	point, err := pub.Bytes()
	if err != nil {
		return AttestedKey{}, ErrFormat
	}
	pubHash := sha256.Sum256(point)
	if subtle.ConstantTimeCompare(pubHash[:], keyID) != 1 {
		return AttestedKey{}, ErrKeyID
	}

	// 6-9. App id, zero counter, environment, credential id.
	ad, err := parseAuthData(obj.AuthData, true)
	if err != nil {
		return AttestedKey{}, err
	}
	if !v.appIDAllowed(ad.rpIDHash) {
		return AttestedKey{}, ErrAppID
	}
	if ad.counter != 0 {
		return AttestedKey{}, ErrCounter
	}
	env, err := v.environment(ad.aaguid)
	if err != nil {
		return AttestedKey{}, err
	}
	if subtle.ConstantTimeCompare(ad.credID, keyID) != 1 {
		return AttestedKey{}, ErrKeyID
	}
	return AttestedKey{PublicKey: point, Environment: env}, nil
}

func (v *Verifier) environment(aaguid []byte) (Environment, error) {
	switch {
	case bytes.Equal(aaguid, aaguidProduction):
		return Production, nil
	case bytes.Equal(aaguid, aaguidDevelopment) && v.allowDev:
		return Development, nil
	}
	return "", ErrEnvironment
}

// leafNonce extracts the nonce from Apple's credential certificate extension:
// SEQUENCE { [1] EXPLICIT OCTET STRING }.
func leafNonce(leaf *x509.Certificate) ([]byte, error) {
	for _, ext := range leaf.Extensions {
		if !ext.Id.Equal(nonceOID) {
			continue
		}
		var seq struct {
			Nonce []byte `asn1:"tag:1,explicit"`
		}
		if rest, err := asn1.Unmarshal(ext.Value, &seq); err != nil || len(rest) != 0 || len(seq.Nonce) != sha256.Size {
			return nil, ErrNonce
		}
		return seq.Nonce, nil
	}
	return nil, ErrNonce
}

type assertionObject struct {
	Signature         []byte `cbor:"signature"`
	AuthenticatorData []byte `cbor:"authenticatorData"`
}

// VerifyAssertion checks an assertion made with a previously attested key and
// returns the new sign counter, which the caller must persist. publicKey is
// the stored uncompressed P-256 point; prevCounter is the stored counter.
func (v *Verifier) VerifyAssertion(assertion, clientDataHash, publicKey []byte, prevCounter uint32) (uint32, error) {
	if len(clientDataHash) != sha256.Size {
		return 0, ErrClientDataHash
	}
	var obj assertionObject
	if err := cbor.Unmarshal(assertion, &obj); err != nil || len(obj.Signature) == 0 {
		return 0, ErrFormat
	}
	pub, err := parsePoint(publicKey)
	if err != nil {
		return 0, err
	}
	nonce := sha256.Sum256(append(append([]byte{}, obj.AuthenticatorData...), clientDataHash...))
	// The device signs the nonce with SHA256withECDSA, which hashes its input
	// once more; VerifyASN1 takes the final digest.
	digest := sha256.Sum256(nonce[:])
	if !ecdsa.VerifyASN1(pub, digest[:], obj.Signature) {
		return 0, ErrSignature
	}
	ad, err := parseAuthData(obj.AuthenticatorData, false)
	if err != nil {
		return 0, err
	}
	if !v.appIDAllowed(ad.rpIDHash) {
		return 0, ErrAppID
	}
	if ad.counter <= prevCounter {
		return 0, ErrCounter
	}
	return ad.counter, nil
}

func parsePoint(point []byte) (*ecdsa.PublicKey, error) {
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return nil, ErrFormat
	}
	return pub, nil
}
