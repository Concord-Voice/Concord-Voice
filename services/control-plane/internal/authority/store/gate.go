package store

import (
	"context"
	"database/sql"
	"time"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// ReplicationProbe answers step 12's question on the request's own session.
// The real one is SQLProbe; tests inject a fake (DoR §3.2.3).
type ReplicationProbe interface {
	Replicated(ctx context.Context, conn *sql.Conn, lsn string) (bool, error)
}

// SQLProbe calls authority.replicated (LD-2.2, D-200).
type SQLProbe struct{}

// Replicated asks authority.replicated whether a named synchronous standby
// has confirmed lsn.
func (SQLProbe) Replicated(ctx context.Context, conn *sql.Conn, lsn string) (bool, error) {
	var ok bool
	err := conn.QueryRowContext(ctx, `SELECT authority.replicated($1::pg_lsn)`, lsn).Scan(&ok)
	return ok, err
}

// release is step 12. An unrecorded error passes untouched (it carries no
// state the reply could run ahead of); every other outcome waits for
// replication or is replaced by 503 replication, its bytes dropped.
func (s *Store) release(ctx context.Context, conn *sql.Conn, o Outcome) Outcome {
	if o.Kind == KindError {
		return o
	}
	if ok, err := waitReplicated(ctx, conn, s.probe); err != nil || !ok {
		return s.replicationError()
	}
	return o
}

// waitReplicated is step 12's wait, the one release and the owner verbs'
// finish share so the two cannot diverge (B3 Task 3, D-225). It reads
// meta.replication_required on conn; when that is false it reports true at
// once and never calls probe. Otherwise it reads the session's WAL insert
// LSN, which is at or past every commit conn has made, and polls probe at
// ReleasePollInterval until probe confirms it or ctx ends.
//
// It reports true, with a nil error, on those two paths alone. Every other
// path reports false with its cause and never (false, nil): a meta or LSN
// read that fails, and ctx ending first. A probe error is not a confirmation
// and not a refusal either: the wait keeps polling until ctx ends, as B1's
// release did. Callers treat anything but (true, nil) as not replicated.
func waitReplicated(ctx context.Context, conn *sql.Conn, probe ReplicationProbe) (bool, error) {
	var required bool
	if err := conn.QueryRowContext(ctx, `SELECT replication_required FROM authority.meta WHERE id = 1`).Scan(&required); err != nil {
		return false, err
	}
	if !required {
		return true, nil
	}
	var lsn string
	if err := conn.QueryRowContext(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&lsn); err != nil {
		return false, err
	}
	tick := time.NewTicker(ReleasePollInterval)
	defer tick.Stop()
	for {
		if ok, err := probe.Replicated(ctx, conn, lsn); err == nil && ok {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-tick.C:
		}
	}
}

// replicationError is step 12's 503. It is a non-client 503 on every endpoint,
// so it sets the tick's D-147 leg (B1-D31).
func (s *Store) replicationError() Outcome {
	s.nonClient503.Store(true)
	return errorOutcome(503, ac.ErrorReplication)
}

// inRecovery is step 1a's read on conn: Hooks.InRecovery, or
// pg_is_in_recovery() (B1-D10). serve's role poll shares it (B2-D6).
func (s *Store) inRecovery(ctx context.Context, conn *sql.Conn) (bool, error) {
	return readInRecovery(ctx, conn, s.hooks.InRecovery)
}

// readInRecovery is the role read itself: hook when it is set, otherwise
// pg_is_in_recovery() on conn. The store's step 1a and the owner verbs'
// leader check (B3-D7) both read it here.
func readInRecovery(ctx context.Context, conn *sql.Conn, hook func(context.Context, *sql.Conn) (bool, error)) (bool, error) {
	if hook != nil {
		return hook(ctx, conn)
	}
	var r bool
	err := conn.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&r)
	return r, err
}

// leaderCheck is step 1a on the request's connection. Any error fails closed.
func (s *Store) leaderCheck(ctx context.Context, conn *sql.Conn) (Outcome, bool) {
	inRecovery, err := s.inRecovery(ctx, conn)
	if err != nil {
		s.nonClient503.Store(true)
		return errorOutcome(503, ac.ErrorStore), false
	}
	if inRecovery {
		return errorOutcome(503, ac.ErrorNotLeader), false
	}
	return Outcome{}, true
}
