package opsmetrics

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	healthStateHealthy  = "healthy"
	healthStateDegraded = "degraded"
	healthStateStopped  = "stopped"
	healthStateUnknown  = "unknown"

	seriesWindow24Hours = "24h"
	seriesWindow7Days   = "7d"

	adminMetricsErrorInvalidQuery     = "invalid_query"
	adminMetricsErrorInvalidMetricKey = "invalid_metric_key"
	adminMetricsErrorInvalidWindow    = "invalid_window"
	adminMetricsErrorInvalidNode      = "invalid_node"
	adminMetricsErrorUnavailable      = "metrics_unavailable"
	adminMetricsErrorRateLimited      = "rate_limited"
	adminMetricsErrorMethodNotAllowed = "method_not_allowed"
	adminMetricsCacheControlHeader    = "Cache-Control"
	adminMetricsCacheControlValue     = "private, no-store"

	adminMetrics24HourBucketLimit = 25
)

var adminSeriesAllowedQueryKeys = map[string]struct{}{
	"key":    {},
	"window": {},
	"node":   {},
}

type adminHealthResponse struct {
	NodeID   string                       `json:"node_id"`
	Services []adminHealthServiceResponse `json:"services"`
}

type adminHealthServiceResponse struct {
	Service   string     `json:"service"`
	State     string     `json:"state"`
	Running   *bool      `json:"running"`
	Healthy   *bool      `json:"healthy"`
	SampledAt *time.Time `json:"sampled_at"`
}

type adminMetricPointResponse struct {
	MetricKey MetricKey `json:"metric_key"`
	Source    Source    `json:"source"`
	Unit      Unit      `json:"unit"`
	Kind      Kind      `json:"kind"`
	Value     float64   `json:"value"`
	SampledAt time.Time `json:"sampled_at"`
}

type adminCurrentResponse struct {
	NodeID  string                     `json:"node_id"`
	Metrics []adminMetricPointResponse `json:"metrics"`
}

type adminCountersResponse struct {
	NodeID   string                     `json:"node_id"`
	Counters []adminMetricPointResponse `json:"counters"`
}

type adminSeriesMetricResponse struct {
	MetricKey MetricKey  `json:"metric_key"`
	Source    Source     `json:"source"`
	Unit      Unit       `json:"unit"`
	Kind      Kind       `json:"kind"`
	Rollup    RollupMode `json:"rollup"`
}

type adminSeriesPointResponse struct {
	BucketStart time.Time `json:"bucket_start"`
	Value       float64   `json:"value"`
	Minimum     float64   `json:"minimum"`
	Maximum     float64   `json:"maximum"`
	SampleCount int       `json:"sample_count"`
}

type adminSeriesResponse struct {
	NodeID        string                     `json:"node_id"`
	Metric        adminSeriesMetricResponse  `json:"metric"`
	Window        string                     `json:"window"`
	BucketSeconds int                        `json:"bucket_seconds"`
	Points        []adminSeriesPointResponse `json:"points"`
}

type adminMetricsErrorResponse struct {
	NodeID *string `json:"node_id"`
	Error  string  `json:"error"`
}

// AdminHandler serves the closed, read-only operations metrics API.
type AdminHandler struct {
	reader             Reader
	nodeID             string
	collectionInterval time.Duration
	log                *logger.Logger
	now                func() time.Time
}

// NewAdminHandler creates an enabled handler when reader and nodeID are both
// present. A nil reader plus empty node creates the fixed unavailable surface
// used when collection is disabled.
func NewAdminHandler(reader Reader, nodeID string, collectionInterval time.Duration, log *logger.Logger) (*AdminHandler, error) {
	if log == nil {
		return nil, errors.New("admin operations metrics logger is required")
	}
	if reader == nil && nodeID == "" {
		return &AdminHandler{log: log, now: time.Now}, nil
	}
	if reader == nil || nodeID == "" {
		return nil, errors.New("admin operations metrics reader and node must be configured together")
	}
	if err := ValidateNodeID(nodeID); err != nil {
		return nil, errors.New("admin operations metrics node is invalid")
	}
	if collectionInterval <= 0 {
		return nil, errors.New("admin operations metrics collection interval must be positive")
	}
	return &AdminHandler{
		reader:             reader,
		nodeID:             nodeID,
		collectionInterval: collectionInterval,
		log:                log,
		now:                time.Now,
	}, nil
}

// Health returns fresh liveness for the fixed seven-service allowlist.
func (handler *AdminHandler) Health(c *gin.Context) {
	if !handler.rejectUnexpectedQuery(c) || !handler.requireAvailable(c) {
		return
	}
	points, err := handler.readLatest(c, "health")
	if err != nil {
		return
	}

	pointsByKey := make(map[MetricKey]Point, len(points))
	for _, point := range points {
		pointsByKey[point.Key] = point
	}
	services := make([]adminHealthServiceResponse, 0, len(concordServiceAllowlist))
	for _, service := range concordServiceAllowlist {
		status, statusErr := adminHealthStatus(service, pointsByKey)
		if statusErr != nil {
			handler.logReadError("health", statusErr)
			handler.writeError(c, http.StatusServiceUnavailable, adminMetricsErrorUnavailable)
			return
		}
		services = append(services, status)
	}
	c.Header(adminMetricsCacheControlHeader, adminMetricsCacheControlValue)
	c.JSON(http.StatusOK, adminHealthResponse{NodeID: handler.nodeID, Services: services})
}

// Current returns all fresh catalogued scalar points.
func (handler *AdminHandler) Current(c *gin.Context) {
	if !handler.rejectUnexpectedQuery(c) || !handler.requireAvailable(c) {
		return
	}
	points, err := handler.readLatest(c, "current")
	if err != nil {
		return
	}
	metrics, err := adminMetricResponses(points, nil)
	if err != nil {
		handler.logReadError("current", err)
		handler.writeError(c, http.StatusServiceUnavailable, adminMetricsErrorUnavailable)
		return
	}
	c.Header(adminMetricsCacheControlHeader, adminMetricsCacheControlValue)
	c.JSON(http.StatusOK, adminCurrentResponse{NodeID: handler.nodeID, Metrics: metrics})
}

// Counters returns only fresh process-lifetime counter points.
func (handler *AdminHandler) Counters(c *gin.Context) {
	if !handler.rejectUnexpectedQuery(c) || !handler.requireAvailable(c) {
		return
	}
	points, err := handler.readLatest(c, "counters")
	if err != nil {
		return
	}
	counterKind := KindCounter
	counters, err := adminMetricResponses(points, &counterKind)
	if err != nil {
		handler.logReadError("counters", err)
		handler.writeError(c, http.StatusServiceUnavailable, adminMetricsErrorUnavailable)
		return
	}
	c.Header(adminMetricsCacheControlHeader, adminMetricsCacheControlValue)
	c.JSON(http.StatusOK, adminCountersResponse{NodeID: handler.nodeID, Counters: counters})
}

// Series returns one bounded UTC-hour series for a catalogued metric.
func (handler *AdminHandler) Series(c *gin.Context) {
	if !handler.requireAvailable(c) {
		return
	}
	key, definition, window, duration, validationError := handler.parseSeriesQuery(c)
	if validationError != "" {
		handler.writeError(c, http.StatusBadRequest, validationError)
		return
	}

	end := handler.now().UTC()
	start := end.Add(-duration)
	buckets, err := handler.reader.Series(c.Request.Context(), handler.nodeID, key, start, end)
	if err != nil {
		handler.logReadError("series", err)
		handler.writeError(c, http.StatusServiceUnavailable, adminMetricsErrorUnavailable)
		return
	}
	bucketLimit := adminMetrics24HourBucketLimit
	if window == seriesWindow7Days {
		bucketLimit = maxSeriesBuckets
	}
	if len(buckets) > bucketLimit {
		handler.logReadError("series", errors.New("reader exceeded series bucket limit"))
		handler.writeError(c, http.StatusServiceUnavailable, adminMetricsErrorUnavailable)
		return
	}

	points := make([]adminSeriesPointResponse, 0, len(buckets))
	for _, bucket := range buckets {
		if err := validateReadBucket(bucket, handler.nodeID, key, start, end); err != nil {
			handler.logReadError("series", err)
			handler.writeError(c, http.StatusServiceUnavailable, adminMetricsErrorUnavailable)
			return
		}
		value := bucket.Average
		if definition.Rollup == RollupLast {
			value = bucket.Last
		}
		points = append(points, adminSeriesPointResponse{
			BucketStart: bucket.BucketStart.UTC(),
			Value:       value,
			Minimum:     bucket.Minimum,
			Maximum:     bucket.Maximum,
			SampleCount: bucket.SampleCount,
		})
	}

	c.Header(adminMetricsCacheControlHeader, adminMetricsCacheControlValue)
	c.JSON(http.StatusOK, adminSeriesResponse{
		NodeID: handler.nodeID,
		Metric: adminSeriesMetricResponse{
			MetricKey: definition.Key,
			Source:    definition.Source,
			Unit:      definition.Unit,
			Kind:      definition.Kind,
			Rollup:    definition.Rollup,
		},
		Window:        window,
		BucketSeconds: int(time.Hour / time.Second),
		Points:        points,
	})
}

// RateLimited emits the privacy-safe fixed response used by route middleware.
func (handler *AdminHandler) RateLimited(c *gin.Context) {
	handler.writeError(c, http.StatusTooManyRequests, adminMetricsErrorRateLimited)
}

// ServiceUnavailable emits the privacy-safe fixed response used when the
// route's fail-closed rate-limit backend cannot evaluate a request.
func (handler *AdminHandler) ServiceUnavailable(c *gin.Context) {
	handler.writeError(c, http.StatusServiceUnavailable, adminMetricsErrorUnavailable)
}

// MethodNotAllowed is the explicit mutating-verb denial handler.
func (handler *AdminHandler) MethodNotAllowed(c *gin.Context) {
	c.Header("Allow", http.MethodGet)
	handler.writeError(c, http.StatusMethodNotAllowed, adminMetricsErrorMethodNotAllowed)
}

func (handler *AdminHandler) readLatest(c *gin.Context, operation string) ([]Point, error) {
	notBefore := handler.now().UTC().Add(-2 * handler.collectionInterval)
	points, err := handler.reader.Latest(c.Request.Context(), handler.nodeID, notBefore)
	if err != nil {
		handler.logReadError(operation, err)
		handler.writeError(c, http.StatusServiceUnavailable, adminMetricsErrorUnavailable)
		return nil, err
	}
	fresh := make([]Point, 0, len(points))
	for _, point := range points {
		if point.SampledAt.Before(notBefore) {
			continue
		}
		if err := validateReadPoint(point, handler.nodeID, notBefore); err != nil {
			handler.logReadError(operation, err)
			handler.writeError(c, http.StatusServiceUnavailable, adminMetricsErrorUnavailable)
			return nil, err
		}
		point.SampledAt = point.SampledAt.UTC()
		fresh = append(fresh, point)
	}
	return fresh, nil
}

func (handler *AdminHandler) parseSeriesQuery(c *gin.Context) (MetricKey, MetricDefinition, string, time.Duration, string) {
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		return "", MetricDefinition{}, "", 0, adminMetricsErrorInvalidQuery
	}
	for key, values := range query {
		if _, allowed := adminSeriesAllowedQueryKeys[key]; !allowed || len(values) != 1 || values[0] == "" {
			return "", MetricDefinition{}, "", 0, adminMetricsErrorInvalidQuery
		}
	}
	if len(query["key"]) != 1 || len(query["window"]) != 1 {
		return "", MetricDefinition{}, "", 0, adminMetricsErrorInvalidQuery
	}

	key := MetricKey(query.Get("key"))
	definition, exists := Definition(key)
	if !exists {
		return "", MetricDefinition{}, "", 0, adminMetricsErrorInvalidMetricKey
	}
	window := query.Get("window")
	var duration time.Duration
	switch window {
	case seriesWindow24Hours:
		duration = 24 * time.Hour
	case seriesWindow7Days:
		duration = 7 * 24 * time.Hour
	default:
		return "", MetricDefinition{}, "", 0, adminMetricsErrorInvalidWindow
	}
	if nodeValues, present := query["node"]; present {
		if len(nodeValues) != 1 || ValidateNodeID(nodeValues[0]) != nil || nodeValues[0] != handler.nodeID {
			return "", MetricDefinition{}, "", 0, adminMetricsErrorInvalidNode
		}
	}
	return key, definition, window, duration, ""
}

func (handler *AdminHandler) rejectUnexpectedQuery(c *gin.Context) bool {
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err == nil && len(query) == 0 {
		return true
	}
	handler.writeError(c, http.StatusBadRequest, adminMetricsErrorInvalidQuery)
	return false
}

func (handler *AdminHandler) requireAvailable(c *gin.Context) bool {
	if handler.reader != nil && handler.nodeID != "" {
		return true
	}
	handler.ServiceUnavailable(c)
	return false
}

func (handler *AdminHandler) writeError(c *gin.Context, status int, code string) {
	var nodeID *string
	if handler.nodeID != "" {
		node := handler.nodeID
		nodeID = &node
	}
	c.Header(adminMetricsCacheControlHeader, adminMetricsCacheControlValue)
	c.AbortWithStatusJSON(status, adminMetricsErrorResponse{NodeID: nodeID, Error: code})
}

func (handler *AdminHandler) logReadError(operation string, err error) {
	handler.log.Error("Admin operations metrics read failed", "operation", operation, "error", err)
}

func adminMetricResponses(points []Point, kind *Kind) ([]adminMetricPointResponse, error) {
	responses := make([]adminMetricPointResponse, 0, len(points))
	for _, point := range points {
		definition, exists := Definition(point.Key)
		if !exists {
			return nil, errors.New("admin operations metrics point has unknown key")
		}
		if kind != nil && definition.Kind != *kind {
			continue
		}
		responses = append(responses, adminMetricPointResponse{
			MetricKey: definition.Key,
			Source:    definition.Source,
			Unit:      definition.Unit,
			Kind:      definition.Kind,
			Value:     point.Value,
			SampledAt: point.SampledAt.UTC(),
		})
	}
	sort.Slice(responses, func(i, j int) bool { return responses[i].MetricKey < responses[j].MetricKey })
	return responses, nil
}

func adminHealthStatus(service concordService, points map[MetricKey]Point) (adminHealthServiceResponse, error) {
	unknown := adminHealthServiceResponse{Service: service.serviceID, State: healthStateUnknown}
	runningPoint, runningPresent := points[service.running]
	healthyPoint, healthyPresent := points[service.healthy]
	if !runningPresent || !healthyPresent {
		return unknown, nil
	}
	running, err := adminLivenessValue(runningPoint.Value)
	if err != nil {
		return adminHealthServiceResponse{}, err
	}
	healthy, err := adminLivenessValue(healthyPoint.Value)
	if err != nil {
		return adminHealthServiceResponse{}, err
	}

	state := healthStateHealthy
	if !running {
		state = healthStateStopped
	} else if !healthy {
		state = healthStateDegraded
	}
	sampledAt := runningPoint.SampledAt.UTC()
	if healthyPoint.SampledAt.Before(sampledAt) {
		sampledAt = healthyPoint.SampledAt.UTC()
	}
	return adminHealthServiceResponse{
		Service:   service.serviceID,
		State:     state,
		Running:   boolPointer(running),
		Healthy:   boolPointer(healthy),
		SampledAt: timePointer(sampledAt),
	}, nil
}

func adminLivenessValue(value float64) (bool, error) {
	switch value {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, errors.New("admin operations metrics liveness value is not boolean")
	}
}

func boolPointer(value bool) *bool { return &value }

func timePointer(value time.Time) *time.Time { return &value }
