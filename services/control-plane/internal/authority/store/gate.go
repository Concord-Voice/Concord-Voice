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
	var required bool
	if err := conn.QueryRowContext(ctx, `SELECT replication_required FROM authority.meta WHERE id = 1`).Scan(&required); err != nil {
		return s.replicationError()
	}
	if !required {
		return o
	}
	var lsn string
	if err := conn.QueryRowContext(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&lsn); err != nil {
		return s.replicationError()
	}
	tick := time.NewTicker(ReleasePollInterval)
	defer tick.Stop()
	for {
		if ok, err := s.probe.Replicated(ctx, conn, lsn); err == nil && ok {
			return o
		}
		select {
		case <-ctx.Done():
			return s.replicationError()
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
	if s.hooks.InRecovery != nil {
		return s.hooks.InRecovery(ctx, conn)
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
