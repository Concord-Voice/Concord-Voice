package stepup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
)

// Step-up tokens (#3509, migration 000162): a single-use, purpose-bound proof
// that a factor was verified a moment ago, spent by the route it was minted
// for inside that route's own transaction.
//
// The invariant: a token minted for (user, factor, purpose) under credential
// epoch E is accepted at most once, only by a consumer naming that same user,
// factor and purpose, only before it expires, and only while the user's
// credential epoch is still E — and a consumer whose transaction rolls back
// leaves it spendable.
//
// What holds it up, and what breaks it:
//   - The spend is ONE statement, SpendToken's DELETE … RETURNING, run on the
//     consumer's transaction. Spending on the pool while the consumer holds a
//     transaction, or splitting the match from the delete, breaks rollback
//     restoration or single use. That is exactly the Redis GETDEL this table
//     replaced for WebAuthn tokens (#3509, Codex P2).
//   - Purpose and factor are columns of the match, never of the token, so a
//     token for another purpose or factor matches no row and is NOT consumed
//     (#3467). A consumer passing a purpose other than its own breaks this.
//   - The epoch is compared against users.credential_epoch inside the spend,
//     so a credential-epoch rotation (password change, key reset, account
//     recovery) strands every token minted before it. A token binds to the
//     user and that epoch, NOT to a session: revoking one session, or all of
//     them, without rotating the epoch leaves an outstanding token spendable
//     until it expires — at most 60 s, by the same user's other session.
//   - Only SHA-256(token) is stored. The token reaches no log and no column.

// TokenFactor is the factor a step-up token stands for. It is a column of the
// match: a password token never satisfies a WebAuthn consumer, nor the reverse.
type TokenFactor string

// The two factors a step-up token may stand for (migration 000162's CHECK).
const (
	FactorPassword TokenFactor = "password"
	FactorWebAuthn TokenFactor = "webauthn"
)

const (
	// TokenTTL is how long a minted token stays spendable.
	TokenTTL = 60 * time.Second

	// MaxLiveTokensPerUser caps one user's live tokens, both factors together.
	// A mint past it deletes that user's oldest; see migrations.md § Step-up
	// tokens for the bound this gives the table.
	MaxLiveTokensPerUser = 16

	// tokenEntropyBytes is the random part of a token, before base64url.
	tokenEntropyBytes = 32
)

// Fixed statements only — never fmt.Sprintf'd SQL.
//
// Every time these compare or stamp is clock_timestamp(), the statement's own
// moment, never now(): now() is the transaction's start, so a consumer that
// began before a token expired and then waited on a lock would spend it
// expired, and a mint that waited on the users row behind other mints would
// stamp its row older than theirs — the row the cap then evicts — and lose
// the wait from its 60 s (#3509 review).
const (
	// mintLockSubjectSQL serializes one user's mints, which is what makes
	// MaxLiveTokensPerUser exact rather than approximate, and reads the epoch
	// the token is bound to. NO KEY UPDATE, not SHARE: two concurrent mints
	// under SHARE would each count the other's row as absent.
	mintLockSubjectSQL = `SELECT credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`

	mintDeleteExpiredSQL = `DELETE FROM step_up_tokens WHERE user_id = $1 AND expires_at <= clock_timestamp()`

	// mintInsertSQL reads the clock once, so created_at and expires_at are
	// exactly the TTL apart.
	mintInsertSQL = `
		INSERT INTO step_up_tokens (token_hash, user_id, factor, purpose, credential_epoch, expires_at, created_at)
		SELECT $1, $2, $3, $4, $5, stamp.at + $6 * interval '1 second', stamp.at
		FROM (SELECT clock_timestamp() AS at) AS stamp`

	// mintCapSQL keeps the newest MaxLiveTokensPerUser rows. The token
	// hash breaks a created_at tie only so the choice is deterministic.
	mintCapSQL = `
		DELETE FROM step_up_tokens WHERE user_id = $1 AND token_hash IN (
			SELECT token_hash FROM step_up_tokens WHERE user_id = $1
			ORDER BY created_at DESC, token_hash DESC OFFSET $2)`

	// spendSQL is the whole spend: match and consume in one statement,
	// with the epoch read inline so no caller has to thread it through.
	spendSQL = `
		DELETE FROM step_up_tokens
		WHERE token_hash = $1 AND user_id = $2 AND purpose = $3 AND factor = $4
		  AND expires_at > clock_timestamp()
		  AND credential_epoch IS NOT DISTINCT FROM (SELECT credential_epoch FROM users WHERE id = $2)
		RETURNING 1`

	// sweepBatchSQL is one sweep batch: at most $1 expired rows, the
	// longest-expired first (idx_step_up_tokens_expires). It skips rows a
	// mint or a spend holds, so the sweep never waits on, or deadlocks with,
	// a request.
	sweepBatchSQL = `
		DELETE FROM step_up_tokens WHERE token_hash IN (
			SELECT token_hash FROM step_up_tokens WHERE expires_at <= clock_timestamp()
			ORDER BY expires_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED)`
)

// sweepBatchSize bounds one sweep batch, and so what a context that ends
// mid-sweep can roll back.
const sweepBatchSize = 1000

// MintToken stores a fresh token for (userID, factor, purpose) and returns it in
// its only wire form, unpadded base64url. It runs its own transaction:
//
//  1. Lock the users row FOR NO KEY UPDATE and read credential_epoch.
//  2. Fence the session: credepoch.MatchEpoch against tokenEpoch, the access
//     token's epoch. A mint from a session older than the last rotation is
//     refused, and the stored
//     epoch is the one read under the lock, never an older one.
//  3. Delete the user's expired rows, insert, and cap the user's rows at
//     MaxLiveTokensPerUser, oldest first.
//
// The order is users first, then step_up_tokens, as every spender's is.
//
// Refusals: a missing users row is 401 ErrMsgSessionNoLongerValid; a stale
// epoch is 401 ErrMsgAuthenticationRequired (EpochMismatch true); anything
// else is a 500 with Cause set. No Cause carries the token: it is hashed
// before it reaches the database, and drivers do not echo parameters.
func MintToken(
	ctx context.Context, db *sql.DB, userID string, factor TokenFactor, purpose Purpose, tokenEpoch string,
) (string, *Error) {
	raw := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", verificationFailed(fmt.Errorf("generate step-up token: %w", err))
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return "", verificationFailed(fmt.Errorf("begin step-up token mint: %w", err))
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit

	var epoch sql.NullString
	err = tx.QueryRowContext(ctx, mintLockSubjectSQL, userID).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", subjectGone()
	}
	if err != nil {
		return "", verificationFailed(fmt.Errorf("lock step-up token subject: %w", err))
	}
	if credepoch.MatchEpoch(epoch, tokenEpoch) != nil {
		return "", &Error{
			Status: http.StatusUnauthorized,
			Body:   gin.H{"error": ErrMsgAuthenticationRequired},
			reason: reasonEpochMismatch,
		}
	}
	if err := storeToken(ctx, tx, token, userID, factor, purpose, epoch); err != nil {
		return "", verificationFailed(err)
	}
	if err := tx.Commit(); err != nil {
		return "", verificationFailed(fmt.Errorf("commit step-up token mint: %w", err))
	}
	return token, nil
}

// storeToken is MintToken's step 3, on the transaction that holds the lock.
func storeToken(
	ctx context.Context, tx *sql.Tx, token, userID string, factor TokenFactor, purpose Purpose, epoch sql.NullString,
) error {
	if _, err := tx.ExecContext(ctx, mintDeleteExpiredSQL, userID); err != nil {
		return fmt.Errorf("delete expired step-up tokens: %w", err)
	}
	if _, err := tx.ExecContext(ctx, mintInsertSQL, hashToken(token), userID, string(factor), string(purpose),
		epoch, int64(TokenTTL/time.Second)); err != nil {
		return fmt.Errorf("insert step-up token: %w", err)
	}
	if _, err := tx.ExecContext(ctx, mintCapSQL, userID, MaxLiveTokensPerUser); err != nil {
		return fmt.Errorf("cap step-up tokens: %w", err)
	}
	return nil
}

// SpendToken spends token on q: the consumer's transaction when it has one,
// which is what makes a rollback restore the token, or the pool when it has
// none. It reports true when exactly one live row matched and was deleted.
//
// It reports false, with a nil error, for every token that matches no row —
// absent, already spent, expired, minted for another user, purpose or factor,
// or under a superseded credential epoch — and nothing tells those apart
// (observability.md principle 7). A false consumes nothing. An empty token
// reads nothing.
func SpendToken(
	ctx context.Context, q RowQuerier, userID string, factor TokenFactor, purpose Purpose, token string,
) (bool, error) {
	if token == "" {
		return false, nil
	}
	var one int
	err := q.QueryRowContext(ctx, spendSQL, hashToken(token), userID, string(purpose), string(factor)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("spend step-up token: %w", err)
	}
	return true, nil
}

// SweepExpiredTokens deletes expired tokens in batches of sweepBatchSize,
// each one statement on the pool and so its own commit, until a batch comes
// up short or ctx ends, and reports how many it drained — on an error too. A
// batch ctx cuts short rolls back alone; what earlier batches deleted stays
// deleted, so a sweep that keeps running out of time still makes progress
// (review of #3509). A row a request holds is skipped, which can end the
// sweep early; the next pass takes it.
func SweepExpiredTokens(ctx context.Context, db *sql.DB) (int64, error) {
	var drained int64
	for {
		if err := ctx.Err(); err != nil {
			return drained, fmt.Errorf("sweep expired step-up tokens: %w", err)
		}
		res, err := db.ExecContext(ctx, sweepBatchSQL, sweepBatchSize)
		if err != nil {
			return drained, fmt.Errorf("sweep expired step-up tokens: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return drained, fmt.Errorf("sweep expired step-up tokens: rows affected: %w", err)
		}
		drained += n
		if n < sweepBatchSize {
			return drained, nil
		}
	}
}

// hashToken is the only form of a token the database sees. It hashes the wire
// string itself, so no decoding step can admit two spellings of one token.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
