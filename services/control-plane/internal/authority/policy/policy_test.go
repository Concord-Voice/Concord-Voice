package policy

import (
	"bytes"
	"crypto/ecdh"
	"testing"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

const realm, acct, req = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"

// pt is b·G on P-384, uncompressed. A1's codec admits only on-curve points
// at every b97 position, so a filler point would not encode.
func pt(b byte) []byte {
	k := make([]byte, 48)
	k[47] = b
	priv, err := ecdh.P384().NewPrivateKey(k)
	if err != nil {
		panic(err)
	}
	return priv.PublicKey().Bytes()
}

var sig96 = bytes.Repeat([]byte{1}, 96)

func allScope() []string {
	return []string{ac.OpCancelPending, ac.OpEnrollExisting, ac.OpEnrollNew, ac.OpErase, ac.OpRecover, ac.OpReplaceRoot,
		ac.OpRevokeRoot, ac.OpRotateEK, ac.OpRotateRoot, ac.OpSuspend, ac.OpUnsuspend, ac.OpAddDevice,
		ac.OpBindRecovery, ac.OpCancelRecoveryBind, ac.ScopeStatus}
}

func activeHead() Head {
	return Head{Pub: ac.PublishedHead{Seq: 3, Entry: ac.Digest([]byte("e3")), State: ac.StateActive, Susp: []string{},
		Root: pt(5), EK: ac.Digest([]byte("ek")), EKGen: 1, RG: 1}, Segment: ac.AssuranceInitial}
}

// in builds an Input for op/actor against h with the bundle positions §2.7 requires.
func in(h Head, op, actor string, ev ac.Evidence, f func(*ac.Intent, *ac.Bundle)) Input {
	it := ac.Intent{RealmID: realm, AccountID: acct, RequestID: req, Op: op, ExpiresAtMs: 10, ExpectedSeq: h.Pub.Seq,
		ExpectedRecoveryGen: h.Pub.RG, ExpectedHeadDigest: h.Pub.Entry}
	b := ac.Bundle{}
	if f != nil {
		f(&it, &b)
	}
	b.Intent = it.Encode()
	b.CPAuthz = ac.CPAuthz{RealmID: realm, AccountID: acct, RequestID: req, IntentDigest: ac.Digest(b.Intent),
		Actor: actor, AssertedAtMs: 1, EvidenceDigest: ac.Digest([]byte("e"))}.Encode()
	return Input{Head: h, Bundle: b, Intent: it, Actor: actor, Evidence: ev, Scope: allScope(), DBNowMs: 1_000_000_000_000}
}

func withSuspend(*ac.Intent, *ac.Bundle) {}

func withRotateEK(h Head) func(*ac.Intent, *ac.Bundle) {
	return func(it *ac.Intent, b *ac.Bundle) {
		it.EKBindingDigest, it.PredecessorRootDigest = ac.Digest([]byte("ekb")), ac.KeyDigest(h.Pub.Root)
		it.DeviceIntentDigest = ac.Digest([]byte("di"))
		b.SigCurrentRoot, b.EKBinding, b.EKBindingSig = sig96, []byte("ekb"), sig96
	}
}

var password = ac.Evidence{Factors: []string{ac.FactorPassword}, PasswordLogin: true} // pragma: allowlist secret

func TestOperatorActorOutsideOperatorRowsIsInsufficientAuthorization(t *testing.T) {
	d := Admit(in(activeHead(), ac.OpRotateEK, ac.ActorOperator, password, withRotateEK(activeHead())))
	if d.Refusal != ac.RefusalInsufficientAuthorization {
		t.Fatalf("operator rotate_ek = %q, want insufficient_authorization", d.Refusal)
	}
}

func TestOpOutsideScopeIsPolicyDisabled(t *testing.T) {
	i := in(activeHead(), ac.OpSuspend, ac.ActorUser, password, withSuspend)
	i.Scope = []string{ac.OpErase, ac.ScopeStatus}
	if d := Admit(i); d.Refusal != ac.RefusalPolicyDisabled {
		t.Fatalf("suspend outside scope = %q, want policy_disabled", d.Refusal)
	}
}

func TestEnrollNewForAPopulationUUIDIsInPopulation(t *testing.T) {
	h := Head{Pub: ac.GenesisPublishedHead(), InPopulation: true}
	i := in(h, ac.OpEnrollNew, ac.ActorUser, ac.Evidence{Factors: []string{ac.FactorRegistration}}, func(it *ac.Intent, b *ac.Bundle) {
		it.CandidateRoot, it.EKBindingDigest, it.DeviceIntentDigest = pt(7), ac.Digest([]byte("ekb")), ac.Digest([]byte("di"))
		b.SigCandidate, b.EKBinding, b.EKBindingSig = sig96, []byte("ekb"), sig96
	})
	if d := Admit(i); d.Refusal != ac.RefusalInPopulation {
		t.Fatalf("enroll_new for a population UUID = %q, want in_population", d.Refusal)
	}
}

func TestNoRowIsWrongState(t *testing.T) {
	h := Head{Pub: ac.GenesisPublishedHead()}
	if d := Admit(in(h, ac.OpSuspend, ac.ActorUser, password, withSuspend)); d.Refusal != ac.RefusalWrongState {
		t.Fatalf("suspend from A0 = %q, want wrong_state", d.Refusal)
	}
}

func TestForceUnverifiedRefusesRows1256And9AndForcesRows3And11(t *testing.T) {
	h := activeHead()
	h.ForceUnverified = true
	i := in(h, ac.OpRotateEK, ac.ActorUser, ac.Evidence{Factors: []string{ac.FactorPassword, ac.FactorTOTP},
		PasswordLogin: true}, withRotateEK(h))
	i.EKBinding = &ac.EKBinding{RealmID: realm, AccountID: acct, RootDigest: ac.KeyDigest(h.Pub.Root), EKGeneration: 2}
	i.EKBindingBytes = []byte("ekb")
	if d := Admit(i); d.Refusal != ac.RefusalWrongState {
		t.Fatalf("rotate_ek under force_unverified = %q, want wrong_state", d.Refusal)
	}
	full := ac.Evidence{Factors: []string{ac.FactorEmail, ac.FactorPassword}, PasswordLogin: true, RecoveryWindowClear: true}
	r := in(h, ac.OpReplaceRoot, ac.ActorUser, full, func(it *ac.Intent, b *ac.Bundle) {
		it.CandidateRoot, it.EKBindingDigest, it.PredecessorRootDigest = pt(8), ac.Digest([]byte("ekb")), ac.KeyDigest(h.Pub.Root)
		it.DeviceIntentDigest = ac.Digest([]byte("di"))
		b.SigCandidate, b.EKBinding, b.EKBindingSig = sig96, []byte("ekb"), sig96
	})
	r.EKBinding = &ac.EKBinding{RealmID: realm, AccountID: acct, RootDigest: ac.KeyDigest(pt(8)), EKGeneration: 2}
	r.EKBindingBytes = []byte("ekb")
	d := Admit(r)
	if d.Refusal != "" || d.Assurance != ac.Assurance(ac.AssuranceUnverified) {
		t.Fatalf("replace_root under force_unverified = (%q, %q), want admitted as unverified", d.Refusal, d.Assurance)
	}
}

func TestIntentValueChecksRunInOrder(t *testing.T) {
	h := activeHead()
	cases := []struct {
		name string
		mut  func(*ac.Intent)
		want ac.Refusal
	}{
		{"seq", func(it *ac.Intent) { it.ExpectedSeq++ }, ac.RefusalStaleSeq},
		{"head", func(it *ac.Intent) { it.ExpectedHeadDigest = ac.Digest([]byte("other")) }, ac.RefusalStaleHead},
		{"rg", func(it *ac.Intent) { it.ExpectedRecoveryGen++ }, ac.RefusalStaleRecoveryGen},
		{"predecessor", func(it *ac.Intent) { it.PredecessorRootDigest = ac.Digest([]byte("x")) }, ac.RefusalPredecessorMismatch},
	}
	for _, c := range cases {
		i := in(h, ac.OpRotateEK, ac.ActorUser, ac.Evidence{Factors: []string{ac.FactorPassword}, PasswordLogin: true}, func(it *ac.Intent, b *ac.Bundle) {
			withRotateEK(h)(it, b)
			c.mut(it)
		})
		i.EKBinding = &ac.EKBinding{RealmID: realm, AccountID: acct, RootDigest: ac.KeyDigest(h.Pub.Root), EKGeneration: 2}
		i.EKBindingBytes = []byte("ekb")
		if d := Admit(i); d.Refusal != c.want {
			t.Fatalf("%s: %q, want %q", c.name, d.Refusal, c.want)
		}
	}
}

func TestEKBindingChecks(t *testing.T) {
	h := activeHead()
	base := func(gen uint64, root []byte, bytesForDigest string) Input {
		i := in(h, ac.OpRotateEK, ac.ActorUser, ac.Evidence{Factors: []string{ac.FactorPassword}, PasswordLogin: true}, withRotateEK(h))
		i.EKBinding = &ac.EKBinding{RealmID: realm, AccountID: acct, RootDigest: root, EKGeneration: gen}
		i.EKBindingBytes = []byte(bytesForDigest)
		return i
	}
	if d := Admit(base(2, ac.KeyDigest(h.Pub.Root), "other")); d.Refusal != ac.RefusalEKMismatch {
		t.Fatalf("digest mismatch = %q, want ek_mismatch", d.Refusal)
	}
	if d := Admit(base(2, ac.KeyDigest(pt(9)), "ekb")); d.Refusal != ac.RefusalEKMismatch {
		t.Fatalf("wrong signer = %q, want ek_mismatch", d.Refusal)
	}
	if d := Admit(base(3, ac.KeyDigest(h.Pub.Root), "ekb")); d.Refusal != ac.RefusalEKGeneration {
		t.Fatalf("generation skip = %q, want ek_generation", d.Refusal)
	}
	if d := Admit(base(2, ac.KeyDigest(h.Pub.Root), "ekb")); d.Refusal != "" {
		t.Fatalf("a valid rotate_ek with S met = %q", d.Refusal)
	}
}

func TestFloorMissIsInsufficientAuthorization(t *testing.T) {
	h := activeHead()
	i := in(h, ac.OpRotateEK, ac.ActorUser, ac.Evidence{Factors: []string{ac.FactorBackupCode}, MFAEnrolled: true,
		PasswordLogin: true}, withRotateEK(h))
	i.EKBinding = &ac.EKBinding{RealmID: realm, AccountID: acct, RootDigest: ac.KeyDigest(h.Pub.Root), EKGeneration: 2}
	i.EKBindingBytes = []byte("ekb")
	if d := Admit(i); d.Refusal != ac.RefusalInsufficientAuthorization {
		t.Fatalf("rotate_ek with a backup code only = %q, want insufficient_authorization (LD-36)", d.Refusal)
	}
}

func replaceIn(h Head) Input {
	full := ac.Evidence{Factors: []string{ac.FactorEmail, ac.FactorPassword}, PasswordLogin: true, RecoveryWindowClear: true}
	i := in(h, ac.OpReplaceRoot, ac.ActorUser, full, func(it *ac.Intent, b *ac.Bundle) {
		it.CandidateRoot, it.EKBindingDigest, it.PredecessorRootDigest = pt(8), ac.Digest([]byte("ekb")), ac.KeyDigest(h.Pub.Root)
		it.DeviceIntentDigest = ac.Digest([]byte("di"))
		b.SigCandidate, b.EKBinding, b.EKBindingSig = sig96, []byte("ekb"), sig96
	})
	i.EKBinding = &ac.EKBinding{RealmID: realm, AccountID: acct, RootDigest: ac.KeyDigest(pt(8)), EKGeneration: 2}
	i.EKBindingBytes = []byte("ekb")
	return i
}

func TestNCancelTurnsRow11IntoD(t *testing.T) {
	h := activeHead()
	i := replaceIn(h)
	h.LastCancelAtMs = i.DBNowMs - NCancelMs + 1
	i.Head = h
	if d := Admit(i); d.Refusal != "" || d.OutcomeP || d.Post.State != ac.StateDisputed || d.Assurance != "" {
		t.Fatalf("row 11 within N_cancel = %+v, want D with assurance \"\"", d)
	}
	h.LastCancelAtMs = i.DBNowMs - NCancelMs
	i.Head = h
	if d := Admit(i); !d.OutcomeP || d.Post.State != ac.StatePending {
		t.Fatalf("row 11 exactly N_cancel after the cancel = %+v, want P", d)
	}
}

func TestRow18WithinNCancelOfAnEraseCancelIsWrongState(t *testing.T) {
	h := activeHead()
	i := in(h, ac.OpErase, ac.ActorUser, password, nil)
	h.LastEraseCancelAtMs = i.DBNowMs - 1
	i.Head = h
	if d := Admit(i); d.Refusal != ac.RefusalWrongState {
		t.Fatalf("erase within N_cancel of an erase cancel = %q, want wrong_state (D-186)", d.Refusal)
	}
}

func TestSameEpochCompetitorIsWrongState(t *testing.T) {
	h := activeHead()
	h.Pub.State, h.Pub.Prior, h.Pub.PendRoot, h.Pub.PendEK = ac.StatePending, ac.StateActive, pt(9), ac.Digest([]byte("pe"))
	h.Pub.Op, h.Pub.ANB, h.PendingEpochDigest = ac.OpReplaceRoot, 5, ac.Digest([]byte("epoch"))
	i := replaceIn(h)
	i.EpochDigest = ac.Digest([]byte("epoch"))
	if d := Admit(i); d.Refusal != ac.RefusalWrongState {
		t.Fatalf("row 12 with the same epoch = %q, want wrong_state", d.Refusal)
	}
	i.EpochDigest = ac.Digest([]byte("other"))
	// Review round 2: a D outcome reports OutcomeP false, as the client derives it.
	if d := Admit(i); d.Refusal != "" || d.OutcomeP || d.Post.State != ac.StateDisputed || d.Assurance != "" {
		t.Fatalf("row 12 with another epoch = %+v, want D with OutcomeP false and assurance \"\"", d)
	}
}

func TestRootCancelOfAPrHeadIsWrongStateAndRevokedRootIsRootRevoked(t *testing.T) {
	h := activeHead()
	h.Pub.State, h.Pub.Prior, h.Pub.PendRoot, h.Pub.PendEK, h.Pub.Op, h.Pub.ANB = ac.StatePending, ac.StateActive, pt(9), ac.Digest([]byte("pe")), ac.OpRecover, 5
	cancel := func(h Head) Input {
		return in(h, ac.OpCancelPending, ac.ActorUser, ac.Evidence{}, func(it *ac.Intent, b *ac.Bundle) {
			it.PredecessorRootDigest = ac.KeyDigest(h.Pub.Root)
			b.SigCurrentRoot = sig96
		})
	}
	if d := Admit(cancel(h)); d.Refusal != ac.RefusalWrongState {
		t.Fatalf("root cancel of Pr = %q, want wrong_state", d.Refusal)
	}
	h.Pub.Op, h.Pub.RootRevoked = ac.OpReplaceRoot, 1
	if d := Admit(cancel(h)); d.Refusal != ac.RefusalRootRevoked {
		t.Fatalf("cancel by a revoked root = %q, want root_revoked", d.Refusal)
	}
}

func TestHeldExemptions(t *testing.T) {
	cases := []struct {
		code      ac.FromCode
		op, actor string
		refuses   bool
	}{
		{ac.FromAc, ac.OpRotateEK, ac.ActorUser, true},
		{ac.FromP, ac.OpCancelPending, ac.ActorUser, true},
		{ac.FromAc, ac.OpRevokeRoot, ac.ActorOperator, true},   // row 15 is not exempt
		{ac.FromP, ac.OpRevokeRoot, ac.ActorOperator, false},   // row 16
		{ac.FromAp, ac.OpRevokeRoot, ac.ActorOperator, false},  // row 17
		{ac.FromAc, ac.OpErase, ac.ActorOperator, false},       // row 19
		{ac.FromAc, ac.OpErase, ac.ActorUser, false},           // row 18 commits a Pe that cannot activate
		{ac.FromP, ac.OpCancelPending, ac.ActorRestore, false}, // row 22
	}
	for _, c := range cases {
		if got := HeldRefuses(c.code, c.op, c.actor); got != c.refuses {
			t.Fatalf("HeldRefuses(%s, %s, %s) = %v, want %v", c.code, c.op, c.actor, got, c.refuses)
		}
	}
}

func TestEmptyAssuranceStaysEmptyUnderForceUnverified(t *testing.T) {
	h := activeHead()
	h.ForceUnverified = true
	d := Admit(in(h, ac.OpSuspend, ac.ActorUser, password, withSuspend))
	if d.Refusal != "" || d.Assurance != "" {
		t.Fatalf("suspend under force_unverified = (%q, %q), want admitted with \"\" (V-12)", d.Refusal, d.Assurance)
	}
}
