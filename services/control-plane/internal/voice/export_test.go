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

// SetDMTerminalDeleteObservedHookForTest observes each DM terminal participant
// delete: (participantID, applied, err). `applied` is the only value that
// distinguishes "the terminal completed" from "the row went away for some other
// reason" -- see the field comment in nats.go for why neither the return value
// nor the logs can do it.
func (s *NATSSubscriber) SetDMTerminalDeleteObservedHookForTest(
	hook func(participantID uuid.UUID, applied bool, err error),
) {
	s.dmTerminalDeleteObservedHook = hook
}

// SetActivityServiceForTest injects RP bridge availability for security-order tests.
func (s *NATSSubscriber) SetActivityServiceForTest(activity *presence.ActivityService) {
	s.activity = activity
}

// CompleteServerVoiceCleanupGraceForTest expires the one-shot startup grace
// without resetting its sync.Once. It returns the deadline armed by the first
// cleanup invocation so tests can assert the lease was bounded correctly.
func (s *NATSSubscriber) CompleteServerVoiceCleanupGraceForTest() time.Time {
	s.serverVoiceCleanupOnce.Do(func() {})
	readyAt := s.serverVoiceCleanupReadyAt
	s.serverVoiceCleanupReadyAt = time.Now().Add(-time.Nanosecond)
	return readyAt
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

// The four bare wrappers below stamp receipt at the moment the test calls them,
// which for a DIRECT handler call is exactly what the dispatcher would have
// stamped: there is no queue between the two, so there is no lateness to carry.
// Use the *At forms whenever the clamp reference is what the test is about --
// a fixture that needs a receipt clock distinct from wall clock, or one that
// must prove a handler did not substitute its own time.Now() (#3205).

// HandleJoined exposes handleJoined for testing.
func (s *NATSSubscriber) HandleJoined(data []byte) { s.handleJoined(data, time.Now()) }

// HandleJoinedAt exposes handleJoined with an explicit NATS receipt time.
func (s *NATSSubscriber) HandleJoinedAt(data []byte, receivedAt time.Time) {
	s.handleJoined(data, receivedAt)
}

// HandleLeft exposes handleLeft for testing.
func (s *NATSSubscriber) HandleLeft(data []byte) { s.handleLeft(data, time.Now()) }

// HandleLeftAt exposes handleLeft with an explicit NATS receipt time.
func (s *NATSSubscriber) HandleLeftAt(data []byte, receivedAt time.Time) {
	s.handleLeft(data, receivedAt)
}

// HandleRoomEmpty exposes handleRoomEmpty for testing.
func (s *NATSSubscriber) HandleRoomEmpty(data []byte) { s.handleRoomEmpty(data, time.Now()) }

// HandleRoomEmptyAt exposes handleRoomEmpty with an explicit NATS receipt time.
func (s *NATSSubscriber) HandleRoomEmptyAt(data []byte, receivedAt time.Time) {
	s.handleRoomEmpty(data, receivedAt)
}

// HandleDMRoomEmptyReplicaForTest exercises one remote replica's terminal path
// without the process-local lifecycle lock shared by in-process test replicas.
//
// receivedAt is a REQUIRED parameter rather than a time.Now() taken inside, so
// no test path can model drain-time clamping (#3205).
func (s *NATSSubscriber) HandleDMRoomEmptyReplicaForTest(
	data []byte,
	conversationID uuid.UUID,
	receivedAt time.Time,
) bool {
	var event voiceRoomEmptyEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return false
	}
	eventAt, _, err := parseVoiceEventTime(event.Timestamp, receivedAt)
	if err != nil {
		return false
	}
	return s.handleDMRoomEmpty(event, conversationID, eventAt)
}

// HandleHeartbeat exposes handleHeartbeat for testing.
func (s *NATSSubscriber) HandleHeartbeat(data []byte) { s.handleHeartbeat(data, time.Now()) }

// HandleHeartbeatAt exposes handleHeartbeat with an explicit NATS receipt time.
func (s *NATSSubscriber) HandleHeartbeatAt(data []byte, receivedAt time.Time) {
	s.handleHeartbeat(data, receivedAt)
}

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

// StaleServerVoiceDiscoverySQLForTest exposes the lease-discovery statement so
// the plan-shape guard runs against the exact SQL production issues.
func StaleServerVoiceDiscoverySQLForTest() string { return staleServerVoiceDiscoverySQL }

// ParseVoiceEventTimeForTest exposes the clamp so its branches are reachable
// without a database or a dispatcher. #3205: nothing pinned this function
// before, and the clamp changes a return VALUE rather than an error, so no
// existing test could have failed on it.
func ParseVoiceEventTimeForTest(
	raw string, receivedAt time.Time,
) (time.Time, time.Duration, error) {
	return parseVoiceEventTime(raw, receivedAt)
}

// MaxVoiceLifecycleForwardSkewForTest exposes the renewal's forward-skew ceiling
// so a regression test binds to the constant rather than to a copied literal.
const MaxVoiceLifecycleForwardSkewForTest = maxVoiceLifecycleForwardSkew

// RenewObservedLeaseForFutureStampedRowsForTest drives the set-based renewal
// directly, so its predicate and degrade-on-failure branch are both reachable.
func (s *NATSSubscriber) RenewObservedLeaseForFutureStampedRowsForTest(
	ctx context.Context,
	channelID uuid.UUID,
	participantIDs []uuid.UUID,
) {
	s.renewObservedLeaseForFutureStampedRows(ctx, channelID, participantIDs)
}
