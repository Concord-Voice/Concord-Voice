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

// ErrAccountDisabled is returned by IssueAccessAndRefresh when the target account
// is terminally disabled (users.disabled = TRUE, e.g. by the #1623 age-verification
// valid_age=false path). It gates the SSO token-mint path the same way the password
// login + refresh gates do, so a disabled user cannot mint a session via SSO.
//
// Used by:
//   - internal/oauth.Handler — translates to HTTP 403 + error_code "account_disabled".
var ErrAccountDisabled = errors.New("account_disabled: account is terminally disabled")
