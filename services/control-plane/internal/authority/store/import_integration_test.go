package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// Task 7 (plan): reconcile/import (DoR:1727-1730, 1765-1769; D-96; D-288;
// B3-D16). Each test builds "committed, then restored to a backup" the way a
// restore leaves the authority: the CP retained the bundles (and their
// cp_evidence) of results the authority no longer has, and the authority's
// database is back at the backup point.
//
// B3-D16 under test: every item first passes ac.AuthenticateBundle; one that
// does not makes the whole batch ErrBadAuthz with nothing held or imported. An
// authenticated item that policy replay, the D-96 margin, the issued_at cap or
// a fork then refuses HOLDS its account (held = true), skips that account's
// remaining items, and the request still answers 200; other accounts import.

// ---- the backup / restore fixture ----

var importTables = []string{"account_heads", "account_events", "device_events", "key_bindings", "request_ledger"}

// snapshotAll copies every table an account's results touch to
// public.zz_<name>_<table> (dropped at the end of the test): a backup point.
func snapshotAll(t *testing.T, e *env, name string) {
	t.Helper()
	for _, tb := range importTables {
		full := "zz_" + name + "_" + tb
		mustExec(t, e.f.Super, `DROP TABLE IF EXISTS public.`+full)
		mustExec(t, e.f.Super, `CREATE TABLE public.`+full+` AS SELECT * FROM authority.`+tb)
		t.Cleanup(func() { _, _ = e.f.Super.Exec(`DROP TABLE IF EXISTS public.` + full) })
	}
}

// restoreAccounts moves the accounts back to the backup: every table's rows for
// them are the snapshot's (none, for an account the snapshot lacks). A
// replica-role transaction skips the schema's append-only guards, as
// restoreTo's does.
func restoreAccounts(t *testing.T, e *env, name string, ids ...string) {
	t.Helper()
	l := uuidList(ids...)
	q := `BEGIN; SET LOCAL session_replication_role = replica;`
	for _, tb := range importTables {
		override := ""
		if tb == "account_events" || tb == "device_events" {
			override = " OVERRIDING SYSTEM VALUE"
		}
		q += fmt.Sprintf(`DELETE FROM authority.%[1]s WHERE account_id IN (%[2]s);
			INSERT INTO authority.%[1]s%[4]s SELECT * FROM public.zz_%[3]s_%[1]s WHERE account_id IN (%[2]s);`,
			tb, l, name, override)
	}
	mustExec(t, e.f.Super, q+`COMMIT`)
}

// rewind restores the accounts to the backup and resyncs the fixture's heads.
func rewind(t *testing.T, e *env, name string, accts ...*authoritytest.Account) {
	t.Helper()
	ids := make([]string, len(accts))
	for i, a := range accts {
		ids[i] = a.ID
	}
	restoreAccounts(t, e, name, ids...)
	for _, a := range accts {
		a.Sync(t, e.f.App)
	}
}

// retained is the import item the CP would hold for the result at (account, seq):
// the stored bundle, its cp_evidence, and the attached device entry when the op
// has one.
func retained(t *testing.T, e *env, account string, seq int) ac.ImportItem {
	t.Helper()
	var bundle, evidence []byte
	var op string
	if err := e.f.App.QueryRow(`SELECT bundle, cp_evidence, op FROM authority.account_events WHERE account_id = $1 AND seq = $2`,
		account, seq).Scan(&bundle, &evidence, &op); err != nil {
		t.Fatalf("retained %s seq %d: %v", account, seq, err)
	}
	it := ac.ImportItem{Bundle: bundle, CPEvidence: evidence}
	var dev []byte
	err := e.f.App.QueryRow(`SELECT bundle FROM authority.device_events WHERE account_id = $1 AND chain_seq = $2 AND op = $3`,
		account, seq, op).Scan(&dev)
	switch {
	case err == nil:
		it.DeviceBundle = dev
	case !errors.Is(err, sql.ErrNoRows):
		t.Fatalf("retained device entry %s seq %d: %v", account, seq, err)
	}
	return it
}

// importBatch encodes items as an import-batch and requires it to be a valid
// one (ascending order included): a test that wants an invalid batch uses
// rawBatch.
func importBatch(t *testing.T, items ...ac.ImportItem) []byte {
	t.Helper()
	b := rawBatch(t, items...)
	if _, err := ac.CheckImportBatch(b); err != nil {
		t.Fatalf("the test import-batch is not a valid one: %v", err)
	}
	return b
}

func rawBatch(t *testing.T, items ...ac.ImportItem) []byte {
	t.Helper()
	raw := make([][]byte, len(items))
	for i, it := range items {
		if raw[i] = it.Encode(); raw[i] == nil {
			t.Fatalf("import item %d does not encode", i)
		}
	}
	b := ac.ImportBatch{Items: raw}.Encode()
	if b == nil {
		t.Fatal("the test import-batch does not encode")
	}
	return b
}

// issuedOf is the issued_at of an item's published result.
func issuedOf(t *testing.T, it ac.ImportItem) uint64 {
	t.Helper()
	bd, err := ac.DecodeBundle(it.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ac.DecodeResult(bd.Result)
	if err != nil {
		t.Fatal(err)
	}
	return r.IssuedAtMs
}

// restorePoint is what restore-begin leaves in meta. Zero fields take the
// defaults: restore_started_at an hour past db_now (no cap in the way) and
// backup_max_issued_at_ms the database's own maximum, as restore-begin records.
type restorePoint struct{ startedMs, backupMaxMs uint64 }

func beginImport(t *testing.T, e *env, p restorePoint) {
	t.Helper()
	if p.startedMs == 0 {
		p.startedMs = e.now.Load() + 3_600_000
	}
	var backupMax any
	if p.backupMaxMs != 0 {
		backupMax = i64(p.backupMaxMs)
	}
	mustExec(t, e.f.Super, `UPDATE authority.meta SET reconciled = false, restore_epoch = restore_epoch + 1,
		restore_started_at = to_timestamp(($1::bigint / 1000.0)::double precision),
		backup_max_commit_id = (SELECT COALESCE(max(commit_id), 0) FROM authority.account_events),
		backup_max_issued_at_ms = COALESCE($2::bigint, GREATEST((SELECT COALESCE(max(issued_at_ms), 0) FROM authority.account_events),
			(SELECT COALESCE(max(issued_at_ms), 0) FROM authority.device_events)))
		WHERE id = 1`, i64(p.startedMs), backupMax)
}

// importOK sends body and requires 200 with an empty body (B3-D14).
func importOK(t *testing.T, e *env, body []byte) {
	t.Helper()
	out, err := e.s.ReconcileImport(liveCtx(t), body)
	if err != nil {
		t.Fatalf("ReconcileImport = %v, want nil", err)
	}
	if out.HTTP != 200 || len(out.Body) != 0 {
		t.Fatalf("ReconcileImport outcome = %+v, want 200 with an empty body", out)
	}
}

func requireBadAuthz(t *testing.T, e *env, body []byte) {
	t.Helper()
	if _, err := e.s.ReconcileImport(liveCtx(t), body); !errors.Is(err, ErrBadAuthz) {
		t.Fatalf("ReconcileImport = %v, want ErrBadAuthz", err)
	}
}

// eventsView is "<seq><t|f>,...": each stored account event and whether it was imported.
func eventsView(t *testing.T, e *env, account string) string {
	t.Helper()
	rows, err := e.f.App.Query(`SELECT seq, imported FROM authority.account_events WHERE account_id = $1 ORDER BY seq`, account)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var parts []string
	for rows.Next() {
		var seq int
		var imp bool
		if err := rows.Scan(&seq, &imp); err != nil {
			t.Fatal(err)
		}
		f := "f"
		if imp {
			f = "t"
		}
		parts = append(parts, fmt.Sprint(seq, f))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(parts, ",")
}

func requireEvents(t *testing.T, e *env, account, want string) {
	t.Helper()
	if got := eventsView(t, e, account); got != want {
		t.Fatalf("account %s events = %q, want %q", account, got, want)
	}
}

func requireHeld(t *testing.T, e *env, want ...string) {
	t.Helper()
	if got := heldIDs(t, e); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("held = %v, want %v", got, want)
	}
}

// headJSON is the account's whole head row but for held: what an import must
// reproduce byte for byte.
func headJSON(t *testing.T, e *env, account string) string {
	t.Helper()
	var s string
	if err := e.f.App.QueryRow(`SELECT (to_jsonb(h) - 'held')::text FROM authority.account_heads h WHERE account_id = $1`,
		account).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func bindingsView(t *testing.T, e *env, account string) string {
	t.Helper()
	rows, err := e.f.App.Query(`SELECT encode(key_digest, 'hex'), role, first_seq FROM authority.key_bindings
		WHERE account_id = $1 ORDER BY key_digest`, account)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var parts []string
	for rows.Next() {
		var d, role string
		var seq int
		if err := rows.Scan(&d, &role, &seq); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, fmt.Sprint(d, ":", role, ":", seq))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(parts, ",")
}

func headSeq(t *testing.T, e *env, account string) uint64 {
	t.Helper()
	var seq uint64
	err := e.f.App.QueryRow(`SELECT seq FROM authority.account_heads WHERE account_id = $1`, account).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// oneAhead enrolls an account, takes the backup "imp", commits one operator
// suspend (seq 2), keeps its item and restores. Callers re-run rewind(e, "imp", a)
// between cases.
func oneAhead(t *testing.T, e *env) (*authoritytest.Account, ac.ImportItem) {
	t.Helper()
	a, _ := e.enrolledWith()
	snapshotAll(t, e, "imp")
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)
	it := retained(t, e, a.ID, 2)
	rewind(t, e, "imp", a)
	return a, it
}

// ---- tests ----

// AC13: the results the CP retained past the backup point are replayed and the
// account is exactly where it was before the restore. The ops cover a plain
// transition, its inverse, and one with an attached device entry and new key
// bindings.
func TestImportReplaysRetainedResults(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	snapshotAll(t, e, "imp")
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)   // 2
	e.commitOK(a, a.Unsuspend(t, ac.ActorOperator).Bytes) // 3
	e.commitOK(a, a.RotateRoot(t).Bytes)                  // 4: attached device entry, new bindings
	wantHead, wantBind := headJSON(t, e, a.ID), bindingsView(t, e, a.ID)
	items := []ac.ImportItem{retained(t, e, a.ID, 2), retained(t, e, a.ID, 3), retained(t, e, a.ID, 4)}
	if items[2].DeviceBundle == nil {
		t.Fatal("fixture: the rotate_root item carries no attached device entry")
	}

	rewind(t, e, "imp", a)
	if headSeq(t, e, a.ID) != 1 {
		t.Fatal("fixture: the restore did not move the head back to seq 1")
	}
	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, items...))

	if got := headJSON(t, e, a.ID); got != wantHead {
		t.Fatalf("head after import =\n%s\nwant (pre-reset)\n%s", got, wantHead)
	}
	if got := bindingsView(t, e, a.ID); got != wantBind {
		t.Fatalf("key bindings after import = %q, want %q", got, wantBind)
	}
	requireEvents(t, e, a.ID, "1f,2t,3t,4t")
	requireHeld(t, e)
	var wrongEpoch int
	if err := e.f.App.QueryRow(`SELECT count(*) FROM authority.account_events ev, authority.meta m
		WHERE ev.account_id = $1 AND ev.imported AND ev.restore_epoch <> m.restore_epoch`, a.ID).Scan(&wrongEpoch); err != nil {
		t.Fatal(err)
	}
	if wrongEpoch != 0 {
		t.Fatalf("%d imported events carry a restore_epoch other than the current one", wrongEpoch)
	}
	var importedDevice int
	if err := e.f.App.QueryRow(`SELECT count(*) FROM authority.device_events WHERE account_id = $1 AND imported`, a.ID).Scan(&importedDevice); err != nil {
		t.Fatal(err)
	}
	if importedDevice != 1 {
		t.Fatalf("%d imported device entries, want exactly the rotate_root's one", importedDevice)
	}
}

// An item the authority already has (same seq, same entry) is skipped: nothing
// is held, and the stored event keeps imported = false.
func TestImportAlreadyPresentIsSkipped(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes) // seq 2, stays in the database
	it := retained(t, e, a.ID, 2)
	want := headJSON(t, e, a.ID)
	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, it))
	requireHeld(t, e)
	requireEvents(t, e, a.ID, "1f,2f")
	if got := headJSON(t, e, a.ID); got != want {
		t.Fatalf("a skipped item changed the head:\n%s\nwant\n%s", got, want)
	}
}

// D-96: an item absent from the database is imported when its issued_at is at
// most 60 s before the backup's newest, and held when it is older.
func TestImportMargin(t *testing.T) {
	e := newEnv(t)
	a, it := oneAhead(t, e)
	x := issuedOf(t, it)
	cases := []struct {
		name  string
		delta uint64
		held  bool
	}{
		{"59 s below the backup maximum imports", 59_000, false},
		{"exactly 60 s below imports", 60_000, false},
		{"61 s below is refused and holds the account", 61_000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rewind(t, e, "imp", a)
			beginImport(t, e, restorePoint{backupMaxMs: x + c.delta})
			importOK(t, e, importBatch(t, it))
			if c.held {
				requireHeld(t, e, a.ID)
				requireEvents(t, e, a.ID, "1f")
				if s := headSeq(t, e, a.ID); s != 1 {
					t.Fatalf("a refused item moved the head to seq %d", s)
				}
				return
			}
			requireHeld(t, e)
			requireEvents(t, e, a.ID, "1f,2t")
		})
	}
}

// issued_at may not pass the restore's start nor db_now + Epsilon: such a result
// could not have been signed before the restore.
func TestImportIssuedAtCap(t *testing.T) {
	e := newEnv(t)
	a, it := oneAhead(t, e)
	x := issuedOf(t, it)
	saved := e.now.Load()

	t.Run("a result issued after the restore started is held", func(t *testing.T) {
		rewind(t, e, "imp", a)
		beginImport(t, e, restorePoint{startedMs: x - 1000})
		importOK(t, e, importBatch(t, it))
		requireHeld(t, e, a.ID)
		requireEvents(t, e, a.ID, "1f")
	})
	t.Run("a result issued beyond db_now + Epsilon is held", func(t *testing.T) {
		rewind(t, e, "imp", a)
		e.now.Store(x - ac.EpsilonMs - 1)
		t.Cleanup(func() { e.now.Store(saved) })
		beginImport(t, e, restorePoint{startedMs: x + 3_600_000})
		importOK(t, e, importBatch(t, it))
		requireHeld(t, e, a.ID)
		requireEvents(t, e, a.ID, "1f")
	})
	t.Run("a result issued exactly at the restore's start imports", func(t *testing.T) {
		e.now.Store(saved)
		rewind(t, e, "imp", a)
		beginImport(t, e, restorePoint{startedMs: x})
		importOK(t, e, importBatch(t, it))
		requireHeld(t, e)
		requireEvents(t, e, a.ID, "1f,2t")
	})
}

// DoR:1769: a result under a delegation a renewal superseded imports while that
// delegation is installed; under one this authority does not have it fails
// ac.AuthenticateBundle, which refuses the whole batch (B3-D16).
func TestImportSupersededUncovered(t *testing.T) {
	t.Run("an installed superseded delegation imports", func(t *testing.T) {
		e := newEnv(t)
		a, _ := e.enrolledWith()
		snapshotAll(t, e, "imp")
		e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes) // under serial s
		it := retained(t, e, a.ID, 2)
		e.r.Install(t, e.f.App, e.r.NextDelegation(t)) // s + 1: s is superseded
		rewind(t, e, "imp", a)
		beginImport(t, e, restorePoint{})
		importOK(t, e, importBatch(t, it))
		requireHeld(t, e)
		requireEvents(t, e, a.ID, "1f,2t")
	})

	t.Run("an uninstalled delegation is a bad batch and holds nothing", func(t *testing.T) {
		e := newEnv(t)
		a, _ := e.enrolledWith()
		snapshotAll(t, e, "imp")
		next := e.r.NextDelegation(t)
		e.r.Install(t, e.f.App, next)
		e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes) // under next
		it := retained(t, e, a.ID, 2)
		rewind(t, e, "imp", a)
		mustExec(t, e.f.Super, fmt.Sprintf(`BEGIN; SET LOCAL session_replication_role = replica;
			DELETE FROM authority.successions WHERE true;
			DELETE FROM authority.delegations WHERE serial = %d; COMMIT`, next.D.Serial))
		beginImport(t, e, restorePoint{})
		requireBadAuthz(t, e, importBatch(t, it))
		requireHeld(t, e)
		requireEvents(t, e, a.ID, "1f")
	})
}

// Review Focus 1: one account's refused item holds that account alone. Its
// earlier items stay, its later ones are skipped, the request answers 200, and
// an account later in the batch still imports.
func TestImportRefusalHoldsOnlyThatAccount(t *testing.T) {
	e := newEnv(t)
	accts := make([]*authoritytest.Account, 0, 2)
	for range 2 {
		a, _ := e.enrolledWith()
		accts = append(accts, a)
	}
	sortedByID(accts)
	x, y := accts[0], accts[1] // x has two items and is first in the batch
	snapshotAll(t, e, "imp")
	e.commitOK(x, x.Suspend(t, ac.ActorOperator).Bytes)
	e.commitOK(y, y.Suspend(t, ac.ActorOperator).Bytes)
	e.advance(100_000)
	e.commitOK(x, x.Unsuspend(t, ac.ActorOperator).Bytes)
	x2, x3, y2 := retained(t, e, x.ID, 2), retained(t, e, x.ID, 3), retained(t, e, y.ID, 2)
	rewind(t, e, "imp", x, y)

	// The restore started between x's seq 2 and seq 3: seq 3 is past the cap.
	beginImport(t, e, restorePoint{startedMs: issuedOf(t, x2) + 50_000})
	importOK(t, e, importBatch(t, x2, x3, y2))

	requireHeld(t, e, x.ID)
	requireEvents(t, e, x.ID, "1f,2t") // the earlier item is kept
	requireEvents(t, e, y.ID, "1f,2t") // a later account imports
}

// An account whose transaction fails after an earlier account committed does
// not let that commit skip the release gate. Here y's head is locked
// elsewhere, so y fails on its 2 s lock wait after x committed; with no
// standby confirming, the batch is 503 replication rather than y's error, and
// x's event stays committed for the CP's retry to find present.
func TestImportFailureAfterACommitStillReleases(t *testing.T) {
	e := newEnv(t)
	accts := make([]*authoritytest.Account, 0, 2)
	for range 2 {
		a, _ := e.enrolledWith()
		accts = append(accts, a)
	}
	sortedByID(accts)
	x, y := accts[0], accts[1] // x is first in the batch
	snapshotAll(t, e, "imp")
	e.commitOK(x, x.Suspend(t, ac.ActorOperator).Bytes)
	e.commitOK(y, y.Suspend(t, ac.ActorOperator).Bytes)
	x2, y2 := retained(t, e, x.ID, 2), retained(t, e, y.ID, 2)
	rewind(t, e, "imp", x, y)
	beginImport(t, e, restorePoint{})
	requireGate(t, e.f, true)
	e.s.txDL = 4 * time.Second // past y's lock wait, so the release has time left to poll

	blocker, err := e.f.Super.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback() }()
	if _, err := blocker.Exec(`SELECT 1 FROM authority.account_heads WHERE account_id = $1 FOR UPDATE`, y.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.s.ReconcileImport(liveCtx(t), importBatch(t, x2, y2))
	var oe OutcomeError
	if !errors.As(err, &oe) || oe.Outcome.Code != string(ac.ErrorReplication) {
		t.Fatalf("ReconcileImport with y failing after x committed = %v, want 503 replication", err)
	}
	if e.probe.calls.Load() == 0 {
		t.Fatal("x committed and y failed, but the release never asked the replication probe")
	}
	_ = blocker.Rollback()
	requireEvents(t, e, x.ID, "1f,2t")
	requireEvents(t, e, y.ID, "1f")
}

// A result at a seq the authority already holds under a different entry is a
// fork: the account is held and the stored history is untouched.
func TestImportForkHolds(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	snapshotAll(t, e, "imp")
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes) // timeline X
	fork := retained(t, e, a.ID, 2)
	rewind(t, e, "imp", a)
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes) // timeline Y: the authority's own seq 2
	var kept []byte
	if err := e.f.App.QueryRow(`SELECT entry_digest FROM authority.account_events WHERE account_id = $1 AND seq = 2`, a.ID).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, fork))

	requireHeld(t, e, a.ID)
	var got []byte
	if err := e.f.App.QueryRow(`SELECT entry_digest FROM authority.account_events WHERE account_id = $1 AND seq = 2`, a.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(kept) {
		t.Fatal("the fork replaced the stored seq 2")
	}
	requireEvents(t, e, a.ID, "1f,2f")
}

// Two accounts whose retained enrollments bind the same root: the first in the
// batch binds it, the second's binding conflicts (bindKeys) and that account is
// held; the winner imports.
func TestImportRootCollisionHoldsLoser(t *testing.T) {
	e := newEnv(t)
	accts := []*authoritytest.Account{e.r.NewAccount(t), e.r.NewAccount(t)}
	sortedByID(accts)
	snapshotAll(t, e, "imp")
	shared := authoritytest.NewDevice(t).DSK // any P-384 key: both candidates use it
	items := make([]ac.ImportItem, 0, len(accts))
	for _, a := range accts {
		dev := authoritytest.NewDevice(t)
		e.commitOK(a, a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true, NewDevice: dev,
			CandidateKey: shared, Factors: []string{ac.FactorRegistration}}).Bytes)
		items = append(items, retained(t, e, a.ID, 1))
		rewind(t, e, "imp", accts...) // the authority never saw this one: the other may bind the root
	}
	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, items...))

	requireHeld(t, e, accts[1].ID)
	requireEvents(t, e, accts[0].ID, "1t")
	requireEvents(t, e, accts[1].ID, "")
}

// DoR:1727: import never erases. A retained user erase imports as a pending
// erase with no activation floor; reconcile-complete sets the floor.
func TestImportNeverErases(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	snapshotAll(t, e, "imp")
	e.commitOK(a, a.Erase(t, ac.ActorUser, ac.FactorPassword).Bytes) // Pe
	it := retained(t, e, a.ID, 2)
	rewind(t, e, "imp", a)
	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, it))

	var state string
	var pendingErase bool
	var floor sql.NullInt64
	var erasedAt sql.NullTime
	if err := e.f.App.QueryRow(`SELECT state, pending_erase, activation_floor_ms, erased_at FROM authority.account_heads WHERE account_id = $1`,
		a.ID).Scan(&state, &pendingErase, &floor, &erasedAt); err != nil {
		t.Fatal(err)
	}
	if state != ac.StatePending || !pendingErase {
		t.Fatalf("imported erase = state %s pending_erase %v, want a pending erase", state, pendingErase)
	}
	if floor.Valid || erasedAt.Valid {
		t.Fatalf("an import set activation_floor_ms=%v erased_at=%v: only reconcile-complete floors, only purge erases", floor, erasedAt)
	}
	requireEvents(t, e, a.ID, "1f,2t")
	requireHeld(t, e)
}

// This is the replay itself (plan mutation 1): an item whose bundle
// authenticates but whose cp_evidence the policy does not admit is refused and
// holds its account.
func TestImportPolicyReplayRefuses(t *testing.T) {
	t.Run("cp_evidence that is not the evidence the cp-authz signed", func(t *testing.T) {
		e := newEnv(t)
		a, it := oneAhead(t, e)
		other, _ := e.enrolledWith() // its enrollment evidence: well formed, wrong digest
		bad := it
		bad.CPEvidence = retained(t, e, other.ID, 1).CPEvidence
		beginImport(t, e, restorePoint{})
		importOK(t, e, importBatch(t, bad))
		requireHeld(t, e, a.ID)
		requireEvents(t, e, a.ID, "1f")
	})

	t.Run("signed evidence that fails policy.Admit", func(t *testing.T) {
		e := newEnv(t)
		a, it := oneAhead(t, e) // an operator suspend: its floor is the admin WebAuthn factor
		bare := forgeEvidence(t, e, it, func(ev *ac.CPEvidence) { ev.Factors = []string{} })
		beginImport(t, e, restorePoint{})
		importOK(t, e, importBatch(t, bare))
		requireHeld(t, e, a.ID)
		requireEvents(t, e, a.ID, "1f")

		// Control: the same forger with the evidence left as it was imports, so the
		// refusal above is the policy's, not the forger's.
		rewind(t, e, "imp", a)
		mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET held = false WHERE account_id = $1`, a.ID)
		same := forgeEvidence(t, e, it, func(*ac.CPEvidence) {})
		importOK(t, e, importBatch(t, same))
		requireHeld(t, e)
		requireEvents(t, e, a.ID, "1f,2t")
	})
}

// forgeEvidence re-signs it over the evidence mutate leaves: a new evidence
// salt, the cp-authz re-signed by the realm's CAK over the new evidence digest,
// and the result re-signed by the realm's RSK over the new cp-authz digest.
// Every signature verifies and every digest binds, so nothing but the policy
// replay can refuse the item; the authority itself would never sign it.
func forgeEvidence(t *testing.T, e *env, it ac.ImportItem, mutate func(*ac.CPEvidence)) ac.ImportItem {
	t.Helper()
	b, err := ac.DecodeBundle(it.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := ac.DecodeCPEvidence(it.CPEvidence)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&ev)
	ev.EvidenceSalt = append([]byte(nil), ev.EvidenceSalt...)
	ev.EvidenceSalt[0] ^= 1 // a fresh result, so no stored entry digest is reused
	eb := ev.Encode()
	az, err := ac.DecodeCPAuthz(b.CPAuthz)
	if err != nil || eb == nil {
		t.Fatalf("forge: cp-authz %v, evidence encodes %v", err, eb != nil)
	}
	az.EvidenceDigest = ac.Digest(eb)
	b.CPAuthz = az.Encode()
	b.CPAuthzSig = authoritytest.SignRaw(t, e.r.CAK, b.CPAuthz)
	res, err := ac.DecodeResult(b.Result)
	if err != nil {
		t.Fatal(err)
	}
	res.CPAuthzDigest = ac.Digest(b.CPAuthz)
	if b.Result = res.Encode(); b.Result == nil {
		t.Fatal("forge: the result does not encode")
	}
	if b.ResultSig, err = e.r.RSK.Sign(b.Result); err != nil {
		t.Fatal(err)
	}
	out := ac.ImportItem{Bundle: b.Encode(), DeviceBundle: it.DeviceBundle, CPEvidence: eb}
	if out.Bundle == nil {
		t.Fatal("forge: the bundle does not encode")
	}
	return out
}

// A refused item stops its account: the batch's later items for it are not
// replayed even when one would link. Here the account's first item forks the
// stored seq 1 (a retained enrollment from another timeline) and its second
// links to the stored seq 2, so only the skip keeps it out.
func TestImportRefusalSkipsTheAccountsLaterItems(t *testing.T) {
	e := newEnv(t)
	a := e.r.NewAccount(t)
	snapshotAll(t, e, "none") // the account has no head
	enroll := func() {
		t.Helper()
		dev := authoritytest.NewDevice(t)
		e.commitOK(a, a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true, NewDevice: dev,
			Factors: []string{ac.FactorRegistration}}).Bytes)
		a.Activated()
		a.Actor = dev
		a.Devices[dev.ID] = dev
	}
	enroll() // timeline X
	forked := retained(t, e, a.ID, 1)
	rewind(t, e, "none", a)
	enroll()                                            // timeline Y, seq 1
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes) // seq 2
	snapshotAll(t, e, "imp")
	e.commitOK(a, a.Unsuspend(t, ac.ActorOperator).Bytes) // seq 3, retained
	linked := retained(t, e, a.ID, 3)
	rewind(t, e, "imp", a)

	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, forked, linked))
	requireHeld(t, e, a.ID)
	requireEvents(t, e, a.ID, "1f,2f")

	// Control: the linked item alone imports, so the skip is what kept it out.
	mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET held = false WHERE account_id = $1`, a.ID)
	importOK(t, e, importBatch(t, linked))
	requireHeld(t, e)
	requireEvents(t, e, a.ID, "1f,2f,3t")
}

func TestImportOnlyWhileUnreconciled(t *testing.T) {
	e := newEnv(t)
	a, it := oneAhead(t, e)
	_, err := e.s.ReconcileImport(liveCtx(t), importBatch(t, it))
	if !errors.Is(err, ErrReconciled) {
		t.Fatalf("ReconcileImport while reconciled = %v, want ErrReconciled", err)
	}
	requireHeld(t, e)
	requireEvents(t, e, a.ID, "1f")
}

// A body that does not decode, or whose items are out of order, is ErrBadAuthz
// before anything is read.
func TestImportMalformedBatchIsBadAuthz(t *testing.T) {
	e := newEnv(t)
	accts := make([]*authoritytest.Account, 0, 2)
	for range 2 {
		a, _ := e.enrolledWith()
		accts = append(accts, a)
	}
	sortedByID(accts)
	snapshotAll(t, e, "imp")
	for _, a := range accts {
		e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)
	}
	i0, i1 := retained(t, e, accts[0].ID, 2), retained(t, e, accts[1].ID, 2)
	rewind(t, e, "imp", accts...)
	beginImport(t, e, restorePoint{})

	requireBadAuthz(t, e, []byte("not a batch"))
	requireBadAuthz(t, e, rawBatch(t, i1, i0)) // descending account order
	requireHeld(t, e)
	requireEvents(t, e, accts[0].ID, "1f")
	requireEvents(t, e, accts[1].ID, "1f")
}

// B3-D16: one item that does not authenticate refuses the whole batch. Nothing
// is held and nothing is imported, the good item before it included.
func TestImportUnauthenticatedItemHoldsNothing(t *testing.T) {
	e := newEnv(t)
	accts := make([]*authoritytest.Account, 0, 2)
	for range 2 {
		a, _ := e.enrolledWith()
		accts = append(accts, a)
	}
	sortedByID(accts)
	snapshotAll(t, e, "imp")
	for _, a := range accts {
		e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes)
	}
	good, victim := retained(t, e, accts[0].ID, 2), retained(t, e, accts[1].ID, 2)
	rewind(t, e, "imp", accts...)

	b, err := ac.DecodeBundle(victim.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	b.ResultSig = append([]byte(nil), b.ResultSig...)
	b.ResultSig[len(b.ResultSig)-1] ^= 1
	forged := victim
	if forged.Bundle = b.Encode(); forged.Bundle == nil {
		t.Fatal("the broken bundle does not encode")
	}

	beginImport(t, e, restorePoint{})
	requireBadAuthz(t, e, importBatch(t, good, forged))
	requireHeld(t, e)
	requireEvents(t, e, accts[0].ID, "1f")
	requireEvents(t, e, accts[1].ID, "1f")
}

// A history that does not rebuild (here the events of an erased account that
// purge_erased has reaped) holds the account; the request is 200, not a 503.
func TestImportPurgedErasedAccount(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	e.commitOK(a, a.Erase(t, ac.ActorOperator).Bytes) // seq 2, the last event
	it := retained(t, e, a.ID, 2)
	mustExec(t, e.f.Super, `BEGIN; SET LOCAL session_replication_role = replica;
		UPDATE authority.account_heads SET erased_at = now() - make_interval(days => 31) WHERE account_id = `+uuidList(a.ID)+`; COMMIT`)
	rep, err := e.s.Purge(liveCtx(t))
	if err != nil || rep.Purged != 1 {
		t.Fatalf("fixture: Purge = %+v, %v, want the one erased account reaped", rep, err)
	}
	requireEvents(t, e, a.ID, "")

	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, it))
	requireHeld(t, e, a.ID)
}

// Backups at seq 0 and at seq 1 of legacy-population accounts: their first
// result is a PoP enroll_existing that only the pin explains, and the import
// replays it without the hold route's help.
func TestImportPinnedAccount(t *testing.T) {
	e, accts := newEnvPop(t, 2)
	sortedByID(accts)
	snapshotAll(t, e, "pin0") // no events yet: every account at seq 0
	for _, a := range accts {
		e.commitOK(a, a.EnrollExisting(t, true, ac.FactorPassword).Bytes)
	}
	snapshotAll(t, e, "pin1")
	e.commitOK(accts[1], accts[1].CancelByPoP(t).Bytes) // seq 2
	i0, i1 := retained(t, e, accts[0].ID, 1), retained(t, e, accts[1].ID, 2)
	restoreAccounts(t, e, "pin0", accts[0].ID)
	restoreAccounts(t, e, "pin1", accts[1].ID)

	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, i0, i1))
	requireHeld(t, e)
	requireEvents(t, e, accts[0].ID, "1t")
	requireEvents(t, e, accts[1].ID, "1f,2t")
}

// Three items for one pinned account in one batch, from seq 0: the replay of
// each links to the one before it.
func TestImportMultiItemChainFromPinnedGenesis(t *testing.T) {
	e, accts := newEnvPop(t, 1)
	a := accts[0]
	snapshotAll(t, e, "pin0")
	commit := func(label string, b []byte) bool {
		t.Helper()
		out := e.submit(b)
		if out.Kind != KindCommitted {
			return false
		}
		_ = label
		a.Sync(t, e.f.App)
		return true
	}
	if !commit("enroll_existing", a.EnrollExisting(t, true, ac.FactorPassword).Bytes) {
		t.Fatal("fixture: the PoP enroll_existing was not committed")
	}
	if !commit("cancel", a.CancelByPoP(t).Bytes) {
		t.Fatal("fixture: the PoP cancel was not committed")
	}
	if !commit("third", a.Suspend(t, ac.ActorOperator).Bytes) && !commit("third", a.EnrollExisting(t, true, ac.FactorPassword).Bytes) {
		t.Skip("fixture: no third op (suspend or enroll_existing) is admitted after a cancelled PoP enroll_existing")
	}
	items := []ac.ImportItem{retained(t, e, a.ID, 1), retained(t, e, a.ID, 2), retained(t, e, a.ID, 3)}
	want := headJSON(t, e, a.ID)
	restoreAccounts(t, e, "pin0", a.ID)

	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, items...))
	requireHeld(t, e)
	requireEvents(t, e, a.ID, "1t,2t,3t")
	if got := headJSON(t, e, a.ID); got != want {
		t.Fatalf("head after a three-item import =\n%s\nwant\n%s", got, want)
	}
}

// T11 sets min_valid_serial at each renewal, so history stored under serial s
// is superseded once s + 1 is installed. Items under s (superseded, covered by
// the installed delegation) and under s + 1 import in one batch.
func TestImportAfterRenewal(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith() // seq 1 under serial s
	snapshotAll(t, e, "imp")
	e.commitOK(a, a.Suspend(t, ac.ActorOperator).Bytes) // seq 2 under s
	underS := retained(t, e, a.ID, 2)
	e.r.Install(t, e.f.App, e.r.NextDelegation(t))
	e.commitOK(a, a.Unsuspend(t, ac.ActorOperator).Bytes) // seq 3 under s + 1
	underNext := retained(t, e, a.ID, 3)
	want := headJSON(t, e, a.ID)
	rewind(t, e, "imp", a)

	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, underS, underNext))
	requireHeld(t, e)
	requireEvents(t, e, a.ID, "1f,2t,3t")
	if got := headJSON(t, e, a.ID); got != want {
		t.Fatalf("head after import across a renewal =\n%s\nwant\n%s", got, want)
	}
}

// reconcile/import shares the operator prologue (Task 6): a standby is 503
// not_leader and writes nothing.
func TestImportStandbyIsNotLeader(t *testing.T) {
	e := newEnv(t)
	a, it := oneAhead(t, e)
	beginImport(t, e, restorePoint{})
	standby := Hooks{InRecovery: func(context.Context, *sql.Conn) (bool, error) { return true, nil }}
	s := openWith(t, e.f, e.r, standby, &fakeProbe{}, nil)
	_, err := s.ReconcileImport(liveCtx(t), importBatch(t, it))
	wantOutcomeError(t, "standby", err, 503, ac.ErrorNotLeader)
	requireHeld(t, e)
	requireEvents(t, e, a.ID, "1f")
}

// An account already held (by /hold, or by an earlier batch's refusal) still
// takes an authenticated item that links, and stays held: import replays the
// authority's own past results and never clears held, so a held account keeps
// refusing live ops until clear-held resolves it (§3.4; DoR 684). The item may
// move the head's state, which is what clear-held then reads.
func TestImportIntoAHeldHeadKeepsItHeld(t *testing.T) {
	e := newEnv(t)
	a, it := oneAhead(t, e)
	rewind(t, e, "imp", a)
	e.pinHeld(a.ID)
	beginImport(t, e, restorePoint{})
	importOK(t, e, importBatch(t, it))
	requireEvents(t, e, a.ID, "1f,2t")
	requireHeld(t, e, a.ID)
}

// A restored head's own activation floor survives an imported commit while it
// is still in the future at the item's issued_at, exactly as the original
// commit carried it, and an already-passed one is dropped: import neither
// loses nor invents a floor (carryFloor; DoR:1766).
func TestImportCarriesARestoredFloor(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	snapshotAll(t, e, "imp")
	e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes)
	it := retained(t, e, a.ID, 2)
	x := issuedOf(t, it)
	for _, c := range []struct {
		name  string
		floor uint64
		want  sql.NullInt64
	}{
		{"a future floor is carried", x + GPrimeMs, sql.NullInt64{Int64: i64(x + GPrimeMs), Valid: true}},
		{"a passed floor is dropped", x - 1, sql.NullInt64{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rewind(t, e, "imp", a)
			mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET activation_floor_ms = $2 WHERE account_id = $1`, a.ID, i64(c.floor))
			beginImport(t, e, restorePoint{})
			importOK(t, e, importBatch(t, it))
			requireEvents(t, e, a.ID, "1f,2t")
			var got sql.NullInt64
			if err := e.f.App.QueryRow(`SELECT activation_floor_ms FROM authority.account_heads WHERE account_id = $1`, a.ID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("activation_floor_ms = %v, want %v", got, c.want)
			}
		})
	}
}
