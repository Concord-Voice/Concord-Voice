package oauth_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/oauth"
)

const (
	testAppleClientID = "chat.concordvoice.signin"
	testAppleTeamID   = "TEAM123ABC"
	testAppleKeyID    = "KEYID12345"
	testAppleRedirect = "http://127.0.0.1:54321/oauth/callback"
)

// generateP256PEM returns a freshly-generated P-256 ECDSA private key encoded as
// PKCS8 PEM bytes — the wire format Apple's developer portal exports for .p8 files.
func generateP256PEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// generateP384PEM returns a freshly-generated P-384 ECDSA private key as PKCS8 PEM.
// Used to verify NewAppleProvider rejects non-P-256 curves.
func generateP384PEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// generateRSAPEM returns a freshly-generated RSA-2048 private key as PKCS8 PEM.
// Used to verify NewAppleProvider rejects RSA keys (Apple uses ES256).
func generateRSAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func newTestAppleProvider(t *testing.T) *oauth.AppleProvider {
	t.Helper()
	p, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  generateP256PEM(t),
		RedirectURI: testAppleRedirect,
	})
	require.NoError(t, err)
	return p
}

// --- Constructor tests ---

func TestNewAppleProvider_RequiresClientID(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    "",
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  generateP256PEM(t),
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ClientID")
}

func TestNewAppleProvider_RequiresTeamID(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      "",
		KeyID:       testAppleKeyID,
		PrivateKey:  generateP256PEM(t),
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TeamID")
}

func TestNewAppleProvider_RequiresKeyID(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       "",
		PrivateKey:  generateP256PEM(t),
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "KeyID")
}

func TestNewAppleProvider_RequiresPrivateKey(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  nil,
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PrivateKey")
}

func TestNewAppleProvider_RequiresRedirectURI(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  generateP256PEM(t),
		RedirectURI: "",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RedirectURI")
}

func TestNewAppleProvider_RejectsMalformedPrivateKey(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  []byte("not a pem block"),
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
}

func TestNewAppleProvider_RejectsRSAPrivateKey(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  generateRSAPEM(t),
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "P-256")
}

func TestNewAppleProvider_RejectsP384Curve(t *testing.T) {
	_, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    testAppleClientID,
		TeamID:      testAppleTeamID,
		KeyID:       testAppleKeyID,
		PrivateKey:  generateP384PEM(t),
		RedirectURI: testAppleRedirect,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "P-256")
}

// --- Provider basics ---

func TestAppleProvider_Name(t *testing.T) {
	p := newTestAppleProvider(t)
	assert.Equal(t, "apple", p.Name())
}

func TestAppleProvider_AuthorizationURL_Shape(t *testing.T) {
	p := newTestAppleProvider(t)
	u := p.AuthorizationURL("state-abc", "nonce-xyz", "challenge-pkce")

	parsed, err := url.Parse(u)
	require.NoError(t, err)
	assert.Equal(t, "https", parsed.Scheme)
	assert.Equal(t, "appleid.apple.com", parsed.Host)
	assert.Equal(t, "/auth/authorize", parsed.Path)

	q := parsed.Query()
	assert.Equal(t, testAppleClientID, q.Get("client_id"))
	assert.Equal(t, testAppleRedirect, q.Get("redirect_uri"))
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "name email", q.Get("scope"))
	assert.Equal(t, "form_post", q.Get("response_mode"))
	assert.Equal(t, "state-abc", q.Get("state"))
	assert.Equal(t, "nonce-xyz", q.Get("nonce"))
	assert.Equal(t, "challenge-pkce", q.Get("code_challenge"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
}
