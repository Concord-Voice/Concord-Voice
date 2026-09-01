package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublicMediaRateLimitExceededResponsesAreNotStored(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	router, hub, natsClient, opsRuntime, permissionEnforcer, _, closePresence, _, _, err := api.NewRouter(
		ts.DB,
		ts.Redis,
		&stubAvatarStore{},
		&config.Config{
			Environment:             "test",
			JWTSecret:               testhelpers.TestJWTSecret,
			AllowedOrigins:          []string{"*"},
			MFAEncryptionKey:        "0000000000000000000000000000000000000000000000000000000000000000",
			MFAEncryptionKeyVersion: 1,
			WebAuthnRPID:            "localhost",
			WebAuthnRPOrigins:       []string{"http://localhost:3001"},
		},
		nil,
		logger.NewWithWriter(io.Discard),
		api.RouterDependencies{PresenceHistory: presencehistory.NewService(
			ts.DB,
			presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{InstanceType: "saas"}),
			true,
		)},
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		closePresence()
		hub.Shutdown()
		permissionEnforcer.Close()
		if natsClient != nil {
			natsClient.Close()
		}
		require.NoError(t, opsRuntime.Stop(context.Background()))
	})

	tests := []struct {
		name         string
		path         string
		rateLimitKey string
		limit        int
	}{
		{
			name:         "friend-code avatar",
			path:         "/api/v1/friends/codes/ZZZZZZZZ/avatar",
			rateLimitKey: "/api/v1/friends/codes/:code/avatar",
			limit:        60,
		},
		{
			name:         "avatar proxy",
			path:         "/api/v1/media/avatars/00000000-0000-0000-0000-000000000000",
			rateLimitKey: "/api/v1/media/avatars/:user_id",
			limit:        120,
		},
		{
			name:         "banner proxy",
			path:         "/api/v1/media/banners/00000000-0000-0000-0000-000000000000",
			rateLimitKey: "/api/v1/media/banners/:user_id",
			limit:        120,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Seed the production limiter's private key at its cap so the next
			// request exercises the actual route middleware without invoking the
			// media handler on every allowed request first.
			key := "ratelimit:ip:192.0.2.1:GET:" + tc.rateLimitKey
			require.NoError(t, ts.Redis.Set(context.Background(), key, tc.limit, time.Minute).Err())

			recording := httptest.NewRecorder()
			router.ServeHTTP(recording, httptest.NewRequest(http.MethodGet, tc.path, nil))
			require.Equal(t, http.StatusTooManyRequests, recording.Code)
			assert.Contains(t, recording.Body.String(), "Rate limit exceeded")
			assert.Equal(t, "no-store", recording.Header().Get("Cache-Control"))
		})
	}
}
