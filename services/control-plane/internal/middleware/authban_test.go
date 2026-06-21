package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathAuthBanTest = "/auth-ban-test"
	testIPClearAuth = "10.0.0.15"
)

var testBanConfig = middleware.AuthBanConfig{
	Threshold: 3,
	Window:    1 * time.Minute,
	Duration:  1 * time.Minute,
}

func TestDefaultAuthBanConfigReturnsSensibleDefaults(t *testing.T) {
	cfg := middleware.DefaultAuthBanConfig()
	assert.Equal(t, 10, cfg.Threshold)
	assert.Equal(t, 15*time.Minute, cfg.Window)
	assert.Equal(t, 15*time.Minute, cfg.Duration)
}

func setupAuthBanRouter(t *testing.T) (*gin.Engine, *testhelpers.TestServer) {
	t.Helper()
	ts := setupTS(t)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(pathAuthBanTest,
		middleware.AuthBanCheck(ts.Redis),
		func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		},
	)

	return router, ts
}

func doAuthBanRequest(router *gin.Engine, ip string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", pathAuthBanTest, nil)
	req.RemoteAddr = ip + ":12345"
	router.ServeHTTP(w, req)
	return w
}

func TestAuthBanCheckAllowsCleanIP(t *testing.T) {
	router, _ := setupAuthBanRouter(t)

	w := doAuthBanRequest(router, "10.0.0.1")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuthBanCheckBlocksBannedIP(t *testing.T) {
	router, ts := setupAuthBanRouter(t)
	ctx := context.Background()

	// Manually set a ban key
	err := ts.Redis.Set(ctx, "auth_ban:ip:10.0.0.2", "1", 1*time.Minute).Err()
	require.NoError(t, err)

	w := doAuthBanRequest(router, "10.0.0.2")
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Too many authentication failures")
}

func TestAuthBanCheckRetryAfterMatchesTTL(t *testing.T) {
	router, ts := setupAuthBanRouter(t)
	ctx := context.Background()

	// Set ban with known TTL
	err := ts.Redis.Set(ctx, "auth_ban:ip:10.0.0.3", "1", 2*time.Minute).Err()
	require.NoError(t, err)

	w := doAuthBanRequest(router, "10.0.0.3")
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	retryAfter := w.Header().Get("Retry-After")
	assert.NotEmpty(t, retryAfter)
	// Parse as int and assert within a reasonable range (avoids timing flakiness)
	retryAfterInt, parseErr := strconv.Atoi(retryAfter)
	require.NoError(t, parseErr)
	assert.True(t, retryAfterInt > 0 && retryAfterInt <= 120, "Retry-After should be >0 and <=120, got %d", retryAfterInt)
}

func TestAuthBanCheckFailsOpenOnRedisDown(t *testing.T) {
	ts := setupTS(t)

	// Close Redis to simulate failure
	_ = ts.Redis.Close()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(pathAuthBanTest,
		middleware.AuthBanCheck(ts.Redis),
		func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		},
	)

	w := doAuthBanRequest(router, "10.0.0.4")
	// Should fail open — allow the request
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuthBanCheckNilRedisPassesThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(pathAuthBanTest,
		middleware.AuthBanCheck(nil),
		func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		},
	)

	w := doAuthBanRequest(router, "10.0.0.5")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRecordAuthFailureNilRedisNoOp(_ *testing.T) {
	ctx := context.Background()
	// Should not panic
	middleware.RecordAuthFailure(ctx, nil, "10.0.0.6", testBanConfig)
}

func TestClearAuthFailuresNilRedisNoOp(_ *testing.T) {
	ctx := context.Background()
	// Should not panic
	middleware.ClearAuthFailures(ctx, nil, "10.0.0.7")
}

func TestRecordAuthFailureIncrementsCounter(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	middleware.RecordAuthFailure(ctx, ts.Redis, "10.0.0.10", testBanConfig)
	middleware.RecordAuthFailure(ctx, ts.Redis, "10.0.0.10", testBanConfig)

	count, err := ts.Redis.Get(ctx, "auth_failures:ip:10.0.0.10").Int()
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

func TestRecordAuthFailureSetsWindowTTL(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	middleware.RecordAuthFailure(ctx, ts.Redis, "10.0.0.11", testBanConfig)

	ttl, err := ts.Redis.TTL(ctx, "auth_failures:ip:10.0.0.11").Result()
	require.NoError(t, err)
	assert.True(t, ttl > 0 && ttl <= testBanConfig.Window, "failure key should have TTL within window")
}

func TestRecordAuthFailureBansAtThreshold(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	for i := 0; i < testBanConfig.Threshold; i++ {
		middleware.RecordAuthFailure(ctx, ts.Redis, "10.0.0.12", testBanConfig)
	}

	// Ban key should now exist
	exists, err := ts.Redis.Exists(ctx, "auth_ban:ip:10.0.0.12").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), exists, "ban key should exist after threshold failures")

	// Ban key should have a TTL
	ttl, err := ts.Redis.TTL(ctx, "auth_ban:ip:10.0.0.12").Result()
	require.NoError(t, err)
	assert.True(t, ttl > 0 && ttl <= testBanConfig.Duration)
}

func TestRecordAuthFailureNoBanBelowThreshold(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	for i := 0; i < testBanConfig.Threshold-1; i++ {
		middleware.RecordAuthFailure(ctx, ts.Redis, "10.0.0.13", testBanConfig)
	}

	exists, err := ts.Redis.Exists(ctx, "auth_ban:ip:10.0.0.13").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "ban key should NOT exist below threshold")
}

func TestRecordAuthFailureResetsCounterOnBan(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	for i := 0; i < testBanConfig.Threshold; i++ {
		middleware.RecordAuthFailure(ctx, ts.Redis, "10.0.0.14", testBanConfig)
	}

	// Failure counter should be deleted after ban
	exists, err := ts.Redis.Exists(ctx, "auth_failures:ip:10.0.0.14").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "failure counter should be cleared after ban")
}

func TestClearAuthFailuresRemovesCounter(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Record some failures
	middleware.RecordAuthFailure(ctx, ts.Redis, testIPClearAuth, testBanConfig)
	middleware.RecordAuthFailure(ctx, ts.Redis, testIPClearAuth, testBanConfig)

	// Clear
	middleware.ClearAuthFailures(ctx, ts.Redis, testIPClearAuth)

	exists, err := ts.Redis.Exists(ctx, "auth_failures:ip:"+testIPClearAuth).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "failure counter should be removed after clear")
}

func TestAuthBanDifferentIPsIndependent(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	// Ban IP A
	for i := 0; i < testBanConfig.Threshold; i++ {
		middleware.RecordAuthFailure(ctx, ts.Redis, "10.0.0.20", testBanConfig)
	}

	// IP A should be banned
	exists, err := ts.Redis.Exists(ctx, "auth_ban:ip:10.0.0.20").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), exists)

	// IP B should NOT be banned
	exists, err = ts.Redis.Exists(ctx, "auth_ban:ip:10.0.0.21").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists)
}

func TestAuthBanIntegrationFullFlow(t *testing.T) {
	ts := setupTS(t)
	ctx := context.Background()

	gin.SetMode(gin.TestMode)
	router := gin.New()

	// Simulate an auth endpoint that records failures
	router.POST(pathAuthBanTest,
		middleware.AuthBanCheck(ts.Redis),
		func(c *gin.Context) {
			// Simulate auth failure
			middleware.RecordAuthFailure(c.Request.Context(), ts.Redis, c.ClientIP(), testBanConfig)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid"})
		},
	)

	ip := "10.0.0.30"

	// Make threshold-1 requests (all should pass through ban check)
	for i := 0; i < testBanConfig.Threshold-1; i++ {
		w := doAuthBanRequest(router, ip)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "request %d should get through ban check", i+1)
	}

	// The threshold-th request passes ban check but triggers ban
	w := doAuthBanRequest(router, ip)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "threshold request should still get a handler response")

	// Next request should be blocked by ban check
	w = doAuthBanRequest(router, ip)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "post-ban request should be blocked")
	assert.NotEmpty(t, w.Header().Get("Retry-After"))

	// Clear the ban manually and verify access is restored
	ts.Redis.Del(ctx, "auth_ban:ip:"+ip)
	w = doAuthBanRequest(router, ip)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "after ban clear, requests should pass through again")
}
