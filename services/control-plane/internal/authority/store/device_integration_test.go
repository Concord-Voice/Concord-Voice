package store

import (
	"bytes"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func (e *env) deviceOK(a *authoritytest.Account, b []byte) ac.DeviceBundle {
	e.t.Helper()
	out := e.device(b)
	if out.Kind != KindCommitted {
		e.t.Fatalf("want device-committed, got %+v", out)
	}
	dc, err := ac.DecodeDeviceCommitted(out.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	db, err := ac.DecodeDeviceBundle(dc.DeviceBundle)
	if err != nil {
		e.t.Fatal(err)
	}
	a.Sync(e.t, e.f.App)
	return db
}

func (e *env) deviceRefused(b []byte, want ac.Refusal) {
	e.t.Helper()
	if out := e.device(b); out.Kind != KindRefused || out.Code != string(want) {
		e.t.Fatalf("want refused %s, got %+v", want, out)
	}
}

func TestAddDeviceWritesOnlyThePrivateLedger(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	seq, entry := a.Head.Seq, a.Head.Entry
	var commitBefore, commitAfter int64
	_ = e.f.App.QueryRow(`SELECT max(commit_id) FROM authority.account_events`).Scan(&commitBefore)
	db := e.deviceOK(a, a.AddDevice(t, authoritytest.NewDevice(t)).Bytes)
	_ = e.f.App.QueryRow(`SELECT max(commit_id) FROM authority.account_events`).Scan(&commitAfter)
	if a.Head.Seq != seq || !bytes.Equal(a.Head.Entry, entry) || commitAfter != commitBefore || a.Head.Dseq != 2 {
		t.Fatalf("add_device moved the published head or max_commit_id (D-238): %+v", a.Head)
	}
	dr, _ := ac.DecodeDeviceResult(db.DeviceResult)
	if dr.ChainSeq != seq || !bytes.Equal(dr.ChainEntryDigest, entry) || dr.Dseq != 2 {
		t.Fatalf("device-result does not name the published head: %+v", dr)
	}
	if len(db.CPAuthz) == 0 || !bytes.Equal(dr.CPAuthzDigest, ac.Digest(db.CPAuthz)) {
		t.Fatal("a private entry must carry its own cp_authz, with #7 its digest (DoR:446, DoR:509)")
	}
	if !ac.VerifyP384(e.r.RSK.PublicKey(), db.DeviceResult, db.DeviceResultSig) {
		t.Fatal("device-result is not signed by this node's RSK")
	}
}

// B1-D33 (review round 4): /v1/device-transactions answers "503 as for
// /v1/transactions" (DoR:1599), so reconcile too.
func TestDeviceTransactionsRefuseWhileUnreconciled(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	mustExec(t, e.f.Owner, `SELECT authority.enter_unreconciled()`)
	if out := e.device(a.AddDevice(t, authoritytest.NewDevice(t)).Bytes); out.HTTP != 503 || out.Code != string(ac.ErrorReconcile) {
		t.Fatalf("add_device while unreconciled = %+v, want 503 reconcile", out)
	}
}

func TestDeviceRefusalsInStep8aOrder(t *testing.T) {
	e := newEnv(t)
	a, genesis := e.enrolledWith()
	stale := a.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpAddDevice, NewDevice: authoritytest.NewDevice(t),
		Factors: []string{ac.FactorPassword}, Mutate: func(p *authoritytest.DeviceParts) {
			if p.DeviceIntent != nil && p.Submit.DeviceIntent == nil {
				p.DeviceIntent.ExpectedDseq++
			}
		}})
	e.deviceRefused(stale.Bytes, ac.RefusalStaleDeviceSeq)
	a.Actor = authoritytest.NewDevice(t) // not in the set
	e.deviceRefused(a.AddDevice(t, authoritytest.NewDevice(t)).Bytes, ac.RefusalDeviceUnknown)
	a.Actor = genesis
	badActor := a.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpAddDevice, NewDevice: authoritytest.NewDevice(t),
		Factors: []string{ac.FactorPassword}, Mutate: func(p *authoritytest.DeviceParts) {
			if len(p.Submit.SigActor) > 0 {
				p.Submit.SigActor[3] ^= 1
			}
		}})
	if out := e.device(badActor.Bytes); out.Code != string(ac.ErrorBadSignature) {
		t.Fatalf("a bad sig_actor = %+v, want 400 bad_signature, unrecorded", out)
	}
	// (2) before (3), DoR:1449: every half of DR1 is the recorded device_unknown,
	// even with a bad sig_actor. A duplicate new device on the device path; an
	// unknown removed ID on the published path's step 8a.
	dup := a.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpAddDevice, NewDevice: genesis,
		Factors: []string{ac.FactorPassword}, Mutate: func(p *authoritytest.DeviceParts) {
			if len(p.Submit.SigActor) > 0 {
				p.Submit.SigActor[3] ^= 1
			}
		}})
	e.deviceRefused(dup.Bytes, ac.RefusalDeviceUnknown)
	removed := a.Build(t, authoritytest.Spec{Op: ac.OpRotateEK, NewEK: true, RootSigns: true, Factors: []string{ac.FactorPassword},
		PasswordLogin: true, Removed: []string{authoritytest.NewDevice(t).ID}, Mutate: func(p *authoritytest.Parts) {
			if len(p.Submit.SigActor) > 0 {
				p.Submit.SigActor[3] ^= 1
			}
		}})
	e.refusedAs(removed.Bytes, ac.RefusalDeviceUnknown)
	young := authoritytest.NewDevice(t)
	e.deviceOK(a, a.AddDevice(t, young).Bytes)
	a.Actor = young
	e.deviceRefused(a.AddDevice(t, authoritytest.NewDevice(t)).Bytes, ac.RefusalDeviceTooNew)
	e.advance(e.r.Current.D.DeltaMs - 1) // mature_at − 1 (P3 at the db_now boundary)
	e.deviceRefused(a.AddDevice(t, authoritytest.NewDevice(t)).Bytes, ac.RefusalDeviceTooNew)
	e.advance(1) // exactly mature_at
	e.deviceOK(a, a.AddDevice(t, authoritytest.NewDevice(t)).Bytes)
}

func TestTheEleventhDeviceIsDeviceLimit(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	for i := 1; i < ac.MaxDevices; i++ {
		e.deviceOK(a, a.AddDevice(t, authoritytest.NewDevice(t)).Bytes)
	}
	e.deviceRefused(a.AddDevice(t, authoritytest.NewDevice(t)).Bytes, ac.RefusalDeviceLimit)
}

func TestPrivateReplayLooksAcrossAllThreeTables(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	var published string
	_ = e.f.App.QueryRow(`SELECT request_id::text FROM authority.account_events WHERE account_id = $1`, a.ID).Scan(&published)
	reuse := a.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpAddDevice, RequestID: published,
		NewDevice: authoritytest.NewDevice(t), Factors: []string{ac.FactorPassword}})
	if out := e.device(reuse.Bytes); out.HTTP != 409 {
		t.Fatalf("a device request reusing a published request_id = %+v, want 409", out)
	}
	add := a.AddDevice(t, authoritytest.NewDevice(t))
	first := e.device(add.Bytes)
	if again := e.device(add.Bytes); !bytes.Equal(again.Body, first.Body) {
		t.Fatal("an identical device re-POST did not return the stored device-committed")
	}
}

func TestBindRecoveryNowThenCollision(t *testing.T) {
	e := newEnv(t)
	a, dev := e.enrolledWith()
	key := authoritytest.NewDevice(t).DSK // any fresh P-384 key
	e.deviceOK(a, a.BindRecovery(t, key, ac.BindWaitNow, nil).Bytes)
	var assurance string
	_ = e.f.App.QueryRow(`SELECT recovery_bind_assurance FROM authority.account_heads WHERE account_id = $1`, a.ID).Scan(&assurance)
	if assurance != ac.AssuranceInitial {
		t.Fatalf("recovery_bind_assurance = %q, want the segment's (initial) (D-273)", assurance)
	}
	b, _ := e.enrolledWith()
	e.deviceRefused(b.BindRecovery(t, key, ac.BindWaitNow, nil).Bytes, ac.RefusalRecoveryKeyBound)
	e.deviceRefused(b.BindRecovery(t, dev.DSK, ac.BindWaitNow, nil).Bytes, ac.RefusalRecoveryKeyBound) // another account's dsk
}

func TestCancelBindRootAndSheetForms(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	// DR9 (b): the root form cancels only a pending first bind (no key bound), so it runs first.
	e.deviceOK(a, a.BindRecovery(t, authoritytest.NewDevice(t).DSK, ac.BindWaitReplace, nil).Bytes)
	root := e.deviceOK(a, a.CancelBindByRoot(t, nil).Bytes)
	if set := readSet(t, e, a.ID); len(set.ReplacementBindDigest) != 0 || len(set.RecoveryKey) != 0 {
		t.Fatalf("root-form cancel: %+v, want an empty slot and no key bound", set)
	}
	if dr, _ := ac.DecodeDeviceResult(root.DeviceResult); !bytes.Equal(dr.DeviceIntentDigest, ac.Digest(root.DeviceIntent)) {
		t.Fatal("root-form device-result #6 is not D (DoR:445); A2's VerifyDeviceLedger refuses it")
	}
	bound := authoritytest.NewDevice(t).DSK
	e.deviceOK(a, a.BindRecovery(t, bound, ac.BindWaitNow, nil).Bytes)
	e.deviceOK(a, a.BindRecovery(t, authoritytest.NewDevice(t).DSK, ac.BindWaitReplace, nil).Bytes) // root-only refresh → the slot
	sheet := e.deviceOK(a, a.CancelBindBySheet(t, bound, nil).Bytes)
	if set := readSet(t, e, a.ID); len(set.ReplacementBindDigest) != 0 || !bytes.Equal(set.RecoveryKey, authoritytest.Point(bound)) {
		t.Fatalf("sheet-form cancel: %+v, want an empty slot and the bound key kept", set)
	}
	if dr, _ := ac.DecodeDeviceResult(sheet.DeviceResult); !bytes.Equal(dr.DeviceIntentDigest, ac.Digest(sheet.RecoveryBind)) {
		t.Fatal("sheet-form device-result #6 is not H(B) (DoR:445)")
	}
}

// A sheet cancel while a first bind holds the slot has no bound key to verify
// under, so it is 400 bad_signature, unrecorded (DoR:679, :1227). DR3's
// signatures run before ApplyDeviceOp's DR9 (b) (DoR:652, :786), so its
// recovery_key_unbound is never reached here. Recording it would let a request
// with no valid recovery-key signature burn its request id (Codex round 3).
func TestSheetCancelWithNoBoundKeyIsAnUnrecordedBadSignature(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	pending := authoritytest.NewDevice(t).DSK
	e.deviceOK(a, a.BindRecovery(t, pending, ac.BindWaitReplace, nil).Bytes) // a first bind → the slot, no key bound
	if set := readSet(t, e, a.ID); len(set.RecoveryKey) != 0 || len(set.ReplacementBindDigest) == 0 {
		t.Fatalf("control: %+v, want a pending first bind and no key bound", set)
	}
	before := countRows(t, e, "device_events") + countRows(t, e, "request_ledger")
	if out := e.device(a.CancelBindBySheet(t, pending, nil).Bytes); out.HTTP != 400 || out.Code != string(ac.ErrorBadSignature) {
		t.Fatalf("a sheet cancel with no key bound = %+v, want 400 bad_signature", out)
	}
	if after := countRows(t, e, "device_events") + countRows(t, e, "request_ledger"); after != before {
		t.Fatalf("an unrecorded bad_signature wrote %d rows", after-before)
	}
}

// DoR:420: device-intent #18 is H(B). A device-intent naming another B is 400
// malformed, even with every signature valid, and writes nothing.
func TestBindRecoveryRefusesADeviceIntentNamingAnotherB(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	key := authoritytest.NewDevice(t).DSK
	proto := a.BindRecovery(t, key, ac.BindWaitNow, nil) // never sent: only its B is reused
	bad := a.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpBindRecovery, Bind: proto.Parts.Bind, BindKey: key,
		BindWait: ac.BindWaitNow, Factors: []string{ac.FactorEmail, ac.FactorPassword, ac.FactorTOTP}, MFAEnrolled: true,
		Mutate: func(p *authoritytest.DeviceParts) {
			if p.DeviceIntent != nil && p.Submit.DeviceIntent == nil { // before signing: every signature covers the change
				p.DeviceIntent.RecoveryBindDigest = ac.Digest([]byte("another recovery-bind"))
			}
		}})
	before := countRows(t, e, "device_events") + countRows(t, e, "request_ledger")
	if out := e.device(bad.Bytes); out.HTTP != 400 || out.Code != string(ac.ErrorMalformed) {
		t.Fatalf("a device-intent naming another B = %+v, want 400 malformed", out)
	}
	if after := countRows(t, e, "device_events") + countRows(t, e, "request_ledger"); after != before {
		t.Fatal("a device-intent naming another B wrote a row")
	}
}

// Review round 2: B naming another request_id than device-intent #5 is 400
// malformed at decode. A2's CheckDeviceSubmit enforces it for bind and unbind
// (submitPatternHolds, E3-10); this pins that decodeDeviceSubmit goes through
// it. #18 = H(B) still holds, so only the request_id differs.
func TestBindRecoveryWhoseBNamesAnotherRequestIsMalformed(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	b := a.BindRecovery(t, authoritytest.NewDevice(t).DSK, ac.BindWaitNow, nil)
	other := *b.Parts.Bind
	other.RequestID = "44444444-4444-4444-8444-444444444444"
	ob := other.Encode()
	di := *b.Parts.DeviceIntent
	di.RecoveryBindDigest = ac.Digest(ob)
	sub := b.Parts.Submit
	sub.DeviceIntent, sub.RecoveryBind = di.Encode(), ob
	if out := e.device(sub.Encode()); out.HTTP != 400 || out.Code != string(ac.ErrorMalformed) {
		t.Fatalf("a bind whose B names another request = %+v, want 400 malformed", out)
	}
}

// Review round 2: sig_new_device is checked at step 4, under a slot, never in
// the decode. With every general slot held, an add_device with a bad
// sig_new_device is 503 busy; with one free, it is 400 bad_signature.
func TestSigNewDeviceIsCheckedUnderASlot(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	bad := a.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpAddDevice, NewDevice: authoritytest.NewDevice(t),
		Factors: []string{ac.FactorPassword}, Mutate: func(p *authoritytest.DeviceParts) {
			if len(p.Submit.SigNewDevice) > 0 {
				p.Submit.SigNewDevice[3] ^= 1
			}
		}})
	var held []func()
	for i := 0; i < MutatingSlots-1; i++ {
		rel, ok := e.s.adm.acquireMutating(classOther)
		if !ok {
			t.Fatal("could not hold a general slot")
		}
		held = append(held, rel)
	}
	if out := e.device(bad.Bytes); out.HTTP != 503 || out.Code != string(ac.ErrorBusy) {
		t.Fatalf("a bad sig_new_device with no slot free = %+v, want 503 busy (no signature before admission)", out)
	}
	held[0]()
	if out := e.device(bad.Bytes); out.HTTP != 400 || out.Code != string(ac.ErrorBadSignature) {
		t.Fatalf("a bad sig_new_device with a slot free = %+v, want 400 bad_signature", out)
	}
}

// B1-D28: one replay key over all three tables, so a published request reusing a
// private event's request_id is 409 and commits nothing.
func TestPrivateReplayKeyBlocksAPublishedRequest(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	add := a.AddDevice(t, authoritytest.NewDevice(t))
	e.deviceOK(a, add.Bytes)
	reused := a.Build(t, authoritytest.Spec{Op: ac.OpSuspend, Actor: ac.ActorUser, RequestID: add.Parts.DeviceIntent.RequestID,
		Factors: []string{ac.FactorPassword}, PasswordLogin: true})
	before := countRows(t, e, "account_events")
	if out := e.submit(reused.Bytes); out.HTTP != 409 {
		t.Fatalf("a published request reusing a private event's request_id = %+v, want 409", out)
	}
	if countRows(t, e, "account_events") != before {
		t.Fatal("the colliding published request committed an event")
	}
}

func readSet(t *testing.T, e *env, acct string) ac.DeviceSet {
	t.Helper()
	var raw []byte
	if err := e.f.App.QueryRow(`SELECT device_set FROM authority.account_heads WHERE account_id = $1`, acct).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	ds, err := ac.DecodeDeviceSet(raw)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func TestDeviceTransactionsPassTheReleaseGate(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	requireGate(t, e.f, true)
	e.s.txDL = 200 * 1e6 // 200 ms
	if out := e.device(a.AddDevice(t, authoritytest.NewDevice(t)).Bytes); out.Code != string(ac.ErrorReplication) {
		t.Fatalf("device-transaction with replication unconfirmed = %+v, want 503 replication", out)
	}
}
