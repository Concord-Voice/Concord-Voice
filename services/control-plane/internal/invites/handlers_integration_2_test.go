package invites_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathInvites     = "/invites"
	pathServers     = "/api/v1/servers/"
	pathInviteSlash = "/invites/"
	pathInvitesJoin = "/api/v1/invites/join"
	pathInvitesAPI  = "/api/v1/invites/"
)

// =====================================================================
// CreateInvite: Edge Cases
// =====================================================================

func TestCreateInviteDefaults(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "invdefaults")
	serverID := ts.CreateTestServer(t, owner.ID, "Defaults Server")

	// Empty body — defaults should apply (max_uses=1, expires_in=86400)
	w := ts.DoRequest("POST", pathServers+serverID+pathInvites, nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	assert.Equal(t, float64(1), invite["max_uses"])
	assert.NotEmpty(t, invite["code"])
}

func TestCreateInviteUnlimitedUses(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "invunlimited")
	serverID := ts.CreateTestServer(t, owner.ID, "Unlimited Server")

	// max_uses=0 means unlimited (NULL in DB)
	w := ts.DoRequest("POST", pathServers+serverID+pathInvites, map[string]interface{}{
		"max_uses": 0,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	assert.Nil(t, invite["max_uses"], "max_uses should be null for unlimited")
}

func TestCreateInviteMaxUsesCapped(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "invcapped")
	serverID := ts.CreateTestServer(t, owner.ID, "Capped Server")

	// max_uses=500 should be capped at 100
	w := ts.DoRequest("POST", pathServers+serverID+pathInvites, map[string]interface{}{
		"max_uses": 500,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	assert.Equal(t, float64(100), invite["max_uses"])
}

func TestCreateInviteExpiresInClamped(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "invclamp")
	serverID := ts.CreateTestServer(t, owner.ID, "Clamp Server")

	// expires_in=10 (below minimum 300) should be clamped to 300
	w := ts.DoRequest("POST", pathServers+serverID+pathInvites, map[string]interface{}{
		"expires_in": 10,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	assert.NotEmpty(t, invite["expires_at"])
}

func TestCreateInviteExpiresInClampedMax(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "invclampmax")
	serverID := ts.CreateTestServer(t, owner.ID, "ClampMax Server")

	// expires_in=999999 (above maximum 604800) should be clamped to 604800
	w := ts.DoRequest("POST", pathServers+serverID+pathInvites, map[string]interface{}{
		"expires_in": 999999,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestCreateInviteInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "invbadserver")

	w := ts.DoRequest("POST", "/api/v1/servers/not-a-uuid/invites", nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =====================================================================
// ListInvites: Edge Cases
// =====================================================================

func TestListInvitesEmpty(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "listemptyown")
	serverID := ts.CreateTestServer(t, owner.ID, "EmptyList Server")

	w := ts.DoRequest("GET", pathServers+serverID+pathInvites, nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invites := body["invites"].([]interface{})
	assert.Empty(t, invites)
}

func TestListInvitesMultiple(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "listmultown")
	serverID := ts.CreateTestServer(t, owner.ID, "MultiList Server")

	// Create 3 invites
	for i := 0; i < 3; i++ {
		createInvite(t, ts, serverID, owner.AccessToken)
	}

	w := ts.DoRequest("GET", pathServers+serverID+pathInvites, nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invites := body["invites"].([]interface{})
	assert.Len(t, invites, 3)
}

func TestListInvitesNotPermitted(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "listpermown")
	member := ts.CreateTestUser(t, "listpermmem")
	serverID := ts.CreateTestServer(t, owner.ID, "ListPerm Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("GET", pathServers+serverID+pathInvites, nil, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestListInvitesInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "listbadid")

	w := ts.DoRequest("GET", "/api/v1/servers/not-a-uuid/invites", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// =====================================================================
// RevokeInvite: Edge Cases
// =====================================================================

func TestRevokeInviteNotFound(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revokenf")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeNF Server")
	fakeInviteID := uuid.New().String()

	w := ts.DoRequest("DELETE", pathServers+serverID+pathInviteSlash+fakeInviteID, nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRevokeInviteAlreadyRevoked(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revokedouble")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeDouble Server")

	// Create and get invite ID
	w := ts.DoRequest("POST", pathServers+serverID+pathInvites, map[string]interface{}{
		"max_uses": 1,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	inviteID := invite["id"].(string)

	// Revoke once — should succeed
	w = ts.DoRequest("DELETE", pathServers+serverID+pathInviteSlash+inviteID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Revoke again — should be not found (already revoked)
	w = ts.DoRequest("DELETE", pathServers+serverID+pathInviteSlash+inviteID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRevokeInviteInvalidServerID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "revokebadsrv")
	fakeInviteID := uuid.New().String()

	w := ts.DoRequest("DELETE", "/api/v1/servers/not-a-uuid/invites/"+fakeInviteID, nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRevokeInviteInvalidInviteID(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revokebadinv")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokeBadInv Server")

	w := ts.DoRequest("DELETE", pathServers+serverID+pathInviteSlash+"not-a-uuid", nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRevokeInviteNotPermitted(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revokepermown")
	member := ts.CreateTestUser(t, "revokepermmem")
	serverID := ts.CreateTestServer(t, owner.ID, "RevokePerm Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Create an invite as owner
	w := ts.DoRequest("POST", pathServers+serverID+pathInvites, map[string]interface{}{
		"max_uses": 5,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	inviteID := invite["id"].(string)

	// Regular member tries to revoke — should fail
	w = ts.DoRequest("DELETE", pathServers+serverID+pathInviteSlash+inviteID, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// =====================================================================
// JoinServer: Edge Cases
// =====================================================================

func TestJoinServerRevokedInvite(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joinrevown")
	joiner := ts.CreateTestUser(t, "joinrevjoin")
	serverID := ts.CreateTestServer(t, owner.ID, "JoinRevoked Server")

	// Create and revoke invite
	w := ts.DoRequest("POST", pathServers+serverID+pathInvites, map[string]interface{}{
		"max_uses": 0, // unlimited uses
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	inviteID := invite["id"].(string)
	code := invite["code"].(string)

	// Revoke it
	w = ts.DoRequest("DELETE", pathServers+serverID+pathInviteSlash+inviteID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Try to join with revoked code
	w = ts.DoRequest("POST", pathInvitesJoin, map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(joiner.AccessToken))

	assert.Equal(t, http.StatusGone, w.Code)
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "revoked")
}

func TestJoinServerExpiredInvite(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joinexpown")
	joiner := ts.CreateTestUser(t, "joinexpjoin")
	serverID := ts.CreateTestServer(t, owner.ID, "JoinExpired Server")

	code := createInvite(t, ts, serverID, owner.AccessToken)

	// Manually expire the invite in the database
	_, err := ts.DB.Exec(
		`UPDATE server_invites SET expires_at = $1 WHERE code = $2`,
		time.Now().UTC().Add(-1*time.Hour), code,
	)
	require.NoError(t, err)

	// Try to join with expired code
	w := ts.DoRequest("POST", pathInvitesJoin, map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(joiner.AccessToken))

	assert.Equal(t, http.StatusGone, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "expired")
}

func TestJoinServerMaxUsesReached(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joinmaxown")
	joiner1 := ts.CreateTestUser(t, "joinmax1")
	joiner2 := ts.CreateTestUser(t, "joinmax2")
	serverID := ts.CreateTestServer(t, owner.ID, "MaxUses Server")

	// Create invite with max_uses=1
	w := ts.DoRequest("POST", pathServers+serverID+pathInvites, map[string]interface{}{
		"max_uses": 1,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	code := invite["code"].(string)

	// First join should succeed
	w = ts.DoRequest("POST", pathInvitesJoin, map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(joiner1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Second join should fail — max uses reached
	w = ts.DoRequest("POST", pathInvitesJoin, map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(joiner2.AccessToken))
	assert.Equal(t, http.StatusGone, w.Code)
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "maximum uses")
}

func TestJoinServerBanned(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joinbannedown")
	banned := ts.CreateTestUser(t, "joinbanneduser")
	serverID := ts.CreateTestServer(t, owner.ID, "Banned Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	// Ban the user
	_, err := ts.DB.Exec(
		`INSERT INTO server_bans (server_id, user_id, banned_by, reason)
		 VALUES ($1, $2, $3, 'test ban')`,
		serverID, banned.ID, owner.ID,
	)
	require.NoError(t, err)

	// Banned user tries to join
	w := ts.DoRequest("POST", pathInvitesJoin, map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(banned.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "banned")
}

func TestJoinServerMissingCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "joinnocode")

	w := ts.DoRequest("POST", pathInvitesJoin, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestJoinServerWrongCodeLength(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "joinshort")

	w := ts.DoRequest("POST", pathInvitesJoin, map[string]interface{}{
		"code": "ABCD",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestJoinServerE2EEKeyRequestsCreated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joine2eeown")
	joiner := ts.CreateTestUser(t, "joine2eejoin")
	serverID := ts.CreateTestServer(t, owner.ID, "E2EE Join Server")

	// Create an encrypted channel
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")

	code := createInvite(t, ts, serverID, owner.AccessToken)

	// Join the server
	w := ts.DoRequest("POST", pathInvitesJoin, map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(joiner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify pending key request was created
	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`,
		channelID, joiner.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "pending key request should be created for E2EE channel")
}

func TestJoinServerUseCountIncremented(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joincountownr")
	joiner := ts.CreateTestUser(t, "joincountjoin")
	serverID := ts.CreateTestServer(t, owner.ID, "UseCount Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	// Get initial use_count
	var initialCount int
	err := ts.DB.QueryRow(`SELECT use_count FROM server_invites WHERE code = $1`, code).Scan(&initialCount)
	require.NoError(t, err)
	assert.Equal(t, 0, initialCount)

	// Join
	w := ts.DoRequest("POST", pathInvitesJoin, map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(joiner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify use_count incremented
	var newCount int
	err = ts.DB.QueryRow(`SELECT use_count FROM server_invites WHERE code = $1`, code).Scan(&newCount)
	require.NoError(t, err)
	assert.Equal(t, 1, newCount)
}

func TestJoinServerDefaultRolesAssigned(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joinroleown")
	joiner := ts.CreateTestUser(t, "joinrolejoin")
	serverID := ts.CreateTestServer(t, owner.ID, "JoinRoles Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	// Join
	w := ts.DoRequest("POST", pathInvitesJoin, map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(joiner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify default @all role was assigned
	var roleCount int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM member_roles mr
		 INNER JOIN roles r ON mr.role_id = r.id
		 WHERE mr.server_id = $1 AND mr.user_id = $2 AND r.is_default = TRUE`,
		serverID, joiner.ID,
	).Scan(&roleCount)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, roleCount, 1, "joiner should have at least the @all role")
}

// =====================================================================
// GetInviteInfo: Edge Cases
// =====================================================================

func TestGetInviteInfoExpired(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "infoxpown")
	serverID := ts.CreateTestServer(t, owner.ID, "InfoExpired Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	// Manually expire the invite
	_, err := ts.DB.Exec(
		`UPDATE server_invites SET expires_at = $1 WHERE code = $2`,
		time.Now().UTC().Add(-1*time.Hour), code,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", pathInvitesAPI+code, nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, false, body["valid"], "expired invite should have valid=false")
}

func TestGetInviteInfoRevoked(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "inforevown")
	serverID := ts.CreateTestServer(t, owner.ID, "InfoRevoked Server")

	// Create and revoke
	w := ts.DoRequest("POST", pathServers+serverID+pathInvites, map[string]interface{}{
		"max_uses": 0,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	invite := createBody["invite"].(map[string]interface{})
	inviteID := invite["id"].(string)
	code := invite["code"].(string)

	w = ts.DoRequest("DELETE", pathServers+serverID+pathInviteSlash+inviteID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// GetInviteInfo should show valid=false
	w = ts.DoRequest("GET", pathInvitesAPI+code, nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, false, body["valid"])
}

func TestGetInviteInfoMaxUsesReached(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "infomaxown")
	joiner := ts.CreateTestUser(t, "infomaxjoin")
	serverID := ts.CreateTestServer(t, owner.ID, "InfoMaxUses Server")

	// Create invite with max_uses=1
	w := ts.DoRequest("POST", pathServers+serverID+pathInvites, map[string]interface{}{
		"max_uses": 1,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	invite := createBody["invite"].(map[string]interface{})
	code := invite["code"].(string)

	// Use it
	w = ts.DoRequest("POST", pathInvitesJoin, map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(joiner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// GetInviteInfo should show valid=false
	w = ts.DoRequest("GET", pathInvitesAPI+code, nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, false, body["valid"])
}

func TestGetInviteInfoNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "infonfound")

	w := ts.DoRequest("GET", "/api/v1/invites/ZZZZZZZZ", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetInviteInfoWrongLength(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "infoshort")

	// Codes must be exactly 8 characters
	w := ts.DoRequest("GET", "/api/v1/invites/ABC", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetInviteInfoShowsMemberCount(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "infomcown")
	serverID := ts.CreateTestServer(t, owner.ID, "MemberCount Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	w := ts.DoRequest("GET", pathInvitesAPI+code, nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	// Owner is the only member
	assert.Equal(t, float64(1), body["member_count"])
	assert.Equal(t, "MemberCount Server", body["server_name"])
}

// =====================================================================
// JoinServer: Verify server response payload
// =====================================================================

func TestJoinServerResponseContainsServerDetails(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joindetailown")
	joiner := ts.CreateTestUser(t, "joindetailjoin")
	serverID := ts.CreateTestServer(t, owner.ID, "Detail Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	w := ts.DoRequest("POST", pathInvitesJoin, map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(joiner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)

	server := body["server"].(map[string]interface{})
	assert.Equal(t, serverID, server["id"])
	assert.Equal(t, "Detail Server", server["name"])
	assert.Equal(t, owner.ID, server["owner_id"])
	assert.Equal(t, "member", body["role"])
}

// =====================================================================
// Cross-server invite isolation
// =====================================================================

func TestRevokeInviteWrongServer(t *testing.T) {
	ts := setupTS(t)
	owner1 := ts.CreateTestUser(t, "crossown1")
	owner2 := ts.CreateTestUser(t, "crossown2")
	serverID1 := ts.CreateTestServer(t, owner1.ID, "Cross Server 1")
	serverID2 := ts.CreateTestServer(t, owner2.ID, "Cross Server 2")

	// Create invite on server 1
	w := ts.DoRequest("POST", pathServers+serverID1+pathInvites, map[string]interface{}{
		"max_uses": 5,
	}, testhelpers.AuthHeaders(owner1.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	inviteID := invite["id"].(string)

	// Try to revoke it from server 2 — should not find it
	w = ts.DoRequest("DELETE", pathServers+serverID2+pathInviteSlash+inviteID, nil, testhelpers.AuthHeaders(owner2.AccessToken))

	// Should be 404 (wrong server) or 403 (no permission on server2 for that invite)
	assert.True(t, w.Code == http.StatusNotFound || w.Code == http.StatusForbidden,
		"expected 404 or 403, got %d", w.Code)
	_ = serverID2 // avoid unused
}
