package mfa

// Reproduction for the WebAuthn inline-token rollback defect (#3509, Codex P2).
//
// consumeWebAuthnInlineToken spends a token with a Redis GETDEL. VerifyCodeTx
// runs it while the caller's SQL transaction is open, and no rollback reaches
// Redis, so a transaction that verified the token and then aborted leaves the
// token spent. TOTP steps and backup codes are spent on the transaction's own
// connection and ARE restored by a rollback
// (TestTOTPStepReplay_GuardRolledBackVerificationLeavesCodeUsable), which is
// the contract the inline token breaks.
//
// Oracle: after a transaction in which VerifyCodeTx accepted a WebAuthn inline
// token rolls back, the same token is still accepted by a second VerifyCodeTx
// for the same user and purpose.
//
// The token comes from a REAL ceremony (ipMintInlineToken drives begin and
// finish), never a seeded key, so this file does not depend on where a token is
// stored. The fix moves tokens from Redis into Postgres step_up_tokens, and
// these tests need no change for it.
//
// TestInlineTokenRollback_RolledBackVerificationLeavesTokenSpendable is
// expected to FAIL on the current tree. The other two are controls that pass
// today and after the fix.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// rollbackPurpose is the purpose the token is minted for and spent under.
// rollbackOtherPurpose is a different valid one for the wrong-purpose control.
const (
	rollbackPurpose      = stepup.PurposeMessageDelete
	rollbackOtherPurpose = stepup.PurposeDMMessageDelete
)

// verifyInTx runs VerifyCodeTx on a fresh transaction, then commits or rolls
// back as asked, and returns what VerifyCodeTx answered.
func verifyInTx(t *testing.T, h *Handler, userID string, purpose stepup.Purpose, token string, commit bool) bool {
	t.Helper()
	tx, err := h.db.Begin()
	require.NoError(t, err)
	// A failed require below must not leave the transaction open. Rollback
	// after Commit or Rollback is a no-op.
	defer func() { _ = tx.Rollback() }()
	ok, err := h.VerifyCodeTx(context.Background(), tx, userID, purpose, token)
	require.NoError(t, err)
	if commit {
		require.NoError(t, tx.Commit())
	} else {
		require.NoError(t, tx.Rollback())
	}
	return ok
}

// regression for #3509 (Codex P2)
func TestInlineTokenRollback_RolledBackVerificationLeavesTokenSpendable(t *testing.T) {
	db := iuNewTestDB(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))

	token := ipMintInlineToken(t, h, rdb, auth, userID, string(rollbackPurpose))

	// Precondition, and the arm being reached: the transaction's own
	// verification accepted the token. Without this, a refusal below could be
	// a token that never worked.
	require.True(t, verifyInTx(t, h, userID, rollbackPurpose, token, false),
		"precondition: VerifyCodeTx must accept a freshly minted token for its own purpose")

	// The transaction rolled back, so nothing it did may stand.
	assert.True(t, verifyInTx(t, h, userID, rollbackPurpose, token, true),
		"a WebAuthn inline token accepted inside a transaction that then rolled back must be accepted again; "+
			"the GETDEL cannot be rolled back, so the token stayed spent")
}

// Control: a committed transaction spends the token. Without this, the
// repro above would also pass for a token that is never spent at all.
func TestInlineTokenRollback_CommittedVerificationSpendsToken(t *testing.T) {
	db := iuNewTestDB(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))

	token := ipMintInlineToken(t, h, rdb, auth, userID, string(rollbackPurpose))

	require.True(t, verifyInTx(t, h, userID, rollbackPurpose, token, true),
		"precondition: the first verification must accept the token")
	assert.False(t, verifyInTx(t, h, userID, rollbackPurpose, token, true),
		"a token spent by a committed transaction must not be accepted a second time")
}

// Control: a token minted for another purpose is refused without being
// consumed, and a rolled-back foreign attempt leaves it spendable for its own.
func TestInlineTokenRollback_WrongPurposeRefusedAndNotConsumed(t *testing.T) {
	db := iuNewTestDB(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))

	token := ipMintInlineToken(t, h, rdb, auth, userID, string(rollbackPurpose))

	assert.False(t, verifyInTx(t, h, userID, rollbackOtherPurpose, token, true),
		"a token minted for %q must be refused under %q", rollbackPurpose, rollbackOtherPurpose)
	assert.True(t, verifyInTx(t, h, userID, rollbackPurpose, token, true),
		"the refusal must not have consumed the token: its own purpose must still accept it")
}
