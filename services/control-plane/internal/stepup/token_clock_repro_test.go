package stepup

// Reproductions for the step-up token clock (#3509 review: migration M1,
// security L1). now() is the TRANSACTION's start, so every expiry and every
// created_at the store stamps or checks with it is as old as the transaction,
// not as old as the statement:
//
//   - a consumer whose transaction began before a token expired and then
//     waited on a lock spends the token after it expired;
//   - a mint that waited on the users-row lock behind other mints stamps its
//     own row older than theirs, so when the user is at the cap its own fresh
//     row is the one the cap evicts, and its 60 s shrinks by the wait.
//
// Both drive two transactions: one holds the users row while the other waits
// on it, which is exactly the wait production can see.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holdUsersRow locks userID's users row FOR NO KEY UPDATE on a transaction of
// its own and returns the transaction. The caller commits it to release.
func holdUsersRow(t *testing.T, db *sql.DB, userID string) *sql.Tx {
	t.Helper()
	holder, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	_, err = holder.Exec(`SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID)
	require.NoError(t, err)
	return holder
}

// regression for #3509 review (migration M1 / security L1), spend side
func TestSpendToken_ExpiryIsCheckedAtTheStatementNotTheTransaction(t *testing.T) {
	db := subjectTestDB(t)
	ctx := context.Background()
	userID := subjectTestUser(t, db)
	token := mintFor(t, db, userID, FactorPassword, PurposeDMClear)
	holder := holdUsersRow(t, db, userID)

	// The consumer's transaction starts now: its now() is fixed here.
	consumer, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = consumer.Rollback() }()
	_, err = consumer.Exec(`SELECT 1`)
	require.NoError(t, err)

	// The token expires 300 ms after the consumer began.
	_, err = db.Exec(`UPDATE step_up_tokens SET expires_at = clock_timestamp() + interval '300 milliseconds' WHERE user_id = $1`, userID)
	require.NoError(t, err)

	// The consumer takes the users row, as every own-rule route does first,
	// and waits behind the holder until well after expiry.
	locked := make(chan error, 1)
	go func() {
		_, lockErr := LockSubjectTx(ctx, consumer, userID, LockForShare, "")
		if lockErr != nil {
			locked <- lockErr
			return
		}
		locked <- nil
	}()
	time.Sleep(800 * time.Millisecond)
	require.NoError(t, holder.Commit())
	require.NoError(t, <-locked)

	spent, err := SpendToken(ctx, consumer, userID, FactorPassword, PurposeDMClear, token)
	require.NoError(t, err)
	assert.False(t, spent, "a token that expired while its consumer waited on a lock must not be spendable")
}

// regression for #3509 review (migration M1 / security L1), mint side
func TestMintToken_AWaitingMintKeepsItsOwnRowAndFullTTL(t *testing.T) {
	db := subjectTestDB(t)
	ctx := context.Background()
	userID := subjectTestUser(t, db)
	holder := holdUsersRow(t, db, userID)

	// The mint begins and blocks on the users row.
	type minted struct {
		token string
		err   *Error
	}
	done := make(chan minted, 1)
	go func() {
		token, e := MintToken(ctx, db, userID, FactorPassword, PurposeMessageDelete, "")
		done <- minted{token, e}
	}()
	time.Sleep(300 * time.Millisecond)

	// Other mints that started after it fill the user to the cap first. They
	// are inserted on the holder's transaction with statement-time stamps, as
	// a real mint that got the lock first would stamp them.
	for range MaxLiveTokensPerUser {
		_, err := holder.Exec(`
			INSERT INTO step_up_tokens (token_hash, user_id, factor, purpose, expires_at, created_at)
			VALUES (sha256(convert_to(gen_random_uuid()::text, 'UTF8')), $1, 'password', 'messages.delete', clock_timestamp() + interval '60 seconds', clock_timestamp())`,
			userID)
		require.NoError(t, err)
	}
	time.Sleep(1200 * time.Millisecond)
	require.NoError(t, holder.Commit())

	got := <-done
	require.Nil(t, got.err)
	assert.Equal(t, MaxLiveTokensPerUser, tokenRows(t, db, userID), "the cap still holds")

	var remaining float64
	err := db.QueryRow(`SELECT EXTRACT(EPOCH FROM expires_at - clock_timestamp()) FROM step_up_tokens WHERE token_hash = $1`,
		hashToken(got.token)).Scan(&remaining)
	require.NoError(t, err, "the waiting mint's own fresh row must survive the cap")
	assert.Greater(t, remaining, 59.0, "the waiting mint's token keeps its full 60 s from the insert, not from when its transaction began")
	assert.True(t, spendIn(t, db, userID, FactorPassword, PurposeMessageDelete, got.token, true),
		"the token the mint returned is spendable")
}
