package users

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
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

// presenceUpsert holds the parameterized fragments for a partial UPSERT: the
// INSERT column list (with $N placeholders) so a first write persists the
// provided values, the matching ON CONFLICT DO UPDATE SET clauses (via
// EXCLUDED), and the ordered argument values. $1 is always user_id.
type presenceUpsert struct {
	insertCols []string      // e.g. "custom_text_tier"
	insertVals []string      // e.g. "$2"
	setClauses []string      // e.g. "custom_text_tier = EXCLUDED.custom_text_tier"
	args       []interface{} // ordered $2..$N values (user_id is prepended by the caller)
}

// buildPresenceClauses validates the request and constructs the UPSERT fragments
// for each provided field. Returns an HTTP status + error message on validation
// failure, or 0/"" on success.
//
// All user-supplied values are parameterized ($N) — never interpolated. An
// empty custom_text / custom_text_emoji string CLEARS the column (stored NULL).
func buildPresenceClauses(req *updatePresenceRequest) (up presenceUpsert, status int, msg string) {
	argIdx := 2 // $1 is reserved for userID

	addField := func(column string, val interface{}) {
		up.insertCols = append(up.insertCols, column)
		up.insertVals = append(up.insertVals, fmt.Sprintf("$%d", argIdx))
		up.setClauses = append(up.setClauses, fmt.Sprintf("%s = EXCLUDED.%s", column, column))
		up.args = append(up.args, val)
		argIdx++
	}

	if req.CustomTextTier != nil {
		tier := *req.CustomTextTier
		if tier < customTextTierMin || tier > customTextTierMax {
			return presenceUpsert{}, http.StatusBadRequest, "custom_text_tier must be 0, 1, or 2"
		}
		addField("custom_text_tier", tier)
	}

	if req.CustomText != nil {
		text := *req.CustomText
		if utf8.RuneCountInString(text) > customTextMaxRunes {
			return presenceUpsert{}, http.StatusBadRequest, "custom_text must be at most 140 characters"
		}
		addField("custom_text", nullIfEmpty(text)) // empty ⇒ clear (NULL)
	}

	if req.CustomTextEmoji != nil {
		emoji := *req.CustomTextEmoji
		if utf8.RuneCountInString(emoji) > customTextEmojiMaxRunes {
			return presenceUpsert{}, http.StatusBadRequest, "custom_text_emoji must be at most 32 characters"
		}
		addField("custom_text_emoji", nullIfEmpty(emoji)) // empty ⇒ clear (NULL)
	}

	return up, 0, ""
}

// nullIfEmpty maps an empty string to a SQL NULL (clear semantics) and any
// non-empty string to itself. Returned as interface{} so the driver binds NULL.
func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
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

	up, status, msg := buildPresenceClauses(&req)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}
	if len(up.setClauses) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}

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

	// updated_at is application-set on every write (no updated_at trigger exists).
	up.setClauses = append(up.setClauses, "updated_at = NOW()")
	args := append([]interface{}{userID}, up.args...)

	// UPSERT: INSERT the provided columns so a FIRST write persists them; on
	// conflict, update the same columns via EXCLUDED. Column names are fixed
	// literals and all VALUES are $N placeholders — no user value is interpolated.
	insertCols := append([]string{"user_id"}, up.insertCols...)
	insertVals := append([]string{"$1"}, up.insertVals...)
	query := fmt.Sprintf(`
		INSERT INTO user_presence_settings (%s) VALUES (%s)
		ON CONFLICT (user_id) DO UPDATE SET %s
		RETURNING custom_text_tier, custom_text, custom_text_emoji
	`, strings.Join(insertCols, ", "), strings.Join(insertVals, ", "), strings.Join(up.setClauses, ", ")) // #nosec G201 -- Safe: column names are fixed literals; all VALUES are $N placeholders; no user value interpolated // nosemgrep:concord-go-sql-sprintf

	var ps presenceSettingsResponse
	err := h.db.QueryRow(query, args...).Scan(&ps.CustomTextTier, &ps.CustomText, &ps.CustomTextEmoji)
	if err != nil {
		// Metadata only — never log custom_text / custom_text_emoji (PII).
		h.log.Error(errMsgFailedUpdatePresence, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdatePresence})
		return
	}

	// Respond BEFORE any audience fan-out.
	c.JSON(http.StatusOK, ps)

	// #1233 Task B3: dispatch custom-text fan-out on a goroutine so the HTTP
	// response is never blocked on the audience computation + per-viewer send.
	// The hub is nil in unit tests that exercise the handler without a hub.
	if h.hub == nil {
		return
	}
	userUUID, err := uuid.Parse(userID)
	if err != nil {
		// userID comes from the auth middleware (a validated UUID), so this is
		// defensive only. Metadata-only log; never the custom text value.
		h.log.Error("presence fan-out: invalid user_id", "error", err)
		return
	}
	go h.hub.BroadcastCustomText(userUUID, oldTier, customTextPayloadFromRow(ps))
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
