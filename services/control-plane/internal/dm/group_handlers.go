package dm

import (
	"database/sql"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
)

const (
	errMsgFailedUpdateRole  = "Failed to update role"
	errMsgFailedDeleteGroup = "Failed to delete group"
)

// UpdateRoleRequest represents the request body for updating a DM participant's role.
type UpdateRoleRequest struct {
	Role string `json:"role" binding:"required"`
}

// UpdateMemberRole changes a group DM participant's role (admin/member).
func (h *Handler) UpdateMemberRole(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")
	targetUserID := c.Param("userId")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}
	if _, err := uuid.Parse(targetUserID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	var req UpdateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}
	if req.Role != "admin" && req.Role != "member" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Role must be 'admin' or 'member'"})
		return
	}

	if targetUserID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot change your own role"})
		return
	}

	// Verify caller is participant, admin, and conversation is a group
	var isGroup bool
	var createdBy string
	var callerRole string
	err := h.db.QueryRow(`
		SELECT dc.is_group, dc.created_by, dp.role FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1`, convID, userID).Scan(&isGroup, &createdBy, &callerRole)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}
	if err != nil {
		h.log.Error("Failed to verify caller role", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateRole})
		return
	}
	if !isGroup {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot change roles in non-group conversations"})
		return
	}
	if callerRole != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only admins can change roles"})
		return
	}

	// Verify target is a participant
	var exists bool
	err = h.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`,
		convID, targetUserID).Scan(&exists)
	if err != nil {
		h.log.Error("Failed to check target participation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateRole})
		return
	}
	if !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Target user is not a participant"})
		return
	}

	// Cannot demote the group creator
	if targetUserID == createdBy && req.Role == "member" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Cannot demote the group creator"})
		return
	}

	// Update the role
	if _, err = h.db.Exec(`UPDATE dm_participants SET role = $1 WHERE conversation_id = $2 AND user_id = $3`,
		req.Role, convID, targetUserID); err != nil {
		h.log.Error("Failed to update member role", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateRole})
		return
	}

	// Broadcast role change to all participants
	h.broadcastToDMParticipants(convID, "", websocket.OutgoingMessage{
		Type: "dm_role_changed",
		Data: map[string]interface{}{
			"conversation_id": convID,
			"user_id":         targetUserID,
			"role":            req.Role,
			"changed_by":      userID,
		},
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "Role updated",
		"user_id": targetUserID,
		"role":    req.Role,
	})
}

// DeleteGroup permanently deletes a group DM conversation and all associated data.
func (h *Handler) DeleteGroup(c *gin.Context) {
	userID := c.GetString("user_id")
	convID := c.Param("id")

	if _, err := uuid.Parse(convID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidConversationID})
		return
	}

	// Verify caller is participant, admin, and conversation is a group
	var isGroup bool
	var callerRole string
	err := h.db.QueryRow(`
		SELECT dc.is_group, dp.role FROM dm_conversations dc
		JOIN dm_participants dp ON dp.conversation_id = dc.id AND dp.user_id = $2
		WHERE dc.id = $1`, convID, userID).Scan(&isGroup, &callerRole)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgNotParticipant})
		return
	}
	if err != nil {
		h.log.Error("Failed to verify caller for group deletion", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteGroup})
		return
	}
	if !isGroup {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot delete non-group conversations"})
		return
	}
	if callerRole != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only admins can delete groups"})
		return
	}

	// Fetch participant list before deletion for broadcasting
	participantIDs, err := h.fetchParticipantIDs(convID)
	if err != nil {
		h.log.Error("Failed to fetch participants for deletion", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteGroup})
		return
	}

	// Delete all group data in a transaction
	if err := h.deleteGroupData(convID); err != nil {
		h.log.Error("Failed to delete group data", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteGroup})
		return
	}

	// Broadcast deletion to all former participants
	if h.hub != nil {
		for _, uid := range participantIDs {
			if targetUUID, parseErr := uuid.Parse(uid); parseErr == nil {
				h.hub.BroadcastToUser(targetUUID, websocket.OutgoingMessage{
					Type: "dm_group_deleted",
					Data: map[string]interface{}{
						"conversation_id": convID,
						"deleted_by":      userID,
					},
				})
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Group deleted"})
}

// fetchParticipantIDs returns user IDs for all participants in a conversation.
func (h *Handler) fetchParticipantIDs(convID string) ([]string, error) {
	rows, err := h.db.Query(`SELECT user_id FROM dm_participants WHERE conversation_id = $1`, convID)
	if err != nil {
		return nil, fmt.Errorf("query participants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("scan participant: %w", err)
		}
		ids = append(ids, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate participants: %w", err)
	}
	return ids, nil
}

// deleteGroupData deletes all data associated with a group DM conversation in a transaction.
func (h *Handler) deleteGroupData(convID string) error {
	tx, err := h.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error(errMsgFailedRollbackTransaction, "error", rbErr)
		}
	}()

	deletes := []struct{ query, label string }{
		{"DELETE FROM dm_voice_participants WHERE conversation_id = $1", "voice participants"},
		{"DELETE FROM dm_read_states WHERE conversation_id = $1", "read states"},
		{"DELETE FROM dm_pending_key_requests WHERE conversation_id = $1", "pending key requests"},
		{"DELETE FROM dm_key_revocations WHERE conversation_id = $1", "key revocations"},
		{"DELETE FROM dm_channel_keys WHERE conversation_id = $1", "channel keys"},
		{"DELETE FROM dm_messages WHERE conversation_id = $1", "messages"},
		{"DELETE FROM dm_participants WHERE conversation_id = $1", "participants"},
		{"DELETE FROM dm_conversations WHERE id = $1", "conversation"},
	}
	for _, d := range deletes {
		if _, err := tx.Exec(d.query, convID); err != nil {
			return fmt.Errorf("delete %s: %w", d.label, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
