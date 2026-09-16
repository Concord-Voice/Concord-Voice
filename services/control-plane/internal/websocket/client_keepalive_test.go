package websocket

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/google/uuid"
	gorillaws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startWritePump runs a REAL writePump against a real socket and returns the
// server-side Client plus the dialer's end. writePump touches only Conn and Send,
// so a minimal literal is a faithful subject rather than a stub.
func startWritePump(t *testing.T) (*Client, *gorillaws.Conn) {
	t.Helper()
	ready := make(chan *Client, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := gorillaws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := &Client{ID: uuid.New(), UserID: uuid.New(), Conn: conn, Send: make(chan []byte, 64)}
		ready <- c
		c.writePump()
	}))
	t.Cleanup(srv.Close)

	peer, _, err := gorillaws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })

	return <-ready, peer
}

// peerTextFrames surfaces every TEXT frame the peer receives. Protocol pings are
// absent by construction: gorilla answers them inside ReadMessage and never
// returns them, which is precisely the reason the edge cannot see them either.
func peerTextFrames(t *testing.T, peer *gorillaws.Conn) <-chan string {
	t.Helper()
	frames := make(chan string, 512)
	go func() {
		defer close(frames)
		for {
			_ = peer.SetReadDeadline(time.Now().Add(10 * time.Second))
			typ, b, err := peer.ReadMessage()
			if err != nil {
				return
			}
			if typ == gorillaws.TextMessage {
				select {
				case frames <- string(b):
				default:
				}
			}
		}
	}()
	return frames
}

func sawKeepalive(frames <-chan string, within time.Duration) bool {
	deadline := time.After(within)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return false
			}
			if f == string(heartbeatAckFrame) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func settleAndDrain(frames <-chan string) {
	time.Sleep(60 * time.Millisecond)
	for {
		select {
		case <-frames:
		default:
			return
		}
	}
}

// TestWritePumpEmitsTheKeepaliveOnlyOnAnIdleSocket drives the REAL writePump,
// because the idle gate is not the mechanism -- the WIRING is.
//
// An earlier version of this test asserted a one-line negation helper
// (`!wroteSinceLastTick`) and so constrained nothing that ships. Three mutants
// survived it, and the three phases below kill one each, in order:
//
//   - drop `wroteSinceLastTick = true` in the send arm, and every busy socket in
//     the fleet gains a redundant frame per tick -- phase 2 catches it;
//   - drop the emit block, and the shipped fix does nothing at all -- phase 1;
//   - drop the per-tick reset, and the keepalive fires exactly once per
//     connection and never again -- phase 3, which is the one a single
//     idle-socket assertion cannot see.
func TestWritePumpEmitsTheKeepaliveOnlyOnAnIdleSocket(t *testing.T) {
	restore := writePumpTickInterval
	writePumpTickInterval = 20 * time.Millisecond
	t.Cleanup(func() { writePumpTickInterval = restore })

	client, peer := startWritePump(t)
	frames := peerTextFrames(t, peer)

	// Phase 1 -- silent socket. The keepalive is the only frame that can arrive.
	assert.True(t, sawKeepalive(frames, 2*time.Second),
		"an idle socket must receive the unsolicited keepalive")

	// Phase 2 -- socket already carrying traffic. Writing an order of magnitude
	// faster than the tick means every tick window contains a real frame, so the
	// gate must stay shut for the whole window.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
				select {
				case client.Send <- []byte(`{"type":"noise"}`):
				case <-stop:
					return
				}
			}
		}
	}()
	settleAndDrain(frames) // discard phase 1's acks and the traffic ramp
	assert.False(t, sawKeepalive(frames, 600*time.Millisecond),
		"a socket already carrying traffic must not be given a redundant keepalive")

	// Phase 3 -- traffic stops. The gate must REOPEN.
	close(stop)
	settleAndDrain(frames)
	assert.True(t, sawKeepalive(frames, 2*time.Second),
		"the gate must reopen once the socket goes quiet again")
}

// TestHeartbeatAckFrameIsReusedVerbatim guards the property that makes this
// change client-free: the keepalive sends an EXISTING frame type, so no schema
// member is added, no capability handshake is needed, and no IPC contract moves.
// If someone introduces a distinct keepalive frame later, every un-updated client
// starts logging an unknown-event wire violation -- so the reuse is load-bearing
// and worth an assertion rather than a comment.
func TestHeartbeatAckFrameIsReusedVerbatim(t *testing.T) {
	assert.JSONEq(t, `{"type":"heartbeat_ack","data":{}}`, string(heartbeatAckFrame))
}

// TestAbnormalSocketCloseIsCounted drives a REAL socket death through readPump.
//
// "Abnormal" is the complement of a clean handshake, and the complement is the
// part worth testing: the counter must fire for a vanished TCP connection AND
// stay silent for a well-behaved client that sent a close frame. A test covering
// only the first would pass against a counter that fires unconditionally, which
// would make every ordinary sign-out look like an edge reaping connections --
// the precise misreading #3328's originating incident already suffered once.
func TestAbnormalSocketCloseIsCounted(t *testing.T) {
	run := func(t *testing.T, closePeer func(peer *gorillaws.Conn)) int {
		t.Helper()
		hub := NewHub(nil, setupHubTestRedis(t))
		counter := &opsCounterSpy{}
		hub.opsCounter = counter
		go hub.Run()
		t.Cleanup(func() { hub.closeOnce.Do(func() { close(hub.done) }) })

		pumpDone := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			up := gorillaws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			conn, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			c := &Client{ID: uuid.New(), UserID: uuid.New(), Conn: conn, Send: make(chan []byte, 8), Hub: hub}
			go func() { c.readPump(); close(pumpDone) }()
		}))
		t.Cleanup(srv.Close)

		peer, _, err := gorillaws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		require.NoError(t, err)
		closePeer(peer)

		select {
		case <-pumpDone:
		case <-time.After(5 * time.Second):
			t.Fatal("readPump did not exit after the peer went away")
		}
		return counter.count(opsmetrics.MetricWebSocketAbnormalClosesTotal)
	}

	t.Run("peer vanishes with no close frame", func(t *testing.T) {
		// Dropping the TCP connection underneath the websocket is the 1006 shape
		// the Cloudflare edge produced before the server owned its own keepalive.
		assert.Equal(t, 1, run(t, func(peer *gorillaws.Conn) {
			_ = peer.UnderlyingConn().Close()
		}))
	})

	t.Run("peer closes cleanly", func(t *testing.T) {
		assert.Zero(t, run(t, func(peer *gorillaws.Conn) {
			_ = peer.WriteMessage(gorillaws.CloseMessage,
				gorillaws.FormatCloseMessage(gorillaws.CloseNormalClosure, ""))
			_ = peer.Close()
		}), "a clean close is not abnormal; counting it would make every sign-out look like a reap")
	})

	t.Run("peer closes with an empty payload", func(t *testing.T) {
		// An EMPTY payload, not FormatCloseMessage(CloseNoStatusReceived, "") --
		// gorilla refuses to SEND 1005 as a code, and receiving it as one would be
		// a protocol error rather than the case under test. The wire shape here is
		// a Close frame carrying zero bytes, which RFC 6455 permits and which
		// gorilla's advanceFrame turns into CloseError{Code: 1005} on our side.
		// That is a completed handshake, so it must not reach the counter.
		assert.Zero(t, run(t, func(peer *gorillaws.Conn) {
			_ = peer.WriteMessage(gorillaws.CloseMessage, []byte{})
			_ = peer.Close()
		}), "a close frame with no status code still completed the handshake")
	})

	t.Run("peer closes with an explicit error code", func(t *testing.T) {
		// The guard against over-correcting the case above. Excluding 1005 is one
		// code, not "every CloseError is graceful" -- a peer that names 1008 is
		// reporting a real failure, and widening the predicate to IsCloseError
		// alone would silently swallow exactly the events this counter exists to
		// witness while both cases above stayed green.
		assert.Equal(t, 1, run(t, func(peer *gorillaws.Conn) {
			_ = peer.WriteMessage(gorillaws.CloseMessage,
				gorillaws.FormatCloseMessage(gorillaws.ClosePolicyViolation, "policy"))
			_ = peer.Close()
		}), "an explicit error code is an abnormal close and must still be counted")
	})
}
