package middleware_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathRateTest       = "/rate-test"
	headerRateLimitRem = "X-RateLimit-Remaining"
	pathUserRateTest   = "/user-rate-test"
	pathMaxIntTest     = "/maxint-test"
)

// setupRateLimitRouter creates a gin router with rate limiting for testing.
func setupRateLimitRouter(t *testing.T, requests int, window time.Duration) (*gin.Engine, *testhelpers.TestServer) {
	t.Helper()
	ts := setupTS(t)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET(pathRateTest, middleware.RateLimitByIP(ts.Redis, requests, window), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	return router, ts
}

func doRateLimitRequest(router *gin.Engine) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathRateTest, nil)
	req.RemoteAddr = "192.168.1.1:12345"
	router.ServeHTTP(w, req)
	return w
}

func TestRateLimitByIP_AllowsWithinLimit(t *testing.T) {
	router, _ := setupRateLimitRouter(t, 5, 1*time.Minute)

	for i := 0; i < 5; i++ {
		w := doRateLimitRequest(router)
		assert.Equal(t, http.StatusOK, w.Code, "request %d should succeed", i+1)
	}
}

func TestRateLimitByIP_BlocksExceeding(t *testing.T) {
	router, _ := setupRateLimitRouter(t, 3, 1*time.Minute)

	// First 3 should succeed
	for i := 0; i < 3; i++ {
		w := doRateLimitRequest(router)
		assert.Equal(t, http.StatusOK, w.Code)
	}

	// 4th should be rate limited
	w := doRateLimitRequest(router)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Rate limit exceeded")
}

func TestRateLimitByIP_SetsHeaders(t *testing.T) {
	router, _ := setupRateLimitRouter(t, 10, 1*time.Minute)

	w := doRateLimitRequest(router)
	assert.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, "10", w.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "9", w.Header().Get(headerRateLimitRem))
	assert.NotEmpty(t, w.Header().Get("X-RateLimit-Reset"))
}

func TestRateLimitByIP_RemainingDecreases(t *testing.T) {
	router, _ := setupRateLimitRouter(t, 5, 1*time.Minute)

	for i := 0; i < 5; i++ {
		w := doRateLimitRequest(router)
		expected := fmt.Sprintf("%d", 5-i-1)
		assert.Equal(t, expected, w.Header().Get(headerRateLimitRem), "request %d", i+1)
	}
}

func TestRateLimitByIP_RemainingNeverNegative(t *testing.T) {
	router, _ := setupRateLimitRouter(t, 2, 1*time.Minute)

	doRateLimitRequest(router)      // 1
	doRateLimitRequest(router)      // 2
	w := doRateLimitRequest(router) // 3 (over limit)

	assert.Equal(t, "0", w.Header().Get(headerRateLimitRem), "remaining should be 0, not negative")
}

func TestRateLimitByIP_RetryAfterHeader(t *testing.T) {
	router, _ := setupRateLimitRouter(t, 1, 1*time.Minute)

	doRateLimitRequest(router)      // Use up the limit
	w := doRateLimitRequest(router) // Exceeds limit

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	retryAfter := w.Header().Get("Retry-After")
	assert.NotEmpty(t, retryAfter, "should include Retry-After header when rate limited")
}

func TestRateLimitByIP_DifferentIPsAreIndependent(t *testing.T) {
	ts := setupTS(t)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET(pathRateTest, middleware.RateLimitByIP(ts.Redis, 2, 1*time.Minute), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// IP 1: use up the limit
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathRateTest, nil)
		req.RemoteAddr = "10.0.0.1:12345"
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	}

	// IP 1: should be blocked
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathRateTest, nil)
	req.RemoteAddr = "10.0.0.1:12345"
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	// IP 2: should still work
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", pathRateTest, nil)
	req.RemoteAddr = "10.0.0.2:12345"
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- RateLimitByUser Tests ---

func TestRateLimitByUser_AuthenticatedUser(t *testing.T) {
	ts := setupTS(t)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET(pathUserRateTest, func(c *gin.Context) {
		c.Set("user_id", "user-123")
	}, middleware.RateLimitByUser(ts.Redis, 3, 1*time.Minute), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathUserRateTest, nil)
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	}

	// 4th request should be blocked
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathUserRateTest, nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestRateLimitByUser_FallsBackToIP(t *testing.T) {
	ts := setupTS(t)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	// No user_id set in context (unauthenticated)
	router.GET(pathUserRateTest, middleware.RateLimitByUser(ts.Redis, 2, 1*time.Minute), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathUserRateTest, nil)
		req.RemoteAddr = "10.0.0.99:9999"
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	}

	// Should be rate limited by IP
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathUserRateTest, nil)
	req.RemoteAddr = "10.0.0.99:9999"
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestRateLimitByUser_DifferentUsersAreIndependent(t *testing.T) {
	ts := setupTS(t)

	gin.SetMode(gin.TestMode)

	callWith := func(userID string) *httptest.ResponseRecorder {
		router := gin.New()
		router.GET(pathUserRateTest, func(c *gin.Context) {
			c.Set("user_id", userID)
		}, middleware.RateLimitByUser(ts.Redis, 1, 1*time.Minute), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		})

		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathUserRateTest, nil)
		router.ServeHTTP(w, req)
		return w
	}

	// User A uses their single request
	w := callWith("user-a")
	assert.Equal(t, http.StatusOK, w.Code)

	// User B should still be able to make a request
	w = callWith("user-b")
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- maxInt unit test ---

func TestMaxIntFunc(t *testing.T) {
	// maxInt is not exported, but we can test it indirectly
	// by verifying the X-RateLimit-Remaining header never goes negative.
	ts := setupTS(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET(pathMaxIntTest, middleware.RateLimitByIP(ts.Redis, 1, 1*time.Minute), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Use the single allowed request
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathMaxIntTest, nil)
	req.RemoteAddr = "10.0.0.50:1234"
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "0", w.Header().Get(headerRateLimitRem))

	// Exceed the limit — remaining should still be 0 (maxInt(0, negative) = 0)
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", pathMaxIntTest, nil)
	req.RemoteAddr = "10.0.0.50:1234"
	router.ServeHTTP(w, req)
	assert.Equal(t, "0", w.Header().Get(headerRateLimitRem))
}

// --- RateLimitByUserFailClosed Tests (#158) ---

// deadRedis returns a redis client pointed at a closed port so Incr errors,
// simulating a Redis backend outage without a mock. No retries / short dial
// timeout keep the failure fast and deterministic.
func deadRedis() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1,
	})
}

func TestRateLimitByUserFailClosed_AllowsUnderLimit(t *testing.T) {
	ts := setupTS(t)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET(pathUserRateTest, func(c *gin.Context) {
		c.Set("user_id", "user-fc-under")
	}, middleware.RateLimitByUserFailClosed(ts.Redis, 3, 1*time.Minute), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathUserRateTest, nil)
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "request %d should pass under the limit", i+1)
	}
}

// Fail-closed: a Redis backend error must REJECT (503), not allow.
func TestRateLimitByUserFailClosed_RejectsOnRedisError(t *testing.T) {
	rdb := deadRedis()
	defer func() { _ = rdb.Close() }()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET(pathUserRateTest, func(c *gin.Context) {
		c.Set("user_id", "user-fc-err")
	}, middleware.RateLimitByUserFailClosed(rdb, 10, 1*time.Hour), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathUserRateTest, nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "fail-closed: Redis error must reject the request")
}

// Regression: the default RateLimitByUser MUST keep failing OPEN on a Redis
// error (availability-first). This locks the asymmetry that Fix #1 introduces.
func TestRateLimitByUser_FailsOpenOnRedisError(t *testing.T) {
	rdb := deadRedis()
	defer func() { _ = rdb.Close() }()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET(pathUserRateTest, func(c *gin.Context) {
		c.Set("user_id", "user-fo-err")
	}, middleware.RateLimitByUser(rdb, 10, 1*time.Hour), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathUserRateTest, nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "fail-open: Redis error must still allow the request")
}
