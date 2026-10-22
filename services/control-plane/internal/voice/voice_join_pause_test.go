package voice

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/joinpause"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

func pausedVoiceJoin(t *testing.T, mediaHop bool, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, server.Set(joinpause.Key, "1700000000:1700007200"))
	// An unreachable database: anything past the pause check fails fast with
	// a non-pause error instead of touching real state.
	db, err := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/x?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h := &Handler{db: db, redis: rdb, log: logger.New("test")}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "22222222-3333-4444-8555-666666666666"}}
	c.Set("user_id", "11111111-2222-4333-8444-555555555555")
	if mediaHop {
		c.Set("concord_service_hop", true)
	}
	h.AuthorizeJoin(c)
	return recorder
}

func TestAuthorizeJoinRefusesClientAndA1WhilePaused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mediaHop bool
		body     string
	}{
		{"client", false, ""},
		{"media-a1", true, `{"admission_id":"33333333-4444-4555-8666-777777777777","socket_id":"s1","activate":false}`},
	} {
		recorder := pausedVoiceJoin(t, tc.mediaHop, tc.body)
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, tc.name)
		require.Contains(t, recorder.Body.String(), joinpause.Code, tc.name)
	}
}

// A2 completes an admission A1 already passed; the pause must not strand it.
func TestAuthorizeJoinLetsA2ActivationThroughThePause(t *testing.T) {
	recorder := pausedVoiceJoin(t, true,
		`{"admission_id":"33333333-4444-4555-8666-777777777777","socket_id":"s1","activate":true}`)
	require.NotContains(t, recorder.Body.String(), joinpause.Code)
}
