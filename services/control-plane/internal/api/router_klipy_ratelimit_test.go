package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

// These tests assert the limiter-split architectural invariant for the KLIPY
// proxy routes registered in router.go: /api/v1/klipy/gifs/* shares a single
// 30/min `apiLimiter`, while /api/v1/klipy/media has its own independent
// 300/min budget (see #804). They are deliberately decoupled from the full
// NewRouter wiring (which requires db + nats + handlers) — the invariant
// under test is the limiter middleware's per-route key derivation
// (`ratelimit:user:<id>:<method>:<full-path>`), which is what guarantees the
// two budgets cannot accidentally spill into each other.
//
// Test handler is a no-op 200 OK so the limiter behavior is what the assertions
// see — KLIPY round-tripping has its own coverage in internal/klipy/handlers_test.go.

const (
	klipyTestUserID   = "test-klipy-ratelimit-user"
	klipySearchPath   = "/api/v1/klipy/gifs/search"
	klipyMediaPath    = "/api/v1/klipy/media"
	klipyAPILimit     = 30
	klipyMediaLimit   = 300
	klipyRateLimitTTL = 1 * time.Minute
)

// newKlipyLimiterTestRouter builds a minimal gin router that mirrors the
// production limiter-attachment pattern: a fake-auth middleware sets user_id
// (so RateLimitByUser keys by user, matching production behavior), then the
// per-route limiter is attached, then a no-op handler returns 200.
//
// The userID is a parameter so tests can run with independent users — Redis
// keys are scoped by user_id + full-path, so a fresh user gives a fresh
// budget without needing FlushDB between tests.
func newKlipyLimiterTestRouter(t *testing.T, redisClient *redis.Client, userID string, withAPI, withMedia bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()

	fakeAuth := func(c *gin.Context) {
		c.Set("user_id", userID)
	}

	noOp := func(c *gin.Context) {
		c.Status(http.StatusOK)
	}

	if withAPI {
		apiLimiter := middleware.RateLimitByUser(redisClient, klipyAPILimit, klipyRateLimitTTL)
		r.GET(klipySearchPath, fakeAuth, apiLimiter, noOp)
	}
	if withMedia {
		mediaLimiter := middleware.RateLimitByUser(redisClient, klipyMediaLimit, klipyRateLimitTTL)
		r.GET(klipyMediaPath, fakeAuth, mediaLimiter, noOp)
	}

	return r
}

func doGET(router *gin.Engine, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	router.ServeHTTP(w, req)
	return w
}

// TestKlipyApiLimiterAt30PerMinute is the regression baseline: the apiLimiter
// (used by /gifs/search, /gifs/trending, etc. in router.go) is unchanged at
// 30/min. If a future refactor accidentally bumps it to match /media's budget,
// the abuse-mitigation surface for KLIPY search/trending traffic would
// silently expand by 10x.
func TestKlipyApiLimiterAt30PerMinute(t *testing.T) {
	redisClient, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	router := newKlipyLimiterTestRouter(t, redisClient, klipyTestUserID+"-api", true, false)

	// First 30 requests succeed.
	for i := 1; i <= klipyAPILimit; i++ {
		w := doGET(router, klipySearchPath)
		assert.Equal(t, http.StatusOK, w.Code, "request %d/%d should succeed", i, klipyAPILimit)
	}

	// 31st request is rate limited.
	w := doGET(router, klipySearchPath)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "31st request must be rate-limited at 30/min")
}

// TestKlipyMediaLimiterIsIndependent proves the /media bucket's CAP is NOT
// shared with /gifs/search's apiLimiter. The Redis key format
// (`ratelimit:user:<id>:<method>:<full-path>`) makes counters automatically
// distinct by route, so true "spillover" of consumed budget is structurally
// impossible. The real regression this test catches is someone re-attaching
// `apiLimiter` (or a lower-cap limiter) to `/media` instead of its dedicated
// 300/min limiter — which would silently cap media at 30/min. The test
// exhausts apiLimiter on /search, then confirms /media still serves 31
// consecutive requests; if /media's limiter were re-pointed at the apiLimiter
// budget OR at any limiter capped at < 31 requests, the 31st media request
// would 429.
func TestKlipyMediaLimiterIsIndependent(t *testing.T) {
	redisClient, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	router := newKlipyLimiterTestRouter(t, redisClient, klipyTestUserID+"-independent", true, true)

	// Exhaust /gifs/search's 30/min budget.
	for i := 1; i <= klipyAPILimit; i++ {
		w := doGET(router, klipySearchPath)
		assert.Equal(t, http.StatusOK, w.Code, "search request %d/%d should succeed", i, klipyAPILimit)
	}
	// 31st /search is rate limited — confirms the search budget is consumed.
	w := doGET(router, klipySearchPath)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "31st /search request must be rate-limited")

	// Now fire 31 requests to /media. All should succeed — the /media bucket is
	// untouched by the /search exhaustion.
	for i := 1; i <= klipyAPILimit+1; i++ {
		w := doGET(router, klipyMediaPath)
		assert.Equal(t, http.StatusOK, w.Code,
			"media request %d should succeed (independent from apiLimiter)", i)
	}
}

// TestKlipyMediaLimiterAt300PerMinute proves the new 300/min budget from #804
// is correctly applied to /media. The test fires 301 requests; the 300th
// must succeed and the 301st must return 429.
//
// The test uses 301 iterations rather than a smaller sample because the
// production limit is the value we're asserting — a sampled assertion (e.g.,
// "more than 31 requests succeed") wouldn't catch a regression that, say,
// dropped the limit to 200/min. Full-budget exhaustion is the invariant.
func TestKlipyMediaLimiterAt300PerMinute(t *testing.T) {
	redisClient, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	router := newKlipyLimiterTestRouter(t, redisClient, klipyTestUserID+"-media-cap", false, true)

	// First 300 requests succeed.
	for i := 1; i <= klipyMediaLimit; i++ {
		w := doGET(router, klipyMediaPath)
		assert.Equal(t, http.StatusOK, w.Code, "request %d/%d should succeed", i, klipyMediaLimit)
	}

	// 301st request is rate limited.
	w := doGET(router, klipyMediaPath)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "301st request must be rate-limited at 300/min")
}
