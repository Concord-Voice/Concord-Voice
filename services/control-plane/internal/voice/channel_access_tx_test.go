package voice_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func channelAccessIdentity(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) loginfamily.AccessIdentity {
	t.Helper()
	claims, err := auth.ValidateAccessToken(user.AccessToken, ts.Config.JWTSecret)
	require.NoError(t, err)
	return loginfamily.AccessIdentity{
		UserID:          uuid.MustParse(user.ID),
		SID:             uuid.MustParse(claims.SessionID),
		CredentialEpoch: claims.CredentialEpoch,
		AccessExpiresAt: claims.ExpiresAt.Time,
	}
}

func TestCheckChannelVoiceFamilyAccessTxRequiresLiveAuthority(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "renewal_channel_owner")
	outsider := ts.CreateTestUser(t, "renewal_channel_outsider")
	serverID := ts.CreateTestServer(t, owner.ID, "renewal authority")
	channelID := ts.CreateVoiceChannel(t, serverID, "renewal room")
	resolver := rbac.NewResolver(nil, nil, nil)
	ownerIdentity := channelAccessIdentity(t, ts, owner)

	for _, test := range []struct {
		name     string
		identity loginfamily.AccessIdentity
		status   int
	}{
		{name: "owner admitted", identity: ownerIdentity, status: http.StatusNoContent},
		{name: "outsider denied", identity: channelAccessIdentity(t, ts, outsider), status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := ts.DB.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			status, err := voice.CheckChannelVoiceFamilyAccessTx(context.Background(), tx, resolver, serverID, channelID, test.identity)
			require.Equal(t, test.status, status)
			if test.status == http.StatusNoContent {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "Channel not found or access denied")
			}
			require.NoError(t, tx.Rollback())
		})
	}

	// A terminal family is denied even though channel authority is intact.
	_, err := ts.DB.Exec(`UPDATE login_families SET terminal_at=clock_timestamp(),epoch_generation=$2
 WHERE family_id=(SELECT family_id FROM login_family_sids WHERE sid=$1)`, ownerIdentity.SID, uuid.New())
	require.NoError(t, err)
	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	status, err := voice.CheckChannelVoiceFamilyAccessTx(context.Background(), tx, resolver, serverID, channelID, ownerIdentity)
	require.Equal(t, http.StatusUnauthorized, status)
	require.ErrorIs(t, err, loginfamily.ErrDenied)
	require.NoError(t, tx.Rollback())

	tx, err = ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	status, err = voice.CheckChannelVoiceFamilyAccessTx(context.Background(), tx, resolver, serverID, channelID, ownerIdentity)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Error(t, err)
}
