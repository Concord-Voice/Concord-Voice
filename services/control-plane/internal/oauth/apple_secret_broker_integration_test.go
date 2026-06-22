package oauth_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/oauth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const brokerPath = "/api/v1/auth/sso/%s/sign-client-secret"

// brokerRig wires the broker endpoint against real Redis with a captured
// logger, mirroring newSSOTestRig's shape.
type brokerRig struct {
	Engine *gin.Engine
	Redis  *redis.Client
	Key    *ecdsa.PrivateKey
	LogBuf *bytes.Buffer
}

func newBrokerRig(t *testing.T, withRateLimit bool) *brokerRig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	provider, err := oauth.NewAppleProvider(oauth.AppleConfig{
		ClientID:    "com.example.concord",
		TeamID:      "TEAM123ABC",
		KeyID:       "KEYID12345",
		PrivateKey:  pemBytes,
		RedirectURI: "http://127.0.0.1:0/oauth/callback",
	})
	require.NoError(t, err)

	registry := oauth.NewRegistry()
	registry.Register(provider)

	var logBuf bytes.Buffer
	h := oauth.NewHandler(oauth.HandlerDeps{
		Registry:   registry,
		Redis:      rdb,
		Log:        logger.NewWithWriter(&logBuf),
		AuditIPKey: []byte("test-audit-key"),
	})

	r := gin.New()
	handlers := []gin.HandlerFunc{h.SignAppleClientSecret}
	if withRateLimit {
		handlers = []gin.HandlerFunc{
			middleware.RateLimitByIP(rdb, 5, time.Minute),
			h.SignAppleClientSecret,
		}
	}
	r.POST(fmt.Sprintf(brokerPath, ":provider"), handlers...)
	return &brokerRig{Engine: r, Redis: rdb, Key: key, LogBuf: &logBuf}
}

// seedBrokerState writes an sso_state record the way Initiate does post-#972
// (state embedded). Returns the state token.
func seedBrokerState(t *testing.T, rdb redis.Cmdable, provider string) string {
	t.Helper()
	state := fmt.Sprintf("test-state-%s-%d", provider, time.Now().UnixNano())
	payload, err := json.Marshal(map[string]any{
		"provider":      provider,
		"state":         state,
		"nonce":         "test-nonce",
		"code_verifier": "test-verifier",
		"redirect_uri":  "http://127.0.0.1:51620/oauth/callback",
		"created_at":    time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), "sso_state:"+state, payload, 10*time.Minute).Err())
	return state
}

func postBroker(rig *brokerRig, provider, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf(brokerPath, provider), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.7:50000"
	w := httptest.NewRecorder()
	rig.Engine.ServeHTTP(w, req)
	return w
}

func TestSignClientSecret_HappyPath(t *testing.T) {
	rig := newBrokerRig(t, false)
	state := seedBrokerState(t, rig.Redis, "apple")

	w := postBroker(rig, "apple", fmt.Sprintf(`{"state":%q}`, state))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		ClientSecret string `json:"client_secret"` // #nosec G117 -- False positive: test-only struct deserializing the broker response to assert on it.
		ExpiresIn    int    `json:"expires_in"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 60, resp.ExpiresIn)

	parsed, err := jwt.Parse(resp.ClientSecret, func(jt *jwt.Token) (interface{}, error) {
		if _, ok := jt.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, fmt.Errorf("expected ECDSA, got %T", jt.Method)
		}
		return &rig.Key.PublicKey, nil
	})
	require.NoError(t, err)
	require.True(t, parsed.Valid)
	claims := parsed.Claims.(jwt.MapClaims)
	assert.Equal(t, "TEAM123ABC", claims["iss"])
	assert.Equal(t, "com.example.concord", claims["sub"])
	assert.Equal(t, "https://appleid.apple.com", claims["aud"])
	assert.Equal(t, "KEYID12345", parsed.Header["kid"])
	assert.Equal(t, "ES256", parsed.Header["alg"])
	iat := int64(claims["iat"].(float64))
	exp := int64(claims["exp"].(float64))
	assert.Equal(t, int64(60), exp-iat)

	// Audit: exactly one event; digests only — never the raw state, IP, or JWT.
	logs := rig.LogBuf.String()
	assert.Equal(t, 1, strings.Count(logs, "apple_client_secret_minted"))
	assert.Contains(t, logs, "state_digest")
	assert.NotContains(t, logs, state)
	assert.NotContains(t, logs, "203.0.113.7")
	assert.NotContains(t, logs, resp.ClientSecret)

	// The sso_state record survives minting (spec F2 — #974's /session needs it).
	exists, err := rig.Redis.Exists(context.Background(), "sso_state:"+state).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), exists)
}

func TestSignClientSecret_ReplayRejected(t *testing.T) {
	rig := newBrokerRig(t, false)
	state := seedBrokerState(t, rig.Redis, "apple")
	body := fmt.Sprintf(`{"state":%q}`, state)

	first := postBroker(rig, "apple", body)
	require.Equal(t, http.StatusOK, first.Code)

	second := postBroker(rig, "apple", body)
	assert.Equal(t, http.StatusUnauthorized, second.Code)
	assert.Contains(t, second.Body.String(), "invalid_state")
	// Replays emit no additional audit event.
	assert.Equal(t, 1, strings.Count(rig.LogBuf.String(), "apple_client_secret_minted"))
}

func TestSignClientSecret_ConcurrentDuplicates_ExactlyOneSucceeds(t *testing.T) {
	rig := newBrokerRig(t, false)
	state := seedBrokerState(t, rig.Redis, "apple")
	body := fmt.Sprintf(`{"state":%q}`, state)

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			codes[i] = postBroker(rig, "apple", body).Code
		}(i)
	}
	wg.Wait()

	okCount := 0
	for _, code := range codes {
		if code == http.StatusOK {
			okCount++
		} else {
			assert.Equal(t, http.StatusUnauthorized, code)
		}
	}
	assert.Equal(t, 1, okCount, "SETNX must admit exactly one concurrent mint")
}

func TestSignClientSecret_WrongProviderState(t *testing.T) {
	rig := newBrokerRig(t, false)
	state := seedBrokerState(t, rig.Redis, "google") // google-owned state

	w := postBroker(rig, "apple", fmt.Sprintf(`{"state":%q}`, state))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_state")
}

func TestSignClientSecret_UnknownProviderParam(t *testing.T) {
	rig := newBrokerRig(t, false)
	state := seedBrokerState(t, rig.Redis, "apple")

	w := postBroker(rig, "google", fmt.Sprintf(`{"state":%q}`, state))
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "unknown_provider")
}

// Missing and TTL-expired states share one branch: Redis eviction IS the
// expiry mechanism, so an expired key is indistinguishable from an absent one.
func TestSignClientSecret_MissingOrExpiredState(t *testing.T) {
	rig := newBrokerRig(t, false)

	w := postBroker(rig, "apple", `{"state":"never-seeded-state-value"}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_state")
	assert.NotContains(t, rig.LogBuf.String(), "apple_client_secret_minted")
}

// Records written before the State field existed (pre-#972 deploys) decode as
// "" and must fail the constant-time compare closed.
func TestSignClientSecret_LegacyRecordWithoutStateField(t *testing.T) {
	rig := newBrokerRig(t, false)
	state := fmt.Sprintf("legacy-state-%d", time.Now().UnixNano())
	payload, err := json.Marshal(map[string]any{
		"provider":      "apple",
		"nonce":         "n",
		"code_verifier": "v",
		"redirect_uri":  "http://127.0.0.1:51620/oauth/callback",
		"created_at":    time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, rig.Redis.Set(context.Background(), "sso_state:"+state, payload, 10*time.Minute).Err())

	w := postBroker(rig, "apple", fmt.Sprintf(`{"state":%q}`, state))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestSignClientSecret_MalformedBody(t *testing.T) {
	rig := newBrokerRig(t, false)
	for name, body := range map[string]string{
		"empty object":   `{}`,
		"empty state":    `{"state":""}`,
		"not json":       `not-json`,
		"oversize state": fmt.Sprintf(`{"state":%q}`, strings.Repeat("x", 300)),
	} {
		t.Run(name, func(t *testing.T) {
			w := postBroker(rig, "apple", body)
			assert.Equal(t, http.StatusUnauthorized, w.Code)
		})
	}
}

func TestSignClientSecret_RateLimited(t *testing.T) {
	rig := newBrokerRig(t, true)

	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last = postBroker(rig, "apple", `{"state":"garbage"}`)
	}
	assert.Equal(t, http.StatusTooManyRequests, last.Code,
		"6th call within the window must trip the 5/min/IP limit")
}

// failingBrokerProvider implements oauth.Provider plus the broker capability
// method, erroring on demand — exercises the compensating-DEL rollback path
// (Gitar finding on PR #1445: a signer error must not burn the state).
type failingBrokerProvider struct{ fail bool }

func (f *failingBrokerProvider) Name() string                           { return "apple" }
func (f *failingBrokerProvider) AuthorizationURL(_, _, _ string) string { return "https://fake/auth" }
func (f *failingBrokerProvider) BrokerClientSecret(_ time.Time) (string, error) {
	if f.fail {
		return "", fmt.Errorf("simulated signer failure")
	}
	return "fake-broker-jwt", nil
}

// TestSignClientSecret_SignerErrorDoesNotBurnState pins the rollback contract:
// a 500 from the signer must roll back the one-shot guard so a legitimate
// retry with the same state can succeed; after that success the one-shot
// guard re-engages as usual.
func TestSignClientSecret_SignerErrorDoesNotBurnState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	t.Cleanup(cleanup)

	provider := &failingBrokerProvider{fail: true}
	registry := oauth.NewRegistry()
	registry.Register(provider)

	h := oauth.NewHandler(oauth.HandlerDeps{
		Registry:   registry,
		Redis:      rdb,
		AuditIPKey: []byte("test-audit-key"),
	})
	r := gin.New()
	r.POST(fmt.Sprintf(brokerPath, ":provider"), h.SignAppleClientSecret)
	rig := &brokerRig{Engine: r, Redis: rdb}

	state := seedBrokerState(t, rig.Redis, "apple")
	body := fmt.Sprintf(`{"state":%q}`, state)

	// First attempt: signer fails -> 500, and the guard must be rolled back.
	first := postBroker(rig, "apple", body)
	require.Equal(t, http.StatusInternalServerError, first.Code, first.Body.String())
	assert.Contains(t, first.Body.String(), "internal_error")

	// Retry after the transient failure clears: must succeed, not 401-replay.
	provider.fail = false
	second := postBroker(rig, "apple", body)
	require.Equal(t, http.StatusOK, second.Code,
		"state must remain usable after a server-side signing failure")

	// One-shot still enforced after the successful mint.
	third := postBroker(rig, "apple", body)
	assert.Equal(t, http.StatusUnauthorized, third.Code)
}
