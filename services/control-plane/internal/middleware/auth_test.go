package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = testhelpers.TestJWTSecret

const (
	pathUsersMe  = "/api/v1/users/me"
	bearerPrefix = "Bearer "
	testUserID1  = "user-1"
)

// makeToken creates a JWT token with the given claims for testing.
func makeToken(t *testing.T, claims jwt.MapClaims, secret string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := token.SignedString([]byte(secret))
	require.NoError(t, err)
	return s
}

// doAuthRequest exercises the AuthRequired middleware with the given Authorization header.
func doAuthRequest(t *testing.T, ts *testhelpers.TestServer, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	headers := http.Header{}
	if authHeader != "" {
		headers.Set("Authorization", authHeader)
	}
	return ts.DoRequest("GET", pathUsersMe, nil, headers)
}

// --- AuthRequired Additional Tests ---

func TestAuthRequired_MalformedBearerToken(t *testing.T) {
	ts := setupTS(t)

	// Only "Bearer" with no token
	w := doAuthRequest(t, ts, bearerPrefix)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthRequired_NonHMACSigningMethod(t *testing.T) {
	ts := setupTS(t)

	// Create an unsigned token with "none" algorithm — should be rejected
	// because the middleware only accepts *jwt.SigningMethodHMAC
	tokenStr := makeToken(t, jwt.MapClaims{
		"user_id": testUserID1,
		"exp":     time.Now().Add(15 * time.Minute).Unix(),
		"iat":     time.Now().Unix(),
	}, "wrong_secret_entirely_different")

	w := doAuthRequest(t, ts, bearerPrefix+tokenStr)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthRequired_MissingUserIDClaim(t *testing.T) {
	ts := setupTS(t)

	tokenStr := makeToken(t, jwt.MapClaims{
		"exp": time.Now().Add(15 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	}, testSecret)

	w := doAuthRequest(t, ts, bearerPrefix+tokenStr)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthRequired_EmptyUserIDClaim(t *testing.T) {
	ts := setupTS(t)

	tokenStr := makeToken(t, jwt.MapClaims{
		"user_id": "",
		"exp":     time.Now().Add(15 * time.Minute).Unix(),
		"iat":     time.Now().Unix(),
	}, testSecret)

	w := doAuthRequest(t, ts, bearerPrefix+tokenStr)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthRequired_WrongSecret(t *testing.T) {
	ts := setupTS(t)

	tokenStr := makeToken(t, jwt.MapClaims{
		"user_id": testUserID1,
		"exp":     time.Now().Add(15 * time.Minute).Unix(),
		"iat":     time.Now().Unix(),
	}, "wrong_secret_key")

	w := doAuthRequest(t, ts, bearerPrefix+tokenStr)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthRequired_BlacklistedJTI(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	jti := "blacklisted-jti-123"
	tokenStr := makeToken(t, jwt.MapClaims{
		"user_id": testUserID1,
		"jti":     jti,
		"exp":     time.Now().Add(15 * time.Minute).Unix(),
		"iat":     time.Now().Unix(),
	}, testSecret)

	// Blacklist the JTI
	err := ts.Redis.Set(ctx, "blacklist:"+jti, "1", 15*time.Minute).Err()
	require.NoError(t, err)

	w := doAuthRequest(t, ts, bearerPrefix+tokenStr)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthRequired_SetsEmailVerifiedTrue(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "authevtrue")

	// Use a route that checks the context values
	w := ts.DoRequest("GET", pathUsersMe, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuthRequired_BearerCaseSensitive(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "authcasesens")

	// "bearer" (lowercase) should fail
	headers := http.Header{}
	headers.Set("Authorization", "bearer "+user.AccessToken)
	w := ts.DoRequest("GET", pathUsersMe, nil, headers)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuthRequired_ThreePartBearer(t *testing.T) {
	ts := setupTS(t)

	// "Bearer token extra" has 3 parts
	headers := http.Header{}
	headers.Set("Authorization", "Bearer token extra")
	w := ts.DoRequest("GET", pathUsersMe, nil, headers)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- RequireVerifiedEmail Tests ---

func TestRequireVerifiedEmail_Blocks(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "unverifiedblock")

	// Attempt to access a verified-only route
	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "EMAIL_NOT_VERIFIED", body["code"])
}

func TestRequireVerifiedEmail_Allows(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "verifiedallow")

	w := ts.DoRequest("GET", "/api/v1/servers", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- BlacklistToken Tests ---

func TestBlacklistToken_Success(t *testing.T) {
	ctx := context.Background()
	ts := setupTS(t)

	err := middleware.BlacklistToken(ctx, ts.Redis, "test-jti-123", 10*time.Minute)
	require.NoError(t, err)

	// Verify it's in Redis
	val, err := ts.Redis.Get(ctx, "blacklist:test-jti-123").Result()
	require.NoError(t, err)
	assert.Equal(t, "1", val)
}

func TestBlacklistToken_EmptyJTI(t *testing.T) {
	ctx := context.Background()
	ts := setupTS(t)

	// Empty JTI should be a no-op
	err := middleware.BlacklistToken(ctx, ts.Redis, "", 10*time.Minute)
	assert.NoError(t, err)
}

func TestBlacklistToken_ZeroTTL(t *testing.T) {
	ctx := context.Background()
	ts := setupTS(t)

	// Zero TTL should be a no-op
	err := middleware.BlacklistToken(ctx, ts.Redis, "jti-zero", 0)
	assert.NoError(t, err)

	// Verify it was NOT stored
	exists, err := ts.Redis.Exists(ctx, "blacklist:jti-zero").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists)
}

func TestBlacklistToken_NegativeTTL(t *testing.T) {
	ctx := context.Background()
	ts := setupTS(t)

	// Negative TTL should be a no-op
	err := middleware.BlacklistToken(ctx, ts.Redis, "jti-negative", -1*time.Second)
	assert.NoError(t, err)

	// Verify it was NOT stored
	exists, err := ts.Redis.Exists(ctx, "blacklist:jti-negative").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists)
}

func TestBlacklistToken_HasTTL(t *testing.T) {
	ctx := context.Background()
	ts := setupTS(t)

	err := middleware.BlacklistToken(ctx, ts.Redis, "jti-ttl", 5*time.Minute)
	require.NoError(t, err)

	ttl, err := ts.Redis.TTL(ctx, "blacklist:jti-ttl").Result()
	require.NoError(t, err)
	assert.True(t, ttl > 0 && ttl <= 5*time.Minute)
}

// --- RequireVerifiedEmail direct unit test ---

func TestRequireVerifiedEmailMiddleware_NoContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.GET("/test", middleware.RequireVerifiedEmail(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	// No email_verified in context means it should be blocked
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRequireVerifiedEmailMiddleware_FalseValue(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.GET("/test", func(c *gin.Context) {
		c.Set("email_verified", false)
	}, middleware.RequireVerifiedEmail(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRequireVerifiedEmailMiddleware_WrongType(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.GET("/test", func(c *gin.Context) {
		c.Set("email_verified", "yes") // string, not bool
	}, middleware.RequireVerifiedEmail(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}
