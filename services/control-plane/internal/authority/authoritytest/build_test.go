package authoritytest

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func TestDelegationIsDualSignedAndAcceptedByTrust(t *testing.T) {
	r := NewRealm(t)
	tr := r.Trust(t, r.Current)
	if tr.CurrentGen() != 0 || tr.HighWater().Serial != 1 {
		t.Fatalf("trust HW = %+v, want gen 0 serial 1", tr.HighWater())
	}
}

func TestRealmOptionsAndNextDelegation(t *testing.T) {
	r := NewRealm(t, WithoutOperatorKeys(), WithScope(ac.OpSuspend, ac.OpErase), WithWindow(1_000, 2_000))
	d := r.Current.D
	if len(d.OperatorPublicKeys) != 0 || !slices.Equal(d.Scope, []string{ac.OpErase, ac.OpSuspend}) ||
		d.NotBeforeMs != 1_000 || d.NotAfterMs != 2_000 {
		t.Fatalf("the options did not reach the delegation: %+v", d)
	}
	if !ac.VerifyP384(Point(r.PrimaryA), r.Current.Msg, r.Current.SigA) ||
		!ac.VerifyP384(Point(r.PrimaryB), r.Current.Msg, r.Current.SigB) {
		t.Fatal("the delegation built with options is not signed by both primary anchors")
	}

	r = NewRealm(t)
	first := r.Current
	other, _ := genKey(t)
	next := r.NextDelegation(t, WithRSKs(Point(other)))
	if next.D.Serial != 2 || next.D.MinValidSerial != 2 || r.Current.D.Serial != 2 ||
		len(next.D.RSKPublicKeys) != 1 || !bytes.Equal(next.D.RSKPublicKeys[0], Point(other)) {
		t.Fatalf("NextDelegation = %+v, want serial 2, min_valid 2 and the given RSK, as the current delegation", next.D)
	}
	if hw := r.Trust(t, first, next).HighWater(); hw.Serial != 2 {
		t.Fatalf("trust HW = %+v, want serial 2", hw)
	}
}

// TestEveryBuilderDecodesAndCarriesValidSignatures checks each published
// builder against §2.5–§2.7: the CAK signs cp-authz, cp-authz binds the
// evidence and the intent, IntentForOp admits the positions, and the op
// carries exactly the published signatures §2.7 names, each over the intent
// digest.
func TestEveryBuilderDecodesAndCarriesValidSignatures(t *testing.T) {
	r := NewRealm(t)
	a := r.NewAccount(t)
	enroll := a.EnrollNew(t) // on the absent head, as in production
	a.Root, _ = genKey(t)
	a.Actor = NewDevice(t)
	lk := NewLegacyKey(t)
	a.Legacy = &lk
	a.Head = HeadView{Seq: 1, Entry: ac.Digest([]byte("e1")), Dseq: 1, DeviceHead: ac.Digest([]byte("d1")),
		Root: Point(a.Root), State: ac.StateActive}
	rec, _ := genKey(t)
	bind := a.BindRecovery(t, rec, ac.BindWaitNow, nil) // sets a.Recovery
	// sigs: c candidate, r current root, o operator, k recovery key.
	for name, tc := range map[string]struct {
		b    Built
		sigs string
	}{
		"enroll_new":         {enroll, "c"},
		"suspend":            {a.Suspend(t, ac.ActorUser), ""},
		"suspend/operator":   {a.Suspend(t, ac.ActorOperator), "o"},
		"unsuspend":          {a.Unsuspend(t, ac.ActorUser), "r"},
		"unsuspend/operator": {a.Unsuspend(t, ac.ActorOperator), "o"},
		"erase":              {a.Erase(t, ac.ActorUser, ac.FactorPassword), ""},
		"erase/operator":     {a.Erase(t, ac.ActorOperator), "o"},
		"rotate_root":        {a.RotateRoot(t), "cr"},
		"rotate_ek":          {a.RotateEK(t), "r"},
		"replace_root":       {a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword), "c"},
		"cancel/root":        {a.CancelByRoot(t), "r"},
		"cancel/pop":         {a.CancelByPoP(t), ""},
		"cancel/recovery":    {a.CancelByRecovery(t), "k"},
		"revoke_root":        {a.RevokeRoot(t), "o"},
		"recover":            {a.Recover(t, bind.BindBytes, bind.Parts.Submit.SigRoot), "ck"},
	} {
		s, err := ac.DecodeSubmit(tc.b.Bytes)
		if err != nil {
			t.Fatalf("%s: DecodeSubmit: %v", name, err)
		}
		if !ac.VerifyP384(Point(r.CAK), s.CPAuthz, s.CPAuthzSig) {
			t.Fatalf("%s: cp_authz_sig does not verify under the CAK", name)
		}
		az, _ := ac.DecodeCPAuthz(s.CPAuthz)
		if string(az.EvidenceDigest) != string(ac.Digest(s.CPEvidence)) {
			t.Fatalf("%s: evidence_digest does not bind cp-evidence", name)
		}
		in, _ := ac.DecodeIntent(s.Intent)
		if string(az.IntentDigest) != string(ac.Digest(s.Intent)) || in.AccountID != a.ID {
			t.Fatalf("%s: cp-authz does not bind the intent", name)
		}
		if got := ac.IntentForOp(in, az.Actor, len(s.LegacyPopSig) > 0, len(s.SigCurrentRoot) > 0); got != "" {
			t.Fatalf("%s: IntentForOp = %s, want admitted (the builder breaks §2.7)", name, got)
		}
		id := ac.Digest(s.Intent)
		for _, p := range []struct {
			tag, role string
			key, sig  []byte
		}{
			{"c", ac.RoleCandidate, in.CandidateRoot, s.SigCandidate},
			{"r", ac.RoleCurrentRoot, Point(a.Root), s.SigCurrentRoot},
			{"o", ac.RoleOperator, Point(r.Operator), s.SigOperator},
			{"k", ac.RoleRecoveryKey, Point(a.Recovery), s.SigRecovery},
		} {
			want := strings.Contains(tc.sigs, p.tag)
			if want != (len(p.sig) > 0) || (want && !ac.VerifyP384(p.key, ac.Sig{Role: p.role, Digest: id}.Encode(), p.sig)) {
				t.Fatalf("%s: %s present = %v, want %v and valid over the intent digest", name, p.role, len(p.sig) > 0, want)
			}
		}
	}
}

func TestEnrollNewSignaturesVerify(t *testing.T) {
	r := NewRealm(t)
	a := r.NewAccount(t)
	b := a.EnrollNew(t)
	s, _ := ac.DecodeSubmit(b.Bytes)
	in, _ := ac.DecodeIntent(s.Intent)
	if !ac.VerifyP384(in.CandidateRoot, ac.Sig{Role: ac.RoleCandidate, Digest: ac.Digest(s.Intent)}.Encode(), s.SigCandidate) {
		t.Fatal("sig_candidate does not verify")
	}
	if !ac.VerifyP384(in.CandidateRoot, s.EKBinding, s.EKBindingSig) {
		t.Fatal("ek_binding_sig does not verify under the candidate")
	}
	if string(in.DeviceIntentDigest) != string(ac.Digest(s.DeviceIntent)) {
		t.Fatal("intent #15 is not SHA-384(device-intent)")
	}
	di, err := ac.DecodeDeviceIntent(s.DeviceIntent)
	if err != nil || ac.DeviceIntentForOp(di, in.CandidateRoot) != "" {
		t.Fatalf("device-intent decode or per-op check failed: %v", err)
	}
	if !ac.VerifyP384(di.NewDeviceDSK, ac.Sig{Role: ac.RoleNewDevice, Digest: ac.Digest(s.DeviceIntent)}.Encode(), s.SigNewDevice) {
		t.Fatal("sig_new_device does not verify over D")
	}
	if _, err := ac.ValidateEKSPKI(b.EKSPKI); err != nil {
		t.Fatalf("FakeEKSPKI is not a valid EK SPKI: %v", err)
	}
}

func TestLegacyKeyPoPVerifies(t *testing.T) {
	r := NewRealm(t)
	a := r.NewAccount(t)
	lk := NewLegacyKey(t)
	a.Legacy = &lk
	b := a.EnrollExisting(t, true, ac.FactorPassword, ac.FactorTOTP)
	s, _ := ac.DecodeSubmit(b.Bytes)
	in, _ := ac.DecodeIntent(s.Intent)
	pop := ac.LegacyEKPoP{RealmID: r.ID, AccountID: a.ID, IntentDigest: ac.Digest(s.Intent), LegacyEKDigest: lk.Digest}.Encode()
	if !ac.VerifyLegacyPSS(s.LegacySPKI, pop, s.LegacyPopSig) || string(in.LegacyEKDigest) != string(lk.Digest) {
		t.Fatal("the PoP does not verify")
	}
}

func TestDeviceBuildersPassCheckDeviceSubmit(t *testing.T) {
	r := NewRealm(t)
	a := r.NewAccount(t)
	a.Root, _ = genKey(t)
	a.Actor = NewDevice(t)
	a.Head = HeadView{Seq: 1, Entry: ac.Digest([]byte("e1")), Dseq: 1, DeviceHead: ac.Digest([]byte("d1")), Root: Point(a.Root), State: ac.StateActive}
	rec, _ := genKey(t)
	for name, b := range map[string]BuiltDevice{
		"add_device":   a.AddDevice(t, NewDevice(t)),
		"bind first":   a.BindRecovery(t, rec, ac.BindWaitNow, nil),
		"unbind":       a.UnbindRecovery(t, ac.BindWaitNow, nil),
		"unbind prior": a.UnbindRecovery(t, "", rec),
		"cancel root":  a.CancelBindByRoot(t, nil),
		"cancel sheet": a.CancelBindBySheet(t, rec, nil),
	} {
		if _, err := ac.CheckDeviceSubmit(b.Bytes); err != nil {
			t.Fatalf("%s: CheckDeviceSubmit: %v", name, err)
		}
	}
}
