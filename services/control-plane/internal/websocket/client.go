// Package websocket provides WebSocket connection handling for real-time messaging.
package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	// Time allowed to write a message to the peer
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer
	pongWait = 60 * time.Second

	// Send pings to peer with this period (must be less than pongWait)
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer
	maxMessageSize = 512 * 1024 // 512 KB
)

const (
	// Token bucket rate limit for chat messages per client.
	rateLimitBurst    = 10 // max burst
	rateLimitPerSec   = 5  // sustained rate (tokens per second)
	rateLimitInterval = time.Second / time.Duration(rateLimitPerSec)

	// Reconnect replacement work is bounded independently of the socket queue.
	// One slot is reserved for presence_snapshot; the rest cover Custom Status
	// replay plus base/Rich Presence deltas that arrive while it is rebuilt.
	clientBootstrapFrameLimit         = 256
	clientBootstrapBufferedFrameLimit = clientBootstrapFrameLimit - 1
)

var (
	errClientBootstrapInactive           = errors.New("client bootstrap is not active")
	errClientBootstrapCanceled           = errors.New("client bootstrap is canceled")
	errClientBootstrapOverflow           = errors.New("client bootstrap frame limit exceeded")
	errClientBootstrapPublicationMissing = errors.New("client bootstrap snapshot was not published")
)

type bootstrapBufferOutcome uint8

const (
	bootstrapBufferInactive bootstrapBufferOutcome = iota
	bootstrapBufferEnqueued
	bootstrapBufferOverflow
	bootstrapBufferCanceled
)

// Client represents a single WebSocket connection
type Client struct {
	// Unique client ID
	ID uuid.UUID

	// User ID from JWT token
	UserID uuid.UUID

	// presenceDispatchSeq is this client's REGISTRATION FRONTIER: the value of
	// Hub.presenceGenCounter at the moment the client was published into the hub
	// maps. A base-presence delta whose generation is at or below it was
	// dispatched before this client existed, so its content is already carried by
	// the bootstrap snapshot and delivering it would be an out-of-order write on
	// top of that snapshot.
	//
	// Written once, on the Run goroutine under Hub.mu, before publication; read
	// under Hub.mu.RLock in enqueuePresenceForAudience. Never mutated after.
	presenceDispatchSeq uint64

	// EXACTLY ONE of presenceSnapshotOmitted / presenceSnapshotCovered is populated
	// after publication, selected by whether the seed was truncated. Two sets rather
	// than one because the complement is only meaningful when the candidate list IS
	// the connected set: above presenceSnapshotConnectedLimit it is not, and a
	// sender dropped BY truncation never appears in the candidates to be subtracted
	// from — so a complement alone silently reports them covered. That is the >512
	// defect this field was introduced to fix, reintroduced by inverting it.
	//
	// Both exist because capturePresenceSnapshotSeed stops at
	// presenceSnapshotConnectedLimit (512) and omits the remainder fail-closed.
	// Without coverage the registration frontier would drop an omitted sender's
	// in-flight delta too, leaving the viewer rendering that friend offline until
	// some later transition — trading an ordering defect for a delivery one at
	// exactly the scale where it is hardest to notice.
	//
	// presenceSnapshotOmitted is the COMPLEMENT: senders that were snapshot
	// candidates but did NOT reach the published snapshot, whether dropped by
	// truncation at presenceSnapshotConnectedLimit or by authorization.
	//
	// Storing the complement rather than the coverage set is what keeps the common
	// case free. On an ordinary hub every candidate is published, so this is EMPTY
	// and stays nil. Retaining the coverage set instead allocated up to 512 UUIDs
	// per socket for its whole lifetime — O(N^2) aggregate — which is what the
	// round-3 fix accidentally introduced.
	//
	// Guarded by bootstrapMu. Nil until the snapshot is published, and nil
	// thereafter whenever nothing was omitted. Used only when NOT truncated.
	presenceSnapshotOmitted map[uuid.UUID]struct{}

	// presenceSnapshotCovered is the PUBLISHED set, populated only when the seed was
	// truncated — where the complement cannot be computed. Bounded by
	// presenceSnapshotConnectedLimit, and allocated only on a hub above it.
	// Guarded by bootstrapMu.
	presenceSnapshotCovered map[uuid.UUID]struct{}

	// presenceSnapshotPublished distinguishes "nothing omitted" from "no snapshot
	// yet", which two nil maps cannot. Guarded by bootstrapMu.
	presenceSnapshotPublished bool

	// Username from database (set at connection time)
	Username string

	// DisplayName from database (set at connection time)
	DisplayName *string

	// AvatarURL from database (set at connection time)
	AvatarURL *string

	// SessionID links this connection to a specific refresh token session
	// (for targeted session revocation). Empty if not provided.
	SessionID string

	// CredEpoch is the credential-epoch claim captured at WS auth time (#2201).
	// Every message frame from this socket carries it so a ciphertext write can
	// be fenced against a destructive reset that advanced the epoch after connect.
	CredEpoch string

	// The WebSocket connection
	Conn *websocket.Conn

	// Hub that manages this client
	Hub *Hub

	// Buffered channel of outbound messages
	Send chan []byte
	// activityRichPresenceCapable is declared explicitly on the authenticated
	// WebSocket upgrade. Older desktop builds accept activity-category clear frames
	// but misapply them to Custom Status, so live activity deltas remain disabled
	// until a client opts into the corrected contract.
	activityRichPresenceCapable bool
	// sendMu serializes every production producer. Code that needs both client
	// locks acquires sendMu before bootstrapMu. The reconnect replacement
	// holds it across its full multi-frame flush so unrelated events cannot
	// consume reserved capacity or interleave between snapshot/replay/live.
	sendMu     sync.Mutex
	sendClosed bool

	// Channels the user is subscribed to
	Channels map[uuid.UUID]bool

	// Rate limiting: token bucket for chat messages
	rateTokens   int
	rateLastFill time.Time

	// asyncWg tracks in-flight async goroutines for this client
	asyncWg sync.WaitGroup

	// asyncCancel cancels the context passed to async registration goroutines
	asyncCancel context.CancelFunc

	// bootstrapMu orders replacement construction against privacy-critical and
	// base-presence deltas. Delivery takes Hub.mu before this mutex; bootstrap
	// completion/cancellation never takes Hub.mu while holding it.
	bootstrapMu       sync.Mutex
	bootstrapActive   bool
	bootstrapCanceled bool
	bootstrapFailed   bool
	bootstrapReplay   [][]byte
	bootstrapLive     [][]byte
}

// setPresenceSnapshotCoverage records the senders the client's snapshot ACTUALLY
// carried, taken from the finalized, re-authorized content rather than from the
// seed.
//
// The distinction is the whole point. seed.connectedIDs is a pre-authorization
// CANDIDATE list: capturePresenceSnapshotSeed enumerates connected users and
// stops at presenceSnapshotConnectedLimit, and authorizeBasePresenceCandidates
// then removes the ones this viewer may not see. Deriving coverage from the seed
// therefore marks senders as covered that the snapshot dropped — for truncation
// OR for authorization — and the registration frontier would discard their
// in-flight deltas too, leaving the viewer rendering them offline until an
// unrelated transition.
//
// Called on the bootstrap goroutine immediately before the snapshot is published,
// so it is in place before completeClientBootstrap flushes the buffered live
// frames that follow it.
func (c *Client) setPresenceSnapshotCoverage(omitted, covered map[uuid.UUID]struct{}) {
	c.bootstrapMu.Lock()
	defer c.bootstrapMu.Unlock()
	c.presenceSnapshotOmitted = omitted
	c.presenceSnapshotCovered = covered
	c.presenceSnapshotPublished = true
}

// snapshotCovered reports whether this client's snapshot carried senderID.
//
// A nil set — no snapshot published yet — reports TRUE, i.e. the frontier still
// filters. That preserves the ordering guarantee the frontier exists for during
// the bootstrap window; the alternative (fail open) would reopen the very defect
// being fixed for every delta racing the snapshot. It is safe in the direction
// that matters: over-filtering shows a stale-offline peer, never a frame to
// somebody unauthorized.
//
// Takes bootstrapMu, which introduces no new lock-ordering edge:
// enqueuePresenceForAudience already holds Hub.mu.RLock while
// enqueueBasePresence -> bufferBootstrapLive acquires this same mutex.
func (c *Client) snapshotCovered(senderID uuid.UUID) bool {
	c.bootstrapMu.Lock()
	defer c.bootstrapMu.Unlock()
	if !c.presenceSnapshotPublished {
		return true
	}
	if c.presenceSnapshotCovered != nil {
		// Truncated seed: the candidate list is not the connected set, so coverage
		// can only be proven by membership in what was actually published.
		_, covered := c.presenceSnapshotCovered[senderID]
		return covered
	}
	_, omitted := c.presenceSnapshotOmitted[senderID]
	return !omitted
}

func (c *Client) beginBootstrap() {
	c.bootstrapMu.Lock()
	defer c.bootstrapMu.Unlock()
	c.bootstrapActive = true
	c.bootstrapCanceled = false
	c.bootstrapFailed = false
	c.bootstrapReplay = nil
	c.bootstrapLive = nil
}

func (c *Client) appendBootstrapReplay(data []byte) error {
	c.bootstrapMu.Lock()
	defer c.bootstrapMu.Unlock()
	if c.bootstrapCanceled {
		return errClientBootstrapCanceled
	}
	if !c.bootstrapActive {
		return errClientBootstrapInactive
	}
	if c.bootstrapFailed || len(c.bootstrapReplay)+len(c.bootstrapLive) >= clientBootstrapBufferedFrameLimit {
		c.bootstrapFailed = true
		return errClientBootstrapOverflow
	}
	c.bootstrapReplay = append(c.bootstrapReplay, append([]byte(nil), data...))
	return nil
}

func (c *Client) bufferBootstrapLive(data []byte) bootstrapBufferOutcome {
	c.bootstrapMu.Lock()
	defer c.bootstrapMu.Unlock()
	if c.bootstrapCanceled {
		return bootstrapBufferCanceled
	}
	if !c.bootstrapActive {
		return bootstrapBufferInactive
	}
	if c.bootstrapFailed || len(c.bootstrapReplay)+len(c.bootstrapLive) >= clientBootstrapBufferedFrameLimit {
		c.bootstrapFailed = true
		return bootstrapBufferOverflow
	}
	c.bootstrapLive = append(c.bootstrapLive, append([]byte(nil), data...))
	return bootstrapBufferEnqueued
}

func (c *Client) cancelBootstrap() {
	c.bootstrapMu.Lock()
	defer c.bootstrapMu.Unlock()
	c.bootstrapActive = false
	c.bootstrapCanceled = true
	c.bootstrapReplay = nil
	c.bootstrapLive = nil
}

func (c *Client) enqueueOutbound(data []byte) bool {
	// While the reconnect replacement is active, ordinary producers append to
	// its live tail instead of waiting on sendMu. This keeps Hub.Run responsive
	// while the publication barrier waits on bounded DB/sender-gate work and
	// guarantees the frame cannot overtake snapshot and replay.
	switch c.bufferBootstrapLive(data) {
	case bootstrapBufferEnqueued:
		return true
	case bootstrapBufferOverflow, bootstrapBufferCanceled:
		return false
	case bootstrapBufferInactive:
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.sendClosed || c.Send == nil {
		return false
	}
	select {
	case c.Send <- data:
		return true
	default:
		return false
	}
}

// enqueueOutboundBootstrapSafe delivers a frame whose loss is tolerable and
// which must NEVER be able to fail a client's reconnect replacement.
//
// It deliberately does NOT call bufferBootstrapLive. Appending to that buffer is
// a disconnect primitive for any producer an unauthenticated peer can drive:
// overflow latches bootstrapFailed, and even a non-overflowing append inflates
// completeClientBootstrap's `cap(Send)-len(Send) < frameCount` preflight. Either
// route ends at failClientBootstrapFlushLocked -> disconnectPrivacyCriticalClient,
// so a caller that merely observes a false return still disconnected the client,
// asynchronously, on the client's own bootstrap goroutine.
//
// A client inside the replacement window therefore drops the frame outright. That
// is safe precisely for frames the pending snapshot already subsumes: an erasure
// clear retracts a sender the authorized snapshot will not contain in the first
// place. Do not reuse this for a frame the snapshot does not subsume.
//
// bootstrapCanceled is treated as bootstrapping so a frame is never written into
// a queue whose replacement is unwinding.
func (c *Client) enqueueOutboundBootstrapSafe(data []byte) bool {
	// sendMu before bootstrapMu, matching enqueuePostBootstrap and
	// completeClientBootstrap. Holding sendMu across the check keeps a
	// replacement from starting between the check and the send, which would
	// otherwise consume the capacity the flush preflight is about to require.
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.bootstrapMu.Lock()
	bootstrapping := c.bootstrapActive || c.bootstrapCanceled
	c.bootstrapMu.Unlock()
	if bootstrapping {
		return false
	}
	if c.sendClosed || c.Send == nil {
		return false
	}
	select {
	case c.Send <- data:
		return true
	default:
		return false
	}
}

func (c *Client) enqueueOutboundBlocking(data []byte) bool {
	switch c.bufferBootstrapLive(data) {
	case bootstrapBufferEnqueued:
		return true
	case bootstrapBufferOverflow, bootstrapBufferCanceled:
		return false
	case bootstrapBufferInactive:
	}
	return c.enqueueOutboundBlockingImmediate(data)
}

// enqueueOutboundBlockingImmediate bypasses bootstrap ordering only for the
// initial connected acknowledgement, which is intentionally published before
// the replacement. All other blocking producers use enqueueOutboundBlocking.
func (c *Client) enqueueOutboundBlockingImmediate(data []byte) bool {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.sendClosed || c.Send == nil {
		return false
	}
	c.Send <- data
	return true
}

// enqueuePostBootstrap linearizes informational registration frames against
// unregister cancellation. If cancellation wins bootstrapMu, no late frame is
// allowed into the queue; if this send wins, unregister waits for it normally.
func (c *Client) enqueuePostBootstrap(ctx context.Context, data []byte) bool {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.bootstrapMu.Lock()
	defer c.bootstrapMu.Unlock()
	if c.bootstrapCanceled || ctx.Err() != nil {
		return false
	}
	if c.sendClosed || c.Send == nil {
		return false
	}
	select {
	case c.Send <- data:
		return true
	default:
		return false
	}
}

func (c *Client) closeOutbound() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.sendClosed {
		return
	}
	c.sendClosed = true
	if c.Send != nil {
		close(c.Send)
	}
}

// rateLimitAllow checks the token bucket and returns true if the message is allowed.
func (c *Client) rateLimitAllow() bool {
	now := time.Now()
	if c.rateLastFill.IsZero() {
		c.rateTokens = rateLimitBurst
		c.rateLastFill = now
	}

	// Refill tokens based on elapsed time
	elapsed := now.Sub(c.rateLastFill)
	newTokens := int(elapsed / rateLimitInterval)
	if newTokens > 0 {
		c.rateTokens += newTokens
		if c.rateTokens > rateLimitBurst {
			c.rateTokens = rateLimitBurst
		}
		c.rateLastFill = now
	}

	if c.rateTokens > 0 {
		c.rateTokens--
		return true
	}
	return false
}

// readPump pumps messages from the WebSocket connection to the hub
func (c *Client) readPump() {
	defer func() {
		c.Hub.unregister <- c
		_ = c.Conn.Close()
	}()

	_ = c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetReadLimit(maxMessageSize)
	c.Conn.SetPongHandler(func(string) error {
		return c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		// Parse incoming message
		var msg IncomingMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Invalid message format: %v", err)
			continue
		}

		// Set sender info
		msg.UserID = c.UserID
		msg.ClientID = c.ID
		msg.CredEpoch = c.CredEpoch

		// Route message to hub
		c.Hub.incoming <- msg
	}
}

// writePump pumps messages from the hub to the WebSocket connection
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub closed the channel
				_ = c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			// Send each message as its own WebSocket frame to ensure valid JSON
			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
