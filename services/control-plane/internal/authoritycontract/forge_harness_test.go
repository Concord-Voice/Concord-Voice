package authoritycontract

// A compromised-authority harness for the fix-retention, first-sight-pin and
// realm regressions (#3587 PR-A2 red-team: RT-1, RT-1b, RT-2; review B-SF1).
// It holds a realm's RSK and CAK (newTestRealm), so it can sign any result,
// status and cp-authz the threat model gives an RSK, or an RSK plus a CP
// (DoR §7, :2106–2107). The regressions built on it assert what the design of
// record requires, each beside a control that passes on any verifier.

import (
	"crypto/ecdsa"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// forgedEK is a fresh EK SPKI that §2.4's three steps accept (fakeek_test.go's
// template: a valid header and trailer around a random odd 4096-bit modulus).
func forgedEK(t *testing.T) []byte {
	t.Helper()
	spki := testEKSPKI(t)
	m := rnd(512)
	m[0] |= 0x80
	m[511] |= 1
	ek := slices.Concat(spki[:33], m, spki[545:])
	_, err := ValidateEKSPKI(ek)
	require.NoError(t, err)
	return ek
}

// forgeChain is one account of a realm whose RSK and CAK the harness holds.
// realm is the realm every message it signs names: the trust's own unless a
// test names another (B-SF1).
type forgeChain struct {
	t       *testing.T
	r       *testRealm
	tr      *Trust
	realm   string
	account string
}

func newForgeChain(t *testing.T) *forgeChain {
	t.Helper()
	r := newTestRealm(t)
	r.addDel(1, nil)
	return &forgeChain{t: t, r: r, tr: r.trust(t), realm: r.ID, account: newID()}
}

func (c *forgeChain) sign(k *ecdsa.PrivateKey, msg []byte) []byte { return signP1363(c.t, k, msg) }

// ekBinding returns an ek-binding signed by root, and its digest and signature.
func (c *forgeChain) ekBinding(root *ecdsa.PrivateKey, gen uint64, ek []byte) (b, dig, sig []byte) {
	m := EKBinding{RealmID: c.realm, AccountID: c.account, RootDigest: Digest(pointOf(c.t, root)),
		EKGeneration: gen, EKAlg: "rsa-oaep-4096-sha256", EKPublicKey: ek}
	b = m.Encode()
	require.NotNil(c.t, b)
	return b, Digest(b), c.sign(root, b)
}

// forgeSigs is what finish signs once the intent digest exists.
type forgeSigs struct {
	candidate, current *ecdsa.PrivateKey
	ekb, ekbSig        []byte
	held               []byte // row 22's held_status; also leaves cp_authz unsigned
}

// finish fills the realm, account and digests, signs the intent's positions,
// the cp-authz (CAK, except on actor restore) and the result (RSK), and
// returns the bundle and its result's entry digest.
func (c *forgeChain) finish(in Intent, a CPAuthz, res Result, s forgeSigs) (bundle, entry []byte) {
	in.RealmID, in.AccountID = c.realm, c.account
	a.RealmID, a.AccountID, a.RequestID = c.realm, c.account, in.RequestID
	res.RealmID, res.AccountID, res.RequestID, res.Op = c.realm, c.account, in.RequestID, in.Op
	if res.SuspendedBy == nil {
		res.SuspendedBy = []string{}
	}
	res.DelegationGen, res.DelegationSerial = 0, 1
	inB := in.Encode()
	require.NotNil(c.t, inB, "intent")
	a.IntentDigest = Digest(inB)
	if s.held != nil {
		a.EvidenceDigest = Digest(s.held)
	}
	aB := a.Encode()
	require.NotNil(c.t, aB, "cp-authz")
	res.IntentDigest, res.CPAuthzDigest = Digest(inB), Digest(aB)
	rB := res.Encode()
	require.NotNil(c.t, rB, "result")
	b := Bundle{Intent: inB, CPAuthz: aB, Result: rB, ResultSig: c.sign(c.r.RSK, rB),
		EKBinding: s.ekb, EKBindingSig: s.ekbSig, HeldStatus: s.held}
	if s.candidate != nil {
		b.SigCandidate = c.sign(s.candidate, sigMsg(RoleCandidate, Digest(inB)))
	}
	if s.current != nil {
		b.SigCurrentRoot = c.sign(s.current, sigMsg(RoleCurrentRoot, Digest(inB)))
	}
	if s.held == nil {
		b.CPAuthzSig = c.sign(c.r.CAK, aB)
	}
	out := b.Encode()
	require.NotNil(c.t, out, "bundle")
	return out, EntryDigest(rB)
}

// status signs st under (0, 1) with the realm's RSK.
func (c *forgeChain) status(st Status) []byte {
	st.RealmID, st.AccountID = c.realm, c.account
	if st.SuspendedBy == nil {
		st.SuspendedBy = []string{}
	}
	st.DelegationGen, st.DelegationSerial = 0, 1
	if st.ValidUntilMs == 0 {
		st.ValidUntilMs = st.AsOfMs + 3_600_000
	}
	msg := st.Encode()
	require.NotNil(c.t, msg, "status")
	return Signed{Type: "status", Msg: msg, SigA: c.sign(c.r.RSK, msg)}.Encode()
}

// enrollNew is a row-1 enroll_new bundle issued at issued, installing root
// and ek, with the status that shows it active a second later.
func (c *forgeChain) enrollNew(root *ecdsa.PrivateKey, ek []byte, issued uint64) (bundle, entry, ekbD, status []byte) {
	ekb, ekbD, ekbSig := c.ekBinding(root, 1, ek)
	in := Intent{RequestID: newID(), Op: OpEnrollNew, ExpiresAtMs: issued + 600_000,
		CandidateRoot: pointOf(c.t, root), EKBindingDigest: ekbD, DeviceIntentDigest: digestOf("dev-1")}
	a := CPAuthz{Actor: ActorUser, AssertedAtMs: issued, EvidenceDigest: digestOf("ev-1")}
	res := Result{Seq: 1, State: StateActive, ActiveRoot: pointOf(c.t, root), EKBindingDigest: ekbD,
		Assurance: AssuranceInitial, IssuedAtMs: issued}
	bundle, entry = c.finish(in, a, res, forgeSigs{candidate: root, ekb: ekb, ekbSig: ekbSig})
	status = c.status(Status{Seq: 1, HeadEntryDigest: entry, State: StateActive,
		ActiveRootDigest: Digest(pointOf(c.t, root)), EKBindingDigest: ekbD, AsOfMs: issued + 1_000})
	return bundle, entry, ekbD, status
}

// forgeActive is an account that row 1 enrolled at testT0 (root1, EK1,
// segment initial), verified by a co-member with VerifyChain from genesis.
type forgeActive struct {
	*forgeChain
	root1      *ecdsa.PrivateKey
	ek1, ekb1D []byte
	entry1     []byte
	status1    []byte
	head1      ChainHead
}

func newForgeActive(t *testing.T) *forgeActive {
	t.Helper()
	c := newForgeChain(t)
	root1, ek1 := testKey(t), forgedEK(t)
	b1, entry1, ekb1D, st := c.enrollNew(root1, ek1, testT0)
	head1, vs, _, r := VerifyChain(c.tr, HighWater{}, GenesisHead(c.realm, c.account), ChainInput{Bundles: [][]byte{b1}, Status: st},
		AccountMemory{}, nowOf(t, testT0+1_000), UseNewSecret)
	require.Equal(t, ReasonOK, r, "harness: row 1 must verify")
	spki, ok := vs.WrapKey(nil)
	require.True(t, ok)
	require.Equal(t, ek1, spki)
	return &forgeActive{forgeChain: c, root1: root1, ek1: ek1, ekb1D: ekb1D, entry1: entry1, status1: st, head1: head1}
}
