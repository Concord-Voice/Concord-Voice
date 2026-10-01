package auth

// This file declares sentinel errors exposed to other packages.
//
// These sentinels let cross-package callers (notably internal/oauth, which
// shares the password-verification path via AuthAdapter) discriminate failure
// modes via errors.Is rather than fragile string matching against err.Error().

import "errors"

// ErrAccountLocked is returned by VerifyPassword when the per-email lockout
// counter has reached the threshold defined by loginLockoutThreshold /
// loginLockoutDurations. Callers should translate to HTTP 423 Locked.
//
// Used by:
//   - internal/oauth.Handler.CompleteLink — translates to 423 + error_code
//     "account_locked" so the renderer can surface a dedicated UX state
//     distinct from generic 401 invalid_credentials.
var ErrAccountLocked = errors.New("account_locked: too many failed attempts")

// ErrInvalidCredentials is returned by VerifyPassword when the password does
// not match (or the user does not exist), after the failure has been counted
// against the shared lockout. Its text is the "invalid_credentials" the
// unexported error carried before it became a sentinel.
//
// Used by:
//   - internal/oauth.Handler.CompleteLink — any non-lockout error is its 401.
//   - internal/stepup.PasswordTokenHandler — answers 403 "Invalid password",
//     and tells it apart from a lookup or hash fault, which is a 500.
var ErrInvalidCredentials = errors.New("invalid_credentials")

// ErrAccountDisabled is returned by IssueAccessAndRefresh when the target account
// is terminally disabled (users.disabled = TRUE, e.g. by the #1623 age-verification
// valid_age=false path). It gates the SSO token-mint path the same way the password
// login + refresh gates do, so a disabled user cannot mint a session via SSO.
//
// Used by:
//   - internal/oauth.Handler — translates to HTTP 403 + error_code "account_disabled".
var ErrAccountDisabled = errors.New("account_disabled: account is terminally disabled")

// ErrSSOIdentityInsert is returned when a CompleteLink transaction cannot add
// the requested SSO identity. The OAuth handler preserves its established
// sso_identity_insert_failed response without exposing the database error.
var ErrSSOIdentityInsert = errors.New("sso identity insert failed")
