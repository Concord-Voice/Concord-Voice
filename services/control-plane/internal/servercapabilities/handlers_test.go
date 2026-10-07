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
	assert.Equal(t, "no-store, no-cache, must-revalidate, max-age=0",
		w.Header().Get("Cache-Control"))
	assert.Equal(t, "no-cache", w.Header().Get("Pragma"))

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
	assert.Equal(t, "2026-10-01", resp.PolicyVersion)
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
	assert.Equal(t, "2026-10-01", resp.PolicyVersion)
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
	// SaaS preserves the enabled Google-only subset.
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

func TestGetCapabilities_AttachmentEnvelopeVersions_ReaderFloor(t *testing.T) {
	tests := []struct {
		name        string
		wired       bool
		readerFloor string
		want        []int
		wantPresent bool
	}{
		{
			name:        "unwired omits versions even with reader floor",
			readerFloor: "0.2.44",
		},
		{
			name:        "wired empty floor remains v2 only",
			wired:       true,
			want:        []int{2},
			wantPresent: true,
		},
		{
			name:        "wired older floor remains v2 only",
			wired:       true,
			readerFloor: "0.2.43",
			want:        []int{2},
			wantPresent: true,
		},
		{
			name:        "wired malformed floor remains v2 only",
			wired:       true,
			readerFloor: "v0.2.44",
			want:        []int{2},
			wantPresent: true,
		},
		{
			name:        "wired floor at minimum advertises v2 and v3",
			wired:       true,
			readerFloor: "0.2.44",
			want:        []int{2, 3},
			wantPresent: true,
		},
		{
			name:        "wired later stable floor advertises v2 and v3",
			wired:       true,
			readerFloor: "0.10.0",
			want:        []int{2, 3},
			wantPresent: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := servercapabilities.NewHandler(&config.Config{
				InstanceType:     "saas",
				ClientMinVersion: tc.readerFloor,
			})
			h.SetChunkedAttachmentUpload(tc.wired)
			w, c := newTestContext()
			h.GetCapabilities(c)

			var body struct {
				Features map[string]json.RawMessage `json:"features"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			rawVersions, present := body.Features["attachmentEnvelopeVersions"]
			assert.Equal(t, tc.wantPresent, present)
			if !present {
				return
			}

			var versions []int
			require.NoError(t, json.Unmarshal(rawVersions, &versions))
			assert.Equal(t, tc.want, versions)
		})
	}
}

// The MFA-enforced dangerous-actions capability (#3454, C5). A client offers to
// turn enforcement ON only when this is true (X16), so a handler nobody wired
// must say false, and say it explicitly.
func TestGetCapabilities_MFAEnforcedDangerousActions_DefaultsFalse(t *testing.T) {
	w, c := newTestContext()
	servercapabilities.NewHandler(&config.Config{InstanceType: "saas"}).GetCapabilities(c)

	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	features, ok := body["features"].(map[string]any)
	require.True(t, ok, "features object missing")
	got, present := features["mfaEnforcedDangerousActions"]
	require.True(t, present, "capability key absent; the client reads it by this exact name")
	assert.Equal(t, false, got)
}

func TestGetCapabilities_MFAEnforcedDangerousActions_ReflectsWiring(t *testing.T) {
	h := servercapabilities.NewHandler(&config.Config{InstanceType: "saas"})
	h.SetMFAEnforcedDangerousActions(true)

	w, c := newTestContext()
	h.GetCapabilities(c)

	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	features, ok := body["features"].(map[string]any)
	require.True(t, ok, "features object missing")
	assert.Equal(t, true, features["mfaEnforcedDangerousActions"])
}

// Through the real router: a deployment that booted has passed the
// dangerous-action boot guard, so it advertises the gates.
// Kills: the router never calling the setter.
func TestServerCapabilitiesEndpoint_MFAEnforcedDangerousActionsWired(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	w := ts.DoRequest("GET", "/api/v1/server/capabilities", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Features map[string]any `json:"features"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, true, resp.Features["mfaEnforcedDangerousActions"])
}

// An enabled raw flag must not advertise hosted SSO on a self-hosted deployment.
func TestGetCapabilities_OAuthDeploymentPolicy(t *testing.T) {
	modes := []struct {
		name, raw, wantType string
		selfHosted          bool
	}{
		{"self-hosted", "self-hosted", "self-hosted", true},
		{"normalized self-hosted", " \tSELF-HOSTED\n", "self-hosted", true},
		{"saas", "saas", "saas", false},
		{"empty fallback", "", "saas", false},
		{"unknown fallback", "enterprise", "saas", false},
	}
	flags := []struct {
		name          string
		google, apple bool
		want          []string
	}{
		{"disabled", false, false, []string{}},
		{"google only", true, false, []string{"google"}},
		{"apple only", false, true, []string{"apple"}},
		{"both enabled", true, true, []string{"google", "apple"}},
	}
	for _, mode := range modes {
		for _, flag := range flags {
			t.Run(mode.name+"/"+flag.name, func(t *testing.T) {
				cfg := &config.Config{InstanceType: mode.raw,
					GoogleSSO: config.GoogleSSOConfig{Enabled: flag.google, ClientID: "fixture-google-client-id"},
					AppleSSO: config.AppleSSOConfig{Enabled: flag.apple, ClientID: "fixture-apple-client-id",
						TeamID: "fixture-team-id", KeyID: "fixture-key-id", PrivateKey: []byte("fixture-private-key")},
				}
				w, c := newTestContext()
				servercapabilities.NewHandler(cfg).GetCapabilities(c)
				require.Equal(t, http.StatusOK, w.Code)
				var resp servercapabilities.Response
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				want := flag.want
				if mode.selfHosted {
					want = []string{}
				}
				assert.Equal(t, want, resp.Auth.OAuthProviders)
				assert.Equal(t, mode.wantType, resp.Server.InstanceType)
				var encoded struct {
					Auth struct {
						Providers json.RawMessage `json:"oauthProviders"`
					} `json:"auth"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &encoded))
				if len(want) == 0 {
					assert.Equal(t, "[]", string(encoded.Auth.Providers))
				}
				assert.Equal(t, "no-store, no-cache, must-revalidate, max-age=0", w.Header().Get("Cache-Control"))
				assert.Equal(t, "no-cache", w.Header().Get("Pragma"))
				for _, credential := range []string{"fixture-google-client-id", "fixture-apple-client-id", "fixture-team-id", "fixture-key-id", "fixture-private-key"} {
					assert.NotContains(t, w.Body.String(), credential)
				}
			})
		}
	}
}
