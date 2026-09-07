//go:build integration

package channels_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const channelExpirationPath = "/api/v1/channels/"

func TestChannelExpirationREST_AuthorityAndValidation(t *testing.T) {
	ts, owner, serverID, channelID := setupWithChannel(t)
	member := ts.CreateTestUser(t, "expirationmember")
	noManage := ts.CreateTestUser(t, "expirationnomanage")
	outsider := ts.CreateTestUser(t, "expirationoutsider")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	ts.AddMemberToServer(t, serverID, noManage.ID, roleMember)
	managerRole := ts.CreateTestRole(t, serverID, "Expiration manager", 5, int64(rbac.PermManageChannels))
	ts.AssignRoleToUser(t, serverID, member.ID, managerRole)

	window := 3600
	w := ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", map[string]interface{}{
		"mode": "set", "window_seconds": window, "retroactive": "new_only",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var policy map[string]interface{}
	testhelpers.ParseJSON(t, w, &policy)
	assert.Equal(t, float64(window), testhelpers.JSONField[float64](t, policy, "window_seconds"))
	assert.Equal(t, float64(1), testhelpers.JSONField[float64](t, policy, "revision"))

	// The policy is exposed consistently through channel GET and server list.
	got := ts.DoRequest(http.MethodGet, channelExpirationPath+channelID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	var channelEnvelope map[string]interface{}
	testhelpers.ParseJSON(t, got, &channelEnvelope)
	channel := testhelpers.JSONField[map[string]interface{}](t, channelEnvelope, "channel")
	assert.Equal(t, float64(window), testhelpers.JSONField[float64](t, channel, "expiration_window_seconds"))
	assert.Equal(t, float64(1), testhelpers.JSONField[float64](t, channel, "expiration_revision"))
	got = ts.DoRequest(http.MethodGet, "/api/v1/servers/"+serverID+"/channels", nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	var listing map[string]interface{}
	testhelpers.ParseJSON(t, got, &listing)
	channels := testhelpers.JSONField[[]interface{}](t, listing, "channels")
	require.NotEmpty(t, channels)

	for _, tc := range []struct {
		name string
		user string
		code int
	}{
		{"member without manage permission is forbidden", noManage.AccessToken, http.StatusForbidden},
		{"nonmember is hidden", outsider.AccessToken, http.StatusNotFound},
		{"missing token is unauthorized", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := expirationRevision(t, ts.DB, channelID)
			var headers = testhelpers.AuthHeaders(tc.user)
			if tc.user == "" {
				headers = nil
			}
			got := ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", map[string]interface{}{
				"mode": "set", "window_seconds": 86400, "retroactive": "new_only",
			}, headers)
			assert.Equal(t, tc.code, got.Code, got.Body.String())
			if tc.name == "nonmember is hidden" {
				assert.JSONEq(t, `{"error":"Channel not found"}`, got.Body.String())
			}
			assert.Equal(t, before, expirationRevision(t, ts.DB, channelID), "denied request mutated policy")
		})
	}

	// A member granted ManageChannels succeeds before a channel-level deny.
	got = ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", map[string]interface{}{
		"mode": "clear", "retroactive": "clear_pending",
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, got.Code, got.Body.String())
	beforeDeny := expirationRevision(t, ts.DB, channelID)
	ts.CreateChannelOverride(t, channelID, "user", member.ID, 0, int64(rbac.PermManageChannels))
	got = ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", map[string]interface{}{
		"mode": "set", "window_seconds": 86400, "retroactive": "new_only",
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, got.Code, got.Body.String())
	assert.Equal(t, beforeDeny, expirationRevision(t, ts.DB, channelID))

	for _, tc := range []struct {
		name, id string
		body     interface{}
	}{
		{"invalid uuid", "not-a-uuid", map[string]interface{}{"mode": "set", "window_seconds": 1}},
		{"invalid mode", channelID, map[string]interface{}{"mode": "bogus", "window_seconds": 1}},
		{"invalid request", channelID, map[string]interface{}{"mode": "set", "window_seconds": 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ts.DoRequest(http.MethodPatch, channelExpirationPath+tc.id+"/expiration", tc.body, testhelpers.AuthHeaders(owner.AccessToken))
			assert.Equal(t, http.StatusBadRequest, got.Code, got.Body.String())
		})
	}

	voiceID := ts.CreateVoiceChannel(t, serverID, "voice")
	got = ts.DoRequest(http.MethodPatch, channelExpirationPath+voiceID+"/expiration", map[string]interface{}{
		"mode": "set", "window_seconds": 3600, "retroactive": "new_only",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, got.Code, got.Body.String())
	got = ts.DoRequest(http.MethodPatch, channelExpirationPath+voiceID+"/expiration", map[string]interface{}{
		"mode": "clear", "retroactive": "clear_pending",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, got.Code, got.Body.String())
	_, err := ts.DB.Exec(`UPDATE channels SET expiration_window_seconds = 3600, expiration_backfill_mode = 'apply', expiration_backfill_cutoff = clock_timestamp(), expiration_revision = 2 WHERE id = $1`, voiceID)
	require.NoError(t, err)
	got = ts.DoRequest(http.MethodPatch, channelExpirationPath+voiceID+"/expiration", map[string]interface{}{
		"mode": "resume", "revision": int64(2),
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, got.Code, got.Body.String())
	var pending bool
	require.NoError(t, ts.DB.QueryRow(`SELECT expiration_backfill_mode IS NOT NULL FROM channels WHERE id = $1`, voiceID).Scan(&pending))
	assert.False(t, pending, "resume must clear the completed marker")
	got = ts.DoRequest(http.MethodPatch, channelExpirationPath+voiceID+"/expiration", map[string]interface{}{
		"mode": "clear", "retroactive": "clear_pending",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, got.Code, got.Body.String())
	got = ts.DoRequest(http.MethodPatch, channelExpirationPath+voiceID+"/expiration", map[string]interface{}{
		"mode": "resume", "revision": int64(2),
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, got.Code, got.Body.String())
}

func TestChannelExpirationREST_StaleCredentialRejected(t *testing.T) {
	ts, owner, serverID, channelID := setupWithChannel(t)
	_ = serverID
	stale := ts.SimulateStaleEpochWindow(t, owner.ID)
	got := ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", map[string]interface{}{
		"mode": "set", "window_seconds": 3600, "retroactive": "new_only",
	}, testhelpers.AuthHeaders(stale))
	assert.Equal(t, http.StatusUnauthorized, got.Code, got.Body.String())
	assert.Equal(t, int64(0), expirationRevision(t, ts.DB, channelID))
}

func TestChannelExpirationREST_UnknownChannelReturnsNotFoundWithoutMutation(t *testing.T) {
	ts, owner, _, existingChannelID := setupWithChannel(t)
	before := expirationRevision(t, ts.DB, existingChannelID)
	missingID := uuid.New().String()
	w := ts.DoRequest(http.MethodPatch, channelExpirationPath+missingID+"/expiration", map[string]interface{}{
		"mode": "set", "window_seconds": 3600, "retroactive": "new_only",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Equal(t, before, expirationRevision(t, ts.DB, existingChannelID))
}

func TestChannelExpirationREST_RetroactiveChoicesAndResumeConflicts(t *testing.T) {
	ts, owner, serverID, channelID := setupWithChannel(t)
	_ = serverID
	auth := testhelpers.AuthHeaders(owner.AccessToken)
	requests := []map[string]interface{}{
		{"mode": "set", "window_seconds": 3600, "retroactive": "apply"},
		{"mode": "clear", "retroactive": "leave_pending"},
		{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"},
		{"mode": "clear", "retroactive": "clear_pending"},
	}
	for i, request := range requests {
		w := ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", request, auth)
		assert.Equal(t, http.StatusOK, w.Code, "choice %d: %s", i, w.Body.String())
	}
	w := ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", map[string]interface{}{
		"mode": "resume", "revision": int64(1),
	}, auth)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())

	// A stale resume revision is rejected while a current marker exists.
	_, err := ts.DB.Exec(`UPDATE channels SET expiration_backfill_mode = 'apply', expiration_backfill_cutoff = clock_timestamp(), expiration_revision = 50 WHERE id = $1`, channelID)
	require.NoError(t, err)
	w = ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", map[string]interface{}{
		"mode": "resume", "revision": int64(49),
	}, auth)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
}

func TestChannelExpirationREST_WaitsForVisibilityAdvisoryAndRechecksAuthority(t *testing.T) {
	ts, _, serverID, channelID := setupWithChannel(t)
	actor := ts.CreateTestUser(t, "advisorymanager")
	ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
	managerRole := ts.CreateTestRole(t, serverID, "Advisory manager", 5, int64(rbac.PermManageChannels))
	ts.AssignRoleToUser(t, serverID, actor.ID, managerRole)
	probe, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	probe.SetMaxOpenConns(4)
	require.NoError(t, probe.Ping())
	t.Cleanup(func() { require.NoError(t, probe.Close()) })
	key, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)
	barrier, err := probe.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := barrier.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("barrier rollback: %v", rollbackErr)
		}
	})
	require.NoError(t, rbac.LockServerVisibilityCapture(context.Background(), barrier, serverID))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- ts.DoRequest(http.MethodPatch, channelExpirationPath+channelID+"/expiration", map[string]interface{}{
			"mode": "set", "window_seconds": 3600, "retroactive": "new_only",
		}, testhelpers.AuthHeaders(actor.AccessToken))
	}()
	dbtest.WaitForAdvisoryLockWaiter(t, probe, key)
	_, err = barrier.Exec(`UPDATE roles SET permissions = 0 WHERE id = $1`, managerRole)
	require.NoError(t, err)
	require.NoError(t, barrier.Commit())
	response := <-done
	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	assert.Equal(t, int64(0), expirationRevision(t, ts.DB, channelID))
}

func TestChannelMessageWriterWaitsForChannelPolicyCommit(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "expirationwriterwait")
	serverID := ts.CreateTestServer(t, owner.ID, "Expiration Writer Wait")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	probe, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	probe.SetMaxOpenConns(4)
	require.NoError(t, probe.Ping())
	t.Cleanup(func() { require.NoError(t, probe.Close()) })
	barrier, err := probe.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := barrier.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("barrier rollback: %v", rollbackErr)
		}
	})
	var txID int64
	require.NoError(t, barrier.QueryRow(`SELECT txid_current()`).Scan(&txID))
	var locked string
	require.NoError(t, barrier.QueryRow(`SELECT id FROM channels WHERE id = $1 FOR NO KEY UPDATE`, channelID).Scan(&locked))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- ts.DoRequest(http.MethodPost, "/api/v1/messages", map[string]interface{}{
			"channel_id": channelID, "content": testhelpers.ValidCiphertext(), "key_version": 1,
		}, testhelpers.AuthHeaders(owner.AccessToken))
	}()
	dbtest.WaitForRowLockWaiter(t, probe, txID)
	_, err = barrier.Exec(`UPDATE channels SET expiration_window_seconds = 3600, expiration_revision = 1 WHERE id = $1`, channelID)
	require.NoError(t, err)
	require.NoError(t, barrier.Commit())
	response := <-done
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	var message map[string]interface{}
	var envelope map[string]interface{}
	testhelpers.ParseJSON(t, response, &envelope)
	message = testhelpers.JSONField[map[string]interface{}](t, envelope, "message")
	id := testhelpers.JSONField[string](t, message, "id")
	var created, expires time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT created_at, expires_at FROM messages WHERE id = $1`, id).Scan(&created, &expires))
	assert.Equal(t, created.Add(time.Hour), expires)
}

func expirationRevision(t *testing.T, db *sql.DB, channelID string) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, db.QueryRow(`SELECT expiration_revision FROM channels WHERE id = $1`, channelID).Scan(&revision))
	return revision
}
