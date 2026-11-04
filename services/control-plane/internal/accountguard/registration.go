package accountguard

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/email"
)

// The registration stamp's statements (#3680 PR3, Task 11; H3-b). Every one is parameterized.
//
// A credential made at registration has no earlier credential to be confused with, so it is
// mature from creation: '1970-01-01Z' is the legacy stamp 000189 gives every pre-existing row.
// Each slot's provenance pair is written in the same statement (H3, both-or-neither): enrolled_at
// is the transaction's now() and enrol_class is NULL, never 0, which the range CHECK refuses.
//
// The SET lists write no column the 000189 accountguard_material triggers watch (password_hash,
// email and the version columns on users; the identity and version columns on
// user_sso_identities), so neither trigger fires: the stamp stands, and no version moves. A
// write that changed one would draw a new version, and the row's email is never rewritten, in
// the caller's spelling or the normalized one. The email guard is therefore in WHERE only.
//
// Both statements match only a row THIS transaction inserted, proven by two predicates
// together. xmin is the id of the transaction that wrote the row's current version, and
// pg_current_xact_id() is this one's, so a row committed earlier, or written by another open
// transaction, matches nothing. pg_current_xact_id() is an xid8; the ::xid cast is the
// narrowing PostgreSQL defines for comparing it with xmin. But an UPDATE also gives the row's
// new version the updater's xid, so xmin alone matches an existing account this transaction
// merely touched. created_at = now() closes that: both created_at columns are timestamptz
// (users' since 000017, the identity's since 000061) defaulting to the transaction's start,
// which now() returns, and neither registration caller sets created_at, so a row this
// transaction inserted carries exactly now(), and an older row, updated or not, does not.
// Both sides are timestamptz, so the comparison is between two instants and the session's
// TimeZone cannot move either one. Either predicate failing is ErrContract.
const (
	// stampUserSQL stamps both credential slots of the user. The guard ties the row to the
	// address the lock was taken on: $2 is already email.NormalizeAddress's fold, and
	// lower(btrim(...)) is the address id the lock derives.
	stampUserSQL = `UPDATE users ` +
		`SET password_matures_at = '1970-01-01Z', email_matures_at = '1970-01-01Z', ` + // pragma: allowlist secret
		`password_enrolled_at = now(), email_enrolled_at = now(), ` +
		`password_enrol_class = NULL, email_enrol_class = NULL ` + // pragma: allowlist secret
		`WHERE id = $1 AND lower(btrim(email)) = lower(btrim($2::text)) ` +
		`AND xmin = pg_current_xact_id()::xid AND created_at = now()`
	// stampSSOIdentitySQL stamps one SSO identity, which must be the user's own.
	stampSSOIdentitySQL = `UPDATE user_sso_identities ` +
		`SET matures_at = '1970-01-01Z', enrolled_at = now(), enrol_class = NULL ` +
		`WHERE id = $1 AND user_id = $2 AND xmin = pg_current_xact_id()::xid AND created_at = now()`
)

// EnrolAtRegistrationTx stamps a new account's credentials mature from creation. The caller
// runs it in the transaction that INSERTed the users row (and, for an SSO registration, the
// identity), after that INSERT: auth/pending.go and oauth/handler.go, wired in PR6. addr is the
// address the row was inserted with, in any spelling the application's normalizer
// (email.NormalizeAddress) folds to it; ssoIdentityID is the identity inserted beside it, or nil.
//
// Fresh rows only. It matures a row only if this transaction inserted it (the xmin and
// created_at predicates on both statements), so it cannot zero the maturity window of an
// existing account's password, email or SSO identity, even one this transaction updated: an
// existing, committed, or other-transaction row is ErrContract and stays as it was. A row
// inserted under a savepoint or subtransaction carries that subtransaction's id, not this
// transaction's, so it does not match and fails closed; neither caller uses a savepoint today,
// and a caller that adds one must INSERT outside it. A caller must also leave created_at to its
// default: a row inserted with an explicit created_at fails closed the same way.
//
// The order is the marker, then the address lock, then the writes. The marker is read FOR SHARE
// (a set marker is ErrActivated and nothing is written) before any address lock is taken. The
// caller already holds the users row from its INSERT, so the order matches the runner's, which
// takes the users authority lock, then the marker, then the address locks. A registration and a
// runner pass that touch one address take turns on the address lock, and the lock provably
// covers the row it stamps: the users write must match the id and the same normalized address,
// and the identity write the id and the user, or the stamp is ErrContract. The error names no id
// and no address.
//
// Any error leaves the caller's transaction for rollback only. The users stamp runs before the
// identity stamp, so a failure at the identity leaves the users row already stamped in the
// transaction, and a driver error leaves PostgreSQL's transaction aborted: the caller must roll
// back and must not commit, log-and-continue, or retry in it.
//
// It claims no loginfamily authority (R5g): the row is the caller's own, just inserted, and no
// session exists to guard. It runs no sourceTx and sets no lock_timeout, because the caller
// owns the transaction and its timeouts.
func EnrolAtRegistrationTx(ctx context.Context, tx *sql.Tx, userID uuid.UUID, addr string, ssoIdentityID *uuid.UUID) error {
	if err := refuseActivatedTx(ctx, tx); err != nil {
		return err
	}
	address := email.NormalizeAddress(addr)
	if err := lockAddressTx(ctx, tx, address); err != nil {
		return err
	}
	if err := execOne(ctx, tx, "stamp credentials", stampUserSQL, userID, address); err != nil {
		return err
	}
	if ssoIdentityID == nil {
		return nil
	}
	return execOne(ctx, tx, "stamp sso identity", stampSSOIdentitySQL, *ssoIdentityID, userID)
}

// execOne runs a write that must touch exactly one row. Any other count is ErrContract: the
// caller named a row that is not its own, not this transaction's insert, or one whose address
// the lock did not cover. The caller must not commit. step is a fixed name; the error carries a
// count and nothing else.
func execOne(ctx context.Context, tx *sql.Tx, step, query string, args ...any) error {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", step, withhold(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", step, withhold(err))
	}
	if n != 1 {
		return fmt.Errorf("%s: %w: wrote %d rows, want 1", step, ErrContract, n)
	}
	return nil
}
