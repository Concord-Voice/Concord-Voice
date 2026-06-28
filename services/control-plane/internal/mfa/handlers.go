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

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/email"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
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

	// Error messages
	errMsgPasswordRequired       = "Password is required"
	errMsgIncorrectPassword      = "Incorrect password"
	errMsgCodeRequired           = "Code is required"
	errMsgFailedBackupCodes      = "Failed to generate backup codes"
	errMsgFailedStartReg         = "Failed to start registration"
	errMsgInvalidSessionData     = "Invalid session data"
	errMsgFailedListDevices      = "Failed to list trusted devices"
	errMsgFailedListRecoveryReqs = "Failed to list recovery requests"
	errMsgFailedLoadCircle       = "Failed to load recovery circle"
	errMsgFailedConfigCircle     = "Failed to configure recovery circle"
	errMsgFailedListSocialReqs   = "Failed to list social recovery requests"
	errMsgFailedSubmitResponse   = "Failed to submit response"
)

// LoginCompleter completes the login flow after MFA verification.
// Implemented by the auth handler to issue tokens and create sessions.
type LoginCompleter interface {
	CompleteLogin(c *gin.Context, userID string, rememberMe bool)
}

// Handler implements MFA API endpoints and the Verifier interface.
type Handler struct {
	db             *sql.DB
	redis          *redis.Client
	log            *logger.Logger
	encKey         []byte // 32-byte AES key for TOTP secret encryption
	jwtSecret      string
	webauthn       *WebAuthnService
	loginCompleter LoginCompleter
	emailSvc       *email.Service
	environment    string // "development", "staging", "production"
}

// Ensure Handler implements Verifier at compile time.
var _ Verifier = (*Handler)(nil)

// NewHandler creates a new MFA handler.
func NewHandler(db *sql.DB, redisClient *redis.Client, log *logger.Logger, encKey []byte, jwtSecret string, webauthnSvc *WebAuthnService, environment string) *Handler {
	return &Handler{
		db:          db,
		redis:       redisClient,
		log:         log,
		encKey:      encKey,
		jwtSecret:   jwtSecret,
		webauthn:    webauthnSvc,
		environment: environment,
	}
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

// IsEnabled returns true if the user has any active MFA method.
func (h *Handler) IsEnabled(ctx context.Context, userID string) bool {
	var enabled bool
	err := h.db.QueryRowContext(ctx, `SELECT mfa_enabled FROM users WHERE id = $1`, userID).Scan(&enabled)
	return err == nil && enabled
}

// VerifyCode checks a TOTP code or backup code against the user's stored MFA secrets.
func (h *Handler) VerifyCode(ctx context.Context, userID string, code string) (bool, error) {
	// Check for WebAuthn inline verification token first (from WebAuthnVerifyInlineFinish)
	if len(code) > 20 {
		tokenKey := fmt.Sprintf("mfa_inline_token:%s:%s", userID, code)
		if h.redis.Exists(ctx, tokenKey).Val() > 0 {
			h.redis.Del(ctx, tokenKey) // Single-use: delete immediately
			return true, nil
		}
	}

	// Try TOTP
	var secretEnc, secretNonce []byte
	var totpEnabled, totpConfirmed bool
	err := h.db.QueryRowContext(ctx,
		`SELECT totp_secret_enc, totp_secret_nonce, enabled, confirmed FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(&secretEnc, &secretNonce, &totpEnabled, &totpConfirmed)

	if err == nil && totpEnabled && totpConfirmed {
		secret, decErr := DecryptSecret(secretEnc, secretNonce, h.encKey)
		if decErr != nil {
			h.log.Error("TOTP secret decryption failed — likely encryption key mismatch", "user_id", userID, "error", decErr)
			return false, fmt.Errorf("TOTP secret decryption failed: %w", decErr)
		}
		if ValidateCode(string(secret), code) {
			return true, nil
		}

		// Try as backup code
		var hashes []string
		var used []bool
		_ = h.db.QueryRowContext(ctx,
			`SELECT backup_codes_hash, backup_codes_used FROM user_mfa_totp WHERE user_id = $1`,
			userID,
		).Scan(pq.Array(&hashes), pq.Array(&used))

		if idx, matched := VerifyBackupCode(code, hashes, used); matched {
			// Mark backup code as used
			used[idx] = true
			_, _ = h.db.ExecContext(ctx,
				`UPDATE user_mfa_totp SET backup_codes_used = $1, updated_at = NOW() WHERE user_id = $2`,
				pq.Array(used), userID,
			)
			return true, nil
		}
	}

	return false, nil
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
func (h *Handler) GenerateLoginChallenge(ctx context.Context, userID string, rememberMe bool) (string, string, error) {
	token, jti, err := GenerateChallengeToken(userID, PurposeLogin, h.jwtSecret)
	if err != nil {
		return "", "", err
	}

	// Store remember_me in Redis so the MFA verify handler can complete login with it
	rememberVal := "0"
	if rememberMe {
		rememberVal = "1"
	}
	key := fmt.Sprintf(redisKeyMFAChallengeRememberMe, jti)
	h.redis.Set(ctx, key, rememberVal, challengeTTL)

	return token, jti, nil
}

// GenerateUpgradeChallenge creates a challenge token for pre-MFA session upgrades.
// On successful MFA verification, fresh tokens are issued (same as login).
func (h *Handler) GenerateUpgradeChallenge(ctx context.Context, userID string, rememberMe bool) (string, string, error) {
	token, jti, err := GenerateChallengeToken(userID, PurposeMFAUpgrade, h.jwtSecret)
	if err != nil {
		return "", "", err
	}

	rememberVal := "0"
	if rememberMe {
		rememberVal = "1"
	}
	key := fmt.Sprintf(redisKeyMFAChallengeRememberMe, jti)
	h.redis.Set(ctx, key, rememberVal, challengeTTL)

	return token, jti, nil
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

// updateUserMFAFlags recalculates and updates the denormalized mfa_enabled and mfa_methods on users.
func (h *Handler) updateUserMFAFlags(ctx context.Context, userID string) error {
	methods := make([]string, 0) // must be non-nil for pq.Array to produce '{}' not NULL

	// Check TOTP
	var totpActive bool
	err := h.db.QueryRowContext(ctx,
		`SELECT enabled AND confirmed FROM user_mfa_totp WHERE user_id = $1`, userID,
	).Scan(&totpActive)
	if err == nil && totpActive {
		methods = append(methods, "totp")
	}

	// Check WebAuthn
	var webauthnCount int
	_ = h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_mfa_webauthn WHERE user_id = $1`, userID,
	).Scan(&webauthnCount)
	if webauthnCount > 0 {
		methods = append(methods, "webauthn")
	}

	// Check Email/SMS (dev stub — tracked via Redis flag)
	for _, m := range []string{"email", "sms"} {
		if h.redis.Exists(ctx, fmt.Sprintf(redisKeyEmailSmsEnabled, userID, m)).Val() > 0 {
			methods = append(methods, m)
		}
	}

	enabled := len(methods) > 0

	if enabled {
		// Set mfa_enabled_at only on the first activation (NULL → NOW).
		// Pre-existing sessions created before this timestamp will be
		// challenged for MFA on their next refresh.
		_, err = h.db.ExecContext(ctx, `
			UPDATE users
			SET mfa_enabled = TRUE,
			    mfa_methods = $1,
			    mfa_enabled_at = COALESCE(mfa_enabled_at, NOW())
			WHERE id = $2
		`, pq.Array(methods), userID)
	} else {
		// MFA fully disabled — clear the timestamp so re-enabling later
		// sets a fresh one.
		_, err = h.db.ExecContext(ctx, `
			UPDATE users
			SET mfa_enabled = FALSE,
			    mfa_methods = $1,
			    mfa_enabled_at = NULL
			WHERE id = $2
		`, pq.Array(methods), userID)
	}
	return err
}

// requirePasswordAndMFA verifies the user's password and, if MFA is already active,
// also verifies an MFA code. Returns true if verification passed, false if a
// response was already written to c.
func (h *Handler) requirePasswordAndMFA(c *gin.Context, userID, password, mfaCode string) bool {
	ctx := c.Request.Context()

	match, err := h.verifyUserPassword(ctx, userID, password)
	if err != nil {
		h.log.Error("Password verification failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify password"})
		return false
	}
	if !match {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgIncorrectPassword})
		return false
	}

	// If MFA is already active, require an MFA code too
	if h.IsEnabled(ctx, userID) {
		if mfaCode == "" {
			c.JSON(http.StatusForbidden, gin.H{"error": "MFA code is required", "mfa_required": true})
			return false
		}
		ok, verifyErr := h.VerifyCode(ctx, userID, mfaCode)
		if verifyErr != nil || !ok {
			c.JSON(http.StatusForbidden, gin.H{"error": "Invalid MFA code"})
			return false
		}
	}

	return true
}

// ── TOTP Endpoints ───────────────────────────────────────────────────────────

// GetStatus returns the user's MFA status across all methods.
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
	var totpEnabled, totpConfirmed bool
	var backupUsed []bool
	var backupHashes []string
	err := h.db.QueryRowContext(ctx, `SELECT enabled, confirmed, backup_codes_hash, backup_codes_used FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(&totpEnabled, &totpConfirmed, pq.Array(&backupHashes), pq.Array(&backupUsed))
	if err == nil {
		result["totp_enabled"] = totpEnabled
		result["totp_confirmed"] = totpConfirmed
		remaining := 0
		for i, used := range backupUsed {
			if !used && i < len(backupHashes) {
				remaining++
			}
		}
		result["backup_codes_remaining"] = remaining
	}

	// WebAuthn credential count
	var webauthnCount int
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_mfa_webauthn WHERE user_id = $1`, userID).Scan(&webauthnCount)
	result["webauthn_credentials"] = webauthnCount

	// Overall — read denormalized flags, then self-heal if stale
	var methods, recoveryOnly []string
	var mfaEnabled, recoveryHardened bool
	var backupEmail sql.NullString
	_ = h.db.QueryRowContext(ctx, `SELECT mfa_enabled, mfa_methods, recovery_only_methods, recovery_hardened, backup_email FROM users WHERE id = $1`, userID).Scan(&mfaEnabled, pq.Array(&methods), pq.Array(&recoveryOnly), &recoveryHardened, &backupEmail)

	// Self-heal: if denormalized flags disagree with actual table data, resync
	actualTOTP := totpEnabled && totpConfirmed
	actualWebAuthn := webauthnCount > 0
	denormTOTP := containsStr(methods, "totp")
	denormWebAuthn := containsStr(methods, "webauthn")
	if actualTOTP != denormTOTP || actualWebAuthn != denormWebAuthn {
		h.log.Warn("MFA flags out of sync, resyncing", "user_id", userID,
			"actual_totp", actualTOTP, "denorm_totp", denormTOTP,
			"actual_webauthn", actualWebAuthn, "denorm_webauthn", denormWebAuthn)
		if syncErr := h.updateUserMFAFlags(ctx, userID); syncErr != nil {
			h.log.Error("Failed to resync MFA flags", "user_id", userID, "error", syncErr)
		} else {
			// Re-read after sync
			_ = h.db.QueryRowContext(ctx, `SELECT mfa_enabled, mfa_methods FROM users WHERE id = $1`, userID).Scan(&mfaEnabled, pq.Array(&methods))
		}
	}

	result["mfa_enabled"] = mfaEnabled
	result["methods"] = methods
	result["recovery_only_methods"] = recoveryOnly
	result["recovery_hardened"] = recoveryHardened
	if backupEmail.Valid {
		result["backup_email"] = backupEmail.String
	} else {
		result["backup_email"] = ""
	}

	// Email/SMS MFA status (stored in Redis)
	emailEnabled := h.redis.Exists(ctx, fmt.Sprintf(redisKeyEmailSmsEnabledEmail, userID)).Val() > 0
	smsEnabled := h.redis.Exists(ctx, fmt.Sprintf("mfa_emailsms_enabled:%s:sms", userID)).Val() > 0
	result["email_mfa_enabled"] = emailEnabled
	result["sms_mfa_enabled"] = smsEnabled

	c.JSON(http.StatusOK, result)
}

// TOTPSetup initiates TOTP enrollment. Requires password confirmation (and MFA if already active).
func (h *Handler) TOTPSetup(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode  string `json:"mfa_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgPasswordRequired})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
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
	ciphertext, nonce, err := EncryptSecret([]byte(key.Secret()), h.encKey)
	if err != nil {
		h.log.Error("Failed to encrypt TOTP secret", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to secure MFA secret"})
		return
	}

	// Upsert (replace pending setup or insert new)

	_, err = h.db.ExecContext(ctx, `
		INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, enabled, confirmed)
		VALUES ($1, $2, $3, FALSE, FALSE)
		ON CONFLICT (user_id) DO UPDATE SET
			totp_secret_enc = EXCLUDED.totp_secret_enc,
			totp_secret_nonce = EXCLUDED.totp_secret_nonce,
			enabled = FALSE,
			confirmed = FALSE,
			verified_at = NULL,
			confirmed_at = NULL,
			backup_codes_hash = '{}',
			backup_codes_used = '{}',
			updated_at = NOW()
	`, userID, ciphertext, nonce)
	if err != nil {
		h.log.Error("Failed to store TOTP secret", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store MFA secret"})
		return
	}

	// Sync user flags — if re-enrolling, the upsert reset enabled/confirmed
	// to FALSE so mfa_enabled must be cleared until the new setup completes.
	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to sync MFA flags after TOTP re-enrollment", "error", err)
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

	// Rate limit: check for MFA setup lockout
	lockoutKey := fmt.Sprintf("mfa_setup_lockout:%s", userID)
	if h.redis.Exists(ctx, lockoutKey).Val() > 0 {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many failed attempts. Try again later."})
		return
	}

	// Fetch the pending TOTP secret
	var secretEnc, secretNonce []byte
	var enabled bool

	err := h.db.QueryRowContext(ctx,
		`SELECT totp_secret_enc, totp_secret_nonce, enabled FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(&secretEnc, &secretNonce, &enabled)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No TOTP setup in progress. Call /mfa/totp/setup first."})
		return
	}
	if enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "TOTP is already verified"})
		return
	}

	// Decrypt and validate
	secret, err := DecryptSecret(secretEnc, secretNonce, h.encKey)
	if err != nil {
		h.log.Error("Failed to decrypt TOTP secret", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify code"})
		return
	}

	if !ValidateCode(string(secret), req.Code) {
		// Track failed attempts
		attemptsKey := fmt.Sprintf("mfa_setup_attempts:%s", userID)
		attempts := h.redis.Incr(ctx, attemptsKey).Val()
		h.redis.Expire(ctx, attemptsKey, 5*time.Minute)
		if attempts >= 5 {
			h.redis.Set(ctx, lockoutKey, "1", 15*time.Minute)
			h.redis.Del(ctx, attemptsKey)
		}
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
	h.redis.Del(ctx, fmt.Sprintf("mfa_setup_attempts:%s", userID))

	c.JSON(http.StatusOK, gin.H{
		"backup_codes": codes,
		"message":      "TOTP verified. Save your backup codes, then call /mfa/totp/confirm-setup to activate MFA.",
	})
}

// TOTPConfirmSetup activates MFA after the user confirms they saved their backup codes.
func (h *Handler) TOTPConfirmSetup(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	// Verify TOTP is enabled (code verified) but not yet confirmed
	var enabled, confirmed bool

	err := h.db.QueryRowContext(ctx,
		`SELECT enabled, confirmed FROM user_mfa_totp WHERE user_id = $1`, userID,
	).Scan(&enabled, &confirmed)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No TOTP setup in progress"})
		return
	}
	if !enabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "TOTP code not yet verified. Complete verify-setup first."})
		return
	}
	if confirmed {
		c.JSON(http.StatusConflict, gin.H{"error": "TOTP MFA is already active"})
		return
	}

	// Activate MFA

	_, err = h.db.ExecContext(ctx, `
		UPDATE user_mfa_totp SET confirmed = TRUE, confirmed_at = NOW(), updated_at = NOW() WHERE user_id = $1
	`, userID)
	if err != nil {
		h.log.Error("Failed to confirm TOTP setup", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to activate MFA"})
		return
	}

	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to update user MFA flags", "error", err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "MFA is now active"})
}

// TOTPDisable disables TOTP MFA. Requires password + a valid MFA code.
func (h *Handler) TOTPDisable(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req struct {
		Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		Code     string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password and MFA code are required"})
		return
	}

	// Check if TOTP is actually enrolled before proceeding
	var totpExists bool
	err := h.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_mfa_totp WHERE user_id = $1)`, userID).Scan(&totpExists)
	if err != nil || !totpExists {
		// Row is gone but mfa_methods may still list 'totp' — clean up the denormalized flags
		if syncErr := h.updateUserMFAFlags(ctx, userID); syncErr != nil {
			h.log.Error("Failed to sync MFA flags after missing TOTP row", "error", syncErr)
		}
		c.JSON(http.StatusOK, gin.H{"message": "TOTP MFA has been disabled"})
		return
	}

	// Verify password
	match, err := h.verifyUserPassword(ctx, userID, req.Password)
	if err != nil || !match {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgIncorrectPassword})
		return
	}

	// Verify MFA code
	valid, err := h.VerifyCode(ctx, userID, req.Code)
	if err != nil {
		h.log.Error("MFA code verification error during TOTP disable", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "MFA verification error. The server may have been restarted with a different encryption key."})
		return
	}
	if !valid {
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid MFA code. Make sure the code hasn't expired."})
		return
	}

	// Delete TOTP enrollment
	_, _ = h.db.ExecContext(ctx, `DELETE FROM user_mfa_totp WHERE user_id = $1`, userID)

	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to update user MFA flags after TOTP disable", "error", err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "TOTP MFA has been disabled"})
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
	var totpEnabled, totpConfirmed bool
	err = h.db.QueryRowContext(ctx,
		`SELECT totp_secret_enc, totp_secret_nonce, enabled, confirmed FROM user_mfa_totp WHERE user_id = $1`,
		userID,
	).Scan(&secretEnc, &secretNonce, &totpEnabled, &totpConfirmed)
	if err != nil || !totpEnabled || !totpConfirmed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "TOTP is not enabled"})
		return
	}

	secret, decErr := DecryptSecret(secretEnc, secretNonce, h.encKey)
	if decErr != nil || !ValidateCode(string(secret), req.Code) {
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
		Password       string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode        string `json:"mfa_code"`
		CredentialName string `json:"credential_name"`
		CredentialType string `json:"credential_type"` // "hardware" or "platform"
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgPasswordRequired})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
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
			UserVerification:        protocol.VerificationPreferred,
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

	c.JSON(http.StatusOK, creation)
}

// WebAuthnRegisterFinish completes WebAuthn credential registration.
func (h *Handler) WebAuthnRegisterFinish(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	// Retrieve session data from Redis
	metaJSON, err := h.redis.Get(ctx, fmt.Sprintf(redisKeyWebAuthnReg, userID)).Bytes()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No registration in progress or session expired"})
		return
	}
	h.redis.Del(ctx, fmt.Sprintf(redisKeyWebAuthnReg, userID))

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

	// Store credential in DB
	transports := make([]string, 0, len(credential.Transport))
	for _, t := range credential.Transport {
		transports = append(transports, string(t))
	}

	_, err = h.db.ExecContext(ctx, `
		INSERT INTO user_mfa_webauthn (user_id, credential_id, public_key, aaguid, sign_count, credential_name, credential_type, transports)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, userID, credential.ID, credential.PublicKey, credential.Authenticator.AAGUID,
		credential.Authenticator.SignCount, meta.CredentialName, meta.CredentialType, pq.Array(transports))
	if err != nil {
		h.log.Error("Failed to store WebAuthn credential", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store credential"})
		return
	}

	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to update user MFA flags after WebAuthn register", "error", err)
	}

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

	match, err := h.verifyUserPassword(ctx, userID, req.Password)
	if err != nil || !match {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgIncorrectPassword})
		return
	}

	result, err := h.db.ExecContext(ctx,
		`DELETE FROM user_mfa_webauthn WHERE id = $1 AND user_id = $2`, credentialID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete credential"})
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Credential not found"})
		return
	}

	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to update user MFA flags after WebAuthn delete", "error", err)
	}

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
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	claims, purpose := h.parseChallengeToken(req.ChallengeToken)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired MFA challenge token"})
		return
	}

	ctx := c.Request.Context()

	// Check single-use: ensure this JTI hasn't been consumed
	usedKey := fmt.Sprintf("mfa_challenge_used:%s", claims.ID)
	if h.redis.Exists(ctx, usedKey).Val() > 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "MFA challenge already used"})
		return
	}

	// Rate limit per user
	attemptsKey := fmt.Sprintf("mfa_verify_attempts:%s", claims.UserID)
	lockoutKey := fmt.Sprintf("mfa_verify_lockout:%s", claims.UserID)
	if h.redis.Exists(ctx, lockoutKey).Val() > 0 {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many failed attempts. Try again later."})
		return
	}

	verified, responded := h.verifyByMethod(ctx, c, req, claims)
	if responded {
		return // Early return already sent a response (e.g. bad request)
	}

	if !verified {
		h.recordVerifyFailure(ctx, attemptsKey, lockoutKey)
		middleware.RecordAuthFailure(ctx, h.redis, c.ClientIP(), middleware.DefaultAuthBanConfig())
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid MFA code"})
		return
	}

	// Mark challenge as used (single-use)
	h.redis.Set(ctx, usedKey, "1", 10*time.Minute)
	h.redis.Del(ctx, attemptsKey)
	middleware.ClearAuthFailures(ctx, h.redis, c.ClientIP())

	h.completeVerifyPurpose(ctx, c, claims, purpose)
}

// parseChallengeToken tries all valid purposes and returns the claims and matched purpose.
func (h *Handler) parseChallengeToken(tokenStr string) (*ChallengeClaims, ChallengePurpose) {
	for _, p := range []ChallengePurpose{PurposeLogin, PurposeSuspiciousRefresh, PurposeMFAUpgrade} {
		if parsed, err := ValidateChallengeToken(tokenStr, h.jwtSecret, p); err == nil {
			return parsed, p
		}
	}
	return nil, ""
}

// verifyByMethod dispatches verification to the appropriate method handler.
// Returns (verified, responded) — responded is true if an HTTP response was already written.
func (h *Handler) verifyByMethod(ctx context.Context, c *gin.Context, req verifyRequest, claims *ChallengeClaims) (bool, bool) {
	switch req.Method {
	case "totp", "backup_code":
		return h.verifyTOTPOrBackup(ctx, c, req.Code, claims.UserID)
	case "webauthn":
		return h.verifyWebAuthnChallenge(ctx, c, req.Assertion, claims)
	case "email":
		return h.verifyEmailCode(ctx, c, req.Code, claims)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid method. Use 'totp', 'backup_code', 'webauthn', or 'email'"})
		return false, true
	}
}

// verifyTOTPOrBackup verifies a TOTP or backup code.
func (h *Handler) verifyTOTPOrBackup(ctx context.Context, c *gin.Context, code, userID string) (bool, bool) {
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgCodeRequired})
		return false, true
	}
	valid, err := h.VerifyCode(ctx, userID, code)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Verification failed"})
		return false, true
	}
	return valid, false
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
func (h *Handler) completeVerifyPurpose(ctx context.Context, c *gin.Context, claims *ChallengeClaims, purpose ChallengePurpose) {
	switch purpose {
	case PurposeLogin:
		if h.loginCompleter == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Login completion not configured"})
			return
		}
		rememberKey := fmt.Sprintf(redisKeyMFAChallengeRememberMe, claims.ID)
		rememberMe := h.redis.Get(ctx, rememberKey).Val() == "1"
		h.redis.Del(ctx, rememberKey)
		h.loginCompleter.CompleteLogin(c, claims.UserID, rememberMe)

	case PurposeMFAUpgrade:
		bypassKey := fmt.Sprintf("mfa_upgrade_bypass:%s", claims.UserID)
		h.redis.Set(ctx, bypassKey, "1", 30*time.Second)
		c.JSON(http.StatusOK, gin.H{"verified": true, "purpose": string(purpose), "user_id": claims.UserID})

	default:
		c.JSON(http.StatusOK, gin.H{"verified": true, "purpose": string(purpose), "user_id": claims.UserID})
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
		Methods  []string `json:"methods"`                     // e.g. ["email", "sms"] or [] to clear
		Password string   `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode  string   `json:"mfa_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgPasswordRequired})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
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
		Enabled  bool   `json:"enabled"`
		Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode  string `json:"mfa_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgPasswordRequired})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
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
		Password string   `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode  string   `json:"mfa_code"`
		Methods  []string `json:"methods" binding:"required"` // ["email"], ["sms"], or ["email", "sms"]
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password and methods are required"})
		return
	}

	if errMsg, status := h.validateEmailSmsMethods(req.Methods); errMsg != "" {
		c.JSON(status, gin.H{"error": errMsg})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
		return
	}

	if !h.IsEnabled(ctx, userID) {
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
		if err != nil {
			return nil, fmt.Sprintf("No pending %s code. Request a new one.", method), http.StatusBadRequest
		}

		if code != stored {
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

	var recoveryHardened bool
	_ = h.db.QueryRowContext(ctx,
		`SELECT recovery_hardened FROM users WHERE id = $1`, userID,
	).Scan(&recoveryHardened)

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

	for _, method := range verified {
		h.redis.Set(ctx, fmt.Sprintf(redisKeyEmailSmsEnabled, userID, method), "1", 0)
		h.redis.Del(ctx, fmt.Sprintf(redisKeyEmailSmsSetup, userID, method))
	}

	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to update MFA flags after email/sms enable", "error", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":  "MFA methods activated",
		"verified": verified,
	})
}

// EmailSmsDisable removes email and/or SMS MFA methods.
func (h *Handler) EmailSmsDisable(c *gin.Context) {

	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	// Remove enabled flags
	for _, m := range []string{"email", "sms"} {
		h.redis.Del(ctx, fmt.Sprintf(redisKeyEmailSmsEnabled, userID, m))
		h.redis.Del(ctx, fmt.Sprintf(redisKeyEmailSmsSetup, userID, m))
	}

	// Also clear from recovery-only if present
	_, _ = h.db.ExecContext(ctx,
		`UPDATE users SET recovery_only_methods = array_remove(array_remove(recovery_only_methods, 'email'), 'sms') WHERE id = $1`,
		userID,
	)

	if err := h.updateUserMFAFlags(ctx, userID); err != nil {
		h.log.Error("Failed to update MFA flags after email/sms disable", "error", err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "Email/SMS MFA methods disabled"})
}

// EmailSmsStatus returns whether email/sms methods are enabled.
func (h *Handler) EmailSmsStatus(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	emailEnabled := h.redis.Exists(ctx, fmt.Sprintf(redisKeyEmailSmsEnabledEmail, userID)).Val() > 0
	smsEnabled := h.redis.Exists(ctx, fmt.Sprintf("mfa_emailsms_enabled:%s:sms", userID)).Val() > 0

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
	_ = h.db.QueryRowContext(c.Request.Context(),
		`SELECT backup_email FROM users WHERE id = $1`, userID,
	).Scan(&backupEmail)
	email := ""
	if backupEmail.Valid {
		email = backupEmail.String
	}
	c.JSON(http.StatusOK, gin.H{"backup_email": email})
}

// SetBackupEmail sets or clears the user's backup email for recovery.
func (h *Handler) SetBackupEmail(c *gin.Context) {
	userID := c.GetString("user_id")
	var req struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	// Basic email validation (or allow empty to clear)
	if req.Email != "" && !isValidEmail(req.Email) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid email address"})
		return
	}

	var val interface{}
	if req.Email == "" {
		val = nil
	} else {
		val = req.Email
	}

	_, err := h.db.ExecContext(c.Request.Context(),
		`UPDATE users SET backup_email = $1 WHERE id = $2`, val, userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update backup email"})
		return
	}

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
	return GenerateRecoveryToken(userID, h.jwtSecret)
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

// StoreRecoveryKey stores or updates the user's recovery-wrapped private key.
func (h *Handler) StoreRecoveryKey(c *gin.Context) {
	userID := c.GetString("user_id")

	var req struct {
		RecoveryWrappedPrivateKey string `json:"recovery_wrapped_private_key" binding:"required"`
		RecoveryKeySalt           string `json:"recovery_key_salt" binding:"required"`
		RecoveryWrappedPrefsKey   string `json:"recovery_wrapped_prefs_key"`
		RecoveryPrefsKeySalt      string `json:"recovery_prefs_key_salt"`
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

	// UPSERT into user_recovery_keys
	_, err = h.db.ExecContext(c.Request.Context(), `
		INSERT INTO user_recovery_keys (user_id, recovery_wrapped_private_key, recovery_key_salt, recovery_wrapped_prefs_key, recovery_prefs_key_salt)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO UPDATE SET
			recovery_wrapped_private_key = EXCLUDED.recovery_wrapped_private_key,
			recovery_key_salt = EXCLUDED.recovery_key_salt,
			recovery_wrapped_prefs_key = EXCLUDED.recovery_wrapped_prefs_key,
			recovery_prefs_key_salt = EXCLUDED.recovery_prefs_key_salt,
			updated_at = NOW()
	`, userID, wrappedKey, keySalt, wrappedPrefsKey, prefsKeySalt)
	if err != nil {
		h.log.Error("Failed to store recovery key", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store recovery key"})
		return
	}

	h.log.Info("Recovery key stored", "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"message": "Recovery key stored"})
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

// DeleteRecoveryKey removes the user's recovery key after verifying password and MFA.
func (h *Handler) DeleteRecoveryKey(c *gin.Context) {
	userID := c.GetString("user_id")

	var req struct {
		Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode  string `json:"mfa_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgPasswordRequired})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
		return
	}

	_, err := h.db.ExecContext(c.Request.Context(),
		`DELETE FROM user_recovery_keys WHERE user_id = $1`, userID,
	)
	if err != nil {
		h.log.Error("Failed to delete recovery key", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove recovery key"})
		return
	}

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
		Password   string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode    string `json:"mfa_code"`
		DeviceName string `json:"device_name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password and device_name are required"})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
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
		Password string `json:"password" binding:"required"` //nolint:gosec // request field, not a secret
		MFACode  string `json:"mfa_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgPasswordRequired})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
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
		Password     string             `json:"password" binding:"required"` //nolint:gosec // G117: request binding struct, not a credential
		MFACode      string             `json:"mfa_code"`
		ThresholdK   int                `json:"threshold_k" binding:"required"`
		TotalSharesN int                `json:"total_shares_n" binding:"required"`
		Shares       []CircleShareEntry `json:"shares" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
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
	c.JSON(http.StatusOK, gin.H{"message": "Recovery circle configured", "share_version": shareVersion})
}

// DeleteRecoveryCircle deletes the user's social recovery circle and all shares.
func (h *Handler) DeleteRecoveryCircle(c *gin.Context) {
	userID := c.GetString("user_id")

	var req struct {
		Password string `json:"password" binding:"required"` //nolint:gosec // G117: request binding struct, not a credential
		MFACode  string `json:"mfa_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password is required"})
		return
	}

	// Require password + MFA verification
	if !h.requirePasswordAndMFA(c, userID, req.Password, req.MFACode) {
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
