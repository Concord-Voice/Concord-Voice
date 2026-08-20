package websocket

import (
	"bytes"
	"fmt"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	gorillaws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression locks for #2854 stage B2: a forged or replayed
// `presence.erasure.cleared` burst must never convert
// Hub.ClearErasedSenderCustomText's unconditional fan-out into a fleet-wide
// Rich Presence disconnect.
//
// The fan-out is best-effort per client: a client whose send queue has no
// immediate capacity DROPS the frame. It is never closed, and its already
// queued frames are never inspected, dequeued, or replaced. A dropped
// erasure-clear costs that client a stale custom-status string it will discard
// on its next reconnect snapshot; a close costs every device on the replica a
// full reconnect, which is exactly the denial-of-service primitive B2 removes.

// erasureClearSendCapacity is the send-queue capacity these tests give their
// victims. It is chosen to match the value production happens to use today
// (`Send: make(chan []byte, 256)`, a bare literal in handler.go), but it is a
// TEST-LOCAL constant and nothing links the two: these tests build their own
// channels and never read the production value, so retuning handler.go would
// not red anything here. Do not describe it as a mirror or a tripwire.
const erasureClearSendCapacity = 256

// newErasureClearSocketServer starts one upgrade endpoint that parks on read for
// the lifetime of each accepted socket. Parking is what makes closure
// observable: nothing on the peer side closes the connection, so a closed
// client socket can only have been closed by the code under test.
func newErasureClearSocketServer(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := gorillaws.Upgrader{
			CheckOrigin: func(request *http.Request) bool {
				return request.Header.Get("Origin") == ""
			},
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, readErr := conn.ReadMessage(); readErr != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// connectLiveRichPresenceClient registers a rich-presence-capable client backed
// by a REAL socket, so "was this client closed?" is answered by connection
// state rather than by a log string.
func connectLiveRichPresenceClient(
	t *testing.T,
	hub *Hub,
	socketURL string,
	sendCapacity int,
) *Client {
	t.Helper()
	conn, _, err := gorillaws.DefaultDialer.Dial(socketURL, nil)
	require.NoError(t, err, "test socket must connect")
	t.Cleanup(func() { _ = conn.Close() })

	client := connectClient(hub, uuid.New())
	client.Conn = conn
	client.Send = make(chan []byte, sendCapacity)
	client.activityRichPresenceCapable = true
	return client
}

// erasureClearSocketOpen probes real connection state. A ping write succeeds on
// a live socket and fails once Conn.Close has run, which is precisely the
// disconnect this stage must stop producing.
func erasureClearSocketOpen(client *Client) bool {
	return client.Conn.WriteControl(
		gorillaws.PingMessage, nil, time.Now().Add(2*time.Second),
	) == nil
}

func countClosedErasureClearSockets(clients []*Client) int {
	closed := 0
	for _, client := range clients {
		if !erasureClearSocketOpen(client) {
			closed++
		}
	}
	return closed
}

// captureErasureClearLog redirects the stdlib logger for the duration of a test.
// The buffer is DIAGNOSTIC ONLY — it is summarized into failure messages and
// never asserted on, because the fan-out's log wording is what B2 changes.
func captureErasureClearLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := stdlog.Default().Writer()
	stdlog.SetOutput(&buf)
	t.Cleanup(func() { stdlog.SetOutput(previous) })
	return &buf
}

// erasureClearLogSummary keeps a burst test's failure output readable: a
// thousand-message burst can emit thousands of lines, which would bury the
// assertion that actually tripped.
func erasureClearLogSummary(logs *bytes.Buffer) string {
	lines := strings.Split(strings.TrimRight(logs.String(), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return "hub emitted no log lines"
	}
	return fmt.Sprintf("hub emitted %d log line(s), first: %s", len(lines), lines[0])
}

// TestClearErasedSenderCustomText_ForgedBurstBeyondSendCapacityDisconnectsNobody
// is the exploit inversion: the burst that used to disconnect every Rich
// Presence client on the replica must now disconnect none of them.
func TestClearErasedSenderCustomText_ForgedBurstBeyondSendCapacityDisconnectsNobody(t *testing.T) {
	hub := NewHub(nil, nil)
	socketURL := newErasureClearSocketServer(t)
	logs := captureErasureClearLog(t)

	// Three unrelated bystanders. None has ANY relationship to the identifiers
	// the burst names — that is the point, the frame is unconditional.
	victims := make([]*Client, 0, 3)
	for i := 0; i < 3; i++ {
		victims = append(victims,
			connectLiveRichPresenceClient(t, hub, socketURL, erasureClearSendCapacity))
	}
	require.Equal(t, 0, countClosedErasureClearSockets(victims),
		"precondition: every bystander starts connected")

	// Each iteration is ONE forged publish of {"user_id":"<random uuid>"} on the
	// unauthenticated `presence.erasure.cleared` subject.
	const burst = erasureClearSendCapacity + 1
	for i := 0; i < burst; i++ {
		hub.ClearErasedSenderCustomText(uuid.New())
	}

	t.Run("no bystander is disconnected by the overflow", func(t *testing.T) {
		assert.Equal(t, 0, countClosedErasureClearSockets(victims),
			"%d forged bus messages must disconnect nobody (%s)",
			burst, erasureClearLogSummary(logs))
	})

	t.Run("every bystander stays usable for later privacy frames", func(t *testing.T) {
		for i, victim := range victims {
			require.Len(t, victim.Send, erasureClearSendCapacity,
				"victim %d queue should be saturated, not drained", i)
			<-victim.Send // the write pump drains one frame
			assert.True(t, victim.enqueueOutbound([]byte(`{"type":"probe"}`)),
				"victim %d must still accept a frame once its queue drains", i)
		}
	})
}

// driveErasureClearBootstrapCompletion runs the PRODUCTION bootstrap-completion
// entry point that hub.handleRegister's async runClientBootstrap reaches. No
// test double: completePreparedClientBootstrap -> clientBootstrapCanPrepare ->
// completeClientBootstrap, whose failure arm is
// failClientBootstrapFlushLocked -> disconnectPrivacyCriticalClient.
//
// The fan-out never closes a socket itself, so a reconnect-window regression
// that stops at "the socket is still open after the burst" proves nothing: the
// close happens here, later, on the victim's own goroutine. Every such test
// below must drive this before it asserts survival.
func driveErasureClearBootstrapCompletion(
	t *testing.T,
	hub *Hub,
	client *Client,
) (completed bool, snapshotPrepared bool) {
	t.Helper()
	prepared := false
	done, err := hub.completePreparedClientBootstrap(client, func(publish func([]byte) error) error {
		prepared = true
		snapshot, marshalErr := marshalCustomTextFrame(uuid.New(), nil)
		require.NoError(t, marshalErr)
		return publish(snapshot)
	}, nil)
	require.NoError(t, err)
	return done, prepared
}

// erasureClearBootstrapFailed reads the reconnect-replacement failure latch —
// the state a forged burst used to poison, and the earliest observable evidence
// that a disconnect is already committed even while the socket is still up.
func erasureClearBootstrapFailed(client *Client) bool {
	client.bootstrapMu.Lock()
	defer client.bootstrapMu.Unlock()
	return client.bootstrapFailed
}

// erasureClearBootstrapBuffered reports how many frames the burst appended to
// the client's pending replacement. A non-zero count is a latent disconnect
// even without an overflow: completeClientBootstrap's preflight requires
// cap(Send)-len(Send) >= 1+len(replay)+len(live).
func erasureClearBootstrapBuffered(client *Client) int {
	client.bootstrapMu.Lock()
	defer client.bootstrapMu.Unlock()
	return len(client.bootstrapReplay) + len(client.bootstrapLive)
}

// TestClearErasedSenderCustomText_ForgedBurstDuringReconnectDisconnectsNobody
// locks the reconnect-window half of the fan-out, which a socket-only assertion
// cannot see. handleRegister opens the replacement boundary BEFORE publishing
// the client in the Hub maps, and rich-presence capability is set at
// construction, so for the whole async bootstrap window a victim is both inside
// h.clients (the fan-out reaches it) and bootstrapActive. A fan-out routed
// through bufferBootstrapLive therefore latches bootstrapFailed at 256 forged
// messages — one FEWER than a full send queue costs, and independent of queue
// capacity — and the completion path closes the socket afterwards.
func TestClearErasedSenderCustomText_ForgedBurstDuringReconnectDisconnectsNobody(t *testing.T) {
	hub := NewHub(nil, nil)
	socketURL := newErasureClearSocketServer(t)
	logs := captureErasureClearLog(t)

	victim := connectLiveRichPresenceClient(t, hub, socketURL, erasureClearSendCapacity)
	victim.beginBootstrap() // mid-reconnect, exactly as handleRegister leaves it
	require.True(t, erasureClearSocketOpen(victim), "precondition: the victim starts connected")
	require.False(t, erasureClearBootstrapFailed(victim),
		"precondition: the replacement latch starts clean")

	const burst = clientBootstrapBufferedFrameLimit + 1
	for i := 0; i < burst; i++ {
		hub.ClearErasedSenderCustomText(uuid.New())
	}

	t.Run("the burst never latches bootstrapFailed", func(t *testing.T) {
		assert.False(t, erasureClearBootstrapFailed(victim),
			"%d forged bus messages must not poison a reconnecting client's bootstrap (%s)",
			burst, erasureClearLogSummary(logs))
	})

	t.Run("the frames are dropped, not appended to the pending replacement", func(t *testing.T) {
		assert.Equal(t, 0, erasureClearBootstrapBuffered(victim),
			"a bootstrapping client's replacement buffer must stay untouched")
		assert.Empty(t, victim.Send,
			"and nothing may be written into the socket queue behind the replacement")
	})

	t.Run("the production completion path then completes the bootstrap", func(t *testing.T) {
		completed, prepared := driveErasureClearBootstrapCompletion(t, hub, victim)
		assert.True(t, prepared,
			"an unpoisoned client must still have its snapshot prepared")
		assert.True(t, completed, "the bootstrap must complete (%s)", erasureClearLogSummary(logs))
		assert.True(t, erasureClearSocketOpen(victim),
			"a forged erasure-clear burst must not disconnect a reconnecting client")
		assert.Len(t, victim.Send, 1,
			"the completed bootstrap must have flushed exactly its snapshot")
	})
}

// TestClearErasedSenderCustomText_ForgedBurstDisconnectsNoReconnectingClient is
// the fleet-scale form. A disconnected client reconnects and a reconnecting
// client is bootstrapActive, so a burst that killed this fleet would be a
// self-refreshing reconnect loop for the whole replica under a sustained flood.
func TestClearErasedSenderCustomText_ForgedBurstDisconnectsNoReconnectingClient(t *testing.T) {
	hub := NewHub(nil, nil)
	socketURL := newErasureClearSocketServer(t)
	logs := captureErasureClearLog(t)

	const fleet = 5
	victims := make([]*Client, 0, fleet)
	for i := 0; i < fleet; i++ {
		victim := connectLiveRichPresenceClient(t, hub, socketURL, erasureClearSendCapacity)
		victim.beginBootstrap() // every one of them is mid-reconnect
		victims = append(victims, victim)
	}
	require.Equal(t, 0, countClosedErasureClearSockets(victims),
		"precondition: the whole fleet starts connected")

	const burst = clientBootstrapBufferedFrameLimit + 1
	for i := 0; i < burst; i++ {
		hub.ClearErasedSenderCustomText(uuid.New())
	}
	for i, victim := range victims {
		completed, _ := driveErasureClearBootstrapCompletion(t, hub, victim)
		assert.True(t, completed, "victim %d must complete its bootstrap", i)
	}

	t.Run("no reconnecting client is disconnected", func(t *testing.T) {
		assert.Equal(t, 0, countClosedErasureClearSockets(victims),
			"one %d-message forged burst must disconnect none of the %d reconnecting clients (%s)",
			burst, fleet, erasureClearLogSummary(logs))
	})

	t.Run("no reconnecting client had its bootstrap latch poisoned", func(t *testing.T) {
		for i, victim := range victims {
			assert.False(t, erasureClearBootstrapFailed(victim),
				"victim %d must have a clean replacement latch", i)
		}
	})
}

// TestClearErasedSenderCustomText_ReconnectSurvivalIsIndependentOfSendQueueCapacity
// pins the capacity-independence property in its surviving direction. The
// reconnect-window kill never consulted cap(Send) at all — it counted frames
// into the replacement buffer — so an over-provisioned queue bought a victim
// nothing. The fix has to be capacity-independent in the same way: a victim
// with 4x production capacity, fully EMPTY, must survive because the frame is
// dropped at the bootstrap gate, not because it had room to absorb the burst.
func TestClearErasedSenderCustomText_ReconnectSurvivalIsIndependentOfSendQueueCapacity(t *testing.T) {
	hub := NewHub(nil, nil)
	socketURL := newErasureClearSocketServer(t)
	logs := captureErasureClearLog(t)

	const overProvisioned = erasureClearSendCapacity * 4
	victim := connectLiveRichPresenceClient(t, hub, socketURL, overProvisioned)
	victim.beginBootstrap()
	require.True(t, erasureClearSocketOpen(victim), "precondition: the victim starts connected")
	require.Empty(t, victim.Send, "precondition: all %d queue slots are empty", overProvisioned)

	const burst = clientBootstrapBufferedFrameLimit + 1
	for i := 0; i < burst; i++ {
		hub.ClearErasedSenderCustomText(uuid.New())
	}

	t.Run("the burst is dropped without consuming queue capacity", func(t *testing.T) {
		assert.False(t, erasureClearBootstrapFailed(victim),
			"message #%d must not kill a victim holding %d empty queue slots (%s)",
			burst, overProvisioned, erasureClearLogSummary(logs))
		assert.Empty(t, victim.Send,
			"the socket queue must never be touched while the replacement is open")
	})

	t.Run("the over-provisioned victim survives the production completion path", func(t *testing.T) {
		completed, _ := driveErasureClearBootstrapCompletion(t, hub, victim)
		assert.True(t, completed, "the bootstrap must complete")
		assert.True(t, erasureClearSocketOpen(victim),
			"survival must not depend on send-queue capacity")
	})
}

// TestClearErasedSenderCustomText_ThousandMessageBurstDisconnectsNobody is the
// stated acceptance criterion for B2.
func TestClearErasedSenderCustomText_ThousandMessageBurstDisconnectsNobody(t *testing.T) {
	hub := NewHub(nil, nil)
	socketURL := newErasureClearSocketServer(t)
	logs := captureErasureClearLog(t)

	victims := make([]*Client, 0, 3)
	for i := 0; i < 3; i++ {
		victims = append(victims,
			connectLiveRichPresenceClient(t, hub, socketURL, erasureClearSendCapacity))
	}
	require.Equal(t, 0, countClosedErasureClearSockets(victims),
		"precondition: every bystander starts connected")

	const burst = 1000
	for i := 0; i < burst; i++ {
		hub.ClearErasedSenderCustomText(uuid.New())
	}

	t.Run("no bystander is disconnected by the burst", func(t *testing.T) {
		assert.Equal(t, 0, countClosedErasureClearSockets(victims),
			"%d forged bus messages must disconnect nobody (%s)",
			burst, erasureClearLogSummary(logs))
	})

	// Without this subtest the test above is VACUOUS: "0 closed" also holds when
	// the fan-out reached nobody at all. A byte-identical copy built with
	// activityRichPresenceCapable = false passed it with zero frames delivered.
	// Saturation is the delivery witness — the burst is 1000 against a 256-slot
	// queue, so a fan-out that ran must leave every queue exactly full.
	t.Run("the burst actually reached every bystander", func(t *testing.T) {
		for i, victim := range victims {
			require.Len(t, victim.Send, erasureClearSendCapacity,
				"bystander %d must be saturated by the burst, proving the fan-out ran", i)
		}
	})
}

// TestClearErasedSenderCustomText_FullSendQueueDropsFrameWithoutClosingClient is
// the drop-path unit. A full queue is never inspected or mutated, and the
// already queued frame outranks the new one.
func TestClearErasedSenderCustomText_FullSendQueueDropsFrameWithoutClosingClient(t *testing.T) {
	hub := NewHub(nil, nil)
	socketURL := newErasureClearSocketServer(t)
	logs := captureErasureClearLog(t)

	client := connectLiveRichPresenceClient(t, hub, socketURL, 1)
	queued, err := marshalCustomTextFrame(uuid.New(), &CustomTextPayload{Text: "already queued"})
	require.NoError(t, err)
	client.Send <- queued
	require.Len(t, client.Send, 1, "precondition: the send queue is full")
	require.True(t, erasureClearSocketOpen(client), "precondition: the client starts connected")

	hub.ClearErasedSenderCustomText(uuid.New())

	t.Run("the client is not closed", func(t *testing.T) {
		assert.True(t, erasureClearSocketOpen(client),
			"a full queue must drop the clear, not disconnect (%s)",
			erasureClearLogSummary(logs))
	})

	t.Run("the new frame is dropped and the queued frame is intact", func(t *testing.T) {
		require.Len(t, client.Send, 1, "the clear must be dropped, never queued or swapped in")
		assert.Equal(t, queued, <-client.Send, "the previously queued frame must be untouched")
	})
}

// TestClearErasedSenderCustomText_DeliversClearFrameToRichPresenceClient is the
// happy path: with capacity available the fan-out still delivers.
func TestClearErasedSenderCustomText_DeliversClearFrameToRichPresenceClient(t *testing.T) {
	hub := NewHub(nil, nil)
	client := connectClient(hub, uuid.New())
	client.activityRichPresenceCapable = true
	erasedSender := uuid.New()

	hub.ClearErasedSenderCustomText(erasedSender)

	expected, err := marshalCustomTextFrame(erasedSender, nil)
	require.NoError(t, err)
	require.Len(t, client.Send, 1, "a client with capacity must receive exactly one clear")
	assert.Equal(t, expected, <-client.Send,
		"the delivered frame must be the erased sender's rich_presence_clear")
}

// TestClearErasedSenderCustomText_SkipsClientWithoutRichPresenceCapability locks
// the capability gate: an older client that would misapply the frame gets none.
//
// The capable client sharing the hub is what makes the exclusion meaningful. On
// its own, "the non-capable client received nothing" also holds when the fan-out
// ran for nobody — a broken fan-out and a correct gate are indistinguishable. The
// pair asserts the gate DISCRIMINATES: same hub, same message, one delivery.
func TestClearErasedSenderCustomText_SkipsClientWithoutRichPresenceCapability(t *testing.T) {
	hub := NewHub(nil, nil)
	legacy := connectClient(hub, uuid.New()) // activityRichPresenceCapable stays false
	capable := connectClient(hub, uuid.New())
	capable.activityRichPresenceCapable = true
	erasedSender := uuid.New()

	hub.ClearErasedSenderCustomText(erasedSender)

	t.Run("the non-rich-presence client receives nothing", func(t *testing.T) {
		assert.Empty(t, legacy.Send, "a non-rich-presence client must receive nothing")
	})

	t.Run("the rich-presence client in the same hub receives exactly one clear", func(t *testing.T) {
		expected, err := marshalCustomTextFrame(erasedSender, nil)
		require.NoError(t, err)
		require.Len(t, capable.Send, 1,
			"the capable client proves the fan-out ran; without it the exclusion above is vacuous")
		assert.Equal(t, expected, <-capable.Send,
			"the delivered frame must be the erased sender's rich_presence_clear")
	})
}

// TestClearErasedSenderCustomText_DeliversOneFrameToEveryCapableClient pins
// fan-out COMPLETENESS, which is a property to keep: an erasure clear is a
// right-to-erasure retraction, so a client with capacity that does not receive
// it keeps showing the erased user's Custom Status. One bus message must reach
// every connected rich-presence client, exactly once each.
//
// This survives stage B1. B1 rate-limits at the NATS subscriber dispatch
// boundary — it decides how many bus messages reach the fan-out at all — and
// does not change what a single call to this function delivers per client.
func TestClearErasedSenderCustomText_DeliversOneFrameToEveryCapableClient(t *testing.T) {
	hub := NewHub(nil, nil)
	const connected = 5
	clients := make([]*Client, 0, connected)
	for i := 0; i < connected; i++ {
		client := connectClient(hub, uuid.New())
		client.activityRichPresenceCapable = true
		clients = append(clients, client)
	}

	erasedSender := uuid.New()
	hub.ClearErasedSenderCustomText(erasedSender) // ONE bus message

	expected, err := marshalCustomTextFrame(erasedSender, nil)
	require.NoError(t, err)
	// Asserted per client, not as a total: a sum of `connected` is also produced
	// by five frames landing on one client and none on the rest, which is the
	// incomplete fan-out this test exists to exclude.
	for i, client := range clients {
		require.Len(t, client.Send, 1,
			"client %d must receive exactly one frame from one bus message", i)
		assert.Equal(t, expected, <-client.Send,
			"client %d must receive the erased sender's rich_presence_clear", i)
	}
}

// TestClearErasedSenderCustomText_NilHubReceiverIsNoOp locks the fail-closed
// nil-receiver guard: a hub-less replica has no local clients to clear, and a
// panic inside the NATS callback would take the subscriber down.
func TestClearErasedSenderCustomText_NilHubReceiverIsNoOp(t *testing.T) {
	var hub *Hub
	assert.NotPanics(t, func() { hub.ClearErasedSenderCustomText(uuid.New()) })
}
