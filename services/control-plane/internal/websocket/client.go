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

	// Username from database (set at connection time)
	Username string

	// DisplayName from database (set at connection time)
	DisplayName *string

	// AvatarURL from database (set at connection time)
	AvatarURL *string

	// SessionID links this connection to a specific refresh token session
	// (for targeted session revocation). Empty if not provided.
	SessionID string

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
