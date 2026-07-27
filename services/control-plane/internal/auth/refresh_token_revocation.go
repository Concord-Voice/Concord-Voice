package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
)

// ErrNoActiveRefreshSession reports that no requested preserved session remains active.
var ErrNoActiveRefreshSession = errors.New("no active refresh session")

// RevokeAllRefreshTokensOptions controls a serialized bulk revocation.
type RevokeAllRefreshTokensOptions struct {
	ExcludeTokenHash        string
	ExcludeSessionID        string
	ExpectedCredentialEpoch string
	EnforceCredentialEpoch  bool
}

// RevokeAllRefreshTokens revokes a user's active refresh tokens. It serializes
// with session mints on the users row and can preserve the authenticated session.
func RevokeAllRefreshTokens(ctx context.Context, db *sql.DB, userID string, options RevokeAllRefreshTokensOptions) (result sql.Result, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin refresh-token revocation transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback refresh-token revocation transaction: %w", rollbackErr))
		}
	}()
	result, err = revokeAllRefreshTokensTx(ctx, tx, userID, options)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit refresh-token revocation transaction: %w", err)
	}
	return result, nil
}

// revokeAllRefreshTokensTx performs a serialized bulk refresh-token revocation
// inside the caller's transaction.
func revokeAllRefreshTokensTx(ctx context.Context, tx *sql.Tx, userID string, options RevokeAllRefreshTokensOptions) (result sql.Result, err error) {
	var credentialEpoch sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID,
	).Scan(&credentialEpoch); err != nil {
		return nil, fmt.Errorf("lock refresh-token revocation user: %w", err)
	}

	if options.EnforceCredentialEpoch {
		if err := credepoch.MatchEpoch(credentialEpoch, options.ExpectedCredentialEpoch); err != nil {
			return nil, fmt.Errorf("match refresh-token revocation credential epoch: %w", err)
		}
	}

	if options.ExcludeSessionID != "" || options.ExcludeTokenHash != "" {
		excludeSessionID, resolveErr := activeRefreshSessionID(ctx, tx, userID, options)
		if resolveErr != nil {
			return nil, resolveErr
		}
		result, err = tx.ExecContext(ctx,
			`UPDATE refresh_tokens SET revoked_at = NOW()
			 WHERE user_id = $1 AND id != $2 AND revoked_at IS NULL`,
			userID, excludeSessionID)
	} else {
		result, err = tx.ExecContext(ctx,
			`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	}
	if err != nil {
		return nil, fmt.Errorf("revoke refresh tokens: %w", err)
	}
	return result, nil
}

// activeRefreshSessionID resolves the active session that bulk revocation must preserve.
// The caller already holds the user lock; this row lock also serializes individual
// session revocation with the following bulk update.
func activeRefreshSessionID(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
	options RevokeAllRefreshTokensOptions,
) (string, error) {
	if options.ExcludeSessionID != "" {
		var sessionID string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM refresh_tokens
			 WHERE user_id = $1 AND id = $2 AND revoked_at IS NULL AND expires_at > NOW()
			 FOR UPDATE`, userID, options.ExcludeSessionID,
		).Scan(&sessionID)
		if err == nil {
			return sessionID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("resolve active bearer refresh session: %w", err)
		}
	}

	if options.ExcludeTokenHash != "" {
		var sessionID string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM refresh_tokens
			 WHERE user_id = $1 AND token_hash = $2 AND revoked_at IS NULL AND expires_at > NOW()
			 FOR UPDATE`, userID, options.ExcludeTokenHash,
		).Scan(&sessionID)
		if err == nil {
			return sessionID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("resolve active cookie refresh session: %w", err)
		}
	}

	return "", ErrNoActiveRefreshSession
}
