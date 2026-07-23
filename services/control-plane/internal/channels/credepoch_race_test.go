package channels_test

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

// #2201 AC-2: a channel creation (which stores wrapped keys) admitted before a
// destructive key reset (stale-cache window) must be stopped by GuardTx — no
// channel row and no wrapped-key rows may land under the superseded epoch.
func TestCredEpochRace_CreateChannelRejected(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "racechanowner")
	serverID := ts.CreateTestServer(t, owner.ID, "Race Channel Server")

	stale := ts.SimulateStaleEpochWindow(t, owner.ID)

	w := ts.DoRequest("POST", "/api/v1/channels", map[string]interface{}{
		"server_id": serverID,
		"name":      "fenced-channel",
		"type":      "text",
		"wrapped_keys": map[string]string{
			owner.ID: "d3JhcHBlZC1rZXktb3duZXI=",
		},
	}, testhelpers.AuthHeaders(stale))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "GuardTx must reject the admitted stale channel creation")

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM channels WHERE server_id = $1 AND name = 'fenced-channel'`, serverID).Scan(&count))
	assert.Zero(t, count, "no channel may be created under the old epoch")
}

// #2201: a GuardTx read failure inside the channel-create transaction (fence
// cache seeded, users renamed) maps to the generic 500, not the epoch 401
// (Codex #2397 review).
func TestCredEpochRace_CreateChannelGuardReadErrorFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "createguarderr")
	serverID := ts.CreateTestServer(t, owner.ID, "Create Guard Error Server")

	require.NoError(t, ts.Redis.Set(context.Background(), credepoch.Key(owner.ID), "none", 5*time.Minute).Err())
	_, err := ts.DB.Exec("ALTER TABLE users RENAME TO users_createguarderr")
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec("ALTER TABLE users_createguarderr RENAME TO users"); err != nil {
			t.Errorf("cleanup: failed to rename users back: %v", err)
		}
	})

	w := ts.DoRequest("POST", "/api/v1/channels", map[string]interface{}{
		"server_id": serverID,
		"name":      "guard-err-channel",
		"type":      "text",
		"wrapped_keys": map[string]string{
			owner.ID: "d3JhcHBlZC1rZXktb3duZXI=",
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// #2201: a GuardTx read failure inside the distribution transaction (fault-
// injected by renaming users while the fence answers from cache) maps to the
// generic 500 — a store failure is never an epoch-mismatch 401.
func TestCredEpochRace_DistributionGuardReadErrorFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "raceguarderr")
	serverID := ts.CreateTestServer(t, owner.ID, "Guard Error Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// The caller must already hold a channel key to distribute.
	_, err := ts.DB.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, 'a2V5', 1)`,
		channelID, owner.ID)
	require.NoError(t, err)

	// Seed the fence cache so middleware passes without the users table, then
	// take the table away so GuardTx's FOR SHARE read errors inside the tx.
	require.NoError(t, ts.Redis.Set(context.Background(), credepoch.Key(owner.ID), "none", 5*time.Minute).Err())
	_, err = ts.DB.Exec("ALTER TABLE users RENAME TO users_guarderrtest")
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec("ALTER TABLE users_guarderrtest RENAME TO users"); err != nil {
			t.Errorf("cleanup: failed to rename users back: %v", err)
		}
	})

	w := ts.DoRequest("POST", "/api/v1/channels/"+channelID+"/keys", map[string]interface{}{
		"wrapped_keys": map[string]string{owner.ID: "d3JhcHBlZC1rZXk="},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
