package klipy_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/klipy"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAppKey         = "test-app-key-123"
	pathSearch         = "/klipy/gifs/search"
	pathTrending       = "/klipy/gifs/trending"
	pathMedia          = "/klipy/media"
	pathMediaURL       = "/klipy/media?url="
	pathRecentBadSpace = "/klipy/gifs/recent/has%20space"
	pathShareBadSpace  = "/klipy/gifs/share/has%20space"
	pathReportBadSpace = "/klipy/gifs/report/has%20space"

	contentTypeHeader = "Content-Type"
	mimeJSON          = "application/json"

	// testUserHeader stands in for the auth middleware, which sets "user_id" on
	// the gin context for every /klipy route (they live under `protected`).
	// Varying it is how these tests model two different authenticated accounts.
	testUserHeader = "X-Test-User-ID"
)

// newTestHandler returns a klipy.Handler whose upstream HTTP client points at
// the given httptest.Server. This is done by overriding the package-level
// klipyAPIBase via the Config — but since the constants are unexported, we
// instead test by calling the public methods through a Gin router and asserting
// the response shape.
//
// For tests that need to capture the upstream URL, we use a real handler with
// a fake config and rely on the upstream call failing predictably.
func newTestHandler(_ *testing.T) *klipy.Handler {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{KlipyAPIKey: testAppKey}
	log := logger.New("test")
	return klipy.NewHandler(cfg, log)
}

func newRouter(h *klipy.Handler) *gin.Engine {
	r := gin.New()
	// Stand-in for middleware.RequireAuth, which is what puts "user_id" on the
	// context in production. Without it every request is unauthenticated and
	// the handlers fail closed, which is itself asserted below.
	r.Use(func(c *gin.Context) {
		if u := c.GetHeader(testUserHeader); u != "" {
			c.Set("user_id", u)
		}
	})
	r.GET("/klipy/gifs/trending", h.Trending)
	r.GET("/klipy/gifs/search", h.Search)
	r.GET("/klipy/gifs/categories", h.Categories)
	r.GET("/klipy/gifs/recent/:customerID", h.Recent)
	r.DELETE("/klipy/gifs/recent/:customerID", h.HideRecent)
	r.GET("/klipy/gifs/items", h.Items)
	r.POST("/klipy/gifs/share/:slug", h.Share)
	r.POST("/klipy/gifs/report/:slug", h.Report)
	r.GET("/klipy/randomid", h.RandomID)
	r.POST("/klipy/customer-id", h.CustomerID)
	r.GET("/klipy/media", h.Media)
	return r
}

// CustomerID mints a fresh random ROTATION NONCE for the client to store, not
// the partition key that reaches KLIPY. Each call must yield a new value —
// that is what makes the Rotate button in Settings actually rotate.
func TestCustomerIDMintsAFreshNonce(t *testing.T) {
	h := newTestHandler(t)
	r := newRouter(h)

	mint := func() string {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/klipy/customer-id", nil)
		req.Header.Set(testUserHeader, "alice")
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		return w.Body.String()
	}

	first := mint()
	assert.Regexp(t, `"customer_id":"[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}"`, first)
	assert.NotEqual(t, first, mint(), "rotation returned the same nonce twice")
}

func TestSearchRequiresQ(t *testing.T) {
	h := newTestHandler(t)
	r := newRouter(h)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathSearch, nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "q parameter is required")
}

func TestSearchQTooLong(t *testing.T) {
	h := newTestHandler(t)
	r := newRouter(h)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathSearch+"?q="+strings.Repeat("a", 200), nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "exceeds maximum length")
}

// A malformed :customerID used to earn a 400. It now earns nothing at all: the
// param is discarded before it can be validated, so the payload cannot reach
// upstream by construction rather than by a check remembering to run. The
// safety property these two tests were written to hold is unchanged and now
// rests on something stronger, so they assert the property, not the 400.
func TestRecentDiscardsMalformedCustomerID(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		lastUpstream = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathRecentBadSpace, nil)
		req.Header.Set(testUserHeader, "some-user")
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, lastUpstream)
		assert.NotContains(t, lastUpstream.Path, "space")
		assert.NotContains(t, lastUpstream.Path, "%20")
	})
}

func TestHideRecentDiscardsMalformedCustomerID(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		lastUpstream = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest("DELETE", pathRecentBadSpace, nil)
		req.Header.Set(testUserHeader, "some-user")
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, lastUpstream)
		assert.NotContains(t, lastUpstream.Path, "space")
	})
}

func TestShareInvalidSlug(t *testing.T) {
	h := newTestHandler(t)
	r := newRouter(h)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", pathShareBadSpace, nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReportInvalidSlug(t *testing.T) {
	h := newTestHandler(t)
	r := newRouter(h)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", pathReportBadSpace, nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestMediaRequiresURL(t *testing.T) {
	h := newTestHandler(t)
	r := newRouter(h)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathMedia, nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "url parameter is required")
}

func TestMediaRejectsHTTPScheme(t *testing.T) {
	h := newTestHandler(t)
	r := newRouter(h)
	w := httptest.NewRecorder()
	target := url.QueryEscape("http://media.klipy.com/abc.gif")
	req := httptest.NewRequest("GET", pathMediaURL+target, nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid url")
}

func TestMediaRejectsNonKlipyHost(t *testing.T) {
	h := newTestHandler(t)
	r := newRouter(h)
	for _, badURL := range []string{
		"https://example.com/foo.gif",
		"https://giphy.com/foo.gif",
		"https://api.klipy.com.evil.com/foo.gif",
		"https://klipy.com.attacker.com/foo.gif",
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathMediaURL+url.QueryEscape(badURL), nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code, "should reject host: %s", badURL)
		assert.Contains(t, w.Body.String(), "host not allowed")
	}
}

func TestMediaAcceptsKlipyCDNHosts(t *testing.T) {
	// We can't actually fetch from real KLIPY in unit tests, but we can verify
	// the handler accepts the host validation step. The upstream call will
	// fail (DNS or network), and the handler maps that to 502.
	h := newTestHandler(t)
	r := newRouter(h)
	for _, goodHost := range []string{
		"https://api.klipy.com/path.gif",
		"https://media.klipy.com/path.gif",
		"https://media0.klipy.com/path.gif",
		"https://media9.klipy.com/path.gif",
		"https://content.klipy.com/path.gif",
		"https://cdn.klipy.com/path.gif",
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathMediaURL+url.QueryEscape(goodHost), nil)
		r.ServeHTTP(w, req)
		// We expect 502 (upstream unreachable in test) — NOT 400 (host rejected).
		// 200 would also be acceptable if a network round-trip somehow succeeded.
		assert.NotEqual(t, http.StatusBadRequest, w.Code, "host should be allowed: %s", goodHost)
	}
}

// TestProxy_AppKeyNeverInResponse verifies the upstream URL is built but the
// app_key never leaks back to the client. We use a fake upstream server that
// echoes the path it received, then assert the app_key appears in the path
// (proving the proxy injected it) but NOT in the response body returned to the
// caller (proving we don't leak it). For this we need to inject a fake base
// URL — since the constant is unexported, we test it indirectly by capturing
// the upstream request via a custom Transport on the http.Client.
//
// Since we can't swap the base URL without exposing internals, we instead
// verify the app key handling at the route level: the test handler is built
// with KlipyAPIKey="test-app-key-123" and we make sure that string never
// appears in the response body for any error path.
func TestProxyAppKeyNeverInErrorResponse(t *testing.T) {
	h := newTestHandler(t)
	r := newRouter(h)

	// Trigger validation errors that go through the response body builder
	for _, path := range []string{
		pathSearch,         // 400 missing q
		pathRecentBadSpace, // 400 invalid customer_id
		pathMedia,          // 400 missing url
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		r.ServeHTTP(w, req)
		assert.NotContains(t, w.Body.String(), testAppKey, "app key leaked in response for %s", path)
	}
}

// TestProxyNoAppKeyLeakOnUpstreamError verifies that even when the upstream
// returns an error (4xx with our fake app_key in the path, or 5xx, or network
// failure), the response body returned to the client never contains the app_key.
// In practice the test environment may either reach KLIPY (and get 403 for the
// fake key) or fail to resolve DNS (and get 502 from us). Both are acceptable —
// we just need to confirm the app_key never appears in either case.
func TestProxyNoAppKeyLeakOnUpstreamError(t *testing.T) {
	h := newTestHandler(t)
	r := newRouter(h)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathTrending, nil)
	r.ServeHTTP(w, req)
	// The handler should return a sensible status and never echo the app_key
	// back to the client, regardless of what KLIPY actually returned upstream.
	assert.NotContains(t, w.Body.String(), testAppKey,
		"app key leaked in response body for status %d", w.Code)
}

// Smoke test: handler construction does not panic.
func TestNewHandler(t *testing.T) {
	cfg := &config.Config{KlipyAPIKey: "x"}
	log := logger.New("test")
	h := klipy.NewHandler(cfg, log)
	require.NotNil(t, h)
}

// --- Upstream-mock tests ---
//
// These tests substitute a local httptest server for KLIPY's API by overriding
// the package variables klipyAPIBase and allowedMediaHosts. They cover the
// proxy success/error paths that the validation-only tests above can't reach.

// capturedRequest records what the fake upstream actually received, so tests
// can assert on the body and headers rather than only the status.
type capturedRequest struct {
	Method      string
	Path        string
	RawQuery    string
	ContentType string
	Body        string
}

// lastUpstream is written by the mock handler and read by the assertions.
// Tests using it run sequentially within a single withMockUpstream closure.
var lastUpstream *capturedRequest

// withMockUpstream spins up an httptest TLS server (so the URL scheme is
// https://, satisfying the production media-proxy validation), swaps it into
// the package variables, and runs the test. State is restored at the end of
// each test. The handler's shared http.Client is reconfigured to skip TLS
// verification of the test cert.
func withMockUpstream(t *testing.T, fn func(t *testing.T, h *klipy.Handler, r *gin.Engine)) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Echo behavior for testing — return a fixed JSON payload that mirrors
		// KLIPY's documented response shape.
		switch {
		case req.URL.Path == "/"+testAppKey+"/gifs/trending":
			lastUpstream = &capturedRequest{Method: req.Method, Path: req.URL.Path, RawQuery: req.URL.RawQuery}
			w.Header().Set(contentTypeHeader, mimeJSON)
			_, _ = w.Write([]byte(`{"data":[{"slug":"trend-1"}],"has_more":false}`))
		case req.URL.Path == "/"+testAppKey+"/gifs/search":
			lastUpstream = &capturedRequest{Method: req.Method, Path: req.URL.Path, RawQuery: req.URL.RawQuery}
			w.Header().Set(contentTypeHeader, mimeJSON)
			_, _ = w.Write([]byte(`{"data":[{"slug":"search-1"}],"has_more":false}`))
		case req.URL.Path == "/"+testAppKey+"/gifs/categories":
			w.Header().Set(contentTypeHeader, mimeJSON)
			_, _ = w.Write([]byte(`{"data":[{"name":"Reactions"}]}`))
		case req.URL.Path == "/"+testAppKey+"/randomid":
			w.Header().Set(contentTypeHeader, mimeJSON)
			_, _ = w.Write([]byte(`{"data":{"random_id":"mock-id"}}`))
		case req.URL.Path == "/"+testAppKey+"/gifs/items":
			lastUpstream = &capturedRequest{Method: req.Method, Path: req.URL.Path, RawQuery: req.URL.RawQuery}
			w.Header().Set(contentTypeHeader, mimeJSON)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasPrefix(req.URL.Path, "/"+testAppKey+"/gifs/share/"),
			strings.HasPrefix(req.URL.Path, "/"+testAppKey+"/gifs/report/"):
			raw, _ := io.ReadAll(req.Body)
			lastUpstream = &capturedRequest{
				Method:      req.Method,
				Path:        req.URL.Path,
				RawQuery:    req.URL.RawQuery,
				ContentType: req.Header.Get("Content-Type"),
				Body:        string(raw),
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasPrefix(req.URL.Path, "/"+testAppKey+"/gifs/recent/"):
			lastUpstream = &capturedRequest{
				Method:   req.Method,
				Path:     req.URL.Path,
				RawQuery: req.URL.RawQuery,
			}
			w.Header().Set(contentTypeHeader, mimeJSON)
			_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
		case req.URL.Path == "/upstream-500":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"upstream broke", "url":"https://api.klipy.com/api/v1/test-app-key-123/gifs/trending"}`))
		case req.URL.Path == "/upstream-403":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"forbidden", "key":"test-app-key-123"}`))
		case strings.HasPrefix(req.URL.Path, "/media/"):
			// Mock GIF binary
			w.Header().Set(contentTypeHeader, "image/gif")
			w.Header().Set("Content-Length", "8")
			_, _ = w.Write([]byte("GIF89a\x00\x00"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	// Override package vars with the mock server URL + permissive host whitelist
	originalBase := klipy.SetAPIBaseForTest(server.URL)
	originalHosts := klipy.SetMediaHostsForTest(regexp.MustCompile(`^127\.0\.0\.1(:\d+)?$`))
	defer func() {
		klipy.SetAPIBaseForTest(originalBase)
		klipy.SetMediaHostsForTest(originalHosts)
	}()

	h := newTestHandler(t)
	// Use the httptest server's pre-configured client (skips TLS verify on the
	// self-signed cert) so the production handler can reach the mock upstream.
	klipy.SetHTTPClientForTest(h, server.Client())
	klipy.SetMediaClientForTest(h, server.Client())
	r := newRouter(h)
	fn(t, h, r)
}

func TestProxyTrendingHappyPath(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathTrending+"?per_page=10", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "trend-1")
		assert.NotContains(t, w.Body.String(), testAppKey)
	})
}

func TestProxySearchHappyPath(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathSearch+"?q=cats", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "search-1")
	})
}

func TestProxyCategoriesHappyPath(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/klipy/gifs/categories", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "Reactions")
	})
}

func TestProxyRandomIDHappyPath(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/klipy/randomid", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "mock-id")
	})
}

func TestProxyRecentHappyPath(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/klipy/gifs/recent/abc123", nil)
		req.Header.Set(testUserHeader, "happy-user")
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestProxyItemsHappyPath(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/klipy/gifs/items?slugs=abc", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestProxyShareHappyPath(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/klipy/gifs/share/test-slug", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestProxyReportHappyPath(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/klipy/gifs/report/bad-slug", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestProxyHideRecentHappyPath(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("DELETE", "/klipy/gifs/recent/abc123", nil)
		req.Header.Set(testUserHeader, "happy-user")
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

// NOTE ON STYLE: this file has NO request helper. Every existing test builds
// the request inline with httptest.NewRecorder() + httptest.NewRequest() +
// r.ServeHTTP — see TestProxyCategoriesHappyPath (:347). Match that exactly.
// NOTE ON ROUTES: newRouter mounts the group at /klipy, NOT /api/v1/klipy.

func TestShareForwardsCustomerIDInBody(t *testing.T) {
	// THE A2 GATE. This is the half of the fix we control and the half that
	// regresses. "Share a GIF, then look at the Recent tab" is deliberately NOT
	// the gate — vendor analytics ingestion latency is undocumented, so it has
	// no failure semantics.
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		lastUpstream = nil
		w := httptest.NewRecorder()
		// Personalization ON is modelled by the client sending its nonce; with it
		// OFF the renderer omits customer_id and the share is deliberately
		// unattributed (see TestPersonalizedRoutesOmitCustomerIDWhenClientSendsNone).
		req := httptest.NewRequest("POST", "/klipy/gifs/share/good-slug?customer_id=stored-nonce", nil)
		req.Header.Set(testUserHeader, "sharing-user")
		r.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, lastUpstream)
		// A customer_id must be ATTACHED (absent one, the ledger stays empty and
		// A2 regresses) and must be the derived 32-hex key, not a client value.
		assert.Regexp(t, `customer_id=[0-9a-f]{32}`, lastUpstream.Body)
		assert.Equal(t, "application/x-www-form-urlencoded", lastUpstream.ContentType)
		assert.Regexp(t, `customer_id=[0-9a-f]{32}`, lastUpstream.RawQuery)
	})
}

func TestShareForwardsQ(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		lastUpstream = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/klipy/gifs/share/good-slug?q=happy+dance", nil)
		r.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, lastUpstream)
		assert.Contains(t, lastUpstream.Body, "q=happy+dance")
	})
}

func TestShareOmitsAbsentParams(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		lastUpstream = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/klipy/gifs/share/good-slug", nil)
		r.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, lastUpstream)
		assert.Empty(t, lastUpstream.Body)
		assert.NotContains(t, lastUpstream.RawQuery, "customer_id")
	})
}

func TestShareRejectsEmptyCustomerID(t *testing.T) {
	// R4 LOCK. ValidateSlug("") returns TRUE in Go. The natural implementation
	//   cid := c.Query("customer_id"); if !ValidateSlug(&cid) { 400 }; form.Set(...)
	// therefore ACCEPTS an absent param and forwards `customer_id=` upstream,
	// reproducing A2 exactly behind a green happy-path test. Branch on presence
	// FIRST. This must be a 200 with the param omitted — not a 400.
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		lastUpstream = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/klipy/gifs/share/good-slug?customer_id=", nil)
		r.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, lastUpstream)
		assert.NotContains(t, lastUpstream.Body, "customer_id=")
		assert.NotContains(t, lastUpstream.RawQuery, "customer_id=")
	})
}

func TestShareDiscardsClientCustomerIDPayload(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		lastUpstream = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/klipy/gifs/share/good-slug?customer_id=..%2Fx", nil)
		req.Header.Set(testUserHeader, "sharing-user")
		r.ServeHTTP(w, req)

		// Previously a 400. The traversal payload is now discarded unread, so
		// the request succeeds and the payload still never reaches upstream —
		// the same property, held by construction instead of by a check.
		assert.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, lastUpstream)
		assert.NotContains(t, lastUpstream.Body, "..")
		assert.NotContains(t, lastUpstream.RawQuery, "..")
	})
}

func TestShareRejectsOversizeQ(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		lastUpstream = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST",
			"/klipy/gifs/share/good-slug?q="+strings.Repeat("a", 101), nil)
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Nil(t, lastUpstream)
	})
}

func TestShareNeverForwardsClientBody(t *testing.T) {
	// THE C4 LOCK. The url.Values signature makes "no client body forwarded"
	// structural rather than conventional; this test is what keeps it true.
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		lastUpstream = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/klipy/gifs/share/good-slug",
			strings.NewReader(`{"evil":"payload"}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, lastUpstream)
		assert.NotContains(t, lastUpstream.Body, "evil")
		assert.NotContains(t, lastUpstream.Body, "payload")
	})
}

func TestShareAppKeyNotLeakedOnParamRejection(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/klipy/gifs/share/good-slug?customer_id=..%2Fx", nil)
		r.ServeHTTP(w, req)

		assert.NotContains(t, w.Body.String(), testAppKey)
	})
}

func TestProxyMediaHappyPath(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		// Get the actual httptest server URL via the package var (we set it above)
		// For media we need to point at the mock server's media path
		base := klipy.GetAPIBaseForTest()
		// Strip the /api/v1 suffix-or-not — the mock has a /media/ prefix
		// Actually the easier path: hit our own httptest server's /media/ path
		mediaURL := base + "/media/test.gif"
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathMediaURL+url.QueryEscape(mediaURL), nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "image/gif", w.Header().Get(contentTypeHeader))
		// The shared withMockUpstream mock at handlers_test.go's /media/ branch
		// does NOT set Cache-Control on upstream, so the handler's default
		// branch at klipy/handlers.go:260-262 must fire and apply the
		// conservative 1-hour public cache. This is load-bearing for the
		// Chromium HTTP cache that absorbs repeat-view scroll bursts (#804);
		// a future refactor that removed the else-branch would silently
		// re-introduce the rate-limit pressure the higher /media limit is
		// designed to relieve.
		assert.Equal(t, "public, max-age=3600", w.Header().Get("Cache-Control"))
	})
}

// TestProxyMediaPassesThroughUpstreamCacheControl verifies the OTHER branch of
// klipy/handlers.go:255-262: when upstream provides a Cache-Control header,
// the handler MUST forward it unchanged rather than apply the default. This
// preserves CDN-controlled caching when KLIPY (or the underlying CDN) signals
// stricter or different cache semantics than our default.
//
// Together with TestProxyMediaHappyPath, this exercises both branches of the
// Cache-Control branch in the Media handler.
func TestProxyMediaPassesThroughUpstreamCacheControl(t *testing.T) {
	const upstreamCacheControl = "public, max-age=300, immutable"
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(contentTypeHeader, "image/gif")
		w.Header().Set("Cache-Control", upstreamCacheControl)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("GIF89a\x00\x00"))
	}
	withMockUpstreamHandler(t, upstream, func(t *testing.T, h *klipy.Handler, r *gin.Engine) {
		server := httptest.NewTLSServer(http.HandlerFunc(upstream))
		defer server.Close()
		klipy.SetMediaClientForTest(h, server.Client())
		savedHosts := klipy.SetMediaHostsForTest(regexp.MustCompile(`^127\.0\.0\.1(:\d+)?$`))
		defer klipy.SetMediaHostsForTest(savedHosts)

		mediaURL := server.URL + "/media/passthrough.gif"
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathMediaURL+url.QueryEscape(mediaURL), nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		// The handler MUST forward the upstream value verbatim, not overwrite
		// it with the default.
		assert.Equal(t, upstreamCacheControl, w.Header().Get("Cache-Control"))
	})
}

// withMockUpstreamHandler is a variant of withMockUpstream that takes a custom
// handler function for the mock server, used by tests that need to simulate
// upstream errors (5xx, 4xx) on otherwise-normal endpoint paths.
func withMockUpstreamHandler(
	t *testing.T,
	upstream http.HandlerFunc,
	fn func(t *testing.T, h *klipy.Handler, r *gin.Engine),
) {
	t.Helper()
	server := httptest.NewTLSServer(upstream)
	defer server.Close()

	originalBase := klipy.SetAPIBaseForTest(server.URL)
	originalHosts := klipy.SetMediaHostsForTest(regexp.MustCompile(`^127\.0\.0\.1(:\d+)?$`))
	defer func() {
		klipy.SetAPIBaseForTest(originalBase)
		klipy.SetMediaHostsForTest(originalHosts)
	}()

	h := newTestHandler(t)
	klipy.SetHTTPClientForTest(h, server.Client())
	r := newRouter(h)
	fn(t, h, r)
}

func TestProxyForwardJSONUpstream5xxReturns502(t *testing.T) {
	// Mock upstream that returns 500 for the trending endpoint with the app
	// key visible in the response body. The proxy must map this to 502 AND
	// must NOT echo the upstream body (which would leak the app key).
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"upstream broke","key":"` + testAppKey + `"}`))
	}
	withMockUpstreamHandler(t, upstream, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathTrending, nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadGateway, w.Code)
		// Generic error message — never the upstream body
		assert.Contains(t, w.Body.String(), errUpstreamDownText)
		assert.NotContains(t, w.Body.String(), testAppKey)
	})
}

func TestProxyForwardJSONUpstream4xxReturns4xxWithoutBodyLeak(t *testing.T) {
	// Mock upstream that returns 403 with the app key embedded in the body
	// (mirroring KLIPY's real "invalid API key" error envelope). The proxy
	// must mirror the 403 status but replace the body with a generic message.
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"result":false,"errors":{"message":["The provided API key is invalid: [` + testAppKey + `]"]}}`))
	}
	withMockUpstreamHandler(t, upstream, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathTrending, nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Contains(t, w.Body.String(), errUpstreamFailedText)
		assert.NotContains(t, w.Body.String(), testAppKey)
	})
}

// errUpstreamDownText / errUpstreamFailedText mirror the package-level
// constants in handlers.go for assertion clarity. Keeping them as test-local
// constants avoids depending on a private symbol from the package under test.
const (
	errUpstreamDownText   = "GIF service temporarily unavailable"
	errUpstreamFailedText = "GIF service request failed"
)

// --- Media handler branch coverage ---

// TestProxyMediaInvalidURL ensures the handler returns 400 when the url
// parameter is not a valid https URL (e.g. a bare word with no scheme).
func TestProxyMediaInvalidURL(t *testing.T) {
	h := newTestHandler(t)
	r := newRouter(h)
	w := httptest.NewRecorder()
	target := url.QueryEscape("not-a-url")
	req := httptest.NewRequest("GET", pathMediaURL+target, nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid url")
}

// TestProxyMediaDisallowedHost ensures the handler returns 400 when the url
// has a valid https scheme but a host not in the allowedMediaHosts whitelist.
func TestProxyMediaDisallowedHost(t *testing.T) {
	h := newTestHandler(t)
	r := newRouter(h)
	w := httptest.NewRecorder()
	target := url.QueryEscape("https://evil.com/img.gif")
	req := httptest.NewRequest("GET", pathMediaURL+target, nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "host not allowed")
}

// TestProxyMediaUpstream5xx verifies that a 5xx response from the upstream
// CDN is translated to a 502 Bad Gateway by our Media handler (matching the
// behaviour of forwardJSON for API endpoints).
func TestProxyMediaUpstream5xx(t *testing.T) {
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`upstream error`))
	}
	withMockUpstreamHandler(t, upstream, func(t *testing.T, h *klipy.Handler, r *gin.Engine) {
		// Also wire the media client so the Media handler uses the mock TLS server.
		server := httptest.NewTLSServer(http.HandlerFunc(upstream))
		defer server.Close()
		klipy.SetMediaClientForTest(h, server.Client())
		// Point allowedMediaHosts at the test server's host so the host check passes.
		savedHosts := klipy.SetMediaHostsForTest(regexp.MustCompile(`^127\.0\.0\.1(:\d+)?$`))
		defer klipy.SetMediaHostsForTest(savedHosts)

		mediaURL := server.URL + "/media/test.gif"
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathMediaURL+url.QueryEscape(mediaURL), nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.Contains(t, w.Body.String(), errUpstreamDownText)
	})
}

// TestProxyMediaUpstream4xx verifies that a 4xx response from the upstream
// CDN is mirrored back to the client with the same status code (no body).
func TestProxyMediaUpstream4xx(t *testing.T) {
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}
	withMockUpstreamHandler(t, upstream, func(t *testing.T, h *klipy.Handler, r *gin.Engine) {
		server := httptest.NewTLSServer(http.HandlerFunc(upstream))
		defer server.Close()
		klipy.SetMediaClientForTest(h, server.Client())
		savedHosts := klipy.SetMediaHostsForTest(regexp.MustCompile(`^127\.0\.0\.1(:\d+)?$`))
		defer klipy.SetMediaHostsForTest(savedHosts)

		mediaURL := server.URL + "/media/missing.gif"
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", pathMediaURL+url.QueryEscape(mediaURL), nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

// --- Upstream-error observability (#804 silent-failure mitigation) ---

// newHandlerWithLogCapture builds a klipy.Handler whose logger writes to the
// returned bytes.Buffer instead of stdout, so tests can assert that error
// paths emit structured logs AND respect the privacy promise (no slugs, no
// search terms, no full upstream URLs).
//
// Used only by upstream-error observability tests. Other tests use
// newTestHandler with the stdout logger.
func newHandlerWithLogCapture(_ *testing.T) (*klipy.Handler, *bytes.Buffer) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{KlipyAPIKey: testAppKey}
	buf := &bytes.Buffer{}
	log := logger.NewWithWriter(buf)
	return klipy.NewHandler(cfg, log), buf
}

// TestProxyMediaLogsUpstream5xxWithoutLeakingURL asserts that the Media
// handler's 5xx-upstream branch emits a structured log entry containing the
// host and status code but NOT the slug-bearing path component of the
// upstream URL. This is the regression-catch for the silent-failure surface
// identified during #804 review: pre-existing code at handlers.go's Media
// handler swallowed upstream errors with no log, so a KLIPY credential
// rotation (returning 401/403) would surface only as "GIF picker is broken"
// to users with no server-side signal for operators.
//
// Privacy invariant: log output must not contain "secret-slug" (the slug
// embedded in the test URL), the full mediaURL, or any URL-decoded path
// component beyond the bounded host.
func TestProxyMediaLogsUpstream5xxWithoutLeakingURL(t *testing.T) {
	const slug = "secret-slug-do-not-leak-12345"
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(upstream))
	defer server.Close()

	h, logBuf := newHandlerWithLogCapture(t)
	klipy.SetMediaClientForTest(h, server.Client())
	savedHosts := klipy.SetMediaHostsForTest(regexp.MustCompile(`^127\.0\.0\.1(:\d+)?$`))
	defer klipy.SetMediaHostsForTest(savedHosts)

	r := newRouter(h)
	mediaURL := server.URL + "/media/" + slug + ".gif"
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathMediaURL+url.QueryEscape(mediaURL), nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadGateway, w.Code)

	logOut := logBuf.String()
	// Affirmative checks: the log must surface the failure to operators.
	assert.Contains(t, logOut, "klipy media: upstream 5xx",
		"5xx upstream must emit a structured log so credential-rotation outages are visible to operators")
	assert.Contains(t, logOut, "status=500",
		"log must include the upstream status code")
	assert.Contains(t, logOut, "host=127.0.0.1",
		"log must include the (allowlisted) upstream host for operator triage")
	// Privacy invariants: the slug and full URL must NEVER appear in logs.
	assert.NotContains(t, logOut, slug,
		"PRIVACY VIOLATION: upstream slug leaked into log output")
	assert.NotContains(t, logOut, "/media/secret-slug",
		"PRIVACY VIOLATION: upstream path leaked into log output")
}

// TestProxyMediaLogsUpstream4xxWithoutLeakingURL is the credential-rotation
// indicator path: KLIPY returns 401/403 when the proxy's app_key is rejected.
// This must be logged so operators detect the outage; the slug must not leak.
func TestProxyMediaLogsUpstream4xxWithoutLeakingURL(t *testing.T) {
	const slug = "another-secret-slug-xyz"
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(upstream))
	defer server.Close()

	h, logBuf := newHandlerWithLogCapture(t)
	klipy.SetMediaClientForTest(h, server.Client())
	savedHosts := klipy.SetMediaHostsForTest(regexp.MustCompile(`^127\.0\.0\.1(:\d+)?$`))
	defer klipy.SetMediaHostsForTest(savedHosts)

	r := newRouter(h)
	mediaURL := server.URL + "/media/" + slug + ".gif"
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathMediaURL+url.QueryEscape(mediaURL), nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)

	logOut := logBuf.String()
	assert.Contains(t, logOut, "klipy media: upstream 4xx")
	assert.Contains(t, logOut, "status=403")
	assert.Contains(t, logOut, "host=127.0.0.1")
	assert.NotContains(t, logOut, slug,
		"PRIVACY VIOLATION: upstream slug leaked into log output on 4xx")
}

// TestProxyAPILogsUpstream4xxWithoutLeakingSearchQuery is the analogous test
// for the forwardJSON helper (used by /gifs/search, /gifs/trending, etc.).
// The 4xx branch must log status + route + method but NOT the search term
// (privacy promise — searches are not logged).
func TestProxyAPILogsUpstream4xxWithoutLeakingSearchQuery(t *testing.T) {
	const sensitiveQuery = "DO-NOT-LEAK-search-term-9999"
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"app_key invalid"}`))
	}
	server := httptest.NewTLSServer(http.HandlerFunc(upstream))
	defer server.Close()

	originalBase := klipy.SetAPIBaseForTest(server.URL)
	defer klipy.SetAPIBaseForTest(originalBase)

	h, logBuf := newHandlerWithLogCapture(t)
	klipy.SetHTTPClientForTest(h, server.Client())
	r := newRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathSearch+"?q="+url.QueryEscape(sensitiveQuery), nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	logOut := logBuf.String()
	// Affirmative: route + method + status surface to operators.
	assert.Contains(t, logOut, "klipy api: upstream 4xx",
		"4xx upstream must emit a structured log (credential-rotation indicator)")
	assert.Contains(t, logOut, "status=401")
	assert.Contains(t, logOut, "method=GET")
	// Privacy invariant: search term must NEVER appear.
	assert.NotContains(t, logOut, sensitiveQuery,
		"PRIVACY VIOLATION: search query leaked into log output")
	assert.NotContains(t, logOut, "DO-NOT-LEAK",
		"PRIVACY VIOLATION: search-term substring leaked into log output")
}

// TestProxyAPILogsUpstreamNetworkFailureWithoutLeakingURL covers the
// `h.client.Do` error branch in forwardJSON: when the upstream KLIPY API is
// unreachable (DNS failure, connection refused, TLS error), the handler must
// emit a structured Warn log AND must NOT leak the search query into the log.
//
// We trigger the network failure by pointing klipyAPIBase at an unreachable
// host (TCP port 1 is reliably refused on localhost) so client.Do returns
// a connection error.
func TestProxyAPILogsUpstreamNetworkFailureWithoutLeakingURL(t *testing.T) {
	const sensitiveQuery = "NETWORK-FAIL-search-leak-check"

	// Unreachable upstream: TCP port 1 reliably yields connection-refused.
	originalBase := klipy.SetAPIBaseForTest("http://127.0.0.1:1")
	defer klipy.SetAPIBaseForTest(originalBase)

	h, logBuf := newHandlerWithLogCapture(t)
	r := newRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathSearch+"?q="+url.QueryEscape(sensitiveQuery), nil)
	r.ServeHTTP(w, req)
	// Network failure must surface to clients as 502 Bad Gateway.
	require.Equal(t, http.StatusBadGateway, w.Code)

	logOut := logBuf.String()
	// Affirmative: the log surfaces the failure to operators.
	assert.Contains(t, logOut, "klipy api: upstream request failed",
		"network failure must emit a structured log so connectivity outages are visible to operators")
	assert.Contains(t, logOut, "method=GET")
	// Privacy invariant: search term must NEVER appear, even via the error string.
	assert.NotContains(t, logOut, sensitiveQuery,
		"PRIVACY VIOLATION: search query leaked into log output on network failure")
	assert.NotContains(t, logOut, "NETWORK-FAIL",
		"PRIVACY VIOLATION: search-term substring leaked via error.Error()")
}

// TestProxyMediaLogsUpstreamNetworkFailureWithoutLeakingURL is the matching
// test for the Media handler's `mediaClient.Do` error branch. Triggers a
// connection failure by closing the mock upstream before the request fires.
func TestProxyMediaLogsUpstreamNetworkFailureWithoutLeakingURL(t *testing.T) {
	const slug = "network-fail-slug-do-not-leak"

	// Start a mock server, capture its URL, then close it BEFORE making the
	// request. The closed server's URL still parses cleanly and passes the
	// host allowlist, but mediaClient.Do will get connection-refused.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	closedURL := server.URL
	server.Close()

	h, logBuf := newHandlerWithLogCapture(t)
	klipy.SetMediaClientForTest(h, &http.Client{Timeout: 1}) // 1ns timeout — guaranteed network-level failure
	savedHosts := klipy.SetMediaHostsForTest(regexp.MustCompile(`^127\.0\.0\.1(:\d+)?$`))
	defer klipy.SetMediaHostsForTest(savedHosts)

	r := newRouter(h)
	mediaURL := closedURL + "/media/" + slug + ".gif"
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathMediaURL+url.QueryEscape(mediaURL), nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadGateway, w.Code)

	logOut := logBuf.String()
	assert.Contains(t, logOut, "klipy media: upstream request failed",
		"network failure must emit a structured log")
	assert.Contains(t, logOut, "host=127.0.0.1")
	assert.NotContains(t, logOut, slug,
		"PRIVACY VIOLATION: slug leaked into log output on network failure")
}

// TestShareRefusesUpstreamRedirect is a #2371 regression lock, and it is
// deliberately NOT written inside withMockUpstream: that harness calls
// SetHTTPClientForTest to swap in the httptest server's client, which carries
// no CheckRedirect — a test written there would replace the very guard it is
// meant to verify and pass vacuously.
//
// Before #2371 the proxy forwarded no request body, so following a redirect was
// harmless. Share now sends a server-constructed form body, and on a 307 Go
// preserves the method and replays that body via GetBody — handing an
// upstream-chosen host the per-user customer_id, plus a Referer whose path
// carries the KLIPY app key. The dial-time egress guard does not stop it: a
// redirect target is an ordinary public address.
func TestShareRefusesUpstreamRedirect(t *testing.T) {
	// httptest binds loopback, which the production dial guard denies.
	defer klipy.SetEgressGuardForTest(func(netip.Addr) bool { return false })()

	var offPathHit bool
	var offPathBody string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		offPathHit = true
		offPathBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()

	restoreBase := klipy.SetAPIBaseForTest(upstream.URL)
	defer klipy.SetAPIBaseForTest(restoreBase)

	r := newRouter(newTestHandler(t))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/klipy/gifs/share/good-slug?customer_id=abc-123", nil)
	r.ServeHTTP(w, req)

	assert.False(t, offPathHit, "a redirect must not deliver the request to an off-path host")
	assert.Empty(t, offPathBody, "the server-constructed form body must never be replayed off-path")
	assert.Equal(t, http.StatusBadGateway, w.Code, "a refused redirect surfaces as an upstream failure")
	assert.NotContains(t, w.Body.String(), testAppKey, "the app key must not leak in the error body")
}

// TestShareEmptySlugIssuesNoUpstreamRequest locks a property that is currently
// true by routing rather than by a guard, which is exactly why it is worth
// pinning. ValidateSlug("") returns true, so if gin ever routed
// /gifs/share/ to Share with an empty :slug, the handler would build
// "/gifs/share/" and call upstream. It does not — the route 404s — but that
// depends on gin's trailing-slash behaviour, not on anything this package
// controls. Raised as a review finding and refuted by this test.
func TestShareEmptySlugIssuesNoUpstreamRequest(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		lastUpstream = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/klipy/gifs/share/", nil)
		r.ServeHTTP(w, req)

		assert.Nil(t, lastUpstream, "an empty slug must never reach the upstream")
		assert.NotEqual(t, http.StatusOK, w.Code)
	})
}

// --- customer_id is derived from the authenticated user, never accepted ------
//
// KLIPY treats customer_id as an opaque partition key and never mints it — the
// CustomerID handler used to return uuid.NewString(), i.e. WE chose the value
// all along. Because it is ours to choose, it is derived from the authenticated
// user rather than supplied by the client, which removes it from the trust
// boundary instead of adding an ownership check a future handler could forget.
//
// Without this, GET /gifs/recent/{id} proxied whatever id the caller typed, so
// any authenticated account could read any other account's share history. That
// was dormant while the recent ledger was structurally empty and went live the
// moment this PR started populating it (#2371 review, Codex P1).

func recentUpstreamPathFor(t *testing.T, r *gin.Engine, user, clientSuppliedID string) string {
	t.Helper()
	lastUpstream = nil
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/klipy/gifs/recent/"+clientSuppliedID, nil)
	req.Header.Set(testUserHeader, user)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, lastUpstream, "no upstream request was made")
	return lastUpstream.Path
}

func TestRecentIgnoresClientCustomerID(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		const victimID = "victimcustomeridvalue"
		path := recentUpstreamPathFor(t, r, "attacker-user", victimID)
		assert.NotContains(t, path, victimID,
			"a client-supplied customer_id reached upstream: any authenticated "+
				"account can read any other account's share history")
	})
}

func TestRecentDerivesADistinctIDPerUser(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		const sameID = "identicalclientvalue"
		attacker := recentUpstreamPathFor(t, r, "attacker-user", sameID)
		victim := recentUpstreamPathFor(t, r, "victim-user", sameID)
		assert.NotEqual(t, attacker, victim,
			"two different users derived the same partition key — one shared ledger")
	})
}

func TestRecentDerivedIDIsStableForOneUser(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		// Stability is what makes the ledger work at all: the id written by
		// Share must be the id read back by Recent, across processes and
		// restarts. A random or time-varying derivation would pass the
		// isolation tests above and still leave the Recent tab permanently
		// empty — which is defect A2 all over again. Stability is per (user,
		// nonce): the SAME stored nonce must always resolve to the same ledger.
		first := recentUpstreamPathFor(t, r, "steady-user", "stored-nonce")
		second := recentUpstreamPathFor(t, r, "steady-user", "stored-nonce")
		assert.Equal(t, first, second, "the derived id is not stable for one user")
	})
}

func TestRecentFailsClosedWhenUnauthenticated(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		// No testUserHeader. In production the auth middleware makes this
		// unreachable; the guard exists so that if it ever IS reachable, every
		// caller does not silently collapse onto one shared derived ledger.
		lastUpstream = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/klipy/gifs/recent/anything", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Nil(t, lastUpstream, "an unauthenticated request reached upstream")
	})
}

func TestShareSendsDerivedCustomerIDNotTheClientOne(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		lastUpstream = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST",
			"/klipy/gifs/share/good-slug?customer_id=attackerchosen", nil)
		req.Header.Set(testUserHeader, "sharing-user")
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, lastUpstream)
		assert.NotContains(t, lastUpstream.Body, "attackerchosen",
			"the client chose the ledger its share was written into")
		assert.Contains(t, lastUpstream.Body, "customer_id=",
			"no customer_id was attached, so the recent ledger stays empty (A2)")
	})
}

// The other half of the contract, and the reason the client's value is folded
// in rather than discarded: the customer id is a user-facing PRIVACY control.
// Settings > Content Safety displays it and offers Rotate, and with
// personalization off the client rotates an ephemeral value every 30 minutes so
// KLIPY cannot correlate a user over time. Deriving from the user id alone
// would have closed the security hole and silently pinned every user to one
// permanent upstream identifier instead.
func TestRotatingTheNonceReachesADifferentLedger(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		before := recentUpstreamPathFor(t, r, "rotating-user", "nonce-before")
		after := recentUpstreamPathFor(t, r, "rotating-user", "nonce-after")
		assert.NotEqual(t, before, after,
			"rotating the customer id had no effect upstream: the Rotate button "+
				"and the 30-minute ephemeral rotation are both inert")
	})
}

// --- absence is meaningful; every personalized route derives ----------------
//
// When personalization is OFF the renderer omits customer_id entirely, and
// klipyClient.notifyShared says why: attaching it "would create a PERSISTENT
// upstream write keyed to an id we promise is ephemeral". Deriving from an
// absent value manufactures exactly that identifier and defeats the opt-out
// from the server side. The pre-derivation Share handler branched on presence
// for the sibling reason recorded as #2371 R4; the derivation dropped that
// branch and these tests are what stop it being dropped again.

func upstreamQueryFor(t *testing.T, r *gin.Engine, method, url string) string {
	t.Helper()
	lastUpstream = nil
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, url, nil)
	req.Header.Set(testUserHeader, "some-user")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, lastUpstream, "no upstream request was made")
	if lastUpstream.Body != "" {
		return lastUpstream.RawQuery + "&" + lastUpstream.Body
	}
	return lastUpstream.RawQuery
}

func TestPersonalizedRoutesOmitCustomerIDWhenClientSendsNone(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		for _, tc := range []struct{ name, method, url string }{
			{"share", "POST", "/klipy/gifs/share/good-slug"},
			{"trending", "GET", "/klipy/gifs/trending"},
			{"search", "GET", "/klipy/gifs/search?q=cats"},
			{"items", "GET", "/klipy/gifs/items?slugs=a"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := upstreamQueryFor(t, r, tc.method, tc.url)
				assert.NotContains(t, got, "customer_id",
					"the server manufactured an identifier the client withheld")
			})
		}
	})
}

func TestPersonalizedRoutesDeriveRatherThanForwardTheNonce(t *testing.T) {
	withMockUpstream(t, func(t *testing.T, _ *klipy.Handler, r *gin.Engine) {
		const nonce = "rawclientnoncevalue"
		for _, tc := range []struct{ name, method, url string }{
			{"share", "POST", "/klipy/gifs/share/good-slug?customer_id=" + nonce},
			{"trending", "GET", "/klipy/gifs/trending?customer_id=" + nonce},
			{"search", "GET", "/klipy/gifs/search?q=cats&customer_id=" + nonce},
			{"items", "GET", "/klipy/gifs/items?slugs=a&customer_id=" + nonce},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := upstreamQueryFor(t, r, tc.method, tc.url)
				assert.NotContains(t, got, nonce,
					"the raw client nonce was forwarded to KLIPY verbatim")
				assert.Regexp(t, `customer_id=[0-9a-f]{32}`, got,
					"no derived customer_id was attached")
			})
		}
	})
}
