package mfa_test

// Reproduction tests for the TOTP step-replay defect (RFC 6238 §5.2):
// ValidateCode (internal/mfa/totp.go:47) accepts any code inside a 3-step
// (±1 skew) window with nothing recording which step a user last used, so one
// TOTP code is accepted every time it is submitted until its window closes,
// on any route. These tests pin the oracle: a TOTP code is accepted at most
// once per user per time step, across every route; a refused replay looks
// exactly like an invalid code. Tests 1–6 are expected to FAIL against the
// current tree. Tests 7–9 are regression guards: green today and green after
// the fix.
//
// This file does not fix the defect and touches no production code.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stepReplayPurpose is the step-up purpose these tests verify under. TOTP and
// backup codes ignore the purpose; the WebAuthn inline token guard below is
// minted for it.
const stepReplayPurpose = stepup.PurposeBackupEmailSet

// totpStepReplayEncKey mirrors testhelpers.SetupTestServer's fixed all-zero
// MFA_ENCRYPTION_KEY (internal/testhelpers/testserver.go) so a Handler built
// directly in this file can decrypt TOTP secrets sealed by the HTTP router's
// own mfa.Handler against the same ts.DB.
const totpStepReplayEncKey = "0000000000000000000000000000000000000000000000000000000000000000"

// stepReplayCode generates a TOTP code for t using the same parameters
// ValidateCode uses internally (internal/mfa/totp.go).
func stepReplayCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)
	return code
}

// newDirectHandler builds an *mfa.Handler wired to the test server's own DB
// and Redis, for tests that call VerifyCode/VerifyCodeTx directly rather than
// through the HTTP router. It uses the same fixed MFA encryption key
// testhelpers.SetupTestServer configures, so it decrypts secrets sealed by
// the router's own handler on the same connections.
func newDirectHandler(t *testing.T, ts *testhelpers.TestServer) *mfa.Handler {
	t.Helper()
	keyring, err := mfa.ParseKeyring(totpStepReplayEncKey, 1, "")
	require.NoError(t, err)
	log := logger.New("test")
	return mfa.NewHandler(ts.DB, ts.Redis, log, keyring, testhelpers.TestJWTSecret, nil, "test")
}

// verifyCodeConcurrently fires n goroutines at h.VerifyCode with the same
// code, released together off a closed channel (no time.Sleep), and fails
// the test if they do not all complete inside timeout.
func verifyCodeConcurrently(t *testing.T, h *mfa.Handler, userID, code string, n int) []bool {
	t.Helper()
	results := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			ok, err := h.VerifyCode(context.Background(), userID, stepReplayPurpose, code)
			results[i] = ok
			errs[i] = err
		}()
	}
	done := make(chan struct{})
	go func() {
		close(start)
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("TOTPStepReplay: concurrent VerifyCode calls did not complete within timeout")
	}
	for i, err := range errs {
		require.NoError(t, err, "VerifyCode goroutine %d returned an error", i)
	}
	return results
}

// TestTOTPStepReplay_SamePathRejectsImmediateReplay pins: one valid TOTP code
// verified twice through the same path (Handler.VerifyCode) must be accepted
// once and refused the second time.
func TestTOTPStepReplay_SamePathRejectsImmediateReplay(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepreplay1")
	secret, _ := enrollTOTP(t, ts, user)
	h := newDirectHandler(t, ts)

	code := stepReplayCode(t, secret, time.Now())

	ok1, err1 := h.VerifyCode(context.Background(), user.ID, stepReplayPurpose, code)
	require.NoError(t, err1)
	require.True(t, ok1, "the first submission of a valid code must be accepted")

	ok2, err2 := h.VerifyCode(context.Background(), user.ID, stepReplayPurpose, code)
	require.NoError(t, err2)
	assert.False(t, ok2,
		"TOTPStepReplay: a TOTP code already accepted once must be refused when submitted again through the same path (Handler.VerifyCode), but it was accepted a second time")
}

// TestTOTPStepReplay_AcceptedOnOneRouteRefusedOnAnother pins: a code accepted
// on one route (login MFA verify) must be refused on a different route
// (RegenerateBackupCodes) inside the same window, because both call the same
// underlying verifyCodeMatchedMethod with no cross-route replay tracking.
func TestTOTPStepReplay_AcceptedOnOneRouteRefusedOnAnother(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepreplay2")
	secret, _ := enrollTOTP(t, ts, user)

	// Route A: login challenge + verify.
	w := ts.DoRequest("POST", urlAuthLogin, map[string]interface{}{
		"email":    user.Email,
		"password": testPassword,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, "login failed: %s", w.Body.String())
	var loginBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &loginBody)
	challengeToken, ok := loginBody["mfa_challenge_token"].(string)
	require.True(t, ok, "expected an MFA challenge token from login")

	code := stepReplayCode(t, secret, time.Now())

	w = ts.DoRequest("POST", urlMFAVerify, map[string]interface{}{
		"mfa_challenge_token": challengeToken,
		"method":              "totp",
		"code":                code,
	}, nil)
	require.Equal(t, http.StatusOK, w.Code, "route A (login verify) should accept the code once: %s", w.Body.String())

	// Route B: RegenerateBackupCodes — a different handler that verifies TOTP
	// codes through the same underlying verifyCodeMatchedMethod path.
	w = ts.DoRequest("POST", urlBackupCodesRegen, map[string]interface{}{
		"password": testPassword,
		"code":     code,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code,
		"TOTPStepReplay: a code already consumed on the login-verify route must be refused on a different route (RegenerateBackupCodes) within the same step; got %d: %s", w.Code, w.Body.String())
}

// TestTOTPStepReplay_ConcurrentSubmissionsAcceptExactlyOnce pins: N=8
// concurrent verifications of the same valid code, released together by a
// barrier, must accept exactly one.
func TestTOTPStepReplay_ConcurrentSubmissionsAcceptExactlyOnce(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepreplay3")
	secret, _ := enrollTOTP(t, ts, user)
	h := newDirectHandler(t, ts)

	code := stepReplayCode(t, secret, time.Now())

	const n = 8
	results := verifyCodeConcurrently(t, h, user.ID, code, n)

	accepted := 0
	for _, ok := range results {
		if ok {
			accepted++
		}
	}
	assert.Equal(t, 1, accepted,
		"TOTPStepReplay: exactly one of %d concurrent verifications of the same code must be accepted; got %d accepted", n, accepted)
}

// TestTOTPStepReplay_EarlierStepRefusedAfterLaterStepAccepted pins: at time T,
// the code for T is accepted; the code for T-30s (an earlier step) must then
// be refused, even though it is independently valid under the ±1 skew window.
//
// Millisecond-scale note: if the process is scheduled right on a 30s step
// boundary between generating "now" and the first VerifyCode call, "now" and
// the verification instant can straddle a boundary. That does not undermine
// this test: codeNow is generated and verified first (skew keeps it valid
// across one boundary crossing), and codePrev names a step strictly earlier
// than codeNow's step by construction (now.Add(-30s)), so the ordering this
// test asserts on holds regardless of exactly when either call lands.
func TestTOTPStepReplay_EarlierStepRefusedAfterLaterStepAccepted(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepreplay4")
	secret, _ := enrollTOTP(t, ts, user)
	h := newDirectHandler(t, ts)

	now := time.Now()
	codeNow := stepReplayCode(t, secret, now)
	codePrev := stepReplayCode(t, secret, now.Add(-30*time.Second))

	okNow, err := h.VerifyCode(context.Background(), user.ID, stepReplayPurpose, codeNow)
	require.NoError(t, err)
	require.True(t, okNow, "the current-step code must be accepted")

	okPrev, err := h.VerifyCode(context.Background(), user.ID, stepReplayPurpose, codePrev)
	require.NoError(t, err)
	assert.False(t, okPrev,
		"TOTPStepReplay: a code from a step earlier than the last accepted step must be refused, even though it independently validates under the skew window")
}

// TestTOTPStepReplay_RegenerateBackupCodesRejectsReplay pins: the code
// accepted by one RegenerateBackupCodes call must be refused by a second
// RegenerateBackupCodes call within the same window.
func TestTOTPStepReplay_RegenerateBackupCodesRejectsReplay(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepreplay5")
	secret, _ := enrollTOTP(t, ts, user)

	code := stepReplayCode(t, secret, time.Now())
	auth := testhelpers.AuthHeaders(user.AccessToken)

	w := ts.DoRequest("POST", urlBackupCodesRegen, map[string]interface{}{
		"password": testPassword,
		"code":     code,
	}, auth)
	require.Equal(t, http.StatusOK, w.Code, "first regeneration should succeed: %s", w.Body.String())

	w = ts.DoRequest("POST", urlBackupCodesRegen, map[string]interface{}{
		"password": testPassword,
		"code":     code,
	}, auth)
	assert.Equal(t, http.StatusForbidden, w.Code,
		"TOTPStepReplay: a code already used for one backup-code regeneration must be refused on a second regeneration within the same step; got %d: %s", w.Code, w.Body.String())
}

// TestTOTPStepReplay_VerifySetupDoubleSubmitPersistsOnlyOneWinner pins: two
// concurrent TOTPVerifySetup submissions of one valid code must produce
// exactly one 200, and that response's backup codes must be the ones
// actually persisted to user_mfa_totp.backup_codes_hash.
func TestTOTPStepReplay_VerifySetupDoubleSubmitPersistsOnlyOneWinner(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepreplay6")
	auth := testhelpers.AuthHeaders(user.AccessToken)

	w := ts.DoRequest("POST", urlTOTPSetup, map[string]interface{}{
		"password": testPassword,
	}, auth)
	require.Equal(t, http.StatusOK, w.Code, "setup failed: %s", w.Body.String())
	var setupBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &setupBody)
	secret := testhelpers.JSONField[string](t, setupBody, "secret")

	code := stepReplayCode(t, secret, time.Now())

	// n=5 rather than the minimal 2: the race window between TOTPVerifySetup's
	// SELECT of `enabled` and its unconditional UPDATE is narrow, so firing
	// only two goroutines sometimes has the second start its SELECT after the
	// first's UPDATE has already committed, which would report the (already
	// correct) 200/403 split rather than the double-accept defect. 5 is the
	// route's own per-minute rate-limit ceiling (internal/api/router.go,
	// `/totp/verify-setup`), so it is the most goroutines this test can add
	// without a 429 becoming a second, unrelated way for it to fail.
	const n = 5
	results := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			results[i] = ts.DoRequest("POST", urlTOTPVerifySetup, map[string]interface{}{
				"code": code,
			}, auth)
		}()
	}
	done := make(chan struct{})
	go func() {
		close(start)
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("TOTPStepReplay: concurrent TOTPVerifySetup calls did not complete within timeout")
	}

	var successBodies []map[string]interface{}
	for _, w := range results {
		if w.Code == http.StatusOK {
			var body map[string]interface{}
			testhelpers.ParseJSON(t, w, &body)
			successBodies = append(successBodies, body)
		}
	}

	require.NotEmpty(t, successBodies, "at least one concurrent verify-setup submission must succeed")
	assert.Len(t, successBodies, 1,
		"TOTPStepReplay: exactly one of %d concurrent TOTPVerifySetup submissions of the same code must return 200; got %d", n, len(successBodies))

	var storedHashes []string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT backup_codes_hash FROM user_mfa_totp WHERE user_id = $1`, user.ID,
	).Scan(pq.Array(&storedHashes)))

	for _, body := range successBodies {
		codes, ok := body["backup_codes"].([]interface{})
		require.True(t, ok, "a 200 verify-setup response must carry backup_codes")
		for _, c := range codes {
			codeStr, isString := c.(string)
			require.True(t, isString, "each backup code must be a string")
			sum := sha256.Sum256([]byte(codeStr))
			hexSum := hex.EncodeToString(sum[:])
			assert.Contains(t, storedHashes, hexSum,
				"TOTPStepReplay: a 200 TOTPVerifySetup response's backup codes must match what is actually stored in user_mfa_totp.backup_codes_hash")
		}
	}
}

// TestTOTPStepReplay_GuardNextStepAcceptedAfterPreviousStep is a regression
// guard (green today and after the fix): a code from the next step is
// accepted after a code from the previous step, i.e. forward movement is
// never blocked by a per-step replay guard.
func TestTOTPStepReplay_GuardNextStepAcceptedAfterPreviousStep(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepreplay7")
	secret, _ := enrollTOTP(t, ts, user)
	h := newDirectHandler(t, ts)

	now := time.Now()
	codeNow := stepReplayCode(t, secret, now)
	codeNext := stepReplayCode(t, secret, now.Add(30*time.Second))

	okNow, err := h.VerifyCode(context.Background(), user.ID, stepReplayPurpose, codeNow)
	require.NoError(t, err)
	require.True(t, okNow, "guard: the current-step code must be accepted")

	okNext, err := h.VerifyCode(context.Background(), user.ID, stepReplayPurpose, codeNext)
	require.NoError(t, err)
	assert.True(t, okNext,
		"guard: a code from the step after the last accepted one must be accepted")
}

// TestTOTPStepReplay_GuardRolledBackVerificationLeavesCodeUsable is a
// regression guard: inside VerifyCodeTx, a verification whose transaction is
// rolled back must leave the code usable — a later verification of the same
// code in a new transaction (or no transaction) must succeed.
func TestTOTPStepReplay_GuardRolledBackVerificationLeavesCodeUsable(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepreplay8")
	secret, _ := enrollTOTP(t, ts, user)
	h := newDirectHandler(t, ts)

	code := stepReplayCode(t, secret, time.Now())

	tx, err := ts.DB.Begin()
	require.NoError(t, err)
	// A failed require below must not leave this transaction holding the
	// user_mfa_totp row lock: the package cleanup's TRUNCATE would then block
	// forever instead of the test failing. Rollback after Rollback is a no-op.
	defer func() { _ = tx.Rollback() }()
	okTx, err := h.VerifyCodeTx(context.Background(), tx, user.ID, stepReplayPurpose, code)
	require.NoError(t, err)
	require.True(t, okTx, "guard: verification inside the transaction must succeed")
	require.NoError(t, tx.Rollback())

	okAfter, err := h.VerifyCode(context.Background(), user.ID, stepReplayPurpose, code)
	require.NoError(t, err)
	assert.True(t, okAfter,
		"guard: a code verified inside a transaction that was rolled back must remain usable in a later verification")
}

// TestTOTPStepReplay_GuardBackupCodeAndWebAuthnTokenIndependentOfTOTP is a
// regression guard: a backup code and a WebAuthn inline token are each
// accepted once (their own, pre-existing, single-use mechanisms), and
// consuming either one has no effect on whether a TOTP code for the current
// step is then accepted.
func TestTOTPStepReplay_GuardBackupCodeAndWebAuthnTokenIndependentOfTOTP(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepreplay9")
	secret, backupCodes := enrollTOTP(t, ts, user)
	h := newDirectHandler(t, ts)
	ctx := context.Background()

	// Backup code: single-use already, by its own mechanism.
	backupCode, isString := backupCodes[0].(string)
	require.True(t, isString, "enrollTOTP must return string backup codes")
	ok1, err := h.VerifyCode(ctx, user.ID, stepReplayPurpose, backupCode)
	require.NoError(t, err)
	require.True(t, ok1, "guard: the first use of a backup code must succeed")

	ok2, err := h.VerifyCode(ctx, user.ID, stepReplayPurpose, backupCode)
	require.NoError(t, err)
	assert.False(t, ok2, "guard: a backup code must not be usable twice")

	// Consuming the backup code must not block a current-step TOTP code.
	codeAfterBackup := stepReplayCode(t, secret, time.Now())
	okTOTP1, err := h.VerifyCode(ctx, user.ID, stepReplayPurpose, codeAfterBackup)
	require.NoError(t, err)
	assert.True(t, okTOTP1, "guard: consuming a backup code must not affect TOTP verification")

	// WebAuthn inline token: single-use already, by its own mechanism
	// (consumeWebAuthnInlineToken, internal/mfa/handlers.go:326).
	token := uuid.New().String() // > 20 chars, required by consumeWebAuthnInlineToken
	require.NoError(t, ts.Redis.Set(ctx, "mfa_inline_purpose_token:"+user.ID+":"+string(stepReplayPurpose)+":"+token, "1", time.Minute).Err())

	ok3, err := h.VerifyCode(ctx, user.ID, stepReplayPurpose, token)
	require.NoError(t, err)
	require.True(t, ok3, "guard: the first use of a WebAuthn inline token must succeed")

	ok4, err := h.VerifyCode(ctx, user.ID, stepReplayPurpose, token)
	require.NoError(t, err)
	assert.False(t, ok4, "guard: a WebAuthn inline token must not be usable twice")

	// Consuming the inline token must not block a current-step TOTP code
	// either. Advance one step from codeAfterBackup so this assertion cannot
	// be confused with a leftover step-replay refusal of that earlier code.
	codeAfterToken := stepReplayCode(t, secret, time.Now().Add(30*time.Second))
	okTOTP2, err := h.VerifyCode(ctx, user.ID, stepReplayPurpose, codeAfterToken)
	require.NoError(t, err)
	assert.True(t, okTOTP2, "guard: consuming a WebAuthn inline token must not affect TOTP verification")
}
