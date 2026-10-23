package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// restore-begin and reconcile-complete (§3.4, DoR:1726, :1729, :1739–1759;
// D-171, D-286, B3-D8): the two owner verbs that bracket a restore, and the
// only two that need serve stopped without signing. Each takes ServeLock
// first and holds it for the whole verb, so no serve on this node commits,
// signs or ticks between its reads and its commit; ErrServeLockHeld passes
// through and nothing is written. Then the leader check on the verb's own
// connection (B3-D7), one transaction, and finish (D-225).
//
// reconcile-complete's invariant (D-286, D-146, DoR:1773): once it commits,
// no pending head activates and no filled recovery-key slot completes before
// reconcile_completed_at + G′. It holds because the verb, holding every head
// and then meta, reads one db_now T and in the same transaction raises every
// pending head's activation_floor_ms and every filled slot's
// replacement_floor_ms to at least T + G′, stamps reconcile_completed_at = T
// and sets reconciled. Normalize (dueOf) takes each floor as the lower bound
// of the instant it reads, and a held head does not normalize at all. A floor
// only ever rises here: one already later, such as a D-186 erase floor
// written under a clock that has since stepped back, is kept, never lowered.
// What breaks it: a floor below T + G′; a pending head or filled slot the
// updates miss; T read before a lock wait rather than after every lock, so the
// floor precedes the commit it fences; reconciled set in another transaction
// than the floors; or a serve committing a fresh pending head between the
// floors and the commit, which ServeLock rules out on this node.
//
// Its lock order is L1's then meta, as reconcile/required's (heads through
// lockHead, then enter_unreconciled's UPDATE of meta): every head in ascending
// account_id, then meta FOR UPDATE, the already-reconciled read included.
// Meta first would deadlock against that transaction (40P01).
//
// restore-begin's invariant (B3-D8, Review Focus 5): restore_epoch rises once
// per restore. A restore is in progress while meta is unreconciled, has a
// restore_started_at, and has completed no reconcile since that start; a run
// in that state returns the recorded point and writes nothing, so a re-run
// neither bumps the epoch nor moves the backup point. The third clause is
// what keeps a finished restore from passing for an open one: reconcile-
// complete keeps restore_started_at, so a reconcile/required after it
// (DoR:1803, a snapshot rollback) leaves meta unreconciled with the old start
// still set, and without the clause restore-begin would hand back the old
// restore's epoch and backup point instead of recording this one.

// RestorePoint is what restore-begin recorded (DoR:1726): the restore epoch,
// the highest account_events.commit_id and the highest issued_at_ms over both
// event tables at that moment (0 for an empty table), and restore_started_at.
type RestorePoint struct {
	Epoch, BackupMaxCommitID, BackupMaxIssuedMs uint64
	StartedAt                                   time.Time
}

// ReconcileCounts is what reconcile-complete prints (B3-D9): the events
// imported since restore_started_at into either ledger, account_events and
// device_events, and the heads still held. A private-op import writes only a
// device event; a published item with an attached device entry counts once in
// each ledger.
type ReconcileCounts struct{ Imported, Held uint64 }

// stopped is the shape of an owner verb that needs serve stopped (D-171,
// DoR:1764): ServeLock first, on its own connection, released on every path
// once the verb, finish included, is done; then writeAfter, prepare and body
// included, under the lock's watch (ServeLock.Watch). A lock session that
// dies mid-verb cancels the verb, so it does not go on beside a serve that
// took the lock after it, and its error then carries ErrServeLockLost.
// ErrServeLockHeld is returned as it is.
func (o *Owner) stopped(ctx context.Context, verb string, prepare func(ctx context.Context, conn *sql.Conn) error,
	body func(ctx context.Context, tx *sql.Tx) error) error {
	lock, err := AcquireServeLock(ctx, o.db)
	if errors.Is(err, ErrServeLockHeld) {
		return err
	}
	if err != nil {
		return fmt.Errorf("authority store: %s: serve lock: %w", verb, err)
	}
	defer lock.Release()
	ctx, stop := lock.Watch(ctx)
	defer stop() // before Release: the watch pings the lock's connection
	return lockLost(ctx, o.writeAfter(ctx, verb, prepare, body))
}

// RestoreBegin is restore-begin (DoR:1726, restore step 5). Holding ServeLock,
// on a leader, it first installs trustFile as serve's LoadTrust installs one
// (DoR:1744 step 4), on the verb's own connection: a file that does not
// verify, or that the installed trust refuses, is an error and nothing is
// written. Then, unless a restore is in progress, it sets reconciled = false,
// restore_epoch + 1, restore_started_at = clock_timestamp() and the backup
// point, and returns what it recorded; while one is in progress it returns
// the recorded point unchanged (B3-D8). Either way it ends on finish.
func (o *Owner) RestoreBegin(ctx context.Context, trustFile []byte) (RestorePoint, error) {
	install := func(ctx context.Context, conn *sql.Conn) error {
		// loadTrust reads only anchors and hooks of its Store: no pool, no
		// signer, and with no DBNow hook db_now is the database's clock.
		if _, err := (&Store{anchors: o.anchors}).loadTrust(ctx, conn, trustFile, true); err != nil {
			return fmt.Errorf("authority store: restore-begin: trust: %w", err)
		}
		return nil
	}
	var pt RestorePoint
	err := o.stopped(ctx, "restore-begin", install, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		pt, err = beginRestore(ctx, tx)
		return err
	})
	if err != nil {
		return RestorePoint{}, err
	}
	return pt, nil
}

// beginRestore is restore-begin's transaction. Its first statement locks meta.
func beginRestore(ctx context.Context, tx *sql.Tx) (RestorePoint, error) {
	var reconciled bool
	var epoch int64
	var started, completed sql.NullTime
	var commitID, issued sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT reconciled, restore_epoch, restore_started_at, reconcile_completed_at,
		backup_max_commit_id, backup_max_issued_at_ms FROM authority.meta WHERE id = 1 FOR UPDATE`).
		Scan(&reconciled, &epoch, &started, &completed, &commitID, &issued)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return RestorePoint{}, errors.New("authority store: restore-begin: meta has no row")
	case err != nil:
		return RestorePoint{}, fmt.Errorf("authority store: restore-begin: meta: %w", err)
	}
	if restoreInProgress(reconciled, started, completed) {
		return pointOf(epoch, started, commitID, issued), nil
	}
	if err := tx.QueryRowContext(ctx, `UPDATE authority.meta SET reconciled = false,
		restore_epoch = restore_epoch + 1, restore_started_at = clock_timestamp(),
		backup_max_commit_id = (SELECT COALESCE(max(commit_id), 0) FROM authority.account_events),
		backup_max_issued_at_ms = GREATEST((SELECT COALESCE(max(issued_at_ms), 0) FROM authority.account_events),
			(SELECT COALESCE(max(issued_at_ms), 0) FROM authority.device_events))
		WHERE id = 1 RETURNING restore_epoch, restore_started_at, backup_max_commit_id, backup_max_issued_at_ms`).
		Scan(&epoch, &started, &commitID, &issued); err != nil {
		return RestorePoint{}, fmt.Errorf("authority store: restore-begin: record: %w", err)
	}
	return pointOf(epoch, started, commitID, issued), nil
}

// restoreInProgress is B3-D8's skip: unreconciled, begun, and no reconcile
// completed since the start (see the invariant above).
func restoreInProgress(reconciled bool, started, completed sql.NullTime) bool {
	return !reconciled && started.Valid && (!completed.Valid || completed.Time.Before(started.Time))
}

func pointOf(epoch int64, started sql.NullTime, commitID, issued sql.NullInt64) RestorePoint {
	return RestorePoint{Epoch: u64(epoch), BackupMaxCommitID: u64(commitID.Int64),
		BackupMaxIssuedMs: u64(issued.Int64), StartedAt: started.Time}
}

// ReconcileComplete is reconcile-complete (DoR:1729, restore step 9): holding
// ServeLock, on a leader, it takes every head and then meta, and while
// unreconciled floors every pending head and every filled recovery-key slot at
// T + G′, stamps reconcile_completed_at = T and sets reconciled, T being one
// db_now read after those locks. Already reconciled, it changes nothing
// (B3-D8). Either way it returns the counts and ends on finish, since a first
// run's commit may be the one a standby has not confirmed.
func (o *Owner) ReconcileComplete(ctx context.Context) (ReconcileCounts, error) {
	var counts ReconcileCounts
	err := o.stopped(ctx, "reconcile-complete", nil, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		counts, err = completeReconcile(ctx, tx)
		return err
	})
	if err != nil {
		return ReconcileCounts{}, err
	}
	return counts, nil
}

// completeReconcile is reconcile-complete's transaction, in the lock order
// above: every head, then meta.
func completeReconcile(ctx context.Context, tx *sql.Tx) (ReconcileCounts, error) {
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM authority.account_heads ORDER BY account_id FOR UPDATE`); err != nil {
		return ReconcileCounts{}, fmt.Errorf("authority store: reconcile-complete: heads: %w", err)
	}
	var reconciled bool
	var started sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT reconciled, restore_started_at FROM authority.meta WHERE id = 1 FOR UPDATE`).
		Scan(&reconciled, &started)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ReconcileCounts{}, errors.New("authority store: reconcile-complete: meta has no row")
	case err != nil:
		return ReconcileCounts{}, fmt.Errorf("authority store: reconcile-complete: meta: %w", err)
	}
	if !reconciled {
		if err := floorAndComplete(ctx, tx); err != nil {
			return ReconcileCounts{}, err
		}
	}
	return reconcileCounts(ctx, tx, started)
}

// floorAndComplete reads T, once, and writes every floor and the completion
// from it. The caller holds every head and meta.
func floorAndComplete(ctx context.Context, tx *sql.Tx) error {
	now, err := clockMs(ctx, tx)
	if err != nil {
		return fmt.Errorf("authority store: reconcile-complete: db_now: %w", err)
	}
	floor := i64(now + GPrimeMs)
	// "pending" is the state that carries an activation (ah_pending) and the
	// only one dueOf activates; a disputed head never activates on the clock.
	if _, err := tx.ExecContext(ctx, `UPDATE authority.account_heads
		SET activation_floor_ms = GREATEST(COALESCE(activation_floor_ms, 0), $1) WHERE state = 'pending'`, floor); err != nil {
		return fmt.Errorf("authority store: reconcile-complete: activation floors: %w", err)
	}
	// A filled slot is exactly one whose replacement_unavail_base_ms is set (ah_slot).
	if _, err := tx.ExecContext(ctx, `UPDATE authority.account_heads
		SET replacement_floor_ms = GREATEST(COALESCE(replacement_floor_ms, 0), $1)
		WHERE replacement_unavail_base_ms IS NOT NULL`, floor); err != nil {
		return fmt.Errorf("authority store: reconcile-complete: slot floors: %w", err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE authority.meta SET reconciled = true, reconcile_completed_at = $1 WHERE id = 1`,
		time.UnixMilli(i64(now)).UTC())
	if err != nil {
		return fmt.Errorf("authority store: reconcile-complete: complete: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return errors.New("authority store: reconcile-complete: meta has no row")
	}
	return nil
}

// reconcileCounts is B3-D9's two numbers: account and device events imported
// at or after started (none when no restore has begun), and held heads.
func reconcileCounts(ctx context.Context, tx *sql.Tx, started sql.NullTime) (ReconcileCounts, error) {
	var imported, held int64
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM authority.account_events WHERE imported AND committed_at >= $1)
			+ (SELECT count(*) FROM authority.device_events WHERE imported AND committed_at >= $1),
		(SELECT count(*) FROM authority.account_heads WHERE held)`, started).Scan(&imported, &held); err != nil {
		return ReconcileCounts{}, fmt.Errorf("authority store: reconcile-complete: counts: %w", err)
	}
	return ReconcileCounts{Imported: u64(imported), Held: u64(held)}, nil
}
