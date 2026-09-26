package stepup

import "slices"

// Purpose names the one step-up consumer ROUTE a WebAuthn inline verification
// token was minted for.
//
// The invariant: a token minted by WebAuthnVerifyInlineFinish for purpose P is
// accepted only by P's consumer. The begin request names the purpose, the
// server stores it with the ceremony session, finish reads it from that session
// (never from the finish body), and the token is stored under a key that embeds
// it. A consumer claims only its own purpose's key, so a token minted for any
// other action is an absent key to it: refused exactly like an invalid code,
// and not consumed.
//
// What breaks the invariant: two routes sharing a purpose (a token minted for
// one becomes spendable on the other — so there is one purpose per ROUTE, never
// per family), a consumer passing a purpose other than its own, or finish
// trusting a purpose the client sends it. The login MFA challenge takes no
// purpose at all and never reads an inline token.
//
// Values are wire-visible: the desktop sends them in the begin body, and
// client/desktop/src/renderer/components/Auth/stepUpPurpose.ts mirrors the ones
// it uses. Renaming one breaks every client that sends it.
type Purpose string

// One constant per consumer route. The naming is <surface>.<action>.
const (
	// MFA-settings routes (internal/mfa settings_stepup.go, plus TOTPDisable).
	PurposeTOTPSetup              Purpose = "mfa_settings.totp_setup"
	PurposeTOTPDisable            Purpose = "mfa_settings.totp_disable"
	PurposeWebAuthnRegister       Purpose = "mfa_settings.webauthn_register"
	PurposeRecoveryOnlySet        Purpose = "mfa_settings.recovery_only_set"
	PurposeRecoveryHardenedSet    Purpose = "mfa_settings.recovery_hardened_set"
	PurposeEmailSmsSetup          Purpose = "mfa_settings.email_sms_setup"
	PurposeEmailSmsDisable        Purpose = "mfa_settings.email_sms_disable"
	PurposeBackupEmailSet         Purpose = "mfa_settings.backup_email_set"
	PurposeRecoveryKeyReplace     Purpose = "mfa_settings.recovery_key_replace"
	PurposeRecoveryKeyRemove      Purpose = "mfa_settings.recovery_key_remove"
	PurposeTrustedDeviceDesignate Purpose = "mfa_settings.trusted_device_designate"
	PurposeTrustedDeviceRemove    Purpose = "mfa_settings.trusted_device_remove"
	PurposeRecoveryCircleUpsert   Purpose = "mfa_settings.recovery_circle_upsert"
	PurposeRecoveryCircleDelete   Purpose = "mfa_settings.recovery_circle_delete"

	// internal/users. The password change and the E2EE key reset verify through
	// one helper (verifyStepUpWithLockedUser) and still take one purpose each.
	PurposePasswordChange    Purpose = "account.password_change"
	PurposeE2EEKeyReset      Purpose = "account.e2ee_key_reset"
	PurposePurgeFenceDisable Purpose = "privacy.purge_fence_disable"

	// internal/sessions.
	PurposeSessionRevoke     Purpose = "sessions.revoke"
	PurposeSessionsRevokeAll Purpose = "sessions.revoke_all"
	PurposeRevocationModeSet Purpose = "sessions.revocation_mode_set"

	// internal/ownership.
	PurposeOwnershipTransfer Purpose = "ownership.transfer_initiate"
	PurposeOwnershipReverse  Purpose = "ownership.transfer_reverse"

	// internal/dm.
	PurposeDMPurge Purpose = "dm.purge"
	PurposeDMClear Purpose = "dm.clear"

	// internal/servers, through mfaenforce.ConfirmTx.
	PurposeServerMFAEnforcementOff Purpose = "servers.mfa_enforcement_disable"
)

// allPurposes is the closed set Valid checks. A constant missing from it can
// never mint a token, which fails closed but silently; TestPurposes_ClosedSet
// pins the list.
var allPurposes = [...]Purpose{
	PurposeTOTPSetup,
	PurposeTOTPDisable,
	PurposeWebAuthnRegister,
	PurposeRecoveryOnlySet,
	PurposeRecoveryHardenedSet,
	PurposeEmailSmsSetup,
	PurposeEmailSmsDisable,
	PurposeBackupEmailSet,
	PurposeRecoveryKeyReplace,
	PurposeRecoveryKeyRemove,
	PurposeTrustedDeviceDesignate,
	PurposeTrustedDeviceRemove,
	PurposeRecoveryCircleUpsert,
	PurposeRecoveryCircleDelete,
	PurposePasswordChange,
	PurposeE2EEKeyReset,
	PurposePurgeFenceDisable,
	PurposeSessionRevoke,
	PurposeSessionsRevokeAll,
	PurposeRevocationModeSet,
	PurposeOwnershipTransfer,
	PurposeOwnershipReverse,
	PurposeDMPurge,
	PurposeDMClear,
	PurposeServerMFAEnforcementOff,
}

// Valid reports whether p is one of the closed set of consumer purposes. The
// empty string is not.
func (p Purpose) Valid() bool {
	return slices.Contains(allPurposes[:], p)
}

// Purposes returns a copy of the closed set, in declaration order.
func Purposes() []Purpose {
	return slices.Clone(allPurposes[:])
}
