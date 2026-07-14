package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/admin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var adminMetricsRouteTargets = map[string]string{
	"/admin/api/v1/health":          "/admin/api/v1/health",
	"/admin/api/v1/metrics/current": "/admin/api/v1/metrics/current",
	"/admin/api/v1/metrics/series":  "/admin/api/v1/metrics/series?key=host_cpu_percent&window=24h",
	"/admin/api/v1/counters":        "/admin/api/v1/counters",
}

func TestAdminMetricsRoutesAreReflectivelyCompleteAndGated(t *testing.T) {
	engine, _ := registerAdminEngineAndSessions(t)
	methodsByPath := make(map[string][]string)
	for _, route := range engine.Routes() {
		if _, expected := adminMetricsRouteTargets[route.Path]; expected {
			methodsByPath[route.Path] = append(methodsByPath[route.Path], route.Method)
		}
	}

	expectedMethods := []string{
		http.MethodDelete,
		http.MethodGet,
		http.MethodPatch,
		http.MethodPost,
		http.MethodPut,
	}
	for path := range adminMetricsRouteTargets {
		sort.Strings(methodsByPath[path])
		assert.Equal(t, expectedMethods, methodsByPath[path], path)
		assert.False(t, admin.IsPreAuthRoute(path), path)
	}
}

func TestAdminMetricsRoutesRejectMissingSessionAndUserJWT(t *testing.T) {
	engine, _ := registerAdminEngineAndSessions(t)
	for path, target := range adminMetricsRouteTargets {
		t.Run(path, func(t *testing.T) {
			for _, authorization := range []string{"", "Bearer user.jwt.only"} {
				request := httptest.NewRequest(http.MethodGet, target, nil)
				if authorization != "" {
					request.Header.Set("Authorization", authorization)
				}
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				assert.Equal(t, http.StatusUnauthorized, response.Code)
			}
		})
	}
}

func TestAdminMetricsRoutesAllowGETAndDenyMutatingMethods(t *testing.T) {
	engine, sessions := registerAdminEngineAndSessions(t)
	sid, err := sessions.Mint(context.Background(), "admin-route-test")
	require.NoError(t, err)

	for path, target := range adminMetricsRouteTargets {
		t.Run(path, func(t *testing.T) {
			getResponse := serveAdminMetricsRoute(engine, sid, http.MethodGet, target)
			assert.Equal(t, http.StatusOK, getResponse.Code)

			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
				response := serveAdminMetricsRoute(engine, sid, method, target)
				assert.Equal(t, http.StatusMethodNotAllowed, response.Code, method)
				assert.Equal(t, http.MethodGet, response.Header().Get("Allow"))
				var body map[string]any
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				assert.Equal(t, "method_not_allowed", body["error"])
				assert.NotContains(t, body, "message")
			}
		})
	}
}

func TestAdminMetricsRouteRateLimitUsesClosed429Response(t *testing.T) {
	engine, sessions := registerAdminEngineAndSessions(t)
	sid, err := sessions.Mint(context.Background(), "admin-rate-test")
	require.NoError(t, err)
	target := adminMetricsRouteTargets["/admin/api/v1/metrics/current"]

	for requestNumber := 1; requestNumber <= 60; requestNumber++ {
		response := serveAdminMetricsRoute(engine, sid, http.MethodGet, target)
		assert.Equal(t, http.StatusOK, response.Code, "request %d", requestNumber)
	}
	response := serveAdminMetricsRoute(engine, sid, http.MethodGet, target)
	assert.Equal(t, http.StatusTooManyRequests, response.Code)
	assert.NotEmpty(t, response.Header().Get("Retry-After"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, map[string]any{
		"node_id": "cvn_aaaaaaaaaaaaaaaa",
		"error":   "rate_limited",
	}, body)
	assert.NotContains(t, body, "message")
}

func TestAdminMetricsRouteRateLimitFailsClosedWithFixed503Response(t *testing.T) {
	limiterRedis := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = limiterRedis.Close() })

	engine, sessions := registerAdminEngineAndSessionsWithLimiter(t, limiterRedis)
	sid, err := sessions.Mint(context.Background(), "admin-rate-backend-test")
	require.NoError(t, err)

	response := serveAdminMetricsRoute(
		engine,
		sid,
		http.MethodGet,
		adminMetricsRouteTargets["/admin/api/v1/metrics/current"],
	)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, map[string]any{
		"node_id": "cvn_aaaaaaaaaaaaaaaa",
		"error":   "metrics_unavailable",
	}, body)
	assert.NotContains(t, body, "message")
}

func serveAdminMetricsRoute(engine http.Handler, sid, method, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	request.RemoteAddr = "192.0.2.10:4242"
	request.AddCookie(&http.Cookie{
		Name:     "__Host-cv_admin_sid",
		Value:    sid,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func TestAdminMetricsRouteListHasNoUnexpectedPaths(t *testing.T) {
	engine, _ := registerAdminEngineAndSessions(t)
	var paths []string
	for _, route := range engine.Routes() {
		if strings.HasPrefix(route.Path, "/admin/api/v1/metrics") ||
			route.Path == "/admin/api/v1/health" || route.Path == "/admin/api/v1/counters" {
			paths = append(paths, route.Path)
		}
	}
	require.NotEmpty(t, paths)
	for _, path := range paths {
		_, expected := adminMetricsRouteTargets[path]
		assert.True(t, expected, path)
	}
}
