package testhelpers

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
)

// HashVerifier checks a password against users.password_hash with no lockout.
// It stands in for *auth.Handler in tests that build users.NewHandler
// directly; route tests go through the real verifier the router wires.
type HashVerifier struct{ DB *sql.DB }

// VerifyPassword reports auth.ErrInvalidCredentials on a mismatch.
func (v HashVerifier) VerifyPassword(ctx context.Context, userID, password string) (string, error) {
	var hash string
	if err := v.DB.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", auth.ErrInvalidCredentials
		}
		return "", err
	}
	ok, err := auth.VerifyPassword(password, hash)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", auth.ErrInvalidCredentials
	}
	return "", nil
}
