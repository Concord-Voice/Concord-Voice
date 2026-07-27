// Package auth provides authentication and authorization functionality including user registration and login.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/attestation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/email"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// SessionDisconnector allows forcefully disconnecting WebSocket connections.
// Defined as an interface to avoid an import cycle (websocket imports auth).
type SessionDisconnector interface {
	DisconnectUser(userID uuid.UUID)
}

// RecoveryClaims contains the user_id extracted from a validated recovery token.
type RecoveryClaims struct {
	UserID string
	JTI    string
}

// MFAChecker checks if a user has MFA enabled and returns their methods.
// Defined as an interface to avoid circular imports (mfa imports auth).
type MFAChecker interface {
	IsEnabled(ctx context.Context, userID string) bool
	GetEnabledMethods(ctx context.Context, userID string) ([]string, error)
	// GetLoginMethods returns methods eligible for login (excludes recovery-only methods).
	GetLoginMethods(ctx context.Context, userID string) ([]string, error)
	// GenerateLoginChallenge creates a challenge token for the two-step login flow.
	// Returns the signed JWT and the JTI. Stores remember_me in Redis keyed by JTI.
	// credEpoch is the user's durable credential epoch at authorization time (#2418);
	// CompleteLogin re-checks it before minting, so an unstamped challenge is refused
	// for any user who has rotated.
	GenerateLoginChallenge(ctx context.Context, userID string, rememberMe bool, credEpoch string) (token string, jti string, err error)
	// GenerateUpgradeChallenge creates a challenge token for pre-MFA sessions that
	// need to verify MFA before continuing. Issues fresh tokens on success.
	GenerateUpgradeChallenge(ctx context.Context, userID string, rememberMe bool) (token string, jti string, err error)
	// BeginWebAuthnLogin starts a WebAuthn assertion ceremony and stores session data
	// in Redis keyed by the challenge JTI. Returns credential request options for the client.
	// Returns nil options if user has no WebAuthn credentials.
	BeginWebAuthnLogin(ctx context.Context, userID string, jti string) (options interface{}, err error)
	// GenerateRecoveryToken creates a recovery-purpose JWT with a 25-hour TTL.
	GenerateRecoveryToken(userID string) (token string, jti string, err error)
	// ValidateRecoveryToken validates a recovery-purpose JWT and returns the claims.
	ValidateRecoveryToken(tokenString string) (*RecoveryClaims, error)
}

const (
	// maxUserAgentLength limits User-Agent storage to reduce metadata footprint.
	maxUserAgentLength = 256

	errMsgInvalidRequestBody     = "Invalid request body"
	errMsgFailedStartTransaction = "Failed to start transaction"
	errMsgFailedRollbackTx       = "Failed to rollback transaction"
	errMsgFailedCreateAccount    = "Failed to create account"
	errMsgLoginFailed            = "Login failed"
	errMsgInvalidCredentials     = "Invalid credentials"   //nolint:gosec // G101 false positive: error message text, not a credential
	errMsgInvalidRefreshToken    = "Invalid refresh token" //nolint:gosec // G101 false positive: error message text, not a credential

	errFailedCommitTransaction     = "Failed to commit transaction"
	errInvalidExpiredRecoveryCode  = "Invalid or expired recovery code"
	errRecoveryVerificationFailed  = "Recovery verification failed"
	errRecoveryNotConfigured       = "Recovery not configured"
	errInvalidExpiredRecoveryToken = "Invalid or expired recovery token" //nolint:gosec // G101 false positive: error message, not a credential
	errFailedResetPwd              = "Failed to reset password"          //nolint:gosec // G101 false positive: error message, not a credential
	errFailedResetAccount          = "Failed to reset account"
	errFailedCreateRecoveryRequest = "Failed to create recovery request"
	bearerPrefix                   = "Bearer "

	// HTTP header names
	headerMachineID  = "X-Machine-Id"
	headerUserAgent  = "User-Agent"
	headerDeviceName = "X-Device-Name"

	// Additional error messages
	errMsgRefreshFailed      = "Refresh failed"
	errMsgFailedAccessToken  = "Failed to generate access token"
	errMsgFailedRefreshToken = "Failed to generate refresh token"

	// Redis key patterns
	redisKeyLoginLockout = "login_lockout:%s"
)

// truncateUserAgent limits the User-Agent string to maxUserAgentLength.
func truncateUserAgent(ua string) string {
	if len(ua) > maxUserAgentLength {
		return ua[:maxUserAgentLength]
	}
	return ua
}

// nilIfEmpty returns nil for empty strings (maps to SQL NULL) or the string pointer.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// SetRefreshCookie sets a refresh_token cookie with secure defaults
// (HttpOnly + SameSite=Lax + Secure when TLS is detected). Exported so
// internal/oauth's SSO handlers can attach the same cookie attributes when
// they issue a session via the AuthAdapter — keeping cookie semantics
// uniform across both /auth/login and /auth/sso/* code paths.
func SetRefreshCookie(c *gin.Context, value string, maxAge int) {
	secure := c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https"
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("refresh_token", value, maxAge, "/", "", secure, true)
}

// Handler handles authentication-related requests including registration and login.
type Handler struct {
	db              *sql.DB
	redis           *redis.Client
	log             *logger.Logger
	jwtSecret       string
	hub             SessionDisconnector
	presenceHistory *presencehistory.Service
	mfaChecker      MFAChecker
	emailSvc        *email.Service
	pending         *PendingRepo
	entCache        *entitlements.Cache
	credFence       *credepoch.Fence
}

// SetCredentialFence injects the per-user credential-epoch fence (#2201).
// The recovery reset flows FAIL CLOSED (503) when it is absent — a destructive
// credential flow must never silently skip epoch rotation.
func (h *Handler) SetCredentialFence(f *credepoch.Fence) {
	h.credFence = f
}

// ErrContinuationEpochAdvanced signals that a concurrent destructive credential
// flow advanced the user's epoch between the caller's committed rotation and
// this best-effort continuation mint — so no pair is issued (#2201). The caller
// drops the continuation pair and the client re-authenticates.
var ErrContinuationEpochAdvanced = errors.New("continuation epoch advanced")

// IssueTokenPair mints an access+refresh pair for the post-rotation continuation
// contract (#2201), BOUND to expectedEpoch — the epoch the caller's destructive
// flow just committed. Called AFTER that flow's transaction commits, it locks the
// user row FOR NO KEY UPDATE and refuses to mint if a SECOND destructive flow
// advanced the epoch in the window between the caller's commit and this mint
// (else the earlier request would receive a live pair the later flow meant to
// terminate — the same "mint reads the current epoch without a lock" gap the
// refresh-rotation fix closes). On advance it returns
// ErrContinuationEpochAdvanced; the access token and refresh row are minted under
// the locked epoch so the pair cannot outlive the rotation it belongs to.
func (h *Handler) IssueTokenPair(c *gin.Context, userID, expectedEpoch string) (accessToken, refreshToken, sessionID string, err error) {
	ctx := c.Request.Context()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", "", fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var emailVerified bool
	var epoch sql.NullString
	if err = tx.QueryRowContext(ctx,
		`SELECT email_verified, credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID,
	).Scan(&emailVerified, &epoch); err != nil {
		return "", "", "", fmt.Errorf("lookup: %w", err)
	}
	if credepoch.MatchEpoch(epoch, expectedEpoch) != nil {
		return "", "", "", ErrContinuationEpochAdvanced
	}

	sessionID = uuid.New().String()
	tier := h.entCache.GetTier(ctx, userID)
	accessToken, err = GenerateAccessToken(userID, h.jwtSecret, emailVerified, epoch.String, sessionID, tier)
	if err != nil {
		return "", "", "", fmt.Errorf("access: %w", err)
	}
	refreshToken, err = GenerateRefreshToken()
	if err != nil {
		return "", "", "", fmt.Errorf("refresh: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		sessionID, userID, HashRefreshToken(refreshToken), c.GetHeader(headerDeviceName),
		c.ClientIP(), truncateUserAgent(c.GetHeader(headerUserAgent)),
		time.Now().Add(30*24*time.Hour), true, nilIfEmpty(c.GetHeader(headerMachineID)),
	); err != nil {
		return "", "", "", fmt.Errorf("store: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return "", "", "", fmt.Errorf("commit: %w", err)
	}
	return accessToken, refreshToken, sessionID, nil
}

// NewHandler creates a new authentication handler.
func NewHandler(db *sql.DB, redisClient *redis.Client, log *logger.Logger, jwtSecret string, hub SessionDisconnector) *Handler {
	return NewHandlerForInstance(db, redisClient, log, jwtSecret, hub, "")
}

// NewHandlerForInstance creates a new authentication handler with the
// deployment-mode entitlement seam used for JWT tier claims.
func NewHandlerForInstance(db *sql.DB, redisClient *redis.Client, log *logger.Logger, jwtSecret string, hub SessionDisconnector, instanceType string) *Handler {
	return &Handler{
		db:        db,
		redis:     redisClient,
		log:       log,
		jwtSecret: jwtSecret,
		hub:       hub,
		pending:   NewPendingRepo(db),
		entCache:  entitlements.NewCacheForInstance(redisClient, db, instanceType),
	}
}

// SetPresenceHistory injects the concrete service shared with users writers,
// acknowledged delivery, reconciliation, and reconnect snapshots.
func (h *Handler) SetPresenceHistory(service *presencehistory.Service) {
	h.presenceHistory = service
}

// SetEmailService sets the email service (called after initialization to avoid circular deps).
func (h *Handler) SetEmailService(svc *email.Service) {
	h.emailSvc = svc
}

// SetMFAChecker sets the MFA checker (called after both handlers are initialized to break circular init).
func (h *Handler) SetMFAChecker(checker MFAChecker) {
	h.mfaChecker = checker
}

// RegisterRequest represents registration payload
type RegisterRequest struct {
	Email             string `json:"email" binding:"required,email"`
	Username          string `json:"username" binding:"required,min=3,max=50"`
	Password          string `json:"password" binding:"required"` // #nosec G117 -- False positive: request field, not stored secret
	AgeConfirmation   bool   `json:"age_confirmation" binding:"required"`
	PublicKey         string `json:"public_key" binding:"required"`          // base64 encoded SPKI
	WrappedPrivateKey string `json:"wrapped_private_key" binding:"required"` // base64 encoded
	KeyDerivationSalt string `json:"key_derivation_salt" binding:"required"` // base64 encoded
	KeyDerivationAlg  string `json:"key_derivation_alg"`                     // "pbkdf2" or "argon2id"; defaults to "argon2id"
}

// LoginRequest represents login payload
type LoginRequest struct {
	Email      string `json:"email" binding:"required,email"`
	Password   string `json:"password" binding:"required"` // #nosec G117 -- False positive: request field, not stored secret
	RememberMe *bool  `json:"remember_me"`                 // nil defaults to true for backward compat
}

func (h *Handler) validateRegistration(req *RegisterRequest) error {
	if !req.AgeConfirmation {
		return fmt.Errorf("You must be at least 16 years old to create an account") //nolint:staticcheck // ST1005: user-facing message rendered directly in UI
	}
	if err := ValidateUsername(req.Username); err != nil {
		return err
	}
	if err := ValidatePasswordStrength(req.Password); err != nil {
		return err
	}
	return nil
}

func decodeE2EEKeys(publicKeyB64, wrappedKeyB64, saltB64 string) (publicKey, wrappedKey, salt []byte, err error) {
	publicKey, err = base64.StdEncoding.DecodeString(publicKeyB64)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("Invalid public key format") //nolint:staticcheck // ST1005: user-facing message rendered directly in UI
	}
	wrappedKey, err = base64.StdEncoding.DecodeString(wrappedKeyB64)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("Invalid wrapped private key format") //nolint:staticcheck // ST1005: user-facing message rendered directly in UI
	}
	salt, err = base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("Invalid salt format") //nolint:staticcheck // ST1005: user-facing message rendered directly in UI
	}
	return publicKey, wrappedKey, salt, nil
}

// generateTokenPair creates an access+refresh token pair and persists the
// refresh token. Its ONLY caller is issueSessionTokens ← ConfirmRegistration,
// which mints the FIRST session for a brand-new user (credential_epoch NULL).
//
// #2418 caution: this reads the epoch and INSERTs the refresh row in two
// UNFENCED statements (no user-row lock, no transaction) — the same shape the
// CompleteLogin fix replaced. That is safe HERE only because a user being
// created for the first time has no pre-reset credential a destructive reset
// could advance the epoch past. Do NOT reuse this for any flow that mints for an
// EXISTING user (SSO, recovery, re-login) without first binding it to the
// authorizing epoch under FOR NO KEY UPDATE, exactly as CompleteLogin and
// IssueTokenPair do — otherwise the pre-reset-mint race this issue closes reopens.
func (h *Handler) generateTokenPair(c *gin.Context, userID string, emailVerified bool) (accessToken, refreshToken, tokenID string, err error) {
	// #2201: embed the user's current credential epoch (fail closed — no mint
	// without epoch knowledge) and the new refresh-session id in the access token.
	var credEpoch sql.NullString
	if err = h.db.QueryRow(`SELECT credential_epoch FROM users WHERE id = $1`, userID).Scan(&credEpoch); err != nil {
		return "", "", "", fmt.Errorf("epoch lookup: %w", err)
	}
	tokenID = uuid.New().String()
	tier := h.entCache.GetTier(c.Request.Context(), userID)
	accessToken, err = GenerateAccessToken(userID, h.jwtSecret, emailVerified, credEpoch.String, tokenID, tier)
	if err != nil {
		return "", "", "", fmt.Errorf("access: %w", err)
	}
	refreshToken, err = GenerateRefreshToken()
	if err != nil {
		return "", "", "", fmt.Errorf("refresh: %w", err)
	}

	tokenHash := HashRefreshToken(refreshToken)
	expiresAt := time.Now().Add(30 * 24 * time.Hour)
	machineID := c.GetHeader(headerMachineID)

	_, err = h.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tokenID, userID, tokenHash, c.GetHeader(headerDeviceName),
		c.ClientIP(), truncateUserAgent(c.GetHeader(headerUserAgent)),
		expiresAt, true, nilIfEmpty(machineID),
	)
	if err != nil {
		return "", "", "", fmt.Errorf("store: %w", err)
	}
	return accessToken, refreshToken, tokenID, nil
}

// Register handles new user registration by creating a pending_registrations row
// and emailing a verification code. The user is NOT promoted to the users table
// until ConfirmRegistration succeeds. See #621.
func (h *Handler) Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	if err := h.validateRegistration(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	publicKey, wrappedKey, salt, err := decodeE2EEKeys(req.PublicKey, req.WrappedPrivateKey, req.KeyDerivationSalt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	passwordHash, err := HashPassword(req.Password)
	if err != nil {
		h.log.Error("Failed to hash password", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateAccount})
		return
	}

	normalizedEmail := strings.ToLower(strings.TrimSpace(req.Email))
	normalizedUsername := NormalizeUsername(req.Username)

	pendingID, expiresAt, _, err := h.pending.InsertOrTakeover(
		c.Request.Context(),
		InsertParams{
			Email:             normalizedEmail,
			Username:          normalizedUsername,
			PasswordHash:      passwordHash,
			WrappedPrivateKey: wrappedKey,
			KeyDerivationSalt: salt,
			KeyDerivationAlg:  req.KeyDerivationAlg,
			PublicKey:         publicKey,
		},
		req.Password,
	)
	if err != nil {
		h.respondPendingError(c, err)
		return
	}

	// Generate + send code synchronously. On failure, roll back the pending
	// insert so the user can retry cleanly.
	codeExpires, err := h.sendInitialCode(c.Request.Context(), pendingID, normalizedEmail)
	if err != nil {
		if _, delErr := h.pending.Delete(c.Request.Context(), pendingID); delErr != nil {
			h.log.Error("Failed to roll back pending after email send error", "error", delErr)
		}
		h.log.Error("Failed to send verification code", "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Failed to send verification code; please retry",
		})
		return
	}

	h.log.Info("Pending registration created",
		"pending_id", pendingID, "username", normalizedUsername)

	c.JSON(http.StatusCreated, gin.H{
		"pending_id":      pendingID,
		"email":           normalizedEmail,
		"expires_at":      expiresAt.UTC().Format(time.RFC3339),
		"code_expires_at": codeExpires.UTC().Format(time.RFC3339),
	})
}

// respondPendingError maps PendingRepo sentinel errors to API responses.
func (h *Handler) respondPendingError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrEmailAlreadyRegister):
		c.JSON(http.StatusConflict, gin.H{
			"code":  "email_already_registered",
			"error": "An account with this email exists. Try logging in or recovering your password.",
		})
	case errors.Is(err, ErrEmailPending):
		c.JSON(http.StatusConflict, gin.H{
			"code":  "registration_pending",
			"error": "A verification code was already sent to this email. Wait 15 minutes or use the correct password to take over.",
		})
	case errors.Is(err, ErrUsernameTaken):
		c.JSON(http.StatusConflict, gin.H{
			"code":  "username_taken",
			"error": "This username is already in use.",
		})
	default:
		h.log.Error("pending registration error", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedCreateAccount})
	}
}

// isTestEnv reports whether the process is running in the integration test
// environment. Guards test-only side effects like writing plaintext
// verification codes to Redis under `test_only:<pending_id>`.
func isTestEnv() bool {
	return os.Getenv("CONCORD_ENV") == "test"
}

func (h *Handler) extendPendingExpiryForCode(ctx context.Context, pendingID string, requestedCodeExpires time.Time) (time.Time, error) {
	var codeExpires time.Time
	err := h.db.QueryRowContext(ctx,
		`UPDATE pending_registrations
		    SET expires_at = GREATEST(expires_at, LEAST($2, created_at + ($3 * INTERVAL '1 second')))
		  WHERE id = $1
		    AND expires_at > NOW()
		    AND created_at + ($3 * INTERVAL '1 second') > NOW()
		  RETURNING LEAST($2, created_at + ($3 * INTERVAL '1 second'))`,
		pendingID, requestedCodeExpires, int(PendingRegistrationTTL/time.Second),
	).Scan(&codeExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrPendingExpired
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("extend pending expiry: %w", err)
	}
	return codeExpires, nil
}

// sendInitialCode generates a fresh 6-digit code, writes the hash to Redis
// under email_verify:<pending_id>, and emails it via the verification
// handler's email service. Returns the code's expiry.
//
// When CONCORD_ENV=test, the plaintext code is additionally written to
// test_only:<pending_id> with the same TTL so integration tests can recover
// it via testhelpers.FetchVerificationCode without reading real email.
func (h *Handler) sendInitialCode(ctx context.Context, pendingID, userEmail string) (time.Time, error) {
	if h.emailSvc == nil {
		return time.Time{}, fmt.Errorf("email service not configured")
	}

	requestedCodeExpires := time.Now().Add(VerifyCodeTTLNew)
	codeExpires, err := h.extendPendingExpiryForCode(ctx, pendingID, requestedCodeExpires)
	if err != nil {
		return time.Time{}, err
	}
	codeTTL := time.Until(codeExpires)
	// The email template promises VerifyCodeTTLNew. If the absolute pending
	// lifetime cannot honor that full window, make the user restart instead.
	if codeTTL <= 0 || codeTTL < VerifyCodeTTLNew-time.Second {
		return time.Time{}, ErrPendingExpired
	}

	code, err := generateCode()
	if err != nil {
		return time.Time{}, fmt.Errorf("generate code: %w", err)
	}

	record := verificationRecord{
		CodeHash: hashCode(code),
		Email:    userEmail,
		Attempts: 0,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return time.Time{}, fmt.Errorf("marshal verification record: %w", err)
	}

	if err := h.redis.Del(ctx, verificationAttemptsKey(pendingID)).Err(); err != nil {
		return time.Time{}, fmt.Errorf("reset verification attempts: %w", err)
	}
	if err := h.redis.Set(ctx, redisKey(pendingID), raw, codeTTL).Err(); err != nil {
		return time.Time{}, fmt.Errorf("store code in Redis: %w", err)
	}

	// Test-only: expose plaintext code for integration tests. Guarded by the
	// CONCORD_ENV=test env var so this never fires in production.
	if isTestEnv() {
		if err := h.redis.Set(ctx, redisKeyTestOnly+pendingID, code, codeTTL).Err(); err != nil {
			h.log.Warn("Failed to write test_only code", "error", err)
		}
	}

	if err := h.emailSvc.SendVerificationCode(userEmail, code); err != nil {
		// Best-effort cleanup so a retry starts fresh.
		h.clearVerificationCode(ctx, pendingID)
		return time.Time{}, fmt.Errorf("send email: %w", err)
	}

	return codeExpires, nil
}

const (
	// redisKeyTestOnly is the Redis key prefix for plaintext verification codes
	// written in test environments only (CONCORD_ENV=test).
	redisKeyTestOnly = "test_only:"

	// errMsgInternalError is the generic internal error message returned to clients.
	errMsgInternalError = "Internal error"
)

const reserveVerificationAttemptLua = `
local code = redis.call("GET", KEYS[1])
if not code then
  return {0, 0, ""}
end
local attempts = redis.call("INCR", KEYS[2])
if redis.call("PTTL", KEYS[2]) < 0 then
  local code_ttl = redis.call("PTTL", KEYS[1])
  if code_ttl > 0 then
    redis.call("PEXPIRE", KEYS[2], code_ttl)
  else
    redis.call("PEXPIRE", KEYS[2], ARGV[1])
  end
end
return {1, attempts, code}
`

// ConfirmRegistrationRequest is the JSON body for the register/confirm endpoint.
type ConfirmRegistrationRequest struct {
	PendingID string `json:"pending_id" binding:"required,uuid"`
	Code      string `json:"code" binding:"required"`
}

// ConfirmRegistration validates the email verification code for a pending registration,
// promotes it to a full user account, and returns tokens. See #621.
func (h *Handler) ConfirmRegistration(c *gin.Context) {
	req, sanitized, ok := h.validateConfirmRequest(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()

	if _, err := h.pending.GetByID(ctx, req.PendingID); err != nil {
		switch {
		case errors.Is(err, ErrPendingNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": "pending_not_found"})
		case errors.Is(err, ErrPendingExpired):
			c.JSON(http.StatusGone, gin.H{"code": "pending_expired"})
		default:
			h.log.Error("confirm: lookup failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalError})
		}
		return
	}

	if !h.attemptsGuard(ctx, c, req.PendingID, sanitized) {
		return
	}

	userID, err := h.pending.Promote(ctx, req.PendingID)
	if err != nil {
		if errors.Is(err, ErrPendingExpired) {
			c.JSON(http.StatusGone, gin.H{"code": "pending_expired"})
			return
		}
		h.log.Error("confirm: promotion failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalError})
		return
	}

	access, refresh, sessionID, err := h.issueSessionTokens(c, userID)
	if err != nil {
		return
	}

	h.clearVerificationCode(ctx, req.PendingID)

	email, username, err := h.loadPromotedUser(ctx, userID)
	if err != nil {
		h.log.Error("confirm: failed to load promoted user", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalError})
		return
	}

	h.writeConfirmSuccess(c, access, refresh, sessionID, confirmedUser{id: userID, email: email, username: username})
}

// loadPromotedUser fetches the email and username for a newly promoted user.
// Extracted to keep ConfirmRegistration within complexity budget.
func (h *Handler) loadPromotedUser(ctx context.Context, userID string) (email, username string, err error) {
	err = h.db.QueryRowContext(ctx,
		`SELECT email, username FROM users WHERE id = $1`,
		userID).Scan(&email, &username)
	return
}

// validateConfirmRequest binds and sanitizes the ConfirmRegistration JSON body.
// Returns (req, sanitizedCode, true) on success; writes the error response and
// returns (nil, "", false) on failure.
func (h *Handler) validateConfirmRequest(c *gin.Context) (*ConfirmRegistrationRequest, string, bool) {
	var req ConfirmRegistrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return nil, "", false
	}
	sanitized := sanitizeVerificationCode(req.Code)
	if !isValidVerificationCode(sanitized) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Code must be 6 digits"})
		return nil, "", false
	}
	return &req, sanitized, true
}

// attemptsGuard reserves an attempt atomically before comparing the code.
// It fetches the verification record after reservation so resend/change-email
// cannot leave a stale code eligible for promotion.
// Returns true if the code matched and the caller should proceed; false if a response
// has already been written (too_many_attempts or invalid_code).
func (h *Handler) attemptsGuard(ctx context.Context, c *gin.Context, pendingID string, sanitized string) bool {
	rec, attempts, err := h.reserveVerificationAttempt(ctx, pendingID)
	if errors.Is(err, redis.Nil) {
		if h.isVerificationExhausted(ctx, pendingID) {
			c.JSON(http.StatusTooManyRequests, gin.H{"code": "too_many_attempts"})
			return false
		}
		h.clearVerificationCode(ctx, pendingID)
		c.JSON(http.StatusGone, gin.H{"code": "code_expired"})
		return false
	}
	if err != nil {
		h.log.Error("confirm: attempts reservation failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalError})
		return false
	}
	if attempts > MaxCodeAttempts {
		h.exhaustVerificationCode(ctx, pendingID)
		c.JSON(http.StatusTooManyRequests, gin.H{"code": "too_many_attempts"})
		return false
	}

	if subtle.ConstantTimeCompare([]byte(hashCode(sanitized)), []byte(rec.CodeHash)) == 1 {
		return true
	}

	remaining := MaxCodeAttempts - attempts
	if remaining < 0 {
		remaining = 0
	}
	c.JSON(http.StatusUnauthorized, gin.H{
		"code":               "invalid_code",
		"attempts_remaining": remaining,
	})
	return false
}

func (h *Handler) reserveVerificationAttempt(ctx context.Context, pendingID string) (*verificationRecord, int, error) {
	result, err := h.redis.Eval(ctx, reserveVerificationAttemptLua,
		[]string{redisKey(pendingID), verificationAttemptsKey(pendingID)},
		int64(VerifyCodeTTLNew/time.Millisecond),
	).Result()
	if err != nil {
		return nil, 0, err
	}

	values, ok := result.([]interface{})
	if !ok || len(values) != 3 {
		return nil, 0, fmt.Errorf("unexpected verification attempt script result: %T", result)
	}
	found, ok := values[0].(int64)
	if !ok {
		return nil, 0, fmt.Errorf("unexpected verification attempt found flag: %T", values[0])
	}
	if found == 0 {
		return nil, 0, redis.Nil
	}
	attemptsRaw, ok := values[1].(int64)
	if !ok {
		return nil, 0, fmt.Errorf("unexpected verification attempt count: %T", values[1])
	}
	raw, ok := values[2].(string)
	if !ok {
		return nil, 0, fmt.Errorf("unexpected verification record payload: %T", values[2])
	}

	var rec verificationRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, 0, fmt.Errorf("unmarshal verification record: %w", err)
	}
	return &rec, int(attemptsRaw), nil
}

func (h *Handler) isVerificationExhausted(ctx context.Context, pendingID string) bool {
	attempts, err := h.redis.Get(ctx, verificationAttemptsKey(pendingID)).Int()
	return err == nil && attempts >= MaxCodeAttempts
}

func (h *Handler) verificationCodeTTL(ctx context.Context, pendingID string) time.Duration {
	ttl := h.redis.TTL(ctx, redisKey(pendingID)).Val()
	if ttl <= 0 {
		return VerifyCodeTTLNew
	}
	return ttl
}

func (h *Handler) clearVerificationCode(ctx context.Context, pendingID string) {
	_ = h.redis.Del(ctx, redisKey(pendingID), redisKeyTestOnly+pendingID, verificationAttemptsKey(pendingID)).Err()
}

func (h *Handler) exhaustVerificationCode(ctx context.Context, pendingID string) {
	ttl := h.verificationCodeTTL(ctx, pendingID)
	_ = h.redis.Set(ctx, verificationAttemptsKey(pendingID), MaxCodeAttempts, ttl).Err()
	_ = h.redis.Del(ctx, redisKey(pendingID), redisKeyTestOnly+pendingID).Err()
}

// issueSessionTokens generates an access+refresh token pair and sets the refresh
// cookie. On error it writes a 500 and returns a non-nil error.
func (h *Handler) issueSessionTokens(c *gin.Context, userID string) (access, refresh, sessionID string, err error) {
	access, refresh, sessionID, err = h.generateTokenPair(c, userID, true)
	if err != nil {
		h.log.Error("confirm: token issuance failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalError})
		return
	}
	SetRefreshCookie(c, refresh, 30*24*60*60)
	return
}

// confirmedUser bundles the promoted user fields needed for the confirm-success response.
type confirmedUser struct {
	id       string
	email    string
	username string
}

// writeConfirmSuccess writes the 200 JSON response for a successful registration confirmation.
func (h *Handler) writeConfirmSuccess(c *gin.Context, access, refresh, sessionID string, u confirmedUser) {
	c.JSON(http.StatusOK, gin.H{
		"access_token":  access,
		"refresh_token": refresh,
		"session_id":    sessionID,
		"expires_in":    900,
		"remember_me":   true,
		"user": gin.H{
			"id":             u.id,
			"username":       u.username,
			"email":          u.email,
			"email_verified": true,
		},
	})
}

// ResendRegistrationRequest is the JSON body for the register/resend endpoint.
type ResendRegistrationRequest struct {
	PendingID string `json:"pending_id" binding:"required,uuid"`
}

// ResendRegistrationCode re-sends the email verification code for a pending registration.
// Enforces cooldown and max-resend limits. See #621.
func (h *Handler) ResendRegistrationCode(c *gin.Context) {
	var req ResendRegistrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}
	ctx := c.Request.Context()

	pending, err := h.pending.GetByID(ctx, req.PendingID)
	if err != nil {
		switch {
		case errors.Is(err, ErrPendingNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": "pending_not_found"})
		case errors.Is(err, ErrPendingExpired):
			c.JSON(http.StatusGone, gin.H{"code": "pending_expired"})
		default:
			h.log.Error("resend: lookup failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalError})
		}
		return
	}

	newCount, err := h.pending.IncrementResend(ctx, pending.ID)
	if err != nil {
		switch {
		case errors.Is(err, ErrResendsExhausted):
			c.JSON(http.StatusTooManyRequests, gin.H{"code": "resends_exhausted"})
		case errors.Is(err, ErrResendCooldown):
			c.JSON(http.StatusTooManyRequests, gin.H{
				"code":                "cooldown_active",
				"retry_after_seconds": int(ResendCooldown.Seconds()),
			})
		case errors.Is(err, ErrPendingExpired):
			c.JSON(http.StatusGone, gin.H{"code": "pending_expired"})
		default:
			h.log.Error("resend: increment failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalError})
		}
		return
	}

	h.clearVerificationCode(ctx, pending.ID)
	codeExpires, err := h.sendInitialCode(ctx, pending.ID, pending.Email)
	if err != nil {
		_ = h.pending.RevertResend(ctx, pending.ID)
		if errors.Is(err, ErrPendingExpired) {
			c.JSON(http.StatusGone, gin.H{"code": "pending_expired"})
			return
		}
		h.log.Error("resend: email send failed", "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Failed to send verification code; please retry",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code_expires_at":   codeExpires.UTC().Format(time.RFC3339),
		"resends_remaining": MaxResends - newCount,
	})
}

// ChangeRegistrationEmailRequest is the JSON body for the register/change-email endpoint.
type ChangeRegistrationEmailRequest struct {
	PendingID string `json:"pending_id" binding:"required,uuid"`
	NewEmail  string `json:"new_email" binding:"required,email"`
}

// ChangeRegistrationEmail updates the email on a pending registration, resets the
// resend counter, and sends a fresh verification code to the new address. See #621.
func (h *Handler) ChangeRegistrationEmail(c *gin.Context) {
	var req ChangeRegistrationEmailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}
	ctx := c.Request.Context()

	if err := h.pending.UpdateEmail(ctx, req.PendingID, req.NewEmail); err != nil {
		switch {
		case errors.Is(err, ErrPendingNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": "pending_not_found"})
		case errors.Is(err, ErrPendingExpired):
			c.JSON(http.StatusGone, gin.H{"code": "pending_expired"})
		case errors.Is(err, ErrEmailAlreadyRegister):
			c.JSON(http.StatusConflict, gin.H{"code": "email_already_registered"})
		case errors.Is(err, ErrEmailPending):
			c.JSON(http.StatusConflict, gin.H{"code": "registration_pending"})
		default:
			h.log.Error("change-email: update failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalError})
		}
		return
	}

	h.clearVerificationCode(ctx, req.PendingID)
	newEmailLower := strings.ToLower(strings.TrimSpace(req.NewEmail))
	codeExpires, err := h.sendInitialCode(ctx, req.PendingID, newEmailLower)
	if err != nil {
		if errors.Is(err, ErrPendingExpired) {
			c.JSON(http.StatusGone, gin.H{"code": "pending_expired"})
			return
		}
		h.log.Error("change-email: send failed", "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Failed to send verification code to new email",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"email":           newEmailLower,
		"code_expires_at": codeExpires.UTC().Format(time.RFC3339),
	})
}

// AbandonRegistration deletes a pending registration by ID, allowing the user to
// start fresh. Returns 204 on success, 404 if not found. See #621.
func (h *Handler) AbandonRegistration(c *gin.Context) {
	pendingID := c.Param("pending_id")
	if _, err := uuid.Parse(pendingID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid pending_id"})
		return
	}
	ctx := c.Request.Context()

	deleted, err := h.pending.Delete(ctx, pendingID)
	if err != nil {
		h.log.Error("abandon: delete failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalError})
		return
	}
	h.clearVerificationCode(ctx, pendingID)

	if !deleted {
		c.Status(http.StatusNotFound)
		return
	}
	c.Status(http.StatusNoContent)
}

// loginLockoutThreshold is the number of failed attempts before lockout.
const loginLockoutThreshold = 5

// loginLockoutDurations defines escalating lockout windows.
var loginLockoutDurations = []time.Duration{15 * time.Minute, 30 * time.Minute, 60 * time.Minute}

// checkLoginLockout returns true if the account is currently locked out.
func (h *Handler) checkLoginLockout(ctx context.Context, email string) bool {
	key := fmt.Sprintf(redisKeyLoginLockout, email)
	if _, err := h.redis.Get(ctx, key).Result(); err == nil {
		return true // lockout key exists
	}
	return false
}

// recordFailedLogin increments the failure counter and applies lockout if threshold is reached.
func (h *Handler) recordFailedLogin(ctx context.Context, email string) {
	attemptsKey := fmt.Sprintf("login_attempts:%s", email)
	lockoutKey := fmt.Sprintf(redisKeyLoginLockout, email)
	lockoutCountKey := fmt.Sprintf("login_lockout_count:%s", email)

	count, _ := h.redis.Incr(ctx, attemptsKey).Result()
	// Set a 1-hour window for the attempts counter to auto-expire
	h.redis.Expire(ctx, attemptsKey, 1*time.Hour)

	if count >= int64(loginLockoutThreshold) {
		// Determine lockout duration (escalating)
		lockoutCount, _ := h.redis.Incr(ctx, lockoutCountKey).Result()
		h.redis.Expire(ctx, lockoutCountKey, 24*time.Hour)
		idx := int(lockoutCount) - 1
		if idx >= len(loginLockoutDurations) {
			idx = len(loginLockoutDurations) - 1
		}
		duration := loginLockoutDurations[idx]

		h.redis.Set(ctx, lockoutKey, "1", duration)
		h.redis.Del(ctx, attemptsKey) // Reset attempts counter
	}
}

// clearLoginAttempts resets counters on successful login.
func (h *Handler) clearLoginAttempts(ctx context.Context, email string) {
	h.redis.Del(ctx,
		fmt.Sprintf("login_attempts:%s", email),
		fmt.Sprintf(redisKeyLoginLockout, email),
		fmt.Sprintf("login_lockout_count:%s", email),
	)
}

// Login handles user authentication
// lookupUserForLogin reads the login candidate plus the credential epoch the
// password is about to be verified under. The epoch is returned SEPARATELY rather
// than on models.User: that struct has a PublicUser() client-facing serializer, and
// a revocation tag must not sit one careless edit away from the wire (#2418).
func (h *Handler) lookupUserForLogin(email string) (models.User, string, error) {
	var user models.User
	var credEpoch sql.NullString
	err := h.db.QueryRow(
		`SELECT id, email, username, password_hash, display_name, bio, avatar_url, COALESCE(links, '[]'::jsonb),
		        email_verified, age_verified, created_at, updated_at, password_login_disabled, disabled, credential_epoch
		 FROM users WHERE email = $1`,
		email,
	).Scan(
		&user.ID, &user.Email, &user.Username, &user.PasswordHash,
		&user.DisplayName, &user.Bio, &user.AvatarURL, &user.Links,
		&user.EmailVerified, &user.AgeVerified,
		&user.CreatedAt, &user.UpdatedAt, &user.PasswordLoginDisabled, &user.Disabled, &credEpoch,
	)
	return user, credEpoch.String, err
}

// readCredentialEpoch reads the user's durable credential epoch for stamping into an
// MFA login challenge (#2418). Callers that already hold the epoch from an in-flight
// user read must reuse that value instead of calling this.
//
// Fails closed: on error the caller MUST abort. Substituting "" would mint a claimless
// challenge that CompleteLogin then rejects, turning a transient DB blip into a
// confusing 401 at the end of the MFA flow.
func (h *Handler) readCredentialEpoch(ctx context.Context, userID string) (string, error) {
	var epoch sql.NullString
	if err := h.db.QueryRowContext(ctx,
		`SELECT credential_epoch FROM users WHERE id = $1`, userID,
	).Scan(&epoch); err != nil {
		return "", fmt.Errorf("read credential epoch: %w", err)
	}
	return epoch.String, nil
}

func (h *Handler) verifyCredentials(ctx context.Context, c *gin.Context, email, password, passwordHash string) bool {
	valid, err := VerifyPassword(password, passwordHash)
	if err != nil {
		h.log.Error("Failed to verify password", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return false
	}
	if !valid {
		h.recordFailedLogin(ctx, email)
		middleware.RecordAuthFailure(ctx, h.redis, c.ClientIP(), middleware.DefaultAuthBanConfig())
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidCredentials})
		return false
	}
	return true
}

// credEpoch is the epoch the password was verified under; it is stamped into the
// challenge so CompleteLogin can refuse a completion that races a reset (#2418).
func (h *Handler) handleMFAChallenge(ctx context.Context, c *gin.Context, userID string, rememberMe bool, credEpoch string) {
	// #2450: these were blank-discarded. errcheck honors an explicit `_`, so the
	// linter never flagged them (the review-only class in [internal]rules/backend.md,
	// founding incidents #1142/#1154). On error the response shipped
	// `"methods": []` alongside a VALID mfa_challenge_token, so the client rendered
	// an MFA prompt with no selectable method and nothing was logged — the user is
	// hard-stuck mid-login with zero server-side signal.
	//
	// loginMethods is load-bearing for the response, so its failure is fatal.
	// allMethods only feeds the recovery-only hint, so a failure there degrades
	// to an empty hint rather than blocking a login that can still proceed.
	allMethods, err := h.mfaChecker.GetEnabledMethods(ctx, userID)
	if err != nil {
		h.log.Error("Failed to read enabled MFA methods", "error", err, "user_id", userID)
	}
	loginMethods, err := h.mfaChecker.GetLoginMethods(ctx, userID)
	if err != nil {
		h.log.Error("Failed to read MFA login methods", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}
	challengeToken, jti, mfaErr := h.mfaChecker.GenerateLoginChallenge(ctx, userID, rememberMe, credEpoch)
	if mfaErr != nil {
		h.log.Error("Failed to generate MFA challenge", "error", mfaErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	recoveryOnly := computeRecoveryOnlyMethods(allMethods, loginMethods)

	h.log.Info("MFA required for login", "user_id", userID, "methods", loginMethods)
	resp := gin.H{
		"mfa_required":        true,
		"mfa_challenge_token": challengeToken,
		"methods":             loginMethods,
	}
	if len(recoveryOnly) > 0 {
		resp["recovery_only_methods"] = recoveryOnly
	}
	addWebAuthnOptions(ctx, h, resp, loginMethods, userID, jti)
	c.JSON(http.StatusOK, resp)
}

func addWebAuthnOptions(ctx context.Context, h *Handler, resp gin.H, loginMethods []string, userID, jti string) {
	for _, m := range loginMethods {
		if m == "webauthn" {
			if opts, err := h.mfaChecker.BeginWebAuthnLogin(ctx, userID, jti); err != nil {
				h.log.Error("Failed to begin WebAuthn login", "error", err)
			} else if opts != nil {
				resp["webauthn_options"] = opts
			}
			return
		}
	}
}

func resolveRememberMe(req *LoginRequest) bool {
	if req.RememberMe != nil {
		return *req.RememberMe
	}
	return true
}

// Login authenticates a user and issues access and refresh tokens.
func (h *Handler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	ctx := c.Request.Context()

	if h.checkLoginLockout(ctx, req.Email) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidCredentials})
		return
	}

	user, loginEpoch, err := h.lookupUserForLogin(req.Email)
	if err == sql.ErrNoRows {
		h.recordFailedLogin(ctx, req.Email)
		middleware.RecordAuthFailure(ctx, h.redis, c.ClientIP(), middleware.DefaultAuthBanConfig())
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidCredentials})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch user", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// SSO-only account: surface a helpful error so the renderer can swap the
	// password form for the SSO button. Does NOT engage the lockout counter —
	// lockout is for password-credential-bruteforce, not user-error.
	if user.PasswordLoginDisabled {
		providers, perr := h.listSSOProviders(ctx, user.ID)
		if perr != nil {
			// Differentiate from "no providers linked" (an impossible-state
			// data integrity issue) so the renderer can render an actionable
			// "could not load options" message rather than an empty SSO
			// button list. Returning an empty providers array on transient DB
			// failure would dead-end the user with no recoverable UX.
			h.log.Error("Failed to list SSO providers", "error", perr, "user_id", user.ID)
			c.JSON(http.StatusInternalServerError, gin.H{"error_code": "sso_provider_lookup_failed"})
			return
		}
		if len(providers) == 0 {
			// Data-integrity violation: password_login_disabled = true with no // pragma: allowlist secret
			// SSO identities means the user has no working authentication path.
			// The API surface (PatchSecurity, DeleteSSOIdentity) refuses
			// transitions that would create this state, so reaching here implies
			// direct DB mutation, a buggy migration, or a race that bypassed the
			// row-lock. Surface as 500 with a distinct error_code so observability
			// captures it and the renderer can render an actionable "contact
			// support" message (the renderer's defensive isEmpty branch shows
			// "try again in a moment" wording, which is misleading for a
			// persistent fault — this branch routes around it via a distinct
			// error_code that the renderer maps to a contact-support message).
			h.log.Error("Login dead-end: password disabled but no SSO identities linked",
				"user_id", user.ID)
			c.JSON(http.StatusInternalServerError, gin.H{"error_code": "sso_account_misconfigured"})
			return
		}
		c.JSON(http.StatusForbidden, gin.H{"error_code": "account_uses_sso", "providers": providers})
		return
	}

	if !h.verifyCredentials(ctx, c, req.Email, req.Password, user.PasswordHash) {
		return
	}

	// Age-verification terminal disable (#1623): block disabled accounts AFTER the
	// credential check, so disabled status is never revealed to an unauthenticated
	// prober. CompleteLogin re-checks this (closing the disable-between-MFA-challenge
	// -and-verify race for MFA accounts).
	if user.Disabled {
		c.JSON(http.StatusForbidden, gin.H{"error_code": "account_disabled"})
		return
	}

	h.clearLoginAttempts(ctx, req.Email)
	rememberMe := resolveRememberMe(&req)

	if h.mfaChecker != nil && h.mfaChecker.IsEnabled(ctx, user.ID) {
		h.handleMFAChallenge(ctx, c, user.ID, rememberMe, loginEpoch)
		return
	}

	h.CompleteLogin(c, user.ID, rememberMe, loginEpoch)
}

// CompleteLogin issues tokens and creates a session for the given user.
// Called directly from Login (no MFA) or from the MFA verify handler after successful verification.
func (h *Handler) CompleteLogin(c *gin.Context, userID string, rememberMe bool, expectedEpoch string) {
	// #2418: bind this mint to the epoch the login was AUTHORIZED under — the epoch
	// read alongside the password verification (direct path) or stamped into the MFA
	// challenge at issuance (MFA path). The whole mint runs in ONE transaction holding
	// the users row FOR NO KEY UPDATE, the same lock every destructive reset holds, so
	// a reset racing this login either commits first (we observe the advanced epoch
	// and refuse) or waits for us (and then revokes what we minted). Before this, five
	// unserialized pool statements let a reset committing mid-flight leave a live
	// post-reset session minted from pre-reset authorization.
	ctx := c.Request.Context()
	// #2450: resolve the tier BEFORE opening the transaction. GetTier is a Redis
	// GET that reads through to the subscriptions table on a miss, so calling it
	// under the row lock would acquire a SECOND pool connection while this handler
	// already holds one for the tx — under a login burst approaching MaxOpenConns
	// that deadlocks until statement timeout. It also extended the hold on the
	// same users-row lock every destructive reset needs. The tier is only a JWT
	// claim and needs no consistent snapshot with the locked read.
	tier := h.entCache.GetTier(ctx, userID)
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error("Failed to begin login mint tx", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Reading disabled, email_verified and credential_epoch in this same locked
	// snapshot keeps the terminal gate, the JWT claim, and the returned user
	// mutually consistent.
	var user models.User
	var credEpoch sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT id, email, username, password_hash, display_name, bio, avatar_url, COALESCE(links, '[]'::jsonb),
		        email_verified, age_verified, created_at, updated_at, password_login_disabled, disabled, credential_epoch
		 FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID,
	).Scan(
		&user.ID, &user.Email, &user.Username, &user.PasswordHash,
		&user.DisplayName, &user.Bio, &user.AvatarURL, &user.Links,
		&user.EmailVerified, &user.AgeVerified,
		&user.CreatedAt, &user.UpdatedAt, &user.PasswordLoginDisabled, &user.Disabled, &credEpoch,
	); err != nil {
		h.log.Error("Failed to lock user for login mint", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}
	if user.Disabled {
		c.JSON(http.StatusForbidden, gin.H{"error_code": "account_disabled"})
		return
	}

	// Refuse if the durable epoch advanced past the one this login was authorized
	// under. A claimless (pre-#2418) challenge presents "" and is refused for any
	// user who has rotated — fail closed. Log the outcome only, never epoch values.
	if err := credepoch.MatchEpoch(credEpoch, expectedEpoch); err != nil {
		h.log.Warn("Login mint refused: credential epoch superseded", "user_id", userID)
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidCredentials})
		return
	}

	// Read the E2EE keys BEFORE minting anything. This ordering is load-bearing:
	// a user with no user_keys row must fail the login WITHOUT a refresh session
	// having been created (regression-locked by
	// TestLoginMissingE2EEKeysDoesNotCreateSession). Doing it after the commit
	// would leave a live session behind a 500.
	var keys models.UserKeys
	if err := tx.QueryRowContext(ctx,
		`SELECT user_id, wrapped_private_key, key_derivation_salt, key_version, key_derivation_alg
		 FROM user_keys WHERE user_id = $1`, userID,
	).Scan(&keys.UserID, &keys.WrappedPrivateKey, &keys.KeyDerivationSalt, &keys.KeyVersion, &keys.KeyDerivationAlg); err != nil {
		h.log.Error("Failed to fetch user keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// Generate access token (JWT, 15 min). tokenID (the refresh-session id) is
	// minted first so the access token can carry it as the sid claim (#2201).
	tokenID := uuid.New().String()
	accessToken, err := GenerateAccessToken(userID, h.jwtSecret, user.EmailVerified, credEpoch.String, tokenID, tier)
	if err != nil {
		h.log.Error(errMsgFailedAccessToken, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// Generate refresh token (random, 30 days)
	refreshToken, err := GenerateRefreshToken()
	if err != nil {
		h.log.Error(errMsgFailedRefreshToken, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// Store refresh token in database
	tokenHash := HashRefreshToken(refreshToken)
	expiresAt := time.Now().Add(30 * 24 * time.Hour)
	deviceName := c.GetHeader(headerDeviceName)
	ipAddress := c.ClientIP()
	userAgent := truncateUserAgent(c.GetHeader(headerUserAgent))
	machineID := c.GetHeader(headerMachineID)

	h.log.Info("CompleteLogin: storing refresh token",
		"request_id", c.GetString(middleware.RequestIDContextKey),
		"remember_me", rememberMe,
		"machine_id", machineID,
		"user_id", userID,
	)

	// Revoke old sessions from the same device. This moves INSIDE the transaction:
	// it mutates refresh_tokens for this user and must be atomic with the mint, or a
	// reset interleaving between the revoke and the INSERT could leave the old
	// session dead and the new one live under a superseded epoch.
	if machineID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE refresh_tokens SET revoked_at = NOW()
			 WHERE user_id = $1 AND machine_id = $2 AND revoked_at IS NULL`,
			userID, machineID,
		); err != nil {
			h.log.Error("Failed to revoke same-device sessions", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
			return
		}
	} else if _, err := tx.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked_at = NOW()
		 WHERE user_id = $1 AND ip_address = $2 AND user_agent = $3 AND revoked_at IS NULL`,
		userID, ipAddress, userAgent,
	); err != nil {
		h.log.Error("Failed to revoke same-origin sessions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tokenID, userID, tokenHash, deviceName, ipAddress, userAgent, expiresAt, rememberMe, nilIfEmpty(machineID),
	); err != nil {
		h.log.Error("Failed to store refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit login mint", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// Cookie MaxAge: 30 days if remember_me, session-scoped otherwise
	cookieMaxAge := 30 * 24 * 60 * 60
	if !rememberMe {
		cookieMaxAge = 0
	}
	SetRefreshCookie(c, refreshToken, cookieMaxAge)
	c.Header(middleware.SessionIssuedHeader, "true")
	c.Header(middleware.SessionIDHeader, tokenID)

	// Clear cumulative IP auth failures only after full auth completion
	// (credentials + MFA if enabled). This prevents resetting the counter
	// by repeatedly passing the credential step while brute-forcing MFA.
	middleware.ClearAuthFailures(c.Request.Context(), h.redis, c.ClientIP())

	h.log.Info("User logged in (MFA verified)", "user_id", userID)

	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"session_id":    tokenID,
		"expires_in":    900,
		"remember_me":   rememberMe,
		"user":          user.PublicUser(),
		"e2ee_keys": gin.H{
			"wrapped_private_key": base64.StdEncoding.EncodeToString(keys.WrappedPrivateKey),
			"key_derivation_salt": base64.StdEncoding.EncodeToString(keys.KeyDerivationSalt),
			"key_version":         keys.KeyVersion,
			"key_derivation_alg":  keys.KeyDerivationAlg,
		},
	})
}

// Refresh handles access token refresh with token rotation.
// Each refresh token is single-use: a new refresh token is issued on every refresh.
// If a previously-used token is presented again, the request is rejected and the
// anomaly is logged. The replayed token is already revoked (single-use rotation).
func (h *Handler) Refresh(c *gin.Context) {
	refreshToken := h.extractRefreshToken(c)
	if refreshToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "No refresh token provided"})
		return
	}

	tokenHash := HashRefreshToken(refreshToken)
	h.log.Info("Refresh attempt", "request_id", c.GetString(middleware.RequestIDContextKey))

	token, err := h.fetchActiveRefreshToken(tokenHash)
	if err != nil {
		h.handleRefreshTokenNotFound(c, err, tokenHash)
		return
	}
	if time.Now().After(token.ExpiresAt) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Refresh token expired"})
		return
	}

	if h.checkRememberMeExpiry(c, token) {
		return
	}

	requestMachineID := c.GetHeader(headerMachineID)
	if h.checkMachineIDTheft(c, token, requestMachineID) {
		return
	}

	if h.checkPreMFASessionLock(c, token) {
		return
	}

	h.rotateAndRespond(c, token, requestMachineID)
}

// extractRefreshToken gets the refresh token from cookie or X-Refresh-Token header.
func (h *Handler) extractRefreshToken(c *gin.Context) string {
	refreshToken, err := c.Cookie("refresh_token")
	if err != nil || refreshToken == "" {
		refreshToken = c.GetHeader("X-Refresh-Token")
	}
	return refreshToken
}

// fetchActiveRefreshToken looks up an active (non-revoked) refresh token by hash.
func (h *Handler) fetchActiveRefreshToken(tokenHash string) (models.RefreshToken, error) {
	var token models.RefreshToken
	var storedMachineID sql.NullString
	// JOIN users + u.disabled = FALSE so a disabled account's token is treated as
	// not-found (#1623). All selected columns are rt.-qualified after the JOIN.
	err := h.db.QueryRow(
		`SELECT rt.id, rt.user_id, rt.token_hash, rt.device_name, rt.ip_address, rt.user_agent, rt.expires_at, rt.created_at, rt.last_used_at, rt.remember_me, COALESCE(rt.machine_id, '')
		 FROM refresh_tokens rt
		 JOIN users u ON u.id = rt.user_id
		 WHERE rt.token_hash = $1 AND rt.revoked_at IS NULL AND u.disabled = FALSE`,
		tokenHash,
	).Scan(
		&token.ID, &token.UserID, &token.TokenHash, &token.DeviceName,
		&token.IPAddress, &token.UserAgent, &token.ExpiresAt, &token.CreatedAt,
		&token.LastUsedAt, &token.RememberMe, &storedMachineID,
	)
	if storedMachineID.Valid {
		token.MachineID = storedMachineID.String
	}
	return token, err
}

// handleRefreshTokenNotFound handles the case where no active refresh token was found.
// Includes replay detection and grace period recovery.
func (h *Handler) handleRefreshTokenNotFound(c *gin.Context, err error, tokenHash string) {
	if err != sql.ErrNoRows {
		h.log.Error("Failed to fetch refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	// Replay detection: check if this token was already used (revoked but exists)
	if h.attemptGracePeriodRecovery(c, tokenHash) {
		return
	}

	h.log.Warn("Refresh token not found in database (not active, not revoked)",
		"request_id", c.GetString(middleware.RequestIDContextKey))
	c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidRefreshToken})
}

// attemptGracePeriodRecovery checks if a revoked token is within the grace period for crash recovery.
// Returns true if recovery was attempted (response already written), false to continue with rejection.
func (h *Handler) attemptGracePeriodRecovery(c *gin.Context, tokenHash string) bool {
	var revokedUserID, revokedIP, revokedUA, revokedMachineID string
	var revokedAt time.Time
	var revokedTokenID string
	revokeErr := h.db.QueryRow(
		`SELECT id, user_id, revoked_at, ip_address, user_agent, COALESCE(machine_id, '')
		 FROM refresh_tokens WHERE token_hash = $1 AND revoked_at IS NOT NULL`,
		tokenHash,
	).Scan(&revokedTokenID, &revokedUserID, &revokedAt, &revokedIP, &revokedUA, &revokedMachineID)
	if revokeErr != nil || revokedUserID == "" {
		return false
	}
	requestIP := c.ClientIP()
	requestUA := truncateUserAgent(c.GetHeader(headerUserAgent))
	graceRequestMachineID := c.GetHeader(headerMachineID)
	storedIP := stripCIDR(revokedIP)
	machineIDOk := revokedMachineID == "" || graceRequestMachineID == "" || revokedMachineID == graceRequestMachineID

	if time.Since(revokedAt) < 30*time.Second && storedIP == requestIP && revokedUA == requestUA && machineIDOk {
		// #2428: the disabled check now happens under the users-row lock inside
		// handleGracePeriodRefresh's transaction. No separate SELECT-then-act
		// pre-check here — that was the exact non-atomic pattern this work closes,
		// and a disable committing before the tx's locked read yields a 403 there.
		h.log.Info("Refresh token replay within grace period, recovering session",
			"user_id", revokedUserID, "revoked_ago_ms", time.Since(revokedAt).Milliseconds(), "ip", requestIP)
		h.handleGracePeriodRefresh(c, revokedUserID, revokedTokenID, revokedAt)
		return true
	}

	h.log.Warn("Refresh token replay detected, stale token replayed",
		"request_id", c.GetString(middleware.RequestIDContextKey),
		"user_id", revokedUserID,
		"same_ip", revokedIP == requestIP, "same_ua", revokedUA == requestUA,
		"revoked_ago_ms", time.Since(revokedAt).Milliseconds())
	c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidRefreshToken})
	return true
}

// stripCIDR removes CIDR notation suffix from an IP address (e.g. "127.0.0.1/32" → "127.0.0.1").
func stripCIDR(ip string) string {
	if idx := strings.IndexByte(ip, '/'); idx != -1 {
		return ip[:idx]
	}
	return ip
}

// checkRememberMeExpiry checks if a remember-me token has expired due to prolonged offline inactivity.
// Returns true if the response was written (session expired).
func (h *Handler) checkRememberMeExpiry(c *gin.Context, token models.RefreshToken) bool {
	if !token.RememberMe {
		return false
	}
	ctx := context.Background()
	presenceKey := fmt.Sprintf("presence:%s", token.UserID)
	if _, presenceErr := h.redis.Get(ctx, presenceKey).Result(); presenceErr != redis.Nil {
		return false // User is online — skip
	}
	lastSeenKey := fmt.Sprintf("last_seen:%s", token.UserID)
	lastSeenStr, lsErr := h.redis.Get(ctx, lastSeenKey).Result()
	if lsErr != nil || lastSeenStr == "" {
		return false
	}
	lastSeenUnix, parseErr := strconv.ParseInt(lastSeenStr, 10, 64)
	if parseErr != nil {
		return false
	}
	if time.Since(time.Unix(lastSeenUnix, 0)) > 30*24*time.Hour {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Session expired due to prolonged inactivity"})
		return true
	}
	return false
}

// checkMachineIDTheft detects machine ID mismatches and handles suspicious/theft scenarios.
// Returns true if the response was written (blocked or MFA challenge sent).
func (h *Handler) checkMachineIDTheft(c *gin.Context, token models.RefreshToken, requestMachineID string) bool {
	if token.MachineID == "" || requestMachineID == "" || token.MachineID == requestMachineID {
		return false
	}

	requestIP := c.ClientIP()
	storedIP := stripCIDR(token.IPAddress)

	if storedIP != requestIP {
		return h.handleTokenTheft(c, token)
	}
	return h.handleSuspiciousMachineID(c, token, requestMachineID, requestIP)
}

// handleTokenTheft handles the case where both machine ID and IP differ — high risk token theft.
// Always returns true (response is written).
func (h *Handler) handleTokenTheft(c *gin.Context, token models.RefreshToken) bool {
	h.log.Error("TOKEN THEFT DETECTED: machine and IP mismatch", "user_id", token.UserID)

	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
	defer cancel()
	if _, err := RevokeAllRefreshTokens(revokeCtx, h.db, token.UserID, RevokeAllRefreshTokensOptions{}); err != nil {
		h.log.Error("Failed to revoke refresh tokens after token theft", "error", err, "user_id", token.UserID)
	}

	if uid, parseErr := uuid.Parse(token.UserID); parseErr == nil {
		h.hub.DisconnectUser(uid)
	}
	h.triggerTheftKeyRevocations(token.UserID)

	c.JSON(http.StatusUnauthorized, gin.H{
		"error": "Session terminated: potential token theft detected", "error_code": "session_theft_detected"})
	return true
}

// handleSuspiciousMachineID handles same-IP but different machine ID — suspicious but not theft.
// Returns true if the response was written (MFA challenge sent), false to allow.
func (h *Handler) handleSuspiciousMachineID(c *gin.Context, token models.RefreshToken, requestMachineID, requestIP string) bool {
	if h.mfaChecker == nil || !h.mfaChecker.IsEnabled(c.Request.Context(), token.UserID) {
		h.log.Warn("Refresh from different machine_id but same IP (no MFA: allowing)",
			"user_id", token.UserID, "stored_machine_id", token.MachineID,
			"request_machine_id", requestMachineID, "ip", requestIP)
		return false
	}

	ctx := c.Request.Context()
	// #2418: stamp the epoch into the challenge. Unlike the GenerateLoginChallenge
	// failure below, this one does NOT degrade-to-allow: passing "" would mint a
	// claimless challenge that CompleteLogin rejects for a rotated user, turning a
	// transient DB blip into a confusing 401 at the END of the MFA flow. Surfacing
	// the read failure here is the honest, debuggable outcome.
	suspiciousEpoch, epochErr := h.readCredentialEpoch(ctx, token.UserID)
	if epochErr != nil {
		h.log.Error("Failed to read credential epoch for suspicious-refresh challenge",
			"error", epochErr, "user_id", token.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return true
	}
	challengeToken, jti, mfaErr := h.mfaChecker.GenerateLoginChallenge(ctx, token.UserID, token.RememberMe, suspiciousEpoch)
	if mfaErr != nil {
		h.log.Error("Failed to generate suspicious refresh MFA challenge", "error", mfaErr)
		return false // Graceful degradation — allow
	}

	h.log.Warn("Suspicious refresh: different machine_id, same IP — MFA required",
		"user_id", token.UserID, "stored_machine_id", token.MachineID,
		"request_machine_id", requestMachineID, "ip", requestIP)

	resp, respErr := h.buildMFAChallengeResponse(ctx, "suspicious_session_mfa", "Session verification required", challengeToken, token.UserID, jti)
	if respErr != nil {
		// Same graceful degradation as the GenerateLoginChallenge failure above — better to
		// allow the refresh than to hand back a challenge with no selectable method (#2450).
		h.log.Error("Failed to build suspicious refresh MFA challenge", "error", respErr)
		return false
	}
	c.JSON(http.StatusForbidden, resp)
	return true
}

// checkPreMFASessionLock checks if a pre-MFA session needs an upgrade challenge.
// Returns true if the response was written (MFA upgrade required).
func (h *Handler) checkPreMFASessionLock(c *gin.Context, token models.RefreshToken) bool {
	if h.mfaChecker == nil || !h.mfaChecker.IsEnabled(c.Request.Context(), token.UserID) {
		return false
	}

	ctx := c.Request.Context()
	bypassKey := fmt.Sprintf("mfa_upgrade_bypass:%s", token.UserID)
	if h.redis.Exists(ctx, bypassKey).Val() > 0 {
		h.redis.Del(ctx, bypassKey)
		h.log.Info("Pre-MFA session bypass consumed", "user_id", token.UserID)
		return false
	}

	var mfaEnabledAt sql.NullTime
	_ = h.db.QueryRow(`SELECT mfa_enabled_at FROM users WHERE id = $1`, token.UserID).Scan(&mfaEnabledAt)
	if !mfaEnabledAt.Valid || !token.CreatedAt.Before(mfaEnabledAt.Time) {
		return false
	}

	challengeToken, jti, mfaErr := h.mfaChecker.GenerateUpgradeChallenge(ctx, token.UserID, token.RememberMe)
	if mfaErr != nil {
		h.log.Error("Failed to generate pre-MFA session challenge", "error", mfaErr)
		return false // Don't block on failure
	}

	h.log.Info("Pre-MFA session requires MFA verification",
		"user_id", token.UserID, "session_created", token.CreatedAt, "mfa_enabled_at", mfaEnabledAt.Time)

	resp, respErr := h.buildMFAChallengeResponse(ctx, "mfa_upgrade_required",
		"This session was created before MFA was enabled. Please verify your identity.",
		challengeToken, token.UserID, jti)
	if respErr != nil {
		// Matches the GenerateUpgradeChallenge failure above — don't block, and don't ship
		// a challenge the user cannot answer (#2450).
		h.log.Error("Failed to build pre-MFA session challenge", "error", respErr)
		return false
	}
	c.JSON(http.StatusForbidden, resp)
	return true
}

// buildMFAChallengeResponse constructs the common MFA challenge JSON response with
// methods, recovery-only methods, and optional WebAuthn options.
//
// #2450: both method reads were blank-discarded, and this is the SHARED builder for the
// suspicious-refresh and MFA-upgrade challenges — so the defect handleMFAChallenge fixed
// on the login path was still live on two others. errcheck honors an explicit `_`, so the
// linter never flagged it (the review-only class in [internal]rules/backend.md, founding
// incidents #1142/#1154). On error the response shipped `"methods": []` beside a VALID
// challenge token: the client renders an MFA prompt with nothing selectable, the challenge
// JTI is burned, and nothing is logged.
//
// It now returns an error rather than a half-built response. Callers already degrade to
// `return false` on their sibling GenerateChallenge error, so they keep that exact posture
// — the point is to never ship an unusable challenge, not to add a new failure mode.
// ctx is the request context (was context.Background(), which discarded cancellation and
// deadline across three backing calls).
func (h *Handler) buildMFAChallengeResponse(ctx context.Context, errorCode, message, challengeToken, userID, jti string) (gin.H, error) {
	// allMethods only feeds the recovery-only hint, so it degrades to an empty hint.
	allMethods, err := h.mfaChecker.GetEnabledMethods(ctx, userID)
	if err != nil {
		h.log.Error("Failed to read enabled MFA methods", "error", err, "user_id", userID)
	}
	// loginMethods is load-bearing for the response — an empty list is the hard-stuck bug.
	loginMethods, err := h.mfaChecker.GetLoginMethods(ctx, userID)
	if err != nil {
		h.log.Error("Failed to read MFA login methods", "error", err, "user_id", userID)
		return nil, fmt.Errorf("read MFA login methods: %w", err)
	}

	resp := gin.H{
		"error":               errorCode,
		"message":             message,
		"mfa_challenge_token": challengeToken,
		"methods":             loginMethods,
	}

	recoveryOnly := computeRecoveryOnlyMethods(allMethods, loginMethods)
	if len(recoveryOnly) > 0 {
		resp["recovery_only_methods"] = recoveryOnly
	}

	loginSet := make(map[string]bool, len(loginMethods))
	for _, m := range loginMethods {
		loginSet[m] = true
	}
	if loginSet["webauthn"] {
		if opts, werr := h.mfaChecker.BeginWebAuthnLogin(ctx, userID, jti); werr != nil {
			h.log.Error("Failed to begin WebAuthn login", "error", werr)
		} else if opts != nil {
			resp["webauthn_options"] = opts
		}
	}

	return resp, nil
}

// computeRecoveryOnlyMethods returns methods in allMethods but not in loginMethods.
func computeRecoveryOnlyMethods(allMethods, loginMethods []string) []string {
	loginSet := make(map[string]bool, len(loginMethods))
	for _, m := range loginMethods {
		loginSet[m] = true
	}
	var recoveryOnly []string
	for _, m := range allMethods {
		if !loginSet[m] {
			recoveryOnly = append(recoveryOnly, m)
		}
	}
	return recoveryOnly
}

// rotateAndRespond completes the refresh: revokes the old token, issues a new one, and responds.
func (h *Handler) rotateAndRespond(c *gin.Context, token models.RefreshToken, requestMachineID string) {
	newRefreshToken, err := GenerateRefreshToken()
	if err != nil {
		h.log.Error(errMsgFailedRefreshToken, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	newExpiry := token.ExpiresAt
	cookieMaxAge := 0
	if token.RememberMe {
		newExpiry = time.Now().Add(30 * 24 * time.Hour)
		cookieMaxAge = 30 * 24 * 60 * 60
	}

	newTokenHash := HashRefreshToken(newRefreshToken)
	newTokenID := uuid.New().String()
	propagatedMachineID := requestMachineID
	if propagatedMachineID == "" {
		propagatedMachineID = token.MachineID
	}
	h.log.Info("Refresh: rotating token",
		"request_id", c.GetString(middleware.RequestIDContextKey),
		"user_id", token.UserID, "remember_me", token.RememberMe)

	// #2201 (Codex #2397 review): serialize this rotation against a concurrent
	// destructive credential reset. One transaction: lock the user row
	// FOR NO KEY UPDATE (the same lock the reset holds), read the epoch + disabled
	// state under it, revoke the SOURCE token only if still active, and mint the
	// successor. If a reset already revoked the source token (bulk revoke under
	// its own hold of this lock), the conditional revoke returns no rows and we
	// refuse — a pre-reset refresh token can no longer mint a fresh session under
	// the new epoch.
	ctx := c.Request.Context()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error("Failed to begin refresh rotation tx", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}
	defer func() { _ = tx.Rollback() }()

	var refreshEmailVerified, disabled bool
	var refreshCredEpoch sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT email_verified, credential_epoch, disabled FROM users WHERE id = $1 FOR NO KEY UPDATE`, token.UserID,
	).Scan(&refreshEmailVerified, &refreshCredEpoch, &disabled); err != nil {
		h.log.Error("Failed to lock user for refresh rotation", "error", err, "user_id", token.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}
	// #1623 disable/refresh race — now closed by the FOR NO KEY UPDATE lock above
	// (a concurrent disable serializes against it) rather than an EXISTS guard.
	if disabled {
		c.JSON(http.StatusForbidden, gin.H{"error_code": "account_disabled"})
		return
	}

	var revokedID string
	if err := tx.QueryRowContext(ctx,
		`UPDATE refresh_tokens SET revoked_at = NOW(), last_used_at = NOW() WHERE id = $1 AND revoked_at IS NULL RETURNING id`,
		token.ID,
	).Scan(&revokedID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// A concurrent destructive reset revoked the source token first.
			c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgRefreshFailed})
			return
		}
		h.log.Error("Failed to revoke old refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id, predecessor_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		newTokenID, token.UserID, newTokenHash, token.DeviceName,
		c.ClientIP(), truncateUserAgent(c.GetHeader(headerUserAgent)),
		newExpiry, token.RememberMe, nilIfEmpty(propagatedMachineID), token.ID,
	); err != nil {
		h.log.Error("Failed to store new refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit refresh rotation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	// Auto-purge old revoked sessions (> 90 days) — best-effort, post-commit.
	_, _ = h.db.Exec(
		`DELETE FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NOT NULL AND revoked_at < NOW() - INTERVAL '90 days'`,
		token.UserID)

	SetRefreshCookie(c, newRefreshToken, cookieMaxAge)

	tier := h.entCache.GetTier(c.Request.Context(), token.UserID)
	accessToken, err := GenerateAccessToken(token.UserID, h.jwtSecret, refreshEmailVerified, refreshCredEpoch.String, newTokenID, tier)
	if err != nil {
		h.log.Error(errMsgFailedAccessToken, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"access_token":        accessToken,
		"refresh_token":       newRefreshToken,
		"session_id":          newTokenID,
		"previous_session_id": token.ID,
		"expires_in":          900,
	})
}

// handleGracePeriodRefresh recovers a session when a recently-revoked refresh token
// is replayed within the grace period — the client process was killed between the
// server revoking the old token and the client persisting the rotated one. It selects
// the exact successor by predecessor_id lineage (#2428), revokes it, and mints a fresh
// pair, all inside one users FOR NO KEY UPDATE transaction so it composes with the
// #2201 credential-epoch fence. The 30s window check already happened in the caller
// (attemptGracePeriodRecovery); recovery keys on revokedTokenID, not the timestamp.
func (h *Handler) handleGracePeriodRefresh(c *gin.Context, userID string, revokedTokenID string, _ time.Time) {
	ctx := c.Request.Context()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error("Failed to begin grace recovery tx", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the user row (the same lock a destructive reset holds) and read epoch +
	// disabled under it, so grace recovery composes with the #2201 epoch fence.
	var graceEmailVerified, disabled bool
	var graceCredEpoch sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT email_verified, credential_epoch, disabled FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID,
	).Scan(&graceEmailVerified, &graceCredEpoch, &disabled); err != nil {
		h.log.Error("Failed to lock user for grace recovery", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}
	if disabled {
		c.JSON(http.StatusForbidden, gin.H{"error_code": "account_disabled"})
		return
	}

	// Exact lineage lookup: the still-live token whose predecessor is the replayed
	// token. A foreign concurrent/post-reset token has no edge to revokedTokenID and
	// can never be selected. LIMIT 1 is defensive — the chain is strictly linear.
	// `expires_at > NOW()` mirrors the normal Refresh path's expiry gate (#2428
	// CWE-613): an already-expired successor must not re-mint a fresh access token;
	// it fails closed to the sql.ErrNoRows -> 401 branch below.
	var successor models.RefreshToken
	var successorMachineID sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT id, user_id, device_name, expires_at, remember_me, COALESCE(machine_id, '')
		 FROM refresh_tokens
		 WHERE predecessor_id = $1 AND revoked_at IS NULL AND expires_at > NOW()
		 ORDER BY created_at DESC LIMIT 1`,
		revokedTokenID,
	).Scan(
		&successor.ID, &successor.UserID, &successor.DeviceName,
		&successor.ExpiresAt, &successor.RememberMe, &successorMachineID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// No live successor in this lineage — nothing to recover; stale replay.
		h.log.Warn("Grace period replay but no live successor in lineage",
			"user_id", userID, "revoked_token_id", revokedTokenID)
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidRefreshToken})
		return
	}
	if err != nil {
		// A real query/DB error must be observable, not collapsed into a 401.
		h.log.Error("Grace recovery successor lookup failed", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}
	if successorMachineID.Valid {
		successor.MachineID = successorMachineID.String
	}

	// Revoke the successor (never delivered to the client) — checked, inside the tx.
	var revokedSuccessorID string
	revokeErr := tx.QueryRowContext(ctx,
		`UPDATE refresh_tokens SET revoked_at = NOW(), last_used_at = NOW()
		 WHERE id = $1 AND revoked_at IS NULL RETURNING id`,
		successor.ID,
	).Scan(&revokedSuccessorID)
	if errors.Is(revokeErr, sql.ErrNoRows) {
		// Revoked out from under us mid-tx (e.g. concurrent reset). Fail closed.
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidRefreshToken})
		return
	}
	if revokeErr != nil {
		h.log.Error("Failed to revoke successor during grace recovery", "error", revokeErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	// Mint the fresh successor', chaining lineage to the token it replaces (D1).
	newRefreshToken, err := GenerateRefreshToken()
	if err != nil {
		h.log.Error("Failed to generate refresh token during grace recovery", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}
	newExpiry := successor.ExpiresAt
	cookieMaxAge := 0
	if successor.RememberMe {
		newExpiry = time.Now().Add(30 * 24 * time.Hour)
		cookieMaxAge = 30 * 24 * 60 * 60
	}
	newTokenHash := HashRefreshToken(newRefreshToken)
	newTokenID := uuid.New().String()
	graceMachineID := c.GetHeader(headerMachineID)
	if graceMachineID == "" {
		graceMachineID = successor.MachineID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id, predecessor_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		newTokenID, userID, newTokenHash, successor.DeviceName,
		c.ClientIP(), truncateUserAgent(c.GetHeader(headerUserAgent)),
		newExpiry, successor.RememberMe, nilIfEmpty(graceMachineID), successor.ID,
	); err != nil {
		h.log.Error("Failed to store new refresh token during grace recovery", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit grace recovery", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	SetRefreshCookie(c, newRefreshToken, cookieMaxAge)

	tier := h.entCache.GetTier(ctx, userID)
	accessToken, err := GenerateAccessToken(userID, h.jwtSecret, graceEmailVerified, graceCredEpoch.String, newTokenID, tier)
	if err != nil {
		h.log.Error("Failed to generate access token during grace recovery", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	h.log.Info("Session recovered via grace period refresh",
		"user_id", userID, "new_session_id", newTokenID)

	c.JSON(http.StatusOK, gin.H{
		"access_token":        accessToken,
		"refresh_token":       newRefreshToken,
		"session_id":          newTokenID,
		"previous_session_id": revokedTokenID,
		"expires_in":          900,
	})
}

// triggerTheftKeyRevocations inserts key_revocations records for all E2EE channels
// that the compromised user has access to. This is called when token theft is detected
// (different machine_id + different IP) because the attacker's session could have
// fetched wrapped channel keys.
func (h *Handler) triggerTheftKeyRevocations(userID string) {
	// Find all channels the user is a member of (all are encrypted under #201)
	rows, err := h.db.Query(
		`SELECT DISTINCT ck.channel_id, COALESCE(MAX(ck.key_version), 1) AS max_epoch
		 FROM channel_keys ck
		 JOIN channels ch ON ch.id = ck.channel_id
		 WHERE ck.user_id = $1
		 GROUP BY ck.channel_id`,
		userID,
	)
	if err != nil {
		h.log.Error("Failed to query channels for theft key revocation", "error", err, "user_id", userID)
		return
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var channelID string
		var maxEpoch int
		if err := rows.Scan(&channelID, &maxEpoch); err != nil {
			h.log.Error("Failed to scan channel for theft key revocation", "error", err)
			continue
		}

		// Insert key_revocations record (ignore conflict — may already be revoked)
		_, err := h.db.Exec(
			`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by)
			 VALUES ($1, $2, $3, 'theft_detected', $4)
			 ON CONFLICT (channel_id, revoked_epoch) DO NOTHING`,
			channelID, maxEpoch, maxEpoch+1, userID,
		)
		if err != nil {
			h.log.Error("Failed to insert key revocation for theft", "error", err, "channel_id", channelID)
		}
	}

	h.log.Info("Key revocations triggered for theft detection", "user_id", userID)
}

// Logout handles session termination. It returns claims only when the bearer
// was valid before this request, so X-Session-ID can never authorize a
// cross-user (or already logged-out) session revoke.
func (h *Handler) blacklistAccessToken(c *gin.Context) *Claims {
	authHeader := c.GetHeader("Authorization")
	if !strings.HasPrefix(authHeader, bearerPrefix) {
		return nil
	}
	token := strings.TrimPrefix(authHeader, bearerPrefix)
	claims, err := ValidateAccessToken(token, h.jwtSecret)
	if err != nil || claims.ID == "" {
		return nil
	}
	wasBlacklisted, lookupErr := h.redis.Exists(
		c.Request.Context(),
		"blacklist:"+claims.ID,
	).Result()
	remaining := time.Until(claims.ExpiresAt.Time)
	if blErr := middleware.BlacklistToken(context.Background(), h.redis, claims.ID, remaining); blErr != nil {
		h.log.Error("Failed to blacklist access token", "error", blErr)
	}
	if lookupErr != nil || wasBlacklisted != 0 {
		return nil
	}
	return claims
}

func (h *Handler) disconnectUserByID(userID string) {
	if userID == "" {
		return
	}
	if uid, parseErr := uuid.Parse(userID); parseErr == nil {
		h.hub.DisconnectUser(uid)
	}
}

// Logout revokes the current session's refresh token and blacklists the access token.
//
// Explicit credentials take precedence over the cookie so best-effort cleanup
// of an aborted flow can never revoke a successor session from the ambient
// cookie jar. Without a valid bearer, X-Session-ID must atomically match the
// ambient refresh cookie's token hash.
func (h *Handler) Logout(c *gin.Context) {
	claims := h.blacklistAccessToken(c)
	revoked := h.revokeLogoutSession(
		c, claims,
		c.GetHeader("X-Refresh-Token"),
		c.GetHeader("X-Session-ID"),
	)
	revokedUserID, revokedSessionID := revoked.userID, revoked.sessionID
	if revoked.err != nil && !errors.Is(revoked.err, sql.ErrNoRows) {
		h.log.Error("Failed to revoke refresh session during logout", "error", revoked.err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalError})
		return
	}

	if !revoked.credentialPresented {
		c.JSON(http.StatusOK, gin.H{"message": "Already logged out"})
		return
	}

	// Free this session's attestation tokens proactively so they don't outlive
	// the session in Redis. Orphan attestation keys are harmless (they bind a
	// session_id that no longer exists), but the explicit cleanup avoids
	// up-to-2h of dead state. Per ADR-0010 (#677). Drives off revokedSessionID
	// rather than c.GetHeader("X-Session-ID") so an attacker can't target
	// arbitrary sessions for cleanup. revokedSessionID is empty when the
	// UPDATE matched no row (e.g., already-revoked refresh token) — in that
	// case the cleanup is a no-op, which is correct: there's no live session
	// to clean up.
	if revokedSessionID != "" {
		attestation.CleanupTokensForSession(c.Request.Context(), h.redis, h.log, revokedSessionID)
	}

	h.disconnectUserByID(revokedUserID)

	if revoked.cookieAuthorized && revokedSessionID != "" {
		SetRefreshCookie(c, "", -1)
	}
	c.JSON(http.StatusOK, gin.H{"message": "Logged out successfully"})
}

// logoutRevocation is the outcome of revokeLogoutSession: which session (if any)
// was revoked, whether the revocation was cookie-authorized (so the caller may
// clear the cookie), whether any credential was presented at all, and the DB
// error (sql.ErrNoRows is expected when no live row matched).
type logoutRevocation struct {
	userID              string
	sessionID           string
	cookieAuthorized    bool
	credentialPresented bool
	err                 error
}

// wrapRevokeErr adds operation context to a refresh-token revocation error.
// A nil error is returned unchanged — wrapping nil would turn a successful
// revoke into a failure — and the %w verb keeps the caller's
// errors.Is(err, sql.ErrNoRows) check matching through the wrapper.
func wrapRevokeErr(err error, op string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, err)
}

// revokeByTokenHash revokes the live refresh-token row whose hash matches
// tokenHash and returns the owning user and session IDs. Shared by the explicit
// X-Refresh-Token path and the ambient-cookie path, which issue an identical
// statement — one copy stops the two from drifting.
func (h *Handler) revokeByTokenHash(
	ctx context.Context,
	tokenHash string,
) (userID, sessionID string, err error) {
	err = h.db.QueryRowContext(
		ctx,
		`UPDATE refresh_tokens SET revoked_at = NOW()
		 WHERE token_hash = $1 AND revoked_at IS NULL
		 RETURNING user_id, id`,
		tokenHash,
	).Scan(&userID, &sessionID)
	return userID, sessionID, wrapRevokeErr(err, "revoke refresh token by token hash")
}

// revokeLogoutSession selects the refresh-token revocation strategy for a logout
// and runs it: explicit X-Refresh-Token first, then X-Session-ID (bearer-scoped
// to claims.UserID, else matched against the ambient cookie's token hash), then
// the ambient refresh cookie alone. Explicit credentials take precedence over the
// cookie so best-effort cleanup of an aborted flow can never revoke a successor
// session from the ambient cookie jar (see Logout's doc comment).
func (h *Handler) revokeLogoutSession(
	c *gin.Context,
	claims *Claims,
	refreshToken, sessionID string,
) logoutRevocation {
	result := logoutRevocation{credentialPresented: refreshToken != "" || sessionID != ""}
	switch {
	case refreshToken != "":
		result.userID, result.sessionID, result.err = h.revokeByTokenHash(
			c.Request.Context(), HashRefreshToken(refreshToken),
		)
	case sessionID != "":
		if _, parseErr := uuid.Parse(sessionID); parseErr != nil {
			return result
		}
		if claims != nil {
			result.err = wrapRevokeErr(h.db.QueryRowContext(
				c.Request.Context(),
				`UPDATE refresh_tokens SET revoked_at = NOW()
				 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
				 RETURNING user_id, id`,
				sessionID, claims.UserID,
			).Scan(&result.userID, &result.sessionID), "revoke session for bearer user")
			return result
		}
		cookieToken, err := c.Cookie("refresh_token")
		if err == nil && cookieToken != "" {
			result.cookieAuthorized = true
			result.err = wrapRevokeErr(h.db.QueryRowContext(
				c.Request.Context(),
				`UPDATE refresh_tokens SET revoked_at = NOW()
				 WHERE id = $1 AND token_hash = $2 AND revoked_at IS NULL
				 RETURNING user_id, id`,
				sessionID, HashRefreshToken(cookieToken),
			).Scan(&result.userID, &result.sessionID), "revoke session by id and cookie hash")
		}
	default:
		cookieToken, err := c.Cookie("refresh_token")
		if err == nil && cookieToken != "" {
			result.credentialPresented = true
			result.cookieAuthorized = true
			result.userID, result.sessionID, result.err = h.revokeByTokenHash(
				c.Request.Context(), HashRefreshToken(cookieToken),
			)
		}
	}
	return result
}

// ── Account Recovery Endpoints ──────────────────────────────────────────────

const (
	recoveryCodeTTL      = 10 * time.Minute // Recovery code expires after 10 minutes
	recoveryMaxAttempts  = 5                // Max wrong-code attempts before invalidating
	recoveryTokenUsedTTL = 25 * time.Hour   // Must match or exceed mfa.recoveryTTL
)

// recoveryRecord is stored in Redis keyed by "recovery_code:{email}".
type recoveryRecord struct {
	CodeHash string `json:"code_hash"`
	UserID   string `json:"user_id"`
	Attempts int    `json:"attempts"`
}

// generateRecoveryCode produces a cryptographically random 6-digit numeric code.
func generateRecoveryCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// hashRecoveryCode returns the SHA-256 hex digest of a recovery code.
func hashRecoveryCode(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

// recoveryRedisKey returns the Redis key for a pending recovery code.
func recoveryRedisKey(email string) string {
	return fmt.Sprintf("recovery_code:%s", strings.ToLower(email))
}

// RecoveryBegin initiates account recovery by sending a verification code to the user's email.
func (h *Handler) RecoveryBegin(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email is required"})
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))

	// Always return 200 to prevent email enumeration
	successMsg := gin.H{"message": "If an account exists with that email, a recovery code has been sent"}

	// Look up user by email
	var userID string
	err := h.db.QueryRow(
		`SELECT id FROM users WHERE LOWER(email) = LOWER($1)`, email,
	).Scan(&userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// User not found — return success anyway (prevent enumeration)
			c.JSON(http.StatusOK, successMsg)
			return
		}
		// Real DB error — log it but still return 200 to prevent enumeration
		h.log.Error("Failed to look up user for recovery", "error", err)
		c.JSON(http.StatusOK, successMsg)
		return
	}

	// Generate 6-digit code
	code, err := generateRecoveryCode()
	if err != nil {
		h.log.Error("Failed to generate recovery code", "error", err)
		// Fall through to generic 200 — recovery won't proceed but attacker can't distinguish
		c.JSON(http.StatusOK, successMsg)
		return
	}

	// Store SHA-256 hash in Redis
	record := recoveryRecord{
		CodeHash: hashRecoveryCode(code),
		UserID:   userID,
		Attempts: 0,
	}
	data, err := json.Marshal(record)
	if err != nil {
		h.log.Error("Failed to marshal recovery record", "error", err)
		c.JSON(http.StatusOK, successMsg)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.redis.Set(ctx, recoveryRedisKey(email), data, recoveryCodeTTL).Err(); err != nil {
		h.log.Error("Failed to store recovery code in Redis", "error", err)
		c.JSON(http.StatusOK, successMsg)
		return
	}

	// Send code via email
	if err := h.emailSvc.SendRecoveryCode(email, code); err != nil {
		h.log.Error("Failed to send recovery code email", "error", err, "email", email)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if delErr := h.redis.Del(cleanupCtx, recoveryRedisKey(email)).Err(); delErr != nil {
			h.log.Warn("Failed to clear unsent recovery code", "error", delErr)
		}
		c.JSON(http.StatusOK, successMsg)
		return
	}

	h.log.Info("Recovery code sent", "email", email)
	c.JSON(http.StatusOK, successMsg)
}

// validateRecoveryCodeFormat normalises and validates a 6-digit recovery code.
// Returns the cleaned code or an error message.
func validateRecoveryCodeFormat(raw string) (string, string) {
	code := strings.TrimSpace(strings.ReplaceAll(raw, "-", ""))
	if len(code) != 6 {
		return "", "Recovery code must be 6 digits"
	}
	for _, ch := range code {
		if ch < '0' || ch > '9' {
			return "", "Recovery code must be 6 digits"
		}
	}
	return code, ""
}

// fetchRecoveryRecord loads and validates the recovery record from Redis.
// Returns the record on success. On failure it writes the HTTP response and returns nil.
func (h *Handler) fetchRecoveryRecord(ctx context.Context, c *gin.Context, redisKey string) *recoveryRecord {
	data, err := h.redis.Get(ctx, redisKey).Bytes()
	if err == redis.Nil {
		middleware.RecordAuthFailure(ctx, h.redis, c.ClientIP(), middleware.DefaultAuthBanConfig())
		c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidExpiredRecoveryCode, "attempts_remaining": 0})
		return nil
	}
	if err != nil {
		h.log.Error("Failed to fetch recovery record from Redis", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errRecoveryVerificationFailed})
		return nil
	}

	var record recoveryRecord
	if err := json.Unmarshal(data, &record); err != nil {
		h.log.Error("Failed to unmarshal recovery record", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errRecoveryVerificationFailed})
		return nil
	}

	if record.Attempts >= recoveryMaxAttempts {
		h.redis.Del(ctx, redisKey)
		middleware.RecordAuthFailure(ctx, h.redis, c.ClientIP(), middleware.DefaultAuthBanConfig())
		c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidExpiredRecoveryCode, "attempts_remaining": 0})
		return nil
	}
	return &record
}

// verifyRecoveryCode does a timing-safe comparison and handles wrong-code bookkeeping.
// Returns true if the code matches. On mismatch it writes the HTTP response.
func (h *Handler) verifyRecoveryCode(ctx context.Context, c *gin.Context, code string, record *recoveryRecord, redisKey string) bool {
	if subtle.ConstantTimeCompare([]byte(hashRecoveryCode(code)), []byte(record.CodeHash)) == 1 {
		return true
	}
	record.Attempts++
	updated, _ := json.Marshal(record)
	ttl := h.redis.TTL(ctx, redisKey).Val()
	if ttl > 0 {
		h.redis.Set(ctx, redisKey, updated, ttl)
	}
	middleware.RecordAuthFailure(ctx, h.redis, c.ClientIP(), middleware.DefaultAuthBanConfig())
	c.JSON(http.StatusUnauthorized, gin.H{
		"error":              errInvalidExpiredRecoveryCode,
		"attempts_remaining": 0,
	})
	return false
}

// buildVerifyCodeResponse constructs the response for a successful recovery code verification.
func (h *Handler) buildVerifyCodeResponse(recoveryToken, userID string) (gin.H, error) {
	resp := gin.H{"recovery_token": recoveryToken}

	var recoveryWrappedPrivateKey, recoveryKeySalt []byte
	var recoveryWrappedPrefsKey, recoveryPrefsKeySalt []byte
	err := h.db.QueryRow(
		`SELECT recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt
		 FROM user_recovery_keys WHERE user_id = $1`, userID,
	).Scan(&recoveryWrappedPrivateKey, &recoveryKeySalt, &recoveryWrappedPrefsKey, &recoveryPrefsKeySalt)
	if err == nil {
		resp["has_recovery_key"] = true
		resp["recovery_wrapped_private_key"] = base64.StdEncoding.EncodeToString(recoveryWrappedPrivateKey)
		resp["recovery_key_salt"] = base64.StdEncoding.EncodeToString(recoveryKeySalt)
		if len(recoveryWrappedPrefsKey) > 0 {
			resp["recovery_wrapped_prefs_key"] = base64.StdEncoding.EncodeToString(recoveryWrappedPrefsKey)
		}
		if len(recoveryPrefsKeySalt) > 0 {
			resp["recovery_prefs_key_salt"] = base64.StdEncoding.EncodeToString(recoveryPrefsKeySalt)
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		resp["has_recovery_key"] = false
	} else {
		return nil, err
	}

	// Trusted devices (table may not exist yet — non-fatal)
	var trustedDeviceCount int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM trusted_recovery_devices WHERE user_id = $1`, userID).Scan(&trustedDeviceCount); err != nil {
		resp["has_trusted_devices"] = false
	} else {
		resp["has_trusted_devices"] = trustedDeviceCount > 0
	}

	// Recovery circles (table may not exist yet — non-fatal)
	var recoveryCircleCount int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM recovery_circles WHERE user_id = $1`, userID).Scan(&recoveryCircleCount); err != nil {
		resp["has_recovery_circle"] = false
	} else {
		resp["has_recovery_circle"] = recoveryCircleCount > 0
	}

	return resp, nil
}

// RecoveryVerifyCode validates the recovery code and returns a recovery token plus recovery key data.
func (h *Handler) RecoveryVerifyCode(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required"`
		Code  string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Email and code are required"})
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	code, errMsg := validateRecoveryCodeFormat(req.Code)
	if errMsg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsg})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	redisKey := recoveryRedisKey(email)
	record := h.fetchRecoveryRecord(ctx, c, redisKey)
	if record == nil {
		return
	}

	if !h.verifyRecoveryCode(ctx, c, code, record, redisKey) {
		return
	}

	// Code matches — delete Redis key (single use)
	h.redis.Del(ctx, redisKey)

	// Generate recovery JWT token
	if h.mfaChecker == nil {
		h.log.Error("MFA checker not configured for recovery token generation")
		c.JSON(http.StatusInternalServerError, gin.H{"error": errRecoveryVerificationFailed})
		return
	}
	recoveryToken, _, err := h.mfaChecker.GenerateRecoveryToken(record.UserID)
	if err != nil {
		h.log.Error("Failed to generate recovery token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errRecoveryVerificationFailed})
		return
	}

	resp, err := h.buildVerifyCodeResponse(recoveryToken, record.UserID)
	if err != nil {
		h.log.Error("Failed to load recovery key data", "error", err, "user_id", record.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errRecoveryVerificationFailed})
		return
	}

	h.log.Info("Recovery code verified, token issued", "user_id", record.UserID)
	c.JSON(http.StatusOK, resp)
}

// validateAndConsumeRecoveryToken validates the recovery token via MFA checker and enforces single-use.
// On success returns the claims and the Redis key used for single-use tracking.
// On failure writes the HTTP response and returns nil.
func (h *Handler) validateAndConsumeRecoveryToken(c *gin.Context, tokenStr string, recordAuthFailure bool) (*RecoveryClaims, string) {
	if h.mfaChecker == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errRecoveryNotConfigured})
		return nil, ""
	}
	claims, err := h.mfaChecker.ValidateRecoveryToken(tokenStr)
	if err != nil {
		if recordAuthFailure {
			middleware.RecordAuthFailure(c.Request.Context(), h.redis, c.ClientIP(), middleware.DefaultAuthBanConfig())
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidExpiredRecoveryToken})
		return nil, ""
	}

	ctx := c.Request.Context()
	recoveryUsedKey := fmt.Sprintf("recovery_token_used:%s", claims.JTI)
	_, err = h.redis.SetArgs(ctx, recoveryUsedKey, "1", redis.SetArgs{TTL: recoveryTokenUsedTTL, Mode: "NX"}).Result()
	if errors.Is(err, redis.Nil) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Recovery token has already been used"})
		return nil, ""
	}
	if err != nil {
		h.log.Error("Failed to check recovery token usage", "error_class", "token_usage_check")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process recovery"})
		return nil, ""
	}
	return claims, recoveryUsedKey
}

// prepareRecoveryPassword validates password strength, hashes it, and decodes E2EE key fields.
// On failure writes the HTTP response (and cleans up the Redis used-key) and returns a non-nil error.
func (h *Handler) prepareRecoveryPassword(ctx context.Context, c *gin.Context, password, wrappedB64, saltB64, recoveryUsedKey string) (string, []byte, []byte, error) {
	if err := ValidatePasswordStrength(password); err != nil {
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return "", nil, nil, err
	}
	passwordHash, err := HashPassword(password)
	if err != nil {
		h.log.Error("Failed to hash new password", "error_class", "password_hash")
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedResetPwd})
		return "", nil, nil, err
	}
	wrappedKey, err := base64.StdEncoding.DecodeString(wrappedB64)
	if err != nil {
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid wrapped_private_key format"})
		return "", nil, nil, err
	}
	kdSalt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid key_derivation_salt format"})
		return "", nil, nil, err
	}
	return passwordHash, wrappedKey, kdSalt, nil
}

// recoveryTxOp is a single SQL statement executed inside a recovery transaction.
type recoveryTxOp struct {
	query string
	args  []interface{}
	desc  string // human-readable for error logging
}

type recoveryPresenceResult struct {
	Operation presencehistory.AudienceOperation
	Plan      presencehistory.DeliveryPlan
}

type recoveryWork func(context.Context, *sql.Tx) (recoveryPresenceResult, error)

// beginRecoveryFence publishes the credential-epoch blocked marker (#2201)
// before a recovery reset transaction. Fails closed (503/500) when the fence
// is unwired or entropy fails — a destructive credential flow must never run
// without epoch rotation. On failure the recovery token's used-marker is
// released so the caller may retry.
func (h *Handler) beginRecoveryFence(c *gin.Context, userID, recoveryUsedKey, errMsg string) (*credepoch.Op, bool) {
	ctx := c.Request.Context()
	if h.credFence == nil {
		h.log.Error("Recovery blocked: credential fence not wired", "error_class", "fence_missing")
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsg})
		return nil, false
	}
	fenceOp, err := h.credFence.Begin(ctx, userID)
	if err != nil {
		h.log.Error("Recovery blocked: fence begin failed", "error_class", "fence_begin")
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsg})
		return nil, false
	}
	return fenceOp, true
}

// recoveryEpochRotationOps locks the user row and rotates the credential
// epoch (#2201). These MUST be the first ops in every recovery reset: the
// recovery path takes no user-row lock of its own, and the lock is what
// serializes the reset against concurrent sensitive-write GuardTx FOR SHARE
// reads. The SELECT executes via Exec (rows discarded) — the lock is the point.
func recoveryEpochRotationOps(userID string, fenceOp *credepoch.Op) []recoveryTxOp {
	return []recoveryTxOp{
		{
			`SELECT credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`,
			[]interface{}{userID},
			"Failed to lock user for epoch rotation",
		},
		{
			`UPDATE users SET credential_epoch = $1, updated_at = NOW() WHERE id = $2`,
			[]interface{}{fenceOp.NewEpochValue(), userID},
			"Failed to rotate credential epoch",
		},
	}
}

// recoveryPresenceOverrideResetOps discards preference ciphertext encrypted
// under the pre-recovery password-derived key. Deleting the preference
// cascades to its materialized exception rows. The shared forced-clear helper
// archives and erases Custom Status before this deletion in the same tx.
func recoveryPresenceOverrideResetOps(userID string) []recoveryTxOp {
	return []recoveryTxOp{
		{
			`DELETE FROM presence_override_preferences WHERE user_id = $1`,
			[]interface{}{userID},
			"Failed to reset presence override preferences",
		},
	}
}

func (h *Handler) recoveryWork(senderID uuid.UUID, ops []recoveryTxOp) recoveryWork {
	return func(ctx context.Context, tx *sql.Tx) (recoveryPresenceResult, error) {
		forcedClear, err := h.presenceHistory.BeginForcedSecurityClear(ctx, tx, senderID)
		if err != nil {
			return recoveryPresenceResult{}, err
		}
		for _, op := range ops {
			if _, err := tx.ExecContext(ctx, op.query, op.args...); err != nil {
				return recoveryPresenceResult{}, fmt.Errorf("recovery statement %s: %w", op.desc, err)
			}
		}
		return recoveryPresenceResult{
			Operation: forcedClear.Operation,
			Plan:      forcedClear.Plan,
		}, nil
	}
}

// execRecoveryTx owns the typed recovery callback and preserves token
// semantics across definite rollback versus ambiguous commit classification.
//
// fenceOp (#2201) rides the same classification: a pre-commit failure is a
// DEFINITE rollback (fenceOp.Rollback — the deferred tx.Rollback ran before
// any commit attempt); completion success is a DEFINITE commit
// (fenceOp.Commit); a completion error that is not an explicit rollback is
// AMBIGUOUS — neither is called, the blocked marker's TTL plus DB
// read-through reconciles to whichever state actually committed.
func (h *Handler) execRecoveryTx(
	ctx context.Context,
	c *gin.Context,
	recoveryUsedKey string,
	errMsg string,
	fenceOp *credepoch.Op,
	work recoveryWork,
) (result recoveryPresenceResult, returnErr error) {
	tx, err := h.beginRecoveryTx(ctx, c, recoveryUsedKey, errMsg, fenceOp)
	if err != nil {
		return result, err
	}
	defer tx.Rollback() //nolint:errcheck
	defer func() {
		if rbErr := h.presenceHistory.RollbackTx(tx); rbErr != nil && rbErr != sql.ErrTxDone {
			returnErr = errors.Join(returnErr, rbErr)
			h.log.Error(errMsgFailedRollbackTx, "error_class", "rollback")
		}
	}()

	result, err = work(ctx, tx)
	if err != nil {
		h.log.Error("Recovery transaction failed", "error_class", "statement")
		h.redis.Del(ctx, recoveryUsedKey)
		if fenceOp != nil {
			fenceOp.Rollback(ctx) // definite: commit was never attempted
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsg})
		return result, err
	}
	completion := h.presenceHistory.CompleteForcedSecurityClear(
		ctx,
		tx,
		presencehistory.ForcedClearResult{
			Mode:      presencehistory.ForcedSecurityClear,
			Operation: result.Operation,
			Plan:      result.Plan,
		},
	)
	if completion.RequiresDisconnect() && h.hub != nil {
		h.hub.DisconnectUser(result.Operation.SenderID)
	}
	if completion.Err != nil {
		classifyRecoveryFenceCompletion(ctx, fenceOp, completion.Outcome)
		h.respondRecoveryPresenceFailure(c, errMsg, completion)
		return result, completion.Err
	}
	if fenceOp != nil {
		fenceOp.Commit(ctx)
	}
	return result, nil
}

// beginRecoveryTx opens the recovery transaction. A begin failure is a
// DEFINITE pre-commit failure: it releases the recovery token's used-marker,
// restores the fence (#2201), and writes the HTTP response.
func (h *Handler) beginRecoveryTx(ctx context.Context, c *gin.Context, recoveryUsedKey, errMsg string, fenceOp *credepoch.Op) (*sql.Tx, error) {
	tx, err := h.presenceHistory.BeginTx(ctx, nil)
	if err == nil && tx != nil {
		return tx, nil
	}
	if err == nil {
		err = errors.New("recovery transaction missing")
	}
	h.log.Error(errMsgFailedStartTransaction, "error_class", "begin")
	h.redis.Del(ctx, recoveryUsedKey)
	if fenceOp != nil {
		fenceOp.Rollback(ctx)
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsg})
	return nil, err
}

// classifyRecoveryFenceCompletion maps a failed forced-clear completion onto
// the fence op (#2201): an explicit rollback outcome is DEFINITE (restore the
// fence); any other completion error is an AMBIGUOUS commit — call neither
// Commit nor Rollback, so the blocked marker's TTL plus DB read-through
// reconciles to whichever state actually committed (spec §4.4 step 3c).
func classifyRecoveryFenceCompletion(ctx context.Context, fenceOp *credepoch.Op, outcome presencehistory.ForcedClearOutcome) {
	if fenceOp != nil && outcome == presencehistory.ForcedClearRolledBack {
		fenceOp.Rollback(ctx)
	}
}

func (h *Handler) executeRecoveryTransaction(
	c *gin.Context,
	senderID uuid.UUID,
	recoveryUsedKey string,
	errMsg string,
	fenceOp *credepoch.Op,
	ops []recoveryTxOp,
) error {
	if h.presenceHistory == nil {
		h.redis.Del(c.Request.Context(), recoveryUsedKey)
		if fenceOp != nil {
			fenceOp.Rollback(c.Request.Context())
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsg})
		return errors.New("recovery presence history unavailable")
	}
	workStarted := false
	err := h.presenceHistory.WithReadySenderMode(
		c.Request.Context(), senderID, presencehistory.ForcedSecurityClear, func() error {
			workStarted = true
			_, workErr := h.execRecoveryTx(
				c.Request.Context(), c, recoveryUsedKey, errMsg, fenceOp, h.recoveryWork(senderID, ops),
			)
			return workErr
		})
	if err != nil && !workStarted {
		h.redis.Del(c.Request.Context(), recoveryUsedKey)
		if fenceOp != nil {
			fenceOp.Rollback(c.Request.Context()) // definite: work never started
		}
		h.respondRecoveryReadinessFailure(c, errMsg, err)
	}
	return err
}

func (h *Handler) respondRecoveryReadinessFailure(c *gin.Context, errMsg string, err error) {
	status := http.StatusInternalServerError
	var serviceErr *presencehistory.ServiceError
	if errors.As(err, &serviceErr) {
		status = serviceErr.Status
		if serviceErr.RetryAfter > 0 {
			seconds := int64(serviceErr.RetryAfter.Round(time.Second) / time.Second)
			c.Header("Retry-After", strconv.FormatInt(seconds, 10))
		}
	}
	h.log.Error("Recovery presence readiness failed", "error_class", "readiness")
	c.JSON(status, gin.H{"error": errMsg})
}

func (h *Handler) respondRecoveryPresenceFailure(
	c *gin.Context,
	errMsg string,
	completion presencehistory.ForcedClearCompletion,
) {
	status := http.StatusInternalServerError
	if completion.Outcome == presencehistory.ForcedClearQuarantined {
		status = http.StatusServiceUnavailable
	}
	h.log.Error(
		"Recovery forced Custom Status clear failed",
		"error_class", recoveryForcedClearClass(completion.Outcome),
	)
	c.JSON(status, gin.H{"error": errMsg})
}

func recoveryForcedClearClass(outcome presencehistory.ForcedClearOutcome) string {
	switch outcome {
	case presencehistory.ForcedClearAcknowledged:
		return "acknowledged"
	case presencehistory.ForcedClearQuarantined:
		return "quarantined"
	case presencehistory.ForcedClearRolledBack:
		return "rolled_back"
	case presencehistory.ForcedClearSuperseded:
		return "superseded"
	default:
		return "unresolved"
	}
}

func (h *Handler) parseRecoverySenderID(
	c *gin.Context,
	claims *RecoveryClaims,
	recoveryUsedKey string,
) (uuid.UUID, bool) {
	senderID, err := uuid.Parse(claims.UserID)
	if err == nil {
		return senderID, true
	}
	h.redis.Del(c.Request.Context(), recoveryUsedKey)
	h.log.Error("Recovery identity validation failed", "error_class", "invalid_claim_identity")
	c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidExpiredRecoveryToken})
	return uuid.Nil, false
}

// decodeOptionalRecoveryKeys decodes optional recovery key fields from base64.
// Returns decoded bytes and an error message on failure.
func decodeOptionalRecoveryKeys(wrappedKey, keySalt, wrappedPrefsKey, prefsKeySalt string) (recKey, recSalt, recPrefsKey, recPrefsSalt []byte, errMsg string) {
	var err error
	recKey, err = base64.StdEncoding.DecodeString(wrappedKey)
	if err != nil {
		return nil, nil, nil, nil, "Invalid recovery_wrapped_private_key format"
	}
	recSalt, err = base64.StdEncoding.DecodeString(keySalt)
	if err != nil {
		return nil, nil, nil, nil, "Invalid recovery_key_salt format"
	}
	if wrappedPrefsKey != "" {
		recPrefsKey, err = base64.StdEncoding.DecodeString(wrappedPrefsKey)
		if err != nil {
			return nil, nil, nil, nil, "Invalid recovery_wrapped_prefs_key format"
		}
	}
	if prefsKeySalt != "" {
		recPrefsSalt, err = base64.StdEncoding.DecodeString(prefsKeySalt)
		if err != nil {
			return nil, nil, nil, nil, "Invalid recovery_prefs_key_salt format"
		}
	}
	return recKey, recSalt, recPrefsKey, recPrefsSalt, ""
}

// RecoveryResetPassword resets the user's password using a recovery token and re-wrapped keys.
func (h *Handler) RecoveryResetPassword(c *gin.Context) {
	var req struct {
		RecoveryToken             string `json:"recovery_token" binding:"required"`
		NewPassword               string `json:"new_password" binding:"required"`
		WrappedPrivateKey         string `json:"wrapped_private_key" binding:"required"`
		KeyDerivationSalt         string `json:"key_derivation_salt" binding:"required"`
		KeyDerivationAlg          string `json:"key_derivation_alg" binding:"required"`
		RecoveryWrappedPrivateKey string `json:"recovery_wrapped_private_key"`
		RecoveryKeySalt           string `json:"recovery_key_salt"`
		RecoveryWrappedPrefsKey   string `json:"recovery_wrapped_prefs_key"`
		RecoveryPrefsKeySalt      string `json:"recovery_prefs_key_salt"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	claims, recoveryUsedKey := h.validateAndConsumeRecoveryToken(c, req.RecoveryToken, true)
	if claims == nil {
		return
	}
	senderID, ok := h.parseRecoverySenderID(c, claims, recoveryUsedKey)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	passwordHash, wrappedKey, kdSalt, err := h.prepareRecoveryPassword(ctx, c, req.NewPassword, req.WrappedPrivateKey, req.KeyDerivationSalt, recoveryUsedKey)
	if err != nil {
		return
	}

	// #2201 review: decode the OPTIONAL recovery-key fields BEFORE the fence
	// side-effect. A bad-base64 field is client input; decoding it after
	// beginRecoveryFence (as before) let the early return strand the published
	// blocked: marker, fail-closing this user's HTTP/WS auth for the full 5m
	// blockedTTL. No transaction has started here, so validating first leaves
	// the fence untouched on bad input.
	var recoveryKeyOp *recoveryTxOp
	if req.RecoveryWrappedPrivateKey != "" && req.RecoveryKeySalt != "" {
		recKey, recSalt, recPrefsKey, recPrefsSalt, decErr := decodeOptionalRecoveryKeys(
			req.RecoveryWrappedPrivateKey, req.RecoveryKeySalt, req.RecoveryWrappedPrefsKey, req.RecoveryPrefsKeySalt)
		if decErr != "" {
			h.redis.Del(ctx, recoveryUsedKey)
			c.JSON(http.StatusBadRequest, gin.H{"error": decErr})
			return
		}
		recoveryKeyOp = &recoveryTxOp{
			`INSERT INTO user_recovery_keys (user_id, recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (user_id) DO UPDATE SET
			     recovery_wrapped_private_key = EXCLUDED.recovery_wrapped_private_key,
			     recovery_key_salt = EXCLUDED.recovery_key_salt,
			     recovery_wrapped_prefs_key = EXCLUDED.recovery_wrapped_prefs_key,
			     recovery_prefs_key_salt = EXCLUDED.recovery_prefs_key_salt,
			     updated_at = NOW()`,
			[]interface{}{claims.UserID, recKey, recSalt, recPrefsKey, recPrefsSalt}, "Failed to upsert recovery key"}
	}

	fenceOp, ok := h.beginRecoveryFence(c, claims.UserID, recoveryUsedKey, errFailedResetPwd)
	if !ok {
		return
	}

	ops := make([]recoveryTxOp, 0, 7)
	ops = append(ops, recoveryEpochRotationOps(claims.UserID, fenceOp)...)
	ops = append(ops,
		recoveryTxOp{`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
			[]interface{}{passwordHash, claims.UserID}, "Failed to update password"},
		recoveryTxOp{`UPDATE user_keys SET wrapped_private_key = $1, key_derivation_salt = $2, key_derivation_alg = $3,
		         key_version = key_version + 1, updated_at = NOW() WHERE user_id = $4`,
			[]interface{}{wrappedKey, kdSalt, req.KeyDerivationAlg, claims.UserID}, "Failed to update user keys"},
	)
	if recoveryKeyOp != nil {
		ops = append(ops, *recoveryKeyOp)
	}

	ops = append(ops, recoveryPresenceOverrideResetOps(claims.UserID)...)
	ops = append(ops, recoveryTxOp{
		`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`,
		[]interface{}{claims.UserID}, "Failed to revoke refresh tokens"})

	if err := h.executeRecoveryTransaction(
		c, senderID, recoveryUsedKey, errFailedResetPwd, fenceOp, ops,
	); err != nil {
		return
	}

	middleware.ClearAuthFailures(ctx, h.redis, c.ClientIP())
	h.log.Info("Password reset via recovery", "operation", "forced_security_clear")
	c.JSON(http.StatusOK, gin.H{"message": "Password reset successfully. Please sign in with your new password."})
}

// RecoveryResetAccount performs a full account reset (new keys, all encrypted data lost).
func (h *Handler) RecoveryResetAccount(c *gin.Context) {
	var req struct {
		RecoveryToken       string `json:"recovery_token" binding:"required"`
		NewPassword         string `json:"new_password" binding:"required"`
		WrappedPrivateKey   string `json:"wrapped_private_key" binding:"required"`
		KeyDerivationSalt   string `json:"key_derivation_salt" binding:"required"`
		KeyDerivationAlg    string `json:"key_derivation_alg" binding:"required"`
		PublicKey           string `json:"public_key" binding:"required"`
		AcknowledgeDataLoss bool   `json:"acknowledge_data_loss"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return
	}

	if !req.AcknowledgeDataLoss {
		c.JSON(http.StatusBadRequest, gin.H{"error": "You must acknowledge that all encrypted message history will be permanently lost"})
		return
	}

	claims, recoveryUsedKey := h.validateAndConsumeRecoveryToken(c, req.RecoveryToken, false)
	if claims == nil {
		return
	}
	senderID, ok := h.parseRecoverySenderID(c, claims, recoveryUsedKey)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	passwordHash, wrappedKey, kdSalt, err := h.prepareRecoveryPassword(ctx, c, req.NewPassword, req.WrappedPrivateKey, req.KeyDerivationSalt, recoveryUsedKey)
	if err != nil {
		return
	}

	publicKey, decErr := base64.StdEncoding.DecodeString(req.PublicKey)
	if decErr != nil {
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid public_key format"})
		return
	}

	fenceOp, fok := h.beginRecoveryFence(c, claims.UserID, recoveryUsedKey, errFailedResetAccount)
	if !fok {
		return
	}

	ops := make([]recoveryTxOp, 0, 11)
	ops = append(ops, recoveryEpochRotationOps(claims.UserID, fenceOp)...)
	ops = append(ops,
		recoveryTxOp{`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
			[]interface{}{passwordHash, claims.UserID}, "Failed to update password"},
		recoveryTxOp{`UPDATE user_keys SET wrapped_private_key = $1, key_derivation_salt = $2, key_derivation_alg = $3,
		         key_version = key_version + 1, updated_at = NOW() WHERE user_id = $4`,
			[]interface{}{wrappedKey, kdSalt, req.KeyDerivationAlg, claims.UserID}, "Failed to update user keys"},
		recoveryTxOp{`UPDATE public_keys SET public_key = $1, key_version = key_version + 1, created_at = NOW() WHERE user_id = $2`,
			[]interface{}{publicKey, claims.UserID}, "Failed to update public key"},
		recoveryTxOp{`DELETE FROM user_recovery_keys WHERE user_id = $1`,
			[]interface{}{claims.UserID}, "Failed to delete recovery keys"},
		recoveryTxOp{`DELETE FROM channel_keys WHERE user_id = $1`,
			[]interface{}{claims.UserID}, "Failed to delete channel keys"},
		recoveryTxOp{`DELETE FROM dm_channel_keys WHERE user_id = $1`,
			[]interface{}{claims.UserID}, "Failed to delete DM channel keys"},
	)
	ops = append(ops, recoveryPresenceOverrideResetOps(claims.UserID)...)
	ops = append(ops, recoveryTxOp{
		`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`,
		[]interface{}{claims.UserID}, "Failed to revoke refresh tokens",
	})

	if err := h.executeRecoveryTransaction(
		c, senderID, recoveryUsedKey, errFailedResetAccount, fenceOp, ops,
	); err != nil {
		return
	}

	h.log.Info("Account reset via recovery (data loss acknowledged)", "operation", "forced_security_clear")
	c.JSON(http.StatusOK, gin.H{"message": "Account reset successfully. All encrypted message history has been permanently lost. Please sign in with your new password."})
}

// ── Trusted Device Recovery Endpoints ───────────────────────────────────────

// CreateDeviceRecoveryRequest initiates a trusted-device recovery request.
// This is an unauthenticated endpoint — the caller must provide a valid recovery token.
func (h *Handler) CreateDeviceRecoveryRequest(c *gin.Context) {
	var req struct {
		RecoveryToken      string `json:"recovery_token" binding:"required"`
		EphemeralPublicKey string `json:"ephemeral_public_key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recovery_token and ephemeral_public_key are required"})
		return
	}

	// Validate recovery token
	if h.mfaChecker == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errRecoveryNotConfigured})
		return
	}
	claims, err := h.mfaChecker.ValidateRecoveryToken(req.RecoveryToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidExpiredRecoveryToken})
		return
	}

	ctx := c.Request.Context()

	// Check user has trusted devices
	var deviceCount int
	err = h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM trusted_recovery_devices WHERE user_id = $1`, claims.UserID,
	).Scan(&deviceCount)
	if err != nil {
		h.log.Error("Failed to check trusted devices", "error", err, "user_id", claims.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedCreateRecoveryRequest})
		return
	}
	if deviceCount == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No trusted devices configured"})
		return
	}

	// Base64-decode ephemeral public key
	ephPubKey, err := base64.StdEncoding.DecodeString(req.EphemeralPublicKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ephemeral_public_key format (must be base64)"})
		return
	}

	// Insert recovery request with 15-minute expiry
	var requestID string
	err = h.db.QueryRowContext(ctx, `
		INSERT INTO recovery_requests (user_id, recovery_token_jti, ephemeral_public_key, expires_at)
		VALUES ($1, $2, $3, NOW() + INTERVAL '15 minutes')
		RETURNING id
	`, claims.UserID, claims.JTI, ephPubKey).Scan(&requestID)
	if err != nil {
		h.log.Error(errFailedCreateRecoveryRequest, "error", err, "user_id", claims.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFailedCreateRecoveryRequest})
		return
	}

	// NOTE: WebSocket notification to trusted device sessions is deferred.
	// The auth handler's hub field is a SessionDisconnector interface which
	// does not expose BroadcastToUser. The client polls via
	// GET /recovery/device-request/:id as a fallback until the interface is extended.

	h.log.Info("Device recovery request created", "user_id", claims.UserID, "request_id", requestID)
	c.JSON(http.StatusOK, gin.H{"request_id": requestID})
}

// extractRecoveryTokenParam extracts a recovery token from the Authorization: Bearer header
// or the recovery_token query parameter. Returns empty string if not found.
func extractRecoveryTokenParam(c *gin.Context) string {
	if authHeader := c.GetHeader("Authorization"); strings.HasPrefix(authHeader, bearerPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(authHeader, bearerPrefix))
	}
	return c.Query("recovery_token")
}

// validateRecoveryTokenParam extracts, validates, and returns recovery claims.
// On failure writes the HTTP response and returns nil.
func (h *Handler) validateRecoveryTokenParam(c *gin.Context) *RecoveryClaims {
	recoveryToken := extractRecoveryTokenParam(c)
	if recoveryToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Recovery token is required (Authorization: Bearer header or recovery_token query parameter)"})
		return nil
	}
	if h.mfaChecker == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errRecoveryNotConfigured})
		return nil
	}
	claims, err := h.mfaChecker.ValidateRecoveryToken(recoveryToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidExpiredRecoveryToken})
		return nil
	}
	return claims
}

// PollDeviceRecoveryRequest polls the status of a trusted-device recovery request.
// This is an unauthenticated endpoint — the caller must provide a valid recovery token.
func (h *Handler) PollDeviceRecoveryRequest(c *gin.Context) {
	requestID := c.Param("id")

	claims := h.validateRecoveryTokenParam(c)
	if claims == nil {
		return
	}

	// Fetch the recovery request (JTI-scoped to prevent cross-token access)
	var status string
	var encryptedPayload, responderPublicKey []byte
	var expiresAt time.Time
	err := h.db.QueryRowContext(c.Request.Context(), `
		SELECT status, encrypted_payload, responder_public_key, expires_at
		FROM recovery_requests
		WHERE id = $1 AND user_id = $2 AND recovery_token_jti = $3
	`, requestID, claims.UserID, claims.JTI).Scan(&status, &encryptedPayload, &responderPublicKey, &expiresAt)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Recovery request not found"})
		return
	}

	// Check if expired
	if time.Now().After(expiresAt) && status == "pending" {
		c.JSON(http.StatusOK, gin.H{"status": "expired"})
		return
	}

	response := gin.H{"status": status}
	if status == "approved" && encryptedPayload != nil {
		response["encrypted_payload"] = base64.StdEncoding.EncodeToString(encryptedPayload)
	}
	if status == "approved" && responderPublicKey != nil {
		response["responder_public_key"] = base64.StdEncoding.EncodeToString(responderPublicKey)
	}

	c.JSON(http.StatusOK, response)
}

// ── Social Recovery Request Endpoints ────────────────────────────────────────

// CreateSocialRecoveryRequest creates a social recovery request for Shamir secret sharing.
// This is an unauthenticated endpoint — the caller provides a valid recovery token.
func (h *Handler) CreateSocialRecoveryRequest(c *gin.Context) {
	var req struct {
		RecoveryToken      string `json:"recovery_token" binding:"required"`
		EphemeralPublicKey string `json:"ephemeral_public_key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recovery_token and ephemeral_public_key are required"})
		return
	}

	// Validate recovery token
	if h.mfaChecker == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errRecoveryNotConfigured})
		return
	}
	claims, err := h.mfaChecker.ValidateRecoveryToken(req.RecoveryToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errInvalidExpiredRecoveryToken})
		return
	}

	ctx := c.Request.Context()

	// Find user's recovery circle
	var circleID string
	var thresholdK int
	err = h.db.QueryRowContext(ctx,
		`SELECT id, threshold_k FROM recovery_circles WHERE user_id = $1`, claims.UserID,
	).Scan(&circleID, &thresholdK)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No social recovery circle configured"})
		return
	}

	// Base64-decode ephemeral public key
	ephPubKey, err := base64.StdEncoding.DecodeString(req.EphemeralPublicKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ephemeral_public_key format (must be base64)"})
		return
	}

	// Insert social recovery request with 24-hour expiry
	var requestID string
	err = h.db.QueryRowContext(ctx, `
		INSERT INTO recovery_circle_requests (circle_id, user_id, recovery_token_jti, ephemeral_public_key, expires_at)
		VALUES ($1, $2, $3, $4, NOW() + INTERVAL '24 hours')
		RETURNING id
	`, circleID, claims.UserID, claims.JTI, ephPubKey).Scan(&requestID)
	if err != nil {
		h.log.Error("Failed to create social recovery request", "error", err, "user_id", claims.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create social recovery request"})
		return
	}

	h.log.Info("Social recovery request created", "user_id", claims.UserID, "request_id", requestID, "threshold_k", thresholdK)
	c.JSON(http.StatusOK, gin.H{"request_id": requestID, "threshold_k": thresholdK})
}

// socialRecoveryResponseEntry represents a single guardian's share response.
type socialRecoveryResponseEntry struct {
	ContactID      string `json:"contact_id"`
	EncryptedShare string `json:"encrypted_share"`
}

// fetchSocialRecoveryResponses loads all guardian share responses for a completed request.
func (h *Handler) fetchSocialRecoveryResponses(ctx context.Context, requestID string) ([]socialRecoveryResponseEntry, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT contact_id, encrypted_share
		FROM recovery_circle_responses
		WHERE request_id = $1
	`, requestID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var responses []socialRecoveryResponseEntry
	for rows.Next() {
		var r socialRecoveryResponseEntry
		var encShare []byte
		if err := rows.Scan(&r.ContactID, &encShare); err != nil {
			h.log.Error("Failed to scan social recovery response", "error", err)
			continue
		}
		r.EncryptedShare = base64.StdEncoding.EncodeToString(encShare)
		responses = append(responses, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return responses, nil
}

// PollSocialRecoveryRequest polls the status of a social recovery request.
// This is an unauthenticated endpoint — the caller provides a valid recovery token.
func (h *Handler) PollSocialRecoveryRequest(c *gin.Context) {
	requestID := c.Param("id")

	claims := h.validateRecoveryTokenParam(c)
	if claims == nil {
		return
	}

	ctx := c.Request.Context()

	// Fetch the social recovery request (JTI-scoped to prevent cross-token access)
	var status string
	var sharesReceived, thresholdK int
	var expiresAt time.Time
	err := h.db.QueryRowContext(ctx, `
		SELECT rr.status, rr.shares_received, rc.threshold_k, rr.expires_at
		FROM recovery_circle_requests rr
		JOIN recovery_circles rc ON rc.id = rr.circle_id
		WHERE rr.id = $1 AND rr.user_id = $2 AND rr.recovery_token_jti = $3
	`, requestID, claims.UserID, claims.JTI).Scan(&status, &sharesReceived, &thresholdK, &expiresAt)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Social recovery request not found"})
		return
	}

	if time.Now().After(expiresAt) && status == "pending" {
		c.JSON(http.StatusOK, gin.H{"status": "expired"})
		return
	}

	response := gin.H{
		"status":          status,
		"shares_received": sharesReceived,
		"threshold_k":     thresholdK,
	}

	if status == "complete" {
		responses, err := h.fetchSocialRecoveryResponses(ctx, requestID)
		if err != nil {
			h.log.Error("Failed to load social recovery responses", "error", err, "request_id", requestID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load recovery responses"})
			return
		}
		response["responses"] = responses
	}

	c.JSON(http.StatusOK, response)
}

// listSSOProviders returns the provider names linked to a user, ordered by
// created_at ASC (earliest-linked first). Used by the /login short-circuit
// when an account has password_login_disabled=TRUE. // pragma: allowlist secret
func (h *Handler) listSSOProviders(ctx context.Context, userID string) ([]string, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT provider FROM user_sso_identities
		 WHERE user_id = $1 ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("listSSOProviders: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}
