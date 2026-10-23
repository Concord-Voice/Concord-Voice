package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// Task 10 (plan): reconcile/import of device bundles and of row-22 items
// (DoR:1727-1730, D-288, DoR mutation 5; B3-D16).
//
// B3-D16 for a private-op item (bundle ""): it authenticates in the pre-pass
// through its own device-result's checks (RSK signature under an installed
// delegation, T7, realm/account), so one that does not makes the WHOLE batch
// ErrBadAuthz with nothing held or imported. An authenticated item that a later
// check refuses (a dseq gap, a chain break, policy) HOLDS its account and the
// request still answers 200.
//
// Shape facts these tests rely on, all in ac.CheckImportItem (so they hold
// before and after Task 10, and some of these tests pass today as guards):
//   - a private-op item carries its device entry's non-empty cp_evidence; only a
//     restore-cancel (row 22) carries none;
//   - a published item carries its attached device entry exactly when its
//     intent has a device-intent digest, so an item stripped of its entry is a
//     malformed import-item (whole batch ErrBadAuthz), never a hold.

// ---- helpers ----

// retainedPrivate is the import item the CP would hold for the private-op
// device entry at (account, dseq): no bundle, the entry as the device_bundle,
// and the entry's own cp_evidence.
func retainedPrivate(t *testing.T, e *env, account string, dseq int) ac.ImportItem {
	t.Helper()
	var dev, evidence []byte
	if err := e.f.App.QueryRow(`SELECT bundle, cp_evidence FROM authority.device_events WHERE account_id = $1 AND dseq = $2`,
		account, dseq).Scan(&dev, &evidence); err != nil {
		t.Fatalf("retained private entry %s dseq %d: %v", account, dseq, err)
	}
	return ac.ImportItem{DeviceBundle: dev, CPEvidence: evidence}
}

// deviceView is the head's device columns, which an import of device entries
// must reproduce: dseq, device_head_digest, device_set and device_set_digest.
func deviceView(t *testing.T, e *env, account string) string {
	t.Helper()
	var s string
	if err := e.f.App.QueryRow(`SELECT dseq::text || '|' || COALESCE(encode(device_head_digest, 'hex'), '') || '|' ||
		COALESCE(encode(device_set, 'hex'), '') || '|' || COALESCE(encode(device_set_digest, 'hex'), '')
		FROM authority.account_heads WHERE account_id = $1`, account).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// deviceEventsView is "<dseq><t|f>,...": each stored device entry and whether
// it was imported.
func deviceEventsView(t *testing.T, e *env, account string) string {
	t.Helper()
	rows, err := e.f.App.Query(`SELECT dseq, imported FROM authority.device_events WHERE account_id = $1 ORDER BY dseq`, account)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var parts []string
	for rows.Next() {
		var d int
		var imp bool
		if err := rows.Scan(&d, &imp); err != nil {
			t.Fatal(err)
		}
		f := "f"
		if imp {
			f = "t"
		}
		parts = append(parts, fmt.Sprint(d, f))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(parts, ",")
}

func requireDeviceEvents(t *testing.T, e *env, account, want string) {
	t.Helper()
	if got := deviceEventsView(t, e, account); got != want {
		t.Fatalf("account %s device entries = %q, want %q", account, got, want)
	}
}

// headSansL1 is the head row but for the columns clear-held's L1 transaction
// sets after row 22 (held, force_unverified and the two floors): the part of
// the head the row-22 event itself determines.
func headSansL1(t *testing.T, e *env, account string) string {
	t.Helper()
	var s string
	if err := e.f.App.QueryRow(`SELECT (to_jsonb(h) - 'held' - 'force_unverified' - 'activation_floor_ms' - 'replacement_floor_ms')::text
		FROM authority.account_heads h WHERE account_id = $1`, account).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// rowTwentyTwoFixture builds a P head (replace_root, seq 2), takes the backup
// "pre22" while the head is NOT held, then holds the head and clear-helds it:
// row 22 is seq 3, signed while held was true (held_status.held = 1) and
// committed by the authority itself. It returns the account, the retained row-22
// item (no cp_evidence), and the post-clear head's L1-independent columns. The
// database is left at the post-clear state; callers rewind to "pre22".
func rowTwentyTwoFixture(t *testing.T, e *env) (*authoritytest.Account, ac.ImportItem, string) {
	t.Helper()
	p, _ := e.enrolledWith()
	e.commitOK(p, p.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes) // P, seq 2
	snapshotAll(t, e, "pre22")
	e.pinHeld(p.ID)
	got, err := signedOwner(t, e).ClearHeld(liveCtx(t), []string{p.ID})
	if err != nil {
		t.Fatalf("fixture: ClearHeld = %v", err)
	}
	wantResults(t, got, ClearResult{p.ID, "restore_cancel"})
	it := retained(t, e, p.ID, 3)
	if len(it.Bundle) == 0 || len(it.CPEvidence) != 0 || len(it.DeviceBundle) != 0 {
		t.Fatalf("fixture: the row-22 item = %d bundle bytes, %d evidence bytes, %d device bytes; want a bundle alone",
			len(it.Bundle), len(it.CPEvidence), len(it.DeviceBundle))
	}
	post := headSansL1(t, e, p.ID)
	e.catchUp(t) // the import's db_now must not be behind row 22's database-stamped issued_at
	return p, it, post
}

// ---- tests ----

// add_device, bind_recovery (a pending first bind, then a bound key: each
// moves the slot and the recovery floor/tier), a root-form cancel_recovery_bind
// and a published suspend, all committed after the backup, are imported in
// (account, chain_seq, dseq) order: the private entries first (chain_seq 1,
// dseq 2..5), the suspend (chain_seq 2) last. The head's dseq, device head
// digest, device set and set digest, and every other column, equal the
// pre-reset head, and exactly those four device entries are imported.
//
// Private ops used: add_device, bind_recovery (wait=replace, then wait=now),
// cancel_recovery_bind (root form). The sheet-form cancel is not used.
func TestImportDeviceLedger(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	snapshotAll(t, e, "imp")
	e.deviceOK(a, a.AddDevice(t, authoritytest.NewDevice(t)).Bytes)                                 // d2
	e.deviceOK(a, a.BindRecovery(t, authoritytest.NewDevice(t).DSK, ac.BindWaitReplace, nil).Bytes) // d3: a first bind to the slot
	e.deviceOK(a, a.CancelBindByRoot(t, nil).Bytes)                                                 // d4: root form, empties the slot
	e.deviceOK(a, a.BindRecovery(t, authoritytest.NewDevice(t).DSK, ac.BindWaitNow, nil).Bytes)     // d5: bound at once
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)                                             // seq 2
	wantHead, wantDev, wantBind := headJSON(t, e, a.ID), deviceView(t, e, a.ID), bindingsView(t, e, a.ID)
	if !strings.HasPrefix(wantDev, "5|") {
		t.Fatalf("fixture: device view %q, want dseq 5", wantDev)
	}
	items := []ac.ImportItem{retainedPrivate(t, e, a.ID, 2), retainedPrivate(t, e, a.ID, 3), retainedPrivate(t, e, a.ID, 4),
		retainedPrivate(t, e, a.ID, 5), retained(t, e, a.ID, 2)}

	rewind(t, e, "imp", a)
	if headSeq(t, e, a.ID) != 1 || !strings.HasPrefix(deviceView(t, e, a.ID), "1|") {
		t.Fatal("fixture: the restore did not move the head back to seq 1, dseq 1")
	}
	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, items...))

	requireHeld(t, e)
	requireEvents(t, e, a.ID, "1f,2t")
	requireDeviceEvents(t, e, a.ID, "1f,2t,3t,4t,5t")
	if got := deviceView(t, e, a.ID); got != wantDev {
		t.Fatalf("device columns after import =\n%s\nwant (pre-reset)\n%s", got, wantDev)
	}
	if got := headJSON(t, e, a.ID); got != wantHead {
		t.Fatalf("head after import =\n%s\nwant (pre-reset)\n%s", got, wantHead)
	}
	if got := bindingsView(t, e, a.ID); got != wantBind {
		t.Fatalf("key bindings after import = %q, want %q", got, wantBind)
	}
}

// A published item stripped of its attached device entry is not an item: its
// intent names a device-intent digest the missing entry would carry
// (ac.CheckImportItem), so the batch is ErrBadAuthz before anything is read.
// It is NOT a hold (the plan's "held, 200" cannot be reached: the shape check
// runs first). The control, the same item with its entry, imports; it passes
// today and keeps the stripped case honest.
func TestImportDeviceMissingAttachedEntryIsBadAuthz(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	snapshotAll(t, e, "imp")
	e.commitOK(a, a.RotateRoot(t).Bytes) // seq 2: an attached device entry
	full := retained(t, e, a.ID, 2)
	if len(full.DeviceBundle) == 0 {
		t.Fatal("fixture: the rotate_root item carries no attached device entry")
	}
	stripped := full
	stripped.DeviceBundle = nil

	rewind(t, e, "imp", a)
	beginImport(t, e, restorePoint{})
	requireBadAuthz(t, e, rawBatch(t, stripped))
	requireHeld(t, e)
	requireEvents(t, e, a.ID, "1f")
	requireDeviceEvents(t, e, a.ID, "1f")

	importOK(t, e, importBatch(t, full))
	requireEvents(t, e, a.ID, "1f,2t")
	requireDeviceEvents(t, e, a.ID, "1f,2t")
}

// An authenticated device entry whose dseq does not follow the head's holds its
// account (200, not a 503, not a batch refusal). d2 and d4 are the authority's
// own entries, so they authenticate; d3 is missing, so d4 breaks the chain.
// The control, d2..d4 whole, imports.
func TestImportDeviceBrokenChainHolds(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	snapshotAll(t, e, "imp")
	e.deviceOK(a, a.AddDevice(t, authoritytest.NewDevice(t)).Bytes)                                 // d2
	e.deviceOK(a, a.BindRecovery(t, authoritytest.NewDevice(t).DSK, ac.BindWaitReplace, nil).Bytes) // d3
	e.deviceOK(a, a.CancelBindByRoot(t, nil).Bytes)                                                 // d4
	wantDev := deviceView(t, e, a.ID)
	d2, d3, d4 := retainedPrivate(t, e, a.ID, 2), retainedPrivate(t, e, a.ID, 3), retainedPrivate(t, e, a.ID, 4)

	rewind(t, e, "imp", a)
	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, d2, d4))
	requireHeld(t, e, a.ID)
	if got := deviceEventsView(t, e, a.ID); strings.Contains(got, "4") {
		t.Fatalf("device entries after a gap = %q, want d4 never written", got)
	}
	if strings.HasPrefix(deviceView(t, e, a.ID), "4|") {
		t.Fatal("the head advanced to dseq 4 across a gap")
	}

	// Control: the whole run imports and holds nothing.
	rewind(t, e, "imp", a)
	importOK(t, e, importBatch(t, d2, d3, d4))
	requireHeld(t, e)
	requireDeviceEvents(t, e, a.ID, "1f,2t,3t,4t")
	if got := deviceView(t, e, a.ID); got != wantDev {
		t.Fatalf("device columns after the whole run =\n%s\nwant\n%s", got, wantDev)
	}
}

// A gap between entries that do not depend on each other: three add_device
// entries, the middle one missing. TestImportDeviceBrokenChainHolds' d4 is a
// cancel of d3's bind, so without d3 DR9 refuses it whatever the link says;
// here d4 applies to a set without d3's device, so only the link refuses it.
// The replay enforces the link three times (deviceLinks, step 8a's
// expected_dseq, and the device result's dseq and prev digest inside its
// bytes), and this is the shape on which removing all three is observable.
func TestImportDeviceGapOfIndependentEntriesHolds(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	snapshotAll(t, e, "imp")
	for range 3 {
		e.deviceOK(a, a.AddDevice(t, authoritytest.NewDevice(t)).Bytes) // d2, d3, d4
	}
	wantDev := deviceView(t, e, a.ID)
	d2, d3, d4 := retainedPrivate(t, e, a.ID, 2), retainedPrivate(t, e, a.ID, 3), retainedPrivate(t, e, a.ID, 4)

	rewind(t, e, "imp", a)
	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, d2, d4))
	requireHeld(t, e, a.ID)
	requireDeviceEvents(t, e, a.ID, "1f,2t")
	if !strings.HasPrefix(deviceView(t, e, a.ID), "2|") {
		t.Fatalf("device columns after a gap = %s, want the head left at dseq 2", deviceView(t, e, a.ID))
	}

	rewind(t, e, "imp", a)
	mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET held = false WHERE account_id = $1`, a.ID)
	importOK(t, e, importBatch(t, d2, d3, d4))
	requireHeld(t, e)
	requireDeviceEvents(t, e, a.ID, "1f,2t,3t,4t")
	if got := deviceView(t, e, a.ID); got != wantDev {
		t.Fatalf("device columns after the whole run =\n%s\nwant\n%s", got, wantDev)
	}
}

// B3-D16 for a private-op item: one whose device-result signature is flipped
// does not authenticate, so the whole batch is ErrBadAuthz with nothing held or
// imported, the good published item before it included. The control, the same
// batch with the genuine entry, imports both (it fails until Task 10: today any
// private-op item is ErrBadAuthz).
func TestImportUnauthenticatedDeviceItemHoldsNothing(t *testing.T) {
	e := newEnv(t)
	accts := make([]*authoritytest.Account, 0, 2)
	for range 2 {
		a, _ := e.enrolledWith()
		accts = append(accts, a)
	}
	sortedByID(accts)
	snapshotAll(t, e, "imp")
	e.commitOK(accts[0], accts[0].Suspend(t, ac.ActorOperator).Bytes)
	e.deviceOK(accts[1], accts[1].AddDevice(t, authoritytest.NewDevice(t)).Bytes)
	good, private := retained(t, e, accts[0].ID, 2), retainedPrivate(t, e, accts[1].ID, 2)
	rewind(t, e, "imp", accts...)

	db, err := ac.DecodeDeviceBundle(private.DeviceBundle)
	if err != nil {
		t.Fatal(err)
	}
	db.DeviceResultSig = append([]byte(nil), db.DeviceResultSig...)
	db.DeviceResultSig[len(db.DeviceResultSig)-1] ^= 1
	forged := private
	if forged.DeviceBundle = db.Encode(); forged.DeviceBundle == nil {
		t.Fatal("the broken device bundle does not encode")
	}

	beginImport(t, e, restorePoint{})
	requireBadAuthz(t, e, importBatch(t, good, forged))
	requireHeld(t, e)
	requireEvents(t, e, accts[0].ID, "1f")
	requireEvents(t, e, accts[1].ID, "1f")
	requireDeviceEvents(t, e, accts[1].ID, "1f")

	importOK(t, e, importBatch(t, good, private))
	requireHeld(t, e)
	requireEvents(t, e, accts[0].ID, "1f,2t")
	requireDeviceEvents(t, e, accts[1].ID, "1f,2t")
}

// D-288 / DoR mutation 5: row 22 is judged by its OWN held_status (held = 1,
// signed before the L1 transaction cleared held), not by the held flag of the
// head it lands on. The backup "pre22" was taken while the head was not held, so
// after the restore the head says held = false although row 22's held_status
// says 1. The row still imports, and the head equals the post-clear head in
// every column row 22 itself determines. Compared: the whole head row but for
// held, force_unverified, activation_floor_ms and replacement_floor_ms, which
// the L1 transaction sets (import never does: reconcile-complete floors).
func TestImportRowTwentyTwoByItsOwnHeldStatus(t *testing.T) {
	e := newEnv(t)
	p, it, post := rowTwentyTwoFixture(t, e)

	t.Run("the head is not held", func(t *testing.T) {
		rewind(t, e, "pre22", p)
		if c := readCleared(t, e.f.Super, p.ID); c.held || !c.pendingRoot {
			t.Fatalf("fixture: restored head %+v, want a pending root and held = false", c)
		}
		beginImport(t, e, restorePoint{})
		importOK(t, e, importBatch(t, it))
		requireHeld(t, e, p.ID) // a re-imported row 22 holds its account, so clear-held re-runs its L1 half (B3-D18)
		requireEvents(t, e, p.ID, "1f,2f,3t")
		if got := headSansL1(t, e, p.ID); got != post {
			t.Fatalf("head after importing row 22 =\n%s\nwant (post-clear, but for the L1 columns)\n%s", got, post)
		}
		if c := readCleared(t, e.f.Super, p.ID); c.pendingRoot {
			t.Fatalf("head after importing row 22 = %+v, want the pending root cancelled", c)
		}
	})

	t.Run("the head is held", func(t *testing.T) {
		rewind(t, e, "pre22", p)
		e.pinHeld(p.ID)
		beginImport(t, e, restorePoint{})
		importOK(t, e, importBatch(t, it))
		requireEvents(t, e, p.ID, "1f,2f,3t")
		requireHeld(t, e, p.ID) // import replays results and never clears held
		if got := headSansL1(t, e, p.ID); got != post {
			t.Fatalf("held head after importing row 22 =\n%s\nwant\n%s", got, post)
		}
	})
}

// A row-22 item's cp_evidence is "" and every other item's is not
// (ac.CheckImportItem), so a violation in either direction is a malformed item:
// the whole batch is ErrBadAuthz, never a hold, because the shape check runs
// before authentication. (The plan expected a hold: the evidence is outside
// AuthenticateBundle, but not outside the batch decode.) The control, row 22 as
// retained, imports; the mutated cases pass today, the control does not.
func TestImportRowTwentyTwoNeedsEmptyEvidence(t *testing.T) {
	t.Run("row 22 with evidence", func(t *testing.T) {
		e := newEnv(t)
		p, it, _ := rowTwentyTwoFixture(t, e)
		rewind(t, e, "pre22", p)
		beginImport(t, e, restorePoint{})

		withEvidence := it
		withEvidence.CPEvidence = []byte("not empty")
		requireBadAuthz(t, e, rawBatch(t, withEvidence))
		requireHeld(t, e)
		requireEvents(t, e, p.ID, "1f,2f")

		importOK(t, e, importBatch(t, it)) // control
		requireEvents(t, e, p.ID, "1f,2f,3t")
	})

	t.Run("an ordinary item without evidence", func(t *testing.T) {
		e := newEnv(t)
		a, it := oneAhead(t, e)
		if len(it.CPEvidence) == 0 {
			t.Fatal("fixture: the retained suspend carries no cp_evidence")
		}
		beginImport(t, e, restorePoint{})

		bare := it
		bare.CPEvidence = nil
		requireBadAuthz(t, e, rawBatch(t, bare))
		requireHeld(t, e)
		requireEvents(t, e, a.ID, "1f")

		importOK(t, e, importBatch(t, it)) // control
		requireEvents(t, e, a.ID, "1f,2t")
	})
}

// RT6-8: a private entry is replayed through the commit core's policy
// (policy.AdmitPrivate) over its cp_evidence, not taken on its signature. The
// forger re-signs a genuine entry over evidence with no factors: the cp-authz
// by the realm's CAK, the device result by the realm's RSK, every digest
// bound, so it authenticates and links, and the device result is the one the
// commit core would build (the set does not depend on the evidence). Only the
// policy replay refuses it: add_device's floor, and bind_recovery's floor H
// and BindTier for an immediate bind. The control, the same forger leaving the
// evidence as it was, imports.
func TestImportDevicePolicyReplayRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(t *testing.T, a *authoritytest.Account) []byte
	}{
		{"add_device", func(t *testing.T, a *authoritytest.Account) []byte {
			return a.AddDevice(t, authoritytest.NewDevice(t)).Bytes
		}},
		{"bind_recovery, bound at once", func(t *testing.T, a *authoritytest.Account) []byte {
			return a.BindRecovery(t, authoritytest.NewDevice(t).DSK, ac.BindWaitNow, nil).Bytes
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			a, _ := e.enrolledWith()
			snapshotAll(t, e, "imp")
			e.deviceOK(a, tc.do(t, a)) // d2
			it := retainedPrivate(t, e, a.ID, 2)
			rewind(t, e, "imp", a)
			beginImport(t, e, restorePoint{})

			bare := forgeDeviceEvidence(t, e, it, func(ev *ac.CPEvidence) { ev.Factors = []string{} })
			importOK(t, e, importBatch(t, bare))
			requireHeld(t, e, a.ID)
			requireDeviceEvents(t, e, a.ID, "1f")

			rewind(t, e, "imp", a)
			mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET held = false WHERE account_id = $1`, a.ID)
			same := forgeDeviceEvidence(t, e, it, func(*ac.CPEvidence) {})
			importOK(t, e, importBatch(t, same))
			requireHeld(t, e)
			requireDeviceEvents(t, e, a.ID, "1f,2t")
		})
	}
}

// forgeDeviceEvidence is forgeEvidence for a private entry: a fresh evidence
// salt, the cp-authz re-signed by the realm's CAK over the new evidence
// digest, and the device result re-signed by the realm's RSK over the new
// cp-authz digest.
func forgeDeviceEvidence(t *testing.T, e *env, it ac.ImportItem, mutate func(*ac.CPEvidence)) ac.ImportItem {
	t.Helper()
	db, err := ac.DecodeDeviceBundle(it.DeviceBundle)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := ac.DecodeCPEvidence(it.CPEvidence)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&ev)
	ev.EvidenceSalt = append([]byte(nil), ev.EvidenceSalt...)
	ev.EvidenceSalt[0] ^= 1
	eb := ev.Encode()
	az, err := ac.DecodeCPAuthz(db.CPAuthz)
	if err != nil || eb == nil {
		t.Fatalf("forge: cp-authz %v, evidence encodes %v", err, eb != nil)
	}
	az.EvidenceDigest = ac.Digest(eb)
	db.CPAuthz = az.Encode()
	db.CPAuthzSig = authoritytest.SignRaw(t, e.r.CAK, db.CPAuthz)
	dr, err := ac.DecodeDeviceResult(db.DeviceResult)
	if err != nil {
		t.Fatal(err)
	}
	dr.CPAuthzDigest = ac.Digest(db.CPAuthz)
	db.DeviceResult = dr.Encode()
	if db.DeviceResultSig, err = e.r.RSK.Sign(db.DeviceResult); err != nil {
		t.Fatal(err)
	}
	out := db.Encode()
	if out == nil || db.DeviceResult == nil {
		t.Fatal("forge: the device bundle does not encode")
	}
	return ac.ImportItem{DeviceBundle: out, CPEvidence: eb}
}

// The live path's db_now gates that are not policy, replayed at the result's
// issued_at on both replay paths (the parity review of 15973d4e7): the cp-authz
// window (steps 3–4), D-271's recover guard and step 9's issued_at order. Live,
// the guard and the order are 503s; replayed, each holds the account, and the
// window is a refusal, never a 503. Each case holds; its control, the same
// item with no setup (or the same forger leaving the cp-authz as it was),
// imports.
func TestImportReplaysTheLiveDBNowGates(t *testing.T) {
	device := func(t *testing.T, e *env, a *authoritytest.Account) ac.ImportItem {
		e.deviceOK(a, a.AddDevice(t, authoritytest.NewDevice(t)).Bytes)
		return retainedPrivate(t, e, a.ID, 2)
	}
	published := func(t *testing.T, e *env, a *authoritytest.Account) ac.ImportItem {
		e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)
		return retained(t, e, a.ID, 2)
	}
	guard := func(t *testing.T, e *env, a *authoritytest.Account) {
		mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET recover_guard_until_ms = 9007199254740991 WHERE account_id = $1`, a.ID)
	}
	newerHead := func(t *testing.T, e *env, a *authoritytest.Account) {
		mustExec(t, e.f.Super, `UPDATE authority.device_events SET issued_at_ms = issued_at_ms + 600000 WHERE account_id = $1 AND dseq = 1`, a.ID)
	}
	for _, tc := range []struct {
		name  string
		item  func(*testing.T, *env, *authoritytest.Account) ac.ImportItem
		setup func(*testing.T, *env, *authoritytest.Account)
		stale bool
	}{
		{"device: the recover guard is armed", device, guard, false},
		{"published: the recover guard is armed", published, guard, false},
		{"device: a stored private entry is newer", device, newerHead, false},
		{"published: a stored private entry is newer", published, newerHead, false},
		{"device: the cp-authz is outside the window", device, nil, true},
		{"published: the cp-authz is outside the window", published, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			a, _ := e.enrolledWith()
			snapshotAll(t, e, "imp")
			it := tc.item(t, e, a)
			forge := func(refused bool) ac.ImportItem {
				if !tc.stale {
					return it
				}
				mut := func(*ac.CPAuthz) {}
				if refused {
					mut = windowEdge(t, it)
				}
				return forgeAuthz(t, e, it, mut)
			}
			rewind(t, e, "imp", a)
			beginImport(t, e, restorePoint{})
			if tc.setup != nil {
				tc.setup(t, e, a)
			}
			importOK(t, e, importBatch(t, forge(true)))
			requireHeld(t, e, a.ID)
			requireEvents(t, e, a.ID, "1f")
			requireDeviceEvents(t, e, a.ID, "1f")

			rewind(t, e, "imp", a)
			mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET held = false WHERE account_id = $1`, a.ID)
			importOK(t, e, importBatch(t, forge(false)))
			requireHeld(t, e)
		})
	}
}

// windowEdge moves the item's cp-authz to the window's lower edge: asserted_at =
// issued_at − INTENT_WINDOW_MS, which step 3 refuses (a ≤ now − window).
func windowEdge(t *testing.T, it ac.ImportItem) func(*ac.CPAuthz) {
	t.Helper()
	var issued uint64
	if len(it.Bundle) == 0 {
		db, err := ac.DecodeDeviceBundle(it.DeviceBundle)
		if err != nil {
			t.Fatal(err)
		}
		dr, err := ac.DecodeDeviceResult(db.DeviceResult)
		if err != nil {
			t.Fatal(err)
		}
		issued = dr.IssuedAtMs
	} else {
		b, err := ac.DecodeBundle(it.Bundle)
		if err != nil {
			t.Fatal(err)
		}
		res, err := ac.DecodeResult(b.Result)
		if err != nil {
			t.Fatal(err)
		}
		issued = res.IssuedAtMs
	}
	return func(az *ac.CPAuthz) { az.AssertedAtMs = issued - ac.IntentWindowMs }
}

// forgeAuthz re-signs the item over the cp-authz mutate leaves: the cp-authz by
// the realm's CAK, and the (device) result by the realm's RSK over its new
// digest. Every signature verifies and every digest binds.
func forgeAuthz(t *testing.T, e *env, it ac.ImportItem, mutate func(*ac.CPAuthz)) ac.ImportItem {
	t.Helper()
	if len(it.Bundle) == 0 {
		db, err := ac.DecodeDeviceBundle(it.DeviceBundle)
		if err != nil {
			t.Fatal(err)
		}
		az, err := ac.DecodeCPAuthz(db.CPAuthz)
		if err != nil {
			t.Fatal(err)
		}
		mutate(&az)
		db.CPAuthz = az.Encode()
		db.CPAuthzSig = authoritytest.SignRaw(t, e.r.CAK, db.CPAuthz)
		dr, err := ac.DecodeDeviceResult(db.DeviceResult)
		if err != nil {
			t.Fatal(err)
		}
		dr.CPAuthzDigest = ac.Digest(db.CPAuthz)
		db.DeviceResult = dr.Encode()
		if db.DeviceResultSig, err = e.r.RSK.Sign(db.DeviceResult); err != nil {
			t.Fatal(err)
		}
		return ac.ImportItem{DeviceBundle: db.Encode(), CPEvidence: it.CPEvidence}
	}
	b, err := ac.DecodeBundle(it.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	az, err := ac.DecodeCPAuthz(b.CPAuthz)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&az)
	b.CPAuthz = az.Encode()
	b.CPAuthzSig = authoritytest.SignRaw(t, e.r.CAK, b.CPAuthz)
	res, err := ac.DecodeResult(b.Result)
	if err != nil {
		t.Fatal(err)
	}
	res.CPAuthzDigest = ac.Digest(b.CPAuthz)
	b.Result = res.Encode()
	if b.ResultSig, err = e.r.RSK.Sign(b.Result); err != nil {
		t.Fatal(err)
	}
	return ac.ImportItem{Bundle: b.Encode(), DeviceBundle: it.DeviceBundle, CPEvidence: it.CPEvidence}
}
