package websocket

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestLiveWebSocketDeadline(t *testing.T) {
	ctx := func(value any, set bool) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		if set {
			c.Set(wsDeadlineContextKey, value)
		}
		return c
	}
	future := time.Now().Add(time.Minute)

	got, ok := liveWebSocketDeadline(ctx(future, true))
	assert.True(t, ok)
	assert.Equal(t, future, got)

	_, ok = liveWebSocketDeadline(ctx(nil, false))
	assert.False(t, ok, "no deadline")
	_, ok = liveWebSocketDeadline(ctx("soon", true))
	assert.False(t, ok, "not a time")
	_, ok = liveWebSocketDeadline(ctx(time.Now().Add(-time.Second), true))
	assert.False(t, ok, "expired")
}
