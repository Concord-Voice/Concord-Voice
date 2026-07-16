package users

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

const (
	presenceOverrideCategoryCustomText    = "custom_text"
	presenceOverrideMaxEncryptedDataBytes = 65_536
	presenceOverrideMaxRequestBodyBytes   = 128 * 1_024
	presenceOverrideMaxTargets            = 1_000
	errMsgFailedReplacePresenceOverrides  = "Failed to replace presence overrides"
)

type presenceOverrideRequest struct {
	EncryptedData   string    `json:"encrypted_data"`
	ExpectedVersion int       `json:"expected_version"`
	ExcludedUserIDs *[]string `json:"excluded_user_ids"`
}

type normalizedPresenceOverrideRequest struct {
	EncryptedData   string
	ExpectedVersion int
	ExcludedUserIDs []uuid.UUID
}

type presenceOverridePreferenceResponse struct {
	EncryptedData string    `json:"encrypted_data"`
	Version       int       `json:"version"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type presenceOverrideResponse struct {
	Preference *presenceOverridePreferenceResponse `json:"preference"`
}

type presenceOverrideWriteResult struct {
	Version   int
	Operation presencehistory.AudienceOperation
	Plan      presencehistory.DeliveryPlan
}

type presenceOverrideAudienceFunc func(context.Context, presence.DBTX, uuid.UUID) (map[uuid.UUID]bool, error)

type presenceOverridePayloadFunc func(context.Context, presence.DBTX, uuid.UUID) (*websocket.CustomTextPayload, error)

type presenceOverrideVersionConflictError struct {
	CurrentVersion int
}

func (e *presenceOverrideVersionConflictError) Error() string {
	return "presence override version conflict"
}

type presenceOverrideOperationError struct {
	Operation string
	cause     error
}

func (e *presenceOverrideOperationError) Error() string {
	return "presence override " + e.Operation + " failed"
}

func (e *presenceOverrideOperationError) Unwrap() error {
	return e.cause
}

func decodePresenceOverrideRequest(w http.ResponseWriter, r *http.Request) (presenceOverrideRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, presenceOverrideMaxRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	var req presenceOverrideRequest
	if err := decoder.Decode(&req); err != nil {
		return presenceOverrideRequest{}, err
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return presenceOverrideRequest{}, fmt.Errorf("trailing JSON is not allowed")
	}
	return req, nil
}

func validatePresenceOverrideRequest(category string, senderID uuid.UUID, req presenceOverrideRequest) (normalizedPresenceOverrideRequest, error) {
	if category != presenceOverrideCategoryCustomText {
		return normalizedPresenceOverrideRequest{}, fmt.Errorf("category must be custom_text")
	}
	if req.EncryptedData == "" {
		return normalizedPresenceOverrideRequest{}, fmt.Errorf("encrypted_data must be nonempty")
	}
	if len(req.EncryptedData) > presenceOverrideMaxEncryptedDataBytes {
		return normalizedPresenceOverrideRequest{}, fmt.Errorf("encrypted_data must be at most 65536 bytes")
	}
	if _, err := base64.StdEncoding.DecodeString(req.EncryptedData); err != nil {
		return normalizedPresenceOverrideRequest{}, fmt.Errorf("encrypted_data must be valid base64")
	}
	if req.ExpectedVersion < 0 {
		return normalizedPresenceOverrideRequest{}, fmt.Errorf("expected_version must be nonnegative")
	}
	if req.ExcludedUserIDs == nil {
		return normalizedPresenceOverrideRequest{}, fmt.Errorf("excluded_user_ids is required")
	}

	unique := make(map[uuid.UUID]struct{}, len(*req.ExcludedUserIDs))
	for _, rawID := range *req.ExcludedUserIDs {
		id, err := uuid.Parse(rawID)
		if err != nil {
			return normalizedPresenceOverrideRequest{}, fmt.Errorf("excluded_user_ids must contain valid UUIDs")
		}
		if id == senderID {
			return normalizedPresenceOverrideRequest{}, fmt.Errorf("excluded_user_ids cannot contain self")
		}
		unique[id] = struct{}{}
	}
	if len(unique) > presenceOverrideMaxTargets {
		return normalizedPresenceOverrideRequest{}, fmt.Errorf("excluded_user_ids must contain at most 1000 unique users")
	}

	normalized := make([]uuid.UUID, 0, len(unique))
	for id := range unique {
		normalized = append(normalized, id)
	}
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].String() < normalized[j].String()
	})

	return normalizedPresenceOverrideRequest{
		EncryptedData:   req.EncryptedData,
		ExpectedVersion: req.ExpectedVersion,
		ExcludedUserIDs: normalized,
	}, nil
}

// GetPresenceOverrides returns only the caller's opaque encrypted preference.
// The materialized target rows are enforcement state and never cross the API.
func (h *Handler) GetPresenceOverrides(c *gin.Context) {
	senderID, err := uuid.Parse(c.GetString("user_id"))
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}
	if c.Param("category") != presenceOverrideCategoryCustomText {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid presence override category"})
		return
	}

	var preference presenceOverridePreferenceResponse
	err = h.db.QueryRowContext(c.Request.Context(), `
		SELECT encrypted_data, version, updated_at
		FROM presence_override_preferences
		WHERE user_id = $1 AND category = $2
	`, senderID, presenceOverrideCategoryCustomText).Scan(
		&preference.EncryptedData, &preference.Version, &preference.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusOK, presenceOverrideResponse{Preference: nil})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch presence overrides", "error_class", "query")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch presence overrides"})
		return
	}
	c.JSON(http.StatusOK, presenceOverrideResponse{Preference: &preference})
}

// ReplacePresenceOverrides atomically replaces the caller's encrypted custom
// status preference and its materialized recipient exclusions.
func (h *Handler) ReplacePresenceOverrides(c *gin.Context) {
	senderID, err := uuid.Parse(c.GetString("user_id"))
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	req, err := decodePresenceOverrideRequest(c.Writer, c.Request)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Request body too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	normalized, err := validatePresenceOverrideRequest(c.Param("category"), senderID, req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid presence override request"})
		return
	}
	if h.presenceHistory == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgFailedReplacePresenceOverrides})
		return
	}
	var result presenceOverrideWriteResult
	err = h.presenceHistory.WithReadySender(c.Request.Context(), senderID, func() error {
		var writeErr error
		result, writeErr = h.replacePresenceOverride(
			c.Request.Context(), senderID, c.Param("category"), normalized,
			presence.ComputeCustomTextAudience, prepareCurrentCustomTextPayload,
		)
		return writeErr
	})
	if err != nil {
		var conflict *presenceOverrideVersionConflictError
		if errors.As(err, &conflict) {
			c.JSON(http.StatusConflict, gin.H{
				"code":            "presence_override_version_conflict",
				"current_version": conflict.CurrentVersion,
			})
			return
		}
		h.respondPresenceWriterFailure(c, errMsgFailedReplacePresenceOverrides, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"version": result.Version})
}

func (h *Handler) replacePresenceOverride(
	ctx context.Context,
	senderID uuid.UUID,
	category string,
	req normalizedPresenceOverrideRequest,
	computeAudience presenceOverrideAudienceFunc,
	preparePayload presenceOverridePayloadFunc,
) (result presenceOverrideWriteResult, err error) {
	tx, err := h.presenceHistory.BeginTx(ctx, nil)
	if err != nil {
		return result, &presenceWriterFailure{status: 500, class: "begin", cause: err}
	}
	if tx == nil {
		return result, &presenceWriterFailure{status: 500, class: "begin", cause: errors.New("presence override transaction missing")}
	}
	defer tx.Rollback() //nolint:errcheck
	defer h.joinPresenceWriterRollback(tx, &err)

	operation, err := h.presenceHistory.BeginAudienceOperation(
		ctx, tx, senderID, presencehistory.OrdinaryAudienceWrite,
	)
	if err != nil {
		return result, err
	}
	currentVersion, err := readPresenceOverrideVersion(ctx, tx, senderID, category, true)
	if err != nil {
		return result, presenceOverrideOperation("read_version", err)
	}
	if currentVersion != req.ExpectedVersion {
		return result, &presenceOverrideVersionConflictError{CurrentVersion: currentVersion}
	}
	materializedTargetIDs, err := selectExistingPresenceOverrideTargets(ctx, tx, req.ExcludedUserIDs)
	if err != nil {
		return result, err
	}

	oldAudience, err := computeAudience(ctx, tx, senderID)
	if err != nil {
		return result, presenceOverrideOperation("prepare_old_audience", err)
	}
	newVersion, err := upsertPresenceOverridePreference(
		ctx, tx, senderID, category, req.EncryptedData, req.ExpectedVersion,
	)
	if err != nil {
		return result, err
	}
	if err := replacePresenceOverrideTargets(ctx, tx, senderID, category, materializedTargetIDs); err != nil {
		return result, err
	}

	newAudience, err := computeAudience(ctx, tx, senderID)
	if err != nil {
		return result, presenceOverrideOperation("prepare_new_audience", err)
	}
	payload, err := preparePayload(ctx, tx, senderID)
	if err != nil {
		return result, presenceOverrideOperation("prepare_payload", err)
	}

	clearRecipients := map[uuid.UUID]bool{}
	updateRecipients := map[uuid.UUID]bool{}
	var deliveryPayload *presencehistory.CustomTextState
	if payload != nil {
		clearRecipients = audienceDifference(oldAudience, newAudience)
		updateRecipients = audienceDifference(newAudience, oldAudience)
		deliveryPayload = &presencehistory.CustomTextState{Text: payload.Text, Emoji: payload.Emoji}
	}
	result = presenceOverrideWriteResult{
		Version:   newVersion,
		Operation: operation,
		Plan: presencehistory.DeliveryPlan{
			Mode:             presencehistory.DeliveryExactDelta,
			OperationID:      operation.ID,
			SenderID:         senderID,
			ClearRecipients:  clearRecipients,
			UpdateRecipients: updateRecipients,
			Payload:          deliveryPayload,
			OverrideVersion:  &newVersion,
		},
	}
	if err := h.commitAndClaimPresenceWriter(ctx, tx, operation, result.Plan); err != nil {
		return result, err
	}
	return result, nil
}

func replacePresenceOverrideTargets(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
	category string,
	excludedUserIDs []uuid.UUID,
) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM user_presence_overrides WHERE sender_id = $1 AND category = $2`,
		senderID, category,
	); err != nil {
		return presenceOverrideOperation("delete_targets", err)
	}
	if len(excludedUserIDs) == 0 {
		return nil
	}
	targets := uuidStrings(excludedUserIDs)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_presence_overrides (sender_id, category, target_user_id)
		SELECT $1, $2, target_id
		FROM unnest($3::uuid[]) AS target_ids(target_id)
	`, senderID, category, pq.Array(targets)); err != nil {
		return presenceOverrideOperation("insert_targets", err)
	}
	return nil
}

func readPresenceOverrideVersion(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
	category string,
	forUpdate bool,
) (int, error) {
	query := `SELECT version FROM presence_override_preferences WHERE user_id = $1 AND category = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var version int
	err := tx.QueryRowContext(ctx, query, senderID, category).Scan(&version)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return version, err
}

func selectExistingPresenceOverrideTargets(
	ctx context.Context,
	tx *sql.Tx,
	targetIDs []uuid.UUID,
) ([]uuid.UUID, error) {
	if len(targetIDs) == 0 {
		return []uuid.UUID{}, nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM users WHERE id = ANY($1::uuid[]) ORDER BY id FOR KEY SHARE`,
		pq.Array(uuidStrings(targetIDs)),
	)
	if err != nil {
		return nil, presenceOverrideOperation("select_targets_query", err)
	}
	existingTargetIDs := make([]uuid.UUID, 0, len(targetIDs))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			closeErr := rows.Close()
			if closeErr != nil {
				return nil, presenceOverrideOperation("select_targets_close", errors.Join(err, closeErr))
			}
			return nil, presenceOverrideOperation("select_targets_scan", err)
		}
		existingTargetIDs = append(existingTargetIDs, id)
	}
	if err := rows.Err(); err != nil {
		closeErr := rows.Close()
		if closeErr != nil {
			return nil, presenceOverrideOperation("select_targets_close", errors.Join(err, closeErr))
		}
		return nil, presenceOverrideOperation("select_targets_rows", err)
	}
	if err := rows.Close(); err != nil {
		return nil, presenceOverrideOperation("select_targets_close", err)
	}
	return existingTargetIDs, nil
}

func upsertPresenceOverridePreference(
	ctx context.Context,
	tx *sql.Tx,
	senderID uuid.UUID,
	category string,
	encryptedData string,
	expectedVersion int,
) (int, error) {
	var version int
	err := tx.QueryRowContext(ctx, `
		INSERT INTO presence_override_preferences (user_id, category, encrypted_data, version)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (user_id, category) DO UPDATE SET
			encrypted_data = EXCLUDED.encrypted_data,
			version = presence_override_preferences.version + 1,
			updated_at = NOW()
		WHERE presence_override_preferences.version = $4
		RETURNING version
	`, senderID, category, encryptedData, expectedVersion).Scan(&version)
	if err == nil {
		return version, nil
	}
	if err != sql.ErrNoRows {
		return 0, presenceOverrideOperation("upsert_preference", err)
	}
	currentVersion, currentErr := readPresenceOverrideVersion(ctx, tx, senderID, category, false)
	if currentErr != nil {
		return 0, presenceOverrideOperation("read_conflict_version", currentErr)
	}
	return 0, &presenceOverrideVersionConflictError{CurrentVersion: currentVersion}
}

func prepareCurrentCustomTextPayload(
	ctx context.Context,
	db presence.DBTX,
	senderID uuid.UUID,
) (*websocket.CustomTextPayload, error) {
	var ps presenceSettingsResponse
	err := db.QueryRowContext(ctx, `
		SELECT master_enabled, custom_text_tier, custom_text, custom_text_emoji
		FROM user_presence_settings
		WHERE user_id = $1
	`, senderID).Scan(&ps.MasterEnabled, &ps.CustomTextTier, &ps.CustomText, &ps.CustomTextEmoji)
	if err == sql.ErrNoRows {
		return customTextPayloadFromRow(presenceSettingsResponse{}), nil
	}
	if err != nil {
		return nil, err
	}
	return customTextPayloadFromRow(ps), nil
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func presenceOverrideOperation(operation string, cause error) error {
	return &presenceOverrideOperationError{Operation: operation, cause: cause}
}
