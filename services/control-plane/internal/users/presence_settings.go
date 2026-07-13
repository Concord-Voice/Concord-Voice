package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/presence"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/presencehistory"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
)

// Rich-presence custom-text limits (#1233). Code-point counts mirror the
// `char_length` CHECK constraints in migration 000074 and the zod schema in
// ws-events.ts (defense-in-depth across layers).
const (
	customTextMaxRunes      = 140
	customTextEmojiMaxRunes = 32
	customTextTierMin       = 0
	customTextTierMax       = 2

	errMsgFailedFetchPresence  = "Failed to fetch presence settings"
	errMsgFailedUpdatePresence = "Failed to update presence settings"
)

// presenceSettingsResponse is the wire shape for GET/PATCH presence-settings.
// custom_text / custom_text_emoji are nullable (SQL NULL ⇒ JSON null) — they
// carry user content and are NEVER logged.
type presenceSettingsResponse struct {
	CustomTextTier  int     `json:"custom_text_tier"`
	CustomText      *string `json:"custom_text"`
	CustomTextEmoji *string `json:"custom_text_emoji"`
}

// GetPresenceSettings returns the caller's own presence settings.
// Returns defaults ({0, null, null}) if no row exists yet.
// GET /users/me/presence-settings
func (h *Handler) GetPresenceSettings(c *gin.Context) {
	userID := c.GetString("user_id")

	var ps presenceSettingsResponse
	err := h.db.QueryRow(`
		SELECT custom_text_tier, custom_text, custom_text_emoji
		FROM user_presence_settings
		WHERE user_id = $1
	`, userID).Scan(&ps.CustomTextTier, &ps.CustomText, &ps.CustomTextEmoji)
	if err == sql.ErrNoRows {
		// No row yet — return schema defaults (tier Off, no text/emoji).
		c.JSON(http.StatusOK, presenceSettingsResponse{
			CustomTextTier:  0,
			CustomText:      nil,
			CustomTextEmoji: nil,
		})
		return
	}
	if err != nil {
		// Metadata only — never log custom_text / custom_text_emoji (PII).
		h.log.Error(errMsgFailedFetchPresence, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchPresence})
		return
	}

	c.JSON(http.StatusOK, ps)
}

// updatePresenceRequest is a partial update to presence settings. Pointer fields
// distinguish "not supplied" from "supplied as empty/zero".
type updatePresenceRequest struct {
	CustomTextTier  *int    `json:"custom_text_tier"`
	CustomText      *string `json:"custom_text"`
	CustomTextEmoji *string `json:"custom_text_emoji"`
}

// presenceUpdate holds validated nullable bind values for the static partial
// UPSERT. Invalid values represent omitted fields; valid empty strings retain
// the API's clear-to-NULL semantics through NULLIF in the query.
type presenceUpdate struct {
	customTextTier  sql.NullInt64
	customText      sql.NullString
	customTextEmoji sql.NullString
	fieldCount      int
}

// buildPresenceUpdate validates the request and constructs its bound values.
// Returns an HTTP status + error message on validation failure, or 0/"" on
// success.
func buildPresenceUpdate(req *updatePresenceRequest) (update presenceUpdate, status int, msg string) {

	if req.CustomTextTier != nil {
		tier := *req.CustomTextTier
		if tier < customTextTierMin || tier > customTextTierMax {
			return presenceUpdate{}, http.StatusBadRequest, "custom_text_tier must be 0, 1, or 2"
		}
		update.customTextTier = sql.NullInt64{Int64: int64(tier), Valid: true}
		update.fieldCount++
	}

	if req.CustomText != nil {
		text := *req.CustomText
		if utf8.RuneCountInString(text) > customTextMaxRunes {
			return presenceUpdate{}, http.StatusBadRequest, "custom_text must be at most 140 characters"
		}
		update.customText = sql.NullString{String: text, Valid: true}
		update.fieldCount++
	}

	if req.CustomTextEmoji != nil {
		emoji := *req.CustomTextEmoji
		if utf8.RuneCountInString(emoji) > customTextEmojiMaxRunes {
			return presenceUpdate{}, http.StatusBadRequest, "custom_text_emoji must be at most 32 characters"
		}
		update.customTextEmoji = sql.NullString{String: emoji, Valid: true}
		update.fieldCount++
	}

	return update, 0, ""
}

const updatePresenceSettingsQuery = `
	UPDATE user_presence_settings SET
		custom_text_tier = COALESCE($2::smallint, custom_text_tier),
		custom_text = CASE
			WHEN $3::text IS NULL THEN custom_text
			ELSE NULLIF($3::text, '')
		END,
		custom_text_emoji = CASE
			WHEN $4::text IS NULL THEN custom_text_emoji
			ELSE NULLIF($4::text, '')
		END,
		updated_at = clock_timestamp()
	WHERE user_id = $1
	RETURNING custom_text_tier, custom_text, custom_text_emoji
`

type presenceWriterFailure struct {
	status int
	class  string
	cause  error
}

func (e *presenceWriterFailure) Error() string { return "presence writer " + e.class + " failed" }
func (e *presenceWriterFailure) Unwrap() error { return e.cause }

type presenceSettingsWrite struct {
	response  presenceSettingsResponse
	operation presencehistory.AudienceOperation
	plan      presencehistory.DeliveryPlan
}

// UpdatePresenceSettings updates the caller's presence settings.
// Accepts a partial JSON body — only provided fields are written. UPSERTs the
// row, application-sets updated_at (no DB trigger), and returns the resulting row.
// PATCH /users/me/presence-settings
func (h *Handler) UpdatePresenceSettings(c *gin.Context) {
	userID := c.GetString("user_id")

	var req updatePresenceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	update, status, msg := buildPresenceUpdate(&req)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}
	if update.fieldCount == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}
	userUUID, err := uuid.Parse(userID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}
	if h.presenceHistory == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgFailedUpdatePresence})
		return
	}
	var write presenceSettingsWrite
	err = h.presenceHistory.WithReadySender(c.Request.Context(), userUUID, func() error {
		var writeErr error
		write, writeErr = h.writePresenceSettings(c.Request.Context(), userUUID, update)
		return writeErr
	})
	if err != nil {
		h.respondPresenceWriterFailure(c, errMsgFailedUpdatePresence, err)
		return
	}
	c.JSON(http.StatusOK, write.response)
}

func (h *Handler) writePresenceSettings(
	ctx context.Context,
	senderID uuid.UUID,
	update presenceUpdate,
) (result presenceSettingsWrite, returnErr error) {
	tx, err := h.presenceHistory.BeginTx(ctx, nil)
	if err != nil {
		return result, &presenceWriterFailure{status: 500, class: "begin", cause: err}
	}
	if tx == nil {
		return result, &presenceWriterFailure{status: 500, class: "begin", cause: errors.New("presence settings transaction missing")}
	}
	defer tx.Rollback() //nolint:errcheck
	defer h.joinPresenceWriterRollback(tx, &returnErr)

	operation, err := h.presenceHistory.BeginAudienceOperation(
		ctx, tx, senderID, presencehistory.OrdinaryAudienceWrite,
	)
	if err != nil {
		return result, err
	}
	var response presenceSettingsResponse
	err = tx.QueryRowContext(
		ctx, updatePresenceSettingsQuery, senderID,
		update.customTextTier, update.customText, update.customTextEmoji,
	).Scan(&response.CustomTextTier, &response.CustomText, &response.CustomTextEmoji)
	if err != nil {
		return result, &presenceWriterFailure{status: 500, class: "update", cause: err}
	}
	after := presencehistory.CustomTextState{}
	if response.CustomText != nil {
		after.Text = *response.CustomText
	}
	if response.CustomTextEmoji != nil {
		after.Emoji = *response.CustomTextEmoji
	}
	if err := h.presenceHistory.RecordCustomTextTransition(
		ctx, tx, senderID, operation.Before, after,
	); err != nil {
		return result, &presenceWriterFailure{status: 500, class: "history", cause: err}
	}
	plan, err := preparePresenceSettingsPlan(ctx, tx, operation, response)
	if err != nil {
		return result, &presenceWriterFailure{status: 500, class: "prepare", cause: err}
	}
	result = presenceSettingsWrite{response: response, operation: operation, plan: plan}
	if err := h.commitAndClaimPresenceWriter(ctx, tx, operation, plan); err != nil {
		return result, err
	}
	return result, nil
}

func preparePresenceSettingsPlan(
	ctx context.Context,
	tx *sql.Tx,
	operation presencehistory.AudienceOperation,
	after presenceSettingsResponse,
) (presencehistory.DeliveryPlan, error) {
	oldAudience := map[uuid.UUID]bool{}
	if operation.BeforeTier > 0 && operation.Before.Text != "" {
		var err error
		oldAudience, err = presence.ComputeCustomTextAudienceForTier(
			ctx, tx, operation.SenderID, operation.BeforeTier,
		)
		if err != nil {
			return presencehistory.DeliveryPlan{}, err
		}
		oldAudience[operation.SenderID] = true
	}
	newAudience := map[uuid.UUID]bool{}
	payload := customTextStateFromRow(after)
	if payload != nil {
		var err error
		newAudience, err = presence.ComputeCustomTextAudience(ctx, tx, operation.SenderID)
		if err != nil {
			return presencehistory.DeliveryPlan{}, err
		}
		newAudience[operation.SenderID] = true
	}
	return presencehistory.DeliveryPlan{
		Mode:             presencehistory.DeliveryExactDelta,
		OperationID:      operation.ID,
		SenderID:         operation.SenderID,
		ClearRecipients:  audienceDifference(oldAudience, newAudience),
		UpdateRecipients: newAudience,
		Payload:          payload,
	}, nil
}

func customTextStateFromRow(ps presenceSettingsResponse) *presencehistory.CustomTextState {
	payload := customTextPayloadFromRow(ps)
	if payload == nil {
		return nil
	}
	return &presencehistory.CustomTextState{Text: payload.Text, Emoji: payload.Emoji}
}

func audienceDifference(left, right map[uuid.UUID]bool) map[uuid.UUID]bool {
	difference := make(map[uuid.UUID]bool)
	for id, included := range left {
		if included && !right[id] {
			difference[id] = true
		}
	}
	return difference
}

func (h *Handler) commitAndClaimPresenceWriter(
	ctx context.Context,
	tx *sql.Tx,
	operation presencehistory.AudienceOperation,
	plan presencehistory.DeliveryPlan,
) error {
	commitErr := h.presenceHistory.CommitTx(tx)
	var confirmedCommitErr error
	if commitErr != nil {
		rollbackErr := h.presenceHistory.RollbackTx(tx)
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			commitErr = errors.Join(commitErr, fmt.Errorf("rollback failed main presence write: %w", rollbackErr))
		}
		switch h.presenceHistory.ClassifyAudienceCommit(ctx, operation) {
		case presencehistory.CommitConfirmed:
			confirmedCommitErr = commitErr
		case presencehistory.RollbackConfirmed:
			return &presenceWriterFailure{status: 500, class: "commit_rollback", cause: commitErr}
		case presencehistory.WriteSuperseded:
			return &presenceWriterFailure{status: 500, class: "commit_superseded", cause: commitErr}
		default:
			resetErr := h.presenceHistory.EmergencyReset(context.WithoutCancel(ctx), plan)
			return &presenceWriterFailure{
				status: 500,
				class:  "commit_unresolved",
				cause:  errors.Join(commitErr, resetErr),
			}
		}
	}
	completion := h.presenceHistory.CompleteClaim(ctx, plan)
	if completion.Err != nil {
		return &presenceWriterFailure{
			status: 503,
			class:  "delivery",
			cause:  errors.Join(confirmedCommitErr, completion.Err),
		}
	}
	return nil
}

func (h *Handler) joinPresenceWriterRollback(tx *sql.Tx, returnErr *error) {
	rollbackErr := h.presenceHistory.RollbackTx(tx)
	if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
		return
	}
	*returnErr = errors.Join(*returnErr, fmt.Errorf("presence writer rollback: %w", rollbackErr))
}

func (h *Handler) respondPresenceWriterFailure(c *gin.Context, message string, err error) {
	status := http.StatusInternalServerError
	errorClass := "unknown"
	var serviceErr *presencehistory.ServiceError
	if errors.As(err, &serviceErr) {
		status = serviceErr.Status
		errorClass = serviceErr.Code
		if serviceErr.RetryAfter > 0 {
			seconds := int64(serviceErr.RetryAfter.Round(time.Second) / time.Second)
			c.Header("Retry-After", fmt.Sprintf("%d", seconds))
		}
	}
	var writerErr *presenceWriterFailure
	if errors.As(err, &writerErr) {
		status = writerErr.status
		errorClass = writerErr.class
	}
	var overrideErr *presenceOverrideOperationError
	if errors.As(err, &overrideErr) {
		errorClass = overrideErr.Operation
	}
	h.log.Error(message, "error_class", errorClass)
	c.JSON(status, gin.H{"error": message})
}

// customTextPayloadFromRow derives the fan-out payload from the persisted row.
// A nil result means CLEAR (rich_presence_clear): the user is Off (tier 0) or has
// no visible custom_text. A non-nil result is an UPDATE carrying the text and the
// optional emoji. This mirrors the audience semantics of
// presence.ComputeCustomTextAudience (tier 0 ⇒ empty audience) while ensuring the
// client also drops any previously-shown status on a clear.
func customTextPayloadFromRow(ps presenceSettingsResponse) *websocket.CustomTextPayload {
	if ps.CustomTextTier == 0 || ps.CustomText == nil || *ps.CustomText == "" {
		return nil // clear
	}
	payload := &websocket.CustomTextPayload{Text: *ps.CustomText}
	if ps.CustomTextEmoji != nil {
		payload.Emoji = *ps.CustomTextEmoji
	}
	return payload
}
