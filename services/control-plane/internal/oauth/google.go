package oauth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// GoogleConfig is the constructor input for GoogleProvider.
// Mirrored on pkg/config/GoogleSSOConfig but kept package-local so callers
// pass exactly what GoogleProvider needs and nothing more.
//
// ClientSecret was removed in #975: the desktop client performs the code
// exchange directly (client-driven PKCE), so the server never needs the
// Google OAuth client secret.
type GoogleConfig struct {
	ClientID    string
	RedirectURI string

	// Endpoint overrides (default to Google's production URLs when empty).
	// Tests point these at httptest.Server. TokenEndpoint was removed in #975:
	// the server no longer exchanges codes (client-driven PKCE).
	JWKSEndpoint string
	Issuer       string

	// HTTPClient is optional; defaults to a 10s-timeout client whose Transport
	// is the standard http.DefaultTransport (TLS verification enabled).
	// Tests inject a fake httptest.Server-backed client.
	// Production callers MUST leave this nil or supply a client whose Transport
	// preserves TLS verification — never set InsecureSkipVerify: true.
	HTTPClient *http.Client
}

const (
	googleAuthEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultJWKSEndpoint = "https://www.googleapis.com/oauth2/v3/certs"
	defaultIssuer       = "https://accounts.google.com"
	// Google also mints id_tokens with the bare issuer form; accepted alongside
	// defaultIssuer (parity with the desktop verifier — #975 review L1).
	bareGoogleIssuer = "accounts.google.com"
)

// Compile-time check that *GoogleProvider implements Provider.
var _ Provider = (*GoogleProvider)(nil)

// GoogleProvider implements Provider for Google's OIDC code flow with PKCE.
type GoogleProvider struct {
	cfg        GoogleConfig
	httpClient *http.Client

	jwksMu    sync.RWMutex
	jwksCache map[string]cachedKey
}

// NewGoogleProvider validates configuration and returns a provider ready to use.
func NewGoogleProvider(cfg GoogleConfig) (*GoogleProvider, error) {
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("oauth/google: ClientID is required")
	}
	if cfg.RedirectURI == "" {
		return nil, fmt.Errorf("oauth/google: RedirectURI is required")
	}
	if cfg.JWKSEndpoint == "" {
		cfg.JWKSEndpoint = defaultJWKSEndpoint
	}
	if cfg.Issuer == "" {
		cfg.Issuer = defaultIssuer
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &GoogleProvider{cfg: cfg, httpClient: httpClient}, nil
}

// Name returns "google".
func (g *GoogleProvider) Name() string { return "google" }

// AuthorizationURL builds the URL the client opens in the system browser.
func (g *GoogleProvider) AuthorizationURL(state, nonce, codeChallenge string) string {
	q := url.Values{}
	q.Set("client_id", g.cfg.ClientID)
	q.Set("redirect_uri", g.cfg.RedirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("prompt", "select_account")
	return googleAuthEndpoint + "?" + q.Encode()
}

type googleClaims struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	Nonce         string `json:"nonce"`
	jwt.RegisteredClaims
}

// validateIDToken verifies signature, iss, aud, exp, and nonce.
func (g *GoogleProvider) validateIDToken(ctx context.Context, idToken, expectedNonce string) (*googleClaims, error) {
	parser := jwt.NewParser(
		jwt.WithAudience(g.cfg.ClientID),
		jwt.WithExpirationRequired(),
	)
	claims := &googleClaims{}
	tok, err := parser.ParseWithClaims(idToken, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Method.Alg())
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("id_token has no kid")
		}
		return g.fetchKey(ctx, kid)
	})
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return nil, fmt.Errorf("id_token exp: %w", err)
		case errors.Is(err, jwt.ErrTokenInvalidAudience):
			return nil, fmt.Errorf("id_token aud: %w", err)
		}
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("id_token invalid")
	}
	// Issuer: golang-jwt v5's WithIssuer is single-valued, but Google mints
	// id_tokens with either the canonical "https://accounts.google.com" or the
	// historical bare "accounts.google.com". Accept both (parity with the
	// desktop verifier's GOOGLE_ISSUERS) so the server re-verification never
	// rejects a token the client accepted (#975 review L1). Fails closed.
	if !g.issuerAccepted(claims.Issuer) {
		return nil, fmt.Errorf("id_token iss: unexpected issuer %q", claims.Issuer)
	}
	if claims.Nonce != expectedNonce {
		return nil, fmt.Errorf("id_token nonce mismatch: got %q expected %q", claims.Nonce, expectedNonce)
	}
	return claims, nil
}

// issuerAccepted reports whether iss is a valid Google id_token issuer. When the
// production default issuer is configured, both the canonical and bare forms are
// accepted (parity with the desktop verifier); a test/override issuer matches
// exactly. Always fails closed for any other value.
func (g *GoogleProvider) issuerAccepted(iss string) bool {
	if iss == g.cfg.Issuer {
		return true
	}
	return g.cfg.Issuer == defaultIssuer && iss == bareGoogleIssuer
}

// JWKS cache (1h TTL).
type cachedKey struct {
	key       *rsa.PublicKey
	expiresAt time.Time
}

// jwksKey is one entry in the Google JWKS response. RSA-only fields; we
// ignore non-RSA entries during parse.
type jwksKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// jwksResponse is the top-level JWKS payload Google returns at /oauth2/v3/certs.
type jwksResponse struct {
	Keys []jwksKey `json:"keys"`
}

// parseJWKResponse converts a decoded JWKS payload into a kid→public-key map.
// Per-key parse failures (non-RSA Kty, malformed base64 N or E) are skipped
// silently — Google occasionally publishes keys we don't understand and the
// goal is to extract whatever signing keys we CAN use, not to fail the whole
// fetch on one stray entry.
func parseJWKResponse(resp *jwksResponse) map[string]*rsa.PublicKey {
	out := make(map[string]*rsa.PublicKey, len(resp.Keys))
	for _, k := range resp.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		eInt := 0
		for _, b := range eBytes {
			eInt = eInt<<8 | int(b)
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: eInt}
	}
	return out
}

func (g *GoogleProvider) fetchKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	g.jwksMu.RLock()
	if ck, ok := g.jwksCache[kid]; ok && time.Now().Before(ck.expiresAt) {
		g.jwksMu.RUnlock()
		return ck.key, nil
	}
	g.jwksMu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.JWKSEndpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.httpClient.Do(req) //nolint:gosec // JWKSEndpoint set from trusted config (env or hardcoded default)
	if err != nil {
		return nil, fmt.Errorf("JWKS HTTP: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var jwksResp jwksResponse
	if err := json.NewDecoder(resp.Body).Decode(&jwksResp); err != nil {
		return nil, fmt.Errorf("JWKS decode: %w", err)
	}

	parsed := parseJWKResponse(&jwksResp)

	// Concurrent cache misses on the same kid will all issue a JWKS fetch and
	// each acquire this lock in turn to write the same keys (last-write-wins).
	// Acceptable at human-paced auth volume; if telemetry shows fetch storms,
	// coalesce concurrent fetches via golang.org/x/sync/singleflight.Group.
	g.jwksMu.Lock()
	defer g.jwksMu.Unlock()
	if g.jwksCache == nil {
		g.jwksCache = make(map[string]cachedKey)
	}
	expiresAt := time.Now().Add(time.Hour)
	for k, pub := range parsed {
		g.jwksCache[k] = cachedKey{key: pub, expiresAt: expiresAt}
	}

	if ck, ok := g.jwksCache[kid]; ok {
		return ck.key, nil
	}
	return nil, fmt.Errorf("JWKS: kid %q not found", kid)
}
