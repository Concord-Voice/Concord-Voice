package accountguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

// errEnrolClassRange means a class column holds a value outside 1 to 3. The 000189 and
// 000196 CHECK constraints refuse such a write, so this is a corrupted row, and it is
// refused rather than read as a class: a class above C3 would outrank every threshold.
var errEnrolClassRange = errors.New("class out of range")

// The reads of loadInputTx. Every one is parameterized. $1 is the account; $2, in the cancel
// read, is the start of the reopen window.
const (
	// requestColumns is the column list both request reads select, in the order
	// requestRow.scan reads it.
	requestColumns = `id, kind, status, target_kind, target_id, target_material_version,
		reset_factors, origin_class, request_fingerprint, coarse_fingerprint, fingerprint_key_id,
		dispute_group_id, opened_at, completes_at, held_ms, held_since, disputed_at, cancelled_at,
		cancel_reason, canceller_class`

	// accountSQL reads the users row without locking it: the runner's authority step holds
	// that lock. The password hash itself is never read, only whether one is stored. The
	// address is the normalized text a reservation carries (its CHECK is lower(btrim(...))).
	// A NULL email_verified (the column is nullable, default false) is an unverified address.
	accountSQL = `SELECT password_hash <> '', password_login_disabled,
			COALESCE(email_verified, false), lower(btrim(email)),
			password_matures_at, password_version, password_enrolled_at, password_enrol_class,
			email_matures_at, email_contested_at, email_version, email_enrolled_at, email_enrol_class
		FROM users WHERE id = $1`

	// lockRequestsSQL takes the account's open requests FOR UPDATE in id order: the first
	// locks of the runner's order, ahead of every factor row (R5).
	lockRequestsSQL = `SELECT ` + requestColumns + `
		FROM account_pending_requests
		WHERE user_id = $1 AND status IN ('pending', 'disputed')
		ORDER BY id
		FOR UPDATE`

	// originsSQL reads the origins of the requests just locked, and only those.
	originsSQL = `SELECT request_id, credential_kind, credential_id, material_version
		FROM account_pending_request_origins
		WHERE request_id = ANY($1::uuid[])`

	// recentCancelsSQL reads the proof and dispute_resolved cancels inside the reopen window.
	// The window is open at its start: a cancel exactly decide.NCancelCP old is outside it.
	// Nothing here is locked: a cancelled row is read, never decided over.
	recentCancelsSQL = `SELECT ` + requestColumns + `
		FROM account_pending_requests
		WHERE user_id = $1 AND status = 'cancelled'
		  AND cancel_reason IN ('proof', 'dispute_resolved') AND cancelled_at > $2
		ORDER BY id`

	reservationsSQL = `SELECT address, displaced_at, floor_until
		FROM account_address_reservations
		WHERE user_id = $1
		ORDER BY address`

	// The four factor reads, each FOR NO KEY UPDATE. The first column is the credential's ID:
	// the row's own id for WebAuthn and SSO, user_id for the two tables keyed by user. The last
	// says whether the row is a live credential: always true, except for a TOTP row that is not
	// both enabled and confirmed. That row is locked all the same (ruling 13), so a confirm,
	// which is an UPDATE of it, queues behind the loader; only the load is filtered, in Go.
	// WebAuthn and SSO hold several rows per account, and lock them in id order.
	lockTOTPSQL = `SELECT user_id, matures_at, maturity_contested_at, material_version,
			enrolled_at, enrol_class, enabled AND confirmed
		FROM user_mfa_totp
		WHERE user_id = $1
		FOR NO KEY UPDATE`
	lockWebAuthnSQL = `SELECT id, matures_at, maturity_contested_at, material_version,
			enrolled_at, enrol_class, true
		FROM user_mfa_webauthn
		WHERE user_id = $1
		ORDER BY id
		FOR NO KEY UPDATE`
	lockSSOSQL = `SELECT id, matures_at, maturity_contested_at, material_version,
			enrolled_at, enrol_class, true
		FROM user_sso_identities
		WHERE user_id = $1
		ORDER BY id
		FOR NO KEY UPDATE`
	lockRecoverySQL = `SELECT user_id, matures_at, maturity_contested_at, material_version,
			enrolled_at, enrol_class, true
		FROM user_recovery_keys
		WHERE user_id = $1
		FOR NO KEY UPDATE`
)

// guardFactor is one factor table loadInputTx reads and locks.
type guardFactor struct {
	name  string
	kind  decide.CredKind
	paper bool
	query string
}

// factorLockOrder is the order the factor tables are locked in, after the requests (R5):
// user_mfa_totp, user_mfa_webauthn, user_sso_identities, user_recovery_keys. A fixed order
// is what stops two runners that lock the same rows from waiting on each other. Only the
// recovery key is Paper (H1): it proves like a backup code. A TOTP row is locked in any
// state and loaded only once it is both enabled and confirmed.
var factorLockOrder = [...]guardFactor{
	{"totp", decide.CredTOTP, false, lockTOTPSQL},
	{"webauthn", decide.CredWebAuthn, false, lockWebAuthnSQL},
	{"sso", decide.CredSSO, false, lockSSOSQL},
	{"recovery key", decide.CredRecoveryKey, true, lockRecoverySQL},
}

// loadInputTx reads the snapshot decide.Decide runs over, under the runner's locks. The
// open requests are locked FOR UPDATE in id order, then the factor rows FOR NO KEY UPDATE in
// the fixed table order of factorLockOrder. The user's TOTP row is locked whatever its state
// and filtered to enabled-and-confirmed in Go, so a confirm queues behind the loader. The
// users row is read first and never locked here, because the runner's authority step holds
// that lock. A factor row INSERTed after this load is not excluded by these row locks (a row
// that does not exist yet cannot be locked), and the users lock need not block the INSERT
// either: a factor INSERT's foreign-key check takes only FOR KEY SHARE on its users row, which
// FOR NO KEY UPDATE does not conflict with. It is excluded by contract: every factor writer
// locks the users row before it writes a factor row, so it queues behind the runner's users
// lock (spec §10.1, PR6). The origins, cancels and reservations are read after the last lock
// is held.
//
// Act and Proof are the runner's to fill, and are left at their zero values. Input.Now is
// the now argument, and so is the start of the reopen window: neither reads a clock.
//
// A field whose zero is legitimate is always loaded (round-2 Q6): a NULL timestamp reads as
// the zero time, never as now, and a NULL enrol_class as decide.ClassNone (D20). The legacy
// 1970-01-01Z sentinel is a real instant and arrives as one.
//
// The load is all or nothing. Any failure returns the zero decide.Input and an error that
// names the step and wraps its cause, and carries no identifier, address or row content.
// A row naming a credential kind decide does not know is such a failure, never a skipped
// row.
func loadInputTx(ctx context.Context, tx *sql.Tx, userID uuid.UUID, keyID int16, now time.Time) (decide.Input, error) {
	acct, err := loadAccount(ctx, tx, userID)
	if err != nil {
		return decide.Input{}, fmt.Errorf("load input: account: %w", err)
	}
	reqs, err := lockOpenRequests(ctx, tx, userID)
	if err != nil {
		return decide.Input{}, fmt.Errorf("load input: lock requests: %w", err)
	}
	factors, err := lockFactors(ctx, tx, userID)
	if err != nil {
		return decide.Input{}, fmt.Errorf("load input: lock factors: %w", err)
	}
	if err := attachOrigins(ctx, tx, reqs); err != nil {
		return decide.Input{}, fmt.Errorf("load input: origins: %w", err)
	}
	// Truncate, never round: PostgreSQL rounds a finer parameter, and rounding up would drop a cancel inside the window.
	windowStart := now.Add(-decide.NCancelCP).Truncate(time.Microsecond)
	cancels, err := queryRows(ctx, tx, recentCancelsSQL, scanRequest, userID, windowStart)
	if err != nil {
		return decide.Input{}, fmt.Errorf("load input: recent cancels: %w", err)
	}
	reservations, err := queryRows(ctx, tx, reservationsSQL, scanReservation, userID)
	if err != nil {
		return decide.Input{}, fmt.Errorf("load input: reservations: %w", err)
	}

	for i := range reqs {
		// PR4 builds the budget and the outbox; until then a visible stub, not a silent zero (decide/doc.go H1).
		reqs[i].MatureNoticesSettled = true
		reqs[i].AcceptedAddressNotice = true
	}
	return decide.Input{
		Now:           now,
		Creds:         append(acct.creds, factors...),
		Primaries:     acct.primaries,
		Reservations:  reservations,
		Reqs:          reqs,
		RecentCancels: cancels,
		// PR4 builds the budget and the outbox; until then a visible stub, not a silent zero (decide/doc.go H1).
		GuessableBudgetClear: true,
		CurrentKeyID:         keyID,
		SSOLogin:             acct.ssoLogin,
	}, nil
}

// credProvenance is the 000189 maturity and enrolment columns of one credential.
type credProvenance struct {
	maturesAt  time.Time
	contested  sql.NullTime
	enrolledAt time.Time
	class      sql.NullInt16
}

// cred returns the decide.Cred for ref with this provenance.
func (p credProvenance) cred(ref decide.CredRef, paper bool) (decide.Cred, error) {
	class, err := classFromColumn(p.class)
	if err != nil {
		return decide.Cred{}, err
	}
	return decide.Cred{
		Ref:         ref,
		MaturesAt:   p.maturesAt,
		ContestedAt: timeFromColumn(p.contested),
		Paper:       paper,
		EnrolledAt:  p.enrolledAt,
		EnrolClass:  class,
	}, nil
}

// guardAccount is what the users row yields: the password and address credentials, the one
// current primary, and the login mode.
type guardAccount struct {
	creds     []decide.Cred
	primaries []decide.Primary
	ssoLogin  bool
}

// loadAccount reads the users row. The password credential is loaded whenever a hash is
// stored, because an SSO account stores a real hash and the login mode is carried apart, by
// password_login_disabled. The address credential, and with it the single current primary
// (BecamePrimaryAt is email_enrolled_at, DisplacedAt is zero), is loaded only for a verified
// address. Its ID is the normalized address text, the identity a reservation carries too.
func loadAccount(ctx context.Context, tx *sql.Tx, userID uuid.UUID) (guardAccount, error) {
	var (
		hasPassword, ssoLogin, verified bool
		address                         string
		password, email                 credProvenance
		passwordVersion, emailVersion   int64
	)
	if err := tx.QueryRowContext(ctx, accountSQL, userID).Scan(
		&hasPassword, &ssoLogin, &verified, &address,
		&password.maturesAt, &passwordVersion, &password.enrolledAt, &password.class,
		&email.maturesAt, &email.contested, &emailVersion, &email.enrolledAt, &email.class,
	); err != nil {
		return guardAccount{}, fmt.Errorf("read: %w", err)
	}

	acct := guardAccount{ssoLogin: ssoLogin}
	if hasPassword {
		cred, err := password.cred(decide.CredRef{Kind: decide.CredPassword, ID: userID.String(), Version: passwordVersion}, false)
		if err != nil {
			return guardAccount{}, fmt.Errorf("password: %w", err)
		}
		acct.creds = append(acct.creds, cred)
	}
	if verified {
		cred, err := email.cred(decide.CredRef{Kind: decide.CredAddress, ID: address, Version: emailVersion}, false)
		if err != nil {
			return guardAccount{}, fmt.Errorf("address: %w", err)
		}
		acct.creds = append(acct.creds, cred)
		acct.primaries = []decide.Primary{{
			Ref:             cred.Ref,
			BecamePrimaryAt: cred.EnrolledAt,
			MaturesAt:       cred.MaturesAt,
			ContestedAt:     cred.ContestedAt,
		}}
	}
	return acct, nil
}

// lockOpenRequests reads and locks the account's pending and disputed requests, in id order.
func lockOpenRequests(ctx context.Context, tx *sql.Tx, userID uuid.UUID) ([]decide.Req, error) {
	return queryRows(ctx, tx, lockRequestsSQL, scanRequest, userID)
}

// lockFactors reads and locks the account's factor rows table by table, in factorLockOrder.
func lockFactors(ctx context.Context, tx *sql.Tx, userID uuid.UUID) ([]decide.Cred, error) {
	var creds []decide.Cred
	for _, f := range factorLockOrder {
		got, err := queryRows(ctx, tx, f.query, func(rows *sql.Rows) (factorRow, error) {
			return scanFactor(rows, f)
		}, userID)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
		for _, row := range got {
			if row.live {
				creds = append(creds, row.cred)
			}
		}
	}
	return creds, nil
}

// factorRow is one row of a factor read: its credential, and whether it is a live one.
type factorRow struct {
	cred decide.Cred
	live bool
}

// scanFactor reads one row of a factor read. A row that is not live is returned as the zero
// factorRow: it was locked, and is not loaded.
func scanFactor(rows *sql.Rows, f guardFactor) (factorRow, error) {
	var (
		id      uuid.UUID
		version int64
		prov    credProvenance
		live    bool
	)
	if err := rows.Scan(&id, &prov.maturesAt, &prov.contested, &version, &prov.enrolledAt, &prov.class, &live); err != nil {
		return factorRow{}, fmt.Errorf("scan: %w", err)
	}
	if !live {
		return factorRow{}, nil
	}
	cred, err := prov.cred(decide.CredRef{Kind: f.kind, ID: id.String(), Version: version}, f.paper)
	if err != nil {
		return factorRow{}, err
	}
	return factorRow{cred: cred, live: true}, nil
}

// requestRow is one account_pending_requests row as the driver returns it.
type requestRow struct {
	id               uuid.UUID
	kind, status     string
	targetKind       sql.NullString
	targetID         sql.NullString
	targetVersion    sql.NullInt64
	resetFactors     bool
	originClass      sql.NullInt16
	fingerprint      []byte
	coarse           []byte
	fingerprintKeyID sql.NullInt16
	disputeGroup     uuid.NullUUID
	openedAt         time.Time
	completesAt      time.Time
	heldMs           int64
	heldSince        sql.NullTime
	disputedAt       sql.NullTime
	cancelledAt      sql.NullTime
	cancelReason     sql.NullString
	cancellerClass   sql.NullInt16
}

// scanRequest reads one row of requestColumns into a request. Origins are attached
// separately. A NULL timestamp is the zero time; a NULL cancel reason is decide.CancelNone.
func scanRequest(rows *sql.Rows) (decide.Req, error) {
	var r requestRow
	if err := rows.Scan(
		&r.id, &r.kind, &r.status, &r.targetKind, &r.targetID, &r.targetVersion,
		&r.resetFactors, &r.originClass, &r.fingerprint, &r.coarse, &r.fingerprintKeyID,
		&r.disputeGroup, &r.openedAt, &r.completesAt, &r.heldMs, &r.heldSince, &r.disputedAt,
		&r.cancelledAt, &r.cancelReason, &r.cancellerClass,
	); err != nil {
		return decide.Req{}, fmt.Errorf("scan: %w", err)
	}
	return r.req()
}

// req converts the row. Text that names no kind, status or reason decide knows is an error
// whose message carries none of the text.
func (r requestRow) req() (decide.Req, error) {
	req := decide.Req{
		ID:               r.id.String(),
		OpenedAt:         r.openedAt,
		CompletesAt:      r.completesAt,
		HeldMs:           r.heldMs,
		HeldSince:        timeFromColumn(r.heldSince),
		DisputedAt:       timeFromColumn(r.disputedAt),
		Fingerprint:      r.fingerprint,
		Coarse:           r.coarse,
		FingerprintKeyID: r.fingerprintKeyID.Int16,
		CancelledAt:      timeFromColumn(r.cancelledAt),
		ResetFactors:     r.resetFactors,
	}
	if r.disputeGroup.Valid {
		req.DisputeGroup = r.disputeGroup.UUID.String()
	}
	if err := req.Kind.UnmarshalText([]byte(r.kind)); err != nil {
		return decide.Req{}, fmt.Errorf("request kind: %w", err)
	}
	if err := req.Status.UnmarshalText([]byte(r.status)); err != nil {
		return decide.Req{}, fmt.Errorf("request status: %w", err)
	}
	// A NULL cancel reason reads as "", which is decide.CancelNone.
	if err := req.CancelReason.UnmarshalText([]byte(r.cancelReason.String)); err != nil {
		return decide.Req{}, fmt.Errorf("cancel reason: %w", err)
	}
	var err error
	if req.OriginClass, err = classFromColumn(r.originClass); err != nil {
		return decide.Req{}, fmt.Errorf("origin class: %w", err)
	}
	if req.CancellerClass, err = classFromColumn(r.cancellerClass); err != nil {
		return decide.Req{}, fmt.Errorf("canceller class: %w", err)
	}
	if req.Targets, err = r.targets(); err != nil {
		return decide.Req{}, fmt.Errorf("target: %w", err)
	}
	return req, nil
}

// targets returns the request's single target ref, or nil when it names none (a
// break-glass). A target with only some of its three columns set is an error: the schema
// refuses it, so it is a corrupted row, and guessing the rest would name a credential the
// request never targeted.
func (r requestRow) targets() ([]decide.CredRef, error) {
	if !r.targetKind.Valid && !r.targetID.Valid && !r.targetVersion.Valid {
		return nil, nil
	}
	if !r.targetKind.Valid || !r.targetID.Valid || !r.targetVersion.Valid {
		return nil, errors.New("incomplete target")
	}
	var kind decide.CredKind
	if err := kind.UnmarshalText([]byte(r.targetKind.String)); err != nil {
		return nil, err
	}
	return []decide.CredRef{{Kind: kind, ID: r.targetID.String, Version: r.targetVersion.Int64}}, nil
}

// originRow is one origin of one request.
type originRow struct {
	requestID string
	ref       decide.CredRef
}

func scanOrigin(rows *sql.Rows) (originRow, error) {
	var (
		requestID uuid.UUID
		kind, id  string
		version   int64
	)
	if err := rows.Scan(&requestID, &kind, &id, &version); err != nil {
		return originRow{}, fmt.Errorf("scan: %w", err)
	}
	ref := decide.CredRef{ID: id, Version: version}
	if err := ref.Kind.UnmarshalText([]byte(kind)); err != nil {
		return originRow{}, fmt.Errorf("origin kind: %w", err)
	}
	return originRow{requestID: requestID.String(), ref: ref}, nil
}

// attachOrigins sets each request's Origins to its own origins, in the one canonical order:
// kind by number, then ID byte for byte, then version (compareRefs, the comparator the
// fingerprints use). The read is bounded to the requests just locked.
func attachOrigins(ctx context.Context, tx *sql.Tx, reqs []decide.Req) error {
	if len(reqs) == 0 {
		return nil
	}
	ids := make([]string, len(reqs))
	for i, r := range reqs {
		ids[i] = r.ID
	}
	rows, err := queryRows(ctx, tx, originsSQL, scanOrigin, pq.Array(ids))
	if err != nil {
		return err
	}
	byRequest := make(map[string][]decide.CredRef, len(reqs))
	for _, o := range rows {
		byRequest[o.requestID] = append(byRequest[o.requestID], o.ref)
	}
	for i := range reqs {
		origins := byRequest[reqs[i].ID]
		slices.SortFunc(origins, compareRefs)
		reqs[i].Origins = origins
	}
	return nil
}

// scanReservation reads one reservation. Its AddressRef is the displaced primary's address
// at version 0: the table keeps no version, and none is invented.
func scanReservation(rows *sql.Rows) (decide.Reservation, error) {
	var (
		address                 string
		displacedAt, floorUntil time.Time
	)
	if err := rows.Scan(&address, &displacedAt, &floorUntil); err != nil {
		return decide.Reservation{}, fmt.Errorf("scan: %w", err)
	}
	return decide.Reservation{
		AddressRef:  decide.CredRef{Kind: decide.CredAddress, ID: address},
		DisplacedAt: displacedAt,
		FloorUntil:  floorUntil,
	}, nil
}

// queryRows runs one read and scans every row it returns. The rows are closed, and the close
// error checked, before it returns, so the next read on the same transaction finds a free
// connection. Any failure returns no rows.
func queryRows[T any](ctx context.Context, tx *sql.Tx, query string, scan func(*sql.Rows) (T, error), args ...any) (out []T, err error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		// A rows result that cannot be closed is not known to be fully read: refuse it, and
		// keep the close error beside an earlier scan error rather than dropping it.
		if closeErr := rows.Close(); closeErr != nil {
			out, err = nil, errors.Join(err, fmt.Errorf("close rows: %w", closeErr))
		}
	}()

	for rows.Next() {
		got, scanErr := scan(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, got)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read rows: %w", err)
	}
	return out, nil
}

// classFromColumn maps a smallint class column to its Class. NULL is decide.ClassNone
// (plan D20). A value outside 1 to 3 is errEnrolClassRange.
func classFromColumn(n sql.NullInt16) (decide.Class, error) {
	if !n.Valid {
		return decide.ClassNone, nil
	}
	switch n.Int16 {
	case int16(decide.C1):
		return decide.C1, nil
	case int16(decide.C2):
		return decide.C2, nil
	case int16(decide.C3):
		return decide.C3, nil
	}
	return decide.ClassNone, errEnrolClassRange
}

// timeFromColumn maps a nullable timestamp to a time.Time. NULL is the zero time, never now.
func timeFromColumn(n sql.NullTime) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return n.Time
}
