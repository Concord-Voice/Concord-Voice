package dm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/activepresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const retirementCandidateLimit = 100

const retirementCandidateQuery = `SELECT c.id FROM dm_conversations AS c
 WHERE c.is_personal = FALSE
	   AND ($1::uuid IS NULL OR c.id > $1::uuid)
   AND NOT EXISTS (SELECT 1 FROM dm_participants AS p
                   WHERE p.conversation_id = c.id AND p.hidden_at IS NULL)
   AND NOT EXISTS (SELECT 1 FROM dm_messages AS m WHERE m.conversation_id = c.id)
 ORDER BY c.id LIMIT $2`

// RetirementSweepResult reports one bounded discovery pass.
type RetirementSweepResult struct {
	Selected int
	Retired  int
	Skipped  int
	Failed   int
}

// retirementPassError intentionally carries only the aggregate count: candidate
// IDs must not leave the sweeper through startup or periodic errors.
type retirementPassError struct {
	failed int
	fatal  bool
}

func (e *retirementPassError) Error() string {
	return fmt.Sprintf("DM retirement pass failed for %d candidates", e.failed)
}

// RetirementSweeper hard-deletes DM conversations that no participant retains.
// The parent lock serializes new participants and messages with the predicate;
// an active private call additionally needs a durable clear plan before that
// cascade destroys its voice-participant evidence.
type RetirementSweeper struct {
	db    *sql.DB
	redis *redis.Client
	rail  ActivePlanRail
	log   *logger.Logger

	// Test-only ordering seams. They are nil in production.
	afterCandidateReadHook func()
	afterUsersLockHook     func(*sql.Tx)
}

// NewRetirementSweeper constructs the bounded retirement terminal.
func NewRetirementSweeper(db *sql.DB, redisClient *redis.Client, rail ActivePlanRail, log *logger.Logger) *RetirementSweeper {
	return &RetirementSweeper{db: db, redis: redisClient, rail: rail, log: log}
}

// RunPass discovers at most retirementCandidateLimit candidates and attempts
// each independently. Eligibility drift is a skip; operational failures are
// returned only as a count so the next pass can retry without exposing IDs.
func (s *RetirementSweeper) RunPass(ctx context.Context) (RetirementSweepResult, error) {
	result, _, err := s.runPassAfter(ctx, "")
	return result, err
}

func (s *RetirementSweeper) runPassAfter(ctx context.Context, after string) (RetirementSweepResult, string, error) {
	ids, err := s.candidateIDsAfter(ctx, after)
	if err != nil {
		return RetirementSweepResult{}, after, err
	}
	result := RetirementSweepResult{Selected: len(ids)}
	fatal := false
	for _, id := range ids {
		retired, retireErr := s.retireOne(ctx, id)
		if retireErr != nil {
			result.Failed++
			fatal = fatal || !isCandidateLocalRetirementError(retireErr)
			continue
		}
		if retired {
			result.Retired++
		} else {
			result.Skipped++
		}
	}
	if result.Failed != 0 {
		return result, lastCandidateID(ids, after), &retirementPassError{failed: result.Failed, fatal: fatal}
	}
	return result, lastCandidateID(ids, after), nil
}

func isCandidateLocalRetirementError(err error) bool {
	return errors.Is(err, activepresence.ErrTooManySubjects) ||
		errors.Is(err, activepresence.ErrDeliveryIncomplete)
}

// RunPreflight drains the durable backlog before startup proceeds.
func (s *RetirementSweeper) RunPreflight(ctx context.Context) error {
	var cursor string
	for {
		result, nextCursor, err := s.runPassAfter(ctx, cursor)
		s.logResult("preflight", result, err != nil)
		if err != nil {
			var passErr *retirementPassError
			if !errors.As(err, &passErr) || passErr.fatal {
				return err
			}
		}
		if result.Selected == 0 {
			return nil
		}
		cursor = nextCursor
	}
}

// RunWorker retries bounded passes on a fixed cadence after preflight.
func (s *RetirementSweeper) RunWorker(ctx context.Context, interval time.Duration) {
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
			result, nextCursor, err := s.runPassAfter(ctx, cursor)
			s.logResult("periodic", result, err != nil)
			if result.Selected != 0 {
				cursor = nextCursor
			} else if err == nil {
				cursor = ""
			}
		}
	}
}

func (s *RetirementSweeper) candidateIDsAfter(ctx context.Context, after string) (ids []string, returnErr error) {
	if s.db == nil {
		return nil, errors.New("DM retirement sweeper requires database")
	}
	var cursor any
	if after != "" {
		cursor = after
	}
	rows, err := s.db.QueryContext(ctx, retirementCandidateQuery, cursor, retirementCandidateLimit)
	if err != nil {
		return nil, fmt.Errorf("discover retirement candidates: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("close retirement candidate rows: %w", closeErr)
		}
	}()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan retirement candidate: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate retirement candidates: %w", err)
	}
	return ids, nil
}

func lastCandidateID(ids []string, fallback string) string {
	if len(ids) == 0 {
		return fallback
	}
	return ids[len(ids)-1]
}

// retireOne's destructive invariant is: gates precede every database lock;
// then users precede the conversation parent; then the locked parent sees the
// complete retention predicate and the current voice set. Taking a newly seen
// subject's gate after the parent lock would invert the rail; deleting without
// the re-read would discard an active-call clear obligation.
func (s *RetirementSweeper) retireOne(ctx context.Context, conversationID string) (bool, error) {
	conversationUUID, err := uuid.Parse(conversationID)
	if err != nil {
		return false, fmt.Errorf("parse retirement conversation ID: %w", err)
	}
	// ponytail: the local lock only coordinates this process; the parent lock and
	// Redis fence cover overlapping #2757 control-plane processes.
	unlockLifecycle := LockDMCallLifecycle(conversationUUID)
	defer unlockLifecycle()

	if HasLocalPendingDMCall(conversationUUID) {
		return false, nil
	}
	if fenced, err := s.hasSharedVoiceFence(ctx, conversationUUID); err != nil {
		return false, err
	} else if fenced {
		return false, nil
	}

	candidates, err := readVoiceCandidates(ctx, s.db, conversationID)
	if err != nil {
		return false, fmt.Errorf("read retirement voice candidates: %w", err)
	}
	if s.afterCandidateReadHook != nil {
		s.afterCandidateReadHook()
	}
	if len(candidates) > maxGroupVoiceCandidates {
		return false, fmt.Errorf("%w: %d", activepresence.ErrTooManySubjects, len(candidates))
	}
	if len(candidates) == 0 {
		return s.retireWithoutPlans(ctx, conversationID)
	}
	if s.rail == nil {
		return false, errors.New("DM retirement sweeper active-plan rail unavailable")
	}

	retired := false
	err = s.rail.WithGatedTx(ctx, candidates, func(tx *sql.Tx) error {
		var txErr error
		retired, txErr = s.retireGatedConversation(ctx, tx, conversationID, conversationUUID, candidates)
		return txErr
	})
	if err != nil {
		return false, err
	}
	return retired, nil
}

func (s *RetirementSweeper) retireGatedConversation(ctx context.Context, tx *sql.Tx, conversationID string, conversationUUID uuid.UUID, candidates []uuid.UUID) (bool, error) {
	if err := lockVoiceCandidates(ctx, tx, candidates); err != nil {
		return false, err
	}
	if s.afterUsersLockHook != nil {
		s.afterUsersLockHook(tx)
	}
	locked, err := lockRetirementConversation(ctx, tx, conversationID)
	if err != nil || !locked {
		return false, err
	}
	fenced, err := s.hasSharedVoiceFence(ctx, conversationUUID)
	if err != nil {
		return false, err
	}
	if fenced {
		return false, nil
	}
	current, err := readVoiceCandidates(ctx, tx, conversationID)
	if err != nil {
		return false, fmt.Errorf("re-read retirement voice candidates: %w", err)
	}
	if hasUngatedVoiceCandidate(current, candidates) {
		return false, nil
	}
	eligible, err := retirementEligibleTx(ctx, tx, conversationID)
	if err != nil || !eligible {
		return false, err
	}
	keys, err := capturePrivateCallPlans(ctx, tx, s.rail, current)
	if err != nil {
		return false, err
	}
	if err := deleteRetirementConversation(ctx, tx, conversationID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit retirement conversation: %w", err)
	}
	return true, s.rail.CompleteAlreadyGated(ctx, nil, keys)
}

func (s *RetirementSweeper) retireWithoutPlans(ctx context.Context, conversationID string) (retired bool, returnErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin retirement transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("rollback retirement transaction: %w", rollbackErr))
		}
	}()
	locked, err := lockRetirementConversation(ctx, tx, conversationID)
	if err != nil || !locked {
		return false, err
	}
	conversationUUID, err := uuid.Parse(conversationID)
	if err != nil {
		return false, fmt.Errorf("parse retirement conversation ID: %w", err)
	}
	fenced, err := s.hasSharedVoiceFence(ctx, conversationUUID)
	if err != nil {
		return false, err
	}
	if fenced {
		return false, nil
	}
	current, err := readVoiceCandidates(ctx, tx, conversationID)
	if err != nil {
		return false, fmt.Errorf("re-read retirement voice candidates: %w", err)
	}
	if len(current) != 0 {
		return false, nil
	}
	eligible, err := retirementEligibleTx(ctx, tx, conversationID)
	if err != nil {
		return false, err
	}
	if !eligible {
		return false, nil
	}
	if err := deleteRetirementConversation(ctx, tx, conversationID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit retirement conversation: %w", err)
	}
	return true, nil
}

func (s *RetirementSweeper) hasSharedVoiceFence(ctx context.Context, conversationID uuid.UUID) (bool, error) {
	pending, err := HasDMPendingVoiceCall(ctx, s.redis, conversationID)
	if err != nil {
		return false, fmt.Errorf("lookup retirement DM pending voice call: %w", err)
	}
	if pending {
		return true, nil
	}
	_, hasLease, err := LookupDMVoiceCallLease(ctx, s.redis, conversationID)
	if err != nil {
		return false, fmt.Errorf("lookup retirement DM voice call lease: %w", err)
	}
	return hasLease, nil
}

func lockRetirementConversation(ctx context.Context, tx *sql.Tx, conversationID string) (bool, error) {
	err := lockConversation(ctx, tx, conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func retirementEligibleTx(ctx context.Context, tx *sql.Tx, conversationID string) (bool, error) {
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT NOT c.is_personal
		AND NOT EXISTS (SELECT 1 FROM dm_participants AS p
		                WHERE p.conversation_id = c.id AND p.hidden_at IS NULL)
		AND NOT EXISTS (SELECT 1 FROM dm_messages AS m WHERE m.conversation_id = c.id)
		FROM dm_conversations AS c WHERE c.id = $1`, conversationID).Scan(&eligible)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("revalidate retirement eligibility: %w", err)
	}
	return eligible, nil
}

func hasUngatedVoiceCandidate(current, gated []uuid.UUID) bool {
	gatedSet := make(map[uuid.UUID]struct{}, len(gated))
	for _, id := range gated {
		gatedSet[id] = struct{}{}
	}
	for _, id := range current {
		if _, ok := gatedSet[id]; !ok {
			return true
		}
	}
	return false
}

func deleteRetirementConversation(ctx context.Context, tx *sql.Tx, conversationID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM dm_conversations WHERE id = $1`, conversationID); err != nil {
		return fmt.Errorf("delete retirement conversation: %w", err)
	}
	return nil
}

func (s *RetirementSweeper) logResult(phase string, result RetirementSweepResult, failed bool) {
	if s.log == nil {
		return
	}
	if failed {
		s.log.Warn("DM retirement pass failed", "phase", phase, "selected", result.Selected,
			"retired", result.Retired, "skipped", result.Skipped, "failed", result.Failed)
		return
	}
	s.log.Info("DM retirement pass completed", "phase", phase, "selected", result.Selected,
		"retired", result.Retired, "skipped", result.Skipped, "failed", result.Failed)
}
