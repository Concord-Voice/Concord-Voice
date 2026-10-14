package authoritycontract

import (
	"crypto/ecdsa"
	"testing"

	"github.com/stretchr/testify/require"
)

// The committed vectors (recoveryVectors in gen.mjs) carry §2.22's checks
// end to end. These tests pin what no well-formed chain can reach: each
// check's own refusal when the caller's invariants (headPub, linkCheck,
// presenceOf, parseBundle) are not there to stand in for it.

// bindProofFixture is a row-20 bundleCtx whose proof is valid: a B bound at
// seq n under root k, checked by a recover at seq resSeq against a head that
// holds `records` seq_index records.
type bindProofFixture struct {
	k   *ecdsa.PrivateKey
	c   *bundleCtx
	rec []byte // B.chain_entry_digest, the entry digest of result n
}

func newBindProofFixture(t *testing.T, n, resSeq uint64, records int) bindProofFixture {
	t.Helper()
	k := testKey(t)
	root := pointOf(t, k)
	realm, account := newID(), newID()
	entry := rnd(48)
	b := RecoveryBind{RealmID: realm, AccountID: account, RequestID: newID(), Kind: BindKindBind,
		RecoveryKey: testPoint(t), ChainSeq: n, ChainEntryDigest: entry, BindingRootDigest: Digest(root),
		EscrowPublicKey: testPoint(t)}
	d := Digest(b.Encode())
	head := ChainHead{RealmID: realm, AccountID: account,
		RootIndex: []string{RootRecord(Digest(root), root, AssuranceInitial)}}
	for i := 1; i <= records; i++ {
		rec := SeqRecord(rnd(48), nil)
		if uint64(i) == n {
			rec = SeqRecord(entry, Digest(root))
		}
		head.SeqIndex = append(head.SeqIndex, rec)
	}
	pb := parsedBundle{bind: &b, bindDigest: d,
		intent: Intent{RealmID: realm, AccountID: account, RecoveryBindDigest: d},
		res:    Result{Seq: resSeq},
		raw:    Bundle{RecoveryBindSig: signP1363(t, k, sigMsg(RoleCurrentRoot, d))}}
	return bindProofFixture{k: k, c: &bundleCtx{head: head, pb: pb}, rec: entry}
}

func TestCheckBindingProofAcceptsAValidProof(t *testing.T) {
	f := newBindProofFixture(t, 2, 4, 3)
	require.Equal(t, ReasonOK, checkBindingProof(f.c))
	require.Equal(t, Assurance(AssuranceInitial), f.c.bindingSegment, "check 5 reads the binding root's segment")
}

// Check 2's position is "B.chain_seq < the recover's seq". A consistent head
// holds exactly seq − 1 records (headPub, linkCheck), so the bound on the
// seq_index read refuses the same B; here the head carries one record more,
// and only the position check can refuse B at the recover's own seq.
func TestCheckBindingProofPositionIsBelowTheRecoversSeq(t *testing.T) {
	f := newBindProofFixture(t, 4, 4, 4)
	require.Equal(t, ReasonTransitionInvalid, checkBindingProof(f.c))
	f = newBindProofFixture(t, 5, 4, 5)
	require.Equal(t, ReasonTransitionInvalid, checkBindingProof(f.c))
}

// Each refusal no well-formed row-20 bundle reaches: no B or no proof
// signature (presenceOf requires both), chain_seq 0 (V2-04 forbids it on a
// bind), a position past the head's records, a recorded bind root that
// root_index lacks, and a recover_index record that does not parse.
func TestCheckBindingProofRefusesWhatItsCallersRuleOut(t *testing.T) {
	cases := map[string]func(f bindProofFixture){
		"no recovery-bind":     func(f bindProofFixture) { f.c.pb.bind = nil },
		"no recovery_bind_sig": func(f bindProofFixture) { f.c.pb.raw.RecoveryBindSig = nil },
		"chain_seq 0":          func(f bindProofFixture) { f.c.pb.bind.ChainSeq = 0 },
		"past the records":     func(f bindProofFixture) { f.c.head.SeqIndex = f.c.head.SeqIndex[:1] },
		"root not in root_index": func(f bindProofFixture) {
			f.c.head.RootIndex = nil
		},
		"unparseable recover_index": func(f bindProofFixture) {
			f.c.head.RecoverIndex = []string{"x." + b64s(rnd(48))}
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			f := newBindProofFixture(t, 2, 4, 3)
			mut(f)
			require.Equal(t, ReasonTransitionInvalid, checkBindingProof(f.c))
		})
	}
}

func TestCheckBindingProofRecoverIndexClauses(t *testing.T) {
	f := newBindProofFixture(t, 2, 4, 3)
	key := Digest(f.c.pb.bind.RecoveryKey)
	f.c.head.RecoverIndex = []string{RecoverRecord(2, rnd(48))}
	require.Equal(t, ReasonOK, checkBindingProof(f.c), "B at the activated recover's own seq is at least it")
	f.c.head.RecoverIndex = []string{RecoverRecord(3, rnd(48))}
	require.Equal(t, ReasonTransitionInvalid, checkBindingProof(f.c), "position below an activated recover")
	f.c.head.RecoverIndex = []string{RecoverRecord(1, key)}
	require.Equal(t, ReasonTransitionInvalid, checkBindingProof(f.c), "a key an activated recover burned")
}

func TestRootByDigest(t *testing.T) {
	root := testPoint(t)
	h := ChainHead{RootIndex: []string{RootRecord(Digest(root), root, AssuranceRecovery)}}
	require.Equal(t, root, rootByDigest(h, Digest(root)))
	require.Nil(t, rootByDigest(h, rnd(48)))
	h.RootIndex = []string{b64s(Digest(root)) + ".%%%." + AssuranceRecovery}
	require.Nil(t, rootByDigest(h, Digest(root)), "an undecodable point is no root")
}

func TestCheckPendingRecoveryKey(t *testing.T) {
	k := testKey(t)
	d := rnd(48)
	c := &bundleCtx{head: ChainHead{PendingRecoveryKey: pointOf(t, k)},
		pb: parsedBundle{intentDigest: d, raw: Bundle{SigRecovery: signP1363(t, k, sigMsg(RoleRecoveryKey, d))}}}
	require.Equal(t, ReasonOK, checkPendingRecoveryKey(c))
	c.pb.raw.SigRecovery = signP1363(t, testKey(t), sigMsg(RoleRecoveryKey, d))
	require.Equal(t, ReasonBadSignature, checkPendingRecoveryKey(c))
	c.head.PendingRecoveryKey = nil
	require.Equal(t, ReasonTransitionInvalid, checkPendingRecoveryKey(c), "a head with no pending recover has no row 21")
}
