package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

// isUserDisabled must FAIL CLOSED: an unreachable Redis is treated as "disabled"
// (mirrors isTokenBlacklisted). The full AuthRequired path, the present-key 403, and
// RebuildDisabledDenylist are exercised against a real Redis/DB in the age integration suite.
func TestIsUserDisabled_RedisDown_FailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	// Redis pointed at a closed port so every command fails — no infra needed.
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1,
	})
	defer func() { _ = rdb.Close() }()

	assert.True(t, isUserDisabled(c, rdb, "11111111-1111-4111-8111-111111111111"),
		"unreachable Redis must fail closed (treat as disabled)")
}
