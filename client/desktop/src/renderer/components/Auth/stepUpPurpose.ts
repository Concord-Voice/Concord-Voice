/**
 * The step-up purposes the renderer sends to `POST /mfa/webauthn/verify-inline/begin`.
 *
 * Mirrors the server's closed `stepup.Purpose` set
 * (`services/control-plane/internal/stepup/purpose.go`), restricted to the
 * routes a desktop step-up prompt (`MFAVerifyPrompt` or the factor picker)
 * actually guards. One value per ROUTE: the
 * server binds the minted WebAuthn inline token to this purpose and accepts it
 * only on that route, so a prompt must name the request its code is sent with,
 * never a neighbouring one. A value missing from the server's set is refused
 * at begin with a 400.
 */
export const STEP_UP_PURPOSES = [
  'mfa_settings.totp_setup',
  'mfa_settings.totp_disable',
  'mfa_settings.webauthn_register',
  'mfa_settings.recovery_only_set',
  'mfa_settings.recovery_hardened_set',
  'mfa_settings.email_sms_disable',
  'mfa_settings.backup_email_set',
  'mfa_settings.recovery_key_replace',
  'privacy.purge_fence_disable',
  'sessions.revoke',
  'sessions.revoke_all',
  'sessions.revocation_mode_set',
  'messages.delete',
  'messages.channel_purge',
  'messages.server_purge',
  'dm.message_delete',
  'dm.purge',
  'dm.clear',
] as const;

export type StepUpPurpose = (typeof STEP_UP_PURPOSES)[number];

/**
 * The own-rule purposes a password step-up token may be minted for at
 * `POST /api/v1/auth/step-up/password` (#3509): the routes the server's
 * `stepup.VerifyOwnRuleTx` guards. Mirrors `ownRulePurposes` in the same Go
 * file, exactly — the server refuses any other purpose with a 400 before it
 * looks at the password, so a value here that Go lacks can never mint, and a Go
 * value missing here is a route whose password prompt cannot complete.
 */
export const PASSWORD_STEP_UP_PURPOSES = [
  'dm.clear',
  'dm.message_delete',
  'messages.delete',
  'messages.channel_purge',
  'messages.server_purge',
] as const;

export type PasswordStepUpPurpose = (typeof PASSWORD_STEP_UP_PURPOSES)[number];
