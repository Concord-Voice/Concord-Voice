/**
 * The step-up purposes the renderer sends to `POST /mfa/webauthn/verify-inline/begin`.
 *
 * Mirrors the server's closed `stepup.Purpose` set
 * (`services/control-plane/internal/stepup/purpose.go`), restricted to the
 * routes a desktop `MFAVerifyPrompt` actually guards. One value per ROUTE: the
 * server binds the minted WebAuthn inline token to this purpose and accepts it
 * only on that route, so a prompt must name the request its code is sent with,
 * never a neighbouring one. A value missing from the server's set is refused
 * at begin with a 400.
 */
export type StepUpPurpose =
  | 'mfa_settings.totp_setup'
  | 'mfa_settings.totp_disable'
  | 'mfa_settings.webauthn_register'
  | 'mfa_settings.recovery_only_set'
  | 'mfa_settings.recovery_hardened_set'
  | 'mfa_settings.email_sms_disable'
  | 'mfa_settings.backup_email_set'
  | 'mfa_settings.recovery_key_replace'
  | 'sessions.revoke'
  | 'sessions.revoke_all'
  | 'sessions.revocation_mode_set';
