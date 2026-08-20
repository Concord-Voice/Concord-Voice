package servers_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

// Owner-authorization outcomes for DeleteServer: the owner deletes, a non-owner
// is refused and nothing is deleted.
//
// This is NOT the `FOR UPDATE` regression guard, and it never was. An earlier
// version of this test took a competing row lock and released it BEFORE issuing
// the request, so no lock was held while the handler ran and the assertion
// passed identically whether ownership was read inside a transaction or on an
// autocommit statement — it claimed a property it could not observe (CodeRabbit,
// PR #2840).
//
// The genuine witness lives in internal/members/lock_order_integration_test.go
// (TestDeleteServerBlocksOnTheHeldServerRow): it holds the servers row ACROSS
// the request and asserts a pg_stat_activity lock wait. Do not delete that one
// believing this covers it.
func TestDeleteServerOwnerAuthorizationOutcomes(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "delrowowner")
	other := ts.CreateTestUser(t, "delrowother")
	serverID := ts.CreateTestServer(t, owner.ID, "DelRowServer")

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID, nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var rows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM servers WHERE id = $1`, serverID).Scan(&rows))
	require.Zero(t, rows)

	// A non-owner must still be refused, and the refusal must not delete.
	other2 := ts.CreateTestServer(t, owner.ID, "DelRowServer2")
	forbidden := ts.DoRequest("DELETE", "/api/v1/servers/"+other2, nil,
		testhelpers.AuthHeaders(other.AccessToken))
	require.Equal(t, http.StatusForbidden, forbidden.Code)

	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM servers WHERE id = $1`, other2).Scan(&rows))
	require.Equal(t, 1, rows, "a forbidden delete must leave the server intact")
}

// AC-6, server-delete arm. The audience capture must run while server_members
// still exists. Moving it after the DELETE would silently yield an EMPTY set —
// the cascade has already emptied the table — so the disconnect would clear
// nobody while every assertion about the server being gone still passed.
//
// The captured count is observable only through the handler's own log line, so
// that is what this asserts. Falsified by moving the capture below the delete:
// the count drops to 0 and this fails.
func TestDeleteServerCapturesMembersBeforeTheCascade(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "delcapowner")
	member := ts.CreateTestUser(t, "delcapmember")
	serverID := ts.CreateTestServer(t, owner.ID, "DelCapServer")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	var before int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_members WHERE server_id = $1`, serverID).Scan(&before))
	require.GreaterOrEqual(t, before, 2, "owner plus the added member")

	logs := ts.CaptureLogs(t)

	w := ts.DoRequest("DELETE", "/api/v1/servers/"+serverID, nil,
		testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	require.Contains(t, logs.String(), "Server delete presence reconciled",
		"the presence reconcile must run on a successful delete")
	require.Contains(t, logs.String(), fmt.Sprintf("member_count=%d", before),
		"the captured audience must be the PRE-cascade member set; an empty or "+
			"short count means the capture ran after DELETE FROM servers")

	var after int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM server_members WHERE server_id = $1`, serverID).Scan(&after))
	require.Zero(t, after, "membership cascades with the server")
}
