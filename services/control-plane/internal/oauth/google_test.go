package oauth_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/oauth"
)

func newTestGoogleProvider(t *testing.T) *oauth.GoogleProvider {
	t.Helper()
	p, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:    "test-client.apps.googleusercontent.com",
		RedirectURI: "http://127.0.0.1:54321/oauth/callback",
	})
	require.NoError(t, err)
	return p
}

func TestGoogleProvider_Name(t *testing.T) {
	p := newTestGoogleProvider(t)
	assert.Equal(t, "google", p.Name())
}

func TestGoogleProvider_AuthorizationURL_Shape(t *testing.T) {
	p := newTestGoogleProvider(t)
	u := p.AuthorizationURL("state-abc", "nonce-xyz", "challenge-pkce")

	parsed, err := url.Parse(u)
	require.NoError(t, err)
	assert.Equal(t, "https", parsed.Scheme)
	assert.Equal(t, "accounts.google.com", parsed.Host)
	assert.Equal(t, "/o/oauth2/v2/auth", parsed.Path)

	q := parsed.Query()
	assert.Equal(t, "test-client.apps.googleusercontent.com", q.Get("client_id"))
	assert.Equal(t, "http://127.0.0.1:54321/oauth/callback", q.Get("redirect_uri"))
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "openid email profile", q.Get("scope"))
	assert.Equal(t, "state-abc", q.Get("state"))
	assert.Equal(t, "nonce-xyz", q.Get("nonce"))
	assert.Equal(t, "challenge-pkce", q.Get("code_challenge"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.Equal(t, "select_account", q.Get("prompt"))
}

// TestNewGoogleProvider_RejectsEmptyClientSecret deleted in #975:
// ClientSecret was removed from GoogleConfig — server never exchanges codes.

func TestNewGoogleProvider_RejectsEmptyClientID(t *testing.T) {
	_, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:    "",
		RedirectURI: "http://127.0.0.1/cb",
	})
	require.Error(t, err)
}

func TestNewGoogleProvider_RejectsEmptyRedirectURI(t *testing.T) {
	_, err := oauth.NewGoogleProvider(oauth.GoogleConfig{
		ClientID:    "id",
		RedirectURI: "",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RedirectURI")
}
