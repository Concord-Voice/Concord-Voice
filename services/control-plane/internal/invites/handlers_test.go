package invites_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

const (
	invitesPath         = "/api/v1/invites/"
	invitePreviewSuffix = "/preview"
)

// percentEncodeFirstByte returns code with its leading byte percent-encoded.
// The DECODED path is byte-identical to the canonical one, so gin routes it to
// the same handler with the same :code param; the only difference lives in
// URL.RawPath — which is precisely what the edge rate-limit rule matches on and
// the origin must therefore reject (#945, VULN-001).
func percentEncodeFirstByte(code string) string {
	return fmt.Sprintf("%%%02X%s", code[0], code[1:])
}

// Helper to create invite and return the code
func createInvite(t *testing.T, ts *testhelpers.TestServer, serverID, token string) string {
	t.Helper()
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/invites", map[string]interface{}{
		"max_uses":   0,
		"expires_in": 86400,
	}, testhelpers.AuthHeaders(token))
	require.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	return invite["code"].(string)
}

// --- Create Invite ---

func TestCreateInviteSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "invowner")
	serverID := ts.CreateTestServer(t, owner.ID, "Invite Server")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/invites", map[string]interface{}{
		"max_uses": 5,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	assert.NotEmpty(t, invite["code"])
	assert.Equal(t, float64(5), invite["max_uses"])
}

func TestCreateInviteNotAdmin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "invown2")
	member := ts.CreateTestUser(t, "invmem")
	serverID := ts.CreateTestServer(t, owner.ID, "Invite No Admin")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/invites", nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- List Invites ---

func TestListInvitesSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "listinvown")
	serverID := ts.CreateTestServer(t, owner.ID, "List Invite Server")
	createInvite(t, ts, serverID, owner.AccessToken)

	w := ts.DoRequest("GET", "/api/v1/servers/"+serverID+"/invites", nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invites := body["invites"].([]interface{})
	assert.Len(t, invites, 1)
}

// --- Revoke Invite ---

func TestRevokeInviteSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "revokeown")
	serverID := ts.CreateTestServer(t, owner.ID, "Revoke Server")

	// Create and get invite ID
	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/invites", map[string]interface{}{
		"max_uses": 1,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	invite := body["invite"].(map[string]interface{})
	inviteID := invite["id"].(string)

	w = ts.DoRequest("DELETE", "/api/v1/servers/"+serverID+"/invites/"+inviteID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Join Server ---

func TestJoinServerSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joinown")
	joiner := ts.CreateTestUser(t, "joiner")
	serverID := ts.CreateTestServer(t, owner.ID, "Join Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	w := ts.DoRequest("POST", "/api/v1/invites/join", map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(joiner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotNil(t, body["server"])
}

func TestJoinServerAlreadyMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "joinown2")
	serverID := ts.CreateTestServer(t, owner.ID, "Already In Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	w := ts.DoRequest("POST", "/api/v1/invites/join", map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestJoinServerInvalidCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badinvite")

	w := ts.DoRequest("POST", "/api/v1/invites/join", map[string]interface{}{
		"code": "ZZZZZZZZ",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- Get Invite Info ---

func TestGetInviteInfoSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "infoown")
	serverID := ts.CreateTestServer(t, owner.ID, "Info Server")
	code := createInvite(t, ts, serverID, owner.AccessToken)

	w := ts.DoRequest("GET", "/api/v1/invites/"+code, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Info Server", body["server_name"])
	assert.Equal(t, true, body["valid"])
}

func TestGetInviteInfoInvalidCode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "infobad")

	w := ts.DoRequest("GET", "/api/v1/invites/BADCODE1", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetPublicInvitePreviewValidMinimalFields(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "publicprevown")
	serverID := ts.CreateTestServer(t, owner.ID, "Public Preview")
	_, err := ts.DB.Exec(`UPDATE servers SET icon_url = $1, banner_url = $2 WHERE id = $3`,
		"/api/v1/media/server-icons/"+serverID,
		"/api/v1/media/server-banners/"+serverID,
		serverID,
	)
	require.NoError(t, err)
	code := createInvite(t, ts, serverID, owner.AccessToken)

	w := ts.DoRequest("GET", "/api/v1/invites/"+code+"/preview", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["valid"])
	assert.Equal(t, "Public Preview", body["server_name"])
	assert.Equal(t, "/api/v1/invites/"+code+"/icon", body["icon_url"])
	assert.NotContains(t, body, "server_id")
	assert.NotContains(t, body, "server_banner")
	assert.NotContains(t, body, "member_count")
}

func TestGetPublicInvitePreviewDeadStatesAreUniform(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "publicdeadown")
	serverID := ts.CreateTestServer(t, owner.ID, "Hidden Dead Server")
	revoked := createInvite(t, ts, serverID, owner.AccessToken)
	_, err := ts.DB.Exec(`UPDATE server_invites SET is_revoked = true WHERE code = $1`, revoked)
	require.NoError(t, err)
	valid := createInvite(t, ts, serverID, owner.AccessToken)

	t.Run("every invalid class returns byte-identical output", func(t *testing.T) {
		// Ordered, not a map: the reference body must be a fixed case rather
		// than whichever one Go's randomized map iteration happened to visit
		// first.
		//
		// The last three carry percent-encoding and use the VALID code on
		// purpose. Gin routes on the DECODED URL.Path while the edge
		// rate-limit rule matches the RAW wire path, so these reach the handler
		// with no managed challenge and no edge bucket. They must land in the
		// same uniform invalid shape as every other rejected class — rejecting
		// them with anything distinguishable would trade the rate-limit bypass
		// for an enumeration oracle (#945, VULN-001).
		cases := []struct{ name, path string }{
			{"revoked", invitesPath + revoked + invitePreviewSuffix},
			{"unknown", invitesPath + "GHJKMNPQ" + invitePreviewSuffix},
			{"malformed", invitesPath + "not-a-code" + invitePreviewSuffix},
			{"encoded separator, uppercase", invitesPath + valid + "%2Fpreview"},
			{"encoded separator, lowercase", invitesPath + valid + "%2fpreview"},
			{
				"encoded character inside the code",
				invitesPath + percentEncodeFirstByte(valid) + invitePreviewSuffix,
			},
		}

		reference := ts.DoRequest("GET", cases[0].path, nil, nil)
		require.Equal(t, http.StatusOK, reference.Code)
		require.JSONEq(t, `{"valid":false}`, reference.Body.String())

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				w := ts.DoRequest("GET", tc.path, nil, nil)
				assert.Equal(t, http.StatusOK, w.Code)
				// Byte equality, not field equality: a whitespace, key-order,
				// or extra-key difference between classes is itself an oracle.
				assert.Equal(t, reference.Body.Bytes(), w.Body.Bytes(),
					"invalid classes must be byte-indistinguishable")
			})
		}
	})

	t.Run("the canonical path still previews while its encoded twin does not", func(t *testing.T) {
		// The pairing is the point: the guard must reject the encoded form
		// WITHOUT costing the canonical form its preview.
		canonical := ts.DoRequest("GET", invitesPath+valid+invitePreviewSuffix, nil, nil)
		require.Equal(t, http.StatusOK, canonical.Code)
		require.Contains(t, canonical.Body.String(), `"valid":true`)
		require.Contains(t, canonical.Body.String(), "Hidden Dead Server")

		encoded := ts.DoRequest("GET", invitesPath+valid+"%2Fpreview", nil, nil)
		assert.Equal(t, http.StatusOK, encoded.Code)
		assert.NotContains(t, encoded.Body.String(), "Hidden Dead Server",
			"the encoded path must disclose nothing about the invite's server")
	})
}

func TestInviteHandlersDoNotLogRawInviteCodes(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "logcodeown")
	joiner := ts.CreateTestUser(t, "logcodejoin")
	serverID := ts.CreateTestServer(t, owner.ID, "Log Code Server")
	logs := ts.CaptureLogs(t)

	w := ts.DoRequest("POST", "/api/v1/servers/"+serverID+"/invites", map[string]interface{}{
		"max_uses": 0,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	code := body["invite"].(map[string]interface{})["code"].(string)

	w = ts.DoRequest("POST", "/api/v1/invites/join", map[string]interface{}{
		"code": code,
	}, testhelpers.AuthHeaders(joiner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	assert.NotContains(t, logs.String(), code)
	assert.False(t, strings.Contains(logs.String(), `"code"`), logs.String())
}
