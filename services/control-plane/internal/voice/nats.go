package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// NATSSubscriber listens for voice events from the media plane and
// updates the database + broadcasts WebSocket messages to clients.
type NATSSubscriber struct {
	db        *sql.DB
	log       *logger.Logger
	hub       *websocket.Hub
	nats      *natsclient.Client
	redis     *redis.Client
	tempGrant *tempGrantManager
	// permEnforcer re-pushes fresh permissions when a join lands (CV-CAN-007
	// review P1 join-race): a join-authorize resolved before a mutation whose
	// recheck sweep ran before this voice_participants row existed would
	// otherwise hold a stale snapshot no push covers. Optional (nil = no-op).
	permEnforcer *PermissionEnforcer
}

var errInvalidDMVoiceCallLifecycle = errors.New("invalid DM voice call lifecycle identity")

const (
	dmRoomEmptyCleanupTimeout = 10 * time.Second
	// ponytail: bounded retries cover transient DB errors; persistent failures
	// fall back to the existing 90-second presence expiry.
	dmRoomEmptyCleanupAttempts = 3
)

// SetPermissionEnforcer wires the mid-session permission push into the
// voice.joined bridge. Called once at router construction.
func (s *NATSSubscriber) SetPermissionEnforcer(e *PermissionEnforcer) {
	s.permEnforcer = e
}

// NewNATSSubscriber creates a new NATS subscriber for voice events. The resolver is
// required so the subscriber can drive temporary-SBAC cleanup (#487 P1) on
// voice.left / heartbeat stale-removal through the shared tempGrantManager.
func NewNATSSubscriber(db *sql.DB, log *logger.Logger, hub *websocket.Hub, nats *natsclient.Client, redisClient *redis.Client, resolver *rbac.Resolver) *NATSSubscriber {
	return &NATSSubscriber{
		db:        db,
		log:       log,
		hub:       hub,
		nats:      nats,
		redis:     redisClient,
		tempGrant: newTempGrantManager(db, log, hub, resolver, nats),
	}
}

// voiceJoinedEvent matches the media plane's voice.joined NATS payload.
type voiceJoinedEvent struct {
	ChannelID   string `json:"channelId"`
	CallID      string `json:"callId"`
	UserID      string `json:"userId"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName,omitempty"`
	Timestamp   string `json:"timestamp"`
}

// voiceLeftEvent matches the media plane's voice.left NATS payload.
type voiceLeftEvent struct {
	ChannelID string `json:"channelId"`
	CallID    string `json:"callId"`
	UserID    string `json:"userId"`
	Timestamp string `json:"timestamp"`
}

// voiceRoomEmptyEvent matches the media plane's voice.room_empty NATS payload.
type voiceRoomEmptyEvent struct {
	ChannelID          string   `json:"channelId"`
	CallID             string   `json:"callId"`
	RingID             string   `json:"ringId"`
	CallerUserID       string   `json:"callerUserId"`
	ParticipantUserIDs []string `json:"participantUserIds"`
	StartedAt          string   `json:"startedAt"`
	Timestamp          string   `json:"timestamp"`
}

// voiceHeartbeatEvent is the per-room heartbeat from the media plane.
type voiceHeartbeatEvent struct {
	ChannelID    string   `json:"channelId"`
	CallID       string   `json:"callId"`
	RingID       string   `json:"ringId"`
	CallerUserID string   `json:"callerUserId"`
	UserIDs      []string `json:"userIds"`
	Timestamp    string   `json:"timestamp"`
}

func completedCallSummaryFromRoomEmpty(event voiceRoomEmptyEvent) (dm.CompletedCallSummary, bool, error) {
	hasSummary := event.CallID != "" || event.CallerUserID != "" ||
		len(event.ParticipantUserIDs) > 0 || event.StartedAt != ""
	if !hasSummary {
		return dm.CompletedCallSummary{}, false, nil
	}

	callID, err := uuid.Parse(event.CallID)
	if err != nil {
		return dm.CompletedCallSummary{}, true, fmt.Errorf("invalid callId: %w", err)
	}
	callerUserID, err := uuid.Parse(event.CallerUserID)
	if err != nil {
		return dm.CompletedCallSummary{}, true, fmt.Errorf("invalid callerUserId: %w", err)
	}
	startedAt, err := time.Parse(time.RFC3339, event.StartedAt)
	if err != nil {
		return dm.CompletedCallSummary{}, true, fmt.Errorf("invalid startedAt: %w", err)
	}
	endedAt, err := time.Parse(time.RFC3339, event.Timestamp)
	if err != nil {
		return dm.CompletedCallSummary{}, true, fmt.Errorf("invalid timestamp: %w", err)
	}

	participants := make([]uuid.UUID, 0, len(event.ParticipantUserIDs))
	seen := make(map[uuid.UUID]struct{}, len(event.ParticipantUserIDs))
	for _, rawUserID := range event.ParticipantUserIDs {
		userID, parseErr := uuid.Parse(rawUserID)
		if parseErr != nil {
			return dm.CompletedCallSummary{}, true, fmt.Errorf("invalid participant user ID: %w", parseErr)
		}
		if _, duplicate := seen[userID]; duplicate {
			continue
		}
		seen[userID] = struct{}{}
		participants = append(participants, userID)
	}

	ringID := uuid.Nil
	if event.RingID != "" {
		ringID, err = uuid.Parse(event.RingID)
		if err != nil {
			return dm.CompletedCallSummary{}, true, fmt.Errorf("invalid ringId: %w", err)
		}
	}

	return dm.CompletedCallSummary{
		CallID:             callID,
		RingID:             ringID,
		CallerUserID:       callerUserID,
		ParticipantUserIDs: participants,
		StartedAt:          startedAt,
		EndedAt:            endedAt,
	}, true, nil
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

// Subscribe registers one wildcard lifecycle handler. nats.go serializes
// callbacks per subscription, so a single subscription preserves one media
// publisher's joined -> left -> room_empty order across distinct subjects. It
// does not establish ordering between independent media-plane publishers.
func (s *NATSSubscriber) Subscribe() error {
	if _, err := s.nats.SubscribeWithSubject(natsSubjectVoiceWildcard, s.handleVoiceLifecycleEvent); err != nil {
		return err
	}

	s.log.Info("Subscribed to voice NATS events")
	return nil
}

func (s *NATSSubscriber) handleVoiceLifecycleEvent(subject string, data []byte) {
	switch subject {
	case natsSubjectVoiceJoined:
		s.handleJoined(data)
	case natsSubjectVoiceLeft:
		s.handleLeft(data)
	case natsSubjectVoiceRoomEmpty:
		s.handleRoomEmpty(data)
	case natsSubjectVoiceHeartbeat:
		s.handleHeartbeat(data)
	}
}

func parseDMVoiceCallLifecycleID(rawID, label string) (uuid.UUID, error) {
	id, err := uuid.Parse(rawID)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: invalid %s ID", errInvalidDMVoiceCallLifecycle, label)
	}
	return id, nil
}

func parseOptionalDMVoiceCallLifecycleID(rawID, label string) (uuid.UUID, error) {
	if rawID == "" {
		return uuid.Nil, nil
	}
	return parseDMVoiceCallLifecycleID(rawID, label)
}

func (s *NATSSubscriber) refreshDMVoiceCallLease(
	ctx context.Context,
	conversationID uuid.UUID,
	rawCallID, rawRingID, rawCallerUserID string,
	authoritativeMetadata bool,
) error {
	callID, err := parseDMVoiceCallLifecycleID(rawCallID, "call")
	if err != nil {
		return err
	}
	ringID, err := parseOptionalDMVoiceCallLifecycleID(rawRingID, "ring")
	if err != nil {
		return err
	}
	callerUserID, err := parseOptionalDMVoiceCallLifecycleID(rawCallerUserID, "caller")
	if err != nil {
		return err
	}

	existing, hasLease, lookupErr := dm.LookupDMVoiceCallLease(ctx, s.redis, conversationID)
	if lookupErr != nil {
		return lookupErr
	}
	// /ring and media lifecycle claims share LockDMCallLifecycle. Once /ring
	// publishes a new pending ring, an old room whose lease expired must not be
	// allowed to reclaim the empty shared slot on its next joined/heartbeat.
	if !hasLease && dm.HasLocalPendingDMCall(conversationID) {
		return fmt.Errorf("%w: pending ring owns conversation", errInvalidDMVoiceCallLifecycle)
	}
	if callerUserID == uuid.Nil && hasLease && existing.CallID == callID {
		callerUserID = existing.CallerUserID
		if ringID == uuid.Nil {
			ringID = existing.RingID
		}
	}
	if callerUserID == uuid.Nil {
		return fmt.Errorf("%w: missing caller ID", errInvalidDMVoiceCallLifecycle)
	}

	return dm.RefreshDMVoiceCallLease(ctx, s.redis, dm.VoiceCallLease{
		ConversationID: conversationID,
		CallID:         callID,
		RingID:         ringID,
		CallerUserID:   callerUserID,
	}, dm.DMVoiceCallLeaseTTL, authoritativeMetadata)
}

// dmCallEventMayMutateLiveState checks whether a left/room-empty event can
// change conversation-wide live presence. Exact events are accepted when no
// newer lease or pending ring exists, or when they match the current lease's
// call ID. An ID-less legacy event is accepted only after the shared lease and
// local pending ring are gone; otherwise there is no safe way to correlate it.
//
// Callers must hold dm.LockDMCallLifecycle for the conversation so a local
// authorize/ring transition cannot appear between this check and DB/WS effects.
func (s *NATSSubscriber) dmCallEventMayMutateLiveState(
	ctx context.Context,
	conversationID uuid.UUID,
	rawCallID string,
) (bool, error) {
	lease, hasLease, err := dm.LookupDMVoiceCallLease(ctx, s.redis, conversationID)
	if err != nil {
		return false, err
	}
	if !hasLease && dm.HasLocalPendingDMCall(conversationID) {
		return false, nil
	}
	if rawCallID == "" {
		return !hasLease, nil
	}
	callID, err := uuid.Parse(rawCallID)
	if err != nil || callID == uuid.Nil {
		return false, fmt.Errorf("%w: invalid call ID", errInvalidDMVoiceCallLifecycle)
	}
	return !hasLease || lease.CallID == callID, nil
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
		unlockLifecycle := dm.LockDMCallLifecycle(ctx.convUUID)
		defer unlockLifecycle()

		// Establish/renew the shared exact call identity before publishing live
		// presence. For direct calls the first joined user is the caller; accepted
		// rings already carry their server-authored caller in the lease.
		if err := s.refreshDMVoiceCallLease(
			context.Background(), ctx.convUUID, event.CallID, "", event.UserID, false,
		); err != nil {
			s.log.Error("Failed to refresh DM voice call lease from join", "error", err,
				"conversation_id", event.ChannelID, "call_id", event.CallID)
			// The lease is the exact-call fence. If Redis cannot prove ownership,
			// fail closed before publishing presence for a potentially stale room.
			return
		}

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

		s.hub.BroadcastToDMParticipants(ctx.convUUID, websocket.OutgoingMessage{
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

		// Close the join-vs-mutation race: re-push freshly resolved permissions
		// now that presence is recorded, so a mutation whose sweep ran before
		// this row existed cannot leave a stale join-time snapshot.
		if s.permEnforcer != nil {
			s.permEnforcer.RecheckParticipant(event.ChannelID, event.UserID)
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
		unlockLifecycle := dm.LockDMCallLifecycle(ctx.convUUID)
		defer unlockLifecycle()

		mayMutate, ownershipErr := s.dmCallEventMayMutateLiveState(
			context.Background(), ctx.convUUID, event.CallID,
		)
		if ownershipErr != nil {
			s.log.Error("Failed to validate DM voice.left lifecycle", "error", ownershipErr,
				"conversation_id", event.ChannelID, "call_id", event.CallID)
			return
		}
		if !mayMutate {
			s.log.Warn("Ignored stale or uncorrelated DM voice.left event",
				"conversation_id", event.ChannelID, "call_id", event.CallID)
			return
		}

		_, err := s.db.Exec(`DELETE FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`, event.ChannelID, event.UserID)
		if err != nil {
			s.log.Error("Failed to delete DM voice participant", "error", err, "conversation_id", event.ChannelID, "user_id", event.UserID)
			return
		}

		s.hub.BroadcastToDMParticipants(ctx.convUUID, websocket.OutgoingMessage{
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

func (s *NATSSubscriber) persistDMRoomEmptySummary(
	event voiceRoomEmptyEvent,
	conversationID uuid.UUID,
) (bool, error) {
	summary, hasSummary, err := completedCallSummaryFromRoomEmpty(event)
	switch {
	case err != nil:
		s.log.Error("Rejected malformed DM room-empty call summary",
			"error", err, "conversation_id", event.ChannelID)
	case hasSummary:
		// Persist an exact old-call summary even when a newer call now owns
		// live presence. The call ID makes this insert idempotent and distinct
		// from the replacement lifecycle.
		if insertErr := dm.InsertCompletedCallEvent(context.Background(), s.db, conversationID, summary); insertErr != nil {
			s.log.Error("Failed to insert completed call_event row",
				"error", insertErr, "conversation_id", event.ChannelID, "call_id", event.CallID)
		}
	default:
		// Best-effort legacy fallback only. ID-less media events cannot renew or
		// terminate an exact shared lease and therefore do not provide complete
		// rolling-version compatibility. We run the live-presence fallback below
		// only when no exact call currently owns the conversation.
		s.log.Warn("Legacy DM room-empty event lacks terminal call summary",
			"conversation_id", event.ChannelID)
	}
	return hasSummary, err
}

func parseDMRoomEmptyCallID(rawCallID string) (uuid.UUID, error) {
	callID, err := uuid.Parse(rawCallID)
	if err != nil {
		return uuid.Nil, err
	}
	if callID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: zero call ID", errInvalidDMVoiceCallLifecycle)
	}
	return callID, nil
}

func (s *NATSSubscriber) beginDMRoomEmptyCleanup(
	event voiceRoomEmptyEvent,
	conversationID uuid.UUID,
) (func(), bool) {
	if event.CallID == "" {
		return func() {
			// Legacy ID-less events do not acquire a distributed cleanup guard.
		}, true
	}
	callID, err := parseDMRoomEmptyCallID(event.CallID)
	if err != nil {
		s.log.Error("Failed to begin malformed DM voice call cleanup", "error", err,
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return nil, false
	}
	acquired, err := dm.BeginDMVoiceCallCleanup(
		context.Background(), s.redis, conversationID, callID,
	)
	if err != nil {
		s.log.Error("Failed to begin terminal DM voice call cleanup", "error", err,
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return nil, false
	}
	if !acquired {
		s.log.Warn("Ignored stale or concurrent DM voice call cleanup",
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return nil, false
	}

	release := func() {
		if err := dm.EndDMVoiceCallCleanup(
			context.Background(), s.redis, conversationID, callID,
		); err != nil {
			s.log.Error("Failed to release terminal DM voice call cleanup guard", "error", err,
				"conversation_id", event.ChannelID, "call_id", event.CallID)
		}
	}
	return release, true
}

func (s *NATSSubscriber) handleDMRoomEmpty(event voiceRoomEmptyEvent, conversationID uuid.UUID) bool {
	hasSummary, summaryErr := s.persistDMRoomEmptySummary(event, conversationID)
	releaseCleanup, acquired := s.beginDMRoomEmptyCleanup(event, conversationID)
	if !acquired {
		return false
	}
	defer releaseCleanup()
	cleanupCtx, cancelCleanup := context.WithTimeout(
		context.Background(), dmRoomEmptyCleanupTimeout,
	)
	defer cancelCleanup()

	mayMutate, err := s.dmCallEventMayMutateLiveState(
		cleanupCtx, conversationID, event.CallID,
	)
	if err != nil {
		s.log.Error("Failed to validate DM room-empty lifecycle", "error", err,
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return false
	}
	if !mayMutate {
		s.log.Warn("Ignored stale or uncorrelated DM room-empty live-state cleanup",
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return false
	}

	return s.finishDMRoomEmptyLiveState(
		cleanupCtx, event, conversationID, !hasSummary && summaryErr == nil,
	)
}

func (s *NATSSubscriber) finishDMRoomEmptyLiveState(
	ctx context.Context,
	event voiceRoomEmptyEvent,
	conversationID uuid.UUID,
	persistFallback bool,
) bool {
	if persistFallback {
		s.persistDMRoomEmptyFallback(ctx, event, conversationID)
	}

	if err := s.clearDMVoiceParticipants(ctx, event.ChannelID); err != nil {
		s.log.Error("Failed to clear DM voice participants", "error", err, "conversation_id", event.ChannelID)
		return false
	}

	s.hub.BroadcastToDMParticipants(conversationID, websocket.OutgoingMessage{
		Type: "dm_voice_state_update",
		Data: map[string]interface{}{
			"conversation_id": event.ChannelID,
			"action":          "room_empty",
		},
	})
	return true
}

func (s *NATSSubscriber) clearDMVoiceParticipants(ctx context.Context, conversationID string) error {
	var err error
	for attempt := 0; attempt < dmRoomEmptyCleanupAttempts; attempt++ {
		_, err = s.db.ExecContext(
			ctx, `DELETE FROM dm_voice_participants WHERE conversation_id = $1`, conversationID,
		)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if attempt+1 < dmRoomEmptyCleanupAttempts {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return err
}

func (s *NATSSubscriber) persistDMRoomEmptyFallback(
	ctx context.Context,
	event voiceRoomEmptyEvent,
	conversationID uuid.UUID,
) {
	if event.CallID == "" {
		if err := dm.InsertCompletedCallEventForDMRoom(
			ctx, s.db, conversationID,
		); err != nil {
			s.log.Error("Failed to insert legacy completed call_event row",
				"error", err, "conversation_id", event.ChannelID)
		}
		return
	}

	callID, err := parseDMRoomEmptyCallID(event.CallID)
	if err != nil {
		s.log.Error("Failed to parse heartbeat fallback call ID", "error", err,
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return
	}
	callerUserID, err := uuid.Parse(event.CallerUserID)
	if err != nil || callerUserID == uuid.Nil {
		s.log.Error("Failed to parse heartbeat fallback caller ID", "error", err,
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return
	}
	ringID, err := parseOptionalDMVoiceCallLifecycleID(event.RingID, "ring")
	if err != nil {
		s.log.Error("Failed to parse heartbeat fallback ring ID", "error", err,
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return
	}
	endedAt, err := time.Parse(time.RFC3339, event.Timestamp)
	if err != nil {
		s.log.Error("Failed to parse heartbeat fallback timestamp", "error", err,
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return
	}
	if err := dm.InsertCompletedCallEventForDMHeartbeat(
		ctx, s.db, conversationID, callID, ringID, callerUserID, endedAt,
	); err != nil {
		s.log.Error("Failed to insert exact heartbeat completed call_event row",
			"error", err, "conversation_id", event.ChannelID, "call_id", event.CallID)
	}
}

func (s *NATSSubscriber) handleEmptyDMHeartbeat(
	event voiceHeartbeatEvent,
	conversationID uuid.UUID,
) bool {
	// An empty heartbeat is an exact terminal reconciliation signal. Fence it
	// against a replacement call before deleting the matching lease/tombstoning
	// its ID, then preserve the presence-derived fallback while rows still exist.
	callID, err := parseDMRoomEmptyCallID(event.CallID)
	if err != nil {
		s.log.Error("Rejected empty DM heartbeat without an exact call ID", "error", err,
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return false
	}
	terminal := voiceRoomEmptyEvent{
		ChannelID:    event.ChannelID,
		CallID:       event.CallID,
		RingID:       event.RingID,
		CallerUserID: event.CallerUserID,
		Timestamp:    event.Timestamp,
	}
	lease, hasLease, err := dm.LookupDMVoiceCallLease(
		context.Background(), s.redis, conversationID,
	)
	if err != nil {
		s.log.Error("Failed to resolve empty DM heartbeat lease metadata", "error", err,
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return false
	}
	if hasLease && lease.CallID == callID {
		if terminal.CallerUserID == "" {
			terminal.CallerUserID = lease.CallerUserID.String()
		}
		if terminal.RingID == "" && lease.RingID != uuid.Nil {
			terminal.RingID = lease.RingID.String()
		}
	}

	releaseCleanup, acquired := s.beginDMRoomEmptyCleanup(terminal, conversationID)
	if !acquired {
		return false
	}
	defer releaseCleanup()
	cleanupCtx, cancelCleanup := context.WithTimeout(
		context.Background(), dmRoomEmptyCleanupTimeout,
	)
	defer cancelCleanup()

	// Re-check after the exact Redis delete. A replacement call claimed on
	// another replica before the cleanup guard was acquired is preserved. While
	// the guard is held, no replacement can claim the lease until the DB cleanup
	// and room-empty broadcast have completed.
	mayMutate, err := s.dmCallEventMayMutateLiveState(
		cleanupCtx, conversationID, event.CallID,
	)
	if err != nil {
		s.log.Error("Failed to validate empty DM heartbeat lifecycle", "error", err,
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return false
	}
	if !mayMutate {
		s.log.Warn("Ignored stale or uncorrelated empty DM heartbeat",
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		return false
	}

	return s.finishDMRoomEmptyLiveState(cleanupCtx, terminal, conversationID, true)
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
		unlockLifecycle := dm.LockDMCallLifecycle(ctx.convUUID)
		defer unlockLifecycle()
		if !s.handleDMRoomEmpty(event, ctx.convUUID) {
			return
		}
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

func (s *NATSSubscriber) refreshDMHeartbeat(event voiceHeartbeatEvent, conversationID uuid.UUID) bool {
	if err := s.refreshDMVoiceCallLease(
		context.Background(), conversationID, event.CallID, event.RingID, event.CallerUserID, true,
	); err != nil {
		s.log.Error("Failed to refresh DM voice call lease from heartbeat", "error", err,
			"conversation_id", event.ChannelID, "call_id", event.CallID)
		// Reconciliation can delete a replacement call's participants, so a
		// Redis error must fail closed just like an explicit ID conflict.
		return false
	}
	if _, err := s.db.Exec(`
		INSERT INTO dm_voice_participants (conversation_id, user_id, joined_at)
		SELECT $1, user_id, NOW()
		FROM unnest($2::uuid[]) AS users(user_id)
		ON CONFLICT (conversation_id, user_id)
		DO UPDATE SET joined_at = NOW()
	`, event.ChannelID, pq.Array(event.UserIDs)); err != nil {
		s.log.Error("Failed to refresh DM voice presence lease", "error", err,
			"conversation_id", event.ChannelID)
	}
	return true
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
	if ctx.isDM {
		unlockLifecycle := dm.LockDMCallLifecycle(ctx.convUUID)
		defer unlockLifecycle()

		if len(event.UserIDs) == 0 {
			_ = s.handleEmptyDMHeartbeat(event, ctx.convUUID)
			return
		}
		if !s.refreshDMHeartbeat(event, ctx.convUUID) {
			return
		}
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
		s.hub.BroadcastToDMParticipants(ctx.convUUID, websocket.OutgoingMessage{
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
		s.hub.BroadcastToDMParticipants(ctx.convUUID, websocket.OutgoingMessage{
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

	natsSubjectVoiceWildcard  = "voice.*"
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
