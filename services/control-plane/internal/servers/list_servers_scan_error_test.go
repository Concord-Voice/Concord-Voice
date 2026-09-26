package servers_test

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/servers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// Regression for a PR #3464 review finding (pre-existing, in a function the PR
// modifies). ListServers logged a row that failed to scan and continued, so the
// response was a 200 that silently omitted that server. The desktop client
// commits the list as a whole-array replace, so purgeMissingServerState then
// tore down the omitted server's channel state. A non-OK response leaves the
// client's last-known list alone, so the handler must refuse the whole list.
func TestListServers_RowScanErrorFailsTheWholeList(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "lsscanowner")
	kept := ts.CreateTestServer(t, owner.ID, "List Scan Kept")
	broken := ts.CreateTestServer(t, owner.ID, "List Scan Broken")

	list := func() *httptest.ResponseRecorder {
		return ts.DoRequest(http.MethodGet, "/api/v1/servers", nil, testhelpers.AuthHeaders(owner.AccessToken))
	}

	// Control: with the real scanner both servers are listed, so the server
	// the fault targets is one the harness can see.
	w := list()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.ElementsMatch(t, []string{kept, broken}, listedServerIDs(t, w), "control: both servers are listed")

	servers.SetListServersRowScannerForTest(t, func(rows *sql.Rows, dest ...any) error {
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		if id, ok := dest[0].(*string); ok && *id == broken {
			return errors.New("injected scan failure")
		}
		return nil
	})

	w = list()
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"a row that fails to scan must fail the list, not vanish from a 200: %s", w.Body.String())
	assert.JSONEq(t, `{"error":"Failed to fetch server"}`, w.Body.String())
}

func listedServerIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	rows := testhelpers.JSONField[[]interface{}](t, body, "servers")
	ids := make([]string, 0, len(rows))
	for _, elem := range rows {
		row, ok := elem.(map[string]interface{})
		require.True(t, ok)
		ids = append(ids, testhelpers.JSONField[string](t, row, "id"))
	}
	return ids
}
