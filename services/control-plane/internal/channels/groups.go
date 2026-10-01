package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

const (
	errInvalidServerID          = "Invalid server ID"
	errInvalidRequestBody       = "Invalid request body"
	errFailedCheckPerms         = "Failed to check permissions"
	errInsufficientPerms        = "insufficient permissions"
	errFailedCreateGroup        = "Failed to create channel group"
	errFailedUpdateGroup        = "Failed to update channel group"
	logMsgGroupRollbackFailed   = "Failed to rollback channel group transaction"
	errChannelGroupNotFound     = "Channel group not found"
	errFailedDeleteGroup        = "Failed to delete channel group"
	errFailedReorderChannels    = "Failed to reorder channels"
	errTemporaryOverrideManaged = "Temporary move access is system-managed"
	errChannelAuthorityRetry    = "Channel authority changed; retry"
	errChannelGroupTooLarge     = "Channel group has more than 500 children; ungroup or reorder children in batches of 500 or fewer before deleting"
)

var errChannelGroupChildLimit = errors.New("channel group child limit exceeded")

// CreateChannelGroupRequest represents a request to create a channel group
type CreateChannelGroupRequest struct {
	Name string `json:"name" binding:"required,min=1,max=100"`
}

// UpdateChannelGroupRequest represents a request to update a channel group
type UpdateChannelGroupRequest struct {
	Name     *string `json:"name,omitempty"`
	Position *int    `json:"position,omitempty"`
}

// ReorderChannelsRequest represents a bulk reorder/move of channels
type ReorderChannelsRequest struct {
	Channels []ChannelPosition `json:"channels" binding:"required"`
}

// ChannelPosition specifies a channel's group and position
type ChannelPosition struct {
	ChannelID string  `json:"channel_id" binding:"required,uuid"`
	GroupID   *string `json:"group_id"` // nil = uncategorized
	Position  int     `json:"position"`
}

type groupedChannelState struct {
	ID              string
	GroupID         *string
	SyncPermissions bool
	IsVoice         bool
}

type reorderChannelAuthorityRequest struct {
	serverID, userID, tokenEpoch string
	reorder                      ReorderChannelsRequest
	channelIDs                   []string
	preflight                    map[string]groupedChannelState
}

type channelGroupDeleteRequest struct {
	serverID, groupID, userID, tokenEpoch string
}

type channelGroupDeletePreflight struct {
	allIDs   []string
	voiceIDs []string
}

func syncedGroupChannelIDs(states []groupedChannelState) (all []string, voice []string) {
	voice = []string{}
	for _, state := range states {
		if !state.SyncPermissions {
			continue
		}
		all = append(all, state.ID)
		if state.IsVoice {
			voice = append(voice, state.ID)
		}
	}
	return all, voice
}

func sameChannelIDSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func authorityRotationDeletedChannelIDs(rotations []keyrotation.Rotation) map[string]struct{} {
	deleted := make(map[string]struct{})
	for _, rotation := range rotations {
		for _, channelID := range rotation.DeletedChannelIDs {
			deleted[channelID] = struct{}{}
		}
	}
	return deleted
}

func filterDeletedReorderedChannels(channels []ChannelPosition, deleted map[string]struct{}) []ChannelPosition {
	remaining := make([]ChannelPosition, 0, len(channels))
	for _, channel := range channels {
		if _, wasDeleted := deleted[channel.ChannelID]; !wasDeleted {
			remaining = append(remaining, channel)
		}
	}
	return remaining
}

func lockChannelGroupsTx(ctx context.Context, tx *sql.Tx, serverID string, groupIDs []string) error {
	if len(groupIDs) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM channel_groups
		WHERE server_id = $1 AND id = ANY($2::uuid[])
		ORDER BY id FOR KEY SHARE`, serverID, pq.Array(groupIDs))
	if err != nil {
		return fmt.Errorf("lock channel groups: %w", err)
	}
	count := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan locked channel group: %w", errors.Join(err, rows.Close()))
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate locked channel groups: %w", errors.Join(err, rows.Close()))
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close locked channel groups: %w", err)
	}
	if count != len(groupIDs) {
		return sql.ErrNoRows
	}
	return nil
}

// lockChannelGroupForDeleteTx conflicts with the FK KEY SHARE taken by an
// attached-channel INSERT. Deletion must hold it before its bounded child scan
// or a concurrent CreateChannel can appear after the scan and bypass the cap.
func lockChannelGroupForDeleteTx(ctx context.Context, tx *sql.Tx, serverID, groupID string) error {
	var lockedID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM channel_groups WHERE server_id = $1 AND id = $2 FOR UPDATE`, serverID, groupID,
	).Scan(&lockedID); err != nil {
		return fmt.Errorf("lock channel group for delete: %w", err)
	}
	return nil
}

func groupIDsForReorder(req ReorderChannelsRequest, states map[string]groupedChannelState) []string {
	groupIDs := make(map[string]struct{})
	for _, change := range req.Channels {
		if groupID := resolveGroupIDParam(change.GroupID); groupID != nil {
			groupIDs[groupID.(string)] = struct{}{}
		}
		if groupID := resolveGroupIDParam(states[change.ChannelID].GroupID); groupID != nil {
			groupIDs[groupID.(string)] = struct{}{}
		}
	}
	ids := make([]string, 0, len(groupIDs))
	for id := range groupIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (h *Handler) groupedChannelStates(ctx context.Context, groupID string) ([]groupedChannelState, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT id, group_id, sync_permissions, type = 'voice'
		FROM channels WHERE group_id = $1 ORDER BY id LIMIT $2`, groupID, maxChannelWrappedKeys+1)
	if err != nil {
		return nil, fmt.Errorf("query grouped channel authority states: %w", err)
	}
	defer h.closeRows(rows, "grouped channel authority states")
	states := []groupedChannelState{}
	for rows.Next() {
		var state groupedChannelState
		if err := rows.Scan(&state.ID, &state.GroupID, &state.SyncPermissions, &state.IsVoice); err != nil {
			return nil, fmt.Errorf("scan grouped channel authority state: %w", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate grouped channel authority states: %w", err)
	}
	if len(states) > maxChannelWrappedKeys {
		return nil, errChannelGroupChildLimit
	}
	return states, nil
}

func groupedChannelStatesTx(ctx context.Context, tx *sql.Tx, groupID string) ([]groupedChannelState, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, group_id, sync_permissions, type = 'voice'
		FROM channels WHERE group_id = $1 ORDER BY id LIMIT $2 FOR UPDATE`, groupID, maxChannelWrappedKeys+1)
	if err != nil {
		return nil, fmt.Errorf("lock grouped channel authority states: %w", err)
	}
	states := []groupedChannelState{}
	for rows.Next() {
		var state groupedChannelState
		if err := rows.Scan(&state.ID, &state.GroupID, &state.SyncPermissions, &state.IsVoice); err != nil {
			return nil, fmt.Errorf("scan locked grouped channel authority state: %w", errors.Join(err, rows.Close()))
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate locked grouped channel authority states: %w", errors.Join(err, rows.Close()))
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close locked grouped channel authority states: %w", err)
	}
	if len(states) > maxChannelWrappedKeys {
		return nil, errChannelGroupChildLimit
	}
	return states, nil
}

func (h *Handler) requestedChannelStates(ctx context.Context, serverID string, channelIDs []string) (map[string]groupedChannelState, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT id, group_id, sync_permissions, type = 'voice'
		FROM channels WHERE server_id = $1 AND id = ANY($2::uuid[]) ORDER BY id`, serverID, pq.Array(channelIDs))
	if err != nil {
		return nil, fmt.Errorf("query requested channel authority states: %w", err)
	}
	defer h.closeRows(rows, "requested channel authority states")
	states := make(map[string]groupedChannelState, len(channelIDs))
	for rows.Next() {
		var state groupedChannelState
		if err := rows.Scan(&state.ID, &state.GroupID, &state.SyncPermissions, &state.IsVoice); err != nil {
			return nil, fmt.Errorf("scan requested channel authority state: %w", err)
		}
		states[state.ID] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate requested channel authority states: %w", err)
	}
	if len(states) != len(channelIDs) {
		return nil, sql.ErrNoRows
	}
	return states, nil
}

func requestedChannelStatesTx(ctx context.Context, tx *sql.Tx, serverID string, channelIDs []string) (map[string]groupedChannelState, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, group_id, sync_permissions, type = 'voice'
		FROM channels WHERE server_id = $1 AND id = ANY($2::uuid[]) ORDER BY id FOR UPDATE`, serverID, pq.Array(channelIDs))
	if err != nil {
		return nil, fmt.Errorf("lock requested channel authority states: %w", err)
	}
	states := make(map[string]groupedChannelState, len(channelIDs))
	for rows.Next() {
		var state groupedChannelState
		if err := rows.Scan(&state.ID, &state.GroupID, &state.SyncPermissions, &state.IsVoice); err != nil {
			return nil, fmt.Errorf("scan locked requested channel authority state: %w", errors.Join(err, rows.Close()))
		}
		states[state.ID] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate locked requested channel authority states: %w", errors.Join(err, rows.Close()))
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close locked requested channel authority states: %w", err)
	}
	if len(states) != len(channelIDs) {
		return nil, sql.ErrNoRows
	}
	return states, nil
}

func authorityAffectedChannelIDs(req ReorderChannelsRequest, states map[string]groupedChannelState) (all []string, voice []string) {
	voice = []string{}
	for _, change := range req.Channels {
		state := states[change.ChannelID]
		if !state.SyncPermissions || sameOptionalGroupID(state.GroupID, change.GroupID) {
			continue
		}
		all = append(all, change.ChannelID)
		if state.IsVoice {
			voice = append(voice, change.ChannelID)
		}
	}
	sort.Strings(all)
	sort.Strings(voice)
	return all, voice
}

// ListChannelGroups returns all channel groups in a server
func (h *Handler) ListChannelGroups(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errInvalidServerID})
		return
	}

	// Check membership
	var isMember bool
	err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
		serverID, userID,
	).Scan(&isMember)
	if err != nil || !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not a member of this server"})
		return
	}

	rows, err := h.db.Query(
		`SELECT id, server_id, name, position, created_at, updated_at
		 FROM channel_groups
		 WHERE server_id = $1
		 ORDER BY position ASC, created_at ASC`,
		serverID,
	)
	if err != nil {
		h.log.Error("Failed to query channel groups", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channel groups"})
		return
	}
	defer func() { _ = rows.Close() }()

	groups := []models.ChannelGroup{}
	for rows.Next() {
		var g models.ChannelGroup
		if err := rows.Scan(&g.ID, &g.ServerID, &g.Name, &g.Position, &g.CreatedAt, &g.UpdatedAt); err != nil {
			h.log.Error("Failed to scan channel group", "error", err)
			continue
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating channel groups", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch channel groups"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"channel_groups": groups})
}

// CreateChannelGroup creates a new channel group in a server
func (h *Handler) CreateChannelGroup(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errInvalidServerID})
		return
	}

	var req CreateChannelGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errInvalidRequestBody})
		return
	}

	groupID := uuid.New().String()
	var group models.ChannelGroup
	tx, err := h.db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		h.log.Error("Begin channel group create transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedCreateGroup})
		return
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			h.log.Error(logMsgGroupRollbackFailed, "operation", "create", "error", rollbackErr)
		}
	}()
	if r := h.authorizeChannelGroupCreateTx(c, tx, serverID, userID); r != nil {
		h.refuseAfterRollback(c, tx, r, errFailedCreateGroup, "create")
		return
	}
	var maxPos int
	if err := tx.QueryRowContext(c.Request.Context(), `SELECT COALESCE(MAX(position), -1) FROM channel_groups WHERE server_id = $1`, serverID).Scan(&maxPos); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedCreateGroup})
		return
	}
	err = tx.QueryRowContext(c.Request.Context(),
		`INSERT INTO channel_groups (id, server_id, name, position, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, NOW(), NOW())
		 RETURNING id, server_id, name, position, created_at, updated_at`,
		groupID, serverID, req.Name, maxPos+1,
	).Scan(&group.ID, &group.ServerID, &group.Name, &group.Position, &group.CreatedAt, &group.UpdatedAt)
	if err != nil {
		h.log.Error(errFailedCreateGroup, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedCreateGroup})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Commit channel group create", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedCreateGroup})
		return
	}

	h.log.Info("Channel group created", "group_id", groupID, "server_id", serverID, "user_id", userID)

	h.broadcastChannelGroupCreated(serverID, group)

	c.JSON(http.StatusCreated, gin.H{"channel_group": group})
}

// UpdateChannelGroup updates a channel group's name or position
func (h *Handler) UpdateChannelGroup(c *gin.Context) {
	userID := c.GetString("user_id")
	groupID := c.Param("group_id")

	if _, err := uuid.Parse(groupID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid group ID"})
		return
	}

	var req UpdateChannelGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errInvalidRequestBody})
		return
	}

	// The initial lookup only chooses the server visibility lock; authorization
	// and the target row are locked and resolved again in the mutation tx.
	var serverID string
	err := h.db.QueryRow(`SELECT server_id FROM channel_groups WHERE id = $1`, groupID).Scan(&serverID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": errChannelGroupNotFound})
		return
	} else if err != nil {
		h.log.Error("Failed to fetch group", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedUpdateGroup})
		return
	}

	var group models.ChannelGroup
	tx, err := h.db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedUpdateGroup})
		return
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			h.log.Error(logMsgGroupRollbackFailed, "operation", "update", "error", rollbackErr)
		}
	}()
	if r := h.authorizeChannelGroupUpdateTx(c, tx, serverID, userID); r != nil {
		h.refuseAfterRollback(c, tx, r, errFailedUpdateGroup, "update")
		return
	}
	err = tx.QueryRowContext(c.Request.Context(),
		`UPDATE channel_groups
		 SET name = COALESCE($1, name),
		     position = COALESCE($2, position),
		     updated_at = NOW()
		 WHERE id = $3 AND server_id = $4
		 RETURNING id, server_id, name, position, created_at, updated_at`,
		req.Name, req.Position, groupID, serverID,
	).Scan(&group.ID, &group.ServerID, &group.Name, &group.Position, &group.CreatedAt, &group.UpdatedAt)
	if err != nil {
		h.log.Error(errFailedUpdateGroup, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedUpdateGroup})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Commit channel group update", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedUpdateGroup})
		return
	}

	h.log.Info("Channel group updated", "group_id", groupID, "user_id", userID)

	h.broadcastChannelGroupUpdated(serverID, group)

	c.JSON(http.StatusOK, gin.H{"channel_group": group})
}

// DeleteChannelGroup deletes a channel group. Channels in this group get group_id = NULL.
func (h *Handler) DeleteChannelGroup(c *gin.Context) {
	request, ok := h.prepareChannelGroupDelete(c)
	if !ok {
		return
	}
	serverID, groupID, userID, tokenEpoch := request.serverID, request.groupID, request.userID, request.tokenEpoch
	var lockedChannelIDs []string
	var plan rbac.PresenceRecheckPlan
	var rotations []keyrotation.Rotation
	var deniedByChannel map[string][]string
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		rotations = nil
		deniedByChannel = nil
		preflight, preflightErr := h.groupedChannelStates(c.Request.Context(), groupID)
		if preflightErr != nil {
			if errors.Is(preflightErr, errChannelGroupChildLimit) {
				c.JSON(http.StatusConflict, gin.H{"error": errChannelGroupTooLarge})
				return
			}
			h.log.Error("Failed to preflight channel group delete authority", "error", preflightErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedDeleteGroup})
			return
		}
		preflightIDs, preflightVoiceIDs := syncedGroupChannelIDs(preflight)
		preflightSet := channelGroupDeletePreflight{allIDs: preflightIDs, voiceIDs: preflightVoiceIDs}
		plan, err = h.authority.RunChannelAuthorityMutation(c.Request.Context(), serverID, preflightSet.voiceIDs,
			func(ctx context.Context, tx *sql.Tx) error {
				var mutationErr error
				lockedChannelIDs, rotations, deniedByChannel, mutationErr = h.deleteChannelGroupAuthorityTx(
					ctx, tx, serverID, groupID, userID, tokenEpoch, preflightSet,
				)
				return mutationErr
			}, userID,
		)
		if errors.Is(err, errChannelAuthoritySetChanged) {
			continue
		}
		break
	}
	if err != nil {
		h.respondChannelGroupDeleteMutationError(c, serverID, lockedChannelIDs, err)
		return
	}
	h.authority.CompleteChannelAuthorityMutationWithRotations(c.Request.Context(), serverID, lockedChannelIDs, plan, rotations, deniedByChannel)

	h.log.Info("Channel group deleted", "group_id", groupID, "user_id", userID)

	if h.hub != nil {
		if serverUUID, err := uuid.Parse(serverID); err == nil {
			h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
				Type: "channel_group_deleted",
				Data: map[string]interface{}{
					"group_id":  groupID,
					"server_id": serverID,
				},
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Channel group deleted"})
}

func (h *Handler) prepareChannelGroupDelete(c *gin.Context) (channelGroupDeleteRequest, bool) {
	request := channelGroupDeleteRequest{
		userID:     c.GetString("user_id"),
		tokenEpoch: middleware.TokenCredentialEpoch(c),
		groupID:    c.Param("group_id"),
	}
	if _, err := uuid.Parse(request.groupID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid group ID"})
		return channelGroupDeleteRequest{}, false
	}
	if err := h.db.QueryRow(`SELECT server_id FROM channel_groups WHERE id = $1`, request.groupID).Scan(&request.serverID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": errChannelGroupNotFound})
		} else {
			h.log.Error("Failed to fetch group", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedDeleteGroup})
		}
		return channelGroupDeleteRequest{}, false
	}
	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), request.serverID, request.userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(errFailedCheckPerms, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedDeleteGroup})
		return channelGroupDeleteRequest{}, false
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errInsufficientPerms})
		return channelGroupDeleteRequest{}, false
	}
	if h.authority == nil {
		h.log.Error("Channel authority coordinator unavailable")
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedDeleteGroup})
		return channelGroupDeleteRequest{}, false
	}
	return request, true
}

func (h *Handler) respondChannelGroupDeleteMutationError(c *gin.Context, serverID string, lockedChannelIDs []string, err error) {
	if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgAuthRequired})
		return
	}
	if errors.Is(err, errManageChannelsDenied) || errors.Is(err, rbac.ErrNotMember) {
		c.JSON(http.StatusForbidden, gin.H{"error": errInsufficientPerms})
		return
	}
	if errors.Is(err, rbac.ErrTemporaryChannelOverrideManaged) {
		c.JSON(http.StatusConflict, gin.H{"error": errTemporaryOverrideManaged})
		return
	}
	if errors.Is(err, errChannelAuthoritySetChanged) {
		c.JSON(http.StatusConflict, gin.H{"error": errChannelAuthorityRetry})
		return
	}
	if errors.Is(err, errChannelGroupChildLimit) {
		c.JSON(http.StatusConflict, gin.H{"error": errChannelGroupTooLarge})
		return
	}
	if rbac.IsAmbiguousAuthorityCommit(err) {
		h.authority.FailClosedChannelAuthorityMutation(c.Request.Context(), serverID, lockedChannelIDs)
	}
	h.log.Error(errFailedDeleteGroup, "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedDeleteGroup})
}

func (h *Handler) deleteChannelGroupAuthorityTx(ctx context.Context, tx *sql.Tx, serverID, groupID, userID, tokenEpoch string, preflight channelGroupDeletePreflight) ([]string, []keyrotation.Rotation, map[string][]string, error) {
	actualIDs, err := h.lockAndAuthorizeChannelGroupDeleteTx(ctx, tx, serverID, groupID, userID, tokenEpoch, preflight)
	if err != nil {
		return nil, nil, nil, err
	}
	candidates, err := rbac.CaptureChannelKeyCandidatesTx(ctx, tx, actualIDs, maxChannelWrappedKeys)
	if err != nil {
		return actualIDs, nil, nil, err
	}
	if err := deleteChannelGroupChildrenTx(ctx, tx, serverID, groupID, actualIDs); err != nil {
		return actualIDs, nil, nil, err
	}
	rotations, deniedByChannel, err := h.authority.RevokeDeniedChannelKeyCandidatesTx(ctx, tx, serverID, userID, actualIDs, candidates)
	if err != nil {
		return actualIDs, nil, nil, err
	}
	return actualIDs, rotations, deniedByChannel, nil
}

func (h *Handler) lockAndAuthorizeChannelGroupDeleteTx(ctx context.Context, tx *sql.Tx, serverID, groupID, userID, tokenEpoch string, preflight channelGroupDeletePreflight) ([]string, error) {
	if err := credepoch.GuardTx(ctx, tx, userID, tokenEpoch); err != nil {
		return nil, err
	}
	if err := lockChannelGroupForDeleteTx(ctx, tx, serverID, groupID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errChannelAuthoritySetChanged
		}
		return nil, err
	}
	locked, err := groupedChannelStatesTx(ctx, tx, groupID)
	if err != nil {
		return nil, err
	}
	actualIDs, actualVoiceIDs := syncedGroupChannelIDs(locked)
	if !sameChannelIDSet(preflight.allIDs, actualIDs) || !sameChannelIDSet(preflight.voiceIDs, actualVoiceIDs) {
		return nil, errChannelAuthoritySetChanged
	}
	actorPerms, err := h.resolver.ResolveServerPermissionsTx(ctx, tx, serverID, userID)
	if err != nil {
		return nil, fmt.Errorf("resolve current channel group delete actor permissions: %w", err)
	}
	if !actorPerms.Has(rbac.PermManageChannels) {
		return nil, errManageChannelsDenied
	}
	protected, err := rbac.HasTemporaryMoveGrantForChannelsTx(ctx, tx, actualIDs)
	if err != nil {
		return nil, err
	}
	if protected {
		return nil, rbac.ErrTemporaryChannelOverrideManaged
	}
	return actualIDs, nil
}

func deleteChannelGroupChildrenTx(ctx context.Context, tx *sql.Tx, serverID, groupID string, channelIDs []string) error {
	if len(channelIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM channel_permission_overrides WHERE channel_id = ANY($1::uuid[])`, pq.Array(channelIDs)); err != nil {
			return fmt.Errorf("clear deleted category channel overrides: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE channels SET group_id = NULL, sync_permissions = FALSE, updated_at = NOW()
		WHERE group_id = $1`, groupID); err != nil {
		return fmt.Errorf("detach deleted channel group children: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM channel_groups WHERE id = $1 AND server_id = $2`, groupID, serverID)
	if err != nil {
		return fmt.Errorf("delete channel group: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count deleted channel group: %w", err)
	}
	if deleted != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// closeRows closes a result set, logging any close error. Callers defer it so
// the "check every error" rule is honored without adding a conditional (and its
// cognitive complexity) to a deferred closure at each call site. The what arg
// labels the originating query in the log line.
func (h *Handler) closeRows(rows *sql.Rows, what string) {
	if err := rows.Close(); err != nil {
		h.log.Error("Failed to close rows", "query", what, "error", err)
	}
}

// groupRefusal is the answer a channel-group transaction reached before its
// write. The authorization helpers return it rather than writing it, because
// it may only be written once the transaction is known to be discarded.
type groupRefusal struct {
	status int
	msg    string
	// cause is why a 500 refusal was reached, logged before the discard.
	cause error
}

// groupFault is the route's 500, carrying the error that caused it.
func groupFault(msg string, cause error) *groupRefusal {
	return &groupRefusal{status: http.StatusInternalServerError, msg: msg, cause: cause}
}

// refuseAfterRollback discards tx and then writes the refusal it reached. A
// refusal describes what the transaction read; a discard that neither
// succeeded nor found the transaction resolved leaves its fate unknown, so the
// answer is the route's fault rather than a clean 403 or 404 (#3508).
func (h *Handler) refuseAfterRollback(c *gin.Context, tx *sql.Tx, r *groupRefusal, faultMsg, operation string) {
	if r.cause != nil {
		h.log.Error("Channel group authorization failed", "operation", operation, "error", r.cause)
	}
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		h.log.Error(logMsgGroupRollbackFailed, "operation", operation, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": faultMsg})
		return
	}
	c.JSON(r.status, gin.H{"error": r.msg})
}

// guardChannelGroupTx runs the credential-epoch guard for a channel-group
// transaction and returns the refusal when it fails. goneMessage is the route's
// vanished-server answer: an owner's erasure deletes their users row and,
// through servers.owner_id's ON DELETE CASCADE, the server and its groups, so
// an owner acting mid-erasure fails the guard's read before the server lock
// below can see the server is gone. Any other guard failure, and a failed
// server read after a missing row, is a 500 carrying its cause.
func guardChannelGroupTx(c *gin.Context, tx *sql.Tx, serverID, userID, goneMessage, faultMessage string) *groupRefusal {
	err := credepoch.GuardTx(c.Request.Context(), tx, userID, middleware.TokenCredentialEpoch(c))
	if err == nil {
		return nil
	}
	if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked) {
		return &groupRefusal{status: http.StatusUnauthorized, msg: "Authentication required"}
	}
	gone, probeErr := rbac.GuardActorGoneWithServer(c.Request.Context(), tx, serverID, err)
	switch {
	case probeErr != nil:
		return groupFault(faultMessage, probeErr)
	case gone:
		return &groupRefusal{status: http.StatusNotFound, msg: goneMessage}
	default:
		return groupFault(faultMessage, fmt.Errorf("channel group epoch guard: %w", err))
	}
}

// lockGroupServerTx takes the servers row lock. Only the advisory lock and the
// epoch guard precede it, and neither holds the servers row, so a server
// deleted since the request began arrives here with no row: that is the
// route's vanished answer (goneMessage), because its groups went with it
// (ON DELETE CASCADE). Only sql.ErrNoRows is reclassified; any other error
// stays a 500.
func lockGroupServerTx(ctx context.Context, tx *sql.Tx, serverID, goneMessage, faultMessage string) *groupRefusal {
	err := tx.QueryRowContext(ctx, `SELECT id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(new(string))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return &groupRefusal{status: http.StatusNotFound, msg: goneMessage}
	default:
		return groupFault(faultMessage, fmt.Errorf("lock channel group server: %w", err))
	}
}

// groupManagerRefusalTx checks the actor's membership row and then Manage
// Channels, both inside the transaction.
func (h *Handler) groupManagerRefusalTx(ctx context.Context, tx *sql.Tx, serverID, userID, faultMessage string) *groupRefusal {
	err := tx.QueryRowContext(ctx, `SELECT user_id FROM server_members WHERE server_id = $1 AND user_id = $2 FOR SHARE`, serverID, userID).Scan(new(string))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return &groupRefusal{status: http.StatusForbidden, msg: errInsufficientPerms}
	case err != nil:
		return groupFault(faultMessage, fmt.Errorf("check channel group membership: %w", err))
	}
	perms, err := h.resolver.ResolveServerPermissionsTx(ctx, tx, serverID, userID)
	switch {
	case err == nil && perms.Has(rbac.PermManageChannels):
		return nil
	case err == nil, errors.Is(err, rbac.ErrNotMember):
		return &groupRefusal{status: http.StatusForbidden, msg: errInsufficientPerms}
	default:
		return groupFault(faultMessage, fmt.Errorf("resolve channel group permissions: %w", err))
	}
}

// authorizeChannelGroupCreateTx takes CreateChannelGroup's locks and checks in
// order: the visibility advisory lock, the credential epoch, the servers row,
// the actor's membership, and Manage Channels. It returns the refusal from the
// first one that fails, or nil.
func (h *Handler) authorizeChannelGroupCreateTx(c *gin.Context, tx *sql.Tx, serverID, userID string) *groupRefusal {
	if err := rbac.LockServerVisibilityCapture(c.Request.Context(), tx, serverID); err != nil {
		return groupFault(errFailedCreateGroup, fmt.Errorf("lock channel group create visibility: %w", err))
	}
	if r := guardChannelGroupTx(c, tx, serverID, userID, "Server not found", errFailedCreateGroup); r != nil {
		return r
	}
	if r := lockGroupServerTx(c.Request.Context(), tx, serverID, "Server not found", errFailedCreateGroup); r != nil {
		return r
	}
	return h.groupManagerRefusalTx(c.Request.Context(), tx, serverID, userID, errFailedCreateGroup)
}

// authorizeChannelGroupUpdateTx is authorizeChannelGroupCreateTx for
// UpdateChannelGroup, whose vanished answer is the preflight's own for a
// missing group: a server deleted since the preflight read of the group took
// the group with it.
func (h *Handler) authorizeChannelGroupUpdateTx(c *gin.Context, tx *sql.Tx, serverID, userID string) *groupRefusal {
	if err := rbac.LockServerVisibilityCapture(c.Request.Context(), tx, serverID); err != nil {
		return groupFault(errFailedUpdateGroup, fmt.Errorf("lock channel group update visibility: %w", err))
	}
	if r := guardChannelGroupTx(c, tx, serverID, userID, errChannelGroupNotFound, errFailedUpdateGroup); r != nil {
		return r
	}
	if r := lockGroupServerTx(c.Request.Context(), tx, serverID, errChannelGroupNotFound, errFailedUpdateGroup); r != nil {
		return r
	}
	return h.groupManagerRefusalTx(c.Request.Context(), tx, serverID, userID, errFailedUpdateGroup)
}

func (h *Handler) broadcastChannelGroupCreated(serverID string, group models.ChannelGroup) {
	h.broadcastChannelGroup(serverID, "channel_group_created", map[string]interface{}{
		"id": group.ID, "server_id": group.ServerID, "name": group.Name, "position": group.Position,
		"created_at": group.CreatedAt, "updated_at": group.UpdatedAt,
	})
}

func (h *Handler) broadcastChannelGroupUpdated(serverID string, group models.ChannelGroup) {
	h.broadcastChannelGroup(serverID, "channel_group_updated", map[string]interface{}{
		"id": group.ID, "server_id": group.ServerID, "name": group.Name, "position": group.Position,
		"updated_at": group.UpdatedAt,
	})
}

func (h *Handler) broadcastChannelGroup(serverID, event string, group map[string]interface{}) {
	if h.hub == nil {
		return
	}
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{Type: event, Data: map[string]interface{}{"channel_group": group}})
}

// validateReorderGroupOwnership verifies every target group_id in the reorder
// request belongs to serverID (CV-CAN-012). On a lookup error it writes a 500;
// on a missing, malformed, or cross-server group it writes a 400. The distinct
// non-null group_ids are resolved in a single round-trip so a bulk drag-and-drop
// that moves many channels (often into the same category) does not fan out into
// N sequential lookups.
func (h *Handler) validateReorderGroupOwnership(c *gin.Context, req ReorderChannelsRequest, serverID string) bool {
	// Collect the distinct target group_ids. nil/empty is uncategorized and
	// always allowed (mirrors groupBelongsToServer); dedupe so moving several
	// channels into the same category costs one lookup, not one per channel.
	wanted := make(map[string]struct{})
	for _, cp := range req.Channels {
		if cp.GroupID == nil || *cp.GroupID == "" {
			continue
		}
		// A malformed (non-UUID) group_id is a client input error, not a server
		// fault: reject it as a bad binding (400) before it reaches the
		// uuid-typed query, which would otherwise fail as a 500.
		if _, err := uuid.Parse(*cp.GroupID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errMsgForeignGroup})
			return false
		}
		wanted[*cp.GroupID] = struct{}{}
	}
	if len(wanted) == 0 {
		return true
	}

	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}

	rows, err := h.db.QueryContext(c.Request.Context(),
		`SELECT id::text FROM channel_groups WHERE id = ANY($1::uuid[]) AND server_id = $2`,
		pq.Array(ids), serverID,
	)
	if err != nil {
		h.log.Error("Failed to validate reorder group ownership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
		return false
	}
	defer h.closeRows(rows, "reorder group ownership")

	// id is the primary key, so each distinct requested id matches at most one
	// row, and the server_id predicate means every returned row is same-server.
	found := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.log.Error("Failed to scan reorder group ownership", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
			return false
		}
		found++
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Failed to read reorder group ownership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
		return false
	}

	// A shortfall means at least one requested group is missing or cross-server.
	if found != len(wanted) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgForeignGroup})
		return false
	}
	return true
}

// ReorderChannels bulk-updates channel positions and group assignments.
// Used for drag-and-drop reordering between groups.
func (h *Handler) ReorderChannels(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")

	if _, err := uuid.Parse(serverID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errInvalidServerID})
		return
	}

	var req ReorderChannelsRequest
	if !bindStrictJSONBody(c, &req, maxChannelWrappedKeysRequestBytes) {
		return
	}
	if !normalizeReorderChannelsRequest(&req) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errInvalidRequestBody})
		return
	}

	hasPerm, err := h.resolver.HasPermission(c.Request.Context(), serverID, userID, "", rbac.PermManageChannels)
	if err != nil {
		h.log.Error(errFailedCheckPerms, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
		return
	}
	if !hasPerm {
		c.JSON(http.StatusForbidden, gin.H{"error": errInsufficientPerms})
		return
	}

	// CV-CAN-012: every non-null target group_id must belong to this server — a
	// bulk reorder must not assign a channel to a category from another server
	// (the permission-sync cascade keys on group_id with no server predicate).
	if !h.validateReorderGroupOwnership(c, req, serverID) {
		return
	}
	h.reorderSyncedChannels(c, serverID, userID, req)
}

func normalizeReorderChannelsRequest(req *ReorderChannelsRequest) bool {
	if len(req.Channels) > maxChannelWrappedKeys {
		return false
	}
	for i := range req.Channels {
		channelID, err := uuid.Parse(req.Channels[i].ChannelID)
		if err != nil {
			return false
		}
		req.Channels[i].ChannelID = channelID.String()
		if req.Channels[i].GroupID == nil || *req.Channels[i].GroupID == "" {
			continue
		}
		groupID, err := uuid.Parse(*req.Channels[i].GroupID)
		if err != nil {
			return false
		}
		canonicalGroupID := groupID.String()
		req.Channels[i].GroupID = &canonicalGroupID
	}
	seenChannelIDs := make(map[string]struct{}, len(req.Channels))
	for _, channel := range req.Channels {
		if _, seen := seenChannelIDs[channel.ChannelID]; seen {
			return false
		}
		seenChannelIDs[channel.ChannelID] = struct{}{}
	}
	return len(req.Channels) != 0
}

func channelIDsFromReorder(req ReorderChannelsRequest) []string {
	ids := make([]string, 0, len(req.Channels))
	for _, change := range req.Channels {
		ids = append(ids, change.ChannelID)
	}
	sort.Strings(ids)
	return ids
}

func (h *Handler) reorderSyncedChannels(c *gin.Context, serverID, userID string, req ReorderChannelsRequest) {
	if h.authority == nil {
		h.log.Error("Channel authority coordinator unavailable")
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
		return
	}
	channelIDs := channelIDsFromReorder(req)
	tokenEpoch := middleware.TokenCredentialEpoch(c)
	var (
		plan            rbac.PresenceRecheckPlan
		affected        []string
		rotations       []keyrotation.Rotation
		deniedByChannel map[string][]string
		err             error
	)
	for attempt := 0; attempt < 2; attempt++ {
		rotations = nil
		deniedByChannel = nil
		preflight, preflightErr := h.requestedChannelStates(c.Request.Context(), serverID, channelIDs)
		if preflightErr != nil {
			if errors.Is(preflightErr, sql.ErrNoRows) {
				c.JSON(http.StatusBadRequest, gin.H{"error": errMsgChannelNotFound})
				return
			}
			err = preflightErr
			break
		}
		_, voiceIDs := authorityAffectedChannelIDs(req, preflight)
		authorityRequest := reorderChannelAuthorityRequest{
			serverID: serverID, userID: userID, tokenEpoch: tokenEpoch,
			reorder: req, channelIDs: channelIDs, preflight: preflight,
		}
		plan, err = h.authority.RunChannelAuthorityMutation(c.Request.Context(), serverID, voiceIDs,
			func(ctx context.Context, tx *sql.Tx) error {
				var mutationErr error
				affected, rotations, deniedByChannel, mutationErr = h.reorderChannelAuthorityTx(ctx, tx, authorityRequest)
				return mutationErr
			}, userID,
		)
		if errors.Is(err, errChannelAuthoritySetChanged) {
			continue
		}
		break
	}
	if err != nil {
		h.respondReorderSyncedError(c, serverID, affected, err)
		return
	}
	h.authority.CompleteChannelAuthorityMutationWithRotations(c.Request.Context(), serverID, affected, plan, rotations, deniedByChannel)
	remaining := filterDeletedReorderedChannels(req.Channels, authorityRotationDeletedChannelIDs(rotations))
	if len(remaining) > 0 {
		h.broadcastChannelsReordered(serverID, remaining)
	}
	h.log.Info("Channels reordered", "server_id", serverID, "user_id", userID, "count", len(req.Channels))
	c.JSON(http.StatusOK, gin.H{"message": "Channels reordered"})
}

// respondReorderSyncedError answers a failed synchronized reorder. An ambiguous
// commit fails the affected channels closed before the 500, as it did inline.
func (h *Handler) respondReorderSyncedError(c *gin.Context, serverID string, affected []string, err error) {
	if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgAuthRequired})
		return
	}
	if errors.Is(err, errManageChannelsDenied) || errors.Is(err, rbac.ErrNotMember) {
		c.JSON(http.StatusForbidden, gin.H{"error": errInsufficientPerms})
		return
	}
	if errors.Is(err, rbac.ErrTemporaryChannelOverrideManaged) {
		c.JSON(http.StatusConflict, gin.H{"error": errTemporaryOverrideManaged})
		return
	}
	if errors.Is(err, errChannelAuthoritySetChanged) {
		c.JSON(http.StatusConflict, gin.H{"error": errChannelAuthorityRetry})
		return
	}
	if rbac.IsAmbiguousAuthorityCommit(err) {
		h.authority.FailClosedChannelAuthorityMutation(c.Request.Context(), serverID, affected)
	}
	h.log.Error("Failed to reorder synchronized channels", "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedReorderChannels})
}

func (h *Handler) reorderChannelAuthorityTx(ctx context.Context, tx *sql.Tx, request reorderChannelAuthorityRequest) ([]string, []keyrotation.Rotation, map[string][]string, error) {
	locked, err := h.lockAndAuthorizeChannelReorderTx(ctx, tx, request)
	if err != nil {
		return nil, nil, nil, err
	}
	affected, _ := authorityAffectedChannelIDs(request.reorder, locked)
	protected, err := rbac.HasTemporaryMoveGrantForChannelsTx(ctx, tx, affected)
	if err != nil {
		return affected, nil, nil, err
	}
	if protected {
		return affected, nil, nil, rbac.ErrTemporaryChannelOverrideManaged
	}
	candidates, err := rbac.CaptureChannelKeyCandidatesTx(ctx, tx, affected, maxChannelWrappedKeys)
	if err != nil {
		return affected, nil, nil, err
	}
	if err := applyReorderedChannelChangesTx(ctx, tx, request.serverID, request.reorder, locked); err != nil {
		return affected, nil, nil, err
	}
	rotations, deniedByChannel, err := h.authority.RevokeDeniedChannelKeyCandidatesTx(ctx, tx, request.serverID, request.userID, affected, candidates)
	if err != nil {
		return affected, nil, nil, err
	}
	return affected, rotations, deniedByChannel, nil
}

func (h *Handler) lockAndAuthorizeChannelReorderTx(ctx context.Context, tx *sql.Tx, request reorderChannelAuthorityRequest) (map[string]groupedChannelState, error) {
	if err := credepoch.GuardTx(ctx, tx, request.userID, request.tokenEpoch); err != nil {
		return nil, err
	}
	if err := lockChannelGroupsTx(ctx, tx, request.serverID, groupIDsForReorder(request.reorder, request.preflight)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errChannelAuthoritySetChanged
		}
		return nil, err
	}
	locked, err := requestedChannelStatesTx(ctx, tx, request.serverID, request.channelIDs)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errChannelAuthoritySetChanged
		}
		return nil, err
	}
	if !sameChannelStates(request.preflight, locked) {
		return nil, errChannelAuthoritySetChanged
	}
	actorPerms, err := h.resolver.ResolveServerPermissionsTx(ctx, tx, request.serverID, request.userID)
	if err != nil {
		return nil, fmt.Errorf("resolve current channel reorder actor permissions: %w", err)
	}
	if !actorPerms.Has(rbac.PermManageChannels) {
		return nil, errManageChannelsDenied
	}
	return locked, nil
}

func sameChannelStates(expected, actual map[string]groupedChannelState) bool {
	for id, left := range expected {
		right, exists := actual[id]
		if !exists || left.SyncPermissions != right.SyncPermissions || left.IsVoice != right.IsVoice || !sameOptionalGroupID(left.GroupID, right.GroupID) {
			return false
		}
	}
	return len(expected) == len(actual)
}

func applyReorderedChannelChangesTx(ctx context.Context, tx *sql.Tx, serverID string, req ReorderChannelsRequest, states map[string]groupedChannelState) error {
	for _, change := range req.Channels {
		if err := applyReorderedChannelChangeTx(ctx, tx, serverID, change, states[change.ChannelID]); err != nil {
			return err
		}
	}
	return nil
}

func applyReorderedChannelChangeTx(ctx context.Context, tx *sql.Tx, serverID string, change ChannelPosition, state groupedChannelState) error {
	if state.SyncPermissions && !sameOptionalGroupID(state.GroupID, change.GroupID) {
		return applyReorderedSynchronizedChannelChangeTx(ctx, tx, serverID, change)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE channels SET group_id = $1, position = $2, updated_at = NOW()
		WHERE id = $3 AND server_id = $4`, resolveGroupIDParam(change.GroupID), change.Position, change.ChannelID, serverID); err != nil {
		return fmt.Errorf("update reordered channel position: %w", err)
	}
	return nil
}

func applyReorderedSynchronizedChannelChangeTx(ctx context.Context, tx *sql.Tx, serverID string, change ChannelPosition) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_permission_overrides WHERE channel_id = $1`, change.ChannelID); err != nil {
		return fmt.Errorf("clear reordered synchronized channel overrides: %w", err)
	}
	if resolveGroupIDParam(change.GroupID) == nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE channels SET group_id = NULL, sync_permissions = FALSE, position = $1, updated_at = NOW()
			WHERE id = $2 AND server_id = $3`, change.Position, change.ChannelID, serverID); err != nil {
			return fmt.Errorf("detach reordered synchronized channel: %w", err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE channels SET group_id = $1, position = $2, updated_at = NOW()
		WHERE id = $3 AND server_id = $4`, resolveGroupIDParam(change.GroupID), change.Position, change.ChannelID, serverID); err != nil {
		return fmt.Errorf("move reordered synchronized channel: %w", err)
	}
	if err := rbac.ReplaceCategoryOverridesForChannelsTx(ctx, tx, *change.GroupID, []string{change.ChannelID}); err != nil {
		return fmt.Errorf("materialize reordered category overrides: %w", err)
	}
	return nil
}

// broadcastChannelsReordered emits a channels_reordered event to the server's
// subscribers (best-effort; no-op when the hub is unset or serverID is unparseable).
func (h *Handler) broadcastChannelsReordered(serverID string, channels []ChannelPosition) {
	if h.hub == nil {
		return
	}
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "channels_reordered",
		Data: map[string]interface{}{
			"server_id": serverID,
			"channels":  channels,
		},
	})
}
