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
// Its only internal import is internal/auth; it otherwise depends on stdlib
// plus gin, which it needs because Error carries a gin.H body and Write emits
// it onto a gin.Context. An earlier comment claimed "internal/auth and stdlib"
// and omitted gin — corrected after review (#2792). The MFA dependency is
// declared here, at the consumer, following the rbac.PresenceRecheck
// precedent, so no new INTERNAL import edge is created.
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

// ErrPasswordVerification marks a 500 that came from the password factor. It is
// a fixed sentinel rather than the underlying error because that error can
// embed the malformed hash — see the Cause field on Error.
var ErrPasswordVerification = errors.New("password verification failed")

// MFAStatusChecker reports whether an account has any MFA factor available.
// LoadSubject needs only this.
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

// RowQuerier is the single-row read surface LoadSubject needs, satisfied by
// *sql.DB, *sql.Tx, and *sql.Conn alike.
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
}

// Error renders the status only. The body may carry call-site copy, and no
// credential value ever reaches either — see [internal]rules/observability.md.
func (e *Error) Error() string {
	return fmt.Sprintf("step-up failed with status %d", e.Status)
}

// Unwrap exposes Cause to errors.Is/errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// Write emits the error onto a gin context.
func (e *Error) Write(c *gin.Context) { c.JSON(e.Status, e.Body) }

// Subject is the account state a step-up decision is made against.
//
// PasswordHash is COALESCE'd to the empty string, so callers never see a Go
// nil/NULL distinction. Empty means "no usable password factor". That state is
// NOT reachable through any current write path — the column is NOT NULL and
// every writer stores a real hash — so treat empty as a defensive case, not as
// the SSO case. SSO accounts are marked by users.password_login_disabled and
// DO carry a real hash.
type Subject struct {
	PasswordHash string
	MFAEnabled   bool
	// MFAMethods is the enabled method names, when the caller read them from
	// the same row it locked. Nil is fine; it only means the missing-code
	// branch must look them up itself.
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

// LoadSubject reads the step-up inputs.
//
// The COALESCE is defensive, not load-bearing today: users.password_hash is
// TEXT NOT NULL (migration 000001, never relaxed), so Scan into a string
// cannot fail on NULL. It is kept because it makes this query correct for free
// if that constraint is ever relaxed, and because the alternative — a bare
// Scan into a string — turns such a migration into a 500 at every call site at
// once. Do not read it as evidence that a NULL occurs.
func LoadSubject(ctx context.Context, q RowQuerier, userID string, v MFAStatusChecker) (Subject, *Error) {
	var s Subject
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(password_hash, '') FROM users WHERE id = $1`, userID,
	).Scan(&s.PasswordHash); err != nil {
		// Safe to surface: a database read failure, not anything derived from
		// a credential. COALESCE means a NULL hash is no longer an error path,
		// so reaching here is a genuine fault worth diagnosing.
		return Subject{}, &Error{
			Status: http.StatusInternalServerError,
			Body:   gin.H{"error": ErrMsgVerificationFailed},
			Cause:  fmt.Errorf("load step-up subject: %w", err),
		}
	}
	s.MFAEnabled = v != nil && v.IsEnabled(ctx, userID)
	return s, nil
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
		return &Error{Status: http.StatusForbidden, Body: gin.H{"error": "Invalid password"}}
	}
	return nil
}

// VerifyMFAFactor checks the MFA half outside a transaction. Only call when the
// actor has MFA enabled.
func VerifyMFAFactor(ctx context.Context, v MFACodeVerifier, userID, mfaCode string) *Error {
	return verifyMFA(ctx, v, userID, mfaCode, nil, func() (bool, error) {
		return v.VerifyCode(ctx, userID, mfaCode)
	})
}

// VerifyMFAFactorTx checks the MFA half INSIDE a transaction.
//
// Use this whenever an enclosing transaction exists. MFA verification is a
// WRITE — backup-code redemption marks the code used — so verifying on the
// pool while a transaction is open lets a rollback burn a single-use factor
// while changing nothing else.
// preloadedMethods carries the enabled MFA method names a caller already read
// from its own locked row, so the missing-code branch does not have to look
// them up. A nil slice means "not preloaded — look them up".
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
		return &Error{Status: http.StatusForbidden, Body: gin.H{"error": "Invalid MFA code"}}
	}
	return nil
}
