package middleware_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestLoggerLogsRoutePatternNotBearerCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	router := gin.New()
	router.Use(middleware.RequestID(), middleware.Logger(logger.NewWithWriter(&buf)))
	router.GET("/invites/:code/preview", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/invites/BEARERCODE123/preview", nil)
	req.Header.Set(middleware.RequestIDHeader, "req-abc")
	router.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	assert.Contains(t, out, "path=/invites/:code/preview")
	assert.NotContains(t, out, "BEARERCODE123", "the concrete path carries the bearer code (CWE-532)")
	assert.Contains(t, out, "method=GET")
	assert.Contains(t, out, "status=200")
	assert.Contains(t, out, "request_id=req-abc")
}

// An unmatched request is not a refused one: CORS answers every preflight 204
// before any route matches, and the desktop client preflights each invite and
// friend-code call, so the raw path there carries a code in use.
func TestLoggerNeverLogsAnUnmatchedPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	router := gin.New()
	router.Use(
		middleware.RequestID(),
		middleware.Logger(logger.NewWithWriter(&buf)),
		middleware.CORS([]string{"https://app.example"}),
	)
	router.GET("/invites/:code/preview", func(c *gin.Context) { c.Status(http.StatusOK) })

	preflight := httptest.NewRequest(http.MethodOptions, "/invites/PREFLIGHTCODE/preview", nil)
	preflight.Header.Set("Origin", "https://app.example")
	router.ServeHTTP(httptest.NewRecorder(), preflight)
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodHead, "/invites/HEADCODE/icon", nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope/MISSCODE", nil))

	out := buf.String()
	assert.Contains(t, out, "method=OPTIONS path=<unmatched> status=204")
	assert.Contains(t, out, "method=HEAD path=<unmatched> status=404")
	assert.Contains(t, out, "method=GET path=<unmatched> status=404")
	for _, code := range []string{"PREFLIGHTCODE", "HEADCODE", "MISSCODE"} {
		assert.NotContains(t, out, code)
	}
}
