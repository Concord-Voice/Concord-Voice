package voice_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/stretchr/testify/require"
)

func TestCheckChannelVoiceAccessTxRequiresLiveAuthority(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "renewal_channel_owner")
	outsider := ts.CreateTestUser(t, "renewal_channel_outsider")
	serverID := ts.CreateTestServer(t, owner.ID, "renewal authority")
	channelID := ts.CreateVoiceChannel(t, serverID, "renewal room")
	resolver := rbac.NewResolver(nil, nil, nil)

	for _, test := range []struct {
		name   string
		userID string
		status int
	}{
		{name: "owner admitted", userID: owner.ID, status: http.StatusNoContent},
		{name: "outsider denied", userID: outsider.ID, status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := ts.DB.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			status, err := voice.CheckChannelVoiceAccessTx(context.Background(), tx, resolver, serverID, channelID, test.userID, "")
			require.Equal(t, test.status, status)
			if test.status == http.StatusNoContent {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "Channel not found or access denied")
			}
			require.NoError(t, tx.Rollback())
		})
	}

	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	status, err := voice.CheckChannelVoiceAccessTx(context.Background(), tx, resolver, serverID, channelID, owner.ID, "")
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.ErrorContains(t, err, "lock voice visibility")
}
