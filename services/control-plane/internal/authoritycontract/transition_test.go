package authoritycontract

import (
	"bytes"
	"crypto/ecdh"
	"testing"
)

// The exported head converts to and from A2's pub without copying field by
// field: the struct conversion fails to compile if the two drift apart.
var _ = pub(PublishedHead{})

func trRealm() string   { return "11111111-1111-4111-8111-111111111111" }
func trAccount() string { return "22222222-2222-4222-8222-222222222222" }
func trReq() string     { return "33333333-3333-4333-8333-333333333333" }

// trPoint is b·G on P-384, uncompressed. A1's codec admits only on-curve
// points at every b97 position (kP97), so a filler point would not encode.
func trPoint(b byte) []byte {
	k := make([]byte, 48)
	k[47] = b
	priv, err := ecdh.P384().NewPrivateKey(k)
	if err != nil {
		panic(err)
	}
	return priv.PublicKey().Bytes()
}

func trBundle(t *testing.T, in Intent, actor string, b Bundle) Bundle {
	t.Helper()
	ib := in.Encode()
	if ib == nil {
		t.Fatal("intent did not encode")
	}
	az := CPAuthz{RealmID: in.RealmID, AccountID: in.AccountID, RequestID: in.RequestID,
		IntentDigest: Digest(ib), Actor: actor, AssertedAtMs: 1, EvidenceDigest: Digest([]byte("e"))}.Encode()
	if az == nil {
		t.Fatal("cp-authz did not encode")
	}
	b.Intent, b.CPAuthz = ib, az
	return b
}

func TestGenesisPublishedHeadIsA0(t *testing.T) {
	if got := GenesisPublishedHead().FromCode(); got != FromA0 {
		t.Fatalf("genesis classifies as %s, want A0", got)
	}
}

func TestSelectRowAndExpectedPostForEnrollNew(t *testing.T) {
	cand := trPoint(7)
	in := Intent{RealmID: trRealm(), AccountID: trAccount(), RequestID: trReq(), Op: OpEnrollNew,
		ExpiresAtMs: 100, CandidateRoot: cand, EKBindingDigest: Digest([]byte("ek")),
		DeviceIntentDigest: Digest([]byte("di"))}
	sig := bytes.Repeat([]byte{1}, 96)
	b := trBundle(t, in, ActorUser, Bundle{SigCandidate: sig, EKBinding: []byte("x"), EKBindingSig: sig})
	row, r := SelectRow(GenesisPublishedHead(), b)
	if r != ReasonOK || row.N != 1 {
		t.Fatalf("SelectRow = (%d, %s), want (1, ok)", row.N, r)
	}
	post := ExpectedPost(row, GenesisPublishedHead(), in, false)
	if post.State != StateActive || !bytes.Equal(post.Root, cand) || len(post.Susp) != 0 || post.Prior != "" {
		t.Fatalf("post = %+v, want active with the candidate root", post)
	}
}

func TestSelectRowRefusesAnUndecodableIntentOrCPAuthzAsMalformed(t *testing.T) {
	in := Intent{RealmID: trRealm(), AccountID: trAccount(), RequestID: trReq(), Op: OpEnrollNew,
		ExpiresAtMs: 100, CandidateRoot: trPoint(7), EKBindingDigest: Digest([]byte("ek")),
		DeviceIntentDigest: Digest([]byte("di"))}
	sig := bytes.Repeat([]byte{1}, 96)
	good := trBundle(t, in, ActorUser, Bundle{SigCandidate: sig, EKBinding: []byte("x"), EKBindingSig: sig})
	if _, r := SelectRow(GenesisPublishedHead(), good); r != ReasonOK {
		t.Fatalf("control: the intact bundle = %s, want ok", r)
	}
	badIntent, badAuthz := good, good
	badIntent.Intent = []byte("[")
	badAuthz.CPAuthz = []byte("[")
	for name, b := range map[string]Bundle{"intent": badIntent, "cp-authz": badAuthz} {
		if _, r := SelectRow(GenesisPublishedHead(), b); r != ReasonMalformed {
			t.Errorf("an undecodable %s = %s, want malformed", name, r)
		}
	}
}

func TestSelectRowRefusesARowTheFromStateDoesNotAdmit(t *testing.T) {
	in := Intent{RealmID: trRealm(), AccountID: trAccount(), RequestID: trReq(), Op: OpRotateEK,
		ExpectedSeq: 0, ExpiresAtMs: 100, EKBindingDigest: Digest([]byte("ek")),
		PredecessorRootDigest: Digest([]byte("r")), DeviceIntentDigest: Digest([]byte("di"))}
	sig := bytes.Repeat([]byte{1}, 96)
	b := trBundle(t, in, ActorUser, Bundle{SigCurrentRoot: sig, EKBinding: []byte("x"), EKBindingSig: sig})
	if _, r := SelectRow(GenesisPublishedHead(), b); r != ReasonTransitionInvalid {
		t.Fatalf("rotate_ek from A0 = %s, want transition_invalid", r)
	}
}

func TestActivatedPendingHeadInstallsTheCandidate(t *testing.T) {
	cand, ek := trPoint(9), Digest([]byte("pending-ek"))
	h := PublishedHead{Seq: 1, Entry: Digest([]byte("e1")), State: StatePending, Prior: StateAbsent, Susp: []string{},
		Pin: Digest([]byte("pin")), PendRoot: cand, PendEK: ek, PendEKGen: 1, RG: 1, ANB: 50, Op: OpEnrollExisting}
	a := h.Activated()
	if a.State != StateActive || !bytes.Equal(a.Root, cand) || !bytes.Equal(a.EK, ek) || a.EKGen != 1 || a.PendRoot != nil {
		t.Fatalf("activated = %+v", a)
	}
	if h.FromCode() != FromP || a.FromCode() != FromAc {
		t.Fatalf("codes = %s → %s, want P → Ac", h.FromCode(), a.FromCode())
	}
}

func TestCheckPostAcceptsTheExpectedResultAndRefusesAnother(t *testing.T) {
	cand := trPoint(7)
	in := Intent{RealmID: trRealm(), AccountID: trAccount(), RequestID: trReq(), Op: OpEnrollNew,
		ExpiresAtMs: 100, CandidateRoot: cand, EKBindingDigest: Digest([]byte("ek")),
		DeviceIntentDigest: Digest([]byte("di"))}
	sig := bytes.Repeat([]byte{1}, 96)
	b := trBundle(t, in, ActorUser, Bundle{SigCandidate: sig, EKBinding: []byte("x"), EKBindingSig: sig})
	row, _ := SelectRow(GenesisPublishedHead(), b)
	post := ExpectedPost(row, GenesisPublishedHead(), in, false)
	res := Result{State: post.State, PriorState: post.Prior, SuspendedBy: post.Susp, ActiveRoot: post.Root,
		EKBindingDigest: post.EK, RecoveryGen: row.DeltaRG, Assurance: AssuranceInitial}
	if r := CheckPost(row, GenesisPublishedHead(), in, res, false, "", ""); r != ReasonOK {
		t.Fatalf("CheckPost(expected) = %s", r)
	}
	res.State = StateSuspended
	if r := CheckPost(row, GenesisPublishedHead(), in, res, false, "", ""); r != ReasonTransitionInvalid {
		t.Fatalf("CheckPost(wrong state) = %s, want transition_invalid", r)
	}
}
