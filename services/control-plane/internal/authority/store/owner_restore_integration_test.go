package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// Task 8: restore-begin and reconcile-complete (DoR:1729, :1739-1759, :1764,
// :1773, D-171, D-186, D-286; B3-D8). Both take ServeLock FIRST, run as
// authority_owner, and end on finish (D-225). The Owner has no clock hook, so
// "now" below is the database's clock_timestamp(), bracketed before and after
// each verb; the pinned-clock half (an env's Hooks.DBNow) is the Store's read
// of what the verb wrote, as TestPendingActivatesNoEarlierThanGPrime shows.

// ---- helpers ----

// dbMs is the database's clock in ms, the clock the owner verbs read.
func dbMs(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var ms int64
	if err := db.QueryRow(`SELECT floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint`).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	return ms
}

func ownerOver(t *testing.T, f *authoritydb.Fixture, r *authoritytest.Realm, cfg OwnerConfig) *Owner {
	t.Helper()
	cfg.DB = f.Owner
	cfg.Anchors = r.AnchorBytes
	o, err := OpenOwner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// metaRestore is the meta columns the two verbs own.
type metaRestore struct {
	epoch                 int64
	reconciled            bool
	started, completed    sql.NullTime
	backupCommit, backupM sql.NullInt64
}

func readMetaRestore(t *testing.T, db *sql.DB) metaRestore {
	t.Helper()
	var m metaRestore
	if err := db.QueryRow(`SELECT restore_epoch, reconciled, restore_started_at, reconcile_completed_at,
		backup_max_commit_id, backup_max_issued_at_ms FROM authority.meta WHERE id = 1`).
		Scan(&m.epoch, &m.reconciled, &m.started, &m.completed, &m.backupCommit, &m.backupM); err != nil {
		t.Fatal(err)
	}
	return m
}

// expectedBackupPoint is what the verb must record, read here with plain SQL:
// the highest commit_id of account_events, and the highest issued_at_ms over
// BOTH event tables.
func expectedBackupPoint(t *testing.T, db *sql.DB) (commit, issued, accountIssued, deviceIssued uint64) {
	t.Helper()
	var c, a, d sql.NullInt64
	if err := db.QueryRow(`SELECT (SELECT max(commit_id) FROM authority.account_events),
		(SELECT max(issued_at_ms) FROM authority.account_events),
		(SELECT max(issued_at_ms) FROM authority.device_events)`).Scan(&c, &a, &d); err != nil {
		t.Fatal(err)
	}
	if !c.Valid || !a.Valid || !d.Valid {
		t.Fatalf("the fixture has no events (commit %v, account %v, device %v)", c, a, d)
	}
	issued = u64(max(a.Int64, d.Int64))
	return u64(c.Int64), issued, u64(a.Int64), u64(d.Int64)
}

func holdsServeLock(t *testing.T, db *sql.DB) bool {
	t.Helper()
	l, err := AcquireServeLock(context.Background(), db)
	if errors.Is(err, ErrServeLockHeld) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	return false
}

// markImported flags one account event imported with committed_at = at. The
// append-only guards and force_committed_at do not fire under the replica
// role, as the harness's own reset relies on.
func markImported(t *testing.T, db *sql.DB, account string, seq int, imported bool, at time.Time) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	res, err := tx.Exec(`UPDATE authority.account_events SET imported = $3, committed_at = $4
		WHERE account_id = $1 AND seq = $2`, account, seq, imported, at)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("markImported touched %d rows, want 1", n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// markDeviceImported is markImported for one device event.
func markDeviceImported(t *testing.T, db *sql.DB, account string, dseq int, at time.Time) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	res, err := tx.Exec(`UPDATE authority.device_events SET imported = true, committed_at = $3
		WHERE account_id = $1 AND dseq = $2`, account, dseq, at)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("markDeviceImported touched %d rows, want 1", n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

type headFloors struct {
	state               string
	held                bool
	activation, replace sql.NullInt64
	slotFilled          bool
}

func readFloors(t *testing.T, db *sql.DB) map[string]headFloors {
	t.Helper()
	rows, err := db.Query(`SELECT account_id::text, state, held, activation_floor_ms, replacement_floor_ms,
		replacement_unavail_base_ms IS NOT NULL FROM authority.account_heads`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]headFloors{}
	for rows.Next() {
		var id string
		var h headFloors
		if err := rows.Scan(&id, &h.state, &h.held, &h.activation, &h.replace, &h.slotFilled); err != nil {
			t.Fatal(err)
		}
		out[id] = h
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// reconcileFx is a realm holding every shape reconcile-complete must tell
// apart: two pending heads, one active head with a filled recovery-key slot,
// one plain active head, and two held active heads.
type reconcileFx struct {
	e       *env
	o       *Owner
	trust   []byte // the installed delegation, as a trust file
	pending []*authoritytest.Account
	slot    *authoritytest.Account
	plain   *authoritytest.Account
	held    []*authoritytest.Account
}

func newReconcileFx(t *testing.T, cfg OwnerConfig) *reconcileFx {
	t.Helper()
	e := newEnv(t)
	fx := &reconcileFx{e: e, trust: trustFile(t, e.r.Current)}
	for range 2 {
		a, _ := e.enrolledWith()
		e.commitOK(a, a.ReplaceRoot(t, ac.FactorEmail, ac.FactorPassword).Bytes) // pending head, no floor yet
		fx.pending = append(fx.pending, a)
	}
	d, _ := e.enrolledWith()
	bound(t, e, d)
	refresh := authoritytest.NewDevice(t).DSK
	e.deviceOK(d, d.BuildDevice(t, authoritytest.DeviceSpec{Op: ac.OpBindRecovery, BindWait: ac.BindWaitDelta,
		Bind: &ac.RecoveryBind{RealmID: e.r.ID, AccountID: d.ID, Kind: ac.BindKindBind,
			RecoveryKey: authoritytest.Point(refresh), ChainSeq: d.Head.Seq, ChainEntryDigest: d.Head.Entry,
			BindingRootDigest: ac.KeyDigest(d.Head.Root), EscrowPublicKey: authoritytest.Point(authoritytest.NewDevice(t).DSK)},
		BindKey: refresh, Factors: []string{ac.FactorEmail, ac.FactorPassword}}).Bytes) // a pending refresh in the slot
	fx.slot = d
	fx.plain, _ = e.enrolledWith()
	for range 2 {
		h, _ := e.enrolledWith()
		e.pinHeld(h.ID)
		fx.held = append(fx.held, h)
	}
	fx.o = ownerOver(t, e.f, e.r, cfg)
	return fx
}

// ---- restore-begin ----

// DoR:1729's restore-begin row: it installs the trust file first, then sets
// reconciled=false, bumps restore_epoch, stamps restore_started_at, and records
// the backup point: max(account_events.commit_id) and max(issued_at_ms) over
// both event tables. The device table is made to hold the highest issued_at_ms,
// so a verb that reads only account_events records the wrong value.
func TestRestoreBeginRecordsTheBackupPoint(t *testing.T) {
	e := newEnv(t)
	a, _ := e.enrolledWith()
	b, _ := e.enrolledWith()
	e.commitOK(b, b.Suspend(t, ac.ActorUser).Bytes)
	e.advance(5_000)
	bound(t, e, a) // a device event, later than every account event
	wantCommit, wantIssued, accountIssued, deviceIssued := expectedBackupPoint(t, e.f.Super)
	if deviceIssued <= accountIssued {
		t.Fatalf("the fixture's device events (%d) do not outlast its account events (%d): the control is void",
			deviceIssued, accountIssued)
	}
	if wantIssued != deviceIssued {
		t.Fatalf("expectedBackupPoint issued = %d, want the device maximum %d", wantIssued, deviceIssued)
	}

	d1 := e.r.Current
	// Not installed: only the verb's LoadTrust can install it. A fresh RSK, since
	// D-223 refuses an RSK two delegations list.
	d2 := e.r.NextDelegation(t, authoritytest.WithRSKs(freshPoint(t)))
	if got := installedSerials(t, e.f); len(got) != 1 {
		t.Fatalf("installed serials before the verb = %v, want just serial 1", got)
	}
	o := ownerOver(t, e.f, e.r, OwnerConfig{})
	before := dbMs(t, e.f.Super)
	pt, err := o.RestoreBegin(liveCtx(t), trustFile(t, d1, d2))
	after := dbMs(t, e.f.Super)
	if err != nil {
		t.Fatalf("RestoreBegin = %v, want nil", err)
	}

	if pt.Epoch != 1 || pt.BackupMaxCommitID != wantCommit || pt.BackupMaxIssuedMs != wantIssued {
		t.Fatalf("RestorePoint = %+v, want epoch 1, commit %d, issued %d", pt, wantCommit, wantIssued)
	}
	if ms := pt.StartedAt.UnixMilli(); ms < before || ms > after {
		t.Fatalf("StartedAt = %d ms, want within the database clock's [%d, %d]", ms, before, after)
	}
	m := readMetaRestore(t, e.f.Super)
	if m.reconciled || m.epoch != 1 || !m.started.Valid || !m.started.Time.Equal(pt.StartedAt) {
		t.Fatalf("meta = %+v, want reconciled=false, restore_epoch=1, restore_started_at = %v", m, pt.StartedAt)
	}
	if !m.backupCommit.Valid || u64(m.backupCommit.Int64) != wantCommit ||
		!m.backupM.Valid || u64(m.backupM.Int64) != wantIssued {
		t.Fatalf("meta backup point = %v/%v, want %d/%d", m.backupCommit, m.backupM, wantCommit, wantIssued)
	}
	if got := installedSerials(t, e.f); len(got) != 2 || got[1] != 2 {
		t.Fatalf("installed serials after the verb = %v, want the trust file's serial 2 installed too (DoR:1744 step 4)", got)
	}
	if holdsServeLock(t, e.f.Owner) {
		t.Fatal("RestoreBegin returned still holding the serve lock")
	}
}

// A trust file that does not decode installs nothing and changes no meta, so
// the epoch is not spent on a refused start.
func TestRestoreBeginRefusesAnUnusableTrustFile(t *testing.T) {
	e := newEnv(t)
	e.enrolledWith()
	o := ownerOver(t, e.f, e.r, OwnerConfig{})
	before := authoritySnapshot(t, e.f.App)
	if _, err := o.RestoreBegin(liveCtx(t), []byte("not a trust file")); err == nil {
		t.Fatal("RestoreBegin accepted a trust file that does not decode")
	}
	if after := authoritySnapshot(t, e.f.App); after != before {
		t.Fatalf("a refused RestoreBegin wrote:\nbefore %s\nafter  %s", before, after)
	}
	if holdsServeLock(t, e.f.Owner) {
		t.Fatal("a refused RestoreBegin leaked the serve lock")
	}
}

// Review Focus 5, B3-D8: a second run prints the same values and bumps
// nothing.
func TestRestoreBeginIdempotent(t *testing.T) {
	e := newEnv(t)
	e.enrolledWith()
	o := ownerOver(t, e.f, e.r, OwnerConfig{})
	trust := trustFile(t, e.r.Current)
	first, err := o.RestoreBegin(liveCtx(t), trust)
	if err != nil {
		t.Fatal(err)
	}
	snap := authoritySnapshot(t, e.f.App)
	time.Sleep(20 * time.Millisecond) // a re-stamped restore_started_at would differ
	second, err := o.RestoreBegin(liveCtx(t), trust)
	if err != nil {
		t.Fatalf("second RestoreBegin = %v, want nil (B3-D8)", err)
	}
	if first.Epoch != 1 || second.Epoch != 1 {
		t.Fatalf("epochs = %d then %d, want 1 and 1: restore_epoch rises once", first.Epoch, second.Epoch)
	}
	if !second.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("StartedAt = %v then %v, want the same value", first.StartedAt, second.StartedAt)
	}
	if second.BackupMaxCommitID != first.BackupMaxCommitID || second.BackupMaxIssuedMs != first.BackupMaxIssuedMs {
		t.Fatalf("backup point = %+v then %+v, want the same", first, second)
	}
	if after := authoritySnapshot(t, e.f.App); after != snap {
		t.Fatalf("the second RestoreBegin changed the database:\nbefore %s\nafter  %s", snap, after)
	}
}

// B3-D8's skip is "reconciled=false AND restore_started_at is set". An
// unreconciled meta whose restore_started_at is NULL got there through
// reconcile/required, not through restore-begin, so the verb still records the
// restore; and a finished restore (reconciled=true, started_at kept) is
// followed by a new one at the next epoch.
func TestRestoreBeginRecordsARestoreOnlyWhenOneIsNotInProgress(t *testing.T) {
	t.Run("unreconciled by reconcile/required, never begun", func(t *testing.T) {
		e := newEnv(t)
		e.enrolledWith()
		mustExec(t, e.f.Owner, `SELECT authority.enter_unreconciled()`)
		if m := readMetaRestore(t, e.f.Super); m.reconciled || m.started.Valid {
			t.Fatalf("the control: meta = %+v, want unreconciled with no restore_started_at", m)
		}
		pt, err := ownerOver(t, e.f, e.r, OwnerConfig{}).RestoreBegin(liveCtx(t), trustFile(t, e.r.Current))
		if err != nil {
			t.Fatal(err)
		}
		if m := readMetaRestore(t, e.f.Super); pt.Epoch != 1 || m.epoch != 1 || !m.started.Valid {
			t.Fatalf("RestoreBegin = %+v, meta %+v: want epoch 1 and restore_started_at set", pt, m)
		}
	})

	t.Run("a completed restore is followed by a new one", func(t *testing.T) {
		e := newEnv(t)
		e.enrolledWith()
		o := ownerOver(t, e.f, e.r, OwnerConfig{})
		trust := trustFile(t, e.r.Current)
		first, err := o.RestoreBegin(liveCtx(t), trust)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.ReconcileComplete(liveCtx(t)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		second, err := o.RestoreBegin(liveCtx(t), trust)
		if err != nil {
			t.Fatal(err)
		}
		if second.Epoch != first.Epoch+1 || !second.StartedAt.After(first.StartedAt) {
			t.Fatalf("second restore = %+v after %+v, want the next epoch and a later start", second, first)
		}
		if m := readMetaRestore(t, e.f.Super); m.reconciled || m.epoch != 2 {
			t.Fatalf("meta = %+v, want reconciled=false at epoch 2", m)
		}
	})

	// reconcile-complete keeps restore_started_at, so a reconcile/required
	// after a finished restore (DoR:1803, a snapshot rollback) leaves meta
	// unreconciled with the OLD start still set. That is not a restore in
	// progress: restore-begin must record a new one, not hand back the old
	// epoch and backup point.
	t.Run("a completed restore, then reconcile/required", func(t *testing.T) {
		e := newEnv(t)
		e.enrolledWith()
		o := ownerOver(t, e.f, e.r, OwnerConfig{})
		trust := trustFile(t, e.r.Current)
		first, err := o.RestoreBegin(liveCtx(t), trust)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.ReconcileComplete(liveCtx(t)); err != nil {
			t.Fatal(err)
		}
		mustExec(t, e.f.Owner, `SELECT authority.enter_unreconciled()`)
		if m := readMetaRestore(t, e.f.Super); m.reconciled || !m.started.Valid || !m.completed.Valid {
			t.Fatalf("the control: meta = %+v, want unreconciled with the finished restore's start and completion", m)
		}
		time.Sleep(20 * time.Millisecond)
		second, err := o.RestoreBegin(liveCtx(t), trust)
		if err != nil {
			t.Fatal(err)
		}
		if second.Epoch != first.Epoch+1 || !second.StartedAt.After(first.StartedAt) {
			t.Fatalf("restore-begin after a finished restore and reconcile/required = %+v after %+v, "+
				"want the next epoch and a later start, not the finished restore's point", second, first)
		}
	})
}

// Codex round 2, G3: whether a restore is in progress compares no clocks.
// restore-begin clears reconcile_completed_at when it records a restore, and
// only reconcile-complete sets it, so a database clock that stepped between
// the two verbs cannot make a finished restore look open or an open one finished.
func TestRestoreBeginIgnoresAClockStep(t *testing.T) {
	// The clock stepped back after reconcile-complete: the completion stamp is
	// ahead of the next restore's start. The next restore-begin records a restore
	// (reconciled is true) and clears the stamp, so its re-run is the same restore.
	t.Run("a completion stamp ahead of the next start", func(t *testing.T) {
		e := newEnv(t)
		e.enrolledWith()
		o := ownerOver(t, e.f, e.r, OwnerConfig{})
		trust := trustFile(t, e.r.Current)
		if _, err := o.RestoreBegin(liveCtx(t), trust); err != nil {
			t.Fatal(err)
		}
		if _, err := o.ReconcileComplete(liveCtx(t)); err != nil {
			t.Fatal(err)
		}
		mustExec(t, e.f.Super, `UPDATE authority.meta SET reconcile_completed_at = now() + interval '1 hour'`)

		first, err := o.RestoreBegin(liveCtx(t), trust)
		if err != nil {
			t.Fatal(err)
		}
		if m := readMetaRestore(t, e.f.Super); m.completed.Valid || m.epoch != 2 || m.reconciled {
			t.Fatalf("meta = %+v after the second restore began, want epoch 2, unreconciled, reconcile_completed_at NULL", m)
		}
		time.Sleep(20 * time.Millisecond) // a re-stamped restore_started_at would differ
		second, err := o.RestoreBegin(liveCtx(t), trust)
		if err != nil {
			t.Fatal(err)
		}
		if second.Epoch != first.Epoch || !second.StartedAt.Equal(first.StartedAt) ||
			second.BackupMaxCommitID != first.BackupMaxCommitID || second.BackupMaxIssuedMs != first.BackupMaxIssuedMs {
			t.Fatalf("re-run = %+v after %+v, want the same epoch, start and backup point (B3-D8)", second, first)
		}
	})

	// The clock stepped back between restore-begin and reconcile-complete: the
	// completion stamp is BEFORE the restore's start. After a snapshot rollback
	// (reconcile/required), the next restore-begin must record a new restore, not
	// hand back the finished one's point.
	t.Run("a completion stamp behind its own start", func(t *testing.T) {
		e := newEnv(t)
		e.enrolledWith()
		o := ownerOver(t, e.f, e.r, OwnerConfig{})
		trust := trustFile(t, e.r.Current)
		first, err := o.RestoreBegin(liveCtx(t), trust)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.ReconcileComplete(liveCtx(t)); err != nil {
			t.Fatal(err)
		}
		mustExec(t, e.f.Super, `UPDATE authority.meta SET reconcile_completed_at = restore_started_at - interval '1 hour'`)
		mustExec(t, e.f.Owner, `SELECT authority.enter_unreconciled()`)
		time.Sleep(20 * time.Millisecond)

		second, err := o.RestoreBegin(liveCtx(t), trust)
		if err != nil {
			t.Fatal(err)
		}
		if second.Epoch != first.Epoch+1 || !second.StartedAt.After(first.StartedAt) {
			t.Fatalf("restore-begin after a finished restore with a stepped clock = %+v after %+v, "+
				"want the next epoch and a later start, not the finished restore's point", second, first)
		}
	})
}

// ---- the serve lock ----

// DoR:1764: the lock is taken first. While another session holds it the verb
// returns ErrServeLockHeld and writes nothing, the trust file included; the
// same call succeeds once the lock is free.
func TestRestoreBeginAndCompleteRefuseWhileServeHoldsLock(t *testing.T) {
	t.Run("restore-begin", func(t *testing.T) {
		e := newEnv(t)
		e.enrolledWith()
		d1 := e.r.Current
		d2 := e.r.NextDelegation(t, authoritytest.WithRSKs(freshPoint(t))) // a delegation only an install would add (a fresh RSK: D-223)
		o := ownerOver(t, e.f, e.r, OwnerConfig{})
		held, err := AcquireServeLock(liveCtx(t), e.f.Owner)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		before := authoritySnapshot(t, e.f.App)
		_, err = o.RestoreBegin(liveCtx(t), trustFile(t, d1, d2))
		if !errors.Is(err, ErrServeLockHeld) {
			t.Fatalf("RestoreBegin while serve holds the lock = %v, want ErrServeLockHeld", err)
		}
		if after := authoritySnapshot(t, e.f.App); after != before {
			t.Fatalf("a refused RestoreBegin wrote:\nbefore %s\nafter  %s", before, after)
		}
		held.Release()
		if _, err := o.RestoreBegin(liveCtx(t), trustFile(t, d1, d2)); err != nil {
			t.Fatalf("RestoreBegin once the lock is free = %v, want nil (the control)", err)
		}
	})

	t.Run("reconcile-complete", func(t *testing.T) {
		fx := newReconcileFx(t, OwnerConfig{})
		if _, err := fx.o.RestoreBegin(liveCtx(t), fx.trust); err != nil {
			t.Fatal(err)
		}
		held, err := AcquireServeLock(liveCtx(t), fx.e.f.Owner)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		before := authoritySnapshot(t, fx.e.f.App)
		_, err = fx.o.ReconcileComplete(liveCtx(t))
		if !errors.Is(err, ErrServeLockHeld) {
			t.Fatalf("ReconcileComplete while serve holds the lock = %v, want ErrServeLockHeld", err)
		}
		if after := authoritySnapshot(t, fx.e.f.App); after != before {
			t.Fatalf("a refused ReconcileComplete wrote:\nbefore %s\nafter  %s", before, after)
		}
		if m := readMetaRestore(t, fx.e.f.Super); m.reconciled {
			t.Fatal("a refused ReconcileComplete set reconciled=true")
		}
		held.Release()
		if _, err := fx.o.ReconcileComplete(liveCtx(t)); err != nil {
			t.Fatalf("ReconcileComplete once the lock is free = %v, want nil (the control)", err)
		}
		if holdsServeLock(t, fx.e.f.Owner) {
			t.Fatal("ReconcileComplete returned still holding the serve lock")
		}
	})
}

// ---- reconcile-complete ----

// DoR:1729, D-286, D-146: every pending head gets activation_floor_ms = now +
// G', every filled recovery-key slot replacement_floor_ms = now + G'; the
// timestamp is set, reconciled is true, and the counts are the imported events
// of both ledgers since restore_started_at and the held heads. A head with nothing pending and
// a slot that is empty get no floor, and one `now` serves the whole verb.
func TestReconcileCompleteFloors(t *testing.T) {
	fx := newReconcileFx(t, OwnerConfig{})
	e := fx.e
	if _, err := fx.o.RestoreBegin(liveCtx(t), fx.trust); err != nil {
		t.Fatal(err)
	}
	started := readMetaRestore(t, e.f.Super).started.Time
	// Imported events: two account events and one device event (a private-op
	// import writes only that) after the restore began, one from before it (an
	// ordinary imported row of an earlier restore), and one non-imported row
	// that merely committed after it. Only the first three are this restore's.
	later := started.Add(time.Second)
	markImported(t, e.f.Super, fx.pending[0].ID, 1, true, later)
	markImported(t, e.f.Super, fx.plain.ID, 1, true, later)
	markDeviceImported(t, e.f.Super, fx.slot.ID, 1, later)
	markImported(t, e.f.Super, fx.slot.ID, 1, true, started.Add(-time.Hour))
	markImported(t, e.f.Super, fx.held[0].ID, 1, false, later)
	for id, h := range readFloors(t, e.f.Super) {
		if h.activation.Valid || h.replace.Valid {
			t.Fatalf("the control: %s has a floor (%+v) before the verb", id, h)
		}
	}

	before := dbMs(t, e.f.Super)
	counts, err := fx.o.ReconcileComplete(liveCtx(t))
	after := dbMs(t, e.f.Super)
	if err != nil {
		t.Fatalf("ReconcileComplete = %v, want nil", err)
	}
	if counts != (ReconcileCounts{Imported: 3, Held: 2}) {
		t.Fatalf("counts = %+v, want {Imported:3 Held:2}", counts)
	}

	m := readMetaRestore(t, e.f.Super)
	if !m.reconciled {
		t.Fatal("meta.reconciled is still false")
	}
	if !m.completed.Valid {
		t.Fatal("meta.reconcile_completed_at is unset")
	}
	completedMs := m.completed.Time.UnixMilli()
	if completedMs < before || completedMs > after {
		t.Fatalf("reconcile_completed_at = %d ms, want within the database clock's [%d, %d]", completedMs, before, after)
	}

	heads := readFloors(t, e.f.Super)
	var floor int64
	same := func(label string, got sql.NullInt64) {
		t.Helper()
		if !got.Valid {
			t.Fatalf("%s has no floor, want now + G'", label)
		}
		if got.Int64 < before+int64(GPrimeMs) || got.Int64 > after+int64(GPrimeMs) {
			t.Fatalf("%s floor = %d, want now + G' within [%d, %d]", label, got.Int64,
				before+int64(GPrimeMs), after+int64(GPrimeMs))
		}
		if got.Int64 < completedMs+int64(GPrimeMs) {
			t.Fatalf("%s floor = %d is before reconcile_completed_at + G' = %d (D-186)", label, got.Int64,
				completedMs+int64(GPrimeMs))
		}
		if floor == 0 {
			floor = got.Int64
		} else if got.Int64 != floor {
			t.Fatalf("%s floor = %d, want the verb's single now + G' = %d", label, got.Int64, floor)
		}
	}
	for i, a := range fx.pending {
		h := heads[a.ID]
		if h.state != string(ac.StatePending) {
			t.Fatalf("pending[%d] is %q, want pending (the control)", i, h.state)
		}
		same("pending head", h.activation)
		if h.replace.Valid {
			t.Fatalf("pending[%d] got a replacement floor with no slot filled", i)
		}
	}
	if h := heads[fx.slot.ID]; !h.slotFilled {
		t.Fatal("the slot account's recovery-key slot is empty (the control)")
	} else {
		same("filled recovery-key slot", h.replace)
		if h.activation.Valid {
			t.Fatal("the slot account is not pending and got an activation floor")
		}
	}
	for _, a := range append([]*authoritytest.Account{fx.plain}, fx.held...) {
		if h := heads[a.ID]; h.activation.Valid || h.replace.Valid {
			t.Fatalf("a head with nothing pending got floors %+v", h)
		}
	}
	for _, a := range fx.held {
		if !heads[a.ID].held {
			t.Fatal("reconcile-complete cleared a hold: only clear-held resolves one (LD-46)")
		}
	}
	if holdsServeLock(t, e.f.Owner) {
		t.Fatal("ReconcileComplete returned still holding the serve lock")
	}
}

// D-286 is a lower bound, so reconcile-complete only ever raises a floor: one
// already later than now + G' (a D-186 erase floor written under a clock that
// has since stepped back, say) is kept, never lowered to now + G'. Lowering it
// would let that head activate, or that slot complete, earlier than the fence
// already promised.
func TestReconcileCompleteNeverLowersAFloor(t *testing.T) {
	fx := newReconcileFx(t, OwnerConfig{})
	e := fx.e
	if _, err := fx.o.RestoreBegin(liveCtx(t), fx.trust); err != nil {
		t.Fatal(err)
	}
	later := dbMs(t, e.f.Super) + 10*int64(GPrimeMs)
	mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET activation_floor_ms = $2 WHERE account_id = $1`,
		fx.pending[0].ID, later)
	mustExec(t, e.f.Owner, `UPDATE authority.account_heads SET replacement_floor_ms = $2 WHERE account_id = $1`,
		fx.slot.ID, later)
	before := dbMs(t, e.f.Super)
	if _, err := fx.o.ReconcileComplete(liveCtx(t)); err != nil {
		t.Fatal(err)
	}
	after := dbMs(t, e.f.Super)
	heads := readFloors(t, e.f.Super)
	if h := heads[fx.pending[0].ID]; !h.activation.Valid || h.activation.Int64 != later {
		t.Fatalf("a later activation floor became %+v, want it kept at %d", h.activation, later)
	}
	if h := heads[fx.slot.ID]; !h.replace.Valid || h.replace.Int64 != later {
		t.Fatalf("a later slot floor became %+v, want it kept at %d", h.replace, later)
	}
	if h := heads[fx.pending[1].ID]; !h.activation.Valid || h.activation.Int64 < before+int64(GPrimeMs) ||
		h.activation.Int64 > after+int64(GPrimeMs) {
		t.Fatalf("the other pending head's floor = %+v, want now + G' (the control)", h.activation)
	}
}

// B3-D8: already reconciled changes nothing and is not an error.
func TestReconcileCompleteIdempotent(t *testing.T) {
	t.Run("a second run after a restore", func(t *testing.T) {
		fx := newReconcileFx(t, OwnerConfig{})
		if _, err := fx.o.RestoreBegin(liveCtx(t), fx.trust); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.o.ReconcileComplete(liveCtx(t)); err != nil {
			t.Fatal(err)
		}
		snap := authoritySnapshot(t, fx.e.f.App)
		time.Sleep(20 * time.Millisecond) // a re-stamped reconcile_completed_at or floor would differ
		if _, err := fx.o.ReconcileComplete(liveCtx(t)); err != nil {
			t.Fatalf("second ReconcileComplete = %v, want nil (B3-D8)", err)
		}
		if after := authoritySnapshot(t, fx.e.f.App); after != snap {
			t.Fatalf("the second ReconcileComplete changed the database:\nbefore %s\nafter  %s", snap, after)
		}
	})

	t.Run("a realm that was never restored", func(t *testing.T) {
		fx := newReconcileFx(t, OwnerConfig{})
		snap := authoritySnapshot(t, fx.e.f.App)
		if _, err := fx.o.ReconcileComplete(liveCtx(t)); err != nil {
			t.Fatalf("ReconcileComplete on a reconciled realm = %v, want nil", err)
		}
		if after := authoritySnapshot(t, fx.e.f.App); after != snap {
			t.Fatalf("ReconcileComplete wrote to a reconciled realm:\nbefore %s\nafter  %s", snap, after)
		}
		if m := readMetaRestore(t, fx.e.f.Super); m.completed.Valid {
			t.Fatal("ReconcileComplete stamped reconcile_completed_at on a realm that was never restored")
		}
	})
}

// DoR:1773 and D-286, through the Store's own read (the pinned-clock hook is
// env's Hooks.DBNow, e.now): a pending head activates no earlier than the floor
// the verb wrote, and so does a filled recovery-key slot's completion.
func TestPendingActivatesNoEarlierThanGPrime(t *testing.T) {
	fx := newReconcileFx(t, OwnerConfig{})
	e := fx.e
	if _, err := fx.o.RestoreBegin(liveCtx(t), fx.trust); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.o.ReconcileComplete(liveCtx(t)); err != nil {
		t.Fatal(err)
	}
	pend, slot := fx.pending[0], fx.slot
	h := readFloors(t, e.f.Super)
	if !h[pend.ID].activation.Valid || !h[slot.ID].replace.Valid {
		t.Fatalf("the verb set no floors: %+v / %+v", h[pend.ID], h[slot.ID])
	}
	actFloor, slotFloor := u64(h[pend.ID].activation.Int64), u64(h[slot.ID].replace.Int64)
	if actFloor != slotFloor {
		t.Fatalf("activation floor %d and slot floor %d differ: one now + G' serves both", actFloor, slotFloor)
	}
	raw := func() []byte {
		var d []byte
		if err := e.f.App.QueryRow(`SELECT device_set_digest FROM authority.account_heads WHERE account_id = $1`,
			slot.ID).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	slotCompleted := func() bool {
		_, dst, _ := readHead(t, e, slot.ID)
		return !bytes.Equal(dst.DeviceSetDigest, raw())
	}

	e.advance(actFloor - 1 - e.now.Load())
	if got := e.state(pend); got != ac.StatePending {
		t.Fatalf("at floor - 1 the pending head is %q, want pending", got)
	}
	if slotCompleted() {
		t.Fatal("the recovery-key slot completed at floor - 1 (D-286)")
	}
	e.advance(1)
	if got := e.state(pend); got != ac.StateActive {
		t.Fatalf("at the floor the pending head is %q, want active", got)
	}
	if !slotCompleted() {
		t.Fatal("the recovery-key slot did not complete at the floor")
	}
}

// ---- lock order ----

// The operator routes take account_heads rows (L1, ascending) and THEN meta
// (ReconcileRequired locks its heads, then calls enter_unreconciled, which
// writes meta). ReconcileComplete must take them in that order too: meta
// first would deadlock against that transaction (40P01).
//
// The test parks a transaction that holds every head, starts the verb, waits
// until it is blocked on a lock, and then lets the parked transaction reach
// for meta. With the heads taken first the verb has not touched meta, so the
// parked transaction proceeds and the verb finishes behind it. With meta taken
// first, each side waits on the other and the database kills one with 40P01.
func TestReconcileCompleteDoesNotDeadlockWithRequired(t *testing.T) {
	fx := newReconcileFx(t, OwnerConfig{})
	if _, err := fx.o.RestoreBegin(liveCtx(t), fx.trust); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), RequestDeadlineTx)
	defer cancel()

	tx, err := fx.e.f.Owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM authority.account_heads ORDER BY account_id FOR UPDATE`); err != nil {
		t.Fatalf("locking the heads: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := fx.o.ReconcileComplete(ctx)
		done <- err
	}()

	var verbErr error
	finished := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !finished; {
		select {
		case verbErr = <-done:
			finished = true
		default:
		}
		if finished {
			break
		}
		var waiting int
		if err := fx.e.f.Super.QueryRow(`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break // the verb is parked behind the heads this transaction holds
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := tx.ExecContext(ctx, `SELECT authority.enter_unreconciled()`); err != nil {
		t.Fatalf("the ReconcileRequired-shaped transaction failed reaching meta after its heads: %v "+
			"(a deadlock, 40P01, means ReconcileComplete took meta before the heads)", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("committing the ReconcileRequired-shaped transaction: %v", err)
	}
	if !finished {
		select {
		case verbErr = <-done:
		case <-ctx.Done():
			t.Fatal("ReconcileComplete did not finish within the transaction deadline")
		}
	}
	if verbErr != nil {
		t.Fatalf("ReconcileComplete = %v, want nil with no deadlock (40P01)", verbErr)
	}
	if m := readMetaRestore(t, fx.e.f.Super); !m.reconciled || !m.completed.Valid {
		t.Fatalf("meta = %+v after the verb, want reconciled with reconcile_completed_at set", m)
	}
	if h := readFloors(t, fx.e.f.Super)[fx.pending[0].ID]; !h.activation.Valid {
		t.Fatal("the verb finished without flooring the pending head")
	}
}

// A verb that needs serve stopped is cancelled once its lock's session dies,
// and its transaction does not commit: from there a serve on this node could
// take the lock beside it. The body tries to commit after the kill.
func TestStoppedVerbAbortsWhenTheServeLockSessionDies(t *testing.T) {
	e := newLiveEnv(t, OwnerConfig{})
	digest := bytes.Repeat([]byte{7}, 48)
	err := e.o.stopped(liveCtx(t), "test", nil, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO authority.revoked_keys (key_digest, kind) VALUES ($1, 'cak')`,
			digest); err != nil {
			return err
		}
		var gone bool
		if err := e.f.Super.QueryRowContext(ctx, `SELECT pg_terminate_backend(pid, 5000) FROM pg_locks
			WHERE locktype = 'advisory' AND granted AND classid = $1::bigint::oid AND objid = $2::bigint::oid
			AND objsubid = 1`, ServeLockKey>>32, ServeLockKey&0xffffffff).Scan(&gone); err != nil || !gone {
			return fmt.Errorf("terminate the lock's session: %v: %w", gone, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(ServeLockWatchInterval + RolePollInterval + time.Second):
			t.Error("fixture timed out waiting for serve-lock cancellation")
			return errors.New("fixture timed out waiting for serve-lock cancellation")
		}
		if !errors.Is(context.Cause(ctx), ErrServeLockLost) {
			t.Fatalf("callback returned before serve-lock cancellation: cause = %v", context.Cause(ctx))
		}
		return nil
	})
	if !errors.Is(err, ErrServeLockLost) {
		t.Fatalf("stopped after its lock's session died = %v, want ErrServeLockLost", err)
	}
	if _, ok := revokedKind(t, e.f.App, digest); ok {
		t.Fatal("the verb committed after its serve lock's session died")
	}
	if holdsServeLock(t, e.f.Owner) {
		t.Fatal("the serve lock is still held after the verb returned")
	}
}

// ---- the release gate (D-225) ----

// Both verbs end on the replication check. A commit no standby has confirmed
// is ErrNotReplicated, the write stays committed locally, and the re-run
// converges: it changes nothing and ends on the check again.
func TestRestoreBeginEndsOnTheReplicationCheck(t *testing.T) {
	e := newEnv(t)
	e.enrolledWith()
	p := &fakeProbe{}
	o := ownerOver(t, e.f, e.r, OwnerConfig{Probe: p})
	trust := trustFile(t, e.r.Current)
	requireGate(t, e.f, true)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := o.RestoreBegin(ctx, trust); !errors.Is(err, ErrNotReplicated) {
		t.Fatalf("RestoreBegin with no standby confirming = %v, want ErrNotReplicated", err)
	}
	if p.calls.Load() == 0 {
		t.Fatal("RestoreBegin never asked the replication probe")
	}
	m := readMetaRestore(t, e.f.Super)
	if m.reconciled || m.epoch != 1 || !m.started.Valid {
		t.Fatalf("meta = %+v, want the restore committed locally (epoch 1, unreconciled)", m)
	}
	if holdsServeLock(t, e.f.Owner) {
		t.Fatal("RestoreBegin leaked the serve lock on ErrNotReplicated")
	}

	p.ok.Store(true)
	asked := p.calls.Load()
	pt, err := o.RestoreBegin(liveCtx(t), trust)
	if err != nil {
		t.Fatalf("re-run once a standby confirms = %v, want nil", err)
	}
	if p.calls.Load() <= asked {
		t.Fatal("the re-run returned without ending on the replication check")
	}
	if after := readMetaRestore(t, e.f.Super); after.epoch != 1 || !after.started.Time.Equal(m.started.Time) ||
		!pt.StartedAt.Equal(m.started.Time) || pt.Epoch != 1 {
		t.Fatalf("the re-run changed the restore: point %+v, meta %+v then %+v", pt, m, after)
	}
}

func TestReconcileCompleteEndsOnTheReplicationCheck(t *testing.T) {
	p := &fakeProbe{}
	fx := newReconcileFx(t, OwnerConfig{Probe: p})
	if _, err := fx.o.RestoreBegin(liveCtx(t), fx.trust); err != nil { // the gate is still off
		t.Fatal(err)
	}
	requireGate(t, fx.e.f, true)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := fx.o.ReconcileComplete(ctx); !errors.Is(err, ErrNotReplicated) {
		t.Fatalf("ReconcileComplete with no standby confirming = %v, want ErrNotReplicated", err)
	}
	if p.calls.Load() == 0 {
		t.Fatal("ReconcileComplete never asked the replication probe")
	}
	m := readMetaRestore(t, fx.e.f.Super)
	if !m.reconciled || !m.completed.Valid {
		t.Fatalf("meta = %+v, want the completion committed locally", m)
	}
	snap := authoritySnapshot(t, fx.e.f.App)
	if holdsServeLock(t, fx.e.f.Owner) {
		t.Fatal("ReconcileComplete leaked the serve lock on ErrNotReplicated")
	}

	p.ok.Store(true)
	asked := p.calls.Load()
	if _, err := fx.o.ReconcileComplete(liveCtx(t)); err != nil {
		t.Fatalf("re-run once a standby confirms = %v, want nil", err)
	}
	if p.calls.Load() <= asked {
		t.Fatal("the re-run returned without ending on the replication check (the first run's commit may be the one still waiting)")
	}
	if after := authoritySnapshot(t, fx.e.f.App); after != snap {
		t.Fatalf("the converging re-run changed the database:\nbefore %s\nafter  %s", snap, after)
	}
}
