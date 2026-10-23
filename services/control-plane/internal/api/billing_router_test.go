package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/subscriptions"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	billingWebhookPath = "/api/v1/billing/stripe/webhook"
	billingPortalPath  = "/api/v1/billing/portal-session"
	billingMePath      = "/api/v1/subscriptions/me"
	billingPlansPath   = "/api/v1/billing/plans"
	billingCheckout    = "/api/v1/billing/checkout-session"
)

// billingRouter builds the production router (api.NewRouter) over the test
// database and Redis, with instanceType and, when withKeys, a Stripe test-mode
// key and webhook secret. The fake values are never real credentials.
func billingRouter(t *testing.T, ts *testhelpers.TestServer, instanceType string, withKeys bool) *gin.Engine {
	t.Helper()
	cfg := &config.Config{
		Environment: "test", InstanceType: instanceType, JWTSecret: testhelpers.TestJWTSecret,
		AllowedOrigins: []string{"*"}, NATSUrl: os.Getenv("NATS_URL"),
		MFAEncryptionKey:        "0000000000000000000000000000000000000000000000000000000000000000",
		MFAEncryptionKeyVersion: 1, WebAuthnRPID: "localhost",
		WebAuthnRPOrigins: []string{"http://localhost:3001"},
	}
	cfg.Billing.SelfHosted = config.IsSelfHostedInstance(instanceType)
	if withKeys {
		cfg.Billing.SecretKey = "sk_test_fake" // pragma: allowlist secret
		cfg.Billing.KeyMode = config.StripeKeyModeTest
		cfg.Billing.WebhookSecret = "whsec_fake" // pragma: allowlist secret
	}
	router, hub, natsClient, opsRuntime, permissionEnforcer, _, closePresence, _, _, _, err := api.NewRouter(
		t.Context(), ts.DB, ts.Redis, cfg, nil, logger.NewWithWriter(io.Discard),
		api.RouterDependencies{PresenceHistory: presencehistory.NewService(ts.DB,
			presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{InstanceType: "saas"}), true)},
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		closePresence()
		hub.Shutdown()
		permissionEnforcer.Close()
		if natsClient != nil {
			_ = natsClient.Close()
		}
		require.NoError(t, opsRuntime.Stop(context.Background()))
	})
	return router
}

func serve(router *gin.Engine, method, path, body string, headers http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header[k] = v
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestRouter_BillingRoutesOnlyWhenConfigured(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	badSignature := http.Header{"Stripe-Signature": {"t=1,v1=00"}, "Content-Type": {"application/json"}}
	jsonHeader := http.Header{"Content-Type": {"application/json"}}

	t.Run("no keys: neither route exists", func(t *testing.T) {
		router := billingRouter(t, ts, config.InstanceTypeSaaS, false)
		assert.Equal(t, http.StatusNotFound, serve(router, http.MethodPost, billingWebhookPath, "{}", badSignature).Code)
		assert.Equal(t, http.StatusNotFound, serve(router, http.MethodPost, billingPortalPath, "{}", jsonHeader).Code)
		assert.Equal(t, http.StatusNotFound, serve(router, http.MethodGet, billingPlansPath, "", nil).Code)
		assert.Equal(t, http.StatusNotFound, serve(router, http.MethodPost, billingCheckout, "{}", jsonHeader).Code)
	})

	t.Run("self-hosted with keys: neither route exists", func(t *testing.T) {
		router := billingRouter(t, ts, config.InstanceTypeSelfHosted, true)
		assert.Equal(t, http.StatusNotFound, serve(router, http.MethodPost, billingWebhookPath, "{}", badSignature).Code)
		assert.Equal(t, http.StatusNotFound, serve(router, http.MethodPost, billingPortalPath, "{}", jsonHeader).Code)
		assert.Equal(t, http.StatusNotFound, serve(router, http.MethodGet, billingPlansPath, "", nil).Code)
		assert.Equal(t, http.StatusNotFound, serve(router, http.MethodPost, billingCheckout, "{}", jsonHeader).Code)
	})

	t.Run("test key: webhook rejects a bad signature, portal needs auth", func(t *testing.T) {
		router := billingRouter(t, ts, config.InstanceTypeSaaS, true)
		assert.Equal(t, http.StatusBadRequest, serve(router, http.MethodPost, billingWebhookPath, "{}", badSignature).Code)
		assert.Equal(t, http.StatusUnauthorized, serve(router, http.MethodPost, billingPortalPath, "{}", jsonHeader).Code)
		assert.Equal(t, http.StatusUnauthorized, serve(router, http.MethodGet, billingPlansPath, "", nil).Code)
		assert.Equal(t, http.StatusUnauthorized, serve(router, http.MethodPost, billingCheckout, "{}", jsonHeader).Code)
	})

	// Buying needs a verified email; the verified-email gate answers before
	// the handler, so no Stripe call is made.
	t.Run("test key: checkout refuses an unverified user", func(t *testing.T) {
		router := billingRouter(t, ts, config.InstanceTypeSaaS, true)
		pending := ts.CreateTestUserUnverified(t, "billing-pending")
		headers := testhelpers.AuthHeaders(pending.AccessToken)
		headers.Set("Content-Type", "application/json")
		w := serve(router, http.MethodPost, billingCheckout, `{"plan":"wing","interval":"year"}`, headers)
		assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	})
}

func TestRouter_SubscriptionsMeCarriesBillingState(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "billing-state")

	tests := []struct {
		name         string
		instanceType string
		withKeys     bool
		want         string
	}{
		{"self-hosted", config.InstanceTypeSelfHosted, true, subscriptions.BillingSelfHosted},
		{"SaaS without keys", config.InstanceTypeSaaS, false, subscriptions.BillingNotConfigured},
		{"SaaS with a test key", config.InstanceTypeSaaS, true, subscriptions.BillingAvailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := billingRouter(t, ts, tc.instanceType, tc.withKeys)
			w := serve(router, http.MethodGet, billingMePath, "", testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var resp map[string]any
			testhelpers.ParseJSON(t, w, &resp)
			assert.Equal(t, tc.want, resp["billing"])
		})
	}
}
