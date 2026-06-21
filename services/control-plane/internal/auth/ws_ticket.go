package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const (
	wsTicketTTL    = 30 * time.Second
	wsTicketPrefix = "ws_ticket:"
	wsTicketBytes  = 32
)

// WSTicketHandler issues and validates short-lived, single-use WebSocket tickets.
type WSTicketHandler struct {
	redis     *redis.Client
	jwtSecret string
}

// NewWSTicketHandler creates a new handler for WebSocket ticket operations.
func NewWSTicketHandler(redisClient *redis.Client, jwtSecret string) *WSTicketHandler {
	return &WSTicketHandler{redis: redisClient, jwtSecret: jwtSecret}
}

// IssueTicket creates a short-lived single-use ticket for WebSocket auth.
// The caller must already be authenticated (AuthRequired middleware).
// Optionally accepts X-Session-ID header to associate the WebSocket connection
// with a specific refresh token session (for targeted session revocation).
func (h *WSTicketHandler) IssueTicket(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	// Generate a random ticket
	b := make([]byte, wsTicketBytes)
	if _, err := rand.Read(b); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate ticket"})
		return
	}
	ticket := hex.EncodeToString(b)

	// Store userID:sessionID in Redis (sessionID is optional, for targeted disconnect)
	sessionID := c.GetHeader("X-Session-ID")
	value := fmt.Sprintf("%s", userID)
	if sessionID != "" {
		value = fmt.Sprintf("%s:%s", userID, sessionID)
	}

	ctx := context.Background()
	key := wsTicketPrefix + ticket
	if err := h.redis.Set(ctx, key, value, wsTicketTTL).Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate ticket"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ticket": ticket})
}

// ValidateTicket checks a ticket against Redis. Returns the user ID and optional
// session ID if valid. The ticket is deleted after use (single-use).
// The stored value format is "userID" or "userID:sessionID".
func ValidateTicket(ctx context.Context, redisClient *redis.Client, ticket string) (string, string, error) {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return "", "", fmt.Errorf("empty ticket")
	}

	key := wsTicketPrefix + ticket

	// Get and delete atomically via a Lua script to prevent race conditions
	script := redis.NewScript(`
		local val = redis.call('GET', KEYS[1])
		if val then
			redis.call('DEL', KEYS[1])
		end
		return val
	`)

	result, err := script.Run(ctx, redisClient, []string{key}).Result()
	if err != nil || result == nil {
		return "", "", fmt.Errorf("invalid or expired ticket")
	}

	value, ok := result.(string)
	if !ok || value == "" {
		return "", "", fmt.Errorf("invalid ticket data")
	}

	// Parse "userID" or "userID:sessionID"
	parts := strings.SplitN(value, ":", 2)
	userID := parts[0]
	sessionID := ""
	if len(parts) == 2 {
		sessionID = parts[1]
	}

	return userID, sessionID, nil
}
