package mfa

import (
	"context"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// Verifier provides MFA status and code verification for use by other handlers
// (sessions, users) without coupling them to the full MFA handler.
type Verifier interface {
	// IsEnabled returns true if the user has at least one confirmed MFA method.
	IsEnabled(ctx context.Context, userID string) bool

	// VerifyCode validates a TOTP code, a backup code, or a WebAuthn inline
	// token minted for purpose, for the given user. Returns true if the code is
	// valid. For backup codes and inline tokens, marks the code as used. purpose
	// is the calling route's own stepup.Purpose; an invalid one is an error.
	VerifyCode(ctx context.Context, userID string, purpose stepup.Purpose, code string) (bool, error)

	// GetEnabledMethods returns the list of MFA methods enabled for the user
	// (e.g. ["totp", "webauthn"]).
	GetEnabledMethods(ctx context.Context, userID string) ([]string, error)
}
