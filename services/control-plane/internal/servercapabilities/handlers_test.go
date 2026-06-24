package servercapabilities_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/servercapabilities"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestContext() (*httptest.ResponseRecorder, *gin.Context) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/server/capabilities", nil)
	return w, c
}

func TestGetCapabilities_SaaS(t *testing.T) {
	cfg := &config.Config{
		InstanceType:  "saas",
		ServerVersion: "0.2.0-Beta",
		SMTPHost:      "smtp.example.com",
		WebAuthnRPID:  "concordvoice.chat",
	}
	cfg.GoogleSSO.Enabled = true
	cfg.AppleSSO.Enabled = true

	w, c := newTestContext()
	servercapabilities.NewHandler(cfg).GetCapabilities(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "public, max-age=300", w.Header().Get("Cache-Control"))

	var resp servercapabilities.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "Concord Voice", resp.Server.Name)
	assert.Equal(t, "0.2.0-Beta", resp.Server.Version)
	assert.Equal(t, "saas", resp.Server.InstanceType)
	assert.True(t, resp.Auth.EmailVerificationRequired)
	assert.Equal(t, []string{"totp", "webauthn"}, resp.Auth.MFAMethods)
	assert.Equal(t, []string{"google", "apple"}, resp.Auth.OAuthProviders)
	assert.True(t, resp.Features.VoiceTiersSupported)
	assert.True(t, resp.Features.E2EEEnforcedEverywhere)
	assert.Equal(t, "saas", resp.Features.EntitlementMode)
}

func TestGetCapabilities_SelfHosted(t *testing.T) {
	cfg := &config.Config{InstanceType: "self-hosted"} // no SMTP/SSO/WebAuthn

	w, c := newTestContext()
	servercapabilities.NewHandler(cfg).GetCapabilities(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp servercapabilities.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "self-hosted", resp.Server.InstanceType)
	assert.Equal(t, "dev", resp.Server.Version) // zero-value guard
	assert.True(t, resp.Auth.EmailVerificationRequired,
		"email verification is structurally required regardless of SMTP")
	assert.Equal(t, []string{"totp"}, resp.Auth.MFAMethods)
	assert.Equal(t, []string{}, resp.Auth.OAuthProviders)
	assert.False(t, resp.Features.VoiceTiersSupported)
	assert.Equal(t, "self-hosted-unlocked", resp.Features.EntitlementMode)
}

func TestGetCapabilities_UnknownInstanceTypeFailsSafeToSaaS(t *testing.T) {
	cfg := &config.Config{InstanceType: "bogus"}
	w, c := newTestContext()
	servercapabilities.NewHandler(cfg).GetCapabilities(c)

	var resp servercapabilities.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "saas", resp.Server.InstanceType)
	assert.Equal(t, "saas", resp.Features.EntitlementMode)
}

func TestGetCapabilities_InstanceTypeCaseAndWhitespaceTolerant(t *testing.T) {
	// An operator's casing/whitespace typo on the unlock seam must still unlock
	// self-hosted rather than silently degrading to SaaS.
	for _, raw := range []string{"Self-Hosted", "SELF-HOSTED", " self-hosted ", "\tself-hosted\n"} {
		cfg := &config.Config{InstanceType: raw}
		w, c := newTestContext()
		servercapabilities.NewHandler(cfg).GetCapabilities(c)

		var resp servercapabilities.Response
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, "self-hosted", resp.Server.InstanceType, "raw=%q", raw)
		assert.Equal(t, "self-hosted-unlocked", resp.Features.EntitlementMode, "raw=%q", raw)
	}
}

func TestGetCapabilities_PartialOAuth_GoogleOnly(t *testing.T) {
	// The most likely real self-hosted-with-one-provider config — exercises the
	// individual SSO branch arms (google enabled, apple disabled).
	cfg := &config.Config{InstanceType: "saas"}
	cfg.GoogleSSO.Enabled = true
	cfg.AppleSSO.Enabled = false

	w, c := newTestContext()
	servercapabilities.NewHandler(cfg).GetCapabilities(c)

	var resp servercapabilities.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, []string{"google"}, resp.Auth.OAuthProviders)
}

func TestGetCapabilities_EmptyArraysMarshalNotNull(t *testing.T) {
	cfg := &config.Config{InstanceType: "self-hosted"}
	w, c := newTestContext()
	servercapabilities.NewHandler(cfg).GetCapabilities(c)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	var auth map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["auth"], &auth))
	assert.Equal(t, "[]", string(auth["oauthProviders"]), "must be [] not null")
}

// Route-level integration tests: prove the public route is registered on the
// real router and that its shape does not depend on auth state (#662 AC).

func TestServerCapabilitiesEndpoint_NoAuthReturnsShape(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	w := ts.DoRequest("GET", "/api/v1/server/capabilities", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	require.Contains(t, resp, "server")
	require.Contains(t, resp, "auth")
	require.Contains(t, resp, "features")
	require.Contains(t, resp, "policyVersion")
}

func TestServerCapabilitiesEndpoint_AuthStateIndependent(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	noAuth := ts.DoRequest("GET", "/api/v1/server/capabilities", nil, nil)
	withAuth := ts.DoRequest("GET", "/api/v1/server/capabilities", nil,
		http.Header{"Authorization": []string{"Bearer any-token"}})

	require.Equal(t, http.StatusOK, noAuth.Code)
	require.Equal(t, http.StatusOK, withAuth.Code)
	assert.Equal(t, noAuth.Body.String(), withAuth.Body.String(),
		"capabilities shape must not depend on auth state (#662 AC)")
}
