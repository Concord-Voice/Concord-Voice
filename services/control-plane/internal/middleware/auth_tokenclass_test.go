package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

// Bearer authentication must be bound to the ACCESS-token class, not merely to a
// valid HMAC signature. router.go hands one cfg.JWTSecret to both mfa.NewHandler and
// middleware.AuthRequired, and every MFA/recovery challenge carries `user_id` — so
// before the issuer check, an `mfa_challenge_token` handed to an unauthenticated
// caller by POST /api/v1/auth/login authenticated as the victim on every protected
// route for 5 minutes (a complete MFA bypass), and a recovery token did so for 25
// hours.
//
// These tests are deliberately in the EXTERNAL test package so they can mint through
// the REAL mfa generators (mfa imports auth imports middleware, so `package
// middleware` could not). Hand-rolling a lookalike JWT here would let the fixture
// drift from the production token shape and go on passing while the real hole
// reopened — the assertion would be right and the violating path never reached.
const (
	tokenClassSecret = "shared-hmac-secret-for-both-token-families" //nolint:gosec // test fixture, not a real credential // pragma: allowlist secret
	tokenClassUser   = "11111111-1111-4111-8111-111111111111"
)

// realChallengeTokens returns every token the MFA package can mint with the shared
// secret, keyed by the flow that produces it.
func realChallengeTokens(t *testing.T) map[string]string {
	t.Helper()

	login, _, err := mfa.GenerateChallengeToken(
		tokenClassUser, mfa.PurposeLogin, mfa.JWTSecret(tokenClassSecret), "")
	require.NoError(t, err)

	suspicious, _, err := mfa.GenerateChallengeToken(
		tokenClassUser, mfa.PurposeSuspiciousRefresh, mfa.JWTSecret(tokenClassSecret), "")
	require.NoError(t, err)

	upgrade, _, err := mfa.GenerateChallengeToken(
		tokenClassUser, mfa.PurposeMFAUpgrade, mfa.JWTSecret(tokenClassSecret), "")
	require.NoError(t, err)

	recovery, _, err := mfa.GenerateRecoveryToken(tokenClassUser, mfa.JWTSecret(tokenClassSecret))
	require.NoError(t, err)

	return map[string]string{
		"login challenge (POST /auth/login on an MFA account)": login,
		"suspicious-refresh challenge":                         suspicious,
		"mfa-upgrade challenge":                                upgrade,
		"account-recovery token (25h TTL)":                     recovery,
	}
}

// realAccessToken is the positive control. Without it these tests would pass just as
// happily against a mistakenly always-false check, proving nothing.
func realAccessToken(t *testing.T) string {
	t.Helper()
	tok, err := auth.GenerateAccessToken(tokenClassUser, tokenClassSecret, true, "", "")
	require.NoError(t, err)
	return tok
}

// parseWithSharedSecret verifies the signature and returns the raw claim map, which
// is exactly what AuthRequired and the WebSocket handshake both hand to IsAccessToken.
func parseWithSharedSecret(t *testing.T, token string) jwt.MapClaims {
	t.Helper()
	parsed, err := jwt.Parse(token, func(*jwt.Token) (interface{}, error) {
		return []byte(tokenClassSecret), nil
	})
	require.NoError(t, err, "fixture must be a validly signed token — otherwise the "+
		"signature check rejects it and IsAccessToken is never reached")
	require.True(t, parsed.Valid)
	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok)
	return claims
}

func authRequiredUnderTest(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { assert.NoError(t, rdb.Close()) })

	r := gin.New()
	// nil fence: the credential-epoch fence is NOT what rejects these tokens, and
	// passing one would mask which guard actually fired.
	r.GET("/protected", middleware.AuthRequired(tokenClassSecret, rdb, nil), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"user_id": c.GetString("user_id")})
	})
	return r
}

func getProtected(t *testing.T, r *gin.Engine, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestAuthRequiredRejectsNonAccessTokenFamilies(t *testing.T) {
	r := authRequiredUnderTest(t)

	for name, token := range realChallengeTokens(t) {
		t.Run(name, func(t *testing.T) {
			rec := getProtected(t, r, token)

			require.Equal(t, http.StatusUnauthorized, rec.Code,
				"a %s is signed with the bearer secret and carries user_id; "+
					"accepting it as an access token is an authentication bypass", name)
			assert.NotContains(t, rec.Body.String(), tokenClassUser,
				"a rejected request must not leak the subject it would have authenticated as")
		})
	}
}

// The positive control. If AuthRequired ever rejects a genuine access token, the
// suite above becomes vacuous — it would pass against a middleware that refuses
// everything, which is not the property under test.
func TestAuthRequiredStillAcceptsGenuineAccessToken(t *testing.T) {
	r := authRequiredUnderTest(t)

	rec := getProtected(t, r, realAccessToken(t))

	require.Equal(t, http.StatusOK, rec.Code,
		"the issuer check must narrow the accepted set to access tokens, not empty it")
	assert.Contains(t, rec.Body.String(), tokenClassUser)
}

// IsAccessToken is exported because the WebSocket handshake shares this boundary
// (websocket.authenticateViaJWT). Pinning it directly keeps that second caller
// covered even though its own path needs a live upgrade to exercise end to end.
func TestIsAccessTokenPartitionsTheTwoMints(t *testing.T) {
	t.Run("rejects every mfa-family token", func(t *testing.T) {
		for name, token := range realChallengeTokens(t) {
			claims := parseWithSharedSecret(t, token)
			assert.False(t, middleware.IsAccessToken(claims), "%s must not pass as an access token", name)
		}
	})

	t.Run("accepts a genuine access token", func(t *testing.T) {
		claims := parseWithSharedSecret(t, realAccessToken(t))
		assert.True(t, middleware.IsAccessToken(claims))
	})

	t.Run("rejects a control-plane-issuer token that carries a purpose", func(t *testing.T) {
		// The defense-in-depth half: a future mint that adopted the control-plane
		// issuer for a purpose-bound token must still be refused, so the boundary
		// does not rest on one string comparison.
		claims := parseWithSharedSecret(t, realAccessToken(t))
		claims["purpose"] = "login"
		assert.False(t, middleware.IsAccessToken(claims))
	})

	t.Run("rejects claims with no issuer at all", func(t *testing.T) {
		assert.False(t, middleware.IsAccessToken(jwt.MapClaims{"user_id": tokenClassUser}))
	})
}

// ValidateAccessToken is the third reader (behind Logout's blacklistAccessToken).
// Gitar and CodeRabbit independently flagged that it enforced only the issuer half
// of the boundary. A boundary that differs by surface is one refactor away from
// being the surface an attacker picks, so it is pinned here on the same terms as
// the untyped path — including the encodings a string-typed field could not see.
func TestValidateAccessTokenEnforcesTheSameBoundary(t *testing.T) {
	t.Run("accepts a genuine access token", func(t *testing.T) {
		claims, err := auth.ValidateAccessToken(realAccessToken(t), tokenClassSecret)
		require.NoError(t, err)
		assert.Equal(t, tokenClassUser, claims.UserID)
	})

	t.Run("rejects every mfa-family token", func(t *testing.T) {
		for name, token := range realChallengeTokens(t) {
			_, err := auth.ValidateAccessToken(token, tokenClassSecret)
			assert.Error(t, err, "%s must not validate as an access token", name)
		}
	})

}

// purposeEncodings is the shared table for BOTH readers. Three cases, each earning
// its place rather than enumerating JSON's type system: `string` is the shape the
// original bug caught, `number` stands for every other non-string (the guard has no
// type branch, so one representative is the whole class), and `null` is genuinely
// distinct — encoding/json invokes UnmarshalJSON for a null, so json.RawMessage
// receives the four bytes `null` while a string field would have decoded it to "".
var purposeEncodings = map[string]interface{}{
	"string": "login",
	"number": float64(123),
	"null":   nil,
}

// Both readers answer the same question and must answer it the same way. They are
// asserted against one table so a future change cannot quietly harden one surface
// and leave the other permissive.
func TestBothReadersRejectAnyPurposeClaim(t *testing.T) {
	for name, val := range purposeEncodings {
		t.Run("untyped/"+name, func(t *testing.T) {
			claims := parseWithSharedSecret(t, realAccessToken(t))
			claims["purpose"] = val
			assert.False(t, middleware.IsAccessToken(claims),
				"purpose encoded as %s must be rejected — the guard tests key presence", name)
		})
		t.Run("typed/"+name, func(t *testing.T) {
			now := time.Now()
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
				"iss":     middleware.AccessTokenIssuer,
				"user_id": tokenClassUser,
				"purpose": val,
				"exp":     now.Add(15 * time.Minute).Unix(),
				"iat":     now.Unix(),
			})
			// nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key — test fixture, not a real credential
			signed, err := tok.SignedString([]byte(tokenClassSecret))
			require.NoError(t, err)
			_, verr := auth.ValidateAccessToken(signed, tokenClassSecret)
			assert.Error(t, verr, "purpose encoded as %s must be rejected by the typed reader too", name)
		})
	}
}

// KNOWN, ACCEPTED, NOT EXPLOITABLE — pinned so it is discovered deliberately.
//
// IsAccessTokenClass shares the RULE between the two readers, but each derives the
// rule's inputs itself, and the derivations disagree on claim-key CASE:
//
//	untyped: `_, ok := claims["purpose"]`  — exact map-key lookup
//	typed:   struct decode, where encoding/json falls back to CASE-INSENSITIVE
//	         field matching when no exact tag match exists
//
// So `{"Purpose":"login"}` is invisible to the map path and lands in Claims.Purpose,
// and `{"Iss":"..."}` is invisible to the map path but fills RegisteredClaims.Issuer.
// Extracting the kernel did NOT close this — the divergence is in the extraction, not
// the rule (measured, not assumed).
//
// It is not exploitable: no mint emits a capitalised key, so presenting one requires
// signing it, and anyone holding JWT_SECRET can mint an ordinary access token instead.
// Closing it means the typed reader must stop trusting struct decoding for these two
// claims — decode to map[string]json.RawMessage and derive both from exact keys — which
// is real complexity for a case no attacker can reach. Recorded rather than built.
//
// If this test fails, someone changed the extraction. That is likely an IMPROVEMENT;
// update the expectations deliberately rather than restoring the old behaviour.
func TestReaderExtractionDivergesOnClaimKeyCase(t *testing.T) {
	for _, tc := range []struct {
		name           string
		key, val       string
		wantUntyped    bool
		wantTypedValid bool
	}{
		{"capitalised purpose hides from the map path", "Purpose", "login", true, false},
		{"capitalised iss hides from the map path", "Iss", middleware.AccessTokenIssuer, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			claims := jwt.MapClaims{
				"user_id": tokenClassUser,
				"exp":     now.Add(15 * time.Minute).Unix(),
				"iat":     now.Unix(),
				tc.key:    tc.val,
			}
			if tc.key != "Iss" {
				claims["iss"] = middleware.AccessTokenIssuer
			}
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
			// nosemgrep: go.jwt-go.security.jwt.hardcoded-jwt-key — test fixture, not a real credential
			signed, err := tok.SignedString([]byte(tokenClassSecret))
			require.NoError(t, err)

			assert.Equal(t, tc.wantUntyped, middleware.IsAccessToken(parseWithSharedSecret(t, signed)),
				"untyped reader")
			_, verr := auth.ValidateAccessToken(signed, tokenClassSecret)
			assert.Equal(t, tc.wantTypedValid, verr == nil, "typed reader")
		})
	}
}
