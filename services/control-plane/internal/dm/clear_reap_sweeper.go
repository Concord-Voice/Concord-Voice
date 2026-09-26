package dm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmvisibility"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	clearReapCandidateLimit = 100 // the retirement precedent
	clearReapVisitCap       = 50  // batches per candidate per periodic pass
	clearReapBatchTimeout   = 30 * time.Second
)

// clearReapCandidateQuery finds conversations where the reap has work: every
// current participant cleared (branch A), or none remain (branch B), and a
// message still exists below W. The EXISTS keeps already-reaped conversations
// out, and re-finds a call-event later stamped below W.
const clearReapCandidateQuery = `WITH cand AS (
  SELECT DISTINCT hr.conversation_id AS id FROM dm_message_hidden_ranges hr
   WHERE hr.includes_own AND hr.hidden_from = '-infinity'::timestamptz
     AND ($1::uuid IS NULL OR hr.conversation_id > $1::uuid)
  UNION
  SELECT c0.id FROM dm_conversations c0
   WHERE NOT EXISTS (SELECT 1 FROM dm_participants p0 WHERE p0.conversation_id = c0.id)
     AND ($1::uuid IS NULL OR c0.id > $1::uuid))
SELECT c.id FROM cand JOIN dm_conversations c ON c.id = cand.id
 CROSS JOIN LATERAL (` + dmvisibility.ClearWatermarkLateral + `) w
 WHERE NOT c.is_personal AND w.uncleared = 0
   AND EXISTS (SELECT 1 FROM dm_messages m WHERE m.conversation_id = c.id
               AND (w.participants = 0 OR m.created_at < w.watermark))
 ORDER BY c.id LIMIT $2`

// ClearReapBatchRunner is the narrow engine surface the sweeper needs.
type ClearReapBatchRunner interface {
	RunClearReapBatch(ctx context.Context, p purge.ClearReapPlan) (purge.ClearReapResult, error)
}

// ClearReapSweepResult reports one bounded discovery pass. Counts only (I6).
// Capped counts candidates that still had work when a periodic visit hit its
// batch cap; they are resumed when the cursor next wraps.
type ClearReapSweepResult struct {
	Selected, Reaped, Skipped, Failed, Deleted, Capped int
}

// clearReapPassError intentionally carries only the aggregate count and one
// representative underlying error, from which logResult recovers a SQLSTATE:
// candidate IDs and driver message text must never leave the sweeper (I6).
type clearReapPassError struct {
	failed int
	fatal  bool
	err    error
}

func (e *clearReapPassError) Error() string {
	return fmt.Sprintf("DM clear reap pass failed for %d candidates", e.failed)
}

// Unwrap lets logResult recover the representative error's SQLSTATE through
// errors.As without exposing its message.
func (e *clearReapPassError) Unwrap() error { return e.err }

// ClearReapSweeper discovers conversations the clear reap can shrink and
// drives the engine terminal. It reads and never deletes (#3462).
type ClearReapSweeper struct {
	db       *sql.DB
	engine   ClearReapBatchRunner
	log      *logger.Logger
	discover func(ctx context.Context, after string) ([]string, error)
}

// NewClearReapSweeper constructs the sweeper.
func NewClearReapSweeper(db *sql.DB, engine ClearReapBatchRunner, log *logger.Logger) *ClearReapSweeper {
	s := &ClearReapSweeper{db: db, engine: engine, log: log}
	s.discover = s.candidateIDsAfter
	return s
}

// RunPass runs one periodic pass from an empty cursor.
func (s *ClearReapSweeper) RunPass(ctx context.Context) (ClearReapSweepResult, error) {
	result, _, err := s.runPassAfter(ctx, "", clearReapVisitCap)
	return result, err
}

// RunPreflight drains every eligible conversation before bind. It tolerates
// candidate-local failures and returns only a fatal one (spec D4).
func (s *ClearReapSweeper) RunPreflight(ctx context.Context) error {
	var cursor string
	for {
		result, next, err := s.runPassAfter(ctx, cursor, 0) // 0 = no visit cap
		s.logResult("preflight", result, err)
		if err != nil {
			var passErr *clearReapPassError
			if !errors.As(err, &passErr) || passErr.fatal {
				return err
			}
		}
		if result.Selected == 0 {
			return nil
		}
		cursor = next
	}
}

// RunWorker runs one pass per tick, resuming from the last cursor and
// restarting from the beginning once a pass reaches the end of the ID space,
// so nothing is permanently skipped.
func (s *ClearReapSweeper) RunWorker(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var cursor string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			result, next, err := s.runPassAfter(ctx, cursor, clearReapVisitCap)
			s.logResult("periodic", result, err)
			cursor = nextClearReapCursor(cursor, next, result, err)
		}
	}
}

// nextClearReapCursor picks the periodic worker's next cursor. A full page may
// have more candidates after it, so the cursor advances. A short or empty page
// reached the end of the ID space, so the next pass starts over; resetting on a
// short page, rather than spending a tick on an empty pass first, matters
// because a candidate that hit the visit cap is resumed only by this reset
// (PR #3463 review). A failed discovery keeps the cursor.
func nextClearReapCursor(cursor, next string, r ClearReapSweepResult, err error) string {
	switch {
	case r.Selected >= clearReapCandidateLimit:
		return next
	case r.Selected > 0 || err == nil:
		return ""
	default:
		return cursor
	}
}

func (s *ClearReapSweeper) runPassAfter(ctx context.Context, after string, visitCap int) (ClearReapSweepResult, string, error) {
	ids, err := s.discover(ctx, after)
	if err != nil {
		return ClearReapSweepResult{}, after, err
	}
	result := ClearReapSweepResult{Selected: len(ids)}
	fatal := false
	// firstFatal is kept apart from lastErr so a fatal pass reports the error
	// that made it fatal, not a later candidate-local one.
	var lastErr, firstFatal error
	for _, id := range ids {
		deleted, capped, visitErr := s.visit(ctx, id, visitCap)
		result.Deleted += deleted
		if capped {
			result.Capped++
		}
		switch {
		case visitErr != nil:
			result.Failed++
			lastErr = visitErr
			if !isCandidateLocalClearReapError(ctx, visitErr) {
				fatal = true
				if firstFatal == nil {
					firstFatal = visitErr
				}
			}
		case deleted > 0:
			result.Reaped++
		default:
			result.Skipped++
		}
		if ctx.Err() != nil {
			fatal = true
			break
		}
	}
	next := lastCandidateID(ids, after)
	if result.Failed != 0 {
		passErr := &clearReapPassError{failed: result.Failed, fatal: fatal, err: lastErr}
		if firstFatal != nil {
			passErr.err = firstFatal
		}
		return result, next, passErr
	}
	return result, next, ctx.Err()
}

// visit calls the engine while it reports More, up to visitCap batches
// (0 = unlimited). It returns rows deleted before any error, and capped when it
// stopped at the cap while the engine still reported More.
func (s *ClearReapSweeper) visit(ctx context.Context, conversationID string, visitCap int) (int, bool, error) {
	deleted := 0
	for calls := 0; visitCap == 0 || calls < visitCap; calls++ {
		batchCtx, cancel := context.WithTimeout(ctx, clearReapBatchTimeout)
		res, err := s.engine.RunClearReapBatch(batchCtx, purge.ClearReapPlan{ConversationID: conversationID})
		cancel()
		deleted += res.DeletedCount
		if err != nil {
			return deleted, false, err
		}
		if res.Outcome != purge.ClearReapReaped || !res.More {
			return deleted, false, nil
		}
	}
	return deleted, true, nil
}

func isCandidateLocalClearReapError(parent context.Context, err error) bool {
	if parent.Err() != nil {
		return false // parent cancellation is not candidate-local
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true // the per-batch deadline
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		switch pqErr.Code {
		// 57014 is query_canceled: lib/pq reports a per-batch deadline that
		// fires mid-statement this way, not as context.DeadlineExceeded, so it
		// is the deadline case above in another form (#3462 red-team H3). A
		// parent cancellation that surfaces as 57014 was already refused by the
		// ctx.Err() check at the top.
		case "40P01", "55P03", "40001", "57014":
			return true
		}
	}
	return false
}

func (s *ClearReapSweeper) candidateIDsAfter(ctx context.Context, after string) ([]string, error) {
	if s.db == nil {
		return nil, errors.New("DM clear reap sweeper requires database")
	}
	var cursor any
	if after != "" {
		cursor = after
	}
	rows, err := s.db.QueryContext(ctx, clearReapCandidateQuery, cursor, clearReapCandidateLimit)
	if err != nil {
		return nil, fmt.Errorf("discover clear reap candidates: %w", err)
	}
	ids, err := purge.ScanIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("read clear reap candidates: %w", err)
	}
	return ids, nil
}

// logResult emits one line per pass: counts only, and on failure the SQLSTATE
// alone, never the error text (I6).
func (s *ClearReapSweeper) logResult(phase string, r ClearReapSweepResult, err error) {
	if s.log == nil {
		return
	}
	args := []any{"phase", phase, "selected", r.Selected, "reaped", r.Reaped,
		"skipped", r.Skipped, "failed", r.Failed, "deleted", r.Deleted, "capped", r.Capped}
	if err == nil {
		s.log.Info("DM clear reap pass completed", args...)
		return
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		args = append(args, "sqlstate", string(pqErr.Code))
	}
	var passErr *clearReapPassError
	args = append(args, "fatal", !errors.As(err, &passErr) || passErr.fatal, "error_class", purge.ErrorClass(err))
	s.log.Warn("DM clear reap pass failed", args...)
}
