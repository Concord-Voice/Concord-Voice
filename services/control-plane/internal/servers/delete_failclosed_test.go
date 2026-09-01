package servers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

// Server delete fails CLOSED on a write failure: the server survives and the
// response reports the failure. Exercised against a REAL transaction so the
// rollback path is the genuine article.
func TestDeleteServerFailsClosedWhenTheDeleteFails(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "delfailowner")
	sender := ts.CreateTestUser(t, "delfailsender")
	serverID := ts.CreateTestServer(t, owner.ID, "DelFailServer")
	ts.AddMemberToServer(t, serverID, sender.ID, "member")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice")
	insertVoiceParticipant(t, ts, channelID, sender.ID,
		time.Date(2026, time.January, 16, 10, 0, 0, 0, time.UTC))

	_, err := ts.DB.Exec(`
		CREATE OR REPLACE FUNCTION concord_test_fail_server_delete() RETURNS TRIGGER AS $$
		BEGIN
			RAISE EXCEPTION 'concord test: forced servers delete failure';
		END;
		$$ LANGUAGE plpgsql;
	`)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		CREATE TRIGGER concord_test_fail_server_delete_trg
		BEFORE DELETE ON servers
		FOR EACH ROW EXECUTE FUNCTION concord_test_fail_server_delete();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(`DROP TRIGGER IF EXISTS concord_test_fail_server_delete_trg ON servers`)
		_, _ = ts.DB.Exec(`DROP FUNCTION IF EXISTS concord_test_fail_server_delete()`)
	})

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID, nil,
		testhelpers.AuthHeaders(owner.AccessToken))

	require.Equal(t, http.StatusInternalServerError, w.Code,
		"the write failed inside the transaction, so nothing committed; a range would accept the 503 that means it did")

	var rows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM servers WHERE id = $1`, serverID).Scan(&rows))
	require.Equal(t, 1, rows, "the server must survive a failed delete")
	require.Equal(t, 1, countRows(t, ts,
		`SELECT COUNT(*) FROM voice_participants WHERE channel_id = $1`, channelID),
		"the active sender must survive a failed delete")
	require.Zero(t, countRows(t, ts,
		`SELECT COUNT(*) FROM presence_active_pending_plans WHERE user_id = $1`, sender.ID),
		"a rolled-back delete must leave no active reconciliation plan")
}
