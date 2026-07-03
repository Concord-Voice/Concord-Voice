package oauth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestSSOTokenTTL_HumanePassphraseWindow is a regression guard for #2045.
//
// The SSO registration sso_token bridges POST /sso/:provider/session (mint) and
// POST /sso/:provider/complete-registration (consume). Its TTL must outlast the
// HUMAN step it gates: choosing a username and creating an E2EE passphrase. A
// 5-minute window was too tight — a user dwelling on the passphrase screen hit a
// Redis GET-miss -> 401 sso_token_invalid and a dead-end "Registration failed."
//
// 15m matches auth.PendingRegistrationTTL (the email-verification analogue for the
// same human step) and sits above stateTTL (10m, the machine-speed OAuth
// round-trip). Nothing else asserts this value, so this tripwire is the sole guard
// against a silent regression back to a hair-trigger window. Do NOT lower it
// without revisiting the #2045 UX rationale and the replay-window analysis in the
// issue's Security Considerations.
func TestSSOTokenTTL_HumanePassphraseWindow(t *testing.T) {
	assert.Equal(t, 15*time.Minute, ssoTokenTTL,
		"sso_token TTL must give a human time to create a passphrase (regression #2045)")
}
