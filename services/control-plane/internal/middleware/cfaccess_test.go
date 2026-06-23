package middleware

import (
	"bytes"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
)

func TestRequireCloudflareAccessDeniesAbsentAndAllowsValidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	verifier := newAccessVerifier(jwks.URL, "test-aud")
	router := gin.New()
	var called int32
	router.Use(RequireCloudflareAccess(verifier, slog.Default()))
	router.GET("/admin/x", func(c *gin.Context) {
		atomic.AddInt32(&called, 1)
		c.Status(http.StatusOK)
	})

	missing := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/x", nil)
	router.ServeHTTP(missing, req)
	require.Equal(t, http.StatusForbidden, missing.Code)
	require.Equal(t, int32(0), atomic.LoadInt32(&called))

	token := signJWT(t, key, "k1", map[string]any{
		"aud": "test-aud",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iss": jwks.URL,
	})
	allowed := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/x", nil)
	req.Header.Set(accessHeader, token)
	router.ServeHTTP(allowed, req)
	require.Equal(t, http.StatusOK, allowed.Code)
	require.Equal(t, int32(1), atomic.LoadInt32(&called))
}

func TestAccessVerifierAllowsValidToken(t *testing.T) {
	key := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	token := signJWT(t, key, "k1", map[string]any{
		"aud":   "test-aud",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"iss":   jwks.URL,
		"email": "admin@example.test",
	})

	claims, err := newAccessVerifier(jwks.URL, "test-aud").Verify(token)
	require.NoError(t, err)
	require.Equal(t, "admin@example.test", claims.Email)
}

func TestAccessVerifierCachesJWKS(t *testing.T) {
	key := newRSAKey(t)
	hits := 0
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	token := signJWT(t, key, "k1", map[string]any{
		"aud": "test-aud",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iss": jwks.URL,
	})
	verifier := newAccessVerifier(jwks.URL, "test-aud")

	_, err := verifier.Verify(token)
	require.NoError(t, err)
	_, err = verifier.Verify(token)
	require.NoError(t, err)
	require.Equal(t, 1, hits)
}

func TestAccessVerifierDebouncesUnknownKidRefresh(t *testing.T) {
	fixedNow := time.Unix(1_700_000_000, 0)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() { nowFunc = oldNow })

	key := newRSAKey(t)
	hits := 0
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	claims := map[string]any{"aud": "test-aud", "exp": fixedNow.Add(time.Hour).Unix(), "iss": jwks.URL}
	verifier := newAccessVerifier(jwks.URL, "test-aud")
	_, err := verifier.Verify(signJWT(t, key, "k1", claims))
	require.NoError(t, err)

	_, err = verifier.Verify(signJWT(t, key, "k2", claims))
	require.ErrorIs(t, err, errUnknownKID)
	require.Equal(t, 1, hits)

	nowFunc = func() time.Time { return fixedNow.Add(time.Second + time.Millisecond) }
	_, err = verifier.Verify(signJWT(t, key, "k2", claims))
	require.ErrorIs(t, err, errUnknownKID)
	require.Equal(t, 2, hits)
}

func TestRequireCloudflareAccessFromConfigAllowsValidToken(t *testing.T) {
	key := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	router := gin.New()
	router.Use(RequireCloudflareAccessFromConfig(&config.Config{CFAccessAUD: "test-aud", CFAccessTeamDomain: jwks.URL}, slog.Default()))
	router.GET("/admin/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/x", nil)
	req.Header.Set(accessHeader, signJWT(t, key, "k1", map[string]any{"aud": []string{"other", "test-aud"}, "exp": time.Now().Add(time.Hour).Unix(), "iss": jwks.URL}))
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestRSAPublicKeyFromJWKRejectsInvalidKeys(t *testing.T) {
	key := newRSAKey(t)
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())

	_, ok := rsaPublicKeyFromJWK("EC", "RS256", n, e)
	require.False(t, ok)
	_, ok = rsaPublicKeyFromJWK("RSA", "HS256", n, e)
	require.False(t, ok)
	_, ok = rsaPublicKeyFromJWK("RSA", "RS256", "%%%", e)
	require.False(t, ok)
	_, ok = rsaPublicKeyFromJWK("RSA", "RS256", n, "%%%")
	require.False(t, ok)
	_, ok = rsaPublicKeyFromJWK("RSA", "RS256", base64.RawURLEncoding.EncodeToString([]byte{}), e)
	require.False(t, ok)
}

func TestAccessVerifierRejectsInvalidTokens(t *testing.T) {
	fixedNow := time.Unix(1_700_000_000, 0)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() { nowFunc = oldNow })

	key := newRSAKey(t)
	otherKey := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	validClaims := map[string]any{
		"aud": "test-aud",
		"exp": fixedNow.Add(time.Hour).Unix(),
		"iss": jwks.URL,
	}

	tests := []struct {
		name  string
		token string
		want  error
	}{
		{"malformed", "not-a-jwt", errMalformed},
		{"bad_signature", signJWT(t, otherKey, "k1", validClaims), errBadSignature},
		{"missing_exp", signJWT(t, key, "k1", map[string]any{"aud": "test-aud", "iss": jwks.URL}), errExpired},
		{"unknown_kid", signJWT(t, key, "k2", validClaims), errUnknownKID},
		{"alg_none", unsignedJWT(t, "none", "k1", validClaims), errBadAlg},
		{"alg_hs256", hmacJWT(t, key.N.Bytes(), "k1", validClaims), errBadAlg},
		{"aud_mismatch", signJWT(t, key, "k1", map[string]any{"aud": "wrong-aud", "exp": fixedNow.Add(time.Hour).Unix(), "iss": jwks.URL}), errAudMismatch},
		{"expired", signJWT(t, key, "k1", map[string]any{"aud": "test-aud", "exp": fixedNow.Add(-2 * time.Minute).Unix(), "iss": jwks.URL}), errExpired},
		{"nbf_future", signJWT(t, key, "k1", map[string]any{"aud": "test-aud", "exp": fixedNow.Add(time.Hour).Unix(), "nbf": fixedNow.Add(2 * time.Minute).Unix(), "iss": jwks.URL}), errExpired},
		{"bad_issuer", signJWT(t, key, "k1", map[string]any{"aud": "test-aud", "exp": fixedNow.Add(time.Hour).Unix(), "iss": "https://other.cloudflareaccess.com"}), errBadIssuer},
	}

	verifier := newAccessVerifier(jwks.URL, "test-aud")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := verifier.Verify(tt.token)
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestAccessVerifierRefetchesOnKidMiss(t *testing.T) {
	currentNow := time.Unix(1_700_000_000, 0)
	oldNow := nowFunc
	nowFunc = func() time.Time { return currentNow }
	t.Cleanup(func() { nowFunc = oldNow })

	key1 := newRSAKey(t)
	key2 := newRSAKey(t)
	hits := 0
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			_, _ = w.Write(jwksJSON(t, "k1", key1))
			return
		}
		_, _ = w.Write(jwksJSONMulti(t, map[string]*rsa.PrivateKey{"k1": key1, "k2": key2}))
	}))
	t.Cleanup(jwks.Close)

	verifier := newAccessVerifier(jwks.URL, "test-aud")
	_, err := verifier.Verify(signJWT(t, key1, "k1", map[string]any{"aud": "test-aud", "exp": currentNow.Add(time.Hour).Unix(), "iss": jwks.URL}))
	require.NoError(t, err)
	currentNow = currentNow.Add(time.Second + time.Millisecond)
	_, err = verifier.Verify(signJWT(t, key2, "k2", map[string]any{"aud": "test-aud", "exp": currentNow.Add(time.Hour).Unix(), "iss": jwks.URL}))
	require.NoError(t, err)
	require.Equal(t, 2, hits)
}

func TestAccessVerifierFailsClosedWhenJWKSUnavailableOnEmptyCache(t *testing.T) {
	key := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(jwks.Close)

	token := signJWT(t, key, "k1", map[string]any{"aud": "test-aud", "exp": time.Now().Add(time.Hour).Unix(), "iss": jwks.URL})
	_, err := newAccessVerifier(jwks.URL, "test-aud").Verify(token)
	require.ErrorIs(t, err, errUnknownKID)
}

func TestAccessVerifierRejectsStaleCachedKeyWhenRefreshFails(t *testing.T) {
	fixedNow := time.Unix(1_700_000_000, 0)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() { nowFunc = oldNow })

	key := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))

	token := signJWT(t, key, "k1", map[string]any{
		"aud": "test-aud",
		"exp": fixedNow.Add(3 * jwksTTL).Unix(),
		"iss": jwks.URL,
	})
	verifier := newAccessVerifier(jwks.URL, "test-aud")
	_, err := verifier.Verify(token)
	require.NoError(t, err)
	jwks.Close()

	nowFunc = func() time.Time { return fixedNow.Add(jwksTTL + time.Second) }
	_, err = verifier.Verify(token)
	require.ErrorIs(t, err, errUnknownKID)
}

func TestRequireCloudflareAccessLogsPIISafeReason(t *testing.T) {
	key := newRSAKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, "k1", key))
	}))
	t.Cleanup(jwks.Close)

	token := signJWT(t, key, "k1", map[string]any{
		"aud":   "wrong-aud",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iss":   jwks.URL,
		"email": "admin@example.test",
	})
	var logs bytes.Buffer
	router := gin.New()
	router.Use(RequireCloudflareAccess(newAccessVerifier(jwks.URL, "test-aud"), slog.New(slog.NewTextHandler(&logs, nil))))
	router.GET("/admin/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/x", nil)
	req.Header.Set(accessHeader, token)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	out := logs.String()
	require.Contains(t, out, "aud_mismatch")
	require.NotContains(t, out, token)
	require.NotContains(t, out, "admin@example.test")
}

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return key
}

func jwksJSON(t *testing.T, kid string, key *rsa.PrivateKey) []byte {
	t.Helper()
	return jwksJSONMulti(t, map[string]*rsa.PrivateKey{kid: key})
}

func jwksJSONMulti(t *testing.T, keys map[string]*rsa.PrivateKey) []byte {
	t.Helper()
	jwks := make([]map[string]string, 0, len(keys))
	for kid, key := range keys {
		jwks = append(jwks, map[string]string{
			"kid": kid,
			"kty": "RSA",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		})
	}
	b, err := json.Marshal(map[string]any{"keys": jwks})
	require.NoError(t, err)
	return b
}

func signJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	h := jsonSegment(t, map[string]string{"alg": "RS256", "kid": kid})
	p := jsonSegment(t, claims)
	signed := h + "." + p
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	require.NoError(t, err)
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func unsignedJWT(t *testing.T, alg, kid string, claims map[string]any) string {
	t.Helper()
	return jsonSegment(t, map[string]string{"alg": alg, "kid": kid}) + "." + jsonSegment(t, claims) + "."
}

func hmacJWT(t *testing.T, secret []byte, kid string, claims map[string]any) string {
	t.Helper()
	signed := jsonSegment(t, map[string]string{"alg": "HS256", "kid": kid}) + "." + jsonSegment(t, claims)
	mac := hmac.New(sha256.New, secret)
	_, err := mac.Write([]byte(signed))
	require.NoError(t, err)
	return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func jsonSegment(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(b)
}
