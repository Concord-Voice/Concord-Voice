package mfa

// Consumer coverage for the WebAuthn inline-token purpose binding (#3453
// RS11). Tokens come from a real ceremony (ipMintInlineToken), never from a
// seeded key, so these tests do not depend on how a token is stored.
//
// Levels: the fourteen MFA-settings consumers are driven at the HANDLER; the
// ten consumers outside this package are driven through the entry point each
// calls with the real verifier (stepup.VerifyMFAFactor/Tx, mfaenforce.ConfirmTx,
// or VerifyCode/VerifyCodeTx directly), because their handlers live in packages
// this one cannot import.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// ipOtherPurpose is a purpose that is not p, for minting a foreign token.
func ipOtherPurpose(p stepup.Purpose) stepup.Purpose {
	if p == stepup.PurposeBackupEmailSet {
		return stepup.PurposeTOTPSetup
	}
	return stepup.PurposeBackupEmailSet
}

// ipRequireStillValidFor proves a refused token was not consumed: its own
// purpose still accepts it (which spends it).
func ipRequireStillValidFor(t *testing.T, h *Handler, userID string, purpose stepup.Purpose, token string) {
	t.Helper()
	ok, err := h.VerifyCode(context.Background(), userID, purpose, token)
	require.NoError(t, err)
	require.True(t, ok, "a token refused by another consumer must survive for its own purpose %q", purpose)
}

func TestInlinePurpose_EveryMFASettingsConsumer(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb := iuNewTestRedis(t)
	h := sfWebAuthnHandler(t, db, rdb, kr)

	key := base64.StdEncoding.EncodeToString([]byte("ip-overwrite-recovery-key-32-byt"))
	salt := base64.StdEncoding.EncodeToString([]byte("ip-overwrite-salt-16"))
	routes := []struct {
		purpose stepup.Purpose
		method  string
		body    string // %[1]q = password, %[2]q = mfa code
		param   string
		prepare func(t *testing.T, userID string)
		handler func(*gin.Context)
	}{
		{purpose: stepup.PurposeTOTPSetup, method: http.MethodPost, body: `{"password":%[1]q,"mfa_code":%[2]q}`, handler: h.TOTPSetup},
		{purpose: stepup.PurposeTOTPDisable, method: http.MethodPost, body: `{"password":%[1]q,"code":%[2]q}`, handler: h.TOTPDisable,
			prepare: func(t *testing.T, userID string) { iuEnrollTOTP(t, db, kr, userID) }},
		{purpose: stepup.PurposeWebAuthnRegister, method: http.MethodPost, body: `{"password":%[1]q,"mfa_code":%[2]q,"credential_name":"k","credential_type":"hardware"}`, handler: h.WebAuthnRegisterBegin},
		{purpose: stepup.PurposeRecoveryOnlySet, method: http.MethodPut, body: `{"password":%[1]q,"mfa_code":%[2]q,"methods":[]}`, handler: h.SetRecoveryOnly},
		{purpose: stepup.PurposeRecoveryHardenedSet, method: http.MethodPut, body: `{"password":%[1]q,"mfa_code":%[2]q,"enabled":true}`, handler: h.SetRecoveryHardened},
		{purpose: stepup.PurposeEmailSmsSetup, method: http.MethodPost, body: `{"password":%[1]q,"mfa_code":%[2]q,"methods":["email"]}`, handler: h.EmailSmsSetup},
		{purpose: stepup.PurposeEmailSmsDisable, method: http.MethodPost, body: `{"password":%[1]q,"mfa_code":%[2]q}`, handler: h.EmailSmsDisable},
		{purpose: stepup.PurposeBackupEmailSet, method: http.MethodPut, body: `{"email":"ip@example.test","password":%[1]q,"mfa_code":%[2]q}`, handler: h.SetBackupEmail},
		{purpose: stepup.PurposeRecoveryKeyReplace, method: http.MethodPut,
			body:    `{"recovery_wrapped_private_key":"` + key + `","recovery_key_salt":"` + salt + `","password":%[1]q,"mfa_code":%[2]q}`,
			handler: h.StoreRecoveryKey,
			prepare: func(t *testing.T, userID string) {
				iuSeedRecoveryKey(t, db, userID, []byte("ip-seed-recovery-key-32-bytes-xx"), []byte("ip-seed-salt-16b"))
			}},
		{purpose: stepup.PurposeRecoveryKeyRemove, method: http.MethodDelete, body: `{"password":%[1]q,"mfa_code":%[2]q}`, handler: h.DeleteRecoveryKey},
		{purpose: stepup.PurposeTrustedDeviceDesignate, method: http.MethodPost, body: `{"password":%[1]q,"mfa_code":%[2]q,"device_name":"d"}`, handler: h.DesignateTrustedDevice},
		{purpose: stepup.PurposeTrustedDeviceRemove, method: http.MethodDelete, body: `{"password":%[1]q,"mfa_code":%[2]q}`, param: "00000000-0000-0000-0000-000000000000", handler: h.RemoveTrustedDevice},
		{purpose: stepup.PurposeRecoveryCircleUpsert, method: http.MethodPut, body: `{"password":%[1]q,"mfa_code":%[2]q,"threshold_k":2,"total_shares_n":3,"shares":[]}`, handler: h.UpsertRecoveryCircle},
		{purpose: stepup.PurposeRecoveryCircleDelete, method: http.MethodDelete, body: `{"password":%[1]q,"mfa_code":%[2]q}`, handler: h.DeleteRecoveryCircle},
	}
	require.Len(t, routes, 14, "every MFA-settings consumer, including TOTPDisable")

	for _, r := range routes {
		t.Run(string(r.purpose), func(t *testing.T) {
			userID := iuCreateUser(t, db, iuPassword)
			auth := sfNewAuthenticator(t, db, userID)
			if r.prepare != nil {
				r.prepare(t, userID)
			}
			drive := func(token string) (int, map[string]interface{}) {
				c, w := iuGinContext(r.method, "/api/v1/mfa/x", fmt.Sprintf(r.body, iuPassword, token), userID, "")
				if r.param != "" {
					c.Params = gin.Params{{Key: "id", Value: r.param}}
				}
				r.handler(c)
				return w.Code, iuBody(t, w)
			}

			other := ipOtherPurpose(r.purpose)
			foreign := ipMintInlineToken(t, h, rdb, auth, userID, string(other))
			status, body := drive(foreign)
			assert.Equal(t, http.StatusForbidden, status, "a token minted for %q must be refused: %v", other, body)
			assert.Equal(t, stepup.ErrMsgInvalidMFACode, body["error"], "the refusal must read exactly like an invalid code")
			ipRequireStillValidFor(t, h, userID, other, foreign)

			own := ipMintInlineToken(t, h, rdb, auth, userID, string(r.purpose))
			status, body = drive(own)
			assert.NotEqual(t, stepup.ErrMsgInvalidMFACode, body["error"], "its own purpose's token must pass the gate: %d %v", status, body)
			spent, err := h.VerifyCode(context.Background(), userID, r.purpose, own)
			require.NoError(t, err)
			assert.False(t, spent, "the consumer must have claimed its own purpose's token")
		})
	}
}

// TestInlinePurpose_EveryOtherConsumerPurpose drives the eleven consumers outside
// internal/mfa through the exact entry point each calls.
func TestInlinePurpose_EveryOtherConsumerPurpose(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb := iuNewTestRedis(t)
	h := sfWebAuthnHandler(t, db, rdb, kr)
	ctx := context.Background()
	methods := []string{"webauthn"}

	// verify reports whether code was accepted for purpose, through the entry
	// point the consumer uses. A refusal must be the invalid-code 403.
	type entry func(t *testing.T, userID string, purpose stepup.Purpose, code string) bool
	stepupErr := func(t *testing.T, e *stepup.Error) bool {
		t.Helper()
		if e == nil {
			return true
		}
		require.Equal(t, http.StatusForbidden, e.Status)
		require.Equal(t, stepup.ErrMsgInvalidMFACode, e.Body["error"])
		return false
	}
	inTx := func(t *testing.T, fn func(tx *sql.Tx) bool) bool {
		t.Helper()
		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		require.NoError(t, err)
		ok := fn(tx)
		require.NoError(t, tx.Commit())
		return ok
	}
	pool := func(t *testing.T, userID string, purpose stepup.Purpose, code string) bool {
		ok, err := h.VerifyCode(ctx, userID, purpose, code)
		require.NoError(t, err)
		return ok
	}
	poolTx := func(t *testing.T, userID string, purpose stepup.Purpose, code string) bool {
		return inTx(t, func(tx *sql.Tx) bool {
			ok, err := h.VerifyCodeTx(ctx, tx, userID, purpose, code)
			require.NoError(t, err)
			return ok
		})
	}
	factor := func(t *testing.T, userID string, purpose stepup.Purpose, code string) bool {
		return stepupErr(t, stepup.VerifyMFAFactor(ctx, h, userID, purpose, code, methods))
	}
	factorTx := func(t *testing.T, userID string, purpose stepup.Purpose, code string) bool {
		return inTx(t, func(tx *sql.Tx) bool {
			return stepupErr(t, stepup.VerifyMFAFactorTx(ctx, tx, h, userID, purpose, code, methods))
		})
	}
	confirm := func(t *testing.T, userID string, purpose stepup.Purpose, code string) bool {
		subj := stepup.Subject{MFAEnabled: true, MFAMethods: methods}
		return inTx(t, func(tx *sql.Tx) bool {
			return stepupErr(t, mfaenforce.ConfirmTx(ctx, tx, subj, h, userID, purpose, code))
		})
	}

	for _, c := range []struct {
		purpose stepup.Purpose
		level   string
		verify  entry
	}{
		{stepup.PurposePasswordChange, "VerifyCodeTx (users.ChangePassword via verifyStepUpWithLockedUser)", poolTx},
		{stepup.PurposeE2EEKeyReset, "VerifyCodeTx (users.ReplaceMyKeys via verifyStepUpWithLockedUser)", poolTx},
		{stepup.PurposePurgeFenceDisable, "stepup.VerifyMFAFactorTx (users.gatePurgeFenceDisable)", factorTx},
		{stepup.PurposeSessionRevoke, "VerifyCode (sessions.authenticateForRevoke)", pool},
		{stepup.PurposeSessionsRevokeAll, "VerifyCode (sessions.authenticateForRevoke)", pool},
		{stepup.PurposeRevocationModeSet, "VerifyCode (sessions.authenticateForRevoke)", pool},
		{stepup.PurposeOwnershipTransfer, "VerifyCode (ownership.verifyMFA)", pool},
		{stepup.PurposeOwnershipReverse, "VerifyCode (ownership.verifyMFA)", pool},
		{stepup.PurposeDMPurge, "stepup.VerifyMFAFactor (dm.verifyPurgeStepUp)", factor},
		{stepup.PurposeDMClear, "stepup.VerifyMFAFactorTx (dm.verifyClearStepUp)", factorTx},
		{stepup.PurposeServerMFAEnforcementOff, "mfaenforce.ConfirmTx (servers toggle OFF)", confirm},
	} {
		t.Run(string(c.purpose), func(t *testing.T) {
			userID := iuCreateUser(t, db, iuPassword)
			auth := sfNewAuthenticator(t, db, userID)

			other := ipOtherPurpose(c.purpose)
			foreign := ipMintInlineToken(t, h, rdb, auth, userID, string(other))
			assert.False(t, c.verify(t, userID, c.purpose, foreign), "%s must refuse a token minted for %q", c.level, other)
			ipRequireStillValidFor(t, h, userID, other, foreign)

			own := ipMintInlineToken(t, h, rdb, auth, userID, string(c.purpose))
			assert.True(t, c.verify(t, userID, c.purpose, own), "%s must accept its own purpose's token", c.level)
			assert.False(t, c.verify(t, userID, c.purpose, own), "and only once")
		})
	}
}

// Every consumer purpose is covered by one of the two tables above. These
// tables drive each purpose at its verify entry point, so they cannot see a
// route passing another route's purpose: that is pinned at the handler, as in
// internal/users/key_reset_inline_purpose_test.go.
func TestInlinePurpose_TablesCoverTheClosedSet(t *testing.T) {
	require.Len(t, stepup.Purposes(), 14+11)
}

func TestInlinePurpose_VerifyCodeRefusesAnInvalidPurpose(t *testing.T) {
	db := iuNewTestDB(t)
	h := sfWebAuthnHandler(t, db, iuNewTestRedis(t), iuKeyring(t))
	for _, p := range []stepup.Purpose{"", "not.a.purpose"} {
		ok, err := h.VerifyCode(context.Background(), "user-fixture", p, "123456")
		require.ErrorIs(t, err, errInvalidStepUpPurpose)
		require.False(t, ok)
		tx, txErr := db.Begin()
		require.NoError(t, txErr)
		ok, err = h.VerifyCodeTx(context.Background(), tx, "user-fixture", p, "123456")
		require.ErrorIs(t, err, errInvalidStepUpPurpose)
		require.False(t, ok)
		require.NoError(t, tx.Rollback())
	}
}

func TestInlinePurpose_BeginBodyValidation(t *testing.T) {
	db := iuNewTestDB(t)
	userID := iuCreateUser(t, db, iuPassword)
	iuAddWebAuthnCredential(t, db, userID)
	rdb := iuNewTestRedis(t)
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))

	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"empty body", ``, http.StatusBadRequest},
		{"missing purpose", `{}`, http.StatusBadRequest},
		{"null purpose", `{"purpose":null}`, http.StatusBadRequest},
		{"unknown purpose", `{"purpose":"not.a.purpose"}`, http.StatusBadRequest},
		{"wrong type", `{"purpose":5}`, http.StatusBadRequest},
		{"not an object", `"mfa_settings.totp_setup"`, http.StatusBadRequest},
		{"array wrapper", `[{"purpose":"mfa_settings.totp_setup"}]`, http.StatusBadRequest},
		{"array value", `{"purpose":["mfa_settings.totp_setup"]}`, http.StatusBadRequest},
		{"empty purpose (the login pseudo-purpose)", `{"purpose":""}`, http.StatusBadRequest},
		{"value case differs", `{"purpose":"MFA_SETTINGS.TOTP_SETUP"}`, http.StatusBadRequest},
		{"value with NUL", `{"purpose":"mfa_settings.totp_setup\u0000"}`, http.StatusBadRequest},
		{"UTF-8 BOM", "\xef\xbb\xbf{\"purpose\":\"mfa_settings.totp_setup\"}", http.StatusBadRequest},
		{"NBSP prefix", "\u00a0{\"purpose\":\"mfa_settings.totp_setup\"}", http.StatusBadRequest},
		{"trailing JSON", `{"purpose":"mfa_settings.totp_setup"} {"purpose":"dm.purge"}`, http.StatusBadRequest},
		{"trailing garbage", `{"purpose":"mfa_settings.totp_setup"}x`, http.StatusBadRequest},
		{"oversized", `{"purpose":"mfa_settings.totp_setup","pad":"` + strings.Repeat("a", maxInlineBeginRequestBytes) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := ipBegin(t, h, userID, c.body)
			require.Equal(t, c.want, status, "%v", body)
			if c.want == http.StatusBadRequest {
				require.Equal(t, errMsgInvalidInlinePurpose, body["error"], "the 400 body is fixed and echoes nothing")
			}
			n, err := rdb.Exists(context.Background(), inlineSessionKey(userID)).Result()
			require.NoError(t, err)
			require.Zero(t, n, "a refused begin must start no ceremony")
		})
	}

	status, body := ipBegin(t, h, userID, `{"purpose":"mfa_settings.totp_setup"}`)
	require.Equal(t, http.StatusOK, status, "positive control: %v", body)
}

// Finish takes the purpose from the stored session. A purpose field in the
// finish body is ignored, so a client cannot re-aim a ceremony.
func TestInlinePurpose_FinishIgnoresABodyPurpose(t *testing.T) {
	db := iuNewTestDB(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))

	status, body := ipBegin(t, h, userID, `{"purpose":"mfa_settings.backup_email_set"}`)
	require.Equal(t, http.StatusOK, status, "%v", body)
	raw, err := rdb.Get(context.Background(), inlineSessionKey(userID)).Bytes()
	require.NoError(t, err)
	var session webauthn.SessionData
	require.NoError(t, json.Unmarshal(raw, &session))

	var assertion map[string]any
	require.NoError(t, json.Unmarshal([]byte(auth.assertion(t, session.Challenge)), &assertion))
	assertion["purpose"] = string(stepup.PurposeServerMFAEnforcementOff)
	finishBody, err := json.Marshal(assertion)
	require.NoError(t, err)
	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/verify-inline/finish", string(finishBody), userID, "")
	h.WebAuthnVerifyInlineFinish(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	token, _ := iuBody(t, w)["mfa_token"].(string)
	require.NotEmpty(t, token)

	ok, err := h.VerifyCode(context.Background(), userID, stepup.PurposeServerMFAEnforcementOff, token)
	require.NoError(t, err)
	require.False(t, ok, "the finish body's purpose must not bind the token")
	ipRequireStillValidFor(t, h, userID, stepup.PurposeBackupEmailSet, token)
}

// A stored session that names no known purpose (one written before purposes
// existed) mints nothing.
func TestInlinePurpose_FinishRefusesASessionWithoutAPurpose(t *testing.T) {
	db := iuNewTestDB(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))
	require.NoError(t, rdb.Set(context.Background(), inlineSessionKey(userID), `{"challenge":"x"}`, 0).Err())

	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/verify-inline/finish", `{}`, userID, "")
	h.WebAuthnVerifyInlineFinish(c)

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Equal(t, errMsgNoInlineSession, iuBody(t, w)["error"])
}
