package voice

import (
	"context"
	"database/sql"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
)

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

// HandleHeartbeat exposes handleHeartbeat for testing.
func (s *NATSSubscriber) HandleHeartbeat(data []byte) { s.handleHeartbeat(data) }

// TestRoomContext is an exported wrapper around roomContext for testing.
type TestRoomContext struct {
	IsDM     bool
	ServerID string
}

// ResolveRoom exposes resolveRoom for testing and returns a TestRoomContext.
func (s *NATSSubscriber) ResolveRoom(channelID string) (*TestRoomContext, error) {
	ctx, err := s.resolveRoom(channelID)
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
	s.reEnforceServer(serverID, channelID, userID)
}

// ReEnforceDM exposes reEnforceDM for testing.
func (s *NATSSubscriber) ReEnforceDM(channelID, userID string) {
	s.reEnforceDM(channelID, userID)
}

// PublishForceDisconnect exposes publishForceDisconnect for testing (#487 P3).
func (s *NATSSubscriber) PublishForceDisconnect(channelID, userID string) {
	s.publishForceDisconnect(channelID, userID)
}
