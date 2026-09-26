package mfa_test

// Reproduction test for a TOTPVerifySetup check-then-act race
// (internal/mfa/handlers.go): it reads the pending TOTP secret with a plain,
// non-locking SELECT, validates the submitted code against it, and only then
// runs `UPDATE user_mfa_totp SET enabled = TRUE, ... WHERE user_id = $4 AND
// enabled = FALSE`. If TOTPSetup re-enrolls (writing a new pending secret,
// also enabled = FALSE) between the SELECT and the UPDATE, the UPDATE's
// WHERE clause still matches and enables the new secret — one the submitted
// code was never validated against.
//
// This file does not fix the defect and touches no production code.

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTOTPVerifySetup_SecretSwapDuringVerifyEnablesNothing pins the oracle:
// verify-setup may enable the TOTP factor only if the secret it validated the
// submitted code against is still the one stored for the user; otherwise it
// must enable nothing and answer with a non-200.
//
// Lock recipe (no production seam, no time.Sleep): a transaction holds
// `SELECT 1 FROM user_mfa_totp WHERE user_id = $1 FOR UPDATE`, which blocks
// verify-setup's later UPDATE but not its earlier, non-locking SELECT. That
// gives a deterministic window — observed via pg_stat_activity, not timing —
// in which to simulate a concurrent re-enrollment (TOTPSetup's own upsert,
// replayed here by hand) before releasing the lock.
func TestTOTPVerifySetup_SecretSwapDuringVerifyEnablesNothing(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "totpswap")
	auth := testhelpers.AuthHeaders(user.AccessToken)

	// Step 1: enroll secret A (pending, unverified) and compute a code for it
	// that will validate under MatchCodeStep.
	w := ts.DoRequest("POST", urlTOTPSetup, map[string]interface{}{
		"password": testPassword,
	}, auth)
	require.Equal(t, http.StatusOK, w.Code, "setup failed: %s", w.Body.String())
	var setupBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &setupBody)
	secretA := testhelpers.JSONField[string](t, setupBody, "secret") // pragma: allowlist secret -- test TOTP seed from response body
	codeA := stepReplayCode(t, secretA, time.Now())

	// Step 2: seal a second secret (B) exactly as the server's own upsert
	// would for a re-enrollment, using the fixed all-zero test keyring
	// (mirrors testhelpers.SetupTestServer's MFA_ENCRYPTION_KEY).
	ring, err := mfa.ParseKeyring(totpStepReplayEncKey, 1, "")
	require.NoError(t, err)
	keyB, err := mfa.GenerateSecret("swap@test.local")
	require.NoError(t, err)
	encB, nonceB, verB, err := ring.Seal([]byte(keyB.Secret())) // pragma: allowlist secret -- sealed swap-in TOTP seed
	require.NoError(t, err)

	// Step 3: hold a row lock on the user's user_mfa_totp row.
	lockTx, err := ts.DB.Begin()
	require.NoError(t, err)
	// A failed require below must not leave this transaction holding the
	// row lock forever; Rollback after Commit is a no-op.
	defer func() { _ = lockTx.Rollback() }()
	var one int
	require.NoError(t, lockTx.QueryRow(
		`SELECT 1 FROM user_mfa_totp WHERE user_id = $1 FOR UPDATE`, user.ID,
	).Scan(&one))

	// Step 4: fire verify-setup with codeA concurrently. Its SELECT is not
	// blocked by the row lock, so it reads secret A and validates codeA; its
	// UPDATE then blocks on the lock held above.
	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		respCh <- ts.DoRequest("POST", urlTOTPVerifySetup, map[string]interface{}{
			"code": codeA,
		}, auth)
	}()

	// Step 5: wait until verify-setup's UPDATE is observed blocked on the
	// held lock. Positive control: if the response arrives first, the
	// SELECT-then-UPDATE interleaving this test needs was not achieved.
	var mu sync.Mutex
	var earlyResp *httptest.ResponseRecorder
	require.Eventually(t, func() bool {
		mu.Lock()
		already := earlyResp != nil
		mu.Unlock()
		if already {
			return true
		}
		select {
		case resp := <-respCh:
			mu.Lock()
			earlyResp = resp
			mu.Unlock()
			return true
		default:
		}
		var waiting int
		qerr := ts.DB.QueryRow(`
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND datname = current_database()
			  AND pid <> pg_backend_pid() AND query ILIKE '%UPDATE user_mfa_totp%enabled = TRUE%'`,
		).Scan(&waiting)
		return qerr == nil && waiting > 0
	}, 10*time.Second, 20*time.Millisecond, "TOTPVerifySetup's UPDATE was never observed blocked on the held row lock")

	mu.Lock()
	resp := earlyResp
	mu.Unlock()
	require.Nil(t, resp,
		"TOTPVerifySetup_SecretSwap positive control failed: verify-setup's response arrived before the row-lock wait was observed, so the required SELECT-then-UPDATE interleaving was not achieved")

	// Step 6: simulate TOTPSetup's re-enrollment upsert, writing secret B
	// pending (enabled = FALSE, matching what the blocked UPDATE's WHERE
	// clause requires), then release the lock.
	_, err = lockTx.Exec(`
		UPDATE user_mfa_totp
		SET totp_secret_enc = $2, totp_secret_nonce = $3, key_version = $4,
		    enabled = FALSE, confirmed = FALSE, verified_at = NULL, confirmed_at = NULL,
		    backup_codes_hash = '{}', backup_codes_used = '{}', last_used_step = NULL,
		    updated_at = NOW()
		WHERE user_id = $1
	`, user.ID, encB, nonceB, verB)
	require.NoError(t, err)
	require.NoError(t, lockTx.Commit())

	// Step 7: verify-setup's UPDATE now proceeds. Today it re-evaluates
	// `enabled = FALSE` on secret B's row, matches, and enables B — a secret
	// the request never proved a code against.
	select {
	case resp = <-respCh:
	case <-time.After(10 * time.Second):
		t.Fatal("TOTPVerifySetup_SecretSwap: verify-setup did not return after the row lock was released")
	}

	assert.NotEqual(t, http.StatusOK, resp.Code,
		"TOTPVerifySetup_SecretSwap: verify-setup must not enable a secret it never validated a code against; got 200: %s", resp.Body.String())

	var enabled bool
	var storedNonce []byte
	require.NoError(t, ts.DB.QueryRow(
		`SELECT enabled, totp_secret_nonce FROM user_mfa_totp WHERE user_id = $1`, user.ID,
	).Scan(&enabled, &storedNonce))
	assert.False(t, enabled,
		"TOTPVerifySetup_SecretSwap: the swapped-in secret must not be enabled by a code validated against the previous secret")
	assert.Equal(t, nonceB, storedNonce,
		"TOTPVerifySetup_SecretSwap: secret B must remain the stored pending secret, untouched by verify-setup's UPDATE")
}
