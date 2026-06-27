package age

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq" // registers the "postgres" sql driver for the closed-DB 503 test
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func doStatusRequest(t *testing.T, h *Handler, setUserID bool) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/age/status", nil)
	if setUserID {
		c.Set("user_id", "11111111-1111-4111-8111-111111111111")
	}
	h.GetStatus(c)
	return w
}

// GetStatus with no authenticated user → 401, before any DB touch (#1763). The nil-db
// newTestHandler is safe here precisely because the guard returns before QueryRowContext.
func TestGetStatus_NoUserID_401(t *testing.T) {
	w := doStatusRequest(t, newTestHandler(), false)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.JSONEq(t, `{"error_code":"unauthenticated"}`, w.Body.String())
}

// GetStatus fails CLOSED on a DB error: a non-ErrNoRows query error → 503, never a 200
// with default-false booleans (which would be a fabricated "unverified" rather than an
// honest "couldn't tell"). A closed *sql.DB makes QueryRowContext error instantly with no
// infra; distinct from sql.ErrNoRows, which is the legitimate {verified:false} path.
func TestGetStatus_DBError_FailsClosed_503(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/x?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close()) // closed handle → every query errors "sql: database is closed"
	h := NewHandler(db, deadRedis(), nil, logger.New("test"))
	w := doStatusRequest(t, h, true)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"error_code":"unavailable"}`, w.Body.String())
}
