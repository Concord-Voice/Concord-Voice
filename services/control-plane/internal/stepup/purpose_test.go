package stepup

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPurposes_ClosedSet pins the consumer purpose set: every constant is
// valid, distinct, and in Purposes(); nothing outside it is. A purpose two
// routes shared would let a token minted for one be spent on the other.
func TestPurposes_ClosedSet(t *testing.T) {
	all := Purposes()
	require.Len(t, all, 25, "one purpose per consumer route; update the table and this count together")

	seen := map[Purpose]bool{}
	for _, p := range all {
		require.True(t, p.Valid(), "%q must be valid", p)
		require.False(t, seen[p], "%q is listed twice", p)
		seen[p] = true
	}
	for _, p := range []Purpose{
		PurposeBackupEmailSet, PurposeEmailSmsDisable, PurposeServerMFAEnforcementOff,
		PurposeDMPurge, PurposeSessionRevoke, PurposeOwnershipTransfer, PurposePasswordChange, PurposeE2EEKeyReset,
	} {
		require.True(t, seen[p], "%q must be in the closed set", p)
	}

	for _, p := range []Purpose{"", "not.a.purpose", "MFA_SETTINGS.BACKUP_EMAIL_SET", "mfa_settings.backup_email_set ", "mfa_settings"} {
		require.False(t, p.Valid(), "%q must not be valid", p)
	}

	all[0] = "mutated"
	require.True(t, Purposes()[0].Valid(), "Purposes returns a copy")
}
