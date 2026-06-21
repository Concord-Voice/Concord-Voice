package websocket

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/redis/go-redis/v9"
)

// Handler handles WebSocket connections
type Handler struct {
	hub            *Hub
	db             *sql.DB
	redis          *redis.Client
	jwtSecret      string
	allowedOrigins []string
	upgrader       websocket.Upgrader
}

// NewHandler creates a new WebSocket handler
func NewHandler(hub *Hub, db *sql.DB, redisClient *redis.Client, jwtSecret string, allowedOrigins []string) *Handler {
	h := &Handler{
		hub:            hub,
		db:             db,
		redis:          redisClient,
		jwtSecret:      jwtSecret,
		allowedOrigins: allowedOrigins,
	}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true // Native apps, CLI tools, and Electron omit Origin entirely
			}
			// Allow file:// origin — the packaged Electron desktop client
			// loads bundled HTML from disk and sends file:// as origin.
			// WebSocket auth is already validated via ws-ticket.
			if strings.HasPrefix(origin, "file:") {
				return true
			}
			// Reject "null" pseudo-origin (sandboxed iframes, data: URIs)
			// before the allowlist loop so a wildcard ("*") doesn't accept it.
			if origin == "null" {
				log.Printf("[websocket] Origin rejected (null pseudo-origin)") //nolint:gosec
				return false
			}
			for _, allowed := range h.allowedOrigins {
				if allowed == "*" || origin == allowed {
					return true
				}
			}
			log.Printf("[websocket] Origin rejected: %q (allowed: %v)", origin, h.allowedOrigins) //nolint:gosec // origin is from HTTP header, safe to log
			return false
		},
	}
	return h
}

// HandleWebSocket upgrades HTTP connections to WebSocket
func (h *Handler) HandleWebSocket(c *gin.Context) {
	// Authenticate: prefer ticket-based auth, fall back to JWT for backward compat
	userID, sessionID, err := h.authenticateWebSocket(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// Look up username, display name, and avatar for this user
	var username string
	var displayName *string
	var avatarURL *string
	if err := h.db.QueryRow("SELECT username, display_name, avatar_url FROM users WHERE id = $1", userID).Scan(&username, &displayName, &avatarURL); err != nil {
		log.Printf("Failed to look up username for user %s: %v", userID, err)
		username = "Unknown"
	}

	// Upgrade HTTP connection to WebSocket
	// Origin is validated by h.upgrader.CheckOrigin (configured in NewHandler)
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil) // nosemgrep: go.gorilla.security.audit.websocket-missing-origin-check.websocket-missing-origin-check
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to upgrade connection"})
		return
	}

	// Create new client
	client := &Client{
		ID:          uuid.New(),
		UserID:      userID,
		SessionID:   sessionID,
		Username:    username,
		DisplayName: displayName,
		AvatarURL:   avatarURL,
		Conn:        conn,
		Hub:         h.hub,
		Send:        make(chan []byte, 256),
		Channels:    make(map[uuid.UUID]bool),
	}

	// Register client with hub
	h.hub.register <- client

	// Start client's read and write pumps
	go client.writePump()
	go client.readPump()
}

// authenticateWebSocket validates the connection using ticket-based auth (preferred)
// or falls back to JWT for backward compatibility. Returns (userID, sessionID, error).
// sessionID is only populated for ticket-based auth when the client provided X-Session-ID.
func (h *Handler) authenticateWebSocket(c *gin.Context) (uuid.UUID, string, error) {
	if ticket := c.Query("ticket"); ticket != "" {
		return h.authenticateViaTicket(c, ticket)
	}

	tokenString := extractBearerToken(c)
	if tokenString == "" {
		return uuid.Nil, "", fmt.Errorf("no authentication provided")
	}

	return h.authenticateViaJWT(c, tokenString)
}

func (h *Handler) authenticateViaTicket(c *gin.Context, ticket string) (uuid.UUID, string, error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	userIDStr, sessionID, err := auth.ValidateTicket(ctx, h.redis, ticket)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("invalid ticket: %w", err)
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("invalid user ID in ticket: %w", err)
	}
	return userID, sessionID, nil
}

func extractBearerToken(c *gin.Context) string {
	if authHeader := c.GetHeader("Authorization"); authHeader != "" {
		if len(authHeader) > 7 && strings.ToLower(authHeader[0:7]) == "bearer " {
			return authHeader[7:]
		}
	}
	return c.Query("token")
}

func (h *Handler) authenticateViaJWT(c *gin.Context, tokenString string) (uuid.UUID, string, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(h.jwtSecret), nil
	})
	if err != nil {
		return uuid.Nil, "", err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return uuid.Nil, "", fmt.Errorf("invalid authentication")
	}

	if err := h.checkTokenBlacklist(c, claims); err != nil {
		return uuid.Nil, "", err
	}

	return parseUserIDFromClaims(claims)
}

func (h *Handler) checkTokenBlacklist(c *gin.Context, claims jwt.MapClaims) error {
	jti, ok := claims["jti"].(string)
	if !ok || jti == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	exists, err := h.redis.Exists(ctx, fmt.Sprintf("blacklist:%s", jti)).Result()
	if err != nil {
		return fmt.Errorf("revocation check unavailable")
	}
	if exists > 0 {
		return fmt.Errorf("token has been revoked")
	}
	return nil
}

func parseUserIDFromClaims(claims jwt.MapClaims) (uuid.UUID, string, error) {
	userIDStr, ok := claims["user_id"].(string)
	if !ok {
		return uuid.Nil, "", jwt.ErrTokenInvalidClaims
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		return uuid.Nil, "", err
	}
	return userID, "", nil
}
