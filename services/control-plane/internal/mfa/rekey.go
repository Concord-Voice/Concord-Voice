package mfa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RekeyFailure records a row the backfill could not re-seal. The row is left
// untouched: restoring its sealing key recovers it, unless the row itself is
// damaged, in which case no key does.
type RekeyFailure struct {
	UserID        string
	SealedVersion int
	Err           error
}

// RekeyResult summarizes one Rekey pass.
type RekeyResult struct {
	Scanned int            // dry-run: rows pending; live: rows examined
	Rekeyed int            // rows re-sealed to the active version
	Skipped int            // rows skipped by the CAS guard (concurrently rewritten)
	Failed  []RekeyFailure // rows that could not be decrypted (left untouched)
}

// Rekey re-encrypts every user_mfa_totp row not sealed under ring's active
// version (#2307). Operational properties the rotation runbook depends on:
//   - one transaction per row: a crash mid-run leaves a decryptable mixed
//     state (the retired key is still in the ring) and a re-run converges;
//   - each row's transaction sets concord.credential_rewrap='mfa-rekey'
//     (transaction-local), so migration 000189's material trigger treats the
//     rewrap as no change of material (#3680 H1); no other code may set it.
//     A rewrap the trigger nonetheless treats as material is rolled back and
//     aborts the run, so the fallback is never silent;
//   - CAS tail (key_version + prior ciphertext + material_version) makes a
//     concurrent re-enrollment or confirmation a harmless skip — server writes
//     are always active-version;
//   - an aborted run still returns the partial result, the failed rows
//     included;
//   - keyset pagination on user_id advances past failed rows, so an
//     undecryptable row cannot loop the scan;
//   - dryRun reports the pending count and mutates nothing.
func Rekey(ctx context.Context, db *sql.DB, ring *Keyring, batchSize int, dryRun bool) (RekeyResult, error) {
	var res RekeyResult
	if batchSize < 1 {
		batchSize = 100
	}
	active := ring.ActiveVersion()

	if dryRun {
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM user_mfa_totp WHERE key_version <> $1`, active,
		).Scan(&res.Scanned); err != nil {
			return res, fmt.Errorf("count pending rows: %w", err)
		}
		return res, nil
	}

	// Keyset pagination on the native uuid PK. The zero uuid is the first-page
	// sentinel — every real gen_random_uuid() is strictly greater — so it never
	// matches a live row. Comparing/ordering by user_id (not user_id::text) lets
	// the primary-key index satisfy both the filter and the sort.
	cursor := zeroUUID
	for {
		batch, err := fetchPendingBatch(ctx, db, active, cursor, batchSize)
		if err != nil {
			return res, err
		}
		if len(batch) == 0 {
			return res, nil
		}
		cursor, err = applyBatch(ctx, db, ring, batch, &res)
		if err != nil {
			return res, err
		}
	}
}

// rekeyOutcome classifies what happened to a single row. The zero value is
// rekeyAborted, so an outcome left unset never counts as a re-sealed row.
type rekeyOutcome int

const (
	rekeyAborted   rekeyOutcome = iota // infrastructure failure; only ever paired with a non-nil error
	rekeyDone                          // re-sealed to the active version
	rekeySkipped                       // CAS guard matched 0 rows (concurrent rewrite)
	rekeyFailedRow                     // could not decrypt/re-seal; left untouched
)

// applyBatch re-keys every row in batch, tallying into res, and returns the last
// examined row's userID as the next keyset cursor. It advances the cursor for
// EVERY row (including failed ones) so the scan cannot loop on an undecryptable
// row. A returned error is an infrastructure failure that aborts the whole run.
func applyBatch(ctx context.Context, db *sql.DB, ring *Keyring, batch []pendingRow, res *RekeyResult) (string, error) {
	cursor := ""
	for _, r := range batch {
		cursor = r.userID
		res.Scanned++
		outcome, failure, err := rekeyRow(ctx, db, ring, r)
		if err != nil {
			return cursor, err
		}
		switch outcome {
		case rekeyDone:
			res.Rekeyed++
		case rekeySkipped:
			res.Skipped++
		case rekeyFailedRow:
			res.Failed = append(res.Failed, *failure)
		}
	}
	return cursor, nil
}

// rekeyRow re-seals one pending row to ring's active version. A decrypt/re-seal
// failure returns rekeyFailedRow + a *RekeyFailure and leaves the row untouched
// (nil error — the run continues). A non-nil error is an infrastructure failure
// that aborts the run; it names the row and comes with rekeyAborted.
func rekeyRow(ctx context.Context, db *sql.DB, ring *Keyring, r pendingRow) (rekeyOutcome, *RekeyFailure, error) {
	plain, err := ring.Open(r.enc, r.nonce, r.version)
	if err != nil {
		// nolint:nilerr // err is not swallowed — it is relocated into
		// RekeyFailure.Err (which the caller reports); a per-row decrypt failure
		// must not abort the whole backfill.
		return rekeyFailedRow, &RekeyFailure{UserID: r.userID, SealedVersion: r.version, Err: err}, nil
	}
	newEnc, newNonce, newVer, err := ring.Seal(plain)
	if err != nil {
		// nolint:nilerr // see above — per-row re-seal failure, reported not aborted.
		return rekeyFailedRow, &RekeyFailure{UserID: r.userID, SealedVersion: r.version, Err: err}, nil
	}

	n, err := resealRowTx(ctx, db, r, newEnc, newNonce, newVer)
	if err != nil {
		return rekeyAborted, nil, fmt.Errorf("reseal user %s (key_version %d): %w", r.userID, r.version, err)
	}
	if n == 0 {
		return rekeySkipped, nil, nil // concurrent rewrite (server writes are active-version)
	}
	return rekeyDone, nil, nil
}

// rewrapFlag and rewrapValue name the transaction-local setting migration
// 000189's material trigger reads to exempt a rewrap of the same secret.
const (
	rewrapFlag  = "concord.credential_rewrap"
	rewrapValue = "mfa-rekey"
)

// resealRowTx writes one re-sealed row in its own transaction under the rewrap
// flag and returns the rows written (0 = the CAS lost to a concurrent write).
// The flag is set with is_local = true, so it ends with the transaction and
// cannot exempt a later statement on the same pooled connection.
//
// The CAS pins the material_version read with the batch, so when it matches,
// the trigger's OLD.material_version is that value, and a different value from
// RETURNING means the trigger did not apply the rewrap exemption and drew a new
// version (its fail-closed fallback). That is an error, returned before Commit,
// so the row is rolled back and the run aborts rather than silently re-stamping
// a credential as newly enrolled.
//
// The deferred rollback discards any transaction that did not commit (a failed
// statement, a later error, a cancelled context) and joins its own failure into
// the returned error. A Commit error is returned, but it does not prove the row
// was not written; a re-run converges either way, because a row already
// re-sealed is no longer pending.
func resealRowTx(ctx context.Context, db *sql.DB, r pendingRow, enc, nonce []byte, ver int) (_ int64, err error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin row transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback row transaction: %w", rbErr))
		}
	}()
	if _, err = tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, rewrapFlag, rewrapValue); err != nil {
		return 0, fmt.Errorf("set rewrap flag: %w", err)
	}
	var written int64
	err = tx.QueryRowContext(ctx, `
		UPDATE user_mfa_totp
		   SET totp_secret_enc = $1, totp_secret_nonce = $2, key_version = $3, updated_at = NOW()
		 WHERE user_id = $4 AND key_version = $5 AND totp_secret_enc = $6 AND material_version = $7
		RETURNING material_version`,
		enc, nonce, ver, r.userID, r.version, r.enc, r.materialVersion).Scan(&written)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil // the CAS lost: a concurrent write changed the row
	}
	if err != nil {
		return 0, fmt.Errorf("update row: %w", err)
	}
	if written != r.materialVersion {
		return 0, fmt.Errorf("rewrap exemption not applied for user %s: material_version changed", r.userID)
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit row transaction: %w", err)
	}
	return 1, nil
}

// zeroUUID is the keyset-pagination first-page sentinel (see Rekey). No real
// gen_random_uuid() equals it, so `user_id > zeroUUID` selects every row.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// pendingRow is one user_mfa_totp row awaiting re-encryption.
type pendingRow struct {
	userID          string
	enc             []byte
	nonce           []byte
	version         int   // key_version: the sealing key
	materialVersion int64 // migration 000189's material_version, pinned by the CAS
}

// fetchPendingBatch pages through rows not sealed under the active version,
// keyset-ordered by user_id so the caller's cursor advances past rows it
// could not process. A failure to close the result set is returned like any
// other read error, with no batch.
func fetchPendingBatch(ctx context.Context, db *sql.DB, active int, cursor string, batchSize int) (batch []pendingRow, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT user_id, totp_secret_enc, totp_secret_nonce, key_version, material_version
		  FROM user_mfa_totp
		 WHERE key_version <> $1 AND user_id > $2::uuid
		 ORDER BY user_id
		 LIMIT $3`, active, cursor, batchSize)
	if err != nil {
		return nil, fmt.Errorf("select pending rows: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			batch = nil
			err = errors.Join(err, fmt.Errorf("close pending rows: %w", cerr))
		}
	}()

	batch = make([]pendingRow, 0, batchSize)
	for rows.Next() {
		var r pendingRow
		if err = rows.Scan(&r.userID, &r.enc, &r.nonce, &r.version, &r.materialVersion); err != nil {
			return nil, fmt.Errorf("scan pending row: %w", err)
		}
		batch = append(batch, r)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending rows: %w", err)
	}
	return batch, nil
}
