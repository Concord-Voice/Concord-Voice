// Package dm provides handlers for direct message conversations and related operations.
package dm

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	messagehandlers "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/messages"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

const (
	errMsgInvalidConversationID      = "Invalid conversation ID"
	errMsgNotParticipant             = "Not a participant"
	errMsgInvalidRequestBody         = "Invalid request body"
	errMsgFailedOpenConversation     = "Failed to open conversation"
	errMsgFailedCreateGroup          = "Failed to create group"
	errMsgUserNotFound               = "User not found"
	errMsgFailedCreatePersonalThread = "Failed to create personal thread"
	errMsgFailedUpdateConversation   = "Failed to update conversation"
	errMsgFailedUpdateMessage        = "Failed to update message"
	errMsgMessageNotFound            = "Message not found"
	errMsgFailedStartTransaction     = "Failed to start transaction"
	errMsgFailedRollbackTransaction  = "Failed to rollback transaction"
	errMsgFailedAddMember            = "Failed to add member"
	errMsgFailedRemoveMember         = "Failed to remove member"
	errMsgNotAdmin                   = "Only admins can perform this action"
	errMsgFailedCheckConversation    = "Failed to check conversation"
	errMsgNotGroupConversation       = "This action is only available for group conversations"
	errMsgInvalidUserID              = "Invalid user ID"
	errMsgInvalidCallerID            = "Invalid caller ID"
	errMsgInvalidVoiceJoinRequest    = "Invalid voice join request"
	errMsgInvalidVoiceCallRequest    = "Invalid voice call request"
	errMsgAlreadyRinging             = "already ringing"
	errMsgVoiceCallRingChanged       = "Voice call ring has changed"
	errMsgVoiceCallNoLongerActive    = "Voice call is no longer active"
	errMsgVoiceCallAlreadyActive     = "voice call already active"
	errMsgCannotTargetSelf           = "Cannot mute yourself"
	errMsgTargetNotInVoice           = "Target user is not in this voice call"
	errMsgFailedCheckParticipation   = "Failed to check DM participation"
	errMsgFailedAuthorize            = "Failed to authorize"
	errMsgHardEnforcementGroupOnly   = "Hard enforcement is only available in group DMs"
	errMsgOnlyAdminCanMute           = "Only group admins can server-mute"
	errMsgOnlyAdminCanDeafen         = "Only group admins can server-deafen"
	errMsgOnlyAdminCanUnmute         = "Only group admins can remove server-mute"
	errMsgOnlyAdminCanUndeafen       = "Only group admins can remove server-deafen"
	errMsgFailedApplyEnforcement     = "Failed to apply enforcement"
	errMsgFailedRemoveEnforcement    = "Failed to remove enforcement"
	errMsgTargetNotParticipant       = "Target is not a participant"
	errMsgCannotUnmuteWhileDeafened  = "Cannot unmute while server-deafened — remove deafen first"
	errMsgFailedDistributeKeys       = "Failed to distribute keys"
)

// dm_privacy_level enum values (matches migration 000032_dm_privacy_level.up.sql).
// Replaces magic-number comparisons (0, 1, 2, 3) flagged during /enhanced-pr-review
// on PR #1151. The legacy boolean columns (messages_friends_only / messages_server_members)
// from migration 000027 still exist in the table but are superseded by dm_privacy_level.
const (
	dmPrivacyOff              = 0 // DMs disabled — no one can DM the user
	dmPrivacyFriendsOnly      = 1 // Friends (and friends-of-friends if enabled), no server-share fallback
	dmPrivacyFriendsAndServer = 2 // Friends + shared-server members; minimum level at which server fallback is allowed
	dmPrivacyOpenToAll        = 3 // Anyone may DM
)

// Handler handles DM-related requests.
type Handler struct {
	db          *sql.DB
	log         *logger.Logger
	hub         *websocket.Hub
	cfg         *config.Config
	nats        *natsclient.Client
	redis       *redis.Client
	entCache    *entitlements.Cache
	purgeEngine *purge.Engine // bulk message purge (#1352)
	mfaVerifier mfa.Verifier  // step-up auth for DM/group purge (#1352)
}

type epochQueryRower interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

// HandlerDeps bundles the DM handler's dependencies. Mirrors the voice/ownership
// HandlerDeps convention: the dependency list outgrew a positional signature when
// the bulk-purge engine and its step-up verifier were added (#1352).
type HandlerDeps struct {
	DB  *sql.DB
	Log *logger.Logger
	Hub *websocket.Hub
	Cfg *config.Config
	// NATS may be nil in tests.
	NATS  *natsclient.Client
	Redis *redis.Client
	// EntCache is the shared entitlement-tier cache (#1296) used to resolve the
	// joining user's media entitlements for DM voice joins (#1300); production
	// wiring passes the same instance the auth and voice handlers receive.
	EntCache *entitlements.Cache
	// PurgeEngine + MFAVerifier back the DM/group bulk purge and its step-up
	// gate (#1352). Both may be nil in tests that do not exercise purge.
	PurgeEngine *purge.Engine
	MFAVerifier mfa.Verifier
}

// NewHandler creates a new DM handler from its dependency bundle.
func NewHandler(deps HandlerDeps) *Handler {
	return &Handler{
		db:          deps.DB,
		log:         deps.Log,
		hub:         deps.Hub,
		cfg:         deps.Cfg,
		nats:        deps.NATS,
		redis:       deps.Redis,
		entCache:    deps.EntCache,
		purgeEngine: deps.PurgeEngine,
		mfaVerifier: deps.MFAVerifier,
	}
}

// dmMessageResponse represents a DM message in API responses.
type dmMessageResponse struct {
	ID               string                     `json:"id"`
	ConversationID   string                     `json:"conversation_id"`
	UserID           string                     `json:"user_id"`
	Content          string                     `json:"content"`
	Type             string                     `json:"type"`
	CallEventPayload json.RawMessage            `json:"call_event_payload,omitempty"`
	KeyVersion       int                        `json:"key_version"`
	EditedAt         *string                    `json:"edited_at,omitempty"`
	CreatedAt        string                     `json:"created_at"`
	Username         string                     `json:"username"`
	DisplayName      *string                    `json:"display_name,omitempty"`
	AvatarURL        *string                    `json:"avatar_url,omitempty"`
	Attachments      []models.AttachmentSummary `json:"attachments,omitempty"`
	Reactions        []models.ReactionSummary   `json:"reactions,omitempty"`
}

func (h *Handler) enrichDMReactions(messages []dmMessageResponse, userID string) {
	if len(messages) == 0 {
		return
	}
	msgIDs := make([]string, len(messages))
	for i, m := range messages {
		msgIDs[i] = m.ID
	}
	reactionMap, err := messagehandlers.LoadDMReactionsForMessages(h.db, msgIDs, userID)
	if err != nil {
		h.log.Error("Failed to load DM reactions", "error", err)
		return
	}
	for i := range messages {
		if reactions, ok := reactionMap[messages[i].ID]; ok {
			messages[i].Reactions = reactions
		}
	}
}

// enrichDMAttachments batch-loads and attaches file attachment summaries to DM messages.
func (h *Handler) enrichDMAttachments(messages []dmMessageResponse) {
	if len(messages) == 0 {
		return
	}
	msgIDs := make([]string, len(messages))
	for i, m := range messages {
		msgIDs[i] = m.ID
	}
	attachmentMap, err := h.loadDMAttachments(msgIDs)
	if err != nil {
		h.log.Error("Failed to load DM attachments", "error", err)
		return
	}
	for i := range messages {
		if attachments, ok := attachmentMap[messages[i].ID]; ok {
			messages[i].Attachments = attachments
		}
	}
}

// --- Conversation Endpoints ---

// conversationResponse represents a DM conversation in API responses.
type conversationResponse struct {
	ID           string                `json:"id"`
	IsGroup      bool                  `json:"is_group"`
	IsPersonal   bool                  `json:"is_personal"`
	Name         *string               `json:"name,omitempty"`
	IconURL      *string               `json:"icon_url,omitempty"`
	CreatedBy    string                `json:"created_by"`
	Participants []participantResponse `json:"participants"`
	LastMessage  *lastMessageResponse  `json:"last_message,omitempty"`
	UnreadCount  int                   `json:"unread_count"`
	CreatedAt    string                `json:"created_at"`
}

type participantResponse struct {
	UserID      string  `json:"user_id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name,omitempty"`
	AvatarURL   *string `json:"avatar_url,omitempty"`
	ColorScheme *string `json:"color_scheme,omitempty"`
	Role        string  `json:"role"`
}

type lastMessageResponse struct {
	Content          string          `json:"content"`
	UserID           string          `json:"user_id"`
	CreatedAt        string          `json:"created_at"`
	Type             string          `json:"type,omitempty"`
	CallEventPayload json.RawMessage `json:"call_event_payload,omitempty"`
}

// ListConversations returns the caller's DM conversations with last message preview.
// GET /dm/conversations
func (h *Handler) ListConversations(c *gin.Context) {
	userID := c.GetString("user_id")

	conversations, convIDs, err := h.queryConversations(userID)
	if err != nil {
		h.log.Error("Failed to query DM conversations", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch conversations"})
		return
	}

	if len(convIDs) > 0 {
		h.attachParticipants(conversations, convIDs)
	}

	c.JSON(http.StatusOK, gin.H{"conversations": conversations})
}

func (h *Handler) queryConversations(userID string) ([]conversationResponse, []string, error) {
	// Both dm_messages subqueries carry hiddenRangeFilter so a receiver-hidden
	// message (#1352) neither surfaces as the list preview nor inflates the
	// unread badge for the user who hid it. The concatenated fragment is a
	// compile-time constant (hardcoded alias + integer placeholder position);
	// all VALUES are parameterized.
	//nolint:gosec // G202: concatenated fragment is a compile-time constant (hardcoded alias + integer placeholder); all values parameterized
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf
	query := `
		SELECT dc.id, dc.is_group, dc.is_personal, dc.name, dc.icon_url, dc.created_by, dc.created_at,
		       dm.content, dm.created_at, dm.user_id, dm.type, dm.call_event_payload,
		       (SELECT COUNT(*) FROM dm_messages m
		        WHERE m.conversation_id = dc.id
		          AND m.created_at > COALESCE(drs.last_read_at, '1970-01-01')
		          AND m.user_id != $1
		        ` + hiddenRangeFilter(1) + `
		       ) AS unread_count
		FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $1
		LEFT JOIN dm_read_states drs ON drs.conversation_id = dc.id AND drs.user_id = $1
		LEFT JOIN LATERAL (
		    SELECT m.content, m.created_at, m.user_id, m.type, m.call_event_payload FROM dm_messages m
		    WHERE m.conversation_id = dc.id ` + hiddenRangeFilter(1) + `
		    ORDER BY m.created_at DESC LIMIT 1
		) dm ON TRUE
		ORDER BY COALESCE(dm.created_at, dc.created_at) DESC
	`

	rows, err := h.db.Query(query, userID)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	conversations := []conversationResponse{}
	convIDs := []string{}
	for rows.Next() {
		conv, scanErr := h.scanConversationRow(rows)
		if scanErr != nil {
			h.log.Error("Failed to scan DM conversation", "error", scanErr)
			continue
		}
		convIDs = append(convIDs, conv.ID)
		conversations = append(conversations, conv)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return conversations, convIDs, nil
}

func (h *Handler) scanConversationRow(rows *sql.Rows) (conversationResponse, error) {
	var conv conversationResponse
	var lmContent, lmCreatedAt, lmUserID, lmType sql.NullString
	var lmCallEventPayload []byte
	if err := rows.Scan(
		&conv.ID, &conv.IsGroup, &conv.IsPersonal, &conv.Name, &conv.IconURL, &conv.CreatedBy, &conv.CreatedAt,
		&lmContent, &lmCreatedAt, &lmUserID, &lmType, &lmCallEventPayload,
		&conv.UnreadCount,
	); err != nil {
		return conv, err
	}
	if lmContent.Valid {
		conv.LastMessage = &lastMessageResponse{
			Content:          lmContent.String,
			UserID:           lmUserID.String,
			CreatedAt:        lmCreatedAt.String,
			Type:             lmType.String,
			CallEventPayload: json.RawMessage(lmCallEventPayload),
		}
	}
	return conv, nil
}

func (h *Handler) attachParticipants(conversations []conversationResponse, convIDs []string) {
	participantMap := make(map[string][]participantResponse)
	pQuery := `
		SELECT dp.conversation_id, u.id, u.username, u.display_name, u.avatar_url, u.color_scheme, dp.role
		FROM dm_participants dp
		INNER JOIN users u ON u.id = dp.user_id
		WHERE dp.conversation_id = ANY($1::uuid[])
	`
	uuidArray := "{"
	for i, id := range convIDs {
		if i > 0 {
			uuidArray += ","
		}
		uuidArray += id
	}
	uuidArray += "}"

	pRows, err := h.db.Query(pQuery, uuidArray)
	if err != nil {
		h.log.Error("Failed to query DM participants", "error", err)
	} else {
		defer func() { _ = pRows.Close() }()
		for pRows.Next() {
			var convID string
			var p participantResponse
			if err := pRows.Scan(&convID, &p.UserID, &p.Username, &p.DisplayName, &p.AvatarURL, &p.ColorScheme, &p.Role); err != nil {
				continue
			}
			participantMap[convID] = append(participantMap[convID], p)
		}
	}

	for i := range conversations {
		conversations[i].Participants = participantMap[conversations[i].ID]
		if conversations[i].Participants == nil {
			conversations[i].Participants = []participantResponse{}
		}
	}
}

// OpenConversationRequest represents a request to open a 1:1 DM.
type OpenConversationRequest struct {
	UserID string `json:"user_id" binding:"required"`
}

// OpenConversation gets or creates a 1:1 DM conversation.
// POST /dm/conversations
func (h *Handler) OpenConversation(c *gin.Context) {
	userID := c.GetString("user_id")

	var req OpenConversationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id is required"})
		return
	}

	if _, err := uuid.Parse(req.UserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user_id"})
		return
	}

	if req.UserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot create a DM with yourself"})
		return
	}

	// Verify target user exists
	var exists bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, req.UserID).Scan(&exists); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotFound})
		return
	}

	if h.enforceDMPrivacy(c, userID, req.UserID) {
		return
	}

	// Check for existing 1:1 conversation
	if h.returnExistingConversation(c, userID, req.UserID) {
		return
	}

	convID, err := h.createOneOnOneConversation(userID, req.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedOpenConversation})
		return
	}

	h.log.Info("DM conversation created", "conversation_id", convID, "user_id", userID, "target", req.UserID)
	h.notifyDMCreated(convID, req.UserID)

	conv := h.fetchConversationResponse(convID, userID)
	c.JSON(http.StatusCreated, gin.H{"conversation": conv})
}

// enforceDMPrivacy checks privacy settings and returns true if the request was blocked.
func (h *Handler) enforceDMPrivacy(c *gin.Context, senderID, targetID string) bool {
	var areFriends bool
	err := h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM friendships
			WHERE ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))
			  AND status = 'accepted'
		)
	`, senderID, targetID).Scan(&areFriends)
	if err != nil {
		h.log.Error("Failed to check friendship", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedOpenConversation})
		return true
	}

	dmPrivacyLevel, dmFriendsOfFriends, err := h.fetchDMPrivacySettings(targetID)
	if err != nil {
		h.log.Error("Failed to check privacy settings", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedOpenConversation})
		return true
	}

	// Level 0 ("No One") means nobody can DM the target — not even accepted
	// friends. Enforce it ahead of the friend short-circuit so the product's
	// "Nobody can DM you" contract holds on every DM entry point, including
	// group DM creation (CV-CAN-029).
	if dmPrivacyLevel == dmPrivacyOff {
		c.JSON(http.StatusForbidden, gin.H{"error": "dm_disabled"})
		return true
	}

	if areFriends {
		return false
	}

	if dmPrivacyLevel >= dmPrivacyOpenToAll {
		return false // Open to all
	}

	allowed, err := h.isDMAllowedByRelationship(senderID, targetID, dmPrivacyLevel, dmFriendsOfFriends)
	if err != nil {
		h.log.Error("Failed to check DM relationship", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedOpenConversation})
		return true
	}
	if allowed {
		return false
	}

	c.JSON(http.StatusForbidden, gin.H{"error": "privacy_blocked"})
	return true
}

// fetchDMPrivacySettings fetches the DM privacy level and friends-of-friends flag for a user.
func (h *Handler) fetchDMPrivacySettings(userID string) (int, bool, error) {
	var dmPrivacyLevel int
	var dmFriendsOfFriends bool
	err := h.db.QueryRow(`
		SELECT dm_privacy_level, dm_friends_of_friends
		FROM privacy_settings WHERE user_id = $1
	`, userID).Scan(&dmPrivacyLevel, &dmFriendsOfFriends)
	if err == sql.ErrNoRows {
		return 2, false, nil // Default: friends + server members
	}
	return dmPrivacyLevel, dmFriendsOfFriends, err
}

// isDMAllowedByRelationship checks if DM is allowed based on friends-of-friends
// or shared-server relationship. The direct-friend case is already handled
// upstream by enforceDMPrivacy at the call site, so this function covers only
// the remaining paths. Returns (allowed, error) — callers MUST propagate the
// error per [internal]rules/backend.md errcheck discipline.
func (h *Handler) isDMAllowedByRelationship(senderID, targetID string, privacyLevel int, fofEnabled bool) (bool, error) {
	if fofEnabled {
		var isFoF bool
		err := h.db.QueryRow(`
			WITH sender_friends AS (
				SELECT CASE WHEN requester_id = $1 THEN addressee_id ELSE requester_id END AS friend_id
				FROM friendships
				WHERE (requester_id = $1 OR addressee_id = $1) AND status = 'accepted'
			),
			target_friends AS (
				SELECT CASE WHEN requester_id = $2 THEN addressee_id ELSE requester_id END AS friend_id
				FROM friendships
				WHERE (requester_id = $2 OR addressee_id = $2) AND status = 'accepted'
			)
			SELECT EXISTS (
				SELECT 1 FROM sender_friends sf
				JOIN target_friends tf ON sf.friend_id = tf.friend_id
				WHERE sf.friend_id != $1 AND sf.friend_id != $2
			)
		`, senderID, targetID).Scan(&isFoF)
		if err != nil {
			return false, fmt.Errorf("check friends-of-friends: %w", err)
		}
		if isFoF {
			return true, nil
		}
	}

	if privacyLevel >= dmPrivacyFriendsAndServer {
		var sharesServer bool
		err := h.db.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM server_members sm1
				JOIN server_members sm2 ON sm1.server_id = sm2.server_id
				WHERE sm1.user_id = $1 AND sm2.user_id = $2
			)
		`, senderID, targetID).Scan(&sharesServer)
		if err != nil {
			return false, fmt.Errorf("check shared server: %w", err)
		}
		return sharesServer, nil
	}

	return false, nil
}

// returnExistingConversation checks for an existing 1:1 conversation and returns it if found.
// Returns true if the response was written (existing conversation found or DB error).
func (h *Handler) returnExistingConversation(c *gin.Context, userID, targetUserID string) bool {
	var existingID string
	err := h.db.QueryRow(`
		SELECT dc.id FROM dm_conversations dc
		JOIN dm_participants p1 ON p1.conversation_id = dc.id AND p1.user_id = $1
		JOIN dm_participants p2 ON p2.conversation_id = dc.id AND p2.user_id = $2
		WHERE dc.is_group = FALSE
		LIMIT 1
	`, userID, targetUserID).Scan(&existingID)

	if err == nil {
		conv := h.fetchConversationResponse(existingID, userID)
		if conv != nil {
			c.JSON(http.StatusOK, gin.H{"conversation": conv})
			return true
		}
	} else if err != sql.ErrNoRows {
		h.log.Error("Failed to check existing DM", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedOpenConversation})
		return true
	}
	return false
}

// createOneOnOneConversation creates a new 1:1 DM conversation with both participants.
func (h *Handler) createOneOnOneConversation(userID, targetUserID string) (string, error) {
	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error(errMsgFailedStartTransaction, "error", err)
		return "", err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error(errMsgFailedRollbackTransaction, "error", rbErr)
		}
	}()

	convID := uuid.New().String()
	if _, err = tx.Exec(`INSERT INTO dm_conversations (id, is_group, created_by) VALUES ($1, FALSE, $2)`, convID, userID); err != nil {
		h.log.Error("Failed to create DM conversation", "error", err)
		return "", err
	}
	if _, err = tx.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, userID); err != nil {
		h.log.Error("Failed to add DM participant", "error", err)
		return "", err
	}
	if _, err = tx.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, targetUserID); err != nil {
		h.log.Error("Failed to add DM participant", "error", err)
		return "", err
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit DM creation", "error", err)
		return "", err
	}
	return convID, nil
}

// notifyDMCreated sends a dm_conversation_created WebSocket event to the target user.
func (h *Handler) notifyDMCreated(convID, targetUserID string) {
	if h.hub == nil {
		return
	}
	targetUUID, parseErr := uuid.Parse(targetUserID)
	if parseErr != nil {
		return
	}
	conv := h.fetchConversationResponse(convID, targetUserID)
	if conv == nil {
		return
	}
	h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
		Type: "dm_conversation_created",
		Data: map[string]interface{}{"conversation": conv},
	})
}

// CreateGroupRequest represents a request to create a group DM.
type CreateGroupRequest struct {
	UserIDs []string `json:"user_ids" binding:"required"`
	Name    *string  `json:"name"`
}

// CreateGroup creates a group DM conversation.
// POST /dm/conversations/group
func (h *Handler) CreateGroup(c *gin.Context) {
	userID := c.GetString("user_id")

	var req CreateGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_ids is required"})
		return
	}

	if errMsg := validateGroupMembers(req.UserIDs, userID); errMsg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsg})
		return
	}

	// Group DM creation must honor each target's DM privacy settings — otherwise a
	// caller can pull a user who has DMs disabled (or restricted to friends/FoF/
	// shared-server) into a group DM, bypassing the 1:1 privacy boundary that
	// OpenConversation and AddMember already enforce (CV-CAN-029). Reuse the shared
	// existence check + enforceDMPrivacy gate per target.
	for _, targetID := range req.UserIDs {
		var exists bool
		if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, targetID).Scan(&exists); err != nil {
			h.log.Error("Failed to verify target user exists", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateGroup})
			return
		} else if !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotFound})
			return
		}
		if h.enforceDMPrivacy(c, userID, targetID) {
			return // enforceDMPrivacy already wrote the 403/500 response
		}
	}

	allUserIDs := append([]string{userID}, req.UserIDs...)

	convID, err := h.insertGroupConversation(req.Name, userID, allUserIDs)
	if err != nil {
		h.log.Error("Failed to create group DM", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateGroup})
		return
	}

	h.log.Info("Group DM created", "conversation_id", convID, "user_id", userID, "members", len(allUserIDs))
	h.notifyGroupCreated(convID, req.UserIDs)

	conv := h.fetchConversationResponse(convID, userID)
	c.JSON(http.StatusCreated, gin.H{"conversation": conv})
}

func validateGroupMembers(userIDs []string, callerID string) string {
	if len(userIDs) < 1 {
		return "At least one other user is required"
	}
	if len(userIDs) > 9 {
		return "Group DMs are limited to 10 participants"
	}
	for _, uid := range userIDs {
		if _, err := uuid.Parse(uid); err != nil {
			return "Invalid user ID: " + uid
		}
		if uid == callerID {
			return "Cannot include yourself in user_ids"
		}
	}
	return ""
}

func (h *Handler) insertGroupConversation(name *string, creatorID string, allUserIDs []string) (string, error) {
	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error(errMsgFailedStartTransaction, "error", err)
		return "", err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error(errMsgFailedRollbackTransaction, "error", rbErr)
		}
	}()

	convID := uuid.New().String()
	if _, err = tx.Exec(`
		INSERT INTO dm_conversations (id, is_group, name, created_by)
		VALUES ($1, TRUE, $2, $3)
	`, convID, name, creatorID); err != nil {
		return "", err
	}

	if err := addGroupParticipants(tx, convID, creatorID, allUserIDs); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit group DM creation", "error", err)
		return "", err
	}
	return convID, nil
}

func addGroupParticipants(tx *sql.Tx, convID, creatorID string, allUserIDs []string) error {
	for _, uid := range allUserIDs {
		role := "member"
		if uid == creatorID {
			role = "admin"
		}
		if _, err := tx.Exec(`INSERT INTO dm_participants (conversation_id, user_id, role) VALUES ($1, $2, $3)`, convID, uid, role); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) notifyGroupCreated(convID string, memberIDs []string) {
	if h.hub == nil {
		return
	}
	for _, uid := range memberIDs {
		targetUUID, parseErr := uuid.Parse(uid)
		if parseErr != nil {
			continue
		}
		conv := h.fetchConversationResponse(convID, uid)
		if conv == nil {
			continue
		}
		h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
			Type: "dm_conversation_created",
			Data: map[string]interface{}{
				"conversation": conv,
			},
		})
	}
}

// GetConversation returns a single conversation with participants.
// GET /dm/conversations/:id
func (h *Handler) GetConversation(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}

	// Verify participation
	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}

	conv := h.fetchConversationResponse(convID, userID)
	if conv == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Conversation not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"conversation": conv})
}

// UpdateConversationRequest represents an update to a group DM.
type UpdateConversationRequest struct {
	Name *string `json:"name"`
}

// AddMemberRequest represents a request to add a member to a group DM.
type AddMemberRequest struct {
	UserID string `json:"user_id" binding:"required"`
}

// UpdateConversation updates a group DM's name.
// PATCH /dm/conversations/:id
func (h *Handler) UpdateConversation(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}

	var req UpdateConversationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	// Verify participation, group status, and admin role
	var isGroup bool
	var role string
	err := h.db.QueryRow(`
		SELECT dc.is_group, dp.role FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1
	`, convID, userID).Scan(&isGroup, &role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	} else if err != nil {
		h.log.Error(errMsgFailedCheckConversation, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return
	}

	if !isGroup {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot rename a 1:1 conversation"})
		return
	}

	if role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only admins can update group settings"})
		return
	}

	_, err = h.db.Exec(`UPDATE dm_conversations SET name = $1, updated_at = NOW() WHERE id = $2`, req.Name, convID)
	if err != nil {
		h.log.Error("Failed to update conversation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateConversation})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Conversation updated"})
}

// --- Member Management Endpoints ---

// AddMember adds a user to a group DM conversation.
// POST /dm/conversations/:id/members
func (h *Handler) AddMember(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}

	var req AddMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	if _, err := uuid.Parse(req.UserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user_id"})
		return
	}

	// Verify caller is participant, group status, and admin role
	isGroup, role, err := h.getParticipantGroupRole(convID, userID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	} else if err != nil {
		h.log.Error(errMsgFailedCheckConversation, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAddMember})
		return
	}

	if !isGroup {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgNotGroupConversation})
		return
	}
	if role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotAdmin})
		return
	}

	// Check member count limit
	var count int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM dm_participants WHERE conversation_id = $1`, convID).Scan(&count); err != nil {
		h.log.Error("Failed to count participants", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAddMember})
		return
	}
	if count >= 10 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Group DM cannot exceed 10 members"})
		return
	}

	// Check target user exists
	var exists bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, req.UserID).Scan(&exists); err != nil {
		h.log.Error("Failed to check user existence", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAddMember})
		return
	}
	if !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgUserNotFound})
		return
	}

	// Check target not already a participant
	var alreadyIn bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`, convID, req.UserID).Scan(&alreadyIn); err != nil {
		h.log.Error("Failed to check participant", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAddMember})
		return
	}
	if alreadyIn {
		c.JSON(http.StatusConflict, gin.H{"error": "User is already a participant"})
		return
	}

	// Enforce DM privacy (block check)
	if h.enforceDMPrivacy(c, userID, req.UserID) {
		return
	}

	// Begin transaction: insert participant + record key revocation
	maxVersion, err := h.addMemberTx(convID, req.UserID, userID)
	if err != nil {
		h.log.Error("Failed to add member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAddMember})
		return
	}

	// Broadcast events
	h.broadcastMemberAdded(convID, req.UserID, userID, maxVersion)

	conv := h.fetchConversationResponse(convID, userID)
	c.JSON(http.StatusOK, gin.H{"conversation": conv})
}

// addMemberTx inserts a new participant and records key revocation in a transaction.
// Returns the max key version before revocation.
func (h *Handler) addMemberTx(convID, targetUserID, callerUserID string) (int, error) {
	tx, err := h.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error(errMsgFailedRollbackTransaction, "error", rbErr)
		}
	}()

	var lockedConversationID string
	if err := tx.QueryRow(
		`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, convID,
	).Scan(&lockedConversationID); err != nil {
		return 0, err
	}

	if _, err := tx.Exec(`INSERT INTO dm_participants (conversation_id, user_id, role) VALUES ($1, $2, 'member')`, convID, targetUserID); err != nil {
		return 0, err
	}

	var maxVersion int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(key_version), 0) FROM dm_channel_keys WHERE conversation_id = $1`, convID).Scan(&maxVersion); err != nil {
		return 0, err
	}

	if maxVersion > 0 {
		if _, err := tx.Exec(`
			INSERT INTO dm_key_revocations (conversation_id, revoked_epoch, successor_epoch, reason, revoked_by)
			VALUES ($1, $2, $3, 'member_added', $4)
			ON CONFLICT (conversation_id, revoked_epoch) DO NOTHING
		`, convID, maxVersion, maxVersion+1, callerUserID); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return maxVersion, nil
}

// broadcastMemberAdded sends WebSocket events for a newly added group DM member.
func (h *Handler) broadcastMemberAdded(convID, targetUserID, callerUserID string, maxVersion int) {
	if h.hub == nil {
		return
	}

	// Notify existing participants
	h.broadcastToDMParticipants(convID, "", websocket.OutgoingMessage{
		Type: "dm_participant_added",
		Data: map[string]interface{}{
			"conversation_id": convID,
			"user_id":         targetUserID,
			"added_by":        callerUserID,
		},
	})

	// Notify the new member with the full conversation
	if targetUUID, parseErr := uuid.Parse(targetUserID); parseErr == nil {
		conv := h.fetchConversationResponse(convID, targetUserID)
		if conv != nil {
			h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
				Type: "dm_conversation_created",
				Data: map[string]interface{}{
					"conversation": conv,
				},
			})
		}
	}

	// Notify all participants of key rotation
	if maxVersion > 0 {
		h.broadcastToDMParticipants(convID, "", websocket.OutgoingMessage{
			Type: "key_rotation",
			Data: map[string]interface{}{
				"channel_id":      convID,
				"triggered_by":    callerUserID,
				"new_key_version": maxVersion + 1,
			},
		})
	}
}

// RemoveMember removes a user from a group DM (admin action or self-leave).
// DELETE /dm/conversations/:id/members/:userId
func (h *Handler) RemoveMember(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")
	targetUserID := c.Param("userId")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return
	}

	// Verify caller is participant and get conversation info
	var isGroup bool
	var createdBy, role string
	err := h.db.QueryRow(`
		SELECT dc.is_group, dc.created_by, dp.role FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1
	`, convID, userID).Scan(&isGroup, &createdBy, &role)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	} else if err != nil {
		h.log.Error(errMsgFailedCheckConversation, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRemoveMember})
		return
	}

	if !isGroup {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgNotGroupConversation})
		return
	}

	isSelfLeave := targetUserID == userID

	// Permission check: only admins can remove others
	if !isSelfLeave && role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotAdmin})
		return
	}

	// If not self-leave, verify target is a participant
	if !isSelfLeave {
		var targetExists bool
		if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`, convID, targetUserID).Scan(&targetExists); err != nil {
			h.log.Error("Failed to check target participant", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRemoveMember})
			return
		}
		if !targetExists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Target user is not a participant"})
			return
		}
	}

	// Cannot remove the creator unless it's self-leave
	if targetUserID == createdBy && !isSelfLeave {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot remove the group creator"})
		return
	}

	// Execute removal in transaction
	newCreatorID, maxVersion, err := h.removeMemberTx(convID, targetUserID, createdBy, userID)
	if err != nil {
		h.log.Error("Failed to remove member", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRemoveMember})
		return
	}

	// Broadcast events
	h.broadcastMemberRemoved(convID, targetUserID, userID, isSelfLeave, newCreatorID, maxVersion)

	c.JSON(http.StatusOK, gin.H{"message": "Member removed"})
}

// removeMemberTx removes a participant and handles creator transfer in a transaction.
// Returns the new creator ID (empty if no transfer) and the max key version.
func (h *Handler) removeMemberTx(convID, targetUserID, createdBy, callerUserID string) (string, int, error) {
	tx, err := h.db.Begin()
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error(errMsgFailedRollbackTransaction, "error", rbErr)
		}
	}()

	var lockedConversationID string
	if err := tx.QueryRow(
		`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, convID,
	).Scan(&lockedConversationID); err != nil {
		return "", 0, err
	}

	var newCreatorID string

	// If creator is leaving, transfer ownership
	if targetUserID == createdBy {
		newCreatorID, err = h.transferCreator(tx, convID, targetUserID)
		if err != nil {
			return "", 0, err
		}
	}

	// Remove the participant
	if _, err := tx.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, convID, targetUserID); err != nil {
		return "", 0, err
	}

	// Record key revocation
	var maxVersion int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(key_version), 0) FROM dm_channel_keys WHERE conversation_id = $1`, convID).Scan(&maxVersion); err != nil {
		return "", 0, err
	}

	if maxVersion > 0 {
		if _, err := tx.Exec(`
			INSERT INTO dm_key_revocations (conversation_id, revoked_epoch, successor_epoch, reason, revoked_by)
			VALUES ($1, $2, $3, 'member_removed', $4)
			ON CONFLICT (conversation_id, revoked_epoch) DO NOTHING
		`, convID, maxVersion, maxVersion+1, callerUserID); err != nil {
			return "", 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	return newCreatorID, maxVersion, nil
}

// transferCreator finds a successor and transfers group ownership within the given transaction.
func (h *Handler) transferCreator(tx *sql.Tx, convID, leavingUserID string) (string, error) {
	var newCreatorID string

	// Try to find another admin first
	err := tx.QueryRow(`
		SELECT user_id FROM dm_participants
		WHERE conversation_id = $1 AND role = 'admin' AND user_id != $2
		ORDER BY joined_at ASC LIMIT 1
	`, convID, leavingUserID).Scan(&newCreatorID)

	if err == sql.ErrNoRows {
		// Fall back to longest-standing member
		err = tx.QueryRow(`
			SELECT user_id FROM dm_participants
			WHERE conversation_id = $1 AND user_id != $2
			ORDER BY joined_at ASC LIMIT 1
		`, convID, leavingUserID).Scan(&newCreatorID)
	}
	if err != nil {
		return "", err
	}

	if _, err := tx.Exec(`UPDATE dm_conversations SET created_by = $1 WHERE id = $2`, newCreatorID, convID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, convID, newCreatorID); err != nil {
		return "", err
	}

	return newCreatorID, nil
}

// broadcastMemberRemoved sends WebSocket events for a removed group DM member.
func (h *Handler) broadcastMemberRemoved(convID, targetUserID, callerUserID string, isSelfLeave bool, newCreatorID string, maxVersion int) {
	if h.hub == nil {
		return
	}

	removedEvent := websocket.OutgoingMessage{
		Type: "dm_participant_removed",
		Data: map[string]interface{}{
			"conversation_id": convID,
			"user_id":         targetUserID,
			"removed_by":      callerUserID,
			"was_self_leave":  isSelfLeave,
		},
	}
	// The target is no longer in dm_participants, so notify them directly before
	// broadcasting the same event to the remaining participants.
	if targetUUID, err := uuid.Parse(targetUserID); err == nil {
		h.hub.BroadcastToUser(targetUUID, removedEvent)
	}
	h.broadcastToDMParticipants(convID, "", removedEvent)

	// If admin was transferred, notify about role change
	if newCreatorID != "" {
		h.broadcastToDMParticipants(convID, "", websocket.OutgoingMessage{
			Type: "dm_role_changed",
			Data: map[string]interface{}{
				"conversation_id": convID,
				"user_id":         newCreatorID,
				"role":            "admin",
			},
		})
	}

	// Notify all participants of key rotation
	if maxVersion > 0 {
		h.broadcastToDMParticipants(convID, "", websocket.OutgoingMessage{
			Type: "key_rotation",
			Data: map[string]interface{}{
				"channel_id":      convID,
				"triggered_by":    callerUserID,
				"new_key_version": maxVersion + 1,
			},
		})
	}
}

// getParticipantGroupRole checks if a user is a participant and returns the group status and role.
func (h *Handler) getParticipantGroupRole(convID, userID string) (bool, string, error) {
	var isGroup bool
	var role string
	err := h.db.QueryRow(`
		SELECT dc.is_group, dp.role FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1
	`, convID, userID).Scan(&isGroup, &role)
	return isGroup, role, err
}

// --- Message Endpoints ---

// GetMessages returns paginated message history for a DM conversation.
// GET /dm/conversations/:id/messages
func (h *Handler) GetMessages(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}

	// Cursor-based pagination
	cursor := c.Query("before")
	limit := 50

	var rows *sql.Rows
	var err error
	if cursor != "" {
		if _, parseErr := uuid.Parse(cursor); parseErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid cursor"})
			return
		}
		// hiddenRangeFilter excludes messages this user purged-from-view (#1352
		// receiver-hide). The concatenated fragment is a compile-time constant;
		// all VALUES are parameterized.
		//nolint:gosec // G202: concatenated fragment is a compile-time constant; all values parameterized
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf
		rows, err = h.db.Query(`
			SELECT m.id, m.conversation_id, m.user_id, m.content, m.type, m.call_event_payload, COALESCE(m.key_version, 1),
			       m.edited_at, m.created_at,
			       u.username, u.display_name, u.avatar_url
			FROM dm_messages m
			INNER JOIN users u ON u.id = m.user_id
			WHERE m.conversation_id = $1
			  AND m.created_at < (SELECT created_at FROM dm_messages WHERE id = $2)
			`+hiddenRangeFilter(4)+`
			ORDER BY m.created_at DESC
			LIMIT $3
		`, convID, cursor, limit, userID)
	} else {
		//nolint:gosec // G202: concatenated fragment is a compile-time constant; all values parameterized
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf
		rows, err = h.db.Query(`
			SELECT m.id, m.conversation_id, m.user_id, m.content, m.type, m.call_event_payload, COALESCE(m.key_version, 1),
			       m.edited_at, m.created_at,
			       u.username, u.display_name, u.avatar_url
			FROM dm_messages m
			INNER JOIN users u ON u.id = m.user_id
			WHERE m.conversation_id = $1
			`+hiddenRangeFilter(3)+`
			ORDER BY m.created_at DESC
			LIMIT $2
		`, convID, limit, userID)
	}
	if err != nil {
		h.log.Error("Failed to query DM messages", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch messages"})
		return
	}
	defer func() { _ = rows.Close() }()

	type messageResponse = dmMessageResponse

	messages := []messageResponse{}
	for rows.Next() {
		var m messageResponse
		var callEventRaw []byte
		if err := rows.Scan(
			&m.ID, &m.ConversationID, &m.UserID, &m.Content, &m.Type, &callEventRaw, &m.KeyVersion,
			&m.EditedAt, &m.CreatedAt,
			&m.Username, &m.DisplayName, &m.AvatarURL,
		); err != nil {
			h.log.Error("Failed to scan DM message", "error", err)
			continue
		}
		if len(callEventRaw) > 0 {
			m.CallEventPayload = json.RawMessage(callEventRaw)
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating DM messages", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch messages"})
		return
	}

	// Enrich messages with reaction and attachment metadata (non-fatal on failure)
	h.enrichDMReactions(messages, userID)
	h.enrichDMAttachments(messages)

	c.JSON(http.StatusOK, gin.H{"messages": messages})
}

// MarkRead updates the caller's last_read_at for a DM conversation.
// POST /dm/conversations/:id/read
func (h *Handler) MarkRead(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}

	_, err := h.db.Exec(`
		INSERT INTO dm_read_states (user_id, conversation_id, last_read_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id, conversation_id) DO UPDATE SET last_read_at = NOW()
	`, userID, convID)
	if err != nil {
		h.log.Error("Failed to mark DM read", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to mark as read"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Marked as read"})
}

// --- E2EE Key Endpoints ---

// GetKeys returns the caller's wrapped key for a DM conversation.
// GET /dm/conversations/:id/keys
func (h *Handler) GetKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}

	// Existence check (all DMs are encrypted under E2EE-everywhere #201).
	var exists bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_conversations WHERE id = $1)`, convID).Scan(&exists); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch keys"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Conversation not found"})
		return
	}

	type keyResponse struct {
		ID             string `json:"id"`
		ConversationID string `json:"conversation_id"`
		UserID         string `json:"user_id"`
		WrappedKey     string `json:"wrapped_key"`
		KeyVersion     int    `json:"key_version"`
		CreatedAt      string `json:"created_at"`
	}

	var key keyResponse
	err := h.db.QueryRow(`
		SELECT id, conversation_id, user_id, wrapped_key, key_version, created_at
		FROM dm_channel_keys
		WHERE conversation_id = $1 AND user_id = $2
		ORDER BY key_version DESC LIMIT 1
	`, convID, userID).Scan(&key.ID, &key.ConversationID, &key.UserID, &key.WrappedKey, &key.KeyVersion, &key.CreatedAt)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "No encryption key available yet", "pending": true})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch DM key", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch keys"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"key": key})
}

// DistributeKeysRequest represents wrapped keys for DM participants.
type DistributeKeysRequest struct {
	WrappedKeys map[string]string `json:"wrapped_keys" binding:"required"`
	KeyVersion  *int              `json:"key_version,omitempty"`
}

// DistributeKeys stores wrapped channel keys for DM participants.
// POST /dm/conversations/:id/keys
func (h *Handler) DistributeKeys(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}

	var req DistributeKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}

	// Determine key version.
	//
	// CRITICAL: the fallback MUST be MAX (not MAX+1). Peer-fulfillment wraps
	// the EXISTING cached CSK, so the inserted row belongs at the existing
	// epoch — tagging it at MAX+1 would tie the wrap to a version no
	// historical message references, breaking decryption of history for
	// the recovering/onboarding user. Rotation paths pass an explicit
	// key_version. New conversations default to version 1. See #1023 /
	// PR #1080.
	keyVersion := 1
	if req.KeyVersion != nil && *req.KeyVersion > 0 {
		keyVersion = *req.KeyVersion
	} else {
		err := h.db.QueryRow(`SELECT COALESCE(MAX(key_version), 1) FROM dm_channel_keys WHERE conversation_id = $1`, convID).Scan(&keyVersion)
		if err != nil {
			h.log.Error("Failed to fetch DM key version", "error", err, "conversation_id", convID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDistributeKeys})
			return
		}
		if keyVersion == 0 {
			keyVersion = 1
		}
	}

	distributed := 0
	for memberUserID, wrappedKey := range req.WrappedKeys {
		if h.distributeKeyToMember(convID, memberUserID, wrappedKey, keyVersion) {
			distributed++
		}
	}

	c.JSON(http.StatusOK, gin.H{"distributed": distributed})
}

func (h *Handler) distributeKeyToMember(convID, memberUserID, wrappedKey string, keyVersion int) bool {
	if _, parseErr := uuid.Parse(memberUserID); parseErr != nil {
		return false
	}

	result, err := h.db.Exec(`
		INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (conversation_id, user_id, key_version) DO NOTHING
	`, convID, memberUserID, wrappedKey, keyVersion)
	if err != nil {
		h.log.Error("Failed to store DM key", "error", err, "user_id", memberUserID)
		return false
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		h.log.Error("Failed to read RowsAffected after key insert", "error", err, "conversation_id", convID, "user_id", memberUserID)
		return false
	}
	if rowsAffected == 0 {
		return false
	}

	if _, err := h.db.Exec(`DELETE FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`, convID, memberUserID); err != nil {
		// Cleanup is best-effort: the key was already inserted into dm_channel_keys above.
		h.log.Warn("Failed to clear pending key request after distribution", "error", err, "conversation_id", convID, "user_id", memberUserID)
	}

	if h.hub != nil {
		if recipientUUID, parseErr := uuid.Parse(memberUserID); parseErr == nil {
			h.hub.BroadcastToUser(recipientUUID, websocket.OutgoingMessage{
				Type: "key_delivered",
				Data: map[string]interface{}{
					"channel_id": convID,
					"user_id":    memberUserID,
				},
			})
		}
	}
	return true
}

// RotateKey handles manual seal & rotate for DM forward secrecy.
// POST /dm/conversations/:id/rotate-key
func (h *Handler) RotateKey(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}

	// Per-resource rate limit: 10 rotations per 24h per conversation.
	// Subscription-tiered limits deferred to issue #603.
	rateLimitKey := fmt.Sprintf("ratelimit:dm_rotate:%s", convID)
	if blocked, retryAfter := middleware.IsRateLimited(c.Request.Context(), h.redis, rateLimitKey, 10, 24*time.Hour); blocked {
		middleware.RespondRateLimited(c, retryAfter, 10)
		return
	}

	// Get current max key version
	var maxVersion int
	err := h.db.QueryRow(`SELECT COALESCE(MAX(key_version), 0) FROM dm_channel_keys WHERE conversation_id = $1`, convID).Scan(&maxVersion)
	if err != nil {
		h.log.Error("Failed to get max key version", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rotate key"})
		return
	}

	h.log.Info("DM key rotation requested", "conversation_id", convID, "user_id", userID, "current_version", maxVersion)

	// Record the revocation of the current epoch
	if maxVersion > 0 {
		if _, err := h.db.Exec(`
			INSERT INTO dm_key_revocations (conversation_id, revoked_epoch, successor_epoch, reason, revoked_by)
			VALUES ($1, $2, $3, 'manual_rotation', $4)
			ON CONFLICT (conversation_id, revoked_epoch) DO NOTHING
		`, convID, maxVersion, maxVersion+1, userID); err != nil {
			h.log.Error("Failed to record DM key revocation", "error", err, "conversation_id", convID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rotate key"})
			return
		}
	}

	// Notify all participants to rotate
	if h.hub != nil {
		h.broadcastToDMParticipants(convID, userID, websocket.OutgoingMessage{
			Type: "key_rotation",
			Data: map[string]interface{}{
				"channel_id":      convID,
				"triggered_by":    userID,
				"new_key_version": maxVersion + 1,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"message":         "Key rotation initiated",
		"new_key_version": maxVersion + 1,
	})
}

// --- Voice Endpoints ---

type voiceJoinState struct {
	isGroup        bool
	serverMuted    bool
	serverDeafened bool
	callerRole     string
}

func parseDMVoiceIDs(c *gin.Context, convID, userID string) (uuid.UUID, uuid.UUID, bool) {
	convUUID, err := uuid.Parse(convID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return uuid.Nil, uuid.Nil, false
	}
	userUUID, err := uuid.Parse(userID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidCallerID})
		return uuid.Nil, uuid.Nil, false
	}
	return convUUID, userUUID, true
}

func (h *Handler) loadVoiceJoinState(c *gin.Context, convID, userID string) (voiceJoinState, bool) {
	var state voiceJoinState
	err := h.db.QueryRow(`
		SELECT dc.is_group, dp.server_muted, dp.server_deafened, dp.role
		FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1
	`, convID, userID).Scan(&state.isGroup, &state.serverMuted, &state.serverDeafened, &state.callerRole)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return voiceJoinState{}, false
	}
	if err != nil {
		h.log.Error(errMsgFailedCheckParticipation, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return voiceJoinState{}, false
	}
	return state, true
}

func (h *Handler) promotePendingDMVoiceCall(ctx context.Context, convUUID uuid.UUID, ring *PendingCall) error {
	if err := RefreshDMVoiceCallLease(ctx, h.redis, VoiceCallLease{
		ConversationID: convUUID,
		CallID:         ring.RingID,
		RingID:         ring.RingID,
		CallerUserID:   ring.CallerUserID,
	}, acceptedDMCallCorrelationTTL, true); err != nil {
		return err
	}
	rememberAcceptedDMCall(ring, acceptedDMCallCorrelationTTL)
	h.hub.BroadcastToDMParticipants(convUUID, websocket.OutgoingMessage{
		Type: "dm_voice_call_canceled",
		Data: map[string]interface{}{
			"conversation_id": convUUID.String(),
			"ring_id":         ring.RingID.String(),
			"canceled_by":     "someone_accepted",
		},
	})
	return nil
}

func (h *Handler) acceptPendingDMVoiceCall(
	c *gin.Context,
	convUUID, userUUID, requestedRingID uuid.UUID,
) (uuid.UUID, bool) {
	storedAny, loaded := pendingDMCalls.Load(convUUID)
	if !loaded {
		return uuid.Nil, true
	}
	ring, ok := storedAny.(*PendingCall)
	if !ok || ring.CallerUserID == userUUID {
		return uuid.Nil, true
	}
	if requestedRingID != uuid.Nil && requestedRingID != ring.RingID {
		c.JSON(http.StatusConflict, gin.H{"error": errMsgVoiceCallRingChanged})
		return uuid.Nil, false
	}
	accepted, err := ring.tryAccept(userUUID, func() error {
		return h.promotePendingDMVoiceCall(c.Request.Context(), convUUID, ring)
	})
	if err != nil {
		h.log.Error("Failed to establish accepted DM call lease", "error", err,
			"conversation_id", sanitizeLogValue(c.Param("id")), "ring_id", ring.RingID.String())
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return uuid.Nil, false
	}
	if !accepted {
		return uuid.Nil, true
	}
	ring.finalizeTerminal()
	return ring.RingID, true
}

func (h *Handler) resolveVoiceJoinCallID(
	c *gin.Context,
	convUUID, userUUID, requestedRingID uuid.UUID,
) (uuid.UUID, bool) {
	callID, ok := h.acceptPendingDMVoiceCall(c, convUUID, userUUID, requestedRingID)
	if !ok || callID != uuid.Nil {
		return callID, ok
	}
	if requestedRingID == uuid.Nil {
		return h.resolveDirectVoiceJoinCallID(c, convUUID, userUUID)
	}

	lease, found, err := LookupDMVoiceCallLease(c.Request.Context(), h.redis, convUUID)
	if err != nil {
		h.log.Error("Failed to lookup accepted DM call lease", "error", err,
			"conversation_id", sanitizeLogValue(c.Param("id")))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return uuid.Nil, false
	}
	if found && lease.CallID == requestedRingID && lease.RingID == requestedRingID {
		return requestedRingID, true
	}
	c.JSON(http.StatusConflict, gin.H{"error": "Voice call ring is no longer active"})
	return uuid.Nil, false
}

func (h *Handler) resolveDirectVoiceJoinCallID(
	c *gin.Context,
	convUUID, userUUID uuid.UUID,
) (uuid.UUID, bool) {
	// The ring caller may probe /voice/join while their invitation is still
	// pending. Do not turn that unresolved ring into a competing direct call.
	if _, ringing := pendingDMCalls.Load(convUUID); ringing {
		return uuid.Nil, true
	}

	lease, found, err := LookupDMVoiceCallLease(c.Request.Context(), h.redis, convUUID)
	if err != nil {
		h.log.Error("Failed to lookup direct DM voice call lease", "error", err,
			"conversation_id", sanitizeLogValue(c.Param("id")))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return uuid.Nil, false
	}
	if found {
		return lease.CallID, true
	}

	// Bind the short direct-call reservation to the renderer-facing join
	// flow. The media-plane authorize endpoint may verify this ID, but cannot
	// create a lease merely because a member called it directly.
	lease = VoiceCallLease{
		ConversationID: convUUID,
		CallID:         uuid.New(),
		CallerUserID:   userUUID,
	}
	if err := RefreshDMVoiceCallLease(
		c.Request.Context(), h.redis, lease, DMVoiceCallReservationTTL, true,
	); err != nil {
		if errors.Is(err, ErrDMVoiceCallLeaseConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "Voice call already active"})
			return uuid.Nil, false
		}
		h.log.Error("Failed to reserve direct DM voice call", "error", err,
			"conversation_id", sanitizeLogValue(c.Param("id")))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return uuid.Nil, false
	}
	return lease.CallID, true
}

// AuthorizeVoiceJoin checks that a user can join a DM voice call.
// POST /dm/conversations/:id/voice/join
func (h *Handler) AuthorizeVoiceJoin(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	convUUID, userUUID, validIDs := parseDMVoiceIDs(c, convID, userID)
	if !validIDs {
		return
	}
	requestedRingID, validRequest := parseOptionalVoiceRingID(c, errMsgInvalidVoiceJoinRequest)
	if !validRequest {
		return
	}

	// Verify participation and fetch enforcement state
	joinState, authorized := h.loadVoiceJoinState(c, convID, userID)
	if !authorized {
		return
	}

	// Accept-path coordination (Gitar #1231 finding G1, #1209 spec §6.1):
	// when a CALLEE (not the original ring caller) authorizes a DM voice join
	// while a pendingDMCall exists for this conversation, that's the signal
	// to consummate the ring — clear it, stop the timeout timer, and broadcast
	// dm_voice_call_canceled with canceled_by='someone_accepted' so the caller's
	// state machine transitions out of outgoing-ringing and calls joinChannel
	// itself (see handleCallCanceled in client/desktop/.../callStateMachine.ts).
	// Without this hop, the caller stalls in outgoing-ringing until the 45s
	// timeout fires even though the callee already joined the room.
	unlockLifecycle := LockDMCallLifecycle(convUUID)
	defer unlockLifecycle()
	callID, resolved := h.resolveVoiceJoinCallID(c, convUUID, userUUID, requestedRingID)
	if !resolved {
		return
	}
	if callID != uuid.Nil {
		if err := RememberDMVoiceJoinAdmission(
			c.Request.Context(), h.redis, convUUID, userUUID, callID, DMVoiceCallReservationTTL,
		); err != nil {
			if errors.Is(err, ErrDMVoiceCallLeaseConflict) {
				c.JSON(http.StatusConflict, gin.H{"error": errMsgVoiceCallNoLongerActive})
				return
			}
			h.log.Error("Failed to bind DM voice join admission", "error", err,
				"conversation_id", sanitizeLogValue(convID))
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
			return
		}
	}

	// Resolve the joining user's media entitlements server-side from the
	// AUTHENTICATED user_id (never a client value). Per-user enforcement is
	// room-kind-independent, so DM voice joins carry the same media_entitlements
	// object as server-channel joins. GetTier fails closed to free, and MediaFor →
	// For("") returns the free caps for an unknown/empty tier — a resolution
	// problem degrades to the free floor, never blocks the join, never grants
	// premium (#1300 §3/§5).
	mediaEnt := entitlements.MediaFor(h.entCache.GetTier(c.Request.Context(), userID))

	// NOTE (#1542): DM voice/join intentionally omits room_owner_tier. DMs have no
	// owner; the media-plane resolves the per-room cap from the MAX tier among present
	// participants (each Participant.tier arrives via the per-user media_entitlements).

	h.log.Info("DM voice join authorized",
		"user_id", sanitizeLogValue(userID),
		"conversation_id", sanitizeLogValue(convID),
		"media_tier", mediaEnt.Tier)

	response := gin.H{
		"allowed":            true,
		"media_server_url":   h.cfg.MediaPlaneURL,
		"ice_servers":        h.cfg.ICEServers(userID),
		"server_muted":       joinState.serverMuted,
		"server_deafened":    joinState.serverDeafened,
		"media_entitlements": mediaEnt,
		"conversation": gin.H{
			"id":          convID,
			"is_group":    joinState.isGroup,
			"caller_role": joinState.callerRole,
		},
	}
	if callID != uuid.Nil {
		response["call_id"] = callID.String()
	}
	c.JSON(http.StatusOK, response)
}

// GetVoiceParticipants returns users currently in a DM voice call.
// GET /dm/conversations/:id/voice/participants
func (h *Handler) GetVoiceParticipants(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}

	rows, err := h.db.Query(`
		SELECT vp.user_id, u.username, COALESCE(u.display_name, ''), COALESCE(u.avatar_url, ''),
		       vp.is_muted, vp.is_deafened, vp.is_video_on, vp.is_screen_sharing, vp.joined_at
		FROM dm_voice_participants vp
		INNER JOIN users u ON u.id = vp.user_id
		WHERE vp.conversation_id = $1
		ORDER BY vp.joined_at ASC
	`, convID)
	if err != nil {
		h.log.Error("Failed to query DM voice participants", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch participants"})
		return
	}
	defer func() { _ = rows.Close() }()

	type voiceParticipant struct {
		UserID          string `json:"user_id"`
		Username        string `json:"username"`
		DisplayName     string `json:"display_name,omitempty"`
		AvatarURL       string `json:"avatar_url,omitempty"`
		IsMuted         bool   `json:"is_muted"`
		IsDeafened      bool   `json:"is_deafened"`
		IsVideoOn       bool   `json:"is_video_on"`
		IsScreenSharing bool   `json:"is_screen_sharing"`
		JoinedAt        string `json:"joined_at"`
	}

	participants := []voiceParticipant{}
	for rows.Next() {
		var p voiceParticipant
		if err := rows.Scan(
			&p.UserID, &p.Username, &p.DisplayName, &p.AvatarURL,
			&p.IsMuted, &p.IsDeafened, &p.IsVideoOn, &p.IsScreenSharing, &p.JoinedAt,
		); err != nil {
			h.log.Error("Failed to scan DM voice participant", "error", err)
			continue
		}
		participants = append(participants, p)
	}

	c.JSON(http.StatusOK, gin.H{"participants": participants})
}

// --- Personal Thread ---

// GetOrCreatePersonalThread returns the user's personal thread (self-DM), creating it if needed.
// POST /dm/conversations/personal
func (h *Handler) GetOrCreatePersonalThread(c *gin.Context) {
	userID := c.GetString("user_id")

	// Check for existing personal thread
	var existingID string
	err := h.db.QueryRow(`
		SELECT dc.id FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $1
		WHERE dc.is_personal = TRUE
	`, userID).Scan(&existingID)

	if err == nil {
		conv := h.fetchConversationResponse(existingID, userID)
		if conv != nil {
			c.JSON(http.StatusOK, gin.H{"conversation": conv})
			return
		}
	} else if err != sql.ErrNoRows {
		h.log.Error("Failed to check personal thread", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get personal thread"})
		return
	}

	// Create personal thread
	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error(errMsgFailedStartTransaction, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreatePersonalThread})
		return
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error(errMsgFailedRollbackTransaction, "error", rbErr)
		}
	}()

	convID := uuid.New().String()
	name := "Personal Thread"
	_, err = tx.Exec(`
		INSERT INTO dm_conversations (id, is_group, is_personal, name, created_by)
		VALUES ($1, FALSE, TRUE, $2, $3)
	`, convID, name, userID)
	if err != nil {
		h.log.Error("Failed to create personal thread", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreatePersonalThread})
		return
	}

	_, err = tx.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, convID, userID)
	if err != nil {
		h.log.Error("Failed to add personal thread participant", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreatePersonalThread})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit personal thread", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreatePersonalThread})
		return
	}

	h.log.Info("Personal thread created", "conversation_id", convID, "user_id", userID)

	conv := h.fetchConversationResponse(convID, userID)
	c.JSON(http.StatusCreated, gin.H{"conversation": conv})
}

// --- Helper methods ---

// isParticipant checks if a user is a participant of a DM conversation.
func (h *Handler) isParticipant(convID, userID string) bool {
	var exists bool
	err := h.db.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)
	`, convID, userID).Scan(&exists)
	return err == nil && exists
}

// fetchConversationResponse builds a full conversation response with participants.
// viewerID scopes the last-message preview through the receiver-hide filter (#1352):
// a message the viewer purged-from-view must not resurface as the preview. Call
// sites pass the acting user; recipients of any broadcast copy re-fetch through
// their own filtered reads.
// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf
func (h *Handler) fetchConversationResponse(convID, viewerID string) *conversationResponse {
	var conv conversationResponse
	var lmContent, lmCreatedAt, lmUserID, lmType sql.NullString
	var lmCallEventPayload []byte
	err := h.db.QueryRow(`
		SELECT dc.id, dc.is_group, dc.is_personal, dc.name, dc.icon_url, dc.created_by, dc.created_at,
		       dm.content, dm.created_at, dm.user_id, dm.type, dm.call_event_payload
		FROM dm_conversations dc
		LEFT JOIN LATERAL (
		    SELECT m.content, m.created_at, m.user_id, m.type, m.call_event_payload FROM dm_messages m
		    WHERE m.conversation_id = dc.id `+hiddenRangeFilter(2)+`
		    ORDER BY m.created_at DESC LIMIT 1
		) dm ON TRUE
		WHERE dc.id = $1
	`, convID, viewerID).Scan(
		&conv.ID, &conv.IsGroup, &conv.IsPersonal, &conv.Name, &conv.IconURL, &conv.CreatedBy, &conv.CreatedAt,
		&lmContent, &lmCreatedAt, &lmUserID, &lmType, &lmCallEventPayload,
	)
	if err != nil {
		return nil
	}
	if lmContent.Valid {
		conv.LastMessage = &lastMessageResponse{
			Content:          lmContent.String,
			UserID:           lmUserID.String,
			CreatedAt:        lmCreatedAt.String,
			Type:             lmType.String,
			CallEventPayload: json.RawMessage(lmCallEventPayload),
		}
	}

	// Fetch participants
	pRows, err := h.db.Query(`
		SELECT u.id, u.username, u.display_name, u.avatar_url, u.color_scheme, dp.role
		FROM dm_participants dp
		INNER JOIN users u ON u.id = dp.user_id
		WHERE dp.conversation_id = $1
	`, convID)
	if err != nil {
		return nil
	}
	defer func() { _ = pRows.Close() }()

	conv.Participants = []participantResponse{}
	for pRows.Next() {
		var p participantResponse
		if err := pRows.Scan(&p.UserID, &p.Username, &p.DisplayName, &p.AvatarURL, &p.ColorScheme, &p.Role); err != nil {
			continue
		}
		conv.Participants = append(conv.Participants, p)
	}

	return &conv
}

// enforceDMMessageEpoch rejects revoked key versions and reports the latest
// successor recorded by the authoritative DM revocation ledger.
func (h *Handler) enforceDMMessageEpoch(c *gin.Context, q epochQueryRower, convID string, keyVersion int) bool {
	var epochRevoked bool
	var currentEpoch int
	if err := q.QueryRowContext(c.Request.Context(),
		`SELECT EXISTS(
			SELECT 1 FROM dm_key_revocations
			WHERE conversation_id = $1 AND revoked_epoch = $2
		), COALESCE(MAX(successor_epoch), 1)
		FROM dm_key_revocations
		WHERE conversation_id = $1`,
		convID, keyVersion,
	).Scan(&epochRevoked, &currentEpoch); err != nil {
		h.log.Error("Failed to check DM epoch revocation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify key epoch"})
		return false
	}
	if !epochRevoked {
		return true
	}

	c.JSON(http.StatusConflict, gin.H{
		"error":           "Key epoch has been revoked — re-encrypt with current epoch",
		"code":            "epoch_revoked",
		"current_epoch":   currentEpoch,
		"conversation_id": convID,
	})
	return false
}

type updateDMMessageRequest struct {
	// max=65536 = the single hard ciphertext ceiling (= the subscribed worst
	// case), flat for everyone; matches messages/hub.go. Boy-scout fix (#1298).
	Content    string `json:"content" binding:"required,max=65536"`
	KeyVersion int    `json:"key_version" binding:"required,min=1"`
}

type updateDMMessageResult struct {
	Content    string  `json:"content"`
	KeyVersion int     `json:"key_version"`
	EditedAt   *string `json:"edited_at"`
	CreatedAt  string  `json:"created_at"`
}

// authorizeDMMessageUpdate verifies that the message exists in the requested
// conversation and belongs to the caller. On failure it writes the response.
func (h *Handler) authorizeDMMessageUpdate(c *gin.Context, convID, messageID, userID string) bool {
	var authorID string
	if err := h.db.QueryRow(`SELECT user_id FROM dm_messages WHERE id = $1 AND conversation_id = $2`, messageID, convID).Scan(&authorID); err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return false
	} else if err != nil {
		h.log.Error("Failed to check DM message author", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return false
	}

	if authorID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only edit your own messages"})
		return false
	}

	return true
}

// updateDMMessageCiphertext serializes the epoch check and ciphertext update.
// On failure it writes the response and returns false.
func (h *Handler) updateDMMessageCiphertext(
	c *gin.Context,
	convID, messageID string,
	req updateDMMessageRequest,
) (updateDMMessageResult, bool) {
	// Serialize the ledger check and edit against every revocation insert.
	// READ COMMITTED gives the check below a fresh snapshot after a lock wait.
	tx, err := h.db.BeginTx(c.Request.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		h.log.Error("Failed to begin DM message update", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return updateDMMessageResult{}, false
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error(errMsgFailedRollbackTransaction, "error", rbErr)
		}
	}()

	var lockedConversationID string
	if err = tx.QueryRowContext(c.Request.Context(),
		`SELECT id FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, convID,
	).Scan(&lockedConversationID); err != nil {
		h.log.Error("Failed to lock DM epoch", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return updateDMMessageResult{}, false
	}
	if !h.enforceDMMessageEpoch(c, tx, convID, req.KeyVersion) {
		return updateDMMessageResult{}, false
	}

	// Update ciphertext and its matching epoch atomically. The server never decrypts content.
	var result updateDMMessageResult
	err = tx.QueryRowContext(c.Request.Context(), `
		UPDATE dm_messages
		SET content = $1, key_version = $2, edited_at = NOW(), updated_at = NOW()
		WHERE id = $3 AND conversation_id = $4
		  AND NOT EXISTS (
		      SELECT 1 FROM dm_key_revocations
		      WHERE conversation_id = $4 AND revoked_epoch = $2
		  )
		RETURNING COALESCE(key_version, 1), edited_at, created_at
	`, req.Content, req.KeyVersion, messageID, convID).Scan(&result.KeyVersion, &result.EditedAt, &result.CreatedAt)
	if err == sql.ErrNoRows {
		if !h.enforceDMMessageEpoch(c, tx, convID, req.KeyVersion) {
			return updateDMMessageResult{}, false
		}
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return updateDMMessageResult{}, false
	} else if err != nil {
		h.log.Error("Failed to update DM message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return updateDMMessageResult{}, false
	}
	result.Content = req.Content
	if err = tx.Commit(); err != nil {
		h.log.Error("Failed to commit DM message update", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateMessage})
		return updateDMMessageResult{}, false
	}

	return result, true
}

// resolveDMMessageRequest runs the shared Update/DeleteMessage prologue:
// validate the conversation and message ID params and require the caller to
// be a conversation participant. On failure the HTTP error has already been
// written and ok is false.
func (h *Handler) resolveDMMessageRequest(c *gin.Context) (convID, messageID, userID string, ok bool) {
	userID = c.GetString("user_id")
	convID = c.Param("id")
	messageID = c.Param("message_id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return "", "", "", false
	}
	if _, err := uuid.Parse(messageID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid message ID"})
		return "", "", "", false
	}

	if !h.isParticipant(convID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return "", "", "", false
	}
	return convID, messageID, userID, true
}

// UpdateMessage updates a DM message's content.
func (h *Handler) UpdateMessage(c *gin.Context) {
	convID, messageID, userID, ok := h.resolveDMMessageRequest(c)
	if !ok {
		return
	}

	var req updateDMMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}
	if !h.authorizeDMMessageUpdate(c, convID, messageID, userID) {
		return
	}

	// E2EE enforcement — all DMs are encrypted under #201; require ciphertext shape unconditionally.
	decoded, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil || len(decoded) < 28 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ciphertext format for encrypted conversation"})
		return
	}

	result, updated := h.updateDMMessageCiphertext(c, convID, messageID, req)
	if !updated {
		return
	}

	// Broadcast to other participants
	h.broadcastToDMParticipants(convID, userID, websocket.OutgoingMessage{
		Type: "dm_message_update",
		Data: map[string]interface{}{
			"id":              messageID,
			"conversation_id": convID,
			"content":         req.Content,
			"key_version":     result.KeyVersion,
			"edited_at":       result.EditedAt,
		},
	})

	c.JSON(http.StatusOK, gin.H{"message": result})
}

// DeleteMessage deletes a DM message.
func (h *Handler) DeleteMessage(c *gin.Context) {
	convID, messageID, userID, ok := h.resolveDMMessageRequest(c)
	if !ok {
		return
	}

	// Check message exists and user is the author
	var authorID string
	if err := h.db.QueryRow(`SELECT user_id FROM dm_messages WHERE id = $1 AND conversation_id = $2`, messageID, convID).Scan(&authorID); err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgMessageNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to check DM message author", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete message"})
		return
	}

	if authorID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only delete your own messages"})
		return
	}

	if _, err := h.db.Exec(`DELETE FROM dm_messages WHERE id = $1`, messageID); err != nil {
		h.log.Error("Failed to delete DM message", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete message"})
		return
	}

	// Broadcast to other participants
	h.broadcastToDMParticipants(convID, userID, websocket.OutgoingMessage{
		Type: "dm_message_delete",
		Data: map[string]interface{}{
			"id":              messageID,
			"conversation_id": convID,
		},
	})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// broadcastToDMParticipants sends a WebSocket message to all participants of a DM except the sender.
func (h *Handler) broadcastToDMParticipants(convID, excludeUserID string, msg websocket.OutgoingMessage) {
	rows, err := h.db.Query(`SELECT user_id FROM dm_participants WHERE conversation_id = $1`, convID)
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil || uid == excludeUserID {
			continue
		}
		if parsed, parseErr := uuid.Parse(uid); parseErr == nil {
			h.hub.BroadcastToUser(parsed, msg)
		}
	}
}

// loadDMAttachments batch-loads attachment summaries for a set of DM message IDs.
func (h *Handler) loadDMAttachments(messageIDs []string) (map[string][]models.AttachmentSummary, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	rows, err := h.db.Query(`
		SELECT ma.message_id, mf.id, mf.file_type, mf.mime_type, mf.file_size
		FROM dm_message_attachments ma
		INNER JOIN media_files mf ON ma.file_id = mf.id
		WHERE ma.message_id = ANY($1::uuid[])
		  AND mf.deleted_at IS NULL
		ORDER BY ma.message_id, ma.position
	`, pq.Array(messageIDs))
	if err != nil {
		return nil, err
	}
	return messagehandlers.ScanAttachmentSummaries(rows)
}

// ---------------------------------------------------------------------------
// DM Voice Enforcement (#488)
// ---------------------------------------------------------------------------

// authorizeDMGroupAdmin validates params, verifies group DM membership, and checks admin role.
// Returns the conversation ID, target ID, and true if authorization succeeds.
// Returns empty strings and false (with HTTP error already sent) on failure.
func (h *Handler) authorizeDMGroupAdmin(c *gin.Context, errMsgNotAdmin string) (convID, targetID string, ok bool) {
	actorID := c.GetString("user_id")
	convID = c.Param("id")
	targetID = c.Param("userId")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return "", "", false
	}
	if _, err := uuid.Parse(targetID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return "", "", false
	}

	var isGroup bool
	var actorRole string
	err := h.db.QueryRow(`SELECT dc.is_group, dp.role FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1`, convID, actorID).Scan(&isGroup, &actorRole)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return "", "", false
	}
	if err != nil {
		h.log.Error(errMsgFailedCheckParticipation, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return "", "", false
	}
	if !isGroup {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgHardEnforcementGroupOnly})
		return "", "", false
	}
	if actorRole != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotAdmin})
		return "", "", false
	}

	return convID, targetID, true
}

// dmIsTargetInVoice checks whether the target is in a DM voice call.
func (h *Handler) dmIsTargetInVoice(convID, targetID string) bool {
	var inVoice bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2)`,
		convID, targetID).Scan(&inVoice); err != nil {
		h.log.Error("Failed to check DM voice status", "error", err, "conversation_id", convID, "target_id", targetID)
		return false
	}
	return inVoice
}

// dmPublishEnforcement publishes a NATS enforcement message if the target is in DM voice.
func (h *Handler) dmPublishEnforcement(convID, targetID, subject, action string) {
	if h.dmIsTargetInVoice(convID, targetID) && h.nats != nil {
		if err := h.nats.Publish(subject, map[string]interface{}{
			"channelId": convID, "userId": targetID, "action": action,
		}); err != nil {
			h.log.Error("Failed to publish DM enforcement", "error", err, "subject", subject, "action", action, "conversation_id", convID, "target_id", targetID)
		}
	}
}

// dmBroadcastVoiceState sends a dm_voice_state_update WS event to DM participants.
func (h *Handler) dmBroadcastVoiceState(convID, targetID, action string) {
	convUUID, _ := uuid.Parse(convID)
	h.hub.BroadcastToDM(convUUID, websocket.OutgoingMessage{
		Type: "dm_voice_state_update",
		Data: map[string]interface{}{
			"conversation_id": convID, "user_id": targetID, "action": action,
		},
	})
}

// DMUserMute soft-mutes a participant in a DM voice call (user can undo).
// POST /dm/conversations/:id/voice/:userId/user-mute
func (h *Handler) DMUserMute(c *gin.Context) {
	actorID := c.GetString("user_id")
	convID := c.Param("id")
	targetID := c.Param("userId")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}
	if _, err := uuid.Parse(targetID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidUserID})
		return
	}
	if actorID == targetID {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgCannotTargetSelf})
		return
	}

	// Verify actor is a participant
	var exists bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`,
		convID, actorID).Scan(&exists); err != nil {
		h.log.Error(errMsgFailedCheckParticipation, "error", err, "conversation_id", convID, "user_id", actorID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckParticipation})
		return
	}
	if !exists {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}

	// Verify target is in DM voice
	var targetInVoice bool
	if err := h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2)`,
		convID, targetID).Scan(&targetInVoice); err != nil {
		h.log.Error(errMsgFailedCheckParticipation, "error", err, "conversation_id", convID, "target_id", targetID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckParticipation})
		return
	}
	if !targetInVoice {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgTargetNotInVoice})
		return
	}

	if h.nats != nil {
		if err := h.nats.Publish("voice.user_mute", map[string]interface{}{
			"channelId": convID,
			"userId":    targetID,
		}); err != nil {
			h.log.Error("Failed to publish DM user mute", "error", err, "conversation_id", convID, "target_id", targetID)
		}
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// dmVoiceEnforcementChange describes one hard mute/deafen state transition:
// the enforcement flag UPDATE plus the NATS command and WS broadcast that
// announce it. updateSQL values are compile-time constants — never build
// them dynamically.
type dmVoiceEnforcementChange struct {
	updateSQL      string
	failLog        string
	failErrMsg     string
	subject        string
	action         string
	broadcastState string
}

// applyDMVoiceEnforcement runs the shared tail of the four DM hard-mute/deafen
// handlers. RowsAffected 0 MUST stay a 404 — it is what stops an enforcement
// call against a non-participant from fabricating success.
func (h *Handler) applyDMVoiceEnforcement(c *gin.Context, convID, targetID string, change dmVoiceEnforcementChange) {
	result, err := h.db.Exec(change.updateSQL, convID, targetID)
	if err != nil {
		h.log.Error(change.failLog, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": change.failErrMsg})
		return
	}
	if ra, _ := result.RowsAffected(); ra == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgTargetNotParticipant})
		return
	}

	h.dmPublishEnforcement(convID, targetID, change.subject, change.action)
	h.dmBroadcastVoiceState(convID, targetID, change.broadcastState)

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// DMHardMute server-mutes a participant in a group DM (admin only, user cannot undo).
// POST /dm/conversations/:id/voice/:userId/mute
func (h *Handler) DMHardMute(c *gin.Context) {
	convID, targetID, ok := h.authorizeDMGroupAdmin(c, errMsgOnlyAdminCanMute)
	if !ok {
		return
	}

	h.applyDMVoiceEnforcement(c, convID, targetID, dmVoiceEnforcementChange{
		updateSQL:      `UPDATE dm_participants SET server_muted = true WHERE conversation_id = $1 AND user_id = $2`,
		failLog:        "Failed to set DM server_muted",
		failErrMsg:     errMsgFailedApplyEnforcement,
		subject:        "voice.enforce.mute",
		action:         "mute",
		broadcastState: "server_muted",
	})
}

// DMHardUnmute removes server-mute from a group DM participant (admin only).
// DELETE /dm/conversations/:id/voice/:userId/mute
func (h *Handler) DMHardUnmute(c *gin.Context) {
	convID, targetID, ok := h.authorizeDMGroupAdmin(c, errMsgOnlyAdminCanUnmute)
	if !ok {
		return
	}

	// Check if deafened — cannot unmute while deafened
	var serverDeafened bool
	if err := h.db.QueryRow(`SELECT server_deafened FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
		convID, targetID).Scan(&serverDeafened); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgTargetNotParticipant})
			return
		}
		h.log.Error("Failed to check deafen state", "error", err, "conversation_id", convID, "target_id", targetID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCheckParticipation})
		return
	}
	if serverDeafened {
		c.JSON(http.StatusConflict, gin.H{"error": errMsgCannotUnmuteWhileDeafened})
		return
	}

	h.applyDMVoiceEnforcement(c, convID, targetID, dmVoiceEnforcementChange{
		updateSQL:      `UPDATE dm_participants SET server_muted = false WHERE conversation_id = $1 AND user_id = $2`,
		failLog:        "Failed to clear DM server_muted",
		failErrMsg:     errMsgFailedRemoveEnforcement,
		subject:        "voice.enforce.mute",
		action:         "unmute",
		broadcastState: "server_unmuted",
	})
}

// DMHardDeafen server-deafens a participant in a group DM (admin only).
// POST /dm/conversations/:id/voice/:userId/deafen
func (h *Handler) DMHardDeafen(c *gin.Context) {
	convID, targetID, ok := h.authorizeDMGroupAdmin(c, errMsgOnlyAdminCanDeafen)
	if !ok {
		return
	}

	h.applyDMVoiceEnforcement(c, convID, targetID, dmVoiceEnforcementChange{
		updateSQL:      `UPDATE dm_participants SET server_muted = true, server_deafened = true WHERE conversation_id = $1 AND user_id = $2`,
		failLog:        "Failed to set DM server_deafened",
		failErrMsg:     errMsgFailedApplyEnforcement,
		subject:        "voice.enforce.deafen",
		action:         "deafen",
		broadcastState: "server_deafened",
	})
}

// DMHardUndeafen removes server-deafen from a group DM participant (admin only).
// DELETE /dm/conversations/:id/voice/:userId/deafen
func (h *Handler) DMHardUndeafen(c *gin.Context) {
	convID, targetID, ok := h.authorizeDMGroupAdmin(c, errMsgOnlyAdminCanUndeafen)
	if !ok {
		return
	}

	h.applyDMVoiceEnforcement(c, convID, targetID, dmVoiceEnforcementChange{
		updateSQL:      `UPDATE dm_participants SET server_muted = false, server_deafened = false WHERE conversation_id = $1 AND user_id = $2`,
		failLog:        "Failed to clear DM server_deafened",
		failErrMsg:     errMsgFailedRemoveEnforcement,
		subject:        "voice.enforce.deafen",
		action:         "undeafen",
		broadcastState: "server_undeafened",
	})
}

// ─── DM voice call ring (#1209) ──────────────────────────────────────────
//
// RingDMCall + DeclineDMCall + CancelDMCall + AuthorizeDMVoiceForMediaPlane
// implement the server-authoritative DM voice-call ring layer per spec
// [internal]specs/2026-05-27-1209-dm-group-voice-calls-design.md §6.1.
// State machinery: voicering.go. Broadcasts: direct hub.BroadcastToUser /
// hub.BroadcastToDM (single-replica per spec §5.4; multi-replica migration
// path documented in §5.5).

// DefaultRingTimeoutSeconds is the wall-clock budget the callee has to
// accept or decline an incoming DM voice call before the server-side
// timer fires dm_voice_call_timed_out. 45s matches the spec §6.1 default.
const DefaultRingTimeoutSeconds = 45

// callerInfoFor fetches the minimal user-identity payload broadcast in the
// dm_voice_call_invited WS event so callees can render IncomingCallBanner.
// Returns DisplayName / AvatarURL as empty strings (NOT NULL) when the
// columns are NULL; the WS schema marks both fields as .optional().
func (h *Handler) callerInfoFor(userID string) (map[string]interface{}, error) {
	var username, displayName, avatarURL string
	err := h.db.QueryRow(`
		SELECT username, COALESCE(display_name, ''), COALESCE(avatar_url, '')
		FROM users WHERE id = $1
	`, userID).Scan(&username, &displayName, &avatarURL)
	if err != nil {
		return nil, err
	}
	info := map[string]interface{}{
		"user_id":  userID,
		"username": username,
	}
	if displayName != "" {
		info["display_name"] = displayName
	}
	if avatarURL != "" {
		info["avatar_url"] = avatarURL
	}
	return info, nil
}

// dmVoiceInvitedData builds the `data` map broadcast in the dm_voice_call_invited
// WS event. Extracted from RingDMCall (#1219 B3) so the is_group emission is
// unit-testable without the hub — the invite is published on the unexported
// userBroadcast channel and is_group is absent from the HTTP response, so it
// cannot be observed from package dm_test. `caller` is the map returned by
// callerInfoFor (user_id, username, optional display_name/avatar_url).
func dmVoiceInvitedData(convID uuid.UUID, isGroup bool, caller map[string]interface{}, ring *PendingCall, timeoutSeconds int) map[string]interface{} {
	return map[string]interface{}{
		"conversation_id":      convID.String(),
		"is_group":             isGroup,
		"caller":               caller,
		"ring_id":              ring.RingID.String(),
		"ring_started_at":      ring.RingStartedAt.UTC().Format(time.RFC3339),
		"ring_timeout_seconds": timeoutSeconds,
	}
}

// fetchDMCalleesExcluding returns the user IDs of all dm_participants for
// the conversation EXCEPT the given exclude user. Used by RingDMCall to
// build the ringing-user-ids set (everyone in the conv minus the caller).
func (h *Handler) fetchDMCalleesExcluding(convID, excludeUserID string) ([]uuid.UUID, error) {
	rows, err := h.db.Query(`
		SELECT user_id FROM dm_participants
		WHERE conversation_id = $1 AND user_id != $2
	`, convID, excludeUserID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	callees := []uuid.UUID{}
	for rows.Next() {
		var uid uuid.UUID
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		callees = append(callees, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return callees, nil
}

// filterOnlineCallees returns the subset of callees with at least one live WS
// client (#1219 B2, decision G1), keeping offline group members out of the
// caller's ringing tally. It reuses the input slice's backing array.
func (h *Handler) filterOnlineCallees(callees []uuid.UUID) []uuid.UUID {
	online := callees[:0]
	for _, callee := range callees {
		if h.hub.IsUserOnline(callee) {
			online = append(online, callee)
		}
	}
	return online
}

// writeRingConflict writes the 409 Conflict response for a ring already in
// flight on the conversation. When existingAny is a *PendingCall the response
// carries that ring's metadata so the renderer can offer a "join existing
// call" affordance (group follow-up #1219); otherwise a generic 409.
func writeRingConflict(c *gin.Context, existingAny any) {
	existing, ok := existingAny.(*PendingCall)
	if !ok {
		c.JSON(http.StatusConflict, gin.H{"error": errMsgAlreadyRinging})
		return
	}
	c.JSON(http.StatusConflict, gin.H{
		"error": errMsgAlreadyRinging,
		"existing_ring": gin.H{
			"caller_user_id":  existing.CallerUserID.String(),
			"ring_id":         existing.RingID.String(),
			"ring_started_at": existing.RingStartedAt.Format(time.RFC3339),
		},
	})
}

// uuidsToStrings converts a slice of UUIDs to their canonical string form.
func uuidsToStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// parseOptionalVoiceRingID binds the optional ring_id from the request body.
// bindErrMsg preserves the historical per-endpoint bad-request copy (voice
// join vs voice call) now that the former twin parsers are unified.
func parseOptionalVoiceRingID(c *gin.Context, bindErrMsg string) (uuid.UUID, bool) {
	if c.Request.ContentLength == 0 {
		return uuid.Nil, true
	}
	var request struct {
		RingID string `json:"ring_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": bindErrMsg})
		return uuid.Nil, false
	}
	if request.RingID == "" {
		return uuid.Nil, true
	}
	ringID, err := uuid.Parse(request.RingID)
	if err != nil || ringID == uuid.Nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ring ID"})
		return uuid.Nil, false
	}
	return ringID, true
}

func (h *Handler) ensureDMVoiceRingAvailable(c *gin.Context, convUUID uuid.UUID, convID string) bool {
	lease, hasLease, err := LookupDMVoiceCallLease(c.Request.Context(), h.redis, convUUID)
	if err != nil {
		h.log.Error("Failed to check active DM voice call lease", "error", err,
			"conversation_id", sanitizeLogValue(convID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return false
	}

	// Presence is a bounded compatibility fence, not durable liveness. New
	// heartbeats refresh joined_at; rows older than the lease window are stale
	// after a dropped terminal event and must not block future calls forever.
	if _, err := h.db.Exec(`
		DELETE FROM dm_voice_participants
		WHERE conversation_id = $1
		  AND joined_at < NOW() - ($2 * INTERVAL '1 second')
	`, convUUID, int(DMVoiceCallLeaseTTL.Seconds())); err != nil {
		h.log.Error("Failed to expire stale DM voice presence", "error", err,
			"conversation_id", sanitizeLogValue(convID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return false
	}
	var hasLiveVoiceParticipants bool
	if err := h.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM dm_voice_participants WHERE conversation_id = $1
		)
	`, convUUID).Scan(&hasLiveVoiceParticipants); err != nil {
		h.log.Error("Failed to check active DM voice room", "error", err,
			"conversation_id", sanitizeLogValue(convID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return false
	}
	if hasLiveVoiceParticipants {
		response := gin.H{"error": errMsgVoiceCallAlreadyActive}
		if hasLease {
			response["existing_call_id"] = lease.CallID.String()
		}
		c.JSON(http.StatusConflict, response)
		return false
	}

	if hasLease {
		// A direct /voice/join reservation has no ring ID and retains the short
		// handoff TTL until the media plane admits a participant. With no live
		// presence it is safe for an explicit ring to supersede this otherwise
		// silent reservation; promoted active calls and accepted rings fail closed.
		cleared, clearErr := ClearUnpromotedDMVoiceCallReservation(
			c.Request.Context(), h.redis, convUUID, lease.CallID,
		)
		if clearErr != nil {
			h.log.Error("Failed to clear unpromoted DM voice reservation", "error", clearErr,
				"conversation_id", sanitizeLogValue(convID))
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
			return false
		}
		if !cleared {
			c.JSON(http.StatusConflict, gin.H{
				"error":            errMsgVoiceCallAlreadyActive,
				"existing_call_id": lease.CallID.String(),
			})
			return false
		}
	}

	// Close the lookup-to-clear race against a reservation or active lease
	// created on another replica while the presence check ran.
	lease, hasLease, err = LookupDMVoiceCallLease(c.Request.Context(), h.redis, convUUID)
	if err != nil {
		h.log.Error("Failed to recheck active DM voice call lease", "error", err,
			"conversation_id", sanitizeLogValue(convID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return false
	}
	if hasLease {
		c.JSON(http.StatusConflict, gin.H{
			"error":            errMsgVoiceCallAlreadyActive,
			"existing_call_id": lease.CallID.String(),
		})
		return false
	}
	return true
}

type voiceRingRecipients struct {
	calleeIDs []uuid.UUID
	isGroup   bool
}

func (h *Handler) loadVoiceRingRecipients(
	c *gin.Context,
	convUUID uuid.UUID,
	convID, callerID string,
) (voiceRingRecipients, bool) {
	callees, err := h.fetchDMCalleesExcluding(convID, callerID)
	if err != nil {
		h.log.Error("Failed to fetch DM callees for ring", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return voiceRingRecipients{}, false
	}
	if len(callees) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no callees in this conversation"})
		return voiceRingRecipients{}, false
	}

	var isGroup bool
	if err := h.db.QueryRow(`SELECT is_group FROM dm_conversations WHERE id = $1`, convUUID).Scan(&isGroup); err != nil {
		h.log.Error("Failed to fetch is_group for ring", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return voiceRingRecipients{}, false
	}
	if !isGroup {
		return voiceRingRecipients{calleeIDs: callees}, true
	}
	callees = h.filterOnlineCallees(callees)
	if len(callees) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no online members to call"})
		return voiceRingRecipients{}, false
	}
	return voiceRingRecipients{calleeIDs: callees, isGroup: true}, true
}

func (h *Handler) initializePendingDMCall(
	ring *PendingCall,
	convUUID uuid.UUID,
	isGroup bool,
	callerInfo map[string]interface{},
	callees []uuid.UUID,
) {
	ring.startTimerLocked(time.Duration(DefaultRingTimeoutSeconds)*time.Second, func() {
		h.onRingTimeout(convUUID, ring)
	})

	invitedPayload := dmVoiceInvitedData(convUUID, isGroup, callerInfo, ring, DefaultRingTimeoutSeconds)
	for _, calleeID := range callees {
		h.hub.BroadcastToUser(calleeID, websocket.OutgoingMessage{
			Type: "dm_voice_call_invited",
			Data: invitedPayload,
		})
	}
}

// RingDMCall initiates a DM voice call ring. POST /api/v1/dm/conversations/:id/voice/ring.
//
// Validates caller ∈ dm_participants. Creates pendingDMCalls[convID] entry,
// broadcasts dm_voice_call_invited to each ringing user via hub.BroadcastToUser,
// arms the 45-second timeout timer, returns ring metadata.
//
// Returns 409 Conflict if a ring is already in flight for the conversation
// (per spec §6.1 already-ringing edge case). The renderer can offer a "join
// the existing call" affordance based on the returned existing_ring metadata
// (group follow-up #1219 wires that UX; #1209 returns the metadata anyway).
func (h *Handler) RingDMCall(c *gin.Context) {
	callerIDStr := c.GetString("user_id")
	convIDStr := c.Param("id")

	convUUID, callerUUID, validIDs := parseDMVoiceIDs(c, convIDStr, callerIDStr)
	if !validIDs {
		return
	}

	if !h.isParticipant(convIDStr, callerIDStr) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}

	// Ring creation and pending->accepted promotion share one conversation-level
	// critical section. This closes the check-then-claim gap where a replacement
	// ring could be created after the prior ring was accepted but before its
	// pending map entry was released.
	unlockLifecycle := LockDMCallLifecycle(convUUID)
	defer unlockLifecycle()

	// Do not create a second ring while an accepted or active media call owns
	// the conversation. The shared expiring lease is refreshed by joined and
	// heartbeat events, so long calls survive the short accepted-ring handoff.
	if !h.ensureDMVoiceRingAvailable(c, convUUID, convIDStr) {
		return
	}

	// Early-exit for the optimistic-conflict case so DB queries are skipped on
	// the common "already ringing" path. The atomic LoadOrStore below is the
	// authoritative race-safe claim — this Load is purely a fast-path.
	if existingAny, loaded := pendingDMCalls.Load(convUUID); loaded {
		writeRingConflict(c, existingAny)
		return
	}

	// Fetch is_group: drives both the presence-aware group ring filter (#1219 B2)
	// and the dm_voice_call_invited is_group emission (#1219 B3).
	recipients, foundRecipients := h.loadVoiceRingRecipients(c, convUUID, convIDStr, callerIDStr)
	if !foundRecipients {
		return
	}

	callerInfo, err := h.callerInfoFor(callerIDStr)
	if err != nil {
		h.log.Error("Failed to fetch caller info for ring", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return
	}

	// Race-safe atomic claim (Gitar #1231 finding G2): construct the ring,
	// then LoadOrStore. If another concurrent /ring won the claim, return 409
	// with the OTHER ring's metadata. This collapses the Load-then-Store
	// window where two concurrent requests could both pass the Load check
	// before either Stores.
	ring := newPendingCall(convUUID, callerUUID, recipients.calleeIDs, time.Duration(DefaultRingTimeoutSeconds)*time.Second)
	existingAny, loaded := loadOrStoreInitializedPendingCall(ring, func() {
		// Arm the timeout and enqueue every invitation while the ring's
		// transition lock is held. A fast accept/cancel can load the claimed
		// ring, but cannot terminally own it until initialization completes.
		h.initializePendingDMCall(ring, convUUID, recipients.isGroup, callerInfo, recipients.calleeIDs)
	})
	if loaded {
		writeRingConflict(c, existingAny)
		return
	}

	h.log.Info("DM voice call ring initiated",
		"conversation_id", convIDStr,
		"caller_user_id", callerIDStr,
		"ring_id", ring.RingID.String(),
		"callee_count", len(recipients.calleeIDs),
	)

	// Response: minimal ring metadata the caller's renderer uses to track
	// its outgoing-ringing state.
	c.JSON(http.StatusOK, gin.H{
		"ring_id":          ring.RingID.String(),
		"ring_started_at":  ring.RingStartedAt.UTC().Format(time.RFC3339),
		"ringing_user_ids": uuidsToStrings(recipients.calleeIDs),
	})
}

// onRingTimeout fires when the ring-timeout timer expires without anyone
// accepting. Idempotent: if pendingDMCalls no longer contains this exact
// ring (because accept/decline/cancel already cleared it), no-op.
//
// For #1209 this broadcasts dm_voice_call_timed_out to all participants
// AND inserts a missed-call event row via insertCallEvent (best-effort —
// failure logs and proceeds; the ring-timeout cleanup is the load-bearing
// part). The row is inserted before the terminal broadcast so clients can
// safely refresh their conversation previews when they receive the event.
func (h *Handler) onRingTimeout(convUUID uuid.UUID, ring *PendingCall) {
	if !ring.tryTerminate() {
		return
	}
	defer ring.finalizeTerminal()

	// Insert missed-call event row (best-effort per spec §6.1 edge cases;
	// failure logged but doesn't block the ring-timeout cleanup).
	if err := h.insertCallEvent(context.Background(), convUUID, callEventMissed(ring)); err != nil {
		h.log.Error("Failed to insert missed call_event row",
			"error", err,
			"conversation_id", convUUID.String(),
			"ring_id", ring.RingID.String(),
		)
	}
	h.hub.BroadcastToDMParticipants(convUUID, websocket.OutgoingMessage{
		Type: "dm_voice_call_timed_out",
		Data: map[string]interface{}{
			"conversation_id": convUUID.String(),
			"ring_id":         ring.RingID.String(),
		},
	})
	h.log.Info("DM voice call ring timed out",
		"conversation_id", convUUID.String(),
		"ring_id", ring.RingID.String(),
	)
}

// resolvePendingDMRing runs the shared Decline/Cancel prologue: parse the
// conversation and acting-user IDs, bind the optional ring_id, and resolve
// the in-flight ring for the conversation, rejecting a stale ring_id with
// 409. On failure the HTTP error has already been written and ok is false.
func (h *Handler) resolvePendingDMRing(c *gin.Context) (ring *PendingCall, convUUID, actorUUID uuid.UUID, ok bool) {
	actorIDStr := c.GetString("user_id")
	convIDStr := c.Param("id")

	convUUID, err := uuid.Parse(convIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return nil, uuid.Nil, uuid.Nil, false
	}
	actorUUID, err = uuid.Parse(actorIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidCallerID})
		return nil, uuid.Nil, uuid.Nil, false
	}
	requestedRingID, validRequest := parseOptionalVoiceRingID(c, errMsgInvalidVoiceCallRequest)
	if !validRequest {
		return nil, uuid.Nil, uuid.Nil, false
	}

	storedAny, loaded := pendingDMCalls.Load(convUUID)
	if !loaded {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active ring for this conversation"})
		return nil, uuid.Nil, uuid.Nil, false
	}
	pending, castOK := storedAny.(*PendingCall)
	if !castOK {
		// Defensive: pendingDMCalls is only ever written with *PendingCall
		// values (RingDMCall is the sole writer). Match the RingDMCall
		// posture so a future sentinel-using code path doesn't crash these
		// handlers via ring.mu.Lock / ring.CallerUserID on a nil interface
		// (Copilot #1231 cycle-4 finding).
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return nil, uuid.Nil, uuid.Nil, false
	}
	if requestedRingID != uuid.Nil && requestedRingID != pending.RingID {
		c.JSON(http.StatusConflict, gin.H{"error": errMsgVoiceCallRingChanged})
		return nil, uuid.Nil, uuid.Nil, false
	}
	return pending, convUUID, actorUUID, true
}

// DeclineDMCall handles a callee declining a DM voice call ring.
// POST /api/v1/dm/conversations/:id/voice/decline.
//
// Validates the user is a currently-ringing callee for this conversation
// (membership check is implicit: only ringing callees are in the
// PendingCall.RingingUserIDs set). Always broadcasts dm_voice_call_declined
// to the caller (supports the per-decliner tally UI in #1219 group calls).
// If this decline empties the ringing set with nobody accepted (the only
// path that can hit this for DM 1:1 — single callee declining), broadcasts
// dm_voice_call_canceled with canceled_by='all_declined' and clears the
// pending ring.
//
// Per spec §6.1: returns 404 when no ring is pending, 403 when the user
// is in the conversation but not currently ringing (already declined /
// accepted, or never was a callee — e.g., the caller themselves).
//
// Per spec §3.4 / §4 axis C: per-decliner cancellation. Each decline only
// removes that decliner's ring; ring continues for others. Group-call tally
// progress event (dm_voice_call_decline_progress) is deferred to #1219.
func (h *Handler) DeclineDMCall(c *gin.Context) {
	ring, convUUID, declinerUUID, ok := h.resolvePendingDMRing(c)
	if !ok {
		return
	}

	transition := ring.tryDecline(declinerUUID, func() {
		// Enqueue the per-decliner tally while the transition lock is held so
		// an accept/cancel/timeout event cannot overtake it on the Hub lane.
		h.hub.BroadcastToUser(ring.CallerUserID, websocket.OutgoingMessage{
			Type: "dm_voice_call_declined",
			Data: map[string]interface{}{
				"conversation_id":  convUUID.String(),
				"ring_id":          ring.RingID.String(),
				"decliner_user_id": declinerUUID.String(),
			},
		})
	})
	switch transition {
	case declineTransitionInactive:
		c.JSON(http.StatusConflict, gin.H{"error": "call state changed before decline"})
		return
	case declineTransitionNotRinging:
		c.JSON(http.StatusForbidden, gin.H{"error": "not a ringing callee for this call"})
		return
	case declineTransitionTerminal:
		defer ring.finalizeTerminal()
		// Insert declined-call event row (best-effort).
		if err := h.insertCallEvent(c.Request.Context(), convUUID, callEventDeclined(ring)); err != nil {
			h.log.Error("Failed to insert declined call_event row",
				"error", err,
				"conversation_id", convUUID.String(),
				"ring_id", ring.RingID.String(),
			)
		}
		h.hub.BroadcastToDMParticipants(convUUID, websocket.OutgoingMessage{
			Type: "dm_voice_call_canceled",
			Data: map[string]interface{}{
				"conversation_id": convUUID.String(),
				"ring_id":         ring.RingID.String(),
				"canceled_by":     "all_declined",
			},
		})
		h.log.Info("DM voice call ring ended (all callees declined)",
			"conversation_id", convUUID.String(),
			"ring_id", ring.RingID.String(),
		)
	case declineTransitionPending:
		h.log.Info("DM voice call: callee declined",
			"conversation_id", convUUID.String(),
			"ring_id", ring.RingID.String(),
			"decliner_user_id", declinerUUID.String(),
		)
	}

	c.Status(http.StatusNoContent)
}

// CancelDMCall lets the caller cancel their own ring before any callee
// has accepted. POST /api/v1/dm/conversations/:id/voice/cancel.
//
// Per spec §6.1: only the ring INITIATOR (CallerUserID) is authorized to
// cancel via this endpoint. A callee who doesn't want the call should hit
// /decline instead. Returns 204 on success, 404 if no ring is pending,
// 403 if the user is in the conversation but isn't the ring initiator.
//
// Broadcasts dm_voice_call_canceled with canceled_by='caller' to the
// conversation, clears pendingDMCalls, and inserts a canceled-status
// call_event row.
func (h *Handler) CancelDMCall(c *gin.Context) {
	ring, convUUID, callerUUID, ok := h.resolvePendingDMRing(c)
	if !ok {
		return
	}

	if ring.CallerUserID != callerUUID {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the ring initiator can cancel"})
		return
	}

	if !ring.tryTerminate() {
		c.JSON(http.StatusConflict, gin.H{"error": "call state changed before cancellation"})
		return
	}
	defer ring.finalizeTerminal()

	if err := h.insertCallEvent(c.Request.Context(), convUUID, callEventCanceled(ring)); err != nil {
		h.log.Error("Failed to insert canceled call_event row",
			"error", err,
			"conversation_id", convUUID.String(),
			"ring_id", ring.RingID.String(),
		)
	}
	h.hub.BroadcastToDMParticipants(convUUID, websocket.OutgoingMessage{
		Type: "dm_voice_call_canceled",
		Data: map[string]interface{}{
			"conversation_id": convUUID.String(),
			"ring_id":         ring.RingID.String(),
			"canceled_by":     "caller",
		},
	})

	h.log.Info("DM voice call ring canceled by caller",
		"conversation_id", convUUID.String(),
		"ring_id", ring.RingID.String(),
		"caller_user_id", callerUUID.String(),
	)

	c.Status(http.StatusNoContent)
}

type dmVoiceAuthorizeIdentity struct {
	isGroup     bool
	username    string
	displayName sql.NullString
	avatarURL   sql.NullString
}

func parseDMVoiceAuthorizeCallID(c *gin.Context) (uuid.UUID, bool) {
	if c.Request.ContentLength == 0 {
		return uuid.Nil, true
	}
	var request struct {
		CallID string `json:"call_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid voice authorization request"})
		return uuid.Nil, false
	}
	if request.CallID == "" {
		return uuid.Nil, true
	}
	callID, err := uuid.Parse(request.CallID)
	if err != nil || callID == uuid.Nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid call ID"})
		return uuid.Nil, false
	}
	return callID, true
}

func (h *Handler) loadDMVoiceAuthorizeIdentity(
	c *gin.Context,
	convID, userID string,
) (dmVoiceAuthorizeIdentity, bool) {
	var identity dmVoiceAuthorizeIdentity
	err := h.db.QueryRow(`
		SELECT dc.is_group, u.username, u.display_name, u.avatar_url
		FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		JOIN users u ON u.id = dp.user_id
		WHERE dc.id = $1
	`, convID, userID).Scan(
		&identity.isGroup,
		&identity.username,
		&identity.displayName,
		&identity.avatarURL,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"authorized": false, "error": errMsgNotParticipant})
		return dmVoiceAuthorizeIdentity{}, false
	}
	if err != nil {
		h.log.Error("Failed to check DM voice authorization (G7)", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return dmVoiceAuthorizeIdentity{}, false
	}
	return identity, true
}

func (h *Handler) resolveDMVoiceAuthorizeLease(
	c *gin.Context,
	convUUID, userUUID, requestedCallID uuid.UUID,
) (VoiceCallLease, bool, bool) {
	lease, hasLease, err := LookupDMVoiceCallLease(c.Request.Context(), h.redis, convUUID)
	if err != nil {
		h.log.Error("Failed to lookup DM voice call lease", "error", err,
			"conversation_id", sanitizeLogValue(c.Param("id")))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return VoiceCallLease{}, false, false
	}
	if !hasLease {
		c.JSON(http.StatusConflict, gin.H{"error": errMsgVoiceCallNoLongerActive})
		return VoiceCallLease{}, false, false
	}
	if requestedCallID == uuid.Nil {
		admittedCallID, admitted, admissionErr := LookupDMVoiceJoinAdmission(
			c.Request.Context(), h.redis, convUUID, userUUID,
		)
		if admissionErr != nil {
			h.log.Error("Failed to lookup DM voice join admission", "error", admissionErr,
				"conversation_id", sanitizeLogValue(c.Param("id")))
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
			return VoiceCallLease{}, false, false
		}
		if !admitted || admittedCallID != lease.CallID {
			c.JSON(http.StatusConflict, gin.H{"error": "Voice call join is no longer active"})
			return VoiceCallLease{}, false, false
		}
	} else if lease.CallID != requestedCallID {
		c.JSON(http.StatusConflict, gin.H{"error": errMsgVoiceCallNoLongerActive})
		return VoiceCallLease{}, false, false
	}
	return lease, true, true
}

// AuthorizeDMVoiceForMediaPlane is the G7 defense-in-depth auth re-check
// endpoint per spec §6.5. Called BY THE MEDIA-PLANE (not the renderer)
// when an SFU client connects with roomId == conversation_id; the
// media-plane forwards the user's JWT and this endpoint validates the
// user is in dm_participants.
//
// POST /api/v1/dm/conversations/:id/voice/authorize.
//
// The renderer-facing voice-join endpoint (AuthorizeVoiceJoin) still does its
// own authorization at the user-facing surface. This endpoint is the second
// check at the SFU boundary and resolves only a server-generated call lease
// already reserved by the renderer-facing /voice/join flow before RoomManager
// can create the room. New clients present the exact ID; legacy clients may
// omit it only while their own short user-scoped join admission still matches
// the lease. This endpoint never creates or refreshes a lease. That closes both
// the original G7 authorization gap and the authorize->joined lifecycle gap
// without breaking already released clients during a rolling upgrade.
//
// Returns 200 + {authorized: true, is_group: bool, media_entitlements: {...}}
// for members; 403 for non-members. The minimal metadata helps the
// media-plane decide whether to enable group-call-specific features
// (deferred to #1219). media_entitlements (#1542 review reconciliation)
// carries the requesting user's server-authoritative per-user media caps —
// this endpoint, not the renderer-facing AuthorizeVoiceJoin, is what the
// media-plane actually parses at the SFU boundary, so Participant.tier (the
// DM room-cap input per ADR-0029) is sourced from here.
func (h *Handler) AuthorizeDMVoiceForMediaPlane(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	convUUID, userUUID, validIDs := parseDMVoiceIDs(c, convID, userID)
	if !validIDs {
		return
	}
	requestedCallID, validRequest := parseDMVoiceAuthorizeCallID(c)
	if !validRequest {
		return
	}

	// Fetch is_group AND the requesting member's authoritative display identity
	// (CV-CAN-017), folded into this existing membership query so no extra
	// round-trip. The media-plane uses username/display_name/avatar_url from here
	// instead of the client-supplied socket.handshake.auth values, closing the
	// display-identity spoof for DM voice rooms (mirrors the server-channel path).
	identity, authorized := h.loadDMVoiceAuthorizeIdentity(c, convID, userID)
	if !authorized {
		return
	}

	// Resolve the requesting user's media entitlements server-side from the
	// AUTHENTICATED user_id (never a client value), mirroring AuthorizeVoiceJoin
	// above. DMs have no channel/server tier, so this is the plain per-user
	// MediaFor — no ChannelAudioUplift. GetTier fails closed to free, so a
	// resolution failure degrades to the free floor, never grants premium.
	//
	// NOTE (#1542): this response intentionally omits room_owner_tier. DMs have
	// no owner; the media-plane resolves the per-room cap from the MAX tier among
	// present participants, each threaded from this media_entitlements object
	// onto Participant.tier (ADR-0029).
	mediaEnt := entitlements.MediaFor(h.entCache.GetTier(c.Request.Context(), userID))

	// Serialize pre-existing lease resolution with /voice/ring, direct
	// /voice/join reservation, and pending->accepted promotion. Every admitted
	// DM media room has a shared call ID before the media plane creates the room,
	// eliminating the authorize->voice.joined gap.
	unlockLifecycle := LockDMCallLifecycle(convUUID)
	defer unlockLifecycle()

	lease, hasLease, resolved := h.resolveDMVoiceAuthorizeLease(c, convUUID, userUUID, requestedCallID)
	if !resolved {
		return
	}
	if !h.validDMVoiceMediaAuthorizationProof(c, convUUID, requestedCallID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Media authorization proof required"})
		return
	}
	if hasLease {
		if err := MarkDMVoiceCallMediaAuthorized(
			c.Request.Context(), h.redis, convUUID, lease.CallID,
		); err != nil {
			if errors.Is(err, ErrDMVoiceCallLeaseConflict) {
				c.JSON(http.StatusConflict, gin.H{"error": errMsgVoiceCallNoLongerActive})
				return
			}
			h.log.Error("Failed to mark DM voice media authorization", "error", err,
				"conversation_id", sanitizeLogValue(convID))
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
			return
		}
	}

	response := gin.H{
		"authorized":         true,
		"is_group":           identity.isGroup,
		"media_entitlements": mediaEnt,
		// CV-CAN-017: server-authoritative display identity (see query above).
		"username":     identity.username,
		"display_name": identity.displayName.String,
		"avatar_url":   identity.avatarURL.String,
	}
	if hasLease {
		response["call_id"] = lease.CallID.String()
		response["call_caller_user_id"] = lease.CallerUserID.String()
		if lease.RingID != uuid.Nil {
			response["call_ring_id"] = lease.RingID.String()
		}
	}
	c.JSON(http.StatusOK, response)
}

// AbortDMVoiceMediaAuthorization releases a media-authorized direct handoff
// when its socket disconnects before admission. The Redis CAS is exact and
// refuses promoted calls, accepted rings, and successor call IDs.
func (h *Handler) AbortDMVoiceMediaAuthorization(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	convUUID, _, validIDs := parseDMVoiceIDs(c, convID, userID)
	if !validIDs {
		return
	}
	callID, validRequest := parseDMVoiceAuthorizeCallID(c)
	if !validRequest {
		return
	}
	if callID == uuid.Nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Call ID is required"})
		return
	}
	if !h.validDMVoiceMediaAuthorizationProof(c, convUUID, callID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Media authorization proof required"})
		return
	}
	if _, authorized := h.loadDMVoiceAuthorizeIdentity(c, convID, userID); !authorized {
		return
	}

	unlockLifecycle := LockDMCallLifecycle(convUUID)
	defer unlockLifecycle()
	if _, err := AbortAuthorizedDMVoiceCallReservation(
		c.Request.Context(), h.redis, convUUID, callID,
	); err != nil {
		h.log.Error("Failed to abort DM voice media authorization", "error", err,
			"conversation_id", sanitizeLogValue(convID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedAuthorize})
		return
	}
	c.Status(http.StatusNoContent)
}

// HandleUserDisconnect cleans up DM voice rings the user has initiated
// when their last WebSocket connection drops. Invoked by the hub's
// handleUnregister via the DMRingCanceller callback wired at startup
// (see SetDMRingCanceller in router.go).
//
// Per spec §6.1 edge case "Caller WS disconnect mid-ring": iterate
// pendingDMCalls; for each entry where CallerUserID matches the
// disconnecting user, stop the timer, clear the map entry, broadcast
// dm_voice_call_canceled with canceled_by='caller' to the conversation,
// and insert a canceled-status call_event row.
//
// Implementation detail: invoked in a goroutine from the hub so blocking
// here doesn't stall the hub's handleUnregister loop. Iterates the
// sync.Map which is safe under concurrent reads.
func (h *Handler) HandleUserDisconnect(userID uuid.UUID) {
	// Defense-in-depth: this runs in a goroutine spawned by the websocket hub
	// (see hub.handleUnregister); a panic here would kill the entire
	// control-plane process. Recover + log so the cleanup is best-effort.
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("DM ring cleanup panic recovered",
				"panic", r,
				"user_id", userID.String(),
			)
		}
	}()

	pendingDMCalls.Range(func(key, value interface{}) bool {
		ring, ok := value.(*PendingCall)
		if !ok {
			return true
		}
		if ring.CallerUserID != userID {
			return true
		}
		convUUID, ok := key.(uuid.UUID)
		if !ok {
			return true
		}

		if !ring.tryTerminate() {
			return true
		}
		defer ring.finalizeTerminal()
		if err := h.insertCallEvent(context.Background(), convUUID, callEventCanceled(ring)); err != nil {
			h.log.Error("Failed to insert canceled call_event row on WS disconnect",
				"error", err,
				"conversation_id", convUUID.String(),
				"ring_id", ring.RingID.String(),
				"user_id", userID.String(),
			)
		}
		h.hub.BroadcastToDMParticipants(convUUID, websocket.OutgoingMessage{
			Type: "dm_voice_call_canceled",
			Data: map[string]interface{}{
				"conversation_id": convUUID.String(),
				"ring_id":         ring.RingID.String(),
				"canceled_by":     "caller",
			},
		})
		h.log.Info("DM voice call ring canceled by caller WS disconnect",
			"conversation_id", convUUID.String(),
			"ring_id", ring.RingID.String(),
			"user_id", userID.String(),
		)
		return true
	})
}
