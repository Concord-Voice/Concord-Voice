package authoritytest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"testing"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/google/uuid"
)

// Spec describes one published submit for Build.
type Spec struct {
	Op, Actor, RequestID                                      string
	Factors                                                   []string
	MFAEnrolled, PasswordLogin, WindowClear, OperatorAssisted bool
	EpochDigest                                               []byte
	AssertedAtMs, ExpiresAtMs                                 uint64
	NewRoot, NewEK, PoP, RootSigns, OperatorSigns             bool
	CandidateKey                                              *ecdsa.PrivateKey // reuse a candidate key (key-binding conflict tests)
	RecoverySig                                               *ecdsa.PrivateKey
	Bind, BindSig                                             []byte
	NoDevice                                                  bool
	NewDevice                                                 *Device
	Removed                                                   []string
	Mutate                                                    func(*Parts)
}

// Parts are a submit's pieces; Spec.Mutate may change them before and after signing.
type Parts struct {
	Intent       ac.Intent
	CPAuthz      ac.CPAuthz
	Evidence     ac.CPEvidence
	DeviceIntent *ac.DeviceIntent
	EKBinding    *ac.EKBinding
	Submit       ac.Submit
	SkipCAKSign  bool
}

// Built is an encoded submit, its parts, and the candidate root and EK it introduced.
type Built struct {
	Bytes     []byte
	Parts     Parts
	Candidate *ecdsa.PrivateKey
	EKSPKI    []byte
}

func u01(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func salt(t testing.TB) []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func digestOrNil(p []byte) []byte {
	if len(p) == 0 {
		return nil
	}
	return ac.KeyDigest(p)
}

// actorOp reports an op whose attached device-intent names an actor device.
func actorOp(op string) bool {
	return op == ac.OpRotateRoot || op == ac.OpRotateEK || op == ac.OpUnsuspend
}

func (a *Account) expires(s Spec) uint64 {
	if s.ExpiresAtMs != 0 {
		return s.ExpiresAtMs
	}
	if s.Op == ac.OpCancelPending {
		return a.R.Clock.Ms + 120_000
	}
	return a.R.Clock.Ms + 600_000
}

// Build assembles, signs and encodes a submit per §2.5–§2.7. Positions the op
// does not use are left "" (IntentForOp's pattern).
func (a *Account) Build(t testing.TB, s Spec) Built {
	t.Helper()
	if s.Actor == "" {
		s.Actor = ac.ActorUser
	}
	if s.RequestID == "" {
		s.RequestID = uuid.NewString()
	}
	if s.AssertedAtMs == 0 {
		s.AssertedAtMs = a.R.Clock.Ms
	}
	var out Built
	p := Parts{Intent: ac.Intent{RealmID: a.R.ID, AccountID: a.ID, RequestID: s.RequestID, Op: s.Op,
		ExpiresAtMs: a.expires(s), ExpectedSeq: a.Head.Seq, ExpectedRecoveryGen: a.Head.RG,
		ExpectedHeadDigest: a.Head.Entry}}
	signerRoot := a.Root
	if s.NewRoot {
		out.Candidate = s.CandidateKey
		if out.Candidate == nil {
			out.Candidate, _ = genKey(t)
		}
		a.Pending = out.Candidate
		p.Intent.CandidateRoot = Point(out.Candidate)
		signerRoot = out.Candidate
	}
	a.positions(t, &s, &p, signerRoot)
	p.Evidence = ac.CPEvidence{RequestID: s.RequestID, Factors: sortedFactors(s.Factors), MFAEnrolled: u01(s.MFAEnrolled),
		PasswordLogin: u01(s.PasswordLogin), RecoveryWindowClear: u01(s.WindowClear),
		OperatorAssisted: u01(s.OperatorAssisted), CredentialEpochDigest: s.EpochDigest, EvidenceSalt: salt(t)}
	p.CPAuthz = ac.CPAuthz{RealmID: a.R.ID, AccountID: a.ID, RequestID: s.RequestID, Actor: s.Actor,
		AssertedAtMs: s.AssertedAtMs}
	if s.Mutate != nil {
		s.Mutate(&p)
	}
	var ekb []byte
	if p.EKBinding != nil { // encoded after Mutate, so a test's change to the binding is what gets signed
		out.EKSPKI, ekb = p.EKBinding.EKPublicKey, p.EKBinding.Encode()
		p.Intent.EKBindingDigest = ac.Digest(ekb)
	}
	a.sign(t, s, &p, ekb, signerRoot)
	if s.Mutate != nil {
		s.Mutate(&p) // a second call lets a test corrupt a signature after signing
	}
	out.Parts = p
	out.Bytes = p.Submit.Encode()
	if out.Bytes == nil {
		t.Fatalf("authoritytest: submit for %s does not encode", s.Op)
	}
	return out
}

// positions fills Build's positions after the candidate root, with the EK
// binding and device-intent behind them. It takes s by pointer for
// deviceIntentFor.
func (a *Account) positions(t testing.TB, s *Spec, p *Parts, signerRoot *ecdsa.PrivateKey) {
	t.Helper()
	// §2.7: unsuspend names the active root for the operator too, not only the user.
	if s.RootSigns || s.Op == ac.OpReplaceRoot || s.Op == ac.OpRecover || s.Op == ac.OpRotateRoot || s.Op == ac.OpRotateEK ||
		s.Op == ac.OpUnsuspend {
		p.Intent.PredecessorRootDigest = digestOrNil(a.Head.Root)
	}
	if s.NewEK {
		b := ac.EKBinding{RealmID: a.R.ID, AccountID: a.ID, RootDigest: ac.KeyDigest(Point(signerRoot)),
			EKGeneration: a.Head.EKGen + 1, EKAlg: "rsa-oaep-4096-sha256", EKPublicKey: FakeEKSPKI(t)}
		p.EKBinding = &b
	}
	if s.PoP && a.Legacy != nil {
		p.Intent.LegacyEKDigest = a.Legacy.Digest
	}
	if len(s.Bind) > 0 {
		p.Intent.RecoveryBindDigest = ac.Digest(s.Bind)
	}
	if !s.NoDevice && attachedDevice(s.Op, s.Actor) {
		di := a.deviceIntentFor(t, s)
		p.DeviceIntent = &di
		p.Intent.DeviceIntentDigest = ac.Digest(di.Encode())
	}
}

func (a *Account) sign(t testing.TB, s Spec, p *Parts, ekb []byte, signerRoot *ecdsa.PrivateKey) {
	t.Helper()
	ib := p.Intent.Encode()
	if ib == nil {
		t.Fatalf("authoritytest: intent for %s does not encode: %+v", s.Op, p.Intent)
	}
	id := ac.Digest(ib)
	eb := p.Evidence.Encode()
	p.CPAuthz.IntentDigest, p.CPAuthz.EvidenceDigest = id, ac.Digest(eb)
	az := p.CPAuthz.Encode()
	sub := ac.Submit{Intent: ib, CPAuthz: az, CPEvidence: eb}
	if !p.SkipCAKSign {
		sub.CPAuthzSig = p1363(t, a.R.CAK, az)
	}
	if s.NewRoot {
		sub.SigCandidate = SignSig(t, signerRoot, ac.RoleCandidate, id)
	}
	if s.RootSigns {
		sub.SigCurrentRoot = SignSig(t, a.Root, ac.RoleCurrentRoot, id)
	}
	if s.OperatorSigns {
		sub.SigOperator = SignSig(t, a.R.Operator, ac.RoleOperator, id)
	}
	if s.RecoverySig != nil {
		sub.SigRecovery = SignSig(t, s.RecoverySig, ac.RoleRecoveryKey, id)
	}
	if len(ekb) > 0 {
		sub.EKBinding, sub.EKBindingSig = ekb, p1363(t, signerRoot, ekb)
	}
	if s.PoP && a.Legacy != nil {
		sub.LegacySPKI = a.Legacy.SPKI
		sub.LegacyPopSig = legacyPoP(t, a, id)
	}
	if len(s.Bind) > 0 {
		sub.RecoveryBind, sub.RecoveryBindSig = s.Bind, s.BindSig
	}
	if p.DeviceIntent != nil {
		a.deviceSigs(t, s, p.DeviceIntent, &sub)
	}
	p.Submit = sub
}

// deviceSigs attaches the encoded device-intent and its sig_new_device and sig_actor.
func (a *Account) deviceSigs(t testing.TB, s Spec, di *ac.DeviceIntent, sub *ac.Submit) {
	t.Helper()
	db := di.Encode()
	d := ac.Digest(db)
	sub.DeviceIntent = db
	if s.NewDevice != nil {
		sub.SigNewDevice = SignSig(t, s.NewDevice.DSK, ac.RoleNewDevice, d)
	}
	if actorOp(s.Op) && a.Actor != nil {
		sub.SigActor = SignSig(t, a.Actor.DSK, ac.RoleActorDevice, d)
	}
}

// legacyPoP is RSA-PSS (SHA-256, MGF1-SHA-256, salt 32) over the
// legacy-ek-pop bytes, as VerifyLegacyPSS expects (§2.4).
func legacyPoP(t testing.TB, a *Account, intentDigest []byte) []byte {
	t.Helper()
	msg := ac.LegacyEKPoP{RealmID: a.R.ID, AccountID: a.ID, IntentDigest: intentDigest, LegacyEKDigest: a.Legacy.Digest}.Encode()
	h := sha256.Sum256(msg)
	sig, err := rsa.SignPSS(rand.Reader, a.Legacy.Priv, crypto.SHA256, h[:], &rsa.PSSOptions{SaltLength: 32})
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func attachedDevice(op, actor string) bool {
	switch op {
	case ac.OpEnrollNew, ac.OpEnrollExisting, ac.OpReplaceRoot, ac.OpRecover, ac.OpRotateRoot, ac.OpRotateEK:
		return true
	case ac.OpUnsuspend:
		return actor == ac.ActorUser
	}
	return false
}

// deviceIntentFor takes s by pointer: a new device it defaults must reach
// sign, which produces sig_new_device from s.NewDevice.
func (a *Account) deviceIntentFor(t testing.TB, s *Spec) ac.DeviceIntent {
	di := ac.DeviceIntent{RealmID: a.R.ID, AccountID: a.ID, RequestID: s.RequestID, Op: s.Op, Salt: salt(t),
		ExpectedSeq: a.Head.Seq, ExpectedHeadDigest: a.Head.Entry, RemovedDeviceIDs: append([]string{}, s.Removed...)}
	if actorOp(s.Op) && a.Actor != nil {
		di.ActorDeviceID, di.ExpectedDseq, di.ExpectedDeviceHead = a.Actor.ID, a.Head.Dseq, a.Head.DeviceHead
	} else {
		if s.NewDevice == nil {
			s.NewDevice = NewDevice(t)
		}
		di.NewDeviceID, di.NewDeviceDSK, di.NewDeviceDKX = s.NewDevice.ID, Point(s.NewDevice.DSK), Point(s.NewDevice.DKX)
	}
	return di
}

func sortedFactors(f []string) []string {
	out := append([]string{}, f...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// Convenience builders: the §2.7 positions and the usual factors for each op.

// EnrollNew builds enroll_new with the registration factor.
func (a *Account) EnrollNew(t testing.TB) Built {
	b := a.Build(t, Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true, Factors: []string{ac.FactorRegistration}})
	return b
}

// EnrollExisting builds enroll_existing with factors, carrying the legacy PoP when pop is set.
func (a *Account) EnrollExisting(t testing.TB, pop bool, factors ...string) Built {
	return a.Build(t, Spec{Op: ac.OpEnrollExisting, NewRoot: true, NewEK: true, PoP: pop, Factors: factors,
		PasswordLogin: true, WindowClear: true})
}

// RotateRoot builds rotate_root signed by the current root, removing the listed devices.
func (a *Account) RotateRoot(t testing.TB, removed ...string) Built {
	return a.Build(t, Spec{Op: ac.OpRotateRoot, NewRoot: true, NewEK: true, RootSigns: true, Removed: removed,
		Factors: []string{ac.FactorPassword}, PasswordLogin: true})
}

// RotateEK builds rotate_ek signed by the current root, removing the listed devices.
func (a *Account) RotateEK(t testing.TB, removed ...string) Built {
	return a.Build(t, Spec{Op: ac.OpRotateEK, NewEK: true, RootSigns: true, Removed: removed,
		Factors: []string{ac.FactorPassword}, PasswordLogin: true})
}

// ReplaceRoot builds replace_root with the given factors.
func (a *Account) ReplaceRoot(t testing.TB, factors ...string) Built {
	return a.Build(t, Spec{Op: ac.OpReplaceRoot, NewRoot: true, NewEK: true, Factors: factors, PasswordLogin: true,
		WindowClear: true})
}

// Suspend builds suspend for actor; an operator signs with admin WebAuthn.
func (a *Account) Suspend(t testing.TB, actor string) Built {
	s := Spec{Op: ac.OpSuspend, Actor: actor, Factors: []string{ac.FactorPassword}, PasswordLogin: true}
	if actor == ac.ActorOperator {
		s.OperatorSigns, s.Factors = true, []string{ac.FactorAdminWebAuthn}
	}
	return a.Build(t, s)
}

// Unsuspend builds unsuspend for actor; the user form is signed by the current root.
func (a *Account) Unsuspend(t testing.TB, actor string) Built {
	s := Spec{Op: ac.OpUnsuspend, Actor: actor, RootSigns: actor == ac.ActorUser, Factors: []string{ac.FactorPassword},
		PasswordLogin: true}
	if actor == ac.ActorOperator {
		s.OperatorSigns, s.Factors = true, []string{ac.FactorAdminWebAuthn}
	}
	return a.Build(t, s)
}

// CancelByRoot builds cancel_pending signed by the current root.
func (a *Account) CancelByRoot(t testing.TB) Built {
	return a.Build(t, Spec{Op: ac.OpCancelPending, RootSigns: true})
}

// CancelByPoP builds cancel_pending carrying the legacy-key PoP.
func (a *Account) CancelByPoP(t testing.TB) Built {
	return a.Build(t, Spec{Op: ac.OpCancelPending, PoP: true})
}

// CancelByRecovery builds cancel_pending signed by the bound recovery key.
func (a *Account) CancelByRecovery(t testing.TB) Built {
	return a.Build(t, Spec{Op: ac.OpCancelPending, RecoverySig: a.Recovery})
}

// RevokeRoot builds the operator's revoke_root.
func (a *Account) RevokeRoot(t testing.TB) Built {
	return a.Build(t, Spec{Op: ac.OpRevokeRoot, Actor: ac.ActorOperator, OperatorSigns: true,
		Factors: []string{ac.FactorAdminWebAuthn}})
}

// Erase builds erase for actor; an operator signs with admin WebAuthn.
func (a *Account) Erase(t testing.TB, actor string, factors ...string) Built {
	s := Spec{Op: ac.OpErase, Actor: actor, Factors: factors, PasswordLogin: true}
	if actor == ac.ActorOperator {
		s.OperatorSigns, s.Factors = true, []string{ac.FactorAdminWebAuthn}
	}
	return a.Build(t, s)
}

// Recover builds row 20 with the binding proof B (the bind's own bytes) and
// its sig_root (LD-45: taken from the bind's device-bundle).
func (a *Account) Recover(t testing.TB, bind, bindSig []byte) Built {
	return a.Build(t, Spec{Op: ac.OpRecover, NewRoot: true, NewEK: true, RecoverySig: a.Recovery, Bind: bind, BindSig: bindSig})
}
