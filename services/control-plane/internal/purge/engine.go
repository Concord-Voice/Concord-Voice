// Package purge implements the store-agnostic message-purge engine (#1352):
// permission-gated bulk deletion of channel / server / DM / group-DM messages,
// scoped by an optional time range and optional target author, with attachment
// reaping and a privacy-safe audit trail.
//
// The engine is deliberately store-agnostic: handlers do all context-specific
// authorization, build a Plan, and call Run. Table/column identifiers select
// only fixed SQL templates; all VALUES flow through parameterized placeholders
// ($1..$4).
package purge

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
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

// partialCountSalvageTimeout bounds the best-effort audit update after an
// interrupted purge. context.WithoutCancel also removes the caller deadline.
const partialCountSalvageTimeout = 3 * time.Second

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

// allowedDeleteSpecs maps MessagesTable -> {ScopeColumn, AttachmentsTable}.
var allowedDeleteSpecs = map[string][2]string{
	"messages":    {"channel_id", "message_attachments"},
	"dm_messages": {"conversation_id", "dm_message_attachments"},
}

// validateIdentifiers rejects any DeleteSpec whose table/column identifiers are not
// an exact, coherent entry in the allow-list.
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

// deleteQueries are fixed per-store templates. PostgreSQL cannot bind table or
// column identifiers, so validated DeleteSpec fields choose one of these static
// statements while all runtime values remain parameters.
//
// The reap CTE returns storage_backend ALONGSIDE storage_key (ADR-0038 / #2759
// unit B2): placement is per object, and the reaper must delete each blob from
// the backend its row names rather than from one process-wide client. The two
// arrays are aggregated in a SINGLE `reaped` CTE and read from that one row, so
// they are index-aligned by construction — one Aggregate node consumes one tuple
// stream, so both array_agg calls see the same rows in the same order. Two
// separate `(SELECT array_agg(...) FROM reap)` subqueries would pair them only
// by relying on a materialized CTE being rescanned in insertion order, which is
// an implementation detail and not something a deletion path should rest on.
var deleteQueries = map[string]string{
	"messages": `
WITH victims AS (
  SELECT id FROM messages
  WHERE channel_id = $1
    AND ($2::timestamp IS NULL OR created_at >= $2::timestamp)
    AND ($3::uuid IS NULL OR user_id = $3)
  ORDER BY created_at LIMIT $4
),
reap AS (
  UPDATE media_files SET deleted_at = NOW()
  WHERE id IN (SELECT file_id FROM message_attachments WHERE message_id IN (SELECT id FROM victims))
    AND deleted_at IS NULL
  RETURNING storage_key, storage_backend
),
del AS (
  DELETE FROM messages WHERE id IN (SELECT id FROM victims) RETURNING 1
),
reaped AS (
  SELECT COALESCE(array_agg(storage_key), '{}') AS keys,
         COALESCE(array_agg(storage_backend), '{}'::text[]) AS backends
  FROM reap
)
SELECT (SELECT count(*) FROM del),
       (SELECT keys FROM reaped),
       (SELECT backends FROM reaped)`,
	"dm_messages": `
WITH victims AS (
  SELECT id FROM dm_messages
  WHERE conversation_id = $1
    AND ($2::timestamptz IS NULL OR created_at >= $2::timestamptz)
    AND ($3::uuid IS NULL OR user_id = $3)
  ORDER BY created_at LIMIT $4
),
reap AS (
  UPDATE media_files SET deleted_at = NOW()
  WHERE id IN (SELECT file_id FROM dm_message_attachments WHERE message_id IN (SELECT id FROM victims))
    AND deleted_at IS NULL
  RETURNING storage_key, storage_backend
),
del AS (
  DELETE FROM dm_messages WHERE id IN (SELECT id FROM victims) RETURNING 1
),
reaped AS (
  SELECT COALESCE(array_agg(storage_key), '{}') AS keys,
         COALESCE(array_agg(storage_backend), '{}'::text[]) AS backends
  FROM reap
)
SELECT (SELECT count(*) FROM del),
       (SELECT keys FROM reaped),
       (SELECT backends FROM reaped)`,
}

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
		n, err := e.deleteBatched(ctx, purgeID, p, ds)
		// Count BEFORE checking err: deleteBatched returns the rows it already
		// committed even when a later batch fails. Those messages are irreversibly
		// gone, so the audit must say so.
		total += n
		if err != nil {
			total = e.recoverPartialCount(ctx, purgeID, total)
			return Result{PurgeID: purgeID, DeletedCount: total}, fmt.Errorf("purge batched delete: %w", err)
		}
	}

	if err := e.finalizeCompleted(ctx, purgeID, total); err != nil {
		// The deletes are committed and irreversible even though the completion stamp
		// failed, so record what was deleted rather than leaving the audit row reading
		// in_progress/0 — the same false-compliance record recordPartialCount exists to
		// prevent, and the reason the caller still gets its PurgeID/DeletedCount back.
		total = e.recoverPartialCount(ctx, purgeID, total)
		return Result{PurgeID: purgeID, DeletedCount: total}, err
	}
	return Result{PurgeID: purgeID, DeletedCount: total}, nil
}

// recoverPartialCount stamps the rows an interrupted purge already deleted onto its
// audit row. status stays in_progress — the purge genuinely did not complete, and
// that row is the async-ready seam (§10) plus the record an operator resumes from.
//
// Without this the audit reads `in_progress, deleted_count=0` for a purge that
// irreversibly deleted thousands of messages: a false compliance record on a
// delete-on-request path, and one that understates the deletion rather than
// overstating it (the worse direction for GDPR Art.17 evidence).
//
// recoverPartialCount supplies one cancellation-detached budget to both the
// durable audit read and corrective update. A client disconnect must not erase
// the audit evidence, but serial recovery operations must not extend the HTTP
// response beyond that one bounded grace period.
func (e *Engine) recoverPartialCount(ctx context.Context, purgeID string, total int) int {
	salvageCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), partialCountSalvageTimeout)
	defer cancel()

	total = e.durableDeletedCount(salvageCtx, purgeID, total)
	e.recordPartialCount(salvageCtx, purgeID, total)
	return total
}

// recordPartialCount uses the bounded, detached context supplied by
// recoverPartialCount. Best-effort — a failure here must not mask the original
// delete error, so it is logged, not returned.
func (e *Engine) recordPartialCount(ctx context.Context, purgeID string, total int) {
	if total == 0 {
		return // nothing was deleted; the in_progress row already says so
	}
	if _, err := e.db.ExecContext(ctx,
		`UPDATE message_purges SET deleted_count = GREATEST(deleted_count, $2) WHERE id = $1`, purgeID, total); err != nil {
		e.log.Warn("purge: failed to record partial count on audit row",
			"error", err, "purge_id", purgeID, "deleted", total)
	}
}

// durableDeletedCount reconciles an uncertain batch commit with the count that
// committed atomically alongside the delete. PostgreSQL can commit while the
// client loses the acknowledgement; returning the locally accumulated count in
// that case would understate irreversible deletion to the moderation response.
func (e *Engine) durableDeletedCount(ctx context.Context, purgeID string, fallback int) int {
	var deleted int
	if err := e.db.QueryRowContext(ctx,
		`SELECT deleted_count FROM message_purges WHERE id = $1`, purgeID).Scan(&deleted); err != nil {
		e.log.Warn("purge: failed to read durable count after batch error", "error", err, "purge_id", purgeID)
		return fallback
	}
	if deleted > fallback {
		return deleted
	}
	return fallback
}

// deleteBatched runs the batched delete for one DeleteSpec until fewer than
// maxBatch rows remain. Each batch is a single round-trip that soft-deletes the
// victims' media_files, hard-deletes the message rows, and returns both the deleted
// count and the reaped blobs (key + backend); the blobs are handed to the
// background reaper.
func (e *Engine) deleteBatched(ctx context.Context, purgeID string, p Plan, ds DeleteSpec) (int, error) {
	q := deleteQueries[ds.MessagesTable]

	total := 0
	for {
		affected, refs, err := e.deleteBatch(ctx, purgeID, q, p, ds)
		if err != nil {
			return total, err
		}

		// Best-effort, off the request path: the reaper drains these to object
		// storage; a dropped/undrained blob is recovered by the straggler sweeper.
		e.reaper.EnqueueBlobDeletes(refs)
		total += affected

		if affected < e.maxBatch {
			break
		}
	}
	return total, nil
}

func (e *Engine) deleteBatch(ctx context.Context, purgeID, query string, p Plan, ds DeleteSpec) (int, []media.BlobRef, error) {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("purge: begin batch tx: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			e.log.Warn("purge: failed to rollback batch transaction", "error", err)
		}
	}()

	var affected int
	var keys []string
	// Nullable per element: NULL is the legacy backend and the permanent value of
	// every pre-cutover object, so this cannot scan into []string.
	var backends []sql.NullString
	// One row: deleted count + the reaped keys and their backends. pq.Array scans
	// the text[] results; the NullString element type routes through pq's
	// GenericArray, which honours sql.Scanner and therefore NULL elements.
	if err := tx.QueryRowContext(ctx, query, ds.ScopeID, p.RangeFrom, ds.Author, e.maxBatch).
		Scan(&affected, pq.Array(&keys), pq.Array(&backends)); err != nil {
		return 0, nil, fmt.Errorf("purge: batch delete query: %w", err)
	}
	refs, err := blobRefs(keys, backends)
	if err != nil {
		// The soft-deletes in this batch are about to commit, so the rows are
		// already straggler-sweep candidates. Drop the enqueue rather than reap
		// a mispaired key against the wrong backend: the sweep re-reads both
		// columns off the same row and recovers them. Loud, because this shape
		// is not supposed to be reachable.
		e.log.Error("purge: reaped key/backend arrays disagree; leaving these blobs to the straggler sweep",
			"error", err, "purge_id", purgeID)
		refs = nil
	}
	if affected > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE message_purges SET deleted_count = deleted_count + $2 WHERE id = $1`, purgeID, affected); err != nil {
			return 0, nil, fmt.Errorf("purge: record batch count: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("purge: commit batch tx: %w", err)
	}
	return affected, refs, nil
}

// blobRefs pairs the reaped storage keys with their backends.
//
// The two arrays come from one Aggregate node over one tuple stream, so they are
// index-aligned by construction and a length mismatch is structurally impossible.
// It is still checked, because the failure it would otherwise produce is the exact
// one this unit exists to close: a key reaped against SOMEBODY ELSE'S backend
// deletes nothing, returns success, and stamps blob_reaped_at on a row whose object
// still exists.
func blobRefs(keys []string, backends []sql.NullString) ([]media.BlobRef, error) {
	if len(keys) != len(backends) {
		return nil, fmt.Errorf("purge: reaped %d keys but %d backends", len(keys), len(backends))
	}
	if len(keys) == 0 {
		return nil, nil
	}
	refs := make([]media.BlobRef, 0, len(keys))
	for i, key := range keys {
		refs = append(refs, media.NewBlobRef(key, backends[i]))
	}
	return refs, nil
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
