package accountguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

// ErrStale means a write found fewer rows than the snapshot promised: a request no longer in
// the state the effect moves it from, a credential at another version or of another account,
// a reservation that is gone. The outcome was decided over a snapshot that no longer holds,
// so it is refused whole.
var ErrStale = errors.New("accountguard: stale write")

// The appliers' invariants (#3680 PR3, ruling 7):
//   - Every write is scoped to applyCtx.userID. A request, a factor row or a reservation of
//     another account matches nothing, and is ErrStale.
//   - Every request transition names the statuses it moves from, so a terminal row is never
//     rewritten, and a Complete also names the request kind it was keyed by.
//   - Every write checks its row count against an exact expected count (execRows).
//   - A password hash reaches SQL only as a parameter or a column, and never leaves in an
//     error: a driver error is reduced to its SQLSTATE and constraint (redactDBError), because
//     the server's DETAIL quotes the failing row.
//   - A password is stored or installed only as an argon2id hash (checkPasswordHash).
//   - The password is written only by setPasswordTx, which counts its writes for finish.

// The writes. Every one is parameterized; $1 is the row's own id and $2 the account, except
// where a comment says otherwise.
const (
	// openSQL inserts the request an EffectOpen opens. held_ms, held_since, the cancel
	// columns and pending_address keep their defaults.
	openSQL = `INSERT INTO account_pending_requests (
			id, user_id, kind, status, target_kind, target_id, target_material_version,
			reset_factors, origin_class, origin_fingerprint, request_fingerprint,
			coarse_fingerprint, fingerprint_key_id, dispute_group_id, opened_at, completes_at,
			disputed_at, completed_at, pending_password_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`

	// openOriginsSQL inserts one origin row per ref, from three parallel arrays.
	openOriginsSQL = `INSERT INTO account_pending_request_origins
			(request_id, credential_kind, credential_id, material_version)
		SELECT $1, o.kind, o.id, o.version
		FROM unnest($2::text[], $3::text[], $4::bigint[]) AS o(kind, id, version)`

	// terminalClearSQL clears what a terminal row may not keep: both payloads and an open hold
	// (apr_terminal_payload). Cancel and Complete share it.
	terminalClearSQL = `pending_password_hash = NULL, pending_address = NULL, held_since = NULL` // pragma: allowlist secret

	// cancelSQL cancels an open request at $3 with reason $4 and canceller class $5. It clears
	// both payloads and the open hold (apr_terminal_payload) and keeps held_ms. The
	// fingerprints and their key id survive only a proof or dispute_resolved cancel, the two
	// the reopen lookup reads (apr_fingerprint_retention).
	cancelSQL = `UPDATE account_pending_requests SET
			status = 'cancelled', cancelled_at = $3, cancel_reason = $4::text, canceller_class = $5,
			` + terminalClearSQL + `,
			request_fingerprint = CASE WHEN $4::text IN ('proof', 'dispute_resolved') THEN request_fingerprint END,
			coarse_fingerprint = CASE WHEN $4::text IN ('proof', 'dispute_resolved') THEN coarse_fingerprint END,
			fingerprint_key_id = CASE WHEN $4::text IN ('proof', 'dispute_resolved') THEN fingerprint_key_id END
		WHERE id = $1 AND user_id = $2 AND status IN ('pending', 'disputed')`

	// disputeSQL puts an open request in dispute group $4. A pending row becomes disputed at
	// $3; a row already disputed keeps its status and its disputed_at, and only its group
	// changes (decide/doc.go "Disputes"). SET reads the row as it was before the UPDATE.
	disputeSQL = `UPDATE account_pending_requests SET
			status = 'disputed',
			disputed_at = CASE WHEN status = 'disputed' THEN disputed_at ELSE $3 END,
			dispute_group_id = $4
		WHERE id = $1 AND user_id = $2 AND status IN ('pending', 'disputed')`

	// returnToPendingSQL moves a disputed request back to pending, out of its group (ruling 12).
	returnToPendingSQL = `UPDATE account_pending_requests SET
			status = 'pending', disputed_at = NULL, dispute_group_id = NULL
		WHERE id = $1 AND user_id = $2 AND status = 'disputed'`

	// holdSQL sets the settled held time to $3 and the open hold to $4: an instant opens a
	// hold, NULL folds one (decide/doc.go "Holds").
	holdSQL = `UPDATE account_pending_requests SET held_ms = $3, held_since = $4
		WHERE id = $1 AND user_id = $2 AND status IN ('pending', 'disputed')`

	// completeSQL completes a pending request of kind $4 at $3. A terminal row keeps neither
	// payload nor an open hold (apr_terminal_payload), and a completed one no fingerprint
	// (apr_fingerprint_retention); the key id goes with the fingerprint (apr_key_id_pairs).
	completeSQL = `UPDATE account_pending_requests SET
			status = 'completed', completed_at = $3,
			` + terminalClearSQL + `,
			request_fingerprint = NULL, coarse_fingerprint = NULL, fingerprint_key_id = NULL
		WHERE id = $1 AND user_id = $2 AND kind = $4 AND status = 'pending'`

	// pendingHashSQL reads a pending break-glass's new password under the row lock the runner
	// already holds; FOR UPDATE takes it again when no runner did.
	pendingHashSQL = `SELECT pending_password_hash FROM account_pending_requests
		WHERE id = $1 AND user_id = $2 AND kind = 'break_glass' AND status = 'pending'
		FOR UPDATE`

	// releaseReservationSQL releases the account's reservation of address $1; $2 is the account.
	releaseReservationSQL = `DELETE FROM account_address_reservations WHERE address = $1 AND user_id = $2`

	// retireHashSQL keeps the stored password in the history, retired at $2. $1 is the
	// account. The hash moves from column to column and never through Go. was_mature is
	// decide's counts() rule on the old maturity: strictly before now. The row lock is the one
	// the runner's authority step holds; it is taken again when no runner did.
	retireHashSQL = `INSERT INTO user_credential_history (user_id, kind, password_hash, retired_at, was_mature)
		SELECT id, 'password', password_hash, $2, password_matures_at < $2
		FROM users WHERE id = $1
		FOR NO KEY UPDATE`

	// writeHashSQL is the F2 statement: the hash $2, its enrolled_at $3 and its enrol class $4
	// in ONE UPDATE, so the 000189 trigger keeps the provenance it was given. $1 is the
	// account. It returns the class the row holds after the trigger.
	writeHashSQL = `UPDATE users SET password_hash = $2, password_enrolled_at = $3, password_enrol_class = $4
		WHERE id = $1
		RETURNING password_enrol_class`

	// countRetiredSQL and trimRetiredSQL keep decide.HistoryMaxPasswords ($2) retired
	// passwords per account ($1), removing the earliest retired first. Addresses and other
	// accounts are never touched.
	countRetiredSQL = `SELECT count(*) FROM user_credential_history WHERE user_id = $1 AND kind = 'password'`
	trimRetiredSQL  = `DELETE FROM user_credential_history WHERE id IN (
			SELECT id FROM user_credential_history
			WHERE user_id = $1 AND kind = 'password'
			ORDER BY retired_at DESC, id DESC
			OFFSET $2)`
)

// factorWrite is the Remove and the Contest of one factor table. In both, $1 is the
// credential's ID (the row's own id for WebAuthn and SSO, user_id for the two tables keyed by
// user, so there the two predicates name one column), $2 the account and $3 the version the
// snapshot read: the version fence. A row at another version is not the credential decide
// decided over, so it is ErrStale and the outcome is refused whole.
type factorWrite struct {
	name    string
	remove  string
	contest string
}

// factorWrites are the factor tables PR3 wires (ruling 6). Contest stamps $4 only when the
// row is uncontested and still matches the fence, so a second contest of a contested row at
// its version is a no-op of one row, and a missing or stale row is ErrStale (ruling 12).
var factorWrites = map[decide.CredKind]factorWrite{
	decide.CredTOTP: {
		name:   "totp",
		remove: `DELETE FROM user_mfa_totp WHERE user_id = $1 AND user_id = $2 AND material_version = $3`,
		contest: `UPDATE user_mfa_totp SET maturity_contested_at = COALESCE(maturity_contested_at, $4)
			WHERE user_id = $1 AND user_id = $2 AND material_version = $3`,
	},
	decide.CredWebAuthn: {
		name:   "webauthn",
		remove: `DELETE FROM user_mfa_webauthn WHERE id = $1 AND user_id = $2 AND material_version = $3`,
		contest: `UPDATE user_mfa_webauthn SET maturity_contested_at = COALESCE(maturity_contested_at, $4)
			WHERE id = $1 AND user_id = $2 AND material_version = $3`,
	},
	decide.CredSSO: {
		name:   "sso",
		remove: `DELETE FROM user_sso_identities WHERE id = $1 AND user_id = $2 AND material_version = $3`,
		contest: `UPDATE user_sso_identities SET maturity_contested_at = COALESCE(maturity_contested_at, $4)
			WHERE id = $1 AND user_id = $2 AND material_version = $3`,
	},
	decide.CredRecoveryKey: {
		name:   "recovery key",
		remove: `DELETE FROM user_recovery_keys WHERE user_id = $1 AND user_id = $2 AND material_version = $3`,
		contest: `UPDATE user_recovery_keys SET maturity_contested_at = COALESCE(maturity_contested_at, $4)
			WHERE user_id = $1 AND user_id = $2 AND material_version = $3`,
	},
}

// applyOpen inserts the request e.Open describes, under the id the caller minted for this
// effect's index, and one origin row per ref. A multi-target opening is refused before any
// write: account_pending_requests holds one target. A break_glass carries
// applyCtx.pendingPasswordHash; one opened pending or disputed stores it, and one opened
// completed (T6c) stores NULL, as a terminal row must, and writes the password at once.
func applyOpen(ctx context.Context, tx *sql.Tx, a applyCtx, e decide.Effect) error {
	o := e.Open
	if len(o.Targets) > 1 {
		return fmt.Errorf("open: %w: %d targets", ErrContract, len(o.Targets))
	}
	id, ok := a.requestIDs[a.index]
	if !ok || id == uuid.Nil {
		return fmt.Errorf("open: %w: no request id from the caller", ErrContract)
	}
	args, err := openArgs(a, id, o, e.Group)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	if err := execRows(ctx, tx, 1, openSQL, args...); err != nil {
		return fmt.Errorf("open: request: %w", err)
	}
	if err := insertOrigins(ctx, tx, id, o.Origins); err != nil {
		return fmt.Errorf("open: origins: %w", err)
	}
	if o.Status == decide.StatusCompleted {
		if err := setPasswordTx(ctx, tx, a, a.pendingPasswordHash); err != nil {
			return fmt.Errorf("open: %w", err)
		}
	}
	return nil
}

// openArgs returns openSQL's arguments for o, refusing an opening the schema or the runner
// cannot honour before anything is written.
func openArgs(a applyCtx, id uuid.UUID, o decide.Opening, group string) ([]any, error) {
	kind, err := spell("request kind", o.Kind)
	if err != nil {
		return nil, err
	}
	status, err := spell("request status", o.Status)
	if err != nil {
		return nil, err
	}
	if err := checkOpening(o); err != nil {
		return nil, err
	}
	pendingHash, err := pendingHashArg(a, o)
	if err != nil {
		return nil, err
	}
	var disputeGroup any
	if o.Status == decide.StatusDisputed {
		g, err := groupFor(a, group)
		if err != nil {
			return nil, err
		}
		disputeGroup = g
	}
	var targetKind, targetID, targetVersion any
	if len(o.Targets) == 1 {
		t := o.Targets[0]
		if targetKind, err = spell("target kind", t.Kind); err != nil {
			return nil, err
		}
		targetID, targetVersion = t.ID, t.Version
	}
	fp, err := OriginFingerprint(o.Origins)
	if err != nil {
		return nil, err
	}
	// A keyed fingerprint always names its key, and only a keyed one does (apr_key_id_pairs).
	var keyID any
	if len(o.Candidate.Keyed) > 0 {
		keyID = o.Candidate.KeyID
	}
	return []any{
		id, a.userID, kind, status, targetKind, targetID, targetVersion,
		o.ResetFactors, int16(o.OriginClass), fp[:], nullBytes(o.Candidate.Keyed),
		nullBytes(o.Candidate.Coarse), keyID, disputeGroup, o.OpenedAt, o.CompletesAt,
		nullTime(o.DisputedAt), nullTime(o.CompletedAt), pendingHash,
	}, nil
}

// checkOpening refuses an opening of a status no opening has, one opened completed that is
// not a break-glass, one with no opened_at or completes_at, no origin class, or no origins.
// A request with no origins has nothing that proved it and an origin_fingerprint over
// nothing (ruling 19).
func checkOpening(o decide.Opening) error {
	switch o.Status {
	case decide.StatusPending, decide.StatusDisputed:
	case decide.StatusCompleted:
		if o.Kind != decide.ReqBreakGlass {
			return fmt.Errorf("%w: only a break-glass opens completed", ErrContract)
		}
	default:
		return fmt.Errorf("%w: an opening is pending, disputed or completed", ErrContract)
	}
	if o.OpenedAt.IsZero() || o.CompletesAt.IsZero() {
		// A zero completes_at would be due at once.
		return fmt.Errorf("%w: no opened_at or completes_at", ErrContract)
	}
	if o.OriginClass < decide.C1 || o.OriginClass > decide.C3 {
		return fmt.Errorf("%w: no origin class", ErrContract)
	}
	if len(o.Origins) == 0 {
		return fmt.Errorf("%w: an opening has no origins", ErrContract)
	}
	return nil
}

// pendingHashArg is the pending_password_hash an opening stores. Only a break-glass carries a
// new password, and it must carry an argon2id one whatever its status. One opened pending or
// disputed stores it; one opened completed (T6c) stores NULL, as a terminal row must, because
// applyOpen installs it at once. Every other kind stores NULL.
func pendingHashArg(a applyCtx, o decide.Opening) (any, error) {
	if o.Kind != decide.ReqBreakGlass {
		return nil, nil
	}
	if err := checkPasswordHash(a.pendingPasswordHash); err != nil {
		return nil, fmt.Errorf("break-glass pending password: %w", err)
	}
	if o.Status == decide.StatusCompleted {
		return nil, nil
	}
	return a.pendingPasswordHash, nil
}

// insertOrigins inserts one origin row per ref in one statement, and requires one row each.
// openArgs has already refused an opening with no origins.
func insertOrigins(ctx context.Context, tx *sql.Tx, id uuid.UUID, origins []decide.CredRef) error {
	kinds := make([]string, len(origins))
	ids := make([]string, len(origins))
	versions := make([]int64, len(origins))
	for i, ref := range origins {
		kind, err := spell("origin kind", ref.Kind)
		if err != nil {
			return err
		}
		kinds[i], ids[i], versions[i] = kind, ref.ID, ref.Version
	}
	return execRows(ctx, tx, int64(len(origins)), openOriginsSQL, id, pq.Array(kinds), pq.Array(ids), pq.Array(versions))
}

// applyCancel cancels the request e names with e.Reason and e.CancellerClass. A cancel
// without a reason is refused: the row's reason is what makes it cancelled.
func applyCancel(ctx context.Context, tx *sql.Tx, a applyCtx, e decide.Effect) error {
	id, err := canonicalUUID(e.RequestID, whatRequestID)
	if err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	if e.Reason == decide.CancelNone {
		return fmt.Errorf("cancel: %w: no reason", ErrContract)
	}
	reason, err := spell("cancel reason", e.Reason)
	if err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	if err := execRows(ctx, tx, 1, cancelSQL, id, a.userID, a.now, reason, nullClass(e.CancellerClass)); err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	return nil
}

// applyDispute puts the request e names in e.Group, or in the outcome's new group.
func applyDispute(ctx context.Context, tx *sql.Tx, a applyCtx, e decide.Effect) error {
	id, err := canonicalUUID(e.RequestID, whatRequestID)
	if err != nil {
		return fmt.Errorf("dispute: %w", err)
	}
	group, err := groupFor(a, e.Group)
	if err != nil {
		return fmt.Errorf("dispute: %w", err)
	}
	if err := execRows(ctx, tx, 1, disputeSQL, id, a.userID, a.now, group); err != nil {
		return fmt.Errorf("dispute: %w", err)
	}
	return nil
}

// applyReturnToPending moves the disputed request e names back to pending.
func applyReturnToPending(ctx context.Context, tx *sql.Tx, a applyCtx, e decide.Effect) error {
	id, err := canonicalUUID(e.RequestID, whatRequestID)
	if err != nil {
		return fmt.Errorf("return to pending: %w", err)
	}
	if err := execRows(ctx, tx, 1, returnToPendingSQL, id, a.userID); err != nil {
		return fmt.Errorf("return to pending: %w", err)
	}
	return nil
}

// applyHold writes the hold accounting of the request e names: e.HeldMs, and an open hold
// from e.HeldSince, or none when it is zero.
func applyHold(ctx context.Context, tx *sql.Tx, a applyCtx, e decide.Effect) error {
	id, err := canonicalUUID(e.RequestID, whatRequestID)
	if err != nil {
		return fmt.Errorf("hold: %w", err)
	}
	if err := execRows(ctx, tx, 1, holdSQL, id, a.userID, e.HeldMs, nullTime(e.HeldSince)); err != nil {
		return fmt.Errorf("hold: %w", err)
	}
	return nil
}

// applyCompleteCredentialRemove completes a pending credential_remove. It removes nothing:
// decide emits the removal as its own EffectRemove (ruling 1), and a second one would find
// no row.
func applyCompleteCredentialRemove(ctx context.Context, tx *sql.Tx, a applyCtx, e decide.Effect) error {
	if err := completeRequest(ctx, tx, a, e.RequestID, decide.ReqCredentialRemove); err != nil {
		return fmt.Errorf("complete credential removal: %w", err)
	}
	return nil
}

// applyCompleteBreakGlass completes a pending break-glass: it reads the row's own pending
// password (never applyCtx's, which belongs to an Open), writes it through setPasswordTx, and
// only then completes the row, clearing the payload. Its removals and its restored primary
// arrive as their own effects (ruling 1).
func applyCompleteBreakGlass(ctx context.Context, tx *sql.Tx, a applyCtx, e decide.Effect) error {
	id, err := canonicalUUID(e.RequestID, whatRequestID)
	if err != nil {
		return fmt.Errorf("complete break-glass: %w", err)
	}
	var pending sql.NullString
	if err := tx.QueryRowContext(ctx, pendingHashSQL, id, a.userID).Scan(&pending); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("complete break-glass: %w", ErrStale)
		}
		return fmt.Errorf("complete break-glass: read: %w", redactDBError(err))
	}
	if !pending.Valid || pending.String == "" {
		return fmt.Errorf("complete break-glass: %w: no pending password", ErrContract)
	}
	if err := setPasswordTx(ctx, tx, a, pending.String); err != nil {
		return fmt.Errorf("complete break-glass: %w", err)
	}
	if err := completeRequest(ctx, tx, a, e.RequestID, decide.ReqBreakGlass); err != nil {
		return fmt.Errorf("complete break-glass: %w", err)
	}
	return nil
}

// completeRequest completes the pending request named requestID, which must be of kind.
func completeRequest(ctx context.Context, tx *sql.Tx, a applyCtx, requestID string, kind decide.ReqKind) error {
	id, err := canonicalUUID(requestID, whatRequestID)
	if err != nil {
		return err
	}
	text, err := spell("request kind", kind)
	if err != nil {
		return err
	}
	return execRows(ctx, tx, 1, completeSQL, id, a.userID, a.now, text)
}

// applyReleaseReservation releases the account's reservation of the address e.Cred names.
func applyReleaseReservation(ctx context.Context, tx *sql.Tx, a applyCtx, e decide.Effect) error {
	if e.Cred.Kind != decide.CredAddress || e.Cred.ID == "" {
		return fmt.Errorf("release reservation: %w: not a primary address", ErrContract)
	}
	if err := execRows(ctx, tx, 1, releaseReservationSQL, e.Cred.ID, a.userID); err != nil {
		return fmt.Errorf("release reservation: %w", err)
	}
	return nil
}

// removeApplier deletes the credential e.Cred names, at its version, from f's table.
func removeApplier(f factorWrite) func(context.Context, *sql.Tx, applyCtx, decide.Effect) error {
	return func(ctx context.Context, tx *sql.Tx, a applyCtx, e decide.Effect) error {
		id, err := canonicalUUID(e.Cred.ID, "credential id")
		if err != nil {
			return fmt.Errorf("remove %s: %w", f.name, err)
		}
		if err := execRows(ctx, tx, 1, f.remove, id, a.userID, e.Cred.Version); err != nil {
			return fmt.Errorf("remove %s: %w", f.name, err)
		}
		return nil
	}
}

// contestApplier marks the credential e.Cred names, at its version, contested now.
func contestApplier(f factorWrite) func(context.Context, *sql.Tx, applyCtx, decide.Effect) error {
	return func(ctx context.Context, tx *sql.Tx, a applyCtx, e decide.Effect) error {
		id, err := canonicalUUID(e.Cred.ID, "credential id")
		if err != nil {
			return fmt.Errorf("contest %s: %w", f.name, err)
		}
		if err := execRows(ctx, tx, 1, f.contest, id, a.userID, e.Cred.Version, a.now); err != nil {
			return fmt.Errorf("contest %s: %w", f.name, err)
		}
		return nil
	}
}

// setPasswordTx is the one password writer (ruling 10), reached from a break-glass Complete
// and from a break-glass Open written completed (T6c). It retires the stored password into
// the history, writes hash with enrolled_at = now and enrol_class = Outcome.EnrolClass in one
// UPDATE (F2), trims the history, and counts the write for finish. A class outside C1 to C3
// is refused rather than written as NULL: an unknown class reads at the rank floor, where a
// lone mailbox could contest the fresh password. The class the row holds after the 000189
// trigger is read back, because the trigger resets it when the write looks like no
// provenance statement (an enrolled_at equal to the stored one). A hash that is not argon2id
// is refused before anything is written. No error carries the hash.
func setPasswordTx(ctx context.Context, tx *sql.Tx, a applyCtx, hash string) error {
	writes := a.passwordWrites
	if writes == nil {
		return fmt.Errorf("set password: %w: no password-write counter", ErrContract)
	}
	if err := checkPasswordHash(hash); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	class := a.outcome.EnrolClass
	if class < decide.C1 || class > decide.C3 {
		return fmt.Errorf("set password: %w: no enrol class", ErrContract)
	}
	if err := execRows(ctx, tx, 1, retireHashSQL, a.userID, a.now); err != nil {
		return fmt.Errorf("set password: retire: %w", err)
	}
	var held sql.NullInt16
	if err := tx.QueryRowContext(ctx, writeHashSQL, a.userID, hash, a.now, int16(class)).Scan(&held); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("set password: write: %w", ErrStale)
		}
		return fmt.Errorf("set password: write: %w", redactDBError(err))
	}
	if !held.Valid || held.Int16 != int16(class) {
		return fmt.Errorf("set password: write: %w: the enrol class did not hold", ErrContract)
	}
	if err := trimRetired(ctx, tx, a.userID); err != nil {
		return fmt.Errorf("set password: trim history: %w", err)
	}
	*writes++
	return nil
}

// trimRetired removes the account's retired passwords beyond decide.HistoryMaxPasswords,
// and requires the delete to remove exactly the excess it counted.
func trimRetired(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error {
	var n int64
	if err := tx.QueryRowContext(ctx, countRetiredSQL, userID).Scan(&n); err != nil {
		return fmt.Errorf("count: %w", redactDBError(err))
	}
	excess := n - decide.HistoryMaxPasswords
	if excess <= 0 {
		return nil
	}
	return execRows(ctx, tx, excess, trimRetiredSQL, userID, decide.HistoryMaxPasswords)
}

// execRows runs a write that must touch exactly want rows. Fewer is ErrStale: the snapshot
// promised rows that are no longer there. More is ErrContract and never ErrStale, because a
// predicate that matched more than it named is a defect, not a race (ruling 17). Either way
// the caller must not commit.
func execRows(ctx context.Context, tx *sql.Tx, want int64, query string, args ...any) error {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return redactDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", redactDBError(err))
	}
	switch {
	case n < want:
		return fmt.Errorf("%w: %d of %d rows", ErrStale, n, want)
	case n > want:
		return fmt.Errorf("%w: wrote %d rows, want %d", ErrContract, n, want)
	}
	return nil
}

// argon2idPrefix opens every password hash this service makes (internal/auth encodes
// "$argon2id$v=19$m=...,t=...,p=...$salt$hash").
const argon2idPrefix = "$argon2id$"

// checkPasswordHash refuses a password the runner would store or install unless it is an
// argon2id hash (ruling 20). Anything else is not a hash this service made: installed, it
// locks the owner out of a password they chose, and if it is plaintext it sits in the clear.
// The error names the fault and never carries the value.
func checkPasswordHash(hash string) error {
	if hash == "" {
		return fmt.Errorf("%w: no password", ErrContract)
	}
	if !strings.HasPrefix(hash, argon2idPrefix) {
		return fmt.Errorf("%w: the password is not an argon2id hash", ErrContract)
	}
	return nil
}

// dbFault stands in for a driver error from an applier's statement. For a constraint failure
// the server's DETAIL quotes the whole failing row, and the rows these statements write carry
// password_hash and pending_password_hash, so only the SQLSTATE and the constraint's name are
// kept and the driver's error is not wrapped.
type dbFault struct {
	code       pqerror.Code
	constraint string
}

func (f *dbFault) Error() string {
	if f.constraint == "" {
		return "database error " + string(f.code)
	}
	return "database error " + string(f.code) + " on " + f.constraint
}

// redactDBError reduces a driver error to a dbFault. An error that is not the server's (a
// cancelled context, a closed connection) carries no row and is returned as it is.
func redactDBError(err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return &dbFault{code: pqErr.Code, constraint: pqErr.Constraint}
	}
	return err
}

// whatRequestID names a request id in canonicalUUID's errors.
const whatRequestID = "request id"

// canonicalUUID parses an id an effect carries. It must be canonical uuid text, which is what
// the snapshot hands decide; anything else names no row the snapshot read.
func canonicalUUID(s, what string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil || id.String() != s {
		return uuid.Nil, fmt.Errorf("%w: %s is not a canonical uuid", ErrContract, what)
	}
	return id, nil
}

// groupFor is the dispute group an effect writes: its own Group, or the outcome's one new
// group when Group is "" (decide/doc.go "New IDs").
func groupFor(a applyCtx, group string) (uuid.UUID, error) {
	if group != "" {
		return canonicalUUID(group, "dispute group")
	}
	if a.newGroupID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: no new dispute group", ErrContract)
	}
	return a.newGroupID, nil
}

// spell is the database spelling of an enum value; a value with none is a contract error.
func spell(what string, m interface{ MarshalText() ([]byte, error) }) (string, error) {
	b, err := m.MarshalText()
	if err != nil {
		return "", fmt.Errorf("%w: %s has no spelling: %w", ErrContract, what, err)
	}
	return string(b), nil
}

// nullTime is t, or NULL for the zero time.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// nullBytes is b, or NULL when it is empty.
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// nullClass is c's column value, or NULL for decide.ClassNone.
func nullClass(c decide.Class) any {
	if c == decide.ClassNone {
		return nil
	}
	return int16(c)
}
