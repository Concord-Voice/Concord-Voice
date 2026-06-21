package oauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// HandlerDeps wires the SSO endpoints. Consumed by api/routes wiring.
type HandlerDeps struct {
	Registry *Registry
	Redis    redis.Cmdable
	DB       *sql.DB
	// AuthHandler grants the SSO flow access to token issuance,
	// password verification (link path), and MFA challenge issuance.
	// Typed as an interface (AuthAdapter) so tests can stub it without
	// pulling in the full *auth.Handler dependency graph.
	AuthHandler AuthAdapter
	// CFKV publishes state→loopback-port mappings to the apple-sso-bridge
	// Workers KV namespace (#973). Nil when the bridge is disabled — the
	// write is skipped and Apple SSO continues on its pre-bridge path.
	CFKV StatePortPutter
	// Log receives structured SSO audit events (apple_client_secret_minted).
	// Nil-safe: audit emission is skipped when unset (test rigs); production
	// wiring (buildOAuthHandler) always sets it.
	Log *logger.Logger
	// AuditIPKey keys the HMAC that pseudonymizes client IPs in audit events.
	// HKDF-derived from JWTSecret at wiring time — see buildOAuthHandler.
	AuditIPKey []byte
}

// AuthAdapter is the subset of internal/auth.Handler methods this package needs.
// Defined here as an interface so tests can stub it without instantiating the
// full *auth.Handler (database, Redis, MFA checker, etc.). The contract lives
// in internal/oauth alongside its consumers.
//
// Note on import direction: internal/oauth DOES import internal/auth (for
// auth.SetRefreshCookie + auth.ErrAccountLocked — see Task 13 follow-ups in
// CompleteLink and respondExistingSSO). The interface is not a cycle defense
// but a test-stubbing convenience; the concrete binding lives in
// internal/auth/oauth_adapter.go.
type AuthAdapter interface {
	// IssueAccessAndRefresh mints an access token and a refresh-token cookie
	// header value for the given userID. The cookie is returned as a Set-Cookie
	// header value so the caller can attach it directly via c.Header.
	IssueAccessAndRefresh(ctx context.Context, userID string) (accessToken string, refreshCookie string, err error)
	// IssueMFAChallenge mints a short-lived MFA challenge token for users with
	// MFA enabled. Returns:
	//   - challengeToken: short-lived JWT the renderer presents to the modal
	//   - methods: login-eligible methods (e.g. ["totp","webauthn"]) — drives modal layout
	//   - recoveryOnlyMethods: methods present but disqualified for login (e.g. backup_code)
	//   - webauthnOptions: PublicKeyCredentialRequestOptions when "webauthn" is in methods, else nil
	//   - mfaEnabled: false when the user has no MFA enrolled — caller must skip the
	//     challenge step and issue tokens directly. The password path checks IsEnabled
	//     before calling handleMFAChallenge; the SSO path's only entry point is here, so
	//     surfacing it on this method keeps behavior parallel.
	IssueMFAChallenge(ctx context.Context, userID string) (challengeToken string, methods []string, recoveryOnlyMethods []string, webauthnOptions interface{}, mfaEnabled bool, err error)
	// VerifyPassword performs a constant-time password check against the user's
	// stored Argon2id hash, used by the link-existing-account flow.
	VerifyPassword(ctx context.Context, userID, password string) error
	// HashPassword computes an Argon2id hash of the supplied passphrase, used
	// by the new-user SSO registration path to populate users.password_hash.
	// Even when password login is disabled, the column is NOT NULL — the hash
	// is stored as a placeholder that becomes usable only if the user later
	// flips password_login_disabled (e.g., via account-recovery flow).
	HashPassword(ctx context.Context, password string) (string, error)
	// ValidateUsername delegates to internal/auth/username.ValidateUsername:
	// charset (alphanumeric + . _ -), length bounds, no consecutive specials,
	// reserved/blocked words, and de-leetspeak profanity check. Defined here
	// to keep the SSO path's username gating identical to the password-path
	// registration without creating an internal/auth import cycle.
	ValidateUsername(username string) error
	// ValidatePasswordStrength delegates to internal/auth/password.ValidatePasswordStrength:
	// length bounds and char-class diversity (≥3 of upper/lower/digit/special).
	// The 128-char max is the critical DoS defense — Argon2id hashing scales
	// linearly with input length, so an unbounded password is a CPU-burn vector.
	ValidatePasswordStrength(password string) error
}

// Handler serves the SSO endpoints (Initiate, ProviderSession, Link, Complete).
type Handler struct {
	deps HandlerDeps
}

// NewHandler constructs a Handler from its dependencies.
func NewHandler(deps HandlerDeps) *Handler { return &Handler{deps: deps} }

const (
	// stateKeyPrefix namespaces the per-attempt OAuth state record in Redis.
	stateKeyPrefix = "sso_state:"
	// ssoTokenKeyPrefix namespaces the short-lived sso_token issued after
	// successful provider exchange (consumed by the link-or-complete step).
	ssoTokenKeyPrefix = "sso_token:" // #nosec G101 -- key prefix, not a credential
	// stateTTL bounds how long an in-flight Initiate->Session can take.
	stateTTL = 10 * time.Minute
	// ssoTokenTTL bounds the link-or-complete window after callback.
	ssoTokenTTL = 5 * time.Minute
	// entropyBytes is 256 bits — exceeds OAuth state/nonce minimums and the
	// PKCE RFC 7636 recommendation (32 bytes after base64url encoding).
	entropyBytes = 32
)

// StatePortPutter is the seam to the Cloudflare KV bridge client; satisfied
// by *cfkv.Client. Interface lives here so tests stub it without importing
// the real client.
type StatePortPutter interface {
	Put(ctx context.Context, key, value string, ttlSeconds int) error
}

// bridgeStateTTLSeconds keeps the KV entry's lifetime aligned with the Redis
// sso_state record (spec F1 — the user may spend minutes at Apple's login).
var bridgeStateTTLSeconds = int(stateTTL / time.Second)

// sso_token "branch" discriminator values — the JSON field that distinguishes
// new-user-registration tokens from account-link tokens. Centralized here so
// the producer (respondNewUser / respondAccountLink) and the consumer
// (CompleteRegistration / CompleteLink) cannot drift apart on the literal.
// A drift would silently break the sso_token branch gate, which is the
// security boundary that prevents a stolen link-flow token from being pivoted
// into a new-user registration in the targeted user's name.
const (
	ssoBranchNewUser     = "new_user"
	ssoBranchAccountLink = "account_link"
)

// maxAppleUserDataBytes caps the apple_user_data field on both Callback and
// AppleSession — mirrors the renderer loopback's body cap (ssoLoopback.ts
// MAX_POST_BODY_BYTES). Oversize input silently truncates to "", which is the
// same legitimate state as Apple's subsequent-auth flow (no name).
const maxAppleUserDataBytes = 64 * 1024

// ssoStateRecord is the JSON payload stored in Redis under sso_state:<state>.
// All fields are required for the callback handler to validate the response
// and complete the code exchange.
type ssoStateRecord struct {
	Provider string `json:"provider"`
	// State duplicates the record's own key suffix so the client-secret
	// broker (#972) can constant-time-compare a request-supplied state
	// against a server-held copy instead of trusting key-lookup semantics.
	// Records written before this field existed decode as "" and fail the
	// broker's comparison closed.
	State        string    `json:"state"`
	Nonce        string    `json:"nonce"`
	CodeVerifier string    `json:"code_verifier"`
	RedirectURI  string    `json:"redirect_uri"`
	CreatedAt    time.Time `json:"created_at"`
}

// ssoTokenPayload is the JSON payload stored in Redis under sso_token:<token>.
// Shared by both the new-user-registration branch (CompleteRegistration) and
// the account-link branch (CompleteLink) — Go's zero-value semantics mean a
// new-user token's missing TargetUserID decodes as "", which is exactly what
// the account-link branch validates against.
//
// This type is the single source of truth for the producer/consumer schema:
// respondNewUser / respondAccountLink build it via map literal, and
// consumeSSOToken decodes it on the consume side. The branch field is the
// security boundary that prevents a stolen link-flow token from being pivoted
// into a new-user registration in the targeted user's name (and vice versa).
type ssoTokenPayload struct {
	Provider       string `json:"provider"`
	ProviderUserID string `json:"provider_user_id"`
	ProviderEmail  string `json:"provider_email"`
	IsRelayEmail   bool   `json:"is_relay_email"`
	TargetUserID   string `json:"target_user_id"`
	Branch         string `json:"branch"`
}

// consumeSSOToken performs the GET-then-DEL single-use Redis dance, unmarshals
// the payload, and validates that the branch matches the caller's expectation.
//
// Returns sentinel error errSSOTokenInvalid for any of the four failure modes
// (Redis miss, JSON unmarshal failure, branch mismatch, missing TargetUserID
// when the account-link branch is expected). The caller translates that into
// HTTP 401 sso_token_invalid.
//
// Get-then-Del is not atomic; see Callback's same-pattern note. Acceptable for
// Phase A — a concurrent duplicate-consume could only race on a token never
// seen by an attacker.
func (h *Handler) consumeSSOToken(ctx context.Context, token, expectedBranch string) (*ssoTokenPayload, error) {
	tokenKey := ssoTokenKeyPrefix + token
	raw, err := h.deps.Redis.Get(ctx, tokenKey).Bytes()
	if err != nil {
		return nil, errSSOTokenInvalid
	}
	// Best-effort delete: token has been read so a delete failure does not
	// enable replay within this request.
	_, _ = h.deps.Redis.Del(ctx, tokenKey).Result()

	var info ssoTokenPayload
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, errSSOTokenInvalid
	}
	if info.Branch != expectedBranch {
		return nil, errSSOTokenInvalid
	}
	// For the account-link branch, the target_user_id is load-bearing — refuse
	// malformed records that would let an attacker without a target pivot an
	// SSO link.
	if expectedBranch == ssoBranchAccountLink && info.TargetUserID == "" {
		return nil, errSSOTokenInvalid
	}
	return &info, nil
}

// errSSOTokenInvalid collapses all sso_token consume failures into a single
// sentinel — the response body never echoes detail (which would leak whether
// the token was missing vs malformed vs branch-wrong), so the caller doesn't
// need to discriminate.
var errSSOTokenInvalid = errors.New("oauth: sso_token invalid")

// decodeSSOKeyMaterial decodes the three base64-StdEncoding fields from the
// CompleteRegistration request body. Returns the first failing decode's error
// so the caller's error_code translation stays opaque (all three failures map
// to invalid_key_material).
func decodeSSOKeyMaterial(req *completeRegistrationRequest) (wrapped, salt, pubKey []byte, err error) {
	wrapped, err = base64.StdEncoding.DecodeString(req.WrappedPrivateKey)
	if err != nil {
		return nil, nil, nil, err
	}
	salt, err = base64.StdEncoding.DecodeString(req.KeyDerivationSalt)
	if err != nil {
		return nil, nil, nil, err
	}
	pubKey, err = base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil {
		return nil, nil, nil, err
	}
	return wrapped, salt, pubKey, nil
}

// createSSOUserParams bundles the non-context inputs to createSSOUser so the
// function stays under the project's 7-parameter ceiling (SonarQube S107).
// All fields are required — there are no optional inputs on this path.
type createSSOUserParams struct {
	Info     *ssoTokenPayload
	Hash     string
	Username string
	Wrapped  []byte
	Salt     []byte
	PubKey   []byte
}

// createSSOUser performs the four-INSERT atomic block that finalizes a new
// SSO-registered user: users → user_keys → public_keys → user_sso_identities.
// Returns the new user_id plus a stable errCode string the caller maps to an
// HTTP status. errCode is "" on success.
//
// errCode values:
//   - "email_taken"                 → 409
//   - "username_taken"              → 409
//   - "user_create_failed"          → 500
//   - "key_insert_failed"           → 500
//   - "pubkey_insert_failed"        → 500
//   - "sso_identity_insert_failed"  → 500
func (h *Handler) createSSOUser(ctx context.Context, tx *sql.Tx, p *createSSOUserParams) (userID, errCode string, err error) {
	err = tx.QueryRowContext(ctx,
		`INSERT INTO users (email, username, password_hash, email_verified,
		                    password_login_disabled, trust_sso_security)
		 VALUES ($1, $2, $3, TRUE, TRUE, FALSE)
		 RETURNING id::text`,
		p.Info.ProviderEmail, p.Username, p.Hash,
	).Scan(&userID)
	if err != nil {
		// Translate UNIQUE violations to a stable error_code so the caller can
		// surface the right field-level error. Constraint names are Postgres-
		// auto-generated from the inline UNIQUE columns in migration 000001
		// (users_email_key, users_username_key).
		if strings.Contains(err.Error(), "users_email_key") {
			return "", "email_taken", err
		}
		if strings.Contains(err.Error(), "users_username_key") {
			return "", "username_taken", err
		}
		return "", "user_create_failed", err
	}

	// user_keys: explicitly stamp key_derivation_alg='argon2id' so this row
	// is not stuck on the legacy 'pbkdf2' default — SSO registrations are
	// new code paths and should always use the modern KDF.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_keys (user_id, wrapped_private_key, key_derivation_salt, key_derivation_alg, key_version)
		 VALUES ($1, $2, $3, 'argon2id', 1)`,
		userID, p.Wrapped, p.Salt,
	); err != nil {
		return "", "key_insert_failed", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO public_keys (user_id, public_key, key_version) VALUES ($1, $2, 1)`,
		userID, p.PubKey,
	); err != nil {
		return "", "pubkey_insert_failed", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email, is_relay_email)
		 VALUES ($1, $2, $3, $4, $5)`,
		userID, p.Info.Provider, p.Info.ProviderUserID, p.Info.ProviderEmail, p.Info.IsRelayEmail,
	); err != nil {
		return "", "sso_identity_insert_failed", err
	}
	return userID, "", nil
}

// Initiate handles GET /api/v1/auth/sso/:provider?redirect_uri=...
// It generates state, nonce, and a PKCE verifier, stores them in Redis with a
// 10-minute TTL, and returns the provider authorization URL plus the state value.
func (h *Handler) Initiate(c *gin.Context) {
	providerName := c.Param("provider")
	if providerName == "" {
		// Fallback for hard-coded route /sso/google (in case routes are wired
		// without a path parameter during incremental rollout).
		providerName = "google"
	}
	provider, err := h.deps.Registry.Get(providerName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error_code": "unknown_provider"})
		return
	}

	redirectURI := c.Query("redirect_uri")
	if err := validateLoopbackRedirect(redirectURI); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error_code": "invalid_redirect_uri",
			"detail":     err.Error(),
		})
		return
	}

	clientChallenge, ok := h.initiateClientChallenge(c, providerName)
	if !ok {
		return
	}

	bridgePort, ok := h.initiateBridgePort(c, providerName, redirectURI)
	if !ok {
		return
	}

	state := randomString(entropyBytes)
	nonce := randomString(entropyBytes)
	// Both apple and google are client-driven after #974/#975: the desktop
	// generates the verifier and supplies only the S256 challenge. The server
	// never sees the verifier (CodeVerifier stored as "").
	verifier := ""
	challenge := clientChallenge

	authURL := buildAuthURLWithRedirect(provider, state, nonce, challenge, redirectURI)

	rec := ssoStateRecord{
		Provider:     providerName,
		State:        state,
		Nonce:        nonce,
		CodeVerifier: verifier,
		RedirectURI:  redirectURI,
		CreatedAt:    time.Now().UTC(),
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		// Marshalling a known-shape struct of strings + time.Time cannot fail
		// in practice; if it does, it's a programmer error and unrecoverable.
		panic(fmt.Errorf("oauth: marshal state record: %w", err))
	}
	if err := h.deps.Redis.Set(c.Request.Context(), stateKeyPrefix+state, payload, stateTTL).Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "redis_unavailable"})
		return
	}

	// Publish state→loopback-port for the apple-sso-bridge Worker (#973).
	// Apple-only: Google's loopback redirect works without a bridge. The
	// port was validated and captured before the state record was written
	// (see the bridge precondition above). Fail loud — a missing mapping
	// would otherwise surface minutes later as a dead "session expired"
	// page after the user typed their Apple credentials (spec F3). Runs
	// before the apple response below so a bridge failure surfaces here.
	if providerName == "apple" && h.deps.CFKV != nil {
		if err := h.deps.CFKV.Put(c.Request.Context(), state, bridgePort, bridgeStateTTLSeconds); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error_code": "bridge_unavailable"})
			return
		}
	}

	// All client-driven providers (apple, google after #974/#975) return the
	// nonce so the desktop can bind the id_token locally before POSTing
	// /session. The server re-verifies against rec.Nonce regardless
	// (CWE-345 defense in depth), so exposing the nonce grants nothing an
	// attacker could use without also holding the state record.
	c.JSON(http.StatusOK, gin.H{"auth_url": authURL, "state": state, "nonce": nonce})
}

// initiateClientChallenge enforces the client-driven PKCE ownership contract
// (#974/#975): ALL providers are client-driven — the DESKTOP generates the
// verifier and supplies only the S256 challenge; the server never sees the
// verifier (CodeVerifier stored as ""). On a missing/malformed challenge it
// writes the 400 response and returns ok=false.
func (h *Handler) initiateClientChallenge(c *gin.Context, _ string) (string, bool) {
	clientChallenge := c.Query("code_challenge")
	if !codeChallengePattern.MatchString(clientChallenge) {
		c.JSON(http.StatusBadRequest, gin.H{"error_code": "code_challenge_required"})
		return "", false
	}
	return clientChallenge, true
}

// initiateBridgePort validates the bridge precondition (#973):
// validateLoopbackRedirect guarantees scheme/host/path but not an explicit
// port, and the apple-sso-bridge cannot relay without one. Initiate calls this
// BEFORE the state record is written so a rejected redirect leaves no orphan
// sso_state entry in Redis. Non-apple providers (and a nil CFKV) pass through
// with an empty port. On violation it writes the 400 response and returns
// ok=false.
func (h *Handler) initiateBridgePort(c *gin.Context, providerName, redirectURI string) (string, bool) {
	if providerName != "apple" || h.deps.CFKV == nil {
		return "", true
	}
	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed.Port() == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error_code": "invalid_redirect_uri",
			"detail":     "explicit loopback port required for apple",
		})
		return "", false
	}
	return parsed.Port(), true
}

// validateLoopbackRedirect enforces RFC 8252 §7.3 — desktop app SSO must use a
// loopback IP redirect with the well-known /oauth/callback path. This blocks
// open-redirect attacks via attacker-controlled hosts.
func validateLoopbackRedirect(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("redirect_uri required")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("redirect_uri parse: %w", err)
	}
	if u.Scheme != "http" {
		return fmt.Errorf("redirect_uri scheme must be http (loopback)")
	}
	host := u.Hostname()
	if host != "127.0.0.1" {
		return fmt.Errorf("redirect_uri host must be 127.0.0.1")
	}
	if u.Path != "/oauth/callback" {
		return fmt.Errorf("redirect_uri path must be /oauth/callback")
	}
	return nil
}

// randomString returns nBytes of CSPRNG output, base64url-encoded without
// padding. crypto/rand failure is treated as unrecoverable: if the OS RNG is
// broken, no auth flow can proceed safely.
func randomString(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("oauth: crypto/rand failure: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// codeChallengePattern bounds the client-supplied PKCE S256 code_challenge
// (#974, RFC 7636 §4.2): base64url charset, 43–128 chars. An S256 challenge
// is exactly 43 chars (SHA-256, base64url-unpadded); the upper bound
// tolerates future hash agility without admitting unbounded input.
var codeChallengePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43,128}$`)

// buildAuthURLWithRedirect overrides redirect_uri at request time so that
// concurrent OAuth attempts from different ephemeral loopback ports do not
// share a single provider-config value. The provider builds the canonical
// authorization URL; we mutate only redirect_uri.
func buildAuthURLWithRedirect(p Provider, state, nonce, challenge, redirectURI string) string {
	base := p.AuthorizationURL(state, nonce, challenge)
	parsed, err := url.Parse(base)
	if err != nil {
		// Provider URLs are constructed from package constants + url.Values;
		// they cannot fail to parse. If they do, that's a programmer bug.
		panic(fmt.Errorf("oauth: parse provider auth URL: %w", err))
	}
	q := parsed.Query()
	q.Set("redirect_uri", redirectURI)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// callbackBranch identifies which scenario the user fell into after a
// successful provider exchange. The three-way classification is documented in
// the design spec's §4.4 / §7.
type callbackBranch int

const (
	branchNewUser     callbackBranch = iota // No matching SSO identity, no matching email.
	branchExistingSSO                       // (provider, sub) is already linked.
	branchAccountLink                       // No SSO link, but verified email matches a user.
)

// classifyCallback decides which scenario applies to the resolved UserInfo.
// Order matters: an existing SSO link wins over an email match (a previously
// linked user may have rotated their email).
func (h *Handler) classifyCallback(ctx context.Context, info *UserInfo) callbackBranch {
	// Existing SSO identity?
	var dummy string
	err := h.deps.DB.QueryRowContext(ctx,
		`SELECT user_id::text FROM user_sso_identities WHERE provider = $1 AND provider_user_id = $2`,
		info.Provider, info.ProviderUserID,
	).Scan(&dummy)
	if err == nil {
		return branchExistingSSO
	}

	// Email match (case-insensitive, verified-on-both-sides, non-relay)?
	// Relay emails (Apple privaterelay) MUST NOT auto-link to existing accounts.
	if !info.IsRelayEmail {
		err = h.deps.DB.QueryRowContext(ctx,
			`SELECT id::text FROM users WHERE LOWER(email) = LOWER($1) AND email_verified = TRUE`,
			info.Email,
		).Scan(&dummy)
		if err == nil {
			return branchAccountLink
		}
	}
	return branchNewUser
}

// respondNewUser issues an opaque sso_token bound to the provider info and
// returns sso_registration_required. The renderer will POST the token plus
// the user's chosen passphrase + wrapped key to CompleteRegistration.
func (h *Handler) respondNewUser(c *gin.Context, info *UserInfo) {
	ssoToken := randomString(32)
	payload, err := json.Marshal(map[string]any{
		"provider":         info.Provider,
		"provider_user_id": info.ProviderUserID,
		"provider_email":   info.Email,
		"is_relay_email":   info.IsRelayEmail,
		"name":             info.Name,
		"avatar_url":       info.AvatarURL,
		"created_at":       time.Now().UTC().Format(time.RFC3339),
		"target_user_id":   "", // empty for new-user; populated for link
		"branch":           ssoBranchNewUser,
	})
	if err != nil {
		// Marshalling a map of strings + bool + time-string cannot fail.
		// If it does, the runtime is broken — fail loud rather than silently
		// hand the renderer a half-baked sso_token.
		panic(fmt.Errorf("oauth: marshal new-user sso_token payload: %w", err))
	}
	if err := h.deps.Redis.Set(c.Request.Context(), ssoTokenKeyPrefix+ssoToken, payload, ssoTokenTTL).Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "redis_unavailable"})
		return
	}
	// Best-effort: clear any stale pending_registration row for this email so
	// the user can complete SSO registration without a uniqueness conflict.
	// Errcheck-suppressed via _, _ — the worst case is a duplicate-email
	// constraint violation downstream that the user can self-resolve.
	_, _ = h.deps.DB.ExecContext(c.Request.Context(),
		`DELETE FROM pending_registrations WHERE LOWER(email) = LOWER($1)`, info.Email)

	c.JSON(http.StatusOK, gin.H{
		"sso_registration_required": true,
		"sso_token":                 ssoToken,
		"email":                     info.Email,
		"name":                      info.Name,
	})
}

// handleSSOIssueError maps an IssueAccessAndRefresh error to the correct HTTP
// response and reports whether it wrote one. Every SSO token-mint site shares
// this mapping, so it lives in one place: a terminally-disabled account
// (auth.ErrAccountDisabled, #1623) becomes 403 account_disabled; any other
// error becomes a generic 500 token_issuance_failed with no SQL/internal detail
// echoed to the client. Callers MUST return when it reports true; on false the
// mint succeeded and the caller writes its own (site-specific) success body.
func handleSSOIssueError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, auth.ErrAccountDisabled) {
		c.JSON(http.StatusForbidden, gin.H{"error_code": "account_disabled"})
		return true
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error_code": "token_issuance_failed"})
	return true
}

// respondExistingSSO issues either a full session (access + refresh cookie)
// or an MFA challenge token, depending on trust_sso_security. last_used_at
// is bumped best-effort regardless.
func (h *Handler) respondExistingSSO(c *gin.Context, info *UserInfo) {
	ctx := c.Request.Context()
	var userID string
	var trustSSO bool
	err := h.deps.DB.QueryRowContext(ctx,
		`SELECT u.id::text, u.trust_sso_security
		 FROM users u
		 JOIN user_sso_identities s ON s.user_id = u.id
		 WHERE s.provider = $1 AND s.provider_user_id = $2`,
		info.Provider, info.ProviderUserID,
	).Scan(&userID, &trustSSO)
	if err != nil {
		// Race: the row was present at classify time but vanished by now.
		// Generic error — do not echo SQL detail to the response.
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "user_lookup_failed"})
		return
	}

	// Best-effort: stamp last_used_at for analytics + key-rotation hints.
	_, _ = h.deps.DB.ExecContext(ctx,
		`UPDATE user_sso_identities SET last_used_at = NOW() WHERE provider = $1 AND provider_user_id = $2`,
		info.Provider, info.ProviderUserID)

	if !trustSSO {
		// Issue an MFA challenge and surface the same fields as the password
		// path (/auth/login when mfa_required=true): methods, recovery-only
		// methods, and WebAuthn options when applicable. The renderer's
		// MFAChallengeModal reads these to render the right UI.
		//
		// If the user has trust_sso_security=FALSE but no MFA enrolled at all,
		// the adapter returns mfaEnabled=false. Mint tokens directly in that
		// case — the SSO link itself is the second factor, the trust flag
		// merely signals "MFA when available", and falling through here would
		// deadlock the user with a challenge they cannot complete.
		challenge, methods, recoveryOnly, webauthnOptions, mfaEnabled, err := h.deps.AuthHandler.IssueMFAChallenge(ctx, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error_code": "mfa_challenge_failed"})
			return
		}
		if !mfaEnabled {
			// Same trust posture as the trustSSO branch below — fall through.
			access, refresh, err := h.deps.AuthHandler.IssueAccessAndRefresh(ctx, userID)
			if handleSSOIssueError(c, err) {
				return
			}
			auth.SetRefreshCookie(c, refresh, 60*60*24*30)
			c.JSON(http.StatusOK, gin.H{"access_token": access})
			return
		}
		resp := gin.H{
			"mfa_challenge_token": challenge,
			"methods":             methods,
		}
		if len(recoveryOnly) > 0 {
			resp["recovery_only_methods"] = recoveryOnly
		}
		if webauthnOptions != nil {
			resp["webauthn_options"] = webauthnOptions
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	access, refresh, err := h.deps.AuthHandler.IssueAccessAndRefresh(ctx, userID)
	if handleSSOIssueError(c, err) {
		return
	}
	// 30-day rolling refresh-token cookie matches the project-wide auth
	// invariant in [internal]: HttpOnly + Secure + SameSite=Lax + 30d max-age.
	// Routed through auth.SetRefreshCookie so cookie attributes stay aligned
	// with /auth/login and /auth/refresh — the SSO sign-in path is just
	// another way to mint a session and must not diverge.
	auth.SetRefreshCookie(c, refresh, 60*60*24*30)
	c.JSON(http.StatusOK, gin.H{"access_token": access})
}

// respondAccountLink offers the user a chance to prove ownership of the
// matched account by entering its password. The opaque sso_token carries the
// targeted user_id forward to the Link endpoint.
func (h *Handler) respondAccountLink(c *gin.Context, info *UserInfo) {
	ctx := c.Request.Context()
	var targetUserID, email string
	err := h.deps.DB.QueryRowContext(ctx,
		`SELECT id::text, email FROM users WHERE LOWER(email) = LOWER($1) AND email_verified = TRUE`,
		info.Email,
	).Scan(&targetUserID, &email)
	if errors.Is(err, sql.ErrNoRows) {
		// Genuine race: classifyCallback saw a row but it vanished before
		// respondAccountLink ran (e.g., concurrent account deletion). Falling
		// through to the new-user path is correct — the user can register
		// fresh; we will not silently link an empty target.
		h.respondNewUser(c, info)
		return
	}
	if err != nil {
		// Real DB error (connectivity, permission, etc.) — must NOT silently
		// fall through to the new-user path, which would let a transient
		// outage downgrade an existing-account scenario to a fresh
		// registration with the same email and a stolen password.
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "user_lookup_failed"})
		return
	}

	ssoToken := randomString(32)
	payload, err := json.Marshal(map[string]any{
		"provider":         info.Provider,
		"provider_user_id": info.ProviderUserID,
		"provider_email":   info.Email,
		"target_user_id":   targetUserID,
		"branch":           ssoBranchAccountLink,
		"created_at":       time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		panic(fmt.Errorf("oauth: marshal account-link sso_token payload: %w", err))
	}
	if err := h.deps.Redis.Set(ctx, ssoTokenKeyPrefix+ssoToken, payload, ssoTokenTTL).Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "redis_unavailable"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"account_link_available": true,
		"sso_token":              ssoToken,
		"masked_email":           maskEmail(email),
	})
}

// maskEmail returns a partially-redacted form of the email so the renderer
// can confirm "yes, this is the address you expect" without echoing it back
// in full. Examples:
//
//	"carol@example.test"  → "c***@example.test"
//	"a@example.com"       → "***@example.com"   (1-char local part)
//	"@example.com"        → "***@example.com"   (degenerate; defensive)
//
// The redaction is non-cryptographic; the @ + domain are still revealed
// because the user must recognize their own address.
func maskEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at < 2 {
		// Includes both at == -1 (malformed; just stamp ***) and at < 2
		// (1-char or empty local part — nothing to preview).
		if at < 0 {
			return "***"
		}
		return "***" + email[at:]
	}
	return email[:1] + "***" + email[at:]
}

// completeRegistrationRequest is the JSON body for finalizing a new SSO user.
// All key material is base64-StdEncoding (NOT base64url) for symmetry with the
// existing registration endpoints — see internal/auth.PendingHandler.
//
// Binding tags are the FIRST line of defense (cheap, fail-fast):
//   - Username: 3..50 chars matches users.username VARCHAR(50) and the
//     password-path validator. AuthAdapter.ValidateUsername runs after binding
//     to enforce charset, reserved words, and profanity.
//   - Password: 12..128 chars. The min=12 mirrors the password-path policy;
//     the max=128 closes the Argon2id-DoS surface (linear CPU scaling on
//     input length). AuthAdapter.ValidatePasswordStrength runs after binding
//     to enforce char-class diversity.
type completeRegistrationRequest struct {
	SSOToken          string `json:"sso_token" binding:"required"`
	Username          string `json:"username" binding:"required,min=3,max=50"`
	Password          string `json:"password" binding:"required,min=12,max=128"` // #nosec G117 -- False positive: request field, not stored secret
	WrappedPrivateKey string `json:"wrapped_private_key" binding:"required"`     // base64
	KeyDerivationSalt string `json:"key_derivation_salt" binding:"required"`     // base64
	PublicKey         string `json:"public_key" binding:"required"`              // base64
}

// CompleteRegistration finalizes a new SSO-registered user.
// POST /api/v1/auth/sso/:provider/complete-registration
//
// Flow:
//  1. Bind + validate the request body (passphrase ≥ 12 chars, base64 fields).
//  2. Consume the sso_token from Redis (single-use: GET then DEL).
//  3. Validate the token's branch == "new_user" — rejects tokens issued for
//     the account-link path, even if the attacker harvests one.
//  4. Hash the passphrase with Argon2id (delegated to internal/auth).
//  5. Single transaction: INSERT users → user_keys → public_keys → SSO link.
//     password_login_disabled=TRUE is the SSO default (per spec §1.5); // pragma: allowlist secret
//     trust_sso_security=FALSE forces a one-time MFA challenge on first
//     subsequent SSO login until the user explicitly opts in.
//  6. Issue access + refresh tokens via AuthAdapter and return 201 Created.
//
// Errors are stable error_code strings; underlying SQL/Redis detail does not
// leak to the response body.
func (h *Handler) CompleteRegistration(c *gin.Context) {
	var req completeRegistrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error_code": "invalid_request"})
		return
	}

	// Validate username + password BEFORE consuming the sso_token. A validation
	// failure on a recoverable input (typo'd username, weak password) must not
	// burn the single-use sso_token — the user can fix the input and retry.
	// The 'detail' field surfaces the specific rule violated (e.g., "username
	// contains reserved word") so the renderer can render an actionable error.
	if err := h.deps.AuthHandler.ValidateUsername(req.Username); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error_code": "invalid_username",
			"detail":     err.Error(),
		})
		return
	}
	if err := h.deps.AuthHandler.ValidatePasswordStrength(req.Password); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error_code": "invalid_password",
			"detail":     err.Error(),
		})
		return
	}

	ctx := c.Request.Context()

	// Consume sso_token (single-use defense) + branch gate: a token issued for
	// the account-link path must NOT be usable here — it would let an attacker
	// who steals a link-flow token register a brand-new account in the
	// targeted user's name.
	info, err := h.consumeSSOToken(ctx, req.SSOToken, ssoBranchNewUser)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "sso_token_invalid"})
		return
	}

	// Argon2id hash via the existing internal/auth path. Hashing happens
	// outside the DB transaction (CPU-bound; we want it before BeginTx so
	// the tx window stays short).
	hash, err := h.deps.AuthHandler.HashPassword(ctx, req.Password)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "hash_failed"})
		return
	}

	// Decode key material. base64.StdEncoding (NOT RawURLEncoding) matches
	// the existing pending-registrations path; the test fixture also uses
	// StdEncoding.EncodeToString.
	wrapped, salt, pubKey, err := decodeSSOKeyMaterial(&req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error_code": "invalid_key_material"})
		return
	}

	// Atomic multi-table insert. defer Rollback is a no-op after Commit
	// succeeds; otherwise it unwinds any partial state.
	tx, err := h.deps.DB.BeginTx(ctx, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "db_unavailable"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	userID, errCode, err := h.createSSOUser(ctx, tx, &createSSOUserParams{
		Info:     info,
		Hash:     hash,
		Username: req.Username,
		Wrapped:  wrapped,
		Salt:     salt,
		PubKey:   pubKey,
	})
	if err != nil {
		c.JSON(httpStatusForCreateUserError(errCode), gin.H{"error_code": errCode})
		return
	}

	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "commit_failed"})
		return
	}

	access, refresh, err := h.deps.AuthHandler.IssueAccessAndRefresh(ctx, userID)
	if handleSSOIssueError(c, err) {
		return
	}
	// 30-day rolling refresh-token cookie matches the project-wide auth
	// invariant in [internal]: HttpOnly + Secure + SameSite=Lax + 30d max-age.
	// Routed through auth.SetRefreshCookie so cookie attributes stay aligned
	// with /auth/login and /auth/refresh.
	auth.SetRefreshCookie(c, refresh, 60*60*24*30)
	c.JSON(http.StatusCreated, gin.H{"access_token": access})
}

// httpStatusForCreateUserError maps the stable errCode strings returned by
// createSSOUser to their HTTP status. UNIQUE-violation errors surface as 409
// (renderer can highlight the offending field); everything else is 500.
func httpStatusForCreateUserError(errCode string) int {
	switch errCode {
	case "email_taken", "username_taken":
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// completeLinkRequest is the JSON body for the email-match account-linking
// flow. The user proves ownership of the target Concord account by entering
// its existing password; the SSO identity row is then attached.
//
// Note: unlike completeRegistrationRequest, Password is NOT capped here.
// The user is verifying an EXISTING password against an EXISTING Argon2id
// hash, so strength validation is irrelevant. The DoS surface is upper-
// bounded by the production AuthAdapter, which enforces its own length
// gate before invoking Argon2id. Tracked as a follow-up for binding-tag
// parallelism with the registration path.
type completeLinkRequest struct {
	SSOToken string `json:"sso_token" binding:"required"`
	Password string `json:"password" binding:"required"` // #nosec G117 -- False positive: request field, not stored secret
}

// CompleteLink finalizes the email-match account-linking flow.
// POST /api/v1/auth/sso/:provider/complete-link
//
// Flow:
//  1. Bind + validate the request body.
//  2. Consume the sso_token from Redis (single-use: GET then DEL).
//  3. Validate the token's branch == "account_link" AND TargetUserID is
//     non-empty — rejects tokens issued for the new-user path (symmetric
//     to CompleteRegistration's branch=="new_user" check) and defends
//     against malformed token records that lack a target.
//  4. VerifyPassword via the AuthAdapter, which shares /login's lockout
//     counter — repeated wrong-password attempts on this endpoint feed
//     the SAME counter that gates /login. The adapter returns the
//     auth.ErrAccountLocked sentinel when the threshold is reached
//     (matched via errors.Is); the handler translates that to 423 Locked.
//  5. INSERT user_sso_identities. password_login_disabled is NOT modified
//     — the user already has a password posture; linking does not change
//     it. The user retains password login + gains SSO login as a parallel
//     option, until they explicitly opt out via Settings (Task 12).
//  6. Issue access + refresh tokens via AuthAdapter and return 200.
//
// No DB transaction is needed — only one INSERT happens. The verify+insert+
// token sequence is not atomic, but the worst-case race (same user invoked
// twice concurrently) is bounded by the user_sso_identities UNIQUE
// (provider, provider_user_id) constraint: a second insert fails with
// sso_identity_insert_failed; harmless beyond a duplicate 500 response on
// the loser. Token-issuance duplication is also harmless — refresh cookies
// are valid as a set, not a singleton.
func (h *Handler) CompleteLink(c *gin.Context) {
	var req completeLinkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error_code": "invalid_request"})
		return
	}

	ctx := c.Request.Context()

	// Consume sso_token (single-use defense) + branch + target_user_id gates.
	// consumeSSOToken returns errSSOTokenInvalid for: Redis miss, JSON
	// unmarshal failure, branch != "account_link" (rejects new-user tokens),
	// or empty TargetUserID (defends against malformed records that would let
	// an attacker without a target pivot an SSO link).
	info, err := h.consumeSSOToken(ctx, req.SSOToken, ssoBranchAccountLink)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "sso_token_invalid"})
		return
	}

	// Verify password — shares /login's lockout counter via the AuthAdapter.
	// The production adapter increments the failure counter on
	// invalid_credentials and returns auth.ErrAccountLocked once threshold is
	// reached. We translate to 423 Locked for the locked path; everything
	// else is opaque 401 invalid_credentials.
	if err := h.deps.AuthHandler.VerifyPassword(ctx, info.TargetUserID, req.Password); err != nil {
		if errors.Is(err, auth.ErrAccountLocked) {
			c.JSON(http.StatusLocked, gin.H{"error_code": "account_locked"})
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_credentials"})
		return
	}

	// Create the SSO identity row. Do NOT modify password_login_disabled —
	// the user is opting INTO an additional sign-in option, not opting OUT
	// of password login. Only the explicit Settings toggle (Task 12) flips
	// that posture.
	if _, err := h.deps.DB.ExecContext(ctx,
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email, is_relay_email)
		 VALUES ($1, $2, $3, $4, $5)`,
		info.TargetUserID, info.Provider, info.ProviderUserID, info.ProviderEmail, info.IsRelayEmail,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "sso_identity_insert_failed"})
		return
	}

	access, refresh, err := h.deps.AuthHandler.IssueAccessAndRefresh(ctx, info.TargetUserID)
	if handleSSOIssueError(c, err) {
		return
	}
	// 30-day rolling refresh-token cookie — same invariant as Callback /
	// CompleteRegistration. auth.SetRefreshCookie keeps cookie attributes
	// aligned with /auth/login and /auth/refresh.
	auth.SetRefreshCookie(c, refresh, 60*60*24*30)
	c.JSON(http.StatusOK, gin.H{"access_token": access})
}
