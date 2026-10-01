package stepup

// Contract tests for the step-up token store (#3509, migration 000162,
// design spec "Developer decisions, 2026-10-01", T-3/T-4). Every refusal path
// names the mutant it kills. They need the real schema: single use, rollback
// restoration and the epoch comparison are properties of the one DELETE
// statement running in PostgreSQL.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spendIn spends token in a fresh transaction, then commits or rolls back.
func spendIn(t *testing.T, db *sql.DB, userID string, factor TokenFactor, purpose Purpose, token string, commit bool) bool {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	ok, err := SpendToken(context.Background(), tx, userID, factor, purpose, token)
	require.NoError(t, err)
	if commit {
		require.NoError(t, tx.Commit())
	}
	return ok
}

// Two credential epochs in credepoch.NewEpoch's format (32 lowercase hex),
// which migration 000162's CHECK requires of a stored epoch.
const (
	epochOne = "11111111111111111111111111111111"
	epochTwo = "22222222222222222222222222222222"
)

func mintFor(t *testing.T, db *sql.DB, userID string, factor TokenFactor, purpose Purpose) string {
	t.Helper()
	token, e := MintToken(context.Background(), db, userID, factor, purpose, "")
	require.Nil(t, e)
	return token
}

func tokenRows(t *testing.T, db *sql.DB, userID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM step_up_tokens WHERE user_id = $1`, userID).Scan(&n))
	return n
}

// A token is 32 random bytes, base64url, and only its SHA-256 is stored, bound
// to the factor, purpose and a 60-second expiry.
//
// Mutants killed: storing the token itself (the hash column would differ),
// a shorter token, a TTL other than 60 s.
func TestMintToken_StoresOnlyTheHash(t *testing.T) {
	db := subjectTestDB(t)
	userID := subjectTestUser(t, db)

	token := mintFor(t, db, userID, FactorPassword, PurposeDMClear)

	require.Len(t, token, 43, "32 bytes, unpadded base64url")
	var hash []byte
	var factor, purpose string
	var ttl float64
	require.NoError(t, db.QueryRow(`SELECT token_hash, factor, purpose, EXTRACT(EPOCH FROM expires_at - created_at)
		FROM step_up_tokens WHERE user_id = $1`, userID).Scan(&hash, &factor, &purpose, &ttl))
	sum := sha256.Sum256([]byte(token))
	assert.Equal(t, sum[:], hash)
	assert.Equal(t, "password", factor)
	assert.Equal(t, string(PurposeDMClear), purpose)
	assert.InDelta(t, 60, ttl, 0.001)
}

// Mutant killed: dropping the DELETE (a SELECT that matches) — the second
// spend would succeed.
func TestSpendToken_SingleUse(t *testing.T) {
	db := subjectTestDB(t)
	userID := subjectTestUser(t, db)
	token := mintFor(t, db, userID, FactorPassword, PurposeMessageDelete)

	require.True(t, spendIn(t, db, userID, FactorPassword, PurposeMessageDelete, token, true))
	assert.False(t, spendIn(t, db, userID, FactorPassword, PurposeMessageDelete, token, true),
		"a committed spend consumes the token")
}

// A rolled-back spend leaves the token spendable: the defect a Redis GETDEL
// had (#3509, Codex P2), for both factors.
//
// Mutant killed: spending on a pool connection or a separate transaction.
func TestSpendToken_RollbackRestores(t *testing.T) {
	db := subjectTestDB(t)
	for _, factor := range []TokenFactor{FactorPassword, FactorWebAuthn} {
		t.Run(string(factor), func(t *testing.T) {
			userID := subjectTestUser(t, db)
			token := mintFor(t, db, userID, factor, PurposeChannelPurge)

			require.True(t, spendIn(t, db, userID, factor, PurposeChannelPurge, token, false))
			assert.True(t, spendIn(t, db, userID, factor, PurposeChannelPurge, token, true),
				"the rolled-back spend must leave the token spendable")
		})
	}
}

// A token for another purpose, factor or user matches no row: refused like an
// invalid code and NOT consumed (#3467).
//
// Mutants killed: dropping the purpose, the factor or the user from the match
// (the foreign spend succeeds); a refusal that deletes (the final spend fails).
func TestSpendToken_ForeignPurposeFactorOrUserRefusedAndNotConsumed(t *testing.T) {
	db := subjectTestDB(t)
	userID := subjectTestUser(t, db)
	other := subjectTestUser(t, db)
	token := mintFor(t, db, userID, FactorPassword, PurposeDMMessageDelete)

	assert.False(t, spendIn(t, db, userID, FactorPassword, PurposeMessageDelete, token, true), "wrong purpose")
	assert.False(t, spendIn(t, db, userID, FactorWebAuthn, PurposeDMMessageDelete, token, true), "wrong factor")
	assert.False(t, spendIn(t, db, other, FactorPassword, PurposeDMMessageDelete, token, true), "wrong user")
	assert.False(t, spendIn(t, db, userID, FactorPassword, PurposeDMMessageDelete, token+"x", true), "wrong token")
	assert.False(t, spendIn(t, db, userID, FactorPassword, PurposeDMMessageDelete, "", true), "empty token")
	assert.True(t, spendIn(t, db, userID, FactorPassword, PurposeDMMessageDelete, token, true),
		"no refusal may consume the token")
}

// Mutant killed: dropping expires_at > now() from the match.
func TestSpendToken_ExpiredRefused(t *testing.T) {
	db := subjectTestDB(t)
	userID := subjectTestUser(t, db)
	token := mintFor(t, db, userID, FactorPassword, PurposeServerPurge)
	_, err := db.Exec(`UPDATE step_up_tokens SET expires_at = now() - interval '1 second' WHERE user_id = $1`, userID)
	require.NoError(t, err)

	assert.False(t, spendIn(t, db, userID, FactorPassword, PurposeServerPurge, token, true))
	assert.Equal(t, 1, tokenRows(t, db, userID), "an expired token is refused, not consumed; the sweep removes it")
}

// A password change or credential revocation between mint and use strands the
// token, in both directions of the NULL-epoch case.
//
// Mutant killed: dropping the epoch comparison from the spend, or comparing
// with = (NULL = NULL is not true, so a never-rotated account could never
// spend).
func TestSpendToken_EpochBumpBetweenMintAndUseRefused(t *testing.T) {
	db := subjectTestDB(t)
	ctx := context.Background()

	t.Run("never rotated, then rotated", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		token := mintFor(t, db, userID, FactorPassword, PurposeDMClear)
		_, err := db.Exec(`UPDATE users SET credential_epoch = '22222222222222222222222222222222' WHERE id = $1`, userID)
		require.NoError(t, err)
		assert.False(t, spendIn(t, db, userID, FactorPassword, PurposeDMClear, token, true))
	})

	t.Run("rotated, then rotated again", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		_, err := db.Exec(`UPDATE users SET credential_epoch = '11111111111111111111111111111111' WHERE id = $1`, userID)
		require.NoError(t, err)
		token, e := MintToken(ctx, db, userID, FactorWebAuthn, PurposeDMClear, epochOne)
		require.Nil(t, e)
		control, e := MintToken(ctx, db, userID, FactorWebAuthn, PurposeDMMessageDelete, epochOne)
		require.Nil(t, e)
		require.True(t, spendIn(t, db, userID, FactorWebAuthn, PurposeDMMessageDelete, control, true),
			"control: an unrotated token spends")

		_, err = db.Exec(`UPDATE users SET credential_epoch = '22222222222222222222222222222222' WHERE id = $1`, userID)
		require.NoError(t, err)
		assert.False(t, spendIn(t, db, userID, FactorWebAuthn, PurposeDMClear, token, true))
	})
}

// The mint fences the session's epoch under the users-row lock, so a session
// a rotation revoked cannot mint, and a missing account cannot either.
//
// Mutant killed: dropping the MatchEpoch check (the stale session mints).
func TestMintToken_FencesTheSessionEpoch(t *testing.T) {
	db := subjectTestDB(t)
	userID := subjectTestUser(t, db)
	_, err := db.Exec(`UPDATE users SET credential_epoch = '22222222222222222222222222222222' WHERE id = $1`, userID)
	require.NoError(t, err)

	_, e := MintToken(context.Background(), db, userID, FactorPassword, PurposeDMClear, epochOne)
	require.NotNil(t, e)
	assert.Equal(t, http.StatusUnauthorized, e.Status)
	assert.True(t, e.EpochMismatch())
	assert.Zero(t, tokenRows(t, db, userID))

	_, e = MintToken(context.Background(), db, "00000000-0000-0000-0000-000000000000", FactorPassword, PurposeDMClear, "")
	require.NotNil(t, e)
	assert.Equal(t, http.StatusUnauthorized, e.Status)
	assert.Equal(t, ErrMsgSessionNoLongerValid, e.Body["error"])
}

// Growth bound: a mint keeps at most 16 rows for its user, dropping the oldest
// first, and deletes that user's expired rows.
//
// Mutants killed: dropping the cap (17 rows), keeping the oldest instead of the
// newest (the newest is gone), dropping the expired-row cleanup.
func TestMintToken_CapsAUsersLiveRowsAtSixteenOldestFirst(t *testing.T) {
	db := subjectTestDB(t)
	userID := subjectTestUser(t, db)
	require.Equal(t, 16, MaxLiveTokensPerUser)

	tokens := make([]string, 0, 17)
	for range 17 {
		tokens = append(tokens, mintFor(t, db, userID, FactorPassword, PurposeMessageDelete))
	}

	assert.Equal(t, 16, tokenRows(t, db, userID))
	assert.False(t, spendIn(t, db, userID, FactorPassword, PurposeMessageDelete, tokens[0], true), "the oldest is dropped")
	assert.True(t, spendIn(t, db, userID, FactorPassword, PurposeMessageDelete, tokens[1], true), "the second-oldest survives")
	assert.True(t, spendIn(t, db, userID, FactorPassword, PurposeMessageDelete, tokens[16], true), "the newest survives")

	_, err := db.Exec(`UPDATE step_up_tokens SET expires_at = now() - interval '1 second' WHERE user_id = $1`, userID)
	require.NoError(t, err)
	mintFor(t, db, userID, FactorWebAuthn, PurposeDMClear)
	assert.Equal(t, 1, tokenRows(t, db, userID), "a mint deletes its user's expired rows")
}

// The sweep deletes expired rows for every user and leaves live ones.
//
// Mutant killed: a sweep that deletes live rows (the live token stops
// spending) or deletes nothing.
func TestSweepExpiredTokens(t *testing.T) {
	db := subjectTestDB(t)
	expiredUser := subjectTestUser(t, db)
	liveUser := subjectTestUser(t, db)
	mintFor(t, db, expiredUser, FactorPassword, PurposeDMClear)
	live := mintFor(t, db, liveUser, FactorPassword, PurposeDMClear)
	_, err := db.Exec(`UPDATE step_up_tokens SET expires_at = now() - interval '1 second' WHERE user_id = $1`, expiredUser)
	require.NoError(t, err)

	n, err := SweepExpiredTokens(context.Background(), db)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))
	assert.Zero(t, tokenRows(t, db, expiredUser))
	assert.True(t, spendIn(t, db, liveUser, FactorPassword, PurposeDMClear, live, true))
}
