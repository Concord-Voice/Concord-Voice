package policy

import (
	"testing"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func devIntent(h Head, op string) *ac.DeviceIntent {
	return &ac.DeviceIntent{RealmID: realm, AccountID: acct, RequestID: req, Op: op, ExpectedSeq: h.Pub.Seq,
		ExpectedHeadDigest: h.Pub.Entry}
}

func TestAddDevicePolicyOrder(t *testing.T) {
	sOK := ac.Evidence{Factors: []string{ac.FactorPassword}, PasswordLogin: true}
	h := activeHead()
	pend := h
	pend.Pub.State, pend.Pub.Prior, pend.Pub.PendRoot, pend.Pub.Op = ac.StatePending, ac.StateActive, pt(9), ac.OpReplaceRoot
	fu := h
	fu.ForceUnverified = true
	stale := devIntent(h, ac.OpAddDevice)
	stale.ExpectedSeq++
	cases := []struct {
		name string
		in   PrivateInput
		want ac.Refusal
	}{
		{"pending head", PrivateInput{Head: pend, Op: ac.OpAddDevice, DeviceIntent: devIntent(pend, ac.OpAddDevice), Evidence: sOK, Scope: allScope()}, ac.RefusalWrongState},
		{"force_unverified", PrivateInput{Head: fu, Op: ac.OpAddDevice, DeviceIntent: devIntent(fu, ac.OpAddDevice), Evidence: sOK, Scope: allScope()}, ac.RefusalWrongState},
		{"stale seq", PrivateInput{Head: h, Op: ac.OpAddDevice, DeviceIntent: stale, Evidence: sOK, Scope: allScope()}, ac.RefusalStaleSeq},
		{"floor", PrivateInput{Head: h, Op: ac.OpAddDevice, DeviceIntent: devIntent(h, ac.OpAddDevice), Evidence: ac.Evidence{Factors: []string{ac.FactorBackupCode}, MFAEnrolled: true, PasswordLogin: true}, Scope: allScope()}, ac.RefusalInsufficientAuthorization},
		{"scope", PrivateInput{Head: h, Op: ac.OpAddDevice, DeviceIntent: devIntent(h, ac.OpAddDevice), Evidence: sOK, Scope: []string{ac.ScopeStatus}}, ac.RefusalPolicyDisabled},
		{"admitted", PrivateInput{Head: h, Op: ac.OpAddDevice, DeviceIntent: devIntent(h, ac.OpAddDevice), Evidence: sOK, Scope: allScope()}, ""},
	}
	for _, c := range cases {
		if got := AdmitPrivate(c.in); got != c.want {
			t.Fatalf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBindRecoveryTierAndBindingPosition(t *testing.T) {
	h := activeHead()
	b := &ac.RecoveryBind{Kind: ac.BindKindBind, ChainSeq: h.Pub.Seq, ChainEntryDigest: h.Pub.Entry, BindingRootDigest: ac.KeyDigest(h.Pub.Root)}
	ev := ac.Evidence{Factors: []string{ac.FactorPassword}, PasswordLogin: true}
	di := devIntent(h, ac.OpBindRecovery)
	di.BindWait = ac.BindWaitNow
	if got := AdmitPrivate(PrivateInput{Head: h, Op: ac.OpBindRecovery, DeviceIntent: di, Bind: b, Evidence: ev, Scope: allScope()}); got != ac.RefusalInsufficientAuthorization {
		t.Fatalf("bind now with {password} = %q, want insufficient_authorization (tier is replace)", got)
	}
	di.BindWait = ac.BindWaitReplace
	if got := AdmitPrivate(PrivateInput{Head: h, Op: ac.OpBindRecovery, DeviceIntent: di, Bind: b, Evidence: ev, Scope: allScope()}); got != "" {
		t.Fatalf("bind replace with {password} = %q, want admitted", got)
	}
	bad := *b
	bad.ChainSeq--
	if got := AdmitPrivate(PrivateInput{Head: h, Op: ac.OpBindRecovery, DeviceIntent: di, Bind: &bad, Evidence: ev, Scope: allScope()}); got != ac.RefusalStaleSeq {
		t.Fatalf("B.chain_seq behind the head = %q, want stale_seq", got)
	}
	bad = *b
	bad.BindingRootDigest = ac.Digest([]byte("x"))
	if got := AdmitPrivate(PrivateInput{Head: h, Op: ac.OpBindRecovery, DeviceIntent: di, Bind: &bad, Evidence: ev, Scope: allScope()}); got != ac.RefusalWrongState {
		t.Fatalf("B.binding_root_digest not the active root = %q, want wrong_state (B1-D15)", got)
	}
	prior := ac.Evidence{Factors: []string{ac.FactorPassword}, PasswordLogin: true}
	di.BindWait = ""
	if got := AdmitPrivate(PrivateInput{Head: h, Op: ac.OpBindRecovery, DeviceIntent: di, Bind: b, PriorSig: true, Evidence: prior, Scope: allScope()}); got != "" {
		t.Fatalf("bind with the old key under H = %q, want admitted (no tier on the old-key form)", got)
	}
}

func TestSheetCancelNeedsNoFloorAndRootCancelChecksTheHead(t *testing.T) {
	h := activeHead()
	if got := AdmitPrivate(PrivateInput{Head: h, Op: ac.OpCancelRecoveryBind, SheetForm: true, Scope: allScope()}); got != "" {
		t.Fatalf("sheet cancel = %q, want admitted (DR9 b is the store's)", got)
	}
	di := devIntent(h, ac.OpCancelRecoveryBind)
	di.ExpectedHeadDigest = ac.Digest([]byte("x"))
	if got := AdmitPrivate(PrivateInput{Head: h, Op: ac.OpCancelRecoveryBind, RootForm: true, DeviceIntent: di, Scope: allScope()}); got != ac.RefusalStaleHead {
		t.Fatalf("root cancel with a stale #10 = %q, want stale_head", got)
	}
}
