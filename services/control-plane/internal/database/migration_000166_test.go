package database_test

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestMigrations000165And000166DefaultsNewRows(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "newpresenceprivacydefaults")
	_, err := ts.DB.Exec(`INSERT INTO user_presence_settings (user_id) VALUES ($1)`, user.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO privacy_settings (user_id) VALUES ($1)`, user.ID)
	require.NoError(t, err)

	var details, fof bool
	var privateTier int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT server_voice_show_details, private_call_tier
		FROM user_presence_settings WHERE user_id = $1`, user.ID).Scan(&details, &privateTier))
	require.False(t, details)
	require.Equal(t, 1, privateTier)
	require.NoError(t, ts.DB.QueryRow(`
		SELECT dm_friends_of_friends FROM privacy_settings WHERE user_id = $1`, user.ID).Scan(&fof))
	require.True(t, fof)
	var friendRequestMode string
	require.NoError(t, ts.DB.QueryRow(`
		SELECT allow_friend_requests_from FROM privacy_settings WHERE user_id = $1
	`, user.ID).Scan(&friendRequestMode))
	require.Equal(t, "mutual_servers", friendRequestMode)
}
