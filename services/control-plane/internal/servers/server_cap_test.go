package servers_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

// Server creation is uncapped for every account (founder un-gating 2026-10-15,
// reversing the #1555 free cap of 5). A free user's 6th create — the one the
// old cap rejected with server_cap_reached — must succeed.
func TestCreateServerFreeUserUncapped(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "servercapfree")

	for i := 1; i <= 6; i++ {
		w := ts.DoRequest("POST", "/api/v1/servers", map[string]interface{}{
			"name": fmt.Sprintf("Uncapped Server %d", i),
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equalf(t, http.StatusCreated, w.Code, "free create %d must succeed (no cap)", i)
	}
}

// Creation is uncapped in total, but bounded to 10 per account per rolling 24h
// (Codex review of #3861: each create can store two inline images). The count
// is of servers the account CREATED, read from the server_created audit row,
// so an ownership transfer neither frees the creator's slot nor charges the
// recipient, deleting a server frees its slot, and a create older than 24h
// drops out. The first 10 are seeded in SQL: 11 HTTP creates would trip the
// route's 10-per-minute limiter, whose 429 the quota's 429 cannot be told apart
// from by status.
func TestCreateServerDailyQuota(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "servercapquota")
	recipient := ts.CreateTestUser(t, "servercaprecipient")
	create := func(u testhelpers.TestUser, name string) *httptest.ResponseRecorder {
		return ts.DoRequest("POST", "/api/v1/servers", map[string]interface{}{"name": name},
			testhelpers.AuthHeaders(u.AccessToken))
	}
	exec := func(query string, args ...interface{}) {
		_, err := ts.DB.Exec(query, args...)
		require.NoError(t, err)
	}
	exec(`WITH s AS (
			INSERT INTO servers (id, name, owner_id, created_at, updated_at)
			SELECT gen_random_uuid(), 'Quota Server ' || g, $1, NOW(), NOW() FROM generate_series(1, 10) g
			RETURNING id)
		INSERT INTO audit_log (server_id, actor_id, action, target_type, target_id, metadata)
		SELECT id, $1, 'server_created', 'server', id, '{}'::jsonb FROM s`, creator.ID)

	w := create(creator, "Over Quota")
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	require.Equal(t, "server_create_quota", testhelpers.JSONField[string](t, body, "code"))
	require.Equal(t, float64(10), testhelpers.JSONField[float64](t, body, "limit"))
	retry, err := strconv.Atoi(w.Header().Get("Retry-After"))
	require.NoError(t, err, "a quota refusal carries Retry-After")
	require.True(t, retry > 0 && retry <= 86400, "Retry-After %d must fall inside the 24h window", retry)

	exec(`UPDATE servers SET owner_id = $1 WHERE owner_id = $2`, recipient.ID, creator.ID)
	require.Equal(t, http.StatusTooManyRequests, create(creator, "After Transfer").Code,
		"transferring servers away must not free the creator's slots")
	require.Equal(t, http.StatusCreated, create(recipient, "Recipient Server").Code,
		"receiving servers must not charge the recipient's quota")

	exec(`DELETE FROM servers WHERE name = 'Quota Server 1'`)
	require.Equal(t, http.StatusCreated, create(creator, "After Delete").Code,
		"deleting a server frees its slot")
	require.Equal(t, http.StatusTooManyRequests, create(creator, "Back At Quota").Code,
		"the create that just succeeded is counted")

	exec(`UPDATE audit_log SET created_at = NOW() - INTERVAL '25 hours'
		WHERE actor_id = $1 AND action = 'server_created'
		  AND server_id = (SELECT id FROM servers WHERE name = 'Quota Server 2')`, creator.ID)
	require.Equal(t, http.StatusCreated, create(creator, "After Aging").Code,
		"a create older than 24h drops out of the count")
}

// A signed access token can outlive its account; mandatory admission rejects it
// before server creation can run.
func TestCreateServerDeletedAccountIsUnauthorized(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "servercapgone")
	_, err := ts.DB.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	require.NoError(t, err)

	w := ts.DoRequest("POST", "/api/v1/servers", map[string]interface{}{"name": "Orphan Server"},
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	require.Equal(t, "Authentication required", testhelpers.JSONField[string](t, body, "error"),
		"a user missing before admission must fail at AuthRequired")

	var n int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM servers WHERE name = 'Orphan Server'`).Scan(&n))
	require.Zero(t, n)
}

// Deletion after real authentication still reaches the quota lock's missing-user
// branch. The SQL spy pauses before that exact lock; it does not supply identity
// or skip any middleware.
func TestCreateServerAccountDisappearsAfterAdmissionIsUnauthorized(t *testing.T) {
	spy, routerDB := openSQLStateSpyDB(t)
	env := setupMFAEnforcementEnvWithDB(t, routerDB)
	ts := env.ts
	user := ts.CreateTestUser(t, "servercapracegone")
	pause := spy.armPause(t, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/servers",
		strings.NewReader(`{"name":"After Admission Orphan"}`)).WithContext(ctx)
	req.Header = testhelpers.AuthHeaders(user.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	request := startRequest(func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		ts.Router.ServeHTTP(w, req)
		return w
	})
	t.Cleanup(func() {
		defer cancel()
		pause.Release()
		select {
		case <-request.done:
			return
		case <-time.After(lockProofBound):
			cancel()
		}
		select {
		case <-request.done:
		case <-time.After(lockProofBound):
			t.Errorf("server creation did not finish after cleanup cancellation within %s", lockProofBound)
		}
	})
	waitForSignal(t, pause.reached, request, "server creation to reach the quota lock after admission")
	_, err := ts.DB.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	require.NoError(t, err)
	pause.Release()
	w := request.await(t, "server creation after account deletion")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	require.Equal(t, "Session no longer valid", testhelpers.JSONField[string](t, body, "error"),
		"this 401 must come from the quota lock after genuine admission")
	var n int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM servers WHERE name = 'After Admission Orphan'`).Scan(&n))
	require.Zero(t, n)
}

// The server_created audit row is what the quota counts, so a create whose
// audit row cannot be written must not commit: an uncounted server would let
// the account go past the quota.
func TestCreateServerAuditFailureRollsBack(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "servercapaudit")
	exec := func(query string) {
		_, err := ts.DB.Exec(query)
		require.NoError(t, err)
	}
	exec(`CREATE OR REPLACE FUNCTION test_refuse_server_created() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused by test'; END $$`)
	exec(`DROP TRIGGER IF EXISTS test_refuse_server_created ON audit_log`)
	exec(`CREATE TRIGGER test_refuse_server_created BEFORE INSERT ON audit_log
		FOR EACH ROW WHEN (NEW.action = 'server_created') EXECUTE FUNCTION test_refuse_server_created()`)
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(`DROP TRIGGER IF EXISTS test_refuse_server_created ON audit_log`)
		_, _ = ts.DB.Exec(`DROP FUNCTION IF EXISTS test_refuse_server_created()`)
	})

	w := ts.DoRequest("POST", "/api/v1/servers", map[string]interface{}{"name": "Unaudited Server"},
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusInternalServerError, w.Code)

	var n int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM servers WHERE name = 'Unaudited Server'`).Scan(&n))
	require.Zero(t, n, "the server row must roll back with its audit row")
}
