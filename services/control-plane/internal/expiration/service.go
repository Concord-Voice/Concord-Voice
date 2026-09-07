// Package expiration owns the durable message-expiration policy and its
// bounded retroactive backfill.
package expiration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const maxBackfillBatchesPerRequest = 5

var (
	// ErrInvalidRequest marks a request whose fields are not one allowed operation shape.
	ErrInvalidRequest = errors.New("expiration: invalid request")
	// ErrInvalidWindow marks a set request outside the policy's closed window set.
	ErrInvalidWindow = errors.New("expiration: invalid window")
	// ErrBackfillPending prevents a second policy mutation while work remains.
	ErrBackfillPending = errors.New("expiration: backfill pending")
	// ErrBackfillNotFound marks a resume request with no active matching marker.
	ErrBackfillNotFound = errors.New("expiration: backfill not found")
	// ErrRevisionMismatch marks a resume request for a different policy generation.
	ErrRevisionMismatch = errors.New("expiration: revision mismatch")
	// ErrScopeNotFound marks a channel or conversation absent from its fixed scope.
	ErrScopeNotFound = errors.New("expiration: scope not found")
	// ErrServiceUnready marks a missing database or caller transaction.
	ErrServiceUnready = errors.New("expiration: service is not configured")
)

// Request is the JSON body for a policy mutation. Validate rejects every
// field combination other than the three documented operation shapes.
type Request struct {
	Mode          string `json:"mode"`
	WindowSeconds *int   `json:"window_seconds"`
	Retroactive   string `json:"retroactive"`
	Revision      *int64 `json:"revision"`
}

// UnmarshalJSON rejects fields outside the expiration request contract before
// assigning the decoded value, so every existing JSON binder shares the same
// closed schema.
func (r *Request) UnmarshalJSON(data []byte) error {
	type requestAlias Request

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var mode string
	if err := json.Unmarshal(fields["mode"], &mode); err != nil {
		return err
	}
	if !hasExactRequestFields(fields, mode) {
		return ErrInvalidRequest
	}

	var decoded requestAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = Request(decoded)
	return nil
}

func hasExactRequestFields(fields map[string]json.RawMessage, mode string) bool {
	var expected []string
	switch mode {
	case "set":
		expected = []string{"mode", "window_seconds", "retroactive"}
	case "clear":
		expected = []string{"mode", "retroactive"}
	case "resume":
		expected = []string{"mode", "revision"}
	default:
		return false
	}
	if len(fields) != len(expected) {
		return false
	}
	for _, name := range expected {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}

// Validate checks the complete operation shape before any policy row changes.
func (r Request) Validate() error {
	switch r.Mode {
	case "set":
		if r.WindowSeconds == nil || r.Revision != nil || (r.Retroactive != "apply" && r.Retroactive != "new_only") {
			return ErrInvalidRequest
		}
		if !allowedWindow(*r.WindowSeconds) {
			return ErrInvalidWindow
		}
	case "clear":
		if r.WindowSeconds != nil || r.Revision != nil || (r.Retroactive != "clear_pending" && r.Retroactive != "leave_pending") {
			return ErrInvalidRequest
		}
	case "resume":
		if r.WindowSeconds != nil || r.Retroactive != "" || r.Revision == nil || *r.Revision < 0 {
			return ErrInvalidRequest
		}
	default:
		return ErrInvalidRequest
	}
	return nil
}

func allowedWindow(window int) bool {
	switch window {
	case 3600, 86400, 604800, 2592000:
		return true
	default:
		return false
	}
}

// Policy is the public durable policy state. BackfillPending is true exactly
// while the parent row retains a backfill marker.
type Policy struct {
	WindowSeconds   *int       `json:"window_seconds"`
	UpdatedAt       *time.Time `json:"updated_at"`
	Revision        int64      `json:"revision"`
	BackfillPending bool       `json:"backfill_pending"`
}

// Service executes post-commit backfill batches through its own transactions.
// Start methods deliberately never commit or roll back the caller's transaction.
type Service struct {
	db *sql.DB
}

// NewService returns a service that owns separately committed resume batches.
func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// StartChannel validates and records a channel policy in the caller's
// transaction. A resume request only locks and verifies the existing marker;
// it does not change the row, so the caller can commit its authorization work
// before ResumeChannel starts independently committed batches.
func (s *Service) StartChannel(ctx context.Context, tx *sql.Tx, channelID string, request Request) (Policy, error) {
	return s.start(ctx, tx, channelScope, channelID, request)
}

// StartConversation is StartChannel for a DM conversation.
func (s *Service) StartConversation(ctx context.Context, tx *sql.Tx, conversationID string, request Request) (Policy, error) {
	return s.start(ctx, tx, conversationScope, conversationID, request)
}

// ResumeChannel finishes the exact pending channel operation, committing no
// more than 5,000 affected rows per transaction.
func (s *Service) ResumeChannel(ctx context.Context, channelID string, revision int64) (Policy, error) {
	return s.runBatches(ctx, channelScope, channelID, revision)
}

// ResumeConversation is ResumeChannel for a DM conversation.
func (s *Service) ResumeConversation(ctx context.Context, conversationID string, revision int64) (Policy, error) {
	return s.runBatches(ctx, conversationScope, conversationID, revision)
}

// A marker's revision, mode, and cutoff are its identity. Every batch locks and
// rechecks all three before it changes rows. Clearing any part early would let a
// later setter race an unfinished batch and resurrect rows at the old cutoff.
type scope struct {
	name             string
	lockSQL          string
	readSQL          string
	updatePolicySQL  string
	applyBatchSQL    string
	clearBatchSQL    string
	applyEligibleSQL string
	clearEligibleSQL string
	clearMarkerSQL   string
}

var channelScope = scope{
	name: "channel",
	lockSQL: `
SELECT expiration_window_seconds, expiration_updated_at, expiration_revision,
       expiration_backfill_mode, expiration_backfill_cutoff
FROM channels
WHERE id = $1
FOR NO KEY UPDATE`,
	readSQL: `
SELECT expiration_window_seconds, expiration_updated_at, expiration_revision,
       expiration_backfill_mode, expiration_backfill_cutoff
FROM channels
WHERE id = $1`,
	updatePolicySQL: `
WITH accepted AS (SELECT clock_timestamp() AS cutoff)
UPDATE channels AS c
SET expiration_window_seconds = $2,
    expiration_updated_at = accepted.cutoff,
    expiration_revision = c.expiration_revision + 1,
    expiration_backfill_mode = $3,
    expiration_backfill_cutoff = CASE WHEN $3::text IS NULL THEN NULL ELSE accepted.cutoff END
FROM accepted
WHERE c.id = $1
RETURNING c.expiration_window_seconds, c.expiration_updated_at, c.expiration_revision,
          c.expiration_backfill_mode, c.expiration_backfill_cutoff`,
	applyBatchSQL: `
WITH batch AS (
  SELECT ctid FROM messages
  WHERE channel_id = $1 AND created_at <= $2
    AND (expires_at IS NULL OR expires_at > $2)
    AND expires_at IS DISTINCT FROM
        (created_at + make_interval(secs => $3))
  LIMIT 5000
)
UPDATE messages m
SET expires_at = m.created_at + make_interval(secs => $3)
FROM batch WHERE m.ctid = batch.ctid`,
	applyEligibleSQL: `
SELECT EXISTS (
  SELECT 1 FROM messages
  WHERE channel_id = $1 AND created_at <= $2
    AND (expires_at IS NULL OR expires_at > $2)
    AND expires_at IS DISTINCT FROM
        (created_at + make_interval(secs => $3))
)`,
	clearBatchSQL: `
WITH batch AS (
  SELECT ctid FROM messages
  WHERE channel_id = $1 AND expires_at > $2
  LIMIT 5000
)
UPDATE messages m
SET expires_at = NULL
FROM batch WHERE m.ctid = batch.ctid`,
	clearEligibleSQL: `
SELECT EXISTS (
  SELECT 1 FROM messages
  WHERE channel_id = $1 AND expires_at > $2
)`,
	clearMarkerSQL: `
UPDATE channels
SET expiration_backfill_mode = NULL, expiration_backfill_cutoff = NULL
WHERE id = $1
  AND expiration_revision = $2
  AND expiration_backfill_mode = $3
  AND expiration_backfill_cutoff = $4`,
}

var conversationScope = scope{
	name: "conversation",
	lockSQL: `
SELECT expiration_window_seconds, expiration_updated_at, expiration_revision,
       expiration_backfill_mode, expiration_backfill_cutoff
FROM dm_conversations
WHERE id = $1
FOR NO KEY UPDATE`,
	readSQL: `
SELECT expiration_window_seconds, expiration_updated_at, expiration_revision,
       expiration_backfill_mode, expiration_backfill_cutoff
FROM dm_conversations
WHERE id = $1`,
	updatePolicySQL: `
WITH accepted AS (SELECT clock_timestamp() AS cutoff)
UPDATE dm_conversations AS c
SET expiration_window_seconds = $2,
    expiration_updated_at = accepted.cutoff,
    expiration_revision = c.expiration_revision + 1,
    expiration_backfill_mode = $3,
    expiration_backfill_cutoff = CASE WHEN $3::text IS NULL THEN NULL ELSE accepted.cutoff END
FROM accepted
WHERE c.id = $1
RETURNING c.expiration_window_seconds, c.expiration_updated_at, c.expiration_revision,
          c.expiration_backfill_mode, c.expiration_backfill_cutoff`,
	applyBatchSQL: `
WITH batch AS (
  SELECT ctid FROM dm_messages
  WHERE conversation_id = $1 AND created_at <= $2
    AND (expires_at IS NULL OR expires_at > $2)
    AND expires_at IS DISTINCT FROM
        (created_at + make_interval(secs => $3))
  LIMIT 5000
)
UPDATE dm_messages m
SET expires_at = m.created_at + make_interval(secs => $3)
FROM batch WHERE m.ctid = batch.ctid`,
	applyEligibleSQL: `
SELECT EXISTS (
  SELECT 1 FROM dm_messages
  WHERE conversation_id = $1 AND created_at <= $2
    AND (expires_at IS NULL OR expires_at > $2)
    AND expires_at IS DISTINCT FROM
        (created_at + make_interval(secs => $3))
)`,
	clearBatchSQL: `
WITH batch AS (
  SELECT ctid FROM dm_messages
  WHERE conversation_id = $1 AND expires_at > $2
  LIMIT 5000
)
UPDATE dm_messages m
SET expires_at = NULL
FROM batch WHERE m.ctid = batch.ctid`,
	clearEligibleSQL: `
SELECT EXISTS (
  SELECT 1 FROM dm_messages
  WHERE conversation_id = $1 AND expires_at > $2
)`,
	clearMarkerSQL: `
UPDATE dm_conversations
SET expiration_backfill_mode = NULL, expiration_backfill_cutoff = NULL
WHERE id = $1
  AND expiration_revision = $2
  AND expiration_backfill_mode = $3
  AND expiration_backfill_cutoff = $4`,
}

type policyState struct {
	policy Policy
	mode   sql.NullString
	cutoff sql.NullTime
}

func (s *Service) start(ctx context.Context, tx *sql.Tx, scope scope, id string, request Request) (Policy, error) {
	if tx == nil {
		return Policy{}, ErrServiceUnready
	}
	if err := request.Validate(); err != nil {
		return Policy{}, err
	}

	current, err := lockPolicy(ctx, tx, scope, id)
	if err != nil {
		return Policy{}, err
	}
	if request.Mode == "resume" {
		if current.policy.Revision != *request.Revision {
			return current.policy, ErrRevisionMismatch
		}
		if !current.policy.BackfillPending || !current.cutoff.Valid {
			return current.policy, ErrBackfillNotFound
		}
		return current.policy, nil
	}
	if current.policy.BackfillPending {
		return current.policy, ErrBackfillPending
	}

	window, mode := policyUpdate(request)
	updated, err := updatePolicy(ctx, tx, scope, id, window, mode)
	if err != nil {
		return current.policy, err
	}
	return updated.policy, nil
}

func policyUpdate(request Request) (any, any) {
	var window any
	if request.WindowSeconds != nil {
		window = *request.WindowSeconds
	}

	var mode any
	switch {
	case request.Mode == "set" && request.Retroactive == "apply":
		mode = "apply"
	case request.Mode == "clear" && request.Retroactive == "clear_pending":
		mode = "clear"
	}
	return window, mode
}

func (s *Service) runBatches(ctx context.Context, scope scope, id string, revision int64) (Policy, error) {
	if s == nil || s.db == nil {
		return Policy{}, ErrServiceUnready
	}
	// A canceled context can prevent the first policy read. Preserve the only
	// known durable value so a caller can retain its Start* policy and retry the
	// exact generation rather than being handed an invented revision zero.
	last := Policy{Revision: revision, BackfillPending: true}
	for batches := 0; batches < maxBackfillBatchesPerRequest; batches++ {
		if err := ctx.Err(); err != nil {
			last.BackfillPending = true
			return last, err
		}

		current, rereadAfterFailure, err := s.runBatch(ctx, scope, id, revision, last)
		if err != nil {
			if rereadAfterFailure {
				return pendingAfterFailure(ctx, s.db, scope, id, current, err)
			}
			return current, err
		}
		last = current
		if !current.BackfillPending {
			return current, nil
		}
	}
	last.BackfillPending = true
	return last, ErrBackfillPending
}

// runBatch owns one transaction. Its defer always releases an uncommitted
// transaction before runBatches performs an ambiguity reread on another
// connection.
func (s *Service) runBatch(ctx context.Context, scope scope, id string, revision int64, fallback Policy) (policy Policy, rereadAfterFailure bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fallback, true, fmt.Errorf("begin %s expiration backfill batch: %w", scope.name, err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback expiration backfill batch: %w", rollbackErr))
		}
	}()

	current, err := lockPolicy(ctx, tx, scope, id)
	if err != nil {
		return current.policy, false, err
	}
	if current.policy.Revision != revision {
		return current.policy, false, ErrRevisionMismatch
	}
	if !current.policy.BackfillPending || !current.mode.Valid || !current.cutoff.Valid {
		return current.policy, false, ErrBackfillNotFound
	}

	completed, err := applyOneBackfillBatch(ctx, tx, scope, id, revision, current)
	if err != nil {
		return current.policy, false, err
	}
	if completed {
		current.policy.BackfillPending = false
	}
	if err := tx.Commit(); err != nil {
		current.policy.BackfillPending = true
		if completed {
			return current.policy, true, fmt.Errorf("commit completed %s expiration backfill: %w", scope.name, err)
		}
		return current.policy, true, fmt.Errorf("commit %s expiration backfill batch: %w", scope.name, err)
	}
	return current.policy, false, nil
}

func applyOneBackfillBatch(ctx context.Context, tx *sql.Tx, scope scope, id string, revision int64, current policyState) (bool, error) {
	result, err := executeBackfillBatch(ctx, tx, scope, id, current)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count %s expiration backfill batch: %w", scope.name, err)
	}
	if affected != 0 {
		return false, nil
	}
	eligible, err := hasEligibleBackfillRow(ctx, tx, scope, id, current)
	if err != nil {
		return false, err
	}
	if eligible {
		return false, nil
	}

	finalized, err := tx.ExecContext(ctx, scope.clearMarkerSQL, id, revision, current.mode.String, current.cutoff.Time)
	if err != nil {
		return false, fmt.Errorf("clear %s expiration backfill marker: %w", scope.name, err)
	}
	cleared, err := finalized.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count cleared %s expiration backfill marker: %w", scope.name, err)
	}
	if cleared != 1 {
		return false, ErrRevisionMismatch
	}
	return true, nil
}

func hasEligibleBackfillRow(ctx context.Context, tx *sql.Tx, scope scope, id string, current policyState) (bool, error) {
	var query string
	var args []any
	switch current.mode.String {
	case "apply":
		if current.policy.WindowSeconds == nil {
			return false, ErrBackfillNotFound
		}
		query = scope.applyEligibleSQL
		args = []any{id, current.cutoff.Time, *current.policy.WindowSeconds}
	case "clear":
		query = scope.clearEligibleSQL
		args = []any{id, current.cutoff.Time}
	default:
		return false, ErrBackfillNotFound
	}

	var eligible bool
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&eligible); err != nil {
		return false, fmt.Errorf("probe %s expiration backfill eligibility: %w", scope.name, err)
	}
	return eligible, nil
}

func executeBackfillBatch(ctx context.Context, tx *sql.Tx, scope scope, id string, current policyState) (sql.Result, error) {
	switch current.mode.String {
	case "apply":
		if current.policy.WindowSeconds == nil {
			return nil, ErrBackfillNotFound
		}
		result, err := tx.ExecContext(ctx, scope.applyBatchSQL, id, current.cutoff.Time, *current.policy.WindowSeconds)
		if err != nil {
			return nil, fmt.Errorf("apply %s expiration backfill batch: %w", scope.name, err)
		}
		return result, nil
	case "clear":
		result, err := tx.ExecContext(ctx, scope.clearBatchSQL, id, current.cutoff.Time)
		if err != nil {
			return nil, fmt.Errorf("apply %s expiration backfill batch: %w", scope.name, err)
		}
		return result, nil
	default:
		return nil, ErrBackfillNotFound
	}
}

func lockPolicy(ctx context.Context, tx *sql.Tx, scope scope, id string) (policyState, error) {
	state, err := scanPolicy(tx.QueryRowContext(ctx, scope.lockSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return policyState{}, ErrScopeNotFound
	}
	if err != nil {
		return policyState{}, fmt.Errorf("lock %s expiration policy: %w", scope.name, err)
	}
	return state, nil
}

func updatePolicy(ctx context.Context, tx *sql.Tx, scope scope, id string, window, mode any) (policyState, error) {
	state, err := scanPolicy(tx.QueryRowContext(ctx, scope.updatePolicySQL, id, window, mode))
	if errors.Is(err, sql.ErrNoRows) {
		return policyState{}, ErrScopeNotFound
	}
	if err != nil {
		return policyState{}, fmt.Errorf("update %s expiration policy: %w", scope.name, err)
	}
	return state, nil
}

func readPolicy(ctx context.Context, db *sql.DB, scope scope, id string) (policyState, error) {
	state, err := scanPolicy(db.QueryRowContext(ctx, scope.readSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return policyState{}, ErrScopeNotFound
	}
	if err != nil {
		return policyState{}, fmt.Errorf("read %s expiration policy: %w", scope.name, err)
	}
	return state, nil
}

func scanPolicy(row *sql.Row) (policyState, error) {
	var window sql.NullInt64
	var updatedAt sql.NullTime
	var state policyState
	if err := row.Scan(&window, &updatedAt, &state.policy.Revision, &state.mode, &state.cutoff); err != nil {
		return policyState{}, err
	}
	if window.Valid {
		value := int(window.Int64)
		state.policy.WindowSeconds = &value
	}
	if updatedAt.Valid {
		value := updatedAt.Time
		state.policy.UpdatedAt = &value
	}
	state.policy.BackfillPending = state.mode.Valid
	return state, nil
}

func pendingAfterFailure(ctx context.Context, db *sql.DB, scope scope, id string, fallback Policy, operationErr error) (Policy, error) {
	policy, err := readPolicy(ctx, db, scope, id)
	if err != nil {
		fallback.BackfillPending = true
		return fallback, errors.Join(operationErr, err)
	}
	return policy.policy, operationErr
}
