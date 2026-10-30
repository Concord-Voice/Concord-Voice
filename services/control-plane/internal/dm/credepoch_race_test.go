package dm_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
)

// #2201 AC-2: a DM wrapped-key distribution admitted before a destructive key
// reset must be stopped by its locked-user epoch comparison — no dm_channel_keys
// rows may be recreated under the superseded epoch.
func TestCredEpochRace_DMKeyDistributionRejected(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	alice := ts.CreateTestUser(t, "racedmalice")
	bob := ts.CreateTestUser(t, "racedmbob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)

	stale := alice.AccessToken
	require.NoError(t, boundary.Arm(stale))
	boundary.Hook.ArmArg(dmDistributionEpochGuardSQL, alice.ID, nil, nil)

	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+convID, map[string]interface{}{
		"wrapped_keys": map[string]string{
			alice.ID: "d3JhcHBlZC1rZXktYWxpY2U=",
			bob.ID:   "d3JhcHBlZC1rZXktYm9i",
		},
	}, testhelpers.AuthHeaders(stale))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "the distribution epoch comparison must reject the admitted stale distribution")
	requireDMDistributionStaleBoundary(t, boundary)

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_channel_keys WHERE conversation_id = $1`, convID).Scan(&count))
	assert.Zero(t, count, "no wrapped-key rows may be recreated under the old epoch")
}

// #2201: reset commits after real HTTP family admission; the creation
// transaction must reject it before it can make a new 1:1 conversation durable.
func TestCredEpochRace_OpenConversationRejected(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	caller := ts.CreateTestUser(t, "raceopencreator")
	target := ts.CreateTestUser(t, "raceopentarget")
	ts.CreateFriendship(t, caller.ID, target.ID, "accepted")

	stale := caller.AccessToken
	require.NoError(t, boundary.Arm(stale))
	boundary.Hook.ArmArg([]string{dmEpochGuardSQL}, caller.ID, nil, nil)
	w := ts.DoRequest(http.MethodPost, pathDMConversations, map[string]string{
		"user_id": target.ID,
	}, testhelpers.AuthHeaders(stale))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	requireDMStaleBoundary(t, boundary)

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_conversations WHERE created_by = $1`, caller.ID,
	).Scan(&count))
	assert.Zero(t, count, "a stale credential must not create a 1:1 DM")
}

// #2201: group creation uses the same complete participant fence as a 1:1
// conversation. A stale creator must not be able to make any group durable.
func TestCredEpochRace_CreateGroupRejected(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	creator := ts.CreateTestUser(t, "racegroupcreator")
	member := ts.CreateTestUser(t, "racegroupmember")
	ts.CreateFriendship(t, creator.ID, member.ID, "accepted")

	stale := creator.AccessToken
	require.NoError(t, boundary.Arm(stale))
	boundary.Hook.ArmArg([]string{dmEpochGuardSQL}, creator.ID, nil, nil)
	w := ts.DoRequest(http.MethodPost, pathDMConversations+pathGroup, map[string]any{
		"user_ids": []string{member.ID},
	}, testhelpers.AuthHeaders(stale))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	requireDMStaleBoundary(t, boundary)

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_conversations WHERE created_by = $1 AND is_group`, creator.ID,
	).Scan(&count))
	assert.Zero(t, count, "a stale credential must not create a group DM")
}

// #2201: a bearer admitted before reset must not read a wrapped key after
// the DB epoch has rotated.
func TestCredEpochRace_GetKeysRejected(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	caller := ts.CreateTestUser(t, "racegetkeyscaller")
	target := ts.CreateTestUser(t, "racegetkeystarget")
	convID := ts.CreateDMConversation(t, caller.ID, target.ID)
	ts.SeedDMKey(t, convID, caller.ID, 1)

	stale := caller.AccessToken
	require.NoError(t, boundary.Arm(stale))
	boundary.Hook.ArmArg([]string{dmEpochGuardSQL}, caller.ID, nil, nil)
	w := ts.DoRequest(http.MethodGet, pathDMConversationsPrefix+convID+"/keys", nil, testhelpers.AuthHeaders(stale))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	requireDMStaleBoundary(t, boundary)

	var body map[string]any
	testhelpers.ParseJSON(t, w, &body)
	assert.NotContains(t, body, "key", "a stale credential must not receive a wrapped key")
}

func TestCredEpochRace_DMManualRotationRejected(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	alice := ts.CreateTestUser(t, "racedmrotatealice")
	bob := ts.CreateTestUser(t, "racedmrotatebob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	ts.SeedDMKey(t, convID, alice.ID, 1)
	ts.SeedDMKey(t, convID, bob.ID, 1)

	stale := alice.AccessToken
	require.NoError(t, boundary.Arm(stale))
	boundary.Hook.ArmArg(dmDistributionEpochGuardSQL, alice.ID, nil, nil)
	w := ts.DoRequest("POST", "/api/v1/dm/conversations/"+convID+"/rotate-key", map[string]interface{}{
		"wrapped_keys": map[string]string{
			alice.ID: "wrapped-racedmrotatealice-v2",
			bob.ID:   "wrapped-racedmrotatebob-v2",
		},
		"key_version": 2,
	}, testhelpers.AuthHeaders(stale))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "the distribution epoch comparison must reject a stale manual rotation")
	requireDMDistributionStaleBoundary(t, boundary)

	var revocations int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_key_revocations WHERE conversation_id = $1`, convID,
	).Scan(&revocations))
	assert.Zero(t, revocations, "a stale credential must not record a DM key revocation")
}

func TestCredEpochRace_DMMessageDeleteRejected(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	alice := ts.CreateTestUser(t, "racedmdeletealice")
	bob := ts.CreateTestUser(t, "racedmdeletebob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	var messageID string
	require.NoError(t, ts.DB.QueryRow(`
		INSERT INTO dm_messages (conversation_id, user_id, content, type, key_version)
		VALUES ($1, $2, 'must-remain', 'text', 1) RETURNING id`, convID, alice.ID).Scan(&messageID))

	stale := alice.AccessToken
	require.NoError(t, boundary.Arm(stale))
	boundary.Hook.ArmArg([]string{dmEpochGuardSQL}, alice.ID, nil, nil)
	w := ts.DoRequest(http.MethodDelete,
		"/api/v1/dm/conversations/"+convID+"/messages/"+messageID,
		nil, testhelpers.AuthHeaders(stale))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	requireDMStaleBoundary(t, boundary)

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_messages WHERE id = $1`, messageID).Scan(&count))
	assert.Equal(t, 1, count, "a stale credential must not delete the DM message")
}

func TestDMMessageDeleteSerializesCurrentMembershipRemoval(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	creator := ts.CreateTestUser(t, "racedmremovalcreator")
	target := ts.CreateTestUser(t, "racedmremovaltarget")
	convID := ts.CreateGroupDMConversation(t, creator.ID, target.ID)
	_, err := ts.DB.Exec(`UPDATE dm_participants SET role = 'admin' WHERE conversation_id = $1 AND user_id = $2`, convID, creator.ID)
	require.NoError(t, err)
	var messageID string
	require.NoError(t, ts.DB.QueryRow(`
		INSERT INTO dm_messages (conversation_id, user_id, content, type, key_version)
		VALUES ($1, $2, 'membership-race-must-remain', 'text', 1) RETURNING id`, convID, target.ID).Scan(&messageID))

	h := crossStoreVoiceHandler(ts)
	fence, err := dm.BeginDMTopologyEffectForTest(context.Background(), h, convID, []uuid.UUID{uuid.MustParse(target.ID)})
	require.NoError(t, err)
	defer func() {
		if rollbackErr := fence.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback topology fence: %v", rollbackErr)
		}
	}()
	var fenceXID int64
	require.NoError(t, fence.QueryRow(`SELECT txid_current()`).Scan(&fenceXID))

	removalDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		removalDone <- ts.DoRequest(http.MethodDelete,
			"/api/v1/dm/conversations/"+convID+"/members/"+target.ID,
			nil, testhelpers.AuthHeaders(creator.AccessToken))
	}()
	dbtest.WaitForRowLockWaiter(t, ts.DB, fenceXID)

	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		deleteDone <- ts.DoRequest(http.MethodDelete,
			"/api/v1/dm/conversations/"+convID+"/messages/"+messageID,
			nil, testhelpers.AuthHeaders(target.AccessToken))
	}()
	require.NoError(t, fence.Commit())

	removal := <-removalDone
	assert.Equal(t, http.StatusOK, removal.Code, removal.Body.String())
	deletion := <-deleteDone
	assert.Equal(t, http.StatusForbidden, deletion.Code, deletion.Body.String())

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_messages WHERE id = $1`, messageID).Scan(&count))
	assert.Equal(t, 1, count, "a membership removal that wins the fence must retain the message")
}

// A distributor epoch read fault after its users lock and valid family auth
// is a store failure. Inject at that exact SQL, keeping auth's users read intact.
func TestDMKeyDistribution_GuardReadErrorFailsClosed(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	alice := ts.CreateTestUser(t, "dmguarderralice")
	bob := ts.CreateTestUser(t, "dmguarderrbob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	boundary.Hook.ArmArg(dmDistributionEpochGuardSQL, alice.ID, nil, errors.New("injected handler guard read failure"))

	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+convID, map[string]interface{}{
		"wrapped_keys": map[string]string{bob.ID: "d3JhcHBlZC1rZXktYm9i"},
		"key_version":  1,
	}, testhelpers.AuthHeaders(alice.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	seen, hookErr := boundary.Hook.Report()
	require.NoError(t, hookErr)
	require.Equal(t, len(dmDistributionEpochGuardSQL), seen, "valid family auth must reach the handler's guard read")
	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_channel_keys WHERE conversation_id = $1`, convID).Scan(&count))
	assert.Zero(t, count, "guard read failure must not distribute keys")
}

// #2201 error-branch coverage: a DB failure while resolving the fallback key
// version maps to the generic distribution 500 (fail closed, no partial write).
func TestDMKeyDistribution_KeyVersionResolveErrorFailsClosed(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "dmkverralice")
	bob := ts.CreateTestUser(t, "dmkverrbob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)

	// Break only the key table; family auth still uses the intact users table.
	_, err := ts.DB.Exec("ALTER TABLE dm_channel_keys RENAME TO dm_channel_keys_dberrtest")
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := ts.DB.Exec("ALTER TABLE dm_channel_keys_dberrtest RENAME TO dm_channel_keys"); err != nil {
			t.Errorf("cleanup: failed to rename dm_channel_keys back: %v", err)
		}
	})

	// No explicit key_version → the handler must read MAX(...) from the
	// renamed table and fail closed with the generic 500.
	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+convID, map[string]interface{}{
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
	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+convID, map[string]interface{}{
		"wrapped_keys": map[string]string{bob.ID: "d3JhcHBlZC1rZXktYm9i"},
		"key_version":  2,
	}, testhelpers.AuthHeaders(alice.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

const dmEpochGuardSQL = `SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`

func requireDMStaleBoundary(t *testing.T, boundary *testhelpers.StaleBoundary) {
	t.Helper()
	boundary.RequireRotated(t)
	seen, hookErr := boundary.Hook.Report()
	require.NoError(t, hookErr)
	require.Equal(t, 1, seen, "the named handler epoch guard must be reached after successful auth commit and rotation")
}

// Distribution and manual rotation share a complete-users lock followed by a
// separate MatchEpoch read. Observe both in order: this is not GuardTx's SQL.
var dmDistributionEpochGuardSQL = []string{
	`SELECT id FROM users WHERE id = ANY($1::uuid[]) ORDER BY id FOR SHARE`,
	`SELECT credential_epoch FROM users WHERE id = $1`,
}

func requireDMDistributionStaleBoundary(t *testing.T, boundary *testhelpers.StaleBoundary) {
	t.Helper()
	boundary.RequireRotated(t)
	seen, hookErr := boundary.Hook.Report()
	require.NoError(t, hookErr)
	require.Equal(t, len(dmDistributionEpochGuardSQL), seen, "the distributor must lock users then reach its actor epoch comparison after auth and rotation")
}
