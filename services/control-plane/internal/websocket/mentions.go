package websocket

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/vmihailenco/msgpack/v5"
)

// Mention addendum size limits to prevent DoS via oversized blobs.
const (
	maxMentionMetaBytes = 4096            // Max base64-encoded length of mention_meta string (checked before decoding)
	maxMentionUsers     = 50              // Max individual user mentions per message
	maxMentionRoles     = 10              // Max role mentions per message
	mentionCtxTimeout   = 3 * time.Second // Timeout for RBAC permission checks
)

// MentionAddendum carries mention routing data alongside a message.
// It is decoded from a binary (msgpack) blob, processed ephemerally for
// RBAC enforcement and notification routing, then wiped from memory.
// It is NEVER persisted to the database or included in broadcast payloads.
type MentionAddendum struct {
	Users    []string `msgpack:"u,omitempty"` // Mentioned user UUIDs
	Roles    []string `msgpack:"r,omitempty"` // Mentioned role UUIDs
	Everyone bool     `msgpack:"e,omitempty"` // @all mention
	Here     bool     `msgpack:"h,omitempty"` // @here mention
}

// Wipe zeroes all identifying data in the addendum to minimize the window
// where mention targets exist in server memory.
func (a *MentionAddendum) Wipe() {
	for i := range a.Users {
		a.Users[i] = ""
	}
	a.Users = nil
	for i := range a.Roles {
		a.Roles[i] = ""
	}
	a.Roles = nil
	a.Everyone = false
	a.Here = false
}

// IsEmpty returns true if the addendum contains no mention targets.
func (a *MentionAddendum) IsEmpty() bool {
	return len(a.Users) == 0 && len(a.Roles) == 0 && !a.Everyone && !a.Here
}

// decodeMentionMeta decodes a base64-encoded msgpack blob into a MentionAddendum.
// Returns nil if the input is empty, malformed, oversized, or decoding fails.
func decodeMentionMeta(raw string) *MentionAddendum {
	if raw == "" {
		return nil
	}
	// Size gate: reject before decoding to prevent allocation abuse.
	// No log — this is client-controlled and would enable log spam.
	if len(raw) > maxMentionMetaBytes {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil
	}
	// Unmarshal into the fixed struct (known field types: []string and bool).
	// The 4KB blob size cap above bounds the worst-case allocation — a 4KB msgpack
	// blob can produce at most ~4KB of decoded strings, not unbounded memory.
	var addendum MentionAddendum
	if err := msgpack.Unmarshal(data, &addendum); err != nil {
		return nil
	}
	if addendum.IsEmpty() {
		return nil
	}
	// Cap target counts to prevent abuse
	if len(addendum.Users) > maxMentionUsers {
		addendum.Users = addendum.Users[:maxMentionUsers]
	}
	if len(addendum.Roles) > maxMentionRoles {
		addendum.Roles = addendum.Roles[:maxMentionRoles]
	}
	// Validate all IDs are valid UUIDs — these come from an untrusted client.
	// Reject malformed strings before they reach any SQL query.
	addendum.Users = filterValidUUIDs(addendum.Users)
	addendum.Roles = filterValidUUIDs(addendum.Roles)
	if addendum.IsEmpty() {
		return nil
	}
	return &addendum
}

// MentionPermissionChecker is the interface the Hub needs from the RBAC resolver
// to enforce mention permissions. This avoids importing the rbac package directly.
// Implementations should accept the permission bit as an int64 (the underlying type
// of rbac.Permission). Use MentionCheckerAdapter to wrap an rbac.Resolver.
type MentionPermissionChecker interface {
	HasMentionPermission(ctx context.Context, serverID, userID, channelID string, permBit int64) (bool, error)
}

// RBAC permission bits for mentions (must match rbac/types.go)
const (
	permMentionEveryone int64 = 1 << 24
	permMentionRoles    int64 = 1 << 26
	permMentionUsers    int64 = 1 << 27
)

// enforceMentionPermissions strips unauthorized mention types from the addendum.
// The message still sends — mentions just don't route notifications.
func (h *Hub) enforceMentionPermissions(
	serverID, userID, channelID string,
	addendum *MentionAddendum,
) {
	if addendum == nil || h.mentionChecker == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), mentionCtxTimeout)
	defer cancel()

	h.enforceEveryonePerm(ctx, serverID, userID, channelID, addendum)
	h.enforceUserMentionPerm(ctx, serverID, userID, channelID, addendum)
	h.enforceRoleMentionPerm(ctx, serverID, userID, channelID, addendum)
}

// enforceEveryonePerm strips @all/@here if the user lacks PermMentionEveryone.
func (h *Hub) enforceEveryonePerm(ctx context.Context, serverID, userID, channelID string, addendum *MentionAddendum) {
	if !addendum.Everyone && !addendum.Here {
		return
	}
	hasPerm, err := h.mentionChecker.HasMentionPermission(ctx, serverID, userID, channelID, permMentionEveryone)
	if err != nil {
		log.Printf("Failed to check PermMentionEveryone: %v", err)
	}
	if err != nil || !hasPerm {
		addendum.Everyone = false
		addendum.Here = false
	}
}

// enforceUserMentionPerm strips @user mentions if the user lacks PermMentionUsers.
func (h *Hub) enforceUserMentionPerm(ctx context.Context, serverID, userID, channelID string, addendum *MentionAddendum) {
	if len(addendum.Users) == 0 {
		return
	}
	hasPerm, err := h.mentionChecker.HasMentionPermission(ctx, serverID, userID, channelID, permMentionUsers)
	if err != nil {
		log.Printf("Failed to check PermMentionUsers: %v", err)
	}
	if err != nil || !hasPerm {
		addendum.Users = nil
	}
}

// enforceRoleMentionPerm strips @role mentions if the user lacks PermMentionRoles,
// and filters to only mentionable roles scoped to this server.
func (h *Hub) enforceRoleMentionPerm(ctx context.Context, serverID, userID, channelID string, addendum *MentionAddendum) {
	if len(addendum.Roles) == 0 {
		return
	}
	hasPerm, err := h.mentionChecker.HasMentionPermission(ctx, serverID, userID, channelID, permMentionRoles)
	if err != nil {
		log.Printf("Failed to check PermMentionRoles: %v", err)
	}
	if err != nil || !hasPerm {
		addendum.Roles = nil
	}
	if len(addendum.Roles) > 0 {
		addendum.Roles = h.filterMentionableRoles(serverID, addendum.Roles)
	}
}

// filterMentionableRoles removes roles that don't have mentionable = true
// or don't belong to the given server (prevents cross-server role injection).
func (h *Hub) filterMentionableRoles(serverID string, roleIDs []string) []string {
	if len(roleIDs) == 0 {
		return nil
	}

	rows, err := h.db.Query(
		`SELECT id::text FROM roles WHERE id = ANY($1::uuid[]) AND mentionable = true AND server_id = $2`,
		uuidArrayParam(roleIDs), serverID,
	)
	if err != nil {
		log.Printf("Failed to filter mentionable roles: %v", err)
		return nil
	}
	defer func() { _ = rows.Close() }()

	mentionable := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			mentionable[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating mentionable roles: %v", err)
	}

	filtered := make([]string, 0, len(roleIDs))
	for _, id := range roleIDs {
		if mentionable[id] {
			filtered = append(filtered, id)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// filterValidUUIDs returns only the strings that parse as valid UUIDs.
// Client-supplied IDs are untrusted — this prevents malformed strings from
// reaching SQL queries or breaking PostgreSQL array literal syntax.
func filterValidUUIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, err := uuid.Parse(id); err == nil {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return nil
	}
	return valid
}

// uuidArrayParam formats a string slice as a PostgreSQL UUID array literal.
func uuidArrayParam(ids []string) string {
	result := "{"
	for i, id := range ids {
		if i > 0 {
			result += ","
		}
		result += id
	}
	result += "}"
	return result
}

// routeMentionNotifications sends enhanced unread_notify messages to mentioned users.
// This resolves mention targets (direct users, role members, everyone/here) and sends
// a "mentioned: true" flag on the notification so clients can differentiate mentions
// from regular unreads.
func (h *Hub) routeMentionNotifications(
	serverID, channelID uuid.UUID,
	senderUserID uuid.UUID,
	addendum *MentionAddendum,
	viewPerm int64,
) {
	if addendum == nil || addendum.IsEmpty() {
		return
	}

	// Collect all user IDs that should receive mention notifications
	mentionedUserIDs := make(map[uuid.UUID]bool)

	h.resolveDirectUserMentions(serverID, senderUserID, addendum, mentionedUserIDs)
	h.resolveRoleMentions(serverID, senderUserID, addendum, mentionedUserIDs)
	h.resolveEveryoneMentions(serverID, senderUserID, addendum, mentionedUserIDs)

	if len(mentionedUserIDs) == 0 {
		return
	}

	// Send mention-enhanced unread_notify to mentioned users who are connected
	// but NOT subscribed to the channel (they'll detect it client-side if subscribed)
	h.sendMentionNotify(serverID, channelID, mentionedUserIDs, addendum.Everyone, addendum.Here, viewPerm)
}

// resolveDirectUserMentions validates direct @user mentions against server membership
// to prevent a modified client from routing notifications to arbitrary non-member users.
func (h *Hub) resolveDirectUserMentions(
	serverID, senderUserID uuid.UUID,
	addendum *MentionAddendum,
	mentionedUserIDs map[uuid.UUID]bool,
) {
	if len(addendum.Users) == 0 {
		return
	}

	rows, err := h.db.Query(
		`SELECT user_id FROM server_members WHERE server_id = $1 AND user_id = ANY($2::uuid[])`,
		serverID, uuidArrayParam(addendum.Users),
	)
	if err != nil {
		log.Printf("Failed to validate mentioned users as server members: %v", err)
		return
	}
	validMembers := make(map[string]bool)
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err == nil {
			validMembers[uid] = true
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating server members for mention validation: %v", err)
	}
	_ = rows.Close()

	for _, uid := range addendum.Users {
		if !validMembers[uid] {
			continue
		}
		parsed, parseErr := uuid.Parse(uid)
		if parseErr == nil && parsed != senderUserID {
			mentionedUserIDs[parsed] = true
		}
	}
}

// resolveRoleMentions batch resolves role membership instead of N+1 queries.
// Scoped to the current server via filterMentionableRoles (already applied).
func (h *Hub) resolveRoleMentions(
	serverID, senderUserID uuid.UUID,
	addendum *MentionAddendum,
	mentionedUserIDs map[uuid.UUID]bool,
) {
	if len(addendum.Roles) == 0 {
		return
	}

	rows, err := h.db.Query(
		`SELECT DISTINCT mr.user_id FROM member_roles mr
		 WHERE mr.role_id = ANY($1::uuid[]) AND mr.server_id = $2`,
		uuidArrayParam(addendum.Roles), serverID,
	)
	if err != nil {
		log.Printf("Failed to batch resolve role members for mention: %v", err)
		return
	}
	collectMentionedUsers(rows, senderUserID, mentionedUserIDs, "role members for mention routing")
}

// resolveEveryoneMentions resolves @everyone / @here: all server members.
// For @here, online filtering happens client-side.
func (h *Hub) resolveEveryoneMentions(
	serverID, senderUserID uuid.UUID,
	addendum *MentionAddendum,
	mentionedUserIDs map[uuid.UUID]bool,
) {
	if !addendum.Everyone && !addendum.Here {
		return
	}

	rows, err := h.db.Query(
		`SELECT user_id FROM server_members WHERE server_id = $1`,
		serverID,
	)
	if err != nil {
		log.Printf("Failed to resolve server members for @everyone mention: %v", err)
		return
	}
	collectMentionedUsers(rows, senderUserID, mentionedUserIDs, "server members for @everyone mention")
}

// collectMentionedUsers scans user_id rows and adds them to the mentionedUserIDs set,
// excluding the sender. The logContext describes the query source for error logging.
func collectMentionedUsers(rows interface {
	Next() bool
	Scan(...interface{}) error
	Err() error
	Close() error
}, senderUserID uuid.UUID, mentionedUserIDs map[uuid.UUID]bool, logContext string) {
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			continue
		}
		parsed, parseErr := uuid.Parse(uid)
		if parseErr == nil && parsed != senderUserID {
			mentionedUserIDs[parsed] = true
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating %s: %v", logContext, err)
	}
	_ = rows.Close()
}

// sendMentionNotify sends a mention-enhanced unread_notify to specific users.
// Users who are subscribed to the channel already get the full message and will
// detect mentions client-side after decryption.
func (h *Hub) sendMentionNotify(
	serverID, channelID uuid.UUID,
	mentionedUsers map[uuid.UUID]bool,
	isEveryone, isHere bool,
	viewPerm int64,
) {
	// Marshal once — payload is identical for all recipients
	notifyMsg, err := marshalOutgoing(OutgoingMessage{
		Type: "unread_notify",
		Data: map[string]interface{}{
			"channel_id":       channelID.String(),
			"server_id":        serverID.String(),
			"mentioned":        true,
			"mention_everyone": isEveryone,
			"mention_here":     isHere,
		},
	})
	if err != nil {
		return
	}

	channelClients := h.channelSubscriptions[channelID]
	recipients := make([]channelDeliveryRecipient, 0)
	for userID := range mentionedUsers {
		clientIDs, ok := h.userClients[userID]
		if !ok {
			continue
		}
		for clientID := range clientIDs {
			if channelClients != nil && channelClients[clientID] {
				continue
			}
			if _, ok := h.clients[clientID]; !ok {
				continue
			}
			recipients = append(recipients, channelDeliveryRecipient{clientID: clientID, userID: userID})
		}
	}
	h.dispatchChannelDelivery(channelDeliveryRequest{
		kind:       channelDeliveryMention,
		serverID:   serverID,
		channelID:  channelID,
		viewPerm:   viewPerm,
		data:       notifyMsg,
		recipients: recipients,
	})
}

// routeDMMentionNotifications sends mention-enhanced dm_unread_notify to mentioned
// DM participants. DMs have no RBAC — all participants can mention each other.
// (#3) Validates mentioned users are actual conversation participants.
func (h *Hub) routeDMMentionNotifications(
	conversationID, senderUserID uuid.UUID,
	addendum *MentionAddendum,
) {
	if addendum == nil || addendum.IsEmpty() {
		return
	}

	participants := h.resolveDMParticipants(conversationID)
	if len(participants) == 0 {
		return
	}

	mentionedUserIDs := h.resolveDMMentionTargets(senderUserID, addendum, participants)
	if len(mentionedUserIDs) == 0 {
		return
	}

	h.sendDMMentionNotify(conversationID, mentionedUserIDs, addendum.Here)
}

// resolveDMParticipants fetches all participants for a DM conversation.
func (h *Hub) resolveDMParticipants(conversationID uuid.UUID) map[uuid.UUID]bool {
	participants := make(map[uuid.UUID]bool)
	rows, err := h.db.Query(
		`SELECT user_id FROM dm_participants WHERE conversation_id = $1`,
		conversationID,
	)
	if err != nil {
		log.Printf("Failed to resolve DM participants for mention routing: %v", err)
		return participants
	}
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err == nil {
			if parsed, parseErr := uuid.Parse(uid); parseErr == nil {
				participants[parsed] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Error iterating DM participants for mention routing: %v", err)
	}
	_ = rows.Close()
	return participants
}

// resolveDMMentionTargets collects direct user mentions and @here targets,
// validating them against conversation participants.
func (h *Hub) resolveDMMentionTargets(
	senderUserID uuid.UUID,
	addendum *MentionAddendum,
	participants map[uuid.UUID]bool,
) map[uuid.UUID]bool {
	mentionedUserIDs := make(map[uuid.UUID]bool)

	// Direct user mentions — only notify actual participants
	for _, uid := range addendum.Users {
		parsed, parseErr := uuid.Parse(uid)
		if parseErr != nil || parsed == senderUserID {
			continue
		}
		if participants[parsed] {
			mentionedUserIDs[parsed] = true
		}
	}

	// @here in DMs: all participants
	if addendum.Here {
		for participantID := range participants {
			if participantID != senderUserID {
				mentionedUserIDs[participantID] = true
			}
		}
	}

	return mentionedUserIDs
}

// sendDMMentionNotify sends dm_unread_notify to mentioned DM participants
// who are not subscribed to the conversation.
func (h *Hub) sendDMMentionNotify(
	conversationID uuid.UUID,
	mentionedUsers map[uuid.UUID]bool,
	isHere bool,
) {
	notifyMsg, err := marshalOutgoing(OutgoingMessage{
		Type: "dm_unread_notify",
		Data: map[string]interface{}{
			"conversation_id": conversationID.String(),
			"mentioned":       true,
			"mention_here":    isHere,
		},
	})
	if err != nil {
		return
	}

	dmClients := h.dmSubscriptions[conversationID]

	for userID := range mentionedUsers {
		h.sendToUnsubscribedClients(userID, dmClients, notifyMsg)
	}
}

// sendToUnsubscribedClients sends a message to all clients of a user that are NOT
// in the given subscription set (channelClients or dmClients).
func (h *Hub) sendToUnsubscribedClients(userID uuid.UUID, subscribedClients map[uuid.UUID]bool, msg []byte) {
	h.sendToUnsubscribedClientsIf(userID, subscribedClients, msg, nil)
}

func (h *Hub) sendToUnsubscribedClientsIf(
	userID uuid.UUID,
	subscribedClients map[uuid.UUID]bool,
	msg []byte,
	allow func(*Client) bool,
) {
	clientIDs, ok := h.userClients[userID]
	if !ok {
		return
	}

	for clientID := range clientIDs {
		if subscribedClients != nil && subscribedClients[clientID] {
			continue
		}
		client, ok := h.clients[clientID]
		if !ok {
			continue
		}
		if allow != nil && !allow(client) {
			continue
		}
		select {
		case client.Send <- msg:
		default:
		}
	}
}

// marshalOutgoing is a helper to JSON-marshal an OutgoingMessage.
func marshalOutgoing(msg OutgoingMessage) ([]byte, error) {
	return json.Marshal(msg)
}
