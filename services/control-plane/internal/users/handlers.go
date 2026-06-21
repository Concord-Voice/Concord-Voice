// Package users provides handlers for user profile management and settings.
package users

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/entitlements"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/media"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

const (
	errMsgUserNotFound         = "User not found"
	errMsgUnauthorized         = "Unauthorized"
	errMsgFailedUpdateProfile  = "Failed to update profile"
	errMsgFailedChangePassword = "Failed to change password"
	dataImagePrefix            = "data:image/"
)

// MFAVerifier checks MFA status and verifies codes for sensitive operations.
type MFAVerifier interface {
	IsEnabled(ctx context.Context, userID string) bool
	VerifyCode(ctx context.Context, userID string, code string) (bool, error)
	GetEnabledMethods(ctx context.Context, userID string) ([]string, error)
}

// Handler handles user-related requests including profile management and settings.
type Handler struct {
	db          *sql.DB
	log         *logger.Logger
	hub         *websocket.Hub
	mfaVerifier MFAVerifier
	tiers       entitlements.TierResolver // resolves the acting user's subscription tier (#1298)
	store       media.ObjectDeleter       // nil when object storage is not configured
}

// NewHandler creates a new user handler.
func NewHandler(db *sql.DB, log *logger.Logger, hub *websocket.Hub, mfaVerifier MFAVerifier, tiers entitlements.TierResolver) *Handler {
	return &Handler{
		db:          db,
		log:         log,
		hub:         hub,
		mfaVerifier: mfaVerifier,
		tiers:       tiers,
	}
}

// SetMediaStore configures optional object storage for media cleanup on profile image removal.
func (h *Handler) SetMediaStore(store media.ObjectDeleter) {
	h.store = store
}

// GetMe returns the current user's profile
func (h *Handler) GetMe(c *gin.Context) {
	// Get user ID from auth middleware
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	// Fetch user from database
	var user models.User
	err := h.db.QueryRow(
		`SELECT id, email, username, display_name, bio, avatar_url, header_image_url, color_scheme,
		        COALESCE(links, '[]'::jsonb),
		        email_verified, age_verified, created_at, updated_at,
		        username_changed_at
		 FROM users WHERE id = $1`,
		userID,
	).Scan(
		&user.ID,
		&user.Email,
		&user.Username,
		&user.DisplayName,
		&user.Bio,
		&user.AvatarURL,
		&user.HeaderImageURL,
		&user.ColorScheme,
		&user.Links,
		&user.EmailVerified,
		&user.AgeVerified,
		&user.CreatedAt,
		&user.UpdatedAt,
		&user.UsernameChangedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotFound})
			return
		}
		h.log.Error("Failed to fetch user", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch user"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user": user.PublicUser(),
	})
}

// GetPublicProfile returns another user's public profile (no email or private fields)
func (h *Handler) GetPublicProfile(c *gin.Context) {
	// Ensure the requester is authenticated
	_, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	targetUserID := c.Param("user_id")

	var user models.User
	err := h.db.QueryRow(
		`SELECT id, email, username, display_name, bio, avatar_url, header_image_url, color_scheme,
		        COALESCE(links, '[]'::jsonb),
		        email_verified, age_verified, created_at, updated_at
		 FROM users WHERE id = $1`,
		targetUserID,
	).Scan(
		&user.ID,
		&user.Email,
		&user.Username,
		&user.DisplayName,
		&user.Bio,
		&user.AvatarURL,
		&user.HeaderImageURL,
		&user.ColorScheme,
		&user.Links,
		&user.EmailVerified,
		&user.AgeVerified,
		&user.CreatedAt,
		&user.UpdatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": errMsgUserNotFound})
			return
		}
		h.log.Error("Failed to fetch user profile", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch user profile"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user": user.ProfileForOthers(),
	})
}

// GetMyKeys returns the current user's E2EE keys (for key re-wrapping during password change)
func (h *Handler) GetMyKeys(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var keys models.UserKeys
	err := h.db.QueryRow(
		`SELECT user_id, wrapped_private_key, key_derivation_salt, key_version, key_derivation_alg
		 FROM user_keys WHERE user_id = $1`,
		userID,
	).Scan(&keys.UserID, &keys.WrappedPrivateKey, &keys.KeyDerivationSalt, &keys.KeyVersion, &keys.KeyDerivationAlg)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "E2EE keys not found"})
			return
		}
		h.log.Error("Failed to fetch E2EE keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch encryption keys"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"e2ee_keys": gin.H{
			"wrapped_private_key": base64.StdEncoding.EncodeToString(keys.WrappedPrivateKey),
			"key_derivation_salt": base64.StdEncoding.EncodeToString(keys.KeyDerivationSalt),
			"key_version":         keys.KeyVersion,
			"key_derivation_alg":  keys.KeyDerivationAlg,
		},
	})
}

// errMsgFailedReplaceKeys is the generic 500 message returned for any failure
// of the key-replacement transaction (begin / apply / commit).
const errMsgFailedReplaceKeys = "Failed to replace keys"

// ReplaceKeysRequest represents a request to replace (reset) E2EE keys.
// Resetting is a destructive, acknowledged operation: it rotates the public
// key and discards all wrapped channel/DM keys (see replaceKeyMaterialTx).
// Because it is destructive, it requires step-up re-authentication
// (current_password + MFA when enabled), mirroring ChangePassword (#1293).
type ReplaceKeysRequest struct {
	WrappedPrivateKey   string `json:"wrapped_private_key" binding:"required"`
	KeyDerivationSalt   string `json:"key_derivation_salt" binding:"required"`
	KeyDerivationAlg    string `json:"key_derivation_alg"`
	PublicKey           string `json:"public_key" binding:"required"`
	CurrentPassword     string `json:"current_password" binding:"required"`
	MFACode             string `json:"mfa_code"` // Required when MFA is enabled
	AcknowledgeDataLoss bool   `json:"acknowledge_data_loss"`
}

// ReplaceMyKeys resets the user's E2EE identity to a new keypair. It atomically
// rotates user_keys + public_keys and clears stale wrapped channel/DM keys, so
// the new private key always matches the published public key (#1293). Gated on
// acknowledge_data_loss because all prior encrypted history becomes unreadable.
func (h *Handler) ReplaceMyKeys(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	req, wrappedKeyBytes, saltBytes, publicKeyBytes, ok := parseReplaceKeysRequest(c)
	if !ok {
		return
	}

	// Step-up re-authentication: this is a destructive, irreversible operation
	// (it purges all of the user's wrapped channel/DM keys and rotates the
	// public key). A valid access token + acknowledgment is not enough — require
	// the current password and MFA when enabled, mirroring ChangePassword, so a
	// stolen access token alone cannot destroy a victim's E2EE history (#1293).
	if !h.verifyResetStepUp(c, userID, req.CurrentPassword, req.MFACode) {
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		h.log.Error("Failed to begin key replacement tx", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReplaceKeys})
		return
	}
	committed := false
	defer func() {
		if !committed {
			if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
				h.log.Error("Failed to roll back key replacement tx", "error", rbErr)
			}
		}
	}()

	if err := replaceKeyMaterialTx(tx, userID, wrappedKeyBytes, saltBytes, req.KeyDerivationAlg, publicKeyBytes); err != nil {
		h.log.Error("Failed to replace E2EE key material", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReplaceKeys})
		return
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit key replacement", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReplaceKeys})
		return
	}
	committed = true

	h.log.Info("E2EE keys replaced (atomic reset)", "user_id", userID, "alg", req.KeyDerivationAlg)
	c.JSON(http.StatusOK, gin.H{"message": "Keys replaced successfully. Encrypted message history was reset."})
}

// parseReplaceKeysRequest binds and validates a ReplaceKeysRequest, writing the
// appropriate 4xx response and returning ok=false on any failure. Extracted from
// ReplaceMyKeys to keep that handler under the cognitive-complexity threshold.
func parseReplaceKeysRequest(c *gin.Context) (req ReplaceKeysRequest, wrapped, salt, publicKey []byte, ok bool) {
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "wrapped_private_key, key_derivation_salt, public_key and current_password are required"})
		return req, nil, nil, nil, false
	}

	if !req.AcknowledgeDataLoss {
		c.JSON(http.StatusBadRequest, gin.H{"error": "You must acknowledge that all encrypted message history will be permanently lost"})
		return req, nil, nil, nil, false
	}

	wrapped, err := base64.StdEncoding.DecodeString(req.WrappedPrivateKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid wrapped_private_key format"})
		return req, nil, nil, nil, false
	}
	salt, err = base64.StdEncoding.DecodeString(req.KeyDerivationSalt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid key_derivation_salt format"})
		return req, nil, nil, nil, false
	}
	publicKey, err = base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid public_key format"})
		return req, nil, nil, nil, false
	}

	if req.KeyDerivationAlg == "" {
		req.KeyDerivationAlg = "argon2id"
	}
	return req, wrapped, salt, publicKey, true
}

// verifyResetStepUp re-authenticates the caller for the destructive key reset:
// current password + MFA when enabled, reusing the same helpers as
// ChangePassword. It writes the appropriate response and returns false on any
// failure. Defeats the stolen-access-token threat on this data-loss path (#1293).
func (h *Handler) verifyResetStepUp(c *gin.Context, userID interface{}, currentPassword, mfaCode string) bool {
	// Fail closed if user_id is not a string: otherwise the comma-ok form would
	// yield "" and verifyMFAForPasswordChange would treat MFA as disabled,
	// silently skipping the step-up on this destructive path (Gitar review).
	uid, ok := userID.(string)
	if !ok {
		h.log.Error("user_id is not a string; refusing key reset", "type", fmt.Sprintf("%T", userID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedReplaceKeys})
		return false
	}
	if _, status, errMsg := h.verifyCurrentPassword(uid, currentPassword); status != 0 {
		c.JSON(status, gin.H{"error": errMsg})
		return false
	}
	if status, body := h.verifyMFAForPasswordChange(c, uid, mfaCode); status != 0 {
		c.JSON(status, body)
		return false
	}
	return true
}

// UpdateProfileRequest represents a request to update profile fields
type UpdateProfileRequest struct {
	Username       *string          `json:"username"`
	DisplayName    *string          `json:"display_name"`
	Bio            *string          `json:"bio"`
	AvatarURL      *string          `json:"avatar_url"`
	HeaderImageURL *string          `json:"header_image_url"`
	ColorScheme    *string          `json:"color_scheme"`
	Links          *json.RawMessage `json:"links"`
}

// profileUpdateBuilder accumulates SET clauses, arguments, and media keys for
// the dynamic UPDATE query in UpdateMe. Extracted to reduce cognitive complexity.
type profileUpdateBuilder struct {
	setClauses        []string
	args              []interface{}
	argIdx            int
	mediaKeysToDelete []string
	usernameChanged   bool
	// usernameChangeInterval is the tier-resolved cadence (#1298). Set in
	// validateUsername before usernameChanged is flipped true, and reused by
	// executeProfileUpdate's SQL WHERE so the precheck and the authoritative
	// double-enforcement use one identical value (no TOCTOU between reads).
	usernameChangeInterval time.Duration
	previousUsername       string
	extraResponse          map[string]interface{} // additional fields to include in error responses
}

func newProfileUpdateBuilder() *profileUpdateBuilder {
	return &profileUpdateBuilder{argIdx: 1}
}

// addClause appends a parameterized SET clause and its argument.
func (b *profileUpdateBuilder) addClause(column string, val interface{}) {
	b.setClauses = append(b.setClauses, fmt.Sprintf("%s = $%d", column, b.argIdx))
	b.args = append(b.args, val)
	b.argIdx++
}

// usernameCadenceMsg renders the tier-aware username cooldown message (#1298).
// The interval is tier-dependent (365d free / 91d premium), so the message can no
// longer hardcode "once per year".
func usernameCadenceMsg(interval time.Duration) string {
	return fmt.Sprintf("You can only change your username once every %d days", int(interval/(24*time.Hour)))
}

// validateUsername checks cooldown, uniqueness, and appends the username clause.
// Returns an HTTP status + error message on failure, or 0/"" on success.
func (h *Handler) validateUsername(b *profileUpdateBuilder, username string, userID interface{}, ent entitlements.Entitlement) (int, string) {
	trimmed := strings.TrimSpace(username)
	if err := auth.ValidateUsername(trimmed); err != nil {
		return http.StatusBadRequest, err.Error()
	}
	normalized := auth.NormalizeUsername(trimmed)

	var currentUsername string
	var usernameChangedAt time.Time
	err := h.db.QueryRow(
		`SELECT username, username_changed_at FROM users WHERE id = $1`,
		userID,
	).Scan(&currentUsername, &usernameChangedAt)
	if err == sql.ErrNoRows {
		return http.StatusNotFound, errMsgUserNotFound
	}
	if err != nil {
		h.log.Error("Failed to check username cooldown", "error", err)
		return http.StatusInternalServerError, errMsgFailedUpdateProfile
	}

	if normalized == currentUsername {
		return 0, "" // no-op
	}

	cooldownEnd := usernameChangedAt.Add(ent.UsernameChangeInterval)
	if time.Now().Before(cooldownEnd) {
		b.extraResponse = map[string]interface{}{
			"username_change_eligible_at": cooldownEnd,
		}
		return http.StatusForbidden, usernameCadenceMsg(ent.UsernameChangeInterval)
	}

	var count int
	err = h.db.QueryRow(
		`SELECT COUNT(*) FROM users WHERE username = $1 AND id != $2`,
		normalized, userID,
	).Scan(&count)
	if err != nil {
		h.log.Error("Failed to check username uniqueness", "error", err)
		return http.StatusInternalServerError, errMsgFailedUpdateProfile
	}
	if count > 0 {
		return http.StatusBadRequest, "Username is already taken"
	}

	b.addClause("username", normalized)
	b.setClauses = append(b.setClauses, "username_changed_at = NOW()")
	b.previousUsername = currentUsername
	b.usernameChangeInterval = ent.UsernameChangeInterval // SQL double-enforcement reuses this exact value
	b.usernameChanged = true
	return 0, ""
}

// validateDisplayName validates and appends the display_name clause.
func validateDisplayName(b *profileUpdateBuilder, displayName string) (int, string) {
	if len(displayName) > 100 {
		return http.StatusBadRequest, "Display name must be at most 100 characters"
	}
	trimmed := strings.TrimSpace(displayName)
	var val interface{} = trimmed
	if trimmed == "" {
		val = nil
	}
	b.addClause("display_name", val)
	return 0, ""
}

// validateBio validates and appends the bio clause.
func validateBio(b *profileUpdateBuilder, bio string) (int, string) {
	if len(bio) > 500 {
		return http.StatusBadRequest, "Bio must be at most 500 characters"
	}
	trimmed := strings.TrimSpace(bio)
	var val interface{} = trimmed
	if trimmed == "" {
		val = nil
	}
	b.addClause("bio", val)
	return 0, ""
}

// dataURLHeaderSlack budgets the "data:image/<type>;base64," prefix length.
const dataURLHeaderSlack = 64

// maxDataURLLen converts an entitlement byte limit into a data-URL string-length
// bound (base64 inflates ~4/3). Both the inline data-URL caps below and the MinIO
// upload path derive from the entitlements source, so neither is a magic number (#1298).
func maxDataURLLen(maxBytes int64) int {
	return base64.StdEncoding.EncodedLen(int(maxBytes)) + dataURLHeaderSlack
}

// Inline data-URL profile images are broadcast VERBATIM to every connected client
// (UpdateMe -> hub.BroadcastToAll embeds avatar_url/header_image_url), so the inline
// size ceiling is pinned to the FREE entitlement for ALL tiers. A premium user's larger
// allowance is honored only on the MinIO upload path (UploadAvatar/UploadBanner), which
// broadcasts a storage KEY rather than the blob. Pinning the inline cap closes the
// broadcast-amplification surface a per-tier inline cap would open (#1298 review, Gitar).
var (
	inlineDataURLAvatarMax = entitlements.For(entitlements.TierFree).MaxAvatarBytes
	inlineDataURLBannerMax = entitlements.For(entitlements.TierFree).MaxBannerBytes
)

// validateAvatarURL validates and appends the avatar_url clause.
func validateAvatarURL(b *profileUpdateBuilder, avatarURL string, userID interface{}) (int, string) {
	if avatarURL == "" {
		b.addClause("avatar_url", nil)
		b.mediaKeysToDelete = append(b.mediaKeysToDelete, fmt.Sprintf("avatars/%s", userID))
		return 0, ""
	}
	expectedAvatarURL := fmt.Sprintf("/api/v1/media/avatars/%s", userID)
	if avatarURL != expectedAvatarURL && !strings.HasPrefix(avatarURL, dataImagePrefix) {
		return http.StatusBadRequest, "Avatar must be an uploaded avatar URL for this user or an image data URL"
	}
	if strings.HasPrefix(avatarURL, dataImagePrefix) && len(avatarURL) > maxDataURLLen(inlineDataURLAvatarMax) {
		return http.StatusBadRequest, "Inline avatar image too large — upload larger images via the avatar upload endpoint"
	}
	b.addClause("avatar_url", avatarURL)
	return 0, ""
}

// validateHeaderImageURL validates and appends the header_image_url clause.
func validateHeaderImageURL(b *profileUpdateBuilder, headerImageURL string, userID interface{}) (int, string) {
	if headerImageURL == "" {
		b.addClause("header_image_url", nil)
		b.mediaKeysToDelete = append(b.mediaKeysToDelete, fmt.Sprintf("banners/%s", userID))
		return 0, ""
	}
	expectedBannerURL := fmt.Sprintf("/api/v1/media/banners/%s", userID)
	if headerImageURL != expectedBannerURL && !strings.HasPrefix(headerImageURL, dataImagePrefix) {
		return http.StatusBadRequest, "Header image must be an uploaded banner URL for this user or an image data URL"
	}
	if strings.HasPrefix(headerImageURL, dataImagePrefix) && len(headerImageURL) > maxDataURLLen(inlineDataURLBannerMax) {
		return http.StatusBadRequest, "Inline header image too large — upload larger images via the banner upload endpoint"
	}
	b.addClause("header_image_url", headerImageURL)
	return 0, ""
}

// validateLinks validates and appends the links clause.
func validateLinks(b *profileUpdateBuilder, raw json.RawMessage) (int, string) {
	var links []string
	if err := json.Unmarshal(raw, &links); err != nil {
		return http.StatusBadRequest, "Links must be an array of strings"
	}
	if len(links) > 5 {
		return http.StatusBadRequest, "Maximum of 5 links allowed"
	}
	for _, link := range links {
		link = strings.TrimSpace(link)
		if link != "" && !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
			return http.StatusBadRequest, "Links must start with http:// or https://"
		}
		if len(link) > 500 {
			return http.StatusBadRequest, "Each link must be at most 500 characters"
		}
	}
	b.addClause("links", string(raw))
	return 0, ""
}

// validateColorScheme validates and appends the color_scheme clause.
func validateColorScheme(b *profileUpdateBuilder, colorScheme string) (int, string) {
	trimmed := strings.TrimSpace(colorScheme)
	if trimmed == "" {
		b.addClause("color_scheme", nil)
		return 0, ""
	}
	if len(trimmed) > 200 {
		return http.StatusBadRequest, "color_scheme must be at most 200 characters"
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return http.StatusBadRequest, "color_scheme must be valid JSON"
	}
	b.addClause("color_scheme", trimmed)
	return 0, ""
}

// profileFieldValidator is a validation function that populates the builder
// and returns an HTTP status + error message on failure, or 0/"" on success.
type profileFieldValidator func(b *profileUpdateBuilder) (int, string)

// buildProfileFields validates all provided profile fields and populates the builder.
// Returns an HTTP status + error message on the first validation failure, or 0/"" on success.
func (h *Handler) buildProfileFields(b *profileUpdateBuilder, req *UpdateProfileRequest, userID interface{}, ent entitlements.Entitlement) (int, string) {
	validators := h.collectProfileValidators(req, userID, ent)
	for _, v := range validators {
		if status, msg := v(b); status != 0 {
			return status, msg
		}
	}
	return 0, ""
}

// collectProfileValidators returns a validator for each non-nil field in the request.
func (h *Handler) collectProfileValidators(req *UpdateProfileRequest, userID interface{}, ent entitlements.Entitlement) []profileFieldValidator {
	var validators []profileFieldValidator
	if req.Username != nil {
		val := *req.Username
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return h.validateUsername(b, val, userID, ent)
		})
	}
	if req.DisplayName != nil {
		val := *req.DisplayName
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return validateDisplayName(b, val)
		})
	}
	if req.Bio != nil {
		val := *req.Bio
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return validateBio(b, val)
		})
	}
	if req.AvatarURL != nil {
		val := *req.AvatarURL
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return validateAvatarURL(b, val, userID)
		})
	}
	if req.HeaderImageURL != nil {
		val := *req.HeaderImageURL
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return validateHeaderImageURL(b, val, userID)
		})
	}
	if req.Links != nil {
		val := *req.Links
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return validateLinks(b, val)
		})
	}
	if req.ColorScheme != nil {
		val := *req.ColorScheme
		validators = append(validators, func(b *profileUpdateBuilder) (int, string) {
			return validateColorScheme(b, val)
		})
	}
	return validators
}

// executeProfileUpdate runs the UPDATE query and scans the result into a User model.
func (h *Handler) executeProfileUpdate(b *profileUpdateBuilder, userID interface{}) (*models.User, int, string) {
	b.setClauses = append(b.setClauses, "updated_at = NOW()")

	whereClause := fmt.Sprintf("WHERE id = $%d", b.argIdx)
	if b.usernameChanged {
		// Tier double-enforcement: the cadence is parameterized (no SQL literal),
		// using $argIdx for the id and $argIdx+1 for the cadence in SECONDS appended
		// below. Seconds (not days) keeps this authoritative SQL bound provably identical
		// to the exact-Duration precheck for any interval granularity (#1298 review, Gitar).
		whereClause = fmt.Sprintf(
			"WHERE id = $%d AND username_changed_at <= NOW() - make_interval(secs => $%d)",
			b.argIdx, b.argIdx+1,
		)
	}
	query := fmt.Sprintf( // #nosec G201 -- Safe: setClauses are parameterized, whereClause uses $N placeholders // nosemgrep:concord-go-sql-sprintf
		`UPDATE users SET %s %s
		 RETURNING id, email, username, display_name, bio, avatar_url, header_image_url, color_scheme,
		           COALESCE(links, '[]'::jsonb),
		           email_verified, age_verified, created_at, updated_at,
		           username_changed_at`,
		strings.Join(b.setClauses, ", "), whereClause,
	)
	b.args = append(b.args, userID)
	if b.usernameChanged {
		// $argIdx+1: tier cadence in whole seconds, matching make_interval(secs => $N)
		// above and the exact-Duration precheck in validateUsername.
		b.args = append(b.args, int64(b.usernameChangeInterval.Seconds()))
	}

	var user models.User
	err := h.db.QueryRow(query, b.args...).Scan(
		&user.ID, &user.Email, &user.Username, &user.DisplayName,
		&user.Bio, &user.AvatarURL, &user.HeaderImageURL, &user.ColorScheme,
		&user.Links, &user.EmailVerified, &user.AgeVerified,
		&user.CreatedAt, &user.UpdatedAt, &user.UsernameChangedAt,
	)
	if err == nil {
		return &user, 0, ""
	}
	if err == sql.ErrNoRows {
		if b.usernameChanged {
			return nil, http.StatusForbidden, usernameCadenceMsg(b.usernameChangeInterval)
		}
		return nil, http.StatusNotFound, errMsgUserNotFound
	}
	if strings.Contains(err.Error(), "users_username_key") {
		return nil, http.StatusBadRequest, "Username is already taken"
	}
	h.log.Error(errMsgFailedUpdateProfile, "error", err)
	return nil, http.StatusInternalServerError, errMsgFailedUpdateProfile
}

// UpdateMe updates the current user's profile
func (h *Handler) UpdateMe(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var req UpdateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	b := newProfileUpdateBuilder()

	// Resolve the acting user's entitlement set once (server-authoritative, fail-closed
	// to free on any cache/DB error) and thread it through the field validators (#1298).
	ent := entitlements.For(h.tiers.GetTier(c.Request.Context(), c.GetString("user_id")))
	if status, msg := h.buildProfileFields(b, &req, userID, ent); status != 0 {
		resp := gin.H{"error": msg}
		for k, v := range b.extraResponse {
			resp[k] = v
		}
		c.JSON(status, resp)
		return
	}

	if len(b.setClauses) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}

	user, status, msg := h.executeProfileUpdate(b, userID)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	// Clean up orphaned media objects from storage after successful DB update
	for _, key := range b.mediaKeysToDelete {
		media.CleanupObject(c.Request.Context(), h.db, h.store, h.log, key)
	}

	// Record username change in history for audit/abuse tracking
	if b.usernameChanged {
		_, histErr := h.db.Exec(
			`INSERT INTO username_history (user_id, old_username, new_username) VALUES ($1, $2, $3)`,
			userID, b.previousUsername, user.Username,
		)
		if histErr != nil {
			h.log.Error("Failed to record username history", "error", histErr)
			// Non-fatal — the username change already succeeded
		}
	}

	h.log.Info("Profile updated", "user_id", userID)

	// Broadcast profile update to all connected clients in real time
	if h.hub != nil {
		h.hub.BroadcastToAll(websocket.OutgoingMessage{
			Type: "profile_updated",
			Data: map[string]interface{}{
				"user_id":          user.ID,
				"username":         user.Username,
				"display_name":     user.DisplayName,
				"avatar_url":       user.AvatarURL,
				"header_image_url": user.HeaderImageURL,
				"color_scheme":     user.ColorScheme,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"user": user.PublicUser(),
	})
}

// ChangePasswordRequest represents a request to change password
type ChangePasswordRequest struct {
	CurrentPassword   string `json:"current_password" binding:"required"`
	NewPassword       string `json:"new_password" binding:"required"`
	WrappedPrivateKey string `json:"wrapped_private_key" binding:"required"`
	KeyDerivationSalt string `json:"key_derivation_salt" binding:"required"`
	KeyDerivationAlg  string `json:"key_derivation_alg"` // "pbkdf2" or "argon2id"
	MFACode           string `json:"mfa_code"`           // Required when MFA is enabled
}

// verifyCurrentPassword fetches the user's password hash and checks it against the provided password.
// Returns the password hash on success, or an HTTP status + error message on failure.
func (h *Handler) verifyCurrentPassword(userID interface{}, currentPassword string) (string, int, string) {
	var passwordHash string
	err := h.db.QueryRow(
		`SELECT password_hash FROM users WHERE id = $1`,
		userID,
	).Scan(&passwordHash)
	if err == sql.ErrNoRows {
		return "", http.StatusNotFound, errMsgUserNotFound
	}
	if err != nil {
		h.log.Error("Failed to fetch password hash", "error", err)
		return "", http.StatusInternalServerError, errMsgFailedChangePassword
	}

	match, err := auth.VerifyPassword(currentPassword, passwordHash)
	if err != nil {
		h.log.Error("Failed to verify password", "error", err)
		return "", http.StatusInternalServerError, errMsgFailedChangePassword
	}
	if !match {
		return "", http.StatusUnauthorized, "Current password is incorrect"
	}
	return passwordHash, 0, ""
}

// verifyMFAForPasswordChange checks MFA if enabled. Returns an HTTP status + body on failure.
func (h *Handler) verifyMFAForPasswordChange(c *gin.Context, uid, mfaCode string) (int, gin.H) {
	ctx := c.Request.Context()
	if h.mfaVerifier == nil || !h.mfaVerifier.IsEnabled(ctx, uid) {
		return 0, nil
	}
	if mfaCode == "" {
		methods, _ := h.mfaVerifier.GetEnabledMethods(ctx, uid)
		return http.StatusForbidden, gin.H{
			"error":   "mfa_required",
			"message": "MFA verification required to change password",
			"methods": methods,
		}
	}
	valid, mfaErr := h.mfaVerifier.VerifyCode(ctx, uid, mfaCode)
	if mfaErr != nil {
		h.log.Error("MFA verification error during password change", "error", mfaErr)
		return http.StatusInternalServerError, gin.H{"error": "MFA verification failed"}
	}
	if !valid {
		return http.StatusForbidden, gin.H{"error": "Invalid MFA code"}
	}
	return 0, nil
}

// validateNewPassword checks strength and ensures it differs from the current password.
func (h *Handler) validateNewPassword(newPassword, currentHash string) (string, int, string) {
	if err := auth.ValidatePasswordStrength(newPassword); err != nil {
		return "", http.StatusBadRequest, err.Error()
	}
	sameAsOld, err := auth.VerifyPassword(newPassword, currentHash)
	if err != nil {
		h.log.Error("Failed to compare passwords", "error", err)
		return "", http.StatusInternalServerError, errMsgFailedChangePassword
	}
	if sameAsOld {
		return "", http.StatusBadRequest, "New password must be different from current password"
	}
	newHash, err := auth.HashPassword(newPassword)
	if err != nil {
		h.log.Error("Failed to hash new password", "error", err)
		return "", http.StatusInternalServerError, errMsgFailedChangePassword
	}
	return newHash, 0, ""
}

// executePasswordChange runs the transactional password + key update + token revocation.
func (h *Handler) executePasswordChange(userID interface{}, newHash string, wrappedKeyBytes, saltBytes []byte, kdAlg string) error {
	tx, err := h.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			h.log.Error("Failed to rollback transaction", "error", rbErr)
		}
	}()

	if _, err := tx.Exec(`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`, newHash, userID); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE user_keys SET wrapped_private_key = $1, key_derivation_salt = $2,
		 key_derivation_alg = $3, updated_at = NOW() WHERE user_id = $4`,
		wrappedKeyBytes, saltBytes, kdAlg, userID,
	); err != nil {
		return fmt.Errorf("update E2EE keys: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`,
		userID,
	); err != nil {
		return fmt.Errorf("revoke refresh tokens: %w", err)
	}
	return tx.Commit()
}

// ChangePassword changes the current user's password and re-wraps E2EE keys
func (h *Handler) ChangePassword(c *gin.Context) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var req ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Current password, new password, and re-wrapped keys are required"})
		return
	}

	wrappedKeyBytes, err := base64.StdEncoding.DecodeString(req.WrappedPrivateKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid wrapped_private_key format"})
		return
	}
	saltBytes, err := base64.StdEncoding.DecodeString(req.KeyDerivationSalt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid key_derivation_salt format"})
		return
	}

	passwordHash, status, msg := h.verifyCurrentPassword(userID, req.CurrentPassword)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	uid := userID.(string)
	if mfaStatus, mfaBody := h.verifyMFAForPasswordChange(c, uid, req.MFACode); mfaStatus != 0 {
		c.JSON(mfaStatus, mfaBody)
		return
	}

	newHash, status, msg := h.validateNewPassword(req.NewPassword, passwordHash)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	kdAlg := req.KeyDerivationAlg
	if kdAlg == "" {
		kdAlg = "argon2id"
	}

	if err := h.executePasswordChange(userID, newHash, wrappedKeyBytes, saltBytes, kdAlg); err != nil {
		h.log.Error(errMsgFailedChangePassword, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedChangePassword})
		return
	}

	// Force-close all WebSocket connections for this user.
	if parsedUID, parseErr := uuid.Parse(uid); parseErr == nil {
		h.hub.DisconnectUser(parsedUID)
	}

	h.log.Info("Password changed with key re-wrap", "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"message": "Password changed successfully"})
}

// SearchUsers searches for users by username or display_name.
// GET /users/search?q=...
func (h *Handler) SearchUsers(c *gin.Context) {
	userID := c.GetString("user_id")
	query := strings.TrimSpace(c.Query("q"))
	if query == "" || len(query) < 2 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Search query must be at least 2 characters"})
		return
	}

	pattern := "%" + strings.ToLower(query) + "%"
	rows, err := h.db.Query(`
		SELECT u.id, u.username, u.display_name, u.avatar_url
		FROM users u
		LEFT JOIN privacy_settings ps ON ps.user_id = u.id
		WHERE (LOWER(u.username) LIKE $1 OR LOWER(COALESCE(u.display_name, '')) LIKE $1)
		  AND u.id != $2
		  AND COALESCE(ps.searchable_by_username, FALSE) = TRUE
		LIMIT 20
	`, pattern, userID)
	if err != nil {
		h.log.Error("Failed to search users", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to search users"})
		return
	}
	defer func() { _ = rows.Close() }()

	type searchResult struct {
		ID          string  `json:"id"`
		Username    string  `json:"username"`
		DisplayName *string `json:"display_name,omitempty"`
		AvatarURL   *string `json:"avatar_url,omitempty"`
	}

	results := []searchResult{}
	for rows.Next() {
		var r searchResult
		if err := rows.Scan(&r.ID, &r.Username, &r.DisplayName, &r.AvatarURL); err != nil {
			continue
		}
		results = append(results, r)
	}

	c.JSON(http.StatusOK, gin.H{"users": results})
}

// GetPublicKey returns a user's latest public key (for E2EE key wrapping).
func (h *Handler) GetPublicKey(c *gin.Context) {
	targetUserID := c.Param("user_id")

	var publicKey []byte
	var keyVersion int
	err := h.db.QueryRow(
		`SELECT public_key, key_version FROM public_keys
		 WHERE user_id = $1 ORDER BY key_version DESC LIMIT 1`,
		targetUserID,
	).Scan(&publicKey, &keyVersion)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "Public key not found"})
			return
		}
		h.log.Error("Failed to fetch public key", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch public key"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user_id":     targetUserID,
		"public_key":  base64.StdEncoding.EncodeToString(publicKey),
		"key_version": keyVersion,
	})
}

// e2eeBlobConfig describes a single E2EE-encrypted JSON blob endpoint
// (preferences, saved GIFs, etc.). SQL queries are stored as constants per
// blob type (no dynamic SQL) so getE2EEBlob/upsertE2EEBlob remain free of
// string concatenation against table names.
type e2eeBlobConfig struct {
	selectSQL     string // SELECT encrypted_data, version, updated_at FROM <table> WHERE user_id = $1
	upsertSQL     string // INSERT ... ON CONFLICT (user_id) DO UPDATE ... RETURNING version
	jsonKey       string // e.g. "preferences", "saved_gifs"
	logLabel      string // e.g. "preferences", "saved gifs"
	broadcastType string // e.g. "preferences_updated", "saved_gifs_updated"
}

// getE2EEBlob fetches an opaque encrypted blob for the authenticated user.
// Returns { <jsonKey>: { encrypted_data, version, updated_at } } or { <jsonKey>: null }.
func (h *Handler) getE2EEBlob(c *gin.Context, cfg e2eeBlobConfig) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var encryptedData string
	var version int
	var updatedAt string
	err := h.db.QueryRow(cfg.selectSQL, userID).Scan(&encryptedData, &version, &updatedAt)

	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusOK, gin.H{cfg.jsonKey: nil})
			return
		}
		h.log.Error("Failed to fetch "+cfg.logLabel, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch " + cfg.logLabel})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		cfg.jsonKey: gin.H{
			"encrypted_data": encryptedData,
			"version":        version,
			"updated_at":     updatedAt,
		},
	})
}

// EncryptedBlobRequest represents a request to update an E2EE blob (preferences, saved GIFs, etc.).
type EncryptedBlobRequest struct {
	EncryptedData string `json:"encrypted_data" binding:"required"`
}

// upsertE2EEBlob validates an encrypted blob, upserts it for the authenticated user,
// and broadcasts an update notification via WebSocket to the user's other clients.
func (h *Handler) upsertE2EEBlob(c *gin.Context, cfg e2eeBlobConfig) {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": errMsgUnauthorized})
		return
	}

	var req EncryptedBlobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "encrypted_data is required"})
		return
	}

	if _, err := base64.StdEncoding.DecodeString(req.EncryptedData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "encrypted_data must be valid base64"})
		return
	}

	if len(req.EncryptedData) > 65536 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "encrypted_data exceeds 64KB limit"})
		return
	}

	var version int
	err := h.db.QueryRow(cfg.upsertSQL, userID, req.EncryptedData).Scan(&version)
	if err != nil {
		h.log.Error("Failed to update "+cfg.logLabel, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update " + cfg.logLabel})
		return
	}

	// Broadcast to user's other connected clients so they can pull the update
	if h.hub != nil {
		if uid, ok := userID.(string); ok {
			if parsed, parseErr := uuid.Parse(uid); parseErr == nil {
				h.hub.BroadcastToUser(parsed, websocket.OutgoingMessage{
					Type: cfg.broadcastType,
					Data: map[string]interface{}{
						"version": version,
					},
				})
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"version": version,
	})
}

var (
	prefsBlobConfig = e2eeBlobConfig{
		selectSQL:     `SELECT encrypted_data, version, updated_at FROM user_preferences WHERE user_id = $1`,
		upsertSQL:     `INSERT INTO user_preferences (user_id, encrypted_data, version, updated_at) VALUES ($1, $2, 1, NOW()) ON CONFLICT (user_id) DO UPDATE SET encrypted_data = $2, version = user_preferences.version + 1, updated_at = NOW() RETURNING version`,
		jsonKey:       "preferences",
		logLabel:      "preferences",
		broadcastType: "preferences_updated",
	}
	savedGifsBlobConfig = e2eeBlobConfig{
		selectSQL:     `SELECT encrypted_data, version, updated_at FROM saved_gifs WHERE user_id = $1`,
		upsertSQL:     `INSERT INTO saved_gifs (user_id, encrypted_data, version, updated_at) VALUES ($1, $2, 1, NOW()) ON CONFLICT (user_id) DO UPDATE SET encrypted_data = $2, version = saved_gifs.version + 1, updated_at = NOW() RETURNING version`,
		jsonKey:       "saved_gifs",
		logLabel:      "saved gifs",
		broadcastType: "saved_gifs_updated",
	}
	friendOrgBlobConfig = e2eeBlobConfig{
		selectSQL:     `SELECT encrypted_data, version, updated_at FROM friend_organization WHERE user_id = $1`,
		upsertSQL:     `INSERT INTO friend_organization (user_id, encrypted_data, version, updated_at) VALUES ($1, $2, 1, NOW()) ON CONFLICT (user_id) DO UPDATE SET encrypted_data = $2, version = friend_organization.version + 1, updated_at = NOW() RETURNING version`,
		jsonKey:       "friend_organization",
		logLabel:      "friend organization",
		broadcastType: "friend_organization_updated",
	}
)

// GetPreferences returns the current user's encrypted preferences blob.
func (h *Handler) GetPreferences(c *gin.Context) {
	h.getE2EEBlob(c, prefsBlobConfig)
}

// UpdatePreferences upserts the current user's encrypted preferences blob.
func (h *Handler) UpdatePreferences(c *gin.Context) {
	h.upsertE2EEBlob(c, prefsBlobConfig)
}

// GetSavedGifs returns the current user's encrypted saved GIFs blob.
func (h *Handler) GetSavedGifs(c *gin.Context) {
	h.getE2EEBlob(c, savedGifsBlobConfig)
}

// UpdateSavedGifs upserts the current user's encrypted saved GIFs blob.
func (h *Handler) UpdateSavedGifs(c *gin.Context) {
	h.upsertE2EEBlob(c, savedGifsBlobConfig)
}

// GetFriendOrganization returns the current user's encrypted friend-organization blob.
func (h *Handler) GetFriendOrganization(c *gin.Context) {
	h.getE2EEBlob(c, friendOrgBlobConfig)
}

// UpdateFriendOrganization upserts the current user's encrypted friend-organization blob.
func (h *Handler) UpdateFriendOrganization(c *gin.Context) {
	h.upsertE2EEBlob(c, friendOrgBlobConfig)
}

// --- Privacy Settings ---

// privacySettingsResponse represents the user's privacy settings.
type privacySettingsResponse struct {
	MessagesFriendsOnly                 bool   `json:"messages_friends_only"`
	MessagesServerMembers               bool   `json:"messages_server_members"`
	DMPrivacyLevel                      int    `json:"dm_privacy_level"` // 0=off, 1=friends, 2=friends+server, 3=all
	DMFriendsOfFriends                  bool   `json:"dm_friends_of_friends"`
	AutoAcceptFriendCodes               bool   `json:"auto_accept_friend_codes"`
	SearchableByUsername                bool   `json:"searchable_by_username"`
	SearchableByEmail                   bool   `json:"searchable_by_email"`
	SearchableByPhone                   bool   `json:"searchable_by_phone"`
	AllowEmbeddedContent                bool   `json:"allow_embedded_content"`
	LoadGifsAutomatically               bool   `json:"load_gifs_automatically"`
	EnableKlipyProxy                    bool   `json:"enable_klipy_proxy"`
	SharePersonalizationWithGifProvider bool   `json:"share_personalization_with_gif_provider"`
	UpdatedAt                           string `json:"updated_at,omitempty"`
}

// GetPrivacySettings returns the current user's privacy settings.
// Returns defaults if no settings have been saved yet.
// GET /users/me/privacy
func (h *Handler) GetPrivacySettings(c *gin.Context) {
	userID := c.GetString("user_id")

	var ps privacySettingsResponse
	err := h.db.QueryRow(`
		SELECT messages_friends_only, messages_server_members, dm_privacy_level, dm_friends_of_friends,
		       auto_accept_friend_codes, searchable_by_username, searchable_by_email, searchable_by_phone,
		       allow_embedded_content, load_gifs_automatically, enable_klipy_proxy,
		       share_personalization_with_gif_provider, updated_at
		FROM privacy_settings
		WHERE user_id = $1
	`, userID).Scan(
		&ps.MessagesFriendsOnly, &ps.MessagesServerMembers, &ps.DMPrivacyLevel, &ps.DMFriendsOfFriends,
		&ps.AutoAcceptFriendCodes, &ps.SearchableByUsername, &ps.SearchableByEmail, &ps.SearchableByPhone,
		&ps.AllowEmbeddedContent, &ps.LoadGifsAutomatically, &ps.EnableKlipyProxy,
		&ps.SharePersonalizationWithGifProvider, &ps.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		// Return full defaults matching the schema defaults (all fields explicit)
		c.JSON(http.StatusOK, gin.H{
			"privacy": privacySettingsResponse{
				MessagesFriendsOnly:                 true,
				MessagesServerMembers:               true,
				DMPrivacyLevel:                      2,
				DMFriendsOfFriends:                  false,
				AutoAcceptFriendCodes:               false,
				SearchableByUsername:                false,
				SearchableByEmail:                   false,
				SearchableByPhone:                   false,
				AllowEmbeddedContent:                false,
				LoadGifsAutomatically:               false,
				EnableKlipyProxy:                    false,
				SharePersonalizationWithGifProvider: true,
			},
		})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch privacy settings", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch privacy settings"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"privacy": ps})
}

// updatePrivacyRequest represents a partial update to privacy settings.
type updatePrivacyRequest struct {
	MessagesFriendsOnly                 *bool `json:"messages_friends_only"`
	MessagesServerMembers               *bool `json:"messages_server_members"`
	DMPrivacyLevel                      *int  `json:"dm_privacy_level"`
	DMFriendsOfFriends                  *bool `json:"dm_friends_of_friends"`
	AutoAcceptFriendCodes               *bool `json:"auto_accept_friend_codes"`
	SearchableByUsername                *bool `json:"searchable_by_username"`
	SearchableByEmail                   *bool `json:"searchable_by_email"`
	SearchableByPhone                   *bool `json:"searchable_by_phone"`
	AllowEmbeddedContent                *bool `json:"allow_embedded_content"`
	LoadGifsAutomatically               *bool `json:"load_gifs_automatically"`
	EnableKlipyProxy                    *bool `json:"enable_klipy_proxy"`
	SharePersonalizationWithGifProvider *bool `json:"share_personalization_with_gif_provider"`
}

// buildPrivacyClauses constructs the SET clauses for a partial privacy settings update.
// Returns an HTTP status + error message on validation failure, or 0/"" on success.
func buildPrivacyClauses(req *updatePrivacyRequest) ([]string, []interface{}, int, string) {
	var setClauses []string
	var args []interface{}
	argIdx := 2 // $1 is reserved for userID

	addBoolClause := func(column string, val *bool) {
		if val == nil {
			return
		}
		setClauses = append(setClauses, fmt.Sprintf("%s = $%d", column, argIdx))
		args = append(args, *val)
		argIdx++
	}

	addBoolClause("messages_friends_only", req.MessagesFriendsOnly)
	addBoolClause("messages_server_members", req.MessagesServerMembers)

	if req.DMPrivacyLevel != nil {
		level := *req.DMPrivacyLevel
		if level < 0 || level > 3 {
			return nil, nil, http.StatusBadRequest, "dm_privacy_level must be 0-3"
		}
		setClauses = append(setClauses, fmt.Sprintf("dm_privacy_level = $%d", argIdx))
		args = append(args, level)
		argIdx++
		setClauses = append(setClauses, dmPrivacyLegacySync(level)...)
	}

	addBoolClause("dm_friends_of_friends", req.DMFriendsOfFriends)
	addBoolClause("auto_accept_friend_codes", req.AutoAcceptFriendCodes)
	addBoolClause("searchable_by_username", req.SearchableByUsername)
	addBoolClause("searchable_by_email", req.SearchableByEmail)
	addBoolClause("searchable_by_phone", req.SearchableByPhone)
	addBoolClause("allow_embedded_content", req.AllowEmbeddedContent)
	addBoolClause("load_gifs_automatically", req.LoadGifsAutomatically)
	addBoolClause("enable_klipy_proxy", req.EnableKlipyProxy)
	addBoolClause("share_personalization_with_gif_provider", req.SharePersonalizationWithGifProvider)

	return setClauses, args, 0, ""
}

// dmPrivacyLegacySync returns the legacy boolean sync clauses for backward compatibility.
func dmPrivacyLegacySync(level int) []string {
	switch level {
	case 0, 1:
		return []string{"messages_friends_only = TRUE", "messages_server_members = FALSE"}
	case 2:
		return []string{"messages_friends_only = TRUE", "messages_server_members = TRUE"}
	case 3:
		return []string{"messages_friends_only = FALSE", "messages_server_members = TRUE"}
	default:
		return nil
	}
}

// UpdatePrivacySettings updates the current user's privacy settings.
// Accepts a partial JSON body — only provided fields are updated.
// PATCH /users/me/privacy
func (h *Handler) UpdatePrivacySettings(c *gin.Context) {
	userID := c.GetString("user_id")

	var req updatePrivacyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	setClauses, extraArgs, status, msg := buildPrivacyClauses(&req)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}
	if len(setClauses) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No fields to update"})
		return
	}

	setClauses = append(setClauses, "updated_at = NOW()")
	args := append([]interface{}{userID}, extraArgs...)

	// Two-step UPSERT in a transaction so a FIRST write persists (#1674). A single
	// `INSERT (user_id) VALUES ($1) ON CONFLICT DO UPDATE SET ...` drops the
	// provided values on a fresh user: the INSERT succeeds with defaults, so there
	// is no conflict and the SET clause never runs. Instead: ensure the row exists
	// (DO NOTHING), then UPDATE the provided columns. The UPDATE applies all
	// setClauses — parameterized $N assignments AND fixed legacy literal clauses
	// from dmPrivacyLegacySync — verbatim. The DB work runs in a closure so the
	// failure response (and its message) is written in exactly one place.
	var ps privacySettingsResponse
	txErr := func() error {
		tx, err := h.db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }() // no-op once Commit succeeds

		if _, err := tx.Exec(`INSERT INTO privacy_settings (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
			return err
		}

		// #nosec G201 -- Safe: setClauses contains only parameterized ($N) assignments and fixed legacy literal clauses; no user value is interpolated
		query := fmt.Sprintf(`
			UPDATE privacy_settings SET %s WHERE user_id = $1
			RETURNING messages_friends_only, messages_server_members, dm_privacy_level, dm_friends_of_friends,
			          auto_accept_friend_codes, searchable_by_username, searchable_by_email, searchable_by_phone,
			          allow_embedded_content, load_gifs_automatically, enable_klipy_proxy,
			          share_personalization_with_gif_provider, updated_at
		`, strings.Join(setClauses, ", "))

		if err := tx.QueryRow(query, args...).Scan(
			&ps.MessagesFriendsOnly, &ps.MessagesServerMembers, &ps.DMPrivacyLevel, &ps.DMFriendsOfFriends,
			&ps.AutoAcceptFriendCodes, &ps.SearchableByUsername, &ps.SearchableByEmail, &ps.SearchableByPhone,
			&ps.AllowEmbeddedContent, &ps.LoadGifsAutomatically, &ps.EnableKlipyProxy,
			&ps.SharePersonalizationWithGifProvider, &ps.UpdatedAt,
		); err != nil {
			return err
		}

		return tx.Commit()
	}()
	if txErr != nil {
		h.log.Error("Failed to update privacy settings", "error", txErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update privacy settings"})
		return
	}

	h.log.Info("Privacy settings updated", "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"privacy": ps})
}

// --- SSO settings endpoints (issue #270) ---

// ListSSOIdentities returns the linked SSO providers for the authenticated user.
// GET /api/v1/users/me/sso-identities
func (h *Handler) ListSSOIdentities(c *gin.Context) {
	userID := c.GetString("user_id")
	rows, err := h.db.QueryContext(c.Request.Context(),
		`SELECT provider, provider_email, is_relay_email, created_at, last_used_at
		 FROM user_sso_identities
		 WHERE user_id = $1
		 ORDER BY created_at ASC`, userID)
	if err != nil {
		h.log.Error("Failed to list SSO identities", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "db_error"})
		return
	}
	defer func() { _ = rows.Close() }()

	type identity struct {
		Provider      string     `json:"provider"`
		ProviderEmail string     `json:"provider_email"`
		IsRelayEmail  bool       `json:"is_relay_email"`
		LinkedAt      time.Time  `json:"linked_at"`
		LastUsedAt    *time.Time `json:"last_used_at"`
	}
	out := []identity{}
	for rows.Next() {
		var i identity
		if err := rows.Scan(&i.Provider, &i.ProviderEmail, &i.IsRelayEmail, &i.LinkedAt, &i.LastUsedAt); err != nil {
			h.log.Error("Failed to scan SSO identity row", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error_code": "scan_error"})
			return
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("Row iteration error listing SSO identities", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "rows_error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"identities": out})
}

// GetSecurity returns the authenticated user's SSO-related security flags so
// the Settings panel can hydrate the toggles on mount instead of defaulting
// to false. Mirrors the columns PatchSecurity writes to.
//
// GET /api/v1/users/me/security
func (h *Handler) GetSecurity(c *gin.Context) {
	userID := c.GetString("user_id")
	var pld, trust bool
	err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`,
		userID,
	).Scan(&pld, &trust)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error_code": "user_not_found"})
		return
	}
	if err != nil {
		h.log.Error("Failed to fetch security flags", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "lookup_failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"password_login_disabled": pld,
		"trust_sso_security":      trust,
	})
}

// patchSecurityRequest is the body of PATCH /api/v1/users/me/security.
// Pointer fields distinguish "not supplied" from "supplied as false".
type patchSecurityRequest struct {
	PasswordLoginDisabled *bool  `json:"password_login_disabled"`
	TrustSSOSecurity      *bool  `json:"trust_sso_security"`
	CurrentPassphrase     string `json:"current_passphrase"`
}

// verifyCurrentPassphrase loads the user's password hash and confirms the
// supplied passphrase matches. On any failure (lookup miss, lookup error,
// verify error, mismatch) it writes the appropriate error response to c and
// returns false; the caller MUST return immediately when this returns false
// to avoid double-writing the response. Extracted from PatchSecurity to keep
// that handler under SonarQube's S3776 cognitive-complexity threshold.
func (h *Handler) verifyCurrentPassphrase(c *gin.Context, userID, plaintext string) bool {
	var passwordHash string
	err := h.db.QueryRowContext(c.Request.Context(),
		`SELECT password_hash FROM users WHERE id = $1`, userID,
	).Scan(&passwordHash)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_credentials"})
		return false
	}
	if err != nil {
		h.log.Error("Failed to fetch password hash for security patch", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "lookup_failed"})
		return false
	}
	match, err := auth.VerifyPassword(plaintext, passwordHash)
	if err != nil {
		h.log.Error("Failed to verify passphrase for security patch", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "verify_failed"})
		return false
	}
	if !match {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "invalid_credentials"})
		return false
	}
	return true
}

// PatchSecurity flips the two SSO-related security flags. Any change requires
// passphrase confirmation to defend against session-token-replay-driven
// settings hijack.
// PATCH /api/v1/users/me/security
func (h *Handler) PatchSecurity(c *gin.Context) {
	userID := c.GetString("user_id")
	var req patchSecurityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error_code": "invalid_request"})
		return
	}
	if req.CurrentPassphrase == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error_code": "passphrase_required"})
		return
	}
	if !h.verifyCurrentPassphrase(c, userID, req.CurrentPassphrase) {
		return
	}

	// Lockout check: if the user is asking to disable password login, ensure
	// they have at least one SSO identity to fall back on. Otherwise the next
	// /auth/login attempt would 403 with account_uses_sso → empty providers
	// list → permanent lockout. This mirrors DeleteSSOIdentity's would_lock_out
	// gate from the other side: that endpoint refuses to remove the LAST
	// identity when password is disabled; this endpoint refuses to disable
	// password when there are NO identities. Together they form a structural
	// invariant: password OR ≥1 SSO identity, always.
	if req.PasswordLoginDisabled != nil && *req.PasswordLoginDisabled { // pragma: allowlist secret
		var ssoCount int64
		if err := h.db.QueryRowContext(c.Request.Context(),
			`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, userID,
		).Scan(&ssoCount); err != nil {
			h.log.Error("Failed to count SSO identities for lockout check", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error_code": "lookup_failed"})
			return
		}
		if ssoCount == 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"error_code": "would_lock_out",
				"detail":     "Link an SSO provider before disabling password login.",
			})
			return
		}
	}

	// Build a dynamic UPDATE so we only set provided fields. Column names are
	// hardcoded literals (not user input); only the values are bound via $N.
	var sets []string
	var args []any
	idx := 1
	if req.PasswordLoginDisabled != nil { // pragma: allowlist secret
		sets = append(sets, fmt.Sprintf("password_login_disabled = $%d", idx)) // pragma: allowlist secret
		args = append(args, *req.PasswordLoginDisabled)
		idx++
	}
	if req.TrustSSOSecurity != nil {
		sets = append(sets, fmt.Sprintf("trust_sso_security = $%d", idx))
		args = append(args, *req.TrustSSOSecurity)
		idx++
	}
	if len(sets) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error_code": "no_fields"})
		return
	}
	args = append(args, userID)
	// Safe: column names in `sets` are hardcoded literals from the if-blocks above;
	// only values are bound via $N placeholders, including the WHERE id = $idx target.
	q := fmt.Sprintf( //nolint:gosec // hardcoded column names + integer idx — no injection risk // nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query,concord-go-sql-sprintf
		"UPDATE users SET %s WHERE id = $%d",
		strings.Join(sets, ", "), idx,
	)
	// nosemgrep: go.net.sql.go-vanillasql-format-string-sqli-taint-med-conf.go-vanillasql-format-string-sqli-taint-med-conf,go.net.sql.go-vanillasql-format-string-sqli-taint.go-vanillasql-format-string-sqli-taint
	if _, err := h.db.ExecContext(c.Request.Context(), q, args...); err != nil { //nolint:gosec // q composed above from hardcoded column names + integer idx via fmt.Sprintf; user values flow only through args... as parameterized $N placeholders.
		h.log.Error("Failed to update security settings", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "update_failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// DeleteSSOIdentity unlinks a provider from the authenticated user.
// Refuses if doing so would leave the user with no authentication method.
// DELETE /api/v1/users/me/sso-identities/:provider
//
// The lockout check + delete run inside a single transaction with SELECT FOR
// UPDATE on the users row to defend against TOCTOU: without the lock, two
// concurrent requests could both observe a "still has fallback" state, both
// pass the wouldHaveAnyAuth gate, and both delete different identities, jointly
// leaving the user locked out. The row lock serializes the check-and-delete so
// the second request observes the post-delete state and refuses.
func (h *Handler) DeleteSSOIdentity(c *gin.Context) {
	userID := c.GetString("user_id")
	provider := c.Param("provider")

	ctx := c.Request.Context()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.log.Error("Failed to begin transaction for SSO identity delete", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "delete_failed"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the users row for the duration of the tx — concurrent
	// DeleteSSOIdentity / PatchSecurity calls for the same user must serialize
	// against this lock. The correlated subquery counts identities EXCLUDING
	// the one being deleted (i.e., the post-delete identity count).
	var pld bool
	var identityCount int64
	err = tx.QueryRowContext(ctx,
		`SELECT u.password_login_disabled, (
		    SELECT COUNT(*) FROM user_sso_identities WHERE user_id = u.id AND provider != $2
		 )
		 FROM users u WHERE u.id = $1
		 FOR UPDATE`,
		userID, provider,
	).Scan(&pld, &identityCount)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error_code": "user_not_found"})
		return
	}
	if err != nil {
		h.log.Error("Failed to evaluate lockout check for SSO identity delete", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "lookup_failed"})
		return
	}
	wouldHaveAnyAuth := !pld /* password counts when not disabled */ || identityCount > 0
	if !wouldHaveAnyAuth {
		c.JSON(http.StatusBadRequest, gin.H{
			"error_code": "would_lock_out",
			"detail":     "Set a passphrase first or link another provider before unlinking this one.",
		})
		return
	}

	res, err := tx.ExecContext(ctx,
		`DELETE FROM user_sso_identities WHERE user_id = $1 AND provider = $2`,
		userID, provider)
	if err != nil {
		h.log.Error("Failed to delete SSO identity", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "delete_failed"})
		return
	}
	rows, err := res.RowsAffected()
	if err != nil {
		h.log.Error("Failed to read RowsAffected for SSO identity delete", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "delete_failed"})
		return
	}
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error_code": "not_linked"})
		return
	}
	if err := tx.Commit(); err != nil {
		h.log.Error("Failed to commit SSO identity delete", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_code": "delete_failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
