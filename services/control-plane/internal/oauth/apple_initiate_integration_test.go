package oauth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/oauth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
)

// newDualProviderInitiateRig wires Initiate with BOTH providers registered so
// the PKCE contract is pinned from one rig: both apple and google REQUIRE a
// client-supplied code_challenge (#974/#975).
func newDualProviderInitiateRig(t *testing.T) (*gin.Engine, *redis.Client) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)

	google, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:    "test-client.apps.googleusercontent.com",
		RedirectURI: "http://127.0.0.1:0/oauth/callback",
	})
	require.NoError(t, err)
	apple, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  generateP256PEM(t),
		RedirectURI: "http://127.0.0.1:0/oauth/callback",
	})
	require.NoError(t, err)

	registry := oauth.NewRegistry()
	registry.Register(google)
	registry.Register(apple)

	h := oauth.NewHandler(oauth.HandlerDeps{Registry: registry, Redis: rdb})
	r := gin.New()
	r.GET("/api/v1/auth/sso/:provider", h.Initiate)
	return r, rdb
}

const initiateRedirect = "http://127.0.0.1:65000/oauth/callback"

func getInitiate(r *gin.Engine, provider, challenge string) *httptest.ResponseRecorder {
	target := "/api/v1/auth/sso/" + provider + "?redirect_uri=" + url.QueryEscape(initiateRedirect)
	if challenge != "" {
		target += "&code_challenge=" + url.QueryEscape(challenge)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestInitiate_Apple_RequiresCodeChallenge(t *testing.T) {
	r, _ := newDualProviderInitiateRig(t)

	cases := map[string]string{
		"absent":      "",
		"too_short":   strings.Repeat("a", 42),
		"too_long":    strings.Repeat("a", 129),
		"bad_charset": strings.Repeat("a", 42) + "+",
	}
	for name, challenge := range cases {
		t.Run(name, func(t *testing.T) {
			w := getInitiate(r, "apple", challenge)
			require.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "code_challenge_required")
		})
	}
}

func TestInitiate_Apple_EmbedsClientChallenge_ReturnsNonce_EmptyVerifier(t *testing.T) {
	r, rdb := newDualProviderInitiateRig(t)
	challenge := strings.Repeat("a", 43)

	w := getInitiate(r, "apple", challenge)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		AuthURL string `json:"auth_url"`
		State   string `json:"state"`
		Nonce   string `json:"nonce"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.AuthURL)
	require.NotEmpty(t, resp.State)
	require.NotEmpty(t, resp.Nonce, "apple initiate must return the nonce for local id_token binding (#974)")

	parsed, err := url.Parse(resp.AuthURL)
	require.NoError(t, err)
	q := parsed.Query()
	assert.Equal(t, challenge, q.Get("code_challenge"),
		"authorize URL must embed the CLIENT-supplied challenge, not a server-generated one")
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.Equal(t, initiateRedirect, q.Get("redirect_uri"))

	raw, err := rdb.Get(context.Background(), "sso_state:"+resp.State).Bytes()
	require.NoError(t, err)
	var rec struct {
		Provider     string `json:"provider"`
		State        string `json:"state"`
		Nonce        string `json:"nonce"`
		CodeVerifier string `json:"code_verifier"`
	}
	require.NoError(t, json.Unmarshal(raw, &rec))
	assert.Equal(t, "apple", rec.Provider)
	assert.Equal(t, resp.State, rec.State)
	assert.Equal(t, resp.Nonce, rec.Nonce, "record nonce must match the response nonce")
	assert.Empty(t, rec.CodeVerifier, "apple records must store NO verifier — the client owns it (#974)")
}

// TestInitiate_Google_RequiresCodeChallenge mirrors TestInitiate_Apple_RequiresCodeChallenge:
// after #975 google is also client-driven, so a missing/malformed challenge
// returns 400 code_challenge_required (identical to apple's gate).
func TestInitiate_Google_RequiresCodeChallenge(t *testing.T) {
	r, _ := newDualProviderInitiateRig(t)

	cases := map[string]string{
		"absent":      "",
		"too_short":   strings.Repeat("a", 42),
		"too_long":    strings.Repeat("a", 129),
		"bad_charset": strings.Repeat("a", 42) + "+",
	}
	for name, challenge := range cases {
		t.Run(name, func(t *testing.T) {
			w := getInitiate(r, "google", challenge)
			require.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "code_challenge_required")
		})
	}
}

// TestInitiate_Google_EmbedsClientChallenge_ReturnsNonce_EmptyVerifier mirrors
// TestInitiate_Apple_EmbedsClientChallenge_ReturnsNonce_EmptyVerifier: after
// #975 google returns nonce + embeds the CLIENT challenge in the auth URL and
// stores an empty CodeVerifier (the client owns the verifier).
func TestInitiate_Google_EmbedsClientChallenge_ReturnsNonce_EmptyVerifier(t *testing.T) {
	r, rdb := newDualProviderInitiateRig(t)
	challenge := strings.Repeat("b", 43)

	w := getInitiate(r, "google", challenge)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		AuthURL string `json:"auth_url"`
		State   string `json:"state"`
		Nonce   string `json:"nonce"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.AuthURL)
	require.NotEmpty(t, resp.State)
	require.NotEmpty(t, resp.Nonce, "google initiate must return nonce for desktop id_token binding (#975)")

	parsed, err := url.Parse(resp.AuthURL)
	require.NoError(t, err)
	q := parsed.Query()
	assert.Equal(t, challenge, q.Get("code_challenge"),
		"authorize URL must embed the CLIENT-supplied challenge, not a server-generated one")
	assert.Equal(t, "S256", q.Get("code_challenge_method"))

	raw, err := rdb.Get(context.Background(), "sso_state:"+resp.State).Bytes()
	require.NoError(t, err)
	var rec struct {
		Provider     string `json:"provider"`
		State        string `json:"state"`
		Nonce        string `json:"nonce"`
		CodeVerifier string `json:"code_verifier"`
	}
	require.NoError(t, json.Unmarshal(raw, &rec))
	assert.Equal(t, "google", rec.Provider)
	assert.Equal(t, resp.State, rec.State)
	assert.Equal(t, resp.Nonce, rec.Nonce, "record nonce must match the response nonce")
	assert.Empty(t, rec.CodeVerifier, "google records must store NO verifier — the client owns it (#975)")
}
