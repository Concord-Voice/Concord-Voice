package presencehistory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
)

var lowercaseSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// UpdateSettingsRequest is the validated, presence-aware PATCH contract.
type UpdateSettingsRequest struct {
	Enabled         *bool   `json:"enabled"`
	RetentionDays   *int16  `json:"retention_days"`
	Acknowledged    *bool   `json:"acknowledged"`
	ConsentVersion  *int16  `json:"consent_version"`
	ConsentCopyHash *string `json:"consent_copy_hash"`
}

// SettingsResponse is the self-only Activity History settings response.
type SettingsResponse struct {
	Available         bool             `json:"available"`
	Enabled           bool             `json:"enabled"`
	ReconsentRequired bool             `json:"reconsent_required"`
	RetentionDays     int16            `json:"retention_days"`
	ConsentVersion    *int16           `json:"consent_version"`
	ConsentCopyHash   *string          `json:"consent_copy_hash"`
	ConsentedAt       *time.Time       `json:"consented_at"`
	RequiredConsent   *RequiredConsent `json:"required_consent,omitempty"`
}

// ServiceError is a stable HTTP-safe error classification.
type ServiceError struct {
	Status     int
	Code       string
	RetryAfter time.Duration
	Disclosure *RequiredConsent
}

func (e *ServiceError) Error() string { return e.Code }

// Service owns self-only settings, reads, and deletion.
type Service struct {
	db                *sql.DB
	repository        *Repository
	disclosure        DisclosureState
	activationEnabled bool
	senderGates       senderGateSet
	readCommitState   func(context.Context, uuid.UUID) (audienceCommitState, error)
	readClaimState    func(context.Context, uuid.UUID) (audienceCommitState, error)
	deliveryMu        sync.RWMutex
	delivery          Delivery
	transactionMu     sync.RWMutex
	transactionHooks  TransactionTestHooks
	deliveryTimeout   time.Duration
	reconcileInterval time.Duration
}

// NewService constructs a self-only Activity History service.
func NewService(db *sql.DB, disclosure DisclosureState, activationEnabled bool) *Service {
	immutableDisclosure := cloneDisclosure(disclosure)
	return &Service{
		db:                db,
		repository:        NewRepository(db, immutableDisclosure),
		disclosure:        immutableDisclosure,
		activationEnabled: activationEnabled,
		senderGates:       newSenderGateSet(),
		deliveryTimeout:   5 * time.Second,
		reconcileInterval: 5 * time.Second,
	}
}

const reconcileStaleDisclosureSQL = `
	WITH pause_clock AS (
		SELECT clock_timestamp() AS paused_at
	), paused AS (
		UPDATE user_presence_settings AS settings
		SET activity_history_enabled = FALSE,
		    activity_history_consent_version = NULL,
		    activity_history_consent_copy_hash = NULL,
		    activity_history_consented_at = NULL,
		    activity_history_reconsent_required = TRUE,
		    updated_at = pause_clock.paused_at
		FROM pause_clock
		WHERE settings.activity_history_enabled = TRUE
		  AND (
		      NOT $1
		      OR settings.activity_history_consent_version IS DISTINCT FROM $2
		      OR settings.activity_history_consent_copy_hash IS DISTINCT FROM $3
		  )
		RETURNING settings.user_id, settings.updated_at AS paused_at
	), closed_history AS (
		UPDATE presence_history AS history
		SET ended_at = paused.paused_at
		FROM paused
		WHERE history.sender_id = paused.user_id
		  AND history.ended_at IS NULL
	)
	SELECT COUNT(*) FROM paused
`

// ReconcileStaleDisclosure atomically pauses enabled rows whose accepted
// disclosure is no longer current. It intentionally preserves history and
// retention cutoffs so users can still read or delete existing data.
func (s *Service) ReconcileStaleDisclosure(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("activity history disclosure reconciliation unavailable")
	}

	available := s.disclosure.Available && s.disclosure.RequiredConsent != nil
	var version int16
	var copyHash string
	if s.disclosure.Available && s.disclosure.RequiredConsent != nil {
		consent := s.disclosure.RequiredConsent
		version = consent.Version
		copyHash = consent.CopyHash
	}
	var paused int64
	if err := s.db.QueryRowContext(
		ctx, reconcileStaleDisclosureSQL, available, version, copyHash,
	).Scan(&paused); err != nil {
		return 0, fmt.Errorf("reconcile stale activity history disclosure: %w", err)
	}
	return paused, nil
}

// TransactionTestHooks injects database/sql transaction faults in tests. Empty
// functions retain the production behavior.
type TransactionTestHooks struct {
	Begin            func(context.Context, *sql.TxOptions) (*sql.Tx, error)
	Commit           func(*sql.Tx) error
	Rollback         func(*sql.Tx) error
	RecordTransition func(context.Context, *sql.Tx, uuid.UUID, CustomTextState, CustomTextState) error
	DeleteClaim      func(context.Context, *sql.Tx, uuid.UUID, uuid.UUID) (sql.Result, error)
}

// SetTransactionTestHooks replaces transaction functions until the returned
// restore function is called. It is intended only for deterministic tests.
func (s *Service) SetTransactionTestHooks(hooks TransactionTestHooks) func() {
	if s == nil {
		return func() {
			// A nil receiver has no transaction hooks to restore.
		}
	}
	s.transactionMu.Lock()
	previous := s.transactionHooks
	s.transactionHooks = hooks
	s.transactionMu.Unlock()
	return func() {
		s.transactionMu.Lock()
		s.transactionHooks = previous
		s.transactionMu.Unlock()
	}
}

// BeginTx starts a Service-owned transaction so callers and reconciliation use
// the same production and fault-injection boundary.
func (s *Service) BeginTx(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("presence history transaction database unavailable")
	}
	s.transactionMu.RLock()
	begin := s.transactionHooks.Begin
	s.transactionMu.RUnlock()
	if begin != nil {
		return begin(ctx, options)
	}
	return s.db.BeginTx(ctx, options)
}

// CommitTx commits through the Service-owned transaction boundary.
func (s *Service) CommitTx(tx *sql.Tx) error {
	if s == nil || tx == nil {
		return errors.New("presence history commit transaction unavailable")
	}
	s.transactionMu.RLock()
	commit := s.transactionHooks.Commit
	s.transactionMu.RUnlock()
	if commit != nil {
		return commit(tx)
	}
	return tx.Commit()
}

// RollbackTx rolls back through the Service-owned transaction boundary.
func (s *Service) RollbackTx(tx *sql.Tx) error {
	if s == nil || tx == nil {
		return errors.New("presence history rollback transaction unavailable")
	}
	s.transactionMu.RLock()
	rollback := s.transactionHooks.Rollback
	s.transactionMu.RUnlock()
	if rollback != nil {
		return rollback(tx)
	}
	return tx.Rollback()
}

// RecordCustomTextTransition forwards the typed recorder contract through the
// concrete service injected into production writers.
func (s *Service) RecordCustomTextTransition(
	ctx context.Context,
	tx *sql.Tx,
	userID uuid.UUID,
	before CustomTextState,
	after CustomTextState,
) error {
	if s == nil || s.repository == nil {
		return errors.New("activity history recorder unavailable")
	}
	if tx == nil {
		return errors.New("activity history recorder transaction unavailable")
	}
	s.transactionMu.RLock()
	record := s.transactionHooks.RecordTransition
	s.transactionMu.RUnlock()
	if record != nil {
		return record(ctx, tx, userID, before, after)
	}
	return s.repository.RecordCustomTextTransition(ctx, tx, userID, before, after)
}

// GetSettings reads defaults without creating a settings row.
func (s *Service) GetSettings(ctx context.Context, userID uuid.UUID) (SettingsResponse, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT activity_history_enabled,
		       activity_history_reconsent_required,
		       activity_history_retention_days,
		       activity_history_consent_version,
		       activity_history_consent_copy_hash,
		       activity_history_consented_at
		FROM user_presence_settings
		WHERE user_id = $1
	`, userID)
	response, err := s.scanSettings(row)
	if errors.Is(err, sql.ErrNoRows) {
		return s.defaultSettings(), nil
	}
	if err != nil {
		return SettingsResponse{}, fmt.Errorf("read activity history settings: %w", err)
	}
	return response, nil
}

// UpdateSettings applies one validated enable, disable, or retention mutation.
func (s *Service) UpdateSettings(
	ctx context.Context,
	userID uuid.UUID,
	request UpdateSettingsRequest,
) (result SettingsResponse, returnErr error) {
	action, err := s.validateUpdate(request)
	if err != nil {
		return SettingsResponse{}, err
	}
	if action == settingsActionDisable {
		if err := s.DisableAndDelete(ctx, userID); err != nil {
			return SettingsResponse{}, err
		}
		return s.GetSettings(ctx, userID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SettingsResponse{}, fmt.Errorf("begin activity history settings update: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	defer joinRollbackError(tx, "rollback activity history settings update", &returnErr)

	settings, err := lockUserAndSettings(ctx, tx, userID)
	if err != nil {
		return SettingsResponse{}, err
	}
	if err := lockPendingOperation(ctx, tx, userID); err != nil {
		return SettingsResponse{}, err
	}

	var reconsentRequired bool
	if err := tx.QueryRowContext(ctx, `
		SELECT activity_history_reconsent_required
		FROM user_presence_settings
		WHERE user_id = $1
	`, userID).Scan(&reconsentRequired); err != nil {
		return SettingsResponse{}, fmt.Errorf("read activity history reconsent state: %w", err)
	}

	if err := s.applySettingsAction(
		ctx, tx, userID, request, action, settings.RetentionDays, reconsentRequired,
	); err != nil {
		return SettingsResponse{}, err
	}

	response, err := s.scanSettings(tx.QueryRowContext(ctx, `
		SELECT activity_history_enabled,
		       activity_history_reconsent_required,
		       activity_history_retention_days,
		       activity_history_consent_version,
		       activity_history_consent_copy_hash,
		       activity_history_consented_at
		FROM user_presence_settings
		WHERE user_id = $1
	`, userID))
	if err != nil {
		return SettingsResponse{}, fmt.Errorf("read updated activity history settings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SettingsResponse{}, fmt.Errorf("commit activity history settings update: %w", err)
	}
	return response, nil
}

// List prunes exact-cutoff rows under canonical locks, then returns a self-only
// keyset page.
func (s *Service) List(
	ctx context.Context,
	userID uuid.UUID,
	options ListOptions,
) (result HistoryPage, returnErr error) {
	if err := validateListOptions(options); err != nil {
		return HistoryPage{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return HistoryPage{}, fmt.Errorf("begin activity history read prune: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	defer joinRollbackError(tx, "rollback activity history read prune", &returnErr)
	if err := lockUser(ctx, tx, userID); err != nil {
		return HistoryPage{}, err
	}
	if err := lockSettingsIfPresent(ctx, tx, userID); err != nil {
		return HistoryPage{}, err
	}
	if err := lockPendingOperation(ctx, tx, userID); err != nil {
		return HistoryPage{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM presence_history
		WHERE sender_id = $1 AND expires_at <= clock_timestamp()
	`, userID); err != nil {
		return HistoryPage{}, fmt.Errorf("prune expired activity history before read: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return HistoryPage{}, fmt.Errorf("commit activity history read prune: %w", err)
	}
	page, err := s.repository.List(ctx, userID, options)
	if errors.Is(err, ErrInvalidListOptions) {
		return HistoryPage{}, invalidRequestError()
	}
	return page, err
}

// DisableAndDelete atomically withdraws consent and deletes all history without
// changing the live Custom Status.
func (s *Service) DisableAndDelete(ctx context.Context, userID uuid.UUID) (returnErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin activity history disable: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	defer joinRollbackError(tx, "rollback activity history disable", &returnErr)
	if err := lockUser(ctx, tx, userID); err != nil {
		return err
	}
	present, err := lockSettingsPresence(ctx, tx, userID)
	if err != nil {
		return err
	}
	if err := lockPendingOperation(ctx, tx, userID); err != nil {
		return err
	}
	if present {
		if _, err := tx.ExecContext(ctx, `
			UPDATE user_presence_settings
			SET activity_history_enabled = FALSE,
			    activity_history_consent_version = NULL,
			    activity_history_consent_copy_hash = NULL,
			    activity_history_consented_at = NULL,
			    activity_history_reconsent_required = FALSE,
			    updated_at = clock_timestamp()
			WHERE user_id = $1
		`, userID); err != nil {
			return fmt.Errorf("disable activity history: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM presence_history WHERE sender_id = $1`, userID); err != nil {
		return fmt.Errorf("delete activity history: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit activity history disable: %w", err)
	}
	return nil
}

type settingsAction int

const (
	settingsActionEnable settingsAction = iota + 1
	settingsActionDisable
	settingsActionRetention
)

func (s *Service) validateUpdate(request UpdateSettingsRequest) (settingsAction, error) {
	if request.Enabled != nil {
		if *request.Enabled {
			return s.validateEnableUpdate(request)
		}
		if request.RetentionDays != nil || request.Acknowledged != nil ||
			request.ConsentVersion != nil || request.ConsentCopyHash != nil {
			return 0, invalidRequestError()
		}
		return settingsActionDisable, nil
	}
	if request.RetentionDays != nil && request.Acknowledged == nil &&
		request.ConsentVersion == nil && request.ConsentCopyHash == nil && validRetention(*request.RetentionDays) {
		return settingsActionRetention, nil
	}
	return 0, invalidRequestError()
}

func (s *Service) validateEnableUpdate(request UpdateSettingsRequest) (settingsAction, error) {
	if request.RetentionDays == nil || request.Acknowledged == nil || !*request.Acknowledged ||
		request.ConsentVersion == nil || request.ConsentCopyHash == nil ||
		!validRetention(*request.RetentionDays) || !lowercaseSHA256.MatchString(*request.ConsentCopyHash) {
		return 0, invalidRequestError()
	}
	if !s.activationEnabled {
		return 0, &ServiceError{Status: http.StatusServiceUnavailable, Code: "activity_history_activation_unavailable"}
	}
	if !s.disclosure.Available || s.disclosure.RequiredConsent == nil {
		return 0, &ServiceError{Status: http.StatusServiceUnavailable, Code: "activity_history_disclosure_unavailable"}
	}
	current := s.disclosure.RequiredConsent
	if *request.ConsentVersion != current.Version || *request.ConsentCopyHash != current.CopyHash {
		return 0, &ServiceError{
			Status:     http.StatusConflict,
			Code:       "activity_history_consent_mismatch",
			Disclosure: cloneRequiredConsent(current),
		}
	}
	return settingsActionEnable, nil
}

func (s *Service) applySettingsAction(
	ctx context.Context,
	tx *sql.Tx,
	userID uuid.UUID,
	request UpdateSettingsRequest,
	action settingsAction,
	currentRetention int16,
	reconsentRequired bool,
) error {
	if action == settingsActionEnable {
		if *request.RetentionDays != currentRetention {
			if err := updateRetention(
				ctx, tx, userID, currentRetention, *request.RetentionDays, false,
			); err != nil {
				return err
			}
		}
		return s.enableWithConsent(ctx, tx, userID, request)
	}
	return updateRetention(
		ctx, tx, userID, currentRetention, *request.RetentionDays, reconsentRequired,
	)
}

func (s *Service) enableWithConsent(
	ctx context.Context,
	tx *sql.Tx,
	userID uuid.UUID,
	request UpdateSettingsRequest,
) error {
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("read activity history consent clock: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET activity_history_enabled = TRUE,
		    activity_history_retention_days = $2,
		    activity_history_consent_version = $3,
		    activity_history_consent_copy_hash = $4,
		    activity_history_consented_at = $5,
		    activity_history_reconsent_required = FALSE,
		    updated_at = $5
		WHERE user_id = $1
	`, userID, *request.RetentionDays, *request.ConsentVersion, *request.ConsentCopyHash, now); err != nil {
		return fmt.Errorf("enable activity history: %w", err)
	}
	return nil
}

func updateRetention(
	ctx context.Context,
	tx *sql.Tx,
	userID uuid.UUID,
	current int16,
	next int16,
	reconsentRequired bool,
) error {
	if reconsentRequired && next > current {
		return &ServiceError{Status: http.StatusConflict, Code: "activity_history_reconsent_required"}
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("read activity history retention clock: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM presence_history
		WHERE sender_id = $1 AND expires_at <= $2
	`, userID, now); err != nil {
		return fmt.Errorf("delete already expired activity history: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET activity_history_retention_days = $2,
		    updated_at = $3
		WHERE user_id = $1
	`, userID, next, now); err != nil {
		return fmt.Errorf("update activity history retention: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE presence_history
		SET expires_at = recorded_at + ($2::INTEGER * INTERVAL '1 day')
		WHERE sender_id = $1 AND expires_at > $3
	`, userID, next, now); err != nil {
		return fmt.Errorf("recompute activity history expiry: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM presence_history
		WHERE sender_id = $1 AND expires_at <= $2
	`, userID, now); err != nil {
		return fmt.Errorf("delete newly expired activity history: %w", err)
	}
	return nil
}

func validRetention(value int16) bool {
	switch value {
	case 7, 30, 90, 365:
		return true
	default:
		return false
	}
}

func validateListOptions(options ListOptions) error {
	if options.Limit < 0 || options.Limit > maxPageLimit {
		return invalidRequestError()
	}
	if options.Before != nil &&
		(options.Before.Version != 1 || options.Before.RecordedAt.IsZero() || options.Before.ID == uuid.Nil) {
		return invalidRequestError()
	}
	return nil
}

func invalidRequestError() *ServiceError {
	return &ServiceError{Status: http.StatusBadRequest, Code: "activity_history_invalid_request"}
}

func joinRollbackError(tx *sql.Tx, operation string, returnErr *error) {
	rollbackErr := tx.Rollback()
	if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
		return
	}
	*returnErr = errors.Join(*returnErr, fmt.Errorf("%s: %w", operation, rollbackErr))
}

func lockUser(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error {
	var locked uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&locked); err != nil {
		return fmt.Errorf("lock activity history owner: %w", err)
	}
	return nil
}

func lockSettingsIfPresent(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error {
	_, err := lockSettingsPresence(ctx, tx, userID)
	return err
}

func lockSettingsPresence(ctx context.Context, tx *sql.Tx, userID uuid.UUID) (bool, error) {
	var locked uuid.UUID
	err := tx.QueryRowContext(ctx, `
		SELECT user_id FROM user_presence_settings WHERE user_id = $1 FOR UPDATE
	`, userID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock activity history settings: %w", err)
	}
	return true, nil
}

func lockPendingOperation(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error {
	var operationID uuid.UUID
	err := tx.QueryRowContext(ctx, `
		SELECT operation_id
		FROM presence_settings_pending_operations
		WHERE user_id = $1
		FOR UPDATE
	`, userID).Scan(&operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock pending activity history operation: %w", err)
	}
	return nil
}

func (s *Service) defaultSettings() SettingsResponse {
	return SettingsResponse{
		Available:       s.disclosure.Available,
		RetentionDays:   30,
		RequiredConsent: cloneRequiredConsent(s.disclosure.RequiredConsent),
	}
}

func (s *Service) scanSettings(row rowScanner) (SettingsResponse, error) {
	var (
		response  SettingsResponse
		version   sql.NullInt16
		hash      sql.NullString
		consented sql.NullTime
	)
	if err := row.Scan(
		&response.Enabled,
		&response.ReconsentRequired,
		&response.RetentionDays,
		&version,
		&hash,
		&consented,
	); err != nil {
		return SettingsResponse{}, err
	}
	response.Available = s.disclosure.Available
	response.RequiredConsent = cloneRequiredConsent(s.disclosure.RequiredConsent)
	if version.Valid {
		value := version.Int16
		response.ConsentVersion = &value
	}
	if hash.Valid {
		value := hash.String
		response.ConsentCopyHash = &value
	}
	if consented.Valid {
		value := consented.Time
		response.ConsentedAt = &value
	}
	return response, nil
}

func cloneRequiredConsent(consent *RequiredConsent) *RequiredConsent {
	if consent == nil {
		return nil
	}
	cloned := *consent
	cloned.Details = append([]string(nil), consent.Details...)
	return &cloned
}
