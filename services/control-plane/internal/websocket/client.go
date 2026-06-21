// Package websocket provides WebSocket connection handling for real-time messaging.
package websocket

import (
	"context"
	"encoding/json"
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

	// Channels the user is subscribed to
	Channels map[uuid.UUID]bool

	// Rate limiting: token bucket for chat messages
	rateTokens   int
	rateLastFill time.Time

	// asyncWg tracks in-flight async goroutines for this client
	asyncWg sync.WaitGroup

	// asyncCancel cancels the context passed to async registration goroutines
	asyncCancel context.CancelFunc
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
