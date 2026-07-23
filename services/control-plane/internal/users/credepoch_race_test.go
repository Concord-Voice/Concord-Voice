package users_test

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

// #2201 AC-2: a request ADMITTED by the middleware before a destructive reset
// (materialized via SimulateStaleEpochWindow's stale-cache window) must be
// stopped by the in-transaction GuardTx and recreate nothing.

func TestCredEpochRace_EncryptedBlobUpsertRejected(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "raceblob")
	stale := ts.SimulateStaleEpochWindow(t, user.ID)

	w := ts.DoRequest("PUT", "/api/v1/users/me/preferences", map[string]interface{}{
		"encrypted_data": "c3RhbGUtY2lwaGVydGV4dA==",
	}, testhelpers.AuthHeaders(stale))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "GuardTx must reject the admitted stale request")

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM user_preferences WHERE user_id = $1`, user.ID).Scan(&count))
	assert.Zero(t, count, "no encrypted blob row may be recreated under the old epoch")
}

// #2201: a store failure on the blob write's users-row lock/epoch read maps to
// the generic 500 — a store error is never an epoch-mismatch 401. Fault-injected
// by seeding the fence cache (so middleware passes without the users table) then
// renaming users so the in-tx FOR NO KEY UPDATE read errors.
func TestCredEpochRace_BlobUpsertGuardReadErrorFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "raceblobguarderr")

	require.NoError(t, ts.Redis.Set(context.Background(), credepoch.Key(user.ID), "none", 5*time.Minute).Err())
	_, err := ts.DB.Exec("ALTER TABLE users RENAME TO users_blobguarderr")
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec("ALTER TABLE users_blobguarderr RENAME TO users"); err != nil {
			t.Errorf("cleanup: failed to rename users back: %v", err)
		}
	})

	w := ts.DoRequest("PUT", "/api/v1/users/me/preferences", map[string]interface{}{
		"encrypted_data": "c3RhbGUtY2lwaGVydGV4dA==",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// #2201: a store failure on the blob UPSERT itself (after the users-row lock +
// epoch read succeed) also maps to 500. Fault-injected by renaming the domain
// table so the upsert errors while the users lock is intact.
func TestCredEpochRace_BlobUpsertWriteErrorFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "raceblobwriteerr")

	_, err := ts.DB.Exec("ALTER TABLE user_preferences RENAME TO user_preferences_writeerr")
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec("ALTER TABLE user_preferences_writeerr RENAME TO user_preferences"); err != nil {
			t.Errorf("cleanup: failed to rename user_preferences back: %v", err)
		}
	})

	w := ts.DoRequest("PUT", "/api/v1/users/me/preferences", map[string]interface{}{
		"encrypted_data": "c3RhbGUtY2lwaGVydGV4dA==",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestCredEpochRace_PresenceOverrideRejected(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "raceoverride")
	stale := ts.SimulateStaleEpochWindow(t, user.ID)

	w := ts.DoRequest("PUT", "/api/v1/users/me/presence-overrides/custom_text", map[string]interface{}{
		"encrypted_data":    "c3RhbGUtY2lwaGVydGV4dA==",
		"expected_version":  0,
		"excluded_user_ids": []string{},
	}, testhelpers.AuthHeaders(stale))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "GuardTx must reject the admitted stale request")

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM presence_override_preferences WHERE user_id = $1`, user.ID).Scan(&count))
	assert.Zero(t, count, "no override ciphertext may be recreated under the old epoch")
}
