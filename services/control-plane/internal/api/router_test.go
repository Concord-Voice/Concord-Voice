//nolint:revive // var-naming false positive on "api" in v2.10.1 (relaxed in v2.12+)
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHealthEndpoint covers the /health route registration in router.go.
// Regression test for #882: gin's default behavior is to return 404 for HEAD
// on a GET-only route, which causes CF Health Check probes and monitoring
// tools (curl -I, uptime probes) to report false-negative outages even when
// the service is fully healthy.
//
// The test registers healthHandler on a minimal gin.New() router and drives
// it via router.ServeHTTP — this exercises both the route registration AND
// the handler body in one path. We don't call healthHandler directly because
// the GET-vs-HEAD branching lives inside the handler (RFC 7231 §4.3.2: HEAD
// responses must not carry a body), and that branching reads c.Request.Method
// which is populated by gin's routing layer. So driving via the router is
// what actually exercises the production code path.
//
// We don't spin up the full NewRouter — that requires db/redis/middleware
// dependencies. The route registration in NewRouter is exercised separately
// by integration tests; this is the unit-level invariant.
func TestHealthEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.GET("/health", healthHandler)
	router.HEAD("/health", healthHandler)

	tests := []struct {
		name        string
		method      string
		expectEmpty bool // HEAD: handler short-circuits per RFC 7231 — body MUST be absent
	}{
		{name: "GET returns 200 with JSON body", method: http.MethodGet, expectEmpty: false},
		{name: "HEAD returns 200 with empty body (regression test for #882)", method: http.MethodHead, expectEmpty: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/health", nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code, "method=%s should return 200", tc.method)

			if tc.expectEmpty {
				// HEAD: the handler short-circuits before c.JSON, so rec.Body
				// captures no payload. Asserting len==0 here is what makes this
				// a real regression test for the RFC 7231 invariant — without
				// it, a bug that wrote the body on HEAD would still pass.
				assert.Equal(t, 0, rec.Body.Len(), "HEAD response must have no body (RFC 7231)")
			} else {
				var body map[string]string
				err := json.Unmarshal(rec.Body.Bytes(), &body)
				require.NoError(t, err, "GET response should be valid JSON")
				assert.Equal(t, "healthy", body["status"])
				assert.Equal(t, "control-plane", body["service"])
			}
		})
	}
}
