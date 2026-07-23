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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
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
//
// The stored value carries the issue-time credential epoch (#2201) — read from
// the request's own already-verified claims, never a second store round-trip —
// so redemption can enforce the same epoch semantics as HTTP bearer auth: a
// ticket minted before a destructive credential reset must not upgrade after it.
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

	// Store userID:sessionID:credEpoch in Redis. sessionID is the authenticated
	// access token's `sid` claim (#2201) — the server-verified refresh-session
	// id, so DisconnectSession can evict this ticket socket on revocation. The
	// client-supplied X-Session-ID header is a spoofable fallback used ONLY for a
	// legacy token minted without a `sid` (a single 30s deploy window); a
	// patched client cannot register its socket under an empty/foreign session to
	// dodge targeted disconnect. credEpoch is the requester's cred_epoch claim
	// ("" until the user's first rotation). UUIDs and hex epochs contain no ':',
	// so the 3-segment SplitN parse in ValidateTicket is unambiguous — and a
	// session ID containing ':' is discarded (session IDs are UUIDs; a colon
	// would let the value shift the epoch segment, #2201).
	sessionID := middleware.TokenSessionID(c)
	if sessionID == "" {
		sessionID = c.GetHeader("X-Session-ID")
	}
	if strings.Contains(sessionID, ":") {
		sessionID = ""
	}
	value := fmt.Sprintf("%s:%s:%s", userID, sessionID, middleware.TokenCredentialEpoch(c))

	ctx := context.Background()
	key := wsTicketPrefix + ticket
	if err := h.redis.Set(ctx, key, value, wsTicketTTL).Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate ticket"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ticket": ticket})
}

// ValidateTicket checks a ticket against Redis. Returns the user ID, optional
// session ID, and the issue-time credential epoch. The ticket is deleted after
// use (single-use). The stored value format is "userID:sessionID:credEpoch";
// legacy pre-#2201 values ("userID" or "userID:sessionID", possible only
// within one 30s deploy window) parse with an empty epoch — harmless for
// never-rotated users, and a post-rotation user's single failed redemption
// self-heals on the client's next ticket fetch.
func ValidateTicket(ctx context.Context, redisClient *redis.Client, ticket string) (userID, sessionID, credEpoch string, err error) {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return "", "", "", fmt.Errorf("empty ticket")
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

	result, serr := script.Run(ctx, redisClient, []string{key}).Result()
	if serr != nil || result == nil {
		return "", "", "", fmt.Errorf("invalid or expired ticket")
	}

	value, ok := result.(string)
	if !ok || value == "" {
		return "", "", "", fmt.Errorf("invalid ticket data")
	}

	// Parse "userID[:sessionID[:credEpoch]]"
	parts := strings.SplitN(value, ":", 3)
	userID = parts[0]
	if len(parts) >= 2 {
		sessionID = parts[1]
	}
	if len(parts) == 3 {
		credEpoch = parts[2]
	}

	return userID, sessionID, credEpoch, nil
}
