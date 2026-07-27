package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
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

// SSOIdentityLink is the provider identity CompleteLink must persist with its
// epoch-bound session mint.
type SSOIdentityLink struct {
	Provider       string
	ProviderUserID string
	ProviderEmail  string
	IsRelayEmail   bool
}

type ssoMintUserState struct {
	emailVerified   bool
	credentialEpoch sql.NullString
}

// IssueAccessAndRefresh mints an access token and a refresh token for the
// given userID, persisting the refresh-token hash and returning its session ID.
// Used by internal/oauth.Handler when an SSO sign-in succeeds.
//
// Reads users.email_verified so the access-token claim reflects current
// state — SSO users are always email-verified at INSERT time (provider
// asserts email_verified=true; the Callback handler refuses otherwise),
// but we re-read in case the user later flips a flag via a recovery flow.
// #2450: the locked read and the refresh INSERT run in ONE transaction holding
// the users row FOR NO KEY UPDATE — the same lock the four destructive resets
// take (ChangePassword / ReplaceMyKeys / RecoveryResetPassword /
// RecoveryResetAccount). The unbound provider-assertion path relies on that
// lock to order itself with resets. CompleteLink captures the password's
// verified epoch and uses IssueAccessAndRefreshBound to compare it inside this
// same transaction before minting.
//
// Scope boundary: a lock only orders actors that take it. The standalone bulk
// revocation flows in #2457 and #2460 now take this same user-row lock through
// RevokeAllRefreshTokens; destructive flows already holding it retain atomic
// sweeps. Provider-assertion SSO carries no separately-authorized epoch, while
// CompleteLink compares its separately-authorized password epoch under this lock.
//
// Before this the epoch SELECT and the INSERT were two unfenced statements with
// a Redis round trip between them, so a reset could bulk-revoke, commit, and
// still leave this row un-swept and live — rotateAndRespond would then rotate it
// into a fully valid post-reset session derived from pre-reset authorization
// (CWE-367 -> CWE-613). handlers.go's generateTokenPair comment already named
// SSO as a flow that must not mint unfenced; this is that fix. The unlocked
// `disabled` read had the identical shape and is closed by the same change.
func (h *Handler) IssueAccessAndRefresh(ctx context.Context, userID string) (accessToken, refreshToken, sessionID string, err error) {
	return h.issueAccessAndRefresh(ctx, userID, nil, nil)
}

// IssueAccessAndRefreshBound mints a session only when the credential epoch
// remains the one under which the caller authorized the request.
func (h *Handler) IssueAccessAndRefreshBound(ctx context.Context, userID, expectedEpoch string, identity SSOIdentityLink) (accessToken, refreshToken, sessionID string, err error) {
	return h.issueAccessAndRefresh(ctx, userID, &expectedEpoch, &identity)
}

func (h *Handler) issueAccessAndRefresh(ctx context.Context, userID string, expectedEpoch *string, identity *SSOIdentityLink) (accessToken, refreshToken, sessionID string, err error) {
	// Resolve the tier BEFORE opening the transaction. GetTier is a Redis GET
	// that reads through to the subscriptions table on a miss, so calling it
	// under the row lock would acquire a second pool connection while this
	// handler already holds one — a pool-exhaustion hazard under load, and it
	// would extend the hold on a lock destructive resets need. The tier is only
	// a JWT claim; it needs no consistent snapshot with the locked read.
	tier := h.entCache.GetTier(ctx, userID)

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", "", fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback: %w", rollbackErr))
		}
	}()

	userState, err := h.lockSSOMintUser(ctx, tx, userID, expectedEpoch)
	if err != nil {
		return "", "", "", err
	}
	if identity != nil {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email, is_relay_email)
			 VALUES ($1, $2, $3, $4, $5)`,
			userID, identity.Provider, identity.ProviderUserID, identity.ProviderEmail, identity.IsRelayEmail,
		); err != nil {
			return "", "", "", fmt.Errorf("%w: %w", ErrSSOIdentityInsert, err)
		}
	}

	// tokenID first so the access token carries it as the sid claim (#2201).
	tokenID := uuid.New().String()
	accessToken, err = GenerateAccessToken(userID, h.jwtSecret, userState.emailVerified, userState.credentialEpoch.String, tokenID, tier)
	if err != nil {
		return "", "", "", fmt.Errorf("access: %w", err)
	}
	refreshToken, err = GenerateRefreshToken()
	if err != nil {
		return "", "", "", fmt.Errorf("refresh: %w", err)
	}

	tokenHash := HashRefreshToken(refreshToken)
	expiresAt := time.Now().Add(30 * 24 * time.Hour)

	// device_name / ip_address / user_agent / machine_id deliberately omitted —
	// the adapter signature is ctx-only. SSO-issued sessions accept the missing
	// metadata; subsequent /auth/refresh + WS ticket flows will stamp it.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, remember_me)
		 VALUES ($1, $2, $3, $4, $5)`,
		tokenID, userID, tokenHash, expiresAt, true,
	); err != nil {
		return "", "", "", fmt.Errorf("store refresh token: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", "", fmt.Errorf("commit: %w", err)
	}

	return accessToken, refreshToken, tokenID, nil
}

// lockSSOMintUser loads the user state used for an SSO session while holding
// the same row lock as destructive credential-reset flows.
func (h *Handler) lockSSOMintUser(ctx context.Context, tx *sql.Tx, userID string, expectedEpoch *string) (ssoMintUserState, error) {
	var state ssoMintUserState
	var disabled bool
	if err := tx.QueryRowContext(ctx,
		`SELECT email_verified, disabled, credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID,
	).Scan(&state.emailVerified, &disabled, &state.credentialEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ssoMintUserState{}, fmt.Errorf("user %s not found", userID)
		}
		return ssoMintUserState{}, fmt.Errorf("lookup user state: %w", err)
	}
	if disabled {
		return ssoMintUserState{}, ErrAccountDisabled
	}
	if expectedEpoch == nil {
		return state, nil
	}
	if err := credepoch.MatchEpoch(state.credentialEpoch, *expectedEpoch); err != nil {
		return ssoMintUserState{}, fmt.Errorf("credential epoch: %w", err)
	}
	return state, nil
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

	// #2418: stamp the epoch the SSO sign-in was authorized under, so CompleteLogin
	// can refuse a completion that races a destructive reset. Fails closed — never
	// substitute "" here; CompleteLogin rejects a claimless challenge for a rotated
	// user, which would surface as a 401 at the end of the MFA flow instead of here.
	ssoEpoch, epochErr := h.readCredentialEpoch(ctx, userID)
	if epochErr != nil {
		return "", nil, nil, nil, false, fmt.Errorf("read credential epoch for SSO MFA challenge: %w", epochErr)
	}

	token, jti, err := h.mfaChecker.GenerateLoginChallenge(ctx, userID, true, ssoEpoch)
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
func (h *Handler) VerifyPassword(ctx context.Context, userID, password string) (string, error) {
	var email, passwordHash string
	var credentialEpoch sql.NullString
	if err := h.db.QueryRowContext(ctx,
		`SELECT email, password_hash, credential_epoch FROM users WHERE id = $1`, userID,
	).Scan(&email, &passwordHash, &credentialEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errors.New("invalid_credentials")
		}
		return "", fmt.Errorf("lookup user: %w", err)
	}

	// Pre-flight lockout check — a locked account must short-circuit before
	// the (relatively expensive) Argon2id hash verification.
	if h.checkLoginLockout(ctx, email) {
		return "", ErrAccountLocked
	}

	valid, err := VerifyPassword(password, passwordHash)
	if err != nil {
		return "", fmt.Errorf("verify password: %w", err)
	}
	if !valid {
		// Increment counter and possibly trigger lockout. recordFailedLogin
		// applies the lockout AFTER threshold; the next request observes it.
		// We deliberately do NOT also check checkLoginLockout post-record —
		// the contract is that the caller sees ErrAccountLocked on the
		// FOLLOWING request, matching /auth/login's UX where the threshold-
		// reaching request still gets 401, not 423.
		h.recordFailedLogin(ctx, email)
		return "", errors.New("invalid_credentials")
	}

	// Successful verify clears the counter — same posture as /auth/login.
	h.clearLoginAttempts(ctx, email)
	return credentialEpoch.String, nil
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
