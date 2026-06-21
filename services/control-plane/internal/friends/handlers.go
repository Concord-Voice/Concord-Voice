// Package friends provides handlers for friend relationship management.
package friends

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/invites"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	errMsgFailedClaimFriendCode   = "Failed to claim friend code"
	errMsgFailedSendFriendRequest = "Failed to send friend request"
	errMsgFailedBlockUser         = "Failed to block user"
)

// Handler handles friend-related requests.
type Handler struct {
	db  *sql.DB
	log *logger.Logger
	hub *websocket.Hub
}

// NewHandler creates a new friends handler.
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub) *Handler {
	return &Handler{
		db:  db,
		log: log,
		hub: hub,
	}
}

// friendResponse represents a friend in API responses.
type friendResponse struct {
	ID          string  `json:"id"`
	UserID      string  `json:"user_id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name,omitempty"`
	AvatarURL   *string `json:"avatar_url,omitempty"`
	ColorScheme *string `json:"color_scheme,omitempty"`
	CreatedAt   string  `json:"created_at"`
}

// friendRequestResponse represents a friend request in API responses.
type friendRequestResponse struct {
	ID              string  `json:"id"`
	FromUserID      string  `json:"from_user_id"`
	FromUsername    string  `json:"from_username"`
	FromDisplayName *string `json:"from_display_name,omitempty"`
	FromAvatarURL   *string `json:"from_avatar_url,omitempty"`
	ToUserID        string  `json:"to_user_id"`
	ToUsername      string  `json:"to_username"`
	ToDisplayName   *string `json:"to_display_name,omitempty"`
	ToAvatarURL     *string `json:"to_avatar_url,omitempty"`
	Direction       string  `json:"direction"`
	CreatedAt       string  `json:"created_at"`
}

// ListFriends returns the caller's accepted friendships with user details.
// GET /friends
func (h *Handler) ListFriends(c *gin.Context) {
	userID := c.GetString("user_id")

	query := `
		SELECT f.id, u.id, u.username, u.display_name, u.avatar_url, u.color_scheme, f.created_at
		FROM friendships f
		INNER JOIN users u ON u.id = CASE
			WHEN f.requester_id = $1 THEN f.addressee_id
			ELSE f.requester_id
		END
		WHERE (f.requester_id = $1 OR f.addressee_id = $1)
		  AND f.status = 'accepted'
		ORDER BY u.username ASC
	`

	rows, err := h.db.Query(query, userID)
	if err != nil {
		h.log.Error("Failed to query friends", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friends"})
		return
	}
	defer func() { _ = rows.Close() }()

	friends := []friendResponse{}
	for rows.Next() {
		var f friendResponse
		if err := rows.Scan(&f.ID, &f.UserID, &f.Username, &f.DisplayName, &f.AvatarURL, &f.ColorScheme, &f.CreatedAt); err != nil {
			h.log.Error("Failed to scan friend", "error", err)
			continue
		}
		friends = append(friends, f)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating friends", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friends"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"friends": friends})
}

// ListRequests returns pending friend requests (sent and received).
// GET /friends/requests
func (h *Handler) ListRequests(c *gin.Context) {
	userID := c.GetString("user_id")

	query := `
		SELECT f.id,
		       req.id, req.username, req.display_name, req.avatar_url,
		       addr.id, addr.username, addr.display_name, addr.avatar_url,
		       CASE WHEN f.requester_id = $1 THEN 'sent' ELSE 'received' END AS direction,
		       f.created_at
		FROM friendships f
		INNER JOIN users req ON req.id = f.requester_id
		INNER JOIN users addr ON addr.id = f.addressee_id
		WHERE (f.requester_id = $1 OR f.addressee_id = $1)
		  AND f.status = 'pending'
		ORDER BY f.created_at DESC
	`

	rows, err := h.db.Query(query, userID)
	if err != nil {
		h.log.Error("Failed to query friend requests", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend requests"})
		return
	}
	defer func() { _ = rows.Close() }()

	requests := []friendRequestResponse{}
	for rows.Next() {
		var r friendRequestResponse
		if err := rows.Scan(
			&r.ID,
			&r.FromUserID, &r.FromUsername, &r.FromDisplayName, &r.FromAvatarURL,
			&r.ToUserID, &r.ToUsername, &r.ToDisplayName, &r.ToAvatarURL,
			&r.Direction, &r.CreatedAt,
		); err != nil {
			h.log.Error("Failed to scan friend request", "error", err)
			continue
		}
		requests = append(requests, r)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating friend requests", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend requests"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"requests": requests})
}

// SendRequestBody represents the request to send a friend request.
type SendRequestBody struct {
	UserID   *string `json:"user_id"`
	Username *string `json:"username"`
}

type resolveResult struct {
	targetUserID string
	status       int
	errMsg       string
}

func (h *Handler) resolveTargetUserID(req SendRequestBody) resolveResult {
	if req.UserID != nil {
		if _, err := uuid.Parse(*req.UserID); err != nil {
			return resolveResult{status: http.StatusBadRequest, errMsg: "Invalid user_id"}
		}
		return resolveResult{targetUserID: *req.UserID}
	}
	if req.Username != nil {
		var targetUserID string
		err := h.db.QueryRow(`SELECT id FROM users WHERE username = $1`, strings.ToLower(strings.TrimSpace(*req.Username))).Scan(&targetUserID)
		if err == sql.ErrNoRows {
			return resolveResult{status: http.StatusNotFound, errMsg: "User not found"}
		}
		if err != nil {
			h.log.Error("Failed to look up user by username", "error", err)
			return resolveResult{status: http.StatusInternalServerError, errMsg: errMsgFailedSendFriendRequest}
		}
		return resolveResult{targetUserID: targetUserID}
	}
	return resolveResult{status: http.StatusBadRequest, errMsg: "user_id or username is required"}
}

func checkExistingFriendship(querier interface {
	QueryRow(string, ...interface{}) *sql.Row
}, userID, targetUserID string) (string, error) {
	var existingStatus string
	err := querier.QueryRow(`
		SELECT status FROM friendships
		WHERE (requester_id = $1 AND addressee_id = $2)
		   OR (requester_id = $2 AND addressee_id = $1)
	`, userID, targetUserID).Scan(&existingStatus)
	return existingStatus, err
}

func friendshipConflictResponse(status string) (int, string) {
	switch status {
	case "accepted":
		return http.StatusConflict, "Already friends"
	case "pending":
		return http.StatusConflict, "Friend request already pending"
	case "blocked":
		return http.StatusForbidden, "Cannot send friend request"
	default:
		return 0, ""
	}
}

func (h *Handler) notifyFriendRequestReceived(targetUserID, friendshipID, userID, createdAt string) {
	if h.hub == nil {
		return
	}
	addresseeUUID, parseErr := uuid.Parse(targetUserID)
	if parseErr != nil {
		return
	}
	var sender userProfile
	if err := h.db.QueryRow(`SELECT username, display_name, avatar_url FROM users WHERE id = $1`, userID).
		Scan(&sender.username, &sender.displayName, &sender.avatarURL); err != nil {
		h.log.Error("notifyFriendRequestReceived: failed to load sender profile", "error", err)
	}

	// Addressee (recipient) profile. The client's friend_request_received handler
	// bails without to_user_id/to_username (#981), and because that check is a
	// truthiness test (useWebSocketMessages.ts), an empty to_username is dropped
	// too. If the addressee profile can't be loaded, skip the broadcast rather than
	// emit a payload the client is guaranteed to discard. (to_user_id is the parsed
	// targetUserID and is always present; to_username is the field that can be empty.)
	var addressee userProfile
	if err := h.db.QueryRow(`SELECT username, display_name, avatar_url FROM users WHERE id = $1`, targetUserID).
		Scan(&addressee.username, &addressee.displayName, &addressee.avatarURL); err != nil {
		h.log.Error("notifyFriendRequestReceived: failed to load addressee profile", "error", err)
		return
	}

	h.hub.BroadcastToUser(addresseeUUID, websocket.OutgoingMessage{
		Type: "friend_request_received",
		Data: friendRequestReceivedData(friendshipID, userID, sender, targetUserID, addressee, createdAt),
	})
}

// SendRequest sends a friend request to another user.
// POST /friends/request
func (h *Handler) SendRequest(c *gin.Context) {
	userID := c.GetString("user_id")

	var req SendRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	resolved := h.resolveTargetUserID(req)
	if resolved.errMsg != "" {
		c.JSON(resolved.status, gin.H{"error": resolved.errMsg})
		return
	}
	targetUserID := resolved.targetUserID

	if targetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot send a friend request to yourself"})
		return
	}

	var exists bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, targetUserID).Scan(&exists); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	existingStatus, err := checkExistingFriendship(h.db, userID, targetUserID)
	if err == nil {
		code, msg := friendshipConflictResponse(existingStatus)
		if code != 0 {
			c.JSON(code, gin.H{"error": msg})
			return
		}
	} else if err != sql.ErrNoRows {
		h.log.Error("Failed to check existing friendship", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendFriendRequest})
		return
	}

	var friendshipID string
	var createdAt string
	err = h.db.QueryRow(`
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'pending')
		RETURNING id, created_at
	`, userID, targetUserID).Scan(&friendshipID, &createdAt)
	if err != nil {
		h.log.Error("Failed to create friend request", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendFriendRequest})
		return
	}

	h.log.Info("Friend request sent", "from", userID, "to", targetUserID)
	h.notifyFriendRequestReceived(targetUserID, friendshipID, userID, createdAt)

	c.JSON(http.StatusCreated, gin.H{
		"id":         friendshipID,
		"status":     "pending",
		"created_at": createdAt,
	})
}

// RespondRequestBody represents a response to a friend request.
type RespondRequestBody struct {
	Action string `json:"action" binding:"required"` // accept, decline
}

func (h *Handler) acceptFriendRequest(c *gin.Context, requestID, userID, requesterID string) {
	_, err := h.db.Exec(`
		UPDATE friendships SET status = 'accepted', updated_at = NOW()
		WHERE id = $1
	`, requestID)
	if err != nil {
		h.log.Error("Failed to accept friend request", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to accept friend request"})
		return
	}

	h.log.Info("Friend request accepted", "request_id", requestID, "user_id", userID)
	h.notifyFriendRequestAccepted(requesterID, requestID, userID)
	c.JSON(http.StatusOK, gin.H{"message": "Friend request accepted"})
}

func (h *Handler) notifyFriendRequestAccepted(requesterID, requestID, userID string) {
	if h.hub == nil {
		return
	}
	requesterUUID, parseErr := uuid.Parse(requesterID)
	if parseErr != nil {
		return
	}
	var acceptorUsername string
	var acceptorDisplayName *string
	var acceptorAvatarURL *string
	if err := h.db.QueryRow(`SELECT username, display_name, avatar_url FROM users WHERE id = $1`, userID).
		Scan(&acceptorUsername, &acceptorDisplayName, &acceptorAvatarURL); err != nil {
		h.log.Error("notifyFriendRequestAccepted: failed to load acceptor profile", "error", err)
	}

	h.hub.BroadcastToUser(requesterUUID, websocket.OutgoingMessage{
		Type: "friend_request_accepted",
		Data: map[string]interface{}{
			"id":           requestID,
			"user_id":      userID,
			"username":     acceptorUsername,
			"display_name": acceptorDisplayName,
			"avatar_url":   acceptorAvatarURL,
		},
	})
}

// RespondRequest accepts or declines a friend request.
// PATCH /friends/request/:id
func (h *Handler) RespondRequest(c *gin.Context) {
	userID := c.GetString("user_id")
	requestID := c.Param("id")

	if _, err := uuid.Parse(requestID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request ID"})
		return
	}

	var req RespondRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if req.Action != "accept" && req.Action != "decline" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Action must be 'accept' or 'decline'"})
		return
	}

	var requesterID string
	err := h.db.QueryRow(`
		SELECT requester_id FROM friendships
		WHERE id = $1 AND addressee_id = $2 AND status = 'pending'
	`, requestID, userID).Scan(&requesterID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Friend request not found"})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch friend request", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to respond to friend request"})
		return
	}

	if req.Action == "accept" {
		h.acceptFriendRequest(c, requestID, userID, requesterID)
		return
	}

	_, err = h.db.Exec(`DELETE FROM friendships WHERE id = $1`, requestID)
	if err != nil {
		h.log.Error("Failed to decline friend request", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decline friend request"})
		return
	}

	h.log.Info("Friend request declined", "request_id", requestID, "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"message": "Friend request declined"})
}

// RemoveFriend removes a friendship.
// DELETE /friends/:user_id
func (h *Handler) RemoveFriend(c *gin.Context) {
	userID := c.GetString("user_id")
	targetUserID := c.Param("user_id")

	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	result, err := h.db.Exec(`
		DELETE FROM friendships
		WHERE ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))
		  AND status = 'accepted'
	`, userID, targetUserID)
	if err != nil {
		h.log.Error("Failed to remove friend", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove friend"})
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Friendship not found"})
		return
	}

	h.log.Info("Friend removed", "user_id", userID, "target", targetUserID)

	// Notify the other user
	if h.hub != nil {
		if targetUUID, parseErr := uuid.Parse(targetUserID); parseErr == nil {
			h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
				Type: "friend_removed",
				Data: map[string]interface{}{
					"user_id": userID,
				},
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Friend removed"})
}

func executeBlockTx(tx *sql.Tx, userID, targetUserID string) error {
	res, err := tx.Exec(`
		UPDATE friendships SET status = 'blocked', updated_at = NOW()
		WHERE (requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1)
	`, userID, targetUserID)
	if err != nil {
		return err
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		_, err = tx.Exec(`
			INSERT INTO friendships (requester_id, addressee_id, status)
			VALUES ($1, $2, 'blocked')
		`, userID, targetUserID)
		if err != nil {
			return err
		}
	}
	return nil
}

type convEpoch struct {
	convID   string
	maxEpoch int
}

func (h *Handler) findDMRevocations(tx *sql.Tx, userID, targetUserID string) []convEpoch {
	revokeRows, revokeErr := tx.Query(`
		SELECT dp1.conversation_id, COALESCE(MAX(dck.key_version), 0)
		FROM dm_participants dp1
		INNER JOIN dm_participants dp2 ON dp1.conversation_id = dp2.conversation_id
		LEFT JOIN dm_channel_keys dck ON dck.conversation_id = dp1.conversation_id
		WHERE dp1.user_id = $1 AND dp2.user_id = $2
		GROUP BY dp1.conversation_id
	`, userID, targetUserID)
	if revokeErr != nil {
		h.log.Error("Failed to query shared DM conversations for revocation", "error", revokeErr)
		return nil
	}
	defer func() { _ = revokeRows.Close() }()

	var revocations []convEpoch
	for revokeRows.Next() {
		var ce convEpoch
		if err := revokeRows.Scan(&ce.convID, &ce.maxEpoch); err != nil || ce.maxEpoch == 0 {
			continue
		}
		revocations = append(revocations, ce)
	}
	if err := revokeRows.Err(); err != nil {
		h.log.Error("Error iterating DM revocation rows", "error", err)
	}
	return revocations
}

func (h *Handler) revokeBlockedDMKeys(tx *sql.Tx, userID, targetUserID string) {
	revocations := h.findDMRevocations(tx, userID, targetUserID)
	for _, ce := range revocations {
		if _, err := tx.Exec(`
			INSERT INTO dm_key_revocations (conversation_id, revoked_epoch, successor_epoch, reason, revoked_by)
			VALUES ($1, $2, $3, 'user_blocked', $4)
			ON CONFLICT (conversation_id, revoked_epoch) DO NOTHING
		`, ce.convID, ce.maxEpoch, ce.maxEpoch+1, userID); err != nil {
			h.log.Error("Failed to record DM key revocation on block", "error", err, "conversation_id", ce.convID)
		}
	}

	_, _ = tx.Exec(`
		DELETE FROM dm_channel_keys
		WHERE user_id = $2
		  AND conversation_id IN (
			SELECT dp1.conversation_id FROM dm_participants dp1
			INNER JOIN dm_participants dp2 ON dp1.conversation_id = dp2.conversation_id
			WHERE dp1.user_id = $1 AND dp2.user_id = $2
		  )
		  AND key_version = (
			SELECT MAX(key_version) FROM dm_channel_keys dck2
			WHERE dck2.conversation_id = dm_channel_keys.conversation_id
		  )
	`, userID, targetUserID)
}

func (h *Handler) notifyBlock(userID, targetUserID string) {
	if h.hub == nil {
		return
	}
	if targetUUID, parseErr := uuid.Parse(targetUserID); parseErr == nil {
		h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
			Type: "friend_removed",
			Data: map[string]interface{}{
				"user_id": userID,
			},
		})
	}
	if blockerUUID, parseErr := uuid.Parse(userID); parseErr == nil {
		h.hub.BroadcastToUser(blockerUUID, websocket.OutgoingMessage{
			Type: "key_revocation",
			Data: map[string]interface{}{
				"blocked_user_id": targetUserID,
			},
		})
	}
}

// BlockUser blocks another user. If a friendship exists, it is updated to 'blocked'.
// If a DM conversation exists, the blocked user's current epoch key is revoked.
// POST /friends/:user_id/block
func (h *Handler) BlockUser(c *gin.Context) {
	userID := c.GetString("user_id")
	targetUserID := c.Param("user_id")

	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	if targetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot block yourself"})
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to start transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBlockUser})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	if err := executeBlockTx(tx, userID, targetUserID); err != nil {
		h.log.Error(errMsgFailedBlockUser, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBlockUser})
		return
	}

	h.revokeBlockedDMKeys(tx, userID, targetUserID)

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit block", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBlockUser})
		return
	}

	h.log.Info("User blocked", "user_id", userID, "blocked", targetUserID)
	h.notifyBlock(userID, targetUserID)
	c.JSON(http.StatusOK, gin.H{"message": "User blocked"})
}

// --- Friend Code types ---

// friendCodeResponse represents a friend code in API responses.
type friendCodeResponse struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Code       string     `json:"code"`
	MaxUses    *int       `json:"max_uses"`
	UseCount   int        `json:"use_count"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	IsRevoked  bool       `json:"is_revoked"`
	AutoAccept bool       `json:"auto_accept"`
	CreatedAt  string     `json:"created_at"`
}

type createFriendCodeRequest struct {
	MaxUses    *int  `json:"max_uses"`
	ExpiresIn  *int  `json:"expires_in"` // seconds; nil → default 3600 (1h)
	AutoAccept *bool `json:"auto_accept"`
}

func resolveMaxUses(input *int) *int {
	if input == nil {
		defaultMax := 1
		return &defaultMax
	}
	if *input <= 0 {
		return nil
	}
	maxUses := *input
	if maxUses > 10 {
		maxUses = 10
	}
	return &maxUses
}

func resolveExpiresIn(input *int) int {
	if input == nil {
		return 3600
	}
	sec := *input
	if sec < 300 {
		return 300
	}
	if sec > 86400 {
		return 86400
	}
	return sec
}

// CreateFriendCode generates a new friend code for the caller.
// POST /friends/codes
func (h *Handler) CreateFriendCode(c *gin.Context) {
	userID := c.GetString("user_id")

	var req createFriendCodeRequest
	_ = c.ShouldBindJSON(&req)

	maxUsesPtr := resolveMaxUses(req.MaxUses)
	expiresAt := time.Now().UTC().Add(time.Duration(resolveExpiresIn(req.ExpiresIn)) * time.Second)

	autoAccept := false
	if req.AutoAccept != nil {
		autoAccept = *req.AutoAccept
	}

	for attempts := 0; attempts < 5; attempts++ {
		code, err := invites.GenerateCode()
		if err != nil {
			h.log.Error("Failed to generate friend code", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create friend code"})
			return
		}

		var fc friendCodeResponse
		insertErr := h.db.QueryRow(`
			INSERT INTO friend_codes (user_id, code, max_uses, expires_at, auto_accept)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, user_id, code, max_uses, use_count, expires_at, is_revoked, auto_accept, created_at
		`, userID, code, maxUsesPtr, expiresAt, autoAccept).Scan(
			&fc.ID, &fc.UserID, &fc.Code, &fc.MaxUses, &fc.UseCount,
			&fc.ExpiresAt, &fc.IsRevoked, &fc.AutoAccept, &fc.CreatedAt,
		)
		if insertErr != nil {
			continue
		}

		h.log.Info("Friend code created", "user_id", userID, "code", code)
		c.JSON(http.StatusCreated, gin.H{"friend_code": fc})
		return
	}

	h.log.Error("Failed to create unique friend code after retries")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create friend code"})
}

// ListFriendCodes returns the caller's non-revoked friend codes.
// GET /friends/codes
func (h *Handler) ListFriendCodes(c *gin.Context) {
	userID := c.GetString("user_id")

	rows, err := h.db.Query(`
		SELECT id, user_id, code, max_uses, use_count, expires_at, is_revoked, auto_accept, created_at
		FROM friend_codes
		WHERE user_id = $1 AND is_revoked = FALSE
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		h.log.Error("Failed to query friend codes", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend codes"})
		return
	}
	defer func() { _ = rows.Close() }()

	codes := []friendCodeResponse{}
	for rows.Next() {
		var fc friendCodeResponse
		if err := rows.Scan(
			&fc.ID, &fc.UserID, &fc.Code, &fc.MaxUses, &fc.UseCount,
			&fc.ExpiresAt, &fc.IsRevoked, &fc.AutoAccept, &fc.CreatedAt,
		); err != nil {
			h.log.Error("Failed to scan friend code", "error", err)
			continue
		}
		codes = append(codes, fc)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating friend codes", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend codes"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"friend_codes": codes})
}

// RevokeFriendCode soft-revokes a friend code owned by the caller.
// DELETE /friends/codes/:id
func (h *Handler) RevokeFriendCode(c *gin.Context) {
	userID := c.GetString("user_id")
	codeID := c.Param("id")

	if _, err := uuid.Parse(codeID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid code ID"})
		return
	}

	result, err := h.db.Exec(`
		UPDATE friend_codes SET is_revoked = TRUE
		WHERE id = $1 AND user_id = $2 AND is_revoked = FALSE
	`, codeID, userID)
	if err != nil {
		h.log.Error("Failed to revoke friend code", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke friend code"})
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Friend code not found or already revoked"})
		return
	}

	h.log.Info("Friend code revoked", "code_id", codeID, "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"message": "Friend code revoked"})
}

// PreviewFriendCode validates a friend code and returns the owner's profile.
// Does NOT consume a use.
// GET /friends/codes/:code
func (h *Handler) PreviewFriendCode(c *gin.Context) {
	code := c.Param("code")

	if len(code) != 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid friend code format"})
		return
	}

	var (
		ownerID     string
		username    string
		displayName *string
		avatarURL   *string
		expiresAt   *time.Time
		isRevoked   bool
		maxUses     *int
		useCount    int
	)

	err := h.db.QueryRow(`
		SELECT fc.user_id, u.username, u.display_name, u.avatar_url,
		       fc.expires_at, fc.is_revoked, fc.max_uses, fc.use_count
		FROM friend_codes fc
		INNER JOIN users u ON fc.user_id = u.id
		WHERE fc.code = $1
	`, code).Scan(
		&ownerID, &username, &displayName, &avatarURL,
		&expiresAt, &isRevoked, &maxUses, &useCount,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invalid friend code"})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch friend code", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch friend code"})
		return
	}

	valid := !isRevoked &&
		(expiresAt == nil || expiresAt.After(time.Now().UTC())) &&
		(maxUses == nil || *maxUses == 0 || useCount < *maxUses)

	c.JSON(http.StatusOK, gin.H{
		"user_id":      ownerID,
		"username":     username,
		"display_name": displayName,
		"avatar_url":   avatarURL,
		"valid":        valid,
	})
}

type friendCodeRow struct {
	codeID     string
	ownerID    string
	maxUses    *int
	useCount   int
	expiresAt  *time.Time
	isRevoked  bool
	autoAccept bool
}

func lookupFriendCode(tx *sql.Tx, code string) (friendCodeRow, error) {
	var fc friendCodeRow
	err := tx.QueryRow(`
		SELECT id, user_id, max_uses, use_count, expires_at, is_revoked, auto_accept
		FROM friend_codes
		WHERE code = $1
		FOR UPDATE
	`, code).Scan(&fc.codeID, &fc.ownerID, &fc.maxUses, &fc.useCount, &fc.expiresAt, &fc.isRevoked, &fc.autoAccept)
	return fc, err
}

func validateFriendCodeClaim(fc friendCodeRow, userID string) (int, string) {
	if fc.isRevoked {
		return http.StatusGone, "This friend code has been revoked"
	}
	if fc.expiresAt != nil && fc.expiresAt.Before(time.Now().UTC()) {
		return http.StatusGone, "This friend code has expired"
	}
	if fc.maxUses != nil && *fc.maxUses > 0 && fc.useCount >= *fc.maxUses {
		return http.StatusGone, "This friend code has reached its maximum uses"
	}
	if fc.ownerID == userID {
		return http.StatusBadRequest, "Cannot claim your own friend code"
	}
	return 0, ""
}

func claimFriendshipConflictResponse(status string) (int, string) {
	switch status {
	case "accepted":
		return http.StatusConflict, "Already friends with this user"
	case "pending":
		return http.StatusConflict, "Friend request already pending with this user"
	case "blocked":
		return http.StatusForbidden, "Cannot add this user as a friend"
	default:
		return 0, ""
	}
}

func executeFriendCodeClaim(tx *sql.Tx, userID, ownerID, codeID string, autoAccept bool) (string, string, string, error) {
	status := "pending"
	if autoAccept {
		status = "accepted"
	}

	var friendshipID, createdAt string
	err := tx.QueryRow(`
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, $3)
		RETURNING id, created_at
	`, userID, ownerID, status).Scan(&friendshipID, &createdAt)
	if err != nil {
		return "", "", "", err
	}

	_, err = tx.Exec(`UPDATE friend_codes SET use_count = use_count + 1 WHERE id = $1`, codeID)
	if err != nil {
		return "", "", "", err
	}

	return friendshipID, createdAt, status, nil
}

type userProfile struct {
	username    string
	displayName *string
	avatarURL   *string
}

func (h *Handler) fetchUserProfile(userID string) userProfile {
	var p userProfile
	// Per [internal]rules/backend.md (#1142/#1154): never silently discard a query
	// error. A failed lookup leaves p.username == "", which the friend_request_received
	// emitters treat as a skip signal (the client drops an empty to_username, #981).
	// Log the error only — never profile values (observability.md).
	if err := h.db.QueryRow(`SELECT username, display_name, avatar_url FROM users WHERE id = $1`, userID).
		Scan(&p.username, &p.displayName, &p.avatarURL); err != nil {
		h.log.Error("fetchUserProfile: failed to load profile", "error", err)
	}
	return p
}

// friendRequestReceivedData builds the friend_request_received WS payload shared by
// both emit sites (SendRequest and the friend-code-claim path). It is pure (no DB,
// no hub) so the from_*/to_* wire shape can be unit-tested directly — that WS↔REST
// shape contract is exactly what drifted in #981, and the unit test guards it from
// regressing again. The client requires to_user_id and to_username.
func friendRequestReceivedData(friendshipID, fromUserID string, from userProfile, toUserID string, to userProfile, createdAt string) map[string]interface{} {
	return map[string]interface{}{
		"id":                friendshipID,
		"from_user_id":      fromUserID,
		"from_username":     from.username,
		"from_display_name": from.displayName,
		"from_avatar_url":   from.avatarURL,
		"to_user_id":        toUserID,
		"to_username":       to.username,
		"to_display_name":   to.displayName,
		"to_avatar_url":     to.avatarURL,
		"created_at":        createdAt,
	}
}

type claimNotification struct {
	ownerID      string
	userID       string
	friendshipID string
	codeID       string
	status       string
	createdAt    string
	autoAccept   bool
	claimer      userProfile
	owner        userProfile
}

func (h *Handler) notifyFriendCodeClaimed(n claimNotification) {
	if h.hub == nil {
		return
	}
	ownerUUID, parseErr := uuid.Parse(n.ownerID)
	if parseErr != nil {
		return
	}

	h.hub.BroadcastToUser(ownerUUID, websocket.OutgoingMessage{
		Type: "friend_code_claimed",
		Data: map[string]interface{}{
			"friendship_id": n.friendshipID,
			"code_id":       n.codeID,
			"status":        n.status,
			"user_id":       n.userID,
			"username":      n.claimer.username,
			"display_name":  n.claimer.displayName,
			"avatar_url":    n.claimer.avatarURL,
			"created_at":    n.createdAt,
		},
	})

	if n.autoAccept {
		if claimerUUID, parseErr2 := uuid.Parse(n.userID); parseErr2 == nil {
			h.hub.BroadcastToUser(claimerUUID, websocket.OutgoingMessage{
				Type: "friend_request_accepted",
				Data: map[string]interface{}{
					"id":           n.friendshipID,
					"user_id":      n.ownerID,
					"username":     n.owner.username,
					"display_name": n.owner.displayName,
					"avatar_url":   n.owner.avatarURL,
				},
			})
		}
		return
	}

	// owner profile is fetched upstream via fetchUserProfile; an empty username means
	// that lookup failed (username is NOT NULL). The client drops a friend_request_received
	// with an empty to_username (#981), so skip rather than emit a payload it will discard.
	if n.owner.username == "" {
		h.log.Warn("notifyFriendCodeClaimed: empty owner username; skipping friend_request_received broadcast")
		return
	}
	h.hub.BroadcastToUser(ownerUUID, websocket.OutgoingMessage{
		Type: "friend_request_received",
		Data: friendRequestReceivedData(n.friendshipID, n.userID, n.claimer, n.ownerID, n.owner, n.createdAt),
	})
}

// ClaimFriendCode redeems a friend code to create a friendship.
// If auto_accept is set on the code, the friendship is created as 'accepted' directly.
// Otherwise, a pending friend request is created from the claimer to the code owner.
// POST /friends/codes/:code/claim
func (h *Handler) ClaimFriendCode(c *gin.Context) {
	userID := c.GetString("user_id")
	code := c.Param("code")

	if len(code) != 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid friend code format"})
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to begin transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedClaimFriendCode})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback", "error", rbErr)
		}
	}()

	fc, err := lookupFriendCode(tx, code)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Invalid friend code"})
		return
	}
	if err != nil {
		h.log.Error("Failed to query friend code", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedClaimFriendCode})
		return
	}

	if errCode, errMsg := validateFriendCodeClaim(fc, userID); errCode != 0 {
		c.JSON(errCode, gin.H{"error": errMsg})
		return
	}

	existingStatus, err := checkExistingFriendship(tx, userID, fc.ownerID)
	if err == nil {
		if respCode, msg := claimFriendshipConflictResponse(existingStatus); respCode != 0 {
			c.JSON(respCode, gin.H{"error": msg})
			return
		}
	} else if err != sql.ErrNoRows {
		h.log.Error("Failed to check existing friendship", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedClaimFriendCode})
		return
	}

	friendshipID, createdAt, status, err := executeFriendCodeClaim(tx, userID, fc.ownerID, fc.codeID, fc.autoAccept)
	if err != nil {
		h.log.Error("Failed to create friendship", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedClaimFriendCode})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedClaimFriendCode})
		return
	}

	h.log.Info("Friend code claimed", "code", code, "claimer", userID, "owner", fc.ownerID, "status", status)

	claimer := h.fetchUserProfile(userID)
	owner := h.fetchUserProfile(fc.ownerID)
	h.notifyFriendCodeClaimed(claimNotification{
		ownerID:      fc.ownerID,
		userID:       userID,
		friendshipID: friendshipID,
		codeID:       fc.codeID,
		status:       status,
		createdAt:    createdAt,
		autoAccept:   fc.autoAccept,
		claimer:      claimer,
		owner:        owner,
	})

	c.JSON(http.StatusOK, gin.H{
		"status":        status,
		"friendship_id": friendshipID,
		"user": map[string]interface{}{
			"user_id":      fc.ownerID,
			"username":     owner.username,
			"display_name": owner.displayName,
			"avatar_url":   owner.avatarURL,
		},
	})
}
