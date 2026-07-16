package opsmetrics_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRequestMetricsMiddlewareCountsStatusClassesAndRouteTemplates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	counters := opsmetrics.NewCounters()
	router := gin.New()
	router.Use(opsmetrics.RequestMetricsMiddleware(counters))
	router.GET("/items/:itemID", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.GET("/invalid", func(c *gin.Context) { c.Status(http.StatusBadRequest) })
	router.GET("/broken", func(c *gin.Context) { c.Status(http.StatusServiceUnavailable) })

	for _, target := range []string{
		"/items/first?user=alice",
		"/items/second?user=bob",
		"/invalid",
		"/broken",
		"/not-registered?token=secret",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		router.ServeHTTP(httptest.NewRecorder(), request)
	}

	snapshot := counters.Snapshot()
	require.Equal(t, float64(5), snapshot[opsmetrics.MetricHTTPRequestsTotal])
	require.Equal(t, float64(2), snapshot[opsmetrics.MetricHTTPClientErrorsTotal])
	require.Equal(t, float64(1), snapshot[opsmetrics.MetricHTTPServerErrorsTotal])

	routes := counters.RouteSnapshot()
	require.Equal(t, uint64(2), routes["/items/:itemID"])
	require.Equal(t, uint64(1), routes["/invalid"])
	require.Equal(t, uint64(1), routes["/broken"])
	require.Equal(t, uint64(1), routes[opsmetrics.UnmatchedRoute])
	require.NotContains(t, routes, "/items/first")
	require.NotContains(t, routes, "user")
	require.NotContains(t, routes, "alice")
	require.NotContains(t, routes, "token")
	require.NotContains(t, routes, "secret")

	for key := range snapshot {
		require.NotContains(t, string(key), "item")
		require.NotContains(t, string(key), "user")
	}
}

func TestRequestMetricsMiddlewareIsNoOpWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(opsmetrics.RequestMetricsMiddleware(nil))
	router.GET("/health", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusNoContent, response.Code)
}
