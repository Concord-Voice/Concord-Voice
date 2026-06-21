// Package sessions provides handlers for managing user authentication sessions.
package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
)

const (
	errMsgUnauthorized          = "Unauthorized"
	errMsgFailedRevokeSession   = "Failed to revoke session"
	errMsgMFAVerificationFailed = "MFA verification failed"
	errMsgInvalidMFACode        = "Invalid MFA code"
	errMsgFailedVerifyPassword  = "Failed to verify password"
	errMsgIncorrectPassword     = "Incorrect password"
	errMsgFailedFetchSessions   = "Failed to fetch sessions"
)

// SessionDisconnector allows forcefully disconnecting WebSocket connections.
// Defined as an interface to avoid coupling to the websocket package.
type SessionDisconnector interface {
	DisconnectUser(userID uuid.UUID)
	DisconnectSession(sessionID string)
}

// MFAVerifier checks MFA status and verifies codes for sensitive operations.
type MFAVerifier interface {
	IsEnabled(ctx context.Context, userID string) bool
	VerifyCode(ctx context.Context, userID string, code string) (bool, error)
	GetEnabledMethods(ctx context.Context, userID string) ([]string, error)
}

// Revocation rate-limiting constants.
const (
	revokeWindowDuration       = 24 * time.Hour
	revokeTrackingTTL          = 25 * time.Hour
	simpleAuthWindowTTL        = 5 * time.Minute
	freeRevokeSessionThreshold = 3 // Must have >= 3 active sessions for a free revoke
)

// maskIPAddress masks the last octet of an IPv4 address (e.g., "192.168.1.x")
// or the last 80 bits of an IPv6 address (keeping the /48 prefix).
// This minimizes PII exposure in API responses while still being useful for session identification.
func maskIPAddress(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "unknown"
	}

	if parsed.To4() != nil {
		// IPv4: mask last octet
		parts := strings.Split(ip, ".")
		if len(parts) == 4 {
			parts[3] = "x"
			return strings.Join(parts, ".")
		}
	}

	// IPv6: keep first 3 groups (/48), mask rest
	parts := strings.Split(ip, ":")
	if len(parts) > 3 {
		return strings.Join(parts[:3], ":") + "::x"
	}

	return "unknown"
}

// Handler handles session-related requests for managing user authentication sessions.
type Handler struct {
	db          *sql.DB
	redis       *redis.Client
	log         *logger.Logger
	hub         SessionDisconnector
	mfaVerifier MFAVerifier
}

// NewHandler creates a new session handler.
func NewHandler(db *sql.DB, redis *redis.Client, log *logger.Logger, hub SessionDisconnector, mfaVerifier MFAVerifier) *Handler {
	return &Handler{
		db:          db,
		redis:       redis,
		log:         log,
		hub:         hub,
		mfaVerifier: mfaVerifier,
	}
}

// ── Revocation Policy Helpers ────────────────────────────────────────────────

func revokeTrackingKey(userID string) string {
	return fmt.Sprintf("session_revokes:%s", userID)
}

func simpleAuthWindowKey(userID string) string {
	return fmt.Sprintf("session_revoke_auth:%s", userID)
}

// countRevokesInWindow prunes expired entries from the tracking set and returns
// the number of revokes recorded within the current 24-hour rolling window.
func (h *Handler) countRevokesInWindow(ctx context.Context, userID string) (int64, error) {
	key := revokeTrackingKey(userID)
	cutoff := fmt.Sprintf("%f", float64(time.Now().Add(-revokeWindowDuration).Unix()))
	h.redis.ZRemRangeByScore(ctx, key, "-inf", cutoff)
	return h.redis.ZCard(ctx, key).Result()
}

// countActiveSessions returns the number of non-revoked, non-expired sessions for a user.
func (h *Handler) countActiveSessions(ctx context.Context, userID string) (int, error) {
	var count int
	err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > NOW()`,
		userID,
	).Scan(&count)
	return count, err
}

// recordRevoke adds a revoke entry to the tracking set. Called for ALL revokes
// (both free and authenticated) so the rolling window slides forward.
func (h *Handler) recordRevoke(ctx context.Context, userID, sessionID string) {
	key := revokeTrackingKey(userID)
	h.redis.ZAdd(ctx, key, redis.Z{
		Score:  float64(time.Now().Unix()),
		Member: sessionID,
	})
	h.redis.Expire(ctx, key, revokeTrackingTTL)
}

// hasSimpleAuthWindow checks if the user has an active 5-minute authentication window (Simple mode).
func (h *Handler) hasSimpleAuthWindow(ctx context.Context, userID string) bool {
	val, err := h.redis.Get(ctx, simpleAuthWindowKey(userID)).Result()
	return err == nil && val == "1"
}

// grantSimpleAuthWindow sets a 5-minute authentication window in Redis.
func (h *Handler) grantSimpleAuthWindow(ctx context.Context, userID string) {
	h.redis.Set(ctx, simpleAuthWindowKey(userID), "1", simpleAuthWindowTTL)
}

// resetRevokeTracking clears both the revoke tracking set and the auth window.
func (h *Handler) resetRevokeTracking(ctx context.Context, userID string) {
	h.redis.Del(ctx, revokeTrackingKey(userID))
	h.redis.Del(ctx, simpleAuthWindowKey(userID))
}

// getUserRevocationMode fetches the user's revocation_mode from the database.
// Returns "secure" as the default if the query fails.
func (h *Handler) getUserRevocationMode(ctx context.Context, userID string) string {
	var mode string
	err := h.db.QueryRowContext(ctx, `SELECT revocation_mode FROM users WHERE id = $1`, userID).Scan(&mode)
	if err != nil {
		return "secure"
	}
	return mode
}

// passwordRequired determines if authentication is needed for this revoke attempt.
// With fewer than 3 active sessions, password is always required.
// With 3+ active sessions, one free revoke per 24h rolling window is allowed.
func passwordRequired(activeCount int, revokesInWindow int64) bool {
	if activeCount < freeRevokeSessionThreshold {
		return true
	}
	return revokesInWindow >= 1
}

// verifyUserPassword fetches the stored Argon2id hash and verifies the provided password.
func (h *Handler) verifyUserPassword(ctx context.Context, userID, password string) (bool, error) {
	var passwordHash string
	if err := h.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&passwordHash); err != nil {
		return false, fmt.Errorf("failed to fetch password hash: %w", err)
	}
	match, err := auth.VerifyPassword(password, passwordHash)
	if err != nil {
		return false, fmt.Errorf("password verification error: %w", err)
	}
	return match, nil
}

// ── Handlers ─────────────────────────────────────────────────────────────────

// ListSessions returns all active sessions for the current user
func (h *Handler) ListSessions(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var currentTokenHash string
	currentToken, cookieErr := c.Cookie("refresh_token")
	if cookieErr == nil && currentToken != "" {
		currentTokenHash = auth.HashRefreshToken(currentToken)
	}

	activeSessions, err := h.fetchActiveSessions(userID.(string), currentTokenHash)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedFetchSessions})
		return
	}

	pastSessions := h.fetchPastSessions(userID.(string))
	mode := h.getUserRevocationMode(c.Request.Context(), userID.(string))

	c.JSON(http.StatusOK, gin.H{
		"sessions":        activeSessions,
		"past_sessions":   pastSessions,
		"total":           len(activeSessions),
		"revocation_mode": mode,
	})
}

// fetchActiveSessions queries active (non-revoked, non-expired) sessions for a user.
func (h *Handler) fetchActiveSessions(uid, currentTokenHash string) ([]gin.H, error) {
	rows, err := h.db.Query(
		`SELECT id, token_hash, device_name, ip_address, user_agent, expires_at, created_at, last_used_at, remember_me, COALESCE(machine_id, '')
		 FROM refresh_tokens
		 WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > NOW()
		 ORDER BY last_used_at DESC`,
		uid,
	)
	if err != nil {
		h.log.Error(errMsgFailedFetchSessions, "error", err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	activeSessions := []gin.H{}
	for rows.Next() {
		var s models.RefreshToken
		if err := rows.Scan(&s.ID, &s.TokenHash, &s.DeviceName, &s.IPAddress, &s.UserAgent,
			&s.ExpiresAt, &s.CreatedAt, &s.LastUsedAt, &s.RememberMe, &s.MachineID); err != nil {
			h.log.Error("Failed to scan session", "error", err)
			continue
		}
		activeSessions = append(activeSessions, gin.H{
			"id": s.ID, "device_name": s.DeviceName, "ip_address": maskIPAddress(s.IPAddress),
			"user_agent": s.UserAgent, "machine_id": s.MachineID, "expires_at": s.ExpiresAt,
			"created_at": s.CreatedAt, "last_used": s.LastUsedAt, "remember_me": s.RememberMe,
			"is_current": currentTokenHash != "" && s.TokenHash == currentTokenHash,
		})
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating sessions", "error", err)
		return nil, err
	}
	return activeSessions, nil
}

// fetchPastSessions queries recently-revoked sessions (last 30 days) for a user.
func (h *Handler) fetchPastSessions(uid string) []gin.H {
	pastRows, err := h.db.Query(
		`SELECT id, device_name, ip_address, user_agent, created_at, last_used_at, revoked_at
		 FROM refresh_tokens
		 WHERE user_id = $1 AND revoked_at IS NOT NULL AND revoked_at > NOW() - INTERVAL '30 days'
		 ORDER BY revoked_at DESC
		 LIMIT 10`,
		uid,
	)
	if err != nil {
		h.log.Error("Failed to fetch past sessions", "error", err)
		return []gin.H{}
	}
	defer func() { _ = pastRows.Close() }()

	pastSessions := []gin.H{}
	for pastRows.Next() {
		var s models.RefreshToken
		if err := pastRows.Scan(&s.ID, &s.DeviceName, &s.IPAddress, &s.UserAgent,
			&s.CreatedAt, &s.LastUsedAt, &s.RevokedAt); err != nil {
			h.log.Error("Failed to scan past session", "error", err)
			continue
		}
		pastSessions = append(pastSessions, gin.H{
			"id": s.ID, "device_name": s.DeviceName, "ip_address": maskIPAddress(s.IPAddress),
			"user_agent": s.UserAgent, "created_at": s.CreatedAt, "last_used": s.LastUsedAt,
			"revoked_at": s.RevokedAt,
		})
	}
	return pastSessions
}

// RevokeSession revokes a specific session by ID with server-authoritative
// rate-limiting policy enforcement. The server independently determines whether
// password authentication is required — no client claims are trusted.
func (h *Handler) RevokeSession(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	sessionID := c.Param("id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Session ID is required"})
		return
	}

	uid := userID.(string)
	ctx := c.Request.Context()

	if !h.verifySessionOwnership(ctx, c, sessionID, uid) {
		return
	}

	needsPassword, mode := h.determineAuthRequired(ctx, uid)

	// Parse optional password from request body
	var req struct {
		Password string `json:"password"` // #nosec G117 -- not a secret field, receives user input for auth verification
		MFACode  string `json:"mfa_code"` // Framework: accepted but unused until MFA is implemented
	}
	if c.ContentType() == "application/json" {
		_ = c.ShouldBindJSON(&req)
	}

	if needsPassword {
		if h.authenticateForRevoke(ctx, c, uid, req.Password, req.MFACode, "revoke this session") {
			return
		}
		if mode == "simple" {
			h.grantSimpleAuthWindow(ctx, uid)
		}
	}

	h.executeRevocation(ctx, c, uid, sessionID, needsPassword)
}

// verifySessionOwnership checks that the session exists, belongs to the user, and is not revoked.
// Returns true if valid, false if a response was already written.
func (h *Handler) verifySessionOwnership(ctx context.Context, c *gin.Context, sessionID, uid string) bool {
	var sessionExists bool
	err := h.db.QueryRowContext(ctx, //nolint:gosec // parameterized query — not injectable
		`SELECT EXISTS(SELECT 1 FROM refresh_tokens WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL)`,
		sessionID, uid,
	).Scan(&sessionExists)
	if err != nil {
		h.log.Error("Failed to verify session ownership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRevokeSession})
		return false
	}
	if !sessionExists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Session not found"})
		return false
	}
	return true
}

// determineAuthRequired uses server-authoritative policy checks to decide if password is needed.
func (h *Handler) determineAuthRequired(ctx context.Context, uid string) (needsPassword bool, mode string) {
	activeCount, err := h.countActiveSessions(ctx, uid)
	if err != nil {
		h.log.Error("Failed to count active sessions", "error", err)
		return true, "secure" // Fail secure
	}

	revokesInWindow, err := h.countRevokesInWindow(ctx, uid)
	if err != nil {
		h.log.Error("Redis error checking revoke window, requiring password", "error", err)
		revokesInWindow = 999
	}

	mode = h.getUserRevocationMode(ctx, uid)

	if mode == "simple" && h.hasSimpleAuthWindow(ctx, uid) {
		return false, mode
	}
	return passwordRequired(activeCount, revokesInWindow), mode
}

// authenticateForRevoke verifies the user's identity via MFA or password for session revocation.
// Returns true if the request was blocked (response written), false if authentication passed.
func (h *Handler) authenticateForRevoke(ctx context.Context, c *gin.Context, uid, password, mfaCode, actionDesc string) bool {
	hasMFA := h.mfaVerifier != nil && h.mfaVerifier.IsEnabled(ctx, uid)

	if hasMFA && mfaCode != "" {
		valid, mfaErr := h.mfaVerifier.VerifyCode(ctx, uid, mfaCode)
		if mfaErr != nil {
			h.log.Error("MFA verification error during session revoke", "error", mfaErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAVerificationFailed})
			return true
		}
		if !valid {
			c.JSON(http.StatusForbidden, gin.H{"error": errMsgInvalidMFACode})
			return true
		}
		return false
	}

	if password != "" {
		match, verifyErr := h.verifyUserPassword(ctx, uid, password)
		if verifyErr != nil {
			h.log.Error("Password verification failed during session revoke", "error", verifyErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPassword})
			return true
		}
		if !match {
			c.JSON(http.StatusForbidden, gin.H{"error": errMsgIncorrectPassword})
			return true
		}
		return false
	}

	// Neither provided — tell client what's needed
	if hasMFA {
		methods, _ := h.mfaVerifier.GetEnabledMethods(ctx, uid)
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "auth_required",
			"message": "Authentication required to " + actionDesc,
			"methods": methods,
		})
	} else {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "password_required",
			"message": "Password verification required to " + actionDesc,
		})
	}
	return true
}

// executeRevocation performs the actual session revocation, tracking, and disconnect.
func (h *Handler) executeRevocation(ctx context.Context, c *gin.Context, uid, sessionID string, needsPassword bool) {
	result, err := h.db.ExecContext(ctx, //nolint:gosec // parameterized query — not injectable
		`UPDATE refresh_tokens SET revoked_at = NOW() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		sessionID, uid,
	)
	if err != nil {
		h.log.Error("Failed to revoke session", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRevokeSession})
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Session not found"})
		return
	}

	h.recordRevoke(ctx, uid, sessionID)
	h.hub.DisconnectSession(sessionID)

	h.log.Info("Session revoked", "user_id", uid, "session_id", sessionID, "password_used", needsPassword)

	c.JSON(http.StatusOK, gin.H{
		"message":    "Session revoked successfully",
		"session_id": sessionID,
	})
}

// RevokeAllSessions revokes all sessions after verifying the user's password.
// Requires a JSON body with "password" and optionally "include_current".
// Password re-verification prevents an attacker with a stolen session from
// mass-revoking the victim's sessions as a DoS.
func (h *Handler) RevokeAllSessions(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var req struct {
		Password       string `json:"password"` // #nosec G117 -- not a secret field, receives user input for auth verification
		MFACode        string `json:"mfa_code"`
		IncludeCurrent bool   `json:"include_current"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	uid := userID.(string)
	ctx := c.Request.Context()

	if h.authenticateForRevoke(ctx, c, uid, req.Password, req.MFACode, "revoke all sessions") {
		return
	}

	result, err := h.revokeAllSessionsDB(uid, req.IncludeCurrent, c)
	if err != nil {
		h.log.Error("Failed to revoke sessions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke sessions"})
		return
	}

	rowsAffected, _ := result.RowsAffected()

	if uidParsed, parseErr := uuid.Parse(uid); parseErr == nil {
		h.hub.DisconnectUser(uidParsed)
	}

	h.resetRevokeTracking(ctx, uid)
	h.log.Info("Sessions revoked", "user_id", uid, "count", rowsAffected, "include_current", req.IncludeCurrent)

	message := "All other sessions revoked successfully"
	if req.IncludeCurrent {
		message = "All sessions revoked successfully"
		c.SetCookie("refresh_token", "", -1, "/", "", c.Request.TLS != nil, true)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":         message,
		"count":           rowsAffected,
		"include_current": req.IncludeCurrent,
	})
}

// revokeAllSessionsDB revokes all sessions, optionally excluding the current one.
func (h *Handler) revokeAllSessionsDB(uid string, includeCurrent bool, c *gin.Context) (sql.Result, error) {
	currentToken, cookieErr := c.Cookie("refresh_token")
	if cookieErr != nil {
		currentToken = ""
	}

	if currentToken != "" && !includeCurrent {
		currentTokenHash := auth.HashRefreshToken(currentToken)
		return h.db.Exec(
			`UPDATE refresh_tokens SET revoked_at = NOW()
			 WHERE user_id = $1 AND token_hash != $2 AND revoked_at IS NULL`,
			uid, currentTokenHash)
	}
	return h.db.Exec(
		`UPDATE refresh_tokens SET revoked_at = NOW()
		 WHERE user_id = $1 AND revoked_at IS NULL`, uid)
}

// UpdateRevocationMode allows users to toggle between "simple" and "secure"
// revocation modes. Requires password verification (+ MFA when available).
// This setting is server-authoritative and bypasses the encrypted preferences system.
func (h *Handler) UpdateRevocationMode(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var req struct {
		Mode     string `json:"mode"`     // "simple" or "secure"
		Password string `json:"password"` // #nosec G117 -- not a secret field, receives user input for auth verification
		MFACode  string `json:"mfa_code"` // Framework: accepted but unused until MFA is implemented
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	if req.Mode != "simple" && req.Mode != "secure" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Mode must be 'simple' or 'secure'"})
		return
	}

	uid := userID.(string)
	ctx := c.Request.Context()

	if h.authenticateForRevoke(ctx, c, uid, req.Password, req.MFACode, "change revocation mode") {
		return
	}

	_, err := h.db.ExecContext(ctx, //nolint:gosec // parameterized query — not injectable
		`UPDATE users SET revocation_mode = $1 WHERE id = $2`,
		req.Mode, uid,
	)
	if err != nil {
		h.log.Error("Failed to update revocation mode", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update revocation mode"})
		return
	}

	if req.Mode == "secure" {
		h.redis.Del(ctx, simpleAuthWindowKey(uid))
	}

	h.log.Info("Revocation mode changed", "user_id", uid, "mode", req.Mode)

	c.JSON(http.StatusOK, gin.H{
		"revocation_mode": req.Mode,
	})
}
