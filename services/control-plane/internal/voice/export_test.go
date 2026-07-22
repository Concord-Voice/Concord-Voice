package voice

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
)

// TestServerVoiceMutationResult exposes the atomic participant-move result.
type TestServerVoiceMutationResult struct {
	Applied                bool
	Added                  bool
	RemovedRoomIDs         []uuid.UUID
	RemovedAudienceUnknown bool
	Duplicate              bool
	ReplayMissing          bool
}

// ServerVoiceMutationNeedsReconnectForTest exposes the post-mutation
// convergence decision for an optimistic pre-lock scope observation.
func ServerVoiceMutationNeedsReconnectForTest(
	result TestServerVoiceMutationResult,
	targetRoomID uuid.UUID,
	observedScope presence.Scope,
	hasObservedScope bool,
) bool {
	return serverHeartbeatMutationNeedsReconnect(serverVoiceMutationResult{
		applied: result.Applied, added: result.Added,
		removedRoomIDs:         append([]uuid.UUID(nil), result.RemovedRoomIDs...),
		removedAudienceUnknown: result.RemovedAudienceUnknown,
		duplicate:              result.Duplicate, replayMissing: result.ReplayMissing,
	}, targetRoomID, observedScope, hasObservedScope)
}

// TestPrivateVoiceLifecycleClaim exposes one entry in the atomic Private Call
// participant-set revision for focused cross-replica integration tests.
type TestPrivateVoiceLifecycleClaim struct {
	UserID  uuid.UUID
	Token   uuid.UUID
	Version int64
	Active  bool
}

// ClaimPrivateVoiceLifecyclesForTest exposes the all-or-nothing multi-sender
// Redis fence without coupling external tests to internal status constants.
func (s *NATSSubscriber) ClaimPrivateVoiceLifecyclesForTest(
	ctx context.Context,
	claims []TestPrivateVoiceLifecycleClaim,
) (accepted bool, duplicate bool, err error) {
	internal := make([]privateVoiceParticipantSetClaim, 0, len(claims))
	for _, claim := range claims {
		internal = append(internal, privateVoiceParticipantSetClaim{
			userID: claim.UserID, token: claim.Token,
			version: claim.Version, active: claim.Active,
		})
	}
	status, err := s.claimPrivateVoiceLifecycles(ctx, internal)
	if err != nil {
		return false, false, err
	}
	return status != voiceLifecycleRejected, status == voiceLifecycleDuplicate, nil
}

// ClaimVoiceLifecycleForTest exposes lifecycle CAS semantics for focused
// integration tests without routing synthetic events through unrelated code.
func (s *NATSSubscriber) ClaimVoiceLifecycleForTest(
	ctx context.Context,
	category presence.Category,
	senderID, token uuid.UUID,
	eventAt time.Time,
	active bool,
) (bool, error) {
	return s.claimVoiceLifecycle(ctx, category, senderID, token, eventAt, active)
}

// SetVoiceLifecycleClaimedHookForTest pauses a lifecycle mutation after Redis
// accepted its watermark. The production critical section must remain held
// until the hook and the subsequent database mutation complete.
func (s *NATSSubscriber) SetVoiceLifecycleClaimedHookForTest(
	hook func(presence.Category, uuid.UUID, time.Time),
) {
	s.voiceLifecycleClaimedHook = hook
}

// SetServerVoiceScopeObservedHookForTest pauses a server participant refresh
// after its optimistic scope/audience reads and before the lifecycle lock.
func (s *NATSSubscriber) SetServerVoiceScopeObservedHookForTest(
	hook func(uuid.UUID, uuid.UUID, time.Time),
) {
	s.serverVoiceScopeObservedHook = hook
}

// SetPrivateJoinHooksForTest exposes deterministic boundaries immediately
// before the guarded join mutation and immediately before its base broadcast.
func (s *NATSSubscriber) SetPrivateJoinHooksForTest(
	beforeMutation, beforeBroadcast func(uuid.UUID, uuid.UUID),
) {
	s.privateJoinBeforeMutationHook = beforeMutation
	s.privateJoinBroadcastHook = beforeBroadcast
}

// SetPrivateVoiceDurabilityHooksForTest exposes deterministic post-commit and
// base-state boundaries for focused Private Call durability regressions.
func (s *NATSSubscriber) SetPrivateVoiceDurabilityHooksForTest(
	leaveAfterCommit func(),
	heartbeatAfterCommit func(),
	stateBroadcast func(uuid.UUID, uuid.UUID, string),
) {
	s.privateLeaveAfterCommitHook = leaveAfterCommit
	s.dmHeartbeatPostCommitHook = heartbeatAfterCommit
	s.privateVoiceStateBroadcastHook = stateBroadcast
}

// SetDMRoomEmptyVerificationHookForTest injects the final terminal roster read.
func (s *NATSSubscriber) SetDMRoomEmptyVerificationHookForTest(hook func() error) {
	s.dmRoomEmptyVerificationHook = hook
}

// SetActivityServiceForTest injects RP bridge availability for security-order tests.
func (s *NATSSubscriber) SetActivityServiceForTest(activity *presence.ActivityService) {
	s.activity = activity
}

// RunVoiceLifecycleMutationForTest exercises the same claim-then-mutate
// ordering as production. It intentionally mirrors the pre-fence behavior
// until withVoiceLifecycleClaim replaces this body during the TDD cycle.
func (s *NATSSubscriber) RunVoiceLifecycleMutationForTest(
	ctx context.Context,
	category presence.Category,
	senderID, token, conversationID uuid.UUID,
	eventAt time.Time,
	active bool,
	mutation func(context.Context, *sql.Tx) (bool, error),
) (bool, error) {
	return s.withVoiceLifecycleClaimInParticipantSet(
		ctx,
		voiceLifecycleClaimRequest{
			category: category, senderID: senderID, token: token,
			eventAt: eventAt, active: active, conversationID: conversationID,
		},
		mutation,
	)
}

// UpsertServerVoiceParticipantForTest exposes the atomic move for integration tests.
func (s *NATSSubscriber) UpsertServerVoiceParticipantForTest(
	ctx context.Context,
	channelID, senderID uuid.UUID,
	eventAt time.Time,
) (TestServerVoiceMutationResult, error) {
	result, err := s.upsertServerVoiceParticipant(ctx, channelID, senderID, eventAt)
	return TestServerVoiceMutationResult{
		Applied:                result.applied,
		Added:                  result.added,
		RemovedRoomIDs:         append([]uuid.UUID(nil), result.removedRoomIDs...),
		RemovedAudienceUnknown: result.removedAudienceUnknown,
		Duplicate:              result.duplicate,
		ReplayMissing:          result.replayMissing,
	}, err
}

// LoadServerVoiceMutationReplayForTest exposes strict replay decoding errors.
func (s *NATSSubscriber) LoadServerVoiceMutationReplayForTest(
	ctx context.Context,
	senderID, targetRoomID uuid.UUID,
	eventAt time.Time,
) error {
	_, _, err := s.loadServerVoiceMutationReplay(ctx, senderID, targetRoomID, eventAt)
	return err
}

// UpsertPrivateVoiceParticipantForTest exposes the exact-call participant-set
// mutation for deterministic cross-replica cap/concurrency integration tests.
func (s *NATSSubscriber) UpsertPrivateVoiceParticipantForTest(
	ctx context.Context,
	conversationID, senderID, callID uuid.UUID,
	eventAt time.Time,
) (bool, error) {
	applied, _, err := s.upsertPrivateVoiceParticipant(
		ctx, conversationID, senderID, callID, eventAt,
	)
	return applied, err
}

// DeleteCapturedPrivateActivityGenerationsForTest exposes exact-generation
// cleanup classification for focused fail-closed integration tests.
func (s *NATSSubscriber) DeleteCapturedPrivateActivityGenerationsForTest(
	ctx context.Context,
	participantIDs []uuid.UUID,
	generations map[uuid.UUID]presence.ActivityGeneration,
) error {
	return s.deleteCapturedPrivateActivityGenerations(ctx, participantIDs, generations)
}

// Export unexported methods and types for testing.

// TempGrantAllow exposes the temp-grant allow bitmask for assertion in tests (#487 D1).
const TempGrantAllow = tempGrantAllow

// TestTempGrantManager is an exported wrapper around tempGrantManager so external
// voice_test package tests can exercise the grant/revoke convergence (#487 Scope C).
type TestTempGrantManager struct {
	m *tempGrantManager
}

// NewTestTempGrantManager builds a tempGrantManager for testing. nats may be nil
// (publishForceDisconnect is then a no-op).
func NewTestTempGrantManager(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver, nats *natsclient.Client) *TestTempGrantManager {
	return &TestTempGrantManager{m: newTempGrantManager(db, log, hub, resolver, nats)}
}

// Grant exposes grantTemporaryChannelAccess.
func (t *TestTempGrantManager) Grant(ctx context.Context, serverID, channelID, userID string) error {
	return t.m.grantTemporaryChannelAccess(ctx, serverID, channelID, userID)
}

// Revoke exposes revokeTemporaryChannelAccess.
func (t *TestTempGrantManager) Revoke(ctx context.Context, serverID, channelID, userID, actorID string) error {
	return t.m.revokeTemporaryChannelAccess(ctx, serverID, channelID, userID, actorID)
}

// HasTemporaryGrant exposes hasTemporaryGrant.
func (t *TestTempGrantManager) HasTemporaryGrant(ctx context.Context, channelID, userID string) (bool, error) {
	return t.m.hasTemporaryGrant(ctx, channelID, userID)
}

// SweepOrphanedTempGrants exposes the sweeper's orphan sweep for testing (#487 T9).
// Returns the number of orphaned temp grants revoked.
func (s *TempGrantSweeper) SweepOrphanedTempGrants(ctx context.Context) (int, error) {
	return s.sweepOrphanedTempGrants(ctx)
}

// HandleJoined exposes handleJoined for testing.
func (s *NATSSubscriber) HandleJoined(data []byte) { s.handleJoined(data) }

// HandleLeft exposes handleLeft for testing.
func (s *NATSSubscriber) HandleLeft(data []byte) { s.handleLeft(data) }

// HandleRoomEmpty exposes handleRoomEmpty for testing.
func (s *NATSSubscriber) HandleRoomEmpty(data []byte) { s.handleRoomEmpty(data) }

// HandleDMRoomEmptyReplicaForTest exercises one remote replica's terminal path
// without the process-local lifecycle lock shared by in-process test replicas.
func (s *NATSSubscriber) HandleDMRoomEmptyReplicaForTest(
	data []byte,
	conversationID uuid.UUID,
) bool {
	var event voiceRoomEmptyEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return false
	}
	eventAt, err := parseVoiceEventTime(event.Timestamp)
	if err != nil {
		return false
	}
	return s.handleDMRoomEmpty(event, conversationID, eventAt)
}

// HandleHeartbeat exposes handleHeartbeat for testing.
func (s *NATSSubscriber) HandleHeartbeat(data []byte) { s.handleHeartbeat(data) }

// TestRoomContext is an exported wrapper around roomContext for testing.
type TestRoomContext struct {
	IsDM     bool
	ServerID string
}

// ResolveRoom exposes resolveRoom for testing and returns a TestRoomContext.
func (s *NATSSubscriber) ResolveRoom(channelID string) (*TestRoomContext, error) {
	ctx, err := s.resolveRoom(context.Background(), channelID)
	if err != nil {
		return nil, err
	}
	return &TestRoomContext{
		IsDM:     ctx.isDM,
		ServerID: ctx.serverID,
	}, nil
}

// ReEnforceServer exposes reEnforceServer for testing.
func (s *NATSSubscriber) ReEnforceServer(serverID, channelID, userID string) {
	s.reEnforceServer(context.Background(), serverID, channelID, userID)
}

// ReEnforceDM exposes reEnforceDM for testing.
func (s *NATSSubscriber) ReEnforceDM(channelID, userID string) {
	s.reEnforceDM(context.Background(), channelID, userID)
}

// PublishForceDisconnect exposes publishForceDisconnect for testing (#487 P3).
func (s *NATSSubscriber) PublishForceDisconnect(channelID, userID string) {
	s.publishForceDisconnect(channelID, userID)
}
