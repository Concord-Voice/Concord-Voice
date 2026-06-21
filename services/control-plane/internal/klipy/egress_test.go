package klipy

// Internal (package klipy) tests for the SSRF egress guard (#1361). These live
// in-package — not in the external handlers_test.go (package klipy_test) —
// because they exercise unexported symbols (isDeniedEgressIP,
// validateRedirectTarget, newGuardedTransport, errEgressBlocked/errRedirectBlocked).

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Layer C: pure IP-deny predicate ---

func TestIsDeniedEgressIP(t *testing.T) {
	deny := []string{
		"127.0.0.1", "::1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "169.254.1.1", "fe80::1", "fc00::1",
		"0.0.0.0", "::", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
		"100.64.0.1",
		// #1362 hardening: deprecated IPv6 site-local (fec0::/10) + all
		// multicast (link-local 224.0.0.0/24 & ff02::/16, global ff0e::/16).
		"fec0::1", "224.0.0.1", "ff02::1", "ff0e::1",
	}
	allow := []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111", "93.184.216.34"}
	for _, s := range deny {
		assert.True(t, isDeniedEgressIP(netip.MustParseAddr(s)), "expected %s denied", s)
	}
	for _, s := range allow {
		assert.False(t, isDeniedEgressIP(netip.MustParseAddr(s)), "expected %s allowed", s)
	}
}

// --- Layer A: pure redirect-target validator (uses the production allowlist) ---

func TestValidateRedirectTarget(t *testing.T) {
	mk := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		return &http.Request{URL: u}
	}
	assert.NoError(t, validateRedirectTarget(mk("https://media.klipy.com/x.gif")))
	assert.ErrorIs(t, validateRedirectTarget(mk("http://media.klipy.com/x.gif")), errRedirectBlocked)
	assert.ErrorIs(t, validateRedirectTarget(mk("https://evil.example/x.gif")), errRedirectBlocked)
	assert.ErrorIs(t, validateRedirectTarget(mk("https://169.254.169.254/x")), errRedirectBlocked)
}

// --- Layer C end-to-end at the transport, plus the test seam ---

func TestGuardedTransportBlocksLoopback(t *testing.T) {
	// httptest binds 127.0.0.1 → the production guard must refuse to dial it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Transport: newGuardedTransport(), Timeout: 2 * time.Second}
	_, err := client.Get(srv.URL)
	require.Error(t, err)
	assert.ErrorIs(t, err, errEgressBlocked)

	// Seam: relaxing the guard lets the same dial succeed.
	restore := SetEgressGuardForTest(func(netip.Addr) bool { return false })
	defer restore()
	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// --- NewHandler wiring ---

func TestNewHandlerWiresMediaCheckRedirect(t *testing.T) {
	h := NewHandler(&config.Config{KlipyAPIKey: "x"}, logger.New("test"))
	require.NotNil(t, h.mediaClient.CheckRedirect)
	mk := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		return &http.Request{URL: u}
	}
	assert.NoError(t, h.mediaClient.CheckRedirect(mk("https://media.klipy.com/x"), nil))
	assert.ErrorIs(t, h.mediaClient.CheckRedirect(mk("https://evil.example/x"), nil), errRedirectBlocked)
	// The JSON client carries no CheckRedirect — it is covered by Layer C only.
	assert.Nil(t, h.client.CheckRedirect)
}

// --- Layer A end-to-end through the Media handler ---

func TestMediaRedirectToDisallowedHostBlocked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	defer SetEgressGuardForTest(func(netip.Addr) bool { return false })() // allow loopback dials

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://disallowed.example/leak", http.StatusFound)
	}))
	defer origin.Close()

	restoreHosts := SetMediaHostsForTest(regexp.MustCompile("^" + regexp.QuoteMeta(hostOf(t, origin.URL)) + "$"))
	defer SetMediaHostsForTest(restoreHosts)

	h := NewHandler(&config.Config{KlipyAPIKey: "x"}, logger.New("test"))
	trustServerCert(t, h, origin)

	w := serveMedia(h, origin.URL)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.NotContains(t, w.Body.String(), "leak")
}

func TestMediaSameHostRedirectStreams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	defer SetEgressGuardForTest(func(netip.Addr) bool { return false })()

	var final *httptest.Server
	final = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, final.URL+"/gif", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write([]byte("GIF89a-bytes"))
	}))
	defer final.Close()

	restoreHosts := SetMediaHostsForTest(regexp.MustCompile("^" + regexp.QuoteMeta(hostOf(t, final.URL)) + "$"))
	defer SetMediaHostsForTest(restoreHosts)

	h := NewHandler(&config.Config{KlipyAPIKey: "x"}, logger.New("test"))
	trustServerCert(t, h, final)

	w := serveMedia(h, final.URL+"/redir")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "GIF89a-bytes", w.Body.String())
}

// --- helpers ---

func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u.Host
}

// trustServerCert makes the handler's (guarded) media transport trust srv's
// self-signed cert via a RootCAs pool — keeping the production CheckRedirect and
// dial guard intact. No InsecureSkipVerify.
func trustServerCert(t *testing.T, h *Handler, srv *httptest.Server) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr, ok := h.mediaClient.Transport.(*http.Transport)
	require.True(t, ok)
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func serveMedia(h *Handler, target string) *httptest.ResponseRecorder {
	r := gin.New()
	r.GET("/klipy/media", h.Media)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/klipy/media?url="+url.QueryEscape(target), nil)
	r.ServeHTTP(w, req)
	return w
}
