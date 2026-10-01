package stepup

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The own-rule seam (#3455 D-1) picks the factor from the account, not from
// the request: an MFA account verifies its code through the tx verifier under
// the caller's purpose, and a password step-up token is never a substitute for
// it. A password account spends a password step-up token (#3509).

func TestVerifyOwnRuleTx_MFAAccountVerifiesTheCodeUnderThePurpose(t *testing.T) {
	v := &fakeMFAVerifier{enabled: true, valid: true}
	subj := Subject{PasswordHash: "unused", MFAEnabled: true, MFAMethods: []string{"totp"}}

	// Not PurposeDMClear: the seam was hoisted out of DM Clear, and a purpose
	// left hard-coded from there is the regression this must catch.
	err := VerifyOwnRuleTx(context.Background(), nil, v, "user-1", OwnRuleRoute{Purpose: PurposeChannelPurge, Copy: testCopy},
		Input{MFACode: "123456"}, subj)

	require.Nil(t, err)
	require.True(t, v.usedTx, "the own rule must verify inside the caller's transaction")
	require.False(t, v.usedPool, "the own rule must not fall back to the pool verifier")
	require.Equal(t, PurposeChannelPurge, v.purpose, "the code must be bound to the caller's purpose")
}

func TestVerifyOwnRuleTx_MFAAccountRefusesAPasswordTokenInPlaceOfTheCode(t *testing.T) {
	v := &fakeMFAVerifier{enabled: true, valid: true}
	subj := Subject{PasswordHash: "unused", MFAEnabled: true, MFAMethods: []string{"totp"}}

	// A nil tx: the MFA arm must refuse before it would touch one.
	err := VerifyOwnRuleTx(context.Background(), nil, v, "user-1", OwnRuleRoute{Purpose: PurposeDMClear, Copy: testCopy},
		Input{StepUpToken: "a-password-step-up-token"}, subj)

	require.NotNil(t, err)
	require.Equal(t, http.StatusForbidden, err.Status)
	require.Equal(t, true, err.Body["mfa_required"], "an MFA account must be asked for its code")
	require.Equal(t, []string{"totp"}, err.Body["methods"])
	require.NotContains(t, err.Body, "password_required")
	require.False(t, v.usedTx, "no code was sent, so nothing reaches the verifier")
}

func TestVerifyOwnRuleTx_MFAAccountWithoutAVerifierIs500(t *testing.T) {
	subj := Subject{MFAEnabled: true, MFAMethods: []string{"totp"}}

	err := VerifyOwnRuleTx(context.Background(), nil, nil, "user-1", OwnRuleRoute{Purpose: PurposeDMClear, Copy: testCopy},
		Input{MFACode: "123456"}, subj)

	require.NotNil(t, err)
	require.Equal(t, http.StatusInternalServerError, err.Status)
	require.Equal(t, ErrMsgVerificationFailed, err.Body["error"])
}

// A password account confirms with a password step-up token spent on the
// caller's transaction (#3509); the password itself never reaches the seam.
func TestVerifyOwnRuleTx_PasswordAccountSpendsAPasswordToken(t *testing.T) {
	db := subjectTestDB(t)
	ctx := context.Background()
	userID := subjectTestUser(t, db)
	route := OwnRuleRoute{Purpose: PurposeDMClear, Copy: testCopy}
	token, e := MintToken(ctx, db, userID, FactorPassword, PurposeDMClear, "")
	require.Nil(t, e)
	subj, e := LoadSubject(ctx, db, userID)
	require.Nil(t, e)
	require.False(t, subj.MFAEnabled, "precondition: a password account")
	v := &fakeMFAVerifier{}

	verify := func(in Input) *Error {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Commit() }()
		return VerifyOwnRuleTx(ctx, tx, v, userID, route, in, subj)
	}

	require.Nil(t, verify(Input{StepUpToken: token}))
	require.False(t, v.usedTx, "a password account never reaches the MFA verifier")

	spent := verify(Input{StepUpToken: token})
	require.NotNil(t, spent, "a token is single-use")
	require.Equal(t, http.StatusForbidden, spent.Status)
	require.Equal(t, gin.H{
		"error": ErrMsgStepUpTokenInvalid, "password_required": true, "step_up_token_invalid": true,
	}, spent.Body)
	require.Nil(t, spent.Cause)

	missing := verify(Input{})
	require.NotNil(t, missing)
	require.Equal(t, http.StatusForbidden, missing.Status)
	require.Equal(t, gin.H{"error": testCopy.CredentialRequired, "password_required": true}, missing.Body,
		"no token is the prompt, without the invalid-token flag")
}

func TestVerifyOwnRuleTx_PasswordAccountWithoutAPasswordUsesTheRouteCopy(t *testing.T) {
	// Bound to a non-credential-shaped identifier, as in stepup_test.go (G101).
	const truncatedHash = "$argon2id$vali"

	// A nil tx: a code alone never reaches the token spend.
	err := VerifyOwnRuleTx(context.Background(), nil, nil, "user-1", OwnRuleRoute{Purpose: PurposeDMClear, Copy: testCopy},
		Input{MFACode: "123456"}, Subject{PasswordHash: truncatedHash})

	require.NotNil(t, err)
	require.Equal(t, http.StatusForbidden, err.Status)
	require.Equal(t, testCopy.CredentialRequired, err.Body["error"])
	require.Equal(t, true, err.Body["password_required"], "a code does not substitute for the password")
}
