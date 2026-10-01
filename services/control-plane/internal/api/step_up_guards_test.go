package api

// The password step-up mint's limiters fail CLOSED (#3509 review, security
// L2): a Redis that refuses the count must refuse the mint, never wave it on
// to an Argon2 verification. Each limiter is driven alone, because with both
// mounted the first one's 503 would hide a second one that failed open.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// serveThroughGuard mounts guard alone, after a stand-in for AuthRequired
// that sets the user, and returns the status.
func serveThroughGuard(t *testing.T, guard gin.HandlerFunc) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/auth/step-up/password",
		func(c *gin.Context) { c.Set("user_id", "mint-guard-user"); c.Next() },
		guard,
		func(c *gin.Context) { c.Status(http.StatusOK) },
	)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/auth/step-up/password", nil))
	return w.Code
}

// Mutants killed: the per-IP limiter back to RateLimitByIP, and the per-user
// limiter as RateLimitByUser (each passes the request on a refusing Redis).
func TestStepUpPasswordRouteGuards_LimitersFailClosed(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	guards := stepUpPasswordRouteGuards(rdb)
	require.Len(t, guards, 3, "AuthBanCheck, the per-IP limiter, the per-user limiter")
	limiters := map[string]gin.HandlerFunc{"per-IP limiter": guards[1], "per-user limiter": guards[2]}

	for name, limiter := range limiters {
		require.Equal(t, http.StatusOK, serveThroughGuard(t, limiter), "control: %s passes on a healthy Redis", name)
	}
	mr.SetError("MISCONF Redis is configured to save RDB snapshots")
	for name, limiter := range limiters {
		require.Equal(t, http.StatusServiceUnavailable, serveThroughGuard(t, limiter),
			"%s: a Redis that refuses the count refuses the mint", name)
	}
}
