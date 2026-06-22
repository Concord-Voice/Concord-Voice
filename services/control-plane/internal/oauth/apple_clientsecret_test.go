package oauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testTeamID   = "TEAM123ABC"
	testClientID = "com.example.concord"
	testKeyID    = "KEYID12345"
)

func newTestAppleSigner(t *testing.T, key *ecdsa.PrivateKey, ttl time.Duration) *appleClientSecretSigner {
	t.Helper()
	return newAppleClientSecretSigner(testTeamID, testClientID, testKeyID, key, ttl)
}

func generateTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return key
}

func TestAppleClientSecret_StructureAndClaims(t *testing.T) {
	key := generateTestKey(t)
	ttl := 5 * time.Minute
	signer := newTestAppleSigner(t, key, ttl)

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	tok, err := signer.signed(now)
	require.NoError(t, err)
	require.NotEmpty(t, tok)

	// Parse and verify the JWT using the public key.
	// jwt.WithoutClaimsValidation skips the exp/nbf wall-clock checks so we can
	// use a fixed past timestamp without the parser rejecting the JWT as expired.
	// Signature verification still runs (the keyfunc is invoked).
	parsed, err := jwt.Parse(tok, func(jt *jwt.Token) (interface{}, error) {
		_, ok := jt.Method.(*jwt.SigningMethodECDSA)
		if !ok {
			return nil, fmt.Errorf("expected ECDSA signing method, got %T", jt.Method)
		}
		return &key.PublicKey, nil
	}, jwt.WithoutClaimsValidation())
	require.NoError(t, err)
	require.True(t, parsed.Valid)

	// Assert header fields.
	assert.Equal(t, "ES256", parsed.Header["alg"])
	assert.Equal(t, testKeyID, parsed.Header["kid"])
	assert.Equal(t, "JWT", parsed.Header["typ"])

	// Assert claims.
	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok)

	assert.Equal(t, testTeamID, claims["iss"])
	assert.Equal(t, testClientID, claims["sub"])
	assert.Equal(t, "https://appleid.apple.com", claims["aud"])

	iat, ok := claims["iat"].(float64)
	require.True(t, ok, "iat should be a number")
	assert.InDelta(t, float64(now.Unix()), iat, 1, "iat should be approximately now")

	exp, ok := claims["exp"].(float64)
	require.True(t, ok, "exp should be a number")
	assert.InDelta(t, float64(now.Add(ttl).Unix()), exp, 1, "exp should be iat + ttl")
}

func TestAppleClientSecret_CacheHitWithinGrace(t *testing.T) {
	key := generateTestKey(t)
	ttl := 10 * time.Minute
	signer := newTestAppleSigner(t, key, ttl)

	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// First call — generates and caches.
	tok1, err := signer.signed(t0)
	require.NoError(t, err)
	require.NotEmpty(t, tok1)

	// Second call 30s later — still within the 60s grace window, should cache hit.
	tok2, err := signer.signed(t0.Add(30 * time.Second))
	require.NoError(t, err)

	assert.Equal(t, tok1, tok2, "should return byte-identical cached JWT within reuse grace")
}

func TestAppleClientSecret_RegeneratesOnExpiry(t *testing.T) {
	key := generateTestKey(t)
	ttl := 120 * time.Second
	signer := newTestAppleSigner(t, key, ttl)

	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// First call at t0.
	tok1, err := signer.signed(t0)
	require.NoError(t, err)
	require.NotEmpty(t, tok1)

	// Second call at t0+90s. The JWT expires at t0+120s.
	// With 60s reuse grace: expiry - grace = t0+120s - 60s = t0+60s.
	// At t0+90s, now.Add(grace) = t0+90s+60s = t0+150s > t0+120s (expiry),
	// so the cache check fails and a fresh JWT is signed.
	tok2, err := signer.signed(t0.Add(90 * time.Second))
	require.NoError(t, err)
	require.NotEmpty(t, tok2)

	assert.NotEqual(t, tok1, tok2, "should regenerate JWT when approaching expiry")

	// Verify the new JWT has a fresh iat.
	// Use WithoutClaimsValidation so the fixed-clock iat/exp don't fail wall-clock checks.
	parsed, err := jwt.Parse(tok2, func(_ *jwt.Token) (interface{}, error) {
		return &key.PublicKey, nil
	}, jwt.WithoutClaimsValidation())
	require.NoError(t, err)

	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok)

	iat, ok := claims["iat"].(float64)
	require.True(t, ok)
	expectedIat := float64(t0.Add(90 * time.Second).Unix())
	assert.InDelta(t, expectedIat, iat, 1, "regenerated JWT should have updated iat")
}

func TestAppleClientSecret_ConcurrentSigning(t *testing.T) {
	key := generateTestKey(t)
	ttl := 10 * time.Minute
	signer := newTestAppleSigner(t, key, ttl)

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	const goroutines = 10

	results := make([]string, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			tok, err := signer.signed(now)
			require.NoError(t, err)
			results[idx] = tok
		}(i)
	}

	wg.Wait()

	require.NotEmpty(t, results[0])

	// Stronger invariant: exactly ONE distinct JWT was returned across all goroutines.
	unique := make(map[string]struct{}, len(results))
	for _, tok := range results {
		unique[tok] = struct{}{}
	}
	assert.Equal(t, 1, len(unique), "expected exactly one unique JWT across %d goroutines, got %d", goroutines, len(unique))
}

// TestBrokerClientSecret_FreshSixtySecondJWT verifies the broker path mints a
// 60-second JWT on every call from its dedicated signer instance — never the
// Exchange path's long-TTL cached signer (#972 spec F1).
func TestBrokerClientSecret_FreshSixtySecondJWT(t *testing.T) {
	key := generateTestKey(t)
	provider := newTestAppleProviderWithKey(t, key)

	now := time.Date(2026, 6, 11, 8, 0, 0, 0, time.UTC)
	tok, err := provider.BrokerClientSecret(now)
	require.NoError(t, err)
	require.NotEmpty(t, tok)

	parsed, err := jwt.Parse(tok, func(jt *jwt.Token) (interface{}, error) {
		if _, ok := jt.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, fmt.Errorf("expected ECDSA signing method, got %T", jt.Method)
		}
		return &key.PublicKey, nil
	}, jwt.WithoutClaimsValidation())
	require.NoError(t, err)
	require.True(t, parsed.Valid)

	assert.Equal(t, "ES256", parsed.Header["alg"])
	assert.Equal(t, testKeyID, parsed.Header["kid"])

	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok)
	assert.Equal(t, testTeamID, claims["iss"])
	assert.Equal(t, testClientID, claims["sub"])
	assert.Equal(t, "https://appleid.apple.com", claims["aud"])

	iat := int64(claims["iat"].(float64))
	exp := int64(claims["exp"].(float64))
	assert.Equal(t, int64(60), exp-iat, "broker JWTs must live exactly 60s")
	assert.Equal(t, now.Unix(), iat)
}

// newTestAppleProviderWithKey builds a real AppleProvider around a generated
// P-256 key, PEM-encoding it the way Apple's .p8 export does.
func newTestAppleProviderWithKey(t *testing.T, key *ecdsa.PrivateKey) *AppleProvider {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	provider, err := NewAppleProvider(AppleConfig{
		ClientID:    testClientID,
		TeamID:      testTeamID,
		KeyID:       testKeyID,
		PrivateKey:  pemBytes,
		RedirectURI: "http://127.0.0.1:0/oauth/callback",
	})
	require.NoError(t, err)
	return provider
}
