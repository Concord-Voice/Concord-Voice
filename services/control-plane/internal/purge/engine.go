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
	"errors"
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

type deleteQuerySet struct {
	selectBatch         string
	selectOne           string
	selectAttachedMedia string
	deleteParents       string
}

// PostgreSQL cannot bind identifiers, so each allowed table has one fixed query
// set. Runtime values remain parameters.
var deleteQueries = map[string]deleteQuerySet{
	"messages": {
		selectBatch: `SELECT id FROM messages
WHERE channel_id = $1
  AND ($2::timestamptz IS NULL OR created_at >= $2::timestamptz)
  AND ($3::uuid IS NULL OR user_id = $3)
ORDER BY created_at, id
LIMIT $4
FOR UPDATE`,
		selectOne: `SELECT id FROM messages WHERE id = $1 AND channel_id = $2 FOR UPDATE`,
		selectAttachedMedia: `SELECT mf.id
FROM media_files mf
WHERE mf.media_tier = 2
  AND mf.deleted_at IS NULL
  AND mf.id IN (
    SELECT ma.file_id FROM message_attachments ma
    WHERE ma.message_id = ANY($1::uuid[])
  )
ORDER BY mf.id
FOR UPDATE`,
		deleteParents: `DELETE FROM messages WHERE id = ANY($1::uuid[])`,
	},
	"dm_messages": {
		selectBatch: `SELECT id FROM dm_messages
WHERE conversation_id = $1
  AND ($2::timestamptz IS NULL OR created_at >= $2::timestamptz)
  AND ($3::uuid IS NULL OR user_id = $3)
ORDER BY created_at, id
LIMIT $4
FOR UPDATE`,
		selectOne: `SELECT id FROM dm_messages WHERE id = $1 AND conversation_id = $2 FOR UPDATE`,
		selectAttachedMedia: `SELECT mf.id
FROM media_files mf
WHERE mf.media_tier = 2
  AND mf.deleted_at IS NULL
  AND mf.id IN (
    SELECT dma.file_id FROM dm_message_attachments dma
    WHERE dma.message_id = ANY($1::uuid[])
  )
ORDER BY mf.id
FOR UPDATE`,
		deleteParents: `DELETE FROM dm_messages WHERE id = ANY($1::uuid[])`,
	},
}

const retireAttachedMediaQuery = `UPDATE media_files mf
SET deleted_at = NOW()
WHERE mf.id = ANY($1::uuid[])
  AND mf.media_tier = 2
  AND mf.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM message_attachments ma WHERE ma.file_id = mf.id)
  AND NOT EXISTS (SELECT 1 FROM dm_message_attachments dma WHERE dma.file_id = mf.id)
RETURNING mf.storage_key, mf.storage_backend`

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
// maxBatch rows remain. Each batch locks parents and attachments, hard-deletes
// the messages, and returns final-reference blobs to the background reaper.
func (e *Engine) deleteBatched(ctx context.Context, purgeID string, p Plan, ds DeleteSpec) (int, error) {
	q, ok := deleteQueries[ds.MessagesTable]
	if !ok {
		return 0, fmt.Errorf("purge: query set for %q not found", ds.MessagesTable)
	}

	total := 0
	for {
		affected, refs, err := e.deleteBatch(ctx, purgeID, q, p, ds)
		if err != nil {
			return total, err
		}

		e.reaper.EnqueueBlobDeletes(refs)
		total += affected
		if affected < e.maxBatch {
			break
		}
	}
	return total, nil
}

func (e *Engine) deleteBatch(ctx context.Context, purgeID string, queries deleteQuerySet, p Plan, ds DeleteSpec) (int, []media.BlobRef, error) {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("purge: begin batch tx: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			e.log.Warn("purge: failed to rollback batch transaction", "error", err)
		}
	}()

	rows, err := tx.QueryContext(ctx, queries.selectBatch, ds.ScopeID, p.RangeFrom, ds.Author, e.maxBatch)
	if err != nil {
		return 0, nil, fmt.Errorf("purge: select batch victims: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			e.log.Warn("purge: failed to close batch victim rows", "error", closeErr)
		}
	}()
	messageIDs := make([]string, 0, e.maxBatch)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, nil, fmt.Errorf("purge: scan batch victim: %w", err)
		}
		messageIDs = append(messageIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("purge: iterate batch victims: %w", err)
	}
	if len(messageIDs) == 0 {
		return 0, nil, nil
	}
	affected, refs, err := e.deleteMessagesTx(ctx, tx, queries, messageIDs)
	if err != nil {
		return 0, nil, err
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

// deleteMessagesTx locks attached media before deleting messages. This parent →
// media order serializes attachment linking with retirement.
func (e *Engine) deleteMessagesTx(ctx context.Context, tx *sql.Tx, queries deleteQuerySet, messageIDs []string) (int, []media.BlobRef, error) {
	if len(messageIDs) == 0 {
		return 0, nil, nil
	}
	mediaRows, err := tx.QueryContext(ctx, queries.selectAttachedMedia, pq.Array(messageIDs))
	if err != nil {
		return 0, nil, fmt.Errorf("purge: lock attached media: %w", err)
	}
	defer func() {
		if closeErr := mediaRows.Close(); closeErr != nil {
			e.log.Warn("purge: failed to close attached media rows", "error", closeErr)
		}
	}()
	mediaIDs := make([]string, 0)
	for mediaRows.Next() {
		var id string
		if err := mediaRows.Scan(&id); err != nil {
			return 0, nil, fmt.Errorf("purge: scan attached media: %w", err)
		}
		mediaIDs = append(mediaIDs, id)
	}
	if err := mediaRows.Err(); err != nil {
		return 0, nil, fmt.Errorf("purge: iterate attached media: %w", err)
	}

	result, err := tx.ExecContext(ctx, queries.deleteParents, pq.Array(messageIDs))
	if err != nil {
		return 0, nil, fmt.Errorf("purge: delete messages: %w", err)
	}
	affected64, err := result.RowsAffected()
	if err != nil {
		return 0, nil, fmt.Errorf("purge: count deleted messages: %w", err)
	}

	if len(mediaIDs) == 0 {
		return int(affected64), nil, nil
	}
	rows, err := tx.QueryContext(ctx, retireAttachedMediaQuery, pq.Array(mediaIDs))
	if err != nil {
		return 0, nil, fmt.Errorf("purge: retire unreferenced media: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			e.log.Warn("purge: failed to close retired media rows", "error", closeErr)
		}
	}()
	refs := make([]media.BlobRef, 0)
	for rows.Next() {
		var key string
		var backend sql.NullString
		if err := rows.Scan(&key, &backend); err != nil {
			return 0, nil, fmt.Errorf("purge: scan retired media: %w", err)
		}
		refs = append(refs, media.NewBlobRef(key, backend))
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("purge: iterate retired media: %w", err)
	}
	return int(affected64), refs, nil
}

// DeleteOne retires one message and its final-reference attachments atomically.
func (e *Engine) DeleteOne(ctx context.Context, messageID string, spec DeleteSpec) error {
	if err := validateIdentifiers(spec); err != nil {
		return err
	}
	queries := deleteQueries[spec.MessagesTable]
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return contextError(ctx, fmt.Errorf("purge: begin single delete tx: %w", err))
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			e.log.Warn("purge: failed to rollback single delete transaction", "error", rollbackErr)
		}
	}()

	var lockedID string
	if err := tx.QueryRowContext(ctx, queries.selectOne, messageID, spec.ScopeID).Scan(&lockedID); err != nil {
		return contextError(ctx, fmt.Errorf("purge: lock message %s: %w", messageID, err))
	}
	_, refs, err := e.deleteMessagesTx(ctx, tx, queries, []string{lockedID})
	if err != nil {
		return contextError(ctx, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("purge: commit single delete: %w", err)
	}
	e.reaper.EnqueueBlobDeletes(refs)
	return nil
}

// CaptureConversationBlobsTx captures all matching Tier-2 media IDs before the
// conversation's foreign-key cascade removes their metadata, returning blob
// refs only for objects not yet reaped. The caller must hold the conversation
// lock and keep this transaction open until the parent deletion commits.
func (e *Engine) CaptureConversationBlobsTx(
	ctx context.Context,
	tx *sql.Tx,
	conversationID string,
) ([]string, []media.BlobRef, error) {
	if tx == nil {
		return nil, nil, errors.New("purge: capture conversation blobs requires a transaction")
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT mf.id, mf.storage_key, mf.storage_backend,
		       COALESCE(mf.conversation_id = $1::uuid, FALSE) AS correctly_scoped,
		       mf.blob_reaped_at IS NULL AS needs_reap
		FROM media_files mf
		WHERE mf.media_tier = 2
		  AND (
			mf.conversation_id = $1::uuid
			OR mf.id IN (
				SELECT dma.file_id
				FROM dm_message_attachments dma
				JOIN dm_messages dm ON dm.id = dma.message_id
				WHERE dm.conversation_id = $1::uuid
			)
		  )
		ORDER BY mf.id
		FOR UPDATE`, conversationID)
	if err != nil {
		return nil, nil, fmt.Errorf("purge: capture conversation blobs: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			e.log.Warn("purge: failed to close conversation blob rows", "error", closeErr)
		}
	}()

	fileIDs := make([]string, 0)
	refs := make([]media.BlobRef, 0)
	for rows.Next() {
		var fileID, key string
		var backend sql.NullString
		var correctlyScoped, needsReap bool
		if err := rows.Scan(&fileID, &key, &backend, &correctlyScoped, &needsReap); err != nil {
			return nil, nil, fmt.Errorf("purge: scan conversation blob: %w", err)
		}
		if !correctlyScoped {
			return nil, nil, fmt.Errorf("purge: conversation attachment %s has an out-of-scope media file", fileID)
		}
		fileIDs = append(fileIDs, fileID)
		if needsReap {
			refs = append(refs, media.NewBlobRef(key, backend))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("purge: iterate conversation blobs: %w", err)
	}
	return fileIDs, refs, nil
}

// EnqueueBlobDeletes hands committed conversation-deletion refs to the existing
// backend-aware reaper. It is intentionally a narrow forwarding seam so group
// deletion cannot accidentally enqueue before its transaction commits.
func (e *Engine) EnqueueBlobDeletes(refs []media.BlobRef) {
	if e == nil || e.reaper == nil {
		return
	}
	e.reaper.EnqueueBlobDeletes(refs)
}

func contextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
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
