package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestDeviceRecoveryV2RouterBudgetsAndBearerOnlyCompletion(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	poll := "/api/v1/auth/recovery/device-request/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	// Invalid tokens exercise the real router/limiter without changing a ceremony.
	for i := 0; i < 61; i++ {
		req := httptest.NewRequest("GET", poll, nil)
		req.RemoteAddr = "192.0.2.20:1234"
		req.Header.Set("Authorization", "Bearer invalid")
		out := httptest.NewRecorder()
		ts.Router.ServeHTTP(out, req)
		if i < 60 {
			require.Equal(t, http.StatusUnauthorized, out.Code, out.Body.String())
		} else {
			require.Equal(t, http.StatusTooManyRequests, out.Code)
			require.NotEmpty(t, out.Header().Get("Retry-After"))
		}
	}
	// Creation retains its original three-per-15-minute budget.
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest("POST", "/api/v1/auth/recovery/device-request", strings.NewReader(`{"protocol_version":1}`))
		req.RemoteAddr = "192.0.2.21:1234"
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		ts.Router.ServeHTTP(out, req)
		if i < 3 {
			require.Equal(t, http.StatusBadRequest, out.Code)
			require.Contains(t, out.Body.String(), "Update both devices")
		} else {
			require.Equal(t, http.StatusTooManyRequests, out.Code)
		}
	}
	complete := httptest.NewRequest("POST", poll+"/complete?recovery_token=ignored", strings.NewReader(`{"protocol_version":2,"transcript_hash":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`))
	complete.RemoteAddr = "192.0.2.22:1234"
	complete.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	ts.Router.ServeHTTP(out, complete)
	require.Equal(t, http.StatusUnauthorized, out.Code, out.Body.String())
}
