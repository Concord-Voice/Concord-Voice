package mfa

// Regressions for the WebAuthn inline-token purpose binding (RS11, #3453)
// across a rolling deploy, plus the surfaces an adversarial pass attacked and
// found holding. Every token comes from a real begin+finish ceremony
// (ipMintInlineToken), never a seeded key, except where a test plants a key to
// model what another binary writes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// legacyConsumeWebAuthnInlineToken is consumeWebAuthnInlineToken as it shipped
// before purpose binding: what a not-yet-upgraded control-plane replica runs
// during a rolling deploy. Kept verbatim so the test models the real old key.
func legacyConsumeWebAuthnInlineToken(ctx context.Context, rdb *redis.Client, userID, code string) (bool, error) {
	if len(code) <= 20 {
		return false, nil
	}
	token, err := rdb.GetDel(ctx, fmt.Sprintf("mfa_inline_token:%s:%s", userID, code)).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return token != "", nil
}

// An old replica must not be able to spend a purpose-bound token. The old
// consumer derives its key as mfa_inline_token:<uid>:<code>. If a new token's
// key were mfa_inline_token:<uid>:<P>:<token>, then mfa_code "<P>:<token>"
// sent to ANY consumer route served by an old replica would address it
// exactly, and the binding would fail open for the whole rolling window. The
// new key must be one no old-binary code can spell.
func TestInlinePurpose_OldReplicaCannotSpendAPurposeBoundToken(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, kr)

	// Minted by the new binary for a low-stakes action.
	token := ipMintInlineToken(t, h, rdb, auth, userID, string(stepup.PurposeBackupEmailSet))

	// Old replica serving, say, POST /sessions/revoke-all or ownership
	// transfer: it knows nothing of purposes and reads the raw code.
	crafted := string(stepup.PurposeBackupEmailSet) + ":" + token
	accepted, err := legacyConsumeWebAuthnInlineToken(context.Background(), rdb, userID, crafted)
	require.NoError(t, err)
	assert.False(t, accepted,
		"an old-binary consumer accepted mfa_code %q, spending a token bound to %q on an arbitrary route",
		"<purpose>:<token>", stepup.PurposeBackupEmailSet)
}

// A new replica's ceremony session must be invisible to an old replica's
// finish. The old finish GETDELs mfa_inline_session:<uid> and decodes it as a
// bare webauthn.SessionData, dropping the purpose, so a ceremony begun on a new
// replica and finished on an old one would mint an UNBOUND token that every
// route an old replica serves accepts.
func TestInlinePurpose_OldReplicaCannotFinishANewSession(t *testing.T) {
	db := iuNewTestDB(t)
	userID := iuCreateUser(t, db, iuPassword)
	iuAddWebAuthnCredential(t, db, userID)
	rdb := iuNewTestRedis(t)
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))
	ctx := context.Background()

	status, body := ipBegin(t, h, userID, fmt.Sprintf(`{"purpose":%q}`, stepup.PurposeBackupEmailSet))
	require.Equal(t, http.StatusOK, status, "%v", body)

	// Positive control: begin stored exactly one session for this user.
	keys, err := rdb.Keys(ctx, "mfa_inline*session:"+userID).Result()
	require.NoError(t, err)
	require.Len(t, keys, 1, "begin must store one ceremony session")

	n, err := rdb.Exists(ctx, "mfa_inline_session:"+userID).Result()
	require.NoError(t, err)
	assert.Zero(t, n, "the new session sits at the key an old-binary finish reads, so an old replica can finish it unbound")
}

// The reverse: a session an old replica's begin stored (no purpose) never
// mints on a new replica.
func TestInlinePurpose_NewReplicaCannotFinishAnOldSession(t *testing.T) {
	db := iuNewTestDB(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))
	ctx := context.Background()

	status, body := ipBegin(t, h, userID, fmt.Sprintf(`{"purpose":%q}`, stepup.PurposeBackupEmailSet))
	require.Equal(t, http.StatusOK, status, "%v", body)
	keys, err := rdb.Keys(ctx, "mfa_inline*session:"+userID).Result()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	raw, err := rdb.GetDel(ctx, keys[0]).Bytes()
	require.NoError(t, err)
	var session webauthn.SessionData
	require.NoError(t, json.Unmarshal(raw, &session))
	// What an old begin stores: the bare session, no purpose, at the old key.
	oldSession, err := json.Marshal(session)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(ctx, "mfa_inline_session:"+userID, oldSession, 0).Err())

	c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/verify-inline/finish", auth.assertion(t, session.Challenge), userID, "")
	h.WebAuthnVerifyInlineFinish(c)
	assert.Equal(t, http.StatusBadRequest, w.Code, "an old session must not mint: %s", w.Body.String())
	assert.Nil(t, iuBody(t, w)["mfa_token"])
}

// An old-format token (mfa_inline_token:<uid>:<token>, minted by an
// old replica's finish) is never accepted by the new code under any purpose,
// nor via a colon-crafted code, and the refusal does not delete it.
func TestInlinePurpose_OldFormatTokenNeverAccepted(t *testing.T) {
	db := iuNewTestDB(t)
	rdb := iuNewTestRedis(t)
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))
	userID := iuCreateUser(t, db, iuPassword)
	ctx := context.Background()
	const oldToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // base64url, 40 chars
	oldKey := "mfa_inline_token:" + userID + ":" + oldToken
	require.NoError(t, rdb.Set(ctx, oldKey, "1", 0).Err())

	for _, p := range stepup.Purposes() {
		for _, code := range []string{oldToken, ":" + oldToken, "::" + oldToken} {
			ok, err := h.VerifyCode(ctx, userID, p, code)
			require.NoError(t, err)
			assert.False(t, ok, "purpose %q accepted an old-format token via code %q", p, code)
		}
	}
	n, err := rdb.Exists(ctx, oldKey).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "the old-format key must be untouched")
}

// A colon-crafted code cannot re-address another purpose's key; the
// real token survives every attempt and still spends on its own purpose.
func TestInlinePurpose_ColonInjectionCannotReachAnotherPurpose(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, kr)
	ctx := context.Background()

	p := stepup.PurposeBackupEmailSet
	token := ipMintInlineToken(t, h, rdb, auth, userID, string(p))
	crafted := []string{
		token,
		string(p) + ":" + token,
		":" + string(p) + ":" + token,
		userID + ":" + string(p) + ":" + token,
		"../" + string(p) + ":" + token,
		token + "*",
		"*" + token,
	}
	for _, q := range stepup.Purposes() {
		if q == p {
			continue
		}
		for _, code := range crafted {
			ok, err := h.VerifyCode(ctx, userID, q, code)
			require.NoError(t, err)
			assert.False(t, ok, "consumer %q accepted crafted code %q for a %q token", q, code, p)
		}
	}
	ok, err := h.VerifyCode(ctx, userID, p, token)
	require.NoError(t, err)
	assert.True(t, ok, "the real token must survive every crafted attempt")
}

// A second begin replaces the session (challenge AND purpose), so an
// assertion over the first begin's challenge mints nothing, and an assertion
// over the second mints a token for the second purpose only.
func TestInlinePurpose_SecondBeginRebindsChallengeAndPurpose(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	auth := sfNewAuthenticator(t, db, userID)
	h := sfWebAuthnHandler(t, db, rdb, kr)
	ctx := context.Background()
	p, q := stepup.PurposeBackupEmailSet, stepup.PurposeServerMFAEnforcementOff

	challengeOf := func() string {
		raw, err := rdb.Get(ctx, inlineSessionKey(userID)).Bytes()
		require.NoError(t, err)
		var s webauthn.SessionData
		require.NoError(t, json.Unmarshal(raw, &s))
		return s.Challenge
	}
	finish := func(challenge string) (int, map[string]interface{}) {
		c, w := iuGinContext(http.MethodPost, "/api/v1/mfa/webauthn/verify-inline/finish", auth.assertion(t, challenge), userID, "")
		h.WebAuthnVerifyInlineFinish(c)
		return w.Code, iuBody(t, w)
	}

	// begin(P) then begin(Q); finish with P's challenge.
	status, _ := ipBegin(t, h, userID, fmt.Sprintf(`{"purpose":%q}`, p))
	require.Equal(t, http.StatusOK, status)
	challengeP := challengeOf()
	status, _ = ipBegin(t, h, userID, fmt.Sprintf(`{"purpose":%q}`, q))
	require.Equal(t, http.StatusOK, status)
	status, body := finish(challengeP)
	assert.Equal(t, http.StatusForbidden, status, "an assertion over a replaced challenge must not mint: %v", body)
	assert.Nil(t, body["mfa_token"])

	// begin(P) then begin(Q); finish with Q's challenge -> Q-only token.
	status, _ = ipBegin(t, h, userID, fmt.Sprintf(`{"purpose":%q}`, p))
	require.Equal(t, http.StatusOK, status)
	status, _ = ipBegin(t, h, userID, fmt.Sprintf(`{"purpose":%q}`, q))
	require.Equal(t, http.StatusOK, status)
	status, body = finish(challengeOf())
	require.Equal(t, http.StatusOK, status, "%v", body)
	token, _ := body["mfa_token"].(string)
	require.NotEmpty(t, token)
	ok, err := h.VerifyCode(ctx, userID, p, token)
	require.NoError(t, err)
	assert.False(t, ok, "the replaced begin's purpose must not bind the token")
	ok, err = h.VerifyCode(ctx, userID, q, token)
	require.NoError(t, err)
	assert.True(t, ok)
}

// The login MFA challenge accepts no inline token, whatever purpose minted it,
// under either code-bearing method, and does not consume it.
func TestInlinePurpose_LoginRefusesATokenOfEveryPurpose(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	rdb := iuNewTestRedis(t)
	h := sfWebAuthnHandler(t, db, rdb, kr)

	for _, p := range stepup.Purposes() {
		for _, method := range []string{"totp", "backup_code"} {
			t.Run(string(p)+"/"+method, func(t *testing.T) {
				userID := iuCreateUser(t, db, iuPassword)
				auth := sfNewAuthenticator(t, db, userID)
				iuEnrollTOTP(t, db, kr, userID)
				token := ipMintInlineToken(t, h, rdb, auth, userID, string(p))
				challenge, _, err := GenerateChallengeToken(userID, PurposeSuspiciousRefresh, JWTSecret("test"), "")
				require.NoError(t, err)
				body := fmt.Sprintf(`{"mfa_challenge_token":%q,"method":%q,"code":%q}`, challenge, method, token)
				c, w := iuGinContext(http.MethodPost, "/api/v1/auth/mfa/verify", body, "", "")
				h.Verify(c)
				resp := iuBody(t, w)
				assert.NotEqual(t, http.StatusOK, w.Code, "login accepted a %q token: %v", p, resp)
				assert.NotEqual(t, true, resp["verified"])
				ok, err := h.VerifyCode(context.Background(), userID, p, token)
				require.NoError(t, err)
				assert.True(t, ok, "login must not have consumed the %q token", p)
			})
		}
	}
}
