package websocket

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
)

// Handler handles WebSocket connections
type Handler struct {
	hub            *Hub
	db             *sql.DB
	redis          *redis.Client
	jwtSecret      string
	allowedOrigins []string
	// minimumClientVersion is optional: empty preserves the existing admission path.
	minimumClientVersion string
	// fence enforces the per-user credential epoch on both WS auth paths
	// (#2201) so ticket redemption and the JWT fallback carry the same
	// revocation semantics as HTTP bearer auth. nil disables (tests only).
	fence *credepoch.Fence
	// securityHeaders is stamped onto the 101 handshake via Upgrade's
	// responseHeader argument — gorilla hijacks the connection and does NOT
	// serialize headers already set on the gin writer, so the security
	// middleware alone never reaches successful upgrades (#2318 review).
	securityHeaders http.Header
	upgrader        websocket.Upgrader
}

// NewHandler creates a new WebSocket handler
func NewHandler(hub *Hub, db *sql.DB, redisClient *redis.Client, jwtSecret string, allowedOrigins []string, fence *credepoch.Fence, securityHeaders http.Header) *Handler {
	h := &Handler{
		hub:             hub,
		db:              db,
		redis:           redisClient,
		jwtSecret:       jwtSecret,
		allowedOrigins:  allowedOrigins,
		fence:           fence,
		securityHeaders: securityHeaders,
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

// SetMinimumClientVersion configures the optional client-version admission floor.
func (h *Handler) SetMinimumClientVersion(minimum string) {
	h.minimumClientVersion = minimum
}

// liveWebSocketDeadline is the authenticated credential's expiry, when it is
// set and still in the future.
func liveWebSocketDeadline(c *gin.Context) (time.Time, bool) {
	expiresAt, ok := c.Get(wsDeadlineContextKey)
	deadline, valid := expiresAt.(time.Time)
	return deadline, ok && valid && time.Now().Before(deadline)
}

// lookupWebSocketProfile reads the username, display name, and avatar for a
// connecting user. A failed read keeps the socket and names the user "Unknown".
func (h *Handler) lookupWebSocketProfile(userID uuid.UUID) (username string, displayName, avatarURL *string) {
	if err := h.db.QueryRow("SELECT username, display_name, avatar_url FROM users WHERE id = $1", userID).Scan(&username, &displayName, &avatarURL); err != nil {
		log.Printf("Failed to look up username for user %s: %v", sanitizeLogValue(userID.String()), err)
		return "Unknown", nil, nil
	}
	return username, displayName, avatarURL
}

// HandleWebSocket upgrades HTTP connections to WebSocket
func (h *Handler) HandleWebSocket(c *gin.Context) {
	// Authenticate: prefer ticket-based auth, fall back to JWT for backward compat
	userID, sessionID, err := h.authenticateWebSocket(c)
	if err != nil {
		status, message := http.StatusUnauthorized, "Unauthorized"
		if errors.Is(err, loginfamily.ErrUnavailable) {
			status, message = http.StatusServiceUnavailable, "Authentication temporarily unavailable"
		} else if errors.Is(err, loginfamily.ErrContract) {
			status = http.StatusInternalServerError
		}
		c.JSON(status, gin.H{"error": message})
		return
	}
	if !middleware.EnforceClientVersion(c, h.minimumClientVersion, c.Query("client_version")) {
		return
	}
	release, admitted := h.hub.reserveConnection(userID)
	if !admitted {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "WebSocket connection limit reached"})
		return
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	activityRichPresenceCapable := requestSupportsActivityRichPresence(c.Request)
	deadline, ok := liveWebSocketDeadline(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	username, displayName, avatarURL := h.lookupWebSocketProfile(userID)
	if !time.Now().Before(deadline) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	family, validFamily := c.Get(wsFamilyContextKey)
	familyID, validID := family.(uuid.UUID)
	if !validFamily || !validID || familyID == uuid.Nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	// Upgrade HTTP connection to WebSocket
	// Origin is validated by h.upgrader.CheckOrigin (configured in NewHandler)
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, h.securityHeaders) // nosemgrep: go.gorilla.security.audit.websocket-missing-origin-check.websocket-missing-origin-check
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to upgrade connection"})
		return
	}

	// Create new client
	client := &Client{
		ID:                          uuid.New(),
		UserID:                      userID,
		SessionID:                   sessionID,
		FamilyID:                    familyID,
		CredEpoch:                   c.GetString(wsCredEpochContextKey),
		TokenExpiresAt:              deadline,
		Username:                    username,
		DisplayName:                 displayName,
		AvatarURL:                   avatarURL,
		Conn:                        conn,
		Hub:                         h.hub,
		Send:                        make(chan []byte, 256),
		Channels:                    make(map[uuid.UUID]bool),
		activityRichPresenceCapable: activityRichPresenceCapable,
		privacyClearThenCloseWake:   make(chan struct{}, 1),
		releaseAdmission:            release,
	}
	client.admissionOwners.Store(2) // Handler and hub; pumps join before starting.
	release = nil
	defer client.releaseConnection()
	closePending := func() {
		client.stopTokenExpiry()
		if err := conn.Close(); err != nil && !alreadyDisconnected(err) {
			log.Printf("Failed to close pending WebSocket: %s", describeSocketFailure(err))
		}
		client.releaseConnection() // No hub owns this client.
	}
	registered := make(chan struct{})
	client.renewAuthority = h
	client.deadlineMu.Lock()
	client.tokenExpiryTimer = time.AfterFunc(time.Until(deadline), func() {
		client.onTokenExpiryTimer(registered)
	})
	client.deadlineMu.Unlock()

	// Register client with hub. This channel is UNBUFFERED (hub.go:322) and that is
	// load-bearing for the re-check below — do not add a buffer.
	registrationTimer := time.NewTimer(pendingRegistrationTimeout)
	defer registrationTimer.Stop()
	select {
	case h.hub.register <- client:
		close(registered)
	case <-h.hub.done:
		closePending()
		return
	case <-c.Request.Context().Done():
		closePending()
		return
	case <-registrationTimer.C:
		closePending()
		return
	}
	if client.tokenExpired() {
		client.closeExpiredToken()
		client.requestUnregister()
		return
	}

	// #2414 §C: close the register-vs-disconnect-sweep ordering race. registerClient
	// and the DisconnectUser handler are both serviced by the Hub's single Run
	// goroutine, so a socket whose register is delivered AFTER a reset's disconnectUser
	// is never swept, and survives until its access token expires (~15m).
	//
	// The unbuffered register channel above blocks until Run receives it, so by here
	// Run has accepted this client and will finish registration before another
	// disconnect sweep. That gives a total order in both directions:
	//   - the reset committed before this check  => we read the new epoch and close;
	//   - the reset commits after this check     => its disconnectUser send follows our
	//                                               already-consumed register, so the
	//                                               sweep sees this client and closes it.
	// This check MUST NOT move inside registerClient: that runs on the Run goroutine,
	// where a Redis/DB round-trip would stall the entire hub.
	//
	// h.fence is nil only in tests that construct a Handler without one (#2201);
	// skip the check rather than dereference a nil fence.
	if h.fence != nil {
		if err := h.fence.Check(c.Request.Context(), userID.String(), client.CredEpoch); err != nil {
			client.stopTokenExpiry()
			log.Printf("Evicting WebSocket client for user %s: credential epoch superseded", sanitizeLogValue(userID.String()))
			client.requestUnregister()
			client.closeRevokedConnection()
			return
		}
	}
	// The theft sweep can complete and clear its temporary deny marker while an
	// upgrade is waiting for hub registration. Compare the durable generation
	// captured before the first theft check, then repeat the pending-state gate.
	// A sweep after this check is ordered after registration and disconnects us.
	generation, err := h.readTokenTheftGeneration(c.Request.Context(), userID)
	if err == nil {
		initial, ok := c.Get(wsTheftGenerationContextKey)
		snapshot, valid := initial.(int64)
		if !ok || !valid || generation != snapshot {
			err = fmt.Errorf("token theft generation changed during WebSocket registration")
		} else {
			err = middleware.CheckTokenTheftBlocked(c.Request.Context(), h.redis, h.db, userID.String())
		}
	}
	if err != nil {
		client.stopTokenExpiry()
		log.Printf("Evicting WebSocket client for user %s: token theft state changed: %v", sanitizeLogValue(userID.String()), err)
		client.requestUnregister()
		client.closeRevokedConnection()
		return
	}
	if client.tokenExpired() {
		client.closeExpiredToken()
		client.requestUnregister()
		return
	}

	if err := h.recheckClientFamily(c.Request.Context(), client); err != nil || client.securityTerminal.Load() {
		client.securityTerminal.Store(true)
		if closeErr := client.closeSecurityTransport(); closeErr != nil {
			log.Printf("WebSocket family transport close failed: %s", describeSocketFailure(closeErr))
		}
		client.requestUnregister()
		return
	}
	// Start client's read and write pumps
	client.admissionOwners.Add(2)
	go client.writePump()
	go client.readPump()
}

func requestSupportsActivityRichPresence(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	values := request.URL.Query()["activity_rich_presence"]
	return len(values) == 1 && values[0] == "1"
}

// authenticateWebSocket validates the connection using ticket-based auth (preferred)
// or falls back to JWT for backward compatibility. Returns (userID, sessionID, error).
// Both paths resolve the SID and immutable family from verified SQL authority.
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
	userIDStr, sessionID, ticketEpoch, expiresAt, err := auth.ValidateTicket(ctx, h.redis, ticket)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("invalid ticket: %w", err)
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("invalid user ID in ticket: %w", err)
	}
	generation, err := h.readTokenTheftGeneration(c.Request.Context(), userID)
	if err != nil {
		return uuid.Nil, "", err
	}
	// A ticket is a 30-second bearer: apply the same user-disabled and
	// credential-epoch gates as HTTP bearer auth (#2201). Pre-#2201 the ticket
	// path checked neither, so a ticket minted just before a disable or a
	// destructive credential reset could still open a socket after it.
	// userID.String(), not the raw userIDStr (#3362): both gates CONCATENATE this
	// into a Redis key, and a non-canonical spelling misses the key while every
	// SQL predicate still resolves to the same row. The JWT rail below already
	// keys on userID.String(); this makes the ticket rail state that invariant
	// itself rather than inherit it from whatever IssueTicket happened to store.
	if err := h.checkUserDisabled(c, userID.String()); err != nil {
		return uuid.Nil, "", err
	}
	if h.fence != nil {
		if err := h.fence.Check(c.Request.Context(), userID.String(), ticketEpoch); err != nil {
			return uuid.Nil, "", fmt.Errorf("credential epoch check failed: %w", err)
		}
	}
	grant, err := h.resolveAccessFamily(c.Request.Context(), userID, sessionID, ticketEpoch, expiresAt)
	if err != nil {
		return uuid.Nil, "", err
	}
	c.Set(wsFamilyContextKey, grant.FamilyID)
	// Retain the epoch for the socket so message-frame ciphertext writes can be
	// fenced against a later reset (#2201, Codex #2397 review).
	c.Set(wsCredEpochContextKey, ticketEpoch)
	c.Set(wsDeadlineContextKey, expiresAt)
	c.Set(wsTheftGenerationContextKey, generation)
	return userID, grant.SID.String(), nil
}

// wsCredEpochContextKey carries the authenticated socket's credential epoch
// from the auth path to Client construction in ServeWS. G101 is a false
// positive: this is a gin.Context key NAME (an opaque tag lookup key), not a
// credential or secret value.
//
//nolint:gosec // G101: context-key name, not a credential
const wsCredEpochContextKey = "ws_cred_epoch"

const wsDeadlineContextKey = "ws_deadline"
const wsFamilyContextKey = "ws_family"

const wsTheftGenerationContextKey = "ws_theft_generation"

func (h *Handler) readTokenTheftGeneration(ctx context.Context, userID uuid.UUID) (int64, error) {
	if h.db == nil {
		return 0, errors.New("read token theft generation: database unavailable")
	}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var generation int64
	if err := h.db.QueryRowContext(readCtx,
		`SELECT COALESCE((SELECT token_theft_generation FROM users WHERE id = $1), 0)`, userID,
	).Scan(&generation); err != nil {
		return 0, fmt.Errorf("read token theft generation: %w", err)
	}
	return generation, nil
}

// checkUserDisabled mirrors the HTTP middleware's immediate-effect denylist
// gate (#1623) for WS upgrades: a present user_disabled key OR a Redis error
// rejects (fail closed).
func (h *Handler) checkUserDisabled(c *gin.Context, userID string) error {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	exists, err := h.redis.Exists(ctx, middleware.UserDisabledKey(userID)).Result()
	if err != nil {
		// Preserve the cause for diagnostics — this is a fail-closed auth outage.
		return fmt.Errorf("check disabled user: %w", err)
	}
	if exists > 0 {
		return fmt.Errorf("account disabled")
	}
	return middleware.CheckTokenTheftBlocked(c.Request.Context(), h.redis, h.db, userID)
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
	verified, err := h.validateAccessToken(c.Request.Context(), tokenString)
	if err != nil {
		return uuid.Nil, "", err
	}
	c.Set(wsFamilyContextKey, verified.FamilyID)
	c.Set(wsCredEpochContextKey, verified.CredEpoch)
	c.Set(wsDeadlineContextKey, verified.ExpiresAt)
	c.Set(wsTheftGenerationContextKey, verified.TheftGeneration)
	return verified.UserID, verified.SessionID, nil
}

// accessTokenAuth is what a fully validated access token proves about its bearer.
type accessTokenAuth struct {
	FamilyID        uuid.UUID
	UserID          uuid.UUID
	SessionID       string
	CredEpoch       string
	ExpiresAt       time.Time
	TheftGeneration int64
}

// validateAccessToken is the single access-token gate shared by the JWT handshake
// fallback and in-band control-socket renewal, so the two cannot drift. It holds
// no gin state: renewal runs on a socket's read goroutine, not inside a request.
func (h *Handler) validateAccessToken(ctx context.Context, tokenString string) (accessTokenAuth, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(h.jwtSecret), nil
	})
	if err != nil {
		return accessTokenAuth{}, err
	}

	// IsAccessToken rejects an MFA-challenge or account-recovery JWT presented here.
	// Those are signed with the same secret and carry `user_id`, so before this the
	// WS handshake was a second front door for the same bypass AuthRequired had —
	// and the `email_verified` gate below does not stop them, since a challenge token
	// omits the claim and emailVerifiedFromClaims defaults an absent claim to true.
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid || !middleware.IsAccessToken(claims) {
		return accessTokenAuth{}, fmt.Errorf("invalid authentication")
	}
	expiresAt, err := claims.GetExpirationTime()
	if err != nil || expiresAt == nil || !time.Now().Before(expiresAt.Time) {
		return accessTokenAuth{}, fmt.Errorf("invalid token deadline")
	}

	if err := h.checkTokenBlacklist(ctx, claims); err != nil {
		return accessTokenAuth{}, err
	}

	userID, sessionID, err := parseUserIDFromClaims(claims)
	if err != nil {
		return accessTokenAuth{}, err
	}
	generation, err := h.readTokenTheftGeneration(ctx, userID)
	if err != nil {
		return accessTokenAuth{}, err
	}
	// Share HTTP bearer auth's live disabled/email state while retaining the
	// WebSocket policy that an explicitly unverified token is rejected here.
	emailVerified, err := middleware.VerifyLiveTokenState(ctx, h.redis, h.db, userID.String(), claims)
	if err != nil {
		return accessTokenAuth{}, err
	}
	if !emailVerified {
		return accessTokenAuth{}, fmt.Errorf("email verification required")
	}
	tokenEpoch, _ := claims["cred_epoch"].(string)
	if h.fence != nil {
		if ferr := h.fence.Check(ctx, userID.String(), tokenEpoch); ferr != nil {
			return accessTokenAuth{}, fmt.Errorf("credential epoch check failed: %w", ferr)
		}
	}
	grant, err := h.resolveAccessFamily(ctx, userID, sessionID, tokenEpoch, expiresAt.Time)
	if err != nil {
		return accessTokenAuth{}, err
	}
	return accessTokenAuth{UserID: userID, FamilyID: grant.FamilyID, SessionID: grant.SID.String(), CredEpoch: tokenEpoch, ExpiresAt: expiresAt.Time, TheftGeneration: generation}, nil
}

func (h *Handler) checkTokenBlacklist(ctx context.Context, claims jwt.MapClaims) error {
	jti, ok := claims["jti"].(string)
	if !ok || jti == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
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
	// #2201 review: propagate the access token's session id so a socket opened
	// via the JWT fallback carries its SessionID and DisconnectSession can evict
	// it when that refresh lineage is revoked. Legacy tokens without sid → "".
	sessionID, _ := claims["sid"].(string)
	return userID, sessionID, nil
}

func (h *Handler) resolveAccessFamily(ctx context.Context, userID uuid.UUID, sessionID, epoch string, expiry time.Time) (loginfamily.AccessGrant, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil || sid == uuid.Nil {
		return loginfamily.AccessGrant{}, loginfamily.NewError(loginfamily.ReasonAccessIdentity, nil)
	}
	return loginfamily.ResolveVerifiedAccess(ctx, h.db, loginfamily.AccessIdentity{UserID: userID, SID: sid, CredentialEpoch: epoch, AccessExpiresAt: expiry})
}

func (h *Handler) recheckClientFamily(ctx context.Context, client *Client) error {
	grant, err := h.resolveAccessFamily(ctx, client.UserID, client.SessionID, client.CredEpoch, client.tokenDeadline())
	if err != nil {
		return err
	}
	if grant.FamilyID != client.FamilyID {
		return loginfamily.NewError(loginfamily.ReasonAccessIdentity, nil)
	}
	return nil
}
