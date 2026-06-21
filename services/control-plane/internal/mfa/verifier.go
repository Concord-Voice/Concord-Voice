package mfa

import "context"

// Verifier provides MFA status and code verification for use by other handlers
// (sessions, users) without coupling them to the full MFA handler.
type Verifier interface {
	// IsEnabled returns true if the user has at least one confirmed MFA method.
	IsEnabled(ctx context.Context, userID string) bool

	// VerifyCode validates a TOTP code or backup code for the given user.
	// Returns true if the code is valid. For backup codes, marks the code as used.
	VerifyCode(ctx context.Context, userID string, code string) (bool, error)

	// GetEnabledMethods returns the list of MFA methods enabled for the user
	// (e.g. ["totp", "webauthn"]).
	GetEnabledMethods(ctx context.Context, userID string) ([]string, error)
}
