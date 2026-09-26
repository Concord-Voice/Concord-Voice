// Package stepup owns password/MFA step-up verification policy for the
// control-plane.
//
// It exists because five call sites independently grew their own step-up
// decision table with materially different behaviour — different statuses for
// the same condition, and only one of them (internal/dm) offering an
// actionable response when no password factor is available. This package is
// that one, extracted, so there is exactly one place the decision lives and
// #2565 / #2562 / #2567 can build on a single seam.
//
// What the duplication actually cost was CONSISTENCY, not correctness against
// a NULL password_hash. users.password_hash is TEXT NOT NULL (migration
// 000001, never relaxed) and SSO registration stores a real Argon2id hash
// alongside a password_login_disabled flag that is set, so an account with no
// stored hash is not currently reachable. That flag — not an absent hash — is
// how this codebase marks "cannot log in with a password". The
// no-password-factor handling here is deliberate defence-in-depth for a state
// the schema forbids today; see VerifyPasswordFactor.
//
// Its internal imports are internal/auth (password verification),
// internal/credepoch (the epoch fence LockSubjectTx applies) and
// internal/middleware (the fail-closed attempt budget); it otherwise depends on
// stdlib, gin (Error carries a gin.H body and Write emits it onto a
// gin.Context) and go-redis (Budget). None of the three imports this package,
// so no cycle is possible. The MFA dependency is declared here, at the
// consumer, following the rbac.PresenceRecheck precedent, so internal/mfa is
// not imported.
//
// Policy P1 lives here too: the MFA leg is required only for a factor the
// server can verify inline — TOTP (enabled AND confirmed) or WebAuthn (any
// credential) — and that set is read from the factor tables by
// InlineMFAMethods, never from users.mfa_enabled / users.mfa_methods, which
// also list email and SMS and can be stale in either direction.
package stepup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
)

// ErrMsgVerificationFailed is the opaque 500 body. Byte-identical to the DM
// purge original so no caller's copy diverges.
const ErrMsgVerificationFailed = "Verification failed"

// ErrMsgInvalidPassword and ErrMsgInvalidMFACode are the 403 refusal bodies.
// The desktop's classifyStepUpRefusal matches them byte-for-byte to place the
// refusal on the right field, so a route off this seam that refuses a password
// or code must send these exact strings too.
const (
	ErrMsgInvalidPassword = "Invalid password" // pragma: allowlist secret
	ErrMsgInvalidMFACode  = "Invalid MFA code"
)

// ErrMsgMFAEnrollmentRequired is the 403 body EnrollmentRequired sends. It is
// byte-stable: the desktop shows it verbatim, and #3456 teaches the desktop's
// step-up refusal classifier the mfa_enrollment_required flag beside it.
const ErrMsgMFAEnrollmentRequired = "Set up an authenticator app or security key to do this."

// ErrPasswordVerification marks a 500 that came from the password factor. It is
// a fixed sentinel rather than the underlying error because that error can
// embed the malformed hash — see the Cause field on Error.
var ErrPasswordVerification = errors.New("password verification failed")

// MFAStatusChecker reports whether an account has any MFA factor available.
//
// No function in this package consults it any more: LoadSubject and
// LockSubjectTx derive MFA from the factor tables (policy P1), because a bool
// has no way to report a failed read and every caller of the old path read
// that failure as "no MFA". It stays in the MFAVerifier union because the
// consumers' own interfaces (internal/dm, internal/users) still name it.
type MFAStatusChecker interface {
	IsEnabled(ctx context.Context, userID string) bool
}

// MFAMethodLister names the enabled factors so a missing-code rejection can
// tell the client which prompt to render.
type MFAMethodLister interface {
	GetEnabledMethods(ctx context.Context, userID string) ([]string, error)
}

// MFACodeVerifier is the pool-scoped MFA surface VerifyMFAFactor needs. It is
// deliberately narrower than MFAVerifier: internal/dm holds an mfa.Verifier,
// which has no VerifyCodeTx, and must still be able to call the non-tx form.
type MFACodeVerifier interface {
	MFAMethodLister
	VerifyCode(ctx context.Context, userID, code string) (bool, error)
}

// MFATxCodeVerifier is the transaction-scoped surface VerifyMFAFactorTx needs.
type MFATxCodeVerifier interface {
	MFAMethodLister
	VerifyCodeTx(ctx context.Context, tx *sql.Tx, userID, code string) (bool, error)
}

// MFAVerifier is the full MFA surface step-up policy can consume. *mfa.Handler
// satisfies it structurally; this package does not import internal/mfa. A
// caller that holds all four methods can pass one value to every entry point.
type MFAVerifier interface {
	MFAStatusChecker
	MFACodeVerifier
	MFATxCodeVerifier
}

// RowQuerier is the single-row read surface LoadSubject and InlineMFAMethods
// need, satisfied by *sql.DB, *sql.Tx, and *sql.Conn alike.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Error carries the HTTP status and body a failed factor must produce.
//
// Callers holding a gin context write it directly; callers inside a
// transaction return it up and let the outer handler write it, which is why
// this is a value rather than a side effect on the context.
type Error struct {
	Status int
	Body   gin.H

	// Cause is the underlying failure behind a 500, for the CALLER to log. It
	// is never serialized — Write emits Body only — so it cannot reach a
	// client. Nil on every 4xx: a rejected credential is an outcome, not a
	// fault, and has nothing to diagnose.
	//
	// It is deliberately NOT populated on the password-verification 500
	// (VerifyPasswordFactor). That error comes from auth.VerifyPassword on a
	// malformed hash, and an argon2 decode error can embed the hash it failed
	// to parse — [internal]rules/observability.md Core principle #1 bars a
	// password hash from every log sink, in any form. That path carries
	// ErrPasswordVerification instead: a fixed sentinel that says which stage
	// broke without carrying anything derived from the credential.
	Cause error

	// reason tags a refusal a caller must tell apart for telemetry without
	// reading the body text. Never serialized.
	reason refusalReason
}

// refusalReason is the closed set of refusals a caller may need to classify.
type refusalReason int

const (
	reasonNone refusalReason = iota
	reasonEpochMismatch
)

// EpochMismatch reports whether this is LockSubjectTx's credential-epoch
// refusal, so a caller can emit its credential-epoch event without comparing
// body strings. Nil-safe.
func (e *Error) EpochMismatch() bool { return e != nil && e.reason == reasonEpochMismatch }

// Error renders the status only. The body may carry call-site copy, and no
// credential value ever reaches either — see [internal]rules/observability.md.
func (e *Error) Error() string {
	return fmt.Sprintf("step-up failed with status %d", e.Status)
}

// Unwrap exposes Cause to errors.Is/errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// Write emits the error onto a gin context.
func (e *Error) Write(c *gin.Context) { c.JSON(e.Status, e.Body) }

// Subject is the account state a step-up decision is made against. Build it
// with LoadSubject (no transaction) or LockSubjectTx (inside the write
// transaction); both derive MFAEnabled and MFAMethods under policy P1.
//
// PasswordHash is COALESCE'd to the empty string, so callers never see a Go
// nil/NULL distinction. Empty means "no usable password factor". That state is
// NOT reachable through any current write path — the column is NOT NULL and
// every writer stores a real hash — so treat empty as a defensive case, not as
// the SSO case. SSO accounts are marked by users.password_login_disabled and
// DO carry a real hash.
type Subject struct {
	PasswordHash string
	// MFAEnabled is true iff MFAMethods is non-empty.
	MFAEnabled bool
	// MFAMethods is the inline-verifiable subset ("totp", "webauthn") read from
	// the factor tables. It is what a missing-code refusal offers the client.
	MFAMethods []string
}

// Copy carries the two call-site-specific strings. Everything else is fixed,
// because the client discriminates on the fixed strings.
type Copy struct {
	// NoFactors is shown when the account has neither a password nor MFA. It
	// must tell the user how to proceed — never a raw 500.
	NoFactors string
	// CredentialRequired is shown when a password account supplied no password.
	//
	// Deliberately NOT named for the credential it describes. The pre-commit
	// detect-secrets hook flags a password-shaped key sitting beside a quoted
	// literal, and the copy that fills this field is a quoted literal — the
	// same reason StepUpFields.tsx calls its prop `credentialError`. Renaming
	// this to PasswordRequired blocks every commit that touches the file.
	CredentialRequired string
}

// VerifyPasswordFactor checks the password half.
//
// An empty PasswordHash means the account has no usable password factor, so
// MFA becomes the only one available: this passes iff MFA is enabled, and
// otherwise returns an actionable 400 telling the actor how to proceed rather
// than a raw 500.
//
// That branch is UNREACHABLE through any current write path (see Subject), and
// is kept deliberately. It is the fail-safe for a schema change, for a
// hand-edited row, and for a future authentication method that genuinely has
// no password — and it costs one string comparison. Do not delete it as dead
// code, and do not cite it as proof that SSO accounts lack a hash: they do not
// lack one. If a migration ever relaxes the NOT NULL constraint, this branch
// and internal/users' TestUsersPasswordHashCannotBeNull are the two places
// that must be revisited together.
//
// Order matters: the no-factor branch is evaluated BEFORE the
// missing-input branch, because an empty stored hash is not the same thing as
// an actor who simply supplied nothing.
func VerifyPasswordFactor(subj Subject, currentPassword string, wording Copy) *Error {
	if subj.PasswordHash == "" {
		if !subj.MFAEnabled {
			return &Error{Status: http.StatusBadRequest, Body: gin.H{"error": wording.NoFactors}}
		}
		return nil // passwordless: MFA carries the step-up
	}

	if currentPassword == "" {
		return &Error{Status: http.StatusForbidden, Body: gin.H{
			"error": wording.CredentialRequired, "password_required": true,
		}}
	}
	match, err := auth.VerifyPassword(currentPassword, subj.PasswordHash)
	if err != nil {
		// The underlying error is DISCARDED, not wrapped. An argon2 decode
		// failure can embed the malformed hash it could not parse, and a
		// password hash may reach no log sink in any form
		// ([internal]rules/observability.md Core principle #1). The sentinel
		// names the stage that broke and carries nothing derived from the
		// credential — enough to tell this 500 apart from the other two.
		return &Error{
			Status: http.StatusInternalServerError,
			Body:   gin.H{"error": ErrMsgVerificationFailed},
			Cause:  ErrPasswordVerification,
		}
	}
	if !match {
		return &Error{Status: http.StatusForbidden, Body: gin.H{"error": ErrMsgInvalidPassword}}
	}
	return nil
}

// VerifyMFAFactor checks the MFA half outside a transaction. Only call when the
// actor has MFA enabled. preloadedMethods has the same meaning as on
// VerifyMFAFactorTx: pass the Subject's MFAMethods so a missing-code refusal
// offers the P1 set rather than users.mfa_methods.
func VerifyMFAFactor(ctx context.Context, v MFACodeVerifier, userID, mfaCode string, preloadedMethods []string) *Error {
	return verifyMFA(ctx, v, userID, mfaCode, preloadedMethods, func() (bool, error) {
		return v.VerifyCode(ctx, userID, mfaCode)
	})
}

// VerifyMFAFactorTx checks the MFA half INSIDE a transaction.
//
// Use this whenever an enclosing transaction exists. MFA verification is a
// WRITE — backup-code redemption marks the code used — so verifying on the
// pool while a transaction is open lets a rollback burn a single-use factor
// while changing nothing else.
// preloadedMethods carries the inline MFA method names the caller's Subject
// already holds, so the missing-code branch does not have to look them up. A
// nil slice means "not preloaded — look them up"; that fallback reads
// users.mfa_methods and so is P1-blind, which is why every production caller
// passes Subject.MFAMethods (never nil from LoadSubject or LockSubjectTx).
func VerifyMFAFactorTx(
	ctx context.Context, tx *sql.Tx, v MFATxCodeVerifier, userID, mfaCode string, preloadedMethods []string,
) *Error {
	return verifyMFA(ctx, v, userID, mfaCode, preloadedMethods, func() (bool, error) {
		return v.VerifyCodeTx(ctx, tx, userID, mfaCode)
	})
}

func verifyMFA(
	ctx context.Context, m MFAMethodLister, userID, mfaCode string,
	preloadedMethods []string, verify func() (bool, error),
) *Error {
	if mfaCode == "" {
		// Prefer methods the caller already read from its locked row. Looking
		// them up here would take a SECOND pooled connection while the caller
		// holds one plus the users-row lock — the hazard [internal]rules/
		// backend.md already records for entCache.GetTier (CodeRabbit review,
		// #2792). A GetEnabledMethods failure degrades the copy, never the
		// status, so the fallback stays error-tolerant.
		methods := preloadedMethods
		if methods == nil {
			methods, _ = m.GetEnabledMethods(ctx, userID)
		}
		return &Error{Status: http.StatusForbidden, Body: gin.H{
			"error": "MFA verification required", "mfa_required": true, "methods": methods,
		}}
	}
	valid, err := verify()
	if err != nil {
		// Safe to surface: this is an MFA BACKEND failure (store unreachable,
		// TOTP subsystem down), not anything derived from the submitted code.
		// The pre-extraction DM handler logged exactly this, and dropping it
		// would leave a 500 from a broken MFA backend with no diagnostic
		// anywhere.
		return &Error{
			Status: http.StatusInternalServerError,
			Body:   gin.H{"error": ErrMsgVerificationFailed},
			Cause:  fmt.Errorf("mfa verify: %w", err),
		}
	}
	if !valid {
		return &Error{Status: http.StatusForbidden, Body: gin.H{"error": ErrMsgInvalidMFACode}}
	}
	return nil
}

// EnrollmentRequired refuses an action that demands an inline MFA confirmation
// from an actor with no inline factor to confirm with (policy P1: no confirmed
// TOTP and no WebAuthn credential). It is distinct from the missing-code
// refusal: there is no code this actor could send, so offering "methods"
// would prompt for a factor that does not exist. A 4xx outcome, so Cause is
// nil. Each call returns a fresh value because Body is a mutable map.
func EnrollmentRequired() *Error {
	return &Error{Status: http.StatusForbidden, Body: gin.H{
		"error": ErrMsgMFAEnrollmentRequired, "mfa_enrollment_required": true,
	}}
}
