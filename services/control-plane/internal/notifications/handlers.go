// Package notifications provides handlers for user notification mute
// preferences (per-server, per-channel, per-DM-conversation).
//
// The server is a passive store: it persists mute state but does NOT filter
// outgoing notify events (e.g. `unread_notify`, `dm_unread_notify`). Clients
// hydrate the full preference map on app start and apply mute logic locally
// when those events arrive. This keeps the server simple, lets clients tune
// resolution policy (channel-pref > server-pref > default) without round-
// trips, and means `muted_until` expiry is a pure client-side timer.
//
// Closes #84.
package notifications

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// Allowed target_type values. Mirrors the CHECK constraint on
// notification_preferences.target_type.
const (
	targetTypeServer  = "server"
	targetTypeChannel = "channel"
	targetTypeDM      = "dm"
)

const (
	errMsgFailedFetchPrefs = "Failed to fetch notification preferences"
	errMsgFailedUpdatePref = "Failed to update notification preference"
	errMsgInvalidUserCtx   = "Invalid user_id in context"
)

// Handler handles notification-preference requests.
type Handler struct {
	db  *sql.DB
	log *logger.Logger
}

// NewHandler creates a new notifications handler.
func NewHandler(db *sql.DB, log *logger.Logger) *Handler {
	return &Handler{db: db, log: log}
}

// preferenceResponse is the JSON shape returned to clients. Empty values are
// omitted so the wire payload stays compact for users with many prefs.
type preferenceResponse struct {
	TargetType string  `json:"target_type"`
	TargetID   string  `json:"target_id"`
	Muted      bool    `json:"muted"`
	MutedUntil *string `json:"muted_until,omitempty"`
	UpdatedAt  string  `json:"updated_at"`
}

// MuteRequest is the body of PUT /notifications/mute. `muted_until` is an
// optional RFC3339 timestamp; nil or absent means the mute is indefinite.
// `muted_until` is ignored when `muted=false` (the row is upserted with the
// new flag and any prior expiry is cleared).
type MuteRequest struct {
	TargetType string  `json:"target_type" binding:"required"`
	TargetID   string  `json:"target_id"   binding:"required"`
	Muted      *bool   `json:"muted"       binding:"required"`
	MutedUntil *string `json:"muted_until,omitempty"`
}

// validateTargetType returns true if t is one of the discriminator values
// the CHECK constraint accepts. Keeps the check on the handler side so we
// produce a 400 rather than letting the DB raise a 500-worthy constraint
// violation.
func validateTargetType(t string) bool {
	return t == targetTypeServer || t == targetTypeChannel || t == targetTypeDM
}

// SetMute upserts a mute preference for a (user, target_type, target_id)
// triple. Setting `muted=false` is meaningful and is NOT a delete — it
// expresses "explicitly unmuted" so a channel can override a muted server.
// PUT /notifications/mute
func (h *Handler) SetMute(c *gin.Context) {
	userIDStr := c.GetString("user_id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidUserCtx})
		return
	}

	var req MuteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if !validateTargetType(req.TargetType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_type must be one of: server, channel, dm"})
		return
	}

	targetID, err := uuid.Parse(req.TargetID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid target_id"})
		return
	}

	// Parse muted_until if provided. We only honor it when muted=true; an
	// expiry on an unmuted row is nonsense and would just cause confusion
	// later when re-reading.
	var mutedUntil sql.NullTime
	if *req.Muted && req.MutedUntil != nil && *req.MutedUntil != "" {
		t, err := time.Parse(time.RFC3339, *req.MutedUntil)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "muted_until must be RFC3339"})
			return
		}
		mutedUntil = sql.NullTime{Time: t, Valid: true}
	}

	// UPSERT keeps the (user, type, id) cardinality and refreshes updated_at
	// so the client can detect "this pref changed on another device" by
	// comparing timestamps after a hydration sync.
	_, err = h.db.Exec(`
		INSERT INTO notification_preferences (user_id, target_type, target_id, muted, muted_until, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
		ON CONFLICT (user_id, target_type, target_id)
		DO UPDATE SET
			muted       = EXCLUDED.muted,
			muted_until = EXCLUDED.muted_until,
			updated_at  = NOW()
	`, userID, req.TargetType, targetID, *req.Muted, mutedUntil)

	if err != nil {
		h.log.Error("Failed to upsert notification preference", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdatePref})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ListPreferences returns every mute preference for the caller. Clients call
// this once at app start to hydrate the local notificationPrefsStore.
// GET /notifications/preferences
func (h *Handler) ListPreferences(c *gin.Context) {
	userIDStr := c.GetString("user_id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidUserCtx})
		return
	}

	prefs, err := h.queryPreferences(`
		SELECT target_type, target_id, muted, muted_until, updated_at
		FROM notification_preferences
		WHERE user_id = $1
		ORDER BY target_type, target_id
	`, userID)
	if err != nil {
		h.log.Error("Failed to list notification preferences", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchPrefs})
		return
	}

	c.JSON(http.StatusOK, gin.H{"preferences": prefs})
}

// GetServerMuteStates returns the caller's mute prefs for one server: the
// server row itself plus any channel rows whose target_id is a channel in
// that server. Useful for re-syncing a single server's view (e.g., after a
// long offline gap) without paying the cost of a full preferences fetch.
// GET /servers/:id/mute-states
func (h *Handler) GetServerMuteStates(c *gin.Context) {
	userIDStr := c.GetString("user_id")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidUserCtx})
		return
	}

	serverIDStr := c.Param("id")
	serverID, err := uuid.Parse(serverIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid server id"})
		return
	}

	// One round-trip via UNION ALL: the server's own row + any channel-pref
	// rows where the channel belongs to this server. Going through `channels`
	// keeps stray channel prefs from other servers (or for deleted channels
	// already orphaned) out of the response.
	prefs, err := h.queryPreferences(`
		SELECT np.target_type, np.target_id, np.muted, np.muted_until, np.updated_at
		FROM notification_preferences np
		WHERE np.user_id = $1
		  AND np.target_type = 'server'
		  AND np.target_id = $2
		UNION ALL
		SELECT np.target_type, np.target_id, np.muted, np.muted_until, np.updated_at
		FROM notification_preferences np
		INNER JOIN channels c ON c.id = np.target_id
		WHERE np.user_id = $1
		  AND np.target_type = 'channel'
		  AND c.server_id = $2
		ORDER BY target_type, target_id
	`, userID, serverID)
	if err != nil {
		h.log.Error("Failed to fetch server mute states", "error", err, "server_id", serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchPrefs})
		return
	}

	c.JSON(http.StatusOK, gin.H{"preferences": prefs})
}

// queryPreferences runs a SELECT returning preference rows in the canonical
// column order (target_type, target_id, muted, muted_until, updated_at) and
// scans them into the wire format. Centralised so the row-scan loop, error
// handling, and timestamp formatting live in one place.
func (h *Handler) queryPreferences(query string, args ...interface{}) ([]preferenceResponse, error) {
	rows, err := h.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	prefs := []preferenceResponse{}
	for rows.Next() {
		var (
			targetType string
			targetID   uuid.UUID
			muted      bool
			mutedUntil sql.NullTime
			updatedAt  time.Time
		)
		if err := rows.Scan(&targetType, &targetID, &muted, &mutedUntil, &updatedAt); err != nil {
			h.log.Error("Failed to scan notification preference", "error", err)
			continue
		}

		p := preferenceResponse{
			TargetType: targetType,
			TargetID:   targetID.String(),
			Muted:      muted,
			UpdatedAt:  updatedAt.UTC().Format(time.RFC3339Nano),
		}
		if mutedUntil.Valid {
			s := mutedUntil.Time.UTC().Format(time.RFC3339Nano)
			p.MutedUntil = &s
		}
		prefs = append(prefs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return prefs, nil
}

// ErrInvalidTargetType is exported for tests that want to assert on the
// specific failure mode without depending on the response body text.
var ErrInvalidTargetType = errors.New("invalid target_type")
