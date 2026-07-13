package opsmetrics

import (
	"sync"
	"sync/atomic"
)

const maxTrackedRoutes = 256

// Counters holds the fixed aggregate counters emitted by the control plane.
type Counters struct {
	httpRequests       atomic.Uint64
	httpClientErrors   atomic.Uint64
	httpServerErrors   atomic.Uint64
	channelMessages    atomic.Uint64
	dmMessages         atomic.Uint64
	snapshotRejections atomic.Uint64

	routesMu sync.RWMutex
	routes   map[string]uint64
}

// NewCounters creates an empty aggregate counter set.
func NewCounters() *Counters {
	return &Counters{routes: make(map[string]uint64)}
}

// Increment adds one to a fixed counter owned by the control plane.
func (c *Counters) Increment(key MetricKey) {
	if c == nil {
		return
	}

	switch key {
	case MetricHTTPRequestsTotal:
		c.httpRequests.Add(1)
	case MetricHTTPClientErrorsTotal:
		c.httpClientErrors.Add(1)
	case MetricHTTPServerErrorsTotal:
		c.httpServerErrors.Add(1)
	case MetricChannelMessagesTotal:
		c.channelMessages.Add(1)
	case MetricDMMessagesTotal:
		c.dmMessages.Add(1)
	case MetricSnapshotRejectionsTotal:
		c.snapshotRejections.Add(1)
	}
}

// Snapshot returns only the fixed aggregate counters that may be persisted.
func (c *Counters) Snapshot() map[MetricKey]float64 {
	if c == nil {
		return nil
	}
	return map[MetricKey]float64{
		MetricHTTPRequestsTotal:       float64(c.httpRequests.Load()),
		MetricHTTPClientErrorsTotal:   float64(c.httpClientErrors.Load()),
		MetricHTTPServerErrorsTotal:   float64(c.httpServerErrors.Load()),
		MetricChannelMessagesTotal:    float64(c.channelMessages.Load()),
		MetricDMMessagesTotal:         float64(c.dmMessages.Load()),
		MetricSnapshotRejectionsTotal: float64(c.snapshotRejections.Load()),
	}
}

func (c *Counters) recordRoute(route string) {
	if c == nil {
		return
	}
	c.routesMu.Lock()
	defer c.routesMu.Unlock()
	if _, exists := c.routes[route]; exists || len(c.routes) < maxTrackedRoutes {
		c.routes[route]++
	}
}

// RouteSnapshot returns a copy of bounded route-template diagnostics.
func (c *Counters) RouteSnapshot() map[string]uint64 {
	if c == nil {
		return nil
	}
	c.routesMu.RLock()
	defer c.routesMu.RUnlock()

	snapshot := make(map[string]uint64, len(c.routes))
	for route, count := range c.routes {
		snapshot[route] = count
	}
	return snapshot
}
