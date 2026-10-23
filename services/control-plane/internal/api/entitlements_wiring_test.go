package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
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

func (r *recordingBroadcaster) DisconnectUserContext(ctx context.Context, id uuid.UUID) bool {
	if ctx.Err() != nil {
		return false
	}
	r.disconnected = append(r.disconnected, id)
	return true
}

func (r *recordingBroadcaster) BroadcastToUserContext(ctx context.Context, id uuid.UUID, msg websocket.OutgoingMessage) bool {
	if ctx.Err() != nil {
		return false
	}
	r.broadcasts = append(r.broadcasts, recordedBroadcast{userID: id, msg: msg})
	return true
}

// TestEntitlementNotifier_BroadcastEntitlements locks the live-update wire
// contract: the literal "entitlements_changed" type string (the discriminator
// the client's EntitlementsChangedSchema keys on) and the full 25-key camelCase
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
	// The full EntitlementDTO includes the deprecated seconds key so older
	// desktop clients can still parse the live entitlement push.
	require.Len(t, b.msg.Data, 25)
	assert.Equal(t, "premium", b.msg.Data["tier"])
	assert.Contains(t, b.msg.Data, "maxMessageChars")
	assert.Contains(t, b.msg.Data, "usernameChangeIntervalMonths")
	assert.Contains(t, b.msg.Data, "usernameChangeIntervalSeconds")
	assert.Contains(t, b.msg.Data, "streamMaxPixelRate")
	assert.Empty(t, rb.disconnected, "a broadcast must not disconnect")
}

// TestEntitlementNotifier_DisconnectUser verifies the adapter delegates the
// downgrade force-disconnect straight to the hub.
func TestEntitlementNotifier_DisconnectUser(t *testing.T) {
	rb := &recordingBroadcaster{}
	n := NewEntitlementNotifier(rb, logger.New("test"))
	uid := uuid.New()

	require.True(t, n.DisconnectUserContext(context.Background(), uid))

	require.Len(t, rb.disconnected, 1)
	assert.Equal(t, uid, rb.disconnected[0])
	assert.Empty(t, rb.broadcasts, "a disconnect must not broadcast")
}

// blockingEntitlementHub models a Hub queue that cannot drain until cleanup.
type blockingEntitlementHub struct {
	blockBroadcast bool
	disconnected   bool
	entered        chan struct{}
	release        chan struct{}
}

func (h *blockingEntitlementHub) wait(ctx context.Context) bool {
	close(h.entered)
	select {
	case <-h.release:
		return true
	case <-ctx.Done():
		return false
	}
}
func (h *blockingEntitlementHub) BroadcastToUserContext(ctx context.Context, _ uuid.UUID, _ websocket.OutgoingMessage) bool {
	if h.blockBroadcast {
		return h.wait(ctx)
	}
	return ctx.Err() == nil
}
func (h *blockingEntitlementHub) DisconnectUserContext(ctx context.Context, _ uuid.UUID) bool {
	if !h.blockBroadcast {
		h.disconnected = h.wait(ctx)
		return h.disconnected
	}
	h.disconnected = ctx.Err() == nil
	return h.disconnected
}

func TestOnTierChange_BlockedHubHonorsCancellation(t *testing.T) {
	for _, broadcast := range []bool{true, false} {
		t.Run(map[bool]string{true: "broadcast", false: "disconnect"}[broadcast], func(t *testing.T) {
			rdb := redistest.Client(t)
			t.Cleanup(func() { require.NoError(t, rdb.Close()) })
			hub := &blockingEntitlementHub{blockBroadcast: broadcast, entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(hub.release) }) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- entitlements.OnTierChange(ctx, entitlements.NewCache(rdb, nil), NewEntitlementNotifier(hub, logger.New("test")), uuid.New(), "premium", "free")
			}()
			<-hub.entered
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
				if broadcast {
					assert.True(t, hub.disconnected, "downgrade revocation must precede a stalled capability broadcast")
				}
			case <-time.After(time.Second):
				t.Fatal("tier change ignored cancellation while the Hub queue was blocked")
			}
		})
	}
}
