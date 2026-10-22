// Package authoritytest builds a test realm and validly signed authority
// messages for the account authority's tests (#3587 PR-B1). Every key is
// generated at runtime (D-167). Import it from _test.go files only.
package authoritytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"database/sql"
	"sort"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/google/uuid"
)

// Clock is the fixture's now. Tests that pin the store's DBNow hook set both.
type Clock struct{ Ms uint64 }

// SignedDelegation is a delegation with its signed message, both anchor signatures and its signed bytes.
type SignedDelegation struct {
	D                       ac.Delegation
	Msg, SigA, SigB, Signed []byte
}

// Realm is a test realm: its anchors, RSK, CAK, operator and TLS keys, clock and current delegation.
type Realm struct {
	ID                                       string
	Clock                                    *Clock
	PrimaryA, PrimaryB, RecoveryA, RecoveryB *ecdsa.PrivateKey
	AnchorBytes                              []byte
	RSK                                      *signer.P384
	CAK, Operator                            *ecdsa.PrivateKey
	TLSKey, CPTLS                            *ecdsa.PrivateKey
	Current                                  SignedDelegation
}

// RealmOption adjusts the delegation NewRealm signs.
type RealmOption func(*ac.Delegation)

func genKey(t testing.TB) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k, Point(k)
}

// Point is the 97-byte uncompressed P-384 point of k.
func Point(k *ecdsa.PrivateKey) []byte {
	e, err := k.PublicKey.ECDH()
	if err != nil {
		panic(err)
	}
	return e.Bytes()
}

func p1363(t testing.TB, k *ecdsa.PrivateKey, msg []byte) []byte {
	t.Helper()
	s, err := signer.NewP384(k)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := s.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// SignSig signs Sig{role, digest}.Encode() with k (§2.5).
func SignSig(t testing.TB, k *ecdsa.PrivateKey, role string, digest []byte) []byte {
	t.Helper()
	return p1363(t, k, ac.Sig{Role: role, Digest: digest}.Encode())
}

func allScope() []string {
	s := []string{ac.OpCancelPending, ac.OpEnrollExisting, ac.OpEnrollNew, ac.OpErase, ac.OpRecover, ac.OpReplaceRoot,
		ac.OpRevokeRoot, ac.OpRotateEK, ac.OpRotateRoot, ac.OpSuspend, ac.OpUnsuspend, ac.OpAddDevice, ac.OpBindRecovery,
		ac.OpCancelRecoveryBind, ac.ScopeStatus}
	sort.Strings(s)
	return s
}

// WithoutOperatorKeys is the GA delegation (LD-37): every operator row refused.
func WithoutOperatorKeys() RealmOption {
	return func(d *ac.Delegation) { d.OperatorPublicKeys = [][]byte{} }
}

// WithScope replaces the delegation's scope.
func WithScope(ops ...string) RealmOption {
	return func(d *ac.Delegation) { s := append([]string(nil), ops...); sort.Strings(s); d.Scope = s }
}

// WithWindow sets the delegation window.
func WithWindow(notBefore, notAfter uint64) RealmOption {
	return func(d *ac.Delegation) { d.NotBeforeMs, d.NotAfterMs = notBefore, notAfter }
}

// WithRSKs replaces the delegation's RSK list.
func WithRSKs(points ...[]byte) RealmOption {
	return func(d *ac.Delegation) { d.RSKPublicKeys = points }
}

func (r *Realm) sign(t testing.TB, d ac.Delegation) SignedDelegation {
	t.Helper()
	msg := d.Encode()
	if msg == nil {
		t.Fatalf("authoritytest: the delegation does not encode: %+v", d)
	}
	sa, sb := p1363(t, r.PrimaryA, msg), p1363(t, r.PrimaryB, msg)
	signed := ac.Signed{Type: "delegation", Msg: msg, SigA: sa, SigB: sb}.Encode()
	if signed == nil {
		t.Fatal("authoritytest: the signed delegation does not encode")
	}
	return SignedDelegation{D: d, Msg: msg, SigA: sa, SigB: sb, Signed: signed}
}

// NewRealm makes anchors, an RSK, a CAK, an operator key and delegation serial 1
// of gen 0, valid from 1 h ago for 180 d, Δ = 15 min, G = 2 min, τ = 10 min.
func NewRealm(t testing.TB, opts ...RealmOption) *Realm {
	t.Helper()
	r := &Realm{ID: uuid.NewString(), Clock: &Clock{Ms: uint64(time.Now().UnixMilli())}}
	r.PrimaryA, _ = genKey(t)
	r.PrimaryB, _ = genKey(t)
	r.RecoveryA, _ = genKey(t)
	r.RecoveryB, _ = genKey(t)
	r.AnchorBytes = ac.Anchors{RealmID: r.ID, PrimaryA: Point(r.PrimaryA), PrimaryB: Point(r.PrimaryB),
		RecoveryA: Point(r.RecoveryA), RecoveryB: Point(r.RecoveryB)}.Encode()
	var err error
	if r.RSK, err = signer.GenerateP384(); err != nil {
		t.Fatal(err)
	}
	r.CAK, _ = genKey(t)
	r.Operator, _ = genKey(t)
	r.TLSKey, _ = genKey(t)
	r.CPTLS, _ = genKey(t)
	// The pin is SHA-384 of the SPKI DER, not of the point (DoR:387, B2-D23).
	cpPin, err := ac.TLSPin(Point(r.CPTLS))
	if err != nil {
		t.Fatal(err)
	}
	now := r.Clock.Ms
	d := ac.Delegation{RealmID: r.ID, Serial: 1, MinValidSerial: 1, ProductRootGen: 0,
		RSKPublicKeys: [][]byte{r.RSK.PublicKey()}, CAKPublicKeys: [][]byte{Point(r.CAK)},
		OperatorPublicKeys: [][]byte{Point(r.Operator)}, AuthorityTLSKeys: [][]byte{Point(r.TLSKey)},
		CPTLSPins: [][]byte{cpPin}, NotBeforeMs: now - 3_600_000,
		NotAfterMs: now - 3_600_000 + 180*86_400_000, Scope: allScope(), AcceptedVersions: []string{"1"},
		TauMs: 600_000, DeltaMs: 900_000, GMs: 120_000}
	for _, o := range opts {
		o(&d)
	}
	r.Current = r.sign(t, d)
	return r
}

// NextDelegation is serial+1 of the current gen with min_valid = serial (T11).
func (r *Realm) NextDelegation(t testing.TB, opts ...RealmOption) SignedDelegation {
	t.Helper()
	d := r.Current.D
	d.Serial++
	d.MinValidSerial = d.Serial
	for _, o := range opts {
		o(&d)
	}
	r.Current = r.sign(t, d)
	return r.Current
}

// Install inserts d into authority.delegations (installed_at is forced by the trigger).
func (r *Realm) Install(t testing.TB, db *sql.DB, d SignedDelegation) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO authority.delegations (product_root_gen, serial, bytes, sig_a, sig_b, digest)
		VALUES ($1, $2, $3, $4, $5, $6)`, d.D.ProductRootGen, d.D.Serial, d.Msg, d.SigA, d.SigB, ac.Digest(d.Msg)); err != nil {
		t.Fatalf("authoritytest: install delegation: %v", err)
	}
}

// Trust builds an A2 Trust over the anchors and ds.
func (r *Realm) Trust(t testing.TB, ds ...SignedDelegation) *ac.Trust {
	t.Helper()
	a, err := ac.DecodeAnchors(r.AnchorBytes)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := ac.NewTrust(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if err := tr.AddSigned(d.Signed); err != nil {
			t.Fatalf("authoritytest: AddSigned: %v", err)
		}
	}
	return tr
}

// SignRaw signs msg with k (P1363 over SHA-384), for tests that rebuild an envelope.
func SignRaw(t testing.TB, k *ecdsa.PrivateKey, msg []byte) []byte { return p1363(t, k, msg) }
