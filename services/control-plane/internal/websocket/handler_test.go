package websocket

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	originMain = "https://concordvoice.chat"
	originApp  = "https://app.concordvoice.chat"
	originFile = "file://"
)

// --- NewHandler tests ---

func TestNewHandlerSetsFields(t *testing.T) {
	hub := NewHub(nil, nil)
	origins := []string{originMain, originApp}

	h := NewHandler(hub, nil, nil, "test-secret", origins)

	require.NotNil(t, h)
	assert.Equal(t, hub, h.hub)
	assert.Equal(t, "test-secret", h.jwtSecret)
	assert.Equal(t, origins, h.allowedOrigins)
	assert.NotNil(t, h.upgrader)
}

func TestNewHandlerNilDepsDoNotPanic(t *testing.T) {
	// NewHandler with nil deps is safe for CheckOrigin testing
	h := NewHandler(nil, nil, nil, "", nil)
	require.NotNil(t, h)
}

// --- CheckOrigin comprehensive tests ---

func TestCheckOriginEmptyAllowList(t *testing.T) {
	h := NewHandler(nil, nil, nil, "", []string{})

	tests := []struct {
		name   string
		origin string
		set    bool
		want   bool
	}{
		{"empty origin allowed (native client)", "", false, true},
		{"null origin rejected", "null", true, false},
		{"file:// allowed (Electron)", originFile, true, true},
		{"any origin rejected with empty list", "https://example.com", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/ws", nil)
			if tt.set {
				r.Header.Set("Origin", tt.origin)
			}
			assert.Equal(t, tt.want, h.upgrader.CheckOrigin(r))
		})
	}
}

func TestCheckOriginMultipleAllowedOrigins(t *testing.T) {
	origins := []string{
		originMain,
		originApp,
		"http://localhost:3001",
	}
	h := NewHandler(nil, nil, nil, "", origins)

	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{"first allowed", originMain, true},
		{"second allowed", originApp, true},
		{"third allowed (localhost dev)", "http://localhost:3001", true},
		{"not in list", "https://evil.com", false},
		{"partial match not allowed", originMain + ".evil.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/ws", nil)
			r.Header.Set("Origin", tt.origin)
			assert.Equal(t, tt.want, h.upgrader.CheckOrigin(r))
		})
	}
}

func TestCheckOriginFileProtocolVariants(t *testing.T) {
	h := NewHandler(nil, nil, nil, "", []string{originMain})

	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{originFile, originFile, true},
		{"file:///", "file:///", true},
		{"file:///C:/Users/app", "file:///C:/Users/app/index.html", true},
		{"file:///home/user", "file:///home/user/app/index.html", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/ws", nil)
			r.Header.Set("Origin", tt.origin)
			assert.Equal(t, tt.want, h.upgrader.CheckOrigin(r))
		})
	}
}

func TestCheckOriginNullOriginAlwaysRejected(t *testing.T) {
	// Even with wildcard, null origin should be rejected
	tests := []struct {
		name    string
		origins []string
	}{
		{"empty list", []string{}},
		{"specific origin", []string{originMain}},
		{"wildcard", []string{"*"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler(nil, nil, nil, "", tt.origins)
			r, _ := http.NewRequest("GET", "/ws", nil)
			r.Header.Set("Origin", "null")
			assert.False(t, h.upgrader.CheckOrigin(r), "null origin should always be rejected")
		})
	}
}

// --- extractBearerToken tests ---

func newGinContext(r *http.Request) *gin.Context {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = r
	return c
}

func TestExtractBearerTokenFromAuthorizationHeader(t *testing.T) {
	r, _ := http.NewRequest("GET", "/ws", nil)
	r.Header.Set("Authorization", "Bearer my-jwt-token")
	c := newGinContext(r)

	assert.Equal(t, "my-jwt-token", extractBearerToken(c))
}

func TestExtractBearerTokenFromQueryParam(t *testing.T) {
	r, _ := http.NewRequest("GET", "/ws?token=query-jwt-token", nil)
	c := newGinContext(r)

	assert.Equal(t, "query-jwt-token", extractBearerToken(c))
}

func TestExtractBearerTokenHeaderTakesPrecedence(t *testing.T) {
	r, _ := http.NewRequest("GET", "/ws?token=query-token", nil)
	r.Header.Set("Authorization", "Bearer header-token")
	c := newGinContext(r)

	assert.Equal(t, "header-token", extractBearerToken(c))
}

func TestExtractBearerTokenEmptyWhenNoneProvided(t *testing.T) {
	r, _ := http.NewRequest("GET", "/ws", nil)
	c := newGinContext(r)

	assert.Equal(t, "", extractBearerToken(c))
}

func TestExtractBearerTokenMalformedAuthHeader(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"no bearer prefix", "Token abc123", ""},
		{"bearer too short", "Bear", ""},
		{"empty bearer", "Bearer ", ""},
		{"lowercase bearer", "bearer my-token", "my-token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/ws", nil)
			r.Header.Set("Authorization", tt.header)
			c := newGinContext(r)
			assert.Equal(t, tt.want, extractBearerToken(c))
		})
	}
}

// --- parseUserIDFromClaims tests ---

func TestParseUserIDFromClaimsValidUUID(t *testing.T) {
	id := uuid.New()
	claims := jwt.MapClaims{"user_id": id.String()}

	userID, sessionID, err := parseUserIDFromClaims(claims)
	require.NoError(t, err)
	assert.Equal(t, id, userID)
	assert.Equal(t, "", sessionID)
}

func TestParseUserIDFromClaimsMissingUserID(t *testing.T) {
	claims := jwt.MapClaims{"sub": "something"}

	_, _, err := parseUserIDFromClaims(claims)
	assert.ErrorIs(t, err, jwt.ErrTokenInvalidClaims)
}

func TestParseUserIDFromClaimsInvalidUUID(t *testing.T) {
	claims := jwt.MapClaims{"user_id": notAValidUUID}

	_, _, err := parseUserIDFromClaims(claims)
	assert.Error(t, err)
}

func TestParseUserIDFromClaimsNonStringUserID(t *testing.T) {
	claims := jwt.MapClaims{"user_id": 12345}

	_, _, err := parseUserIDFromClaims(claims)
	assert.ErrorIs(t, err, jwt.ErrTokenInvalidClaims)
}

// --- authenticateWebSocket tests ---

const (
	testJWTSecret  = "test-jwt-secret-for-handler" //nolint:gosec
	wsTokenPath    = "/ws?token="
	wsTicketPath   = "/ws?ticket="
	wsTicketKeyPfx = "ws_ticket:"
	notAValidUUID  = "not-a-uuid"
)

func generateTestJWT(t *testing.T, userID string, secret string) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"user_id": userID,
		"exp":     jwt.NewNumericDate(now.Add(15 * time.Minute)),
		"iat":     jwt.NewNumericDate(now),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	require.NoError(t, err)
	return signed
}

func TestAuthenticateWebSocketNoAuth(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil)
	r, _ := http.NewRequest("GET", "/ws", nil)
	c := newGinContext(r)

	_, _, err := h.authenticateWebSocket(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no authentication provided")
}

func TestAuthenticateWebSocketJWTViaHeader(t *testing.T) {
	userID := uuid.New()
	h := NewHandler(nil, nil, nil, testJWTSecret, nil)
	token := generateTestJWT(t, userID.String(), testJWTSecret)

	r, _ := http.NewRequest("GET", "/ws", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	c := newGinContext(r)

	gotID, sessionID, err := h.authenticateWebSocket(c)
	require.NoError(t, err)
	assert.Equal(t, userID, gotID)
	assert.Equal(t, "", sessionID)
}

func TestAuthenticateWebSocketJWTViaQueryParam(t *testing.T) {
	userID := uuid.New()
	h := NewHandler(nil, nil, nil, testJWTSecret, nil)
	token := generateTestJWT(t, userID.String(), testJWTSecret)

	r, _ := http.NewRequest("GET", wsTokenPath+token, nil)
	c := newGinContext(r)

	gotID, _, err := h.authenticateWebSocket(c)
	require.NoError(t, err)
	assert.Equal(t, userID, gotID)
}

func TestAuthenticateWebSocketInvalidJWT(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil)

	r, _ := http.NewRequest("GET", wsTokenPath+"not-a-valid-jwt", nil)
	c := newGinContext(r)

	_, _, err := h.authenticateWebSocket(c)
	require.Error(t, err)
}

func TestAuthenticateWebSocketWrongSecret(t *testing.T) {
	userID := uuid.New()
	h := NewHandler(nil, nil, nil, testJWTSecret, nil)
	token := generateTestJWT(t, userID.String(), "wrong-secret")

	r, _ := http.NewRequest("GET", wsTokenPath+token, nil)
	c := newGinContext(r)

	_, _, err := h.authenticateWebSocket(c)
	require.Error(t, err)
}

func TestAuthenticateWebSocketExpiredJWT(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil)
	now := time.Now()
	claims := jwt.MapClaims{
		"user_id": uuid.New().String(),
		"exp":     jwt.NewNumericDate(now.Add(-1 * time.Hour)),
		"iat":     jwt.NewNumericDate(now.Add(-2 * time.Hour)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	// nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key — test fixture, not a real credential
	signed, err := token.SignedString([]byte(testJWTSecret))
	require.NoError(t, err)

	r, _ := http.NewRequest("GET", wsTokenPath+signed, nil)
	c := newGinContext(r)

	_, _, err = h.authenticateWebSocket(c)
	require.Error(t, err)
}

func TestAuthenticateWebSocketJWTMissingUserIDClaim(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil)
	now := time.Now()
	claims := jwt.MapClaims{
		"sub": "something",
		"exp": jwt.NewNumericDate(now.Add(15 * time.Minute)),
		"iat": jwt.NewNumericDate(now),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	// nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key — test fixture, not a real credential
	signed, err := token.SignedString([]byte(testJWTSecret))
	require.NoError(t, err)

	r, _ := http.NewRequest("GET", wsTokenPath+signed, nil)
	c := newGinContext(r)

	_, _, err = h.authenticateWebSocket(c)
	require.Error(t, err)
	assert.ErrorIs(t, err, jwt.ErrTokenInvalidClaims)
}

func TestAuthenticateWebSocketJWTInvalidUserIDFormat(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil)
	now := time.Now()
	claims := jwt.MapClaims{
		"user_id": notAValidUUID,
		"exp":     jwt.NewNumericDate(now.Add(15 * time.Minute)),
		"iat":     jwt.NewNumericDate(now),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	// nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key — test fixture, not a real credential
	signed, err := token.SignedString([]byte(testJWTSecret))
	require.NoError(t, err)

	r, _ := http.NewRequest("GET", wsTokenPath+signed, nil)
	c := newGinContext(r)

	_, _, err = h.authenticateWebSocket(c)
	require.Error(t, err)
}

func TestAuthenticateWebSocketJWTNonHMACSigningMethod(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil)
	now := time.Now()
	claims := jwt.MapClaims{
		"user_id": uuid.New().String(),
		"exp":     jwt.NewNumericDate(now.Add(15 * time.Minute)),
		"iat":     jwt.NewNumericDate(now),
	}
	// Sign with RS256 (a strong algorithm the server doesn't accept — server only allows HMAC).
	// This tests rejection of non-HMAC tokens without using the weak "none" algorithm.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(rsaKey)
	require.NoError(t, err)

	r, _ := http.NewRequest("GET", wsTokenPath+signed, nil)
	c := newGinContext(r)

	_, _, err = h.authenticateWebSocket(c)
	require.Error(t, err)
}

// --- checkTokenBlacklist tests (require Redis) ---

func TestCheckTokenBlacklistNoJTI(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil)
	r, _ := http.NewRequest("GET", "/ws", nil)
	c := newGinContext(r)

	claims := jwt.MapClaims{"user_id": uuid.New().String()}
	err := h.checkTokenBlacklist(c, claims)
	assert.NoError(t, err)
}

func TestCheckTokenBlacklistEmptyJTI(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil)
	r, _ := http.NewRequest("GET", "/ws", nil)
	c := newGinContext(r)

	claims := jwt.MapClaims{"user_id": uuid.New().String(), "jti": ""}
	err := h.checkTokenBlacklist(c, claims)
	assert.NoError(t, err)
}

func TestCheckTokenBlacklistBlacklistedToken(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil)

	jti := uuid.New().String()
	ctx := context.Background()
	err := redisClient.Set(ctx, fmt.Sprintf("blacklist:%s", jti), "1", 5*time.Minute).Err()
	require.NoError(t, err)

	r, _ := http.NewRequest("GET", "/ws", nil)
	c := newGinContext(r)
	claims := jwt.MapClaims{"user_id": uuid.New().String(), "jti": jti}

	err = h.checkTokenBlacklist(c, claims)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token has been revoked")
}

func TestCheckTokenBlacklistNonBlacklistedToken(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil)

	r, _ := http.NewRequest("GET", "/ws", nil)
	c := newGinContext(r)
	claims := jwt.MapClaims{"user_id": uuid.New().String(), "jti": uuid.New().String()}

	err := h.checkTokenBlacklist(c, claims)
	assert.NoError(t, err)
}

// --- authenticateViaTicket tests (require Redis) ---

func TestAuthenticateViaTicketValid(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil)

	userID := uuid.New()
	ticket := "test-ticket-valid"
	ctx := context.Background()
	err := redisClient.Set(ctx, wsTicketKeyPfx+ticket, userID.String(), 30*time.Second).Err()
	require.NoError(t, err)

	r, _ := http.NewRequest("GET", wsTicketPath+ticket, nil)
	c := newGinContext(r)

	gotID, sessionID, err := h.authenticateWebSocket(c)
	require.NoError(t, err)
	assert.Equal(t, userID, gotID)
	assert.Equal(t, "", sessionID)
}

func TestAuthenticateViaTicketWithSessionID(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil)

	userID := uuid.New()
	ticket := "test-ticket-session"
	ctx := context.Background()
	err := redisClient.Set(ctx, wsTicketKeyPfx+ticket, userID.String()+":my-session-id", 30*time.Second).Err()
	require.NoError(t, err)

	r, _ := http.NewRequest("GET", wsTicketPath+ticket, nil)
	c := newGinContext(r)

	gotID, sessionID, err := h.authenticateWebSocket(c)
	require.NoError(t, err)
	assert.Equal(t, userID, gotID)
	assert.Equal(t, "my-session-id", sessionID)
}

func TestAuthenticateViaTicketInvalidTicket(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil)

	r, _ := http.NewRequest("GET", wsTicketPath+"nonexistent-ticket", nil)
	c := newGinContext(r)

	_, _, err := h.authenticateWebSocket(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid ticket")
}

func TestAuthenticateViaTicketInvalidUserIDInTicket(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil)

	ticket := "test-ticket-bad-uuid"
	ctx := context.Background()
	err := redisClient.Set(ctx, wsTicketKeyPfx+ticket, notAValidUUID, 30*time.Second).Err()
	require.NoError(t, err)

	r, _ := http.NewRequest("GET", wsTicketPath+ticket, nil)
	c := newGinContext(r)

	_, _, err = h.authenticateWebSocket(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid user ID in ticket")
}

// --- authenticateWebSocket with blacklisted JWT via full flow ---

func TestAuthenticateWebSocketBlacklistedJWT(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil)

	userID := uuid.New()
	jti := uuid.New().String()
	now := time.Now()
	claims := jwt.MapClaims{
		"user_id": userID.String(),
		"jti":     jti,
		"exp":     jwt.NewNumericDate(now.Add(15 * time.Minute)),
		"iat":     jwt.NewNumericDate(now),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	// nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key — test fixture, not a real credential
	signed, err := token.SignedString([]byte(testJWTSecret))
	require.NoError(t, err)

	ctx := context.Background()
	err = redisClient.Set(ctx, fmt.Sprintf("blacklist:%s", jti), "1", 5*time.Minute).Err()
	require.NoError(t, err)

	r, _ := http.NewRequest("GET", wsTokenPath+signed, nil)
	c := newGinContext(r)

	_, _, err = h.authenticateWebSocket(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token has been revoked")
}

// --- HandleWebSocket tests ---

func TestHandleWebSocketUnauthorized(t *testing.T) {
	hub := NewHub(nil, nil)
	h := NewHandler(hub, nil, nil, testJWTSecret, []string{"*"})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/ws", nil)

	h.HandleWebSocket(c)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func setupWSTestServer(t *testing.T, h *Handler, hub *Hub) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/ws", h.HandleWebSocket)
	srv := httptest.NewServer(router)
	// Cleanup order (LIFO): hub.Shutdown runs first (waits for goroutines),
	// then srv.Close stops accepting new connections.
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		hub.Shutdown()
	})
	return srv
}

func TestHandleWebSocketDBLookupFallback(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	go hub.Run()

	h := NewHandler(hub, db, redisClient, testJWTSecret, []string{"*"})

	userID := uuid.New()
	token := generateTestJWT(t, userID.String(), testJWTSecret)

	srv := setupWSTestServer(t, h, hub)

	wsURL := "ws" + srv.URL[4:] + wsTokenPath + token
	conn, resp, err := gorillaWS.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	_ = conn.Close()
}

func TestHandleWebSocketDBLookupSuccess(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	go hub.Run()

	userID := uuid.New()
	hash := "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, $2, $3, $4, true, true)`,
		userID.String(), "wstest@test.concord.chat", "wshandleruser", hash)
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM users WHERE id = $1`, userID.String()) //nolint:errcheck,gosec
	})

	h := NewHandler(hub, db, redisClient, testJWTSecret, []string{"*"})
	token := generateTestJWT(t, userID.String(), testJWTSecret)

	srv := setupWSTestServer(t, h, hub)

	wsURL := "ws" + srv.URL[4:] + wsTokenPath + token
	conn, resp, err := gorillaWS.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	_ = conn.Close()
}

func TestHandleWebSocketTicketAuth(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	go hub.Run()

	userID := uuid.New()
	hash := "$argon2id$v=19$m=65536,t=3,p=4$3pE9STD1TqLPoZQ2/BTLCg$8SKTCjsZh8Q7pAulEqAIEzJQK9eeOb5ipWhPz4REdCY" //nolint:gosec
	_, err := db.Exec(`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified) VALUES ($1, $2, $3, $4, true, true)`,
		userID.String(), "wsticket@test.concord.chat", "wsticketuser", hash)
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM users WHERE id = $1`, userID.String()) //nolint:errcheck,gosec
	})

	ticket := "integration-test-ticket"
	ctx := context.Background()
	err = redisClient.Set(ctx, wsTicketKeyPfx+ticket, userID.String()+":test-session", 30*time.Second).Err()
	require.NoError(t, err)

	h := NewHandler(hub, db, redisClient, testJWTSecret, []string{"*"})

	srv := setupWSTestServer(t, h, hub)

	wsURL := "ws" + srv.URL[4:] + wsTicketPath + ticket
	conn, resp, err := gorillaWS.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	_ = conn.Close()
}
