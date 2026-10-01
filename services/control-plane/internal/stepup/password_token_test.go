package stepup

// Handler-level contract tests for POST /api/v1/auth/step-up/password (#3509,
// design spec "Developer decisions, 2026-10-01", T-2). The verifier is a fake
// that records whether it was called, so "refused BEFORE the password is
// verified" is observable; the real shared lockout is pinned through the full
// router in password_token_lockout_test.go.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// mintFixturePassword is what the fake verifier accepts. Bound to a constant
// so detect-secrets sees no keyword beside a literal.
const mintFixturePassword = "mint-fixture-correct" // #nosec G101 -- test fixture, not a real credential // pragma: allowlist secret

// fakePasswordVerifier records each call. err, when set, is returned for every
// password, and it deliberately EMBEDS the password, as a careless verifier's
// error might: the handler must still never log it.
type fakePasswordVerifier struct {
	calls int
	err   error
}

func (f *fakePasswordVerifier) VerifyPassword(_ context.Context, _, password string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if password != mintFixturePassword { // pragma: allowlist secret
		return "", auth.ErrInvalidCredentials
	}
	return "", nil
}

type mintHarness struct {
	db       *sql.DB
	userID   string
	verifier *fakePasswordVerifier
	logs     *bytes.Buffer
	handler  *PasswordTokenHandler
}

func newMintHarness(t *testing.T) *mintHarness {
	t.Helper()
	db := subjectTestDB(t)
	logs := &bytes.Buffer{}
	m := &mintHarness{db: db, userID: subjectTestUser(t, db), verifier: &fakePasswordVerifier{}, logs: logs}
	m.handler = NewPasswordTokenHandler(db, m.verifier, nil, logger.NewWithWriter(logs))
	return m
}

func (m *mintHarness) mint(t *testing.T, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/step-up/password", strings.NewReader(body))
	c.Set("user_id", m.userID)
	m.handler.MintPasswordToken(c)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out) // a non-JSON body leaves out nil, and the assertions on it fail
	return w, out
}

func mintBody(password string, purpose Purpose) string {
	raw, _ := json.Marshal(map[string]string{"current_password": password, "purpose": string(purpose)})
	return string(raw)
}

// Mutants killed: answering without the token, a TTL other than 60, dropping
// no-store, minting a factor other than password.
func TestMintPasswordToken_Success(t *testing.T) {
	m := newMintHarness(t)

	w, body := m.mint(t, mintBody(mintFixturePassword, PurposeDMClear))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	token, _ := body["step_up_token"].(string)
	require.Len(t, token, 43)
	assert.EqualValues(t, 60, body["expires_in"])
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.True(t, spendIn(t, m.db, m.userID, FactorPassword, PurposeDMClear, token, true),
		"the minted token is a password token for the requested purpose")
}

// Mutant killed: minting on a wrong password, or answering it with any body
// other than the seam's exact refusal the desktop matches.
func TestMintPasswordToken_WrongPassword(t *testing.T) {
	m := newMintHarness(t)

	w, body := m.mint(t, mintBody("not-the-password", PurposeDMClear))

	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, map[string]any{"error": ErrMsgInvalidPassword}, body)
	assert.Equal(t, 1, m.verifier.calls)
	assert.Zero(t, tokenRows(t, m.db, m.userID))
}

// Every purpose outside the own-rule set is a 400 before the password is
// verified, including valid purposes of other routes.
//
// Mutants killed: checking Valid() instead of OwnRule() (the settings purpose
// mints), checking the purpose after verifying (the verifier is called).
func TestMintPasswordToken_PurposeOutsideTheAllowlistIs400BeforeVerification(t *testing.T) {
	m := newMintHarness(t)
	for _, purpose := range []Purpose{"", "not.a.purpose", PurposeTOTPSetup, PurposeDMPurge, PurposeSessionsRevokeAll, PurposeServerMFAEnforcementOff} {
		w, body := m.mint(t, mintBody(mintFixturePassword, purpose))
		require.Equal(t, http.StatusBadRequest, w.Code, "purpose %q", purpose)
		assert.Equal(t, ErrMsgInvalidStepUpPurpose, body["error"])
	}
	assert.Zero(t, m.verifier.calls, "no purpose outside the allowlist may reach the password verifier")
	assert.Zero(t, tokenRows(t, m.db, m.userID))

	require.ElementsMatch(t, []Purpose{PurposeDMClear, PurposeDMMessageDelete, PurposeMessageDelete,
		PurposeChannelPurge, PurposeServerPurge}, OwnRulePurposes(), "the allowlist is exactly the five own-rule routes")
}

// An account with inline MFA confirms these routes with MFA, so the mint
// refuses it before the password is verified, offering its P1 methods.
//
// Mutants killed: dropping the MFA check (the account mints), running it after
// verification (the verifier is called).
func TestMintPasswordToken_MFAAccountRefusedBeforeVerification(t *testing.T) {
	m := newMintHarness(t)
	subjectTestTOTP(t, m.db, m.userID, true, true)

	w, body := m.mint(t, mintBody(mintFixturePassword, PurposeMessageDelete))

	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, map[string]any{
		"error": "MFA verification required", "mfa_required": true, "mfa_methods": []any{"totp"},
	}, body)
	assert.Zero(t, m.verifier.calls)
	assert.Zero(t, tokenRows(t, m.db, m.userID))
}

// Malformed bodies and an empty or over-long password are 400 without
// verification.
//
// Mutant killed: dropping the 1 KiB bound (the verifier is called).
func TestMintPasswordToken_BadBodyIs400(t *testing.T) {
	m := newMintHarness(t)
	for name, body := range map[string]string{ // #nosec G101 -- test case names and request bodies, not credentials
		"empty":         "",
		"null":          "null",
		"array":         "[]",
		"trailing":      mintBody(mintFixturePassword, PurposeDMClear) + "{}",
		"no password":   `{"purpose":"dm.clear"}`,
		"over 1 KiB":    mintBody(strings.Repeat("p", 1025), PurposeDMClear),
		"wrong type":    `{"current_password":1,"purpose":"dm.clear"}`,
		"over the body": `{"purpose":"dm.clear","pad":"` + strings.Repeat("x", 5000) + `"}`,
	} {
		w, out := m.mint(t, body)
		require.Equal(t, http.StatusBadRequest, w.Code, name)
		assert.Equal(t, ErrMsgInvalidRequestBody, out["error"], name)
	}
	assert.Zero(t, m.verifier.calls)
}

// The shared lockout answers 423 with CompleteLink's error_code, and a
// verifier fault is an opaque 500.
//
// Mutant killed: folding the lockout into the wrong-password 403 (the client
// would prompt for a password that cannot work until the lockout ends).
func TestMintPasswordToken_LockoutAndFault(t *testing.T) {
	m := newMintHarness(t)

	m.verifier.err = auth.ErrAccountLocked
	w, body := m.mint(t, mintBody(mintFixturePassword, PurposeDMClear))
	require.Equal(t, http.StatusLocked, w.Code)
	assert.Equal(t, map[string]any{"error": ErrMsgStepUpLocked, "error_code": "account_locked"}, body)

	m.verifier.err = errors.New("verify password: decode failed near " + mintFixturePassword)
	w, body = m.mint(t, mintBody(mintFixturePassword, PurposeDMClear))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, map[string]any{"error": ErrMsgVerificationFailed}, body)
	assert.Zero(t, tokenRows(t, m.db, m.userID))
}

// The password reaches no log line on any arm, including a verifier fault
// whose error text carries it.
//
// Mutant killed: logging the verifier's error on the 500 arm (the password
// appears in the captured output).
func TestMintPasswordToken_PasswordNeverLogged(t *testing.T) {
	m := newMintHarness(t)
	m.mint(t, mintBody(mintFixturePassword, PurposeDMClear))
	m.mint(t, mintBody("wrong-"+mintFixturePassword, PurposeDMClear))
	m.mint(t, mintBody(mintFixturePassword, "bad.purpose"))
	m.verifier.err = errors.New("verify password: decode failed near " + mintFixturePassword)
	m.mint(t, mintBody(mintFixturePassword, PurposeDMClear))

	require.Contains(t, m.logs.String(), "failure_class=step_up_password_verify_failed",
		"precondition: the fault arm logged, so the assertion below is not vacuous")
	assert.NotContains(t, m.logs.String(), mintFixturePassword)
}

// An account with no usable password factor is answered with the route's own
// NoFactors copy, from the P1 read and before the verifier is called (#3509
// review, security L5).
//
// Mutants killed: dropping the empty-hash check (the verifier is called), and
// answering every purpose with one route's copy.
func TestMintPasswordToken_NoPasswordFactorIsRefusedBeforeVerification(t *testing.T) {
	m := newMintHarness(t)
	_, err := m.db.Exec(`UPDATE users SET password_hash = '' WHERE id = $1`, m.userID)
	require.NoError(t, err)

	for _, purpose := range OwnRulePurposes() {
		w, body := m.mint(t, mintBody(mintFixturePassword, purpose))
		require.Equal(t, http.StatusBadRequest, w.Code, "purpose %q", purpose)
		assert.Equal(t, OwnRuleCopy(purpose).NoFactors, body["error"], "purpose %q", purpose)
	}
	assert.NotEqual(t, OwnRuleCopy(PurposeDMClear).NoFactors, OwnRuleCopy(PurposeMessageDelete).NoFactors,
		"precondition: the routes' copies differ, so the per-purpose assertion has teeth")
	assert.Zero(t, m.verifier.calls)
	assert.Zero(t, tokenRows(t, m.db, m.userID))
}
