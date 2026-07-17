package opsmetrics

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAdminMetricsReader struct {
	latest      []Point
	latestErr   error
	series      []Bucket
	seriesErr   error
	latestCalls int
	seriesCalls int
	latestNode  string
	latestAfter time.Time
	seriesNode  string
	seriesKey   MetricKey
	seriesStart time.Time
	seriesEnd   time.Time
}

func TestAdminMetricsOpenAPIMetricKeysMatchCatalog(t *testing.T) {
	contents := adminMetricsOpenAPI(t)

	metricSectionStart := strings.Index(contents, "\n    MetricKey:\n")
	require.NotEqual(t, -1, metricSectionStart)
	metricSectionEnd := strings.Index(contents[metricSectionStart:], "\n    CounterMetricKey:\n")
	require.NotEqual(t, -1, metricSectionEnd)
	metricSection := contents[metricSectionStart : metricSectionStart+metricSectionEnd]
	matches := regexp.MustCompile(`(?m)^        - ([a-z0-9_]+)$`).FindAllStringSubmatch(metricSection, -1)

	documented := make(map[MetricKey]struct{}, len(matches))
	for _, match := range matches {
		documented[MetricKey(match[1])] = struct{}{}
	}
	expected := Catalog()
	require.Len(t, documented, len(expected))
	for _, definition := range expected {
		_, exists := documented[definition.Key]
		assert.Truef(t, exists, "metric %q missing from admin OpenAPI", definition.Key)
	}
}

func TestAdminMetricsOpenAPICounterKeysMatchCatalog(t *testing.T) {
	contents := adminMetricsOpenAPI(t)
	counterSectionStart := strings.Index(contents, "\n    CounterMetricKey:\n")
	require.NotEqual(t, -1, counterSectionStart)
	counterSectionEnd := strings.Index(contents[counterSectionStart:], "\n    MetricSource:\n")
	require.NotEqual(t, -1, counterSectionEnd)
	counterSection := contents[counterSectionStart : counterSectionStart+counterSectionEnd]
	matches := regexp.MustCompile(`(?m)^        - ([a-z0-9_]+)$`).FindAllStringSubmatch(counterSection, -1)

	documented := make(map[MetricKey]struct{}, len(matches))
	for _, match := range matches {
		documented[MetricKey(match[1])] = struct{}{}
	}
	expectedCount := 0
	for _, definition := range Catalog() {
		if definition.Kind != KindCounter {
			continue
		}
		expectedCount++
		_, exists := documented[definition.Key]
		assert.Truef(t, exists, "counter metric %q missing from admin OpenAPI", definition.Key)
	}
	require.Len(t, documented, expectedCount)
	assert.Contains(t, contents, "maxItems: 11\n          items:\n            $ref: '#/components/schemas/AdminCounterPoint'")
}

func TestAdminMetricsOpenAPISeriesBoundsMatchHandlers(t *testing.T) {
	contents := adminMetricsOpenAPI(t)
	assert.Contains(t, contents, "AdminSeries24HourResponse:")
	assert.Contains(t, contents, "maxItems: 25")
	assert.Contains(t, contents, "AdminSeries7DayResponse:")
	assert.Contains(t, contents, "maxItems: 169")
}

func adminMetricsOpenAPI(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	path := filepath.Join(filepath.Dir(filename), "..", "..", "..", "..", "docs", "api", "admin-metrics.openapi.yaml")
	contents, err := os.ReadFile(path) // #nosec G304 -- path is derived from this fixed test source file.
	require.NoError(t, err)
	return string(contents)
}

func (reader *fakeAdminMetricsReader) Latest(_ context.Context, nodeID string, notBefore time.Time) ([]Point, error) {
	reader.latestCalls++
	reader.latestNode = nodeID
	reader.latestAfter = notBefore
	return append([]Point(nil), reader.latest...), reader.latestErr
}

func (reader *fakeAdminMetricsReader) Series(_ context.Context, nodeID string, key MetricKey, start, end time.Time) ([]Bucket, error) {
	reader.seriesCalls++
	reader.seriesNode = nodeID
	reader.seriesKey = key
	reader.seriesStart = start
	reader.seriesEnd = end
	return append([]Bucket(nil), reader.series...), reader.seriesErr
}

func TestAdminHealthReturnsStableFreshServiceStates(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	reader := &fakeAdminMetricsReader{latest: []Point{
		{NodeID: nodeID, Key: MetricServiceControlPlaneRunning, Value: 1, SampledAt: now.Add(-5 * time.Second)},
		{NodeID: nodeID, Key: MetricServiceControlPlaneHealthy, Value: 1, SampledAt: now.Add(-4 * time.Second)},
		{NodeID: nodeID, Key: MetricServiceMediaPlaneRunning, Value: 1, SampledAt: now.Add(-5 * time.Second)},
		{NodeID: nodeID, Key: MetricServiceMediaPlaneHealthy, Value: 0, SampledAt: now.Add(-5 * time.Second)},
		{NodeID: nodeID, Key: MetricServicePostgresRunning, Value: 0, SampledAt: now.Add(-5 * time.Second)},
		{NodeID: nodeID, Key: MetricServicePostgresHealthy, Value: 0, SampledAt: now.Add(-5 * time.Second)},
		{NodeID: nodeID, Key: MetricServiceRedisRunning, Value: 1, SampledAt: now.Add(-31 * time.Second)},
		{NodeID: nodeID, Key: MetricServiceRedisHealthy, Value: 1, SampledAt: now.Add(-31 * time.Second)},
	}}
	handler := newAdminMetricsTestHandler(t, reader, nodeID, 15*time.Second, now)

	response := performAdminMetricsRequest(t, handler.Health, "/admin/api/v1/health")
	require.Equal(t, http.StatusOK, response.Code)
	assertAdminMetricsPrivateNoStore(t, response)

	var body adminHealthResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, nodeID, body.NodeID)
	require.Len(t, body.Services, 7)
	assert.Equal(t, "control_plane", body.Services[0].Service)
	assert.Equal(t, healthStateHealthy, body.Services[0].State)
	assert.Equal(t, true, *body.Services[0].Running)
	assert.Equal(t, true, *body.Services[0].Healthy)
	assert.Equal(t, now.Add(-5*time.Second), *body.Services[0].SampledAt)
	assert.Equal(t, "media_plane", body.Services[1].Service)
	assert.Equal(t, healthStateDegraded, body.Services[1].State)
	assert.Equal(t, "postgres", body.Services[2].Service)
	assert.Equal(t, healthStateStopped, body.Services[2].State)
	assert.Equal(t, "redis", body.Services[3].Service)
	assert.Equal(t, healthStateUnknown, body.Services[3].State)
	assert.Nil(t, body.Services[3].Running)
	assert.Nil(t, body.Services[3].Healthy)
	assert.Nil(t, body.Services[3].SampledAt)
	assert.Equal(t, []string{"nats", "minio", "coturn"}, []string{
		body.Services[4].Service,
		body.Services[5].Service,
		body.Services[6].Service,
	})
	assert.Equal(t, now.Add(-30*time.Second), reader.latestAfter)
}

func TestAdminCurrentReturnsFreshSortedCatalogPoints(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	reader := &fakeAdminMetricsReader{latest: []Point{
		{NodeID: nodeID, Key: MetricHTTPRequestsTotal, Value: 20, SampledAt: now.Add(-2 * time.Second)},
		{NodeID: nodeID, Key: MetricHostCPUPercent, Value: 25, SampledAt: now.Add(-3 * time.Second)},
		{NodeID: nodeID, Key: MetricHostMemoryPercent, Value: 50, SampledAt: now.Add(-31 * time.Second)},
	}}
	handler := newAdminMetricsTestHandler(t, reader, nodeID, 15*time.Second, now)

	response := performAdminMetricsRequest(t, handler.Current, "/admin/api/v1/metrics/current")
	require.Equal(t, http.StatusOK, response.Code)
	assertAdminMetricsPrivateNoStore(t, response)

	var body adminCurrentResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, nodeID, body.NodeID)
	require.Len(t, body.Metrics, 2)
	assert.Equal(t, MetricHostCPUPercent, body.Metrics[0].MetricKey)
	assert.Equal(t, SourceHost, body.Metrics[0].Source)
	assert.Equal(t, UnitPercent, body.Metrics[0].Unit)
	assert.Equal(t, KindGauge, body.Metrics[0].Kind)
	assert.Equal(t, 25.0, body.Metrics[0].Value)
	assert.Equal(t, MetricHTTPRequestsTotal, body.Metrics[1].MetricKey)
}

func TestAdminCountersReturnsOnlyProcessLifetimeCounters(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	reader := &fakeAdminMetricsReader{latest: []Point{
		{NodeID: nodeID, Key: MetricWebSocketConnections, Value: 3, SampledAt: now},
		{NodeID: nodeID, Key: MetricHTTPRequestsTotal, Value: 20, SampledAt: now},
		{NodeID: nodeID, Key: MetricMediaEgressCumulativeBytes, Value: 1000, SampledAt: now},
	}}
	handler := newAdminMetricsTestHandler(t, reader, nodeID, 15*time.Second, now)

	response := performAdminMetricsRequest(t, handler.Counters, "/admin/api/v1/counters")
	require.Equal(t, http.StatusOK, response.Code)
	assertAdminMetricsPrivateNoStore(t, response)

	var body adminCountersResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(t, body.Counters, 2)
	assert.Equal(t, MetricHTTPRequestsTotal, body.Counters[0].MetricKey)
	assert.Equal(t, KindCounter, body.Counters[0].Kind)
	assert.Equal(t, MetricMediaEgressCumulativeBytes, body.Counters[1].MetricKey)
}

func TestAdminSeriesUsesCatalogRollupAndServerBounds(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 37, 0, 0, time.UTC)
	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	reader := &fakeAdminMetricsReader{series: []Bucket{
		{
			NodeID: nodeID, Key: MetricHostCPUPercent,
			BucketStart: time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC),
			Minimum:     10, Maximum: 30, Average: 20, Last: 25, SampleCount: 4,
		},
	}}
	handler := newAdminMetricsTestHandler(t, reader, nodeID, 15*time.Second, now)

	response := performAdminMetricsRequest(
		t,
		handler.Series,
		"/admin/api/v1/metrics/series?key=host_cpu_percent&window=24h&node="+nodeID,
	)
	require.Equal(t, http.StatusOK, response.Code)
	assertAdminMetricsPrivateNoStore(t, response)

	var body adminSeriesResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, nodeID, body.NodeID)
	assert.Equal(t, MetricHostCPUPercent, body.Metric.MetricKey)
	assert.Equal(t, RollupAverage, body.Metric.Rollup)
	assert.Equal(t, seriesWindow24Hours, body.Window)
	assert.Equal(t, 3600, body.BucketSeconds)
	require.Len(t, body.Points, 1)
	assert.Equal(t, 20.0, body.Points[0].Value)
	assert.Equal(t, 10.0, body.Points[0].Minimum)
	assert.Equal(t, 30.0, body.Points[0].Maximum)
	assert.Equal(t, 4, body.Points[0].SampleCount)
	assert.Equal(t, now.Add(-24*time.Hour), reader.seriesStart)
	assert.Equal(t, now, reader.seriesEnd)
	assert.Equal(t, nodeID, reader.seriesNode)
	assert.Equal(t, MetricHostCPUPercent, reader.seriesKey)
}

func TestAdminSeriesUsesLastValueForLastRollup(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 37, 0, 0, time.UTC)
	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	reader := &fakeAdminMetricsReader{series: []Bucket{
		{
			NodeID: nodeID, Key: MetricHTTPRequestsTotal,
			BucketStart: time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC),
			Minimum:     10, Maximum: 30, Average: 20, Last: 25, SampleCount: 4,
		},
	}}
	handler := newAdminMetricsTestHandler(t, reader, nodeID, 15*time.Second, now)

	response := performAdminMetricsRequest(
		t,
		handler.Series,
		"/admin/api/v1/metrics/series?key=http_requests_total&window=7d",
	)
	require.Equal(t, http.StatusOK, response.Code)

	var body adminSeriesResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(t, body.Points, 1)
	assert.Equal(t, 25.0, body.Points[0].Value)
	assert.Equal(t, RollupLast, body.Metric.Rollup)
	assert.Equal(t, seriesWindow7Days, body.Window)
	assert.Equal(t, now.Add(-7*24*time.Hour), reader.seriesStart)
}

func TestAdminMetricsUnavailableAndReaderErrorsAreFixed503(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	unavailable := newAdminMetricsTestHandler(t, nil, "", 0, now)
	response := performAdminMetricsRequest(t, unavailable.Current, "/admin/api/v1/metrics/current")
	assertAdminMetricsError(t, response, http.StatusServiceUnavailable, nil, adminMetricsErrorUnavailable)

	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	reader := &fakeAdminMetricsReader{latestErr: errors.New("database detail must not escape")}
	handler := newAdminMetricsTestHandler(t, reader, nodeID, 15*time.Second, now)
	response = performAdminMetricsRequest(t, handler.Current, "/admin/api/v1/metrics/current")
	assertAdminMetricsError(t, response, http.StatusServiceUnavailable, &nodeID, adminMetricsErrorUnavailable)
	assert.NotContains(t, response.Body.String(), "database detail")
}

func TestAdminRateLimitedAndMethodNotAllowedAreFixedErrors(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	handler := newAdminMetricsTestHandler(t, &fakeAdminMetricsReader{}, nodeID, 15*time.Second, now)

	for _, testCase := range []struct {
		name        string
		handler     gin.HandlerFunc
		status      int
		errorCode   string
		allowHeader string
	}{
		{name: "rate limited", handler: handler.RateLimited, status: http.StatusTooManyRequests, errorCode: adminMetricsErrorRateLimited},
		{name: "service unavailable", handler: handler.ServiceUnavailable, status: http.StatusServiceUnavailable, errorCode: adminMetricsErrorUnavailable},
		{name: "method not allowed", handler: handler.MethodNotAllowed, status: http.StatusMethodNotAllowed, errorCode: adminMetricsErrorMethodNotAllowed, allowHeader: http.MethodGet},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := performAdminMetricsRequest(t, testCase.handler, "/admin/api/v1/health")
			assertAdminMetricsError(t, response, testCase.status, &nodeID, testCase.errorCode)
			assert.Equal(t, testCase.allowHeader, response.Header().Get("Allow"))
		})
	}
}

func TestAdminNoQueryEndpointsRejectEveryQueryParameter(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	for _, testCase := range []struct {
		name    string
		target  string
		handler func(*AdminHandler) gin.HandlerFunc
	}{
		{name: "health user identifier", target: "/admin/api/v1/health?user_id=550e8400-e29b-41d4-a716-446655440000", handler: func(handler *AdminHandler) gin.HandlerFunc { return handler.Health }},
		{name: "current pagination", target: "/admin/api/v1/metrics/current?limit=1000000", handler: func(handler *AdminHandler) gin.HandlerFunc { return handler.Current }},
		{name: "counters cursor", target: "/admin/api/v1/counters?cursor=opaque", handler: func(handler *AdminHandler) gin.HandlerFunc { return handler.Counters }},
		{name: "health malformed percent escape", target: "/admin/api/v1/health?user_id=%ZZ", handler: func(handler *AdminHandler) gin.HandlerFunc { return handler.Health }},
		{name: "current semicolon delimiter", target: "/admin/api/v1/metrics/current?user_id=hidden;limit=1", handler: func(handler *AdminHandler) gin.HandlerFunc { return handler.Current }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reader := &fakeAdminMetricsReader{}
			handler := newAdminMetricsTestHandler(t, reader, nodeID, 15*time.Second, now)
			response := performAdminMetricsRequest(t, testCase.handler(handler), testCase.target)
			assertAdminMetricsError(t, response, http.StatusBadRequest, &nodeID, adminMetricsErrorInvalidQuery)
			assert.Zero(t, reader.latestCalls)
			assert.NotContains(t, response.Body.String(), "550e8400")
			assert.NotContains(t, response.Body.String(), "opaque")
		})
	}
}

func TestAdminSeriesRejectsInvalidOrAbusiveQueries(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	nodeID := "cvn_aaaaaaaaaaaaaaaa"
	for _, testCase := range []struct {
		name      string
		query     string
		errorCode string
	}{
		{name: "missing key", query: "window=24h", errorCode: adminMetricsErrorInvalidQuery},
		{name: "missing window", query: "key=host_cpu_percent", errorCode: adminMetricsErrorInvalidQuery},
		{name: "empty key", query: "key=&window=24h", errorCode: adminMetricsErrorInvalidQuery},
		{name: "unknown key", query: "key=custom_metric&window=24h", errorCode: adminMetricsErrorInvalidMetricKey},
		{name: "invalid window", query: "key=host_cpu_percent&window=30d", errorCode: adminMetricsErrorInvalidWindow},
		{name: "duplicate key", query: "key=host_cpu_percent&key=host_memory_percent&window=24h", errorCode: adminMetricsErrorInvalidQuery},
		{name: "empty node", query: "key=host_cpu_percent&window=24h&node=", errorCode: adminMetricsErrorInvalidQuery},
		{name: "invalid node", query: "key=host_cpu_percent&window=24h&node=api.concordvoice.chat", errorCode: adminMetricsErrorInvalidNode},
		{name: "mismatched node", query: "key=host_cpu_percent&window=24h&node=cvn_bbbbbbbbbbbbbbbb", errorCode: adminMetricsErrorInvalidNode},
		{name: "user identifier", query: "key=host_cpu_percent&window=24h&user_id=550e8400-e29b-41d4-a716-446655440000", errorCode: adminMetricsErrorInvalidQuery},
		{name: "malformed user identifier percent escape", query: "key=host_cpu_percent&window=24h&user_id=%ZZ", errorCode: adminMetricsErrorInvalidQuery},
		{name: "semicolon delimited user identifier", query: "key=host_cpu_percent&window=24h&user_id=hidden;page=1", errorCode: adminMetricsErrorInvalidQuery},
		{name: "page", query: "key=host_cpu_percent&window=24h&page=1", errorCode: adminMetricsErrorInvalidQuery},
		{name: "limit", query: "key=host_cpu_percent&window=24h&limit=1000000", errorCode: adminMetricsErrorInvalidQuery},
		{name: "offset", query: "key=host_cpu_percent&window=24h&offset=1", errorCode: adminMetricsErrorInvalidQuery},
		{name: "cursor", query: "key=host_cpu_percent&window=24h&cursor=opaque", errorCode: adminMetricsErrorInvalidQuery},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reader := &fakeAdminMetricsReader{}
			handler := newAdminMetricsTestHandler(t, reader, nodeID, 15*time.Second, now)
			response := performAdminMetricsRequest(t, handler.Series, "/admin/api/v1/metrics/series?"+testCase.query)
			assertAdminMetricsError(t, response, http.StatusBadRequest, &nodeID, testCase.errorCode)
			assert.Zero(t, reader.seriesCalls)
			assert.NotContains(t, response.Body.String(), "550e8400")
			assert.NotContains(t, response.Body.String(), "api.concordvoice.chat")
		})
	}
}

func TestNewAdminHandlerRejectsPartialOrInvalidConfiguration(t *testing.T) {
	log := logger.NewWithWriter(io.Discard)
	reader := &fakeAdminMetricsReader{}
	for _, testCase := range []struct {
		name     string
		reader   Reader
		nodeID   string
		interval time.Duration
		log      *logger.Logger
	}{
		{name: "reader without node", reader: reader, interval: 15 * time.Second, log: log},
		{name: "node without reader", nodeID: "cvn_aaaaaaaaaaaaaaaa", interval: 15 * time.Second, log: log},
		{name: "invalid node", reader: reader, nodeID: "node-a", interval: 15 * time.Second, log: log},
		{name: "empty interval", reader: reader, nodeID: "cvn_aaaaaaaaaaaaaaaa", log: log},
		{name: "nil logger", reader: reader, nodeID: "cvn_aaaaaaaaaaaaaaaa", interval: 15 * time.Second},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			handler, err := NewAdminHandler(testCase.reader, testCase.nodeID, testCase.interval, testCase.log)
			assert.Nil(t, handler)
			assert.Error(t, err)
		})
	}
}

func newAdminMetricsTestHandler(
	t *testing.T,
	reader Reader,
	nodeID string,
	interval time.Duration,
	now time.Time,
) *AdminHandler {
	t.Helper()
	handler, err := NewAdminHandler(reader, nodeID, interval, logger.NewWithWriter(io.Discard))
	require.NoError(t, err)
	handler.now = func() time.Time { return now }
	return handler
}

func performAdminMetricsRequest(t *testing.T, handler gin.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/admin/api/v1/health", handler)
	router.GET("/admin/api/v1/metrics/current", handler)
	router.GET("/admin/api/v1/counters", handler)
	router.GET("/admin/api/v1/metrics/series", handler)

	request := httptest.NewRequest(http.MethodGet, target, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func assertAdminMetricsError(
	t *testing.T,
	response *httptest.ResponseRecorder,
	status int,
	nodeID *string,
	code string,
) {
	t.Helper()
	assert.Equal(t, status, response.Code)
	assertAdminMetricsPrivateNoStore(t, response)
	var body adminMetricsErrorResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, nodeID, body.NodeID)
	assert.Equal(t, code, body.Error)
}

func assertAdminMetricsPrivateNoStore(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, "private, no-store", response.Header().Get("Cache-Control"))
}
