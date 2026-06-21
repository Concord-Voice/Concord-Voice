package ownership_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// ConfirmTransfer: cancel-then-confirm guard
// =============================================================================

// TestConfirmTransferCancelledThenConfirm verifies that confirming a cancelled
// transfer returns 404 (no pending transfer found).
func TestConfirmTransferCancelledThenConfirm(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cnfcancelown")
	member := ts.CreateTestUser(t, "cnfcancelmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Cancel Then Confirm Server")
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

	// Confirm should fail — no pending transfer
	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =============================================================================
// ReverseTransfer: double reversal (token already used / status changed)
// =============================================================================

// TestReverseTransferAlreadyReversed verifies that using the same reversal token
// twice fails — after the first reversal the transfer status is 'reversed', not
// 'completed', so lookupCompletedTransfer returns 404.
func TestReverseTransferAlreadyReversed(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revdblown")
	member := ts.CreateTestUser(t, "revdblmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Double Reverse Server")
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

	// First reversal succeeds
	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Second reversal fails — token status is 'reversed', not 'completed'
	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =============================================================================
// ReverseTransfer: token length edge cases
// =============================================================================

// TestReverseTransferLongToken verifies that a token longer than 64 characters
// is rejected with 400.
func TestReverseTransferLongToken(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revlongtkn")

	// 65 hex chars (1 char too long)
	longToken := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	w := ts.DoRequest("POST", pathOwnershipReverse+longToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =============================================================================
// InitiateTransfer: transfer to non-existent user (valid UUID, no DB record)
// =============================================================================

// TestInitiateTransferTargetNonExistent verifies that transferring to a valid
// UUID that doesn't correspond to any user fails — the target won't be a server
// member, so requireMembership returns 400.
func TestInitiateTransferTargetNonExistent(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferghostowner")
	serverID := ts.CreateTestServer(t, owner.ID, "Ghost Target Server")

	fakeUserID := uuid.New().String()
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: fakeUserID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =============================================================================
// GetTransferStatus: after completed transfer (no pending)
// =============================================================================

// TestGetTransferStatusAfterCompletion verifies that GetTransferStatus returns
// null transfer after a transfer has been completed (not pending anymore).
func TestGetTransferStatusAfterCompletion(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "gstcmplown")
	member := ts.CreateTestUser(t, "gstcmplmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Completed Status Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate + confirm
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// New owner checks transfer status — should show no pending
	w = ts.DoRequest("GET", pathServersPrefix+serverID+pathTransferOwnership, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Nil(t, body[keyTransfer])
}

// =============================================================================
// CancelTransfer: double cancel (second cancel finds no pending row)
// =============================================================================

// TestCancelTransferAlreadyCancelled verifies that cancelling a transfer that
// was already cancelled returns 404 (no pending transfer to cancel).
func TestCancelTransferAlreadyCancelled(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "can2xown")
	member := ts.CreateTestUser(t, "can2xmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Double Cancel Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// First cancel
	w = ts.DoRequest(keyDelete, pathServersPrefix+serverID+pathTransferOwnership, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Second cancel — no pending transfer
	w = ts.DoRequest(keyDelete, pathServersPrefix+serverID+pathTransferOwnership, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =============================================================================
// ReverseTransfer: MFA invalid code path for reversal
// =============================================================================

// TestReverseTransferMFAInvalidCode verifies that providing a wrong MFA code
// during reversal returns 403.
func TestReverseTransferMFAInvalidCode(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revmfabadcown")
	member := ts.CreateTestUser(t, "revmfabadcmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Rev MFA Bad Code Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate + confirm (without MFA)
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Enable MFA for original owner
	enableMFAForUser(t, ts, owner.ID)

	var reversalToken string
	err := ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	// Invalid MFA code — should get 403
	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
		"mfa_code":  "000000",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// =============================================================================
// Full lifecycle: new owner initiates another transfer after receiving ownership
// =============================================================================

// TestTransferChain verifies that after ownership is transferred, the new owner
// can initiate a fresh transfer to a third party.
func TestTransferChain(t *testing.T) {
	ts := setupTS(t)
	originalOwner := ts.CreateTestUser(t, "chainorigown")
	secondOwner := ts.CreateTestUser(t, "chainsecown")
	thirdUser := ts.CreateTestUser(t, "chainthird")
	serverID := ts.CreateTestServer(t, originalOwner.ID, "Chain Transfer Server")
	ts.AddMemberToServer(t, serverID, secondOwner.ID, keyMember)
	ts.AddMemberToServer(t, serverID, thirdUser.ID, keyMember)

	// Transfer 1: originalOwner -> secondOwner
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: secondOwner.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(originalOwner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(originalOwner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Transfer 2: secondOwner -> thirdUser
	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: thirdUser.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(secondOwner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(secondOwner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify thirdUser is now the owner
	var ownerID string
	err := ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&ownerID)
	require.NoError(t, err)
	assert.Equal(t, thirdUser.ID, ownerID)
}

// =============================================================================
// ReverseTransfer: nil request body (ShouldBindJSON error path)
// =============================================================================

// TestReverseTransferNilBody verifies that sending no request body to the
// reverse endpoint returns 400 (ShouldBindJSON fails because password is required).
func TestReverseTransferNilBody(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revnilown")
	member := ts.CreateTestUser(t, "revnilmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Rev Nil Body Server")
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

	// Send nil body
	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =============================================================================
// CancelTransfer: verify cancelled_at and response body
// =============================================================================

// TestCancelTransferVerifyAuditAndStatus does a cancel and verifies the
// cancelled_at timestamp is set and the response body has the right message.
func TestCancelTransferVerifyAuditAndStatus(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "canverifyown")
	member := ts.CreateTestUser(t, "canverifymem")
	serverID := ts.CreateTestServer(t, owner.ID, "Cancel Verify Server")
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

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Transfer cancelled", body["message"])

	// Verify cancelled_at is set
	var cancelledAtNotNull bool
	err := ts.DB.QueryRow(
		`SELECT cancelled_at IS NOT NULL FROM ownership_transfers WHERE server_id = $1 ORDER BY requested_at DESC LIMIT 1`,
		serverID,
	).Scan(&cancelledAtNotNull)
	require.NoError(t, err)
	assert.True(t, cancelledAtNotNull, "cancelled_at should be set after cancellation")
}

// =============================================================================
// InitiateTransfer: response body field validation
// =============================================================================

// TestInitiateTransferResponseFields verifies that the response includes all
// expected fields with correct types and values.
func TestInitiateTransferResponseFields(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "xferrespown")
	member := ts.CreateTestUser(t, "xferrespmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Response Fields Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)

	// Verify all fields present
	assert.Equal(t, keyPending, body[keyStatus])
	assert.Equal(t, serverID, body["server_id"])
	assert.Equal(t, owner.ID, body["from_user_id"])
	assert.Equal(t, member.ID, body[keyToUserID])
	assert.NotEmpty(t, body["transfer_id"])
	assert.NotEmpty(t, body["expires_at"])

	// Verify transfer_id is a valid UUID
	_, err := uuid.Parse(body["transfer_id"].(string))
	assert.NoError(t, err, "transfer_id should be a valid UUID")
}
