package users

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/google/uuid"
)

const (
	activitySettingsCleanupEvidenceVersion = 1
	activitySettingsCleanupResumeAttempts  = 2
)

var (
	errActivitySettingsCleanupPending = errors.New("activity settings cleanup remains pending")
	errInvalidActivityCleanupEvidence = errors.New("invalid activity settings cleanup evidence")
)

type persistedActivityPolicySettings struct {
	MasterEnabled          bool `json:"master_enabled"`
	ServerVoiceTier        int  `json:"server_voice_tier"`
	ServerVoiceShowDetails bool `json:"server_voice_show_details"`
	PrivateCallTier        int  `json:"private_call_tier"`
	PrivateCallShowDetails bool `json:"private_call_show_details"`
}

type activitySettingsCleanupEvidence struct {
	Version    int                             `json:"version"`
	Suppressed bool                            `json:"suppressed,omitempty"`
	Before     persistedActivityPolicySettings `json:"before,omitempty"`
	After      persistedActivityPolicySettings `json:"after,omitempty"`
}

type activitySettingsCleanupMarker struct {
	OperationID uuid.UUID
	Suppressed  bool
	Before      presence.ActivityPolicySettings
	After       presence.ActivityPolicySettings
}

type activitySettingsCleanupDBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func activitySettingsCleanupAdvisoryKey(userID uuid.UUID) (int64, error) {
	if userID == uuid.Nil {
		return 0, errors.New("invalid activity settings cleanup lock user")
	}
	digest := sha256.Sum256([]byte("activity_settings_cleanup\x00" + userID.String()))
	// PostgreSQL advisory locks accept signed int64 keys; preserve all digest bits.
	return int64(binary.BigEndian.Uint64(digest[:8])), nil //nolint:gosec
}

func lockActivitySettingsCleanup(
	ctx context.Context,
	tx *sql.Tx,
	userID uuid.UUID,
) error {
	if tx == nil {
		return errors.New("activity settings cleanup transaction unavailable")
	}
	lockKey, err := activitySettingsCleanupAdvisoryKey(userID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey,
	); err != nil {
		return fmt.Errorf("lock activity settings cleanup: %w", err)
	}
	return nil
}

func encodeActivitySettingsCleanupEvidence(
	before, after presence.ActivityPolicySettings,
) ([]byte, error) {
	if before == after {
		return nil, fmt.Errorf("%w: unchanged policy", errInvalidActivityCleanupEvidence)
	}
	evidence := activitySettingsCleanupEvidence{
		Version: activitySettingsCleanupEvidenceVersion,
		Before:  persistActivityPolicySettings(before),
		After:   persistActivityPolicySettings(after),
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return nil, fmt.Errorf("encode activity settings cleanup evidence: %w", err)
	}
	return encoded, nil
}

func decodeActivitySettingsCleanupEvidence(
	raw []byte,
) (presence.ActivityPolicySettings, presence.ActivityPolicySettings, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var evidence activitySettingsCleanupEvidence
	if err := decoder.Decode(&evidence); err != nil {
		return presence.ActivityPolicySettings{}, presence.ActivityPolicySettings{},
			false, fmt.Errorf("%w: %v", errInvalidActivityCleanupEvidence, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return presence.ActivityPolicySettings{}, presence.ActivityPolicySettings{},
			false, fmt.Errorf("%w: trailing JSON value", errInvalidActivityCleanupEvidence)
	}
	if evidence.Version != activitySettingsCleanupEvidenceVersion {
		return presence.ActivityPolicySettings{}, presence.ActivityPolicySettings{},
			false, fmt.Errorf("%w: unsupported version", errInvalidActivityCleanupEvidence)
	}
	if evidence.Suppressed {
		return presence.ActivityPolicySettings{}, presence.ActivityPolicySettings{}, true, nil
	}
	before, err := restoreActivityPolicySettings(evidence.Before)
	if err != nil {
		return presence.ActivityPolicySettings{}, presence.ActivityPolicySettings{}, false, err
	}
	after, err := restoreActivityPolicySettings(evidence.After)
	if err != nil {
		return presence.ActivityPolicySettings{}, presence.ActivityPolicySettings{}, false, err
	}
	if before == after {
		return presence.ActivityPolicySettings{}, presence.ActivityPolicySettings{},
			false, fmt.Errorf("%w: unchanged policy", errInvalidActivityCleanupEvidence)
	}
	return before, after, false, nil
}

func persistActivityPolicySettings(
	settings presence.ActivityPolicySettings,
) persistedActivityPolicySettings {
	return persistedActivityPolicySettings{
		MasterEnabled:          settings.MasterEnabled,
		ServerVoiceTier:        int(settings.ServerVoiceTier),
		ServerVoiceShowDetails: settings.ServerVoiceShowDetails,
		PrivateCallTier:        int(settings.PrivateCallTier),
		PrivateCallShowDetails: settings.PrivateCallShowDetails,
	}
}

func restoreActivityPolicySettings(
	settings persistedActivityPolicySettings,
) (presence.ActivityPolicySettings, error) {
	serverTier, err := activityTierFromPersisted(settings.ServerVoiceTier)
	if err != nil {
		return presence.ActivityPolicySettings{}, fmt.Errorf(
			"%w: server voice tier", errInvalidActivityCleanupEvidence,
		)
	}
	privateTier, err := activityTierFromPersisted(settings.PrivateCallTier)
	if err != nil {
		return presence.ActivityPolicySettings{}, fmt.Errorf(
			"%w: private call tier", errInvalidActivityCleanupEvidence,
		)
	}
	return presence.ActivityPolicySettings{
		MasterEnabled:          settings.MasterEnabled,
		ServerVoiceTier:        serverTier,
		ServerVoiceShowDetails: settings.ServerVoiceShowDetails,
		PrivateCallTier:        privateTier,
		PrivateCallShowDetails: settings.PrivateCallShowDetails,
	}, nil
}

func activitySettingsCleanupPending(
	ctx context.Context,
	tx *sql.Tx,
	userID uuid.UUID,
) (bool, error) {
	var pending bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM activity_settings_pending_cleanups WHERE user_id = $1
		)
	`, userID).Scan(&pending); err != nil {
		return false, fmt.Errorf("check pending activity settings cleanup: %w", err)
	}
	return pending, nil
}

func insertActivitySettingsCleanup(
	ctx context.Context,
	tx *sql.Tx,
	userID, operationID uuid.UUID,
	before, after presence.ActivityPolicySettings,
) error {
	evidence, err := encodeActivitySettingsCleanupEvidence(before, after)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO activity_settings_pending_cleanups (
			user_id, operation_id, evidence
		) VALUES ($1, $2, $3)
	`, userID, operationID, string(evidence)); err != nil {
		return fmt.Errorf("insert pending activity settings cleanup: %w", err)
	}
	return nil
}

func loadActivitySettingsCleanup(
	ctx context.Context,
	db activitySettingsCleanupDBTX,
	userID uuid.UUID,
) (*activitySettingsCleanupMarker, error) {
	var operationID uuid.UUID
	var evidence []byte
	err := db.QueryRowContext(ctx, `
		SELECT operation_id, evidence
		FROM activity_settings_pending_cleanups
		WHERE user_id = $1
		FOR UPDATE
	`, userID).Scan(&operationID, &evidence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load pending activity settings cleanup: %w", err)
	}
	if operationID == uuid.Nil {
		return nil, fmt.Errorf("%w: missing operation", errInvalidActivityCleanupEvidence)
	}
	before, after, suppressed, err := decodeActivitySettingsCleanupEvidence(evidence)
	if err != nil {
		return nil, err
	}
	return &activitySettingsCleanupMarker{
		OperationID: operationID,
		Suppressed:  suppressed,
		Before:      before,
		After:       after,
	}, nil
}

func markActivitySettingsCleanupSuppressed(
	ctx context.Context,
	db activitySettingsCleanupDBTX,
	userID, operationID uuid.UUID,
) error {
	receipt, err := json.Marshal(activitySettingsCleanupEvidence{
		Version: activitySettingsCleanupEvidenceVersion, Suppressed: true,
	})
	if err != nil {
		return fmt.Errorf("encode completed activity settings cleanup: %w", err)
	}
	result, err := db.ExecContext(ctx, `
		UPDATE activity_settings_pending_cleanups
		SET evidence = $3, updated_at = clock_timestamp()
		WHERE user_id = $1 AND operation_id = $2
	`, userID, operationID, string(receipt))
	if err != nil {
		return fmt.Errorf("mark activity settings cleanup suppressed: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read marked activity settings cleanup rows: %w", err)
	}
	if rows != 1 {
		return errActivitySettingsCleanupPending
	}
	return nil
}

func deleteActivitySettingsCleanup(
	ctx context.Context,
	db activitySettingsCleanupDBTX,
	userID, operationID uuid.UUID,
) error {
	if _, err := db.ExecContext(ctx, `
		DELETE FROM activity_settings_pending_cleanups
		WHERE user_id = $1 AND operation_id = $2
	`, userID, operationID); err != nil {
		return fmt.Errorf("delete completed activity settings cleanup: %w", err)
	}
	return nil
}

func joinActivitySettingsCleanupRollback(rollback func() error, returnErr *error) {
	rollbackErr := rollback()
	if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
		return
	}
	wrapped := fmt.Errorf("activity settings cleanup rollback: %w", rollbackErr)
	if *returnErr == nil {
		*returnErr = &presenceWriterFailure{
			status: 503, class: "activity_cleanup", cause: wrapped,
		}
		return
	}
	*returnErr = errors.Join(*returnErr, wrapped)
}

func (h *Handler) resumePendingActivitySettingsCleanup(
	requestCtx context.Context,
	userID uuid.UUID,
) (resumed bool, returnErr error) {
	cleanupCtx, cancelCleanup := context.WithTimeout(
		context.WithoutCancel(requestCtx),
		activityCleanupTimeout,
	)
	defer cancelCleanup()
	if h == nil || h.db == nil {
		return false, activitySettingsCleanupFailure(
			503, errors.New("activity settings cleanup database unavailable"),
		)
	}
	marker, err := h.suppressPendingActivitySettingsCleanup(cleanupCtx, userID)
	if err != nil {
		return marker != nil, err
	}
	if marker == nil {
		return false, nil
	}
	return true, h.finalizeSuppressedActivitySettingsCleanup(
		cleanupCtx, userID, marker.OperationID,
	)
}

func (h *Handler) suppressPendingActivitySettingsCleanup(
	cleanupCtx context.Context,
	userID uuid.UUID,
) (marker *activitySettingsCleanupMarker, returnErr error) {
	tx, err := h.db.BeginTx(cleanupCtx, nil)
	if err != nil {
		return nil, activitySettingsCleanupFailure(503, err)
	}
	defer tx.Rollback() //nolint:errcheck
	defer joinActivitySettingsCleanupRollback(tx.Rollback, &returnErr)
	if err := lockActivitySettingsCleanup(cleanupCtx, tx, userID); err != nil {
		return nil, activitySettingsCleanupFailure(503, err)
	}
	marker, err = loadActivitySettingsCleanup(cleanupCtx, tx, userID)
	if err != nil {
		return nil, classifyActivitySettingsCleanupFailure(err)
	}
	if marker == nil || marker.Suppressed {
		return marker, commitActivitySettingsCleanup(tx)
	}
	if h.activitySuppressor == nil {
		return marker, activitySettingsCleanupFailure(
			503, errors.New("activity settings cleanup unavailable"),
		)
	}
	if err := h.activitySuppressor.ApplySettingsSuppressionAlreadyGated(
		cleanupCtx,
		userID,
		marker.Before,
		marker.After,
	); err != nil {
		return marker, activitySettingsCleanupFailure(503, err)
	}
	if err := markActivitySettingsCleanupSuppressed(
		cleanupCtx, tx, userID, marker.OperationID,
	); err != nil {
		return marker, activitySettingsCleanupFailure(503, err)
	}
	if err := h.commitSuppressedActivitySettingsCleanup(
		cleanupCtx, tx, userID, marker.OperationID,
	); err != nil {
		return marker, err
	}
	marker.Suppressed = true
	return marker, nil
}

func (h *Handler) finalizeSuppressedActivitySettingsCleanup(
	cleanupCtx context.Context,
	userID, operationID uuid.UUID,
) (returnErr error) {
	tx, err := h.db.BeginTx(cleanupCtx, nil)
	if err != nil {
		return activitySettingsCleanupFailure(503, err)
	}
	defer tx.Rollback() //nolint:errcheck
	defer joinActivitySettingsCleanupRollback(tx.Rollback, &returnErr)
	if err := lockActivitySettingsCleanup(cleanupCtx, tx, userID); err != nil {
		return activitySettingsCleanupFailure(503, err)
	}
	marker, err := loadActivitySettingsCleanup(cleanupCtx, tx, userID)
	if err != nil {
		return classifyActivitySettingsCleanupFailure(err)
	}
	if marker == nil {
		return commitActivitySettingsCleanup(tx)
	}
	if marker.OperationID != operationID || !marker.Suppressed {
		return activitySettingsCleanupFailure(503, errActivitySettingsCleanupPending)
	}
	if err := deleteActivitySettingsCleanup(
		cleanupCtx, tx, userID, operationID,
	); err != nil {
		return activitySettingsCleanupFailure(503, err)
	}
	return commitActivitySettingsCleanup(tx)
}

func (h *Handler) commitSuppressedActivitySettingsCleanup(
	ctx context.Context,
	tx *sql.Tx,
	userID, operationID uuid.UUID,
) error {
	commitErr := tx.Commit()
	if commitErr == nil {
		return nil
	}
	if err := h.persistSuppressedActivitySettingsCleanup(
		ctx, userID, operationID,
	); err != nil {
		return activitySettingsCleanupFailure(503, errors.Join(commitErr, err))
	}
	return nil
}

func (h *Handler) persistSuppressedActivitySettingsCleanup(
	ctx context.Context,
	userID, operationID uuid.UUID,
) (returnErr error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	defer joinActivitySettingsCleanupRollback(tx.Rollback, &returnErr)
	if err := lockActivitySettingsCleanup(ctx, tx, userID); err != nil {
		return err
	}
	marker, err := loadActivitySettingsCleanup(ctx, tx, userID)
	if err != nil {
		return err
	}
	if marker == nil {
		return tx.Commit()
	}
	if marker.OperationID != operationID {
		return errActivitySettingsCleanupPending
	}
	if !marker.Suppressed {
		if err := markActivitySettingsCleanupSuppressed(
			ctx, tx, userID, operationID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func commitActivitySettingsCleanup(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return activitySettingsCleanupFailure(503, err)
	}
	return nil
}

func classifyActivitySettingsCleanupFailure(err error) error {
	status := 503
	if errors.Is(err, errInvalidActivityCleanupEvidence) {
		status = 500
	}
	return activitySettingsCleanupFailure(status, err)
}

func activitySettingsCleanupFailure(status int, cause error) error {
	return &presenceWriterFailure{
		status: status, class: "activity_cleanup", cause: cause,
	}
}
