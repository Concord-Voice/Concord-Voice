// Package signer holds this node's RSK and signs results, statuses,
// device-results and device-statuses with it (#3587 PR-B1, DoR §2.10 T10,
// §3.2.4 step 11). Key storage and keygen are PR-B2's.
package signer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"errors"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// ErrSelfVerify means a signature failed its own verification; the caller
// returns 503 and never releases it.
var ErrSelfVerify = errors.New("authority signer: self-verification failed")

// Signer signs with this node's RSK.
type Signer interface {
	PublicKey() []byte
	Sign(msg []byte) ([]byte, error)
}

// P384 is an in-memory ECDSA P-384 RSK.
type P384 struct {
	priv *ecdsa.PrivateKey
	pub  []byte
}

// NewP384 wraps a P-384 private key.
func NewP384(priv *ecdsa.PrivateKey) (*P384, error) {
	if priv == nil || priv.Curve != elliptic.P384() {
		return nil, errors.New("authority signer: the RSK must be ECDSA P-384")
	}
	pub, err := priv.PublicKey.ECDH()
	if err != nil {
		return nil, err
	}
	return &P384{priv: priv, pub: pub.Bytes()}, nil
}

// GenerateP384 creates a fresh RSK (tests, and B2's keygen).
func GenerateP384() (*P384, error) {
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return NewP384(k)
}

// PublicKey is the 97-byte uncompressed point.
func (s *P384) PublicKey() []byte { return append([]byte(nil), s.pub...) }

// Sign returns r‖s, 48 bytes each, over SHA-384(msg) (§2.4).
func (s *P384) Sign(msg []byte) ([]byte, error) {
	h := sha512.Sum384(msg)
	r, sv, err := ecdsa.Sign(rand.Reader, s.priv, h[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, 96)
	r.FillBytes(out[:48])
	sv.FillBytes(out[48:])
	return out, nil
}

// SignAndVerify signs msg and verifies the signature under the signer's own
// public key with the contract's verifier before returning it.
func SignAndVerify(s Signer, msg []byte) ([]byte, error) {
	sig, err := s.Sign(msg)
	if err != nil {
		return nil, err
	}
	if !authoritycontract.VerifyP384(s.PublicKey(), msg, sig) {
		return nil, ErrSelfVerify
	}
	return sig, nil
}
