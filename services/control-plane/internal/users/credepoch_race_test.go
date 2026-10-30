package users_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// #2201 AC-2: a request ADMITTED by the middleware before a destructive reset
// is rotated after successful family admission and must be stopped by the
// handler's locked epoch comparison before recreating anything.

func TestCredEpochRace_EncryptedBlobUpsertRejected(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	user := ts.CreateTestUser(t, "raceblob")
	require.NoError(t, boundary.Arm(user.AccessToken))
	boundary.Hook.ArmArg([]string{`SELECT credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`}, user.ID, nil, nil)

	w := ts.DoRequest("PUT", "/api/v1/users/me/preferences", map[string]interface{}{
		"encrypted_data": "c3RhbGUtY2lwaGVydGV4dA==",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "locked epoch comparison must reject the admitted stale request")
	boundary.RequireRotated(t)
	requireUsersEpochGuard(t, boundary, 1)

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM user_preferences WHERE user_id = $1`, user.ID).Scan(&count))
	assert.Zero(t, count, "no encrypted blob row may be recreated under the old epoch")
}

// A fault at the handler lock must follow valid HTTP family admission and map
// to 500 rather than an epoch-mismatch 401.
func TestCredEpochRace_BlobUpsertGuardReadErrorFailsClosed(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	user := ts.CreateTestUser(t, "raceblobguarderr")

	require.NoError(t, boundary.ArmControl(user.AccessToken))
	boundary.Hook.ArmArg([]string{`SELECT credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`}, user.ID, nil, stmthook.ErrInjected)

	w := ts.DoRequest("PUT", "/api/v1/users/me/preferences", map[string]interface{}{
		"encrypted_data": "c3RhbGUtY2lwaGVydGV4dA==",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	require.NoError(t, boundary.Check())
	requireUsersEpochGuard(t, boundary, 1)
	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM user_preferences WHERE user_id=$1`, user.ID).Scan(&count))
	assert.Zero(t, count, "failed guard read must not write ciphertext")
}

// #2201: a store failure on the blob UPSERT itself (after the users-row lock +
// epoch read succeed) also maps to 500. Inject at the domain
// statement after successful admission and the users lock/epoch read.
func TestCredEpochRace_BlobUpsertWriteErrorFailsClosed(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	user := ts.CreateTestUser(t, "raceblobwriteerr")

	require.NoError(t, boundary.ArmControl(user.AccessToken))
	boundary.Hook.ArmArg([]string{
		`SELECT credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`,
		`INSERT INTO user_preferences`,
	}, user.ID, nil, stmthook.ErrInjected)

	w := ts.DoRequest("PUT", "/api/v1/users/me/preferences", map[string]interface{}{
		"encrypted_data": "c3RhbGUtY2lwaGVydGV4dA==",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	require.NoError(t, boundary.Check())
	requireUsersEpochGuard(t, boundary, 2)
	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM user_preferences WHERE user_id=$1`, user.ID).Scan(&count))
	assert.Zero(t, count, "failed upsert must not write ciphertext")
}

func TestCredEpochRace_PresenceOverrideRejected(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	user := ts.CreateTestUser(t, "raceoverride")
	require.NoError(t, boundary.Arm(user.AccessToken))
	boundary.Hook.ArmArg([]string{
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`,
		`SELECT credential_epoch FROM users WHERE id = $1`,
	}, user.ID, nil, nil)

	w := ts.DoRequest("PUT", "/api/v1/users/me/presence-overrides/custom_text", map[string]interface{}{
		"encrypted_data":    "c3RhbGUtY2lwaGVydGV4dA==",
		"expected_version":  0,
		"excluded_user_ids": []string{},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "MatchEpoch must reject the admitted stale request")
	boundary.RequireRotated(t)
	requireUsersEpochGuard(t, boundary, 2)

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM presence_override_preferences WHERE user_id = $1`, user.ID).Scan(&count))
	assert.Zero(t, count, "no override ciphertext may be recreated under the old epoch")
}

func requireUsersEpochGuard(t *testing.T, boundary *testhelpers.StaleBoundary, want int) {
	t.Helper()
	seen, err := boundary.Hook.Report()
	require.NoError(t, err)
	require.Equal(t, want, seen, "the handler's named epoch guard must be reached after admission")
}
