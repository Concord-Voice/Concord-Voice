package users

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Rich-presence custom-text limits (#1233). Code-point counts mirror the
// `char_length` CHECK constraints in migration 000074 and the zod schema in
// ws-events.ts (defense-in-depth across layers).
const (
	customTextMaxRunes      = 140
	customTextEmojiMaxRunes = 32
	presenceTierMin         = 0
	presenceTierMax         = 2
	maxPresenceSettingsBody = 16 * 1024

	errMsgFailedFetchPresence  = "Failed to fetch presence settings"
	errMsgFailedUpdatePresence = "Failed to update presence settings"
)

var errInvalidPresenceSettingsBody = errors.New("invalid presence settings body")

// presenceSettingsResponse is the wire shape for GET/PATCH presence-settings.
// custom_text / custom_text_emoji are nullable (SQL NULL ⇒ JSON null) — they
// carry user content and are NEVER logged.
type presenceSettingsResponse struct {
	MasterEnabled          bool    `json:"master_enabled"`
	ServerVoiceTier        int     `json:"server_voice_tier"`
	ServerVoiceShowDetails bool    `json:"server_voice_show_details"`
	PrivateCallTier        int     `json:"private_call_tier"`
	PrivateCallShowDetails bool    `json:"private_call_show_details"`
	CustomTextTier         int     `json:"custom_text_tier"`
	CustomText             *string `json:"custom_text"`
	CustomTextEmoji        *string `json:"custom_text_emoji"`
}

func defaultPresenceSettingsResponse() presenceSettingsResponse {
	return presenceSettingsResponse{
		MasterEnabled:          true,
		ServerVoiceTier:        1,
		ServerVoiceShowDetails: true,
	}
}

// GetPresenceSettings returns the caller's own presence settings.
// Returns virtual schema defaults if no row exists yet.
// GET /users/me/presence-settings
func (h *Handler) GetPresenceSettings(c *gin.Context) {
	userID := c.GetString("user_id")

	ps := defaultPresenceSettingsResponse()
	err := h.db.QueryRow(`
		SELECT master_enabled, server_voice_tier, server_voice_show_details,
		       private_call_tier, private_call_show_details,
		       custom_text_tier, custom_text, custom_text_emoji
		FROM user_presence_settings
		WHERE user_id = $1
	`, userID).Scan(
		&ps.MasterEnabled,
		&ps.ServerVoiceTier,
		&ps.ServerVoiceShowDetails,
		&ps.PrivateCallTier,
		&ps.PrivateCallShowDetails,
		&ps.CustomTextTier,
		&ps.CustomText,
		&ps.CustomTextEmoji,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusOK, defaultPresenceSettingsResponse())
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
	MasterEnabled          *bool   `json:"master_enabled"`
	ServerVoiceTier        *int    `json:"server_voice_tier"`
	ServerVoiceShowDetails *bool   `json:"server_voice_show_details"`
	PrivateCallTier        *int    `json:"private_call_tier"`
	PrivateCallShowDetails *bool   `json:"private_call_show_details"`
	CustomTextTier         *int    `json:"custom_text_tier"`
	CustomText             *string `json:"custom_text"`
	CustomTextEmoji        *string `json:"custom_text_emoji"`
}

func readExactPresenceSettingsBody(reader io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, maxPresenceSettingsBody+1))
	if err != nil || len(raw) == 0 || len(raw) > maxPresenceSettingsBody {
		return nil, errInvalidPresenceSettingsBody
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errInvalidPresenceSettingsBody
	}
	fieldCount, err := readPresenceSettingsMembers(decoder)
	if err != nil {
		return nil, err
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || fieldCount == 0 {
		return nil, errInvalidPresenceSettingsBody
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errInvalidPresenceSettingsBody
	}
	return raw, nil
}

func readPresenceSettingsMembers(decoder *json.Decoder) (int, error) {
	seen := make(map[string]struct{}, 8)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return 0, errInvalidPresenceSettingsBody
		}
		key, ok := token.(string)
		if !ok {
			return 0, errInvalidPresenceSettingsBody
		}
		switch key {
		case "master_enabled",
			"server_voice_tier",
			"server_voice_show_details",
			"private_call_tier",
			"private_call_show_details",
			"custom_text_tier",
			"custom_text",
			"custom_text_emoji":
		default:
			return 0, errInvalidPresenceSettingsBody
		}
		if _, duplicate := seen[key]; duplicate {
			return 0, errInvalidPresenceSettingsBody
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return 0, errInvalidPresenceSettingsBody
		}
		seen[key] = struct{}{}
	}
	return len(seen), nil
}

// presenceUpdate holds validated nullable bind values for the static partial
// UPSERT. Invalid values represent omitted fields; valid empty strings retain
// the API's clear-to-NULL semantics through NULLIF in the query.
type presenceUpdate struct {
	masterEnabled          sql.NullBool
	serverVoiceTier        sql.NullInt64
	serverVoiceShowDetails sql.NullBool
	privateCallTier        sql.NullInt64
	privateCallShowDetails sql.NullBool
	customTextTier         sql.NullInt64
	customText             sql.NullString
	customTextEmoji        sql.NullString
	fieldCount             int
}

func validPresenceTier(tier int) bool {
	return tier >= presenceTierMin && tier <= presenceTierMax
}

func presenceTierValue(tier *int, field string) (sql.NullInt64, int, string) {
	if tier == nil {
		return sql.NullInt64{}, 0, ""
	}
	if !validPresenceTier(*tier) {
		return sql.NullInt64{}, 0, field + " must be 0, 1, or 2"
	}
	return sql.NullInt64{Int64: int64(*tier), Valid: true}, 1, ""
}

// buildPresenceUpdate validates the request and constructs its bound values.
// Returns an HTTP status + error message on validation failure, or 0/"" on
// success.
func buildPresenceUpdate(req *updatePresenceRequest) (update presenceUpdate, status int, msg string) {
	if req.MasterEnabled != nil {
		update.masterEnabled = sql.NullBool{Bool: *req.MasterEnabled, Valid: true}
		update.fieldCount++
	}

	var count int
	update.serverVoiceTier, count, msg = presenceTierValue(req.ServerVoiceTier, "server_voice_tier")
	if msg != "" {
		return presenceUpdate{}, http.StatusBadRequest, msg
	}
	update.fieldCount += count

	if req.ServerVoiceShowDetails != nil {
		update.serverVoiceShowDetails = sql.NullBool{Bool: *req.ServerVoiceShowDetails, Valid: true}
		update.fieldCount++
	}

	update.privateCallTier, count, msg = presenceTierValue(req.PrivateCallTier, "private_call_tier")
	if msg != "" {
		return presenceUpdate{}, http.StatusBadRequest, msg
	}
	update.fieldCount += count

	if req.PrivateCallShowDetails != nil {
		update.privateCallShowDetails = sql.NullBool{Bool: *req.PrivateCallShowDetails, Valid: true}
		update.fieldCount++
	}

	update.customTextTier, count, msg = presenceTierValue(req.CustomTextTier, "custom_text_tier")
	if msg != "" {
		return presenceUpdate{}, http.StatusBadRequest, msg
	}
	update.fieldCount += count

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
		master_enabled = COALESCE($2::boolean, master_enabled),
		server_voice_tier = COALESCE($3::smallint, server_voice_tier),
		server_voice_show_details = COALESCE($4::boolean, server_voice_show_details),
		private_call_tier = COALESCE($5::smallint, private_call_tier),
		private_call_show_details = COALESCE($6::boolean, private_call_show_details),
		custom_text_tier = COALESCE($7::smallint, custom_text_tier),
		custom_text = CASE
			WHEN $8::text IS NULL THEN custom_text
			ELSE NULLIF($8::text, '')
		END,
		custom_text_emoji = CASE
			WHEN $9::text IS NULL THEN custom_text_emoji
			ELSE NULLIF($9::text, '')
		END,
		updated_at = clock_timestamp()
	WHERE user_id = $1
	RETURNING master_enabled, server_voice_tier, server_voice_show_details,
	          private_call_tier, private_call_show_details,
	          custom_text_tier, custom_text, custom_text_emoji
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

	raw, err := readExactPresenceSettingsBody(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
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
	mode := presencehistory.OrdinaryAudienceWrite
	if update.masterEnabled.Valid && !update.masterEnabled.Bool {
		mode = presencehistory.ForcedSecurityClear
	}
	var write presenceSettingsWrite
	err = h.presenceHistory.WithReadySenderMode(c.Request.Context(), userUUID, mode, func() error {
		var writeErr error
		write, writeErr = h.writePresenceSettings(c.Request.Context(), userUUID, update, mode)
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
	mode presencehistory.OperationMode,
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
		ctx, tx, senderID, mode,
	)
	if err != nil {
		return result, err
	}
	var response presenceSettingsResponse
	err = tx.QueryRowContext(
		ctx, updatePresenceSettingsQuery, senderID,
		update.masterEnabled,
		update.serverVoiceTier,
		update.serverVoiceShowDetails,
		update.privateCallTier,
		update.privateCallShowDetails,
		update.customTextTier,
		update.customText,
		update.customTextEmoji,
	).Scan(
		&response.MasterEnabled,
		&response.ServerVoiceTier,
		&response.ServerVoiceShowDetails,
		&response.PrivateCallTier,
		&response.PrivateCallShowDetails,
		&response.CustomTextTier,
		&response.CustomText,
		&response.CustomTextEmoji,
	)
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
	if operation.SupersededPending {
		return presencehistory.DeliveryPlan{
			Mode:        presencehistory.DeliveryConservativeReset,
			OperationID: operation.ID,
			SenderID:    operation.SenderID,
		}, nil
	}
	oldAudience := map[uuid.UUID]bool{}
	if operation.BeforeMasterEnabled && operation.BeforeTier > 0 && operation.Before.Text != "" {
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
// A nil result means CLEAR (rich_presence_clear): the master is disabled, the
// user is Off (tier 0), or no visible custom_text exists. A non-nil result is an
// UPDATE carrying the text and optional emoji. This mirrors the audience
// semantics of presence.ComputeCustomTextAudience while ensuring the client also
// drops any previously-shown status on a clear.
func customTextPayloadFromRow(ps presenceSettingsResponse) *websocket.CustomTextPayload {
	if !ps.MasterEnabled || ps.CustomTextTier == 0 || ps.CustomText == nil || *ps.CustomText == "" {
		return nil // clear
	}
	payload := &websocket.CustomTextPayload{Text: *ps.CustomText}
	if ps.CustomTextEmoji != nil {
		payload.Emoji = *ps.CustomTextEmoji
	}
	return payload
}
