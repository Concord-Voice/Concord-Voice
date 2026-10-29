package attestation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AppAttestKey mirrors a row from app_attest_keys (ADR-0010 D17).
type AppAttestKey struct {
	KeyID       []byte
	PublicKey   []byte
	SignCount   uint32
	Environment string
}

// InsertAppAttestKey records a freshly attested key. A key is attested once,
// so an existing key id returns ErrConflict rather than being overwritten.
func (r *Repository) InsertAppAttestKey(ctx context.Context, k AppAttestKey, firstUserHash string) error {
	const q = `
		INSERT INTO app_attest_keys (key_id, public_key, sign_count, environment, first_user_hash)
		VALUES ($1, $2, 0, $3, $4)
		ON CONFLICT (key_id) DO NOTHING
	`
	res, err := r.db.ExecContext(ctx, q, k.KeyID, k.PublicKey, k.Environment, firstUserHash)
	if err != nil {
		return fmt.Errorf("insert app attest key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("insert app attest key rows affected: %w", err)
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

// GetAppAttestKey returns the key or nil if it is not on record.
func (r *Repository) GetAppAttestKey(ctx context.Context, keyID []byte) (*AppAttestKey, error) {
	const q = `SELECT key_id, public_key, sign_count, environment FROM app_attest_keys WHERE key_id = $1`
	k := &AppAttestKey{}
	var count int64
	err := r.db.QueryRowContext(ctx, q, keyID).Scan(&k.KeyID, &k.PublicKey, &count, &k.Environment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get app attest key: %w", err)
	}
	k.SignCount = uint32(count) //nolint:gosec // the column CHECK bounds it to uint32
	return k, nil
}

// AdvanceAppAttestCounter stores a verified assertion's counter. The update
// only applies when the counter strictly increases, so two concurrent
// requests replaying one counter cannot both succeed: the loser gets
// ErrConflict.
func (r *Repository) AdvanceAppAttestCounter(ctx context.Context, keyID []byte, counter uint32) error {
	const q = `
		UPDATE app_attest_keys SET sign_count = $2, last_used_at = NOW()
		WHERE key_id = $1 AND sign_count < $2
	`
	res, err := r.db.ExecContext(ctx, q, keyID, int64(counter))
	if err != nil {
		return fmt.Errorf("advance app attest counter: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("advance app attest counter rows affected: %w", err)
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}
