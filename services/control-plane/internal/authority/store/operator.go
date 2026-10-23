package store

import (
	"context"
	"database/sql"
	"fmt"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// operatorRun is the prologue the four operator routes share (DoR:196, :1431,
// :1596), then the route's write and step 12's release. An operator route is
// not a user op, so it takes Submit's steps 1, 1a and 2b without the parts that
// belong to ops:
//
//  1. A general mutating slot, never the cancel or the enroll reserve: those
//     are kept for the ops that name them (D-271, D-179). No slot free is 503
//     busy. The body decodes under the slot, as Submit's does (review round 2).
//  2. No commit-bucket token. The bucket meters user ops (D-211), and a drained
//     bucket must not shut the CP out of restore detection or the population.
//  3. The transaction deadline, then the leader check on the route's own
//     connection: a standby is 503 not_leader.
//  4. This request's own hard-floor Statfs, a Statfs error included: 503 disk.
//     The soft floor (disk_ready) is an op's refusal and is not read here.
//  5. write: the route's state checks and its write, in a transaction it
//     commits.
//  6. The release gate. A commit no standby confirmed is 503 replication; the
//     write stays committed locally, and each route converges on the CP's retry.
//
// The refusals of 1, 3, 4 and 6 are OutcomeError, so the server puts each on
// the wire with its own code instead of flattening it to 503 store. leaderCheck,
// hardFloorCheck and release have already set the tick's non-client leg where
// the refusal warrants it. Every other error, decode's and write's, passes as
// it is.
//
// The one lock order (B3 Task 6 rework). A transaction that locks both takes
// the account heads (L1) first, in ascending account_id (DoR:1465), and meta
// after them; none takes meta and then a head, which would deadlock (40P01)
// against one that does the reverse:
//
//   - reconcile/required: the heads (lockHead), then enter_unreconciled's
//     UPDATE of meta;
//   - reconcile-complete: every head, then meta FOR UPDATE;
//   - Purge: L1 (purgeAccount), then purge_erased's meta FOR SHARE and its
//     re-take of the L1 already held. purge_erased alone takes meta first, and
//     000001 is not edited, so the caller's L1 is what puts it in this order;
//   - reconcile/hold: every head (aheadBundles), then meta FOR SHARE, as
//     reconcile/import takes its one head and then meta FOR SHARE;
//   - heads and no meta lock: the account and device commits and
//     restore-cancel (one head; meta read without a lock);
//   - meta and no head: tick, seal-legacy, restore-begin, and the population
//     insert (the guard's meta FOR SHARE).
//
// TestPurgeAndRequiredDoNotDeadlock pins both directions against the purge.
func operatorRun[T any](ctx context.Context, s *Store, verb string, body []byte, decode func([]byte) (T, error),
	write func(context.Context, *sql.Conn, T) error) error {
	rel, ok := s.adm.acquireGeneral()
	if !ok {
		return OutcomeError{errorOutcome(503, ac.ErrorBusy)}
	}
	defer rel()
	v, err := decode(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.txDL)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("authority store: %s: connection: %w", verb, err)
	}
	defer func() { _ = conn.Close() }()
	if out, ok := s.leaderCheck(ctx, conn); !ok {
		return OutcomeError{out}
	}
	if out, ok := s.hardFloorCheck(); !ok {
		return OutcomeError{out}
	}
	if err := write(ctx, conn, v); err != nil {
		return err
	}
	if out := s.release(ctx, conn, Outcome{HTTP: 200, Kind: KindOK}); out.Kind == KindError {
		return OutcomeError{out}
	}
	return nil
}

// operatorTx runs fn in one READ COMMITTED transaction on conn under B1-D3's
// local timeouts, and commits only when fn returns nil. Every other path rolls
// back, so a refusal fn returns writes nothing.
func operatorTx(ctx context.Context, conn *sql.Conn, verb string, fn func(*sql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("authority store: %s: begin: %w", verb, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := setLocalTimeouts(ctx, tx); err != nil {
		return fmt.Errorf("authority store: %s: timeouts: %w", verb, err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("authority store: %s: commit: %w", verb, err)
	}
	return nil
}
