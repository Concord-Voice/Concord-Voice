package models

import (
	"encoding/json"
	"time"
)

// User represents a Concord user
type User struct {
	ID                string          `json:"id" db:"id"`
	Email             string          `json:"email" db:"email"`
	Username          string          `json:"username" db:"username"`
	PasswordHash      string          `json:"-" db:"password_hash"` // Never send in JSON
	DisplayName       *string         `json:"display_name,omitempty" db:"display_name"`
	Bio               *string         `json:"bio,omitempty" db:"bio"`
	AvatarURL         *string         `json:"avatar_url,omitempty" db:"avatar_url"`
	HeaderImageURL    *string         `json:"header_image_url,omitempty" db:"header_image_url"`
	ColorScheme       *string         `json:"color_scheme,omitempty" db:"color_scheme"`
	Links             json.RawMessage `json:"links" db:"links"`
	EmailVerified     bool            `json:"email_verified" db:"email_verified"`
	AgeVerified       bool            `json:"age_verified" db:"age_verified"`
	CreatedAt         time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at" db:"updated_at"`
	UsernameChangedAt *time.Time      `json:"username_changed_at,omitempty" db:"username_changed_at"`

	// PasswordLoginDisabled indicates the account has SSO-only login. Set via
	// PATCH /api/v1/users/me/security (with passphrase confirmation); cleared
	// via the same endpoint or by the account-recovery flow. The field is also
	// initialised by POST /api/v1/auth/sso/:provider/complete-registration when
	// a user completes first-time SSO registration without a password. Loaded
	// by lookupUserForLogin so POST /api/v1/auth/login can short-circuit with
	// HTTP 403 account_uses_sso instead of returning 401 invalid_credentials.
	PasswordLoginDisabled bool `json:"-" db:"password_login_disabled"`

	// Disabled is the soft-disable flag set terminally by the age-verification
	// enforcement path (migration 000069, #1623) when a client-signed claim
	// reports valid_age=false. Loaded by lookupUserForLogin / fetchActiveRefreshToken
	// so login and refresh fail closed with HTTP 403 account_disabled, and mirrored
	// into the Redis user_disabled:<id> denylist for immediate mid-session effect.
	// Never serialized to clients (json:"-").
	Disabled bool `json:"-" db:"disabled"`
}

// addOptionalProfileFields adds the shared optional fields to a profile map.
func (u *User) addOptionalProfileFields(result map[string]interface{}) {
	if u.DisplayName != nil {
		result["display_name"] = *u.DisplayName
	}
	if u.Bio != nil {
		result["bio"] = *u.Bio
	}
	if u.AvatarURL != nil {
		result["avatar_url"] = *u.AvatarURL
	}
	if u.HeaderImageURL != nil {
		result["header_image_url"] = *u.HeaderImageURL
	}
	if u.ColorScheme != nil {
		result["color_scheme"] = *u.ColorScheme
	}
	if len(u.Links) > 0 {
		result["links"] = u.Links
	}
}

// PublicUser returns user data safe for the authenticated user
func (u *User) PublicUser() map[string]interface{} {
	result := map[string]interface{}{
		"id":             u.ID,
		"email":          u.Email,
		"username":       u.Username,
		"email_verified": u.EmailVerified,
		"created_at":     u.CreatedAt,
		"links":          json.RawMessage("[]"),
	}
	u.addOptionalProfileFields(result)
	if u.UsernameChangedAt != nil {
		result["username_changed_at"] = *u.UsernameChangedAt
		result["username_change_eligible_at"] = u.UsernameChangedAt.AddDate(0, 0, 365)
	}
	return result
}

// ProfileForOthers returns user data safe for viewing by other users (no email or private fields)
func (u *User) ProfileForOthers() map[string]interface{} {
	result := map[string]interface{}{
		"id":         u.ID,
		"username":   u.Username,
		"created_at": u.CreatedAt,
		"links":      json.RawMessage("[]"),
	}
	u.addOptionalProfileFields(result)
	return result
}

// UserKeys represents E2EE keys for a user
type UserKeys struct {
	UserID            string    `json:"user_id" db:"user_id"`
	WrappedPrivateKey []byte    `json:"-" db:"wrapped_private_key"` // Never send directly
	KeyDerivationSalt []byte    `json:"-" db:"key_derivation_salt"` // Never send directly
	KeyVersion        int       `json:"key_version" db:"key_version"`
	KeyDerivationAlg  string    `json:"key_derivation_alg" db:"key_derivation_alg"` // "pbkdf2" or "argon2id"
	CreatedAt         time.Time `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time `json:"updated_at" db:"updated_at"`
}

// RefreshToken represents a session refresh token
type RefreshToken struct {
	ID         string     `json:"id" db:"id"`
	UserID     string     `json:"user_id" db:"user_id"`
	TokenHash  string     `json:"-" db:"token_hash"` // Never send
	DeviceName string     `json:"device_name" db:"device_name"`
	IPAddress  string     `json:"ip_address" db:"ip_address"`
	UserAgent  string     `json:"user_agent" db:"user_agent"`
	MachineID  string     `json:"machine_id,omitempty" db:"machine_id"`
	ExpiresAt  time.Time  `json:"expires_at" db:"expires_at"`
	CreatedAt  time.Time  `json:"created_at" db:"created_at"`
	LastUsedAt time.Time  `json:"last_used_at" db:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty" db:"revoked_at"`
	RememberMe bool       `json:"remember_me" db:"remember_me"`
}

// Session returns a public-safe session representation
func (rt *RefreshToken) Session() map[string]interface{} {
	return map[string]interface{}{
		"id":          rt.ID,
		"device_name": rt.DeviceName,
		"ip_address":  rt.IPAddress,
		"machine_id":  rt.MachineID,
		"created_at":  rt.CreatedAt,
		"last_used":   rt.LastUsedAt,
		"expires_at":  rt.ExpiresAt,
	}
}
