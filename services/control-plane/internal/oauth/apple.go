package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Apple OIDC endpoint defaults. Tests override via AppleConfig.
//
// Note: appleIssuer is declared in apple_clientsecret.go and reused here as
// both (a) the "aud" claim for the client_secret JWT (required by Apple) and
// (b) the expected "iss" claim for verified id_tokens. Same string, different
// roles in the protocol.
const (
	appleAuthEndpoint    = "https://appleid.apple.com/auth/authorize"
	appleJWKSEndpoint    = "https://appleid.apple.com/auth/keys"
	applePrivateRelayDom = "privaterelay.appleid.com"
)

// AppleConfig is the constructor input for AppleProvider. Mirrored on
// pkg/config/AppleSSOConfig but kept package-local so callers pass exactly
// what AppleProvider needs and nothing more.
type AppleConfig struct {
	ClientID    string // Apple Services ID, e.g. "chat.concordvoice.signin"
	TeamID      string // 10-char Apple Developer Team ID
	KeyID       string // 10-char Apple Sign-In Key ID
	PrivateKey  []byte // #nosec G117 G101 -- False positive: config field name, actual key loaded from env (.p8 PEM bytes)
	RedirectURI string // per-request loopback URI supplied at Initiate time

	// Endpoint overrides default to Apple's production URLs when empty.
	// Tests point these at httptest.Server.
	AuthEndpoint string
	JWKSEndpoint string
	Issuer       string

	// Clock is injectable for deterministic tests. Defaults to time.Now.
	Clock func() time.Time

	// HTTPClient is optional; defaults to a 10s-timeout client whose Transport
	// is the standard http.DefaultTransport (TLS verification enabled).
	// Production callers MUST leave this nil or supply a client whose
	// Transport preserves TLS verification — never set InsecureSkipVerify.
	HTTPClient *http.Client
}

// Compile-time check that *AppleProvider implements Provider.
var _ Provider = (*AppleProvider)(nil)

// AppleProvider implements Provider for Apple's OIDC code flow with PKCE,
// ES256 client_secret JWTs, and form_post callbacks.
type AppleProvider struct {
	cfg          AppleConfig
	httpClient   *http.Client
	brokerSecret *appleClientSecretSigner
	clock        func() time.Time

	jwksMu    sync.RWMutex
	jwksCache map[string]cachedKey
}

// NewAppleProvider validates configuration, parses the PKCS8 PEM private key,
// asserts it is a P-256 ECDSA key, and returns a provider ready to use.
func NewAppleProvider(cfg AppleConfig) (*AppleProvider, error) {
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("oauth/apple: ClientID is required")
	}
	if cfg.TeamID == "" {
		return nil, fmt.Errorf("oauth/apple: TeamID is required")
	}
	if cfg.KeyID == "" {
		return nil, fmt.Errorf("oauth/apple: KeyID is required")
	}
	if len(cfg.PrivateKey) == 0 {
		return nil, fmt.Errorf("oauth/apple: PrivateKey is required")
	}
	if cfg.RedirectURI == "" {
		return nil, fmt.Errorf("oauth/apple: RedirectURI is required")
	}

	priv, err := parseApplePrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("oauth/apple: parse PrivateKey: %w", err)
	}

	if cfg.AuthEndpoint == "" {
		cfg.AuthEndpoint = appleAuthEndpoint
	}
	if cfg.JWKSEndpoint == "" {
		cfg.JWKSEndpoint = appleJWKSEndpoint
	}
	if cfg.Issuer == "" {
		cfg.Issuer = appleIssuer
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	brokerSigner := newAppleClientSecretSigner(cfg.TeamID, cfg.ClientID, cfg.KeyID, priv, brokerClientSecretTTL)

	return &AppleProvider{
		cfg:          cfg,
		httpClient:   httpClient,
		brokerSecret: brokerSigner,
		clock:        clock,
	}, nil
}

// parseApplePrivateKey decodes a PEM-encoded PKCS8 private key and asserts it
// is a P-256 ECDSA key. Apple's .p8 export format is exactly this.
func parseApplePrivateKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("not a PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("PKCS8: %w", err)
	}
	priv, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("expected EC P-256 private key, got non-ECDSA key")
	}
	if priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("expected EC P-256 private key, got curve %s", priv.Curve.Params().Name)
	}
	return priv, nil
}

// Name returns "apple".
func (a *AppleProvider) Name() string { return "apple" }

// AuthorizationURL builds the URL the client opens in the system browser.
// Apple requires response_mode=form_post when scope=name email is requested,
// so the redirect_uri receives the callback as a POST body rather than a GET
// query.
func (a *AppleProvider) AuthorizationURL(state, nonce, codeChallenge string) string {
	q := url.Values{}
	q.Set("client_id", a.cfg.ClientID)
	q.Set("redirect_uri", a.cfg.RedirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "name email")
	q.Set("response_mode", "form_post")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	return a.cfg.AuthEndpoint + "?" + q.Encode()
}

// BrokerClientSecret returns a freshly-signed 60-second client-secret JWT for
// the client-driven exchange path (epic #971 / #972). It uses a dedicated
// signer instance: the Exchange path's signer carries a long TTL (90 days in
// production) plus a reuse cache, both of which would be wrong to hand to
// clients. See the #972 spec, finding F1.
func (a *AppleProvider) BrokerClientSecret(now time.Time) (string, error) {
	return a.brokerSecret.signed(now)
}

// isAppleRelayEmail returns true when either the domain matches Apple's relay
// suffix OR the id_token's is_private_email claim is true. Defense in depth:
// any single positive signal is sufficient, which protects against future
// changes to Apple's relay surface (new domains, claim-only signaling, etc.).
func isAppleRelayEmail(email string, isPrivate bool) bool {
	if isPrivate {
		return true
	}
	return strings.HasSuffix(strings.ToLower(email), "@"+applePrivateRelayDom)
}

// appleBool unmarshals a JSON value that Apple may serialize as either a
// JSON boolean (`true`/`false`) OR a JSON string ("true"/"false"). The
// quirk affects email_verified, is_private_email, and nonce_supported on
// Apple's id_token — historically delivered as strings, now sometimes as
// booleans depending on the endpoint. Accepting both forms is defense-in-
// depth: a boolean-only field would silently break against string-form
// tokens with `oauth_token_invalid` errors that look like signature/claim
// validation failures, hiding the real cause.
//
// Empty/missing values map to false (matches Go's json.Unmarshal default
// for absent bool fields).
type appleBool bool

func (b *appleBool) UnmarshalJSON(data []byte) error {
	switch s := strings.Trim(string(data), `"`); s {
	case "true":
		*b = true
		return nil
	case "false", "":
		*b = false
		return nil
	default:
		return fmt.Errorf("appleBool: cannot unmarshal %q (expected true/false or \"true\"/\"false\")", s)
	}
}

// appleClaims is the subset of Apple's id_token claims we verify. Apple sets
// is_private_email=true on relay-issued addresses; nonce_supported=true is
// asserted on all modern tokens (Apple has supported nonces since 2020).
//
// Bool fields use appleBool to tolerate Apple's historical quirk of
// serializing these as JSON strings ("true"/"false") rather than JSON
// booleans on some endpoint paths. See appleBool's comment.
type appleClaims struct {
	Email          string    `json:"email"`
	EmailVerified  appleBool `json:"email_verified"`
	IsPrivateEmail appleBool `json:"is_private_email"`
	Nonce          string    `json:"nonce"`
	NonceSupported appleBool `json:"nonce_supported"`
	jwt.RegisteredClaims
}

// validateIDToken verifies signature, iss, aud, exp, iat, nonce, and
// nonce_supported on Apple's id_token.
//
// Algorithm-confusion defense: the keyfunc type-asserts *jwt.SigningMethodRSA,
// rejecting HS256-signed tokens that would otherwise verify against the JWKS
// public key bytes treated as an HMAC secret.
//
// Temporal validity: WithExpirationRequired enforces an exp claim and
// rejects expired tokens; WithIssuedAt rejects tokens whose iat claim is
// in the future (clock-skew defense — Apple controls iat, but a hostile
// proxy can't manufacture a token with an iat ahead of now).
//
// Order of checks after Parse: nonce_supported defense runs BEFORE the
// nonce equality check so that a token without nonce binding is rejected
// on its own terms (claim missing/false), not via the indirect nonce
// mismatch error. Both rejections are equivalent for the authorization
// outcome, but distinguishing them at the error layer aids triage.
func (a *AppleProvider) validateIDToken(ctx context.Context, idToken, expectedNonce string) (*appleClaims, error) {
	parser := jwt.NewParser(
		jwt.WithIssuer(a.cfg.Issuer),
		jwt.WithAudience(a.cfg.ClientID),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	)
	claims := &appleClaims{}
	tok, err := parser.ParseWithClaims(idToken, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Method.Alg())
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("id_token has no kid")
		}
		return a.fetchKey(ctx, kid)
	})
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return nil, fmt.Errorf("id_token exp: %w", err)
		case errors.Is(err, jwt.ErrTokenInvalidAudience):
			return nil, fmt.Errorf("id_token aud: %w", err)
		case errors.Is(err, jwt.ErrTokenInvalidIssuer):
			return nil, fmt.Errorf("id_token iss: %w", err)
		case errors.Is(err, jwt.ErrTokenUsedBeforeIssued):
			return nil, fmt.Errorf("id_token iat: %w", err)
		}
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("id_token invalid")
	}
	if !claims.NonceSupported {
		return nil, errors.New("id_token nonce_supported claim missing or false")
	}
	if claims.Nonce != expectedNonce {
		return nil, fmt.Errorf("id_token nonce mismatch: got %q expected %q", claims.Nonce, expectedNonce)
	}
	return claims, nil
}

// fetchKey returns the RSA public key for the given kid, populating the
// per-provider JWKS cache on miss. Cache policy: 1h TTL, write-through
// last-write-wins on concurrent miss (acceptable at human-paced auth volume;
// coalesce via singleflight if telemetry shows fetch storms).
//
// Uses the shared cachedKey, jwksKey, jwksResponse types and parseJWKResponse
// helper from google.go since both providers' JWKS responses are RFC 7517
// RSA JWK Sets — same wire format, no need to duplicate. A unilateral edit
// to either provider's JWKS handling is a deliberate divergence that should
// be reviewed against the other.
func (a *AppleProvider) fetchKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.jwksMu.RLock()
	if ck, ok := a.jwksCache[kid]; ok && time.Now().Before(ck.expiresAt) {
		a.jwksMu.RUnlock()
		return ck.key, nil
	}
	a.jwksMu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.JWKSEndpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.httpClient.Do(req) //nolint:gosec // JWKSEndpoint set from trusted config (env or hardcoded default)
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
	a.jwksMu.Lock()
	defer a.jwksMu.Unlock()
	if a.jwksCache == nil {
		a.jwksCache = make(map[string]cachedKey)
	}
	expiresAt := time.Now().Add(time.Hour)
	for k, pub := range parsed {
		a.jwksCache[k] = cachedKey{key: pub, expiresAt: expiresAt}
	}

	if ck, ok := a.jwksCache[kid]; ok {
		return ck.key, nil
	}
	return nil, fmt.Errorf("JWKS: kid %q not found", kid)
}

// parseAppleUserData decodes Apple's first-auth user JSON, returning a
// best-effort display name. Returns "" on empty input or parse failure
// (subsequent auths legitimately have no user data, and a malformed payload
// must not block sign-in).
func parseAppleUserData(raw string) string {
	if raw == "" {
		return ""
	}
	var u struct {
		Name struct {
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
		} `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		return ""
	}
	return strings.TrimSpace(u.Name.FirstName + " " + u.Name.LastName)
}
