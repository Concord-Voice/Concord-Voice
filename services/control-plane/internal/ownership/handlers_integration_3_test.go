package ownership_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// MFA coverage — verifyMFA paths (InitiateTransfer + ReverseTransfer)
// =============================================================================

// enableMFAForUser sets the user's mfa_enabled flag so IsEnabled returns true.
// Does NOT insert a TOTP secret row — VerifyCode will gracefully return false
// for invalid codes without hitting decryption errors.
func enableMFAForUser(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(`UPDATE users SET mfa_enabled = true, mfa_methods = '{totp}' WHERE id = $1`, userID)
	require.NoError(t, err)
}

// seedMFAInlineToken stores a WebAuthn inline verification token in Redis so
// VerifyCode accepts it as a valid MFA code.
func seedMFAInlineToken(t *testing.T, ts *testhelpers.TestServer, userID, token string) {
	t.Helper()
	key := fmt.Sprintf("mfa_inline_token:%s:%s", userID, token)
	require.NoError(t, ts.Redis.Set(context.Background(), key, "1", 5*time.Minute).Err())
}

func TestInitiateTransferMFARequired(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mfareqown")
	member := ts.CreateTestUser(t, "mfareqmem")
	serverID := ts.CreateTestServer(t, owner.ID, "MFA Required Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	enableMFAForUser(t, ts, owner.ID)

	// No mfa_code provided — should get 403 with mfa_required
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["mfa_required"])
	assert.NotNil(t, body["methods"])
}

func TestInitiateTransferMFAInvalidCode(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mfabadcown")
	member := ts.CreateTestUser(t, "mfabadcmem")
	serverID := ts.CreateTestServer(t, owner.ID, "MFA Bad Code Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	enableMFAForUser(t, ts, owner.ID)

	// Invalid mfa_code — should get 403
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
		"mfa_code":      "000000",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Invalid MFA code")
}

func TestInitiateTransferMFAValidCode(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mfagoodcown")
	member := ts.CreateTestUser(t, "mfagoodcmem")
	serverID := ts.CreateTestServer(t, owner.ID, "MFA Good Code Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	enableMFAForUser(t, ts, owner.ID)

	// Seed a valid inline MFA token
	mfaToken := "valid-inline-mfa-token-for-testing-1234" //nolint:gosec // test token, not real credential
	seedMFAInlineToken(t, ts, owner.ID, mfaToken)

	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
		"mfa_code":      mfaToken,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestReverseTransferMFARequired(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revmfareqown")
	member := ts.CreateTestUser(t, "revmfareqmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Rev MFA Req Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate + confirm (without MFA — enable it after transfer)
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

	// No mfa_code — should get 403
	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["mfa_required"])
}

func TestReverseTransferMFAValidCode(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revmfagown")
	member := ts.CreateTestUser(t, "revmfagmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Rev MFA Good Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate + confirm
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

	// Seed a valid inline MFA token
	mfaToken := "valid-inline-mfa-token-for-reversal-1234" //nolint:gosec // test token, not real credential
	seedMFAInlineToken(t, ts, owner.ID, mfaToken)

	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
		"mfa_code":  mfaToken,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// =============================================================================
// Reversal edge case: original owner left server
// =============================================================================

func TestReverseTransferOriginalOwnerLeftServer(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revleftown")
	member := ts.CreateTestUser(t, "revleftmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Rev Left Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate + confirm
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Remove original owner from server_members (simulating they left)
	_, err := ts.DB.Exec(`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2`, serverID, owner.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, owner.ID)
	require.NoError(t, err)

	var reversalToken string
	err = ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	// Reversal should fail — original owner not a member
	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Original owner is no longer a member")
}

// =============================================================================
// ConfirmTransfer: from_user membership missing (edge case)
// =============================================================================

func TestConfirmTransferFromUserMembershipMissing(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "cnffromown")
	member := ts.CreateTestUser(t, "cnffrommem")
	serverID := ts.CreateTestServer(t, owner.ID, "From Missing Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Remove the owner's membership row (but keep owner_id in servers table)
	_, err := ts.DB.Exec(`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2`, serverID, owner.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, owner.ID)
	require.NoError(t, err)

	// Re-insert the owner row so requireServerOwner passes (they're still in the servers table as owner)
	// Actually, requireServerOwner checks servers.owner_id, not server_members. The membership
	// removal only affects the role swap in executeTransfer.

	// Confirm — should fail because from_user membership is missing
	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "Current owner membership record is missing")
}

// =============================================================================
// Reversal: transfer recipient left server (warning path, not failure)
// =============================================================================

func TestReverseTransferRecipientLeftServer(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revrecipown")
	member := ts.CreateTestUser(t, "revrecipmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Rev Recip Left Server")
	ts.AddMemberToServer(t, serverID, member.ID, keyMember)

	// Initiate + confirm
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: member.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Get reversal token
	var reversalToken string
	err := ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken)
	require.NoError(t, err)

	// Remove the transfer recipient (new owner) from server_members but keep as owner
	// First change the owner_id back to original to simulate a weird state, no.
	// Actually, we need the current owner to be the recipient (member) for the reversal check
	// to pass (currentOwnerID == rec.toUserID). But we want to test the "recipient not a member" warning path.
	// The recipient IS the current owner. We can delete their server_members row.
	_, err = ts.DB.Exec(`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2`, serverID, member.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, member.ID)
	require.NoError(t, err)

	// Reversal should succeed (recipient leaving is a warning, not a failure)
	w = ts.DoRequest("POST", pathOwnershipReverse+reversalToken, map[string]interface{}{
		keyPassword: testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify ownership reverted
	var currentOwner string
	err = ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&currentOwner)
	require.NoError(t, err)
	assert.Equal(t, owner.ID, currentOwner)
}

// =============================================================================
// GetTransferStatus: target user sees details
// =============================================================================

func TestGetTransferStatusOwnerAndTargetSeeFullDetails(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "gstfullown")
	target := ts.CreateTestUser(t, "gstfulltgt")
	serverID := ts.CreateTestServer(t, owner.ID, "Full Details Server")
	ts.AddMemberToServer(t, serverID, target.ID, keyMember)

	// Initiate
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
		keyTargetUserID: target.ID,
		keyPassword:     testhelpers.TestAuthPlaintext,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	// Owner sees from_user_id and to_user_id
	w = ts.DoRequest("GET", pathServersPrefix+serverID+pathTransferOwnership, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var ownerBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &ownerBody)
	transfer := ownerBody[keyTransfer].(map[string]interface{})
	assert.Equal(t, owner.ID, transfer["from_user_id"])
	assert.Equal(t, target.ID, transfer[keyToUserID])

	// Target sees from_user_id and to_user_id
	w = ts.DoRequest("GET", pathServersPrefix+serverID+pathTransferOwnership, nil, testhelpers.AuthHeaders(target.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var targetBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &targetBody)
	transfer = targetBody[keyTransfer].(map[string]interface{})
	assert.Equal(t, owner.ID, transfer["from_user_id"])
	assert.Equal(t, target.ID, transfer[keyToUserID])
}
