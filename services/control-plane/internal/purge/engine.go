// Package purge implements the store-agnostic message-purge engine (#1352):
// permission-gated bulk deletion of channel / server / DM / group-DM messages,
// scoped by an optional time range and optional target author, with attachment
// reaping and a privacy-safe audit trail.
//
// The engine is deliberately store-agnostic: handlers do all context-specific
// authorization, build a Plan, and call Run. Table/column identifiers are never
// taken from user input — they come only from the fixed allowedDeleteSpecs
// allow-list and are validated by validateIdentifiers before any SQL is built.
// All VALUES flow through parameterized placeholders ($1..$4).
package purge

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// ContextType is the audited purge context.
type ContextType string

// Purge context types recorded in the message_purges audit row.
const (
	ContextChannel ContextType = "channel"
	ContextServer  ContextType = "server"
	ContextDM      ContextType = "dm"
	ContextGroup   ContextType = "group"
)

// DeleteSpec describes rows to hard-delete. Author nil = all authors; set = only that author.
type DeleteSpec struct {
	MessagesTable    string  // "messages" | "dm_messages"
	ScopeColumn      string  // "channel_id" | "conversation_id"
	ScopeID          string  // the channel_id / conversation_id value
	AttachmentsTable string  // "message_attachments" | "dm_message_attachments"
	Author           *string // nil = all authors; set = only this user_id
}

// Plan is built by a handler AFTER authorization. RangeFrom nil = All Time.
type Plan struct {
	ContextType ContextType
	ContextID   string  // channel_id | server_id | conversation_id (audit)
	ServerID    *string // set for channel/server
	ActorID     string
	Target      *string      // audit target_user_id (nil in #1352 manual; set by #1353)
	Reason      string       // "manual" | "ban" | "kick"
	RangeFrom   *time.Time   // nil = All Time
	Deletes     []DeleteSpec // >=1 (server purge = one per channel)
}

// Result is returned to the handler for the HTTP 200 body + WS payload.
type Result struct {
	PurgeID      string
	DeletedCount int
	// HiddenCount is left 0 by the engine; PurgeConversation assigns it after the
	// receiver-hide and sources both the JSON body and FinalizeHidden from it.
	HiddenCount int
}

// allowedDeleteSpecs maps MessagesTable -> {ScopeColumn, AttachmentsTable, TimeCast}.
// TimeCast is the message table's own created_at timestamp type, so the range
// predicate casts the (UTC) range param to the COLUMN's type rather than triggering
// a session-TZ-dependent implicit coercion that would shift the purge window (M2).
//   - messages.created_at    is TIMESTAMP   (migration 000006)
//   - dm_messages.created_at is TIMESTAMPTZ (migration 000026)
var allowedDeleteSpecs = map[string][3]string{
	"messages":    {"channel_id", "message_attachments", "timestamp"},
	"dm_messages": {"conversation_id", "dm_message_attachments", "timestamptz"},
}

// validateIdentifiers rejects any DeleteSpec whose table/column identifiers are not
// an exact, coherent entry in the allow-list. This is the ONLY gate between the
// identifiers and fmt.Sprintf-based SQL construction — it must run before any query
// is built.
func validateIdentifiers(s DeleteSpec) error {
	want, ok := allowedDeleteSpecs[s.MessagesTable]
	if !ok || want[0] != s.ScopeColumn || want[1] != s.AttachmentsTable {
		return fmt.Errorf(
			"purge: illegal delete-spec identifiers %q/%q/%q",
			s.MessagesTable, s.ScopeColumn, s.AttachmentsTable,
		)
	}
	return nil
}

// timeCastFor returns the allow-listed timestamp cast keyword for a table. Callers
// MUST have validated the spec via validateIdentifiers first (a non-allow-listed
// table returns the zero string, which would be a malformed cast — never reach here
// with an unvalidated table).
func timeCastFor(table string) string { return allowedDeleteSpecs[table][2] }

// Engine runs audit-first -> batched delete -> reap. Store-agnostic.
type Engine struct {
	db       *sql.DB
	log      *logger.Logger
	reaper   *Reaper
	maxBatch int
}

// NewEngine constructs a purge engine. maxBatch is the batched-delete stride.
// A non-positive maxBatch (an unset hand-built config) falls back to the
// production default: LIMIT 0 would delete nothing while `affected < maxBatch`
// (0 < 0) never terminates the batch loop.
func NewEngine(db *sql.DB, log *logger.Logger, reaper *Reaper, maxBatch int) *Engine {
	if maxBatch <= 0 {
		maxBatch = 5000
	}
	return &Engine{db: db, log: log, reaper: reaper, maxBatch: maxBatch}
}

// Run writes the in_progress audit row, executes each DeleteSpec's batched loop
// (reaping attachments per batch), flips the audit row to completed with the final
// count, and returns the Result. Synchronous.
func (e *Engine) Run(ctx context.Context, p Plan) (Result, error) {
	// Validate ALL specs up front so a malformed identifier aborts before any write
	// (including the audit row) exists.
	for _, ds := range p.Deletes {
		if err := validateIdentifiers(ds); err != nil {
			return Result{}, err
		}
	}

	purgeID, err := e.writeAuditInProgress(ctx, p)
	if err != nil {
		return Result{}, err
	}

	total := 0
	for _, ds := range p.Deletes {
		n, err := e.deleteBatched(ctx, p, ds)
		// Count BEFORE checking err: deleteBatched returns the rows it already
		// committed even when a later batch fails. Those messages are irreversibly
		// gone, so the audit must say so.
		total += n
		if err != nil {
			e.recordPartialCount(ctx, purgeID, total)
			return Result{PurgeID: purgeID, DeletedCount: total}, fmt.Errorf("purge batched delete: %w", err)
		}
	}

	if err := e.finalizeCompleted(ctx, purgeID, total); err != nil {
		// The deletes are committed and irreversible even though the completion stamp
		// failed, so record what was deleted rather than leaving the audit row reading
		// in_progress/0 — the same false-compliance record recordPartialCount exists to
		// prevent, and the reason the caller still gets its PurgeID/DeletedCount back.
		e.recordPartialCount(ctx, purgeID, total)
		return Result{PurgeID: purgeID, DeletedCount: total}, err
	}
	return Result{PurgeID: purgeID, DeletedCount: total}, nil
}

// recordPartialCount stamps the rows an interrupted purge already deleted onto its
// audit row. status stays in_progress — the purge genuinely did not complete, and
// that row is the async-ready seam (§10) plus the record an operator resumes from.
//
// Without this the audit reads `in_progress, deleted_count=0` for a purge that
// irreversibly deleted thousands of messages: a false compliance record on a
// delete-on-request path, and one that understates the deletion rather than
// overstating it (the worse direction for GDPR Art.17 evidence).
//
// context.WithoutCancel is load-bearing: the dominant interruption is the client
// disconnecting, which cancels ctx. Reusing that ctx would fail this UPDATE too, in
// exactly the case it exists to record. Best-effort — a failure here must not mask
// the original delete error, so it is logged, not returned.
func (e *Engine) recordPartialCount(ctx context.Context, purgeID string, total int) {
	if total == 0 {
		return // nothing was deleted; the in_progress row already says so
	}
	if _, err := e.db.ExecContext(context.WithoutCancel(ctx),
		`UPDATE message_purges SET deleted_count = $2 WHERE id = $1`, purgeID, total); err != nil {
		e.log.Warn("purge: failed to record partial count on audit row",
			"error", err, "purge_id", purgeID, "deleted", total)
	}
}

// deleteBatched runs the 3-CTE batched delete for one DeleteSpec until fewer than
// maxBatch rows remain. Each batch is a single round-trip that soft-deletes the
// victims' media_files, hard-deletes the message rows, and returns both the deleted
// count and the reaped storage keys; the keys are handed to the background reaper.
func (e *Engine) deleteBatched(ctx context.Context, p Plan, ds DeleteSpec) (int, error) {
	// %[1]s MessagesTable, %[2]s ScopeColumn, %[3]s AttachmentsTable (all validated,
	// allow-list only), %[4]s the column's own timestamp cast. VALUES are $1..$4.
	// $1 scopeID, $2 rangeFrom (referenced twice), $3 author, $4 maxBatch.
	const tmpl = `
WITH victims AS (
  SELECT id FROM %[1]s
  WHERE %[2]s = $1
    AND ($2::%[4]s IS NULL OR created_at >= $2::%[4]s)
    AND ($3::uuid IS NULL OR user_id = $3)
  ORDER BY created_at LIMIT $4
),
reap AS (
  UPDATE media_files SET deleted_at = NOW()
  WHERE id IN (SELECT file_id FROM %[3]s WHERE message_id IN (SELECT id FROM victims))
    AND deleted_at IS NULL
  RETURNING storage_key
),
del AS (
  DELETE FROM %[1]s WHERE id IN (SELECT id FROM victims) RETURNING 1
)
SELECT (SELECT count(*) FROM del),
       COALESCE((SELECT array_agg(storage_key) FROM reap), '{}')`

	// identifiers come only from the fixed allowedDeleteSpecs allow-list (validated by
	// validateIdentifiers before this call); all VALUES are parameterized ($1..$4).
	q := fmt.Sprintf(tmpl, ds.MessagesTable, ds.ScopeColumn, ds.AttachmentsTable, timeCastFor(ds.MessagesTable)) // #nosec G201 -- identifiers allow-list-validated; values parameterized // nosemgrep:concord-go-sql-sprintf

	total := 0
	for {
		tx, err := e.db.BeginTx(ctx, nil)
		if err != nil {
			return total, fmt.Errorf("purge: begin batch tx: %w", err)
		}

		var affected int
		var keys []string
		// One row: deleted count + reaped storage keys. pq.Array scans the text[] result.
		if err := tx.QueryRowContext(ctx, q, ds.ScopeID, p.RangeFrom, ds.Author, e.maxBatch).
			Scan(&affected, pq.Array(&keys)); err != nil {
			_ = tx.Rollback()
			return total, fmt.Errorf("purge: batch delete query: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return total, fmt.Errorf("purge: commit batch tx: %w", err)
		}

		// Best-effort, off the request path: the reaper drains these to object
		// storage; a dropped/undrained key is recovered by the straggler sweeper.
		e.reaper.EnqueueBlobDeletes(keys)
		total += affected

		if affected < e.maxBatch {
			break
		}
	}
	return total, nil
}

// writeAuditInProgress inserts the in_progress audit row and returns its id.
func (e *Engine) writeAuditInProgress(ctx context.Context, p Plan) (string, error) {
	var id string
	err := e.db.QueryRowContext(ctx, `
		INSERT INTO message_purges
		    (actor_id, context_type, context_id, server_id, target_user_id, range_from, reason, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'in_progress')
		RETURNING id`,
		p.ActorID, string(p.ContextType), p.ContextID, p.ServerID, p.Target, p.RangeFrom, p.Reason,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("purge: write in_progress audit: %w", err)
	}
	return id, nil
}

// finalizeCompleted flips an audit row to completed with the final deleted count.
func (e *Engine) finalizeCompleted(ctx context.Context, purgeID string, deleted int) error {
	if _, err := e.db.ExecContext(ctx, `
		UPDATE message_purges
		SET status = 'completed', deleted_count = $2, completed_at = NOW()
		WHERE id = $1`,
		purgeID, deleted,
	); err != nil {
		return fmt.Errorf("purge: finalize completed: %w", err)
	}
	return nil
}

// FinalizeHidden updates an already-written audit row's hidden_count (DM receiver-hide).
func (e *Engine) FinalizeHidden(ctx context.Context, purgeID string, hidden int) error {
	if _, err := e.db.ExecContext(ctx, `
		UPDATE message_purges SET hidden_count = $2 WHERE id = $1`,
		purgeID, hidden,
	); err != nil {
		return fmt.Errorf("purge: finalize hidden: %w", err)
	}
	return nil
}
