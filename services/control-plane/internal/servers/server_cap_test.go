package servers_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// grantPremium inserts an active premium subscription and clears the cached tier
// (login during CreateTestUser may have cached the then-free tier). Server
// enforcement reads the subscriptions table via the entitlements cache (#1298).
func grantPremium(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO subscriptions (user_id, tier, status, source) VALUES ($1, 'premium', 'active', 'code')`,
		userID,
	)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.Del(context.Background(), "ent:"+userID).Err())
}

func createServerViaAPI(ts *testhelpers.TestServer, accessToken, name string) *httptest.ResponseRecorder {
	return ts.DoRequest("POST", "/api/v1/servers", map[string]interface{}{
		"name": name,
	}, testhelpers.AuthHeaders(accessToken))
}

// --- Server-creation cap (#1555 Gate 1) ---

// A free ("Sonic") user may own at most 5 servers; the 6th create is rejected
// with a typed, machine-readable 403.
func TestCreateServerFreeUserCappedAtFive(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "servercapfree")

	for i := 1; i <= 5; i++ {
		w := createServerViaAPI(ts, user.AccessToken, fmt.Sprintf("Cap Server %d", i))
		require.Equalf(t, http.StatusCreated, w.Code, "create %d of 5 must succeed under the cap", i)
	}

	w := createServerViaAPI(ts, user.AccessToken, "Cap Server 6")
	require.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "server_cap_reached", body["code"])
	assert.Equal(t, float64(5), body["limit"])
	assert.NotEmpty(t, body["error"])
}

// A premium ("Supersonic") user has no cap on server creation — creating more
// than the free limit succeeds.
func TestCreateServerPremiumUserUnlimited(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "servercapprem")
	grantPremium(t, ts, user.ID)

	for i := 1; i <= 6; i++ {
		w := createServerViaAPI(ts, user.AccessToken, fmt.Sprintf("Premium Server %d", i))
		require.Equalf(t, http.StatusCreated, w.Code, "premium create %d must succeed (no cap)", i)
	}
}

// The cap counts servers currently OWNED, not lifetime creations — deleting a
// server frees a slot for a new create.
func TestCreateServerDeleteFreesSlot(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "servercapslot")

	serverIDs := make([]string, 0, 5)
	for i := 1; i <= 5; i++ {
		serverIDs = append(serverIDs, ts.CreateTestServer(t, user.ID, fmt.Sprintf("Owned Server %d", i)))
	}

	w := createServerViaAPI(ts, user.AccessToken, "Over The Cap")
	require.Equal(t, http.StatusForbidden, w.Code, "at cap, create must be rejected")

	w = ts.DoRequest("DELETE", "/api/v1/servers/"+serverIDs[0], nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	w = createServerViaAPI(ts, user.AccessToken, "Fits After Delete")
	assert.Equal(t, http.StatusCreated, w.Code, "deleting a server must free a cap slot")
}
