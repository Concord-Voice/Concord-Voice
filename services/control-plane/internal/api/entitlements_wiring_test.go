package api

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// recordedBroadcast captures one BroadcastToUser call.
type recordedBroadcast struct {
	userID uuid.UUID
	msg    websocket.OutgoingMessage
}

// recordingBroadcaster satisfies entitlementBroadcaster and records calls so the
// adapter's wire output can be asserted without a real WebSocket hub.
type recordingBroadcaster struct {
	disconnected []uuid.UUID
	broadcasts   []recordedBroadcast
}

func (r *recordingBroadcaster) DisconnectUser(id uuid.UUID) {
	r.disconnected = append(r.disconnected, id)
}

func (r *recordingBroadcaster) BroadcastToUser(id uuid.UUID, msg websocket.OutgoingMessage) {
	r.broadcasts = append(r.broadcasts, recordedBroadcast{userID: id, msg: msg})
}

// TestEntitlementNotifier_BroadcastEntitlements locks the live-update wire
// contract: the literal "entitlements_changed" type string (the discriminator
// the client's EntitlementsChangedSchema keys on) and the full 18-key camelCase
// DTO payload. A typo in the type string or wrong Data wrapping would silently
// break the entire live-update path with no other test catching it.
func TestEntitlementNotifier_BroadcastEntitlements(t *testing.T) {
	rb := &recordingBroadcaster{}
	n := NewEntitlementNotifier(rb, logger.New("test"))
	uid := uuid.New()

	n.BroadcastEntitlements(uid, entitlements.ToDTO(entitlements.For(entitlements.TierPremium)))

	require.Len(t, rb.broadcasts, 1)
	b := rb.broadcasts[0]
	assert.Equal(t, uid, b.userID)
	// The literal discriminator the client zod schema matches on.
	assert.Equal(t, "entitlements_changed", b.msg.Type)
	// The full EntitlementDTO is forwarded as the WS payload (18 camelCase keys).
	require.Len(t, b.msg.Data, 18)
	assert.Equal(t, "premium", b.msg.Data["tier"])
	assert.Contains(t, b.msg.Data, "maxMessageChars")
	assert.Contains(t, b.msg.Data, "usernameChangeIntervalSeconds")
	assert.Empty(t, rb.disconnected, "a broadcast must not disconnect")
}

// TestEntitlementNotifier_DisconnectUser verifies the adapter delegates the
// downgrade force-disconnect straight to the hub.
func TestEntitlementNotifier_DisconnectUser(t *testing.T) {
	rb := &recordingBroadcaster{}
	n := NewEntitlementNotifier(rb, logger.New("test"))
	uid := uuid.New()

	n.DisconnectUser(uid)

	require.Len(t, rb.disconnected, 1)
	assert.Equal(t, uid, rb.disconnected[0])
	assert.Empty(t, rb.broadcasts, "a disconnect must not broadcast")
}
