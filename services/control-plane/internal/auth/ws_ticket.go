package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

const (
	wsTicketTTL          = 30 * time.Second
	wsTicketPrefix       = "ws_ticket:"
	wsTicketBytes        = 32
	wsTicketAuthRequired = "Authentication required"
)

// WSTicketHandler issues and validates short-lived, single-use WebSocket tickets.
type WSTicketHandler struct {
	redis     *redis.Client
	jwtSecret string
	now       func() time.Time
}

// NewWSTicketHandler creates a new handler for WebSocket ticket operations.
func NewWSTicketHandler(redisClient *redis.Client, jwtSecret string) *WSTicketHandler {
	return &WSTicketHandler{redis: redisClient, jwtSecret: jwtSecret, now: time.Now}
}

// IssueTicket creates a short-lived single-use ticket for WebSocket auth.
// The caller must already be authenticated (AuthRequired middleware).
// The SID comes only from the verified middleware identity.
//
// The stored value carries the issue-time credential epoch (#2201) — read from
// the request's copied verified identity after a fresh family-authority check —
// so redemption can enforce the same epoch semantics as HTTP bearer auth: a
// ticket minted before a destructive credential reset must not upgrade after it.
func (h *WSTicketHandler) IssueTicket(c *gin.Context) {
	identity, _, err := middleware.RecheckVerifiedAccess(c)
	if err != nil {
		status, message := http.StatusUnauthorized, wsTicketAuthRequired
		if errors.Is(err, loginfamily.ErrUnavailable) {
			status, message = http.StatusServiceUnavailable, "Authentication temporarily unavailable"
		} else if !errors.Is(err, loginfamily.ErrDenied) {
			status = http.StatusInternalServerError
		}
		c.JSON(status, gin.H{"error": message})
		return
	}
	expiresAt := identity.AccessExpiresAt

	// Generate a random ticket
	b := make([]byte, wsTicketBytes)
	if _, err := rand.Read(b); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate ticket"})
		return
	}
	ticket := hex.EncodeToString(b)

	// Preserve the four-part single-use format; family authority is never stored in Redis.
	value := fmt.Sprintf("%s:%s:%s:%d", identity.UserID, identity.SID, identity.CredentialEpoch, expiresAt.Unix())
	ctx := c.Request.Context()

	key := wsTicketPrefix + ticket
	ttl := min(wsTicketTTL, expiresAt.Sub(h.now()))
	if ttl <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": wsTicketAuthRequired})
		return
	}
	if err := h.redis.Set(ctx, key, value, ttl).Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate ticket"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ticket": ticket})
}

// ValidateTicket checks a ticket against Redis. Returns the user ID, optional
// session ID, credential epoch, and signed access-token deadline. Consumption
// is atomic and single-use. Values without a deadline fail closed; clients can
// fetch a fresh ticket during the 30-second rollout window.
func ValidateTicket(ctx context.Context, redisClient *redis.Client, ticket string) (userID, sessionID, credEpoch string, expiresAt time.Time, err error) {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return "", "", "", time.Time{}, fmt.Errorf("empty ticket")
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
		return "", "", "", time.Time{}, fmt.Errorf("invalid or expired ticket")
	}

	value, ok := result.(string)
	if !ok || value == "" {
		return "", "", "", time.Time{}, fmt.Errorf("invalid ticket data")
	}

	parts := strings.Split(value, ":")
	if len(parts) != 4 {
		return "", "", "", time.Time{}, fmt.Errorf("invalid ticket deadline")
	}
	expires, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || !time.Now().Before(time.Unix(expires, 0)) {
		return "", "", "", time.Time{}, fmt.Errorf("invalid or expired ticket deadline")
	}
	return parts[0], parts[1], parts[2], time.Unix(expires, 0), nil
}
