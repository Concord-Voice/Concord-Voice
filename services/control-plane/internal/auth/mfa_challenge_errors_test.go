package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// #2450: handleMFAChallenge previously blank-discarded both MFA-method reads. errcheck
// honors an explicit `_`, so the linter never flagged them — the review-only class in
// [internal]rules/backend.md (founding incidents #1142/#1154).
//
// The failure was not cosmetic: on error the response shipped `"methods": []` alongside a
// VALID mfa_challenge_token, so the client rendered an MFA prompt with no selectable
// method, the challenge JTI was burned, and nothing was logged — the user hard-stuck
// mid-login with zero server-side signal.
//
// These tests pin the asymmetry deliberately: loginMethods is load-bearing for the
// response and is FATAL; allMethods only feeds the recovery-only hint and DEGRADES.
//
// In-package (not auth_test) because handleMFAChallenge is unexported; the auth_test
// stubMFAChecker is therefore unreachable and this file carries its own minimal stub.

type mfaMethodsStub struct {
	enabledMethods []string
	enabledErr     error
	loginMethods   []string
	loginErr       error
	challengeTok   string
}

func (s *mfaMethodsStub) IsEnabled(context.Context, string) bool { return true }
func (s *mfaMethodsStub) GetEnabledMethods(context.Context, string) ([]string, error) {
	return s.enabledMethods, s.enabledErr
}
func (s *mfaMethodsStub) GetLoginMethods(context.Context, string) ([]string, error) {
	return s.loginMethods, s.loginErr
}
func (s *mfaMethodsStub) GenerateLoginChallenge(context.Context, string, bool, string) (string, string, error) {
	return s.challengeTok, "stub-jti", nil
}
func (s *mfaMethodsStub) GenerateUpgradeChallenge(context.Context, string, bool) (string, string, error) {
	return s.challengeTok, "stub-jti", nil
}
func (s *mfaMethodsStub) BeginWebAuthnLogin(context.Context, string, string) (interface{}, error) {
	return nil, nil
}
func (s *mfaMethodsStub) GenerateRecoveryToken(string) (string, string, error) {
	return "", "", nil
}
func (s *mfaMethodsStub) ValidateRecoveryToken(string) (*RecoveryClaims, error) { return nil, nil }

func newMFAChallengeRecorder() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	return c, rec
}

// A GetLoginMethods failure must NOT ship a challenge the user cannot answer.
func TestHandleMFAChallengeGetLoginMethodsErrorIsFatal(t *testing.T) {
	stub := &mfaMethodsStub{
		loginErr: errors.New("db unavailable"),
		// A challenge would generate fine — the point is that we never get there.
		challengeTok: "should-not-be-issued",
	}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}

	c, rec := newMFAChallengeRecorder()
	h.handleMFAChallenge(c.Request.Context(), c, "11111111-1111-1111-1111-111111111111", false, "epoch-1")

	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"a GetLoginMethods failure must fail the login, not ship an unanswerable challenge")

	body := rec.Body.String()
	require.NotContains(t, body, "mfa_challenge_token",
		"no challenge token may be issued when the method list could not be read (#2450)")
	require.NotContains(t, body, "should-not-be-issued")
}

// buildMFAChallengeResponse is the SHARED builder behind the suspicious-refresh and
// MFA-upgrade challenges. Testing it directly is what stops the #2450 defect from
// reappearing on a third caller: handleMFAChallenge was fixed by hand-rolling its own
// response rather than routing through this helper, which is precisely why one copy got
// fixed and two paths stayed broken.
func TestBuildMFAChallengeResponseLoginMethodsErrorReturnsError(t *testing.T) {
	stub := &mfaMethodsStub{loginErr: errors.New("db unavailable")}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}

	resp, err := h.buildMFAChallengeResponse(context.Background(),
		"suspicious_session_mfa", "Session verification required", "tok", "user-1", "jti-1")

	require.Error(t, err, "a GetLoginMethods failure must surface, not yield a partial response")
	require.Nil(t, resp, "no half-built response may escape — an empty method list beside a "+
		"valid challenge token is the hard-stuck bug (#2450)")
}

func TestBuildMFAChallengeResponseSucceedsWithMethods(t *testing.T) {
	stub := &mfaMethodsStub{
		loginMethods:   []string{"totp"},
		enabledMethods: []string{"totp", "recovery_code"},
	}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}

	resp, err := h.buildMFAChallengeResponse(context.Background(),
		"mfa_upgrade_required", "Verify your identity", "tok-2", "user-2", "jti-2")

	require.NoError(t, err)
	require.Equal(t, "tok-2", resp["mfa_challenge_token"])
	require.Equal(t, []string{"totp"}, resp["methods"])
	require.Equal(t, []string{"recovery_code"}, resp["recovery_only_methods"],
		"a method enabled but not login-eligible must surface as recovery-only")
}

// A GetEnabledMethods failure only costs the recovery-only hint; the login proceeds.
func TestHandleMFAChallengeGetEnabledMethodsErrorDegradesHint(t *testing.T) {
	stub := &mfaMethodsStub{
		enabledErr:   errors.New("db unavailable"),
		loginMethods: []string{"totp"},
		challengeTok: "challenge-token-abc",
	}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}

	c, rec := newMFAChallengeRecorder()
	h.handleMFAChallenge(c.Request.Context(), c, "22222222-2222-2222-2222-222222222222", false, "epoch-1")

	require.Equal(t, http.StatusOK, rec.Code,
		"an allMethods failure must not block a login that still has a usable method list")

	body := rec.Body.String()
	require.Contains(t, body, "challenge-token-abc", "the challenge must still be issued")
	require.Contains(t, body, "totp", "the selectable method list must still be present")
	require.NotContains(t, body, "recovery_only_methods",
		"the recovery-only hint degrades to absent when the superset read fails")
}
