package voice

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/dm"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
)

// NATSSubscriber listens for voice events from the media plane and
// updates the database + broadcasts WebSocket messages to clients.
type NATSSubscriber struct {
	db        *sql.DB
	log       *logger.Logger
	hub       *websocket.Hub
	nats      *natsclient.Client
	tempGrant *tempGrantManager
}

// NewNATSSubscriber creates a new NATS subscriber for voice events. The resolver is
// required so the subscriber can drive temporary-SBAC cleanup (#487 P1) on
// voice.left / heartbeat stale-removal through the shared tempGrantManager.
func NewNATSSubscriber(db *sql.DB, log *logger.Logger, hub *websocket.Hub, nats *natsclient.Client, resolver *rbac.Resolver) *NATSSubscriber {
	return &NATSSubscriber{
		db:        db,
		log:       log,
		hub:       hub,
		nats:      nats,
		tempGrant: newTempGrantManager(db, log, hub, resolver, nats),
	}
}

// voiceJoinedEvent matches the media plane's voice.joined NATS payload.
type voiceJoinedEvent struct {
	ChannelID   string `json:"channelId"`
	UserID      string `json:"userId"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName,omitempty"`
	Timestamp   string `json:"timestamp"`
}

// voiceLeftEvent matches the media plane's voice.left NATS payload.
type voiceLeftEvent struct {
	ChannelID string `json:"channelId"`
	UserID    string `json:"userId"`
	Timestamp string `json:"timestamp"`
}

// voiceRoomEmptyEvent matches the media plane's voice.room_empty NATS payload.
type voiceRoomEmptyEvent struct {
	ChannelID string `json:"channelId"`
	Timestamp string `json:"timestamp"`
}

// voiceHeartbeatEvent is the per-room heartbeat from the media plane.
type voiceHeartbeatEvent struct {
	ChannelID string   `json:"channelId"`
	UserIDs   []string `json:"userIds"`
	Timestamp string   `json:"timestamp"`
}

// roomContext holds the resolved context for a voice room.
// Either serverID is set (server channel) or isDM is true (DM conversation).
type roomContext struct {
	isDM       bool
	serverID   string
	serverUUID uuid.UUID
	convUUID   uuid.UUID
}

// resolveRoom performs the dual-lookup: tries channels first, falls back to dm_conversations.
func (s *NATSSubscriber) resolveRoom(channelID string) (*roomContext, error) {
	var serverID string
	err := s.db.QueryRow("SELECT server_id FROM channels WHERE id = $1", channelID).Scan(&serverID)
	if err == nil {
		serverUUID, parseErr := uuid.Parse(serverID)
		if parseErr != nil {
			return nil, parseErr
		}
		return &roomContext{isDM: false, serverID: serverID, serverUUID: serverUUID}, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	// Not a server channel — try DM conversation
	var convID string
	err = s.db.QueryRow("SELECT id FROM dm_conversations WHERE id = $1", channelID).Scan(&convID)
	if err != nil {
		return nil, err
	}
	convUUID, parseErr := uuid.Parse(convID)
	if parseErr != nil {
		return nil, parseErr
	}
	return &roomContext{isDM: true, convUUID: convUUID}, nil
}

// Subscribe registers handlers for all voice NATS subjects.
func (s *NATSSubscriber) Subscribe() error {
	if _, err := s.nats.Subscribe(natsSubjectVoiceJoined, s.handleJoined); err != nil {
		return err
	}
	if _, err := s.nats.Subscribe(natsSubjectVoiceLeft, s.handleLeft); err != nil {
		return err
	}
	if _, err := s.nats.Subscribe(natsSubjectVoiceRoomEmpty, s.handleRoomEmpty); err != nil {
		return err
	}
	if _, err := s.nats.Subscribe(natsSubjectVoiceHeartbeat, s.handleHeartbeat); err != nil {
		return err
	}

	s.log.Info("Subscribed to voice NATS events")
	return nil
}

func (s *NATSSubscriber) handleJoined(data []byte) {
	var event voiceJoinedEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Failed to unmarshal voice.joined", "error", err)
		return
	}

	ctx, err := s.resolveRoom(event.ChannelID)
	if err != nil {
		s.log.Error("Failed to resolve room for voice.joined", "error", err, "channel_id", event.ChannelID)
		return
	}

	if ctx.isDM {
		// Insert into dm_voice_participants
		_, err := s.db.Exec(`
			INSERT INTO dm_voice_participants (conversation_id, user_id, joined_at)
			VALUES ($1, $2, NOW())
			ON CONFLICT (conversation_id, user_id) DO UPDATE SET joined_at = NOW()
		`, event.ChannelID, event.UserID)
		if err != nil {
			s.log.Error("Failed to insert DM voice participant", "error", err, "conversation_id", event.ChannelID, "user_id", event.UserID)
			return
		}

		s.hub.BroadcastToDM(ctx.convUUID, websocket.OutgoingMessage{
			Type: "dm_voice_state_update",
			Data: map[string]interface{}{
				"conversation_id": event.ChannelID,
				"user_id":         event.UserID,
				"username":        event.Username,
				"display_name":    event.DisplayName,
				"action":          "joined",
			},
		})

		// Re-enforce DM hard mute/deafen if active (#488)
		s.reEnforceDM(event.ChannelID, event.UserID)
	} else {
		// Insert into voice_participants (server channel)
		_, err := s.db.Exec(`
			INSERT INTO voice_participants (channel_id, user_id, joined_at)
			VALUES ($1, $2, NOW())
			ON CONFLICT (channel_id, user_id) DO UPDATE SET joined_at = NOW()
		`, event.ChannelID, event.UserID)
		if err != nil {
			s.log.Error("Failed to insert voice participant", "error", err, "channel_id", event.ChannelID, "user_id", event.UserID)
			return
		}

		s.hub.BroadcastToServer(ctx.serverUUID, websocket.OutgoingMessage{
			Type: "voice_state_update",
			Data: map[string]interface{}{
				"channel_id":   event.ChannelID,
				"user_id":      event.UserID,
				"username":     event.Username,
				"display_name": event.DisplayName,
				"action":       "joined",
				"server_id":    ctx.serverID,
			},
		})

		// Re-enforce server mute/deafen if active (#488)
		s.reEnforceServer(ctx.serverID, event.ChannelID, event.UserID)
	}

	s.log.Info("Voice participant joined", "channel_id", event.ChannelID, "user_id", event.UserID, "is_dm", ctx.isDM)

	if !ctx.isDM {
		s.hub.BroadcastServerVoiceCounts()
	}
}

func (s *NATSSubscriber) handleLeft(data []byte) {
	var event voiceLeftEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Failed to unmarshal voice.left", "error", err)
		return
	}

	ctx, err := s.resolveRoom(event.ChannelID)
	if err != nil {
		s.log.Error("Failed to resolve room for voice.left", "error", err, "channel_id", event.ChannelID)
		return
	}

	if ctx.isDM {
		_, err := s.db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, event.ChannelID, event.UserID)
		if err != nil {
			s.log.Error("Failed to delete DM voice participant", "error", err, "conversation_id", event.ChannelID, "user_id", event.UserID)
			return
		}

		s.hub.BroadcastToDM(ctx.convUUID, websocket.OutgoingMessage{
			Type: "dm_voice_state_update",
			Data: map[string]interface{}{
				"conversation_id": event.ChannelID,
				"user_id":         event.UserID,
				"action":          "left",
			},
		})
	} else {
		_, err := s.db.Exec(`DELETE FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, event.ChannelID, event.UserID)
		if err != nil {
			s.log.Error("Failed to delete voice participant", "error", err, "channel_id", event.ChannelID, "user_id", event.UserID)
			return
		}

		s.hub.BroadcastToServer(ctx.serverUUID, websocket.OutgoingMessage{
			Type: "voice_state_update",
			Data: map[string]interface{}{
				"channel_id": event.ChannelID,
				"user_id":    event.UserID,
				"action":     "left",
				"server_id":  ctx.serverID,
			},
		})

		// #487 T8 cleanup trigger: if the leaver held a temporary SBAC grant on this
		// channel, converge on the single cleanup path (revoke override + rotate CSK +
		// force-disconnect + notify). System-triggered, so actorID is "". This is the
		// explicit graceful-leave path, so respectGrace=false — an intentional leave is
		// authoritative regardless of how recently the grant was issued.
		s.revokeTempGrantIfHeld(ctx.serverID, event.ChannelID, event.UserID, false)
	}

	s.log.Info("Voice participant left", "channel_id", event.ChannelID, "user_id", event.UserID, "is_dm", ctx.isDM)

	if !ctx.isDM {
		s.hub.BroadcastServerVoiceCounts()
	}
}

func (s *NATSSubscriber) handleRoomEmpty(data []byte) {
	var event voiceRoomEmptyEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Failed to unmarshal voice.room_empty", "error", err)
		return
	}

	ctx, err := s.resolveRoom(event.ChannelID)
	if err != nil {
		s.log.Error("Failed to resolve room for voice.room_empty", "error", err, "channel_id", event.ChannelID)
		return
	}

	if ctx.isDM {
		// Insert the completed call_event row BEFORE the DELETE so the
		// helper can read the in-flight dm_voice_participants. Per spec
		// section 6.1 edge case "Call-event insert on hang-up failure":
		// best-effort; failure logged, doesn't block the room-cleanup.
		// #1209 plan task B7 Part 1.
		if err := dm.InsertCompletedCallEventForDMRoom(context.Background(), s.db, ctx.convUUID); err != nil {
			s.log.Error("Failed to insert completed call_event row",
				"error", err, "conversation_id", event.ChannelID)
		}

		_, err := s.db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1`, event.ChannelID)
		if err != nil {
			s.log.Error("Failed to clear DM voice participants", "error", err, "conversation_id", event.ChannelID)
		}

		s.hub.BroadcastToDM(ctx.convUUID, websocket.OutgoingMessage{
			Type: "dm_voice_state_update",
			Data: map[string]interface{}{
				"conversation_id": event.ChannelID,
				"action":          "room_empty",
			},
		})
	} else {
		_, err := s.db.Exec(`DELETE FROM voice_participants WHERE channel_id = $1`, event.ChannelID)
		if err != nil {
			s.log.Error("Failed to clear voice participants", "error", err, "channel_id", event.ChannelID)
		}

		s.hub.BroadcastToServer(ctx.serverUUID, websocket.OutgoingMessage{
			Type: "voice_state_update",
			Data: map[string]interface{}{
				"channel_id": event.ChannelID,
				"action":     "room_empty",
				"server_id":  ctx.serverID,
			},
		})
	}

	s.log.Info("Voice room empty", "channel_id", event.ChannelID, "is_dm", ctx.isDM)

	if !ctx.isDM {
		s.hub.BroadcastServerVoiceCounts()
	}
}

// handleHeartbeat reconciles voice_participants against the media plane's
// ground-truth room state. Any DB entries not present in the heartbeat are
// stale (client crashed / network dropped) and get cleaned up.
func (s *NATSSubscriber) handleHeartbeat(data []byte) {
	var event voiceHeartbeatEvent
	if err := json.Unmarshal(data, &event); err != nil {
		s.log.Error("Failed to unmarshal voice.heartbeat", "error", err)
		return
	}

	ctx, err := s.resolveRoom(event.ChannelID)
	if err != nil {
		s.log.Error("Failed to resolve room for voice.heartbeat", "error", err, "channel_id", event.ChannelID)
		return
	}

	dbUsers, err := s.collectDBParticipants(event.ChannelID, ctx.isDM)
	if err != nil {
		s.log.Error("Failed to query voice_participants for reconciliation", "error", err, "channel_id", event.ChannelID)
		return
	}
	if len(dbUsers) == 0 {
		return
	}

	mpUsers := make(map[string]bool)
	for _, uid := range event.UserIDs {
		mpUsers[uid] = true
	}

	removedAny := s.reconcileVoiceParticipants(event.ChannelID, ctx, dbUsers, mpUsers)

	if len(event.UserIDs) == 0 {
		s.broadcastRoomEmpty(event.ChannelID, ctx)
	}

	if !ctx.isDM && removedAny {
		s.hub.BroadcastServerVoiceCounts()
	}
}

func (s *NATSSubscriber) collectDBParticipants(channelID string, isDM bool) (map[string]bool, error) {
	query := `SELECT user_id FROM voice_participants WHERE channel_id = $1`
	if isDM {
		query = `SELECT user_id FROM dm_voice_participants WHERE conversation_id = $1`
	}
	rows, err := s.db.Query(query, channelID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	dbUsers := make(map[string]bool)
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err == nil {
			dbUsers[uid] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dbUsers, nil
}

func (s *NATSSubscriber) reconcileVoiceParticipants(channelID string, ctx *roomContext, dbUsers, mpUsers map[string]bool) bool {
	removedAny := false
	for uid := range dbUsers {
		if mpUsers[uid] {
			continue
		}
		removedAny = true
		s.removeStaleParticipant(channelID, uid, ctx)
		s.log.Info("Reconciled stale voice participant", "channel_id", channelID, "user_id", uid, "is_dm", ctx.isDM)
	}
	return removedAny
}

func (s *NATSSubscriber) removeStaleParticipant(channelID, userID string, ctx *roomContext) {
	if ctx.isDM {
		_, _ = s.db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, channelID, userID)
		s.hub.BroadcastToDM(ctx.convUUID, websocket.OutgoingMessage{
			Type: "dm_voice_state_update",
			Data: map[string]interface{}{
				"conversation_id": channelID,
				"user_id":         userID,
				"action":          "left",
			},
		})
		return
	}
	_, _ = s.db.Exec(`DELETE FROM voice_participants WHERE channel_id = $1 AND user_id = $2`, channelID, userID)
	s.hub.BroadcastToServer(ctx.serverUUID, websocket.OutgoingMessage{
		Type: "voice_state_update",
		Data: map[string]interface{}{
			"channel_id": channelID,
			"user_id":    userID,
			"action":     "left",
			"server_id":  ctx.serverID,
		},
	})

	// #487 T8 cleanup trigger (crash / network-loss path): server-authoritative
	// reconciliation revokes a stale temp-grant holder's override too. respectGrace=true
	// (finding #7): a heartbeat that races a fresh grant→join must NOT revoke a grant
	// younger than the 60s grace window. The heartbeat UserIDs is socket-transport-level
	// ground truth, so a genuine miss past 60s is a real disconnect and still revokes;
	// the grace is narrow defense-in-depth covering only the grant→join window.
	s.revokeTempGrantIfHeld(ctx.serverID, channelID, userID, true)
}

// revokeTempGrantIfHeld is the #487 T8 cleanup-trigger guard shared by the
// voice.left handler and the heartbeat stale-removal path. It cheaply checks
// whether the departing user holds a temporary SBAC grant on the channel and, only
// if so, drives the single revoke convergence point (revoke override + CSK rotation
// + force-disconnect + directed notify). The common no-temp-grant case short-
// circuits after the EXISTS probe so plain leaves stay cheap. actorID is "" because
// these triggers are system-initiated (no human moderator) — the rotator stores
// revoked_by as NULL.
//
// respectGrace=true (heartbeat reconcile path only, finding #7) additionally
// requires the grant be past a 60s grace window, so a brand-new grant whose
// voice.joined event has not yet landed in voice_participants is not revoked by a
// heartbeat that races the join. The voice.left graceful-leave path passes
// respectGrace=false: an explicit leave is authoritative regardless of grant age,
// and the moderator-revoke endpoint never goes through this guard at all.
func (s *NATSSubscriber) revokeTempGrantIfHeld(serverID, channelID, userID string, respectGrace bool) {
	ctx := context.Background()
	var held bool
	var err error
	if respectGrace {
		held, err = s.tempGrant.hasTemporaryGrantPastGrace(ctx, channelID, userID)
	} else {
		held, err = s.tempGrant.hasTemporaryGrant(ctx, channelID, userID)
	}
	if err != nil {
		s.log.Error("temp-grant cleanup: hasTemporaryGrant probe", "error", err, "channel_id", channelID, "user_id", userID)
		return
	}
	if !held {
		return
	}
	if err := s.tempGrant.revokeTemporaryChannelAccess(ctx, serverID, channelID, userID, ""); err != nil {
		s.log.Error("temp-grant cleanup: revoke", "error", err, "channel_id", channelID, "user_id", userID)
	}
}

func (s *NATSSubscriber) broadcastRoomEmpty(channelID string, ctx *roomContext) {
	if ctx.isDM {
		s.hub.BroadcastToDM(ctx.convUUID, websocket.OutgoingMessage{
			Type: "dm_voice_state_update",
			Data: map[string]interface{}{
				"conversation_id": channelID,
				"action":          "room_empty",
			},
		})
		return
	}
	s.hub.BroadcastToServer(ctx.serverUUID, websocket.OutgoingMessage{
		Type: "voice_state_update",
		Data: map[string]interface{}{
			"channel_id": channelID,
			"action":     "room_empty",
			"server_id":  ctx.serverID,
		},
	})
}

const (
	natsSubjectEnforceMute       = "voice.enforce.mute"
	natsSubjectEnforceDeafen     = "voice.enforce.deafen"
	natsSubjectEnforceDisconnect = "voice.enforce.disconnect"

	natsSubjectVoiceJoined    = "voice.joined"
	natsSubjectVoiceLeft      = "voice.left"
	natsSubjectVoiceRoomEmpty = "voice.room_empty"
	natsSubjectVoiceHeartbeat = "voice.heartbeat"
)

// publishForceDisconnect publishes a voice.enforce.disconnect command so the
// media plane closes that peer's transports and removes it from the room (#487
// P3). Revoking VIEW/CONNECT does NOT eject an already-connected peer, so this
// is the authoritative ejection path used by temporary-SBAC access revocation.
// Delegates to the shared tempGrantManager so the publish primitive lives in one
// place (the manager is also driven from the REST Handler's moderator-revoke
// endpoint, DELETE /servers/:id/voice/:userId/temp-access — RevokeTempAccess).
func (s *NATSSubscriber) publishForceDisconnect(channelID, userID string) {
	s.tempGrant.publishForceDisconnect(channelID, userID)
}

// publishEnforcementFlags publishes NATS enforcement commands for active mute/deafen flags.
func (s *NATSSubscriber) publishEnforcementFlags(channelID, userID, context string, serverMuted, serverDeafened bool) {
	if s.nats == nil {
		return
	}
	if serverMuted {
		if err := s.nats.Publish(natsSubjectEnforceMute, map[string]interface{}{
			"channelId": channelID, "userId": userID, "action": "mute",
		}); err != nil {
			s.log.Error("Failed to publish re-enforcement", "error", err, "subject", natsSubjectEnforceMute, "context", context, "user_id", userID)
		}
	}
	if serverDeafened {
		if err := s.nats.Publish(natsSubjectEnforceDeafen, map[string]interface{}{
			"channelId": channelID, "userId": userID, "action": "deafen",
		}); err != nil {
			s.log.Error("Failed to publish re-enforcement", "error", err, "subject", natsSubjectEnforceDeafen, "context", context, "user_id", userID)
		}
	}
}

// reEnforceServer publishes NATS enforcement commands if a server member has
// active server_muted or server_deafened flags. Called on voice.joined as a
// belt-and-suspenders safety net alongside the join authorization response.
func (s *NATSSubscriber) reEnforceServer(serverID, channelID, userID string) {
	var serverMuted, serverDeafened bool
	if err := s.db.QueryRow(`SELECT server_muted, server_deafened FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, userID).Scan(&serverMuted, &serverDeafened); err != nil {
		s.log.Error("Failed to query enforcement flags", "error", err, "server_id", serverID, "user_id", userID)
		return
	}
	s.publishEnforcementFlags(channelID, userID, "server", serverMuted, serverDeafened)
}

// reEnforceDM publishes NATS enforcement commands if a DM participant has
// active server_muted or server_deafened flags (group DM hard enforcement).
func (s *NATSSubscriber) reEnforceDM(channelID, userID string) {
	var serverMuted, serverDeafened bool
	if err := s.db.QueryRow(`SELECT server_muted, server_deafened FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
		channelID, userID).Scan(&serverMuted, &serverDeafened); err != nil {
		s.log.Error("Failed to query DM enforcement flags", "error", err, "conversation_id", channelID, "user_id", userID)
		return
	}
	s.publishEnforcementFlags(channelID, userID, "dm", serverMuted, serverDeafened)
}
