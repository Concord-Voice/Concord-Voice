package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2201 (Codex #2397 review, F1): the post-rotation continuation pair must bind
// to the epoch the destructive flow committed. If a concurrent second flow
// advanced users.credential_epoch in the window between that commit and this
// best-effort mint, IssueTokenPair must skip — no live pair, no refresh row —
// so the earlier request cannot receive a session the later flow meant to kill.

func TestIssueTokenPair_SkipsWhenEpochAdvanced(t *testing.T) {
	rig := newFamilyTestRig(t)
	const currentEpoch = "00112233445566778899aabbccddeeff"
	const staleEpoch = "ffeeddccbbaa99887766554433221100"

	// The caller committed staleEpoch before a later flow advanced it. Keep
	// both epochs valid so only the superseded-epoch guard refuses the mint.
	_, err := rig.db.Exec(`UPDATE users SET credential_epoch = $1 WHERE id = $2`, currentEpoch, rig.user)
	require.NoError(t, err)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	access, refresh, sessionID, err := rig.h.IssueTokenPair(c, rig.user, staleEpoch)
	assert.ErrorIs(t, err, ErrContinuationEpochAdvanced,
		"a continuation mint under a superseded epoch must be refused")

	assert.Empty(t, access, "no access token may be returned when the epoch advanced")
	assert.Empty(t, refresh, "no refresh token may be returned when the epoch advanced")
	assert.Empty(t, sessionID, "no session id may be returned when the epoch advanced")
	for _, query := range []string{
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1`,
		`SELECT count(*) FROM login_families WHERE user_id = $1`,
		`SELECT count(*) FROM login_family_sids WHERE user_id = $1`,
	} {
		var n int
		require.NoError(t, rig.db.QueryRow(query, rig.user).Scan(&n))
		assert.Zero(t, n, "no refresh or family authority may be minted when the epoch advanced: %s", query)
	}
}

func TestIssueTokenPair_MintsWhenEpochMatches(t *testing.T) {
	rig := newFamilyTestRig(t)
	const epoch = "00112233445566778899aabbccddeeff"

	_, err := rig.db.Exec(`UPDATE users SET credential_epoch = $1, email_verified = true WHERE id = $2`, epoch, rig.user)
	require.NoError(t, err)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	access, refresh, sessionID, err := rig.h.IssueTokenPair(c, rig.user, epoch)
	require.NoError(t, err)
	assert.NotEmpty(t, access)
	assert.NotEmpty(t, refresh)
	assert.NotEmpty(t, sessionID)

	_, signedSID := assertFamilyAccess(t, rig, access)
	assert.Equal(t, sessionID, signedSID)
	claims, err := ValidateAccessToken(access, familyTestSecret)
	require.NoError(t, err)
	assert.Equal(t, epoch, claims.CredentialEpoch)
	require.NotNil(t, claims.EmailVerified)
	assert.True(t, *claims.EmailVerified)

	var storedID string
	require.NoError(t, rig.db.QueryRow(
		`SELECT id FROM refresh_tokens WHERE user_id = $1 AND token_hash = $2`, rig.user, HashRefreshToken(refresh)).Scan(&storedID))
	assert.Equal(t, sessionID, storedID, "the minted refresh row must carry the returned session id")
}
