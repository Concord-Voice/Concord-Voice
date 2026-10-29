package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// TokenTheftRecoveryInterval bounds how long a durable pending sweep can wait
	// after a transient database or Redis failure.
	TokenTheftRecoveryInterval     = 10 * time.Second
	tokenTheftRecoveryTimeout      = 5 * time.Second
	tokenTheftEnqueueTimeout       = 6 * time.Second
	tokenTheftRedisPersistTimeout  = 2 * time.Second
	tokenTheftLegacyStateValue     = "1"
	tokenTheftLegacyFallbackSetKey = "token_theft_pending_revocations:fallback"    // #nosec G101 -- Redis namespace, not a credential
	tokenTheftFallbackHashKey      = "token_theft_pending_revocations:fallback:v2" // #nosec G101 -- Redis namespace, not a credential
)

var persistTokenTheftRecoveryStateScript = redis.NewScript(`
redis.call("HSET", KEYS[1], ARGV[1], ARGV[2])
redis.call("SET", KEYS[2], ARGV[2])
return 1
`)

var finalizeTokenTheftRecoveryStateScript = redis.NewScript(`
local fallback = redis.call("HGET", KEYS[1], ARGV[1])
if fallback == ARGV[2] or (ARGV[3] ~= "" and fallback == ARGV[3]) then
  redis.call("HDEL", KEYS[1], ARGV[1])
end
if redis.call("GET", KEYS[2]) == ARGV[2] then
  redis.call("DEL", KEYS[2])
end
redis.call("SREM", KEYS[3], ARGV[1])
return 1
`)

var beginTokenTheftSweepScript = redis.NewScript(`
local fallback = redis.call("HGET", KEYS[1], ARGV[1])
redis.call("SET", KEYS[2], ARGV[2])
return fallback or ""
`)

var finalizeLegacyTokenTheftRecoveryStateScript = redis.NewScript(`
redis.call("SREM", KEYS[1], ARGV[1])
if redis.call("GET", KEYS[2]) == ARGV[2] then
  redis.call("DEL", KEYS[2])
end
return 1
`)

type tokenTheftPendingRevocation struct {
	operationID           sql.NullString
	supersededOperationID sql.NullString
	completedAt           sql.NullTime
}

func newTokenTheftRecoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), tokenTheftRecoveryTimeout)
}

// EnqueueTokenTheftRevocation records a theft-triggered sweep before it starts,
// allowing another process to resume it after a crash.
func EnqueueTokenTheftRevocation(ctx context.Context, db *sql.DB, userID string) error {
	operationID := uuid.NewString()
	return enqueueTokenTheftRevocation(ctx, db, userID, &operationID)
}

func enqueueTokenTheftRevocation(ctx context.Context, db *sql.DB, userID string, operationID *string) error {
	_, _, err := enqueueTokenTheftRevocationWithState(ctx, db, userID, operationID, nil)
	return err
}

// Keep the user-row lock through the Redis write and pending-row commit. Refresh
// mints take the same lock, and a second theft event cannot publish an older
// fallback after a newer event has already completed.
func enqueueTokenTheftRevocationWithState(ctx context.Context, db *sql.DB, userID string, operationID *string, persistState func() error) (stateAttempted bool, stateErr error, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, fmt.Errorf("begin token theft enqueue transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback token theft enqueue transaction: %w", rollbackErr))
		}
	}()
	if _, err := lockTokenTheftUser(ctx, tx, userID); err != nil {
		return false, nil, err
	}
	if persistState != nil {
		stateAttempted = true
		stateErr = persistState()
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO token_theft_pending_revocations (user_id, operation_id)
		VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE
		SET operation_id = EXCLUDED.operation_id, superseded_operation_id = NULL,
		    completed_at = NULL, updated_at = NOW()`, userID, operationID)
	if err != nil {
		return stateAttempted, stateErr, fmt.Errorf("enqueue token theft revocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return stateAttempted, stateErr, fmt.Errorf("commit token theft enqueue transaction: %w", err)
	}
	return stateAttempted, stateErr, nil
}

func lockTokenTheftUser(ctx context.Context, tx *sql.Tx, userID string) (bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock token theft user: %w", err)
	}
	return true, nil
}

// queueTokenTheftRevocation records the normal PostgreSQL recovery row. When
// that store is temporarily unavailable, it atomically persists a Redis deny
// marker and fallback record so recovery can promote it once PostgreSQL returns.
func queueTokenTheftRevocation(ctx context.Context, db *sql.DB, redisClient *redis.Client, userID string, recoveryClient ...func(context.Context) (*redis.Client, error)) (queuedInDB bool, err error) {
	operationID := uuid.NewString()
	stateAttempted, stateErr, enqueueErr := enqueueTokenTheftRevocationWithState(ctx, db, userID, &operationID, func() error {
		return persistTokenTheftRecoveryState(ctx, redisClient, userID, operationID, recoveryClient...)
	})
	if !stateAttempted {
		// PostgreSQL was unavailable before the user lock. Keep the fallback
		// event even though no SQL transaction could be opened.
		fallbackCtx, cancelFallback := context.WithTimeout(context.WithoutCancel(ctx), tokenTheftRedisPersistTimeout)
		defer cancelFallback()
		stateErr = persistTokenTheftRecoveryState(fallbackCtx, redisClient, userID, operationID, recoveryClient...)
	}
	if stateErr != nil && enqueueErr != nil {
		return false, errors.Join(enqueueErr, stateErr)
	}
	if stateErr != nil {
		return true, stateErr
	}
	if enqueueErr == nil {
		return true, nil
	}
	return false, nil
}

func persistTokenTheftRecoveryState(stateCtx context.Context, redisClient *redis.Client, userID, operationID string, recoveryClient ...func(context.Context) (*redis.Client, error)) (err error) {
	if redisClient == nil {
		return errors.New("persist token theft recovery state: token theft block store unavailable")
	}
	// Keep time in the outer recovery context for the PostgreSQL INSERT and
	// COMMIT when Redis is reachable but too slow to answer.
	fallbackCtx, cancelFallback := context.WithTimeout(stateCtx, tokenTheftRedisPersistTimeout)
	defer cancelFallback()
	// WAITAOF applies only to writes on the same Redis connection. Without this
	// acknowledgement, a crash can lose the sole fallback after PostgreSQL
	// rejects the enqueue transaction.
	conn, closeConn, openErr := tokenTheftRecoveryConnection(fallbackCtx, redisClient, recoveryClient...)
	if openErr != nil {
		return openErr
	}
	defer func() {
		if closeErr := closeConn(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("release token theft Redis connection: %w", closeErr))
		}
	}()
	if _, stateErr := persistTokenTheftRecoveryStateScript.Run(fallbackCtx, conn,
		[]string{tokenTheftFallbackHashKey, middleware.TokenTheftBlockedKey(userID)}, userID, operationID,
	).Result(); stateErr != nil {
		return fmt.Errorf("persist token theft recovery state: %w", stateErr)
	}
	local, stateErr := conn.WaitAOF(fallbackCtx, 1, 0, tokenTheftRedisPersistTimeout).Result()
	if stateErr != nil {
		return fmt.Errorf("sync token theft recovery state to Redis AOF: %w", stateErr)
	}
	if len(local) != 2 || local[0] != 1 {
		return fmt.Errorf("sync token theft recovery state to Redis AOF: no local fsync acknowledgement: %v", local)
	}
	return nil
}

// ProcessTokenTheftRevocation keeps the deny marker set until all active refresh
// tokens are durably revoked. A completed row retains cleanup state after the
// SQL commit, so a Redis cleanup retry never repeats the token sweep.
func ProcessTokenTheftRevocation(ctx context.Context, db *sql.DB, redisClient *redis.Client, userID string, recoveryClient ...func(context.Context) (*redis.Client, error)) (err error) {
	if redisClient == nil {
		return errors.New("token theft block store unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin token theft revocation transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback token theft revocation transaction: %w", rollbackErr))
		}
	}()
	userExists, err := lockTokenTheftUser(ctx, tx, userID)
	if err != nil {
		return err
	}
	if !userExists {
		return commitTokenTheftNoop(tx, "token theft deleted-user transaction")
	}

	pending, found, err := lockTokenTheftPendingRevocation(ctx, tx, userID)
	if err != nil {
		return err
	}
	if !found {
		return commitTokenTheftNoop(tx, "token theft no-op transaction")
	}
	if !pending.completedAt.Valid {
		supersededID, stateErr := beginTokenTheftSweepScript.Run(ctx, redisClient,
			[]string{tokenTheftFallbackHashKey, middleware.TokenTheftBlockedKey(userID)}, userID, tokenTheftStateValue(pending.operationID),
		).Text()
		if stateErr != nil {
			return fmt.Errorf("set token theft block: %w", stateErr)
		}
		if supersededID == tokenTheftStateValue(pending.operationID) {
			supersededID = ""
		}
		if _, err := revokeAllRefreshTokensTx(ctx, tx, userID, RevokeAllRefreshTokensOptions{}); err != nil {
			return fmt.Errorf("revoke refresh tokens: %w", err)
		}
		if err := completeTokenTheftRevocation(ctx, tx, userID, pending.operationID, supersededID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit token theft revocation transaction: %w", err)
	}
	if err := finalizeTokenTheftRevocation(ctx, db, redisClient, userID, pending.operationID, recoveryClient...); err != nil {
		return err
	}
	return nil
}

func lockTokenTheftPendingRevocation(ctx context.Context, tx *sql.Tx, userID string) (tokenTheftPendingRevocation, bool, error) {
	var pending tokenTheftPendingRevocation
	if err := tx.QueryRowContext(ctx,
		`SELECT operation_id, completed_at FROM token_theft_pending_revocations WHERE user_id = $1 FOR UPDATE`, userID,
	).Scan(&pending.operationID, &pending.completedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return pending, false, nil
		}
		return pending, false, fmt.Errorf("lock pending token theft revocation: %w", err)
	}
	return pending, true, nil
}

func tokenTheftStateValue(operationID sql.NullString) string {
	if operationID.Valid {
		return operationID.String
	}
	return tokenTheftLegacyStateValue
}

func nullableTokenTheftOperationID(operationID string) any {
	if operationID == "" {
		return nil
	}
	return operationID
}

func completeTokenTheftRevocation(ctx context.Context, tx *sql.Tx, userID string, operationID sql.NullString, supersededID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET token_theft_generation = token_theft_generation + 1 WHERE id = $1`, userID,
	); err != nil {
		return fmt.Errorf("advance token theft generation: %w", err)
	}
	query := `UPDATE token_theft_pending_revocations SET completed_at = NOW(), updated_at = NOW(), superseded_operation_id = $2 WHERE user_id = $1 AND operation_id IS NULL`
	args := []any{userID, nullableTokenTheftOperationID(supersededID)}
	if operationID.Valid {
		query = `UPDATE token_theft_pending_revocations SET completed_at = NOW(), updated_at = NOW(), superseded_operation_id = $2 WHERE user_id = $1 AND operation_id = $3`
		args = append(args, operationID.String)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("complete token theft revocation: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check completed token theft revocation: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("complete token theft revocation: expected one row, updated %d", updated)
	}
	return nil
}

func commitTokenTheftNoop(tx *sql.Tx, name string) error {
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", name, err)
	}
	return nil
}

// finalizeTokenTheftRevocation clears post-commit Redis state before removing
// the completed row. Failures leave the completed row for cleanup-only retry.
func finalizeTokenTheftRevocation(ctx context.Context, db *sql.DB, redisClient *redis.Client, userID string, operationID sql.NullString, recoveryClient ...func(context.Context) (*redis.Client, error)) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin token theft cleanup transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback token theft cleanup transaction: %w", rollbackErr))
		}
	}()
	userExists, err := lockTokenTheftUser(ctx, tx, userID)
	if err != nil {
		return err
	}
	if !userExists {
		return commitTokenTheftNoop(tx, "token theft deleted-user cleanup transaction")
	}

	pending, found, err := lockCompletedTokenTheftRevocation(ctx, tx, userID, operationID)
	if err != nil {
		return err
	}
	if !found || !pending.completedAt.Valid {
		return commitTokenTheftNoop(tx, "token theft cleanup no-op transaction")
	}
	if err := clearTokenTheftRecoveryState(ctx, redisClient, userID, operationID, pending.supersededOperationID, recoveryClient...); err != nil {
		return err
	}
	if err := retireCompletedTokenTheftRevocation(ctx, tx, userID, operationID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit token theft cleanup transaction: %w", err)
	}
	return nil
}

func lockCompletedTokenTheftRevocation(ctx context.Context, tx *sql.Tx, userID string, operationID sql.NullString) (tokenTheftPendingRevocation, bool, error) {
	var pending tokenTheftPendingRevocation
	query := `SELECT operation_id, superseded_operation_id, completed_at FROM token_theft_pending_revocations WHERE user_id = $1 AND operation_id IS NULL FOR UPDATE`
	args := []any{userID}
	if operationID.Valid {
		query = `SELECT operation_id, superseded_operation_id, completed_at FROM token_theft_pending_revocations WHERE user_id = $1 AND operation_id = $2 FOR UPDATE`
		args = append(args, operationID.String)
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&pending.operationID, &pending.supersededOperationID, &pending.completedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return pending, false, nil
		}
		return pending, false, fmt.Errorf("lock completed token theft revocation: %w", err)
	}
	return pending, true, nil
}

// Each sticky scope owns its snapshot client; the shared streaming pool is never closed.
func tokenTheftRecoveryConnection(ctx context.Context, client *redis.Client, recoveryClient ...func(context.Context) (*redis.Client, error)) (*redis.Conn, func() error, error) {
	owned := len(recoveryClient) > 0 && recoveryClient[0] != nil
	if owned {
		var err error
		client, err = recoveryClient[0](ctx)
		if err != nil {
			return nil, nil, err
		}
	}
	if client == nil {
		return nil, nil, errors.New("token theft block store unavailable")
	}
	connClient := client
	if !owned {
		connClient = client.WithTimeout(tokenTheftRedisPersistTimeout)
	}
	// WithTimeout drops freshness hooks; managed clients already honor the scope deadline.
	conn := connClient.Conn()
	return conn, func() (err error) {
		if closeErr := conn.Close(); closeErr != nil {
			err = fmt.Errorf("close token theft Redis connection: %w", closeErr)
		}
		if owned {
			if closeErr := client.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close token theft Redis client: %w", closeErr))
			}
		}
		return err
	}, nil
}

func clearTokenTheftRecoveryState(ctx context.Context, redisClient *redis.Client, userID string, operationID, supersededOperationID sql.NullString, recoveryClient ...func(context.Context) (*redis.Client, error)) (err error) {
	clearCtx, cancel := context.WithTimeout(ctx, tokenTheftRedisPersistTimeout)
	defer cancel()
	conn, closeConn, err := tokenTheftRecoveryConnection(clearCtx, redisClient, recoveryClient...)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closeConn(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("release token theft cleanup Redis connection: %w", closeErr))
		}
	}()
	if operationID.Valid {
		supersededID := ""
		if supersededOperationID.Valid {
			supersededID = supersededOperationID.String
		}
		_, err = finalizeTokenTheftRecoveryStateScript.Run(clearCtx, conn,
			[]string{tokenTheftFallbackHashKey, middleware.TokenTheftBlockedKey(userID), tokenTheftLegacyFallbackSetKey}, userID, operationID.String, supersededID,
		).Result()
	} else {
		_, err = finalizeLegacyTokenTheftRecoveryStateScript.Run(clearCtx, conn,
			[]string{tokenTheftLegacyFallbackSetKey, middleware.TokenTheftBlockedKey(userID)}, userID, tokenTheftLegacyStateValue,
		).Result()
	}
	if err != nil {
		return fmt.Errorf("clear token theft recovery state: %w", err)
	}
	local, err := conn.WaitAOF(clearCtx, 1, 0, tokenTheftRedisPersistTimeout).Result()
	if err != nil {
		return fmt.Errorf("sync token theft recovery cleanup to Redis AOF: %w", err)
	}
	if len(local) != 2 || local[0] != 1 {
		return fmt.Errorf("sync token theft recovery cleanup to Redis AOF: no local fsync acknowledgement: %v", local)
	}
	return nil
}

func retireCompletedTokenTheftRevocation(ctx context.Context, tx *sql.Tx, userID string, operationID sql.NullString) error {
	query := `DELETE FROM token_theft_pending_revocations WHERE user_id = $1 AND operation_id IS NULL AND completed_at IS NOT NULL`
	args := []any{userID}
	if operationID.Valid {
		query = `DELETE FROM token_theft_pending_revocations WHERE user_id = $1 AND operation_id = $2 AND completed_at IS NOT NULL`
		args = append(args, operationID.String)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("retire completed token theft revocation: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check retired token theft revocation: %w", err)
	}
	if deleted != 1 {
		return fmt.Errorf("retire completed token theft revocation: expected one row, deleted %d", deleted)
	}
	return nil
}

// RecoverPendingTokenTheftRevocations replays durable theft sweeps. It is safe
// for several replicas to run: ProcessTokenTheftRevocation locks each pending row.
func RecoverPendingTokenTheftRevocations(ctx context.Context, db *sql.DB, redisClient *redis.Client, log *logger.Logger, recoveryClient ...func(context.Context) (*redis.Client, error)) error {
	userIDs, err := listPendingTokenTheftRevocationUserIDs(ctx, db)
	if err != nil {
		return err
	}
	seenUserIDs := make(map[string]struct{}, len(userIDs))
	for _, userID := range userIDs {
		seenUserIDs[userID] = struct{}{}
	}

	var errs []error
	if redisClient == nil {
		errs = append(errs, errors.New("list token theft revocation fallbacks: token theft block store unavailable"))
	} else {
		userIDs, err = recoverTokenTheftFallbacks(ctx, db, redisClient, log, userIDs, seenUserIDs)
		if err != nil {
			errs = append(errs, err)
		}
		userIDs, err = recoverLegacyTokenTheftFallbacks(ctx, db, redisClient, log, userIDs, seenUserIDs)
		if err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, recoverPendingTokenTheftRevocations(ctx, db, redisClient, log, userIDs, recoveryClient...)...)
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return nil
}

func listPendingTokenTheftRevocationUserIDs(ctx context.Context, db *sql.DB) (userIDs []string, err error) {
	rows, err := db.QueryContext(ctx,
		`SELECT user_id FROM token_theft_pending_revocations ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list pending token theft revocations: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close pending token theft revocations: %w", closeErr))
		}
	}()
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("scan pending token theft revocation: %w", err)
		}
		userIDs = append(userIDs, userID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending token theft revocations: %w", err)
	}
	return userIDs, nil
}

func recoverTokenTheftFallbacks(ctx context.Context, db *sql.DB, redisClient *redis.Client, log *logger.Logger, userIDs []string, seenUserIDs map[string]struct{}) ([]string, error) {
	fallbackOperations, err := redisClient.HGetAll(ctx, tokenTheftFallbackHashKey).Result()
	if err != nil {
		return userIDs, fmt.Errorf("list token theft revocation fallbacks: %w", err)
	}
	var errs []error
	for userID, operationID := range fallbackOperations {
		if _, seen := seenUserIDs[userID]; seen {
			continue
		}
		added, err := promoteTokenTheftFallback(ctx, db, redisClient, userID, &operationID)
		if err != nil {
			errs = append(errs, fmt.Errorf("promote token theft fallback: %w", err))
			log.Error("Token theft fallback promotion failed", "error", err, "user_id", userID)
			continue
		}
		if added {
			seenUserIDs[userID] = struct{}{}
			userIDs = append(userIDs, userID)
		}
	}
	return userIDs, errors.Join(errs...)
}

func recoverLegacyTokenTheftFallbacks(ctx context.Context, db *sql.DB, redisClient *redis.Client, log *logger.Logger, userIDs []string, seenUserIDs map[string]struct{}) ([]string, error) {
	legacyFallbackUserIDs, err := redisClient.SMembers(ctx, tokenTheftLegacyFallbackSetKey).Result()
	if err != nil {
		return userIDs, fmt.Errorf("list legacy token theft revocation fallbacks: %w", err)
	}
	var errs []error
	for _, userID := range legacyFallbackUserIDs {
		if _, seen := seenUserIDs[userID]; seen {
			// The matching SQL row's cleanup removes this set member on its
			// pinned Redis connection and waits for AOF before retiring the row.
			continue
		}
		added, err := promoteTokenTheftFallback(ctx, db, redisClient, userID, nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("promote legacy token theft fallback: %w", err))
			log.Error("Legacy token theft fallback promotion failed", "error", err, "user_id", userID)
			continue
		}
		if added {
			seenUserIDs[userID] = struct{}{}
			userIDs = append(userIDs, userID)
		}
	}
	return userIDs, errors.Join(errs...)
}

func promoteTokenTheftFallback(ctx context.Context, db *sql.DB, redisClient *redis.Client, userID string, operationID *string) (bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, tokenTheftRecoveryTimeout)
	added, enqueueErr := insertCurrentTokenTheftFallback(attemptCtx, db, redisClient, userID, operationID)
	cancel()
	if enqueueErr == nil {
		return added, nil
	}
	retired, retireErr := retireTokenTheftRecoveryStateIfUserDeleted(ctx, db, redisClient, userID, operationID)
	if retireErr != nil {
		return false, retireErr
	}
	if retired {
		return false, nil
	}
	return false, enqueueErr
}

// A scan may hold a stale Redis snapshot. Compare the entry while holding the
// same user lock as cleanup, then commit only if that exact fallback still exists.
func insertCurrentTokenTheftFallback(ctx context.Context, db *sql.DB, redisClient *redis.Client, userID string, operationID *string) (added bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin token theft fallback promotion: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback token theft fallback promotion: %w", rollbackErr))
		}
	}()
	userExists, err := lockTokenTheftUser(ctx, tx, userID)
	if err != nil {
		return false, err
	}
	if !userExists {
		return false, errors.New("token theft fallback user deleted")
	}
	stillCurrent, err := tokenTheftFallbackStillCurrent(ctx, redisClient, userID, operationID)
	if err != nil || !stillCurrent {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO token_theft_pending_revocations (user_id, operation_id)
		VALUES ($1, $2) ON CONFLICT (user_id) DO NOTHING`, userID, operationID); err != nil {
		return false, fmt.Errorf("promote token theft fallback: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit token theft fallback promotion: %w", err)
	}
	return true, nil
}

// tokenTheftFallbackStillCurrent reports whether the fallback this scan read is
// still the one in Redis: the same operation, or a legacy set entry.
func tokenTheftFallbackStillCurrent(ctx context.Context, redisClient *redis.Client, userID string, operationID *string) (bool, error) {
	if operationID == nil {
		current, err := redisClient.SIsMember(ctx, tokenTheftLegacyFallbackSetKey, userID).Result()
		if err != nil {
			return false, fmt.Errorf("recheck legacy token theft fallback: %w", err)
		}
		return current, nil
	}
	current, err := redisClient.HGet(ctx, tokenTheftFallbackHashKey, userID).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("recheck token theft fallback: %w", err)
	}
	return current == *operationID, nil
}

func recoverPendingTokenTheftRevocations(ctx context.Context, db *sql.DB, redisClient *redis.Client, log *logger.Logger, userIDs []string, recoveryClient ...func(context.Context) (*redis.Client, error)) []error {
	var errs []error
	for _, userID := range userIDs {
		attemptCtx, cancel := context.WithTimeout(ctx, tokenTheftRecoveryTimeout)
		err := ProcessTokenTheftRevocation(attemptCtx, db, redisClient, userID, recoveryClient...)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("recover token theft revocation for %s: %w", userID, err))
			log.Error("Pending token theft revocation recovery failed", "error", err, "user_id", userID)
		}
	}
	return errs
}

// retireTokenTheftRecoveryStateIfUserDeleted clears Redis-only recovery state
// after an account erase cascades its durable row. The comparison-and-delete
// scripts preserve state belonging to a newer theft event for the same user.
func retireTokenTheftRecoveryStateIfUserDeleted(ctx context.Context, db *sql.DB, redisClient *redis.Client, userID string, operationID *string) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check token theft fallback user: %w", err)
	}
	if exists {
		return false, nil
	}
	if operationID != nil {
		if _, err := finalizeTokenTheftRecoveryStateScript.Run(ctx, redisClient,
			[]string{tokenTheftFallbackHashKey, middleware.TokenTheftBlockedKey(userID), tokenTheftLegacyFallbackSetKey}, userID, *operationID, "",
		).Result(); err != nil {
			return false, fmt.Errorf("clear deleted-user token theft recovery state: %w", err)
		}
		return true, nil
	}
	if _, err := finalizeLegacyTokenTheftRecoveryStateScript.Run(ctx, redisClient,
		[]string{tokenTheftLegacyFallbackSetKey, middleware.TokenTheftBlockedKey(userID)}, userID, tokenTheftLegacyStateValue,
	).Result(); err != nil {
		return false, fmt.Errorf("clear deleted-user legacy token theft recovery state: %w", err)
	}
	return true, nil
}

// StartTokenTheftRecoveryWorker runs startup recovery before returning, then
// replays pending theft sweeps on a fixed interval until ctx is cancelled.
func StartTokenTheftRecoveryWorker(ctx context.Context, db *sql.DB, redisClient *redis.Client, log *logger.Logger, interval time.Duration, recoveryClient ...func(context.Context) (*redis.Client, error)) error {
	if err := RecoverPendingTokenTheftRevocations(ctx, db, redisClient, log, recoveryClient...); err != nil {
		return fmt.Errorf("startup token theft revocation recovery incomplete: %w", err)
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		run := func() {
			if err := RecoverPendingTokenTheftRevocations(ctx, db, redisClient, log, recoveryClient...); err != nil {
				log.Error("Pending token theft revocation recovery failed", "error", err)
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
	return nil
}
