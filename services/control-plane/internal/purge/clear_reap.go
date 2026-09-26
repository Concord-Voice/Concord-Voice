package purge

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmvisibility"
)

// ClearReason records deletion by the DM clear reap (#3462).
const ClearReason = "clear"

// ClearReapMaxBatch caps rows per clear-reap batch. It is provisional and is
// fixed by the spec §12.6 measurement (p99 batch wall time <= 250 ms) (D5).
const ClearReapMaxBatch = 1000

// ClearReapPlan is the only input to the clear reap. The caller names a
// conversation and nothing else: no cutoff, table, author or context (I1).
type ClearReapPlan struct{ ConversationID string }

// ClearReapOutcome says what one batch found.
type ClearReapOutcome int

// Clear-reap outcomes. Gone and NotEligible delete nothing and write no evidence.
const (
	ClearReapGone        ClearReapOutcome = iota // conversation row absent
	ClearReapNotEligible                         // is_personal, or a current participant has no Clear range
	ClearReapReaped                              // eligible; DeletedCount may be 0 if every candidate was SKIP LOCKED
)

// ClearReapResult reports one batch. More means the batch was full, so the
// caller may call again. PurgeID is "" unless DeletedCount > 0.
type ClearReapResult struct {
	Outcome      ClearReapOutcome
	PurgeID      string
	DeletedCount int
	More         bool
}

var errClearReapPlan = errors.New("purge: clear reap plan requires a non-nil conversation UUID")

const (
	clearReapLockConversation = `SELECT is_personal, is_group FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`
	// A separate statement from the lock, on purpose: under READ COMMITTED each
	// statement takes a fresh snapshot, so this one sees every participant and
	// Clear range committed before the lock was granted. Folding it into the
	// locking statement would evaluate the participants under the snapshot taken
	// BEFORE a lock wait, and miss an AddMember that committed during it.
	clearReapWatermark = `SELECT w.participants, w.uncleared, w.watermark
  FROM dm_conversations c CROSS JOIN LATERAL (` + dmvisibility.ClearWatermarkLateral + `) w
 WHERE c.id = $1`
	// dm_messages only: no messages-table variant exists, so a channel reap is
	// structurally impossible. No pinned_at predicate (I10).
	clearReapSelectBelow = `SELECT id FROM dm_messages
WHERE conversation_id = $1 AND created_at < $2::timestamptz
ORDER BY created_at, id LIMIT $3 FOR UPDATE SKIP LOCKED`
	clearReapSelectAll = `SELECT id FROM dm_messages
WHERE conversation_id = $1
ORDER BY created_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`
	clearReapAudit = `INSERT INTO message_purges
    (actor_id, context_type, context_id, server_id, target_user_id, range_from, range_to,
     reason, status, deleted_count, completed_at)
VALUES (NULL, $1, $2, NULL, NULL, NULL, $3, $4, 'completed', $5, NOW())
RETURNING id`
)

// RunClearReapBatch deletes at most one batch of DM messages that every
// current participant has cleared (#3462). It derives W itself, inside the
// conversation lock, on every call (never cached: an AddMember can lower W).
// Evidence is written in the same transaction and only when rows were
// deleted, so the audit exists if and only if the deletion committed (I9).
//
// INVARIANT. A row is deleted only if, in a snapshot taken after this
// transaction was granted the conversation's FOR NO KEY UPDATE lock, the row's
// created_at is below every current participant's latest Clear cutoff — or the
// conversation has no current participant. It holds because every writer that
// could lower W (AddMember) or add a row below it (send, call event) takes the
// same parent lock, so it either committed before the watermark statement's
// snapshot or waits for this commit. Reading W before the lock, caching it
// across batches, or a participant insert that skips the parent lock each
// breaks it; the failure is deleting history a newcomer can still read.
func (e *Engine) RunClearReapBatch(ctx context.Context, p ClearReapPlan) (ClearReapResult, error) {
	conversationID, err := uuid.Parse(p.ConversationID)
	if err != nil || conversationID == uuid.Nil {
		return ClearReapResult{}, errClearReapPlan
	}
	stride := min(e.maxBatch, ClearReapMaxBatch)

	tx, err := e.beginClearReapTx(ctx)
	if err != nil {
		return ClearReapResult{}, err
	}
	defer e.rollbackClearReapTx(tx)

	if e.beforeClearLockHook != nil {
		e.beforeClearLockHook()
	}
	group, outcome, proceed, err := lockClearReapConversationTx(ctx, tx, conversationID)
	if err != nil || !proceed {
		return ClearReapResult{Outcome: outcome}, err
	}

	w, eligible, err := readClearWatermarkTx(ctx, tx, conversationID)
	if err != nil {
		return ClearReapResult{}, err
	}
	if !eligible {
		return ClearReapResult{Outcome: ClearReapNotEligible}, nil
	}
	if e.afterClearWatermarkHook != nil {
		e.afterClearWatermarkHook(tx)
	}

	ids, err := selectClearReapVictims(ctx, tx, conversationID, w, stride)
	if err != nil {
		return ClearReapResult{}, err
	}
	if len(ids) == 0 {
		return ClearReapResult{Outcome: ClearReapReaped}, nil // rollback; no audit row (D1)
	}

	deleted, refs, err := e.deleteMessagesTx(ctx, tx, deleteQueries["dm_messages"], ids)
	if err != nil {
		return ClearReapResult{}, err
	}
	purgeID, err := writeClearReapAuditTx(ctx, tx, conversationID, group, w, deleted)
	if err != nil {
		return ClearReapResult{}, err
	}
	if err := tx.Commit(); err != nil {
		// Ambiguous commit: do not enqueue. SweepStragglers recovers soft-deleted
		// media if the commit did land (I8). A deadline that already rolled the
		// transaction back surfaces here as a bare sql.ErrTxDone; joining the
		// context error keeps it classifiable as the per-batch deadline.
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = errors.Join(ctxErr, err)
		}
		return ClearReapResult{}, fmt.Errorf("purge: commit clear reap batch: %w", err)
	}
	e.EnqueueBlobDeletes(refs)
	return ClearReapResult{Outcome: ClearReapReaped, PurgeID: purgeID, DeletedCount: deleted, More: len(ids) == stride}, nil
}

// beginClearReapTx opens a batch transaction whose lock waits are bounded, so
// a stuck parent or media lock costs one batch, not a wedged worker.
func (e *Engine) beginClearReapTx(ctx context.Context) (*sql.Tx, error) {
	// Pinned, not inherited: W must be read in a snapshot taken AFTER the
	// parent-lock wait. Under a REPEATABLE READ (or SERIALIZABLE) role/database
	// default the snapshot is fixed at the first statement, so an AddMember that
	// committed during the wait would be invisible and the batch would delete
	// history the newcomer can read (#3462 red-team H1).
	tx, err := e.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("purge: begin clear reap tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		e.rollbackClearReapTx(tx)
		return nil, fmt.Errorf("purge: set clear reap lock timeout: %w", err)
	}
	return tx, nil
}

// lockClearReapConversationTx takes the conversation parent lock. proceed is
// false when the batch must stop without error: a missing row is Gone, and a
// personal conversation is never reaped (I5).
func lockClearReapConversationTx(ctx context.Context, tx *sql.Tx, conversationID uuid.UUID) (group bool, outcome ClearReapOutcome, proceed bool, err error) {
	var personal bool
	err = tx.QueryRowContext(ctx, clearReapLockConversation, conversationID).Scan(&personal, &group)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, ClearReapGone, false, nil
	case err != nil:
		return false, ClearReapGone, false, fmt.Errorf("purge: lock clear reap conversation: %w", err)
	case personal: // I5
		return false, ClearReapNotEligible, false, nil
	default:
		return group, ClearReapReaped, true, nil
	}
}

// rollbackClearReapTx ends a batch that did not commit. The error is not
// logged: a driver error can carry statement detail (I6).
func (e *Engine) rollbackClearReapTx(tx *sql.Tx) {
	if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		e.log.Warn("purge: failed to rollback clear reap transaction", "error_class", ErrorClass(rollbackErr))
	}
}

// ErrorClass maps err to a closed set of labels that are safe to log. Error
// text is not: a storage error names the object key, which embeds the file ID,
// and a driver error can carry statement detail (#3462 I6). A PostgreSQL error
// is labelled by its SQLSTATE, which is a fixed code.
func ErrorClass(err error) string {
	var pqErr *pq.Error
	var netErr net.Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &pqErr):
		return "sqlstate_" + string(pqErr.Code)
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, driver.ErrBadConn), errors.Is(err, sql.ErrConnDone):
		return "bad_conn"
	case errors.Is(err, sql.ErrTxDone):
		return "tx_done"
	case errors.Is(err, sql.ErrNoRows):
		return "no_rows"
	case errors.As(err, &netErr):
		return "network"
	default:
		return "other"
	}
}

// readClearWatermarkTx computes W from the shared fragment. The caller must
// already hold the conversation lock; W is never cached across batches.
func readClearWatermarkTx(ctx context.Context, tx *sql.Tx, conversationID uuid.UUID) (dmvisibility.ClearWatermark, bool, error) {
	var participants, uncleared int
	var raw sql.NullTime
	if err := tx.QueryRowContext(ctx, clearReapWatermark, conversationID).Scan(&participants, &uncleared, &raw); err != nil {
		return dmvisibility.ClearWatermark{}, false, fmt.Errorf("purge: compute clear watermark: %w", err)
	}
	w, eligible, err := dmvisibility.DecideClearWatermark(participants, uncleared, raw)
	if err != nil {
		return dmvisibility.ClearWatermark{}, false, fmt.Errorf("purge: decide clear watermark: %w", err)
	}
	return w, eligible, nil
}

// writeClearReapAuditTx records one committed-with-it batch (§4). range_to is
// the same W that bounded the victim select, so the evidence matches the
// deletion bound exactly.
func writeClearReapAuditTx(ctx context.Context, tx *sql.Tx, conversationID uuid.UUID, group bool, w dmvisibility.ClearWatermark, deleted int) (string, error) {
	contextType := ContextDM
	if group {
		contextType = ContextGroup
	}
	var rangeTo any // NULL for the unbounded, zero-participant case (D9)
	if !w.Unbounded {
		rangeTo = w.At
	}
	var purgeID string
	if err := tx.QueryRowContext(ctx, clearReapAudit, string(contextType), conversationID, rangeTo, ClearReason, deleted).Scan(&purgeID); err != nil {
		return "", fmt.Errorf("purge: write clear reap audit: %w", err)
	}
	return purgeID, nil
}

// selectClearReapVictims locks at most stride rows below W, skipping any row
// another transaction holds so the reap never waits on a message lock (§10).
func selectClearReapVictims(ctx context.Context, tx *sql.Tx, conversationID uuid.UUID, w dmvisibility.ClearWatermark, stride int) ([]string, error) {
	var rows *sql.Rows
	var err error
	if w.Unbounded {
		rows, err = tx.QueryContext(ctx, clearReapSelectAll, conversationID, stride)
	} else {
		rows, err = tx.QueryContext(ctx, clearReapSelectBelow, conversationID, w.At, stride)
	}
	if err != nil {
		return nil, fmt.Errorf("purge: select clear reap victims: %w", err)
	}
	ids, err := ScanIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("purge: read clear reap victims: %w", err)
	}
	return ids, nil
}

// ScanIDs drains a one-column result set of IDs and closes it. It is shared
// with the DM clear reap sweeper's discovery query. Errors are wrapped, never
// replaced, so a caller can still recover the SQLSTATE with errors.As.
func ScanIDs(rows *sql.Rows) (ids []string, returnErr error) {
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			ids, returnErr = nil, errors.Join(returnErr, fmt.Errorf("close rows: %w", closeErr))
		}
	}()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}
	return ids, nil
}
