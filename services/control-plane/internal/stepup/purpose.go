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

	// internal/dm. PurposeDMMessageDelete confirms a DM message delete past the
	// delete-rate soft-lock (#3455).
	PurposeDMPurge         Purpose = "dm.purge"
	PurposeDMClear         Purpose = "dm.clear"
	PurposeDMMessageDelete Purpose = "dm.message_delete"

	// internal/messages: a channel message delete, and a channel or server
	// self-purge, past the delete-rate soft-lock (#3455). The two purge routes
	// take one purpose each, never a shared "messages.purge".
	PurposeMessageDelete Purpose = "messages.delete"
	PurposeChannelPurge  Purpose = "messages.channel_purge"
	PurposeServerPurge   Purpose = "messages.server_purge"

	// internal/servers, through mfaenforce.ConfirmTx.
	PurposeServerMFAEnforcementOff Purpose = "servers.mfa_enforcement_disable"

	// The dangerous-action gates (#3454), through mfaenforce.Require: one per
	// D1 route. A D1 channel or server purge reuses PurposeChannelPurge and
	// PurposeServerPurge, because it is the same route and the same request as
	// the self-purge, and one verification must not run under two purposes.
	PurposeChannelDelete            Purpose = "channels.delete"
	PurposeServerUpdate             Purpose = "servers.update"
	PurposeServerIconUpload         Purpose = "media.server_icon_upload"
	PurposeServerBannerUpload       Purpose = "media.server_banner_upload"
	PurposeMemberBan                Purpose = "members.ban"
	PurposeMemberKickPurge          Purpose = "members.kick_purge"
	PurposeRoleDelete               Purpose = "roles.delete"
	PurposeRoleCreate               Purpose = "roles.create"
	PurposeRoleUpdate               Purpose = "roles.update"
	PurposeChannelExpirationShorten Purpose = "channels.expiration_shorten"
	PurposeServerDelete             Purpose = "servers.delete"
	PurposeChannelOverrideUpsert    Purpose = "overrides.channel_upsert"
	PurposeCategoryOverrideUpsert   Purpose = "overrides.category_upsert"
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
	PurposeDMMessageDelete,
	PurposeMessageDelete,
	PurposeChannelPurge,
	PurposeServerPurge,
	PurposeServerMFAEnforcementOff,
	PurposeChannelDelete,
	PurposeServerUpdate,
	PurposeServerIconUpload,
	PurposeServerBannerUpload,
	PurposeMemberBan,
	PurposeMemberKickPurge,
	PurposeRoleDelete,
	PurposeRoleCreate,
	PurposeRoleUpdate,
	PurposeChannelExpirationShorten,
	PurposeServerDelete,
	PurposeChannelOverrideUpsert,
	PurposeCategoryOverrideUpsert,
}

// ownRulePurposes are the routes stepup.VerifyOwnRuleTx guards: the only
// purposes a password step-up token may be minted for (#3509). Each is also in
// allPurposes. A purpose outside this list is refused at the mint endpoint
// before the password is verified, so a password token can never be spent on
// an MFA-settings, session or ownership route, whose consumers never read one
// anyway. TestOwnRulePurposes_ClosedSet pins the list and
// client/desktop/src/renderer/components/Auth/stepUpPurpose.ts mirrors it.
var ownRulePurposes = [...]Purpose{
	PurposeDMClear,
	PurposeDMMessageDelete,
	PurposeMessageDelete,
	PurposeChannelPurge,
	PurposeServerPurge,
}

// graceEligiblePurposes are the routes a step-up grace may cover (#3454 D-2,
// A-5): within GraceTTL of a verified confirmation, the same session repeats
// an action of the same kind without a code. Each is also in allPurposes.
//
// Two families qualify. The D1 gates whose action is reversible or bounded,
// keyed by the dangerous bit the route requires; and the delete-rate
// soft-lock's four routes, keyed by the soft-lock scope, which A-10 retrofits
// (the two purge purposes serve both families). A purpose outside this list
// is always fresh: GraceRead.Covers refuses it whatever the stored grace
// says. Deleting a server, turning MFA enforcement off, and the grant gate on
// role create/update and both override upserts are deliberately absent: each
// either cannot be undone or hands out the authority the gate protects.
// TestGraceEligiblePurposes_ClosedSet pins the list.
var graceEligiblePurposes = [...]Purpose{
	PurposeMessageDelete,
	PurposeDMMessageDelete,
	PurposeChannelPurge,
	PurposeServerPurge,
	PurposeChannelDelete,
	PurposeServerUpdate,
	PurposeServerIconUpload,
	PurposeServerBannerUpload,
	PurposeMemberBan,
	PurposeMemberKickPurge,
	PurposeRoleDelete,
	PurposeChannelExpirationShorten,
}

// GraceEligible reports whether a step-up grace may cover p (see
// graceEligiblePurposes).
func (p Purpose) GraceEligible() bool {
	return slices.Contains(graceEligiblePurposes[:], p)
}

// GraceEligiblePurposes returns a copy of the grace-eligible set, in
// declaration order.
func GraceEligiblePurposes() []Purpose {
	return slices.Clone(graceEligiblePurposes[:])
}

// OwnRule reports whether p is one of the own-rule purposes a password
// step-up token may be minted for.
func (p Purpose) OwnRule() bool {
	return slices.Contains(ownRulePurposes[:], p)
}

// OwnRulePurposes returns a copy of the own-rule set, in declaration order.
func OwnRulePurposes() []Purpose {
	return slices.Clone(ownRulePurposes[:])
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
