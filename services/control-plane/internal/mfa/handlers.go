package mfa

import (
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
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// Duplicated string literals extracted for SonarQube S1192 compliance.
const (
	// Redis key patterns
	redisKeyMFAChallengeRememberMe = "mfa_challenge:%s:remember_me"
	redisKeyEmailSmsEnabled        = "mfa_emailsms_enabled:%s:%s"
	redisKeyEmailSmsEnabledEmail   = "mfa_emailsms_enabled:%s:email"
	redisKeyEmailSmsSetup          = "mfa_emailsms_setup:%s:%s"
	redisKeyWebAuthnReg            = "webauthn_reg:%s"
	redisKeyTOTPSetupSession       = "mfa_totp_setup_session:%s"

	// mfaUpgradeBypassTTL bounds a one-refresh exemption from the pre-MFA
	// session lock (auth.checkPreMFASessionLock): long enough for the refresh
	// that follows, never a standing exemption.
	mfaUpgradeBypassTTL = 30 * time.Second
	// totpSetupSessionTTL bounds how long confirm-setup can match the session
	// that proved the code at verify-setup.
	totpSetupSessionTTL = 15 * time.Minute

	// Error messages
	errMsgPasswordRequired           = "Password is required"
	errMsgIncorrectPassword          = "Incorrect password"
	errMsgCodeRequired               = "Code is required"
	errMsgFailedBackupCodes          = "Failed to generate backup codes"
	errMsgFailedStartReg             = "Failed to start registration"
	errMsgFailedActivateMFA          = "Failed to activate MFA"
	errMsgInvalidSessionData         = "Invalid session data"
	errMsgFailedListDevices          = "Failed to list trusted devices"
	errMsgFailedListRecoveryReqs     = "Failed to list recovery requests"
	errMsgFailedLoadCircle           = "Failed to load recovery circle"
	errMsgFailedConfigCircle         = "Failed to configure recovery circle"
	errMsgFailedListSocialReqs       = "Failed to list social recovery requests"
	errMsgFailedSubmitResponse       = "Failed to submit response"
	errMsgMFAVerificationUnavailable = "MFA verification unavailable"
	errMsgInvalidRequest             = "Invalid request"
	errMsgFailedLoadMFAStatus        = "Failed to load MFA status"
	errMsgFailedUpdateBackupEmail    = "Failed to update backup email"
	errMsgFailedStoreRecoveryKey     = "Failed to store recovery key"
	msgRecoveryKeyStored             = "Recovery key stored"
	errMsgFailedDisableEmailSms      = "Failed to disable Email/SMS MFA methods"
	errMsgFailedDisableMFA           = "Failed to disable MFA"
	errMsgFailedDeleteCredential     = "Failed to delete credential"

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

// VerifyCode checks a TOTP code or backup code against the user's stored MFA secrets.
func (h *Handler) VerifyCode(ctx context.Context, userID string, code string) (bool, error) {
	return h.verifyCode(ctx, h.db, userID, code)
}

// VerifyCodeTx performs the same verification on the caller's transaction
// connection. Sensitive rotations use this after locking the users row so they
// neither re-authorize against superseded MFA state nor acquire a second pooled
// database connection while holding the first.
func (h *Handler) VerifyCodeTx(ctx context.Context, tx *sql.Tx, userID string, code string) (bool, error) {
	if tx == nil {
		return false, fmt.Errorf("MFA verification transaction is required")
	}
	return h.verifyCode(ctx, tx, userID, code)
}

func (h *Handler) verifyCode(ctx context.Context, store codeVerificationStore, userID string, code string) (bool, error) {
	verified, _, err := h.verifyCodeMatchedMethod(ctx, store, userID, code)
	return verified, err
}

// verifyCodeMatchedMethod preserves VerifyCode's public boolean contract while
// retaining the server-observed factor for success telemetry. A submitted
// method is only an attempted method: a backup-code submission can validate a
// TOTP value (and vice versa), so it must not choose the success event label.
func (h *Handler) verifyCodeMatchedMethod(ctx context.Context, store codeVerificationStore, userID string, code string) (bool, string, error) {
	// Check for WebAuthn inline verification token first (from WebAuthnVerifyInlineFinish)
	inlineVerified, err := h.consumeWebAuthnInlineToken(ctx, userID, code)
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
		if ValidateCode(string(secret), code) {
			return true, "totp", nil
		}

		backupVerified, backupErr := consumeBackupCode(ctx, store, userID, code)
		if backupVerified {
			return true, "backup_code", nil
		}
		return false, "", backupErr
	}

	return false, "", nil
}

func (h *Handler) consumeWebAuthnInlineToken(ctx context.Context, userID, code string) (bool, error) {
	if len(code) <= 20 {
		return false, nil
	}
	token, err := h.redis.GetDel(ctx, fmt.Sprintf("mfa_inline_token:%s:%s", userID, code)).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("consume WebAuthn inline verification token: %w", err)
	}
	return token != "", nil
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
	key := fmt.Sprintf(redisKeyMFAChallengeRememberMe, jti)
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

	sessionJSON, _ := json.Marshal(session)
	sessionKey := fmt.Sprintf("mfa_webauthn_session:%s", jti)
	h.redis.Set(ctx, sessionKey, sessionJSON, challengeTTL)

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
// either. errSubjectGone is returned unwrapped-matchable.
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit MFA factor write: %w", err)
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

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, totpSetupStepUpCopy); !ok {
		return
	}

	// Check if already has confirmed TOTP
	var existingConfirmed bool
	checkErr := h.db.QueryRowContext(ctx, `SELECT confirmed FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(&existingConfirmed)
	if checkErr == nil && existingConfirmed {
		c.JSON(http.StatusConflict, gin.H{"error": "TOTP is already enabled. Disable it first to re-enroll."})
		return
	}

	// Get user email for the TOTP issuer label
	var email string
	if h.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email) != nil {
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
				updated_at = NOW()
		`, userID, ciphertext, nonce, keyVer); err != nil {
			return fmt.Errorf("store TOTP secret: %w", err)
		}
		return nil
	}); err != nil {
		h.failMFAFactorWrite(c, "Failed to store TOTP secret", "Failed to store MFA secret", err)
		return
	}
	h.clearStepUpAfterSuccess(c, userID)

	// The secret was replaced, so a session that verified the previous one must
	// not match at confirm-setup for this one. Stop if the record survives: the
	// user retries setup, and nothing is lost.
	if err := h.redis.Del(ctx, fmt.Sprintf(redisKeyTOTPSetupSession, userID)).Err(); err != nil {
		h.log.Error("Failed to clear the TOTP setup session", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store MFA secret"})
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

	// Rate limit: check for MFA setup lockout. An unreadable lockout stops the
	// request; checking codes without it would lift the attempt limit.
	lockoutKey := fmt.Sprintf("mfa_setup_lockout:%s", userID)
	locked, err := h.redis.Exists(ctx, lockoutKey).Result()
	if err != nil {
		h.log.Error("Failed to read the MFA setup lockout", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify code"})
		return
	}
	if locked > 0 {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many failed attempts. Try again later."})
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
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No TOTP setup in progress. Call /mfa/totp/setup first."})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify code"})
		return
	}

	if !ValidateCode(string(secret), req.Code) {
		h.recordFailedSetupAttempt(ctx, userID, lockoutKey)
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid code"})
		return
	}

	// Code valid — generate backup codes
	codes, hashes, err := GenerateBackupCodes()
	if err != nil {
		h.log.Error(errMsgFailedBackupCodes, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBackupCodes})
		return
	}

	// Mark as enabled (code verified) but NOT confirmed (backup codes not yet acknowledged)
	usedFlags := make([]bool, len(hashes))

	_, err = h.db.ExecContext(ctx, `
		UPDATE user_mfa_totp
		SET enabled = TRUE, verified_at = NOW(), backup_codes_hash = $1, backup_codes_used = $2, updated_at = NOW()
		WHERE user_id = $3
	`, pq.Array(hashes), pq.Array(usedFlags), userID)
	if err != nil {
		h.log.Error("Failed to update TOTP status", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to complete verification"})
		return
	}

	// Clear attempt tracking
	if err := h.redis.Del(ctx, fmt.Sprintf("mfa_setup_attempts:%s", userID)).Err(); err != nil {
		h.log.Error("Failed to reset the MFA setup attempt count", "error", err)
	}

	// confirm-setup takes no code, so it may exempt a session from the pre-MFA
	// challenge only if that session is the one that proved the code here.
	if sid := middleware.TokenSessionID(c); sid != "" {
		if err := h.redis.Set(ctx, fmt.Sprintf(redisKeyTOTPSetupSession, userID), sid, totpSetupSessionTTL).Err(); err != nil {
			h.log.Error("Failed to record the TOTP setup session", "error", err)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"backup_codes": codes,
		"message":      "TOTP verified. Save your backup codes, then call /mfa/totp/confirm-setup to activate MFA.",
	})
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

// recordFailedSetupAttempt counts a wrong verify-setup code and locks setup
// after five. Its Redis errors are logged rather than answered. The lockout
// read at the top of verify-setup fails closed, so an outage that fails reads
// stops setup entirely. An outage that fails only writes (out of memory, a
// read-only replica) leaves no attempt limit here; the caller is guessing a
// code for a secret it was just shown.
func (h *Handler) recordFailedSetupAttempt(ctx context.Context, userID, lockoutKey string) {
	attemptsKey := fmt.Sprintf("mfa_setup_attempts:%s", userID)
	attempts, err := h.redis.Incr(ctx, attemptsKey).Result()
	if err != nil {
		h.log.Error("Failed to count an MFA setup attempt", "error", err)
		return
	}
	if err := h.redis.Expire(ctx, attemptsKey, 5*time.Minute).Err(); err != nil {
		h.log.Error("Failed to expire the MFA setup attempt count", "error", err)
	}
	if attempts < 5 {
		return
	}
	if err := h.redis.Set(ctx, lockoutKey, "1", 15*time.Minute).Err(); err != nil {
		// Keep the count, so the next wrong code tries the lockout again.
		h.log.Error("Failed to lock MFA setup", "error", err)
		return
	}
	if err := h.redis.Del(ctx, attemptsKey).Err(); err != nil {
		h.log.Error("Failed to reset the MFA setup attempt count", "error", err)
	}
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
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit TOTP disable", "error", err)
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

	valid, err := h.VerifyCodeTx(ctx, tx, userID, req.Code)
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
	if err != nil || !match {
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
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid TOTP code"})
		return
	}
	if !ValidateCode(string(secret), req.Code) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid TOTP code"})
		return
	}

	// Generate new backup codes
	codes, hashes, err := GenerateBackupCodes()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedBackupCodes})
		return
	}

	usedFlags := make([]bool, len(hashes))
	_, err = h.db.ExecContext(ctx, `
		UPDATE user_mfa_totp SET backup_codes_hash = $1, backup_codes_used = $2, updated_at = NOW() WHERE user_id = $3
	`, pq.Array(hashes), pq.Array(usedFlags), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store backup codes"})
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

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, webAuthnRegisterStepUpCopy); !ok {
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

	// Store session data in Redis (keyed by user ID, 5-min TTL)
	sessionJSON, _ := json.Marshal(session)
	credName := req.CredentialName
	if credName == "" {
		credName = "Security Key"
	}
	// Store session + metadata together
	meta := map[string]interface{}{
		"session":         string(sessionJSON),
		"credential_name": credName,
		"credential_type": credType,
	}
	metaJSON, _ := json.Marshal(meta)
	h.redis.Set(ctx, fmt.Sprintf(redisKeyWebAuthnReg, userID), metaJSON, 5*time.Minute)

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
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No registration in progress or session expired"})
		return
	}

	var meta struct {
		Session        string `json:"session"`
		CredentialName string `json:"credential_name"`
		CredentialType string `json:"credential_type"`
	}
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInvalidSessionData})
		return
	}

	var session webauthn.SessionData
	if err := json.Unmarshal([]byte(meta.Session), &session); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInvalidSessionData})
		return
	}

	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list credentials"})
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

	var creds []credInfo
	for rows.Next() {
		var ci credInfo
		var lastUsed sql.NullTime
		var createdAt time.Time
		if err := rows.Scan(&ci.ID, &ci.CredentialName, &ci.CredentialType, &createdAt, &lastUsed); err != nil {
			continue
		}
		ci.CreatedAt = createdAt.Format(time.RFC3339)
		if lastUsed.Valid {
			s := lastUsed.Time.Format(time.RFC3339)
			ci.LastUsedAt = &s
		}
		creds = append(creds, ci)
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
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit WebAuthn delete", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedDeleteCredential})
		return
	}
	h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonFactorDisabled, AuthMethod: securityevent.AuthWebAuthn})

	// Return remaining credential IDs so the client can signal the authenticator
	var remainingIDs [][]byte
	credRows, queryErr := h.db.QueryContext(ctx,
		`SELECT credential_id FROM user_mfa_webauthn WHERE user_id = $1`, userID)
	if queryErr == nil {
		defer credRows.Close() //nolint:errcheck
		for credRows.Next() {
			var cid []byte
			if credRows.Scan(&cid) == nil {
				remainingIDs = append(remainingIDs, cid)
			}
		}
	}

	// Base64url-encode remaining credential IDs for the client
	encoded := make([]string, len(remainingIDs))
	for i, cid := range remainingIDs {
		encoded[i] = base64.RawURLEncoding.EncodeToString(cid)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":                  "Credential deleted",
		"remaining_credential_ids": encoded,
		"user_id":                  userID,
	})
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
		c.JSON(http.StatusNotFound, gin.H{"error": "Credential not found"})
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

	// Check single-use: ensure this JTI hasn't been consumed
	usedKey := fmt.Sprintf("mfa_challenge_used:%s", claims.ID)
	if h.redis.Exists(ctx, usedKey).Val() > 0 {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid, RouteTemplate: securityevent.RouteAuthMFAVerify})
		c.JSON(http.StatusUnauthorized, gin.H{"error": "MFA challenge already used"})
		return
	}

	// Rate limit per user
	attemptsKey := fmt.Sprintf("mfa_verify_attempts:%s", claims.UserID)
	lockoutKey := fmt.Sprintf("mfa_verify_lockout:%s", claims.UserID)
	if h.redis.Exists(ctx, lockoutKey).Val() > 0 {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeLocked, RouteTemplate: securityevent.RouteAuthMFAVerify})
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many failed attempts. Try again later."})
		return
	}

	verified, matchedMethod, responded := h.verifyByMethod(ctx, c, req, claims)
	if responded {
		return // Early return already sent a response (e.g. bad request)
	}

	if !verified {
		h.recordVerifyFailure(ctx, attemptsKey, lockoutKey)
		outcome := middleware.RecordAuthFailure(ctx, h.redis, c.ClientIP(), middleware.DefaultAuthBanConfig())
		h.emitHTTPEvent(c, mfaChallengeInvalidEvent(req.Method))
		middleware.MarkAuthFailureOutcome(c, outcome)
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid MFA code"})
		return
	}

	h.completeVerifiedChallenge(ctx, c, claims, purpose, matchedMethod)
}

// completeVerifiedChallenge records only the outcome of a completed challenge
// purpose. For login, the completer returns false until its session-mint
// transaction commits, so this path cannot report an MFA success for a failed
// login transaction.
func (h *Handler) completeVerifiedChallenge(ctx context.Context, c *gin.Context, claims *ChallengeClaims, purpose ChallengePurpose, method string) {
	// Atomically claim the verified challenge before any session mint. A
	// check-then-set permits two successful factor verifications to mint two
	// sessions from one challenge.
	usedKey := fmt.Sprintf("mfa_challenge_used:%s", claims.ID)
	attemptsKey := fmt.Sprintf("mfa_verify_attempts:%s", claims.UserID)
	claimed, err := h.redis.SetNX(ctx, usedKey, "1", challengeTTL).Result()
	if err != nil {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable, RouteTemplate: securityevent.RouteAuthMFAVerify})
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAVerificationUnavailable})
		return
	}
	if !claimed {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventMFA, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonChallengeInvalid, RouteTemplate: securityevent.RouteAuthMFAVerify})
		c.JSON(http.StatusUnauthorized, gin.H{"error": "MFA challenge already used"})
		return
	}
	h.redis.Del(ctx, attemptsKey)
	middleware.ClearAuthFailures(ctx, h.redis, c.ClientIP())
	if h.completeVerifyPurpose(ctx, c, claims, purpose) {
		h.emitHTTPEvent(c, mfaChallengeVerifiedEvent(method))
	}
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
	valid, matchedMethod, err := h.verifyCodeMatchedMethod(ctx, h.db, userID, code)
	if err != nil {
		h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable, RouteTemplate: securityevent.RouteAuthMFAVerify})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Verification failed"})
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

	sessionKey := fmt.Sprintf("mfa_webauthn_session:%s", claims.ID)
	sessionJSON, err := h.redis.Get(ctx, sessionKey).Bytes()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No WebAuthn session found. Request a new challenge."})
		return false, true
	}

	var session webauthn.SessionData
	if err := json.Unmarshal(sessionJSON, &session); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid WebAuthn session"})
		return false, true
	}

	user, err := h.buildWebAuthnUser(ctx, claims.UserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify"})
		return false, true
	}

	credential, err := h.webauthn.FinishLoginWithBytes(user, session, assertion)
	if err != nil {
		h.log.Warn("WebAuthn login verification failed", "error", err, "user_id", claims.UserID)
		h.redis.Del(ctx, sessionKey)
		return false, false
	}
	if _, err := h.db.ExecContext(ctx, `
		UPDATE user_mfa_webauthn SET sign_count = $1, last_used_at = NOW() WHERE credential_id = $2 AND user_id = $3
	`, credential.Authenticator.SignCount, credential.ID, claims.UserID); err != nil {
		h.log.Error("Failed to update WebAuthn sign count", "error", err, "user_id", claims.UserID)
	}
	h.redis.Del(ctx, sessionKey)
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
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No email code pending. Request a new one."})
		return false, true
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(stored)) != 1 {
		return false, false
	}
	h.redis.Del(ctx, codeKey)
	h.redis.Del(ctx, fmt.Sprintf("mfa_email_sent:%s", claims.ID))
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

// recordVerifyFailure increments the failure counter and applies lockout if threshold reached.
func (h *Handler) recordVerifyFailure(ctx context.Context, attemptsKey, lockoutKey string) {
	attempts := h.redis.Incr(ctx, attemptsKey).Val()
	h.redis.Expire(ctx, attemptsKey, 5*time.Minute)
	if attempts >= 5 {
		h.redis.Set(ctx, lockoutKey, "1", 15*time.Minute)
		h.redis.Del(ctx, attemptsKey)
	}
}

// completeVerifyPurpose performs the action associated with the MFA challenge purpose.
func (h *Handler) completeVerifyPurpose(ctx context.Context, c *gin.Context, claims *ChallengeClaims, purpose ChallengePurpose) bool {
	switch purpose {
	case PurposeLogin:
		if h.loginCompleter == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Login completion not configured"})
			return false
		}
		rememberKey := fmt.Sprintf(redisKeyMFAChallengeRememberMe, claims.ID)
		rememberValue, err := h.redis.Get(ctx, rememberKey).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable, RouteTemplate: securityevent.RouteAuthMFAVerify})
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAVerificationUnavailable})
			return false
		}
		rememberMe := rememberValue == "1"
		if err == nil {
			h.redis.Del(ctx, rememberKey)
		}
		primaryAuthMethod, err := normalizePrimaryAuthMethod(claims.PrimaryAuthMethod)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid MFA challenge"})
			return false
		}
		return h.loginCompleter.CompleteLogin(c, claims.UserID, rememberMe, claims.CredentialEpoch, primaryAuthMethod)

	case PurposeMFAUpgrade:
		bypassKey := auth.MFAUpgradeBypassKey(claims.UserID, claims.RefreshSessionID)
		if err := h.redis.Set(ctx, bypassKey, "1", mfaUpgradeBypassTTL).Err(); err != nil {
			h.emitHTTPEvent(c, securityevent.Event{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable, RouteTemplate: securityevent.RouteAuthMFAVerify})
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgMFAVerificationUnavailable})
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

	// Try all purposes — the token encodes which one
	var claims *ChallengeClaims
	for _, p := range []ChallengePurpose{PurposeLogin, PurposeSuspiciousRefresh, PurposeMFAUpgrade} {
		if parsed, err := ValidateChallengeToken(req.ChallengeToken, h.jwtSecret, p); err == nil {
			claims = parsed
			break
		}
	}
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired challenge token"})
		return
	}

	ctx := c.Request.Context()

	// Rate limit: 1 email per challenge JTI
	sentKey := fmt.Sprintf("mfa_email_sent:%s", claims.ID)
	if h.redis.Exists(ctx, sentKey).Val() > 0 {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Email code already sent. Check your inbox or wait for it to expire."})
		return
	}

	// Verify user has email MFA enabled
	enabledKey := fmt.Sprintf(redisKeyEmailSmsEnabledEmail, claims.UserID)
	if h.redis.Exists(ctx, enabledKey).Val() == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email MFA is not enabled for this account"})
		return
	}

	// Look up user's email
	var userEmail string
	if err := h.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, claims.UserID).Scan(&userEmail); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send code"})
		return
	}

	// Generate code
	code, err := generateNumericCode(6)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate code"})
		return
	}

	// Send first, then persist — avoids stale Redis keys if send fails
	if h.emailSvc != nil {
		if err := h.emailSvc.SendVerificationCode(userEmail, code); err != nil {
			h.log.Error("Failed to send MFA email code", "error", err, "user_id", claims.UserID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send verification email"})
			return
		}
	} else {
		// Dev mode — log only
		h.log.Info("DEV MODE — MFA email code", "user_id", claims.UserID, "code", code)
	}

	// Store code + sent flag in Redis only after successful send
	codeKey := fmt.Sprintf("mfa_email_login:%s", claims.ID)
	if err := h.redis.Set(ctx, codeKey, code, 10*time.Minute).Err(); err != nil {
		h.log.Error("Failed to store MFA email code in Redis", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Code sent but failed to store — request a new one"})
		return
	}
	if err := h.redis.Set(ctx, sentKey, "1", 10*time.Minute).Err(); err != nil {
		h.log.Error("Failed to store MFA email sent flag in Redis", "error", err)
		// Code is stored, sent flag failed — non-fatal, user can still verify
	}

	c.JSON(http.StatusOK, gin.H{
		"message":    "Verification code sent to your email",
		"expires_in": 600,
	})
}

// ── Inline WebAuthn Verify (for protected operations) ────────────────────────

// WebAuthnVerifyInlineBegin starts a WebAuthn assertion for MFA verification on
// protected endpoints (setup, revoke, etc.). Returns assertion options for
// navigator.credentials.get(). The session is stored in Redis keyed by user ID.
func (h *Handler) WebAuthnVerifyInlineBegin(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
		h.log.Error("Failed to build WebAuthn user", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start verification"})
		return
	}
	if len(user.WebAuthnCredentials()) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No WebAuthn credentials registered"})
		return
	}

	assertion, session, err := h.webauthn.BeginLogin(user)
	if err != nil {
		h.log.Error("WebAuthn BeginLogin failed for inline verify", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start verification"})
		return
	}

	sessionJSON, _ := json.Marshal(session)
	sessionKey := fmt.Sprintf("mfa_inline_session:%s", userID)
	h.redis.Set(ctx, sessionKey, sessionJSON, 2*time.Minute)

	c.JSON(http.StatusOK, assertion)
}

// WebAuthnVerifyInlineFinish validates a WebAuthn assertion for protected
// operations. On success, returns a short-lived verification token that can be
// used as mfa_code on any protected endpoint.
func (h *Handler) WebAuthnVerifyInlineFinish(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	// Read raw body for WebAuthn assertion parsing
	sessionKey := fmt.Sprintf("mfa_inline_session:%s", userID)
	sessionJSON, err := h.redis.Get(ctx, sessionKey).Bytes()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No verification session found. Start a new verification."})
		return
	}
	// Delete session immediately (single-use)
	h.redis.Del(ctx, sessionKey)

	var session webauthn.SessionData
	if err := json.Unmarshal(sessionJSON, &session); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInvalidSessionData})
		return
	}

	user, err := h.buildWebAuthnUser(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify"})
		return
	}

	credential, err := h.webauthn.FinishLogin(user, session, c.Request)
	if err != nil {
		h.log.Warn("WebAuthn inline verify assertion failed", "error", err, "user_id", userID)
		c.JSON(http.StatusForbidden, gin.H{"error": "Verification failed. Try again."})
		return
	}

	// Update sign count
	_, _ = h.db.ExecContext(ctx,
		`UPDATE user_mfa_webauthn SET sign_count = $1, last_used_at = NOW() WHERE credential_id = $2 AND user_id = $3`,
		credential.Authenticator.SignCount, credential.ID, userID,
	)

	// Generate a short-lived verification token (60s, single-use)
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	tokenKey := fmt.Sprintf("mfa_inline_token:%s:%s", userID, token)
	h.redis.Set(ctx, tokenKey, "1", 60*time.Second)

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

	var creds []webauthn.Credential
	for rows.Next() {
		var credID, pubKey, aaguid []byte
		var signCount int64
		var transports []string
		if err := rows.Scan(&credID, &pubKey, &aaguid, &signCount, pq.Array(&transports)); err != nil {
			continue
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

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, recoveryOnlyStepUpCopy); !ok {
		return
	}

	if req.Methods == nil {
		req.Methods = []string{}
	}

	enabledMethods, err := h.GetEnabledMethods(ctx, userID)
	if err != nil {
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

	if hasEmailOrSms(validRecoveryOnly) {
		_, err = h.db.ExecContext(ctx,
			`UPDATE users SET recovery_only_methods = $1, recovery_hardened = TRUE WHERE id = $2`,
			pq.Array(validRecoveryOnly), userID,
		)
	} else {
		_, err = h.db.ExecContext(ctx,
			`UPDATE users SET recovery_only_methods = $1 WHERE id = $2`,
			pq.Array(validRecoveryOnly), userID,
		)
	}
	if err != nil {
		h.log.Error("Failed to update recovery_only_methods", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update settings"})
		return
	}

	var recoveryHardened bool
	_ = h.db.QueryRowContext(ctx, `SELECT recovery_hardened FROM users WHERE id = $1`, userID).Scan(&recoveryHardened)

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

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, recoveryHardenedStepUpCopy); !ok {
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
		if m == "sms" && h.environment == "production" {
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
		key := fmt.Sprintf(redisKeyEmailSmsSetup, userID, method)
		h.redis.Set(ctx, key, code, 10*time.Minute)
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Email delivery is not configured"})
		return false
	}

	if err := h.emailSvc.SendVerificationCode(userEmail, code); err != nil {
		h.log.Error("Failed to send MFA email code", "error", err, "user_id", userID)
		if h.redis != nil {
			ctx := context.Background()
			if c.Request != nil {
				ctx = c.Request.Context()
			}
			h.redis.Del(ctx, fmt.Sprintf(redisKeyEmailSmsSetup, userID, "email"))
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

	subj, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, emailSmsSetupStepUpCopy)
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
		if method == "email" && h.emailSvc == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Email delivery is not configured"})
			return
		}
	}

	var userEmail string
	if err := h.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&userEmail); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to look up account email"})
		return
	}

	codes, err := h.generateAndStoreEmailSmsCodes(ctx, userID, req.Methods)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate code"})
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
	if h.environment != "production" {
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
			h.log.Error("Failed to read pending MFA setup code", "method", method, "error", err)
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
	if err := h.activateEmailSmsMethods(ctx, userID, middleware.TokenSessionID(c), verified); err != nil {
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
// This request checked the code, so the session activating the method is the
// one that proved it, and a first activation exempts that session as TOTP and
// WebAuthn do.
func (h *Handler) activateEmailSmsMethods(ctx context.Context, userID, sessionID string, verified []string) error {
	firstActivation := h.mfaNeverEnabled(ctx, userID)
	for _, method := range verified {
		if err := h.redis.Set(ctx, fmt.Sprintf(redisKeyEmailSmsEnabled, userID, method), "1", 0).Err(); err != nil {
			h.log.Error("Failed to persist MFA method activation", "method", method, "error", err)
			return err
		}
	}

	es := h.readEmailSmsForSync(ctx, userID).withKnownOn(verified)
	if err := h.withMFAFactorWriteTx(ctx, userID, es, nil); err != nil {
		h.log.Error("Failed to update MFA flags after email/sms enable", "error", err)
		return err
	}
	if firstActivation {
		h.grantEnrollmentUpgrade(ctx, userID, sessionID)
	}

	// Best-effort cleanup, and only now that the activation is durable. The
	// asymmetry with the Set above is deliberate: a failed Set must fail the
	// request (the method is NOT active), but a failed Del may log-and-continue —
	// the method genuinely IS active and the stale code expires by its own TTL.
	for _, method := range verified {
		if err := h.redis.Del(ctx, fmt.Sprintf(redisKeyEmailSmsSetup, userID, method)).Err(); err != nil {
			h.log.Error("Failed to clear pending MFA setup code", "method", method, "error", err)
		}
	}

	return nil
}

// settingsStepUpCopy builds a route's refusal copy. NoFactors is reachable
// only if a password hash is ever empty (see internal/stepup); prompt is what
// a request that sent no password is told, and must be honest to a renderer
// that shows it verbatim.
func settingsStepUpCopy(action, prompt string) stepup.Copy {
	return stepup.Copy{
		NoFactors:          action + " requires proving your identity, but this account has no password and no MFA method.",
		CredentialRequired: prompt,
	}
}

// Per-route refusal copy for the MFA-settings step-up gate (settings_stepup.go).
// A renderer that predates the in-transaction gate never asks for a password,
// so the two of its routes it still drives carry an update hint. The nine
// pool-side routes always required a password, so an old renderer already
// sends one and their copy needs no hint.
var (
	emailSmsDisableStepUpCopy = settingsStepUpCopy("Turning off email verification",
		"Enter your password to turn off email verification. If you aren't asked for it, update Concord Voice.")
	backupEmailStepUpCopy = settingsStepUpCopy("Changing your backup email",
		"Enter your password to change your backup email. If you aren't asked for it, update Concord Voice.")
	recoveryKeyReplaceStepUpCopy = settingsStepUpCopy("Replacing your recovery key",
		"Enter your password to replace your recovery key.")
	recoveryKeyRemoveStepUpCopy = settingsStepUpCopy("Removing your recovery key",
		"Enter your password to remove your recovery key.")

	totpSetupStepUpCopy = settingsStepUpCopy("Setting up an authenticator app",
		"Enter your password to set up an authenticator app.")
	webAuthnRegisterStepUpCopy = settingsStepUpCopy("Adding a security key",
		"Enter your password to add a security key.")
	recoveryOnlyStepUpCopy = settingsStepUpCopy("Changing your recovery-only methods",
		"Enter your password to change which methods are for recovery only.")
	recoveryHardenedStepUpCopy = settingsStepUpCopy("Changing hardened recovery",
		"Enter your password to change hardened recovery.")
	emailSmsSetupStepUpCopy = settingsStepUpCopy("Turning on email or text-message codes",
		"Enter your password to turn on email or text-message codes.")
	designateTrustedDeviceStepUpCopy = settingsStepUpCopy("Trusting this device for recovery",
		"Enter your password to trust this device for account recovery.")
	removeTrustedDeviceStepUpCopy = settingsStepUpCopy("Removing a trusted device",
		"Enter your password to remove a trusted recovery device.")
	upsertRecoveryCircleStepUpCopy = settingsStepUpCopy("Setting up your recovery circle",
		"Enter your password to set up your recovery circle.")
	deleteRecoveryCircleStepUpCopy = settingsStepUpCopy("Deleting your recovery circle",
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
	if e, stage := h.verifyMFASettingsStepUpTx(ctx, tx, userID, subj, creds, emailSmsDisableStepUpCopy); e != nil {
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
	if e, stage := h.verifyMFASettingsStepUpTx(ctx, tx, userID, subj, req.mfaStepUpCredentials, backupEmailStepUpCopy); e != nil {
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
	if e, stage := h.verifyMFASettingsStepUpTx(ctx, tx, userID, subj, creds, recoveryKeyReplaceStepUpCopy); e != nil {
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
	if e, stage := h.verifyMFASettingsStepUpTx(ctx, tx, userID, subj, creds, recoveryKeyRemoveStepUpCopy); e != nil {
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
			h.log.Error("Failed to scan trusted device row", "error", err)
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

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, designateTrustedDeviceStepUpCopy); !ok {
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

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, removeTrustedDeviceStepUpCopy); !ok {
		return
	}

	result, err := h.db.ExecContext(c.Request.Context(),
		`DELETE FROM trusted_recovery_devices WHERE id = $1 AND user_id = $2`, deviceID, userID,
	)
	if err != nil {
		h.log.Error("Failed to remove trusted device", "error", err, "user_id", userID, "device_id", deviceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove trusted device"})
		return
	}

	rowsAffected, _ := result.RowsAffected()
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
			h.log.Error("Failed to scan recovery request row", "error", err)
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

// executeRecoveryResponse persists the approve or reject action to the database.
func (h *Handler) executeRecoveryResponse(ctx context.Context, requestID, action string, encPayload, respPubKey []byte) error {
	if action == "approve" {
		_, err := h.db.ExecContext(ctx, `
			UPDATE recovery_requests
			SET status = 'approved', encrypted_payload = $1, responder_public_key = $2, responded_at = NOW()
			WHERE id = $3
		`, encPayload, respPubKey, requestID)
		return err
	}
	_, err := h.db.ExecContext(ctx, `
		UPDATE recovery_requests
		SET status = 'rejected', responded_at = NOW()
		WHERE id = $1
	`, requestID)
	return err
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

	// Verify the request belongs to this user and is still pending
	var requestUserID, status string
	err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT user_id, status FROM recovery_requests WHERE id = $1 AND expires_at > NOW()`, requestID,
	).Scan(&requestUserID, &status)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Recovery request not found"})
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
	if err := h.executeRecoveryResponse(ctx, requestID, req.Action, encPayload, respPubKey); err != nil {
		h.log.Error("Failed to respond to recovery request", "error", err, "request_id", requestID, "action", req.Action)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to respond to recovery request"})
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
			h.log.Error("Failed to scan recovery circle contact", "error", err)
			continue
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

	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, upsertRecoveryCircleStepUpCopy); !ok {
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
	if _, ok := h.requirePasswordAndMFA(c, userID, req.mfaStepUpCredentials, deleteRecoveryCircleStepUpCopy); !ok {
		return
	}

	// Delete circle (shares cascade via FK)
	result, err := h.db.ExecContext(c.Request.Context(),
		`DELETE FROM recovery_circles WHERE user_id = $1`, userID,
	)
	if err != nil {
		h.log.Error("Failed to delete recovery circle", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete recovery circle"})
		return
	}

	rowsAffected, _ := result.RowsAffected()
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load recovery shares"})
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
			h.log.Error("Failed to scan recovery share row", "error", err)
			continue
		}
		s.EncryptedShare = base64.StdEncoding.EncodeToString(encShare)
		shares = append(shares, s)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Error iterating recovery shares", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load recovery shares"})
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
			h.log.Error("Failed to scan social recovery request row", "error", err)
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
	var info socialRecoveryRequestInfo
	err := h.db.QueryRowContext(ctx, `
		SELECT rr.status, rr.circle_id, rc.threshold_k, rr.expires_at
		FROM recovery_circle_requests rr
		JOIN recovery_circles rc ON rc.id = rr.circle_id
		JOIN recovery_circle_shares cs ON cs.circle_id = rc.id AND cs.contact_id = $1
		WHERE rr.id = $2
		LIMIT 1
	`, userID, requestID).Scan(&info.Status, &info.CircleID, &info.ThresholdK, &info.ExpiresAt)
	if err != nil {
		return nil, "Recovery request not found or you are not a contact", http.StatusNotFound
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
		h.log.Error("Failed to begin transaction for social recovery response", "error", err)
		return 0, errMsgFailedSubmitResponse, http.StatusInternalServerError
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback social recovery response transaction", "error", rbErr)
		}
	}()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO recovery_circle_responses (request_id, contact_id, encrypted_share)
		VALUES ($1, $2, $3)
		ON CONFLICT (request_id, contact_id) DO NOTHING
	`, requestID, userID, encShare)
	if err != nil {
		h.log.Error("Failed to insert social recovery response", "error", err)
		return 0, errMsgFailedSubmitResponse, http.StatusInternalServerError
	}
	rowsAffected, _ := res.RowsAffected()
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
		h.log.Error("Failed to update shares_received", "error", err, "request_id", requestID)
		return 0, errMsgFailedSubmitResponse, http.StatusInternalServerError
	}

	if sharesReceived >= thresholdK {
		_, err = tx.ExecContext(ctx, `
			UPDATE recovery_circle_requests SET status = 'complete' WHERE id = $1
		`, requestID)
		if err != nil {
			h.log.Error("Failed to mark social recovery request as complete", "error", err, "request_id", requestID)
			return 0, errMsgFailedSubmitResponse, http.StatusInternalServerError
		}
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit social recovery response transaction", "error", err, "request_id", requestID)
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
