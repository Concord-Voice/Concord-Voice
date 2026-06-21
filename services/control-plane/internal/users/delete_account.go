// services/control-plane/internal/users/delete_account.go

package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// ErrUserNotFound is returned when DeleteAccount targets a user row that no
// longer exists. Callers should map this to HTTP 404; the endpoint remains
// idempotent (retry after a successful delete is harmless) while still
// distinguishing "already gone" from "just deleted" in operational logs.
var ErrUserNotFound = errors.New("user not found")

// AccountDeleter is the narrow interface the privacy handler depends on.
// Declared here so privacy can import it without pulling the concrete
// service (and its database handle) into the handler's test surface.
type AccountDeleter interface {
	DeleteAccount(ctx context.Context, userID string) error
}

// AccountService is the concrete AccountDeleter backed by the primary
// Postgres pool. It owns its own transaction boundary so the caller
// never holds a connection across any slow or external operation.
type AccountService struct {
	db  *sql.DB
	log *logger.Logger
}

// NewAccountService constructs an AccountService. The logger is optional;
// a nil value is tolerated so tests that do not exercise the failure path
// can construct a service without one. Production callers must always
// pass a non-nil logger.
func NewAccountService(db *sql.DB, log *logger.Logger) *AccountService {
	return &AccountService{db: db, log: log}
}

// DeleteAccount performs the full erasure inside one transaction:
//  1. DELETE FROM users WHERE id = $1 — cascades through every user_id-FK
//     table configured with ON DELETE CASCADE. If zero rows match, the user
//     is already gone and we return ErrUserNotFound WITHOUT writing an audit
//     row (nothing happened to audit).
//  2. INSERT an audit row into account_deletions with user_id = NULL. We
//     cannot reference the just-deleted user_id at this point — the FK
//     check would fire on insert and fail — but a NULL is the intended
//     post-commit state anyway (the schema's ON DELETE SET NULL would have
//     nulled it automatically had we ordered INSERT before DELETE). The
//     audit row captures the deletion event and timestamp.
//  3. COMMIT.
//
// The DELETE-then-INSERT ordering makes the retry-after-success path behave
// correctly: a second call gets RowsAffected = 0, returns ErrUserNotFound,
// and leaves no audit side effect. Ordering INSERT-first would make the
// FK reject the retry's audit INSERT with 23503 even though semantically
// nothing needs to happen.
//
// On failure at any stage the deferred rollback restores the pre-call
// state; callers may retry safely (idempotent).
func (s *AccountService) DeleteAccount(
	ctx context.Context,
	userID string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete account: begin tx: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			if s.log != nil {
				s.log.Error("delete account: rollback failed", "error", rbErr)
			}
		}
	}()

	result, err := tx.ExecContext(ctx,
		`DELETE FROM users WHERE id = $1`,
		userID,
	)
	if err != nil {
		return fmt.Errorf("delete account: delete user: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete account: rows affected: %w", err)
	}
	if rows == 0 {
		return ErrUserNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO account_deletions (user_id) VALUES (NULL)`,
	); err != nil {
		return fmt.Errorf("delete account: insert audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete account: commit: %w", err)
	}
	return nil
}
