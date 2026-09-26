package mfa

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/email"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// Duplicated string literals extracted for SonarQube S1192 compliance.
const (
	// Redis key patterns. A login challenge's remember-me key is
	// auth.MFAChallengeRememberMeKey, since auth discards it too.
	redisKeyMFAChallengeUsed     = "mfa_challenge_used:%s"
	redisKeyMFAVerifyAttempts    = "mfa_verify_attempts:%s"
	redisKeyMFAVerifyLockout     = "mfa_verify_lockout:%s"
	redisKeyEmailSmsEnabled      = "mfa_emailsms_enabled:%s:%s"
	redisKeyEmailSmsEnabledEmail = "mfa_emailsms_enabled:%s:email"
	redisKeyEmailSmsSetup        = "mfa_emailsms_setup:%s:%s"
	redisKeyWebAuthnReg          = "webauthn_reg:%s"
	redisKeyTOTPSetupSession     = "mfa_totp_setup_session:%s"

	// mfaUpgradeBypassTTL bounds a one-refresh exemption from the pre-MFA
	// session lock (auth.checkPreMFASessionLock): long enough for the refresh
	// that follows, never a standing exemption.
	mfaUpgradeBypassTTL = 30 * time.Second
	// totpSetupSessionTTL bounds how long confirm-setup can match the session
	// that proved the code at verify-setup.
	totpSetupSessionTTL = 15 * time.Minute
	// failedAttemptLimit is how many wrong codes lock further attempts out, in
	// login verification and TOTP setup verification alike.
	failedAttemptLimit = 5
	// verifyAttemptWindow is how long an attempt count lives: fixed from the
	// first attempt at login (reserveVerifyAttempt), renewed by each wrong code
	// at TOTP setup (recordCodeFailure). verifyLockoutDuration is how long the
	// lockout lasts.
	verifyAttemptWindow   = 5 * time.Minute
	verifyLockoutDuration = 15 * time.Minute
	// emailCodeTTL is how long a login email code, and the one-send-per-challenge
	// marker that guards it, stay valid.
	emailCodeTTL = 10 * time.Minute
	// cleanupTimeout bounds the undo of a partial write, which runs detached
	// from the request.
	cleanupTimeout = 5 * time.Second

	// Error messages
	errMsgPasswordRequired           = "Password is required"
	errMsgIncorrectPassword          = "Incorrect password"
	errMsgCodeRequired               = "Code is required"
	errMsgInvalidTOTPCode            = "Invalid TOTP code" // wrong, replayed and undecryptable codes all answer this
	errMsgFailedBackupCodes          = "Failed to generate backup codes"
	errMsgFailedStartReg             = "Failed to start registration"
	errMsgFailedActivateMFA          = "Failed to activate MFA"
	errMsgFailedDisableMFA           = "Failed to disable MFA"
	errMsgFailedVerifyLoginFactor    = "Failed to verify password"
	errMsgFailedStartVerification    = "Failed to start verification"
	errMsgFailedListKeys             = "Failed to list credentials"
	errMsgFailedDeleteCredential     = "Failed to delete credential"
	errMsgFailedSendCode             = "Failed to send code"
	errMsgFailedVerify               = "Failed to verify"
	errMsgFailedLoadShares           = "Failed to load recovery shares"
	errMsgEmailNotConfigured         = "Email delivery is not configured"
	errMsgFailedRemoveDevice         = "Failed to remove trusted device"
	errMsgFailedDeleteCircle         = "Failed to delete recovery circle"
	errMsgFailedStoreTOTPSeed        = "Failed to store MFA secret"
	errMsgFailedRespondRecovery      = "Failed to respond to recovery request"
	errMsgInvalidSessionData         = "Invalid session data"
	errMsgFailedListDevices          = "Failed to list trusted devices"
	errMsgFailedListRecoveryReqs     = "Failed to list recovery requests"
	errMsgFailedLoadCircle           = "Failed to load recovery circle"
	errMsgFailedConfigCircle         = "Failed to configure recovery circle"
	errMsgFailedListSocialReqs       = "Failed to list social recovery requests"
	errMsgFailedSubmitResponse       = "Failed to submit response"
	errMsgMFAVerificationUnavailable = "MFA verification unavailable"
	errMsgTooManyAttempts            = "Too many failed attempts. Try again later."
	errMsgKeyNotFound                = "Credential not found"
	errMsgFailedVerifyCode           = "Failed to verify code"
	errMsgInvalidRequest             = "Invalid request"
	errMsgFailedLoadMFAStatus        = "Failed to load MFA status"
	errMsgFailedUpdateBackupEmail    = "Failed to update backup email"
	errMsgFailedStoreRecoveryKey     = "Failed to store recovery key"
	msgRecoveryKeyStored             = "Recovery key stored"
	errMsgFailedDisableEmailSms      = "Failed to disable Email/SMS MFA methods"

	// errMsgInlineFactorRequired is the D1 refusal: removing the last inline
	// factor while email or SMS is on would leave an account whose login
	// demands an email/SMS code but whose step-up (policy P1) has no MFA leg.
	errMsgInlineFactorRequired = "Turn off email and text-message codes before removing your last authenticator app or security key."
)

// LoginCompleter completes the login flow after MFA verification.
// Implemented by the auth handler to issue tokens and create sessions.
// expectedEpoch is the credential epoch stamped into the challenge at issuance
// (#2418); the implementation refuses to mint if the durable epoch advanced past it.
type LoginCompleter interface {
	// CompleteLogin reports true only after its authoritative session-mint
	// transaction has committed. MFA must not record challenge verification
	// before that point.
	CompleteLogin(c *gin.Context, userID string, rememberMe bool, expectedEpoch string, primaryAuthMethod securityevent.AuthMethod) bool
}

// PermissionInvalidator makes a user's cached permissions miss on their next
// read. Whether a user holds an inline factor (policy P1) decides what the
// #3453 mask removes from their permissions on every enforcing server, so each
// committed factor write must call it. Declared here, at the consumer (the
// rbac.PresenceRecheck / VoiceEnforcer precedent), so mfa does not import rbac;
// *rbac.Resolver satisfies it.
type PermissionInvalidator interface {
	BumpUserPermissionGeneration(ctx context.Context, userID string) error
}

// Handler implements MFA API endpoints and the Verifier interface.
type Handler struct {
	db             *sql.DB
	redis          *redis.Client
	log            *logger.Logger
	keyring        *Keyring // versioned AES keys for TOTP secret encryption (#2307)
	jwtSecret      string
	webauthn       *WebAuthnService
	loginCompleter LoginCompleter
	emailSvc       *email.Service
	environment    string // "development", "staging", "production"
	securityEvents securityevent.Emitter
	permissions    PermissionInvalidator
}

// Ensure Handler implements Verifier at compile time.
var _ Verifier = (*Handler)(nil)

// NewHandler creates a new MFA handler.
func NewHandler(db *sql.DB, redisClient *redis.Client, log *logger.Logger, keyring *Keyring, jwtSecret string, webauthnSvc *WebAuthnService, environment string) *Handler {
	return &Handler{
		db:             db,
		redis:          redisClient,
		log:            log,
		keyring:        keyring,
		jwtSecret:      jwtSecret,
		webauthn:       webauthnSvc,
		environment:    environment,
		securityEvents: securityevent.Discard,
	}
}

// SetSecurityEvents injects bounded Nightwatch telemetry without changing the
// public constructor used by existing callers.
func (h *Handler) SetSecurityEvents(events securityevent.Emitter) {
	if events == nil {
		events = securityevent.Discard
	}
	h.securityEvents = events
}

func (h *Handler) emitSecurityEvent(ctx context.Context, event securityevent.Event) {
	if h.securityEvents != nil {
		h.securityEvents.Emit(ctx, event)
	}
}

func (h *Handler) emitHTTPEvent(c *gin.Context, event securityevent.Event) {
	h.emitSecurityEvent(c.Request.Context(), event)
	middleware.MarkNightwatchHandled(c)
}

// SetLoginCompleter sets the login completer (called after both handlers are initialized).
func (h *Handler) SetLoginCompleter(lc LoginCompleter) {
	h.loginCompleter = lc
}

// SetEmailService sets the email service for email-based MFA delivery.
func (h *Handler) SetEmailService(svc *email.Service) {
	h.emailSvc = svc
}

// SetPermissionInvalidator injects the permission-cache invalidator the factor
// writes call after they commit. The router wires it at construction, and
// requirePermissionInvalidatorWired refuses to boot without it: unwired, a
// factor change would leave cached permissions stale until the cache TTL.
func (h *Handler) SetPermissionInvalidator(p PermissionInvalidator) {
	h.permissions = p
}

// HasPermissionInvalidator reports whether SetPermissionInvalidator ran with a
// non-nil value. It is what the boot guard asks, because a nil check on the
// resolver the router holds could not see a deleted setter call.
func (h *Handler) HasPermissionInvalidator() bool {
	return h.permissions != nil
}

// invalidatePermissionState bumps userID's permission generation after a
// committed change to their inline factors (spec §6). Call it only after the
// commit: called earlier, a concurrent read could recompute from the factor
// state the commit is about to replace and cache it under the new generation.
//
// It runs detached from the request's cancellation (context.WithoutCancel keeps
// the values), since a client that hangs up after the commit must not leave the
// cache serving what the commit replaced. A failure is logged and dropped: the
// change has committed, and the resolver has already retried and fallen back to
// a scan-invalidate before reporting one. The line is identical for a gained
// and a lost factor and names no method (I7, observability principle 7), which
// the signature guarantees by carrying neither.
func (h *Handler) invalidatePermissionState(ctx context.Context, userID string) {
	if h.permissions == nil {
		return
	}
	if err := h.permissions.BumpUserPermissionGeneration(context.WithoutCancel(ctx), userID); err != nil {
		h.log.Error("Failed to invalidate cached permissions after an MFA factor change",
			"failure_class", "perm_generation_bump", "error", err)
	}
}

// ── Verifier Interface Implementation ────────────────────────────────────────

type codeVerificationStore interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

// IsEnabled reports whether a step-up must demand an MFA code from this user:
// true iff the account holds an inline-verifiable factor (policy P1 —
// confirmed TOTP or any WebAuthn credential), read from the factor tables
// rather than users.mfa_enabled, which also counts email/SMS and can be stale.
//
// It fails CLOSED. The signature has no error, and every caller reads false
// as "skip the MFA leg", so a failed read answers true: the caller then asks
// for a code, which a user without a factor cannot supply until the database
// answers again. Reading the failure as false is exactly the fail-open this
// replaced (it read `err == nil && enabled`).
func (h *Handler) IsEnabled(ctx context.Context, userID string) bool {
	methods, err := stepup.InlineMFAMethods(ctx, h.db, userID)
	if err != nil {
		h.log.Error("MFA status unreadable; requiring the MFA leg", "error", err)
		return true
	}
	return len(methods) > 0
}

// errInvalidStepUpPurpose refuses a step-up verification whose caller named
// no known consumer purpose. It is a wiring fault, so it is a 5xx, never a
// silent downgrade to "TOTP and backup codes only".
var errInvalidStepUpPurpose = errors.New("MFA verification requires a known step-up purpose")

// noInlinePurpose is the purpose the login MFA challenge verifies with. It is
// deliberately not a valid stepup.Purpose, so no inline token is ever minted
// for it and consumeWebAuthnInlineToken never reads one: the login modal
// answers WebAuthn with an assertion, and an inline token reaching login can
// only be a proof minted for some other action.
const noInlinePurpose stepup.Purpose = ""

// VerifyCode checks a TOTP code, a backup code, or a WebAuthn inline token
// minted for purpose against the user's stored MFA state. purpose is the
// calling route's own stepup.Purpose; a token minted for any other purpose is
// refused like an invalid code and left unconsumed.
func (h *Handler) VerifyCode(ctx context.Context, userID string, purpose stepup.Purpose, code string) (bool, error) {
	if !purpose.Valid() {
		return false, errInvalidStepUpPurpose
	}
	return h.verifyCode(ctx, h.db, userID, purpose, code)
}

// VerifyCodeTx performs the same verification on the caller's transaction
// connection. Sensitive rotations use this after locking the users row so they
// neither re-authorize against superseded MFA state nor acquire a second pooled
// database connection while holding the first.
func (h *Handler) VerifyCodeTx(ctx context.Context, tx *sql.Tx, userID string, purpose stepup.Purpose, code string) (bool, error) {
	if tx == nil {
		return false, fmt.Errorf("MFA verification transaction is required")
	}
	if !purpose.Valid() {
		return false, errInvalidStepUpPurpose
	}
	return h.verifyCode(ctx, tx, userID, purpose, code)
}

func (h *Handler) verifyCode(ctx context.Context, store codeVerificationStore, userID string, purpose stepup.Purpose, code string) (bool, error) {
	verified, _, err := h.verifyCodeMatchedMethod(ctx, store, userID, purpose, code)
	return verified, err
}

// verifyCodeMatchedMethod preserves VerifyCode's public boolean contract while
// retaining the server-observed factor for success telemetry. A submitted
// method is only an attempted method: a backup-code submission can validate a
// TOTP value (and vice versa), so it must not choose the success event label.
func (h *Handler) verifyCodeMatchedMethod(ctx context.Context, store codeVerificationStore, userID string, purpose stepup.Purpose, code string) (bool, string, error) {
	// A WebAuthn inline token (from WebAuthnVerifyInlineFinish) counts only for
	// the purpose it was minted for; noInlinePurpose reads none.
	inlineVerified, err := h.consumeWebAuthnInlineToken(ctx, userID, purpose, code)
	if err != nil {
		return false, "", err
	}
	if inlineVerified {
		return true, "webauthn", nil
	}

	// Try TOTP
	var secretEnc, secretNonce []byte
	var keyVersion int
	var totpEnabled, totpConfirmed bool
	err = store.QueryRowContext(ctx,
		`SELECT totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(&secretEnc, &secretNonce, &keyVersion, &totpEnabled, &totpConfirmed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", fmt.Errorf("read TOTP MFA state: %w", err)
	}

	if err == nil && totpEnabled && totpConfirmed {
		secret, decErr := h.keyring.Open(secretEnc, secretNonce, keyVersion)
		if decErr != nil {
			h.log.Error("TOTP secret decryption failed",
				"user_id", userID, "sealed_version", keyVersion, "active_version", h.keyring.ActiveVersion(), "error", decErr)
			return false, "", fmt.Errorf("TOTP secret decryption failed: %w", decErr)
		}
		if step, matched := MatchCodeStep(string(secret), code); matched {
			return acceptTOTPStep(ctx, store, userID, step)
		}

		backupVerified, backupErr := consumeBackupCode(ctx, store, userID, code)
		if backupVerified {
			return true, "backup_code", nil
		}
		return false, "", backupErr
	}

	return false, "", nil
}

// inlineTokenKey is where WebAuthnVerifyInlineFinish stores a token minted for
// purpose, and the only key a consumer of that purpose claims.
//
// Both inline keys use the mfa_inline_purpose_ prefix, which a binary from
// before purpose binding never builds. That binary claims
// mfa_inline_token:<uid>:<code> and finishes mfa_inline_session:<uid>. If the
// new keys shared those prefixes, then during a rolling deploy an old replica
// would spend a purpose-bound token sent as mfa_code "<purpose>:<token>" on
// any route, and would finish a new ceremony into an unbound token. Disjoint
// prefixes leave each binary able to reach only its own keys.
func inlineTokenKey(userID string, purpose stepup.Purpose, token string) string {
	return fmt.Sprintf("mfa_inline_purpose_token:%s:%s:%s", userID, purpose, token)
}

// inlineSessionKey holds the one pending inline ceremony for userID: the
// WebAuthn session data and the purpose begin was given. A second begin
// replaces it, challenge and purpose together.
func inlineSessionKey(userID string) string {
	return fmt.Sprintf("mfa_inline_purpose_session:%s", userID)
}

// errInlineTokenStoreUnavailable replaces a Redis error from the inline-token
// consume. The raw error is deliberately not wrapped: the key embeds the live
// token, and a client hook may annotate a go-redis error with the command's
// arguments, so wrapping it would put a spendable token into the log line of
// every caller that logs a verification dependency failure. The finish path
// withholds its SET error for the same reason. The consume still fails closed.
var errInlineTokenStoreUnavailable = errors.New("consume WebAuthn inline verification token: token store unavailable")

// consumeWebAuthnInlineToken claims a token minted for purpose. It GETDELs only
// that purpose's key, so a token minted for another purpose is an absent key:
// refused exactly like an invalid code, and not consumed. Nothing here logs or
// counts which of the two a refusal was (observability.md principle 7). An
// invalid purpose (noInlinePurpose) reads nothing.
func (h *Handler) consumeWebAuthnInlineToken(ctx context.Context, userID string, purpose stepup.Purpose, code string) (bool, error) {
	if !purpose.Valid() || len(code) <= 20 {
		return false, nil
	}
	token, err := h.redis.GetDel(ctx, inlineTokenKey(userID, purpose, code)).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, errInlineTokenStoreUnavailable
	}
	return token != "", nil
}

// acceptTOTPStep finishes verifyCodeMatchedMethod for a TOTP code that matched
// step: it records step as userID's last accepted step, and accepts the code
// only if that write happened. The guard admits only a step strictly later
// than the last one accepted, so a replayed code updates no row, and so does
// the loser of two concurrent submissions of one code: under READ COMMITTED it
// waits on the winner's row lock and re-evaluates the guard against the
// committed step. A refused advance answers exactly as a wrong code, and the
// code is not then tried as a backup code.
//
// It runs on the store the caller verified on, so inside a transaction a
// rollback un-burns the step with everything else, as it does a backup code.
// On the pool the advance commits at once, so an action that fails after
// verification has still spent the step.
func acceptTOTPStep(ctx context.Context, store codeVerificationStore, userID string, step int64) (bool, string, error) {
	result, err := store.ExecContext(ctx,
		`UPDATE user_mfa_totp SET last_used_step = $2, updated_at = NOW() WHERE user_id = $1 AND (last_used_step IS NULL OR last_used_step < $2)`,
		userID, step,
	)
	if err != nil {
		return false, "", fmt.Errorf("record TOTP step: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, "", fmt.Errorf("read TOTP step result: %w", err)
	}
	if rows != 1 {
		return false, "", nil
	}
	return true, "totp", nil
}

func consumeBackupCode(ctx context.Context, store codeVerificationStore, userID string, code string) (bool, error) {
	var hashes []string
	var used []bool
	if err := store.QueryRowContext(ctx,
		`SELECT backup_codes_hash, backup_codes_used FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(pq.Array(&hashes), pq.Array(&used)); err != nil {
		return false, fmt.Errorf("read backup codes: %w", err)
	}

	idx, matched := VerifyBackupCode(code, hashes, used)
	if !matched {
		return false, nil
	}

	updatedUsed := append([]bool(nil), used...)
	updatedUsed[idx] = true
	result, err := store.ExecContext(ctx,
		`UPDATE user_mfa_totp SET backup_codes_used = $1, updated_at = NOW() WHERE user_id = $2 AND backup_codes_used = $3`,
		pq.Array(updatedUsed), userID, pq.Array(used),
	)
	if err != nil {
		return false, fmt.Errorf("consume backup code: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read backup-code consumption result: %w", err)
	}
	return rows == 1, nil
}

// GetEnabledMethods returns the list of active MFA methods for a user.
func (h *Handler) GetEnabledMethods(ctx context.Context, userID string) ([]string, error) {
	var methods []string
	err := h.db.QueryRowContext(ctx, `SELECT mfa_methods FROM users WHERE id = $1`, userID).Scan(pq.Array(&methods))
	if err != nil {
		return nil, err
	}
	return methods, nil
}

// GetLoginMethods returns methods eligible for login and sensitive ops — excludes recovery-only methods.
// Recovery-only methods are like a spare key: they can unlock the door (account recovery) but can't start the engine (login).
func (h *Handler) GetLoginMethods(ctx context.Context, userID string) ([]string, error) {
	var methods, recoveryOnly []string
	err := h.db.QueryRowContext(ctx,
		`SELECT mfa_methods, recovery_only_methods FROM users WHERE id = $1`, userID,
	).Scan(pq.Array(&methods), pq.Array(&recoveryOnly))
	if err != nil {
		return nil, err
	}

	recoveryOnly = effectiveRecoveryOnly(methods, recoveryOnly)
	if len(recoveryOnly) == 0 {
		return methods, nil
	}

	// Filter out recovery-only methods
	excluded := make(map[string]bool, len(recoveryOnly))
	for _, m := range recoveryOnly {
		excluded[m] = true
	}
	var loginMethods []string
	for _, m := range methods {
		if !excluded[m] {
			loginMethods = append(loginMethods, m)
		}
	}
	return loginMethods, nil
}

// GenerateLoginChallenge creates a challenge token for two-step login and stores
// the remember_me preference in Redis keyed by JTI for retrieval after MFA verify.
// credEpoch is stamped into the challenge and re-checked at CompleteLogin (#2418),
// so a challenge issued before a destructive reset cannot complete after it.
func (h *Handler) GenerateLoginChallenge(ctx context.Context, userID string, rememberMe bool, credEpoch string, primaryAuthMethod securityevent.AuthMethod) (string, string, error) {
	primaryAuthMethod, err := normalizePrimaryAuthMethod(primaryAuthMethod)
	if err != nil {
		return "", "", err
	}
	token, jti, err := generateChallengeTokenWithTTL(userID, PurposeLogin, JWTSecret(h.jwtSecret), CredEpoch(credEpoch), primaryAuthMethod, "", challengeTTL)
	if err != nil {
		return "", "", err
	}

	// Store remember_me in Redis so the MFA verify handler can complete login with it
	rememberVal := "0"
	if rememberMe {
		rememberVal = "1"
	}
	key := auth.MFAChallengeRememberMeKey(jti)
	if err := h.redis.Set(ctx, key, rememberVal, challengeTTL).Err(); err != nil {
		return "", "", fmt.Errorf("store MFA challenge remember state: %w", err)
	}

	return token, jti, nil
}

// GenerateUpgradeChallenge creates a challenge token for pre-MFA session upgrades.
// On successful MFA verification, fresh tokens are issued (same as login).
func (h *Handler) GenerateUpgradeChallenge(_ context.Context, userID, refreshSessionID string) (string, string, error) {
	// PurposeMFAUpgrade completes into a 30s Redis bypass key (completeVerifyPurpose),
	// never a session mint — the subsequent refresh mints via rotateAndRespond, which
	// is already epoch-fenced. So this challenge carries no epoch (#2418).
	if refreshSessionID == "" {
		return "", "", errors.New("MFA upgrade challenge requires a refresh session")
	}
	return generateChallengeTokenWithTTL(userID, PurposeMFAUpgrade, JWTSecret(h.jwtSecret), "", "", RefreshSessionID(refreshSessionID), challengeTTL)
}

// BeginWebAuthnLogin starts a WebAuthn assertion ceremony for login.
// Stores session data in Redis keyed by the challenge JTI.
// Returns credential request options for the client, or nil if user has no WebAuthn credentials.
func (h *Handler) BeginWebAuthnLogin(ctx context.Context, userID string, jti string) (interface{}, error) {
	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("build webauthn user: %w", err)
	}
	if len(user.WebAuthnCredentials()) == 0 {
		return nil, nil
	}

	assertion, session, err := h.webauthn.BeginLogin(user)
	if err != nil {
		return nil, fmt.Errorf("begin login: %w", err)
	}

	sessionJSON, err := json.Marshal(session)
	if err != nil {
		return nil, fmt.Errorf("marshal webauthn login session: %w", err)
	}
	// The verify step reads this session back; options handed out without it
	// describe a ceremony the server can never complete.
	sessionKey := fmt.Sprintf("mfa_webauthn_session:%s", jti)
	if err := h.redis.Set(ctx, sessionKey, sessionJSON, challengeTTL).Err(); err != nil {
		return nil, fmt.Errorf("store webauthn login session: %w", err)
	}

	return assertion, nil
}

// ── Helper ───────────────────────────────────────────────────────────────────

func (h *Handler) verifyUserPassword(ctx context.Context, userID, password string) (bool, error) {
	var passwordHash string
	if err := h.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&passwordHash); err != nil {
		return false, fmt.Errorf("fetch password hash: %w", err)
	}
	return auth.VerifyPassword(password, passwordHash)
}

func containsStr(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}

// ── MFA flag sync (B1) ───────────────────────────────────────────────────────
//
// users.mfa_methods / mfa_enabled / mfa_enabled_at are a denormalized mirror of
// four sources: user_mfa_totp and user_mfa_webauthn in Postgres, and the
// email/SMS enabled keys in Redis. Login and refresh read the mirror, so a
// mirror that disagrees with a factor is a live defect in either direction: an
// enrolled factor missing from it is never challenged, and a removed factor
// still in it is demanded at login with nothing to satisfy it.
//
// The invariant: every factor write and its flag write commit in ONE
// transaction, and that transaction locks the users row FIRST, so every writer
// (and every step-up gate) takes users → factor-table locks in one order.
// Redis is read BEFORE the transaction opens, never inside it. When that read
// fails, the flags are still written — the TOTP/WebAuthn half is exact because
// it comes from the tables inside the transaction, and the email/SMS half keeps
// whatever the row already lists (mfaFlagsDegradedSQL). A failed Redis read may
// therefore leave email/SMS listed that is no longer on, which is fail-closed;
// it can never drop one that is on, and it can never skip the write.

// errSubjectGone reports that the authenticated account's users row no longer
// exists (a deleted account holding a still-valid JWT).
var errSubjectGone = errors.New("user no longer exists")

// mfaFlagsExactSQL writes the flags when the email/SMS state is known. It is
// also the first-activation rule: mfa_enabled_at is set only on a NULL → on
// transition (pre-existing sessions are challenged on their next refresh) and
// cleared when MFA goes fully off, so re-enabling later stamps a fresh one.
const mfaFlagsExactSQL = `
	UPDATE users
	SET mfa_methods    = $1::text[],
	    mfa_enabled    = cardinality($1::text[]) > 0,
	    mfa_enabled_at = CASE WHEN cardinality($1::text[]) > 0 THEN COALESCE(mfa_enabled_at, NOW()) END
	WHERE id = $2`

// mfaFlagsDegradedSQL writes the flags when the email/SMS state could not be
// read: $1 holds the exact inline factors plus any email/SMS known to be on,
// and the row's own email/SMS entries are carried forward. Every SET
// expression reads the OLD row, so the statement is atomic.
const mfaFlagsDegradedSQL = `
	UPDATE users
	SET mfa_methods    = $1::text[] || ARRAY(
	        SELECT m FROM unnest(mfa_methods) AS m
	        WHERE m IN ('email', 'sms') AND m <> ALL ($1::text[])),
	    mfa_enabled    = cardinality($1::text[]) > 0 OR mfa_methods && ARRAY['email', 'sms'],
	    mfa_enabled_at = CASE WHEN cardinality($1::text[]) > 0 OR mfa_methods && ARRAY['email', 'sms']
	                          THEN COALESCE(mfa_enabled_at, NOW()) END
	WHERE id = $2`

// mfaNeverEnabled reports whether MFA has never been active on the account.
// Read it before the enable's flag-sync transaction writes
// users.mfa_enabled_at. A read error answers false, which keeps the pre-MFA
// challenge.
func (h *Handler) mfaNeverEnabled(ctx context.Context, userID string) bool {
	var never bool
	if err := h.db.QueryRowContext(ctx, `SELECT mfa_enabled_at IS NULL FROM users WHERE id = $1`, userID).Scan(&never); err != nil {
		h.log.Error("Failed to read MFA enablement for the enrollment upgrade", "user_id", userID, "error", err)
		return false
	}
	return never
}

// grantEnrollmentUpgrade exempts the enrolling session from the pre-MFA refresh
// challenge. That challenge makes every session older than mfa_enabled_at prove
// the new factor once, and this session just did. Call it only for the first
// activation: a session older than an EXISTING mfa_enabled_at keeps its
// challenge even when it adds a factor. A token without a sid keeps it too.
func (h *Handler) grantEnrollmentUpgrade(ctx context.Context, userID, sessionID string) {
	if sessionID == "" {
		return
	}
	if err := h.redis.Set(ctx, auth.MFAUpgradeBypassKey(userID, sessionID), "1", mfaUpgradeBypassTTL).Err(); err != nil {
		// The enable succeeded; this session is simply challenged as before.
		h.log.Error("Failed to exempt the enrolling session from the MFA upgrade challenge", "user_id", userID, "error", err)
	}
}

// emailSmsState is the Redis half of the flags, read before a transaction.
type emailSmsState struct {
	// known is false when the state store could not be read.
	known bool
	// email and sms are the exact state when known; when !known they name the
	// methods known to be ON regardless (e.g. just activated), and the row's
	// own entries are carried forward for the rest.
	email, sms bool
}

// withKnownOn marks methods as on without claiming anything about the others.
func (s emailSmsState) withKnownOn(methods []string) emailSmsState {
	s.email = s.email || containsStr(methods, "email")
	s.sms = s.sms || containsStr(methods, "sms")
	return s
}

// readEmailSmsForSync reads the email/SMS state for a flag sync. It never
// fails: an unreadable store yields known=false, which the sync handles by
// carrying the row's entries forward.
func (h *Handler) readEmailSmsForSync(ctx context.Context, userID string) emailSmsState {
	email, sms, err := h.readEmailSmsEnabled(ctx, userID)
	if err != nil {
		h.log.Warn("Email/SMS MFA state unreadable; MFA flags keep the listed email/SMS factors", "error", err)
		return emailSmsState{}
	}
	return emailSmsState{known: true, email: email, sms: sms}
}

// lockUserForMFAWriteTx is the first statement of a factor write that takes no
// credential (it has no step-up of its own): it only orders the write against
// every other users-row holder.
func lockUserForMFAWriteTx(ctx context.Context, tx *sql.Tx, userID string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return errSubjectGone
	}
	if err != nil {
		return fmt.Errorf("lock user for MFA write: %w", err)
	}
	return nil
}

// writeMFAFlagsTx writes the flags from an already-read inline set. Use it
// only when inline was read on this transaction after the users-row lock.
func writeMFAFlagsTx(ctx context.Context, tx *sql.Tx, userID string, inline []string, es emailSmsState) error {
	methods := make([]string, 0, len(inline)+2) // non-nil: pq.Array must send '{}', never NULL
	methods = append(methods, inline...)
	if es.email {
		methods = append(methods, "email")
	}
	if es.sms {
		methods = append(methods, "sms")
	}
	query := mfaFlagsExactSQL
	if !es.known {
		query = mfaFlagsDegradedSQL
	}
	res, err := tx.ExecContext(ctx, query, pq.Array(methods), userID)
	if err != nil {
		return fmt.Errorf("write MFA flags: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("read MFA flags write count: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("write MFA flags: %d rows affected", n)
	}
	return nil
}

// syncMFAFlagsTx derives the inline factors on tx and writes the flags.
func syncMFAFlagsTx(ctx context.Context, tx *sql.Tx, userID string, es emailSmsState) error {
	inline, err := stepup.InlineMFAMethods(ctx, tx, userID)
	if err != nil {
		return err
	}
	return writeMFAFlagsTx(ctx, tx, userID, inline, es)
}

// withMFAFactorWriteTx runs write (nil for a flags-only resync) and the flag
// sync in ONE transaction that locks the users row first. Any error rolls the
// whole thing back: a factor whose flags could not be written is not written
// either. errSubjectGone is returned unwrapped-matchable. Every error before
// Commit skips the permission invalidation; Commit itself is always followed by
// one, whatever it returns, because a Commit error does not prove a rollback.
func (h *Handler) withMFAFactorWriteTx(ctx context.Context, userID string, es emailSmsState, write func(*sql.Tx) error) error {
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin MFA factor write: %w", err)
	}
	defer h.rollbackQuietly(tx)
	if err := lockUserForMFAWriteTx(ctx, tx, userID); err != nil {
		return err
	}
	if write != nil {
		if err := write(tx); err != nil {
			return err
		}
	}
	if err := syncMFAFlagsTx(ctx, tx, userID, es); err != nil {
		return err
	}
	commitErr := tx.Commit()
	// Invalidate whatever Commit returned. A Commit error does not prove a
	// rollback: the server may have committed and the acknowledgement been
	// lost. An extra bump costs one cache miss; a skipped one after a commit
	// that did apply would leave a removed factor's dangerous bits cached until
	// the TTL (#3453).
	h.invalidatePermissionState(ctx, userID)
	if commitErr != nil {
		return fmt.Errorf("commit MFA factor write: %w", commitErr)
	}
	return nil
}

// failMFAFactorWrite answers a failed withMFAFactorWriteTx: a vanished account
// is the client's 401, anything else is logged and a 500 with the route's body.
func (h *Handler) failMFAFactorWrite(c *gin.Context, logMsg, body string, err error) {
	if errors.Is(err, errSubjectGone) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": stepup.ErrMsgSessionNoLongerValid})
		return
	}
	h.log.Error(logMsg, "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": body})
}

// emailOrSmsOnTx answers D1's "is email or SMS on?" for a transaction that
// already holds the users-row lock. A known state answers directly; an unknown
// one falls back to the row's own listing — the same evidence the degraded
// flag write carries forward, so the invariant and the flags agree.
func emailOrSmsOnTx(ctx context.Context, tx *sql.Tx, userID string, es emailSmsState) (bool, error) {
	if es.email || es.sms {
		return true, nil
	}
	if es.known {
		return false, nil
	}
	var listed bool
	if err := tx.QueryRowContext(ctx,
		`SELECT mfa_methods && ARRAY['email', 'sms']::text[] FROM users WHERE id = $1`, userID,
	).Scan(&listed); err != nil {
		return false, fmt.Errorf("read listed email/SMS factors: %w", err)
	}
	return listed, nil
}

// refuseLastInlineFactorTx enforces D1 after a factor delete, on the same
// transaction: if no inline factor remains while email or SMS is on, it writes
// the 409 and returns true (the caller returns; the deferred rollback undoes
// the delete). An account reaching that state would be asked for an email/SMS
// code at login while its step-up (policy P1) had no MFA leg at all. A read
// failure is a 500 with failBody, and also returns true.
func (h *Handler) refuseLastInlineFactorTx(c *gin.Context, tx *sql.Tx, userID string, es emailSmsState, failBody string) bool {
	ctx := c.Request.Context()
	remaining, err := stepup.InlineMFAMethods(ctx, tx, userID)
	if err != nil {
		h.log.Error("Failed to read remaining inline MFA factors", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": failBody})
		return true
	}
	if len(remaining) > 0 {
		return false
	}
	on, err := emailOrSmsOnTx(ctx, tx, userID, es)
	if err != nil {
		h.log.Error("Failed to read email/SMS MFA state for the last-factor check", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": failBody})
		return true
	}
	if !on {
		return false
	}
	c.JSON(http.StatusConflict, gin.H{"error": errMsgInlineFactorRequired, "inline_factor_required": true})
	return true
}

// ── TOTP Endpoints ───────────────────────────────────────────────────────────

// GetStatus returns the user's MFA status across all methods. Every read fails
// closed to a 500: a failed read served as "no factor" would tell the client
// MFA is off when the server does not know that.
func (h *Handler) GetStatus(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	result := gin.H{
		"totp_enabled":           false,
		"totp_confirmed":         false,
		"webauthn_credentials":   0,
		"backup_codes_remaining": 0,
		"mfa_enabled":            false,
		"methods":                []string{},
	}

	// TOTP status
	totpActive, err := h.addTOTPStatus(ctx, userID, result)
	if err != nil {
		h.failMFAStatus(c, "totp", err)
		return
	}

	// WebAuthn credential count
	var webauthnCount int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&webauthnCount); err != nil {
		h.failMFAStatus(c, "webauthn", err)
		return
	}
	result["webauthn_credentials"] = webauthnCount

	// Overall — read denormalized flags, then self-heal if stale
	flags, err := h.readMFAUserFlags(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		// A deleted account holding a still-valid JWT (the EmailSmsVerify
		// precedent): client-side, so 401, not 5xx.
		c.JSON(http.StatusUnauthorized, gin.H{"error": stepup.ErrMsgSessionNoLongerValid})
		return
	}
	if err != nil {
		h.failMFAStatus(c, "flags", err)
		return
	}

	// Email/SMS MFA status (stored in Redis). Read before the resync so the
	// resync compares — and writes — all four sources, not two.
	emailEnabled, smsEnabled, err := h.readEmailSmsEnabled(ctx, userID)
	if err != nil {
		h.failMFAStatus(c, "email_sms", err)
		return
	}
	actual := mfaFactorState{totp: totpActive, webauthn: webauthnCount > 0, email: emailEnabled, sms: smsEnabled}
	if err := h.resyncStaleMFAFlags(ctx, userID, &flags, actual); err != nil {
		h.failMFAStatus(c, "flags_resync", err)
		return
	}

	result["mfa_enabled"] = flags.enabled
	result["methods"] = flags.methods
	result["recovery_only_methods"] = flags.recoveryOnly
	result["recovery_hardened"] = flags.recoveryHardened
	if flags.backupEmail.Valid {
		result["backup_email"] = flags.backupEmail.String
	} else {
		result["backup_email"] = ""
	}
	result["email_mfa_enabled"] = emailEnabled
	result["sms_mfa_enabled"] = smsEnabled

	c.JSON(http.StatusOK, result)
}

// failMFAStatus answers an MFA status read failure. stage is a fixed
// identifier naming the read that broke; the cause is logged and never
// serialized.
func (h *Handler) failMFAStatus(c *gin.Context, stage string, err error) {
	h.log.Error(errMsgFailedLoadMFAStatus, "stage", stage, "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedLoadMFAStatus})
}

// mfaFactorState is what the four factor sources say right now.
type mfaFactorState struct {
	totp, webauthn, email, sms bool
}

// agrees reports whether the denormalized methods list every factor that is
// on and none that is off.
func (s mfaFactorState) agrees(methods []string) bool {
	return s.totp == containsStr(methods, "totp") &&
		s.webauthn == containsStr(methods, "webauthn") &&
		s.email == containsStr(methods, "email") &&
		s.sms == containsStr(methods, "sms")
}

// addTOTPStatus fills GetStatus's TOTP fields and reports whether TOTP is
// active (enabled AND confirmed). No row is the ordinary not-enrolled state;
// any other read error is returned rather than served as "not enrolled".
func (h *Handler) addTOTPStatus(ctx context.Context, userID string, result gin.H) (bool, error) {
	var totpEnabled, totpConfirmed bool
	var backupUsed []bool
	var backupHashes []string
	err := h.db.QueryRowContext(ctx, `SELECT enabled, confirmed, backup_codes_hash, backup_codes_used FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(&totpEnabled, &totpConfirmed, pq.Array(&backupHashes), pq.Array(&backupUsed))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read TOTP status: %w", err)
	}
	result["totp_enabled"] = totpEnabled
	result["totp_confirmed"] = totpConfirmed
	remaining := 0
	for i, used := range backupUsed {
		if !used && i < len(backupHashes) {
			remaining++
		}
	}
	result["backup_codes_remaining"] = remaining
	return totpEnabled && totpConfirmed, nil
}

// mfaUserFlags is the users-row half of GetStatus.
type mfaUserFlags struct {
	enabled          bool
	methods          []string
	recoveryOnly     []string
	recoveryHardened bool
	backupEmail      sql.NullString
}

// readMFAUserFlags reads the denormalized MFA flags. sql.ErrNoRows stays
// matchable through the wrap.
func (h *Handler) readMFAUserFlags(ctx context.Context, userID string) (mfaUserFlags, error) {
	var f mfaUserFlags
	if err := h.db.QueryRowContext(ctx,
		`SELECT mfa_enabled, mfa_methods, recovery_only_methods, recovery_hardened, backup_email FROM users WHERE id = $1`, userID,
	).Scan(&f.enabled, pq.Array(&f.methods), pq.Array(&f.recoveryOnly), &f.recoveryHardened, &f.backupEmail); err != nil {
		return mfaUserFlags{}, fmt.Errorf("read MFA flags: %w", err)
	}
	return f, nil
}

// resyncStaleMFAFlags self-heals the denormalized flags when they disagree
// with any of the four factor sources — including email/SMS, which a commit
// failing after EmailSmsDisable's Redis delete can leave listed — then
// re-reads them into f. A failed resync is returned, never served as if the
// flags were right: the caller answers 500 rather than showing a status the
// server knows to be stale.
func (h *Handler) resyncStaleMFAFlags(ctx context.Context, userID string, f *mfaUserFlags, actual mfaFactorState) error {
	if actual.agrees(f.methods) {
		return nil
	}
	h.log.Warn("MFA flags out of sync, resyncing", "user_id", userID,
		"actual_totp", actual.totp, "actual_webauthn", actual.webauthn,
		"actual_email", actual.email, "actual_sms", actual.sms)
	es := emailSmsState{known: true, email: actual.email, sms: actual.sms}
	if err := h.withMFAFactorWriteTx(ctx, userID, es, nil); err != nil {
		return fmt.Errorf("resync MFA flags: %w", err)
	}
	if err := h.db.QueryRowContext(ctx, `SELECT mfa_enabled, mfa_methods FROM users WHERE id = $1`, userID).
		Scan(&f.enabled, pq.Array(&f.methods)); err != nil {
		return fmt.Errorf("re-read MFA flags after resync: %w", err)
	}
	return nil
}

// TOTPSetup initiates TOTP enrollment. Requires password confirmation (and MFA if already active).
func (h *Handler) TOTPSetup(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		mfaStepUpCredentials
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequest})
		return
	}

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, totpSetupStepUp); !ok {
		return
	}

	// Check if already has confirmed TOTP. The upsert below resets enabled and
	// confirmed to FALSE, so an unreadable guard must stop here: proceeding
	// would silently disable a working factor.
	var existingConfirmed bool
	checkErr := h.db.QueryRowContext(ctx, `SELECT confirmed FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(&existingConfirmed)
	switch {
	case errors.Is(checkErr, sql.ErrNoRows):
		// No TOTP row yet: a first enrolment proceeds.
	case checkErr != nil:
		h.log.Error("Failed to read existing TOTP state", "error", checkErr, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start TOTP setup"})
		return
	case existingConfirmed:
		c.JSON(http.StatusConflict, gin.H{"error": "TOTP is already enabled. Disable it first to re-enroll."})
		return
	}

	// Get user email for the TOTP issuer label
	var email string
	if err := h.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		h.log.Error("Failed to read the account email for TOTP setup", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch user"})
		return
	}

	// Generate TOTP secret
	key, err := GenerateSecret(email)
	if err != nil {
		h.log.Error("Failed to generate TOTP secret", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate MFA secret"})
		return
	}

	// Encrypt the secret for storage
	ciphertext, nonce, keyVer, err := h.keyring.Seal([]byte(key.Secret()))
	if err != nil {
		h.log.Error("Failed to encrypt TOTP secret", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to secure MFA secret"})
		return
	}

	// Upsert (replace pending setup or insert new) and sync the flags in one
	// transaction: re-enrolling resets enabled/confirmed to FALSE, so the old
	// "totp" entry must leave the flags in the same commit, never after it.
	es := h.readEmailSmsForSync(ctx, userID)
	if err := h.withMFAFactorWriteTx(ctx, userID, es, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
			VALUES ($1, $2, $3, $4, FALSE, FALSE)
			ON CONFLICT (user_id) DO UPDATE SET
				totp_secret_enc = EXCLUDED.totp_secret_enc,
				totp_secret_nonce = EXCLUDED.totp_secret_nonce,
				key_version = EXCLUDED.key_version,
				enabled = FALSE,
				confirmed = FALSE,
				verified_at = NULL,
				confirmed_at = NULL,
				backup_codes_hash = '{}',
				backup_codes_used = '{}',
				last_used_step = NULL,
				updated_at = NOW()
		`, userID, ciphertext, nonce, keyVer); err != nil {
			return fmt.Errorf("store TOTP secret: %w", err)
		}
		return nil
	}); err != nil {
		h.failMFAFactorWrite(c, "Failed to store TOTP secret", errMsgFailedStoreTOTPSeed, err)
		return
	}
	h.clearStepUpAfterSuccess(c, userID)

	// The secret was replaced, so a session that verified the previous one must
	// not match at confirm-setup for this one. Stop if the record survives: the
	// user retries setup, and nothing is lost.
	if err := h.redis.Del(ctx, fmt.Sprintf(redisKeyTOTPSetupSession, userID)).Err(); err != nil {
		h.log.Error("Failed to clear the TOTP setup session", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreTOTPSeed})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"otpauth_url": key.URL(),
		"secret":      key.Secret(),
	})
}

// TOTPVerifySetup validates a TOTP code to complete step 1 of enrollment.
// Returns backup codes but does NOT activate MFA yet (requires confirm-setup).
func (h *Handler) TOTPVerifySetup(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Code string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgCodeRequired})
		return
	}

	// Rate limit: check for MFA setup lockout. An unreadable lockout fails
	// closed — reading it as "not locked" lets codes be guessed during an outage.
	lockoutKey := fmt.Sprintf("mfa_setup_lockout:%s", userID)
	locked, err := h.redis.Exists(ctx, lockoutKey).Result()
	if err != nil {
		h.log.Error("Failed to read the MFA setup lockout", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAVerificationUnavailable})
		return
	}
	if locked > 0 {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": errMsgTooManyAttempts})
		return
	}

	// Fetch the pending TOTP secret
	var secretEnc, secretNonce []byte
	var keyVersion int
	var enabled bool

	err = h.db.QueryRowContext(ctx,
		`SELECT totp_secret_enc, totp_secret_nonce, key_version, enabled FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(&secretEnc, &secretNonce, &keyVersion, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No TOTP setup in progress. Call /mfa/totp/setup first."})
		return
	}
	if err != nil {
		h.log.Error("Failed to read the pending TOTP secret", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyCode})
		return
	}
	if enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "TOTP is already verified"})
		return
	}

	// Decrypt and validate
	secret, err := h.keyring.Open(secretEnc, secretNonce, keyVersion)
	if err != nil {
		h.log.Error("Failed to decrypt TOTP secret",
			"user_id", userID, "sealed_version", keyVersion, "active_version", h.keyring.ActiveVersion(), "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyCode})
		return
	}

	step, matched := MatchCodeStep(string(secret), req.Code)
	if !matched {
		// Track failed attempts. An attempt that could not be counted fails
		// closed rather than answering "invalid code" uncounted.
		attemptsKey := fmt.Sprintf("mfa_setup_attempts:%s", userID)
		if err := h.recordCodeFailure(ctx, userID, attemptsKey, lockoutKey); err != nil {
			h.log.Error("Failed to record MFA setup attempt", "error", err, "user_id", userID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAVerificationUnavailable})
			return
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid code"})
		return
	}

	// Code valid — generate backup codes
	codes, hashes, err := GenerateBackupCodes()
	if err != nil {
		h.log.Error(errMsgFailedBackupCodes, "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBackupCodes})
		return
	}

	// Mark as enabled (code verified) but NOT confirmed (backup codes not yet
	// acknowledged), recording the code's step. The guard is the compare-and-set
	// for two races. `enabled = FALSE`: of two submissions that both passed the
	// read above, the second waits on the first's row lock, re-reads enabled as
	// TRUE and updates nothing, so only one response carries backup codes, and
	// they are the stored ones. `totp_secret_nonce`: a TOTPSetup re-enrolment
	// that lands between the read and this write stores a new secret whose row
	// is also not enabled; the nonce, fresh on every seal, is what tells the
	// secret this code was checked against from the one now stored.
	if !h.enableVerifiedTOTP(ctx, c, userID, hashes, step, secretNonce) {
		return
	}
	// The UPDATE autocommitted, so this is after its commit.
	h.invalidatePermissionState(ctx, userID)

	// Clear attempt tracking
	h.delBestEffort(ctx, "Failed to clear MFA setup attempt counter", userID, fmt.Sprintf("mfa_setup_attempts:%s", userID))

	// confirm-setup takes no code, so it may exempt a session from the pre-MFA
	// challenge only if that session is the one that proved the code here.
	if sid := middleware.TokenSessionID(c); sid != "" {
		if err := h.redis.Set(ctx, fmt.Sprintf(redisKeyTOTPSetupSession, userID), sid, totpSetupSessionTTL).Err(); err != nil {
			h.log.Error("Failed to record the TOTP setup session", "user_id", userID, "error", err)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"backup_codes": codes,
		"message":      "TOTP verified. Save your backup codes, then call /mfa/totp/confirm-setup to activate MFA.",
	})
}

// enableVerifiedTOTP runs TOTPVerifySetup's guarded UPDATE: it marks the
// factor enabled, stores the backup-code hashes and records the code's step,
// but only while the row is still not enabled and still holds the secret the
// code was checked against. It answers the request itself and returns false
// when the UPDATE failed or changed no row.
func (h *Handler) enableVerifiedTOTP(ctx context.Context, c *gin.Context, userID string, hashes []string, step int64, secretNonce []byte) bool {
	usedFlags := make([]bool, len(hashes))
	result, err := h.db.ExecContext(ctx, `
		UPDATE user_mfa_totp
		SET enabled = TRUE, verified_at = NOW(), backup_codes_hash = $1, backup_codes_used = $2, last_used_step = $3, updated_at = NOW()
		WHERE user_id = $4 AND enabled = FALSE AND totp_secret_nonce = $5
	`, pq.Array(hashes), pq.Array(usedFlags), step, userID, secretNonce)
	if err != nil {
		h.log.Error("Failed to update TOTP status", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to complete verification"})
		return false
	}
	rows, err := result.RowsAffected()
	if err != nil {
		h.log.Error("Failed to read the TOTP status update result", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to complete verification"})
		return false
	}
	if rows != 1 {
		h.refuseUnappliedVerifySetup(ctx, c, userID)
		return false
	}
	return true
}

// errMsgTOTPSetupRestarted is the 409 for a verify-setup whose secret was
// replaced by a newer TOTPSetup after its code was checked.
const errMsgTOTPSetupRestarted = "TOTP setup was restarted. Enter a code from the newest QR code."

// refuseUnappliedVerifySetup answers a verify-setup whose guarded UPDATE
// changed no row. It re-reads only to choose honest copy: a concurrent
// submission already enabled the factor, or a re-enrolment replaced the
// secret the code was checked against. Either way nothing was enabled and no
// backup codes are returned, so a failed re-read still answers 409.
func (h *Handler) refuseUnappliedVerifySetup(ctx context.Context, c *gin.Context, userID string) {
	var enabled bool
	err := h.db.QueryRowContext(ctx, `SELECT enabled FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(&enabled)
	if err == nil && enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "TOTP is already verified"})
		return
	}
	c.JSON(http.StatusConflict, gin.H{"error": errMsgTOTPSetupRestarted})
}

// TOTP confirm-setup refusals, carried out of the write transaction so it
// rolls back before the handler answers.
var (
	errTOTPSetupNotStarted  = errors.New("no TOTP setup in progress")
	errTOTPSetupNotVerified = errors.New("TOTP code not yet verified")
)

// TOTPConfirmSetup activates MFA after the user confirms they saved their backup codes.
func (h *Handler) TOTPConfirmSetup(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	// The confirmation and the flag write commit together (B1). On the
	// already-confirmed path nothing is confirmed, but the flags are still
	// recomputed and committed: a confirm whose flag write was lost before B1
	// left a live TOTP factor that login never challenged, and this retry is
	// how that account heals. Only a row whose flags already list TOTP is a
	// completed activation (409); the healing retry answers 200 and grants as
	// a first confirm would.
	es := h.readEmailSmsForSync(ctx, userID)
	firstActivation := h.mfaNeverEnabled(ctx, userID)
	alreadyActive := false
	err := h.withMFAFactorWriteTx(ctx, userID, es, func(tx *sql.Tx) error {
		var enabled, confirmed bool
		err := tx.QueryRowContext(ctx,
			`SELECT enabled, confirmed FROM user_mfa_totp WHERE user_id = $1 FOR UPDATE`, userID,
		).Scan(&enabled, &confirmed)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return errTOTPSetupNotStarted
		case err != nil:
			return fmt.Errorf("read TOTP setup state: %w", err)
		case !enabled:
			return errTOTPSetupNotVerified
		case confirmed:
			// The users row is locked, so these are the flags this commit replaces.
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE('totp' = ANY(mfa_methods), FALSE) FROM users WHERE id = $1`, userID,
			).Scan(&alreadyActive); err != nil {
				return fmt.Errorf("read TOTP activation state: %w", err)
			}
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE user_mfa_totp SET confirmed = TRUE, confirmed_at = NOW(), updated_at = NOW() WHERE user_id = $1
		`, userID); err != nil {
			return fmt.Errorf("confirm TOTP setup: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, errTOTPSetupNotStarted):
		c.JSON(http.StatusBadRequest, gin.H{"error": "No TOTP setup in progress"})
	case errors.Is(err, errTOTPSetupNotVerified):
		c.JSON(http.StatusBadRequest, gin.H{"error": "TOTP code not yet verified. Complete verify-setup first."})
	case err != nil:
		h.failMFAFactorWrite(c, "Failed to confirm TOTP setup", errMsgFailedActivateMFA, err)
	case alreadyActive:
		c.JSON(http.StatusConflict, gin.H{"error": "TOTP MFA is already active"})
	default:
		h.grantTOTPEnrollmentUpgrade(c, userID, firstActivation)
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorEnabled, AuthMethod: securityevent.AuthTOTP})
		c.JSON(http.StatusOK, gin.H{"message": "MFA is now active"})
	}
}

// grantTOTPEnrollmentUpgrade exempts the session that verified the new secret,
// and only on a first activation. confirm-setup takes no code, so verify-setup
// recorded which session proved it; a missing or unreadable record matches no
// session, so it keeps the challenge. The record is consumed either way.
func (h *Handler) grantTOTPEnrollmentUpgrade(c *gin.Context, userID string, firstActivation bool) {
	ctx := c.Request.Context()
	provedBy, err := h.redis.GetDel(ctx, fmt.Sprintf(redisKeyTOTPSetupSession, userID)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		h.log.Error("Failed to read the TOTP setup session", "error", err)
	}
	sid := middleware.TokenSessionID(c)
	if firstActivation && sid != "" && subtle.ConstantTimeCompare([]byte(sid), []byte(provedBy)) == 1 {
		h.grantEnrollmentUpgrade(ctx, userID, sid)
	}
}

// totpDisableRequest is TOTPDisable's body. Its bodies predate the step-up
// seam and are unchanged; only the transaction around them is new.
type totpDisableRequest struct {
	Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
	Code     string `json:"code" binding:"required"`
}

// TOTPDisable disables TOTP MFA. Requires password + a valid MFA code.
//
// Everything runs in ONE transaction that takes the users row FOR NO KEY
// UPDATE first (it writes the flags): the password and code are checked under
// that lock, the code on the transaction so a rollback cannot burn a backup
// code, the delete and the flags commit together (B1), and D1 is checked
// before the commit.
func (h *Handler) TOTPDisable(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req totpDisableRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password and MFA code are required"})
		return
	}

	es := h.readEmailSmsForSync(ctx, userID)
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		h.log.Error("Failed to begin TOTP disable transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDisableMFA})
		return
	}
	defer h.rollbackQuietly(tx)
	subj, e := stepup.LockSubjectTx(ctx, tx, userID, stepup.LockForNoKeyUpdate, middleware.TokenCredentialEpoch(c))
	if e != nil {
		h.refuseMFASettingsStepUp(c, e, subjectStage(e))
		return
	}

	// Check if TOTP is actually enrolled before proceeding. A read error is a
	// 500, never "not enrolled" (which answered 200 on a fault).
	var totpExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_mfa_totp WHERE user_id = $1)`, userID).Scan(&totpExists); err != nil {
		h.log.Error("Failed to read TOTP enrollment", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDisableMFA})
		return
	}
	// Row gone but mfa_methods may still list 'totp': the sync below cleans
	// the denormalized flags either way.
	if totpExists && !h.verifyAndDeleteTOTPTx(c, tx, userID, subj, req, es) {
		return
	}
	if err := syncMFAFlagsTx(ctx, tx, userID, es); err != nil {
		h.log.Error("Failed to sync MFA flags after TOTP disable", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDisableMFA})
		return
	}
	commitErr := tx.Commit()
	// Whatever Commit returned; see withMFAFactorWriteTx.
	h.invalidatePermissionState(ctx, userID)
	if commitErr != nil {
		h.log.Error("Failed to commit TOTP disable", "error", commitErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDisableMFA})
		return
	}

	if totpExists {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorDisabled, AuthMethod: securityevent.AuthTOTP})
	}
	c.JSON(http.StatusOK, gin.H{"message": "TOTP MFA has been disabled"})
}

// verifyAndDeleteTOTPTx is TOTPDisable's verified delete, on the caller's
// locked transaction. On refusal or failure it has written the response and
// returns false; the caller's deferred rollback undoes the delete.
func (h *Handler) verifyAndDeleteTOTPTx(c *gin.Context, tx *sql.Tx, userID string, subj stepup.Subject, req totpDisableRequest, es emailSmsState) bool {
	ctx := c.Request.Context()

	match, err := auth.VerifyPassword(req.Password, subj.PasswordHash)
	if err != nil {
		// Never log err: an argon2 decode failure can embed the hash
		// ([internal]rules/observability.md Core principle #1).
		h.log.Error("Password verification failed during TOTP disable", "error", stepup.ErrPasswordVerification)
		c.JSON(http.StatusInternalServerError, gin.H{"error": stepup.ErrMsgVerificationFailed})
		return false
	}
	if !match {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonInvalidCredentials, AuthMethod: securityevent.AuthPassword})
		c.JSON(http.StatusForbidden, gin.H{"error": stepup.ErrMsgInvalidPassword})
		return false
	}

	valid, err := h.VerifyCodeTx(ctx, tx, userID, stepup.PurposeTOTPDisable, req.Code)
	if err != nil {
		h.log.Error("MFA code verification error during TOTP disable", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "MFA verification failed because of a server error. Contact support if this continues."})
		return false
	}
	if !valid {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid})
		c.JSON(http.StatusForbidden, gin.H{"error": stepup.ErrMsgInvalidMFACode})
		return false
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM user_mfa_totp WHERE user_id = $1`, userID); err != nil {
		h.log.Error("Failed to disable TOTP MFA", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDisableMFA})
		return false
	}
	return !h.refuseLastInlineFactorTx(c, tx, userID, es, errMsgFailedDisableMFA)
}

// RegenerateBackupCodes generates new backup codes. Requires password + TOTP code.
func (h *Handler) RegenerateBackupCodes(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		Code     string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password and TOTP code are required"})
		return
	}

	match, err := h.verifyUserPassword(ctx, userID, req.Password)
	if err != nil {
		// A failed lookup is not a wrong password.
		h.log.Error("Password verification failed during backup-code regeneration", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyLoginFactor})
		return
	}
	if !match {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgIncorrectPassword})
		return
	}

	// Verify the TOTP code (not backup code — must prove they have the authenticator)
	var secretEnc, secretNonce []byte
	var keyVersion int
	var totpEnabled, totpConfirmed bool
	err = h.db.QueryRowContext(ctx,
		`SELECT totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(&secretEnc, &secretNonce, &keyVersion, &totpEnabled, &totpConfirmed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		h.log.Error("Failed to read TOTP state for backup-code regeneration", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBackupCodes})
		return
	}
	if err != nil || !totpEnabled || !totpConfirmed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "TOTP is not enabled"})
		return
	}

	secret, decErr := h.keyring.Open(secretEnc, secretNonce, keyVersion)
	if decErr != nil {
		// A keyring decrypt failure (e.g. a retired key missing from the ring
		// during rotation) is an operator problem, not a wrong code — log it
		// with the versions so it stays diagnosable, matching the other decrypt
		// sites. The client still sees the same 403 (no oracle).
		h.log.Error("Failed to decrypt TOTP secret",
			"user_id", userID, "sealed_version", keyVersion, "active_version", h.keyring.ActiveVersion(), "error", decErr)
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInvalidTOTPCode})
		return
	}
	step, matched := MatchCodeStep(string(secret), req.Code)
	if !matched {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInvalidTOTPCode})
		return
	}

	// Generate new backup codes
	codes, hashes, err := GenerateBackupCodes()
	if err != nil {
		h.log.Error(errMsgFailedBackupCodes, "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBackupCodes})
		return
	}

	// The code's step is recorded in the same statement, under the guard
	// acceptTOTPStep uses: a replayed code, or the loser of two concurrent
	// submissions of one code, updates no row and answers as a wrong code.
	usedFlags := make([]bool, len(hashes))
	result, err := h.db.ExecContext(ctx, `
		UPDATE user_mfa_totp SET backup_codes_hash = $1, backup_codes_used = $2, last_used_step = $3, updated_at = NOW()
		WHERE user_id = $4 AND (last_used_step IS NULL OR last_used_step < $3)
	`, pq.Array(hashes), pq.Array(usedFlags), step, userID)
	if err != nil {
		h.log.Error("Failed to store regenerated backup codes", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store backup codes"})
		return
	}
	rows, err := result.RowsAffected()
	if err != nil {
		h.log.Error("Failed to read the backup-code store result", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store backup codes"})
		return
	}
	if rows != 1 {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInvalidTOTPCode})
		return
	}

	c.JSON(http.StatusOK, gin.H{"backup_codes": codes})
}

// ── WebAuthn Endpoints ──────────────────────────────────────────────────────

// WebAuthnRegisterBegin starts WebAuthn credential registration.
func (h *Handler) WebAuthnRegisterBegin(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		mfaStepUpCredentials
		CredentialName string `json:"credential_name"`
		CredentialType string `json:"credential_type"` // "hardware" or "platform"
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequest})
		return
	}

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, webAuthnRegisterStepUp); !ok {
		return
	}

	// Enforce credential limit: 10 total WebAuthn credentials per user
	credType := req.CredentialType
	if credType != "platform" {
		credType = "hardware"
	}
	var totalCount int
	err := h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM user_mfa_webauthn WHERE user_id = $1
	`, userID).Scan(&totalCount)
	if err != nil {
		h.log.Error("Failed to check credential counts", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStartReg})
		return
	}
	if totalCount >= 10 {
		c.JSON(http.StatusConflict, gin.H{"error": "Maximum of 10 WebAuthn credentials reached. Remove an existing key first."})
		return
	}

	// Build WebAuthn user with existing credentials (for exclusion)
	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
		h.log.Error("Failed to build WebAuthn user", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStartReg})
		return
	}

	// Set authenticator selection based on credential type
	var regOpts []webauthn.RegistrationOption
	if credType == "platform" {
		regOpts = append(regOpts, webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			AuthenticatorAttachment: protocol.Platform,
			RequireResidentKey:      protocol.ResidentKeyRequired(),
			ResidentKey:             protocol.ResidentKeyRequirementRequired,
			UserVerification:        protocol.VerificationRequired,
		}))
	} else {
		regOpts = append(regOpts, webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			AuthenticatorAttachment: protocol.CrossPlatform,
			UserVerification:        protocol.VerificationPreferred,
		}))
	}

	creation, session, err := h.webauthn.BeginRegistration(user, regOpts...)
	if err != nil {
		h.log.Error("WebAuthn BeginRegistration failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStartReg})
		return
	}

	credName := req.CredentialName
	if credName == "" {
		credName = "Security Key"
	}
	// Options handed out without their stored session describe a ceremony
	// the finish step can never complete, so a failed store fails the request.
	if err := h.storeRegistrationSession(ctx, userID, session, credName, credType); err != nil {
		h.log.Error("Failed to store WebAuthn registration session", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStartReg})
		return
	}

	h.clearStepUpAfterSuccess(c, userID)
	c.JSON(http.StatusOK, creation)
}

// WebAuthnRegisterFinish completes WebAuthn credential registration.
func (h *Handler) WebAuthnRegisterFinish(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	// Read and delete the session data in one step, so no failed delete can
	// leave it replayable until its TTL runs out.
	metaJSON, err := h.redis.GetDel(ctx, fmt.Sprintf(redisKeyWebAuthnReg, userID)).Bytes()
	if errors.Is(err, redis.Nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No registration in progress or session expired"})
		return
	}
	if err != nil {
		h.log.Error("Failed to read WebAuthn registration session", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to complete registration"})
		return
	}

	var meta struct {
		Session        string `json:"session"`
		CredentialName string `json:"credential_name"`
		CredentialType string `json:"credential_type"`
	}
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		h.log.Error("Failed to decode WebAuthn registration metadata", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInvalidSessionData})
		return
	}

	var session webauthn.SessionData
	if err := json.Unmarshal([]byte(meta.Session), &session); err != nil {
		h.log.Error("Failed to decode WebAuthn registration session", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInvalidSessionData})
		return
	}

	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
		h.log.Error("Failed to build the WebAuthn user for registration", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to complete registration"})
		return
	}

	credential, err := h.webauthn.FinishRegistration(user, session, c.Request)
	if err != nil {
		h.log.Error("WebAuthn FinishRegistration failed", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Registration verification failed"})
		return
	}

	firstActivation := h.mfaNeverEnabled(ctx, userID)

	// Store credential in DB
	transports := make([]string, 0, len(credential.Transport))
	for _, t := range credential.Transport {
		transports = append(transports, string(t))
	}

	// The credential and the flags commit together (B1): a key that is stored
	// but not listed is never challenged at login.
	es := h.readEmailSmsForSync(ctx, userID)
	if err := h.withMFAFactorWriteTx(ctx, userID, es, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_mfa_webauthn (user_id, credential_id, public_key, aaguid, sign_count, credential_name, credential_type, transports)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, userID, credential.ID, credential.PublicKey, credential.Authenticator.AAGUID,
			credential.Authenticator.SignCount, meta.CredentialName, meta.CredentialType, pq.Array(transports)); err != nil {
			return fmt.Errorf("store WebAuthn credential: %w", err)
		}
		return nil
	}); err != nil {
		h.failMFAFactorWrite(c, "Failed to store WebAuthn credential", "Failed to store credential", err)
		return
	}
	// The proof is the attestation this request just verified, so the session
	// finishing registration holds the new key, whichever session began it.
	if firstActivation {
		h.grantEnrollmentUpgrade(ctx, userID, middleware.TokenSessionID(c))
	}
	h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorEnabled, AuthMethod: securityevent.AuthWebAuthn})

	c.JSON(http.StatusOK, gin.H{
		"message":         "Security key registered successfully",
		"credential_name": meta.CredentialName,
		"credential_type": meta.CredentialType,
	})
}

// WebAuthnListCredentials returns the user's registered WebAuthn credentials.
func (h *Handler) WebAuthnListCredentials(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	rows, err := h.db.QueryContext(ctx, `
		SELECT id, credential_name, credential_type, created_at, last_used_at
		FROM user_mfa_webauthn WHERE user_id = $1 ORDER BY created_at
	`, userID)
	if err != nil {
		h.log.Error("Failed to list WebAuthn credentials", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedListKeys})
		return
	}
	defer rows.Close() //nolint:errcheck

	type credInfo struct {
		ID             string  `json:"id"`
		CredentialName string  `json:"credential_name"`
		CredentialType string  `json:"credential_type"`
		CreatedAt      string  `json:"created_at"`
		LastUsedAt     *string `json:"last_used_at"`
	}

	// A row it cannot read is a 500, not an omission: a list short one key
	// tells the user a registered key is gone.
	var creds []credInfo
	for rows.Next() {
		var ci credInfo
		var lastUsed sql.NullTime
		var createdAt time.Time
		if err := rows.Scan(&ci.ID, &ci.CredentialName, &ci.CredentialType, &createdAt, &lastUsed); err != nil {
			h.log.Error("Failed to read a WebAuthn credential", "user_id", userID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedListKeys})
			return
		}
		ci.CreatedAt = createdAt.Format(time.RFC3339)
		if lastUsed.Valid {
			s := lastUsed.Time.Format(time.RFC3339)
			ci.LastUsedAt = &s
		}
		creds = append(creds, ci)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Failed to list WebAuthn credentials", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedListKeys})
		return
	}

	if creds == nil {
		creds = []credInfo{}
	}
	c.JSON(http.StatusOK, gin.H{"credentials": creds})
}

// WebAuthnDeleteCredential removes a WebAuthn credential.
func (h *Handler) WebAuthnDeleteCredential(c *gin.Context) {
	userID := c.GetString("user_id")
	credentialID := c.Param("id")
	ctx := c.Request.Context()

	var req struct {
		Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgPasswordRequired})
		return
	}

	// One transaction, users row locked FOR NO KEY UPDATE first (it writes the
	// flags): password under the lock, delete, D1, flags, commit (B1).
	es := h.readEmailSmsForSync(ctx, userID)
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		h.log.Error("Failed to begin WebAuthn delete transaction", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteCredential})
		return
	}
	defer h.rollbackQuietly(tx)
	subj, e := stepup.LockSubjectTx(ctx, tx, userID, stepup.LockForNoKeyUpdate, middleware.TokenCredentialEpoch(c))
	if e != nil {
		h.refuseMFASettingsStepUp(c, e, subjectStage(e))
		return
	}
	if !h.verifyAndDeleteWebAuthnTx(c, tx, userID, credentialID, subj, req.Password, es) {
		return
	}
	if err := syncMFAFlagsTx(ctx, tx, userID, es); err != nil {
		h.log.Error("Failed to sync MFA flags after WebAuthn delete", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteCredential})
		return
	}
	commitErr := tx.Commit()
	// Whatever Commit returned; see withMFAFactorWriteTx.
	h.invalidatePermissionState(ctx, userID)
	if commitErr != nil {
		h.log.Error("Failed to commit WebAuthn delete", "error", commitErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteCredential})
		return
	}
	h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorDisabled, AuthMethod: securityevent.AuthWebAuthn})

	resp := gin.H{
		"message": "Credential deleted",
		"user_id": userID,
	}
	// The client hands this list to the authenticator as every key still
	// accepted, which hides the rest. Send it only when it is complete: an
	// empty list after a failed read would hide every remaining key.
	if remaining, err := h.remainingCredentialIDs(ctx, userID); err != nil {
		h.log.Error("Failed to list remaining WebAuthn credentials", "error", err)
	} else {
		resp["remaining_credential_ids"] = remaining
	}
	c.JSON(http.StatusOK, resp)
}

// remainingCredentialIDs returns the user's WebAuthn credential IDs, base64url
// encoded, or an error if any of them could not be read.
func (h *Handler) remainingCredentialIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT credential_id FROM user_mfa_webauthn WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("query credential IDs: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	encoded := make([]string, 0)
	for rows.Next() {
		var cid []byte
		if err := rows.Scan(&cid); err != nil {
			return nil, fmt.Errorf("read credential ID: %w", err)
		}
		encoded = append(encoded, base64.RawURLEncoding.EncodeToString(cid))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate credential IDs: %w", err)
	}
	return encoded, nil
}

// verifyAndDeleteWebAuthnTx is WebAuthnDeleteCredential's verified delete, on
// the caller's locked transaction. On refusal or failure it has written the
// response and returns false; the caller's deferred rollback undoes the delete.
func (h *Handler) verifyAndDeleteWebAuthnTx(
	c *gin.Context, tx *sql.Tx, userID, credentialID string, subj stepup.Subject, password string, es emailSmsState,
) bool {
	ctx := c.Request.Context()

	match, err := auth.VerifyPassword(password, subj.PasswordHash)
	if err != nil {
		// Never log err: an argon2 decode failure can embed the hash.
		h.log.Error("Password verification failed during WebAuthn delete", "error", stepup.ErrPasswordVerification)
		c.JSON(http.StatusInternalServerError, gin.H{"error": stepup.ErrMsgVerificationFailed})
		return false
	}
	if !match {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonInvalidCredentials, AuthMethod: securityevent.AuthPassword})
		c.JSON(http.StatusForbidden, gin.H{"error": stepup.ErrMsgInvalidPassword})
		return false
	}

	// An id that is not a UUID names no credential; without this the
	// database's parse error would answer 500 for a request that matches nothing.
	if _, err := uuid.Parse(credentialID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgKeyNotFound})
		return false
	}

	result, err := tx.ExecContext(ctx,
		`DELETE FROM user_mfa_webauthn WHERE id = $1 AND user_id = $2`, credentialID, userID)
	if err != nil {
		h.log.Error("Failed to delete WebAuthn credential", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteCredential})
		return false
	}
	rows, err := result.RowsAffected()
	if err != nil {
		h.log.Error("Failed to read WebAuthn delete count", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteCredential})
		return false
	}
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgKeyNotFound})
		return false
	}
	return !h.refuseLastInlineFactorTx(c, tx, userID, es, errMsgFailedDeleteCredential)
}

// ── Shared MFA Verify (Unauthenticated — uses challenge token) ──────────────

// verifyRequest holds the parsed MFA verify request body.
type verifyRequest struct {
	ChallengeToken string          `json:"mfa_challenge_token" binding:"required"`
	Method         string          `json:"method" binding:"required"` // "totp", "backup_code", "webauthn"
	Code           string          `json:"code"`                      // for totp/backup_code
	Assertion      json.RawMessage `json:"assertion"`                 // for webauthn
}

// Verify validates an MFA challenge (TOTP, backup code, or WebAuthn).
// This endpoint is unauthenticated — identity comes from the challenge token.
func (h *Handler) Verify(c *gin.Context) {
	var req verifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequest})
		return
	}

	claims, purpose, expired := h.parseChallengeToken(req.ChallengeToken)
	if claims == nil {
		if expired {
			h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeExpired, RouteTemplate: securityevent.RouteAuthMFAVerify})
		} else {
			h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid, RouteTemplate: securityevent.RouteAuthMFAVerify})
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired MFA challenge token"})
		return
	}

	ctx := c.Request.Context()

	lockoutKey := fmt.Sprintf(redisKeyMFAVerifyLockout, claims.UserID)
	if h.challengeRefused(ctx, c, claims, lockoutKey) {
		return
	}

	// Judging a code can spend it: a backup code is marked used and a WebAuthn
	// ceremony is deleted. Everything a Redis failure could still stop on is
	// therefore read first, so an outage never costs the user a factor.
	rememberMe, err := h.readRememberMe(ctx, claims, purpose)
	if err != nil {
		h.verifyDependencyFailed(c, claims.UserID, err)
		return
	}
	restricted, err := h.recoveryOnlyMethods(ctx, claims.UserID)
	if err != nil {
		h.verifyDependencyFailed(c, claims.UserID, err)
		return
	}

	attemptsKey := fmt.Sprintf(redisKeyMFAVerifyAttempts, claims.UserID)
	attempt, ok := h.reserveVerifyAttempt(ctx, c, claims, attemptsKey)
	if !ok {
		return
	}

	verified, matchedMethod, responded := h.verifyByMethod(ctx, c, req, claims)
	if responded {
		return // Early return already sent a response (e.g. bad request)
	}
	if verified && containsStr(restricted, answeredFactor(req.Method, matchedMethod)) {
		// A recovery-only method cannot answer this challenge. It is refused
		// exactly as a wrong code is, so the answer never confirms the code.
		verified = false
	}

	if !verified {
		h.refuseWrongCode(ctx, c, req.Method, claims.UserID, lockoutKey, attempt)
		return
	}

	h.completeVerifiedChallenge(ctx, c, claims, purpose, matchedMethod, rememberMe)
}

// recoveryOnlyMethods returns the user's recovery-only methods, which may not
// answer a sign-in challenge. Every challenge Verify and SendEmailMFACode
// accept is a sign-in challenge (parseChallengeToken admits no recovery
// purpose). The login response already leaves these methods out of the offered
// ones; this is where the server holds to it.
func (h *Handler) recoveryOnlyMethods(ctx context.Context, userID string) ([]string, error) {
	var methods, restricted []string
	if err := h.db.QueryRowContext(ctx,
		`SELECT mfa_methods, recovery_only_methods FROM users WHERE id = $1`, userID,
	).Scan(pq.Array(&methods), pq.Array(&restricted)); err != nil {
		return nil, fmt.Errorf("read recovery-only MFA methods: %w", err)
	}
	return effectiveRecoveryOnly(methods, restricted), nil
}

// effectiveRecoveryOnly returns the recovery-only methods that actually
// restrict sign-in. A restriction that would leave no enabled method able to
// sign in lapses. SetRecoveryOnly refuses to create that state, but removing
// the last other factor (a TOTP disable, a key delete, an email/SMS disable)
// reaches it, and honouring the restriction there leaves a challenge nothing
// can answer, which the login paths read as "no MFA" and sign in on the
// password alone. Requiring the recovery-only factor is the safe reading.
func effectiveRecoveryOnly(methods, recoveryOnly []string) []string {
	for _, m := range methods {
		if !containsStr(recoveryOnly, m) {
			return recoveryOnly
		}
	}
	return nil
}

// answeredFactor names the factor that answered a challenge. A code sent as
// "totp" can be a TOTP code, a backup code or an inline WebAuthn token, and
// the match says which; an email code is only ever an email code.
func answeredFactor(requested, matched string) string {
	if matched != "" {
		return matched
	}
	return requested
}

// reserveVerifyAttempt counts this attempt before any code is judged and
// reports its number. Counting first is what makes the limit hold under
// concurrency: every request gets its own number from INCR, so however many
// arrive at once, only the first failedAttemptLimit have a code checked.
// Counting after the check let every request that read a count below the limit
// try a code.
//
// A count that cannot be written, or whose window cannot be set, answers 500
// without judging the code, so the response carries nothing about whether the
// code was right. The window is set with EXPIRE NX: it starts at the first
// attempt and later attempts, refused ones included, do not extend it, so a
// trickle of requests cannot hold an account's MFA locked. NX also gives a
// count left without an expiry (an INCR whose EXPIRE failed) one on the next
// attempt, so a missed expiry is never permanent.
func (h *Handler) reserveVerifyAttempt(ctx context.Context, c *gin.Context, claims *ChallengeClaims, attemptsKey string) (int64, bool) {
	attempt, err := h.redis.Incr(ctx, attemptsKey).Result()
	if err != nil {
		h.verifyDependencyFailed(c, claims.UserID, fmt.Errorf("count MFA verify attempt: %w", err))
		return 0, false
	}
	if err := h.redis.ExpireNX(ctx, attemptsKey, verifyAttemptWindow).Err(); err != nil {
		h.verifyDependencyFailed(c, claims.UserID, fmt.Errorf("set MFA verify attempt window: %w", err))
		return 0, false
	}
	if attempt > failedAttemptLimit {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeLocked, RouteTemplate: securityevent.RouteAuthMFAVerify})
		c.JSON(http.StatusTooManyRequests, gin.H{"error": errMsgTooManyAttempts})
		return 0, false
	}
	return attempt, true
}

// releaseOwnedScript deletes each key only while it still holds ARGV[1], the
// value the caller wrote. An undo that outlives its request can therefore not
// remove a value a newer request has since written to the same key.
var releaseOwnedScript = redis.NewScript(`
local n = 0
for _, key in ipairs(KEYS) do
	if redis.call('GET', key) == ARGV[1] then
		n = n + redis.call('DEL', key)
	end
end
return n
`)

// refuseWrongCode answers a code that did not verify. At the last allowed
// attempt it arms the lockout first, and a lockout it cannot write fails the
// request closed. The per-IP failure counter is independent state: a wrong
// code is recorded there either way.
func (h *Handler) refuseWrongCode(ctx context.Context, c *gin.Context, method, userID, lockoutKey string, attempt int64) {
	var lockErr error
	if attempt >= failedAttemptLimit {
		lockErr = h.lockOutVerify(ctx, lockoutKey)
	}
	outcome := middleware.RecordAuthFailure(ctx, h.redis, c.ClientIP(), middleware.DefaultAuthBanConfig())
	middleware.MarkAuthFailureOutcome(c, outcome)
	if lockErr != nil {
		h.verifyDependencyFailed(c, userID, lockErr)
		return
	}
	h.emitHTTPEvent(c, mfaChallengeInvalidEvent(method))
	c.JSON(http.StatusForbidden, gin.H{"error": "Invalid MFA code"})
}

// lockOutVerify locks further attempts out after the last allowed wrong code.
// The caller fails the request closed when the write fails. The count still
// refuses every further attempt until its window ends, so a failed write
// shortens the lockout from verifyLockoutDuration to verifyAttemptWindow; the
// 500 stops the caller reading the limit as engaged at its full length.
func (h *Handler) lockOutVerify(ctx context.Context, lockoutKey string) error {
	if err := h.redis.Set(ctx, lockoutKey, "1", verifyLockoutDuration).Err(); err != nil {
		return fmt.Errorf("lock out MFA verify attempts: %w", err)
	}
	return nil
}

// delBestEffort removes Redis state whose purpose is already served — a
// consumed code or session, an attempt counter after success or lockout. A
// failure here must not undo the completed operation before it, so it is
// logged rather than returned. Keys can embed challenge identifiers and
// codes, so only user_id is logged beside the fixed message.
func (h *Handler) delBestEffort(ctx context.Context, msg, userID string, keys ...string) {
	if err := h.redis.Del(ctx, keys...).Err(); err != nil {
		h.log.Warn(msg, "user_id", userID)
	}
}

// recordCodeFailure counts one wrong code against attemptsKey and arms
// lockoutKey at the threshold. These writes are what bound code guessing, so
// every error is returned: a failed INCR leaves the attempt uncounted, a
// failed EXPIRE leaves a window that never slides, and a failed SET leaves
// the threshold reached with no lockout in force. The caller fails closed on
// any of them rather than answering an ordinary wrong code.
func (h *Handler) recordCodeFailure(ctx context.Context, userID, attemptsKey, lockoutKey string) error {
	attempts, err := h.redis.Incr(ctx, attemptsKey).Result()
	if err != nil {
		return fmt.Errorf("count failed attempt: %w", err)
	}
	if err := h.redis.Expire(ctx, attemptsKey, verifyAttemptWindow).Err(); err != nil {
		return fmt.Errorf("set attempt window: %w", err)
	}
	if attempts < failedAttemptLimit {
		return nil
	}
	if err := h.redis.Set(ctx, lockoutKey, "1", verifyLockoutDuration).Err(); err != nil {
		return fmt.Errorf("arm lockout: %w", err)
	}
	h.delBestEffort(ctx, "Failed to reset MFA attempt counter after lockout", userID, attemptsKey)
	return nil
}

// storeRegistrationSession stores the registration ceremony's session data
// together with the requested credential metadata (keyed by user ID, 5-min TTL).
func (h *Handler) storeRegistrationSession(ctx context.Context, userID string, session *webauthn.SessionData, credName, credType string) error {
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("marshal registration session: %w", err)
	}
	meta := map[string]interface{}{
		"session":         string(sessionJSON),
		"credential_name": credName,
		"credential_type": credType,
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal registration metadata: %w", err)
	}
	if err := h.redis.Set(ctx, fmt.Sprintf(redisKeyWebAuthnReg, userID), metaJSON, 5*time.Minute).Err(); err != nil {
		return fmt.Errorf("store registration session: %w", err)
	}
	return nil
}

// challengeRefused answers a challenge that is already used or whose user is
// locked out, and reports whether it did. Both checks fail closed: reading past
// an unreadable lockout would let a guesser try codes past it.
func (h *Handler) challengeRefused(ctx context.Context, c *gin.Context, claims *ChallengeClaims, lockoutKey string) bool {
	used, err := h.redis.Exists(ctx, fmt.Sprintf(redisKeyMFAChallengeUsed, claims.ID)).Result()
	if err != nil {
		h.verifyDependencyFailed(c, claims.UserID, err)
		return true
	}
	if used > 0 {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid, RouteTemplate: securityevent.RouteAuthMFAVerify})
		c.JSON(http.StatusUnauthorized, gin.H{"error": "MFA challenge already used"})
		return true
	}
	locked, err := h.redis.Exists(ctx, lockoutKey).Result()
	if err != nil {
		h.verifyDependencyFailed(c, claims.UserID, err)
		return true
	}
	if locked > 0 {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeLocked, RouteTemplate: securityevent.RouteAuthMFAVerify})
		c.JSON(http.StatusTooManyRequests, gin.H{"error": errMsgTooManyAttempts})
		return true
	}
	return false
}

// verifyDependencyFailed answers an MFA verification that could not reach Redis
// or the database, and logs the cause the response leaves out.
func (h *Handler) verifyDependencyFailed(c *gin.Context, userID string, err error) {
	h.log.Error("MFA verification could not reach a dependency", "user_id", userID, "error", err)
	h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable, RouteTemplate: securityevent.RouteAuthMFAVerify})
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAVerificationUnavailable})
}

// completeVerifiedChallenge records only the outcome of a completed challenge
// purpose. For login, the completer returns false until its session-mint
// transaction commits, so this path cannot report an MFA success for a failed
// login transaction.
//
// Atomically claiming the challenge before any session mint matters: a
// check-then-set permits two successful factor verifications to mint two
// sessions from one challenge. rememberMe is read by the caller before the code
// is judged. A failure from the claim onward, including the MFA-upgrade bypass
// write, spends the challenge; the user starts again.
func (h *Handler) completeVerifiedChallenge(ctx context.Context, c *gin.Context, claims *ChallengeClaims, purpose ChallengePurpose, method string, rememberMe bool) {
	usedKey := fmt.Sprintf(redisKeyMFAChallengeUsed, claims.ID)
	attemptsKey := fmt.Sprintf(redisKeyMFAVerifyAttempts, claims.UserID)
	claimed, err := h.redis.SetNX(ctx, usedKey, "1", challengeTTL).Result()
	if err != nil {
		h.verifyDependencyFailed(c, claims.UserID, err)
		return
	}
	if !claimed {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid, RouteTemplate: securityevent.RouteAuthMFAVerify})
		c.JSON(http.StatusUnauthorized, gin.H{"error": "MFA challenge already used"})
		return
	}
	// A stale count only brings the next lockout closer.
	h.delBestEffort(ctx, "Failed to clear MFA verification attempt counter", claims.UserID, attemptsKey)
	middleware.ClearAuthFailures(ctx, h.redis, c.ClientIP())
	if h.completeVerifyPurpose(ctx, c, claims, purpose, rememberMe) {
		h.emitHTTPEvent(c, mfaChallengeVerifiedEvent(method))
	}
}

// readRememberMe returns a login challenge's remember-me choice. Other
// purposes have none. An absent key means false; any other error is returned.
func (h *Handler) readRememberMe(ctx context.Context, claims *ChallengeClaims, purpose ChallengePurpose) (bool, error) {
	if purpose != PurposeLogin {
		return false, nil
	}
	value, err := h.redis.Get(ctx, auth.MFAChallengeRememberMeKey(claims.ID)).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read MFA challenge remember state: %w", err)
	}
	return value == "1", nil
}

func mfaChallengeInvalidEvent(method string) securityevent.Event {
	switch method {
	case "totp":
		return securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid, AuthMethod: securityevent.AuthTOTP, RouteTemplate: securityevent.RouteAuthMFAVerify}
	case "backup_code":
		return securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid, AuthMethod: securityevent.AuthBackupCode, RouteTemplate: securityevent.RouteAuthMFAVerify}
	case "webauthn":
		return securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid, AuthMethod: securityevent.AuthWebAuthn, RouteTemplate: securityevent.RouteAuthMFAVerify}
	default:
		return securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid, RouteTemplate: securityevent.RouteAuthMFAVerify}
	}
}

func mfaChallengeVerifiedEvent(method string) securityevent.Event {
	switch method {
	case "totp":
		return securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonChallengeVerified, AuthMethod: securityevent.AuthTOTP, RouteTemplate: securityevent.RouteAuthMFAVerify}
	case "backup_code":
		return securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonChallengeVerified, AuthMethod: securityevent.AuthBackupCode, RouteTemplate: securityevent.RouteAuthMFAVerify}
	case "webauthn":
		return securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonChallengeVerified, AuthMethod: securityevent.AuthWebAuthn, RouteTemplate: securityevent.RouteAuthMFAVerify}
	default:
		return securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonChallengeVerified, RouteTemplate: securityevent.RouteAuthMFAVerify}
	}
}

// parseChallengeToken tries all valid purposes and returns the claims, matched
// purpose, and whether every rejected parse established expiration. A malformed,
// incorrectly signed, or purpose-bound token is invalid, not expired.
func (h *Handler) parseChallengeToken(tokenStr string) (*ChallengeClaims, ChallengePurpose, bool) {
	expired := true
	for _, p := range []ChallengePurpose{PurposeLogin, PurposeSuspiciousRefresh, PurposeMFAUpgrade} {
		if parsed, err := ValidateChallengeToken(tokenStr, h.jwtSecret, p); err == nil {
			if p == PurposeMFAUpgrade && parsed.RefreshSessionID == "" {
				expired = false
				continue
			}
			return parsed, p, false
		} else if !errors.Is(err, jwt.ErrTokenExpired) {
			expired = false
		}
	}
	return nil, "", expired
}

// verifyByMethod dispatches verification to the appropriate method handler.
// Returns (verified, matchedMethod, responded) — responded is true if an HTTP
// response was already written.
func (h *Handler) verifyByMethod(ctx context.Context, c *gin.Context, req verifyRequest, claims *ChallengeClaims) (bool, string, bool) {
	switch req.Method {
	case "totp", "backup_code":
		return h.verifyTOTPOrBackup(ctx, c, req.Code, claims.UserID)
	case "webauthn":
		verified, responded := h.verifyWebAuthnChallenge(ctx, c, req.Assertion, claims)
		return verified, "webauthn", responded
	case "email":
		verified, responded := h.verifyEmailCode(ctx, c, req.Code, claims)
		return verified, "", responded
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid method. Use 'totp', 'backup_code', 'webauthn', or 'email'"})
		return false, "", true
	}
}

// verifyTOTPOrBackup verifies a TOTP or backup code.
func (h *Handler) verifyTOTPOrBackup(ctx context.Context, c *gin.Context, code, userID string) (bool, string, bool) {
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgCodeRequired})
		return false, "", true
	}
	// Login never accepts a WebAuthn inline token (see noInlinePurpose).
	valid, matchedMethod, err := h.verifyCodeMatchedMethod(ctx, h.db, userID, noInlinePurpose, code)
	if err != nil {
		h.verifyDependencyFailed(c, userID, err)
		return false, "", true
	}
	return valid, matchedMethod, false
}

// verifyWebAuthnChallenge verifies a WebAuthn assertion.
func (h *Handler) verifyWebAuthnChallenge(ctx context.Context, c *gin.Context, assertion json.RawMessage, claims *ChallengeClaims) (bool, bool) {
	if len(assertion) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Assertion is required for WebAuthn verification"})
		return false, true
	}

	// Read the keys before spending the ceremony, so a failed database read
	// leaves the ceremony for a retry.
	user, err := h.buildWebAuthnUser(ctx, claims.UserID)
	if err != nil {
		h.verifyDependencyFailed(c, claims.UserID, err)
		return false, true
	}

	// The ceremony is single-use: read and delete it in one step, so no failed
	// delete can leave it replayable.
	sessionJSON, err := h.redis.GetDel(ctx, fmt.Sprintf("mfa_webauthn_session:%s", claims.ID)).Bytes()
	if errors.Is(err, redis.Nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No WebAuthn session found. Request a new challenge."})
		return false, true
	}
	if err != nil {
		h.verifyDependencyFailed(c, claims.UserID, err)
		return false, true
	}

	var session webauthn.SessionData
	if err := json.Unmarshal(sessionJSON, &session); err != nil {
		h.log.Error("Failed to decode WebAuthn login session", "user_id", claims.UserID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid WebAuthn session"})
		return false, true
	}

	credential, err := h.webauthn.FinishLoginWithBytes(user, session, assertion)
	if err != nil {
		h.log.Warn("WebAuthn login verification failed", "error", err, "user_id", claims.UserID)
		return false, false
	}
	// Cloned-authenticator detection compares the next assertion against this
	// value, so the login does not verify unless it landed.
	if _, err := h.db.ExecContext(ctx, `
		UPDATE user_mfa_webauthn SET sign_count = $1, last_used_at = NOW() WHERE credential_id = $2 AND user_id = $3
	`, credential.Authenticator.SignCount, credential.ID, claims.UserID); err != nil {
		h.verifyDependencyFailed(c, claims.UserID, fmt.Errorf("update WebAuthn sign count: %w", err))
		return false, true
	}
	return true, false
}

// verifyEmailCode verifies a 6-digit email MFA code.
func (h *Handler) verifyEmailCode(ctx context.Context, c *gin.Context, rawCode string, claims *ChallengeClaims) (bool, bool) {
	if rawCode == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgCodeRequired})
		return false, true
	}
	code := strings.TrimSpace(rawCode)
	if !isValidEmailCode(code) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email code must be 6 digits"})
		return false, true
	}

	codeKey := fmt.Sprintf("mfa_email_login:%s", claims.ID)
	stored, err := h.redis.Get(ctx, codeKey).Result()
	if errors.Is(err, redis.Nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No email code pending. Request a new one."})
		return false, true
	}
	if err != nil {
		h.verifyDependencyFailed(c, claims.UserID, err)
		return false, true
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(stored)) != 1 {
		return false, false
	}
	// The code is bound to this challenge, and completing it claims the
	// challenge, so a code left behind by a failed delete cannot be spent twice.
	h.delBestEffort(ctx, "Failed to clear MFA email code", claims.UserID, codeKey, fmt.Sprintf("mfa_email_sent:%s", claims.ID))
	return true, false
}

// isValidEmailCode checks that a string is exactly 6 ASCII digits.
func isValidEmailCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, ch := range code {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// completeVerifyPurpose performs the action associated with the MFA challenge purpose.
func (h *Handler) completeVerifyPurpose(ctx context.Context, c *gin.Context, claims *ChallengeClaims, purpose ChallengePurpose, rememberMe bool) bool {
	switch purpose {
	case PurposeLogin:
		if h.loginCompleter == nil {
			h.log.Error("MFA login verified with no login completer wired", "user_id", claims.UserID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Login completion not configured"})
			return false
		}
		// The key expires with the challenge, which is already claimed.
		h.delBestEffort(ctx, "Failed to clear MFA challenge remember-me state", claims.UserID, auth.MFAChallengeRememberMeKey(claims.ID))
		primaryAuthMethod, err := normalizePrimaryAuthMethod(claims.PrimaryAuthMethod)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid MFA challenge"})
			return false
		}
		return h.loginCompleter.CompleteLogin(c, claims.UserID, rememberMe, claims.CredentialEpoch, primaryAuthMethod)

	case PurposeMFAUpgrade:
		bypassKey := auth.MFAUpgradeBypassKey(claims.UserID, claims.RefreshSessionID)
		if err := h.redis.Set(ctx, bypassKey, "1", mfaUpgradeBypassTTL).Err(); err != nil {
			h.verifyDependencyFailed(c, claims.UserID, err)
			return false
		}
		c.JSON(http.StatusOK, gin.H{"verified": true, "purpose": string(purpose), "user_id": claims.UserID})
		return true

	default:
		c.JSON(http.StatusOK, gin.H{"verified": true, "purpose": string(purpose), "user_id": claims.UserID})
		return true
	}
}

// normalizePrimaryAuthMethod accepts only methods issued by server-authenticated
// paths. Empty is the pre-claim token format and retains password semantics for
// backward compatibility; unknown nonempty claims fail closed rather than
// acquiring SSO provenance.
func normalizePrimaryAuthMethod(method securityevent.AuthMethod) (securityevent.AuthMethod, error) {
	switch method {
	case "", securityevent.AuthPassword:
		return securityevent.AuthPassword, nil
	case securityevent.AuthSSO, securityevent.AuthSession:
		return method, nil
	default:
		return "", errors.New("invalid primary authentication method")
	}
}

// ── Email MFA Code Delivery ──────────────────────────────────────────────────

// SendEmailMFACode sends a 6-digit code to the user's email for MFA verification.
// This is unauthenticated — identity comes from the challenge token.
// Called by the client when the user selects "email" as their MFA method during login.
func (h *Handler) SendEmailMFACode(c *gin.Context) {
	var req struct {
		ChallengeToken string `json:"mfa_challenge_token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mfa_challenge_token is required"})
		return
	}

	// The token encodes its purpose; Verify accepts the same set.
	claims, _, _ := h.parseChallengeToken(req.ChallengeToken)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired challenge token"})
		return
	}

	ctx := c.Request.Context()

	restricted, err := h.recoveryOnlyMethods(ctx, claims.UserID)
	if err != nil {
		h.log.Error("Failed to read recovery-only MFA methods", "user_id", claims.UserID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendCode})
		return
	}
	if containsStr(restricted, "email") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email is set for account recovery only"})
		return
	}

	// Verify user has email MFA enabled
	enabled, err := h.redis.Exists(ctx, fmt.Sprintf(redisKeyEmailSmsEnabledEmail, claims.UserID)).Result()
	if err != nil {
		h.log.Error("Failed to read whether email MFA is enabled", "user_id", claims.UserID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendCode})
		return
	}
	if enabled == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email MFA is not enabled for this account"})
		return
	}
	if !h.emailCodesAllowed() {
		h.log.Error("Email MFA code requested with no email delivery configured", "user_id", claims.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgEmailNotConfigured})
		return
	}

	// Look up user's email
	var userEmail string
	if err := h.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, claims.UserID).Scan(&userEmail); err != nil {
		h.log.Error("Failed to read the account email for an MFA code", "user_id", claims.UserID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendCode})
		return
	}

	// Generate code
	code, err := generateNumericCode(6)
	if err != nil {
		h.log.Error("Failed to generate an MFA email code", "user_id", claims.UserID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate code"})
		return
	}

	if !h.sendEmailCodeOnce(ctx, c, claims, userEmail, code) {
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":    "Verification code sent to your email",
		"expires_in": int(emailCodeTTL.Seconds()),
	})
}

// sendEmailCodeOnce sends at most one code per challenge and stores it for
// verification. It claims the challenge's send slot atomically before sending,
// so an unreadable limit or two concurrent requests cannot send twice, and it
// gives the slot back when the send or the store fails so the user can ask
// again. It reports whether it succeeded; on failure it has answered c.
func (h *Handler) sendEmailCodeOnce(ctx context.Context, c *gin.Context, claims *ChallengeClaims, userEmail, code string) bool {
	sentKey := fmt.Sprintf("mfa_email_sent:%s", claims.ID)
	claimed, err := h.redis.SetNX(ctx, sentKey, "1", emailCodeTTL).Result()
	if err != nil {
		h.log.Error("Failed to claim the MFA email send limit", "user_id", claims.UserID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendCode})
		return false
	}
	if !claimed {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Email code already sent. Check your inbox or wait for it to expire."})
		return false
	}

	// Store before sending, so a store failure sends nothing the user cannot use.
	codeKey := fmt.Sprintf("mfa_email_login:%s", claims.ID)
	if err := h.redis.Set(ctx, codeKey, code, emailCodeTTL).Err(); err != nil {
		h.log.Error("Failed to store MFA email code in Redis", "user_id", claims.UserID, "error", err)
		h.releaseEmailSend(ctx, claims.UserID, sentKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedSendCode})
		return false
	}

	if h.emailSvc == nil {
		// Development and tests only; SendEmailMFACode refused everywhere else.
		h.log.Info("DEV MODE — MFA email code", "user_id", claims.UserID, "code", code)
		return true
	}
	if err := h.emailSvc.SendVerificationCode(userEmail, code); err != nil {
		h.log.Error("Failed to send MFA email code", "error", err, "user_id", claims.UserID)
		h.releaseEmailSend(ctx, claims.UserID, sentKey, codeKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send verification email"})
		return false
	}
	return true
}

// releaseEmailSend gives back a challenge's send slot, and discards a code
// that was never sent, so the user can ask again.
func (h *Handler) releaseEmailSend(ctx context.Context, userID string, keys ...string) {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := h.redis.Del(ctx, keys...).Err(); err != nil {
		h.log.Error("Failed to release the MFA email send limit", "user_id", userID, "error", err)
	}
}

// devCodesAllowed reports whether this handler may expose a code it cannot
// deliver, by logging it or returning it in a response, and so whether SMS,
// which has no provider, may be enrolled at all. Only an explicit development
// or test environment may; an unrecognised value is treated as production.
func (h *Handler) devCodesAllowed() bool {
	return h.environment == "development" || h.environment == "test"
}

// emailCodesAllowed reports whether a code may go out by email. The email
// service runs in its own development mode whenever SMTP is unconfigured,
// which is decided by config alone and logs the code instead of sending it, so
// a service that exists does not mean a code is delivered. Outside development
// and tests the code must actually be sent.
func (h *Handler) emailCodesAllowed() bool {
	if h.emailSvc != nil && !h.emailSvc.IsDevMode() {
		return true
	}
	return h.devCodesAllowed()
}

// canSendSetupEmail reports whether an email setup code can go out. Unlike the
// login code, setup has no fallback of its own: it always sends through the
// email service.
func (h *Handler) canSendSetupEmail() bool {
	return h.emailSvc != nil && h.emailCodesAllowed()
}

// ── Inline WebAuthn Verify (for protected operations) ────────────────────────

// errMsgNoInlineSession is finish's 400 when there is no usable ceremony.
const errMsgNoInlineSession = "No verification session found. Start a new verification."

// maxInlineBeginRequestBytes bounds the begin body, which carries one purpose.
const maxInlineBeginRequestBytes = 1 << 10

// errMsgInvalidInlinePurpose is the fixed 400 for a begin body that is not one
// JSON object naming a known purpose. It never echoes what was sent.
const errMsgInvalidInlinePurpose = "A valid verification purpose is required"

// inlineVerifySession is the stored inline ceremony: the WebAuthn session and
// the purpose begin was asked for. SessionData is embedded so its fields stay
// at the top level of the stored JSON; finish takes the purpose from here and
// never from its own request body.
type inlineVerifySession struct {
	webauthn.SessionData
	Purpose stepup.Purpose `json:"purpose"`
}

// bindInlineVerifyBegin applies the bounded strict-JSON body rule (backend.md
// § Gin Conventions) to {"purpose": "<stepup.Purpose>"}: an oversized body is a
// 413 first, and anything but exactly one JSON object naming a known purpose
// is the fixed 400. On refusal it has written the response. Nothing is logged:
// a refused purpose may not reach a log line (observability.md principle 7).
func bindInlineVerifyBegin(c *gin.Context) (stepup.Purpose, bool) {
	var req struct {
		Purpose stepup.Purpose `json:"purpose"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxInlineBeginRequestBytes)
	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Request body too large"})
			return "", false
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidInlinePurpose})
		return "", false
	}
	body, ok := c.Get(gin.BodyBytesKey)
	bodyBytes, bodyIsBytes := body.([]byte)
	if !ok || !bodyIsBytes || !json.Valid(bodyBytes) ||
		!bytes.HasPrefix(bytes.TrimSpace(bodyBytes), []byte("{")) || !req.Purpose.Valid() {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidInlinePurpose})
		return "", false
	}
	return req.Purpose, true
}

// WebAuthnVerifyInlineBegin starts a WebAuthn assertion for MFA verification on
// one protected action, named by the body's purpose. Returns assertion options
// for navigator.credentials.get(). The session and its purpose are stored in
// Redis keyed by user ID, so a second begin replaces the first.
func (h *Handler) WebAuthnVerifyInlineBegin(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	purpose, ok := bindInlineVerifyBegin(c)
	if !ok {
		return
	}

	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
		h.log.Error("Failed to build WebAuthn user", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStartVerification})
		return
	}
	if len(user.WebAuthnCredentials()) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No WebAuthn credentials registered"})
		return
	}

	assertion, session, err := h.webauthn.BeginLogin(user)
	if err != nil {
		h.log.Error("WebAuthn BeginLogin failed for inline verify", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStartVerification})
		return
	}

	sessionJSON, err := json.Marshal(inlineVerifySession{SessionData: *session, Purpose: purpose})
	if err != nil {
		h.log.Error("Failed to encode inline WebAuthn session", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStartVerification})
		return
	}
	// Options handed out without their stored session describe a ceremony
	// the finish step can never complete.
	if err := h.redis.Set(ctx, inlineSessionKey(userID), sessionJSON, 2*time.Minute).Err(); err != nil {
		h.log.Error("Failed to store inline WebAuthn session", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStartVerification})
		return
	}

	c.JSON(http.StatusOK, assertion)
}

// WebAuthnVerifyInlineFinish validates a WebAuthn assertion for protected
// operations. On success, returns a short-lived verification token usable as
// mfa_code only on the action whose purpose begin stored with the session.
func (h *Handler) WebAuthnVerifyInlineFinish(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	// Consume the session atomically (single-use). A GET followed by a DEL
	// whose failure went unchecked left a completed ceremony reusable; GETDEL
	// cannot, and a failure to consume it fails closed.
	sessionJSON, err := h.redis.GetDel(ctx, inlineSessionKey(userID)).Bytes()
	if errors.Is(err, redis.Nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgNoInlineSession})
		return
	}
	if err != nil {
		h.log.Error("Failed to consume inline WebAuthn session", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerify})
		return
	}

	var session inlineVerifySession
	if err := json.Unmarshal(sessionJSON, &session); err != nil {
		h.log.Error("Failed to decode inline WebAuthn session", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInvalidSessionData})
		return
	}
	// A session with no known purpose (one stored before purposes existed)
	// could mint only a token no consumer accepts, so it is treated as absent.
	if !session.Purpose.Valid() {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgNoInlineSession})
		return
	}

	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
		h.log.Error("Failed to build the WebAuthn user for inline verification", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerify})
		return
	}

	credential, err := h.webauthn.FinishLogin(user, session.SessionData, c.Request)
	if err != nil {
		h.log.Warn("WebAuthn inline verify assertion failed", "error", err, "user_id", userID)
		c.JSON(http.StatusForbidden, gin.H{"error": "Verification failed. Try again."})
		return
	}

	// Update sign count. Cloned-authenticator detection compares the next
	// assertion against this value, so a token is not issued unless it landed.
	if _, err := h.db.ExecContext(ctx,
		`UPDATE user_mfa_webauthn SET sign_count = $1, last_used_at = NOW() WHERE credential_id = $2 AND user_id = $3`,
		credential.Authenticator.SignCount, credential.ID, userID,
	); err != nil {
		h.log.Error("Failed to update WebAuthn sign count", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerify})
		return
	}

	// Generate a short-lived verification token (60s, single-use)
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		h.log.Error("Failed to generate an inline WebAuthn token", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	// A token the server never stored is one it will never accept. The key
	// embeds the token, so the error — which a client hook may annotate with
	// the key — is deliberately not logged.
	if err := h.redis.Set(ctx, inlineTokenKey(userID, session.Purpose, token), "1", 60*time.Second).Err(); err != nil {
		h.log.Error("Failed to store inline WebAuthn token", "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"mfa_token": token})
}

// ── WebAuthn Helpers ─────────────────────────────────────────────────────────

func (h *Handler) buildWebAuthnUser(ctx context.Context, userID string) (*WebAuthnUser, error) {
	var email, username, displayName string
	err := h.db.QueryRowContext(ctx,
		`SELECT email, username, COALESCE(display_name, username) FROM users WHERE id = $1`, userID,
	).Scan(&email, &username, &displayName)
	if err != nil {
		return nil, fmt.Errorf("fetch user: %w", err)
	}

	// Load existing credentials for exclusion during registration
	rows, err := h.db.QueryContext(ctx, `
		SELECT credential_id, public_key, aaguid, sign_count, transports
		FROM user_mfa_webauthn WHERE user_id = $1
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("fetch credentials: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	// A row it cannot read is an error, not an omission: the list is the
	// login allow-list and the registration exclusion list, and a key missing
	// from the second can be registered twice.
	var creds []webauthn.Credential
	for rows.Next() {
		var credID, pubKey, aaguid []byte
		var signCount int64
		var transports []string
		if err := rows.Scan(&credID, &pubKey, &aaguid, &signCount, pq.Array(&transports)); err != nil {
			return nil, fmt.Errorf("read credential: %w", err)
		}
		cred := webauthn.Credential{
			ID:        credID,
			PublicKey: pubKey,
			Authenticator: webauthn.Authenticator{
				AAGUID:    aaguid,
				SignCount: uint32(signCount), //nolint:gosec // sign count won't overflow uint32
			},
		}
		for _, t := range transports {
			cred.Transport = append(cred.Transport, protocol.AuthenticatorTransport(t))
		}
		creds = append(creds, cred)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}

	return &WebAuthnUser{
		ID:          []byte(userID),
		Name:        username,
		DisplayName: displayName,
		Credentials: creds,
	}, nil
}

// ── Recovery-Only Methods ─────────────────────────────────────────────────────

// countLoginEligible returns the number of enabled methods not in the recovery-only set.
func countLoginEligible(enabledMethods, recoveryOnlyMethods []string) int {
	excluded := make(map[string]bool, len(recoveryOnlyMethods))
	for _, r := range recoveryOnlyMethods {
		excluded[r] = true
	}
	count := 0
	for _, m := range enabledMethods {
		if !excluded[m] {
			count++
		}
	}
	return count
}

// filterValidRecoveryOnly returns only those requested methods that are actually enabled.
func filterValidRecoveryOnly(requested []string, enabled map[string]bool) []string {
	var valid []string
	for _, m := range requested {
		if enabled[m] {
			valid = append(valid, m)
		}
	}
	return valid
}

// hasEmailOrSms returns true if the slice contains "email" or "sms".
func hasEmailOrSms(methods []string) bool {
	for _, m := range methods {
		if m == "email" || m == "sms" {
			return true
		}
	}
	return false
}

// SetRecoveryOnly updates which MFA methods are restricted to account recovery only.
// Recovery-only methods can verify identity for recovery flows but are excluded from
// login and sensitive-operation MFA challenges — like a spare key that unlocks the
// door but doesn't start the engine.
func (h *Handler) SetRecoveryOnly(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Methods []string `json:"methods"` // e.g. ["email", "sms"] or [] to clear
		mfaStepUpCredentials
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequest})
		return
	}

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, recoveryOnlyStepUp); !ok {
		return
	}

	if req.Methods == nil {
		req.Methods = []string{}
	}

	enabledMethods, err := h.GetEnabledMethods(ctx, userID)
	if err != nil {
		h.log.Error("Failed to read MFA methods for recovery-only update", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch MFA status"})
		return
	}

	if countLoginEligible(enabledMethods, req.Methods) == 0 && len(enabledMethods) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "At least one MFA method must remain eligible for login"})
		return
	}

	enabled := make(map[string]bool, len(enabledMethods))
	for _, m := range enabledMethods {
		enabled[m] = true
	}
	validRecoveryOnly := filterValidRecoveryOnly(req.Methods, enabled)
	// Clearing sends `methods: []`, which filters to a nil slice; pq.Array(nil)
	// binds SQL NULL and the NOT NULL column refuses it. Clearing stores '{}'.
	if validRecoveryOnly == nil {
		validRecoveryOnly = []string{}
	}

	// recovery_hardened comes back from the write itself, so the response can
	// never report a value the row does not hold.
	query := `UPDATE users SET recovery_only_methods = $1 WHERE id = $2 RETURNING recovery_hardened`
	if hasEmailOrSms(validRecoveryOnly) {
		query = `UPDATE users SET recovery_only_methods = $1, recovery_hardened = TRUE WHERE id = $2 RETURNING recovery_hardened`
	}
	var recoveryHardened bool
	if err := h.db.QueryRowContext(ctx, query, pq.Array(validRecoveryOnly), userID).Scan(&recoveryHardened); err != nil {
		h.log.Error("Failed to update recovery_only_methods", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update settings"})
		return
	}

	h.clearStepUpAfterSuccess(c, userID)
	c.JSON(http.StatusOK, gin.H{
		"recovery_only_methods": validRecoveryOnly,
		"recovery_hardened":     recoveryHardened,
	})
}

// SetRecoveryHardened toggles dual-channel hardened mode for Email+SMS recovery.
// When enabled, account recovery requires BOTH an email code AND an SMS code simultaneously.
// An attacker must compromise both channels — neither alone is sufficient.
func (h *Handler) SetRecoveryHardened(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Enabled bool `json:"enabled"`
		mfaStepUpCredentials
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequest})
		return
	}

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, recoveryHardenedStepUp); !ok {
		return
	}

	_, err := h.db.ExecContext(ctx,
		`UPDATE users SET recovery_hardened = $1 WHERE id = $2`,
		req.Enabled, userID,
	)
	if err != nil {
		h.log.Error("Failed to update recovery_hardened", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update settings"})
		return
	}

	h.clearStepUpAfterSuccess(c, userID)
	c.JSON(http.StatusOK, gin.H{
		"recovery_hardened": req.Enabled,
	})
}

// ── Email/SMS MFA ──────────────────────────────────────────────────────────────
//
// Email setup sends codes through the configured email service. SMS remains
// unavailable in production until an SMS provider is wired.

// generateNumericCode creates a cryptographically random N-digit numeric code.
func generateNumericCode(digits int) (string, error) {
	maxVal := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	n, err := rand.Int(rand.Reader, maxVal)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", digits, n), nil
}

// validateEmailSmsMethods checks that all requested methods are valid and available.
// Returns (errorMessage, httpStatusCode). Empty errorMessage means OK.
func (h *Handler) validateEmailSmsMethods(methods []string) (string, int) {
	for _, m := range methods {
		if m == "sms" && !h.devCodesAllowed() {
			return "SMS MFA is not yet available. Requires SMS provider integration.", http.StatusForbidden
		}
		if m != "email" && m != "sms" {
			return "methods must be 'email' and/or 'sms'", http.StatusBadRequest
		}
	}
	return "", 0
}

// generateAndStoreEmailSmsCodes generates codes for each method and stores them in Redis.
func (h *Handler) generateAndStoreEmailSmsCodes(ctx context.Context, userID string, methods []string) (map[string]string, error) {
	codes := make(map[string]string)
	for _, method := range methods {
		code, err := generateNumericCode(6)
		if err != nil {
			return nil, err
		}
		// A code that was never stored can never verify, so it must not be sent.
		key := fmt.Sprintf(redisKeyEmailSmsSetup, userID, method)
		if err := h.redis.Set(ctx, key, code, 10*time.Minute).Err(); err != nil {
			return nil, fmt.Errorf("store %s setup code: %w", method, err)
		}
		codes[method] = code
	}
	return codes, nil
}

func (h *Handler) sendEmailSmsSetupEmail(c *gin.Context, userID string, userEmail string, codes map[string]string) bool {
	code, ok := codes["email"]
	if !ok {
		return true
	}

	if h.emailSvc == nil {
		// EmailSmsSetup refuses this case before storing any code; this is the
		// backstop for any other caller.
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgEmailNotConfigured})
		return false
	}

	if err := h.emailSvc.SendVerificationCode(userEmail, code); err != nil {
		h.log.Error("Failed to send MFA email code", "error", err, "user_id", userID)
		if h.redis != nil {
			ctx := context.Background()
			if c.Request != nil {
				ctx = c.Request.Context()
			}
			ctx, cancel := cleanupContext(ctx)
			defer cancel()
			// Only this request's code: a newer setup request may already have
			// stored and sent its own.
			if err := releaseOwnedScript.Run(ctx, h.redis, []string{fmt.Sprintf(redisKeyEmailSmsSetup, userID, "email")}, code).Err(); err != nil {
				// The unsent code expires with its TTL; nobody holds it.
				h.log.Error("Failed to discard an unsent email setup code", "user_id", userID, "error", err)
			}
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send verification email"})
		return false
	}

	return true
}

// EmailSmsSetup enables email and/or SMS as MFA methods.
// Requires password + MFA (if active). Email codes are delivered via the email service;
// SMS is still dev-only (requires Twilio integration).
func (h *Handler) EmailSmsSetup(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		mfaStepUpCredentials
		Methods []string `json:"methods" binding:"required"` // ["email"], ["sms"], or ["email", "sms"]
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "methods are required"})
		return
	}

	if errMsg, status := h.validateEmailSmsMethods(req.Methods); errMsg != "" {
		c.JSON(status, gin.H{"error": errMsg})
		return
	}

	subj, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, emailSmsSetupStepUp)
	if !ok {
		return
	}

	// The step-up's own P1 read answers "is there a Standard factor?" — the
	// same predicate, already read, with no second lookup to fail open.
	if !subj.MFAEnabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enable a Standard or higher MFA method first"})
		return
	}

	for _, method := range req.Methods {
		if method == "email" && !h.canSendSetupEmail() {
			h.log.Error("Email MFA setup requested with no email delivery configured")
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgEmailNotConfigured})
			return
		}
	}

	var userEmail string
	if err := h.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&userEmail); err != nil {
		h.log.Error("Failed to read the account email for email/SMS setup", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to look up account email"})
		return
	}

	codes, err := h.generateAndStoreEmailSmsCodes(ctx, userID, req.Methods)
	if err != nil {
		h.log.Error("Failed to prepare email/SMS setup codes", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare verification codes"})
		return
	}

	if !h.sendEmailSmsSetupEmail(c, userID, userEmail, codes) {
		return
	}

	h.clearStepUpAfterSuccess(c, userID)
	resp := gin.H{
		"message":    "Verification codes sent",
		"methods":    req.Methods,
		"expires_in": "10 minutes",
	}
	if h.devCodesAllowed() {
		if smsCode, ok := codes["sms"]; ok {
			resp["dev_codes"] = map[string]string{"sms": smsCode}
			resp["dev_mode"] = true
		}
	}
	c.JSON(http.StatusOK, resp)
}

// ValidateHardenedModeCodes checks that hardened mode requirements are met.
func ValidateHardenedModeCodes(codes map[string]string) string {
	if _, hasEmail := codes["email"]; !hasEmail {
		return "Hardened mode requires both email and SMS codes"
	}
	if _, hasSms := codes["sms"]; !hasSms {
		return "Hardened mode requires both email and SMS codes"
	}
	return ""
}

// verifyEmailSmsCodes validates each code against Redis and returns the verified methods.
// Returns (verified, errorMessage, httpStatus).
func (h *Handler) verifyEmailSmsCodes(ctx context.Context, userID string, codes map[string]string) ([]string, string, int) {
	verified := []string{}
	for method, code := range codes {
		if method != "email" && method != "sms" {
			return nil, fmt.Sprintf("invalid method: %s", method), http.StatusBadRequest
		}

		key := fmt.Sprintf(redisKeyEmailSmsSetup, userID, method)
		stored, err := h.redis.Get(ctx, key).Result()
		if errors.Is(err, redis.Nil) {
			return nil, fmt.Sprintf("No pending %s code. Request a new one.", method), http.StatusBadRequest
		}
		if err != nil {
			h.log.Error("Failed to read pending MFA setup code", "user_id", userID, "method", method, "error", err)
			return nil, "Failed to verify MFA settings", http.StatusInternalServerError
		}

		if subtle.ConstantTimeCompare([]byte(code), []byte(stored)) != 1 {
			return nil, fmt.Sprintf("Invalid %s code", method), http.StatusForbidden
		}

		verified = append(verified, method)
	}
	return verified, "", 0
}

// EmailSmsVerify verifies the email/SMS code(s) and activates the method(s).
// In hardened mode, both email AND sms codes must be provided and correct.
func (h *Handler) EmailSmsVerify(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Codes map[string]string `json:"codes" binding:"required"` // {"email": "123456"} or {"email": "...", "sms": "..."}
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "codes map is required"})
		return
	}

	if len(req.Codes) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "At least one code is required"})
		return
	}

	// Fail closed: a discarded error here would leave recoveryHardened at its
	// false zero value and silently skip the hardened-mode dual-code
	// requirement below, defeating a security control on an infrastructure
	// fault. Refusing the request is the correct posture — assuming hardened
	// instead would reject legitimate non-hardened users with a confusing 400.
	var recoveryHardened bool
	if err := h.db.QueryRowContext(ctx,
		`SELECT recovery_hardened FROM users WHERE id = $1`, userID,
	).Scan(&recoveryHardened); err != nil {
		// A missing users row is routine and client-side (a deleted account
		// holding a still-valid JWT — AuthRequired's live check is Redis-only),
		// so it is a 401, not a 5xx. Still fail closed: the request is refused.
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": stepup.ErrMsgSessionNoLongerValid})
			return
		}
		h.log.Error("Failed to read recovery_hardened for MFA verify", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify MFA settings"})
		return
	}

	if recoveryHardened {
		if errMsg := ValidateHardenedModeCodes(req.Codes); errMsg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": errMsg})
			return
		}
	}

	verified, errMsg, status := h.verifyEmailSmsCodes(ctx, userID, req.Codes)
	if errMsg != "" {
		c.JSON(status, gin.H{"error": errMsg})
		return
	}

	// Extracted to keep EmailSmsVerify under the cognitive-complexity budget
	// (go:S3776). The sequencing rationale lives on the helper.
	if err := h.activateEmailSmsMethods(ctx, userID, middleware.TokenSessionID(c), req.Codes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to activate MFA methods"})
		return
	}
	h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorEnabled})

	c.JSON(http.StatusOK, gin.H{
		"message":  "MFA methods activated",
		"verified": verified,
	})
}

// activateEmailSmsMethods commits the verified email/SMS methods and then clears
// their pending setup codes.
//
// go-redis returns *StatusCmd/*IntCmd rather than an error, so errcheck is
// structurally blind here: a dropped Set would leave the method inactive while
// the handler still answered 200, and the flag sync — which reads this exact
// key back to derive mfa_methods — could then write mfa_enabled = FALSE.
//
// Two-phase: activate every method and commit the durable flags BEFORE deleting
// any pending setup code. A Del is irreversible, so deleting mid-loop makes a
// later failure unretryable — with a hardened account's email+sms pair, a failed
// sms Set after a successful email Set+Del would fail the request while the
// caller's resubmitted (still valid) email code now hits a key we already
// removed, answering "No pending email code". Found by CodeRabbit + Codex on
// PR #2654.
//
// The flag write is fatal: mfa_enabled_at is load-bearing (pre-existing
// sessions are challenged based on it), so a failed write would leave those
// sessions silently never challenged while the user is told MFA is on. The
// state is re-read after the Sets; if that read fails, the just-activated
// methods are still known to be on and are written, and the row's other
// email/SMS entries are carried forward.
//
// A failure part-way must not leave a method on in Redis that the flags do not
// list, so every method this call turns on is taken back if anything after it
// fails. SET ... GET reports whether the key was already there, so a method
// that was on before this call is left alone. The pending codes stay, so a
// retry activates them again. Each key holds this request's own value, which
// the undo compares before deleting: a retry of the same code overwrites it,
// so the undo of a timed-out earlier attempt cannot switch off a method the
// retry reported on.
//
// This request checked the code, so the session activating the method is the
// one that proved it, and a first activation exempts that session as TOTP and
// WebAuthn do.
func (h *Handler) activateEmailSmsMethods(ctx context.Context, userID, sessionID string, codes map[string]string) error {
	firstActivation := h.mfaNeverEnabled(ctx, userID)
	owner := uuid.NewString()
	verified := make([]string, 0, len(codes))
	var added []string
	for method := range codes {
		verified = append(verified, method)
		key := fmt.Sprintf(redisKeyEmailSmsEnabled, userID, method)
		err := h.redis.SetArgs(ctx, key, owner, redis.SetArgs{Get: true}).Err()
		if errors.Is(err, redis.Nil) {
			added = append(added, key)
			continue
		}
		if err != nil {
			h.log.Error("Failed to persist MFA method activation", "method", method, "error", err)
			h.withdrawEmailSmsMethods(ctx, userID, owner, added)
			return err
		}
	}

	es := h.readEmailSmsForSync(ctx, userID).withKnownOn(verified)
	if err := h.withMFAFactorWriteTx(ctx, userID, es, nil); err != nil {
		h.log.Error("Failed to update MFA flags after email/sms enable", "error", err)
		h.withdrawEmailSmsMethods(ctx, userID, owner, added)
		return err
	}
	if firstActivation {
		h.grantEnrollmentUpgrade(ctx, userID, sessionID)
	}

	// Best-effort cleanup, and only now that the activation is durable. The
	// asymmetry with the Set above is deliberate: a failed Set must fail the
	// request (the method is NOT active), but a failed delete may log-and-continue —
	// the method genuinely IS active and the stale code expires by its own TTL.
	// Only the code this request used goes, so a newer setup's code survives.
	for method, code := range codes {
		if err := releaseOwnedScript.Run(ctx, h.redis, []string{fmt.Sprintf(redisKeyEmailSmsSetup, userID, method)}, code).Err(); err != nil {
			h.log.Error("Failed to clear pending MFA setup code", "method", method, "error", err)
		}
	}

	return nil
}

// withdrawEmailSmsMethods turns back off the email/SMS methods an activation
// turned on before it failed, then re-syncs the flags. The re-sync is what
// makes the flags match Redis whatever the failure left behind: a flags write
// that reported failure may have committed, and a write whose reply was lost
// may have turned a method on without the caller knowing. A key a newer
// request has written since holds that request's value and is left alone.
func (h *Handler) withdrawEmailSmsMethods(ctx context.Context, userID, owner string, keys []string) {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	if len(keys) > 0 {
		if err := releaseOwnedScript.Run(ctx, h.redis, keys, owner).Err(); err != nil {
			h.log.Error("Failed to withdraw a partly activated email/SMS method", "error", err)
		}
	}
	if err := h.withMFAFactorWriteTx(ctx, userID, h.readEmailSmsForSync(ctx, userID), nil); err != nil {
		h.log.Error("Failed to re-sync MFA flags after a failed email/SMS activation", "error", err)
	}
}

// cleanupContext returns a context for undoing a partial write. It outlives the
// request, since a client that disconnects mid-request cancels exactly the
// requests whose cleanup matters, and is bounded so a stalled dependency cannot
// hold the goroutine.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// settingsStepUp is one MFA-settings route's step-up: the purpose a WebAuthn
// inline token must have been minted for, and the route's refusal copy. They
// are declared together so a route cannot take one without the other, and
// each route has its own purpose (see stepup.Purpose).
type settingsStepUp struct {
	purpose stepup.Purpose
	wording stepup.Copy
}

// newSettingsStepUp builds a route's step-up. NoFactors is reachable only if a
// password hash is ever empty (see internal/stepup); prompt is what a request
// that sent no password is told, and must be honest to a renderer that shows
// it verbatim.
func newSettingsStepUp(purpose stepup.Purpose, action, prompt string) settingsStepUp {
	return settingsStepUp{
		purpose: purpose,
		wording: stepup.Copy{
			NoFactors:          action + " requires proving your identity, but this account has no password and no MFA method.",
			CredentialRequired: prompt,
		},
	}
}

// Per-route step-ups for the MFA-settings gate (settings_stepup.go). A
// renderer that predates the in-transaction gate never asks for a password,
// so the two of its routes it still drives carry an update hint. The nine
// pool-side routes always required a password, so an old renderer already
// sends one and their copy needs no hint.
var (
	emailSmsDisableStepUp = newSettingsStepUp(stepup.PurposeEmailSmsDisable, "Turning off email verification",
		"Enter your password to turn off email verification. If you aren't asked for it, update Concord Voice.")
	backupEmailStepUp = newSettingsStepUp(stepup.PurposeBackupEmailSet, "Changing your backup email",
		"Enter your password to change your backup email. If you aren't asked for it, update Concord Voice.")
	recoveryKeyReplaceStepUp = newSettingsStepUp(stepup.PurposeRecoveryKeyReplace, "Replacing your recovery key",
		"Enter your password to replace your recovery key.")
	recoveryKeyRemoveStepUp = newSettingsStepUp(stepup.PurposeRecoveryKeyRemove, "Removing your recovery key",
		"Enter your password to remove your recovery key.")

	totpSetupStepUp = newSettingsStepUp(stepup.PurposeTOTPSetup, "Setting up an authenticator app",
		"Enter your password to set up an authenticator app.")
	webAuthnRegisterStepUp = newSettingsStepUp(stepup.PurposeWebAuthnRegister, "Adding a security key",
		"Enter your password to add a security key.")
	recoveryOnlyStepUp = newSettingsStepUp(stepup.PurposeRecoveryOnlySet, "Changing your recovery-only methods",
		"Enter your password to change which methods are for recovery only.")
	recoveryHardenedStepUp = newSettingsStepUp(stepup.PurposeRecoveryHardenedSet, "Changing hardened recovery",
		"Enter your password to change hardened recovery.")
	emailSmsSetupStepUp = newSettingsStepUp(stepup.PurposeEmailSmsSetup, "Turning on email or text-message codes",
		"Enter your password to turn on email or text-message codes.")
	designateTrustedDeviceStepUp = newSettingsStepUp(stepup.PurposeTrustedDeviceDesignate, "Trusting this device for recovery",
		"Enter your password to trust this device for account recovery.")
	removeTrustedDeviceStepUp = newSettingsStepUp(stepup.PurposeTrustedDeviceRemove, "Removing a trusted device",
		"Enter your password to remove a trusted recovery device.")
	upsertRecoveryCircleStepUp = newSettingsStepUp(stepup.PurposeRecoveryCircleUpsert, "Setting up your recovery circle",
		"Enter your password to set up your recovery circle.")
	deleteRecoveryCircleStepUp = newSettingsStepUp(stepup.PurposeRecoveryCircleDelete, "Deleting your recovery circle",
		"Enter your password to delete your recovery circle.")
)

// EmailSmsDisable removes email and/or SMS MFA methods. Turning off a factor is
// gated by the in-transaction step-up (settings_stepup.go); the users row is
// locked FOR NO KEY UPDATE because this transaction writes it.
func (h *Handler) EmailSmsDisable(c *gin.Context) {
	userID := c.GetString("user_id")
	var creds mfaStepUpCredentials
	if err := bindOptionalJSON(c, &creds); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequest})
		return
	}
	tx, subj, ok := h.openMFASettingsTx(c, userID, creds, lockForNoKeyUpdate)
	if !ok {
		return
	}
	defer h.rollbackQuietly(tx)
	ctx := c.Request.Context()
	if e, stage := h.verifyMFASettingsStepUpTx(ctx, tx, userID, subj, creds, emailSmsDisableStepUp); e != nil {
		h.refuseMFASettingsStepUp(c, e, stage)
		return
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET recovery_only_methods = array_remove(array_remove(recovery_only_methods, 'email'), 'sms') WHERE id = $1`,
		userID,
	); err != nil {
		h.log.Error("Failed to clear email/SMS from recovery-only methods", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDisableEmailSms})
		return
	}
	// The new state is known exactly — both off — and subj.MFAMethods is the
	// inline set LockSubjectTx read under this transaction's lock, so the
	// flags are written here, in the same transaction, with no Redis read.
	if err := writeMFAFlagsTx(ctx, tx, userID, subj.MFAMethods, emailSmsState{known: true}); err != nil {
		h.log.Error("Failed to write MFA flags for email/SMS disable", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDisableEmailSms})
		return
	}
	// Redis last, while the lock is still held: on failure the deferred
	// rollback undoes both users writes and nothing has changed. All four keys
	// go in one command so a transport failure cannot leave a partial disable.
	//
	// This is NOT atomic across the two stores: a commit that fails AFTER the
	// Del leaves email/SMS off in Redis while the row still lists them. That
	// is the fail-closed direction (the flags over-report MFA, never
	// under-report it), and GetStatus's resync compares email/SMS and repairs
	// the row on the next status load.
	if h.redis == nil {
		h.log.Error("MFA state store is not configured")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "MFA service temporarily unavailable"})
		return
	}
	if err := h.redis.Del(ctx,
		fmt.Sprintf(redisKeyEmailSmsEnabled, userID, "email"),
		fmt.Sprintf(redisKeyEmailSmsEnabled, userID, "sms"),
		fmt.Sprintf(redisKeyEmailSmsSetup, userID, "email"),
		fmt.Sprintf(redisKeyEmailSmsSetup, userID, "sms"),
	).Err(); err != nil {
		h.log.Error("Failed to disable email/SMS MFA methods in Redis", "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "MFA service temporarily unavailable"})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit email/SMS disable", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDisableEmailSms})
		return
	}
	h.clearStepUpAfterSuccess(c, userID)
	h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorDisabled})
	c.JSON(http.StatusOK, gin.H{"message": "Email/SMS MFA methods disabled"})
}

// readEmailSmsEnabled reports the email and SMS MFA flags from Redis. A
// transport error is returned rather than read as "disabled": reporting a
// factor as off when the store could not be read tells the client something
// the server does not know.
func (h *Handler) readEmailSmsEnabled(ctx context.Context, userID string) (emailEnabled, smsEnabled bool, err error) {
	if h.redis == nil {
		return false, false, errors.New("MFA state store is not configured")
	}
	emailCount, err := h.redis.Exists(ctx, fmt.Sprintf(redisKeyEmailSmsEnabledEmail, userID)).Result()
	if err != nil {
		return false, false, fmt.Errorf("read email MFA state: %w", err)
	}
	smsCount, err := h.redis.Exists(ctx, fmt.Sprintf(redisKeyEmailSmsEnabled, userID, "sms")).Result()
	if err != nil {
		return false, false, fmt.Errorf("read SMS MFA state: %w", err)
	}
	return emailCount > 0, smsCount > 0, nil
}

// EmailSmsStatus returns whether email/sms methods are enabled.
func (h *Handler) EmailSmsStatus(c *gin.Context) {
	userID := c.GetString("user_id")

	emailEnabled, smsEnabled, err := h.readEmailSmsEnabled(c.Request.Context(), userID)
	if err != nil {
		h.failMFAStatus(c, "email_sms", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"email_enabled": emailEnabled,
		"sms_enabled":   smsEnabled,
	})
}

// ── Backup Email ─────────────────────────────────────────────────────────────

// GetBackupEmail returns the user's backup email.
func (h *Handler) GetBackupEmail(c *gin.Context) {
	userID := c.GetString("user_id")
	var backupEmail sql.NullString
	if err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT backup_email FROM users WHERE id = $1`, userID,
	).Scan(&backupEmail); err != nil {
		// A missing users row is a deleted account holding a still-valid JWT
		// (the EmailSmsVerify precedent): client-side, so 401, not 5xx.
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": stepup.ErrMsgSessionNoLongerValid})
			return
		}
		h.log.Error("Failed to load backup email", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load backup email"})
		return
	}
	email := ""
	if backupEmail.Valid {
		email = backupEmail.String
	}
	c.JSON(http.StatusOK, gin.H{"backup_email": email})
}

// SetBackupEmail sets or clears the user's backup email for recovery. Both are
// gated by the in-transaction step-up (settings_stepup.go) on the requested
// action; the users row is locked FOR NO KEY UPDATE because this transaction
// writes it.
func (h *Handler) SetBackupEmail(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct {
		Email string `json:"email"`
		mfaStepUpCredentials
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequest})
		return
	}

	// Basic email validation (or allow empty to clear). Shape errors are
	// answered before the budget and the transaction, so they consume nothing.
	if req.Email != "" && !isValidEmail(req.Email) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid email address"})
		return
	}

	tx, subj, ok := h.openMFASettingsTx(c, userID, req.mfaStepUpCredentials, lockForNoKeyUpdate)
	if !ok {
		return
	}
	defer h.rollbackQuietly(tx)
	ctx := c.Request.Context()
	// Gated on the requested action, set or clear alike; the prior value is
	// never read.
	if e, stage := h.verifyMFASettingsStepUpTx(ctx, tx, userID, subj, req.mfaStepUpCredentials, backupEmailStepUp); e != nil {
		h.refuseMFASettingsStepUp(c, e, stage)
		return
	}

	var val any
	if req.Email != "" {
		val = req.Email
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET backup_email = $1 WHERE id = $2`, val, userID); err != nil {
		h.log.Error(errMsgFailedUpdateBackupEmail, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateBackupEmail})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit backup email change", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateBackupEmail})
		return
	}
	h.clearStepUpAfterSuccess(c, userID)

	c.JSON(http.StatusOK, gin.H{"backup_email": req.Email})
}

func isValidEmail(email string) bool {
	// Simple validation: has @ with something on both sides
	at := -1
	for i, c := range email {
		if c == '@' {
			if at >= 0 {
				return false // multiple @
			}
			at = i
		}
	}
	return at > 0 && at < len(email)-1
}

// ── Recovery Token Methods ──────────────────────────────────────────────────

// GenerateRecoveryToken creates a recovery-purpose JWT with a 25-hour TTL.
// Implements auth.MFAChecker interface.
func (h *Handler) GenerateRecoveryToken(userID string) (string, string, error) {
	return GenerateRecoveryToken(userID, JWTSecret(h.jwtSecret))
}

// ValidateRecoveryToken validates a recovery-purpose JWT and returns the claims.
// Implements auth.MFAChecker interface.
func (h *Handler) ValidateRecoveryToken(tokenString string) (*auth.RecoveryClaims, error) {
	claims, err := ValidateChallengeToken(tokenString, h.jwtSecret, PurposeRecovery)
	if err != nil {
		return nil, err
	}
	return &auth.RecoveryClaims{UserID: claims.UserID, JTI: claims.ID}, nil
}

// ── Recovery Key Endpoints ──────────────────────────────────────────────────

// StoreRecoveryKey stores the user's recovery-wrapped private key. A first store
// is token-only; replacing an existing key requires the step-up (B1, see
// storeRecoveryKey).
func (h *Handler) StoreRecoveryKey(c *gin.Context) {
	userID := c.GetString("user_id")

	var req struct {
		RecoveryWrappedPrivateKey string `json:"recovery_wrapped_private_key" binding:"required"`
		RecoveryKeySalt           string `json:"recovery_key_salt" binding:"required"`
		RecoveryWrappedPrefsKey   string `json:"recovery_wrapped_prefs_key"`
		RecoveryPrefsKeySalt      string `json:"recovery_prefs_key_salt"`
		mfaStepUpCredentials
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recovery_wrapped_private_key and recovery_key_salt are required"})
		return
	}

	// Validate prefs fields are provided together (both or neither)
	hasPrefsKey := req.RecoveryWrappedPrefsKey != ""
	hasPrefsSalt := req.RecoveryPrefsKeySalt != ""
	if hasPrefsKey != hasPrefsSalt {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recovery_wrapped_prefs_key and recovery_prefs_key_salt must be provided together"})
		return
	}

	// Base64-decode all fields
	wrappedKey, err := base64.StdEncoding.DecodeString(req.RecoveryWrappedPrivateKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid recovery_wrapped_private_key format (must be base64)"})
		return
	}
	keySalt, err := base64.StdEncoding.DecodeString(req.RecoveryKeySalt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid recovery_key_salt format (must be base64)"})
		return
	}

	var wrappedPrefsKey, prefsKeySalt []byte
	if req.RecoveryWrappedPrefsKey != "" {
		wrappedPrefsKey, err = base64.StdEncoding.DecodeString(req.RecoveryWrappedPrefsKey)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid recovery_wrapped_prefs_key format (must be base64)"})
			return
		}
	}
	if req.RecoveryPrefsKeySalt != "" {
		prefsKeySalt, err = base64.StdEncoding.DecodeString(req.RecoveryPrefsKeySalt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid recovery_prefs_key_salt format (must be base64)"})
			return
		}
	}

	h.storeRecoveryKey(c, userID, req.mfaStepUpCredentials, recoveryKeyMaterial{
		wrappedKey:      wrappedKey,
		keySalt:         keySalt,
		wrappedPrefsKey: wrappedPrefsKey,
		prefsKeySalt:    prefsKeySalt,
	})
}

// recoveryKeyMaterial is the decoded body of PUT /mfa/recovery-key.
type recoveryKeyMaterial struct {
	wrappedKey, keySalt, wrappedPrefsKey, prefsKeySalt []byte
}

// storeRecoveryKey is StoreRecoveryKey's write half (B1). The users row is
// locked FOR SHARE — this transaction never writes users — and the unique
// index alone decides whether a key already exists, so there is no read of
// absence to lose a race against. A first store (TOTP enrollment's automatic
// upload) needs no credentials; an overwrite, which can do everything
// DeleteRecoveryKey can, needs the same step-up. It always writes the response.
func (h *Handler) storeRecoveryKey(c *gin.Context, userID string, creds mfaStepUpCredentials, m recoveryKeyMaterial) {
	tx, subj, ok := h.openMFASettingsTx(c, userID, creds, lockForShare)
	if !ok {
		return
	}
	defer h.rollbackQuietly(tx)
	ctx := c.Request.Context()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO user_recovery_keys (user_id, recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO NOTHING
	`, userID, m.wrappedKey, m.keySalt, m.wrappedPrefsKey, m.prefsKeySalt)
	if err != nil {
		h.log.Error(errMsgFailedStoreRecoveryKey, "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreRecoveryKey})
		return
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		h.log.Error("Failed to read recovery key insert count", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreRecoveryKey})
		return
	}
	// Exactly one inserted row is the only token-only outcome; anything else
	// takes the step-up, so an unexpected count fails toward more checking.
	replacing := inserted != 1
	if replacing && !creds.sent() && h.answerRepeatedFirstStoreTx(c, tx, userID, m) {
		return
	}
	if replacing && !h.replaceRecoveryKeyTx(c, tx, userID, subj, creds, m) {
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit recovery key", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreRecoveryKey})
		return
	}
	if replacing {
		// Only a VERIFIED commit clears the budget. A first-time insert
		// verified nothing, even when credentials were sent and charged.
		h.clearStepUpAfterSuccess(c, userID)
	}

	h.log.Info(msgRecoveryKeyStored, "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"message": msgRecoveryKeyStored})
}

// answerRepeatedFirstStoreTx makes the token-only first store idempotent (F2).
// A client whose first-store response was lost after the commit retries with
// the SAME bytes and no credentials; without this, the retry met the existing
// row, fell to the step-up, and was refused — and the client read that refusal
// as "the key you are holding was kept", while it had in fact been stored.
//
// It reads the stored row FOR SHARE (lock order users → user_recovery_keys,
// e2ee.md) and answers 200 — the existing success body, nothing written, the
// budget untouched because nothing was verified — only when EVERY column is
// byte-identical to the submission. Any difference, and a row that has
// vanished since the insert conflicted, returns false so the caller takes the
// ordinary step-up path. A read failure is a 500 and also returns true.
//
// What this grants a bearer without credentials: confirmation that bytes they
// already hold equal the stored ciphertext, and nothing else. The stored row
// is never returned, nothing is written, and matching requires the whole
// wrapped key.
func (h *Handler) answerRepeatedFirstStoreTx(c *gin.Context, tx *sql.Tx, userID string, m recoveryKeyMaterial) bool {
	same, err := recoveryKeyMatchesTx(c.Request.Context(), tx, userID, m)
	if err != nil {
		h.log.Error("Failed to read the stored recovery key for comparison", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreRecoveryKey})
		return true
	}
	if !same {
		return false
	}
	h.log.Info("Recovery key store repeated an identical key", "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"message": msgRecoveryKeyStored})
	return true
}

// recoveryKeyMatchesTx reports whether the stored recovery key is
// byte-identical, column by column, to m. No row is (false, nil).
func recoveryKeyMatchesTx(ctx context.Context, tx *sql.Tx, userID string, m recoveryKeyMaterial) (bool, error) {
	var stored recoveryKeyMaterial
	err := tx.QueryRowContext(ctx, `
		SELECT recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt
		FROM user_recovery_keys WHERE user_id = $1 FOR SHARE
	`, userID).Scan(&stored.wrappedKey, &stored.keySalt, &stored.wrappedPrefsKey, &stored.prefsKeySalt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read stored recovery key: %w", err)
	}
	return stored.sameAs(m), nil
}

// sameAs compares every column in constant time for equal lengths, and
// evaluates all four before answering so the result does not reveal which
// column differed.
func (m recoveryKeyMaterial) sameAs(o recoveryKeyMaterial) bool {
	same := sameRecoveryColumn(m.wrappedKey, o.wrappedKey) &
		sameRecoveryColumn(m.keySalt, o.keySalt) &
		sameRecoveryColumn(m.wrappedPrefsKey, o.wrappedPrefsKey) &
		sameRecoveryColumn(m.prefsKeySalt, o.prefsKeySalt)
	return same == 1
}

// sameRecoveryColumn returns 1 when a and b are equal, else 0. NULL (a nil
// slice, which is how both the driver scans a NULL bytea and the handler
// represents an absent optional field) matches only NULL; an empty non-NULL
// value never matches an absent one. A length difference is 0 before any byte
// is compared.
func sameRecoveryColumn(a, b []byte) int {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return 0
	}
	return subtle.ConstantTimeCompare(a, b)
}

// replaceRecoveryKeyTx is B1's overwrite arm: a key already exists, so the
// step-up must pass before anything is written. It is INSERT … DO UPDATE, not
// a plain UPDATE, so a delete that commits between the insert above and this
// statement still leaves the caller's key stored. On refusal or failure it has
// written the response and returns false.
func (h *Handler) replaceRecoveryKeyTx(
	c *gin.Context, tx *sql.Tx, userID string, subj stepup.Subject, creds mfaStepUpCredentials, m recoveryKeyMaterial,
) bool {
	ctx := c.Request.Context()
	if e, stage := h.verifyMFASettingsStepUpTx(ctx, tx, userID, subj, creds, recoveryKeyReplaceStepUp); e != nil {
		h.refuseMFASettingsStepUp(c, e, stage)
		return false
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_recovery_keys (user_id, recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO UPDATE SET
			recovery_wrapped_private_key = EXCLUDED.recovery_wrapped_private_key,
			recovery_key_salt = EXCLUDED.recovery_key_salt,
			recovery_wrapped_prefs_key = EXCLUDED.recovery_wrapped_prefs_key,
			recovery_prefs_key_salt = EXCLUDED.recovery_prefs_key_salt,
			updated_at = NOW()
	`, userID, m.wrappedKey, m.keySalt, m.wrappedPrefsKey, m.prefsKeySalt); err != nil {
		h.log.Error("Failed to replace recovery key", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreRecoveryKey})
		return false
	}
	return true
}

// GetRecoveryKeyStatus returns whether the user has a recovery key and when it was created.
func (h *Handler) GetRecoveryKeyStatus(c *gin.Context) {
	userID := c.GetString("user_id")

	var createdAt time.Time
	err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT created_at FROM user_recovery_keys WHERE user_id = $1`, userID,
	).Scan(&createdAt)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No row found — user has no recovery key
			c.JSON(http.StatusOK, gin.H{"has_recovery_key": false})
			return
		}
		h.log.Error("Failed to query recovery key status", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check recovery key status"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"has_recovery_key": true,
		"created_at":       createdAt.Format(time.RFC3339),
	})
}

// DeleteRecoveryKey removes the user's recovery key. It is gated by the
// in-transaction step-up (settings_stepup.go); the users row is locked FOR
// SHARE because this transaction never writes users.
func (h *Handler) DeleteRecoveryKey(c *gin.Context) {
	userID := c.GetString("user_id")
	var creds mfaStepUpCredentials
	if err := bindOptionalJSON(c, &creds); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequest})
		return
	}
	tx, subj, ok := h.openMFASettingsTx(c, userID, creds, lockForShare)
	if !ok {
		return
	}
	defer h.rollbackQuietly(tx)
	ctx := c.Request.Context()
	if e, stage := h.verifyMFASettingsStepUpTx(ctx, tx, userID, subj, creds, recoveryKeyRemoveStepUp); e != nil {
		h.refuseMFASettingsStepUp(c, e, stage)
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_recovery_keys WHERE user_id = $1`, userID); err != nil {
		h.log.Error("Failed to delete recovery key", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove recovery key"})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit recovery key removal", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove recovery key"})
		return
	}
	h.clearStepUpAfterSuccess(c, userID)

	h.log.Info("Recovery key removed", "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"message": "Recovery key removed"})
}

// ── Trusted Device Recovery Endpoints ───────────────────────────────────────

// ListTrustedDevices returns all trusted recovery devices for the authenticated user.
func (h *Handler) ListTrustedDevices(c *gin.Context) {
	userID := c.GetString("user_id")

	rows, err := h.db.QueryContext(c.Request.Context(),
		`SELECT id, device_name, machine_id, designated_at, last_seen_at
		 FROM trusted_recovery_devices WHERE user_id = $1
		 ORDER BY designated_at DESC`, userID,
	)
	if err != nil {
		h.log.Error(errMsgFailedListDevices, "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedListDevices})
		return
	}
	defer func() { _ = rows.Close() }()

	type trustedDevice struct {
		ID           string     `json:"id"`
		DeviceName   string     `json:"device_name"`
		MachineID    string     `json:"machine_id"`
		DesignatedAt time.Time  `json:"designated_at"`
		LastSeenAt   *time.Time `json:"last_seen_at"`
	}

	devices := []trustedDevice{}
	var scanErr error
	for rows.Next() {
		var d trustedDevice
		if err := rows.Scan(&d.ID, &d.DeviceName, &d.MachineID, &d.DesignatedAt, &d.LastSeenAt); err != nil {
			h.log.Error("Failed to scan trusted device row", "user_id", userID, "error", err)
			scanErr = err
			break
		}
		devices = append(devices, d)
	}
	if scanErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedListDevices})
		return
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating trusted devices", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedListDevices})
		return
	}

	c.JSON(http.StatusOK, gin.H{"devices": devices})
}

// DesignateTrustedDevice designates the current device as a trusted recovery device.
func (h *Handler) DesignateTrustedDevice(c *gin.Context) {
	userID := c.GetString("user_id")

	var req struct {
		mfaStepUpCredentials
		DeviceName string `json:"device_name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_name is required"})
		return
	}

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, designateTrustedDeviceStepUp); !ok {
		return
	}

	machineID := c.GetHeader("X-Machine-Id")
	if machineID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Machine ID required"})
		return
	}

	var deviceID string
	var designatedAt time.Time
	err := h.db.QueryRowContext(c.Request.Context(), `
		INSERT INTO trusted_recovery_devices (user_id, device_name, machine_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, machine_id) DO UPDATE SET
			device_name = EXCLUDED.device_name,
			designated_at = NOW()
		RETURNING id, designated_at
	`, userID, req.DeviceName, machineID).Scan(&deviceID, &designatedAt)
	if err != nil {
		h.log.Error("Failed to designate trusted device", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to designate trusted device"})
		return
	}

	h.log.Info("Trusted device designated", "user_id", userID, "device_id", deviceID, "machine_id", machineID)
	h.clearStepUpAfterSuccess(c, userID)
	c.JSON(http.StatusOK, gin.H{
		"id":            deviceID,
		"device_name":   req.DeviceName,
		"machine_id":    machineID,
		"designated_at": designatedAt.Format(time.RFC3339),
	})
}

// RemoveTrustedDevice removes a trusted recovery device after verifying password and MFA.
func (h *Handler) RemoveTrustedDevice(c *gin.Context) {
	userID := c.GetString("user_id")
	deviceID := c.Param("id")

	var req struct {
		mfaStepUpCredentials
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequest})
		return
	}

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, removeTrustedDeviceStepUp); !ok {
		return
	}

	// An id that is not a UUID names no device; without this the database's
	// parse error would answer 500 for a request that simply matches nothing.
	if _, err := uuid.Parse(deviceID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Trusted device not found"})
		return
	}

	result, err := h.db.ExecContext(c.Request.Context(),
		`DELETE FROM trusted_recovery_devices WHERE id = $1 AND user_id = $2`, deviceID, userID,
	)
	if err != nil {
		h.log.Error(errMsgFailedRemoveDevice, "error", err, "user_id", userID, "device_id", deviceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRemoveDevice})
		return
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		h.log.Error("Failed to read the trusted device delete result", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRemoveDevice})
		return
	}
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Trusted device not found"})
		return
	}

	h.log.Info("Trusted device removed", "user_id", userID, "device_id", deviceID)
	h.clearStepUpAfterSuccess(c, userID)
	c.JSON(http.StatusOK, gin.H{"message": "Trusted device removed"})
}

// ListRecoveryRequests returns pending recovery requests for the authenticated user.
func (h *Handler) ListRecoveryRequests(c *gin.Context) {
	userID := c.GetString("user_id")

	rows, err := h.db.QueryContext(c.Request.Context(), `
		SELECT id, status, ephemeral_public_key, created_at, expires_at
		FROM recovery_requests
		WHERE user_id = $1 AND status = 'pending' AND expires_at > NOW()
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		h.log.Error(errMsgFailedListRecoveryReqs, "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedListRecoveryReqs})
		return
	}
	defer func() { _ = rows.Close() }()

	type recoveryRequest struct {
		ID                 string    `json:"id"`
		Status             string    `json:"status"`
		EphemeralPublicKey string    `json:"ephemeral_public_key"`
		CreatedAt          time.Time `json:"created_at"`
		ExpiresAt          time.Time `json:"expires_at"`
	}

	requests := []recoveryRequest{}
	var scanErr error
	for rows.Next() {
		var r recoveryRequest
		var ephPubKey []byte
		if err := rows.Scan(&r.ID, &r.Status, &ephPubKey, &r.CreatedAt, &r.ExpiresAt); err != nil {
			h.log.Error("Failed to scan recovery request row", "user_id", userID, "error", err)
			scanErr = err
			break
		}
		r.EphemeralPublicKey = base64.StdEncoding.EncodeToString(ephPubKey)
		requests = append(requests, r)
	}
	if scanErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedListRecoveryReqs})
		return
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating recovery requests", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedListRecoveryReqs})
		return
	}

	c.JSON(http.StatusOK, gin.H{"requests": requests})
}

// DecodeApprovalPayloads validates and base64-decodes the encrypted_payload and
// responder_public_key required for an approval action.
// Returns (encPayload, respPubKey, errorMessage, httpStatus).
func DecodeApprovalPayloads(encPayloadB64, respPubKeyB64 string) ([]byte, []byte, string, int) {
	if encPayloadB64 == "" || respPubKeyB64 == "" {
		return nil, nil, "encrypted_payload and responder_public_key are required for approval", http.StatusBadRequest
	}
	encPayload, err := base64.StdEncoding.DecodeString(encPayloadB64)
	if err != nil {
		return nil, nil, "Invalid encrypted_payload format (must be base64)", http.StatusBadRequest
	}
	respPubKey, err := base64.StdEncoding.DecodeString(respPubKeyB64)
	if err != nil {
		return nil, nil, "Invalid responder_public_key format (must be base64)", http.StatusBadRequest
	}
	return encPayload, respPubKey, "", 0
}

// executeRecoveryResponse answers a pending request and reports whether it was
// still pending and unexpired. Both checks are part of the write, so two
// concurrent answers cannot both land, the later one overwriting the first,
// and a request that expired after the handler read it cannot be answered.
func (h *Handler) executeRecoveryResponse(ctx context.Context, requestID, action string, encPayload, respPubKey []byte) (bool, error) {
	var result sql.Result
	var err error
	if action == "approve" {
		result, err = h.db.ExecContext(ctx, `
			UPDATE recovery_requests
			SET status = 'approved', encrypted_payload = $1, responder_public_key = $2, responded_at = NOW()
			WHERE id = $3 AND status = 'pending' AND expires_at > NOW()
		`, encPayload, respPubKey, requestID)
	} else {
		result, err = h.db.ExecContext(ctx, `
			UPDATE recovery_requests
			SET status = 'rejected', responded_at = NOW()
			WHERE id = $1 AND status = 'pending' AND expires_at > NOW()
		`, requestID)
	}
	if err != nil {
		return false, fmt.Errorf("write recovery response: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read recovery response result: %w", err)
	}
	return rows == 1, nil
}

// RespondToRecoveryRequest allows the authenticated user to approve or reject a recovery request.
func (h *Handler) RespondToRecoveryRequest(c *gin.Context) {
	userID := c.GetString("user_id")
	requestID := c.Param("id")

	var req struct {
		Action             string `json:"action" binding:"required"`
		EncryptedPayload   string `json:"encrypted_payload"`
		ResponderPublicKey string `json:"responder_public_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "action is required (approve or reject)"})
		return
	}

	if req.Action != "approve" && req.Action != "reject" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be 'approve' or 'reject'"})
		return
	}

	// An id that is not a UUID names no request. Refusing it here keeps the
	// database's parse error out of the fault path below.
	if _, err := uuid.Parse(requestID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Recovery request not found"})
		return
	}

	// Verify the request belongs to this user and is still pending
	var requestUserID, status string
	err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT user_id, status FROM recovery_requests WHERE id = $1 AND expires_at > NOW()`, requestID,
	).Scan(&requestUserID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Recovery request not found"})
		return
	}
	if err != nil {
		h.log.Error("Failed to read recovery request", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRespondRecovery})
		return
	}

	if requestUserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not authorized to respond to this request"})
		return
	}

	if status != "pending" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Request already responded to"})
		return
	}

	var encPayload, respPubKey []byte
	if req.Action == "approve" {
		var errMsg string
		var httpStatus int
		encPayload, respPubKey, errMsg, httpStatus = DecodeApprovalPayloads(req.EncryptedPayload, req.ResponderPublicKey)
		if errMsg != "" {
			c.JSON(httpStatus, gin.H{"error": errMsg})
			return
		}
	}

	ctx := c.Request.Context()
	answered, err := h.executeRecoveryResponse(ctx, requestID, req.Action, encPayload, respPubKey)
	if err != nil {
		h.log.Error("Failed to respond to recovery request", "error", err, "user_id", userID, "request_id", requestID, "action", req.Action)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRespondRecovery})
		return
	}
	if !answered {
		// Another response landed between the read above and this write.
		c.JSON(http.StatusBadRequest, gin.H{"error": "Request already responded to"})
		return
	}

	statusWord := "approved"
	if req.Action == "reject" {
		statusWord = "rejected"
	}
	h.log.Info("Recovery request responded", "user_id", userID, "request_id", requestID, "action", req.Action)
	c.JSON(http.StatusOK, gin.H{"message": "Recovery request " + statusWord})
}

// ── Social Recovery Circle Endpoints ─────────────────────────────────────────

// GetRecoveryCircle returns the user's social recovery circle configuration.
func (h *Handler) GetRecoveryCircle(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var circleID string
	var thresholdK, totalSharesN, shareVersion int
	var createdAt time.Time
	err := h.db.QueryRowContext(ctx,
		`SELECT id, threshold_k, total_shares_n, share_version, created_at
		 FROM recovery_circles WHERE user_id = $1`, userID,
	).Scan(&circleID, &thresholdK, &totalSharesN, &shareVersion, &createdAt)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No circle configured
			c.JSON(http.StatusOK, gin.H{"has_circle": false})
			return
		}
		h.log.Error("Failed to query recovery circle", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedLoadCircle})
		return
	}

	// Query contacts for the current share version
	rows, err := h.db.QueryContext(ctx, `
		SELECT DISTINCT cs.contact_id, u.username, u.display_name
		FROM recovery_circle_shares cs
		JOIN users u ON u.id = cs.contact_id
		WHERE cs.circle_id = $1 AND cs.share_version = $2
	`, circleID, shareVersion)
	if err != nil {
		h.log.Error("Failed to query recovery circle contacts", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedLoadCircle})
		return
	}
	defer func() { _ = rows.Close() }()

	type contact struct {
		UserID      string `json:"user_id"`
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
	}

	contacts := []contact{}
	for rows.Next() {
		var ct contact
		var displayName sql.NullString
		if err := rows.Scan(&ct.UserID, &ct.Username, &displayName); err != nil {
			// A circle shown short one contact misstates who can recover the account.
			h.log.Error("Failed to scan recovery circle contact", "error", err, "user_id", userID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedLoadCircle})
			return
		}
		if displayName.Valid {
			ct.DisplayName = displayName.String
		}
		contacts = append(contacts, ct)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating recovery circle contacts", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedLoadCircle})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"has_circle":     true,
		"threshold_k":    thresholdK,
		"total_shares_n": totalSharesN,
		"share_version":  shareVersion,
		"created_at":     createdAt,
		"contacts":       contacts,
	})
}

// CircleShareEntry is a single share in a recovery circle upsert request.
type CircleShareEntry struct {
	ContactID      string `json:"contact_id" binding:"required"`
	ShareIndex     int    `json:"share_index" binding:"required"`
	EncryptedShare string `json:"encrypted_share" binding:"required"`
}

// CircleDecodedShare holds a share after base64 decoding.
type CircleDecodedShare struct {
	ContactID      string
	ShareIndex     int
	EncryptedShare []byte
}

// ValidateCircleConstraints checks threshold/shares count constraints.
// Returns (errorMessage, httpStatus). Empty errorMessage means OK.
func ValidateCircleConstraints(thresholdK, totalSharesN, sharesLen int) (string, int) {
	if thresholdK < 2 || thresholdK > totalSharesN || totalSharesN > 7 {
		return "threshold_k must be >= 2 and <= total_shares_n, total_shares_n must be <= 7", http.StatusBadRequest
	}
	if sharesLen != totalSharesN {
		return "Number of shares must equal total_shares_n", http.StatusBadRequest
	}
	return "", 0
}

// ValidateShareUniqueness checks for duplicate contact_ids, duplicate share_indexes,
// and out-of-range share indexes. Returns (errorMessage, httpStatus).
func ValidateShareUniqueness(shares []CircleShareEntry, totalSharesN int) (string, int) {
	contactSet := make(map[string]bool)
	indexSet := make(map[int]bool)
	for _, s := range shares {
		if contactSet[s.ContactID] {
			return "Duplicate contact_id in shares", http.StatusBadRequest
		}
		contactSet[s.ContactID] = true

		if indexSet[s.ShareIndex] {
			return "Duplicate share_index in shares", http.StatusBadRequest
		}
		indexSet[s.ShareIndex] = true

		if s.ShareIndex < 1 || s.ShareIndex > totalSharesN {
			return fmt.Sprintf("share_index must be between 1 and %d", totalSharesN), http.StatusBadRequest
		}
	}
	return "", 0
}

// validateShareContacts verifies no self-references and all contacts are accepted friends.
func (h *Handler) validateShareContacts(ctx context.Context, userID string, shares []CircleShareEntry) (string, int) {
	for _, s := range shares {
		if s.ContactID == userID {
			return "You cannot be your own recovery contact", http.StatusBadRequest
		}

		var friendCount int
		err := h.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM friendships
			WHERE status = 'accepted'
			AND ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))
		`, userID, s.ContactID).Scan(&friendCount)
		if err != nil {
			h.log.Error("Failed to check friendship for recovery contact", "error", err, "user_id", userID, "contact_id", s.ContactID)
			return "Failed to validate recovery contacts", http.StatusInternalServerError
		}
		if friendCount == 0 {
			return fmt.Sprintf("Contact %s is not an accepted friend", s.ContactID), http.StatusBadRequest
		}
	}
	return "", 0
}

// DecodeCircleShares base64-decodes all encrypted shares.
func DecodeCircleShares(shares []CircleShareEntry) ([]CircleDecodedShare, string, int) {
	decoded := make([]CircleDecodedShare, len(shares))
	for i, s := range shares {
		encShare, err := base64.StdEncoding.DecodeString(s.EncryptedShare)
		if err != nil {
			return nil, fmt.Sprintf("Invalid encrypted_share format for share %d (must be base64)", i), http.StatusBadRequest
		}
		decoded[i] = CircleDecodedShare{
			ContactID:      s.ContactID,
			ShareIndex:     s.ShareIndex,
			EncryptedShare: encShare,
		}
	}
	return decoded, "", 0
}

// executeCircleUpsert performs the circle upsert, old-share cleanup, and new-share
// insertion inside a single transaction. Returns (circleID, shareVersion, error).
func (h *Handler) executeCircleUpsert(ctx context.Context, userID string, thresholdK, totalSharesN int, shares []CircleDecodedShare) (string, int, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var circleID string
	var shareVersion int
	err = tx.QueryRowContext(ctx, `
		INSERT INTO recovery_circles (user_id, threshold_k, total_shares_n)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET
			threshold_k = $2,
			total_shares_n = $3,
			share_version = recovery_circles.share_version + 1,
			updated_at = NOW()
		RETURNING id, share_version
	`, userID, thresholdK, totalSharesN).Scan(&circleID, &shareVersion)
	if err != nil {
		return "", 0, fmt.Errorf("upsert circle: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		DELETE FROM recovery_circle_shares WHERE circle_id = $1 AND share_version < $2
	`, circleID, shareVersion)
	if err != nil {
		return "", 0, fmt.Errorf("delete old shares: %w", err)
	}

	for _, s := range shares {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO recovery_circle_shares (circle_id, contact_id, share_index, encrypted_share, share_version)
			VALUES ($1, $2, $3, $4, $5)
		`, circleID, s.ContactID, s.ShareIndex, s.EncryptedShare, shareVersion)
		if err != nil {
			return "", 0, fmt.Errorf("insert share for %s: %w", s.ContactID, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("commit: %w", err)
	}
	return circleID, shareVersion, nil
}

// UpsertRecoveryCircle creates or updates the user's social recovery circle with Shamir shares.
func (h *Handler) UpsertRecoveryCircle(c *gin.Context) {
	userID := c.GetString("user_id")

	var req struct {
		mfaStepUpCredentials
		ThresholdK   int                `json:"threshold_k" binding:"required"`
		TotalSharesN int                `json:"total_shares_n" binding:"required"`
		Shares       []CircleShareEntry `json:"shares" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, upsertRecoveryCircleStepUp); !ok {
		return
	}

	if errMsg, status := ValidateCircleConstraints(req.ThresholdK, req.TotalSharesN, len(req.Shares)); errMsg != "" {
		c.JSON(status, gin.H{"error": errMsg})
		return
	}

	if errMsg, status := ValidateShareUniqueness(req.Shares, req.TotalSharesN); errMsg != "" {
		c.JSON(status, gin.H{"error": errMsg})
		return
	}

	ctx := c.Request.Context()
	if errMsg, status := h.validateShareContacts(ctx, userID, req.Shares); errMsg != "" {
		c.JSON(status, gin.H{"error": errMsg})
		return
	}

	decodedShares, errMsg, status := DecodeCircleShares(req.Shares)
	if errMsg != "" {
		c.JSON(status, gin.H{"error": errMsg})
		return
	}

	_, shareVersion, err := h.executeCircleUpsert(ctx, userID, req.ThresholdK, req.TotalSharesN, decodedShares)
	if err != nil {
		h.log.Error("Failed to upsert recovery circle", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedConfigCircle})
		return
	}

	h.log.Info("Recovery circle configured", "user_id", userID, "threshold_k", req.ThresholdK, "total_shares_n", req.TotalSharesN, "share_version", shareVersion)
	h.clearStepUpAfterSuccess(c, userID)
	c.JSON(http.StatusOK, gin.H{"message": "Recovery circle configured", "share_version": shareVersion})
}

// DeleteRecoveryCircle deletes the user's social recovery circle and all shares.
func (h *Handler) DeleteRecoveryCircle(c *gin.Context) {
	userID := c.GetString("user_id")

	var req struct {
		mfaStepUpCredentials
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequest})
		return
	}

	// Require password + MFA verification
	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, deleteRecoveryCircleStepUp); !ok {
		return
	}

	// Delete circle (shares cascade via FK)
	result, err := h.db.ExecContext(c.Request.Context(),
		`DELETE FROM recovery_circles WHERE user_id = $1`, userID,
	)
	if err != nil {
		h.log.Error(errMsgFailedDeleteCircle, "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteCircle})
		return
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		h.log.Error("Failed to read the recovery circle delete result", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteCircle})
		return
	}
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "No recovery circle found"})
		return
	}

	h.log.Info("Recovery circle deleted", "user_id", userID)
	h.clearStepUpAfterSuccess(c, userID)
	c.JSON(http.StatusOK, gin.H{"message": "Recovery circle deleted"})
}

// GetMyRecoveryShares returns shares that other users have assigned to this user as a recovery contact.
func (h *Handler) GetMyRecoveryShares(c *gin.Context) {
	userID := c.GetString("user_id")

	rows, err := h.db.QueryContext(c.Request.Context(), `
		SELECT cs.id, cs.circle_id, cs.share_index, cs.encrypted_share, cs.share_version,
		       rc.user_id AS owner_id, u.username AS owner_username
		FROM recovery_circle_shares cs
		JOIN recovery_circles rc ON rc.id = cs.circle_id
		JOIN users u ON u.id = rc.user_id
		WHERE cs.contact_id = $1
	`, userID)
	if err != nil {
		h.log.Error("Failed to query recovery shares", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedLoadShares})
		return
	}
	defer func() { _ = rows.Close() }()

	type share struct {
		ID             string `json:"id"`
		CircleID       string `json:"circle_id"`
		ShareIndex     int    `json:"share_index"`
		EncryptedShare string `json:"encrypted_share"`
		ShareVersion   int    `json:"share_version"`
		OwnerID        string `json:"owner_id"`
		OwnerUsername  string `json:"owner_username"`
	}

	shares := []share{}
	for rows.Next() {
		var s share
		var encShare []byte
		if err := rows.Scan(&s.ID, &s.CircleID, &s.ShareIndex, &encShare, &s.ShareVersion, &s.OwnerID, &s.OwnerUsername); err != nil {
			// A share left out is one this contact cannot hand back.
			h.log.Error("Failed to scan recovery share row", "error", err, "user_id", userID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedLoadShares})
			return
		}
		s.EncryptedShare = base64.StdEncoding.EncodeToString(encShare)
		shares = append(shares, s)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating recovery shares", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedLoadShares})
		return
	}

	c.JSON(http.StatusOK, shares)
}

// ListSocialRecoveryRequests returns pending social recovery requests where this user is a contact.
func (h *Handler) ListSocialRecoveryRequests(c *gin.Context) {
	userID := c.GetString("user_id")

	rows, err := h.db.QueryContext(c.Request.Context(), `
		SELECT DISTINCT rr.id, rr.user_id, u.username, u.display_name,
		       rr.ephemeral_public_key, rr.created_at, rr.expires_at
		FROM recovery_circle_requests rr
		JOIN recovery_circles rc ON rc.id = rr.circle_id
		JOIN recovery_circle_shares cs ON cs.circle_id = rc.id
		JOIN users u ON u.id = rr.user_id
		WHERE cs.contact_id = $1 AND rr.status = 'pending' AND rr.expires_at > NOW()
	`, userID)
	if err != nil {
		h.log.Error(errMsgFailedListSocialReqs, "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedListSocialReqs})
		return
	}
	defer func() { _ = rows.Close() }()

	type socialRecoveryRequest struct {
		ID                 string    `json:"id"`
		UserID             string    `json:"user_id"`
		Username           string    `json:"username"`
		DisplayName        string    `json:"display_name"`
		EphemeralPublicKey string    `json:"ephemeral_public_key"`
		CreatedAt          time.Time `json:"created_at"`
		ExpiresAt          time.Time `json:"expires_at"`
	}

	requests := []socialRecoveryRequest{}
	var scanErr error
	for rows.Next() {
		var r socialRecoveryRequest
		var displayName sql.NullString
		var ephPubKey []byte
		if err := rows.Scan(&r.ID, &r.UserID, &r.Username, &displayName, &ephPubKey, &r.CreatedAt, &r.ExpiresAt); err != nil {
			h.log.Error("Failed to scan social recovery request row", "user_id", userID, "error", err)
			scanErr = err
			break
		}
		if displayName.Valid {
			r.DisplayName = displayName.String
		}
		r.EphemeralPublicKey = base64.StdEncoding.EncodeToString(ephPubKey)
		requests = append(requests, r)
	}
	if scanErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedListSocialReqs})
		return
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating social recovery requests", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedListSocialReqs})
		return
	}

	c.JSON(http.StatusOK, gin.H{"requests": requests})
}

// socialRecoveryRequestInfo holds the validated state of a social recovery request.
type socialRecoveryRequestInfo struct {
	Status     string
	CircleID   string
	ThresholdK int
	ExpiresAt  time.Time
}

// lookupSocialRecoveryRequest fetches and validates that the request is pending and not expired.
// Returns (info, errorMessage, httpStatus).
func (h *Handler) lookupSocialRecoveryRequest(ctx context.Context, userID, requestID string) (*socialRecoveryRequestInfo, string, int) {
	const notFound = "Recovery request not found or you are not a contact"
	if _, err := uuid.Parse(requestID); err != nil {
		return nil, notFound, http.StatusNotFound
	}
	var info socialRecoveryRequestInfo
	err := h.db.QueryRowContext(ctx, `
		SELECT rr.status, rr.circle_id, rc.threshold_k, rr.expires_at
		FROM recovery_circle_requests rr
		JOIN recovery_circles rc ON rc.id = rr.circle_id
		JOIN recovery_circle_shares cs ON cs.circle_id = rc.id AND cs.contact_id = $1
		WHERE rr.id = $2
		LIMIT 1
	`, userID, requestID).Scan(&info.Status, &info.CircleID, &info.ThresholdK, &info.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound, http.StatusNotFound
	}
	if err != nil {
		h.log.Error("Failed to read social recovery request", "error", err, "user_id", userID, "request_id", requestID)
		return nil, errMsgFailedSubmitResponse, http.StatusInternalServerError
	}
	if info.Status != "pending" {
		return nil, "Recovery request is no longer pending", http.StatusBadRequest
	}
	if time.Now().After(info.ExpiresAt) {
		return nil, "Recovery request has expired", http.StatusBadRequest
	}
	return &info, "", 0
}

// executeSocialRecoveryResponse inserts the response, increments shares_received,
// and marks the request complete if threshold is met — all in one transaction.
// Returns (sharesReceived, errorMessage, httpStatus).
func (h *Handler) executeSocialRecoveryResponse(ctx context.Context, requestID, userID string, encShare []byte, thresholdK int) (int, string, int) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error("Failed to begin transaction for social recovery response", "request_id", requestID, "contact_id", userID, "error", err)
		return 0, errMsgFailedSubmitResponse, http.StatusInternalServerError
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			h.log.Error("Failed to rollback social recovery response transaction", "request_id", requestID, "contact_id", userID, "error", rbErr)
		}
	}()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO recovery_circle_responses (request_id, contact_id, encrypted_share)
		VALUES ($1, $2, $3)
		ON CONFLICT (request_id, contact_id) DO NOTHING
	`, requestID, userID, encShare)
	if err != nil {
		h.log.Error("Failed to insert social recovery response", "request_id", requestID, "contact_id", userID, "error", err)
		return 0, errMsgFailedSubmitResponse, http.StatusInternalServerError
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		// Read as 0, this would claim the contact had already responded.
		h.log.Error("Failed to read the social recovery response result", "error", err, "contact_id", userID, "request_id", requestID)
		return 0, errMsgFailedSubmitResponse, http.StatusInternalServerError
	}
	if rowsAffected == 0 {
		return 0, "You have already responded to this request", http.StatusConflict
	}

	var sharesReceived int
	err = tx.QueryRowContext(ctx, `
		UPDATE recovery_circle_requests
		SET shares_received = shares_received + 1
		WHERE id = $1
		RETURNING shares_received
	`, requestID).Scan(&sharesReceived)
	if err != nil {
		h.log.Error("Failed to update shares_received", "contact_id", userID, "error", err, "request_id", requestID)
		return 0, errMsgFailedSubmitResponse, http.StatusInternalServerError
	}

	if sharesReceived >= thresholdK {
		_, err = tx.ExecContext(ctx, `
			UPDATE recovery_circle_requests SET status = 'complete' WHERE id = $1
		`, requestID)
		if err != nil {
			h.log.Error("Failed to mark social recovery request as complete", "contact_id", userID, "error", err, "request_id", requestID)
			return 0, errMsgFailedSubmitResponse, http.StatusInternalServerError
		}
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit social recovery response transaction", "contact_id", userID, "error", err, "request_id", requestID)
		return 0, errMsgFailedSubmitResponse, http.StatusInternalServerError
	}
	return sharesReceived, "", 0
}

// RespondToSocialRecovery allows an authenticated contact to submit their re-encrypted share.
func (h *Handler) RespondToSocialRecovery(c *gin.Context) {
	userID := c.GetString("user_id")
	requestID := c.Param("id")

	var req struct {
		EncryptedShare string `json:"encrypted_share" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "encrypted_share is required"})
		return
	}

	encShare, err := base64.StdEncoding.DecodeString(req.EncryptedShare)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid encrypted_share format (must be base64)"})
		return
	}

	ctx := c.Request.Context()

	info, errMsg, status := h.lookupSocialRecoveryRequest(ctx, userID, requestID)
	if errMsg != "" {
		c.JSON(status, gin.H{"error": errMsg})
		return
	}

	sharesReceived, errMsg, status := h.executeSocialRecoveryResponse(ctx, requestID, userID, encShare, info.ThresholdK)
	if errMsg != "" {
		c.JSON(status, gin.H{"error": errMsg})
		return
	}

	h.log.Info("Social recovery response submitted", "request_id", requestID, "contact_id", userID, "shares_received", sharesReceived, "threshold_k", info.ThresholdK)
	c.JSON(http.StatusOK, gin.H{"message": "Share submitted", "shares_received": sharesReceived, "threshold_met": sharesReceived >= info.ThresholdK})
}
