package stepup

// Concurrency and isolation properties of the step-up token store (#3509),
// from the adversarial review's proofs of concept. The sequential tests in
// token_test.go pin each refusal; these pin what only contention shows: one
// winner among simultaneous spenders, restoration under a storm of aborts, a
// cap that stays exact under simultaneous mints, and isolation between users,
// factors and purposes sharing one table.

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// race runs fn on n goroutines released together, and returns how many of
// them reported an error.
func race(n int, fn func() error) int64 {
	var (
		wg     sync.WaitGroup
		failed atomic.Int64
	)
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := fn(); err != nil {
				failed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	return failed.Load()
}

// Many consumers spend one token at once, each on its own transaction, and
// each commits what it got. Exactly one may see the token live.
//
// Mutant killed: a spend that reads the row instead of deleting it.
func TestSpendToken_ConcurrentSpendersSpendItOnce(t *testing.T) {
	db := subjectTestDB(t)
	userID := subjectTestUser(t, db)
	token := mintFor(t, db, userID, FactorPassword, PurposeDMClear)
	var spent atomic.Int64

	failed := race(16, func() error {
		tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		ok, err := SpendToken(context.Background(), tx, userID, FactorPassword, PurposeDMClear, token)
		if err != nil {
			return err
		}
		if ok {
			spent.Add(1)
		}
		return tx.Commit()
	})

	require.Zero(t, failed)
	assert.Equal(t, int64(1), spent.Load(), "exactly one racer may see the token as live")
	assert.Zero(t, tokenRows(t, db, userID), "the winning spend consumed the row")
}

// Many consumers spend one token at once and every one of them rolls back.
// The token survives the storm and is still spendable, once.
func TestSpendToken_ConcurrentRollbacksAllRestore(t *testing.T) {
	db := subjectTestDB(t)
	userID := subjectTestUser(t, db)
	token := mintFor(t, db, userID, FactorPassword, PurposeMessageDelete)

	failed := race(8, func() error {
		tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		_, err = SpendToken(context.Background(), tx, userID, FactorPassword, PurposeMessageDelete, token)
		return err
	})

	require.Zero(t, failed)
	assert.Equal(t, 1, tokenRows(t, db, userID), "no rolled-back racer consumed the token")
	assert.True(t, spendIn(t, db, userID, FactorPassword, PurposeMessageDelete, token, true))
	assert.False(t, spendIn(t, db, userID, FactorPassword, PurposeMessageDelete, token, true))
}

// Simultaneous mints for one user never leave more than the cap.
//
// Mutant killed: the mint's users-row lock taken FOR SHARE (concurrent mints
// each miss the others' rows and the user ends above 16).
func TestMintToken_ConcurrentMintsStayWithinTheCap(t *testing.T) {
	db := subjectTestDB(t)
	userID := subjectTestUser(t, db)

	failed := race(40, func() error {
		if _, e := MintToken(context.Background(), db, userID, FactorPassword, PurposeDMClear, ""); e != nil {
			return e
		}
		return nil
	})

	require.Zero(t, failed)
	assert.Equal(t, MaxLiveTokensPerUser, tokenRows(t, db, userID))
}

// One user's flood of mints evicts only that user's rows.
//
// Mutant killed: a cap or expired-row cleanup not scoped to the minting user.
func TestMintToken_OneUsersCapNeverEvictsAnothersToken(t *testing.T) {
	db := subjectTestDB(t)
	victim := subjectTestUser(t, db)
	flooder := subjectTestUser(t, db)
	victimToken := mintFor(t, db, victim, FactorPassword, PurposeDMClear)

	for range 3 * MaxLiveTokensPerUser {
		mintFor(t, db, flooder, FactorPassword, PurposeMessageDelete)
	}

	assert.Equal(t, MaxLiveTokensPerUser, tokenRows(t, db, flooder))
	assert.Equal(t, 1, tokenRows(t, db, victim))
	assert.True(t, spendIn(t, db, victim, FactorPassword, PurposeDMClear, victimToken, true))
}

// With both factors and two purposes live for one user at once, a consumer
// takes only the row matching its own factor and purpose, and its refusals
// leave the others in place.
func TestSpendToken_FactorsAndPurposesCoexistWithoutCrossing(t *testing.T) {
	db := subjectTestDB(t)
	userID := subjectTestUser(t, db)
	passwordClear := mintFor(t, db, userID, FactorPassword, PurposeDMClear)
	webauthnClear := mintFor(t, db, userID, FactorWebAuthn, PurposeDMClear)
	passwordDelete := mintFor(t, db, userID, FactorPassword, PurposeMessageDelete)

	assert.False(t, spendIn(t, db, userID, FactorPassword, PurposeDMClear, webauthnClear, true), "another factor")
	assert.False(t, spendIn(t, db, userID, FactorPassword, PurposeDMClear, passwordDelete, true), "another purpose")
	assert.True(t, spendIn(t, db, userID, FactorPassword, PurposeDMClear, passwordClear, true))
	assert.True(t, spendIn(t, db, userID, FactorWebAuthn, PurposeDMClear, webauthnClear, true), "untouched by the refusals")
	assert.True(t, spendIn(t, db, userID, FactorPassword, PurposeMessageDelete, passwordDelete, true), "untouched by the refusals")
}
