package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// Task 14 (plan, DoR:1767, AC13): the restore sequence end to end, with serve
// stopped at exactly the steps that need it.
//
// LAYERS. No `serve` process runs (Task 0's record). A held store.ServeLock
// stands in for serve wherever the CLI must see it running. The CLI assertions
// (restore-begin, reconcile-complete and clear-held: exit code, reason, stdout)
// go through the real runMain over a real store.Owner. The request-path
// assertions go through store.Store, the object every HTTP route calls, NOT
// through HTTP: a *server.Server is built only by server.Run (a real listener
// and mTLS), and `serveRealm` is in package server's tests. So "503 reconcile"
// is the Outcome Submit returns (the route writes it unchanged), and "409 with
// an empty body" is store.ErrReconciled, the error the route maps to that 409
// (server.operator, pinned in package server's own tests).

// restoreRig is the realm, a Store on the application pool, and a way to move
// the realm's pinned clock to now before an account builds a bundle.
type restoreRig struct {
	*ownerRig
	s *store.Store
}

func newRestoreRig(t *testing.T) *restoreRig {
	t.Helper()
	r := okRig(t)
	// Seal the legacy population, so enroll_new is admitted (store tests' seal).
	if _, err := r.f.Owner.Exec(`UPDATE authority.meta SET legacy_sealed_at = clock_timestamp(), population_digest = $1`,
		bytes.Repeat([]byte{4}, 48)); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(store.Config{DB: r.f.App, Signer: r.rl.RSK, Anchors: r.rl.AnchorBytes, Probe: okProbe{true},
		Hooks:      store.Hooks{Statfs: func() (uint64, error) { return 1 << 40, nil }},
		TxDeadline: 5 * time.Second, ReadDeadline: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return &restoreRig{ownerRig: r, s: s}
}

// fresh moves the realm's clock to the real now, which the Store reads (no
// DBNow hook), so no bundle is built from a stale fixture clock.
func (r *restoreRig) fresh() { r.rl.Clock.Ms = uint64(time.Now().UnixMilli()) }

func (r *restoreRig) submit(b []byte) store.Outcome {
	r.fresh()
	return r.s.Submit(context.Background(), b)
}

// commit submits b and requires `committed`, then resyncs the account's head.
func (r *restoreRig) commit(t *testing.T, a *authoritytest.Account, b []byte) {
	t.Helper()
	if out := r.submit(b); out.Kind != store.KindCommitted {
		t.Fatalf("submit: want committed, got %+v (%s)", out, out.Code)
	}
	a.Sync(t, r.f.App)
}

// replaceRoot is a replace_root for a, valid after a restore: an intent must
// outlive restore_started_at + the intent window (txsteps' expiry), which the
// builder's default expiry never does, so it expires at the latest instant the
// Store admits.
func (r *restoreRig) replaceRoot(t *testing.T, a *authoritytest.Account) []byte {
	t.Helper()
	r.fresh()
	return a.Build(t, authoritytest.Spec{Op: ac.OpReplaceRoot, NewRoot: true, NewEK: true,
		Factors: []string{ac.FactorEmail, ac.FactorPassword}, PasswordLogin: true, WindowClear: true,
		ExpiresAtMs: r.rl.Clock.Ms + ac.IntentWindowMs}).Bytes
}

// enroll is an enrolled account with a known genesis device, so a later
// operation can be signed by it.
func (r *restoreRig) enroll(t *testing.T) *authoritytest.Account {
	t.Helper()
	r.fresh()
	a := r.rl.NewAccount(t)
	dev := authoritytest.NewDevice(t)
	r.commit(t, a, a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true, NewDevice: dev,
		Factors: []string{ac.FactorRegistration}}).Bytes)
	a.Activated()
	a.Actor = dev
	a.Devices[dev.ID] = dev
	return a
}

var restoreTables = []string{"account_heads", "account_events", "device_events", "key_bindings", "request_ledger"}

// snapshot copies every table an account's results touch to public.zz_rs_<table>
// (dropped at the end of the test): the backup point.
func (r *restoreRig) snapshot(t *testing.T) {
	t.Helper()
	for _, tb := range restoreTables {
		full := "zz_rs_" + tb
		for _, q := range []string{`DROP TABLE IF EXISTS public.` + full, `CREATE TABLE public.` + full + ` AS SELECT * FROM authority.` + tb} {
			if _, err := r.f.Super.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _, _ = r.f.Super.Exec(`DROP TABLE IF EXISTS public.` + full) })
	}
}

// restoreTo moves the accounts back to the snapshot. A replica-role transaction
// skips the schema's append-only guards, as a restore does.
func (r *restoreRig) restoreTo(t *testing.T, accts ...*authoritytest.Account) {
	t.Helper()
	list := ""
	for i, a := range accts {
		if i > 0 {
			list += ","
		}
		list += "'" + a.ID + "'"
	}
	q := `BEGIN; SET LOCAL session_replication_role = replica;`
	for _, tb := range restoreTables {
		override := ""
		if tb == "account_events" || tb == "device_events" {
			override = " OVERRIDING SYSTEM VALUE"
		}
		q += fmt.Sprintf(`DELETE FROM authority.%[1]s WHERE account_id IN (%[2]s);
			INSERT INTO authority.%[1]s%[3]s SELECT * FROM public.zz_rs_%[1]s WHERE account_id IN (%[2]s);`, tb, list, override)
	}
	if _, err := r.f.Super.Exec(q + `COMMIT`); err != nil {
		t.Fatal(err)
	}
	for _, a := range accts {
		a.Sync(t, r.f.App)
	}
}

// event is the stored account_events row at (account, seq): the bundle the CP
// would have retained, and its cp_evidence.
func (r *restoreRig) event(t *testing.T, account string, seq int) (bundle, evidence []byte) {
	t.Helper()
	if err := r.f.App.QueryRow(`SELECT bundle, cp_evidence FROM authority.account_events WHERE account_id = $1 AND seq = $2`,
		account, seq).Scan(&bundle, &evidence); err != nil {
		t.Fatalf("event %s seq %d: %v", account, seq, err)
	}
	return bundle, evidence
}

type metaView struct {
	epoch      int64
	reconciled bool
}

func (r *restoreRig) meta(t *testing.T) metaView {
	t.Helper()
	var m metaView
	if err := r.f.Super.QueryRow(`SELECT restore_epoch, reconciled FROM authority.meta WHERE id = 1`).Scan(&m.epoch, &m.reconciled); err != nil {
		t.Fatal(err)
	}
	return m
}

func (r *restoreRig) heldOf(t *testing.T, account string) bool {
	t.Helper()
	var h bool
	if err := r.f.Super.QueryRow(`SELECT held FROM authority.account_heads WHERE account_id = $1`, account).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

// serving stands in for serve: it holds the serve lock until the returned
// function runs.
func (r *restoreRig) serving(t *testing.T) (release func()) {
	t.Helper()
	l, err := store.AcquireServeLock(context.Background(), r.f.App)
	if err != nil {
		t.Fatal(err)
	}
	var once bool
	release = func() {
		if !once {
			once = true
			l.Release()
		}
	}
	t.Cleanup(release)
	return release
}

// A VERB that needs serve stopped refuses with exit 1 and reason=lock while a
// serve holds the lock, and writes nothing.
func (r *restoreRig) requireLockRefused(t *testing.T, args ...string) {
	t.Helper()
	before := r.meta(t)
	code, stdout, stderr := run(args...)
	if stderr != "" {
		t.Fatalf("%q: stderr %q", args, stderr)
	}
	if code != 1 {
		t.Fatalf("%q while serve holds the lock: exit %d, stdout %q, want exit 1", args, code, stdout)
	}
	exitFields(t, stdout, "lock") // reason lock, step owner
	if after := r.meta(t); after != before {
		t.Fatalf("%q refused on the lock but meta moved %+v -> %+v", args, before, after)
	}
}

// AC13: restore-begin -> import -> required/hold -> reconcile-complete ->
// clear-held, serve stopped at each owner step and absent from none of them.
func TestRestoreSequenceEndToEnd(t *testing.T) {
	r := newRestoreRig(t)
	file := ac.TrustEnvelope{Items: [][]byte{r.rl.Current.Signed}}.Encode()
	if file == nil {
		t.Fatal("the trust file does not encode")
	}
	if err := os.WriteFile(filepath.Join(r.dir, "trust"), file, 0o600); err != nil { // ownerEnv's AUTHORITY_TRUST_PATH
		t.Fatal(err)
	}

	// The backup: A and H are enrolled (seq 1) and snapshotted. Past the backup
	// each commits one operator suspend (seq 2), which the CP retains; then the
	// authority is restored to the backup, behind both.
	a, h := r.enroll(t), r.enroll(t)
	r.snapshot(t)
	var retained [2]struct{ bundle, evidence []byte }
	for i, acct := range []*authoritytest.Account{a, h} {
		r.commit(t, acct, acct.Suspend(t, ac.ActorOperator).Bytes)
		retained[i].bundle, retained[i].evidence = r.event(t, acct.ID, 2)
	}
	r.restoreTo(t, a, h)
	if a.Head.Seq != 1 || h.Head.Seq != 1 {
		t.Fatalf("fixture: restored heads at seq %d and %d, want 1 and 1", a.Head.Seq, h.Head.Seq)
	}
	if m := r.meta(t); !m.reconciled || m.epoch != 0 {
		t.Fatalf("fixture: meta before restore-begin = %+v, want reconciled at epoch 0", m)
	}

	// Steps 1-5, with a serve running: restore-begin exits 1, reason=lock.
	release := r.serving(t)
	r.requireLockRefused(t, "restore-begin")

	// Serve stopped: restore-begin records the point and leaves the realm unreconciled.
	release()
	point := regexp.MustCompile(`^restore_epoch=1 backup_max_commit_id=\d+ backup_max_issued_at_ms=\d+ restore_started_at=\S+$`)
	if lines := okRun(t, "restore-begin"); len(lines) != 1 || !point.MatchString(lines[0]) {
		t.Fatalf("restore-begin stdout %q, want one restore-point line at epoch 1", lines)
	}
	if m := r.meta(t); m.reconciled || m.epoch != 1 {
		t.Fatalf("meta after restore-begin = %+v, want unreconciled at epoch 1", m)
	}

	// Unreconciled: a transaction is refused 503 reconcile (store layer).
	r.fresh()
	fresh := r.rl.NewAccount(t)
	if out := r.submit(fresh.EnrollNew(t).Bytes); out.Kind != store.KindError || out.HTTP != 503 || out.Code != string(ac.ErrorReconcile) {
		t.Fatalf("POST /v1/transactions while unreconciled = %+v, want 503 reconcile", out)
	}

	// Steps 6-8: A's retained suspend is imported; H's bundle is proven ahead by
	// /required and held by /hold. (Store layer: the routes call these methods.)
	ctx := context.Background()
	r.fresh()
	item := ac.ImportItem{Bundle: retained[0].bundle, CPEvidence: retained[0].evidence}.Encode()
	if item == nil {
		t.Fatal("the import item does not encode")
	}
	batch := ac.ImportBatch{Items: [][]byte{item}}.Encode()
	if out, err := r.s.ReconcileImport(ctx, batch); err != nil || out.HTTP != 200 || len(out.Body) != 0 {
		t.Fatalf("ReconcileImport = %+v, %v, want 200 with an empty body", out, err)
	}
	a.Sync(t, r.f.App)
	if a.Head.Seq != 2 || r.heldOf(t, a.ID) {
		t.Fatalf("A after import: seq %d, held %v, want seq 2, not held", a.Head.Seq, r.heldOf(t, a.ID))
	}
	holdReq := ac.HoldReq{Bundles: [][]byte{retained[1].bundle}}.Encode()
	if holdReq == nil {
		t.Fatal("the hold-req does not encode")
	}
	if err := r.s.ReconcileRequired(ctx, holdReq); err != nil {
		t.Fatalf("ReconcileRequired = %v, want nil", err)
	}
	if r.meta(t).reconciled || r.heldOf(t, h.ID) {
		t.Fatal("/required must leave the realm unreconciled and hold nothing")
	}
	if out, err := r.s.ReconcileHold(ctx, holdReq); err != nil || out.HTTP != 200 || len(out.Body) != 0 {
		t.Fatalf("ReconcileHold = %+v, %v, want 200 with an empty body", out, err)
	}
	if !r.heldOf(t, h.ID) || r.heldOf(t, a.ID) {
		t.Fatalf("held: H %v, A %v, want H held and A not", r.heldOf(t, h.ID), r.heldOf(t, a.ID))
	}

	// Step 9, with a serve running: reconcile-complete exits 1, reason=lock.
	release = r.serving(t)
	r.requireLockRefused(t, "reconcile-complete")
	release()
	if lines := okRun(t, "reconcile-complete"); len(lines) != 1 || lines[0] != "imported=1 held=1" {
		t.Fatalf("reconcile-complete stdout %q, want imported=1 held=1", lines)
	}
	if !r.meta(t).reconciled {
		t.Fatal("meta is still unreconciled after reconcile-complete")
	}

	// Reconciled: import and hold are 409 with no body (store.ErrReconciled).
	if _, err := r.s.ReconcileImport(ctx, batch); !errors.Is(err, store.ErrReconciled) {
		t.Fatalf("ReconcileImport after reconcile-complete = %v, want ErrReconciled", err)
	}
	if _, err := r.s.ReconcileHold(ctx, holdReq); !errors.Is(err, store.ErrReconciled) {
		t.Fatalf("ReconcileHold after reconcile-complete = %v, want ErrReconciled", err)
	}

	// The held account refuses a replace_root until the owner clears it.
	h.Sync(t, r.f.App)
	if out := r.s.Submit(ctx, r.replaceRoot(t, h)); out.Kind != store.KindRefused || out.Code != string(ac.RefusalHeld) {
		t.Fatalf("a held account's replace_root = %+v (%s), want refused held", out, out.Code)
	}

	// Step 12's window: clear-held needs serve stopped too. The dry run reads
	// only, so it runs with serve up; the real run refuses on the lock.
	release = r.serving(t)
	if lines := okRun(t, "clear-held", "--dry-run", h.ID); len(lines) != 1 || lines[0] != "account="+h.ID+" outcome="+store.ClearUnverified {
		t.Fatalf("clear-held --dry-run stdout %q, want account=%s outcome=%s", lines, h.ID, store.ClearUnverified)
	}
	if !r.heldOf(t, h.ID) {
		t.Fatal("a dry run cleared the hold")
	}
	r.requireLockRefused(t, "clear-held", h.ID)
	if !r.heldOf(t, h.ID) {
		t.Fatal("clear-held under serve's lock cleared the hold")
	}
	release()
	if lines := okRun(t, "clear-held", h.ID); len(lines) != 1 || lines[0] != "account="+h.ID+" outcome="+store.ClearUnverified {
		t.Fatalf("clear-held stdout %q, want account=%s outcome=%s", lines, h.ID, store.ClearUnverified)
	}
	if r.heldOf(t, h.ID) {
		t.Fatal("H is still held after clear-held")
	}

	// The cleared account's replacement is admitted.
	h.Sync(t, r.f.App)
	out := r.s.Submit(ctx, r.replaceRoot(t, h))
	if out.Kind != store.KindCommitted {
		t.Fatalf("replace_root after clear-held = %+v (%s), want committed", out, out.Code)
	}
}
