package appattest_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation/appattest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/appattesttest"
	"github.com/fxamacker/cbor/v2"
)

const appID = appattesttest.AppID

func verifier(t *testing.T, f *appattesttest.Fixture, allowDev bool) *appattest.Verifier {
	t.Helper()
	v, err := appattest.NewWithRoots(f.Roots, []string{"OTHERTEAM1.com.example.other", appID}, allowDev, func() time.Time { return appattesttest.Now })
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func challengeHash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func TestEmbeddedAppleRootLoads(t *testing.T) {
	if _, err := appattest.New([]string{appID}, false); err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := appattest.New(nil, false); err == nil {
		t.Fatal("New with no app ids must fail")
	}
}

func TestVerifyAttestationAcceptsProductionKey(t *testing.T) {
	f := appattesttest.New(t)
	cdh := challengeHash("c1")
	key, err := verifier(t, f, false).VerifyAttestation(f.KeyID, f.Attestation(t, cdh, nil), cdh)
	if err != nil {
		t.Fatalf("VerifyAttestation: %v", err)
	}
	if string(key.PublicKey) != string(f.PublicKey(t)) || key.Environment != appattest.Production {
		t.Fatalf("got %+v", key)
	}
}

func TestVerifyAttestationDevelopmentNeedsFlag(t *testing.T) {
	f := appattesttest.New(t)
	cdh := challengeHash("c1")
	att := f.Attestation(t, cdh, func(o *appattesttest.Options) { o.AAGUID = appattesttest.AAGUIDDevelopment })
	if _, err := verifier(t, f, false).VerifyAttestation(f.KeyID, att, cdh); !errors.Is(err, appattest.ErrEnvironment) {
		t.Fatalf("want environment rejection, got %v", err)
	}
	key, err := verifier(t, f, true).VerifyAttestation(f.KeyID, att, cdh)
	if err != nil || key.Environment != appattest.Development {
		t.Fatalf("got %+v, %v", key, err)
	}
}

func TestVerifyAttestationRejections(t *testing.T) {
	f := appattesttest.New(t)
	other := appattesttest.New(t)
	cdh := challengeHash("c1")
	cases := []struct {
		name  string
		keyID []byte
		att   []byte
		cdh   []byte
		want  error
	}{
		{"untrusted root", other.KeyID, other.Attestation(t, cdh, nil), cdh, appattest.ErrChain},
		{"different challenge", f.KeyID, f.Attestation(t, cdh, nil), challengeHash("c2"), appattest.ErrNonce},
		{"key id mismatch", other.KeyID, f.Attestation(t, cdh, nil), cdh, appattest.ErrKeyID},
		{"credential id mismatch", f.KeyID, f.Attestation(t, cdh, func(o *appattesttest.Options) { o.CredID = other.KeyID }), cdh, appattest.ErrKeyID},
		{"app id not allowed", f.KeyID, f.Attestation(t, cdh, func(o *appattesttest.Options) { o.RPID = "EVILTEAM12.com.example.app" }), cdh, appattest.ErrAppID},
		{"nonzero counter", f.KeyID, f.Attestation(t, cdh, func(o *appattesttest.Options) { o.Counter = 1 }), cdh, appattest.ErrCounter},
		{"unknown aaguid", f.KeyID, f.Attestation(t, cdh, func(o *appattesttest.Options) { o.AAGUID = make([]byte, 16) }), cdh, appattest.ErrEnvironment},
		{"wrong format", f.KeyID, f.Attestation(t, cdh, func(o *appattesttest.Options) { o.Fmt = "packed" }), cdh, appattest.ErrFormat},
		{"not cbor", f.KeyID, []byte("nope"), cdh, appattest.ErrFormat},
		{"short client hash", f.KeyID, f.Attestation(t, cdh, nil), cdh[:16], appattest.ErrClientDataHash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifier(t, f, true).VerifyAttestation(tc.keyID, tc.att, tc.cdh)
			if !errors.Is(err, tc.want) || !errors.Is(err, appattest.ErrRejected) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestVerifyAttestationRejectsExpiredChain(t *testing.T) {
	f := appattesttest.New(t)
	cdh := challengeHash("c1")
	att := f.Attestation(t, cdh, nil)
	v, err := appattest.NewWithRoots(f.Roots, []string{appID}, false, func() time.Time { return appattesttest.Now.Add(100 * 24 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.VerifyAttestation(f.KeyID, att, cdh); !errors.Is(err, appattest.ErrChain) {
		t.Fatalf("want chain rejection, got %v", err)
	}
}

func TestVerifyAssertion(t *testing.T) {
	f := appattesttest.New(t)
	v := verifier(t, f, false)
	point := f.PublicKey(t)
	cdh := challengeHash("a1")

	got, err := v.VerifyAssertion(f.Assertion(t, cdh, appID, 5, f.CredKey), cdh, point, 4)
	if err != nil || got != 5 {
		t.Fatalf("got %d, %v", got, err)
	}

	cases := []struct {
		name  string
		asrt  []byte
		cdh   []byte
		point []byte
		prev  uint32
		want  error
	}{
		{"replayed counter", f.Assertion(t, cdh, appID, 5, f.CredKey), cdh, point, 5, appattest.ErrCounter},
		{"different challenge", f.Assertion(t, cdh, appID, 6, f.CredKey), challengeHash("a2"), point, 5, appattest.ErrSignature},
		{"other key", f.Assertion(t, cdh, appID, 6, appattesttest.MustKey(t, elliptic.P256())), cdh, point, 5, appattest.ErrSignature},
		{"app id not allowed", f.Assertion(t, cdh, "EVILTEAM12.com.example.app", 6, f.CredKey), cdh, point, 5, appattest.ErrAppID},
		{"bad stored key", f.Assertion(t, cdh, appID, 6, f.CredKey), cdh, []byte{4, 1, 2}, 5, appattest.ErrFormat},
		{"not cbor", []byte("nope"), cdh, point, 5, appattest.ErrFormat},
		{"short client hash", f.Assertion(t, cdh, appID, 6, f.CredKey), cdh[:8], point, 5, appattest.ErrClientDataHash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.VerifyAssertion(tc.asrt, tc.cdh, tc.point, tc.prev); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// The device signs with SHA256withECDSA, so the signed digest is SHA-256 of the
// nonce. A signature over the bare nonce must not verify, or the verifier and
// the fixture could agree on the wrong digest and reject real devices.
func TestVerifyAssertion_RejectsSignatureOverBareNonce(t *testing.T) {
	f := appattesttest.New(t)
	v := verifier(t, f, false)
	cdh := challengeHash("a1")
	ad := appattesttest.AuthData(appID, 1, nil, nil)
	nonce := sha256.Sum256(append(append([]byte{}, ad...), cdh...))
	sig, err := ecdsa.SignASN1(rand.Reader, f.CredKey, nonce[:])
	if err != nil {
		t.Fatal(err)
	}
	asrt, err := cbor.Marshal(map[string]any{"signature": sig, "authenticatorData": ad})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.VerifyAssertion(asrt, cdh, f.PublicKey(t), 0); !errors.Is(err, appattest.ErrSignature) {
		t.Fatalf("want ErrSignature, got %v", err)
	}
}
