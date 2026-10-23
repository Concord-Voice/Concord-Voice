package store

import (
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// The refusals of the device import replay (importdevice.go) that hold an
// account. The held outcome carries no reason, so each row also replays its
// batch through replayWhy, which runs the importer's own per-item sequence and
// returns the reason the importer would have held the account for: a row that
// is refused by some earlier check than the one it names fails on the text.

// replayWhy replays items (one account's, in batch order) exactly as
// importer.account does, in a transaction it rolls back, and returns the reason
// of the first item the replay refuses ("" when none is). Nothing it does is
// visible afterwards, so the caller can still run the real ReconcileImport.
func replayWhy(t *testing.T, e *env, items ...ac.ImportItem) string {
	t.Helper()
	ctx := liveCtx(t)
	conn, err := e.s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	trust, accepted, err := e.s.trustFor(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	hw := trust.HighWater()
	groups, err := authenticateItems(trust, hw, items)
	if err != nil || len(groups) != 1 {
		t.Fatalf("replayWhy wants one authenticated account: %d groups, %v", len(groups), err)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := setLocalTimeouts(ctx, tx); err != nil {
		t.Fatal(err)
	}
	imp := importer{s: e.s, trust: trust, hw: hw, accepted: accepted}
	h, inPop, err := lockHead(ctx, tx, groups[0].account)
	if err != nil {
		t.Fatal(err)
	}
	st := &txState{tx: tx, h: h, inPop: inPop, trust: trust, accepted: accepted, replay: true}
	im, err := imp.readMeta(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	prev, err := chainHeadOf(ctx, tx, trust, hw, h)
	if err != nil {
		t.Fatalf("replayWhy: the fold: %v", err)
	}
	for _, it := range groups[0].items {
		next, why, err := imp.replayItem(ctx, st, prev, it, im)
		if err != nil {
			t.Fatalf("replayWhy: %v", err)
		}
		if why != "" {
			return why
		}
		prev = next
	}
	return ""
}

// issuedOfDevice is the issued_at of a private-op item's device result.
func issuedOfDevice(t *testing.T, it ac.ImportItem) uint64 {
	t.Helper()
	db, err := ac.DecodeDeviceBundle(it.DeviceBundle)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := ac.DecodeDeviceResult(db.DeviceResult)
	if err != nil {
		t.Fatal(err)
	}
	return dr.IssuedAtMs
}

// reencodeDevice puts db back into an item: the bundle as the fields stand
// (the device result and its signature included, as the caller left them).
func reencodeDevice(t *testing.T, it ac.ImportItem, db ac.DeviceBundle) ac.ImportItem {
	t.Helper()
	out := db.Encode()
	if out == nil {
		t.Fatal("forge: the device bundle does not encode")
	}
	return ac.ImportItem{DeviceBundle: out, CPEvidence: it.CPEvidence}
}

// flipDeviceBundle is the item with mutate applied to its device bundle's
// request parts and nothing re-signed: the device result still names the same
// digests, so the entry authenticates (only the result is signed) and any
// refusal is the replay's.
func flipDeviceBundle(t *testing.T, it ac.ImportItem, mutate func(*ac.DeviceBundle)) ac.ImportItem {
	t.Helper()
	db, err := ac.DecodeDeviceBundle(it.DeviceBundle)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&db)
	return reencodeDevice(t, it, db)
}

func flipped(b []byte) []byte {
	out := append([]byte(nil), b...)
	out[len(out)-1] ^= 1
	return out
}

// forgeDeviceResultOf re-signs the item over a device result mutate changed:
// every signature verifies and every digest in the entry binds, so the entry
// authenticates; only the replay can refuse it.
func forgeDeviceResultOf(t *testing.T, e *env, it ac.ImportItem, mutate func(*ac.DeviceResult)) ac.ImportItem {
	t.Helper()
	db, err := ac.DecodeDeviceBundle(it.DeviceBundle)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := ac.DecodeDeviceResult(db.DeviceResult)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&dr)
	if db.DeviceResult = dr.Encode(); db.DeviceResult == nil {
		t.Fatal("forge: the device result does not encode")
	}
	if db.DeviceResultSig, err = e.r.RSK.Sign(db.DeviceResult); err != nil {
		t.Fatal(err)
	}
	return reencodeDevice(t, it, db)
}

// reforgeDeviceIntent is the entry for a device-intent mutate changed, with
// everything that depends on D redone: the actor's, the root's and the new
// device's signatures over it, the cp-authz bound to it and signed by the
// realm's CAK, and the device result naming it, signed by the realm's RSK. nd
// is the device an add_device adds (nil on a bind). The entry authenticates and
// binds; what the replay makes of the device-intent is the replay's.
func reforgeDeviceIntent(t *testing.T, e *env, a *authoritytest.Account, nd *authoritytest.Device, it ac.ImportItem,
	mutate func(*ac.DeviceIntent)) ac.ImportItem {
	t.Helper()
	db, err := ac.DecodeDeviceBundle(it.DeviceBundle)
	if err != nil {
		t.Fatal(err)
	}
	di, err := ac.DecodeDeviceIntent(db.DeviceIntent)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&di)
	if db.DeviceIntent = di.Encode(); db.DeviceIntent == nil {
		t.Fatal("forge: the device-intent does not encode")
	}
	d := ac.Digest(db.DeviceIntent)
	db.SigActor = authoritytest.SignSig(t, a.Actor.DSK, ac.RoleActorDevice, d)
	if di.Op == ac.OpAddDevice {
		db.SigRoot = authoritytest.SignSig(t, a.Root, ac.RoleCurrentRoot, d)
		db.SigNewDevice = authoritytest.SignSig(t, nd.DSK, ac.RoleNewDevice, d)
	}
	az, err := ac.DecodeCPAuthz(db.CPAuthz)
	if err != nil {
		t.Fatal(err)
	}
	az.IntentDigest = d
	db.CPAuthz = az.Encode()
	db.CPAuthzSig = authoritytest.SignRaw(t, e.r.CAK, db.CPAuthz)
	dr, err := ac.DecodeDeviceResult(db.DeviceResult)
	if err != nil {
		t.Fatal(err)
	}
	dr.DeviceIntentDigest, dr.CPAuthzDigest = d, ac.Digest(db.CPAuthz)
	if db.DeviceResult = dr.Encode(); db.DeviceResult == nil {
		t.Fatal("forge: the device result does not encode")
	}
	if db.DeviceResultSig, err = e.r.RSK.Sign(db.DeviceResult); err != nil {
		t.Fatal(err)
	}
	return reencodeDevice(t, it, db)
}

// TestImportDeviceReplayRefusals: each way a device entry that authenticates
// can still be refused by the replay holds exactly its account, with the reason
// the replay names, and writes no event.
func TestImportDeviceReplayRefusals(t *testing.T) {
	type fixture struct {
		items []ac.ImportItem // the batch under test, for the account
		early []ac.ImportItem // imported (and required clean) before it
		late  func()          // run once the account is rewound
	}
	var nd *authoritytest.Device
	addDevice := func(t *testing.T, e *env, a *authoritytest.Account) ac.ImportItem {
		nd = authoritytest.NewDevice(t)
		e.deviceOK(a, a.AddDevice(t, nd).Bytes)
		return retainedPrivate(t, e, a.ID, 2)
	}
	bindRecovery := func(t *testing.T, e *env, a *authoritytest.Account) ac.ImportItem {
		e.deviceOK(a, a.BindRecovery(t, authoritytest.NewDevice(t).DSK, ac.BindWaitReplace, nil).Bytes)
		return retainedPrivate(t, e, a.ID, 2)
	}
	one := func(f func(*testing.T, *env, *authoritytest.Account, ac.ImportItem) ac.ImportItem,
		base func(*testing.T, *env, *authoritytest.Account) ac.ImportItem) func(*testing.T, *env, *authoritytest.Account) fixture {
		return func(t *testing.T, e *env, a *authoritytest.Account) fixture {
			it := base(t, e, a)
			return fixture{items: []ac.ImportItem{f(t, e, a, it)}}
		}
	}
	same := func(_ *testing.T, _ *env, _ *authoritytest.Account, it ac.ImportItem) ac.ImportItem { return it }

	for _, tc := range []struct {
		name string
		why  string
		make func(*testing.T, *env, *authoritytest.Account) fixture
		rp   func(*testing.T, ac.ImportItem) restorePoint
		post func(*testing.T, *env, *authoritytest.Account)
	}{
		{name: "no restore point is recorded", why: "no restore point is recorded", make: one(same, addDevice),
			post: func(t *testing.T, e *env, _ *authoritytest.Account) {
				mustExec(t, e.f.Super, `UPDATE authority.meta SET restore_started_at = NULL WHERE id = 1`)
			}},
		{name: "issued before the D-96 margin", why: "issued before the D-96 margin", make: one(same, addDevice),
			rp: func(t *testing.T, it ac.ImportItem) restorePoint {
				return restorePoint{backupMaxMs: issuedOfDevice(t, it) + 61_000}
			}},
		{name: "issued after the restore began", why: "issued after the restore began", make: one(same, addDevice),
			rp: func(t *testing.T, it ac.ImportItem) restorePoint {
				return restorePoint{startedMs: issuedOfDevice(t, it) - 1000}
			}},
		{name: "another private head", why: "a broken device chain: another private head",
			make: one(func(t *testing.T, e *env, _ *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				return forgeDeviceResultOf(t, e, it, func(dr *ac.DeviceResult) { dr.PrevDeviceEntryDigest = flipped(dr.ChainEntryDigest) })
			}, addDevice)},
		{name: "another published head by its entry digest", why: "a broken device chain: another published head",
			make: one(func(t *testing.T, e *env, _ *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				return forgeDeviceResultOf(t, e, it, func(dr *ac.DeviceResult) { dr.ChainEntryDigest = flipped(dr.ChainEntryDigest) })
			}, addDevice)},
		{name: "the entry was built on a published head the replay has passed", why: "a broken device chain: another published head",
			make: func(t *testing.T, e *env, a *authoritytest.Account) fixture {
				d2 := addDevice(t, e, a)                            // chain_seq 1
				e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes) // seq 2
				return fixture{items: []ac.ImportItem{d2}, early: []ac.ImportItem{retained(t, e, a.ID, 2)}}
			}},
		{name: "the item does not rebuild a device-submit", why: "the item does not rebuild a device-submit",
			make: one(func(_ *testing.T, _ *env, _ *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				it.CPEvidence = []byte("not an evidence")
				return it
			}, addDevice)},
		{name: "cp_evidence the cp-authz did not sign", why: "the cp-authz does not bind the request and its evidence",
			make: func(t *testing.T, e *env, a *authoritytest.Account) fixture {
				it := addDevice(t, e, a)
				other, _ := e.enrolledWith() // well-formed evidence of another request
				it.CPEvidence = retained(t, e, other.ID, 1).CPEvidence
				return fixture{items: []ac.ImportItem{it}}
			}},
		{name: "sig_new_device does not verify", why: "a request signature does not verify",
			make: one(func(t *testing.T, _ *env, _ *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				return flipDeviceBundle(t, it, func(db *ac.DeviceBundle) { db.SigNewDevice = flipped(db.SigNewDevice) })
			}, addDevice)},
		{name: "a device-intent malformed for its op", why: "the device-intent is malformed for its op",
			make: one(func(t *testing.T, e *env, a *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				return reforgeDeviceIntent(t, e, a, nil, it, func(di *ac.DeviceIntent) { di.RemovedDeviceIDs = []string{authoritytest.NewDevice(t).ID} })
			}, bindRecovery)},
		{name: "sig_root does not verify", why: "a private signature does not verify",
			make: one(func(t *testing.T, _ *env, _ *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				return flipDeviceBundle(t, it, func(db *ac.DeviceBundle) { db.SigRoot = flipped(db.SigRoot) })
			}, addDevice)},
		{name: "sig_actor does not verify", why: "a step's signature does not verify",
			make: one(func(t *testing.T, _ *env, _ *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				return flipDeviceBundle(t, it, func(db *ac.DeviceBundle) { db.SigActor = flipped(db.SigActor) })
			}, addDevice)},
		{name: "a stale device sequence", why: "refused: " + string(ac.RefusalStaleDeviceSeq),
			make: one(func(t *testing.T, e *env, a *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				return reforgeDeviceIntent(t, e, a, nd, it, func(di *ac.DeviceIntent) { di.ExpectedDseq++ })
			}, addDevice)},
		{name: "an actor device the account does not hold", why: "refused: " + string(ac.RefusalDeviceUnknown),
			make: one(func(t *testing.T, e *env, a *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				return reforgeDeviceIntent(t, e, a, nd, it, func(di *ac.DeviceIntent) { di.ActorDeviceID = authoritytest.NewDevice(t).ID })
			}, addDevice)},
		{name: "a key bound to another account", why: "refused: " + string(ac.RefusalDeviceKeyBound),
			make: func(t *testing.T, e *env, a *authoritytest.Account) fixture {
				it := addDevice(t, e, a)
				// The same device is enrolled for another account after the backup: its
				// keys are then bound there, and this account's add_device may not bind them.
				b, _ := e.enrolledWith()
				return fixture{items: []ac.ImportItem{it}, late: func() { e.deviceOK(b, b.AddDevice(t, nd).Bytes) }}
			}},
		{name: "a device result that is not the commit core's", why: "the signed device result is not the commit core's",
			make: one(func(t *testing.T, e *env, _ *authoritytest.Account, it ac.ImportItem) ac.ImportItem {
				return forgeDeviceResultOf(t, e, it, func(dr *ac.DeviceResult) { dr.DeviceSetDigest = flipped(dr.DeviceSetDigest) })
			}, addDevice)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			a, _ := e.enrolledWith()
			snapshotAll(t, e, "imp")
			fx := tc.make(t, e, a)
			rewind(t, e, "imp", a)
			if fx.late != nil {
				fx.late()
			}
			rp := restorePoint{}
			if tc.rp != nil {
				rp = tc.rp(t, fx.items[0])
			}
			beginImport(t, e, rp)
			if len(fx.early) > 0 {
				importOK(t, e, importBatch(t, fx.early...))
				requireHeld(t, e)
			}
			if tc.post != nil {
				tc.post(t, e, a)
			}
			events, devices := eventsView(t, e, a.ID), deviceEventsView(t, e, a.ID)

			if why := replayWhy(t, e, fx.items...); why != tc.why {
				t.Fatalf("the replay refused for %q, want %q", why, tc.why)
			}
			importOK(t, e, importBatch(t, fx.items...))
			requireHeld(t, e, a.ID)
			if got := eventsView(t, e, a.ID); got != events {
				t.Fatalf("published events after the refusal = %q, want %q", got, events)
			}
			if got := deviceEventsView(t, e, a.ID); got != devices {
				t.Fatalf("device entries after the refusal = %q, want %q", got, devices)
			}
		})
	}
}
