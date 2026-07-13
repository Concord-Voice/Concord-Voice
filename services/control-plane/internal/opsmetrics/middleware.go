package opsmetrics

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// UnmatchedRoute is the fixed diagnostic bucket for requests without a Gin route.
const UnmatchedRoute = "<unmatched>"

// RequestMetricsMiddleware records aggregate status classes and route templates.
func RequestMetricsMiddleware(counters *Counters) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if counters == nil {
			return
		}

		counters.Increment(MetricHTTPRequestsTotal)
		status := c.Writer.Status()
		switch {
		case status >= http.StatusInternalServerError:
			counters.Increment(MetricHTTPServerErrorsTotal)
		case status >= http.StatusBadRequest:
			counters.Increment(MetricHTTPClientErrorsTotal)
		}

		route := c.FullPath()
		if route == "" {
			route = UnmatchedRoute
		}
		counters.recordRoute(route)
	}
}
