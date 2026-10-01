package api_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Exercise the production mount with both providers enabled and valid credentials.
// Empty requests keep this check at the deployment boundary, without SSO tokens or state.
func TestNewRouter_SelfHostedSSODeploymentBoundary(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	cfg := &config.Config{
		InstanceType: "self-hosted", Environment: "test", JWTSecret: testhelpers.TestJWTSecret,
		AllowedOrigins: []string{"*"}, NATSUrl: os.Getenv("NATS_URL"),
		MFAEncryptionKey:        "0000000000000000000000000000000000000000000000000000000000000000",
		MFAEncryptionKeyVersion: 1, WebAuthnRPID: "localhost",
		WebAuthnRPOrigins: []string{"http://localhost:3001"},
		GoogleSSO:         config.GoogleSSOConfig{Enabled: true, ClientID: "test-client.apps.googleusercontent.com"},
		AppleSSO: config.AppleSSOConfig{Enabled: true, ClientID: "chat.test.signin", TeamID: "TEAM123ABC",
			KeyID: "KEYID12345", PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})},
	}
	router, hub, natsClient, opsRuntime, permissionEnforcer, _, closePresence, _, _, _, err := api.NewRouter(
		t.Context(), ts.DB, ts.Redis, cfg, nil, logger.NewWithWriter(io.Discard),
		api.RouterDependencies{PresenceHistory: presencehistory.NewService(ts.DB,
			presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{InstanceType: "self-hosted"}), true)},
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		closePresence()
		hub.Shutdown()
		permissionEnforcer.Close()
		if natsClient != nil {
			require.NoError(t, natsClient.Close())
		}
		require.NoError(t, opsRuntime.Stop(context.Background()))
	})

	routes := []struct{ name, method, suffix string }{
		{"initiate", http.MethodGet, ""},
		{"session", http.MethodPost, "/session"},
		{"complete registration", http.MethodPost, "/complete-registration"},
		{"complete link", http.MethodPost, "/complete-link"},
		{"sign client secret", http.MethodPost, "/sign-client-secret"},
	}
	for _, route := range routes {
		for _, provider := range []string{"google", "apple", "unlisted"} {
			t.Run(route.name+"/"+provider, func(t *testing.T) {
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(route.method, "/api/v1/auth/sso/"+provider+route.suffix, nil))
				require.Equal(t, http.StatusForbidden, w.Code)
				assert.JSONEq(t, `{"error_code":"sso_disabled_self_hosted"}`, w.Body.String())
			})
		}
	}
	t.Run("descriptor agrees with boundary", func(t *testing.T) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/server/capabilities", nil))
		require.Equal(t, http.StatusOK, w.Code)
		var body struct {
			Auth struct {
				Providers json.RawMessage `json:"oauthProviders"`
			} `json:"auth"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "[]", string(body.Auth.Providers))
		assert.Equal(t, "no-store, no-cache, must-revalidate, max-age=0", w.Header().Get("Cache-Control"))
		for _, credential := range []string{cfg.GoogleSSO.ClientID, cfg.AppleSSO.ClientID, cfg.AppleSSO.TeamID, cfg.AppleSSO.KeyID, "PRIVATE KEY"} {
			assert.NotContains(t, w.Body.String(), credential)
		}
	})
}
