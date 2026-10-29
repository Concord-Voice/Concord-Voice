package middleware_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

type unreadPauseBody struct{ t *testing.T }

func (b unreadPauseBody) Read([]byte) (int, error) {
	b.t.Fatal("pause read request body")
	return 0, io.EOF
}
func (unreadPauseBody) Close() error { return nil }

func TestRecoveryCirclePauseMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			router := gin.New()
			called := false
			router.Handle(method, "/paused", middleware.RecoveryCirclePaused, func(c *gin.Context) {
				called = true
				c.JSON(http.StatusOK, gin.H{"encrypted_share": "must-not-escape"})
			})
			request := httptest.NewRequest(method, "/paused", nil)
			request.Body = unreadPauseBody{t: t}
			request.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, request)
			assert.Equal(t, http.StatusServiceUnavailable, w.Code, "pause must return 503 without reading even a malformed request body")
			assert.JSONEq(t, `{"error":"Recovery Circle recovery is temporarily paused. Use another recovery method.","error_code":"recovery_circle_paused"}`, w.Body.String(), "pause must return exactly the stable two-field error contract")
			assert.Empty(t, w.Header().Get("Retry-After"), "pause must not promise a reopening time")
			assert.False(t, called, "pause must abort downstream handlers and limiters")
		})
	}
}

// This isolated parent-chain fixture enables attestation without constructing an
// unrelated OIDC publish service. Real API auth/version/email composition is
// covered by the API pause integration suite.
func TestRecoveryCirclePausePreservesEnabledAttestationParent(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)
	const userID = "pause-attested-user"
	writeTokenWithUser(t, rdb, "attestation:pause-session:pause-machine", "pause-token", "0.2.7", userID)
	router := gin.New()
	router.GET("/paused", func(c *gin.Context) { c.Set("user_id", userID); c.Next() },
		middleware.RequireAttestation(true, rdb, logger.NewWithWriter(io.Discard)), middleware.RecoveryCirclePaused)
	for _, tc := range []struct {
		name, token, code string
		status            int
	}{
		{"missing", "", "ATTESTATION_MISSING", http.StatusForbidden},
		{"invalid", "invalid-token", "ATTESTATION_INVALID", http.StatusForbidden},
		{"valid reaches pause", "pause-token", "recovery_circle_paused", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/paused", nil)
			req.Header.Set("X-Attestation-Token", tc.token)
			req.Header.Set("X-Session-ID", "pause-session")
			req.Header.Set("X-Machine-Id", "pause-machine")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			assert.Equal(t, tc.status, w.Code, "enabled attestation must retain refusal authority before the pause")
			assert.Contains(t, w.Body.String(), tc.code, "the correct parent or pause contract must remain observable")
			assert.Empty(t, w.Header().Get("Retry-After"))
		})
	}
}
