package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVoiceAuthPayload(t *testing.T) {
	t.Run("matching variant passes through", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		value, ok := voiceAuthPayload(c, func() (string, bool) { return "payload", true })
		require.True(t, ok)
		assert.Equal(t, "payload", value)
		assert.False(t, c.Writer.Written())
	})
	t.Run("mismatched variant answers 500", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		_, ok := voiceAuthPayload(c, func() (string, bool) { return "", false })
		require.False(t, ok)
		c.Writer.WriteHeaderNow()
		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	})
}
