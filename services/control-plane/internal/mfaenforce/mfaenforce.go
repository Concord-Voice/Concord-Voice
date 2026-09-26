// Package mfaenforce is the in-transaction gate for a server's "Enforce MFA On
// Dangerous Actions" setting (servers.enforce_mfa_dangerous_actions, #3453).
// The toggle endpoint, the dangerous-action gates (#3454) and the soft-lock
// (#3455) all consume it.
//
// # Sequence
//
// A caller runs, inside one READ COMMITTED transaction:
//
//  1. LockGateTx, which locks the actor's users row and then the servers row
//     and returns the flag, the owner and the actor's P1 subject;
//  2. its OWN permission check, answering its own 403;
//  3. RequireConfirmationTx (a gated action) or ConfirmTx (an action that
//     always needs confirmation, such as turning the setting off).
//
// Step 2 must sit between the other two (invariant I7). RequireConfirmationTx
// answers nil on a server that does not enforce and an MFA refusal on one that
// does, so running it before the permission check would tell an unauthorized
// member whether the server enforces MFA, which only the owner and
// Administrators may read. The same applies to Gate.Enforcing: nothing derived
// from it may reach a caller who fails step 2.
//
// # Lock order
//
// The global order is: advisory locks, then users, then servers, then children
// (channels, roles, members, messages). LockGateTx takes users and then servers
// itself, so a caller cannot interleave them. Advisory locks and SET LOCAL
// lock_timeout may precede it; see stepup.LockSubjectTx for the full contract.
//
// The serverLock a caller asks for MUST be at least the strongest lock the
// transaction later takes on that servers row. Asking for less turns the later
// statement into a lock upgrade, and two concurrent requests that both hold the
// weaker lock deadlock trying to upgrade it. FOR KEY SHARE is deliberately not
// offered: it does not conflict with the toggle's UPDATE (FOR NO KEY UPDATE),
// so the flag could change under a gate that had already read it.
//
// # Errors
//
// Classify a LockGateTx error in this order: IsLockConflict (503, Retry-After),
// then errors.As into *stepup.Error (write its status and body), then
// errors.Is ErrServerNotFound (404), then 500. IsLockConflict must come first:
// a lock timeout on the users row arrives as a 500 *stepup.Error whose Cause
// chain carries the 55P03.
//
// Never log Gate.Enforcing, Subject.MFAEnabled or Subject.MFAMethods. Which of
// these a member has is exactly what I7 keeps indistinguishable
// ([internal]rules/observability.md principle 7).
//
// # Imports
//
// This is a leaf. It imports stepup and the standard library, plus lib/pq for
// SQLSTATE matching and gin for the *stepup.Error body type, which stepup
// already imports. The packages that consume it (rbac, servers, members,
// channels, api) must never become its dependencies; imports_test.go enforces
// that over the whole transitive closure.
package mfaenforce

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// ServerLock selects the row lock LockGateTx takes on the servers row. Choose
// the strongest lock the transaction will later take on that row (see the
// package comment). The zero value is not a lock, so a forgotten argument is
// refused rather than defaulted.
type ServerLock int

const (
	// ServerForShare suits a transaction that never writes the servers row.
	ServerForShare ServerLock = iota + 1
	// ServerForNoKeyUpdate suits a transaction that updates non-key columns of
	// the servers row, such as the toggle itself.
	ServerForNoKeyUpdate
	// ServerForUpdate suits a transaction that deletes the servers row or
	// already holds it FOR UPDATE.
	ServerForUpdate
)

// ErrServerNotFound reports that no servers row matched. The server may have
// been deleted after the caller's membership check.
var ErrServerNotFound = errors.New("mfaenforce: server not found")

// Gate is what LockGateTx read under its locks.
type Gate struct {
	// Enforcing is servers.enforce_mfa_dangerous_actions. It cannot change
	// before the transaction ends: every lock LockGateTx offers conflicts with
	// the toggle's UPDATE.
	Enforcing bool
	// OwnerID is servers.owner_id. It is read under the same lock, so an
	// ownership transfer cannot commit between this read and the caller's
	// permission check.
	OwnerID string
	// Subject is the actor's P1 subject, read after the users lock. Enrolled
	// means Subject.MFAEnabled; the methods to offer are Subject.MFAMethods.
	Subject stepup.Subject
}

// One statement per lock, chosen by ServerLock and never built from input.
// Each reads the isolation level in the same statement, so the READ COMMITTED
// check costs no round trip.
const (
	gateServerForShareSQL = `SELECT owner_id, enforce_mfa_dangerous_actions, current_setting('transaction_isolation')
		FROM servers WHERE id = $1 FOR SHARE`
	gateServerForNoKeyUpdateSQL = `SELECT owner_id, enforce_mfa_dangerous_actions, current_setting('transaction_isolation')
		FROM servers WHERE id = $1 FOR NO KEY UPDATE`
	gateServerForUpdateSQL = `SELECT owner_id, enforce_mfa_dangerous_actions, current_setting('transaction_isolation')
		FROM servers WHERE id = $1 FOR UPDATE`
)

// isolationReadCommitted is what current_setting('transaction_isolation')
// reports under READ COMMITTED.
const isolationReadCommitted = "read committed"

// The two SQLSTATEs a consistent lock order should never produce, and a
// lock_timeout turns a hang into.
const (
	sqlStateDeadlockDetected = "40P01"
	sqlStateLockNotAvailable = "55P03"
)

func (l ServerLock) query() (string, bool) {
	switch l {
	case ServerForShare:
		return gateServerForShareSQL, true
	case ServerForNoKeyUpdate:
		return gateServerForNoKeyUpdateSQL, true
	case ServerForUpdate:
		return gateServerForUpdateSQL, true
	default:
		return "", false
	}
}

// LockGateTx locks the actor's users row (userLock), fences the session
// against tokenEpoch, reads the actor's P1 subject, and then locks the servers
// row (serverLock) and reads its owner and enforcement flag. See the package
// comment for where it must sit in a transaction and how to classify its error.
//
// The error is a *stepup.Error (401 for a deleted account or a revoked
// session, 500 with Cause for a failed users read), ErrServerNotFound, or a
// wrapped internal error. A *stepup.Error is returned only when non-nil, so a
// nil error is a true nil.
func LockGateTx(
	ctx context.Context, tx *sql.Tx, serverID, actorID string,
	userLock stepup.Lock, serverLock ServerLock, tokenEpoch string,
) (Gate, error) {
	// Validate before any statement: a refusal here must not leave the users
	// row locked for a transaction that then carries on.
	query, ok := serverLock.query()
	if !ok {
		return Gate{}, fmt.Errorf("mfaenforce: unknown server lock %d", serverLock)
	}

	subj, e := stepup.LockSubjectTx(ctx, tx, actorID, userLock, tokenEpoch)
	if e != nil {
		return Gate{}, e
	}

	var g Gate
	var isolation string
	err := tx.QueryRowContext(ctx, query, serverID).Scan(&g.OwnerID, &g.Enforcing, &isolation)
	if errors.Is(err, sql.ErrNoRows) {
		// Reported before the isolation check, which needs a row to run. Both
		// outcomes refuse the action.
		return Gate{}, ErrServerNotFound
	}
	if err != nil {
		return Gate{}, fmt.Errorf("mfaenforce: lock server: %w", err)
	}
	if isolation != isolationReadCommitted {
		// The subject above was read under this isolation level, so its P1
		// verdict may be stale. Refusing here means nothing ever uses it.
		return Gate{}, fmt.Errorf("mfaenforce: the gate requires READ COMMITTED, the transaction is %s", isolation)
	}
	g.Subject = subj
	return g, nil
}

// ConfirmTx demands an inline MFA confirmation from the actor, whatever the
// server's setting. The toggle's OFF path calls it directly. Invariant I8:
// there is no path to nil without a verified factor.
//
// An actor with no inline factor gets stepup.EnrollmentRequired. A nil
// verifier is a 500. Otherwise the code is checked on tx through
// stepup.VerifyMFAFactorTx, so a backup code is spent only if tx commits.
func ConfirmTx(
	ctx context.Context, tx *sql.Tx, subj stepup.Subject, v stepup.MFATxCodeVerifier, actorID, code string,
) *stepup.Error {
	// Never `return nil` here. That is the shape of the purge-fence gate,
	// which skips the MFA leg for an unenrolled user because a password
	// carries its step-up. Nothing else carries this one.
	//
	// Both fields are checked. A Subject built by hand with MFAEnabled but no
	// methods would otherwise reach VerifyMFAFactorTx with a nil slice, which
	// falls back to users.mfa_methods: a list that ignores P1, read on a
	// second pooled connection while tx holds the users row.
	if !subj.MFAEnabled || len(subj.MFAMethods) == 0 {
		return stepup.EnrollmentRequired()
	}
	if v == nil {
		return &stepup.Error{
			Status: http.StatusInternalServerError,
			Body:   gin.H{"error": stepup.ErrMsgVerificationFailed},
			Cause:  errors.New("mfaenforce: MFA verifier is not configured"),
		}
	}
	return stepup.VerifyMFAFactorTx(ctx, tx, v, actorID, code, subj.MFAMethods)
}

// RequireConfirmationTx is the gate for a dangerous action: nil when the
// server does not enforce, without consulting the verifier, and ConfirmTx when
// it does. It must run after the caller's own permission check (see the
// package comment).
func RequireConfirmationTx(
	ctx context.Context, tx *sql.Tx, g Gate, v stepup.MFATxCodeVerifier, actorID, code string,
) *stepup.Error {
	if !g.Enforcing {
		return nil
	}
	return ConfirmTx(ctx, tx, g.Subject, v, actorID, code)
}

// IsLockConflict reports whether err carries PostgreSQL deadlock_detected
// (40P01) or lock_not_available (55P03) anywhere in its chain, including
// inside a *stepup.Error's Cause. Callers answer it with 503 and Retry-After.
// Under the lock order above it is a backstop that should not fire.
func IsLockConflict(err error) bool {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return false
	}
	return pqErr.Code == sqlStateDeadlockDetected || pqErr.Code == sqlStateLockNotAvailable
}
