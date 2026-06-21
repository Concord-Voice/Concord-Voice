package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jwksServerForKey starts an httptest.Server that serves a JWKS with the
// provided RSA public key under the given kid.
func jwksServerForKey(t *testing.T, priv *rsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid, "n": n, "e": e,
			}},
		})
	}))
}

// signGoogleToken signs a JWT using jwt.MapClaims with the provided RSA key and kid.
func signGoogleToken(t *testing.T, priv *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(priv)
	require.NoError(t, err)
	return signed
}

// newGoogleProviderWithJWKS returns a GoogleProvider pointing at the given JWKS server URL.
func newGoogleProviderWithJWKS(t *testing.T, jwksURL string) *GoogleProvider {
	t.Helper()
	p, err := NewGoogleProvider(GoogleConfig{
		ClientID:     "test-client.apps.googleusercontent.com",
		RedirectURI:  "http://127.0.0.1:0/cb",
		JWKSEndpoint: jwksURL,
		Issuer:       "https://accounts.google.com",
	})
	require.NoError(t, err)
	return p
}

// TestGoogleProvider_ValidateIDToken_Happy verifies that a well-formed token
// with matching issuer, audience, expiry, and nonce is accepted.
func TestGoogleProvider_ValidateIDToken_Happy(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "kid-happy"

	jwks := jwksServerForKey(t, priv, kid)
	defer jwks.Close()

	now := time.Now()
	idToken := signGoogleToken(t, priv, kid, jwt.MapClaims{
		"iss":            "https://accounts.google.com",
		"aud":            "test-client.apps.googleusercontent.com",
		"sub":            "111",
		"email":          "alice@example.test",
		"email_verified": true,
		"name":           "Alice",
		"nonce":          "expected-nonce",
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	})

	p := newGoogleProviderWithJWKS(t, jwks.URL)
	claims, err := p.validateIDToken(context.Background(), idToken, "expected-nonce")
	require.NoError(t, err)
	assert.Equal(t, "111", claims.Sub)
	assert.Equal(t, "alice@example.test", claims.Email)
	assert.True(t, claims.EmailVerified)
}

// TestGoogleProvider_ValidateIDToken_BareIssuer verifies the bare
// "accounts.google.com" issuer form is accepted server-side, matching the
// desktop verifier's dual-issuer set (#975 review L1 — avoids a token the
// client accepted being rejected at /session re-verification).
func TestGoogleProvider_ValidateIDToken_BareIssuer(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "kid-bare-iss"

	jwks := jwksServerForKey(t, priv, kid)
	defer jwks.Close()

	now := time.Now()
	idToken := signGoogleToken(t, priv, kid, jwt.MapClaims{
		"iss":            "accounts.google.com",
		"aud":            "test-client.apps.googleusercontent.com",
		"sub":            "222",
		"email_verified": true,
		"nonce":          "n",
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	})

	p := newGoogleProviderWithJWKS(t, jwks.URL)
	claims, err := p.validateIDToken(context.Background(), idToken, "n")
	require.NoError(t, err)
	assert.Equal(t, "222", claims.Sub)
}

// TestGoogleProvider_ValidateIDToken_WrongIssuer verifies a non-Google issuer
// is rejected (the dual-issuer check still fails closed for everything else).
func TestGoogleProvider_ValidateIDToken_WrongIssuer(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "kid-wrong-iss"

	jwks := jwksServerForKey(t, priv, kid)
	defer jwks.Close()

	now := time.Now()
	idToken := signGoogleToken(t, priv, kid, jwt.MapClaims{
		"iss":            "https://evil.example.com",
		"aud":            "test-client.apps.googleusercontent.com",
		"sub":            "333",
		"email_verified": true,
		"nonce":          "n",
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	})

	p := newGoogleProviderWithJWKS(t, jwks.URL)
	_, err = p.validateIDToken(context.Background(), idToken, "n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "iss")
}

// TestGoogleProvider_ValidateIDToken_NonceMismatch verifies that a token with
// the wrong nonce is rejected with an error mentioning "nonce".
func TestGoogleProvider_ValidateIDToken_NonceMismatch(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "kid-nonce"

	jwks := jwksServerForKey(t, priv, kid)
	defer jwks.Close()

	now := time.Now()
	idToken := signGoogleToken(t, priv, kid, jwt.MapClaims{
		"iss": "https://accounts.google.com", "aud": "test-client.apps.googleusercontent.com",
		"sub": "x", "email": "x@x", "email_verified": true,
		"nonce": "wrong-nonce",
		"iat":   now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})

	p := newGoogleProviderWithJWKS(t, jwks.URL)
	_, err = p.validateIDToken(context.Background(), idToken, "expected-nonce")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonce")
}

// TestGoogleProvider_ValidateIDToken_AudMismatch verifies that a token with
// the wrong audience is rejected.
func TestGoogleProvider_ValidateIDToken_AudMismatch(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "kid-aud"

	jwks := jwksServerForKey(t, priv, kid)
	defer jwks.Close()

	now := time.Now()
	idToken := signGoogleToken(t, priv, kid, jwt.MapClaims{
		"iss": "https://accounts.google.com", "aud": "wrong-audience",
		"sub": "x", "email": "x@x", "email_verified": true,
		"nonce": "n",
		"iat":   now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})

	p := newGoogleProviderWithJWKS(t, jwks.URL)
	_, err = p.validateIDToken(context.Background(), idToken, "n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aud")
}

// TestGoogleProvider_ValidateIDToken_Expired verifies that an expired token
// is rejected with an error mentioning "exp".
func TestGoogleProvider_ValidateIDToken_Expired(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "kid-exp"

	jwks := jwksServerForKey(t, priv, kid)
	defer jwks.Close()

	idToken := signGoogleToken(t, priv, kid, jwt.MapClaims{
		"iss": "https://accounts.google.com", "aud": "test-client.apps.googleusercontent.com",
		"sub": "x", "email": "x@x", "email_verified": true, "nonce": "n",
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
	})

	p := newGoogleProviderWithJWKS(t, jwks.URL)
	_, err = p.validateIDToken(context.Background(), idToken, "n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exp")
}
