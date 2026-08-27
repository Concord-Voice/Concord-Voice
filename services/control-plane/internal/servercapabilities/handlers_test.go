package servercapabilities_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/servercapabilities"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/gin-gonic/gin"
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

func TestGetCapabilities_ActivityHistorySupportedTrueOrOmitted(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *config.Config
		wantPresent bool
	}{
		{
			name: "validated single replica gate",
			cfg: &config.Config{
				ActivityHistoryClusterEnabled:    true,
				ControlPlaneReplicaCount:         1,
				ControlPlaneReplicaCountExplicit: true,
			},
			wantPresent: true,
		},
		{
			name: "gate disabled",
			cfg: &config.Config{
				ControlPlaneReplicaCount:         1,
				ControlPlaneReplicaCountExplicit: true,
			},
		},
		{
			name: "manual config missing explicit count",
			cfg: &config.Config{
				ActivityHistoryClusterEnabled: true,
				ControlPlaneReplicaCount:      1,
			},
		},
		{
			name: "manual config has wrong count",
			cfg: &config.Config{
				ActivityHistoryClusterEnabled:    true,
				ControlPlaneReplicaCount:         2,
				ControlPlaneReplicaCountExplicit: true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, c := newTestContext()
			servercapabilities.NewHandler(tc.cfg).GetCapabilities(c)

			var raw struct {
				Features map[string]json.RawMessage `json:"features"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
			value, present := raw.Features["activityHistorySupported"]
			assert.Equal(t, tc.wantPresent, present)
			if tc.wantPresent {
				assert.JSONEq(t, "true", string(value))
			}
		})
	}
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

// The chunked attachment upload capability (#2157 PR 2).
//
// This capability is the ONLY evidence the client has that the chunked session
// routes exist. It is not derived from config and it is not a property of the
// build -- the routes compile in unconditionally -- so nothing but the router's
// own wiring can make it true, and nothing but these tests can stop it from
// silently defaulting the wrong way.
func TestGetCapabilities_ChunkedAttachmentUpload_DefaultsFalse(t *testing.T) {
	// Fail-closed. A deployment without object storage registers no media routes
	// at all, so a true default would advertise an endpoint that 404s.
	w, c := newTestContext()
	servercapabilities.NewHandler(&config.Config{InstanceType: "saas"}).GetCapabilities(c)

	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	features, ok := body["features"].(map[string]any)
	require.True(t, ok, "features object missing")

	// Present and false -- NOT absent. An old client that fails closed on a
	// missing key and a new server that means "no" must look the same, but a
	// deployment that means "no" should say so rather than stay silent.
	got, present := features["chunkedAttachmentUpload"]
	require.True(t, present, "capability key absent; the client reads it by this exact name")
	assert.Equal(t, false, got)
}

func TestGetCapabilities_ChunkedAttachmentUpload_ReflectsWiring(t *testing.T) {
	h := servercapabilities.NewHandler(&config.Config{InstanceType: "saas"})
	h.SetChunkedAttachmentUpload(true)

	w, c := newTestContext()
	h.GetCapabilities(c)

	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	features, ok := body["features"].(map[string]any)
	require.True(t, ok, "features object missing")

	// The literal key matters as much as the value: the client reads
	// serverCapabilities.features.chunkedAttachmentUpload and compares it to
	// true, so a renamed field is indistinguishable from an absent capability
	// and downgrades every client to the legacy path in silence.
	assert.Equal(t, true, features["chunkedAttachmentUpload"])
}
