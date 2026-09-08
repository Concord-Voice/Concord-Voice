package members_test

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/members"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type securityEventRecorder struct{ events []securityevent.Event }

func (r *securityEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.events = append(r.events, event)
}

type memberAuditOperation struct {
	name          string
	method        string
	action        string
	path          func(string, string) string
	body          []byte
	seed          func(*testing.T, *testhelpers.TestServer, string, string)
	mutationTable string
	mutationEvent string
	selfRemoval   bool
}

func TestMemberAuditMirrorsFollowAuthoritativeMutation(t *testing.T) {
	operations := []memberAuditOperation{
		{name: "update", method: http.MethodPatch, action: "member_updated", path: memberPath, body: []byte(`{"role":"admin"}`), seed: func(t *testing.T, ts *testhelpers.TestServer, serverID, targetID string) {
			ts.AddMemberToServer(t, serverID, targetID, "member")
		}, mutationTable: "server_members", mutationEvent: "UPDATE"},
		{name: "remove", method: http.MethodDelete, action: "member_removed", path: memberPath, seed: func(t *testing.T, ts *testhelpers.TestServer, serverID, targetID string) {
			ts.AddMemberToServer(t, serverID, targetID, "member")
		}, mutationTable: "server_members", mutationEvent: "DELETE"},
		{name: "leave", method: http.MethodDelete, action: "member_left", path: memberPath, seed: func(t *testing.T, ts *testhelpers.TestServer, serverID, targetID string) {
			ts.AddMemberToServer(t, serverID, targetID, "member")
		}, mutationTable: "server_members", mutationEvent: "DELETE", selfRemoval: true},
		{name: "unban", method: http.MethodDelete, action: "member_unbanned", path: banPath, seed: func(t *testing.T, ts *testhelpers.TestServer, serverID, targetID string) {
			_, err := ts.DB.Exec(`INSERT INTO server_bans (server_id, user_id, banned_by) VALUES ($1, $2, $3)`, serverID, targetID, serverOwner(t, ts, serverID))
			require.NoError(t, err)
		}, mutationTable: "server_bans", mutationEvent: "DELETE"},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			ts := setupTS(t)
			owner := ts.CreateTestUser(t, "nightwatch-"+operation.name+"-owner")
			target := ts.CreateTestUser(t, "nightwatch-"+operation.name+"-target")
			serverID := ts.CreateTestServer(t, owner.ID, "Nightwatch member audit")
			operation.seed(t, ts, serverID, target.ID)

			successRecorder := &securityEventRecorder{}
			beforeRows := memberAuditRows(t, ts.DB, serverID, operation.action)
			actorID := owner.ID
			if operation.selfRemoval {
				actorID = target.ID
			}
			response := runMemberAuditOperation(t, ts, actorID, operation, serverID, target.ID, successRecorder, ts.DB)
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, beforeRows+1, memberAuditRows(t, ts.DB, serverID, operation.action))
			assertMemberMutationApplied(t, ts.DB, operation, serverID, target.ID)
			require.Equal(t, []securityevent.Event{{EventType: securityevent.EventAudit, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuditCommitted}}, withoutEvidenceRefs(successRecorder.events))

			mutationRecorder := &securityEventRecorder{}
			mutationTarget := ts.CreateTestUser(t, "nightwatch-"+operation.name+"-mutation-fail")
			operation.seed(t, ts, serverID, mutationTarget.ID)
			beforeRows = memberAuditRows(t, ts.DB, serverID, operation.action)
			removeFailureTrigger := forceMemberMutationFailure(t, ts.DB, operation)
			actorID = owner.ID
			if operation.selfRemoval {
				actorID = mutationTarget.ID
			}
			response = runMemberAuditOperation(t, ts, actorID, operation, serverID, mutationTarget.ID, mutationRecorder, ts.DB)
			require.GreaterOrEqual(t, response.Code, http.StatusInternalServerError)
			require.Equal(t, beforeRows, memberAuditRows(t, ts.DB, serverID, operation.action))
			require.Empty(t, mutationRecorder.events, "a failed authoritative mutation must emit neither success nor audit-write-failure")
			removeFailureTrigger()

			target = ts.CreateTestUser(t, "nightwatch-"+operation.name+"-audit-fail")
			operation.seed(t, ts, serverID, target.ID)
			auditFailureRecorder := &securityEventRecorder{}
			beforeRows = memberAuditRows(t, ts.DB, serverID, operation.action)
			auditFailureDB, err := sql.Open("postgres", "")
			require.NoError(t, err)
			require.NoError(t, auditFailureDB.Close())
			actorID = owner.ID
			if operation.selfRemoval {
				actorID = target.ID
			}
			response = runMemberAuditOperation(t, ts, actorID, operation, serverID, target.ID, auditFailureRecorder, auditFailureDB)
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, beforeRows, memberAuditRows(t, ts.DB, serverID, operation.action))
			require.Equal(t, []securityevent.Event{{EventType: securityevent.EventAudit, Outcome: securityevent.OutcomeFailure, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuditWriteFailed}}, auditFailureRecorder.events)
		})
	}
}

func serverOwner(t *testing.T, ts *testhelpers.TestServer, serverID string) string {
	t.Helper()
	var owner string
	require.NoError(t, ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&owner))
	return owner
}

func runMemberAuditOperation(t *testing.T, ts *testhelpers.TestServer, actorID string, operation memberAuditOperation, serverID, targetID string, recorder *securityEventRecorder, auditDB *sql.DB) *httptest.ResponseRecorder {
	t.Helper()
	audit := rbac.NewAuditWriter(auditDB, logger.NewWithWriter(&bytes.Buffer{}))
	audit.SetSecurityEvents(recorder)
	handler := members.NewHandler(ts.DB, logger.NewWithWriter(&bytes.Buffer{}), ts.Redis, ts.Hub, rbac.NewResolver(ts.DB, rbac.NewPermissionCache(ts.Redis), logger.New("test")), audit)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("user_id", actorID); c.Next() })
	router.PATCH("/api/v1/servers/:id/members/:user_id", handler.UpdateMember)
	router.DELETE("/api/v1/servers/:id/members/:user_id", handler.RemoveMember)
	router.DELETE("/api/v1/servers/:id/bans/:user_id", handler.UnbanMember)
	request := httptest.NewRequest(operation.method, operation.path(serverID, targetID), bytes.NewReader(operation.body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func memberAuditRows(t *testing.T, db *sql.DB, serverID, action string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE server_id = $1 AND action = $2`, serverID, action).Scan(&count))
	return count
}

func assertMemberMutationApplied(t *testing.T, db *sql.DB, operation memberAuditOperation, serverID, targetID string) {
	t.Helper()
	var count int
	switch operation.name {
	case "update":
		var role string
		require.NoError(t, db.QueryRow(`SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, targetID).Scan(&role))
		require.Equal(t, "admin", role)
	case "remove", "leave":
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, targetID).Scan(&count))
		require.Zero(t, count)
	case "unban":
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM server_bans WHERE server_id = $1 AND user_id = $2`, serverID, targetID).Scan(&count))
		require.Zero(t, count)
	default:
		require.Failf(t, "unexpected member audit operation", "%s", operation.name)
	}
}

func forceMemberMutationFailure(t *testing.T, db *sql.DB, operation memberAuditOperation) func() {
	t.Helper()
	const functionName = "nightwatch_member_mutation_fail"
	const triggerName = "nightwatch_member_mutation_fail_trigger"
	require.NoError(t, db.Ping())
	_, err := db.Exec(`CREATE OR REPLACE FUNCTION nightwatch_member_mutation_fail() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'nightwatch forced member mutation failure';
END;
$$`)
	require.NoError(t, err)
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- identifiers are closed-set test literals; no external data reaches SQL
	_, err = db.Exec(`CREATE TRIGGER nightwatch_member_mutation_fail_trigger BEFORE ` + operation.mutationEvent + ` ON ` + operation.mutationTable + ` FOR EACH ROW EXECUTE FUNCTION nightwatch_member_mutation_fail()`)
	require.NoError(t, err)
	remove := func() {
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- identifiers are closed-set test literals; no external data reaches SQL
		_, err := db.Exec(`DROP TRIGGER IF EXISTS ` + triggerName + ` ON ` + operation.mutationTable)
		require.NoError(t, err)
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- identifiers are closed-set test literals; no external data reaches SQL
		_, err = db.Exec(`DROP FUNCTION IF EXISTS ` + functionName + `()`)
		require.NoError(t, err)
	}
	t.Cleanup(remove)
	return remove
}

func withoutEvidenceRefs(events []securityevent.Event) []securityevent.Event {
	clean := append([]securityevent.Event(nil), events...)
	for index := range clean {
		clean[index].EvidenceRef = ""
	}
	return clean
}
