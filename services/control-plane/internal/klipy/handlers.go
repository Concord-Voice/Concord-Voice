// Package klipy provides HTTP handlers that proxy KLIPY API and media
// requests through the control-plane. The proxy is used by the desktop
// client — see PrivacySecuritySection.tsx and the renderer's
// gifProvider/klipyClient.ts.
//
// The proxy is allowed by KLIPY's API ToS (no proxy prohibition exists in
// their terms). It exists so KLIPY never sees per-user IP addresses or
// search terms.
//
// As of this package version, GIF media is also fully proxied: the renderer
// rewrites all KLIPY CDN URLs to /api/v1/klipy/media?url=<encoded> before
// setting them as <img>/<video> src attributes, so the user's IP is never
// exposed to KLIPY's CDN. See the Media handler below and
// klipyClient.ts#rewriteMediaUrl.
//
// Compliance notes:
//   - We never modify API response bodies returned by KLIPY.
//   - We never log slugs or search terms in the proxy routes (privacy promise).
//     Reachable upstream error paths (network failure, upstream 5xx, upstream
//     4xx) log only: route (gin pattern), method, host (bounded by
//     allowedMediaHosts), and status code / error class. This is enough to
//     detect credential-rotation outages and KLIPY CDN issues without leaking
//     user content interest or the upstream app_key (#804). The
//     http.NewRequestWithContext error branches are intentionally NOT logged
//     — they are structurally unreachable (hardcoded method + validated URL)
//     and would only count against new-code coverage without operational value.
//   - We never cache responses except via standard Cache-Control headers on
//     media binaries — KLIPY does not require revalidation.
//   - We never present KLIPY as a support contact (Section 1).
package klipy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	upstreamTimeout = 10 * time.Second
	// mediaTimeout is intentionally longer than upstreamTimeout: media binaries
	// can be several MB and CDN latency varies by geography.
	mediaTimeout = 30 * time.Second
	maxQueryLen  = 100
	maxPerPage   = 50

	headerContentType  = "Content-Type"
	headerCacheControl = "Cache-Control"
	errUpstreamDown    = "GIF service temporarily unavailable"
	errUpstreamFailed  = "GIF service request failed"
)

// klipyAPIBase is the upstream base URL for KLIPY's API. Defined as a package
// variable rather than a const so tests can substitute an httptest server URL.
// Production code never reassigns it.
var klipyAPIBase = "https://api.klipy.com/api/v1"

// allowedMediaHosts is the host allowlist for the media proxy. Any URL whose
// host does not match one of these patterns is rejected with 400. This is the
// primary SSRF guard — never proxy arbitrary URLs.
//
// Covered hosts:
//   - klipy.com             — bare apex (uncommon but defensive)
//   - *.klipy.com           — API, media, content, and CDN subdomains
//   - klipy.io              — bare apex for the .io TLD
//   - *.klipy.io            — CDN subdomains on the .io TLD
//
// We use a regexp because KLIPY uses numbered CDN sub-hosts (media0, media1,
// etc.). Defined as a package variable rather than a regex literal in the
// function so tests can substitute a more permissive pattern (allowing
// 127.0.0.1) when validating the proxy logic against an httptest server.
var allowedMediaHosts = regexp.MustCompile(`^(klipy\.com|[a-z0-9-]+\.klipy\.com|klipy\.io|[a-z0-9-]+\.klipy\.io)$`)

// Handler holds the dependencies for the KLIPY proxy.
type Handler struct {
	cfg         *config.Config
	log         *logger.Logger
	client      *http.Client // API proxy (JSON endpoints)
	mediaClient *http.Client // media proxy (binary streaming, longer timeout)
}

// NewHandler constructs a KLIPY proxy handler with sane defaults.
// Two http.Clients share the same underlying transport (connection pool) but
// have different timeouts: API calls use a tight 10s deadline while media
// binaries get 30s to accommodate larger payloads and CDN variance.
func NewHandler(cfg *config.Config, log *logger.Logger) *Handler {
	transport := newGuardedTransport()
	return &Handler{
		cfg: cfg,
		log: log,
		client: &http.Client{
			Timeout:   upstreamTimeout,
			Transport: transport,
		},
		mediaClient: &http.Client{
			Timeout:   mediaTimeout,
			Transport: transport,
			// Layer A (SSRF #1361): re-validate scheme+host on every redirect hop.
			// mediaClient only — it is the sole user-supplied-URL surface.
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return validateRedirectTarget(req)
			},
		},
	}
}

// --- SSRF egress guard (#1361) ---

// Egress-guard sentinels. Both remain errors.Is-matchable through the *url.Error
// / *net.OpError wrapping that net/http applies to Client.Do errors.
var (
	errEgressBlocked   = errors.New("klipy egress: destination IP not permitted")
	errRedirectBlocked = errors.New("klipy egress: redirect target not permitted")
)

// cgnatPrefix is RFC6598 carrier-grade-NAT space (100.64.0.0/10), commonly used
// for cloud internal networks and NOT covered by netip.Addr.IsPrivate.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// siteLocalV6 is deprecated IPv6 site-local space (fec0::/10, RFC 3879). It is
// NOT matched by netip.Addr.IsPrivate (which covers only fc00::/7 ULA) nor by
// the link-local predicates, yet may still route on legacy internal networks.
var siteLocalV6 = netip.MustParsePrefix("fec0::/10")

// isDeniedEgressIP reports whether dialing addr would reach a non-public
// destination: loopback, RFC1918/ULA private, link-local (incl. 169.254.169.254
// cloud metadata), any multicast, unspecified, CGNAT, or deprecated IPv6
// site-local. Unmap normalizes IPv4-mapped IPv6 (e.g. ::ffff:169.254.169.254)
// so mapped forms cannot bypass the predicates. IsMulticast subsumes the
// link-local-multicast class and adds admin/global-scoped multicast.
func isDeniedEgressIP(addr netip.Addr) bool {
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsMulticast() || addr.IsUnspecified() {
		return true
	}
	return cgnatPrefix.Contains(addr) || siteLocalV6.Contains(addr)
}

// egressGuard is the active egress predicate. It is a package var (not a direct
// call to isDeniedEgressIP) so SetEgressGuardForTest can relax it — httptest
// servers bind to 127.0.0.1, which the production guard denies.
var egressGuard = isDeniedEgressIP

// validateRedirectTarget re-applies the media-proxy URL policy (https + host
// allowlist) on every redirect hop. The handler validates only hop 0; without
// this, an allowlisted host could 302 the proxy to an arbitrary destination.
func validateRedirectTarget(req *http.Request) error {
	if req.URL.Scheme != "https" || !allowedMediaHosts.MatchString(req.URL.Host) {
		return errRedirectBlocked
	}
	return nil
}

// newGuardedTransport clones the default transport and installs a dial-time
// egress guard. Dialer.Control fires after DNS resolution and before connect, on
// the actual dialed IP, so it defends both redirect-SSRF and DNS-rebinding with
// no TOCTOU window. Cloning preserves ForceAttemptHTTP2 (h2 via ALPN) and gives
// the two klipy clients their own connection pool.
func newGuardedTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("klipy egress: unparseable dial address: %w", err)
			}
			if egressGuard(ap.Addr()) {
				return errEgressBlocked
			}
			return nil
		},
	}).DialContext
	return tr
}

// sanitizedNetErr returns the underlying network error string from a *url.Error
// without leaking the URL. *url.Error.Error() formats as `Op "URL": Err`, where
// URL carries the upstream app_key (forwardJSON) or slug (Media handler) —
// both of which the privacy promise requires we never log. Unwrapping to
// urlErr.Err gives the operator-actionable cause (e.g., "dial tcp 127.0.0.1:1:
// connect: connection refused") without the privacy-sensitive URL.
//
// If err is not a *url.Error or has no wrapped Err, falls back to err.Error().
// In that fallback case, the caller is responsible for ensuring the error does
// not embed a URL — currently only network-stack errors from http.Client.Do
// route through this helper, and those are always *url.Error in practice.
func sanitizedNetErr(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err.Error()
	}
	return err.Error()
}

// --- API endpoint proxies ---

// Trending forwards GET /gifs/trending to KLIPY.
func (h *Handler) Trending(c *gin.Context) {
	h.proxyAPIGet(c, "/gifs/trending", []string{"page", "per_page", "customer_id", "locale", "format_filter"})
}

// Search forwards GET /gifs/search to KLIPY.
func (h *Handler) Search(c *gin.Context) {
	q := c.Query("q")
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "q parameter is required"})
		return
	}
	if len(q) > maxQueryLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "q parameter exceeds maximum length"})
		return
	}
	h.proxyAPIGet(c, "/gifs/search", []string{"q", "page", "per_page", "customer_id", "locale", "content_filter", "format_filter"})
}

// Categories forwards GET /gifs/categories to KLIPY.
func (h *Handler) Categories(c *gin.Context) {
	h.proxyAPIGet(c, "/gifs/categories", []string{"locale"})
}

// Recent forwards GET /gifs/recent/{customer_id} to KLIPY.
func (h *Handler) Recent(c *gin.Context) {
	customerID := c.Param("customerID")
	if !ValidateSlug(&customerID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid customer_id"})
		return
	}
	h.proxyAPIGet(c, "/gifs/recent/"+customerID, []string{"page", "per_page"})
}

// HideRecent forwards DELETE /gifs/recent/{customer_id} to KLIPY.
func (h *Handler) HideRecent(c *gin.Context) {
	customerID := c.Param("customerID")
	if !ValidateSlug(&customerID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid customer_id"})
		return
	}
	h.proxyAPIRequest(c, http.MethodDelete, "/gifs/recent/"+customerID, nil)
}

// Items forwards GET /gifs/items to KLIPY (used to resolve saved-tab slugs).
func (h *Handler) Items(c *gin.Context) {
	h.proxyAPIGet(c, "/gifs/items", []string{"slugs", "customer_id", "format_filter"})
}

// Share forwards POST /gifs/share/{slug} to KLIPY (optional analytics).
func (h *Handler) Share(c *gin.Context) {
	slug := c.Param("slug")
	if !ValidateSlug(&slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": SlugValidationError(&slug)})
		return
	}
	h.proxyAPIRequest(c, http.MethodPost, "/gifs/share/"+slug, nil)
}

// Report forwards POST /gifs/report/{slug} to KLIPY.
func (h *Handler) Report(c *gin.Context) {
	slug := c.Param("slug")
	if !ValidateSlug(&slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": SlugValidationError(&slug)})
		return
	}
	h.proxyAPIRequest(c, http.MethodPost, "/gifs/report/"+slug, nil)
}

// RandomID forwards GET /randomid to KLIPY (per-user opaque session ID).
//
// Deprecated: kept for compatibility with any in-flight clients. New clients
// should call CustomerID below, which generates the opaque ID server-side
// without depending on KLIPY's /randomid endpoint (which is undocumented and
// occasionally returns 404).
func (h *Handler) RandomID(c *gin.Context) {
	h.proxyAPIGet(c, "/randomid", nil)
}

// CustomerID returns a fresh opaque per-user identifier for KLIPY personalization.
//
// KLIPY's customer_id is an opaque, server-defined string — KLIPY does not
// validate any particular format. We generate a UUID v4 server-side rather
// than calling KLIPY's undocumented /randomid endpoint (which has been
// observed returning 404 in production, see QA bug #571 item #15). This
// preserves the personalization feature without leaving a hard dependency
// on a third-party endpoint that may disappear, and it removes the need for
// the client to ever speak directly to api.klipy.com.
//
// Auth requirements match the rest of the klipy proxy routes (JWT-protected,
// rate-limited via the same apiLimiter middleware in router.go).
func (h *Handler) CustomerID(c *gin.Context) {
	id := uuid.NewString()
	c.JSON(http.StatusOK, gin.H{"customer_id": id})
}

// --- Media proxy ---

// Media streams a KLIPY-hosted GIF/MP4/WEBP binary back to the client.
// Accepts the full upstream URL via ?url=<urlencoded>. The host must match
// the allowedMediaHosts whitelist; query params on the upstream URL are
// preserved verbatim (defensive against future signed-URL behavior).
func (h *Handler) Media(c *gin.Context) {
	rawURL := c.Query("url")
	if rawURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url parameter is required"})
		return
	}

	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url"})
		return
	}
	if !allowedMediaHosts.MatchString(parsed.Host) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url host not allowed"})
		return
	}

	// http.NewRequestWithContext is structurally unreachable as an error path
	// here — the method is hardcoded GET and parsed.String() canonicalizes a
	// URL that already passed url.Parse + the host allowlist. Defensive code
	// stays for future-proofing but is intentionally not logged or tested.
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, parsed.String(), nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": errUpstreamDown})
		return
	}

	// Use the dedicated media client (30s timeout) instead of the API client.
	resp, err := h.mediaClient.Do(req) //nolint:gosec // URL validated: scheme=https + host allowlist; egress guarded per-hop redirect + dial-time IP denylist (#1361)
	if err != nil {
		h.log.Warn("klipy media: upstream request failed",
			"host", parsed.Host,
			"error", sanitizedNetErr(err))
		c.JSON(http.StatusBadGateway, gin.H{"error": errUpstreamDown})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 500 {
		h.log.Warn("klipy media: upstream 5xx",
			"host", parsed.Host,
			"status", resp.StatusCode)
		c.JSON(http.StatusBadGateway, gin.H{"error": errUpstreamDown})
		return
	}
	if resp.StatusCode >= 400 {
		// 4xx is the credential-rotation indicator (KLIPY rejecting app_key →
		// typically 401/403). Logging surfaces this to operators without
		// leaking the slug/URL.
		h.log.Warn("klipy media: upstream 4xx",
			"host", parsed.Host,
			"status", resp.StatusCode)
		c.Status(resp.StatusCode)
		return
	}

	// Forward content headers from upstream
	if ct := resp.Header.Get(headerContentType); ct != "" {
		c.Header(headerContentType, ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		c.Header("Content-Length", cl)
	}
	// Use the upstream Cache-Control if present; otherwise apply a conservative
	// public cache with a 1-hour TTL so CDN bursts are absorbed without risk of
	// stale content. KLIPY ToS does not prohibit client-side caching.
	if cc := resp.Header.Get(headerCacheControl); cc != "" {
		c.Header(headerCacheControl, cc)
	} else {
		c.Header(headerCacheControl, "public, max-age=3600")
	}
	c.Status(http.StatusOK)
	if _, copyErr := io.Copy(c.Writer, resp.Body); copyErr != nil && !errors.Is(copyErr, io.EOF) {
		// Best-effort: client likely disconnected mid-stream. Don't log slugs.
		return
	}
}

// --- Internal helpers ---

// proxyAPIGet builds a KLIPY API URL from the path + whitelisted query params,
// fetches the upstream JSON response, and forwards it to the client.
func (h *Handler) proxyAPIGet(c *gin.Context, path string, allowedParams []string) {
	upstreamURL := h.buildUpstreamURL(path, c, allowedParams)
	h.forwardJSON(c, http.MethodGet, upstreamURL, nil)
}

// proxyAPIRequest is the same as proxyAPIGet but for non-GET methods.
// No request body forwarding is supported in v1 — KLIPY's POST/DELETE endpoints
// in our scope take all parameters in the URL.
func (h *Handler) proxyAPIRequest(c *gin.Context, method, path string, _ io.Reader) {
	upstreamURL := h.buildUpstreamURL(path, c, nil)
	h.forwardJSON(c, method, upstreamURL, nil)
}

// sanitizeParam normalizes a query param value for forwarding to KLIPY.
// Numeric pagination params are validated; per_page is clamped to maxPerPage
// so callers cannot request unbounded results. Returns the value to forward,
// or "" to skip the param entirely.
func sanitizeParam(name, raw string) string {
	switch name {
	case "per_page":
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return ""
		}
		if n > maxPerPage {
			n = maxPerPage
		}
		return strconv.Itoa(n)
	case "page":
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return ""
		}
		return strconv.Itoa(n)
	default:
		return raw
	}
}

// buildUpstreamURL constructs the upstream KLIPY URL with the server-side
// app_key injected into the path and only the whitelisted query params copied
// from the incoming request. Any other query params (including a client-supplied
// app_key, defense-in-depth) are dropped. Numeric pagination params are
// validated and clamped via sanitizeParam.
func (h *Handler) buildUpstreamURL(path string, c *gin.Context, allowedParams []string) string {
	base := fmt.Sprintf("%s/%s%s", klipyAPIBase, h.cfg.KlipyAPIKey, path)
	if len(allowedParams) == 0 {
		return base
	}
	values := url.Values{}
	for _, name := range allowedParams {
		raw := c.Query(name)
		if raw == "" {
			continue
		}
		if v := sanitizeParam(name, raw); v != "" {
			values.Set(name, v)
		}
	}
	if len(values) == 0 {
		return base
	}
	if strings.Contains(base, "?") {
		return base + "&" + values.Encode()
	}
	return base + "?" + values.Encode()
}

// forwardJSON performs the upstream HTTP request and copies the response to the
// client. JSON content type is preserved; rate limit + auth headers are stripped
// to avoid leaking the app_key in error envelopes.
func (h *Handler) forwardJSON(c *gin.Context, method, upstreamURL string, body io.Reader) {
	// http.NewRequestWithContext is structurally unreachable as an error path
	// here — all callers pass an internal-controlled method constant and a
	// upstreamURL built from the validated config base URL. Defensive code
	// stays for future-proofing but is intentionally not logged or tested.
	req, err := http.NewRequestWithContext(c.Request.Context(), method, upstreamURL, body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": errUpstreamDown})
		return
	}
	req.Header.Set("Accept", "application/json")

	resp, err := h.client.Do(req) //nolint:gosec // upstreamURL from config base URL, not user input; dial-time IP egress guard (#1361)
	if err != nil {
		h.log.Warn("klipy api: upstream request failed",
			"route", c.FullPath(),
			"method", method,
			"error", sanitizedNetErr(err))
		c.JSON(http.StatusBadGateway, gin.H{"error": errUpstreamDown})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 500 {
		h.log.Warn("klipy api: upstream 5xx",
			"route", c.FullPath(),
			"method", method,
			"status", resp.StatusCode)
		c.JSON(http.StatusBadGateway, gin.H{"error": errUpstreamDown})
		return
	}
	if resp.StatusCode >= 400 {
		// 4xx is the credential-rotation indicator (KLIPY rejecting app_key →
		// typically 401/403). Logging surfaces this to operators without
		// leaking the slug/URL/search term.
		//
		// Mirror the status but never forward the upstream error body — KLIPY's
		// error envelopes can echo the URL (and therefore the app_key) and we
		// must not leak that to the client. Surface a generic message instead.
		h.log.Warn("klipy api: upstream 4xx",
			"route", c.FullPath(),
			"method", method,
			"status", resp.StatusCode)
		c.JSON(resp.StatusCode, gin.H{"error": errUpstreamFailed})
		return
	}

	if ct := resp.Header.Get(headerContentType); ct != "" {
		c.Header(headerContentType, ct)
	}
	c.Status(resp.StatusCode)
	if _, copyErr := io.Copy(c.Writer, resp.Body); copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return
	}
}
