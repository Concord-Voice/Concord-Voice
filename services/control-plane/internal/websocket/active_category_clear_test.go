package websocket

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
)

// activeClearProbeFrame is the filler these tests queue to saturate a victim's
// send queue. It is deliberately NOT an active-category frame: the drop test
// asserts the queued bytes are untouched, and a frame that could plausibly have
// been produced by the fan-out would make that assertion unreadable.
var activeClearProbeFrame = []byte(`{"type":"probe"}`)

// newTestHubWithRichPresenceClient registers exactly one rich-presence-capable
// client backed by a REAL socket, reusing the erasure-clear fixtures rather than
// inventing a second shape. The live socket is what makes "was anyone
// disconnected?" answerable by connection state instead of by a stubbed hook
// alone -- a future terminal that called client.Conn.Close() directly would slip
// past a hook counter, but not past this.
func newTestHubWithRichPresenceClient(t *testing.T) *Hub {
	t.Helper()
	hub := NewHub(nil, nil)
	connectLiveRichPresenceClient(t, hub, newErasureClearSocketServer(t), erasureClearSendCapacity)
	return hub
}

// soleRichPresenceClient returns the fixture's single capable client, asserting
// the singleton assumption the readers below rely on.
func soleRichPresenceClient(t *testing.T, hub *Hub) *Client {
	t.Helper()
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	var found *Client
	for _, client := range hub.clients {
		if !activityRichPresenceClient(client) {
			continue
		}
		require.Nil(t, found, "the fixture must register exactly one rich-presence client")
		found = client
	}
	require.NotNil(t, found, "the fixture must register a rich-presence client")
	return found
}

// readOneOutboundFrame consumes the fan-out's single delivered frame, asserting
// exactly one was queued: a sum is also produced by a fan-out that duplicates.
func readOneOutboundFrame(t *testing.T, hub *Hub) []byte {
	t.Helper()
	client := soleRichPresenceClient(t, hub)
	require.Len(t, client.Send, 1, "the fan-out must queue exactly one frame")
	return <-client.Send
}

// saturateOutboundQueue fills the fixture client's send queue so the next
// enqueue has no capacity, which is the drop path under test.
func saturateOutboundQueue(t *testing.T, hub *Hub) {
	t.Helper()
	client := soleRichPresenceClient(t, hub)
	for len(client.Send) < cap(client.Send) {
		client.Send <- activeClearProbeFrame
	}
	require.Len(t, client.Send, cap(client.Send), "precondition: the send queue is saturated")
}

// T-6, the #2840 regression guard. The terminal must clear, never disconnect.
// If a later change routes this through any disconnect sink, this test fails --
// which is the entire reason it exists. Do not relax it.
//
// Our exposure is worse than #2840's: the trigger is an ordinary authenticated
// group-DM deletion on a route bounded only by RateLimitByUser (5/min/user),
// not a forged bus
// message, so a disconnect terminal here is a user-reachable DoS primitive.
func TestClearSenderActiveCategoryDisconnectsNobody(t *testing.T) {
	hub := newTestHubWithRichPresenceClient(t)
	victim := soleRichPresenceClient(t, hub)
	disconnects := 0
	hub.customTextClientDisconnect = func(*Client) error {
		disconnects++
		return nil
	}
	require.True(t, erasureClearSocketOpen(victim), "precondition: the victim starts connected")

	hub.ClearSenderActiveCategory(uuid.New(), presence.CategoryPrivateCall)

	assert.Zero(t, disconnects, "the proportional terminal must never disconnect a client")
	assert.True(t, erasureClearSocketOpen(victim),
		"no client socket may be closed, including outside the disconnect hook")
	// Without this the assertions above are VACUOUS: "nobody disconnected" also
	// holds for a fan-out that reached nobody at all.
	assert.Len(t, victim.Send, 1, "the fan-out must have run, proving the guard is not vacuous")
}

func TestClearSenderActiveCategorySendsOneClearFrame(t *testing.T) {
	hub := newTestHubWithRichPresenceClient(t)
	senderID := uuid.New()

	hub.ClearSenderActiveCategory(senderID, presence.CategoryPrivateCall)

	frame := readOneOutboundFrame(t, hub)
	var message map[string]any
	require.NoError(t, json.Unmarshal(frame, &message))
	require.Equal(t, "rich_presence_clear", message["type"])

	data, ok := message["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, senderID.String(), data["user_id"])
	assert.Equal(t, string(presence.CategoryPrivateCall), data["category"])
	assert.NotContains(t, data, "payload", "a clear frame carries no payload")
}

// A saturated client must be dropped, never closed. Closing on overflow is what
// made the Custom Status fan-out a DoS primitive in the first place.
func TestClearSenderActiveCategoryDropsRatherThanClosing(t *testing.T) {
	hub := newTestHubWithRichPresenceClient(t)
	victim := soleRichPresenceClient(t, hub)
	saturateOutboundQueue(t, hub)
	// A second capable client with free capacity. Without it every assertion
	// below is VACUOUS -- "not disconnected, queue unchanged" is exactly what an
	// empty fan-out produces too. This one is the delivery witness: same hub,
	// same call, one delivery.
	witness := connectClient(hub, uuid.New())
	witness.activityRichPresenceCapable = true
	disconnects := 0
	hub.customTextClientDisconnect = func(*Client) error {
		disconnects++
		return nil
	}

	hub.ClearSenderActiveCategory(uuid.New(), presence.CategoryPrivateCall)

	require.Len(t, witness.Send, 1, "the fan-out must have run for the client that had capacity")
	assert.Zero(t, disconnects)
	assert.True(t, erasureClearSocketOpen(victim), "a full queue must drop the clear, not disconnect")
	// The queue is never inspected, dequeued, or swapped: the already queued
	// frame outranks the new one.
	require.Len(t, victim.Send, cap(victim.Send), "the clear must be dropped, never queued")
	assert.Equal(t, activeClearProbeFrame, <-victim.Send, "the queued frame must be untouched")
}

func TestClearSenderActiveCategoryIsNilSafe(t *testing.T) {
	var hub *Hub
	assert.NotPanics(t, func() {
		hub.ClearSenderActiveCategory(uuid.New(), presence.CategoryPrivateCall)
	})
}

// TestClearSenderActiveCategorySkipsClientWithoutRichPresenceCapability locks the
// capability gate. The capable client sharing the hub is what makes the exclusion
// meaningful: alone, "the legacy client received nothing" also holds for a
// fan-out that ran for nobody.
func TestClearSenderActiveCategorySkipsClientWithoutRichPresenceCapability(t *testing.T) {
	hub := newTestHubWithRichPresenceClient(t)
	legacy := connectClient(hub, uuid.New()) // activityRichPresenceCapable stays false
	capable := soleRichPresenceClient(t, hub)

	hub.ClearSenderActiveCategory(uuid.New(), presence.CategoryServerVoice)

	assert.Empty(t, legacy.Send, "a non-rich-presence client must receive nothing")
	assert.Len(t, capable.Send, 1,
		"the capable client proves the fan-out ran; without it the exclusion above is vacuous")
}
