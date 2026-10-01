//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// generateTestApplePEM returns a freshly-generated P-256 ECDSA private key encoded as
// PKCS8 PEM bytes — the wire format Apple's developer portal exports for .p8 files.
// Duplicated from internal/oauth/apple_test.go's generateP256PEM and pkg/config's
// helper; consolidating to a shared internal/testkeys package is tracked as a
// future cleanup.
func generateTestApplePEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// TestBuildOAuthHandler_GoogleSSODisabled verifies the disabled branch returns
// a non-nil handler whose registry is empty — every /sso/:provider lookup
// should 404 with unknown_provider. This is the production posture for any
// deployment that has not opted into Google SSO.
func TestBuildOAuthHandler_GoogleSSODisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := logger.New("test")

	cfg := &config.Config{
		Environment: "test",
		GoogleSSO: config.GoogleSSOConfig{
			Enabled: false,
		},
	}

	// nil DB / nil Redis are tolerated by buildOAuthHandler — it does not
	// dereference them at construction time, only stores them on HandlerDeps.
	// Behavioral assertions about the registry's empty state happen via the
	// public Registry surface in handler.go's Initiate handler tests.
	h := buildOAuthHandler(nil, nil, cfg, nil, log)
	require.NotNil(t, h, "buildOAuthHandler must return a non-nil handler when SSO is disabled")
}

// TestBuildOAuthHandler_GoogleSSOEnabled verifies the enabled branch
// constructs without panic when ClientID + ClientSecret are present. The seed
// RedirectURI is the documented placeholder; the per-request ExchangeParams
// override carries the real loopback URI at exchange time.
//
// We don't try to drive the registry through Initiate here — that would
// require a real Redis. The assertion is structural: enabled + valid creds
// must not panic and must return a handler.
func TestBuildOAuthHandler_GoogleSSOEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := logger.New("test")

	cfg := &config.Config{
		Environment: "test",
		GoogleSSO: config.GoogleSSOConfig{
			Enabled:  true,
			ClientID: "test-client.apps.googleusercontent.com",
			// ClientSecret removed in #975 — server no longer exchanges codes
		},
	}

	require.NotPanics(t, func() {
		h := buildOAuthHandler(nil, nil, cfg, nil, log)
		assert.NotNil(t, h, "enabled SSO must yield a non-nil handler")
	})
}

// TestBuildOAuthHandler_Disabled_DoesNotPanic pins the disabled-branch
// construction. Production main.go always passes a non-nil logger; the
// rationale for keeping a constructor that doesn't dereference log on the
// disabled path is that the wiring helper should fail loudly at the enabled
// path's structural mistakes (missing client ID, etc.) rather than implicitly
// at a nil-log dereference inside an unrelated branch.
//
// The earlier name (TestBuildOAuthHandler_NilLogger_Disabled) was misleading
// — the test passes a non-nil logger.New("test"), not nil. Renamed to reflect
// what it actually asserts.
func TestBuildOAuthHandler_Disabled_DoesNotPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{
		GoogleSSO: config.GoogleSSOConfig{Enabled: false},
		AppleSSO:  config.AppleSSOConfig{Enabled: false},
	}

	require.NotPanics(t, func() {
		_ = buildOAuthHandler(nil, nil, cfg, nil, logger.New("test"))
	})
}

// TestBuildOAuthHandler_AppleSSODisabled mirrors the Google-disabled test:
// when AppleSSO is off, the wiring helper still returns a non-nil handler
// — every /sso/apple route then 404s with unknown_provider, which is the
// production posture for any deployment that has not opted into Apple SSO.
func TestBuildOAuthHandler_AppleSSODisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := logger.New("test")

	cfg := &config.Config{
		Environment: "test",
		GoogleSSO:   config.GoogleSSOConfig{Enabled: false},
		AppleSSO:    config.AppleSSOConfig{Enabled: false},
	}

	h := buildOAuthHandler(nil, nil, cfg, nil, log)
	require.NotNil(t, h, "buildOAuthHandler must return a non-nil handler when Apple SSO is disabled")
}

// TestBuildOAuthHandler_AppleSSOEnabled verifies the Apple-enabled branch
// constructs without panic when all four credentials are present and the
// PrivateKey is a parseable P-256 PEM. Like the Google-enabled test, the
// assertion is structural — registry introspection is not exposed by the
// public Handler surface; behavioral checks happen via Initiate handler
// tests with a real Redis.
func TestBuildOAuthHandler_AppleSSOEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := logger.New("test")

	cfg := &config.Config{
		Environment: "test",
		GoogleSSO:   config.GoogleSSOConfig{Enabled: false},
		AppleSSO: config.AppleSSOConfig{
			Enabled:    true,
			ClientID:   "chat.concordvoice.signin",
			TeamID:     "TEAM123ABC",
			KeyID:      "KEYID12345",
			PrivateKey: generateTestApplePEM(t),
		},
	}

	require.NotPanics(t, func() {
		h := buildOAuthHandler(nil, nil, cfg, nil, log)
		assert.NotNil(t, h, "Apple SSO enabled with valid creds must yield a non-nil handler")
	})
}

// TestBuildOAuthHandler_BothProvidersEnabled verifies that Google and Apple
// can coexist in the same registry. Both blocks register independently;
// neither's failure mode (log.Fatal on construction error) should affect
// the other when both are valid.
func TestBuildOAuthHandler_BothProvidersEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := logger.New("test")

	cfg := &config.Config{
		Environment: "test",
		GoogleSSO: config.GoogleSSOConfig{
			Enabled:  true,
			ClientID: "test-client.apps.googleusercontent.com",
			// ClientSecret removed in #975 — server no longer exchanges codes
		},
		AppleSSO: config.AppleSSOConfig{
			Enabled:    true,
			ClientID:   "chat.concordvoice.signin",
			TeamID:     "TEAM123ABC",
			KeyID:      "KEYID12345",
			PrivateKey: generateTestApplePEM(t),
		},
	}

	require.NotPanics(t, func() {
		h := buildOAuthHandler(nil, nil, cfg, nil, log)
		assert.NotNil(t, h, "both providers enabled must yield a non-nil handler")
	})
}

// TestBuildOAuthHandler_CloudflareKVBridgeEnabled exercises the enabled
// branch of the KV-bridge wiring (#973): a real cfkv client is constructed
// and stored on HandlerDeps without panicking. Behavioral coverage of the
// bridge write itself lives in internal/oauth's Initiate integration tests;
// this pins the construction path.
func TestBuildOAuthHandler_CloudflareKVBridgeEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := logger.New("test")

	cfg := &config.Config{
		Environment: "test",
		CloudflareKVBridge: config.CloudflareKVBridgeConfig{
			Enabled:     true,
			AccountID:   "acct-test",
			NamespaceID: "ns-test",
			APIToken:    "tok-test",
		},
	}

	h := buildOAuthHandler(nil, nil, cfg, nil, log)
	require.NotNil(t, h, "buildOAuthHandler must construct with the KV bridge enabled")
}

// TestBuildOAuthHandler_CloudflareKVBridgeDisabled pins the disabled branch
// (nil putter — Initiate's apple path must no-op, covered behaviorally in
// internal/oauth tests).
func TestBuildOAuthHandler_CloudflareKVBridgeDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := logger.New("test")

	cfg := &config.Config{
		Environment:        "test",
		CloudflareKVBridge: config.CloudflareKVBridgeConfig{Enabled: false},
	}

	h := buildOAuthHandler(nil, nil, cfg, nil, log)
	require.NotNil(t, h)
}

// Calling Initiate without the deployment middleware proves the configured
// registry itself omits hosted providers; a route guard cannot hide a bad registry.
func TestBuildOAuthHandler_DeploymentProviderAvailability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key := generateTestApplePEM(t)
	modes := []struct {
		name, raw  string
		selfHosted bool
	}{
		{"self-hosted", "self-hosted", true},
		{"normalized self-hosted", " \tSELF-HOSTED\n", true},
		{"saas", "saas", false},
		{"empty fallback", "", false},
		{"unknown fallback", "enterprise", false},
	}
	flags := []struct {
		name          string
		google, apple bool
	}{
		{"disabled", false, false}, {"google only", true, false},
		{"apple only", false, true}, {"both enabled", true, true},
	}
	for _, mode := range modes {
		for _, flag := range flags {
			t.Run(mode.name+"/"+flag.name, func(t *testing.T) {
				cfg := &config.Config{InstanceType: mode.raw, Environment: "test",
					GoogleSSO: config.GoogleSSOConfig{Enabled: flag.google, ClientID: "test-client.apps.googleusercontent.com"},
					AppleSSO: config.AppleSSOConfig{Enabled: flag.apple, ClientID: "chat.test.signin",
						TeamID: "TEAM123ABC", KeyID: "KEYID12345", PrivateKey: key},
				}
				h := buildOAuthHandler(nil, nil, cfg, nil, logger.NewWithWriter(io.Discard))
				router := gin.New()
				router.GET("/sso/:provider", h.Initiate)
				for _, provider := range []struct {
					name    string
					enabled bool
				}{{"google", flag.google}, {"apple", flag.apple}} {
					t.Run(provider.name, func(t *testing.T) {
						w := httptest.NewRecorder()
						// No loopback inputs, credentials or state: registered providers
						// reach input validation before any external work.
						router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sso/"+provider.name, nil))
						var body struct {
							ErrorCode string `json:"error_code"`
						}
						require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
						if mode.selfHosted || !provider.enabled {
							assert.Equal(t, http.StatusNotFound, w.Code)
							assert.Equal(t, "unknown_provider", body.ErrorCode)
						} else {
							assert.Equal(t, http.StatusBadRequest, w.Code)
							assert.Equal(t, "invalid_redirect_uri", body.ErrorCode)
						}
					})
				}
			})
		}
	}
}
