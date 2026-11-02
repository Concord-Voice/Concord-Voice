package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type dpopVectorFile struct {
	JKT   string `json:"jkt"`
	Now   int64  `json:"now"`
	Cases []struct {
		Name         string   `json:"name"`
		Method       string   `json:"method"`
		Path         string   `json:"path"`
		RefreshToken string   `json:"refresh_token"`
		SigningInput string   `json:"signing_input"`
		JWSParts     []string `json:"jws_parts"`
		Expect       string   `json:"expect"`
	} `json:"cases"`
}

// The same file drives the iOS XCTest suite, so a server change that breaks a
// vector breaks the frozen wire contract, not just this test.
func TestDPoPSharedVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/dpop_vectors.json")
	require.NoError(t, err)
	var v dpopVectorFile
	require.NoError(t, json.Unmarshal(raw, &v))
	require.NotEmpty(t, v.Cases)
	for _, tc := range v.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			require.Len(t, tc.JWSParts, 3)
			jws := strings.Join(tc.JWSParts, ".")
			require.Equal(t, tc.SigningInput, tc.JWSParts[0]+"."+tc.JWSParts[1])
			var rth []byte
			if tc.RefreshToken != "" {
				d := sha256.Sum256([]byte(tc.RefreshToken))
				rth = d[:]
			}
			jkt, jti, err := verifyDPoP(jws, tc.Method, tc.Path, rth, time.Unix(v.Now, 0))
			if tc.Expect == "ok" {
				require.NoError(t, err)
				assert.Equal(t, v.JKT, jkt)
				assert.NotEmpty(t, jti)
				return
			}
			var pe *popError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, tc.Expect, pe.code)
			assert.Empty(t, jkt)
			if pe.code == errCodePoPStaleIAT {
				assert.Equal(t, v.Now, pe.serverTime)
			}
		})
	}
}

func TestDPoPRejectsMalformedProofs(t *testing.T) {
	key := newDPoPTestKey(t)
	now := time.Now()
	good := key.proof(t, "POST", dpopTestOrigin+"/r", nil, now)
	for name, proof := range map[string]string{
		"empty":            "",
		"two parts":        "a.b",
		"oversize":         string(make([]byte, dpopMaxLen+1)),
		"padded signature": good[:len(good)-1] + "=",
		"bad claims":       key.signRaw(t, key.header(), "not-json"),
		"no iat":           key.signRaw(t, key.header(), `{"jti":"j","htm":"POST","htu":"https://x/r"}`),
		"empty jti":        key.signRaw(t, key.header(), `{"jti":"","htm":"POST","htu":"https://x/r","iat":1}`),
		"long jti":         key.signRaw(t, key.header(), `{"jti":"`+strings.Repeat("a", 65)+`","htm":"POST","htu":"https://x/r","iat":1}`),
		"extra claim":      key.signRaw(t, key.header(), `{"jti":"j","htm":"POST","htu":"https://x/r","iat":1,"nonce":"n"}`),
		"wrong curve":      key.signRaw(t, `{"typ":"dpop+jwt","alg":"ES256","jwk":{"kty":"EC","crv":"P-384","x":"`+key.x+`","y":"`+key.y+`"}}`, `{}`),
		"off curve":        key.signRaw(t, `{"typ":"dpop+jwt","alg":"ES256","jwk":{"kty":"EC","crv":"P-256","x":"`+key.x+`","y":"`+key.x+`"}}`, `{}`),
		"short x":          key.signRaw(t, `{"typ":"dpop+jwt","alg":"ES256","jwk":{"kty":"EC","crv":"P-256","x":"AA","y":"`+key.y+`"}}`, `{}`),
		"no jwk":           key.signRaw(t, `{"typ":"dpop+jwt","alg":"ES256"}`, `{}`),
		"alg none":         key.signRaw(t, `{"typ":"dpop+jwt","alg":"none","jwk":{}}`, `{}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := verifyDPoP(proof, "POST", "/r", nil, now)
			assert.ErrorIs(t, err, errPoPInvalid)
		})
	}
	_, _, err := verifyDPoP(good, "POST", "/r", nil, now)
	require.NoError(t, err, "the control proof must pass")
}

func TestDPoPHTUMatches(t *testing.T) {
	for htu, want := range map[string]bool{
		"https://a.example/api/v1/auth/refresh":       true,
		"http://127.0.0.1:8080/api/v1/auth/refresh":   true,
		"https://a.example/api/v1/auth/refresh?":      false,
		"https://a.example/api/v1/auth/refresh#f":     false,
		"https://u:p@a.example/api/v1/auth/refresh":   false,
		"wss://a.example/api/v1/auth/refresh":         false,
		"/api/v1/auth/refresh":                        false,
		"https://a.example/api/v1/auth/refresh/":      false,
		"https://a.example/api/v1/auth/%72efresh":     true, // path compared decoded
		"https://a.example/api/v1/auth/refresh%2Fx":   false,
		"https://a.example:bad/api/v1/auth/refresh":   false,
		"https://a.example/api/v1/auth/refresh?a=b#c": false,
	} {
		assert.Equal(t, want, dpopHTUMatches(htu, "/api/v1/auth/refresh"), htu)
	}
}

func TestConsumeDPoPJTIIsSingleUsePerKey(t *testing.T) {
	rdb := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), rdb))
	h := &Handler{redis: rdb}
	ctx := context.Background()
	require.NoError(t, h.consumeDPoPJTI(ctx, "jkt-a", "jti-1"))
	assert.ErrorIs(t, h.consumeDPoPJTI(ctx, "jkt-a", "jti-1"), errPoPReplayed)
	require.NoError(t, h.consumeDPoPJTI(ctx, "jkt-b", "jti-1"), "one key cannot burn another key's jti")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err := h.consumeDPoPJTI(cancelled, "jkt-a", "jti-2")
	require.Error(t, err)
	var pe *popError
	assert.False(t, errors.As(err, &pe), "a Redis failure is not a client proof error")
}

const dpopTestOrigin = "https://api.test"

type dpopTestKey struct {
	priv *ecdsa.PrivateKey
	x, y string
	jkt  string
}

func newDPoPTestKey(t *testing.T) dpopTestKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pub, err := priv.PublicKey.Bytes()
	require.NoError(t, err)
	k := dpopTestKey{priv: priv, x: b64.EncodeToString(pub[1:33]), y: b64.EncodeToString(pub[33:])}
	thumb := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + k.x + `","y":"` + k.y + `"}`))
	k.jkt = b64.EncodeToString(thumb[:])
	return k
}

func (k dpopTestKey) header() string {
	return `{"typ":"dpop+jwt","alg":"ES256","jwk":{"kty":"EC","crv":"P-256","x":"` + k.x + `","y":"` + k.y + `"}}`
}

func (k dpopTestKey) signRaw(t *testing.T, header, claims string) string {
	t.Helper()
	in := b64.EncodeToString([]byte(header)) + "." + b64.EncodeToString([]byte(claims))
	d := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, d[:])
	require.NoError(t, err)
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return in + "." + b64.EncodeToString(sig)
}

// proof signs a contract-shaped proof with a fresh jti. refreshToken nil means
// a login proof (no rth).
func (k dpopTestKey) proof(t *testing.T, htm, htu string, refreshToken *string, iat time.Time) string {
	t.Helper()
	claims := map[string]any{"jti": uuid.NewString(), "htm": htm, "htu": htu, "iat": iat.Unix()}
	if refreshToken != nil {
		d := sha256.Sum256([]byte(*refreshToken))
		claims["rth"] = b64.EncodeToString(d[:])
	}
	raw, err := json.Marshal(claims)
	require.NoError(t, err)
	return k.signRaw(t, k.header(), string(raw))
}

func TestWritePoPError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		err    error
		status int
		body   string
	}{
		{errPoPRequired, http.StatusUnauthorized, `{"code":"refresh_pop_required","error":"Session proof rejected"}`},
		{&popError{code: errCodePoPStaleIAT, serverTime: 42}, http.StatusUnauthorized, `{"code":"refresh_pop_stale_iat","error":"Session proof rejected","server_time":42}`},
		{errors.New("redis down"), http.StatusServiceUnavailable, `{"error":"Session proof check failed"}`},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		writePoPError(c, tc.err)
		assert.Equal(t, tc.status, w.Code)
		assert.JSONEq(t, tc.body, w.Body.String())
	}
	assert.Equal(t, errCodePoPInvalid, errPoPInvalid.Error())
	assert.Error(t, decodeStrictJSON(b64.EncodeToString([]byte(`{} {}`)), &dpopClaims{}), "trailing JSON is refused")
}
