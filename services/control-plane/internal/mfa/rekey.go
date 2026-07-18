package mfa

import (
	"context"
	"database/sql"
	"fmt"
)

// RekeyFailure records a row the backfill could not re-seal. The row is left
// untouched (still decryptable by whoever holds its sealing key).
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
//   - per-row autocommit UPDATE: a crash mid-run leaves a decryptable mixed
//     state (the retired key is still in the ring) and a re-run converges;
//   - CAS tail (key_version + prior ciphertext) makes a concurrent
//     re-enrollment a harmless skip — server writes are always active-version;
//   - keyset pagination on user_id::text advances past failed rows, so an
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

// rekeyOutcome classifies what happened to a single row.
type rekeyOutcome int

const (
	rekeyDone      rekeyOutcome = iota // re-sealed to the active version
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
		case rekeySkipped:
			res.Skipped++
		case rekeyFailedRow:
			res.Failed = append(res.Failed, *failure)
		default:
			res.Rekeyed++
		}
	}
	return cursor, nil
}

// rekeyRow re-seals one pending row to ring's active version. A decrypt/re-seal
// failure returns rekeyFailedRow + a *RekeyFailure and leaves the row untouched
// (nil error — the run continues). A non-nil error is an infrastructure failure
// that aborts the run.
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

	tag, err := db.ExecContext(ctx, `
		UPDATE user_mfa_totp
		   SET totp_secret_enc = $1, totp_secret_nonce = $2, key_version = $3, updated_at = NOW()
		 WHERE user_id = $4 AND key_version = $5 AND totp_secret_enc = $6`,
		newEnc, newNonce, newVer, r.userID, r.version, r.enc)
	if err != nil {
		return rekeyDone, nil, fmt.Errorf("update row: %w", err)
	}
	n, err := tag.RowsAffected()
	if err != nil {
		return rekeyDone, nil, fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return rekeySkipped, nil, nil // concurrent rewrite (server writes are active-version)
	}
	return rekeyDone, nil, nil
}

// zeroUUID is the keyset-pagination first-page sentinel (see Rekey). No real
// gen_random_uuid() equals it, so `user_id > zeroUUID` selects every row.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// pendingRow is one user_mfa_totp row awaiting re-encryption.
type pendingRow struct {
	userID  string
	enc     []byte
	nonce   []byte
	version int
}

// fetchPendingBatch pages through rows not sealed under the active version,
// keyset-ordered by user_id::text so the caller's cursor advances past rows it
// could not process.
func fetchPendingBatch(ctx context.Context, db *sql.DB, active int, cursor string, batchSize int) ([]pendingRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT user_id, totp_secret_enc, totp_secret_nonce, key_version
		  FROM user_mfa_totp
		 WHERE key_version <> $1 AND user_id > $2::uuid
		 ORDER BY user_id
		 LIMIT $3`, active, cursor, batchSize)
	if err != nil {
		return nil, fmt.Errorf("select pending rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	batch := make([]pendingRow, 0, batchSize)
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.userID, &r.enc, &r.nonce, &r.version); err != nil {
			return nil, fmt.Errorf("scan pending row: %w", err)
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending rows: %w", err)
	}
	return batch, nil
}
