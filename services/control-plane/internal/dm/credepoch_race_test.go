package dm_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// #2201 AC-2: a DM wrapped-key distribution admitted before a destructive key
// reset (stale-cache window) must be stopped by GuardTx — no dm_channel_keys
// rows may be recreated under the superseded epoch.
func TestCredEpochRace_DMKeyDistributionRejected(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "racedmalice")
	bob := ts.CreateTestUser(t, "racedmbob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)

	stale := ts.SimulateStaleEpochWindow(t, alice.ID)

	w := ts.DoRequest("POST", "/api/v1/dm/conversations/"+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{
			alice.ID: "d3JhcHBlZC1rZXktYWxpY2U=",
			bob.ID:   "d3JhcHBlZC1rZXktYm9i",
		},
	}, testhelpers.AuthHeaders(stale))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "GuardTx must reject the admitted stale distribution")

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_channel_keys WHERE conversation_id = $1`, convID).Scan(&count))
	assert.Zero(t, count, "no wrapped-key rows may be recreated under the old epoch")
}

// #2201: a GuardTx read failure inside the distribution transaction (fence
// cache seeded so middleware passes, then users renamed so the FOR SHARE read
// errors) maps to the generic 500 — a store failure is never an epoch 401.
func TestDMKeyDistribution_GuardReadErrorFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "dmguarderralice")
	bob := ts.CreateTestUser(t, "dmguarderrbob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)

	require.NoError(t, ts.Redis.Set(context.Background(), credepoch.Key(alice.ID), "none", 5*time.Minute).Err())
	_, err := ts.DB.Exec("ALTER TABLE users RENAME TO users_dmguarderr")
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec("ALTER TABLE users_dmguarderr RENAME TO users"); err != nil {
			t.Errorf("cleanup: failed to rename users back: %v", err)
		}
	})

	w := ts.DoRequest("POST", "/api/v1/dm/conversations/"+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{bob.ID: "d3JhcHBlZC1rZXktYm9i"},
		"key_version":  1,
	}, testhelpers.AuthHeaders(alice.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// #2201 error-branch coverage: a DB failure while resolving the fallback key
// version maps to the generic distribution 500 (fail closed, no partial write).
func TestDMKeyDistribution_KeyVersionResolveErrorFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "dmkverralice")
	bob := ts.CreateTestUser(t, "dmkverrbob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)

	// Seed the fence cache so AuthRequired + participant checks pass without
	// touching the renamed table below.
	_, err := ts.DB.Exec("ALTER TABLE dm_channel_keys RENAME TO dm_channel_keys_dberrtest")
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec("ALTER TABLE dm_channel_keys_dberrtest RENAME TO dm_channel_keys"); err != nil {
			t.Errorf("cleanup: failed to rename dm_channel_keys back: %v", err)
		}
	})

	// No explicit key_version → the handler must read MAX(...) from the
	// renamed table and fail closed with the generic 500.
	w := ts.DoRequest("POST", "/api/v1/dm/conversations/"+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{bob.ID: "d3JhcHBlZC1rZXktYm9i"},
	}, testhelpers.AuthHeaders(alice.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// #2201 error-branch coverage: a statement failure inside the guarded
// distribution transaction fails the batch with the generic 500.
func TestDMKeyDistribution_InsertErrorFailsBatch(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "dminserralice")
	bob := ts.CreateTestUser(t, "dminserrbob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)

	_, err := ts.DB.Exec("ALTER TABLE dm_channel_keys RENAME TO dm_channel_keys_inserrtest")
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec("ALTER TABLE dm_channel_keys_inserrtest RENAME TO dm_channel_keys"); err != nil {
			t.Errorf("cleanup: failed to rename dm_channel_keys back: %v", err)
		}
	})

	// Explicit key_version skips the resolve read; the guarded INSERT then
	// hits the renamed table and the whole batch fails closed.
	w := ts.DoRequest("POST", "/api/v1/dm/conversations/"+convID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{bob.ID: "d3JhcHBlZC1rZXktYm9i"},
		"key_version":  2,
	}, testhelpers.AuthHeaders(alice.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
