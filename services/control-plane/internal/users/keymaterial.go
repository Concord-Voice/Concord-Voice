package users

import (
	"database/sql"
	"fmt"
)

// replaceKeyMaterialTx atomically rotates a user's E2EE key material to a new
// keypair and purges wrapped keys that were encrypted to the prior public key.
// This is the SOLE place the #1293 consistency invariant is enforced for the
// authenticated key-replacement path: user_keys + public_keys rotate together
// (matching key_version bump); stale channel_keys / dm_channel_keys are cleared.
//
// The public_keys UPDATE MUST affect exactly one row, else the transaction is
// rolled back: the table has no UNIQUE(user_id) constraint (migration 000004),
// so a 0-row UPDATE would silently re-create the orphaned-public-key bug this
// helper exists to prevent.
//
// Callers MUST pass an open *sql.Tx and commit/rollback around this call.
func replaceKeyMaterialTx(tx *sql.Tx, userID any, wrappedPriv, salt []byte, alg string, publicKey []byte) error {
	if _, err := tx.Exec(
		`UPDATE user_keys SET wrapped_private_key = $1, key_derivation_salt = $2,
		 key_version = key_version + 1, key_derivation_alg = $3, updated_at = NOW()
		 WHERE user_id = $4`,
		wrappedPriv, salt, alg, userID,
	); err != nil {
		return fmt.Errorf("update user_keys: %w", err)
	}

	// NB: this UPDATEs created_at = NOW() (not updated_at) to mirror the audited
	// RecoveryResetAccount column set exactly (the de-facto one-row-per-user,
	// mutated-in-place model). public_keys has no updated_at column.
	res, err := tx.Exec(
		`UPDATE public_keys SET public_key = $1, key_version = key_version + 1, created_at = NOW()
		 WHERE user_id = $2`,
		publicKey, userID,
	)
	if err != nil {
		return fmt.Errorf("update public_keys: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("public_keys rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("expected exactly 1 public_keys row for user, got %d", n)
	}

	if _, err := tx.Exec(`DELETE FROM channel_keys WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("clear channel_keys: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM dm_channel_keys WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("clear dm_channel_keys: %w", err)
	}
	return nil
}
