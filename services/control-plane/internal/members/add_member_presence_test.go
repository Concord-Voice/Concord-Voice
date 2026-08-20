package members_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

// AC-10. The default-role insert used to be a blank-discarded h.db.Exec, so a
// failure left a roleless member and still returned 201. Both writes now share
// one transaction.
func TestAddMemberRollsBackWhenDefaultRoleInsertFails(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "atomicowner")
	joiner := ts.CreateTestUser(t, "atomicjoiner")
	serverID := ts.CreateTestServer(t, owner.ID, "AtomicServer")

	_, err := ts.DB.Exec(`
		CREATE OR REPLACE FUNCTION concord_test_fail_member_roles() RETURNS TRIGGER AS $$
		BEGIN
			RAISE EXCEPTION 'concord test: forced member_roles failure';
		END;
		$$ LANGUAGE plpgsql;
	`)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		CREATE TRIGGER concord_test_fail_member_roles_trg
		BEFORE INSERT ON member_roles
		FOR EACH ROW EXECUTE FUNCTION concord_test_fail_member_roles();
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(`DROP TRIGGER IF EXISTS concord_test_fail_member_roles_trg ON member_roles`)
		_, _ = ts.DB.Exec(`DROP FUNCTION IF EXISTS concord_test_fail_member_roles()`)
	})

	w := ts.DoRequest("POST", membersPath(serverID),
		map[string]string{"user_id": joiner.ID},
		testhelpers.AuthHeaders(owner.AccessToken))

	require.Equal(t, http.StatusInternalServerError, w.Code,
		"the role insert failed inside the transaction, so nothing committed")

	var rows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, joiner.ID).Scan(&rows))
	require.Zero(t, rows,
		"the membership row must roll back with the role insert; a roleless member is the bug")
}

// The happy path must still work end to end after the rewrite onto the hook.
func TestAddMemberStillSucceedsAndAssignsDefaultRoles(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "addowner")
	joiner := ts.CreateTestUser(t, "addjoiner")
	serverID := ts.CreateTestServer(t, owner.ID, "AddServer")

	w := ts.DoRequest("POST", membersPath(serverID),
		map[string]string{"user_id": joiner.ID},
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var members int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, joiner.ID).Scan(&members))
	require.Equal(t, 1, members, "the membership row is committed")

	// The default-role insert is the whole point of moving both writes into one
	// transaction; without this a regression dropping it entirely stays green.
	var roles int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM member_roles WHERE server_id = $1 AND user_id = $2`,
		serverID, joiner.ID).Scan(&roles))
	require.Positive(t, roles, "the default roles must be assigned in the same transaction")
}

// A second add of the same member is a no-op, not a 500 and not a duplicate
// row. The in-transaction sentinel must reach the client as 409.
func TestAddMemberIsIdempotentOnRepeat(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dupowner")
	joiner := ts.CreateTestUser(t, "dupjoiner")
	serverID := ts.CreateTestServer(t, owner.ID, "DupServer")

	first := ts.DoRequest("POST", membersPath(serverID),
		map[string]string{"user_id": joiner.ID},
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, first.Code)

	second := ts.DoRequest("POST", membersPath(serverID),
		map[string]string{"user_id": joiner.ID},
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusConflict, second.Code,
		"a repeat add is a conflict, never a 5xx")

	var rows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_members WHERE server_id = $1 AND user_id = $2`,
		serverID, joiner.ID).Scan(&rows))
	require.Equal(t, 1, rows)
}
