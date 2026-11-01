package websocket

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

const (
	originMain = "https://concordvoice.chat"
	originApp  = "https://app.concordvoice.chat"
	originFile = "file://"
)

func testTicketValue(userID, sessionID, epoch string) string {
	return fmt.Sprintf("%s:%s:%s:%d", userID, sessionID, epoch, time.Now().Add(15*time.Minute).Unix())
}

// --- NewHandler tests ---

func TestNewHandlerSetsFields(t *testing.T) {
	hub := NewHub(nil, nil)
	origins := []string{originMain, originApp}

	h := NewHandler(hub, nil, nil, "test-secret", origins, nil, nil)

	require.NotNil(t, h)
	assert.Equal(t, hub, h.hub)
	assert.Equal(t, "test-secret", h.jwtSecret)
	assert.Equal(t, origins, h.allowedOrigins)
	assert.NotNil(t, h.upgrader)
}

func TestNewHandlerNilDepsDoNotPanic(t *testing.T) {
	// NewHandler with nil deps is safe for CheckOrigin testing
	h := NewHandler(nil, nil, nil, "", nil, nil, nil)
	require.NotNil(t, h)
}

func TestRequestSupportsActivityRichPresenceRequiresOneExactOptIn(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "absent", path: "/ws", want: false},
		{name: "exact opt in", path: "/ws?activity_rich_presence=1", want: true},
		{name: "boolean text is not accepted", path: "/ws?activity_rich_presence=true", want: false},
		{name: "zero is not accepted", path: "/ws?activity_rich_presence=0", want: false},
		{name: "duplicate values fail closed", path: "/ws?activity_rich_presence=1&activity_rich_presence=0", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, test.path, nil)
			require.NoError(t, err)
			assert.Equal(t, test.want, requestSupportsActivityRichPresence(request))
		})
	}
}

// --- CheckOrigin comprehensive tests ---

func TestCheckOriginEmptyAllowList(t *testing.T) {
	h := NewHandler(nil, nil, nil, "", []string{}, nil, nil)

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
	h := NewHandler(nil, nil, nil, "", origins, nil, nil)

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
	h := NewHandler(nil, nil, nil, "", []string{originMain}, nil, nil)

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
			h := NewHandler(nil, nil, nil, "", tt.origins, nil, nil)
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

func TestParseUserIDFromClaimsPropagatesSessionID(t *testing.T) {
	// #2201 review: the JWT fallback must carry the access token's sid so a
	// socket it opens is evictable by DisconnectSession when that refresh
	// lineage is revoked.
	id := uuid.New()
	claims := jwt.MapClaims{"user_id": id.String(), "sid": "sess-abc123"}

	userID, sessionID, err := parseUserIDFromClaims(claims)
	require.NoError(t, err)
	assert.Equal(t, id, userID)
	assert.Equal(t, "sess-abc123", sessionID)
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
		"iss":     middleware.AccessTokenIssuer,
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
	h := NewHandler(nil, nil, nil, testJWTSecret, nil, nil, nil)
	r, _ := http.NewRequest("GET", "/ws", nil)
	c := newGinContext(r)

	_, _, err := h.authenticateWebSocket(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no authentication provided")
}

func TestAuthenticateWebSocketJWTViaHeader(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	userID := dbtest.CreateUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)
	h := NewHandler(nil, db, setupHubTestRedis(t), testJWTSecret, nil, nil, nil)
	token := access.token

	r, _ := http.NewRequest("GET", "/ws", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	c := newGinContext(r)

	gotID, sessionID, err := h.authenticateWebSocket(c)
	require.NoError(t, err)
	assert.Equal(t, userID, gotID)
	assert.Equal(t, access.sid.String(), sessionID)
}

func TestAuthenticateWebSocketJWTViaQueryParam(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	userID := dbtest.CreateUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)
	h := NewHandler(nil, db, setupHubTestRedis(t), testJWTSecret, nil, nil, nil)
	token := access.token

	r, _ := http.NewRequest("GET", wsTokenPath+token, nil)
	c := newGinContext(r)

	gotID, sessionID, err := h.authenticateWebSocket(c)
	require.NoError(t, err)
	assert.Equal(t, userID, gotID)
	assert.Equal(t, access.sid.String(), sessionID)
}

func TestAuthenticateWebSocketRejectsPendingTokenTheft(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	redisClient := setupHubTestRedis(t)
	userID := dbtest.CreateUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)
	baselineTicket := admissionMintTicket(t, db, redisClient, access)
	pendingTicket := admissionMintTicket(t, db, redisClient, access)
	h := NewHandler(nil, db, redisClient, testJWTSecret, nil, nil, nil)

	jwtRequest, _ := http.NewRequest("GET", wsTokenPath+access.token, nil)
	gotID, sid, err := h.authenticateWebSocket(newGinContext(jwtRequest))
	require.NoError(t, err, "ordinary JWT admission must remain available")
	require.Equal(t, userID, gotID)
	require.Equal(t, access.sid.String(), sid)
	baselineRequest, _ := http.NewRequest("GET", wsTicketPath+baselineTicket, nil)
	gotID, sid, err = h.authenticateWebSocket(newGinContext(baselineRequest))
	require.NoError(t, err, "ordinary producer-ticket admission must remain available")
	require.Equal(t, userID, gotID)
	require.Equal(t, access.sid.String(), sid)

	_, err = db.Exec(`INSERT INTO token_theft_pending_revocations (user_id, operation_id) VALUES ($1, $2)`, userID, uuid.New())
	require.NoError(t, err)
	blocked, err := redisClient.Exists(context.Background(), middleware.TokenTheftBlockedKey(userID.String())).Result()
	require.NoError(t, err)
	require.Zero(t, blocked, "the database pending row must suffice without a Redis marker")
	_, _, err = h.authenticateWebSocket(newGinContext(jwtRequest))
	require.ErrorIs(t, err, middleware.ErrTokenTheftRevocationPending)

	ticketRequest, _ := http.NewRequest("GET", wsTicketPath+pendingTicket, nil)
	_, _, err = h.authenticateWebSocket(newGinContext(ticketRequest))
	require.ErrorIs(t, err, middleware.ErrTokenTheftRevocationPending)
}

func TestAuthenticateWebSocketInvalidJWT(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil, nil, nil)

	r, _ := http.NewRequest("GET", wsTokenPath+"not-a-valid-jwt", nil)
	c := newGinContext(r)

	_, _, err := h.authenticateWebSocket(c)
	require.Error(t, err)
}

func TestAuthenticateWebSocketWrongSecret(t *testing.T) {
	userID := uuid.New()
	h := NewHandler(nil, nil, nil, testJWTSecret, nil, nil, nil)
	token := generateTestJWT(t, userID.String(), "wrong-secret")

	r, _ := http.NewRequest("GET", wsTokenPath+token, nil)
	c := newGinContext(r)

	_, _, err := h.authenticateWebSocket(c)
	require.Error(t, err)
}

func TestAuthenticateWebSocketExpiredJWT(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil, nil, nil)
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":     middleware.AccessTokenIssuer,
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
	h := NewHandler(nil, nil, nil, testJWTSecret, nil, nil, nil)
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": middleware.AccessTokenIssuer,
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
	h := NewHandler(nil, nil, nil, testJWTSecret, nil, nil, nil)
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":     middleware.AccessTokenIssuer,
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
	h := NewHandler(nil, nil, nil, testJWTSecret, nil, nil, nil)
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":     middleware.AccessTokenIssuer,
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
	h := NewHandler(nil, nil, nil, testJWTSecret, nil, nil, nil)
	r, _ := http.NewRequest("GET", "/ws", nil)
	c := newGinContext(r)

	claims := jwt.MapClaims{"user_id": uuid.New().String()}
	err := h.checkTokenBlacklist(c, claims)
	assert.NoError(t, err)
}

func TestCheckTokenBlacklistEmptyJTI(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil, nil, nil)
	r, _ := http.NewRequest("GET", "/ws", nil)
	c := newGinContext(r)

	claims := jwt.MapClaims{"user_id": uuid.New().String(), "jti": ""}
	err := h.checkTokenBlacklist(c, claims)
	assert.NoError(t, err)
}

func TestCheckTokenBlacklistBlacklistedToken(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil, nil, nil)

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
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil, nil, nil)

	r, _ := http.NewRequest("GET", "/ws", nil)
	c := newGinContext(r)
	claims := jwt.MapClaims{"user_id": uuid.New().String(), "jti": uuid.New().String()}

	err := h.checkTokenBlacklist(c, claims)
	assert.NoError(t, err)
}

// --- authenticateViaTicket tests (require Redis) ---

func TestAuthenticateViaTicketValid(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(nil, db, redisClient, testJWTSecret, nil, nil, nil)

	userID := dbtest.CreateUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)
	ticket := admissionMintTicket(t, db, redisClient, access)

	r, _ := http.NewRequest("GET", wsTicketPath+ticket, nil)
	c := newGinContext(r)

	gotID, sessionID, err := h.authenticateWebSocket(c)
	require.NoError(t, err)
	assert.Equal(t, userID, gotID)
	assert.Equal(t, access.sid.String(), sessionID)

	_, _, err = h.authenticateWebSocket(newGinContext(r))
	require.ErrorContains(t, err, "invalid ticket", "a redeemed producer ticket must be single use")
}

func TestAuthenticateViaTicketWithSessionID(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(nil, db, redisClient, testJWTSecret, nil, nil, nil)

	userID := dbtest.CreateUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)
	ticket := admissionMintTicket(t, db, redisClient, access)

	r, _ := http.NewRequest("GET", wsTicketPath+ticket, nil)
	c := newGinContext(r)

	gotID, sessionID, err := h.authenticateWebSocket(c)
	require.NoError(t, err)
	assert.Equal(t, userID, gotID)
	assert.Equal(t, access.sid.String(), sessionID)
}

func TestAuthenticateViaTicketInvalidTicket(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil, nil, nil)

	r, _ := http.NewRequest("GET", wsTicketPath+"nonexistent-ticket", nil)
	c := newGinContext(r)

	_, _, err := h.authenticateWebSocket(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid ticket")
}

func TestAuthenticateViaTicketInvalidUserIDInTicket(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil, nil, nil)

	ticket := "test-ticket-bad-uuid"
	ctx := context.Background()
	err := redisClient.Set(ctx, wsTicketKeyPfx+ticket, testTicketValue(notAValidUUID, "", ""), 30*time.Second).Err()
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
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil, nil, nil)

	userID := uuid.New()
	jti := uuid.New().String()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":     middleware.AccessTokenIssuer,
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
	h := NewHandler(hub, nil, nil, testJWTSecret, []string{"*"}, nil, nil)
	h.SetClientMinimums(middleware.ClientMinimums{Desktop: "3.0.0"})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("GET", "/ws?client_version=0.0.1", nil)

	h.HandleWebSocket(c)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.JSONEq(t, `{"error":"Unauthorized"}`, w.Body.String())
}

func TestHandlerSetClientMinimums(t *testing.T) {
	h := NewHandler(nil, nil, nil, testJWTSecret, nil, nil, nil)
	minimums := middleware.ClientMinimums{Desktop: "3.0.0", IOS: "1.0.0", Android: "1.1.0"}

	h.SetClientMinimums(minimums)

	assert.Equal(t, minimums, h.clientMinimums)
}

func TestHandleWebSocketClientVersionFloorTicket(t *testing.T) {
	testHandleWebSocketClientVersionFloor(t, "ticket", func(t *testing.T, db *sql.DB, redisClient *redis.Client, access admissionAccess) string {
		t.Helper()
		return wsTicketPath + admissionMintTicket(t, db, redisClient, access)
	})
}

func TestHandleWebSocketClientVersionFloorJWT(t *testing.T) {
	testHandleWebSocketClientVersionFloor(t, "direct JWT", func(t *testing.T, _ *sql.DB, _ *redis.Client, access admissionAccess) string {
		t.Helper()
		return wsTokenPath + access.token
	})
}

func testHandleWebSocketClientVersionFloor(t *testing.T, authPathName string, authPath func(*testing.T, *sql.DB, *redis.Client, admissionAccess) string) {
	t.Helper()
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	go hub.Run()
	handler := NewHandler(hub, db, redisClient, testJWTSecret, []string{"*"}, nil, nil)
	server := setupWSTestServer(t, handler, hub)
	userID := dbtest.CreateUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)

	tests := []struct {
		name          string
		minimum       string
		ios           string
		android       string
		platform      string
		clientVersion string
		wantStatus    int
	}{
		{name: "iOS uses its own floor", minimum: "3.0.0", ios: "1.0.0", platform: "ios", clientVersion: "1.0.0", wantStatus: http.StatusSwitchingProtocols},
		{name: "iOS older", minimum: "0.1.0", ios: "1.0.0", platform: "ios", clientVersion: "0.9.0", wantStatus: http.StatusForbidden},
		{name: "Android uses its own floor", minimum: "3.0.0", android: "1.0.0", platform: "android", clientVersion: "1.0.0", wantStatus: http.StatusSwitchingProtocols},
		{name: "Android older", minimum: "0.1.0", android: "1.0.0", platform: "android", clientVersion: "0.9.0", wantStatus: http.StatusForbidden},
		{name: "unknown platform", platform: "web", clientVersion: "3.0.0", wantStatus: http.StatusBadRequest},
		{name: "Pilot equal", minimum: "1.0.0", clientVersion: "1.0.0-pilot.0", wantStatus: http.StatusSwitchingProtocols},
		{name: "Pilot newer", minimum: "1.0.0", clientVersion: "1.1.0-pilot.2", wantStatus: http.StatusSwitchingProtocols},
		{name: "Pilot older", minimum: "1.0.0", clientVersion: "0.9.0-pilot.99", wantStatus: http.StatusForbidden},
		{name: "Pilot malformed", minimum: "1.0.0", clientVersion: "1.1.0-pilot.01", wantStatus: http.StatusForbidden},
		{name: "Pilot nonzero patch", minimum: "1.0.0", clientVersion: "1.1.1-pilot.2", wantStatus: http.StatusForbidden},
		{name: "Pilot floor", minimum: "1.0.0-pilot.0", clientVersion: "1.1.0-pilot.2", wantStatus: http.StatusForbidden},
		{name: "disabled", clientVersion: "0.0.1", wantStatus: http.StatusSwitchingProtocols},
		{name: "missing", minimum: "3.0.0", wantStatus: http.StatusForbidden},
		{name: "malformed", minimum: "3.0.0", clientVersion: "three", wantStatus: http.StatusForbidden},
		{name: "older", minimum: "3.0.0", clientVersion: "2.9.9", wantStatus: http.StatusForbidden},
		{name: "equal", minimum: "3.0.0", clientVersion: "3.0.0", wantStatus: http.StatusSwitchingProtocols},
		{name: "newer", minimum: "3.0.0", clientVersion: "3.0.1", wantStatus: http.StatusSwitchingProtocols},
	}

	for _, test := range tests {
		t.Run(authPathName+"/"+test.name, func(t *testing.T) {
			handler.SetClientMinimums(middleware.ClientMinimums{Desktop: test.minimum, IOS: test.ios, Android: test.android})
			wsURL := "ws" + server.URL[4:] + authPath(t, db, redisClient, access) + "&client_version=" + test.clientVersion
			if test.platform != "" {
				wsURL += "&client_platform=" + test.platform
			}

			conn, response, err := gorillaWS.DefaultDialer.Dial(wsURL, nil)
			if test.wantStatus == http.StatusSwitchingProtocols {
				require.NoError(t, err)
				assert.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
				require.NoError(t, conn.Close())
				return
			}

			require.Error(t, err)
			assert.Nil(t, conn, "a version refusal must not upgrade the connection")
			require.NotNil(t, response, "version denial must happen before the WebSocket upgrade")
			defer func() {
				assert.NoError(t, response.Body.Close())
			}()
			assert.Equal(t, test.wantStatus, response.StatusCode)
			if test.wantStatus == http.StatusBadRequest {
				return
			}

			wantMinimum := test.minimum
			switch test.platform {
			case "ios":
				wantMinimum = test.ios
			case "android":
				wantMinimum = test.android
			}
			var body attestation.ErrorResponse
			require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
			assert.Equal(t, attestation.ErrorResponse{
				Error:              "Client version too old",
				Code:               attestation.ErrVersionTooOld,
				UpdateAvailable:    true,
				RequiredMinVersion: wantMinimum,
				DownloadHelpURL:    attestation.DownloadHelpURLDefault,
			}, body)
		})
	}
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

// The fault is limited to the optional profile read; every authority query uses
// the real PostgreSQL connection, including the final registration guard.
type handlerProfileFailureConnector struct {
	driver.Connector
	failures atomic.Int64
}

func (c *handlerProfileFailureConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &handlerProfileFailureConn{
		admissionProfileConn: &admissionProfileConn{Conn: conn, reads: &c.failures},
		failures:             &c.failures,
	}, nil
}

type handlerProfileFailureConn struct {
	*admissionProfileConn
	failures *atomic.Int64
}

func (c *handlerProfileFailureConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if query == "SELECT username, display_name, avatar_url FROM users WHERE id = $1" {
		c.failures.Add(1)
		return nil, errors.New("injected downstream profile lookup failure")
	}
	return c.admissionProfileConn.QueryContext(ctx, query, args)
}

func TestHandleWebSocketDBLookupFallback(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	userID := dbtest.CreateUser(t, db)
	access := admissionCanonicalAccess(t, db, userID)
	connector, err := pq.NewConnector(dbtest.DatabaseURL())
	require.NoError(t, err)
	profile := &handlerProfileFailureConnector{Connector: connector}
	handlerDB := sql.OpenDB(profile)
	t.Cleanup(func() { require.NoError(t, handlerDB.Close()) })
	hub := NewHub(db, redisClient)
	go hub.Run()
	h := NewHandler(hub, handlerDB, redisClient, testJWTSecret, []string{"*"}, nil, nil)
	before, err := h.resolveAccessFamily(context.Background(), userID, access.sid.String(), access.epoch, access.expiresAt)
	require.NoError(t, err, "the fixture must have live family authority before the profile fault")
	require.Zero(t, profile.failures.Load())
	srv := setupWSTestServer(t, h, hub)

	wsURL := "ws" + srv.URL[4:] + wsTokenPath + access.token
	conn, resp, err := gorillaWS.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	require.EqualValues(t, 1, profile.failures.Load(), "the exact downstream profile SELECT must have failed")
	require.Eventually(t, func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		for clientID := range hub.userClients[userID] {
			client := hub.clients[clientID]
			if client != nil && client.SessionID == access.sid.String() {
				return client.Username == "Unknown" && client.DisplayName == nil && client.AvatarURL == nil && client.FamilyID == before.FamilyID
			}
		}
		return false
	}, time.Second, 10*time.Millisecond, "the live registered client must retain the Unknown profile fallback")
	after, err := h.resolveAccessFamily(context.Background(), userID, access.sid.String(), access.epoch, access.expiresAt)
	require.NoError(t, err, "family authority must remain live after registration")
	require.Equal(t, before.FamilyID, after.FamilyID)
}

func TestHandleWebSocketCapturesActivityCapabilityBeforeRegistration(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	go hub.Run()
	handler := NewHandler(hub, db, redisClient, testJWTSecret, []string{"*"}, nil, nil)

	userID := dbtest.CreateUser(t, db)
	token := admissionCanonicalAccess(t, db, userID).token
	server := setupWSTestServer(t, handler, hub)
	baseURL := "ws" + server.URL[4:] + wsTokenPath + token
	legacy, _, err := gorillaWS.DefaultDialer.Dial(baseURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = legacy.Close() })
	capable, _, err := gorillaWS.DefaultDialer.Dial(baseURL+"&activity_rich_presence=1", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = capable.Close() })

	require.Eventually(t, func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		legacyCount := 0
		capableCount := 0
		for clientID := range hub.userClients[userID] {
			client := hub.clients[clientID]
			if client == nil {
				continue
			}
			if client.activityRichPresenceCapable {
				capableCount++
			} else {
				legacyCount++
			}
		}
		return legacyCount == 1 && capableCount == 1
	}, time.Second, 10*time.Millisecond)
}

// TestHandleWebSocketSecurityHeadersOn101 locks the #2318 fix: gorilla builds
// the 101 handshake from Upgrade's responseHeader argument (headers set on the
// gin writer are NOT serialized), so the set passed to NewHandler must surface
// on the handshake response.
func TestHandleWebSocketSecurityHeadersOn101(t *testing.T) {
	db := setupHubTestDB(t)
	redisClient := setupHubTestRedis(t)
	hub := NewHub(db, redisClient)
	go hub.Run()

	secHeaders := http.Header{}
	secHeaders.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
	secHeaders.Set("X-Content-Type-Options", "nosniff")

	h := NewHandler(hub, db, redisClient, testJWTSecret, []string{"*"}, nil, secHeaders)

	userID := dbtest.CreateUser(t, db)
	token := admissionCanonicalAccess(t, db, userID).token

	srv := setupWSTestServer(t, h, hub)

	wsURL := "ws" + srv.URL[4:] + wsTokenPath + token
	conn, resp, err := gorillaWS.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	assert.Equal(t, "max-age=63072000; includeSubDomains; preload", resp.Header.Get("Strict-Transport-Security"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
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

	displayName := "WebSocket Profile"
	avatarURL := "https://example.test/avatar.png"
	_, err = db.Exec(`UPDATE users SET display_name = $2, avatar_url = $3 WHERE id = $1`, userID, displayName, avatarURL)
	require.NoError(t, err)
	h := NewHandler(hub, db, redisClient, testJWTSecret, []string{"*"}, nil, nil)
	access := admissionCanonicalAccess(t, db, userID)
	token := access.token

	srv := setupWSTestServer(t, h, hub)

	wsURL := "ws" + srv.URL[4:] + wsTokenPath + token
	conn, resp, err := gorillaWS.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	require.Eventually(t, func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		for clientID := range hub.userClients[userID] {
			client := hub.clients[clientID]
			if client != nil && client.SessionID == access.sid.String() {
				return client.Username == "wshandleruser" && client.DisplayName != nil && *client.DisplayName == displayName && client.AvatarURL != nil && *client.AvatarURL == avatarURL
			}
		}
		return false
	}, time.Second, 10*time.Millisecond, "the registered client must carry the SQL profile")
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

	access := admissionCanonicalAccess(t, db, userID)
	ticket := admissionMintTicket(t, db, redisClient, access)

	h := NewHandler(hub, db, redisClient, testJWTSecret, []string{"*"}, nil, nil)

	srv := setupWSTestServer(t, h, hub)

	wsURL := "ws" + srv.URL[4:] + wsTicketPath + ticket
	conn, resp, err := gorillaWS.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	_ = conn.Close()
}
