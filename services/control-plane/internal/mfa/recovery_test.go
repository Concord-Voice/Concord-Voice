package mfa

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/stretchr/testify/require"
)

const recoveryPolicyQuery = "SELECT recovery_only_methods, recovery_hardened, mfa_methods"

func TestRecoveryMethodsUseActualReservedFactors(t *testing.T) {
	for _, tc := range []struct {
		name, reserved, flags    string
		totp, webauthn, hardened bool
		want                     []string
		refused                  bool
	}{
		{name: "no extra factor with default hardened flag", reserved: "{}", hardened: true, want: []string{}},
		{name: "email already proved", reserved: "{email}", want: []string{}},
		{name: "actual TOTP despite stale flags", reserved: "{totp}", totp: true, want: []string{"totp"}},
		{name: "actual key despite stale flags", reserved: "{webauthn}", webauthn: true, want: []string{"webauthn"}},
		{name: "removed factor no longer required", reserved: "{totp,webauthn}", want: []string{}},
		{name: "reserved inline alternatives", reserved: "{webauthn,totp,totp}", totp: true, webauthn: true, want: []string{"webauthn", "totp"}},
		{name: "unknown factor fails closed", reserved: "{unknown}", refused: true},
		{name: "SMS fails closed", reserved: "{sms}", refused: true},
		{name: "hardened SMS mirror fails closed", reserved: "{}", flags: "{sms}", hardened: true, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.flags == "" {
				tc.flags = "{}"
			}
			h, _ := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{match: recoveryPolicyQuery, values: []driver.Value{[]byte(tc.reserved), tc.hardened, []byte(tc.flags), tc.totp, tc.webauthn}})
			methods, err := h.GetRecoveryMethods(context.Background(), enrollUser)
			if tc.refused {
				require.Error(t, err)
				require.Nil(t, methods)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, methods)
			}
		})
	}
}

func TestRecoveryMethodsFailClosedOnDependencyErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		answer     scriptedAnswer
		redisFault bool
	}{
		{name: "database failure", answer: scriptedAnswer{match: recoveryPolicyQuery, err: errReadFailed}},
		{name: "missing user", answer: scriptedAnswer{match: recoveryPolicyQuery, noRows: true}},
		{name: "SMS policy store failure", answer: scriptedAnswer{match: recoveryPolicyQuery, values: []driver.Value{[]byte("{}"), true, []byte("{}"), false, false}}, redisFault: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, rdb := scriptedHandler(t, &mfaEventDB{}, "test", tc.answer)
			if tc.redisFault {
				rdb.AddHook(failKeyHook{name: "exists", prefix: "mfa_emailsms_enabled:"})
			}
			methods, err := h.GetRecoveryMethods(context.Background(), enrollUser)
			require.Error(t, err)
			require.Nil(t, methods)
		})
	}
}

type recoveryCompletionRecorder struct{ calls int }

func (r *recoveryCompletionRecorder) CompleteRecovery(c *gin.Context, _, _ string) bool {
	r.calls++
	c.JSON(http.StatusOK, gin.H{"recovery_token": "verified-test-token"})
	return true
}

func TestRecoveryChallengeRefusesUnreservedFactorBeforeSpendingIt(t *testing.T) {
	f := newVerifyFixture(t)
	h, _ := scriptedHandlerFull(t, f.state, "test", f.h.keyring, []scriptedAnswer{{match: recoveryPolicyQuery, values: []driver.Value{[]byte("{webauthn}"), false, []byte("{totp,webauthn}"), true, true}}}, nil)
	token, _, err := h.GenerateRecoveryChallenge(context.Background(), enrollUser)
	require.NoError(t, err)
	r := &recoveryCompletionRecorder{}
	h.SetRecoveryCompleter(r)
	w := verifyWith(h, fmt.Sprintf(`{"mfa_challenge_token":%q,"method":"totp","code":%q}`, token, f.right))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Zero(t, r.calls)
	require.Zero(t, f.judged(), "a valid unreserved factor must stay unspent")
	// A recovery challenge cannot turn the already-proved email into an MFA alternative.
	w = sfSendEmailCode(h, token)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRecoveryChallengeRechecksPolicyBeforeSpendingIt(t *testing.T) {
	f := newVerifyFixture(t)
	token, _, err := f.h.GenerateRecoveryChallenge(context.Background(), enrollUser)
	require.NoError(t, err)
	// The current policy no longer offers the authenticator this challenge began with.
	h, _ := scriptedHandlerFull(t, f.state, "test", f.h.keyring, []scriptedAnswer{{match: recoveryPolicyQuery, values: []driver.Value{[]byte("{}"), false, []byte("{totp}"), true, false}}}, nil)
	w := verifyWith(h, fmt.Sprintf(`{"mfa_challenge_token":%q,"method":"totp","code":%q}`, token, f.right))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Zero(t, f.judged())
}

func TestRecoveryWebAuthnProofIsBoundToItsChallenge(t *testing.T) {
	db := iuNewTestDB(t)
	rdb := iuNewTestRedis(t)
	userID := iuCreateUser(t, db, iuPassword)
	authenticator := sfNewAuthenticator(t, db, userID)
	_, err := db.Exec(`UPDATE users SET recovery_only_methods='{webauthn}',mfa_methods='{}' WHERE id=$1`, userID)
	require.NoError(t, err)
	h := sfWebAuthnHandler(t, db, rdb, iuKeyring(t))
	r := &recoveryCompletionRecorder{}
	h.SetRecoveryCompleter(r)
	token, jti, err := h.GenerateRecoveryChallenge(context.Background(), userID)
	require.NoError(t, err)
	_, err = h.BeginWebAuthnLogin(context.Background(), userID, jti)
	require.NoError(t, err)
	raw, err := rdb.Get(context.Background(), "mfa_webauthn_session:"+jti).Bytes()
	require.NoError(t, err)
	var session webauthn.SessionData
	require.NoError(t, json.Unmarshal(raw, &session))
	assertion := authenticator.assertion(t, session.Challenge)
	other, _, err := h.GenerateRecoveryChallenge(context.Background(), userID)
	require.NoError(t, err)
	w := verifyWith(h, fmt.Sprintf(`{"mfa_challenge_token":%q,"method":"webauthn","assertion":%s}`, other, assertion))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Zero(t, r.calls)
	w = verifyWith(h, fmt.Sprintf(`{"mfa_challenge_token":%q,"method":"webauthn","assertion":%s}`, token, assertion))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, r.calls)
}

func TestRecoveryPolicyRejectsPriorEmailAuthorityAfterTightening(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{
		match:  recoveryPolicyQuery,
		values: []driver.Value{[]byte("{totp}"), true, []byte("{totp}"), true, false},
	})
	token, _, err := GenerateRecoveryToken(enrollUser, JWTSecret("test"))
	require.NoError(t, err)
	_, err = h.ValidateRecoveryToken(context.Background(), token)
	require.Error(t, err, "email-only authority must not bypass the current reserved authenticator")
	require.NotErrorIs(t, err, auth.ErrRecoveryPolicyUnavailable)
}

func TestFinalRecoveryAuthoritySatisfiesCurrentPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, factor, reserved string
		want                   bool
	}{
		{name: "ordinary email recovery", factor: "email", reserved: "{}", want: true},
		{name: "reserved email already proved", factor: "email", reserved: "{email}", want: true},
		{name: "reserved authenticator proved", factor: "totp", reserved: "{totp}", want: true},
		{name: "reserved security key proved", factor: "webauthn", reserved: "{webauthn}", want: true},
		{name: "previous factor does not satisfy new reservation", factor: "totp", reserved: "{webauthn}"},
		{name: "removed reservation needs no extra proof", factor: "webauthn", reserved: "{}", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{match: recoveryPolicyQuery, values: []driver.Value{[]byte(tc.reserved), true, []byte("{}"), true, true}})
			token, _, err := h.GenerateRecoveryToken(enrollUser, tc.factor)
			require.NoError(t, err)
			claims, err := h.ValidateRecoveryToken(context.Background(), token)
			if tc.want {
				require.NoError(t, err)
				require.Equal(t, enrollUser, claims.UserID)
				require.Equal(t, tc.factor, claims.RecoveryFactor)
			} else {
				require.Error(t, err)
				require.Nil(t, claims)
			}
		})
	}
}

func TestFinalRecoveryAuthorityFailsClosedWithoutProofOrPolicy(t *testing.T) {
	h, _ := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{match: recoveryPolicyQuery, err: errReadFailed})
	token, _, err := h.GenerateRecoveryToken(enrollUser, "email")
	require.NoError(t, err)
	_, err = h.ValidateRecoveryToken(context.Background(), token)
	require.ErrorIs(t, err, errReadFailed)
	require.ErrorIs(t, err, auth.ErrRecoveryPolicyUnavailable)
	token, _, err = GenerateChallengeToken(enrollUser, PurposeRecovery, JWTSecret("test"), "")
	require.NoError(t, err)
	_, err = h.ValidateRecoveryToken(context.Background(), token)
	require.ErrorContains(t, err, "recovery proof missing")
	require.NotErrorIs(t, err, auth.ErrRecoveryPolicyUnavailable)
	_, _, err = h.GenerateRecoveryToken(enrollUser, "unverified")
	require.ErrorContains(t, err, "invalid recovery proof")
}

func TestFinalRecoveryAuthorityClassifiesRedisPolicyFailure(t *testing.T) {
	h, rdb := scriptedHandler(t, &mfaEventDB{}, "test", scriptedAnswer{
		match:  recoveryPolicyQuery,
		values: []driver.Value{[]byte("{}"), true, []byte("{}"), false, false},
	})
	rdb.AddHook(failKeyHook{name: "exists", prefix: "mfa_emailsms_enabled:"})
	token, _, err := h.GenerateRecoveryToken(enrollUser, "email")
	require.NoError(t, err)
	claims, err := h.ValidateRecoveryToken(context.Background(), token)
	require.Nil(t, claims)
	require.ErrorIs(t, err, auth.ErrRecoveryPolicyUnavailable)
}

func TestRecoveryClaimsTransactionUsesLockedPolicyWithoutRedis(t *testing.T) {
	for _, tc := range []struct {
		name, factor, reserved, flags string
		hardened, totp                bool
		lockErr, policyErr, want      error
	}{
		{name: "email without reservation", factor: "email", reserved: "{}", flags: "{}"},
		{name: "reserved proof", factor: "totp", reserved: "{totp}", flags: "{totp}", totp: true},
		{name: "policy strengthened", factor: "email", reserved: "{totp}", flags: "{totp}", totp: true, want: auth.ErrRecoveryProofInvalid},
		{name: "policy database unavailable", factor: "email", policyErr: errReadFailed, want: auth.ErrRecoveryPolicyUnavailable},
		{name: "owner lock unavailable", factor: "email", lockErr: errReadFailed, want: auth.ErrRecoveryPolicyUnavailable},
		{name: "unsupported reservation", factor: "email", reserved: "{unknown}", flags: "{}", want: auth.ErrRecoveryPolicyUnavailable},
		{name: "durable hardened SMS", factor: "email", reserved: "{}", flags: "{sms}", hardened: true, want: auth.ErrRecoveryPolicyUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &mfaEventDB{}
			h, rdb := scriptedHandler(t, state, "test",
				scriptedAnswer{match: "FOR NO KEY UPDATE", values: []driver.Value{enrollUser}, err: tc.lockErr},
				scriptedAnswer{match: recoveryPolicyQuery, values: []driver.Value{[]byte(tc.reserved), tc.hardened, []byte(tc.flags), tc.totp, false}, err: tc.policyErr},
			)
			rdb.AddHook(failKeyHook{name: "exists", prefix: ""})
			tx, err := h.db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, tx.Rollback()) }()
			err = h.ValidateRecoveryClaimsTx(context.Background(), tx, &auth.RecoveryClaims{UserID: enrollUser, RecoveryFactor: tc.factor})
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
			require.Contains(t, state.statements[0], "FOR NO KEY UPDATE")
		})
	}
}

func TestRecoveryClaimsTransactionRejectsMissingProof(t *testing.T) {
	h := &Handler{}
	require.ErrorIs(t, h.ValidateRecoveryClaimsTx(context.Background(), nil, nil), auth.ErrRecoveryProofInvalid)
	require.ErrorIs(t, h.ValidateRecoveryClaimsTx(context.Background(), nil, &auth.RecoveryClaims{RecoveryFactor: "email"}), auth.ErrRecoveryProofInvalid)
}
