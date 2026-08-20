package members_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

// failServerMemberDeletes makes every DELETE on server_members error, so the
// hooked revoking paths take their failure branch against a REAL transaction.
// A fabricated error object could not exercise the handler's classification.
func failServerMemberDeletes(t *testing.T, ts *testhelpers.TestServer) {
	t.Helper()
	_, err := ts.DB.Exec(`
		CREATE OR REPLACE FUNCTION concord_test_fail_member_delete() RETURNS TRIGGER AS $$
		BEGIN
			RAISE EXCEPTION 'concord test: forced server_members delete failure';
		END;
		$$ LANGUAGE plpgsql;
	`)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		CREATE TRIGGER concord_test_fail_member_delete_trg
		BEFORE DELETE ON server_members
		FOR EACH ROW EXECUTE FUNCTION concord_test_fail_member_delete();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(`DROP TRIGGER IF EXISTS concord_test_fail_member_delete_trg ON server_members`)
		_, _ = ts.DB.Exec(`DROP FUNCTION IF EXISTS concord_test_fail_member_delete()`)
	})
}

// Removal fails CLOSED: the write errors, nothing commits, and the member is
// still a member. The response must be classified rather than a blanket 500 —
// the distinction matters because a post-commit failure means the member IS
// removed, and telling a client otherwise invites a wrong retry.
func TestRemoveMemberFailsClosedWhenTheDeleteFails(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "failclosedowner")
	victim := ts.CreateTestUser(t, "failclosedvictim")
	serverID := ts.CreateTestServer(t, owner.ID, "FailClosedServer")
	ts.AddMemberToServer(t, serverID, victim.ID, "member")
	failServerMemberDeletes(t, ts)

	w := ts.DoRequest("DELETE", memberPath(serverID, victim.ID), nil,
		testhelpers.AuthHeaders(owner.AccessToken))

	require.Equal(t, http.StatusInternalServerError, w.Code,
		"the write failed inside the transaction, so nothing committed; a range would accept the 503 that means it did")

	var rows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, victim.ID).Scan(&rows))
	require.Equal(t, 1, rows, "the member must survive a failed removal")
}

// Ban fails closed the same way, and additionally must leave no ban row: a ban
// that recorded the ban but failed to remove the member would leave someone
// banned and still inside the server.
func TestBanMemberFailsClosedWhenTheDeleteFails(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "banclosedowner")
	victim := ts.CreateTestUser(t, "banclosedvictim")
	serverID := ts.CreateTestServer(t, owner.ID, "BanClosedServer")
	ts.AddMemberToServer(t, serverID, victim.ID, "member")
	failServerMemberDeletes(t, ts)

	w := ts.DoRequest("POST", banPath(serverID, victim.ID), nil,
		testhelpers.AuthHeaders(owner.AccessToken))

	require.Equal(t, http.StatusInternalServerError, w.Code,
		"the write failed inside the transaction, so nothing committed; a range would accept the 503 that means it did")

	var members, bans int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, victim.ID).Scan(&members))
	require.Equal(t, 1, members, "the member must survive a failed ban")
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_bans WHERE server_id = $1 AND user_id = $2`,
		serverID, victim.ID).Scan(&bans))
	require.Zero(t, bans, "no ban row may survive a rolled-back ban")
}

// Banning a NON-member must stay permitted (a pre-emptive ban) AND must not
// reconcile presence.
//
// FamilyMemberBan is a revoking family carrying no counterpart, so
// graphpresence's accepted-edge gate — which is written as
// `policy.CanRevokeVisibility && subject.Counterpart != uuid.Nil` — is
// structurally unreachable for it, and an ungated capture seeds plan.viewers
// with whoever was named. On the SUCCESS path that is a full websocket teardown
// of every device belonging to that person. Since any user can create a server
// and thereby hold PermBan on it, that would let an attacker force-disconnect an
// arbitrary stranger with no relationship to the server at all.
//
// execBanTx therefore probes membership inside the transaction and skips the
// capture entirely when the target is not a member.
//
// Coverage limit, stated rather than implied: this asserts the BEHAVIOUR is
// preserved (the ban lands, no error), not the absence of the disconnect. The
// disconnect happens on the hub, which this harness cannot observe, and the
// probe cannot be unit-tested because a nil *sql.Tx panics rather than erroring
// and `package members` cannot import testhelpers without an import cycle. The
// guard itself is verified by reading execBanTx.
func TestBanOfNonMemberStillSucceeds(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "preemptbanowner")
	stranger := ts.CreateTestUser(t, "preemptbanstranger")
	serverID := ts.CreateTestServer(t, owner.ID, "PreemptBanServer")

	w := ts.DoRequest("POST", banPath(serverID, stranger.ID), nil,
		testhelpers.AuthHeaders(owner.AccessToken))

	require.Less(t, w.Code, 500, "a pre-emptive ban must not error")

	var bans int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_bans WHERE server_id = $1 AND user_id = $2`,
		serverID, stranger.ID).Scan(&bans))
	require.Equal(t, 1, bans, "the ban row is recorded even though the target was not a member")
}
