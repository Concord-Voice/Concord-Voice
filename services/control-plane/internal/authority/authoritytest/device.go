package authoritytest

import (
	"crypto/ecdsa"
	"testing"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/google/uuid"
)

// DeviceSpec describes one private device submit for BuildDevice.
type DeviceSpec struct {
	Op, RequestID, BindWait   string
	NewDevice                 *Device
	Bind                      *ac.RecoveryBind
	BindKey, PriorKey         *ecdsa.PrivateKey
	SheetForm, RootForm       bool
	Factors                   []string
	MFAEnrolled               bool
	AssertedAtMs, ExpiresAtMs uint64
	Mutate                    func(*DeviceParts)
}

// DeviceParts are a device submit's pieces; DeviceSpec.Mutate may change them.
type DeviceParts struct {
	DeviceIntent *ac.DeviceIntent
	Bind         *ac.RecoveryBind
	CPAuthz      ac.CPAuthz
	Evidence     ac.CPEvidence
	Submit       ac.DeviceSubmit
}

// BuiltDevice is an encoded device submit, its parts, and its bind's bytes.
type BuiltDevice struct {
	Bytes     []byte
	Parts     DeviceParts
	BindBytes []byte
}

// BuildDevice builds a device-submit (§2.5 private table, §2.22 forms). The
// cp-authz intent_digest is D, or H(B) on the sheet form (DR3).
func (a *Account) BuildDevice(t testing.TB, s DeviceSpec) BuiltDevice {
	t.Helper()
	if s.RequestID == "" {
		s.RequestID = uuid.NewString()
	}
	if s.AssertedAtMs == 0 {
		s.AssertedAtMs = a.R.Clock.Ms
	}
	if s.ExpiresAtMs == 0 {
		s.ExpiresAtMs = a.R.Clock.Ms + 600_000
	}
	var p DeviceParts
	var bb []byte
	if s.Bind != nil {
		b := *s.Bind
		b.RequestID = s.RequestID
		p.Bind = &b
		bb = b.Encode()
		if bb == nil {
			t.Fatalf("authoritytest: recovery-bind does not encode: %+v", b)
		}
	}
	if !s.SheetForm {
		di := ac.DeviceIntent{RealmID: a.R.ID, AccountID: a.ID, RequestID: s.RequestID, Op: s.Op, Salt: salt(t),
			ExpiresAtMs: s.ExpiresAtMs, ExpectedSeq: a.Head.Seq, ExpectedHeadDigest: a.Head.Entry,
			ExpectedDseq: a.Head.Dseq, ExpectedDeviceHead: a.Head.DeviceHead, ActorDeviceID: a.Actor.ID,
			RemovedDeviceIDs: []string{}, BindWait: s.BindWait}
		if s.NewDevice != nil {
			di.NewDeviceID, di.NewDeviceDSK, di.NewDeviceDKX = s.NewDevice.ID, Point(s.NewDevice.DSK), Point(s.NewDevice.DKX)
		}
		if bb != nil {
			di.RecoveryBindDigest = ac.Digest(bb)
		}
		p.DeviceIntent = &di
	}
	p.Evidence = ac.CPEvidence{RequestID: s.RequestID, Factors: sortedFactors(s.Factors), MFAEnrolled: u01(s.MFAEnrolled),
		PasswordLogin: 1, EvidenceSalt: salt(t)}
	p.CPAuthz = ac.CPAuthz{RealmID: a.R.ID, AccountID: a.ID, RequestID: s.RequestID, Actor: ac.ActorUser,
		AssertedAtMs: s.AssertedAtMs}
	if s.Mutate != nil {
		s.Mutate(&p)
	}
	a.signDevice(t, s, &p, bb)
	if s.Mutate != nil {
		s.Mutate(&p)
	}
	out := BuiltDevice{Parts: p, BindBytes: bb, Bytes: p.Submit.Encode()}
	if out.Bytes == nil {
		t.Fatalf("authoritytest: device-submit for %s does not encode", s.Op)
	}
	return out
}

func (a *Account) signDevice(t testing.TB, s DeviceSpec, p *DeviceParts, bb []byte) {
	t.Helper()
	eb := p.Evidence.Encode()
	var sub ac.DeviceSubmit
	var d []byte
	if p.DeviceIntent != nil {
		db := p.DeviceIntent.Encode()
		if db == nil {
			t.Fatalf("authoritytest: device-intent does not encode: %+v", *p.DeviceIntent)
		}
		d = ac.Digest(db)
		sub.DeviceIntent = db
		sub.SigActor = SignSig(t, a.Actor.DSK, ac.RoleActorDevice, d)
	}
	authzOver := d
	if s.SheetForm {
		authzOver = ac.Digest(bb)
	}
	p.CPAuthz.IntentDigest, p.CPAuthz.EvidenceDigest = authzOver, ac.Digest(eb)
	az := p.CPAuthz.Encode()
	sub.CPAuthz, sub.CPAuthzSig, sub.CPEvidence = az, p1363(t, a.R.CAK, az), eb
	switch {
	case s.Op == ac.OpAddDevice:
		sub.SigRoot = SignSig(t, a.Root, ac.RoleCurrentRoot, d)
		sub.SigNewDevice = SignSig(t, s.NewDevice.DSK, ac.RoleNewDevice, d)
	case bb != nil:
		h := ac.Digest(bb)
		sub.RecoveryBind = bb
		if !s.SheetForm {
			sub.SigRoot = SignSig(t, a.Root, ac.RoleCurrentRoot, h)
		}
		if s.BindKey != nil {
			sub.SigRecovery = SignSig(t, s.BindKey, ac.RoleRecoveryKey, h)
		}
		if s.PriorKey != nil {
			sub.SigRecoveryPrior = SignSig(t, s.PriorKey, ac.RoleRecoveryKey, h)
		}
	}
	p.Submit = sub
}

func (a *Account) bindAt(kind string, key []byte, escrow []byte) *ac.RecoveryBind {
	return &ac.RecoveryBind{RealmID: a.R.ID, AccountID: a.ID, Kind: kind, RecoveryKey: key, ChainSeq: a.Head.Seq,
		ChainEntryDigest: a.Head.Entry, BindingRootDigest: ac.KeyDigest(a.Head.Root), EscrowPublicKey: escrow}
}

// AddDevice builds add_device for d, authorized by the password factor.
func (a *Account) AddDevice(t testing.TB, d *Device) BuiltDevice {
	return a.BuildDevice(t, DeviceSpec{Op: ac.OpAddDevice, NewDevice: d, Factors: []string{ac.FactorPassword}})
}

// BindRecovery is a bind B (first, root-only or with the old key when prior != nil).
func (a *Account) BindRecovery(t testing.TB, key *ecdsa.PrivateKey, wait string, prior *ecdsa.PrivateKey) BuiltDevice {
	esc, _ := genKey(t)
	s := DeviceSpec{Op: ac.OpBindRecovery, Bind: a.bindAt(ac.BindKindBind, Point(key), Point(esc)), BindKey: key,
		PriorKey: prior, BindWait: wait, Factors: []string{ac.FactorEmail, ac.FactorPassword, ac.FactorTOTP}, MFAEnrolled: true}
	if prior != nil {
		s.BindWait = ""
	}
	a.Recovery = key
	return a.BuildDevice(t, s)
}

// UnbindRecovery is an unbind B, root-only or with the old key when prior != nil.
func (a *Account) UnbindRecovery(t testing.TB, wait string, prior *ecdsa.PrivateKey) BuiltDevice {
	s := DeviceSpec{Op: ac.OpBindRecovery, Bind: a.bindAt(ac.BindKindUnbind, nil, nil), PriorKey: prior, BindWait: wait,
		Factors: []string{ac.FactorEmail, ac.FactorPassword, ac.FactorTOTP}, MFAEnrolled: true}
	if prior != nil {
		s.BindWait = ""
	}
	return a.BuildDevice(t, s)
}

func (a *Account) cancelBind(target []byte, key []byte, expires uint64) *ac.RecoveryBind {
	return &ac.RecoveryBind{RealmID: a.R.ID, AccountID: a.ID, Kind: ac.BindKindCancel, ExpiresAtMs: expires,
		RecoveryKey: key, TargetDigest: target}
}

// CancelBindByRoot is the root form (LD-43): device-intent, actor, sig_root over H(B).
// B.recovery_key is the slot's key (a.Recovery).
func (a *Account) CancelBindByRoot(t testing.TB, target []byte) BuiltDevice {
	exp := a.R.Clock.Ms + 120_000
	return a.BuildDevice(t, DeviceSpec{Op: ac.OpCancelRecoveryBind, RootForm: true, ExpiresAtMs: exp,
		Bind: a.cancelBind(target, Point(a.Recovery), exp)})
}

// CancelBindBySheet is the sheet form (LD-34): no device-intent, sig_recovery under the bound key.
func (a *Account) CancelBindBySheet(t testing.TB, key *ecdsa.PrivateKey, target []byte) BuiltDevice {
	exp := a.R.Clock.Ms + 120_000
	return a.BuildDevice(t, DeviceSpec{Op: ac.OpCancelRecoveryBind, SheetForm: true, ExpiresAtMs: exp,
		Bind: a.cancelBind(target, Point(key), exp), BindKey: key})
}
