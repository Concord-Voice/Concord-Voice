package stepup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
)

// ErrMsgSessionNoLongerValid is the 401 body for a valid JWT whose users row
// is gone (a deleted account). It is the client's to handle — sign out — so
// it is a 401, not a 500 (the EmailSmsVerify precedent in internal/mfa).
const ErrMsgSessionNoLongerValid = "Session no longer valid"

// ErrMsgAuthenticationRequired is the 401 body for a credential-epoch
// mismatch. Byte-identical to the other fenced writes so the client's existing
// 401 handling applies unchanged.
const ErrMsgAuthenticationRequired = "Authentication required"

// inlineMFAMethodsSQL is policy P1's predicate: TOTP only when enabled AND
// confirmed (a pending enrollment verifies nothing), WebAuthn when any
// credential exists. Email and SMS are absent on purpose — no inline verifier
// exists for them.
const inlineMFAMethodsSQL = `
	SELECT EXISTS (SELECT 1 FROM user_mfa_totp WHERE user_id = $1 AND enabled AND confirmed),
	       EXISTS (SELECT 1 FROM user_mfa_webauthn WHERE user_id = $1)`

// loadSubjectSQL reads the password hash and the P1 predicate in ONE
// statement, so the two halves of an unlocked Subject share a snapshot and a
// missing users row is a single sql.ErrNoRows.
const loadSubjectSQL = `
	SELECT COALESCE(u.password_hash, ''),
	       EXISTS (SELECT 1 FROM user_mfa_totp WHERE user_id = u.id AND enabled AND confirmed),
	       EXISTS (SELECT 1 FROM user_mfa_webauthn WHERE user_id = u.id)
	FROM users u WHERE u.id = $1`

// Two const statements chosen by Lock — never fmt.Sprintf'd SQL.
const (
	lockForShareSQL       = `SELECT credential_epoch, COALESCE(password_hash, '') FROM users WHERE id = $1 FOR SHARE`
	lockForNoKeyUpdateSQL = `SELECT credential_epoch, COALESCE(password_hash, '') FROM users WHERE id = $1 FOR NO KEY UPDATE`
)

// Lock selects the users-row lock LockSubjectTx takes as the transaction's
// first statement. Strength follows the WRITES, not the route: a transaction
// that later writes users must take LockForNoKeyUpdate, because FOR SHARE
// followed by UPDATE users is a lock upgrade and two concurrent same-user
// requests deadlock on it. Either lock conflicts with the FOR NO KEY UPDATE
// every destructive reset holds, which is what serializes a step-up against a
// reset that supersedes its credentials.
type Lock int

// The two users-row locks a step-up gate may take; see Lock.
const (
	LockForShare Lock = iota
	LockForNoKeyUpdate
)

// InlineMFAMethods returns exactly the factors the MFA verifier can accept
// inline for this user: "totp" when a TOTP row is enabled AND confirmed,
// "webauthn" when any credential exists (policy P1). It reads the factor
// tables, never users.mfa_methods. The result is never nil, so a caller can
// hand it straight to a missing-code refusal.
func InlineMFAMethods(ctx context.Context, q RowQuerier, userID string) ([]string, error) {
	var totp, webauthn bool
	if err := q.QueryRowContext(ctx, inlineMFAMethodsSQL, userID).Scan(&totp, &webauthn); err != nil {
		return nil, fmt.Errorf("read inline MFA methods: %w", err)
	}
	return inlineMethods(totp, webauthn), nil
}

func inlineMethods(totp, webauthn bool) []string {
	methods := make([]string, 0, 2)
	if totp {
		methods = append(methods, "totp")
	}
	if webauthn {
		methods = append(methods, "webauthn")
	}
	return methods
}

// LoadSubject reads the step-up inputs for a caller that holds no transaction.
// MFA comes from the factor tables (policy P1) and a failed read is a 500 —
// it is never read as "no MFA", which would skip the MFA leg.
//
// The COALESCE is defensive, not load-bearing today: users.password_hash is
// TEXT NOT NULL (migration 000001, never relaxed). It is kept because it makes
// this query correct for free if that constraint is ever relaxed. Do not read
// it as evidence that a NULL occurs.
func LoadSubject(ctx context.Context, q RowQuerier, userID string) (Subject, *Error) {
	var s Subject
	var totp, webauthn bool
	err := q.QueryRowContext(ctx, loadSubjectSQL, userID).Scan(&s.PasswordHash, &totp, &webauthn)
	if errors.Is(err, sql.ErrNoRows) {
		return Subject{}, subjectGone()
	}
	if err != nil {
		// Safe to surface: a database read failure, not anything derived from
		// a credential.
		return Subject{}, verificationFailed(fmt.Errorf("load step-up subject: %w", err))
	}
	s.MFAMethods = inlineMethods(totp, webauthn)
	s.MFAEnabled = len(s.MFAMethods) > 0
	return s, nil
}

// LockSubjectTx is the common prefix of every in-transaction step-up gate. It
// MUST be the transaction's first statement. In order:
//
//  1. Lock the users row (lock) and read credential_epoch + password_hash.
//  2. Fence the session: credepoch.MatchEpoch against tokenEpoch. The step-up
//     proves knowledge of a password; the fence proves the session was not
//     revoked. MatchEpoch, not GuardTx: the row is already locked here, and
//     GuardTx would issue a second locking read of it.
//  3. Read the P1 predicate in a SEPARATE statement, after the lock.
//
// Step 3 being its own statement is load-bearing, not tidiness. Under READ
// COMMITTED a statement's snapshot is taken when it starts; if the lock in
// step 1 had to wait for a concurrent factor write (every such write locks
// the users row first), subqueries folded into step 1 would still see the
// factor tables as they were BEFORE that write committed, while the row itself
// is re-read at its newest version. A statement issued after the lock is
// granted sees the committed write.
//
// Refusals: a missing users row is 401 ErrMsgSessionNoLongerValid; a
// mismatched epoch is 401 ErrMsgAuthenticationRequired (EpochMismatch true);
// any read failure is 500 with Cause set. The caller rolls back on refusal.
func LockSubjectTx(ctx context.Context, tx *sql.Tx, userID string, lock Lock, tokenEpoch string) (Subject, *Error) {
	var query string
	switch lock {
	case LockForShare:
		query = lockForShareSQL
	case LockForNoKeyUpdate:
		query = lockForNoKeyUpdateSQL
	default:
		// Fail closed on a value no caller should be able to construct.
		return Subject{}, verificationFailed(fmt.Errorf("lock step-up subject: unknown lock %d", lock))
	}

	var s Subject
	var epoch sql.NullString
	err := tx.QueryRowContext(ctx, query, userID).Scan(&epoch, &s.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Subject{}, subjectGone()
	}
	if err != nil {
		return Subject{}, verificationFailed(fmt.Errorf("lock step-up subject: %w", err))
	}
	if credepoch.MatchEpoch(epoch, tokenEpoch) != nil {
		return Subject{}, &Error{
			Status: http.StatusUnauthorized,
			Body:   gin.H{"error": ErrMsgAuthenticationRequired},
			reason: reasonEpochMismatch,
		}
	}

	methods, err := InlineMFAMethods(ctx, tx, userID)
	if err != nil {
		return Subject{}, verificationFailed(err)
	}
	s.MFAMethods = methods
	s.MFAEnabled = len(methods) > 0
	return s, nil
}

func subjectGone() *Error {
	return &Error{Status: http.StatusUnauthorized, Body: gin.H{"error": ErrMsgSessionNoLongerValid}}
}

func verificationFailed(cause error) *Error {
	return &Error{
		Status: http.StatusInternalServerError,
		Body:   gin.H{"error": ErrMsgVerificationFailed},
		Cause:  cause,
	}
}
