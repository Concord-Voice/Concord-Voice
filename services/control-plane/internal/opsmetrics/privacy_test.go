package opsmetrics

import (
	"encoding/json"
	"math"
	"net"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	adminMetricsForbiddenField = regexp.MustCompile(`(?i)^(user_id|email|username|display_name|content|ip|message|.*_url)$`)
	adminMetricsUUIDValue      = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	adminMetricsEmailValue     = regexp.MustCompile(`^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$`)
)

func TestAdminMetricsResponsesStayInsidePrivacyBoundary(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	reader := &fakeAdminMetricsReader{
		latest: []Point{
			{NodeID: nodeID, Key: MetricServiceControlPlaneRunning, Value: 1, SampledAt: now},
			{NodeID: nodeID, Key: MetricServiceControlPlaneHealthy, Value: 1, SampledAt: now},
			{NodeID: nodeID, Key: MetricChannelMessagesTotal, Value: 20, SampledAt: now},
			{NodeID: nodeID, Key: MetricDMMessagesTotal, Value: 10, SampledAt: now},
		},
		series: []Bucket{
			{
				NodeID: nodeID, Key: MetricChannelMessagesTotal,
				BucketStart: now.Add(-time.Hour),
				Minimum:     10, Maximum: 20, Average: 15, Last: 20, SampleCount: 4,
			},
		},
	}
	handler := newAdminMetricsTestHandler(t, reader, nodeID, 15*time.Second, now)
	unavailable := newAdminMetricsTestHandler(t, nil, "", 0, now)

	testCases := []struct {
		name    string
		handler gin.HandlerFunc
		target  string
	}{
		{name: "health", handler: handler.Health, target: "/admin/api/v1/health"},
		{name: "current", handler: handler.Current, target: "/admin/api/v1/metrics/current"},
		{name: "counters", handler: handler.Counters, target: "/admin/api/v1/counters"},
		{name: "series", handler: handler.Series, target: "/admin/api/v1/metrics/series?key=channel_messages_total&window=24h"},
		{name: "invalid query", handler: handler.Current, target: "/admin/api/v1/metrics/current?user_id=550e8400-e29b-41d4-a716-446655440000"},
		{name: "unavailable", handler: unavailable.Current, target: "/admin/api/v1/metrics/current"},
		{name: "rate limited", handler: handler.RateLimited, target: "/admin/api/v1/metrics/current"},
		{name: "limiter unavailable", handler: handler.ServiceUnavailable, target: "/admin/api/v1/metrics/current"},
		{name: "method denied", handler: handler.MethodNotAllowed, target: "/admin/api/v1/metrics/current"},
	}

	allowedStrings := adminMetricsAllowedStrings()
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			response := performAdminMetricsRequest(t, testCase.handler, testCase.target)
			value := decodeAdminMetricsJSON(t, response)
			walkAdminMetricsJSON(t, value, allowedStrings)
		})
	}
}

func TestAdminMetricsPositiveSchemasAreClosed(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	reader := &fakeAdminMetricsReader{
		latest: []Point{
			{NodeID: nodeID, Key: MetricServiceControlPlaneRunning, Value: 1, SampledAt: now},
			{NodeID: nodeID, Key: MetricServiceControlPlaneHealthy, Value: 1, SampledAt: now},
			{NodeID: nodeID, Key: MetricHTTPRequestsTotal, Value: 20, SampledAt: now},
		},
		series: []Bucket{
			{
				NodeID: nodeID, Key: MetricHTTPRequestsTotal,
				BucketStart: now.Add(-time.Hour),
				Minimum:     10, Maximum: 20, Average: 15, Last: 20, SampleCount: 4,
			},
		},
	}
	handler := newAdminMetricsTestHandler(t, reader, nodeID, 15*time.Second, now)

	health := decodeAdminMetricsObject(t, performAdminMetricsRequest(t, handler.Health, "/admin/api/v1/health"))
	assertAdminMetricsObjectKeys(t, health, "node_id", "services")
	for _, service := range health["services"].([]any) {
		assertAdminMetricsObjectKeys(t, service.(map[string]any), "service", "state", "running", "healthy", "sampled_at")
	}

	current := decodeAdminMetricsObject(t, performAdminMetricsRequest(t, handler.Current, "/admin/api/v1/metrics/current"))
	assertAdminMetricsObjectKeys(t, current, "node_id", "metrics")
	for _, metric := range current["metrics"].([]any) {
		assertAdminMetricsObjectKeys(t, metric.(map[string]any), "metric_key", "source", "unit", "kind", "value", "sampled_at")
	}

	counters := decodeAdminMetricsObject(t, performAdminMetricsRequest(t, handler.Counters, "/admin/api/v1/counters"))
	assertAdminMetricsObjectKeys(t, counters, "node_id", "counters")
	for _, counter := range counters["counters"].([]any) {
		assertAdminMetricsObjectKeys(t, counter.(map[string]any), "metric_key", "source", "unit", "kind", "value", "sampled_at")
	}

	series := decodeAdminMetricsObject(t, performAdminMetricsRequest(
		t,
		handler.Series,
		"/admin/api/v1/metrics/series?key=http_requests_total&window=24h",
	))
	assertAdminMetricsObjectKeys(t, series, "node_id", "metric", "window", "bucket_seconds", "points")
	assertAdminMetricsObjectKeys(t, series["metric"].(map[string]any), "metric_key", "source", "unit", "kind", "rollup")
	for _, point := range series["points"].([]any) {
		assertAdminMetricsObjectKeys(t, point.(map[string]any), "bucket_start", "value", "minimum", "maximum", "sample_count")
	}

	errorBody := decodeAdminMetricsObject(t, performAdminMetricsRequest(
		t,
		handler.Current,
		"/admin/api/v1/metrics/current?limit=1",
	))
	assertAdminMetricsObjectKeys(t, errorBody, "node_id", "error")
}

func adminMetricsAllowedStrings() map[string]struct{} {
	allowed := map[string]struct{}{
		"control_plane": {},
		"media_plane":   {},
		"postgres":      {},
		"redis":         {},
		"nats":          {},
		"minio":         {},
		"coturn":        {},

		healthStateHealthy:  {},
		healthStateDegraded: {},
		healthStateStopped:  {},
		healthStateUnknown:  {},

		seriesWindow24Hours: {},
		seriesWindow7Days:   {},

		adminMetricsErrorInvalidQuery:     {},
		adminMetricsErrorInvalidMetricKey: {},
		adminMetricsErrorInvalidWindow:    {},
		adminMetricsErrorInvalidNode:      {},
		adminMetricsErrorUnavailable:      {},
		adminMetricsErrorRateLimited:      {},
		adminMetricsErrorMethodNotAllowed: {},
	}
	for _, definition := range Catalog() {
		allowed[string(definition.Key)] = struct{}{}
		allowed[string(definition.Source)] = struct{}{}
		allowed[string(definition.Unit)] = struct{}{}
		allowed[string(definition.Kind)] = struct{}{}
		allowed[string(definition.Rollup)] = struct{}{}
	}
	return allowed
}

func walkAdminMetricsJSON(t *testing.T, value any, allowedStrings map[string]struct{}) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			assert.NotRegexp(t, adminMetricsForbiddenField, key)
			walkAdminMetricsJSON(t, child, allowedStrings)
		}
	case []any:
		for _, child := range typed {
			walkAdminMetricsJSON(t, child, allowedStrings)
		}
	case string:
		assert.False(t, adminMetricsUUIDValue.MatchString(typed), "UUID-shaped value escaped: %q", typed)
		assert.False(t, adminMetricsEmailValue.MatchString(typed), "email-shaped value escaped: %q", typed)
		assert.Nil(t, net.ParseIP(typed), "IP-shaped value escaped: %q", typed)
		assert.False(t, strings.HasPrefix(typed, "http://") || strings.HasPrefix(typed, "https://"), "URL-shaped value escaped: %q", typed)
		if _, allowed := allowedStrings[typed]; allowed {
			return
		}
		if ValidateNodeID(typed) == nil {
			return
		}
		if _, err := time.Parse(time.RFC3339Nano, typed); err == nil {
			return
		}
		assert.Failf(t, "unexpected string", "string is outside the response allowlist: %q", typed)
	case json.Number:
		number, err := typed.Float64()
		require.NoError(t, err)
		assert.False(t, math.IsNaN(number) || math.IsInf(number, 0))
	case bool, nil:
		return
	default:
		assert.Failf(t, "unexpected JSON type", "%T", typed)
	}
}

func decodeAdminMetricsJSON(t *testing.T, response *httptest.ResponseRecorder) any {
	t.Helper()
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	var value any
	require.NoError(t, decoder.Decode(&value))
	return value
}

func decodeAdminMetricsObject(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	value := decodeAdminMetricsJSON(t, response)
	object, ok := value.(map[string]any)
	require.True(t, ok)
	return object
}

func assertAdminMetricsObjectKeys(t *testing.T, object map[string]any, expected ...string) {
	t.Helper()
	actual := make([]string, 0, len(object))
	for key := range object {
		actual = append(actual, key)
	}
	sort.Strings(actual)
	sort.Strings(expected)
	assert.Equal(t, expected, actual)
}
