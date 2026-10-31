package middleware_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// The media plane relays the client's own X-Attestation-Token, X-Session-ID and
// X-Machine-Id on its voice re-validation hop (#4206, ADR-0010 D1 amendment
// 2026-09-02). These tests run that hop through the production gate order and
// pin the two properties the relay depends on: a service-hop proof never
// exempts attestation, and a relayed triple bound to another user is refused.
func relayRouter(rdb *redis.Client, caller string) (*gin.Engine, *bool) {
	r := gin.New()
	reached := new(bool)
	authed := r.Group("/")
	authed.Use(func(c *gin.Context) { c.Set("user_id", caller); c.Next() })
	authed.Use(middleware.MediaPlaneServiceHop(hopSecret, nil))
	authed.Use(middleware.RequireClientVersion(pinnedMinimum))
	authed.Use(middleware.RequireAttestation(true, rdb, logger.NewWithWriter(&bytes.Buffer{})))
	authed.POST(channelJoinTemplate, func(c *gin.Context) { *reached = true; c.Status(http.StatusNoContent) })
	return r, reached
}

func sendRelayedHop(r *gin.Engine, relay map[string]string) *httptest.ResponseRecorder {
	h := freshHop(http.MethodPost, channelJoinPath).signed(hopSecret)
	req := httptest.NewRequest(http.MethodPost, channelJoinPath, nil)
	req.Header.Set("Authorization", "Bearer "+h.bearer)
	req.Header.Set("X-Concord-Service-Timestamp", h.timestamp)
	req.Header.Set("X-Concord-Service-Proof", h.proof)
	for name, value := range relay {
		req.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func attestationCode(t *testing.T, w *httptest.ResponseRecorder) attestation.ErrorCode {
	t.Helper()
	var resp attestation.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Code
}

func TestRelayedHop_WithoutTripleIsRefusedDespiteProof(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()
	r, reached := relayRouter(rdb, "user-a")

	w := sendRelayedHop(r, nil)

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, attestation.ErrMissing, attestationCode(t, w))
	require.False(t, *reached)
}

func TestRelayedHop_CallersOwnTriplePasses(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()
	writeTokenWithUser(t, rdb, "attestation:s-a:m-a", "token-a", "0.2.7", "user-a")
	r, reached := relayRouter(rdb, "user-a")

	w := sendRelayedHop(r, map[string]string{
		"X-Attestation-Token": "token-a", "X-Session-ID": "s-a", "X-Machine-Id": "m-a",
	})

	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	require.True(t, *reached)
}

func TestRelayedHop_AnotherUsersTripleIsRefused(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()
	// user-b's valid attestation, relayed beside user-a's bearer.
	writeTokenWithUser(t, rdb, "attestation:s-b:m-b", "token-b", "0.2.7", "user-b")
	r, reached := relayRouter(rdb, "user-a")

	w := sendRelayedHop(r, map[string]string{
		"X-Attestation-Token": "token-b", "X-Session-ID": "s-b", "X-Machine-Id": "m-b",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, attestation.ErrInvalid, attestationCode(t, w))
	require.False(t, *reached)
}
