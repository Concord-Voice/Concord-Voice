package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #2450: the SSO mint (IssueAccessAndRefresh) must serialize against a destructive
// reset by taking the users row FOR NO KEY UPDATE, the same lock ChangePassword /
// ReplaceMyKeys / RecoveryReset* hold.
//
// Before the fix it read credential_epoch on a bare pool connection and INSERTed the
// refresh row in a separate autocommit statement, so a reset could bulk-revoke,
// commit, and still leave the freshly-inserted row un-swept and live —
// rotateAndRespond would then rotate it into a fully valid post-reset session
// derived from pre-reset authorization (CWE-367 -> CWE-613).
//
// Asserting the LOCK rather than racing goroutines keeps this deterministic: a true
// interleaving test would be timing-dependent, whereas "does this call block behind
// the reset's lock" is exactly the property that makes the interleaving impossible.
// Reverting the fix makes this test fail — the unfenced version never blocks.
func TestIssueAccessAndRefreshBlocksOnUserRowLock(t *testing.T) {
	h, db, userID := newAdmitPathHandler(t)
	ctx := context.Background()

	// Stand in for a destructive reset: hold the users row exactly as those flows do.
	resetTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = resetTx.Rollback() }()

	var lockedID string
	require.NoError(t, resetTx.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID,
	).Scan(&lockedID))

	// The mint runs on a cancellable context so an early t.Fatal below cannot leave it
	// mid-query while t.Cleanup tears the pool down — that races into a
	// "sql: database is closed" panic on a non-test goroutine, crashing the package run
	// and masking the real failure.
	mintCtx, cancelMint := context.WithCancel(ctx)
	defer cancelMint()

	done := make(chan error, 1)
	go func() {
		_, _, _, mintErr := h.IssueAccessAndRefresh(mintCtx, userID.String())
		done <- mintErr
	}()

	// The mint must not complete while the reset holds the row. A correct implementation
	// blocks INDEFINITELY here, so a loaded runner makes this assertion more certain, not
	// less — the window is not flaky in the failing direction.
	//
	// Note the mint resolves entCache.GetTier (Redis, DB read-through on miss) BEFORE
	// BeginTx, so in principle a pathologically slow Redis could consume the window
	// without the goroutine reaching the lock at all, passing this vacuously. The
	// post-release assertions below (mint succeeds, exactly one live row) are what make
	// the test meaningful in that case.
	select {
	case err := <-done:
		t.Fatalf("IssueAccessAndRefresh completed while the users row was locked "+
			"(err=%v) — expected it to block on the lock. If err is a lock/statement "+
			"timeout rather than nil, the mint may be serialized correctly and the test "+
			"environment is at fault; otherwise the mint is not serialized against a "+
			"destructive reset and a concurrent reset can leave its refresh row "+
			"un-swept (#2450)", err)
	case <-time.After(750 * time.Millisecond):
		// Blocked as required.
	}

	// Releasing the reset lets the mint proceed.
	require.NoError(t, resetTx.Rollback())

	select {
	case err := <-done:
		require.NoError(t, err, "mint should succeed once the reset releases the row")
	case <-time.After(10 * time.Second):
		t.Fatal("IssueAccessAndRefresh did not complete after the users row was released")
	}

	// The refresh row is committed and live — proving the INSERT rode the same
	// transaction as the locked read rather than landing on a separate connection.
	var live int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`,
		userID,
	).Scan(&live))
	require.Equal(t, 1, live, "expected exactly one live refresh row after the mint")
}

// A reset that commits BEFORE the SSO mint acquires the row is the acceptable
// ordering: the sign-in is genuinely post-reset, so it mints against the advanced
// epoch. This pins that half of the contract so a future change cannot "fix" the
// race by refusing every SSO sign-in that follows a reset.
func TestIssueAccessAndRefreshSucceedsAfterCommittedReset(t *testing.T) {
	h, db, userID := newAdmitPathHandler(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx,
		`UPDATE users SET credential_epoch = $2 WHERE id = $1`, userID, "epoch-after-reset")
	require.NoError(t, err)

	access, refresh, sessionID, err := h.IssueAccessAndRefresh(ctx, userID.String())
	require.NoError(t, err)
	require.NotEmpty(t, access)
	require.NotEmpty(t, refresh)
	require.NotEmpty(t, sessionID)

	var live int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`,
		userID,
	).Scan(&live))
	require.Equal(t, 1, live)
}
