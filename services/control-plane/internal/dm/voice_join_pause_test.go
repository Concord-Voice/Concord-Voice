package dm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/joinpause"
)

// Every DM voice-admission entry point refuses while the legacy-conversion
// pause is set, before it reads or writes anything else.
func TestDMVoiceAdmissionsRefuseWhileJoinsArePaused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, server.Set(joinpause.Key, "1700000000:1700007200"))
	h := &Handler{redis: rdb}

	for name, handle := range map[string]gin.HandlerFunc{
		"join":            h.AuthorizeVoiceJoin,
		"ring":            h.RingDMCall,
		"media-authorize": h.AuthorizeDMVoiceForMediaPlane,
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
		c.Params = gin.Params{{Key: "id", Value: "22222222-3333-4444-8555-666666666666"}}
		c.Set("user_id", "11111111-2222-4333-8444-555555555555")
		handle(c)
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, name)
		require.Contains(t, recorder.Body.String(), joinpause.Code, name)
	}
}
