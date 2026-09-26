package mfa

// Reproduction tests for the WebAuthn inline-verification purpose defect: a
// token minted by WebAuthnVerifyInlineFinish proves only "this user just
// touched their security key", not what the touch was for, so a token minted
// for one protected action is accepted by any other step-up consumer
// (including, worse, the login MFA challenge, which never mints one).
//
// Every test below is written against the fix contract:
//   - WebAuthnVerifyInlineBegin takes a JSON body {"purpose": "<purpose>"}; a
//     missing or unknown purpose is a 400.
//   - A token minted for purpose P is accepted only by P's own consumer;
//     any other consumer refuses it exactly as it refuses an invalid code,
//     and the refusal does not consume it.
//   - The login MFA Verify never accepts an inline token.
//
// None of that exists yet, so tests 1-4 are expected to FAIL on the current
// tree; test 5 is the single-use guard and is expected to PASS both before
// and after the fix. Helpers here reuse sfNewAuthenticator, sfAuthenticator,
// sfWebAuthnHandler and the sfInlineVerify ceremony shape from
// silent_failure_regression_test.go, plus the iu* fixtures from
// settings_stepup_internal_test.go. ipBeginWithPurpose is this file's own
// ceremony helper (sfInlineVerify's begin always sends {}), so the two do not
// collide.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ipBegin posts the given raw JSON body to WebAuthnVerifyInlineBegin.
func ipBegin(t *testing.T, h *Handler, userID, body string) (int, map[string]interface{}) {
	t.Helper()
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/verify-inline/begin", body, userID, "")
	h.WebAuthnVerifyInlineBegin(c)
	return w.Code, iuBody(t, w)
}

// ipMintInlineToken runs a real begin+finish ceremony for purpose and returns
// the minted mfa_token. It requires begin to succeed (200), which is true
// today for any body (the purpose contract does not exist yet) and must
// remain true once purpose validation lands, for a KNOWN valid purpose.
func ipMintInlineToken(t *testing.T, h *Handler, rdb *redis.Client, auth *sfAuthenticator, userID, purpose string) string {
	t.Helper()
	beginBody := fmt.Sprintf(`{"purpose":%q}`, purpose)
	status, body := ipBegin(t, h, userID, beginBody)
	require.Equal(t, http.StatusOK, status, "begin with a known valid purpose must succeed: %v", body)

	raw, err := rdb.Get(context.Background(), inlineSessionKey(userID)).Bytes()
	require.NoError(t, err)
	var session webauthn.SessionData
	require.NoError(t, json.Unmarshal(raw, &session))

	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/verify-inline/finish", auth.assertion(t, session.Challenge), userID, "")
	h.WebAuthnVerifyInlineFinish(c)
	require.Equal(t, http.StatusOK, w.Code, "inline finish must succeed for a real ceremony: %s", w.Body.String())
	token, _ := iuBody(t, w)["mfa_token"].(string)
	require.NotEmpty(t, token, "a successful finish must hand back a non-empty token")
	return token
}

// ipEmailSmsDisable drives EmailSmsDisable's step-up gate directly.
func ipEmailSmsDisable(t *testing.T, h *Handler, userID, password, mfaCode string) (int, map[string]interface{}) {
	t.Helper()
	body := fmt.Sprintf(`{"password":%q,"mfa_code":%q}`, password, mfaCode)
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/email-sms/disable", body, userID, "")
	h.EmailSmsDisable(c)
	return w.Code, iuBody(t, w)
}

// ipSetBackupEmail drives SetBackupEmail's step-up gate directly.
func ipSetBackupEmail(t *testing.T, h *Handler, userID, email, password, mfaCode string) (int, map[string]interface{}) {
	t.Helper()
	body := fmt.Sprintf(`{"email":%q,"password":%q,"mfa_code":%q}`, email, password, mfaCode)
	c, w := iuGinContext(http.MethodPut, "/api/v1/mfa/backup-email", body, userID, "")
	h.SetBackupEmail(c)
	return w.Code, iuBody(t, w)
}

// TestInlinePurpose_CrossPurposeRefusedAndTokenSurvives pins two properties
// together because the second can only be observed as a continuation of the
// first: (1) a token minted for mfa_settings.backup_email_set must be refused
// by EmailSmsDisable exactly as an invalid code is, and (2) that refusal must
// not consume the token, so SetBackupEmail can still accept it afterward.
// FAILS TODAY on both counts: consumeWebAuthnInlineToken accepts any inline
// token regardless of which purpose minted it, so EmailSmsDisable succeeds
// (property 1 violated) and its GETDEL burns the token in the process, so the
// later SetBackupEmail call then sees an already-consumed token and is
// refused (property 2 violated too, for the same underlying reason).
func TestInlinePurpose_CrossPurposeRefusedAndTokenSurvives(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, kr)

	token := ipMintInlineToken(t, h, rdb, auth, userID, "mfa_settings.backup_email_set")

	t.Run("cross-purpose token is refused like an invalid code", func(t *testing.T) {
		status, body := ipEmailSmsDisable(t, h, userID, iuPassword, token)
		assert.Equal(t, http.StatusForbidden, status,
			"a token minted for mfa_settings.backup_email_set must be refused by EmailSmsDisable, not accepted; got %d %v", status, body)
		assert.Equal(t, "Invalid MFA code", body["error"],
			"the refusal must read exactly like an invalid code, the same way any other wrong-purpose credential is refused")
	})

	t.Run("the refused token still works for its own purpose", func(t *testing.T) {
		status, body := ipSetBackupEmail(t, h, userID, "backup@example.test", iuPassword, token)
		assert.Equal(t, http.StatusOK, status,
			"a token's own purpose consumer must still accept it after a DIFFERENT consumer refused it — the refusal must not have burned it; got %d %v", status, body)
	})
}

// TestInlinePurpose_BeginRequiresPurpose pins that WebAuthnVerifyInlineBegin
// requires a recognized purpose in its JSON body before minting a ceremony.
// FAILS TODAY: Begin reads no request body at all, so both a body with no
// purpose and a body naming an unrecognized purpose start a real ceremony and
// return 200, never 400.
func TestInlinePurpose_BeginRequiresPurpose(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	userID := iuCreateUser(t, db, iuPassword)
	iuAddWebAuthnCredential(t, db, userID)
	h := sfWebAuthnHandler(t, db, iuNewTestRedis(t), kr)

	t.Run("missing purpose", func(t *testing.T) {
		status, body := ipBegin(t, h, userID, `{}`)
		assert.Equal(t, http.StatusBadRequest, status,
			"begin with no purpose must be refused before a ceremony is minted; today it starts one for any body: got %d %v", status, body)
	})

	t.Run("unknown purpose", func(t *testing.T) {
		status, body := ipBegin(t, h, userID, `{"purpose":"not.a.purpose"}`)
		assert.Equal(t, http.StatusBadRequest, status,
			"begin with an unrecognized purpose string must be refused; today any string is accepted: got %d %v", status, body)
	})
}

// TestInlinePurpose_LoginNeverAcceptsInlineToken pins that the login MFA
// Verify endpoint never accepts a WebAuthn inline-verification token as a
// submitted code, regardless of which purpose minted it — the login modal
// never mints one, so an inline token reaching Verify can only be a stolen or
// misused proof from an unrelated action. FAILS TODAY: verifyCodeMatchedMethod
// (shared by every step-up consumer including login's verifyTOTPOrBackup)
// checks consumeWebAuthnInlineToken unconditionally, before ever looking at
// TOTP, so the token verifies the login challenge and returns 200.
func TestInlinePurpose_LoginNeverAcceptsInlineToken(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, kr)

	token := ipMintInlineToken(t, h, rdb, auth, userID, "mfa_settings.backup_email_set")

	challengeToken, _, err := GenerateChallengeToken(userID, PurposeSuspiciousRefresh, JWTSecret("test"), "")
	require.NoError(t, err)
	body := fmt.Sprintf(`{"mfa_challenge_token":%q,"method":"totp","code":%q}`, challengeToken, token)
	c, w := iuGinContext(http.MethodPost, "/api/v1/auth/mfa/verify", body, "", "")
	h.Verify(c)

	respBody := iuBody(t, w)
	assert.NotEqual(t, http.StatusOK, w.Code,
		"a WebAuthn inline verification token must never satisfy the login MFA challenge; got %d %v", w.Code, respBody)
	assert.NotEqual(t, true, respBody["verified"],
		"the login response must never report verified=true for a submitted inline token")
}

// TestInlinePurpose_SingleUseGuard is the guard case: a token accepted by its
// own purpose's consumer is still single-use. Green both before and after the
// fix — proves the reproduction above fails for the purpose defect and not
// because single-use enforcement itself is broken.
func TestInlinePurpose_SingleUseGuard(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, kr)

	token := ipMintInlineToken(t, h, rdb, auth, userID, "mfa_settings.backup_email_set")

	status, body := ipSetBackupEmail(t, h, userID, "backup@example.test", iuPassword, token)
	require.Equal(t, http.StatusOK, status, "the first use of a freshly minted token must succeed: %v", body)

	status2, body2 := ipSetBackupEmail(t, h, userID, "backup2@example.test", iuPassword, token)
	assert.Equal(t, http.StatusForbidden, status2,
		"a token already spent once must be refused on reuse, single-use must hold regardless of the purpose fix: got %d %v", status2, body2)
}
