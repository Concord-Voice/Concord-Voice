package signer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"math/big"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func TestP384SignsP1363ThatTheContractVerifies(t *testing.T) {
	s, err := GenerateP384()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.PublicKey()) != 97 || s.PublicKey()[0] != 0x04 {
		t.Fatalf("public key is %d bytes, prefix %#x", len(s.PublicKey()), s.PublicKey()[0])
	}
	if _, err := authoritycontract.ParsePoint(s.PublicKey()); err != nil {
		t.Fatalf("ParsePoint: %v", err)
	}
	msg := []byte(`["concord-account-authority",1,"result"]`)
	sig, err := SignAndVerify(s, msg)
	if err != nil || len(sig) != 96 {
		t.Fatalf("SignAndVerify = %d bytes, %v", len(sig), err)
	}
	if !authoritycontract.VerifyP384(s.PublicKey(), msg, sig) {
		t.Fatal("the contract does not verify the signer's output")
	}
	if authoritycontract.VerifyP384(s.PublicKey(), append(msg, 'x'), sig) {
		t.Fatal("a signature verified over a different message")
	}
}

func TestNewP384RefusesOtherCurves(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := NewP384(k); err == nil {
		t.Fatal("a P-256 key was accepted as an RSK")
	}
}

func TestNewP384RefusesAPointOffTheCurve(t *testing.T) {
	// Every non-deprecated constructor refuses an off-curve point, so only the
	// raw coordinates can build the input this test needs.
	k := &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: elliptic.P384(), X: big.NewInt(1), Y: big.NewInt(1)}} //nolint:staticcheck // SA1019 is the point: an off-curve key
	if _, err := NewP384(k); err == nil {
		t.Fatal("a P-384 key whose public point is off the curve was accepted as an RSK")
	}
}

type failingSigner struct{ *P384 }

func (failingSigner) Sign([]byte) ([]byte, error) { return nil, errors.New("hsm unavailable") }

func TestSignAndVerifyReturnsTheSignError(t *testing.T) {
	s, _ := GenerateP384()
	if sig, err := SignAndVerify(failingSigner{s}, []byte("m")); err == nil || errors.Is(err, ErrSelfVerify) || sig != nil {
		t.Fatalf("SignAndVerify(failing) = %x, %v; want the sign error", sig, err)
	}
}

type lyingSigner struct{ *P384 }

func (l lyingSigner) Sign(msg []byte) ([]byte, error) {
	sig, err := l.P384.Sign(msg)
	if err == nil {
		sig[95] ^= 1
	}
	return sig, err
}

func TestSignAndVerifyRefusesASignatureThatDoesNotVerify(t *testing.T) {
	s, _ := GenerateP384()
	if _, err := SignAndVerify(lyingSigner{s}, []byte("m")); !errors.Is(err, ErrSelfVerify) {
		t.Fatalf("SignAndVerify(corrupted) = %v, want ErrSelfVerify", err)
	}
}
