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

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/attestation"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/email"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
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
	GenerateLoginChallenge(ctx context.Context, userID string, rememberMe bool) (token string, jti string, err error)
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
	headerMachineID = "X-Machine-Id"
	headerUserAgent = "User-Agent"

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
	db         *sql.DB
	redis      *redis.Client
	log        *logger.Logger
	jwtSecret  string
	hub        SessionDisconnector
	mfaChecker MFAChecker
	emailSvc   *email.Service
	pending    *PendingRepo
	entCache   *entitlements.Cache
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
// refresh token. Used by issueSessionTokens and other flows.
func (h *Handler) generateTokenPair(c *gin.Context, userID string, emailVerified bool) (accessToken, refreshToken, tokenID string, err error) {
	tier := h.entCache.GetTier(c.Request.Context(), userID)
	accessToken, err = GenerateAccessToken(userID, h.jwtSecret, emailVerified, tier)
	if err != nil {
		return "", "", "", fmt.Errorf("access: %w", err)
	}
	refreshToken, err = GenerateRefreshToken()
	if err != nil {
		return "", "", "", fmt.Errorf("refresh: %w", err)
	}

	tokenHash := HashRefreshToken(refreshToken)
	tokenID = uuid.New().String()
	expiresAt := time.Now().Add(30 * 24 * time.Hour)
	machineID := c.GetHeader(headerMachineID)

	_, err = h.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tokenID, userID, tokenHash, c.GetHeader("X-Device-Name"),
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

	if err := h.redis.Set(ctx, redisKey(pendingID), raw, VerifyCodeTTLNew).Err(); err != nil {
		return time.Time{}, fmt.Errorf("store code in Redis: %w", err)
	}

	// Test-only: expose plaintext code for integration tests. Guarded by the
	// CONCORD_ENV=test env var so this never fires in production.
	if isTestEnv() {
		if err := h.redis.Set(ctx, redisKeyTestOnly+pendingID, code, VerifyCodeTTLNew).Err(); err != nil {
			h.log.Warn("Failed to write test_only code", "error", err)
		}
	}

	if err := h.emailSvc.SendVerificationCode(userEmail, code); err != nil {
		// Best-effort cleanup so a retry starts fresh.
		_ = h.redis.Del(ctx, redisKey(pendingID)).Err()
		if isTestEnv() {
			_ = h.redis.Del(ctx, redisKeyTestOnly+pendingID).Err()
		}
		return time.Time{}, fmt.Errorf("send email: %w", err)
	}

	return time.Now().Add(VerifyCodeTTLNew), nil
}

const (
	// redisKeyTestOnly is the Redis key prefix for plaintext verification codes
	// written in test environments only (CONCORD_ENV=test).
	redisKeyTestOnly = "test_only:"

	// errMsgInternalError is the generic internal error message returned to clients.
	errMsgInternalError = "Internal error"
)

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

	rec, err := h.fetchVerificationRecord(ctx, c, req.PendingID)
	if err != nil {
		return
	}

	if !h.attemptsGuard(ctx, c, req.PendingID, rec, sanitized) {
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

	_ = h.redis.Del(ctx, redisKey(req.PendingID)).Err()
	_ = h.redis.Del(ctx, redisKeyTestOnly+req.PendingID).Err()

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

// fetchVerificationRecord retrieves and unmarshals the verification record from
// Redis. On redis.Nil it writes a 410 code_expired response and returns a non-nil
// error so the caller can return immediately. Other errors write a 500.
func (h *Handler) fetchVerificationRecord(ctx context.Context, c *gin.Context, pendingID string) (*verificationRecord, error) {
	raw, err := h.redis.Get(ctx, redisKey(pendingID)).Result()
	if errors.Is(err, redis.Nil) {
		c.JSON(http.StatusGone, gin.H{"code": "code_expired"})
		return nil, err
	}
	if err != nil {
		h.log.Error("confirm: redis get failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalError})
		return nil, err
	}
	var rec verificationRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		h.log.Error("confirm: record unmarshal failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalError})
		return nil, err
	}
	return &rec, nil
}

// attemptsGuard enforces the attempt limit and validates the code in one step.
// Fix 4: the limit is checked BEFORE incrementing so attempts_remaining is accurate.
// Fix 5: TTL is guarded against zero/negative so the Redis key always has an expiry.
// Returns true if the code matched and the caller should proceed; false if a response
// has already been written (too_many_attempts or invalid_code).
func (h *Handler) attemptsGuard(ctx context.Context, c *gin.Context, pendingID string, rec *verificationRecord, sanitized string) bool {
	if rec.Attempts >= MaxCodeAttempts {
		_ = h.redis.Del(ctx, redisKey(pendingID)).Err()
		_ = h.redis.Del(ctx, redisKeyTestOnly+pendingID).Err()
		c.JSON(http.StatusTooManyRequests, gin.H{"code": "too_many_attempts"})
		return false
	}
	rec.Attempts++

	if subtle.ConstantTimeCompare([]byte(hashCode(sanitized)), []byte(rec.CodeHash)) != 1 {
		newRaw, _ := json.Marshal(rec)
		// Fix 5: guard against zero/negative TTL so the key always has an expiry.
		ttl := h.redis.TTL(ctx, redisKey(pendingID)).Val()
		if ttl <= 0 {
			ttl = VerifyCodeTTLNew
		}
		_ = h.redis.Set(ctx, redisKey(pendingID), newRaw, ttl).Err()
		c.JSON(http.StatusUnauthorized, gin.H{
			"code":               "invalid_code",
			"attempts_remaining": MaxCodeAttempts - rec.Attempts,
		})
		return false
	}
	return true
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

	_ = h.redis.Del(ctx, redisKey(pending.ID)).Err()
	codeExpires, err := h.sendInitialCode(ctx, pending.ID, pending.Email)
	if err != nil {
		_ = h.pending.RevertResend(ctx, pending.ID)
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

	_ = h.redis.Del(ctx, redisKey(req.PendingID)).Err()
	newEmailLower := strings.ToLower(strings.TrimSpace(req.NewEmail))
	codeExpires, err := h.sendInitialCode(ctx, req.PendingID, newEmailLower)
	if err != nil {
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
	_ = h.redis.Del(ctx, redisKey(pendingID)).Err()
	_ = h.redis.Del(ctx, redisKeyTestOnly+pendingID).Err()

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
func (h *Handler) lookupUserForLogin(email string) (models.User, error) {
	var user models.User
	err := h.db.QueryRow(
		`SELECT id, email, username, password_hash, display_name, bio, avatar_url, COALESCE(links, '[]'::jsonb),
		        email_verified, age_verified, created_at, updated_at, password_login_disabled, disabled
		 FROM users WHERE email = $1`,
		email,
	).Scan(
		&user.ID, &user.Email, &user.Username, &user.PasswordHash,
		&user.DisplayName, &user.Bio, &user.AvatarURL, &user.Links,
		&user.EmailVerified, &user.AgeVerified,
		&user.CreatedAt, &user.UpdatedAt, &user.PasswordLoginDisabled, &user.Disabled,
	)
	return user, err
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

func (h *Handler) handleMFAChallenge(ctx context.Context, c *gin.Context, userID string, rememberMe bool) {
	allMethods, _ := h.mfaChecker.GetEnabledMethods(ctx, userID)
	loginMethods, _ := h.mfaChecker.GetLoginMethods(ctx, userID)
	challengeToken, jti, mfaErr := h.mfaChecker.GenerateLoginChallenge(ctx, userID, rememberMe)
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

	user, err := h.lookupUserForLogin(req.Email)
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
		h.handleMFAChallenge(ctx, c, user.ID, rememberMe)
		return
	}

	h.CompleteLogin(c, user.ID, rememberMe)
}

// CompleteLogin issues tokens and creates a session for the given user.
// Called directly from Login (no MFA) or from the MFA verify handler after successful verification.
func (h *Handler) CompleteLogin(c *gin.Context, userID string, rememberMe bool) {
	// Look up email_verified + disabled. email_verified feeds the JWT claim;
	// disabled gates token issuance here so a terminal age-disable (#1623) that
	// landed DURING an MFA challenge blocks completion before any token is minted
	// (the disable/MFA-window race). This runs on every CompleteLogin path (direct
	// and post-MFA-verify), so it backs the Login-handler gate symmetrically.
	var emailVerified bool
	var disabled bool
	if err := h.db.QueryRow(`SELECT email_verified, disabled FROM users WHERE id = $1`, userID).Scan(&emailVerified, &disabled); err != nil {
		h.log.Error("Failed to look up email_verified for login", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}
	if disabled {
		c.JSON(http.StatusForbidden, gin.H{"error_code": "account_disabled"})
		return
	}

	// Generate access token (JWT, 15 min)
	tier := h.entCache.GetTier(c.Request.Context(), userID)
	accessToken, err := GenerateAccessToken(userID, h.jwtSecret, emailVerified, tier)
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
	tokenID := uuid.New().String()
	expiresAt := time.Now().Add(30 * 24 * time.Hour)
	deviceName := c.GetHeader("X-Device-Name")
	ipAddress := c.ClientIP()
	userAgent := truncateUserAgent(c.GetHeader(headerUserAgent))
	machineID := c.GetHeader(headerMachineID)

	h.log.Info("CompleteLogin: storing refresh token",
		"request_id", c.GetString(middleware.RequestIDContextKey),
		"remember_me", rememberMe,
		"machine_id", machineID,
		"user_id", userID,
	)

	// Revoke old sessions from the same device
	if machineID != "" {
		_, _ = h.db.Exec(
			`UPDATE refresh_tokens SET revoked_at = NOW()
			 WHERE user_id = $1 AND machine_id = $2 AND revoked_at IS NULL`,
			userID, machineID,
		)
	} else {
		_, _ = h.db.Exec(
			`UPDATE refresh_tokens SET revoked_at = NOW()
			 WHERE user_id = $1 AND ip_address = $2 AND user_agent = $3 AND revoked_at IS NULL`,
			userID, ipAddress, userAgent,
		)
	}

	_, err = h.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tokenID, userID, tokenHash, deviceName, ipAddress, userAgent, expiresAt, rememberMe, nilIfEmpty(machineID),
	)
	if err != nil {
		h.log.Error("Failed to store refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// Cookie MaxAge: 30 days if remember_me, session-scoped otherwise
	cookieMaxAge := 30 * 24 * 60 * 60
	if !rememberMe {
		cookieMaxAge = 0
	}
	SetRefreshCookie(c, refreshToken, cookieMaxAge)

	// Fetch user data
	var user models.User
	err = h.db.QueryRow(
		`SELECT id, email, username, password_hash, display_name, bio, avatar_url, COALESCE(links, '[]'::jsonb),
		        email_verified, age_verified, created_at, updated_at, password_login_disabled
		 FROM users WHERE id = $1`, userID,
	).Scan(
		&user.ID, &user.Email, &user.Username, &user.PasswordHash,
		&user.DisplayName, &user.Bio, &user.AvatarURL, &user.Links,
		&user.EmailVerified, &user.AgeVerified,
		&user.CreatedAt, &user.UpdatedAt, &user.PasswordLoginDisabled,
	)
	if err != nil {
		h.log.Error("Failed to fetch user for login response", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

	// Fetch E2EE keys
	var keys models.UserKeys
	err = h.db.QueryRow(
		`SELECT user_id, wrapped_private_key, key_derivation_salt, key_version, key_derivation_alg
		 FROM user_keys WHERE user_id = $1`, userID,
	).Scan(&keys.UserID, &keys.WrappedPrivateKey, &keys.KeyDerivationSalt, &keys.KeyVersion, &keys.KeyDerivationAlg)
	if err != nil {
		h.log.Error("Failed to fetch user keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgLoginFailed})
		return
	}

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
	h.log.Info("Refresh attempt", "request_id", c.GetString(middleware.RequestIDContextKey), "machine_id", c.GetHeader(headerMachineID))

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
		// #1623: never grace-recover a disabled account. The terminal-disable tx
		// revokes the token, which routes here within the 30s grace window;
		// recovering would re-mint tokens for a disabled user, bypassing the gate
		// (this path is not covered by the rotateAndRespond conditional INSERT).
		// Fail closed on a lookup error — deny rather than recover.
		var graceDisabled bool
		if derr := h.db.QueryRow(`SELECT disabled FROM users WHERE id = $1`, revokedUserID).Scan(&graceDisabled); derr != nil || graceDisabled {
			h.log.Warn("Grace recovery denied: account disabled or lookup failed",
				"user_id", revokedUserID, "lookup_failed", derr != nil)
			c.JSON(http.StatusForbidden, gin.H{"error_code": "account_disabled"})
			return true
		}
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
		return h.handleTokenTheft(c, token, requestMachineID, storedIP, requestIP)
	}
	return h.handleSuspiciousMachineID(c, token, requestMachineID, requestIP)
}

// handleTokenTheft handles the case where both machine ID and IP differ — high risk token theft.
// Always returns true (response is written).
func (h *Handler) handleTokenTheft(c *gin.Context, token models.RefreshToken, requestMachineID, storedIP, requestIP string) bool {
	h.log.Error("TOKEN THEFT DETECTED: different machine_id and different IP",
		"user_id", token.UserID, "stored_machine_id", token.MachineID,
		"request_machine_id", requestMachineID, "stored_ip", storedIP, "request_ip", requestIP)

	_, _ = h.db.Exec(
		`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`,
		token.UserID)

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
	challengeToken, jti, mfaErr := h.mfaChecker.GenerateLoginChallenge(ctx, token.UserID, token.RememberMe)
	if mfaErr != nil {
		h.log.Error("Failed to generate suspicious refresh MFA challenge", "error", mfaErr)
		return false // Graceful degradation — allow
	}

	h.log.Warn("Suspicious refresh: different machine_id, same IP — MFA required",
		"user_id", token.UserID, "stored_machine_id", token.MachineID,
		"request_machine_id", requestMachineID, "ip", requestIP)

	resp := h.buildMFAChallengeResponse("suspicious_session_mfa", "Session verification required", challengeToken, token.UserID, jti)
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

	resp := h.buildMFAChallengeResponse("mfa_upgrade_required",
		"This session was created before MFA was enabled. Please verify your identity.",
		challengeToken, token.UserID, jti)
	c.JSON(http.StatusForbidden, resp)
	return true
}

// buildMFAChallengeResponse constructs the common MFA challenge JSON response with
// methods, recovery-only methods, and optional WebAuthn options.
func (h *Handler) buildMFAChallengeResponse(errorCode, message, challengeToken, userID, jti string) gin.H {
	ctx := context.Background()
	allMethods, _ := h.mfaChecker.GetEnabledMethods(ctx, userID)
	loginMethods, _ := h.mfaChecker.GetLoginMethods(ctx, userID)

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

	return resp
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
	// Revoke the old refresh token (single-use rotation)
	_, err := h.db.Exec(
		`UPDATE refresh_tokens SET revoked_at = NOW(), last_used_at = NOW() WHERE id = $1`,
		token.ID,
	)
	if err != nil {
		h.log.Error("Failed to revoke old refresh token", "error", err)
	}

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

	// Conditional INSERT (#1623): mint the new token ONLY if the account is not
	// disabled. This is the atomic close of the disable/refresh race — if the
	// terminal-disable tx committed between fetchActiveRefreshToken and here, the
	// EXISTS guard fails and no live token is minted (TOCTOU-free, unlike a
	// SELECT-then-INSERT). The old token was already revoked above.
	res, err := h.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id)
		 SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9
		 WHERE EXISTS (SELECT 1 FROM users WHERE id = $2 AND disabled = FALSE)`,
		newTokenID, token.UserID, newTokenHash, token.DeviceName,
		c.ClientIP(), truncateUserAgent(c.GetHeader(headerUserAgent)),
		newExpiry, token.RememberMe, nilIfEmpty(propagatedMachineID),
	)

	// Auto-purge old revoked sessions (> 90 days)
	_, _ = h.db.Exec(
		`DELETE FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NOT NULL AND revoked_at < NOW() - INTERVAL '90 days'`,
		token.UserID)
	if err != nil {
		h.log.Error("Failed to store new refresh token", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}
	// 0 rows => the EXISTS guard failed => account disabled (or a RowsAffected
	// failure => fail closed). Refuse; the old token is already revoked.
	if affected, raErr := res.RowsAffected(); raErr != nil || affected == 0 {
		c.JSON(http.StatusForbidden, gin.H{"error_code": "account_disabled"})
		return
	}

	SetRefreshCookie(c, newRefreshToken, cookieMaxAge)

	var refreshEmailVerified bool
	if err := h.db.QueryRow(`SELECT email_verified FROM users WHERE id = $1`, token.UserID).Scan(&refreshEmailVerified); err != nil {
		h.log.Error("Failed to look up email_verified for refresh", "error", err, "user_id", token.UserID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	tier := h.entCache.GetTier(c.Request.Context(), token.UserID)
	accessToken, err := GenerateAccessToken(token.UserID, h.jwtSecret, refreshEmailVerified, tier)
	if err != nil {
		h.log.Error(errMsgFailedAccessToken, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessToken,
		"refresh_token": newRefreshToken,
		"session_id":    newTokenID,
		"expires_in":    900,
	})
}

// handleGracePeriodRefresh recovers a session when a recently-revoked refresh token
// is replayed within the grace period. This happens when the client process was killed
// between the server revoking the old token and the client persisting the new one.
// We find the successor token (issued during the rotation that revoked the replayed
// token), revoke it, and issue a fresh token pair — effectively re-rotating.
func (h *Handler) handleGracePeriodRefresh(c *gin.Context, userID string, revokedTokenID string, revokedAt time.Time) {
	// Find the successor token: same user, created around the time the replayed token
	// was revoked (within a small window), and not yet revoked itself.
	var successor models.RefreshToken
	var successorMachineID sql.NullString
	err := h.db.QueryRow(
		`SELECT id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, created_at, last_used_at, remember_me, COALESCE(machine_id, '')
		 FROM refresh_tokens
		 WHERE user_id = $1
		   AND revoked_at IS NULL
		   AND created_at >= $2 - INTERVAL '2 seconds'
		   AND created_at <= $2 + INTERVAL '2 seconds'
		 ORDER BY created_at DESC
		 LIMIT 1`,
		userID, revokedAt,
	).Scan(
		&successor.ID,
		&successor.UserID,
		&successor.TokenHash,
		&successor.DeviceName,
		&successor.IPAddress,
		&successor.UserAgent,
		&successor.ExpiresAt,
		&successor.CreatedAt,
		&successor.LastUsedAt,
		&successor.RememberMe,
		&successorMachineID,
	)
	if successorMachineID.Valid {
		successor.MachineID = successorMachineID.String
	}
	if err != nil {
		// No successor found — can't recover; treat as stale replay
		h.log.Warn("Grace period replay but no successor token found",
			"user_id", userID,
			"revoked_token_id", revokedTokenID,
		)
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgInvalidRefreshToken})
		return
	}

	// Revoke the successor (it was never delivered to the client)
	_, err = h.db.Exec(
		`UPDATE refresh_tokens SET revoked_at = NOW(), last_used_at = NOW() WHERE id = $1`,
		successor.ID,
	)
	if err != nil {
		h.log.Error("Failed to revoke successor token during grace recovery", "error", err)
	}

	// Generate a fresh refresh token
	newRefreshToken, err := GenerateRefreshToken()
	if err != nil {
		h.log.Error("Failed to generate refresh token during grace recovery", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	// Determine expiry
	newExpiry := successor.ExpiresAt
	cookieMaxAge := 0
	if successor.RememberMe {
		newExpiry = time.Now().Add(30 * 24 * time.Hour)
		cookieMaxAge = 30 * 24 * 60 * 60
	}

	// Store the new token (propagate machine_id)
	newTokenHash := HashRefreshToken(newRefreshToken)
	newTokenID := uuid.New().String()
	graceMachineID := c.GetHeader(headerMachineID)
	if graceMachineID == "" {
		graceMachineID = successor.MachineID
	}
	// Conditional INSERT mirroring rotateAndRespond (#1623): the caller
	// attemptGracePeriodRecovery already gates on users.disabled, but that is a
	// SELECT-then-act; the EXISTS guard makes the grace mint atomically race-safe so
	// a disable committing between that check and this INSERT cannot mint a live token.
	res, err := h.db.Exec(
		`INSERT INTO refresh_tokens (id, user_id, token_hash, device_name, ip_address, user_agent, expires_at, remember_me, machine_id)
		 SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9
		 WHERE EXISTS (SELECT 1 FROM users WHERE id = $2 AND disabled = FALSE)`,
		newTokenID,
		userID,
		newTokenHash,
		successor.DeviceName,
		c.ClientIP(),
		truncateUserAgent(c.GetHeader(headerUserAgent)),
		newExpiry,
		successor.RememberMe,
		nilIfEmpty(graceMachineID),
	)
	if err != nil {
		h.log.Error("Failed to store new refresh token during grace recovery", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}
	if affected, raErr := res.RowsAffected(); raErr != nil || affected == 0 {
		// 0 rows => account disabled (or RowsAffected failed => fail closed).
		c.JSON(http.StatusForbidden, gin.H{"error_code": "account_disabled"})
		return
	}

	// Set cookie
	SetRefreshCookie(c, newRefreshToken, cookieMaxAge)

	// Look up current email_verified for grace recovery token
	var graceEmailVerified bool
	if err := h.db.QueryRow(`SELECT email_verified FROM users WHERE id = $1`, userID).Scan(&graceEmailVerified); err != nil {
		h.log.Error("Failed to look up email_verified for grace recovery", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	// Generate access token
	tier := h.entCache.GetTier(c.Request.Context(), userID)
	accessToken, err := GenerateAccessToken(userID, h.jwtSecret, graceEmailVerified, tier)
	if err != nil {
		h.log.Error("Failed to generate access token during grace recovery", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgRefreshFailed})
		return
	}

	h.log.Info("Session recovered via grace period refresh",
		"user_id", userID,
		"new_session_id", newTokenID,
	)

	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessToken,
		"refresh_token": newRefreshToken,
		"session_id":    newTokenID,
		"expires_in":    900,
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

// Logout handles session termination
func (h *Handler) blacklistAccessToken(c *gin.Context) {
	authHeader := c.GetHeader("Authorization")
	if !strings.HasPrefix(authHeader, bearerPrefix) {
		return
	}
	token := strings.TrimPrefix(authHeader, bearerPrefix)
	claims, err := ValidateAccessToken(token, h.jwtSecret)
	if err != nil || claims.ID == "" {
		return
	}
	remaining := time.Until(claims.ExpiresAt.Time)
	if blErr := middleware.BlacklistToken(context.Background(), h.redis, claims.ID, remaining); blErr != nil {
		h.log.Error("Failed to blacklist access token", "error", blErr)
	}
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
// Attestation cleanup is tied to the refresh-token revocation rather than the
// client-supplied X-Session-ID header. The /auth/logout route is rate-limited
// only (not behind AuthRequired) and X-Session-ID is not cross-checked against
// the bearer's session — an attacker could otherwise POST with arbitrary
// X-Session-ID and silently wipe another session's attestation tokens (sibling
// of #1142/#1154; finding #15 of the #1264 review). Driving cleanup off the
// `refresh_tokens.id` we just revoked closes the gap: only an attacker who
// already possesses the victim's refresh token can trigger the wipe, and they
// could already terminate the victim's session by other means at that point.
func (h *Handler) Logout(c *gin.Context) {
	h.blacklistAccessToken(c)

	refreshToken := h.extractRefreshToken(c)
	if refreshToken == "" {
		c.JSON(http.StatusOK, gin.H{"message": "Already logged out"})
		return
	}

	tokenHash := HashRefreshToken(refreshToken)
	var revokedUserID, revokedSessionID string
	_ = h.db.QueryRow(
		`UPDATE refresh_tokens SET revoked_at = NOW() WHERE token_hash = $1 RETURNING user_id, id`,
		tokenHash,
	).Scan(&revokedUserID, &revokedSessionID)

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

	SetRefreshCookie(c, "", -1)
	c.JSON(http.StatusOK, gin.H{"message": "Logged out successfully"})
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
		// Still return success to prevent enumeration — the code is in Redis for retry
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
		h.log.Error("Failed to check recovery token usage", "error", err)
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
		h.log.Error("Failed to hash new password", "error", err)
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

// execRecoveryTx runs a series of SQL ops in a transaction. On failure it cleans up
// the Redis used-key, writes the HTTP response, and returns an error.
func (h *Handler) execRecoveryTx(ctx context.Context, c *gin.Context, ops []recoveryTxOp, recoveryUsedKey, errMsg string) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error(errMsgFailedStartTransaction, "error", err)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsg})
		return err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error(errMsgFailedRollbackTx, "error", rbErr)
		}
	}()

	for _, op := range ops {
		if _, err := tx.Exec(op.query, op.args...); err != nil {
			h.log.Error(op.desc, "error", err)
			h.redis.Del(ctx, recoveryUsedKey)
			c.JSON(http.StatusInternalServerError, gin.H{"error": errMsg})
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		h.log.Error(errFailedCommitTransaction, "error", err)
		h.redis.Del(ctx, recoveryUsedKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsg})
		return err
	}
	return nil
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

	ctx := c.Request.Context()
	passwordHash, wrappedKey, kdSalt, err := h.prepareRecoveryPassword(ctx, c, req.NewPassword, req.WrappedPrivateKey, req.KeyDerivationSalt, recoveryUsedKey)
	if err != nil {
		return
	}

	ops := []recoveryTxOp{
		{`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
			[]interface{}{passwordHash, claims.UserID}, "Failed to update password"},
		{`UPDATE user_keys SET wrapped_private_key = $1, key_derivation_salt = $2, key_derivation_alg = $3,
		         key_version = key_version + 1, updated_at = NOW() WHERE user_id = $4`,
			[]interface{}{wrappedKey, kdSalt, req.KeyDerivationAlg, claims.UserID}, "Failed to update user keys"},
	}

	// Optional recovery key upsert
	if req.RecoveryWrappedPrivateKey != "" && req.RecoveryKeySalt != "" {
		recKey, recSalt, recPrefsKey, recPrefsSalt, decErr := decodeOptionalRecoveryKeys(
			req.RecoveryWrappedPrivateKey, req.RecoveryKeySalt, req.RecoveryWrappedPrefsKey, req.RecoveryPrefsKeySalt)
		if decErr != "" {
			h.redis.Del(ctx, recoveryUsedKey)
			c.JSON(http.StatusBadRequest, gin.H{"error": decErr})
			return
		}
		ops = append(ops, recoveryTxOp{
			`INSERT INTO user_recovery_keys (user_id, recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (user_id) DO UPDATE SET
			     recovery_wrapped_private_key = EXCLUDED.recovery_wrapped_private_key,
			     recovery_key_salt = EXCLUDED.recovery_key_salt,
			     recovery_wrapped_prefs_key = EXCLUDED.recovery_wrapped_prefs_key,
			     recovery_prefs_key_salt = EXCLUDED.recovery_prefs_key_salt,
			     updated_at = NOW()`,
			[]interface{}{claims.UserID, recKey, recSalt, recPrefsKey, recPrefsSalt}, "Failed to upsert recovery key"})
	}

	ops = append(ops, recoveryTxOp{
		`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`,
		[]interface{}{claims.UserID}, "Failed to revoke refresh tokens"})

	if err := h.execRecoveryTx(ctx, c, ops, recoveryUsedKey, errFailedResetPwd); err != nil {
		return
	}

	h.hub.DisconnectUser(uuid.MustParse(claims.UserID))
	middleware.ClearAuthFailures(ctx, h.redis, c.ClientIP())
	h.log.Info("Password reset via recovery", "user_id", claims.UserID)
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

	ops := []recoveryTxOp{
		{`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
			[]interface{}{passwordHash, claims.UserID}, "Failed to update password"},
		{`UPDATE user_keys SET wrapped_private_key = $1, key_derivation_salt = $2, key_derivation_alg = $3,
		         key_version = key_version + 1, updated_at = NOW() WHERE user_id = $4`,
			[]interface{}{wrappedKey, kdSalt, req.KeyDerivationAlg, claims.UserID}, "Failed to update user keys"},
		{`UPDATE public_keys SET public_key = $1, key_version = key_version + 1, created_at = NOW() WHERE user_id = $2`,
			[]interface{}{publicKey, claims.UserID}, "Failed to update public key"},
		{`DELETE FROM user_recovery_keys WHERE user_id = $1`,
			[]interface{}{claims.UserID}, "Failed to delete recovery keys"},
		{`DELETE FROM channel_keys WHERE user_id = $1`,
			[]interface{}{claims.UserID}, "Failed to delete channel keys"},
		{`DELETE FROM dm_channel_keys WHERE user_id = $1`,
			[]interface{}{claims.UserID}, "Failed to delete DM channel keys"},
		{`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`,
			[]interface{}{claims.UserID}, "Failed to revoke refresh tokens"},
	}

	if err := h.execRecoveryTx(ctx, c, ops, recoveryUsedKey, errFailedResetAccount); err != nil {
		return
	}

	h.hub.DisconnectUser(uuid.MustParse(claims.UserID))
	h.log.Info("Account reset via recovery (data loss acknowledged)", "user_id", claims.UserID)
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
