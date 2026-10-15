package store

import (
	"context"
	"database/sql"
	"time"
)

// TickInput is B2's input to Tick: the last authenticated CP contact, an
// in-process atomic the tick persists (D-97).
type TickInput struct{ LastPeerContactMs uint64 }

const tickMs = uint64(30_000)

// tickIncrement is the tick's accumulator legs (§3.2.4, B1-D25).
func tickIncrement(prevTickMs, nowMs uint64, reconciled bool, lastPeerMs uint64, nonClient503 bool) uint64 {
	var add uint64
	if nowMs > prevTickMs && nowMs-prevTickMs > TickGapThresholdMs {
		add += nowMs - prevTickMs - TickGapThresholdMs
	}
	noContact := lastPeerMs+PeerContactWindowMs < nowMs
	if !reconciled || noContact || nonClient503 {
		add += tickMs
	}
	return add
}

func purgeDue(lastPurgeMs, nowMs uint64) bool {
	return lastPurgeMs == 0 || nowMs >= lastPurgeMs+86_400_000
}

// Tick updates meta only (lock order: it never touches a head). B2 calls it
// every 30 s on the leader.
func (s *Store) Tick(ctx context.Context, in TickInput) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := setLocalTimeouts(ctx, tx); err != nil { // B1-D3: every store transaction (review round 2)
		return err
	}
	now, err := s.dbNow(ctx, tx)
	if err != nil {
		return err
	}
	s.pruneAsOf(now) // bounds D-241's map whatever this tick's outcome (review round 3)
	var last sql.NullTime
	var reconciled bool
	if err := tx.QueryRowContext(ctx, `SELECT last_tick_at, reconciled FROM authority.meta WHERE id = 1 FOR UPDATE`).
		Scan(&last, &reconciled); err != nil {
		return err
	}
	// Only a tick that commits consumes the non-client 503 leg (D-147). On any
	// failure the flag is set again; Store(true) never clears a 503 recorded
	// meanwhile, and a commit whose outcome is unknown at worst counts one tick
	// twice, which delays an activation rather than hastening it.
	nonClient503 := s.nonClient503.Swap(false)
	committed := false
	defer func() {
		if nonClient503 && !committed {
			s.nonClient503.Store(true)
		}
	}()
	add := tickIncrement(msOfNull(last), now, reconciled, in.LastPeerContactMs, nonClient503)
	if _, err := tx.ExecContext(ctx, `UPDATE authority.meta SET last_tick_at = $1, last_peer_contact_at = $2,
		cancel_unavailable_ms = cancel_unavailable_ms + $3 WHERE id = 1`,
		time.UnixMilli(i64(now)).UTC(), nullMs(in.LastPeerContactMs), i64(add)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}
