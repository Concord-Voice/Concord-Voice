package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
)

const (
	hopPurpose = "concord/media-plane-service-hop/v1"
	hopVersion = "v1"
	hopBearer  = "media-plane-forwarded-user-token"
	// Literal, not middleware.ClientVersionHeader: this is a wire contract with
	// a different service in a different language, and only a literal makes a
	// Go-side rename fail a Go test. The same discipline caught a real bug in
	// development (X-Client-Version vs X-Concord-Client-Version).
	clientVersionHeaderLiteral = "X-Concord-Client-Version"
	channelJoinTemplate        = "/api/v1/channels/:id/voice/join"
	channelJoinPath            = "/api/v1/channels/ch-1/voice/join"
	dmAuthorizeTemplate        = "/api/v1/dm/conversations/:id/voice/authorize"
	dmAuthorizePath            = "/api/v1/dm/conversations/conv-1/voice/authorize"
	pinnedMinimum              = "0.2.44"
)

// Assembled from parts so the static credential detectors do not read a test
// fixture as a committed secret (same idiom as testdb.go).
var hopSecret = strings.Join([]string{"service", "hop", "test"}, "-")

// hopRouter mirrors the production chain: AuthRequired's effect (a user_id on
// the context), then the hop marker, then the gate it exempts. The ordering
// itself is pinned separately against router.go by
// internal/api/router_client_version_wiring_test.go — this harness exercises
// behaviour, that one exercises wiring, and neither substitutes for the other.
func hopRouter(secret, minimum string) (*gin.Engine, map[string]*bool) {
	r := gin.New()
	reached := map[string]*bool{}
	handler := func(key string) gin.HandlerFunc {
		flag := new(bool)
		reached[key] = flag
		return func(c *gin.Context) {
			*flag = true
			c.Status(http.StatusNoContent)
		}
	}
	authed := r.Group("/")
	authed.Use(func(c *gin.Context) { c.Set("user_id", "u-1"); c.Next() })
	authed.Use(middleware.MediaPlaneServiceHop(secret, nil))
	authed.Use(middleware.RequireClientVersion(minimum))
	authed.POST(channelJoinTemplate, handler("channel"))
	authed.POST(dmAuthorizeTemplate, handler("dm"))
	authed.DELETE(dmAuthorizeTemplate, handler("dm-delete"))
	authed.POST("/api/v1/servers/:id/roles", handler("unrelated"))
	return r, reached
}

type hop struct {
	timestamp, proof, bearer, method, uri string
}

func freshHop(method, uri string) hop {
	return hop{
		timestamp: strconv.FormatInt(time.Now().Unix(), 10),
		bearer:    hopBearer,
		method:    method,
		uri:       uri,
	}
}

func (h hop) signed(secret string) hop {
	h.proof = mediaproof.Sign(
		mediaproof.DeriveKey(secret, hopPurpose),
		hopVersion, h.timestamp, h.method, h.uri, mediaproof.TokenDigest(h.bearer),
	)
	return h
}

// send issues the request as the media plane would: NO client-version header,
// which is the omission that 403'd every voice join on 2026-09-02.
func send(r *gin.Engine, method, target string, h *hop, version string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if h != nil {
		req.Header.Set("Authorization", "Bearer "+h.bearer)
		req.Header.Set("X-Concord-Service-Timestamp", h.timestamp)
		req.Header.Set("X-Concord-Service-Proof", h.proof)
	}
	if version != "" {
		req.Header.Set(clientVersionHeaderLiteral, version)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// THE OUTAGE, AS A RED-GREEN TEST. Remove the IsMediaPlaneServiceHop early
// return from RequireClientVersion and this goes red — it is the only test here
// with that property, and it is what proves the fix works.
func TestProvenHopIsExemptFromTheVersionGate(t *testing.T) {
	r, reached := hopRouter(hopSecret, pinnedMinimum)
	h := freshHop(http.MethodPost, channelJoinPath).signed(hopSecret)

	w := send(r, http.MethodPost, channelJoinPath, &h, "")

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, *reached["channel"])
}

// The pre-fix behaviour, kept as a fail-closed guard rather than as the
// reproduction: it passes both before and after the fix, and its value is that
// it goes red if the exemption is ever granted unconditionally.
func TestUnprovenHopStillFailsTheVersionGate(t *testing.T) {
	r, reached := hopRouter(hopSecret, pinnedMinimum)

	w := send(r, http.MethodPost, channelJoinPath, nil, "")

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, *reached["channel"])
}

// The DM release. This path matters MORE than the join, not less: a gated
// failure here strands the call lease silently rather than failing a join
// loudly, so it gets a positive test of its own.
func TestProvenHopIsExemptOnTheDMAuthorizeRelease(t *testing.T) {
	r, reached := hopRouter(hopSecret, pinnedMinimum)
	h := freshHop(http.MethodDelete, dmAuthorizePath).signed(hopSecret)

	w := send(r, http.MethodDelete, dmAuthorizePath, &h, "")

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, *reached["dm-delete"])
}

func TestProvenHopIsExemptOnTheDMAuthorizeAdmission(t *testing.T) {
	r, reached := hopRouter(hopSecret, pinnedMinimum)
	h := freshHop(http.MethodPost, dmAuthorizePath).signed(hopSecret)

	w := send(r, http.MethodPost, dmAuthorizePath, &h, "")

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, *reached["dm"])
}

// The exemption is scoped to the two voice endpoints. The middleware is
// registered group-wide, so without the allowlist a proof would be honoured on
// any route beneath it — broader than the middleware's own doc comment claims.
func TestProofIsIgnoredOnRoutesOutsideTheAllowlist(t *testing.T) {
	r, reached := hopRouter(hopSecret, pinnedMinimum)
	target := "/api/v1/servers/s-1/roles"
	h := freshHop(http.MethodPost, target).signed(hopSecret)

	w := send(r, http.MethodPost, target, &h, "")

	assert.Equal(t, http.StatusForbidden, w.Code, "a valid proof must not exempt an unrelated route")
	assert.False(t, *reached["unrelated"])
}

// Every bound field must be load-bearing: a proof minted for one request must
// not carry another. Each case signs a mutated request and replays it against
// the original, so a pass means that field is not actually part of the MAC.
func TestHopProofDoesNotTransferAcrossRequests(t *testing.T) {
	for name, mutate := range map[string]func(*hop){
		"different bearer token": func(h *hop) { h.bearer = "someone-elses-token" },
		"different path":         func(h *hop) { h.uri = "/api/v1/channels/ch-2/voice/join" },
		"different method":       func(h *hop) { h.method = http.MethodDelete },
		"appended query string":  func(h *hop) { h.uri = channelJoinPath + "?attacker=chosen" },
	} {
		t.Run(name, func(t *testing.T) {
			r, reached := hopRouter(hopSecret, pinnedMinimum)
			h := freshHop(http.MethodPost, channelJoinPath)
			mutate(&h)
			h = h.signed(hopSecret)
			h.bearer = hopBearer // replay against the ORIGINAL request shape

			w := send(r, http.MethodPost, channelJoinPath, &h, "")

			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.False(t, *reached["channel"])
		})
	}
}

// The reverse of the query-string case above: a proof for the bare path must
// not be replayable at the same path WITH a query string appended. Binding
// RequestURI rather than Path is what closes this.
func TestHopProofDoesNotReplayWithAnAppendedQueryString(t *testing.T) {
	r, reached := hopRouter(hopSecret, pinnedMinimum)
	h := freshHop(http.MethodPost, channelJoinPath).signed(hopSecret)

	w := send(r, http.MethodPost, channelJoinPath+"?attacker=chosen", &h, "")

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, *reached["channel"])
}

func TestHopProofIsRejectedWhenNotMintedWithTheSharedSecret(t *testing.T) {
	r, reached := hopRouter(hopSecret, pinnedMinimum)
	h := freshHop(http.MethodPost, channelJoinPath).signed("an-attacker-guess")

	w := send(r, http.MethodPost, channelJoinPath, &h, "")

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, *reached["channel"])
}

func TestHopProofIsRejectedOutsideTheSkewWindow(t *testing.T) {
	r, reached := hopRouter(hopSecret, pinnedMinimum)
	h := freshHop(http.MethodPost, channelJoinPath)
	h.timestamp = strconv.FormatInt(time.Now().Add(-2*mediaproof.MaxClockSkew).Unix(), 10)
	h = h.signed(hopSecret)

	w := send(r, http.MethodPost, channelJoinPath, &h, "")

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, *reached["channel"])
}

// An unconfigured secret must not become a wildcard: with no key derivable,
// nothing can be proven and the client gate applies to everyone.
func TestUnconfiguredSecretGrantsNoExemption(t *testing.T) {
	r, reached := hopRouter("", pinnedMinimum)
	h := freshHop(http.MethodPost, channelJoinPath).signed("")

	w := send(r, http.MethodPost, channelJoinPath, &h, "")

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, *reached["channel"])
}

// A rejected proof must not fail the request outright — it simply earns no
// exemption. A caller that also supplies a valid version still gets through,
// which is what keeps a clock-skew blip or a mid-deploy secret rotation from
// becoming a second outage.
//
// Read as a PAIR with TestHopProofIsRejectedWhenNotMintedWithTheSharedSecret:
// that one proves the same invalid proof yields 403 without a version header,
// so together they show the request fell through to the gate rather than being
// exempted. Neither is load-bearing alone.
func TestAnInvalidProofFallsBackToTheClientGateRatherThanFailing(t *testing.T) {
	r, reached := hopRouter(hopSecret, pinnedMinimum)
	h := freshHop(http.MethodPost, channelJoinPath).signed("wrong-secret")

	w := send(r, http.MethodPost, channelJoinPath, &h, pinnedMinimum)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, *reached["channel"])
}

// Registered before AuthRequired there is no authenticated user to bind a proof
// to, so the middleware must decline rather than mark the request. The wiring
// test pins the real ordering; this pins the behaviour if it is ever wrong.
func TestProofIsIgnoredWhenNoAuthenticatedUserIsOnTheContext(t *testing.T) {
	r := gin.New()
	reached := new(bool)
	r.Use(middleware.MediaPlaneServiceHop(hopSecret, nil))
	r.Use(middleware.RequireClientVersion(pinnedMinimum))
	r.POST(channelJoinTemplate, func(c *gin.Context) { *reached = true; c.Status(http.StatusNoContent) })
	h := freshHop(http.MethodPost, channelJoinPath).signed(hopSecret)

	w := send(r, http.MethodPost, channelJoinPath, &h, "")

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, *reached)
}

func TestIsMediaPlaneServiceHopIsFalseWhenTheMiddlewareNeverRan(t *testing.T) {
	r := gin.New()
	var observed bool
	r.GET("/no-hop-middleware", func(c *gin.Context) {
		observed = middleware.IsMediaPlaneServiceHop(c)
		c.Status(http.StatusNoContent)
	})

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/no-hop-middleware", nil))

	assert.False(t, observed, "a route group without the middleware must never grant the exemption")
}

// The header names are a cross-language wire contract with the media plane.
// Pinning the literals is the only mechanism that makes a Go-side rename fail a
// Go test — there is no shared type and no generated client.
func TestWireHeaderNamesArePinned(t *testing.T) {
	r, _ := hopRouter(hopSecret, pinnedMinimum)
	h := freshHop(http.MethodPost, channelJoinPath).signed(hopSecret)
	req := httptest.NewRequest(http.MethodPost, channelJoinPath, nil)
	req.Header.Set("Authorization", "Bearer "+h.bearer)
	req.Header.Set("X-Concord-Service-Timestamp", h.timestamp)
	req.Header.Set("X-Concord-Service-Proof", h.proof)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code,
		"the literal header names above are what the media plane sends")
	assert.Equal(t, clientVersionHeaderLiteral, middleware.ClientVersionHeader,
		"the version header's literal value is a cross-service contract")
}
