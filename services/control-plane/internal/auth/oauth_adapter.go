package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// AuthAdapter wrappers on *Handler — production binding for the
// internal/oauth.AuthAdapter interface.
//
// Why a separate file: keeps the SSO-only entry points (which exist purely to
// satisfy a downstream interface) visually distinct from the primary
// /api/v1/auth/* handlers in handlers.go. The adapter methods deliberately
// take a context.Context (not *gin.Context) — they're called from
// internal/oauth.Handler, which doesn't expose its Gin context to keep
// internal/auth from being the wrong place to import gin types.
//
// One consequence: the SSO refresh-token row is stored without device_name,
// IP, or user-agent. Those columns are nullable; the missing metadata is the
// trade-off for the cleaner adapter signature. WS ticket flow + /auth/refresh
// stamp them on first use, so SSO-issued sessions get full device metadata
// once the renderer connects WebSocket.

// IssueAccessAndRefresh mints an access token and a refresh token for the
// given userID, persisting the refresh-token hash in refresh_tokens. Used by
// internal/oauth.Handler when an SSO sign-in succeeds.
//
// Reads users.email_verified so the access-token claim reflects current
// state — SSO users are always email-verified at INSERT time (provider
// asserts email_verified=true; the Callback handler refuses otherwise),
// but we re-read in case the user later flips a flag via a recovery flow.
func (h *Handler) IssueAccessAndRefresh(ctx context.Context, userID string) (accessToken, refreshToken string, err error) {
	var emailVerified, disabled bool
	if err := h.db.QueryRowContext(ctx,
		`SELECT email_verified, disabled FROM users WHERE id = $1`, userID,
	).Scan(&emailVerified, &disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", fmt.Errorf("user %s not found", userID)
		}
		return "", "", fmt.Errorf("lookup user state: %w", err)
	}
	// Gate the SSO mint path on the terminal disable (#1623), so a disabled user
	// cannot establish a session via SSO — symmetric with the password Login /
	// CompleteLogin gate and the refresh gate. (AuthRequired's denylist also
	// backstops any access derived from a token, but we must not mint one.)
	if disabled {
		return "", "", ErrAccountDisabled
	}

	tier := h.entCache.GetTier(ctx, userID)
	accessToken, err = GenerateAccessToken(userID, h.jwtSecret, emailVerified, tier)
	if err != nil {
		return "", "", fmt.Errorf("access: %w", err)
	}
	refreshToken, err = GenerateRefreshToken()
	if err != nil {
		return "", "", fmt.Errorf("refresh: %w", err)
	}

	tokenHash := HashRefreshToken(refreshToken)
	tokenID := uuid.New().String()
	expiresAt := time.Now().Add(30 * 24 * time.Hour)

	// device_name / ip_address / user_agent / machine_id deliberately omitted —
	// the adapter signature is ctx-only. SSO-issued sessions accept the missing
	// metadata; subsequent /auth/refresh + WS ticket flows will stamp it.
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, remember_me)
		 VALUES ($1, $2, $3, $4, $5)`,
		tokenID, userID, tokenHash, expiresAt, true,
	); err != nil {
		return "", "", fmt.Errorf("store refresh token: %w", err)
	}

	return accessToken, refreshToken, nil
}

// IssueMFAChallenge mints a short-lived MFA challenge token for users with
// MFA enabled or with trust_sso_security=false. Used by
// internal/oauth.Handler.respondExistingSSO when a returning SSO user has not
// opted into "trust SSO" — they must verify a second factor before tokens
// are issued.
//
// remember_me defaults to true here — the SSO flow has no separate
// remember-me toggle (the existence of the SSO link itself implies a
// long-lived trust posture).
//
// Returns mfaEnabled=false (with all other return values zero-valued) when the
// user has trust_sso_security=FALSE but no MFA factors enrolled. The caller
// (respondExistingSSO) treats this as "fall through to direct token issuance"
// rather than "issue an unverifiable challenge token". This matches the
// password path's IsEnabled gate in handlers.go before handleMFAChallenge.
func (h *Handler) IssueMFAChallenge(ctx context.Context, userID string) (
	challengeToken string,
	loginMethods []string,
	recoveryOnlyMethods []string,
	webauthnOptions interface{},
	mfaEnabled bool,
	err error,
) {
	if h.mfaChecker == nil {
		return "", nil, nil, nil, false, errors.New("mfa checker not wired")
	}
	// Pre-flight IsEnabled gate — the password path checks this before issuing
	// a challenge. Without the gate, a user with trust_sso_security=FALSE but
	// no MFA enrolled would be served a challenge token they can never
	// complete (no methods to verify against), deadlocking SSO sign-in.
	if !h.mfaChecker.IsEnabled(ctx, userID) {
		return "", nil, nil, nil, false, nil
	}

	allMethods, err := h.mfaChecker.GetEnabledMethods(ctx, userID)
	if err != nil {
		return "", nil, nil, nil, false, fmt.Errorf("get enabled methods: %w", err)
	}
	loginMethods, err = h.mfaChecker.GetLoginMethods(ctx, userID)
	if err != nil {
		return "", nil, nil, nil, false, fmt.Errorf("get login methods: %w", err)
	}
	recoveryOnlyMethods = computeRecoveryOnlyMethods(allMethods, loginMethods)

	token, jti, err := h.mfaChecker.GenerateLoginChallenge(ctx, userID, true)
	if err != nil {
		return "", nil, nil, nil, false, fmt.Errorf("generate login challenge: %w", err)
	}

	// WebAuthn options when applicable — same posture as addWebAuthnOptions in
	// handlers.go. We log and continue on BeginWebAuthnLogin errors; the
	// renderer falls back to a non-WebAuthn method.
	for _, m := range loginMethods {
		if m == "webauthn" {
			opts, beginErr := h.mfaChecker.BeginWebAuthnLogin(ctx, userID, jti)
			if beginErr != nil {
				h.log.Error("Failed to begin WebAuthn login on SSO MFA challenge", "error", beginErr)
			} else if opts != nil {
				webauthnOptions = opts
			}
			break
		}
	}

	return token, loginMethods, recoveryOnlyMethods, webauthnOptions, true, nil
}

// VerifyPassword is the production binding for oauth.AuthAdapter.VerifyPassword.
// It is used by internal/oauth.Handler.CompleteLink to confirm the user owns
// the existing Concord account they're attaching an SSO identity to.
//
// Sharing /login's lockout counter is a security requirement — a brute-force
// attacker who fails CompleteLink five times must be rate-limited the same
// way they would be at /auth/login (and a /auth/login attacker who fails
// five times must be locked out of CompleteLink). The shared counter is
// keyed by email, so we look up email-by-userID before consulting it.
//
// Returns ErrAccountLocked when the lockout threshold is reached, which
// CompleteLink translates to HTTP 423 Locked.
func (h *Handler) VerifyPassword(ctx context.Context, userID, password string) error {
	var email, passwordHash string
	if err := h.db.QueryRowContext(ctx,
		`SELECT email, password_hash FROM users WHERE id = $1`, userID,
	).Scan(&email, &passwordHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("invalid_credentials")
		}
		return fmt.Errorf("lookup user: %w", err)
	}

	// Pre-flight lockout check — a locked account must short-circuit before
	// the (relatively expensive) Argon2id hash verification.
	if h.checkLoginLockout(ctx, email) {
		return ErrAccountLocked
	}

	valid, err := VerifyPassword(password, passwordHash)
	if err != nil {
		return fmt.Errorf("verify password: %w", err)
	}
	if !valid {
		// Increment counter and possibly trigger lockout. recordFailedLogin
		// applies the lockout AFTER threshold; the next request observes it.
		// We deliberately do NOT also check checkLoginLockout post-record —
		// the contract is that the caller sees ErrAccountLocked on the
		// FOLLOWING request, matching /auth/login's UX where the threshold-
		// reaching request still gets 401, not 423.
		h.recordFailedLogin(ctx, email)
		return errors.New("invalid_credentials")
	}

	// Successful verify clears the counter — same posture as /auth/login.
	h.clearLoginAttempts(ctx, email)
	return nil
}

// HashPassword adapts the package-level HashPassword to the
// oauth.AuthAdapter signature. Argon2id is a CPU-bound operation with no
// I/O, so the ctx is unused.
func (h *Handler) HashPassword(_ context.Context, password string) (string, error) {
	return HashPassword(password)
}

// ValidateUsername delegates to the package-level ValidateUsername — the
// charset / reserved-word / profanity gate that's shared with the password-
// path registration. Exposed on *Handler so internal/oauth can call through
// the AuthAdapter without importing internal/auth's free functions.
func (h *Handler) ValidateUsername(username string) error {
	return ValidateUsername(username)
}

// NormalizeUsername delegates to the package-level NormalizeUsername (lowercase
// fold) so the SSO registration path stores usernames identically to the
// password path (#1931). Exposed on *Handler for the same import-cycle-avoidance
// reason as ValidateUsername.
func (h *Handler) NormalizeUsername(username string) string {
	return NormalizeUsername(username)
}

// ValidatePasswordStrength delegates to the package-level
// ValidatePasswordStrength — length bounds (≥12, ≤128) and char-class
// diversity (≥3 of upper/lower/digit/special). Same delegation rationale as
// ValidateUsername.
func (h *Handler) ValidatePasswordStrength(password string) error {
	return ValidatePasswordStrength(password)
}
