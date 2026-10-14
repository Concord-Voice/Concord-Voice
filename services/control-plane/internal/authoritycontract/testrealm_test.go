package authoritycontract

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"errors"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

const testT0 uint64 = 1_760_000_000_000

type testGen struct{ A, B, RA, RB *ecdsa.PrivateKey }

// testRealm signs trust objects and statuses with runtime-generated keys
// (DoR §3.1: nothing private is committed).
type testRealm struct {
	t            testing.TB
	ID           string
	Gens         []testGen
	Anchors      Anchors
	RSK, CAK, Op *ecdsa.PrivateKey
	Items        [][]byte
}

func newTestRealm(t testing.TB) *testRealm {
	g := testGen{testKey(t), testKey(t), testKey(t), testKey(t)}
	r := &testRealm{t: t, ID: newID(), Gens: []testGen{g}, RSK: testKey(t), CAK: testKey(t), Op: testKey(t)}
	r.Anchors = Anchors{RealmID: r.ID, PrimaryA: pointOf(t, g.A), PrimaryB: pointOf(t, g.B),
		RecoveryA: pointOf(t, g.RA), RecoveryB: pointOf(t, g.RB)}
	return r
}

// del is a valid delegation of the top generation; mut edits it before signing.
func (r *testRealm) del(serial uint64, mut func(*Delegation)) Delegation {
	d := Delegation{RealmID: r.ID, Serial: serial, MinValidSerial: serial, ProductRootGen: uint64(len(r.Gens)) - 1,
		RSKPublicKeys: [][]byte{pointOf(r.t, r.RSK)}, CAKPublicKeys: [][]byte{pointOf(r.t, r.CAK)},
		OperatorPublicKeys: [][]byte{}, AuthorityTLSKeys: [][]byte{pointOf(r.t, testKey(r.t))},
		CPTLSPins: [][]byte{digestOf("cp-pin")}, NotBeforeMs: testT0 - 86_400_000, NotAfterMs: testT0 + 180*86_400_000,
		Scope: append([]string(nil), scopeValues...), AcceptedVersions: []string{"1"}, TauMs: 600_000,
		DeltaMs: 259_200_000, GMs: 900_000}
	if mut != nil {
		mut(&d)
	}
	return d
}

func (r *testRealm) signDel(d Delegation, a, b *ecdsa.PrivateKey) []byte {
	msg := d.Encode()
	require.NotNil(r.t, msg)
	return Signed{Type: "delegation", Msg: msg, SigA: signP1363(r.t, a, msg), SigB: signP1363(r.t, b, msg)}.Encode()
}

// addDel signs del(serial, mut) with the top generation's primaries and records it.
func (r *testRealm) addDel(serial uint64, mut func(*Delegation)) []byte {
	g := r.Gens[len(r.Gens)-1]
	b := r.signDel(r.del(serial, mut), g.A, g.B)
	r.Items = append(r.Items, b)
	return b
}

// succeed signs a succession from the top generation and records it.
func (r *testRealm) succeed(mut func(*Succession)) []byte {
	prev := r.Gens[len(r.Gens)-1]
	next := testGen{testKey(r.t), testKey(r.t), testKey(r.t), testKey(r.t)}
	s := Succession{RealmID: r.ID, PrevRootGen: uint64(len(r.Gens)) - 1, NewRootGen: uint64(len(r.Gens)),
		NewPrimaryA: pointOf(r.t, next.A), NewPrimaryB: pointOf(r.t, next.B),
		NewRecoveryA: pointOf(r.t, next.RA), NewRecoveryB: pointOf(r.t, next.RB)}
	if mut != nil {
		mut(&s)
	}
	msg := s.Encode()
	require.NotNil(r.t, msg)
	b := Signed{Type: "succession", Msg: msg, SigA: signP1363(r.t, prev.RA, msg), SigB: signP1363(r.t, prev.RB, msg)}.Encode()
	r.Gens = append(r.Gens, next)
	r.Items = append(r.Items, b)
	return b
}

func (r *testRealm) trust(t testing.TB) *Trust {
	tr, err := NewTrust(r.Anchors)
	require.NoError(t, err)
	for _, it := range r.Items {
		if err := tr.AddSigned(it); err != nil && !errors.Is(err, ErrIgnored) {
			require.NoError(t, err)
		}
	}
	return tr
}

// status signs a minimal absent seq-0 status under (gen 0, serial 1).
func (r *testRealm) status(t testing.TB, mut func(*Status)) []byte {
	s := Status{RealmID: r.ID, AccountID: newID(), State: StateAbsent, SuspendedBy: []string{},
		AsOfMs: testT0, ValidUntilMs: testT0 + 259_200_000, DelegationSerial: 1}
	if mut != nil {
		mut(&s)
	}
	msg := s.Encode()
	require.NotNil(t, msg)
	return Signed{Type: "status", Msg: msg, SigA: signP1363(t, r.RSK, msg)}.Encode()
}

// highS returns the other valid P1363 signature (r, n − s): EN7 allows high-s (D-222).
func highS(t testing.TB, sig []byte) []byte {
	require.Len(t, sig, 96)
	n := elliptic.P384().Params().N
	s := new(big.Int).SetBytes(sig[48:])
	out := append([]byte(nil), sig[:48]...)
	return append(out, new(big.Int).Sub(n, s).FillBytes(make([]byte, 48))...)
}
