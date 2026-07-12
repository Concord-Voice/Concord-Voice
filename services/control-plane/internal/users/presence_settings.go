package users

import (
	"database/sql"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	INSERT INTO user_presence_settings (
		user_id, custom_text_tier, custom_text, custom_text_emoji
	) VALUES (
		$1, COALESCE($2::smallint, 0), NULLIF($3::text, ''), NULLIF($4::text, '')
	)
	ON CONFLICT (user_id) DO UPDATE SET
		custom_text_tier = COALESCE($2::smallint, user_presence_settings.custom_text_tier),
		custom_text = CASE
			WHEN $3::text IS NULL THEN user_presence_settings.custom_text
			ELSE NULLIF($3::text, '')
		END,
		custom_text_emoji = CASE
			WHEN $4::text IS NULL THEN user_presence_settings.custom_text_emoji
			ELSE NULLIF($4::text, '')
		END,
		updated_at = NOW()
	RETURNING custom_text_tier, custom_text, custom_text_emoji
`

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
	h.withCustomStatusSender(userUUID, func() {
		// Read the PRIOR tier before the UPSERT so the fan-out can clear viewers who
		// lose visibility when the tier narrows or custom text is turned off
		// (#1233/Gitar; risk: privacy). No row ⇒ oldTier stays 0 (no prior audience).
		// On a GENUINE read error we can't know the old tier, so fail SAFE to the
		// widest tier (2): over-clearing the broadest prior audience never leaks,
		// whereas defaulting to 0 would skip the clear and leave a stale status on a
		// viewer who just lost permission (Gitar review on #1685).
		var oldTier int
		if tierErr := h.db.QueryRow(
			`SELECT custom_text_tier FROM user_presence_settings WHERE user_id = $1`, userID,
		).Scan(&oldTier); tierErr != nil && tierErr != sql.ErrNoRows {
			h.log.Error("presence: read prior tier failed", "error", tierErr)
			oldTier = 2 // fail-safe: clear the widest possible prior audience
		}

		var ps presenceSettingsResponse
		err = h.db.QueryRow(
			updatePresenceSettingsQuery,
			userID,
			update.customTextTier,
			update.customText,
			update.customTextEmoji,
		).Scan(&ps.CustomTextTier, &ps.CustomText, &ps.CustomTextEmoji)
		if err != nil {
			// Metadata only — never log custom_text / custom_text_emoji (PII).
			h.log.Error(errMsgFailedUpdatePresence, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdatePresence})
			return
		}

		// Respond BEFORE any audience fan-out.
		c.JSON(http.StatusOK, ps)

		// Keep persistence and fan-out ordered with override writes for this sender.
		if h.presenceOverrideBroadcaster == nil {
			return
		}
		h.presenceOverrideBroadcaster.BroadcastCustomText(userUUID, oldTier, customTextPayloadFromRow(ps))
	})
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
