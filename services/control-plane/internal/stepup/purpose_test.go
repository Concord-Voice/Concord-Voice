package stepup

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPurposes_ClosedSet pins the consumer purpose set: every constant is
// valid, distinct, and in Purposes(); nothing outside it is. A purpose two
// routes shared would let a token minted for one be spent on the other.
func TestPurposes_ClosedSet(t *testing.T) {
	all := Purposes()
	require.Len(t, all, 42, "one purpose per consumer route; update the table and this count together")

	seen := map[Purpose]bool{}
	for _, p := range all {
		require.True(t, p.Valid(), "%q must be valid", p)
		require.False(t, seen[p], "%q is listed twice", p)
		seen[p] = true
	}
	for _, p := range []Purpose{
		PurposeBackupEmailSet, PurposeEmailSmsDisable, PurposeServerMFAEnforcementOff,
		PurposeDMPurge, PurposeSessionRevoke, PurposeOwnershipTransfer, PurposePasswordChange, PurposeE2EEKeyReset,
		PurposeMessageDelete, PurposeDMMessageDelete, PurposeChannelPurge, PurposeServerPurge,
	} {
		require.True(t, seen[p], "%q must be in the closed set", p)
	}

	for _, p := range []Purpose{"", "not.a.purpose", "MFA_SETTINGS.BACKUP_EMAIL_SET", "mfa_settings.backup_email_set ", "mfa_settings"} {
		require.False(t, p.Valid(), "%q must not be valid", p)
	}

	// The delete and self-purge purposes are mirrored in the desktop's StepUpPurpose union
	// (#3455), so their wire spelling is pinned here as well as there.
	require.Equal(t, Purpose("messages.delete"), PurposeMessageDelete)
	require.Equal(t, Purpose("dm.message_delete"), PurposeDMMessageDelete)
	require.Equal(t, Purpose("messages.channel_purge"), PurposeChannelPurge)
	require.Equal(t, Purpose("messages.server_purge"), PurposeServerPurge)

	// The dangerous-action purposes (#3454) are wire-visible: #3456's desktop
	// sends them in the WebAuthn begin body. Pinned by value, in A-5's order.
	dangerous := map[Purpose]string{
		PurposeChannelDelete:            "channels.delete",
		PurposeServerUpdate:             "servers.update",
		PurposeServerIconUpload:         "media.server_icon_upload",
		PurposeServerBannerUpload:       "media.server_banner_upload",
		PurposeMemberBan:                "members.ban",
		PurposeMemberKickPurge:          "members.kick_purge",
		PurposeRoleDelete:               "roles.delete",
		PurposeRoleCreate:               "roles.create",
		PurposeRoleUpdate:               "roles.update",
		PurposeChannelExpirationShorten: "channels.expiration_shorten",
		PurposeServerDelete:             "servers.delete",
		PurposeChannelOverrideUpsert:    "overrides.channel_upsert",
		PurposeCategoryOverrideUpsert:   "overrides.category_upsert",
	}
	require.Len(t, dangerous, 13, "A-5 adds exactly 13 purposes")
	for p, wire := range dangerous {
		require.Equal(t, Purpose(wire), p)
		require.True(t, seen[p], "%q must be in the closed set", p)
		require.False(t, p.OwnRule(), "%q must never accept a password token (A-5)", p)
	}

	all[0] = "mutated"
	require.True(t, Purposes()[0].Valid(), "Purposes returns a copy")
}

// TestGraceEligiblePurposes_ClosedSet pins which routes a step-up grace may
// cover (#3454 D-2, A-5). Adding a purpose here lets a stolen session repeat
// that action for ten minutes after one confirmation, so the always-fresh
// routes are asserted by name as well as by count.
func TestGraceEligiblePurposes_ClosedSet(t *testing.T) {
	want := []Purpose{
		PurposeMessageDelete, PurposeDMMessageDelete, PurposeChannelPurge, PurposeServerPurge,
		PurposeChannelDelete, PurposeServerUpdate, PurposeServerIconUpload, PurposeServerBannerUpload,
		PurposeMemberBan, PurposeMemberKickPurge, PurposeRoleDelete, PurposeChannelExpirationShorten,
	}
	require.ElementsMatch(t, want, GraceEligiblePurposes())

	for _, p := range Purposes() {
		require.Equal(t, slices.Contains(want, p), p.GraceEligible(), "%q", p)
	}

	for _, p := range []Purpose{
		PurposeServerDelete, PurposeServerMFAEnforcementOff, PurposeRoleCreate, PurposeRoleUpdate,
		PurposeChannelOverrideUpsert, PurposeCategoryOverrideUpsert,
		// The own-rule-only and settings routes are outside D-2 entirely.
		PurposeDMClear, PurposeDMPurge, PurposeTOTPDisable, PurposePasswordChange,
		"", "not.a.purpose",
	} {
		require.False(t, p.GraceEligible(), "%q must always ask for a fresh confirmation", p)
	}

	got := GraceEligiblePurposes()
	got[0] = PurposeServerDelete
	require.False(t, PurposeServerDelete.GraceEligible(), "GraceEligiblePurposes returns a copy")
}
