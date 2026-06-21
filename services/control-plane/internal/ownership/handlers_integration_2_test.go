package ownership_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const notAUUID = "not-a-uuid"

// =============================================================================
// InitiateTransfer additional edge cases
// =============================================================================

func TestInitiateTransferInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferinvsrv")

	w := ts.DoRequest("POST", pathServersPrefix+notAUUID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: uuid.New().String(),
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestInitiateTransferServerNotFound(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xfernotfound")
	target := ts.CreateTestUser(t, "xfertargnf")
	fakeID := uuid.New().String()

	w := ts.DoRequest("POST", pathServersPrefix+fakeID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: target.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestInitiateTransferMissingPassword(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xfernopw")
	member := ts.CreateTestUser(t, "xfernopwtgt")
	serverID := ts.CreateTestServer(t, owner.ID, "NoPW Transfer")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestInitiateTransferMissingTargetUserID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xfernotarg")
	serverID := ts.CreateTestServer(t, owner.ID, "NoTarget Transfer")

	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestInitiateTransferInvalidTargetUserIDFormat(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferbadtarg")
	serverID := ts.CreateTestServer(t, owner.ID, "BadTarget Transfer")

	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: notAUUID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestInitiateTransferEmptyBody(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferempty")
	serverID := ts.CreateTestServer(t, owner.ID, "Empty Body Transfer")

	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =============================================================================
// GetTransferStatus additional edge cases
// =============================================================================

func TestGetTransferStatusInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "gstinvsrv")

	w := ts.DoRequest("GET", pathServersPrefix+notAUUID+pathTransferOwnership, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetTransferStatusTargetSeesDetails(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "gsttowner")
	target := ts.CreateTestUser(t, "gstttarget")
	serverID := ts.CreateTestServer(t, owner.ID, "Target Sees Details")
	ts.AddMemberToServer(t, serverID, target.ID, keyMember)

	// Initiate transfer
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: target.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Target should see from_user_id and to_user_id
	w = ts.DoRequest("GET", pathServersPrefix+serverID+pathTransferOwnership, nil, testhelpers.AuthHeaders(target.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	transfer := body[keyTransfer].(map[string]interface{})
	assert.Equal(t, keyPending, transfer[keyStatus])
	assert.Equal(t, owner.ID, transfer["from_user_id"])
	assert.Equal(t, target.ID, transfer[keyToUserID])
}

func TestGetTransferStatusUnauthenticated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "gstunauthown")
	serverID := ts.CreateTestServer(t, owner.ID, "Unauth Status")

	w := ts.DoRequest("GET", pathServersPrefix+serverID+pathTransferOwnership, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// =============================================================================
// CancelTransfer additional edge cases
// =============================================================================

func TestCancelTransferInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "caninvsrv")

	w := ts.DoRequest(keyDelete, pathServersPrefix+notAUUID+pathTransferOwnership, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCancelTransferServerNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "cannfserv")
	fakeID := uuid.New().String()

	w := ts.DoRequest(keyDelete, pathServersPrefix+fakeID+pathTransferOwnership, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestCancelTransferUnauthenticated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "canunauthown")
	serverID := ts.CreateTestServer(t, owner.ID, "Unauth Cancel")

	w := ts.DoRequest(keyDelete, pathServersPrefix+serverID+pathTransferOwnership, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestCancelTransferThenStatusShowsNoPending(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "canstatusown")
	member := ts.CreateTestUser(t, "canstatusmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Cancel Status Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Cancel
	w = ts.DoRequest(keyDelete, pathServersPrefix+serverID+pathTransferOwnership, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify DB status
	var status string
	err := ts.DB.QueryRow(
		`SELECT status FROM ownership_transfers WHERE server_id = $1 ORDER BY requested_at DESC LIMIT 1`,
		serverID,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", status)
}

// =============================================================================
// ConfirmTransfer additional edge cases
// =============================================================================

func TestConfirmTransferInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "cnfinvsrv")

	w := ts.DoRequest("POST", pathServersPrefix+notAUUID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestConfirmTransferServerNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "cnfnfserv")
	fakeID := uuid.New().String()

	w := ts.DoRequest("POST", pathServersPrefix+fakeID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestConfirmTransferUnauthenticated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cnfunauthown")
	member := ts.CreateTestUser(t, "cnfunauthmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Unauth Confirm")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Unauthenticated confirm
	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestConfirmTransferVerifiesRoleSwap(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cnfroleown")
	member := ts.CreateTestUser(t, "cnfrolemem")
	serverID := ts.CreateTestServer(t, owner.ID, "Role Swap Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate + confirm
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify DB state
	var ownerRole, memberRole string
	err := ts.DB.QueryRow(`SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, owner.ID).Scan(&ownerRole)
	require.NoError(t, err)
	assert.Equal(t, keyMember, ownerRole)

	err = ts.DB.QueryRow(`SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, member.ID).Scan(&memberRole)
	require.NoError(t, err)
	assert.Equal(t, keyOwner, memberRole)
}

func TestConfirmTransferInvalidatesPermissionCache(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cnfcacheown")
	member := ts.CreateTestUser(t, "cnfcachemem")
	serverID := ts.CreateTestServer(t, owner.ID, "Cache Confirm Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	ctx := context.Background()

	// Seed permission cache
	ownerCacheKey := "perm:" + serverID + ":" + owner.ID
	memberCacheKey := "perm:" + serverID + ":" + member.ID
	require.NoError(t, ts.Redis.Set(ctx, ownerCacheKey, "99999", 5*time.Minute).Err())
	require.NoError(t, ts.Redis.Set(ctx, memberCacheKey, "99999", 5*time.Minute).Err())

	// Initiate + confirm
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify cache invalidated
	_, err := ts.Redis.Get(ctx, ownerCacheKey).Result()
	assert.Error(t, err, "owner cache should be invalidated after confirm")
	_, err = ts.Redis.Get(ctx, memberCacheKey).Result()
	assert.Error(t, err, "member cache should be invalidated after confirm")
}

func TestConfirmTransferAuditLogWritten(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cnfauditown")
	member := ts.CreateTestUser(t, "cnfauditmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Audit Confirm Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate + confirm
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Check audit_log for ownership_transferred
	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE server_id = $1 AND action = 'ownership_transferred'`,
		serverID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// =============================================================================
// ReverseTransfer additional edge cases
// =============================================================================

func TestReverseTransferShortToken(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revshorttkn")

	w := ts.DoRequest("POST", pathOwnershipReverse+"tooshort", map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReverseTransferMissingPassword(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revnopwown")
	member := ts.CreateTestUser(t, "revnopwmem")
	serverID := ts.CreateTestServer(t, owner.ID, "NoPW Reverse")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate + confirm
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var reversalToken string
	err := ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	// Missing password
	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReverseTransferWrongPassword(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revbadpwown")
	member := ts.CreateTestUser(t, "revbadpwmem")
	serverID := ts.CreateTestServer(t, owner.ID, "BadPW Reverse")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate + confirm
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var reversalToken string
	err := ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	// Wrong password
	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: "WrongPassword999!",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestReverseTransferUnauthenticated(t *testing.T) {
	ts := setupTS(t)

	fakeToken := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	w := ts.DoRequest("POST", pathOwnershipReverse+fakeToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestReverseTransferVerifiesRoleRevert(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revroleown")
	member := ts.CreateTestUser(t, "revrolemem")
	serverID := ts.CreateTestServer(t, owner.ID, "Role Revert Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate + confirm
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify ownership is with member now
	var ownerIDAfterTransfer string
	err := ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerIDAfterTransfer)
	require.NoError(t, err)
	assert.Equal(t, member.ID, ownerIDAfterTransfer)

	// Reverse
	var reversalToken string
	err = ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify roles reverted
	var ownerRole, memberRole string
	err = ts.DB.QueryRow(`SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, owner.ID).Scan(&ownerRole)
	require.NoError(t, err)
	assert.Equal(t, keyOwner, ownerRole)

	err = ts.DB.QueryRow(`SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, member.ID).Scan(&memberRole)
	require.NoError(t, err)
	assert.Equal(t, keyMember, memberRole)

	// Verify transfer status is 'reversed'
	var transferStatus string
	err = ts.DB.QueryRow(`SELECT status FROM ownership_transfers WHERE server_id = $1 AND reversal_token = $2`, serverID, reversalToken).Scan(&transferStatus)
	require.NoError(t, err)
	assert.Equal(t, "reversed", transferStatus)
}

func TestReverseTransferOwnershipChangedSinceTransfer(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revchangeown")
	member1 := ts.CreateTestUser(t, "revchangemem1")
	member2 := ts.CreateTestUser(t, "revchangemem2")
	serverID := ts.CreateTestServer(t, owner.ID, "Changed Owner Server")
	ts.AddMemberToServer(t, serverID, member1.ID, keyMember)
	ts.AddMemberToServer(t, serverID, member2.ID, keyMember)

	// Transfer to member1
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member1.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var reversalToken string
	err := ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	// Manually change ownership to member2 (simulating another transfer)
	_, err = ts.DB.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, member2.ID, serverID)
	require.NoError(t, err)

	// Reversal should fail because current owner is no longer the transfer recipient
	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestReverseTransferAuditLogWritten(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revauditown")
	member := ts.CreateTestUser(t, "revauditmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Audit Reverse Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate + confirm + reverse
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var reversalToken string
	err := ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Check audit_log for ownership_transfer_reversed
	var count int
	err = ts.DB.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE server_id = $1 AND action = 'ownership_transfer_reversed'`,
		serverID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestReverseTransferCacheInvalidated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revcacheown")
	member := ts.CreateTestUser(t, "revcachemem")
	serverID := ts.CreateTestServer(t, owner.ID, "Cache Reverse Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	ctx := context.Background()

	// Initiate + confirm
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Seed cache again (after confirm invalidated it)
	ownerCacheKey := "perm:" + serverID + ":" + owner.ID
	memberCacheKey := "perm:" + serverID + ":" + member.ID
	require.NoError(t, ts.Redis.Set(ctx, ownerCacheKey, "88888", 5*time.Minute).Err())
	require.NoError(t, ts.Redis.Set(ctx, memberCacheKey, "88888", 5*time.Minute).Err())

	// Reverse
	var reversalToken string
	err := ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify cache invalidated
	_, err = ts.Redis.Get(ctx, ownerCacheKey).Result()
	assert.Error(t, err, "owner cache should be invalidated after reversal")
	_, err = ts.Redis.Get(ctx, memberCacheKey).Result()
	assert.Error(t, err, "member cache should be invalidated after reversal")
}

// =============================================================================
// CancelTransfer audit log
// =============================================================================

func TestCancelTransferAuditLogWritten(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "canauditown")
	member := ts.CreateTestUser(t, "canauditmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Audit Cancel Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Cancel
	w = ts.DoRequest(keyDelete, pathServersPrefix+serverID+pathTransferOwnership, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Check audit_log for ownership_transfer_cancelled
	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE server_id = $1 AND action = 'ownership_transfer_cancelled'`,
		serverID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// =============================================================================
// Full lifecycle: initiate -> confirm -> reverse -> re-initiate
// =============================================================================

func TestTransferLifecycleConfirmReverseReinitiate(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "lcfullown")
	member := ts.CreateTestUser(t, "lcfullmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Full Lifecycle Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Confirm
	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Reverse
	var reversalToken string
	err := ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Brief pause for Redis cache invalidation to propagate (prevents CI flakiness)
	time.Sleep(100 * time.Millisecond)

	// Re-initiate (should work — original owner is back)
	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)
}

// =============================================================================
// Double confirm should fail
// =============================================================================

func TestConfirmTransferAlreadyCompleted(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dbconfown")
	member := ts.CreateTestUser(t, "dbconfmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Double Confirm Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// First confirm
	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Second confirm — the original owner is no longer owner (member is), so this fails
	// The new owner (member) tries to confirm but there's no pending transfer
	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}
