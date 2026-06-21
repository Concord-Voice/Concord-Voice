package servers_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathServers        = "/api/v1/servers"
	pathServersSlash   = "/api/v1/servers/"
	pathServersInvalid = "/api/v1/servers/not-a-uuid"
	dataImagePrefix    = "data:image/png;base64,"
	testDataURL        = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
)

// =============================================================================
// CreateServer edge cases
// =============================================================================

func TestCreateServerMissingName(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createnoname")

	w := ts.DoRequest("POST", pathServers, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateServerNameTooLong(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createlongname")

	longName := strings.Repeat("a", 101)
	w := ts.DoRequest("POST", pathServers, map[string]interface{}{
		"name": longName,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateServerWithIconDataURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createicon")

	w := ts.DoRequest("POST", pathServers, map[string]interface{}{
		"name":     "Icon Server",
		"icon_url": testDataURL,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Equal(t, testDataURL, server["icon_url"])
}

func TestCreateServerWithInvalidIconURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createbadiconurl")

	w := ts.DoRequest("POST", pathServers, map[string]interface{}{
		"name":     "Bad Icon Server",
		"icon_url": "https://example.com/icon.png",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateServerWithTooLargeIcon(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createlargeicon")

	largeIcon := dataImagePrefix + strings.Repeat("A", 1500001)
	w := ts.DoRequest("POST", pathServers, map[string]interface{}{
		"name":     "Large Icon Server",
		"icon_url": largeIcon,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateServerWithBannerDataURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createbanner")

	w := ts.DoRequest("POST", pathServers, map[string]interface{}{
		"name":       "Banner Server",
		"banner_url": testDataURL,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Equal(t, testDataURL, server["banner_url"])
}

func TestCreateServerWithInvalidBannerURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createbadbanner")

	w := ts.DoRequest("POST", pathServers, map[string]interface{}{
		"name":       "Bad Banner Server",
		"banner_url": "https://example.com/banner.png",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateServerWithTooLargeBanner(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createlargebanner")

	largeBanner := dataImagePrefix + strings.Repeat("A", 3000001)
	w := ts.DoRequest("POST", pathServers, map[string]interface{}{
		"name":       "Large Banner Server",
		"banner_url": largeBanner,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateServerOwnerAddedAsMember(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createownermem")

	w := ts.DoRequest("POST", pathServers, map[string]interface{}{
		"name": "Membership Check Server",
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	serverID := server["id"].(string)

	// Verify the owner is a member via DB
	var role string
	err := ts.DB.QueryRow(`SELECT role FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, user.ID).Scan(&role)
	require.NoError(t, err)
	assert.Equal(t, "owner", role)
}

func TestCreateServerDefaultRoleCreated(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createdfltrole")

	w := ts.DoRequest("POST", pathServers, map[string]interface{}{
		"name": "Default Role Server",
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	serverID := server["id"].(string)

	// Verify @all role was created
	var roleName string
	var isDefault bool
	err := ts.DB.QueryRow(`SELECT name, is_default FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&roleName, &isDefault)
	require.NoError(t, err)
	assert.Equal(t, "@all", roleName)
	assert.True(t, isDefault)
}

func TestCreateServerUnauthenticated(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("POST", pathServers, map[string]interface{}{
		"name": "Unauth Server",
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// =============================================================================
// GetServer edge cases
// =============================================================================

func TestGetServerInvalidID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getinvalidid")

	w := ts.DoRequest("GET", pathServersInvalid, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetServerNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getnotfound")
	fakeID := uuid.New().String()

	w := ts.DoRequest("GET", pathServersSlash+fakeID, nil, testhelpers.AuthHeaders(user.AccessToken))
	// User is not a member of this nonexistent server, so 403 (membership check first)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetServerReturnsMemberRole(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "getrolowner")
	member := ts.CreateTestUser(t, "getrolmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Role Test Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("GET", pathServersSlash+serverID, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "member", body["role"])
}

func TestGetServerUnauthenticated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "getunauthowner")
	serverID := ts.CreateTestServer(t, owner.ID, "Unauth Get Server")

	w := ts.DoRequest("GET", pathServersSlash+serverID, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// =============================================================================
// UpdateServer edge cases
// =============================================================================

func TestUpdateServerInvalidID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updinvalidid")

	w := ts.DoRequest("PATCH", pathServersInvalid, map[string]interface{}{
		"name": "New Name",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateServerInvalidBody(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updinvalidbody")
	serverID := ts.CreateTestServer(t, user.ID, "Invalid Body Server")

	// Missing required name field
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateServerWithIconURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updiconurl")
	serverID := ts.CreateTestServer(t, user.ID, "Icon Update Server")

	// Provide the expected media endpoint path
	expectedIconPath := "/api/v1/media/server-icons/" + serverID
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":     "Updated Server",
		"icon_url": expectedIconPath,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Equal(t, expectedIconPath, server["icon_url"])
}

func TestUpdateServerWithDataURLIcon(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "upddataurlicon")
	serverID := ts.CreateTestServer(t, user.ID, "Data URL Icon Server")

	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":     "Data URL Server",
		"icon_url": testDataURL,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateServerRemoveIcon(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updremoveicon")
	serverID := ts.CreateTestServer(t, user.ID, "Remove Icon Server")

	// Set an icon first via data URL
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":     "Has Icon",
		"icon_url": testDataURL,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Now send icon_url: null to remove it
	w = ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":     "No Icon",
		"icon_url": nil,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Nil(t, server["icon_url"])
}

func TestUpdateServerWithInvalidIconURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updinvalidicon")
	serverID := ts.CreateTestServer(t, user.ID, "Invalid Icon Server")

	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":     "Bad Icon",
		"icon_url": "https://evil.com/icon.png",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateServerWithBannerURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updbannerurl")
	serverID := ts.CreateTestServer(t, user.ID, "Banner Update Server")

	expectedBannerPath := "/api/v1/media/server-banners/" + serverID
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":       "Updated Banner",
		"banner_url": expectedBannerPath,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Equal(t, expectedBannerPath, server["banner_url"])
}

func TestUpdateServerWithInvalidBannerURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updinvalidbanner")
	serverID := ts.CreateTestServer(t, user.ID, "Invalid Banner Server")

	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":       "Bad Banner",
		"banner_url": "https://evil.com/banner.png",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateServerWithTooLargeDataURLIcon(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updlrgdataicon")
	serverID := ts.CreateTestServer(t, user.ID, "Large Data Icon Server")

	largeIcon := dataImagePrefix + strings.Repeat("A", 1500001)
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":     "Large Icon",
		"icon_url": largeIcon,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateServerWithTooLargeDataURLBanner(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updlrgdatabnr")
	serverID := ts.CreateTestServer(t, user.ID, "Large Data Banner Server")

	largeBanner := dataImagePrefix + strings.Repeat("A", 3000001)
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":       "Large Banner",
		"banner_url": largeBanner,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateServerWithAllowEmbeddedContent(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updembed")
	serverID := ts.CreateTestServer(t, user.ID, "Embed Server")

	allowEmbed := true
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":                   "Embed Enabled",
		"allow_embedded_content": allowEmbed,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Equal(t, true, server["allow_embedded_content"])
}

func TestUpdateServerAsAdminForbidden(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "updadminowner")
	admin := ts.CreateTestUser(t, "updadminuser")
	serverID := ts.CreateTestServer(t, owner.ID, "Admin Update Server")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	// AdminPermissions does NOT include PermManageServer — only OwnerPermissions does
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name": "Admin Updated",
	}, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateServerNonexistent(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updnonexist")
	fakeID := uuid.New().String()

	w := ts.DoRequest("PATCH", pathServersSlash+fakeID, map[string]interface{}{
		"name": "Ghost Server",
	}, testhelpers.AuthHeaders(user.AccessToken))
	// Permission check will fail (not a member, so resolver returns no permission)
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError}, w.Code)
}

func TestUpdateServerNameTooShort(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updshortname")
	serverID := ts.CreateTestServer(t, user.ID, "Short Name Server")

	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name": "ab",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateServerRemoveBanner(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updremovebanner")
	serverID := ts.CreateTestServer(t, user.ID, "Remove Banner Server")

	// Set a banner first
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":       "Has Banner",
		"banner_url": testDataURL,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Now send banner_url: null to remove it (covers buildUpdateClauses banner nil branch)
	w = ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":       "No Banner",
		"banner_url": nil,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Nil(t, server["banner_url"])
}

func TestUpdateServerInvalidIconURLType(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updinvicontype")
	serverID := ts.CreateTestServer(t, user.ID, "Invalid Icon Type Server")

	// Send icon_url as a number instead of string (triggers parseMediaURL error)
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":     "Bad Icon Type",
		"icon_url": 12345,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateServerInvalidBannerURLType(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updinvbnrtype")
	serverID := ts.CreateTestServer(t, user.ID, "Invalid Banner Type Server")

	// Send banner_url as a number (triggers parseMediaURL error for banner)
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":       "Bad Banner Type",
		"banner_url": 12345,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateServerIconAndBannerTogether(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updbothimg")
	serverID := ts.CreateTestServer(t, user.ID, "Both Images Server")

	iconPath := "/api/v1/media/server-icons/" + serverID
	bannerPath := "/api/v1/media/server-banners/" + serverID
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":       "Both Images",
		"icon_url":   iconPath,
		"banner_url": bannerPath,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Equal(t, iconPath, server["icon_url"])
	assert.Equal(t, bannerPath, server["banner_url"])
}

func TestUpdateServerRemoveIconAndBannerTogether(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updremoveboth")
	serverID := ts.CreateTestServer(t, user.ID, "Remove Both Server")

	// Set both first
	w := ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":       "Has Both",
		"icon_url":   testDataURL,
		"banner_url": testDataURL,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Remove both via null (covers both nil branches in buildUpdateClauses)
	w = ts.DoRequest("PATCH", pathServersSlash+serverID, map[string]interface{}{
		"name":       "Neither",
		"icon_url":   nil,
		"banner_url": nil,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	server := body["server"].(map[string]interface{})
	assert.Nil(t, server["icon_url"])
	assert.Nil(t, server["banner_url"])
}

// =============================================================================
// DeleteServer edge cases
// =============================================================================

func TestDeleteServerInvalidID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "delinvalidid")

	w := ts.DoRequest("DELETE", pathServersInvalid, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDeleteServerNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "delnotfound")
	fakeID := uuid.New().String()

	w := ts.DoRequest("DELETE", pathServersSlash+fakeID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteServerUnauthenticated(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "delunauthown")
	serverID := ts.CreateTestServer(t, owner.ID, "Unauth Del Server")

	w := ts.DoRequest("DELETE", pathServersSlash+serverID, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestDeleteServerCascadesMembers(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "delcascadeown")
	member := ts.CreateTestUser(t, "delcascademem")
	serverID := ts.CreateTestServer(t, owner.ID, "Cascade Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("DELETE", pathServersSlash+serverID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Verify members are gone
	var count int
	err := ts.DB.QueryRow(`SELECT COUNT(*) FROM server_members WHERE server_id = $1`, serverID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestDeleteServerAdminForbidden(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "deladminfrbdn")
	admin := ts.CreateTestUser(t, "deladminuser")
	serverID := ts.CreateTestServer(t, owner.ID, "Admin Del Server")
	ts.AddMemberToServer(t, serverID, admin.ID, "admin")

	w := ts.DoRequest("DELETE", pathServersSlash+serverID, nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// =============================================================================
// ListServers edge cases
// =============================================================================

func TestListServersMultipleServers(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "listmultiuser")
	ts.CreateTestServer(t, user.ID, "Server Alpha")
	ts.CreateTestServer(t, user.ID, "Server Beta")
	ts.CreateTestServer(t, user.ID, "Server Gamma")

	w := ts.DoRequest("GET", pathServers, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	servers := body["servers"].([]interface{})
	assert.Len(t, servers, 3)
}

func TestListServersShowsMemberCount(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "listcntowner")
	member := ts.CreateTestUser(t, "listcntmem")
	ts.CreateTestServer(t, owner.ID, "Count Server")
	serverID := ts.CreateTestServer(t, owner.ID, "Count Server 2")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("GET", pathServers, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	servers := body["servers"].([]interface{})

	for _, s := range servers {
		srv := s.(map[string]interface{})
		if srv["name"] == "Count Server 2" {
			// Owner + member = 2
			assert.Equal(t, float64(2), srv["member_count"])
		}
	}
}

func TestListServersUnauthenticated(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", pathServers, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestListServersOnlyUserServers(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "listfilter1")
	user2 := ts.CreateTestUser(t, "listfilter2")
	ts.CreateTestServer(t, user1.ID, "User1 Server")
	ts.CreateTestServer(t, user2.ID, "User2 Server")

	// user1 should only see their own server
	w := ts.DoRequest("GET", pathServers, nil, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	servers := body["servers"].([]interface{})
	assert.Len(t, servers, 1)
	assert.Equal(t, "User1 Server", servers[0].(map[string]interface{})["name"])
}
