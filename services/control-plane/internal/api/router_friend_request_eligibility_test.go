package api_test

// Spec T8 / T9 / AC-6: route construction and rate-limit budget for
// GET /api/v1/users/:user_id/friend-request-eligibility (#1240).
//
// The failure this file exists to catch is a BOOT failure, not a serving
// failure. gin builds one radix tree per method, and two routes that reach the
// same tree position with differently-named wildcards panic at REGISTRATION.
// The sibling routes at that position are /:user_id/public-key and
// /:user_id/profile, so writing ":id" — which both issue bodies did, and which
// reads perfectly naturally — takes the whole control-plane down at startup
// with no request ever served. A route test that only drove requests would
// never see it, because the process would already be dead.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

const (
	eligibilityFullPath  = "/api/v1/users/:user_id/friend-request-eligibility"
	sendRequestFullPath  = "/api/v1/friends/request"
	eligibilityRateLimit = 10
	eligibilityWindow    = 1 * time.Minute
)

// TestNewRouterRegistersFriendRequestEligibilityWithUserIDParam is T8 proper.
//
// Constructing the real router is the assertion: a mis-named wildcard panics
// inside NewRouter, so the test dies at setup rather than at an assertion. The
// FullPath check that follows pins WHICH name won — a router that had settled
// on ":id" everywhere would build fine and break every sibling handler's
// c.Param("user_id") instead.
func TestNewRouterRegistersFriendRequestEligibilityWithUserIDParam(t *testing.T) {
	var ts *testhelpers.TestServer
	require.NotPanics(t, func() { ts = testhelpers.SetupTestServer(t) },
		"NewRouter must build; a wildcard named anything but :user_id at this tree "+
			"position panics here and the control-plane fails to BOOT")

	var found bool
	for _, route := range ts.Router.Routes() {
		if route.Path == eligibilityFullPath {
			assert.Equal(t, http.MethodGet, route.Method,
				"the eligibility probe is a read; a non-GET here breaks the client's cache "+
					"and the per-method rate-limit key")
			found = true
		}
	}
	require.True(t, found,
		"no route registered at %s. The desktop client codes against this exact path "+
			"(§6.1) and degrades OPEN on a 404, so a missing route silently disables the "+
			"whole gate client-side instead of failing loudly", eligibilityFullPath)
}

// TestDifferentlyNamedWildcardAtTheUserIDPositionPanics is the POSITIVE CONTROL
// for the test above.
//
// Without it, "a wrong param name panics at construction" is folklore that the
// NotPanics assertion can never demonstrate — an assertion that something does
// not happen proves nothing unless the thing can be made to happen. This builds
// the collision deliberately, against the real sibling route, and needs no
// database.
func TestDifferentlyNamedWildcardAtTheUserIDPositionPanics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	noOp := func(c *gin.Context) { c.Status(http.StatusOK) }

	router := gin.New()
	users := router.Group("/api/v1/users")
	users.GET("/:user_id/profile", noOp)

	require.Panics(t, func() {
		users.GET("/:id/friend-request-eligibility", noOp)
	}, "gin must reject a second wildcard name at the same tree position — this is the "+
		"boot failure TestNewRouterRegistersFriendRequestEligibilityWithUserIDParam guards")

	// The correctly-named sibling coexists, which is what makes the panic above
	// attributable to the NAME rather than to the path segment.
	require.NotPanics(t, func() {
		users.GET("/:user_id/friend-request-eligibility", noOp)
	})
}

// TestEligibilityProbeBudgetIsSeparateFromTheSendBudget is T9.
//
// RateLimitByUser keys on ratelimit:user:<id>:<method>:<FullPath>, so the two
// budgets are structurally distinct. What this actually catches is the
// regression that matters: someone attaching a SHARED limiter to both, which
// would let a member-list scroll consume the user's ability to send requests —
// or the reverse, which would make probing free.
//
// Deliberately decoupled from NewRouter (the klipy-limiter precedent in
// router_klipy_ratelimit_test.go): the invariant under test is the middleware's
// per-route key derivation, and miniredis supplies a keyspace with no network
// to fail open on.
//
// NOTE: eligibilityRateLimit below is a local MIRROR of router.go's budget, not
// a pin on it. A source-regex lock used to tie the two together and was removed
// as brittle (it broke on any gofmt-neutral rename). So if router.go's budget
// changes, this test keeps passing at the old number — what it still proves is
// the property that matters here, that the two routes draw on SEPARATE buckets,
// which holds at any budget. The value itself is pinned only by review and by
// the OpenAPI description.
func TestEligibilityProbeBudgetIsSeparateFromTheSendBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	const userID = "eligibility-budget-user"
	noOp := func(c *gin.Context) { c.Status(http.StatusOK) }
	fakeAuth := func(c *gin.Context) { c.Set("user_id", userID) }

	router := gin.New()
	router.GET(eligibilityFullPath, fakeAuth,
		middleware.RateLimitByUser(client, eligibilityRateLimit, eligibilityWindow), noOp)
	router.POST(sendRequestFullPath, fakeAuth,
		middleware.RateLimitByUser(client, eligibilityRateLimit, eligibilityWindow), noOp)

	probe := func() int {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(
			http.MethodGet, "/api/v1/users/"+userID+"/friend-request-eligibility", nil))
		return w.Code
	}

	for i := 1; i <= eligibilityRateLimit; i++ {
		require.Equal(t, http.StatusOK, probe(), "probe %d/%d must be admitted",
			i, eligibilityRateLimit)
	}
	assert.Equal(t, http.StatusTooManyRequests, probe(),
		"the %dth probe in the window must be refused", eligibilityRateLimit+1)

	// The send budget is untouched by the exhausted probe budget. If it were
	// not, a client that prefetched eligibility for a member list would lose the
	// ability to send the request the prefetch exists to enable.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, sendRequestFullPath, nil))
	assert.Equal(t, http.StatusOK, w.Code,
		"probing must never consume the POST /friends/request budget (§6.1)")
}
