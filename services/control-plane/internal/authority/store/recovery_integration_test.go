package store

import (
	"bytes"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// bound binds a recovery key at once and returns the B bytes and sig_root
// a later recover must carry (LD-45).
func bound(t *testing.T, e *env, a *authoritytest.Account) ([]byte, []byte) {
	t.Helper()
	b := a.BindRecovery(t, authoritytest.NewDevice(t).DSK, ac.BindWaitNow, nil)
	e.deviceOK(a, b.Bytes)
	return b.BindBytes, b.Parts.Submit.SigRoot
}

func TestRecoverCommitsWithTheBucketEmptyAndIsRecoveryAssured(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	bb, sig := bound(t, e, a)
	e.drainBucket()
	r := e.commitOK(a, a.Recover(t, bb, sig).Bytes)
	if r.State != ac.StatePending || r.Assurance != ac.AssuranceRecovery {
		t.Fatalf("recover: %s/%s, want Pr with recovery (min(recovery, initial))", r.State, r.Assurance)
	}
	e.commitOK(a, a.CancelByRecovery(t).Bytes) // row 21: only the recovery key ends it
	if set := readSet(t, e, a.ID); set.Pending != "" || len(set.RecoveryKey) == 0 {
		t.Fatalf("row 21 left the set %+v: want pending := \"\" and the bound key kept (DR8)", set)
	}
}

func TestRecoverGuard(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	bb, sig := bound(t, e, a)
	k0 := a.Recovery // the bound key; the guard arms only for a recover under it (DoR:687)
	refresh := authoritytest.NewDevice(t).DSK
	e.deviceOK(a, a.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpBindRecovery, BindWait: ac.BindWaitReplace,
		Bind: &ac.RecoveryBind{RealmID: e.r.ID, AccountID: a.ID, Kind: ac.BindKindBind, RecoveryKey: authoritytest.Point(refresh),
			ChainSeq: a.Head.Seq, ChainEntryDigest: a.Head.Entry, BindingRootDigest: ac.KeyDigest(a.Head.Root),
			EscrowPublicKey: authoritytest.Point(authoritytest.NewDevice(t).DSK)}, BindKey: refresh,
		Factors: []string{ac.FactorPassword}}).Bytes) // a pending refresh in the slot
	pre := a.Head                                   // both stale recovers are built on this head
	stale := a.Recover(t, bb, sig)                  // built on the head published now
	e.commitOK(a, a.Suspend(t, ac.ActorUser).Bytes) // the head moves after asserted − ε
	e.refusedAs(stale.Bytes, ac.RefusalStaleSeq)
	var until int64
	_ = e.f.App.QueryRow(`SELECT recover_guard_until_ms FROM authority.account_heads WHERE account_id = $1`, a.ID).Scan(&until)
	if until != i64(e.now.Load()+e.r.Current.D.GMs) {
		t.Fatalf("guard = %d, want db_now + g_ms", until)
	}
	e.errorAs(a.Unsuspend(t, ac.ActorUser).Bytes, 503, ac.ErrorBusy)
	if out := e.device(a.BindRecovery(t, authoritytest.NewDevice(t).DSK, ac.BindWaitNow, nil).Bytes); out.Code != string(ac.ErrorBusy) {
		t.Fatalf("bind_recovery under the guard = %+v, want 503 busy", out)
	}
	// Both cancel_recovery_bind forms pass the guard (RB3-10). A key is bound, so DR9 (b)
	// refuses the root form wrong_state (DoR:658) rather than 503 busy, and the sheet form
	// under the bound key cancels the pending refresh.
	a.Recovery = refresh // the slot's key, so the root form's only fault is the bound key
	e.deviceRefused(a.CancelBindByRoot(t, nil).Bytes, ac.RefusalWrongState)
	e.deviceOK(a, a.CancelBindBySheet(t, k0, nil).Bytes)
	e.advance(1)
	a.Recovery = k0
	cur := a.Head
	a.Head = pre
	again := a.Recover(t, bb, sig) // stale again, under the bound key, while armed
	a.Head = cur
	e.refusedAs(again.Bytes, ac.RefusalStaleSeq)
	_ = e.f.App.QueryRow(`SELECT recover_guard_until_ms FROM authority.account_heads WHERE account_id = $1`, a.ID).Scan(&until)
	if until != i64(e.now.Load()-1+e.r.Current.D.GMs) {
		t.Fatal("a stale refusal while armed extended the guard (RB3-1)")
	}
}

func TestRecoverGuardArmsNothingForAnUnboundKeyOrASupersededHead(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	bound(t, e, a)
	strangerKey := authoritytest.NewDevice(t).DSK
	esc := authoritytest.Point(authoritytest.NewDevice(t).DSK)
	b := ac.RecoveryBind{RealmID: e.r.ID, AccountID: a.ID, RequestID: "66666666-6666-4666-8666-666666666666", Kind: ac.BindKindBind,
		RecoveryKey: authoritytest.Point(strangerKey), ChainSeq: a.Head.Seq, ChainEntryDigest: a.Head.Entry,
		BindingRootDigest: ac.KeyDigest(a.Head.Root), EscrowPublicKey: esc}.Encode()
	a.Recovery = strangerKey
	unbound := a.Recover(t, b, make([]byte, 96))
	e.commitOK(a, a.Suspend(t, ac.ActorUser).Bytes)
	e.refusedAs(unbound.Bytes, ac.RefusalStaleSeq)
	var until any
	_ = e.f.App.QueryRow(`SELECT recover_guard_until_ms FROM authority.account_heads WHERE account_id = $1`, a.ID).Scan(&until)
	if until != nil {
		t.Fatal("a stale recover under an unbound key armed the guard")
	}

	c, _ := e.enrolledWith()
	cb, csig := bound(t, e, c)
	saved := c.Head
	e.commitOK(c, c.Suspend(t, ac.ActorUser).Bytes) // supersedes `saved` at T
	e.advance(ac.EpsilonMs + 1)
	cur := c.Head
	c.Head = saved
	late := c.Recover(t, cb, csig) // asserted_at > T + ε: it names no published head
	c.Head = cur
	e.refusedAs(late.Bytes, ac.RefusalStaleSeq)
	until = nil
	_ = e.f.App.QueryRow(`SELECT recover_guard_until_ms FROM authority.account_heads WHERE account_id = $1`, c.ID).Scan(&until)
	if until != nil {
		t.Fatal("a recover built on a head superseded before asserted_at − ε armed the guard (RT6-2)")
	}
}

func TestSlotCompletionExtensionHeldFloorAndRotateRoot(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	bound(t, e, a)
	issued := e.now.Load()
	refresh := authoritytest.NewDevice(t).DSK
	e.deviceOK(a, a.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpBindRecovery, BindWait: ac.BindWaitDelta,
		Bind: &ac.RecoveryBind{RealmID: e.r.ID, AccountID: a.ID, Kind: ac.BindKindBind,
			RecoveryKey: authoritytest.Point(refresh), ChainSeq: a.Head.Seq, ChainEntryDigest: a.Head.Entry,
			BindingRootDigest: ac.KeyDigest(a.Head.Root), EscrowPublicKey: authoritytest.Point(authoritytest.NewDevice(t).DSK)},
		BindKey: refresh, Factors: []string{ac.FactorEmail, ac.FactorPassword}}).Bytes) // tier delta
	growth := uint64(5_000)
	mustExec(t, e.f.App, `UPDATE authority.meta SET cancel_unavailable_ms = cancel_unavailable_ms + $1`, int64(growth))
	completes := issued + e.r.Current.D.DeltaMs + e.r.Current.D.GMs + growth
	raw := func() []byte {
		var d []byte
		_ = e.f.App.QueryRow(`SELECT device_set_digest FROM authority.account_heads WHERE account_id = $1`, a.ID).Scan(&d)
		return d
	}
	completed := func() bool { _, dst, _ := readHead(t, e, a.ID); return !bytes.Equal(dst.DeviceSetDigest, raw()) }
	e.now.Store(completes - 1)
	if completed() {
		t.Fatal("the slot completed before nb + g + growth (RT6-9)")
	}
	e.pinHeld(a.ID)
	e.now.Store(completes)
	if completed() {
		t.Fatal("a held account's slot completed (D-286)")
	}
	floor := completes + GPrimeMs
	mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET held = false, replacement_floor_ms = $2 WHERE account_id = $1`, a.ID, i64(floor))
	if completed() {
		t.Fatal("the slot completed before the post-hold floor now + G′")
	}
	e.now.Store(floor)
	if !completed() {
		t.Fatal("the slot did not complete at the floor")
	}

	e.r.Clock.Ms = e.now.Load() // the Store calls above moved db_now only; builders read the realm clock
	b, _ := e.enrolledWith()
	key := authoritytest.NewDevice(t).DSK
	e.deviceOK(b, b.BindRecovery(t, key, ac.BindWaitNow, nil).Bytes)
	refreshB := authoritytest.NewDevice(t).DSK
	e.deviceOK(b, b.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpBindRecovery, BindWait: ac.BindWaitReplace,
		Bind: &ac.RecoveryBind{RealmID: e.r.ID, AccountID: b.ID, Kind: ac.BindKindBind,
			RecoveryKey: authoritytest.Point(refreshB), ChainSeq: b.Head.Seq, ChainEntryDigest: b.Head.Entry,
			BindingRootDigest: ac.KeyDigest(b.Head.Root), EscrowPublicKey: authoritytest.Point(authoritytest.NewDevice(t).DSK)},
		BindKey: refreshB, Factors: []string{ac.FactorPassword}}).Bytes)
	e.commitOK(b, b.RotateRoot(t).Bytes)
	set := readSet(t, e, b.ID)
	if len(set.ReplacementBindDigest) != 0 || !bytes.Equal(set.RecoveryKey, authoritytest.Point(key)) {
		t.Fatalf("rotate_root: %+v, want the slot emptied and the bound key kept (LD-52)", set)
	}
}
