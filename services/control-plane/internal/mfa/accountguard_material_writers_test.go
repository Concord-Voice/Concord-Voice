package mfa_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The real TOTP writers under migration 000189's material trigger (#3680 PR1).
//
// TOTPSetup answers 409 for a CONFIRMED row ("Disable it first to re-enroll"),
// so a re-setup can reach an enabled row only between verify-setup and
// confirm-setup. The re-setup test therefore stops after verify-setup
// (version 2) and re-runs setup (version 3).

type agTOTPRow struct {
	version  int64
	immature bool // matures_at > now() + 71 hours
	legacy   bool // matures_at = the legacy sentinel
	enabled  bool
}

func readAGTOTP(t *testing.T, ts *testhelpers.TestServer, userID string) agTOTPRow {
	t.Helper()
	var r agTOTPRow
	require.NoError(t, ts.DB.QueryRow(`
		SELECT material_version, matures_at > now() + interval '71 hours', matures_at = '1970-01-01Z', enabled
		  FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(&r.version, &r.immature, &r.legacy, &r.enabled))
	return r
}

func resetAGMaturity(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(`UPDATE user_mfa_totp SET matures_at = '1970-01-01Z' WHERE user_id = $1`, userID)
	require.NoError(t, err)
}

// startTOTPSetupAndVerify runs enrollTOTP's steps 1 and 2 only, leaving the row
// enabled but unconfirmed.
func startTOTPSetupAndVerify(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) {
	t.Helper()
	auth := testhelpers.AuthHeaders(user.AccessToken)
	w := ts.DoRequest("POST", urlTOTPSetup, map[string]interface{}{"password": testPassword}, auth)
	require.Equal(t, http.StatusOK, w.Code)
	var setupBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &setupBody)
	secret, ok := setupBody["secret"].(string)
	require.True(t, ok)
	w = verifySetupWithPreviousStep(t, ts, auth, secret)
	if w.Code == http.StatusForbidden {
		w = verifySetupWithPreviousStep(t, ts, auth, secret)
	}
	require.Equal(t, http.StatusOK, w.Code)
}

// Every version on this table comes from accountguard_material_version_seq: the
// setup INSERT, the verify-setup enable (mfa/handlers.go enableVerifiedTOTP) and
// the confirm each draw one. A version is equality-only, so no test here asserts
// a number beyond "from the sequence" (at least 2) and "moved" or "unchanged".
func TestAccountGuardMaterial_TOTPEnrolmentEndsImmature(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "agenrol")
	enrollTOTP(t, ts, user)

	r := readAGTOTP(t, ts, user.ID)
	assert.GreaterOrEqual(t, r.version, int64(2), "a TOTP version is drawn from the sequence")
	assert.True(t, r.immature, "a fresh enrolment must start immature")
}

// Review Focus 5: the setup upsert over an enabled (not yet confirmed) row goes
// through ON CONFLICT DO UPDATE and must bump the version and stamp maturity.
func TestAccountGuardMaterial_TOTPResetupBumpsAndStamps(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "agresetup")
	startTOTPSetupAndVerify(t, ts, user)
	verified := readAGTOTP(t, ts, user.ID)
	require.GreaterOrEqual(t, verified.version, int64(2))
	resetAGMaturity(t, ts, user.ID)

	w := ts.DoRequest("POST", urlTOTPSetup, map[string]interface{}{"password": testPassword},
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	r := readAGTOTP(t, ts, user.ID)
	assert.GreaterOrEqual(t, r.version, int64(2), "the upsert draws from the sequence")
	assert.NotEqual(t, verified.version, r.version, "the upsert is a material change, so the version moves")
	assert.False(t, r.enabled, "re-setup resets enabled")
	assert.True(t, r.immature, "the upsert must stamp maturity")
}

func TestAccountGuardMaterial_BackupCodeRegenerationIsNotMaterial(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "agregen")
	secret, _ := enrollTOTP(t, ts, user)
	resetAGMaturity(t, ts, user.ID)
	enrolled := readAGTOTP(t, ts, user.ID)

	code, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)
	w := ts.DoRequest("POST", urlBackupCodesRegen, map[string]interface{}{
		"password": testPassword,
		"code":     code,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	r := readAGTOTP(t, ts, user.ID)
	assert.Equal(t, enrolled.version, r.version, "regenerating backup codes is not material")
	assert.True(t, r.legacy, "maturity must stay at the legacy sentinel")
}
