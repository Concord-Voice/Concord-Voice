package store

import (
	"context"
	"errors"
	"time"
)

// PurgeReport is one Purge run: ledger rows pruned, erased accounts purged,
// and Complete when a batch came back short rather than the budget ending.
type PurgeReport struct {
	Pruned, Purged int
	Complete       bool
}

// Purge is one budgeted run (LD-22): prune_ledger(1000) and purge_erased over
// up to 100 candidates per batch, keyset-paged, until a batch comes back
// short or the 20-s budget ends; then last_purge_at, in both cases (§3.2.4).
// Each function call is its own transaction under the store's timeouts
// (purgeCall), taking L1 inside purge_erased; one the budget or a timeout cuts
// off rolls back whole.
func (s *Store) Purge(ctx context.Context) (PurgeReport, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, s.purgeBudget, errPurgeBudget)
	defer cancel()
	var rep PurgeReport
	// Only this run's own budget ends it as a finished run. A caller's deadline
	// or cancel carries the caller's cause, so it skips the bookkeeping and the
	// purge stays due (review round 3).
	if err := s.purgeBatches(ctx, &rep); err != nil && !errors.Is(context.Cause(ctx), errPurgeBudget) {
		return rep, err
	}
	// The budget may be nearly spent here, so the bookkeeping takes its own bound,
	// the statement timeout (DoR §3.2.1), and never waits forever. It records
	// db_now, the clock the purge leg compares with (§3.2.4). WithoutCancel
	// drops the spent budget and keeps ctx's values.
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), purgeBookkeeping)
	defer bcancel()
	now, err := s.dbNow(bctx, s.db)
	if err != nil {
		return rep, err
	}
	_, err = s.db.ExecContext(bctx, `UPDATE authority.meta SET last_purge_at = $1 WHERE id = 1`, time.UnixMilli(i64(now)).UTC())
	return rep, err
}

const purgeBookkeeping = 5 * time.Second

var errPurgeBudget = errors.New("authority store: purge budget spent")

// purgeBatches runs batches until one comes back short (rep.Complete) or ctx ends.
func (s *Store) purgeBatches(ctx context.Context, rep *PurgeReport) error {
	after := "00000000-0000-0000-0000-000000000000"
	for ctx.Err() == nil {
		var pruned int
		if err := s.purgeCall(ctx, &pruned, `SELECT authority.prune_ledger($1)`, PurgeBatch); err != nil {
			return err
		}
		rep.Pruned += pruned
		cands, err := s.purgeCandidates(ctx, after)
		if err != nil {
			return err
		}
		for _, id := range cands {
			var ok bool
			if err := s.purgeCall(ctx, &ok, `SELECT authority.purge_erased($1)`, id); err != nil {
				return err
			}
			if ok {
				rep.Purged++
			}
			after = id
		}
		if pruned < PurgeBatch && len(cands) < PurgeCandidates {
			rep.Complete = true
			return nil
		}
	}
	return ctx.Err()
}

// purgeCall runs one purge function in its own transaction with the store's
// lock and statement timeouts (B1-D3, DoR §3.2.1), so a call blocked on L1
// fails at 2 s instead of spending the run's budget while it holds meta FOR
// SHARE. The error leaves last_purge_at unset, so the purge stays due.
func (s *Store) purgeCall(ctx context.Context, dst any, q string, arg any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := setLocalTimeouts(ctx, tx); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, q, arg).Scan(dst); err != nil {
		return err
	}
	return tx.Commit()
}

// purgeCandidates: the partial index, filtered to heads that still have an
// event (a primary-key probe), so purged tombstones drop out (D-232).
func (s *Store) purgeCandidates(ctx context.Context, after string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT h.account_id::text FROM authority.account_heads h
		WHERE (h.state = 'erased' OR h.pending_erase) AND h.account_id > $1::uuid
		  AND EXISTS (SELECT 1 FROM authority.account_events e WHERE e.account_id = h.account_id)
		ORDER BY h.account_id LIMIT $2`, after, PurgeCandidates)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
