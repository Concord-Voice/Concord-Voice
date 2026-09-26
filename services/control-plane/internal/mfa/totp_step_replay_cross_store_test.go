package mfa_test

// Guards for the TOTP step-replay fix across stores and guard shapes, from an
// adversarial pass over it. Each attack held; these keep it that way.
//
//  1. A pool VerifyCode racing a committing VerifyCodeTx for one code accepts
//     it exactly once, and the step is burned for both.
//  2. The code that completed verify-setup (guarded by enabled/nonce) does not
//     verify again as a confirmed-factor code (guarded by last_used_step): the
//     two guards share one step namespace.
//  3. A step burned inside a committed transaction refuses a later pool
//     submission of the same code, and the next step still verifies.
//  4. A refused replay answers byte-for-byte like a wrong code.

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestTOTPStepReplay_PoolRacingACommittingTxAcceptsOnce(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepxpooltx")
	secret, _ := enrollTOTP(t, ts, user)
	h := newDirectHandler(t, ts)
	code := stepReplayCode(t, secret, time.Now())

	var okPool, okTx, committed bool
	var poolErr, txErr, commitErr error
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		okPool, poolErr = h.VerifyCode(context.Background(), user.ID, stepReplayPurpose, code)
	}()
	go func() {
		defer wg.Done()
		<-start
		tx, err := ts.DB.Begin()
		if err != nil {
			txErr = err
			return
		}
		okTx, txErr = h.VerifyCodeTx(context.Background(), tx, user.ID, stepReplayPurpose, code)
		if txErr != nil {
			_ = tx.Rollback()
			return
		}
		if commitErr = tx.Commit(); commitErr == nil {
			committed = true
		}
	}()

	done := make(chan struct{})
	go func() { close(start); wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the pool/tx race did not complete")
	}
	require.NoError(t, poolErr)
	require.NoError(t, txErr)
	require.NoError(t, commitErr)

	accepted := 0
	if okPool {
		accepted++
	}
	if okTx && committed {
		accepted++
	}
	assert.Equal(t, 1, accepted, "exactly one of the pool and the committed tx accepts the code (pool=%v tx=%v)", okPool, okTx)

	replay, err := h.VerifyCode(context.Background(), user.ID, stepReplayPurpose, code)
	require.NoError(t, err)
	assert.False(t, replay, "the step is burned for both stores")
}

func TestTOTPStepReplay_VerifySetupCodeRefusedAsAConfirmedFactor(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepxsetup")
	auth := testhelpers.AuthHeaders(user.AccessToken)

	w := ts.DoRequest(http.MethodPost, urlTOTPSetup, map[string]interface{}{"password": testPassword}, auth)
	require.Equal(t, http.StatusOK, w.Code, "setup: %s", w.Body.String())
	var setupBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &setupBody)
	secret := testhelpers.JSONField[string](t, setupBody, "secret")

	// One code string completes verify-setup (retried once across a step
	// boundary); that same string is the replay.
	code := stepReplayCode(t, secret, time.Now())
	w = ts.DoRequest(http.MethodPost, urlTOTPVerifySetup, map[string]interface{}{"code": code}, auth)
	if w.Code == http.StatusForbidden {
		code = stepReplayCode(t, secret, time.Now())
		w = ts.DoRequest(http.MethodPost, urlTOTPVerifySetup, map[string]interface{}{"code": code}, auth)
	}
	require.Equal(t, http.StatusOK, w.Code, "verify-setup: %s", w.Body.String())
	w = ts.DoRequest(http.MethodPost, urlTOTPConfirmSetup, nil, auth)
	require.Equal(t, http.StatusOK, w.Code, "confirm-setup: %s", w.Body.String())

	ok, err := newDirectHandler(t, ts).VerifyCode(context.Background(), user.ID, stepReplayPurpose, code)
	require.NoError(t, err)
	assert.False(t, ok, "the code that completed verify-setup must not verify again within its window")
}

func TestTOTPStepReplay_CommittedTxBurnRefusesAPoolReplay(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepxtxburn")
	secret, _ := enrollTOTP(t, ts, user)
	h := newDirectHandler(t, ts)
	now := time.Now()
	code := stepReplayCode(t, secret, now)

	tx, err := ts.DB.Begin()
	require.NoError(t, err)
	ok, err := h.VerifyCodeTx(context.Background(), tx, user.ID, stepReplayPurpose, code)
	require.NoError(t, err)
	require.True(t, ok, "the transaction accepts the fresh code")
	require.NoError(t, tx.Commit())

	ok, err = h.VerifyCode(context.Background(), user.ID, stepReplayPurpose, code)
	require.NoError(t, err)
	assert.False(t, ok, "a step burned by a committed transaction is refused on the pool")

	// Positive control: the guard is per step. The next step's code (inside
	// the +1 skew window) still verifies unless it happens to equal this one.
	next := stepReplayCode(t, secret, now.Add(30*time.Second))
	if next != code {
		ok, err = h.VerifyCode(context.Background(), user.ID, stepReplayPurpose, next)
		require.NoError(t, err)
		assert.True(t, ok, "the following step's code still verifies")
	}
}

func TestTOTPStepReplay_RefusalIsByteIdenticalToAWrongCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "stepxoracle")
	secret, _ := enrollTOTP(t, ts, user)
	auth := testhelpers.AuthHeaders(user.AccessToken)
	code := stepReplayCode(t, secret, time.Now())
	regen := func(c string) (int, string) {
		w := ts.DoRequest(http.MethodPost, urlBackupCodesRegen, map[string]interface{}{"password": testPassword, "code": c}, auth)
		return w.Code, w.Body.String()
	}

	status, body := regen(code)
	require.Equal(t, http.StatusOK, status, "the first use succeeds: %s", body)
	replayStatus, replayBody := regen(code)

	// A well-formed code far outside the skew window.
	wrong := stepReplayCode(t, secret, time.Now().Add(time.Hour))
	if wrong == code {
		wrong = stepReplayCode(t, secret, time.Now().Add(2*time.Hour))
	}
	wrongStatus, wrongBody := regen(wrong)

	assert.Equal(t, http.StatusForbidden, replayStatus)
	assert.Equal(t, wrongStatus, replayStatus, "a replay answers with a wrong code's status")
	assert.Equal(t, wrongBody, replayBody, "a replay answers with a wrong code's body")
}
