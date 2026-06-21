package clientconfig_test

import (
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

func TestGetConfigReturnsExpectedShape(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)

	// Verify all expected top-level keys exist
	require.Contains(t, resp, "minVersion")
	require.Contains(t, resp, "featureFlags")
	require.Contains(t, resp, "mediaPlaneUrl")
	require.Contains(t, resp, "turn")
	require.NotContains(t, resp, "telemetryPolicyVersion", "field removed from /client/config response")

	// Feature flags is a nested object with only the live gifsEnabled member.
	// voice/video/e2ee were removed as inert under #1649 (e2ee per E2EE-everywhere #201).
	flags, ok := resp["featureFlags"].(map[string]interface{})
	require.True(t, ok, "featureFlags should be an object")
	assert.Contains(t, flags, "gifsEnabled")
	assert.NotContains(t, flags, "e2ee", "e2ee flag removed — encryption is structural under #201")
	assert.NotContains(t, flags, "voice", "voice flag removed as inert (#1649)")
	assert.NotContains(t, flags, "video", "video flag removed as inert (#1649)")

	// TURN should be a nested object (fields may be omitted when empty)
	_, ok = resp["turn"].(map[string]interface{})
	require.True(t, ok, "turn should be an object")
}

func TestGetConfigReflectsConfigValues(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)

	// Default test config: no min version; test config uses zero-value Config{}
	// so KlipyAPIKey is empty → gifsEnabled is false.
	assert.Equal(t, "", resp["minVersion"])

	flags := resp["featureFlags"].(map[string]interface{})
	assert.Equal(t, false, flags["gifsEnabled"])

	// Default media plane URL is empty in test config
	assert.Equal(t, "", resp["mediaPlaneUrl"])

	// SPA fields omitted when zero-valued (omitempty)
	assert.Nil(t, resp["spaUrl"])
	assert.Nil(t, resp["spaIpcContract"])
}

func TestGetConfigNoAuthRequired(t *testing.T) {
	ts := setupTS(t)

	// Endpoint should work without any auth headers
	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGetConfigEmptyTURNFieldsOmitted(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)

	// With empty TURN host/realm (test config default), the turn object
	// should exist but contain no host/realm keys (omitempty on both fields)
	turn, ok := resp["turn"].(map[string]interface{})
	require.True(t, ok, "turn should be an object")
	assert.NotContains(t, turn, "host", "empty host should be omitted")
	assert.NotContains(t, turn, "realm", "empty realm should be omitted")
}

func TestGetConfigSpaFieldsOmittedWhenZero(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)

	// spaUrl and spaIpcContract have omitempty — absent when zero-valued
	assert.Nil(t, resp["spaUrl"], "empty spaUrl should be omitted")
	assert.Nil(t, resp["spaIpcContract"], "zero spaIpcContract should be omitted")
}

func TestGetConfigContentTypeJSON(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)

	ct := w.Header().Get("Content-Type")
	assert.Contains(t, ct, "application/json")
}

func TestGetConfigFeatureFlagsIndependent(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", "/api/v1/client/config", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)

	flags := resp["featureFlags"].(map[string]interface{})

	// After #1649 the only featureFlags member is gifsEnabled. Test config has
	// KlipyAPIKey empty → gifsEnabled is explicitly false rather than missing.
	assert.Equal(t, false, flags["gifsEnabled"], "gifsEnabled should be explicitly false when no KLIPY credential is configured")

	// Verify exactly 1 key — no extra fields leaking. CRITICALLY: the
	// raw KLIPY app key must NEVER appear here.
	assert.Len(t, flags, 1, "featureFlags should have exactly 1 key (gifsEnabled) after #1649")
	_, hasKey := flags["klipyApiKey"]
	assert.False(t, hasKey, "klipyApiKey must NEVER be present in /client/config — it would leak the server-side KLIPY app key into the renderer bundle")
}
