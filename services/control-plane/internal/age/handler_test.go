package age

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// deadRedis points at a port with nothing listening, so every command fails fast —
// used to exercise the fail-closed (503) path of the disabled-check without infra.
func deadRedis() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
		MaxRetries:  -1,
	})
}

func newTestHandler() *Handler {
	return NewHandler(nil, deadRedis(), nil, logger.New("test"))
}

func doClaimRequest(t *testing.T, h *Handler, setUserID bool, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/age/claim", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	if setUserID {
		c.Set("user_id", "11111111-1111-4111-8111-111111111111")
	}
	h.SubmitClaim(c)
	return w
}

// Step 1: no authenticated user → 401, before any Redis/DB touch.
func TestSubmitClaim_NoUserID_401(t *testing.T) {
	w := doClaimRequest(t, newTestHandler(), false, `{}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.JSONEq(t, `{"error_code":"unauthenticated"}`, w.Body.String())
}

// Step 2: the disabled-check runs FIRST and fails CLOSED — an unreachable Redis yields
// 503 (not a pass-through), and it happens before body bind/validate/signature verify
// so a malformed/unsigned body can never bypass it.
func TestSubmitClaim_RedisDown_FailsClosed_503(t *testing.T) {
	w := doClaimRequest(t, newTestHandler(), true, `not even json`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"error_code":"unavailable"}`, w.Body.String())
}
