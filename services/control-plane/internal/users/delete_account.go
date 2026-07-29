// services/control-plane/internal/users/delete_account.go

package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
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

// ChannelDeletedBroadcaster notifies connected server members after account
// erasure removes an incomplete channel.
type ChannelDeletedBroadcaster func(serverID, channelID string)

// AccountService is the concrete AccountDeleter backed by the primary
// Postgres pool. Its erasure transaction starts only after sender-gated
// activity cleanup has completed.
type AccountService struct {
	db              *sql.DB
	log             *logger.Logger
	activityCleanup *Handler
	channelDeleted  ChannelDeletedBroadcaster
}

// NewAccountService constructs an AccountService. The logger is optional;
// a nil value is tolerated so tests that do not exercise the failure path
// can construct a service without one. Production callers must always
// pass a non-nil logger.
func NewAccountService(db *sql.DB, log *logger.Logger) *AccountService {
	return &AccountService{db: db, log: log}
}

// SetActivitySettingsCleanupHandler binds the same sender coordinator and
// suppression service used by presence-settings writers.
func (s *AccountService) SetActivitySettingsCleanupHandler(handler *Handler) {
	s.activityCleanup = handler
}

// SetChannelDeletedBroadcaster binds the post-commit incomplete-channel event.
func (s *AccountService) SetChannelDeletedBroadcaster(broadcaster ChannelDeletedBroadcaster) {
	s.channelDeleted = broadcaster
}

// DeleteAccount first resumes any durable activity-policy cleanup while holding
// the sender gate, then performs the database erasure inside one transaction:
//  1. Lock the user and delete any incomplete E2EE channels they created, so
//     the post-commit caller can notify their server members.
//  2. DELETE FROM users WHERE id = $1 — cascades through every remaining
//     user_id-FK table configured with ON DELETE CASCADE.
//  3. INSERT an audit row into account_deletions with user_id = NULL. We
//     cannot reference the just-deleted user_id at this point — the FK
//     check would fire on insert and fail — but a NULL is the intended
//     post-commit state anyway (the schema's ON DELETE SET NULL would have
//     nulled it automatically had we ordered INSERT before DELETE). The
//     audit row captures the deletion event and timestamp.
//  4. COMMIT.
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
	if s.activityCleanup == nil {
		return s.deleteAccount(ctx, userID)
	}
	parsedUserID, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("delete account: invalid user id: %w", err)
	}
	if s.activityCleanup.presenceHistory == nil {
		return errors.New("delete account: activity cleanup coordinator unavailable")
	}
	if s.activityCleanup.activitySuppressor == nil {
		return errors.New("delete account: full activity suppression unavailable")
	}
	return s.activityCleanup.presenceHistory.WithSender(ctx, parsedUserID, func() error {
		if _, err := s.activityCleanup.resumePendingActivitySettingsCleanup(
			ctx, parsedUserID,
		); err != nil {
			return fmt.Errorf("delete account: resume activity cleanup: %w", err)
		}
		if err := s.activityCleanup.activitySuppressor.SuppressAllActivityAlreadyGated(
			ctx, parsedUserID,
		); err != nil {
			return fmt.Errorf("delete account: suppress active activity: %w", err)
		}
		return s.deleteAccount(ctx, userID)
	})
}

func (s *AccountService) deleteAccount(ctx context.Context, userID string) error {
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
	channelIDs, serverIDs, err := s.deleteAccountTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete account: commit: %w", err)
	}
	if s.channelDeleted != nil {
		for index, channelID := range channelIDs {
			s.channelDeleted(serverIDs[index], channelID)
		}
	}
	return nil
}

func (s *AccountService) deleteAccountTx(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
) (pq.StringArray, pq.StringArray, error) {
	var lockedUserID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&lockedUserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrUserNotFound
		}
		return nil, nil, fmt.Errorf("delete account: lock user: %w", err)
	}

	var channelIDs, serverIDs pq.StringArray
	if err := tx.QueryRowContext(ctx, `
		WITH incomplete AS (
			SELECT channel_id
			FROM channel_initial_key_distributions
			WHERE creator_id = $1
		)
		SELECT COALESCE(array_agg(c.id::text ORDER BY c.id), ARRAY[]::text[]),
		       COALESCE(array_agg(c.server_id::text ORDER BY c.id), ARRAY[]::text[])
		FROM channels c
		WHERE c.id IN (SELECT channel_id FROM incomplete)
		   OR c.linked_voice_channel_id IN (SELECT channel_id FROM incomplete)`, lockedUserID,
	).Scan(&channelIDs, &serverIDs); err != nil {
		return nil, nil, fmt.Errorf("delete account: list incomplete channels: %w", err)
	}
	if len(channelIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE id = ANY($1::uuid[])`, pq.Array(channelIDs)); err != nil {
			return nil, nil, fmt.Errorf("delete account: delete incomplete channels: %w", err)
		}
	}

	result, err := tx.ExecContext(ctx,
		`DELETE FROM users WHERE id = $1`,
		lockedUserID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("delete account: delete user: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, nil, fmt.Errorf("delete account: rows affected: %w", err)
	}
	if rows == 0 {
		return nil, nil, ErrUserNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO account_deletions (user_id) VALUES (NULL)`,
	); err != nil {
		return nil, nil, fmt.Errorf("delete account: insert audit: %w", err)
	}
	return channelIDs, serverIDs, nil
}
