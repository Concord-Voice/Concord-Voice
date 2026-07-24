package mfa_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// testRing is any valid keyring — the challenge generators do not use it, they
// only touch h.jwtSecret and h.redis, but NewHandler requires one.
func testRing(t *testing.T) *mfa.Keyring {
	t.Helper()
	ring, err := mfa.ParseKeyring("0101010101010101010101010101010101010101010101010101010101010101", 1, "")
	require.NoError(t, err)
	return ring
}

// TestGenerateLoginChallenge_StampsEpoch covers the #2418 login-challenge issuer:
// the epoch it is handed is carried in the token's cred_epoch claim, and the
// remember_me preference is stored in Redis keyed by the challenge JTI.
func TestGenerateLoginChallenge_StampsEpoch(t *testing.T) {
	ts := setupTS(t)
	h := mfa.NewHandler(ts.DB, ts.Redis, logger.New("test"), testRing(t), testhelpers.TestJWTSecret, nil, "test")

	token, jti, err := h.GenerateLoginChallenge(context.Background(), "user-xyz", true, "epoch-E1")
	require.NoError(t, err)
	require.NotEmpty(t, token)
	require.NotEmpty(t, jti)

	claims, err := mfa.ValidateChallengeToken(token, testhelpers.TestJWTSecret, mfa.PurposeLogin)
	require.NoError(t, err)
	assert.Equal(t, "epoch-E1", claims.CredentialEpoch,
		"the login challenge must carry the epoch it was issued under")

	remembered := ts.Redis.Get(context.Background(), fmt.Sprintf("mfa_challenge:%s:remember_me", jti)).Val()
	assert.Equal(t, "1", remembered, "remember_me must be stored keyed by the challenge JTI")
}

// TestGenerateUpgradeChallenge_CarriesNoEpoch covers the #2418 decision that an
// MFA-upgrade challenge is unstamped: it completes into a 30s Redis bypass key, not
// a session mint, so binding it to an epoch would be incoherent.
func TestGenerateUpgradeChallenge_CarriesNoEpoch(t *testing.T) {
	ts := setupTS(t)
	h := mfa.NewHandler(ts.DB, ts.Redis, logger.New("test"), testRing(t), testhelpers.TestJWTSecret, nil, "test")

	token, jti, err := h.GenerateUpgradeChallenge(context.Background(), "user-xyz", false)
	require.NoError(t, err)
	require.NotEmpty(t, token)
	require.NotEmpty(t, jti)

	claims, err := mfa.ValidateChallengeToken(token, testhelpers.TestJWTSecret, mfa.PurposeMFAUpgrade)
	require.NoError(t, err)
	assert.Empty(t, claims.CredentialEpoch, "an MFA-upgrade challenge must carry no epoch")
}
