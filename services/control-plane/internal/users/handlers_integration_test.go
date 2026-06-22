package users_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	invalidBase64     = "!!!not-base64!!!"
	urlUsersMePrefs   = "/api/v1/users/me/preferences"
	urlUsersMePrivacy = "/api/v1/users/me/privacy"
)

// Note: setupTS, testPassword, and various constants are defined in handlers_test.go.

func pathPublicProfile(userID string) string {
	return fmt.Sprintf("/api/v1/users/%s/profile", userID)
}

func pathPublicKey(userID string) string {
	return fmt.Sprintf("/api/v1/users/%s/public-key", userID)
}

// ── GetMe (extended) ─────────────────────────────────────────────────────────

func TestGetMeReturnsAllFields(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getmefields")

	w := ts.DoRequest("GET", urlUsersMe, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	u := body["user"].(map[string]interface{})
	assert.Equal(t, user.ID, u["id"])
	assert.Equal(t, user.Username, u[keyUsername])
	assert.NotEmpty(t, u["email"])
	assert.NotNil(t, u["created_at"])
	// Regression (#1648): the vestigial e2ee_preference field was removed
	// end-to-end. The GET /users/me profile response must not carry it.
	_, hasE2EE := u["e2ee_preference"]
	assert.False(t, hasE2EE, "e2ee_preference removed in #1648; must not appear in GET /users/me")
}

// ── GetPublicProfile ─────────────────────────────────────────────────────────

func TestGetPublicProfileSuccess(t *testing.T) {
	ts := setupTS(t)
	viewer := ts.CreateTestUser(t, "profileviewer")
	target := ts.CreateTestUser(t, "profiletarget")

	w := ts.DoRequest("GET", pathPublicProfile(target.ID), nil, testhelpers.AuthHeaders(viewer.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	u := body["user"].(map[string]interface{})
	assert.Equal(t, target.ID, u["id"])
	assert.Equal(t, target.Username, u[keyUsername])
	// Public profile should NOT include email
	_, hasEmail := u["email"]
	assert.False(t, hasEmail, "public profile should not include email")
}

func TestGetPublicProfileNotFound(t *testing.T) {
	ts := setupTS(t)
	viewer := ts.CreateTestUser(t, "profileviewer2")

	fakeID := "00000000-0000-0000-0000-000000000099"
	w := ts.DoRequest("GET", pathPublicProfile(fakeID), nil, testhelpers.AuthHeaders(viewer.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetPublicProfileUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("GET", pathPublicProfile("someid"), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── UpdateMe (extended) ──────────────────────────────────────────────────────

func TestUpdateMeBioOnly(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updatebio")

	payload := map[string]interface{}{
		"bio": "Hello, I'm a test user!",
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	u := body["user"].(map[string]interface{})
	assert.Equal(t, "Hello, I'm a test user!", u["bio"])
}

func TestUpdateMeBioTooLong(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "biolong")

	payload := map[string]interface{}{
		"bio": strings.Repeat("a", 501),
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMeDisplayNameTooLong(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "dnlong")

	payload := map[string]interface{}{
		"display_name": strings.Repeat("x", 101),
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMeClearDisplayName(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "cleardisplay")

	// First set a display name
	payload := map[string]interface{}{"display_name": "SomeName"}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Clear it
	payload = map[string]interface{}{"display_name": ""}
	w = ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMeNoFields(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "nofields")

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "No fields")
}

func TestUpdateMeInvalidBody(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badbody")

	w := ts.DoRequest(methodPatch, urlUsersMe, "not json", testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMeUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest(methodPatch, urlUsersMe, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestUpdateMeAvatarDataURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "avatardata")

	payload := map[string]interface{}{
		"avatar_url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==",
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMeAvatarClear(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "avatarclear")

	payload := map[string]interface{}{
		"avatar_url": "",
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMeHeaderImageDataURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "headerdata")

	payload := map[string]interface{}{
		"header_image_url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==",
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMeHeaderImageInvalidURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "headerinvalid")

	payload := map[string]interface{}{
		"header_image_url": "https://evil.com/banner.png",
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMeHeaderImageClear(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "headerclear")

	payload := map[string]interface{}{
		"header_image_url": "",
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMeValidAvatarUploadURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "avatarupload")

	payload := map[string]interface{}{
		"avatar_url": fmt.Sprintf("/api/v1/media/avatars/%s", user.ID),
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMeValidBannerUploadURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bannerupload")

	payload := map[string]interface{}{
		"header_image_url": fmt.Sprintf("/api/v1/media/banners/%s", user.ID),
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMeColorSchemeValid(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "colorscheme")

	payload := map[string]interface{}{
		"color_scheme": `{"primary":"#ff0000","secondary":"#00ff00"}`,
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMeColorSchemeInvalidJSON(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "colorbad")

	payload := map[string]interface{}{
		"color_scheme": "not valid json",
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMeColorSchemeTooLong(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "colorlong")

	payload := map[string]interface{}{
		"color_scheme": `{"x":"` + strings.Repeat("a", 200) + `"}`,
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMeColorSchemeClear(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "colorclear")

	payload := map[string]interface{}{
		"color_scheme": "",
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMeLinks(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updatelinks")

	links := json.RawMessage(`["https://github.com/test","https://twitter.com/test"]`)
	payload := map[string]interface{}{
		"links": links,
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMeLinksTooMany(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "linksmany")

	links := json.RawMessage(`["https://a.com","https://b.com","https://c.com","https://d.com","https://e.com","https://f.com"]`)
	payload := map[string]interface{}{
		"links": links,
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMeLinksInvalidProtocol(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "linksbad")

	links := json.RawMessage(`["ftp://evil.com/payload"]`)
	payload := map[string]interface{}{
		"links": links,
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMeLinksNotArray(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "linksnotarr")

	links := json.RawMessage(`"just a string"`)
	payload := map[string]interface{}{
		"links": links,
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMeMultipleFields(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "multifld")

	payload := map[string]interface{}{
		"display_name": "Multi Update",
		"bio":          "Testing multiple fields at once",
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	u := body["user"].(map[string]interface{})
	assert.Equal(t, "Multi Update", u["display_name"])
}

func TestUpdateMeUsernameAlreadyTaken(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "usernamea")
	ts.CreateTestUser(t, "usernameb")

	// Set cooldown to the past so the username change is allowed
	_, err := ts.DB.Exec(
		`UPDATE users SET username_changed_at = NOW() - INTERVAL '400 days' WHERE id = $1`,
		user1.ID,
	)
	require.NoError(t, err)

	payload := map[string]interface{}{
		keyUsername: "usernameb",
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user1.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "taken")
}

// ── GetMyKeys (extended) ─────────────────────────────────────────────────────

func TestGetMyKeysReturnsAlgorithm(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getkeyalg")

	w := ts.DoRequest("GET", urlUsersMeKeys, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	keys := body["e2ee_keys"].(map[string]interface{})
	assert.NotEmpty(t, keys["wrapped_private_key"])
	assert.NotEmpty(t, keys["key_derivation_salt"])
	assert.NotNil(t, keys["key_version"])
	assert.NotNil(t, keys["key_derivation_alg"])
}

func TestGetMyKeysUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("GET", urlUsersMeKeys, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── ReplaceMyKeys (extended) ─────────────────────────────────────────────────

func TestReplaceMyKeysWithAlgorithm(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replkeysalg")

	pubKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		keyWrappedPrivateKey:    wrappedKey,
		keyKeyDerivationSalt:    salt,
		"key_derivation_alg":    "argon2id",
		"public_key":            pubKey,
		"acknowledge_data_loss": true,
		keyCurrentPassword:      user.Password,
	}
	w := ts.DoRequest("PUT", urlUsersMeKeys, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReplaceMyKeysUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("PUT", urlUsersMeKeys, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestReplaceMyKeysRequiresAcknowledgeDataLoss(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replnoack")

	pubKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		keyWrappedPrivateKey: wrappedKey,
		keyKeyDerivationSalt: salt,
		"public_key":         pubKey,
		keyCurrentPassword:   user.Password,
		// acknowledge_data_loss omitted => false. current_password is supplied so
		// the binding:required check passes and the acknowledge_data_loss gate is
		// the specific validation that produces the 400.
	}
	w := ts.DoRequest("PUT", urlUsersMeKeys, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReplaceMyKeysRequiresPublicKey(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replnopub")

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		keyWrappedPrivateKey:    wrappedKey,
		keyKeyDerivationSalt:    salt,
		"acknowledge_data_loss": true,
		keyCurrentPassword:      user.Password,
		// public_key omitted => binding:"required" fails. current_password is
		// supplied so public_key is the only missing required field.
	}
	w := ts.DoRequest("PUT", urlUsersMeKeys, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReplaceMyKeysInvalidPublicKeyBase64(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replbadpub")

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		keyWrappedPrivateKey:    wrappedKey,
		keyKeyDerivationSalt:    salt,
		"public_key":            invalidBase64,
		"acknowledge_data_loss": true,
		keyCurrentPassword:      user.Password,
		// current_password is supplied so binding passes and the public_key
		// base64 decode is the specific validation that produces the 400.
	}
	w := ts.DoRequest("PUT", urlUsersMeKeys, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// The core #1293 invariant: after a reset, user_keys and public_keys are
// consistent, GetPublicKey returns the submitted key, and stale wrapped keys
// for the user are gone.
func TestReplaceMyKeysKeepsPublicKeyConsistent(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replconsist")

	pubKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		keyWrappedPrivateKey:    wrappedKey,
		keyKeyDerivationSalt:    salt,
		"key_derivation_alg":    "argon2id",
		"public_key":            pubKey,
		"acknowledge_data_loss": true,
		keyCurrentPassword:      user.Password,
	}
	w := ts.DoRequest("PUT", urlUsersMeKeys, payload, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// user_keys.key_version == public_keys.key_version
	var ukVersion, pkVersion int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT key_version FROM user_keys WHERE user_id = $1`, user.ID).Scan(&ukVersion))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT key_version FROM public_keys WHERE user_id = $1 ORDER BY key_version DESC LIMIT 1`, user.ID).Scan(&pkVersion))
	assert.Equal(t, ukVersion, pkVersion, "user_keys and public_keys versions must match after reset")

	// GetPublicKey returns the public key whose private counterpart the client holds.
	gw := ts.DoRequest("GET", pathPublicKey(user.ID), nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, gw.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, gw, &body)
	assert.Equal(t, pubKey, body["public_key"], "GetPublicKey must return the just-submitted public key")
}

// Defense-in-depth: if no public_keys row exists for the user, the UPDATE
// affects 0 rows and the whole reset rolls back (user_keys unchanged).
func TestReplaceMyKeysRollsBackWhenNoPublicKeyRow(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replnopkrow")

	var beforeVersion int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT key_version FROM user_keys WHERE user_id = $1`, user.ID).Scan(&beforeVersion))

	_, err := ts.DB.Exec(`DELETE FROM public_keys WHERE user_id = $1`, user.ID)
	require.NoError(t, err)

	pubKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		keyWrappedPrivateKey:    wrappedKey,
		keyKeyDerivationSalt:    salt,
		"public_key":            pubKey,
		"acknowledge_data_loss": true,
		keyCurrentPassword:      user.Password,
	}
	w := ts.DoRequest("PUT", urlUsersMeKeys, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var afterVersion int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT key_version FROM user_keys WHERE user_id = $1`, user.ID).Scan(&afterVersion))
	assert.Equal(t, beforeVersion, afterVersion, "user_keys must be unchanged after rollback")
}

// Step-up auth (#1293): current_password is binding:"required", so a payload
// that omits it is rejected at binding time with 400 — before any DB work.
func TestReplaceMyKeysRequiresCurrentPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replnocurpw")

	pubKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		keyWrappedPrivateKey:    wrappedKey,
		keyKeyDerivationSalt:    salt,
		"public_key":            pubKey,
		"acknowledge_data_loss": true,
		// current_password omitted => binding:"required" fails
	}
	w := ts.DoRequest("PUT", urlUsersMeKeys, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// Step-up auth (#1293): a fully valid payload with the WRONG current_password
// is rejected by verifyResetStepUp → verifyCurrentPassword with 401, before the
// destructive key-replacement transaction runs.
func TestReplaceMyKeysWrongCurrentPassword(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replwrongpw")

	pubKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		keyWrappedPrivateKey:    wrappedKey,
		keyKeyDerivationSalt:    salt,
		"public_key":            pubKey,
		"acknowledge_data_loss": true,
		keyCurrentPassword:      "WrongPassword!!!!",
	}
	w := ts.DoRequest("PUT", urlUsersMeKeys, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// The load-bearing DELETE half of the #1293 invariant: a successful reset must
// purge every wrapped channel/DM key for the user (they were encrypted to the
// now-rotated public key and are unreadable). Seed one channel_keys row and one
// dm_channel_keys row, perform a successful reset, then assert both are gone.
func TestReplaceMyKeysClearsChannelKeys(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replclearkeys")
	other := ts.CreateTestUser(t, "replclearpeer")

	// Seed a channel_keys row. channel_keys.channel_id FKs channels(id), which
	// FKs servers(id) — create the parent rows via the testhelpers.
	serverID := ts.CreateTestServer(t, user.ID, "replclear-server")
	channelID := ts.CreateTestChannel(t, serverID, "replclear-channel")
	_, err := ts.DB.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, $3, 1)`,
		channelID, user.ID, []byte("test-wrapped-channel-key"),
	)
	require.NoError(t, err)

	// Seed a dm_channel_keys row via the DM conversation + key helpers.
	convID := ts.CreateDMConversation(t, user.ID, other.ID)
	ts.SeedDMKey(t, convID, user.ID, 1)

	// Sanity: both rows exist before the reset.
	var preChannel, preDM int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM channel_keys WHERE user_id = $1`, user.ID).Scan(&preChannel))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_channel_keys WHERE user_id = $1`, user.ID).Scan(&preDM))
	require.Equal(t, 1, preChannel, "precondition: one channel_keys row seeded")
	require.Equal(t, 1, preDM, "precondition: one dm_channel_keys row seeded")

	// Perform a successful reset with a full valid payload incl. current_password.
	pubKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		keyWrappedPrivateKey:    wrappedKey,
		keyKeyDerivationSalt:    salt,
		"public_key":            pubKey,
		"acknowledge_data_loss": true,
		keyCurrentPassword:      user.Password,
	}
	w := ts.DoRequest("PUT", urlUsersMeKeys, payload, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// After: both the channel_keys and dm_channel_keys rows must be cleared.
	var postChannel, postDM int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM channel_keys WHERE user_id = $1`, user.ID).Scan(&postChannel))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM dm_channel_keys WHERE user_id = $1`, user.ID).Scan(&postDM))
	assert.Equal(t, 0, postChannel, "channel_keys must be cleared after reset")
	assert.Equal(t, 0, postDM, "dm_channel_keys must be cleared after reset")
}

// ── ChangePassword (extended) ────────────────────────────────────────────────

func TestChangePasswordMissingFields(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "chgpwmiss")

	payload := map[string]interface{}{
		keyCurrentPassword: testPassword,
	}
	w := ts.DoRequest("POST", urlUsersMePassword, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestChangePasswordInvalidKeyBase64(t *testing.T) {
	ts := setupTS(t)
	accessToken := registerAndVerify(t, ts, "chgpwb64@test.concord.chat", "chgpwb64")

	payload := map[string]interface{}{
		keyCurrentPassword:   testPassword,
		keyNewPassword:       "NewPassword456!",
		keyWrappedPrivateKey: invalidBase64,
		keyKeyDerivationSalt: invalidBase64,
	}
	w := ts.DoRequest("POST", urlUsersMePassword, payload, testhelpers.AuthHeaders(accessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestChangePasswordUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("POST", urlUsersMePassword, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── SearchUsers ──────────────────────────────────────────────────────────────

func TestSearchUsersSuccess(t *testing.T) {
	ts := setupTS(t)
	searcher := ts.CreateTestUser(t, "searcher")
	target := ts.CreateTestUser(t, "searchable")

	// Enable searchable_by_username for target
	_, err := ts.DB.Exec(
		`INSERT INTO privacy_settings (user_id, searchable_by_username) VALUES ($1, TRUE)
		 ON CONFLICT (user_id) DO UPDATE SET searchable_by_username = TRUE`,
		target.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", "/api/v1/users/search?q=searchable", nil, testhelpers.AuthHeaders(searcher.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	users := body["users"].([]interface{})
	assert.GreaterOrEqual(t, len(users), 1)
}

func TestSearchUsersNotSearchable(t *testing.T) {
	ts := setupTS(t)
	searcher := ts.CreateTestUser(t, "searcher2")
	ts.CreateTestUser(t, "hiddenusr")

	w := ts.DoRequest("GET", "/api/v1/users/search?q=hiddenusr", nil, testhelpers.AuthHeaders(searcher.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	users := body["users"].([]interface{})
	assert.Empty(t, users, "user with searchable_by_username=false should not appear")
}

func TestSearchUsersQueryTooShort(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "searchshort")

	w := ts.DoRequest("GET", "/api/v1/users/search?q=a", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSearchUsersEmptyQuery(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "searchmempty")

	w := ts.DoRequest("GET", "/api/v1/users/search?q=", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSearchUsersExcludesSelf(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "selfexclude")

	_, err := ts.DB.Exec(
		`INSERT INTO privacy_settings (user_id, searchable_by_username) VALUES ($1, TRUE)
		 ON CONFLICT (user_id) DO UPDATE SET searchable_by_username = TRUE`,
		user.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", "/api/v1/users/search?q=selfexclude", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	users := body["users"].([]interface{})
	for _, u := range users {
		usr := u.(map[string]interface{})
		assert.NotEqual(t, user.ID, usr["id"], "search should exclude self")
	}
}

// ── GetPublicKey (extended) ──────────────────────────────────────────────────

func TestGetPublicKeyReturnsVersion(t *testing.T) {
	ts := setupTS(t)
	requester := ts.CreateTestUser(t, "keyreq")
	target := ts.CreateTestUser(t, "keytarget")

	w := ts.DoRequest("GET", pathPublicKey(target.ID), nil, testhelpers.AuthHeaders(requester.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, target.ID, body["user_id"])
	assert.NotEmpty(t, body["public_key"])
	assert.NotNil(t, body["key_version"])
}

// ── GetPreferences (extended) ────────────────────────────────────────────────

func TestGetPreferencesWithSavedData(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "hasprefuser")

	encData := base64.StdEncoding.EncodeToString([]byte("test-encrypted-prefs"))
	_, err := ts.DB.Exec(
		`INSERT INTO user_preferences (user_id, encrypted_data, version) VALUES ($1, $2, 1)`,
		user.ID, encData,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", urlUsersMePrefs, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	prefs := body["preferences"].(map[string]interface{})
	assert.Equal(t, encData, prefs["encrypted_data"])
	assert.Equal(t, float64(1), prefs["version"])
}

func TestGetPreferencesUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("GET", urlUsersMePrefs, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── UpdatePreferences (extended) ─────────────────────────────────────────────

func TestUpdatePreferencesVersionIncrements(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "versioninc")

	encData := base64.StdEncoding.EncodeToString([]byte("v1"))
	payload := map[string]interface{}{"encrypted_data": encData}
	w := ts.DoRequest("PUT", urlUsersMePrefs, payload, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	encData2 := base64.StdEncoding.EncodeToString([]byte("v2"))
	payload = map[string]interface{}{"encrypted_data": encData2}
	w = ts.DoRequest("PUT", urlUsersMePrefs, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(2), body["version"])
}

func TestUpdatePreferencesInvalidBase64(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "prefb64bad")

	payload := map[string]interface{}{
		"encrypted_data": invalidBase64,
	}
	w := ts.DoRequest("PUT", urlUsersMePrefs, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdatePreferencesTooLarge(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "preflarge")

	bigData := base64.StdEncoding.EncodeToString(make([]byte, 50000))
	payload := map[string]interface{}{
		"encrypted_data": bigData,
	}
	w := ts.DoRequest("PUT", urlUsersMePrefs, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdatePreferencesMissingData(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "prefmissing")

	w := ts.DoRequest("PUT", urlUsersMePrefs, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdatePreferencesUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("PUT", urlUsersMePrefs, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── GetPrivacySettings ───────────────────────────────────────────────────────

func TestGetPrivacySettingsDefaults(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privdefault")

	w := ts.DoRequest("GET", urlUsersMePrivacy, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	priv := body["privacy"].(map[string]interface{})
	assert.Equal(t, true, priv["messages_friends_only"])
	assert.Equal(t, true, priv["messages_server_members"])
	assert.Equal(t, float64(2), priv["dm_privacy_level"])
	assert.Equal(t, false, priv["searchable_by_username"])
	// #1766: no-row fallback must default load_gifs_automatically ON for new users.
	assert.Equal(t, true, priv["load_gifs_automatically"])
}

func TestGetPrivacySettingsWithSavedSettings(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privsaved")

	_, err := ts.DB.Exec(
		`INSERT INTO privacy_settings (user_id, searchable_by_username, dm_privacy_level) VALUES ($1, TRUE, 3)`,
		user.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", urlUsersMePrivacy, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	priv := body["privacy"].(map[string]interface{})
	assert.Equal(t, true, priv["searchable_by_username"])
	assert.Equal(t, float64(3), priv["dm_privacy_level"])
}

// ── UpdatePrivacySettings ────────────────────────────────────────────────────

func TestUpdatePrivacySettingsSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privupd")

	// Regression lock for #1674: a SINGLE PATCH on a fresh user (no pre-existing
	// privacy_settings row) MUST persist the submitted value. The prior UPSERT
	// (INSERT (user_id) VALUES ($1) ON CONFLICT DO UPDATE) silently dropped
	// first-write values because the no-conflict INSERT skipped the SET clause;
	// this test previously masked that with a two-PATCH workaround.
	payload := map[string]interface{}{
		"searchable_by_username": true,
	}
	w := ts.DoRequest(methodPatch, urlUsersMePrivacy, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	priv := body["privacy"].(map[string]interface{})
	assert.Equal(t, true, priv["searchable_by_username"], "response must reflect the submitted value")

	// Verify the value actually persisted in the DB (not just echoed in the response).
	var persisted bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT searchable_by_username FROM privacy_settings WHERE user_id = $1`, user.ID,
	).Scan(&persisted))
	assert.True(t, persisted, "first PATCH must persist to the DB, not leave defaults")
}

func TestUpdatePrivacySettingsDMPrivacyLevel(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privdm")

	// Single PATCH on a fresh user (no seed) — first write must persist (#1674).
	payload := map[string]interface{}{
		"dm_privacy_level": 1,
	}
	w := ts.DoRequest(methodPatch, urlUsersMePrivacy, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	priv := body["privacy"].(map[string]interface{})
	assert.Equal(t, float64(1), priv["dm_privacy_level"])
}

func TestUpdatePrivacySettingsInvalidDMLevel(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privdmbad")

	payload := map[string]interface{}{
		"dm_privacy_level": 5,
	}
	w := ts.DoRequest(methodPatch, urlUsersMePrivacy, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdatePrivacySettingsNoFields(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privnofld")

	w := ts.DoRequest(methodPatch, urlUsersMePrivacy, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdatePrivacySettingsInvalidBody(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privbadbody")

	w := ts.DoRequest(methodPatch, urlUsersMePrivacy, "not json", testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdatePrivacySettingsMultipleBooleans(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privmulti")

	// Seed the row so ON CONFLICT UPDATE fires
	_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, user.ID)
	require.NoError(t, err)

	payload := map[string]interface{}{
		"searchable_by_username":   true,
		"searchable_by_email":      true,
		"allow_embedded_content":   true,
		"auto_accept_friend_codes": false,
	}
	w := ts.DoRequest(methodPatch, urlUsersMePrivacy, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	priv := body["privacy"].(map[string]interface{})
	assert.Equal(t, true, priv["searchable_by_username"])
	assert.Equal(t, true, priv["searchable_by_email"])
	assert.Equal(t, true, priv["allow_embedded_content"])
	assert.Equal(t, false, priv["auto_accept_friend_codes"])
}

func TestGetPrivacySettingsKlipyDefaults(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privklipydef")

	w := ts.DoRequest("GET", urlUsersMePrivacy, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	priv := body["privacy"].(map[string]interface{})
	// #1766: GIF auto-load now defaults ON for new users (KLIPY media is always
	// proxied through the control-plane, so auto-load doesn't expose the user's IP).
	assert.Equal(t, true, priv["load_gifs_automatically"])
	// Proxy toggle stays privacy-first OFF (opt-in to the slight latency cost).
	assert.Equal(t, false, priv["enable_klipy_proxy"])
	// Personalization defaults ON because turning it OFF degrades search quality
	assert.Equal(t, true, priv["share_personalization_with_gif_provider"])
}

func TestUpdatePrivacySettingsKlipyFields(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privklipyupd")

	// Seed the row so ON CONFLICT UPDATE fires
	_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, user.ID)
	require.NoError(t, err)

	payload := map[string]interface{}{
		"load_gifs_automatically":                 true,
		"enable_klipy_proxy":                      true,
		"share_personalization_with_gif_provider": false,
	}
	w := ts.DoRequest(methodPatch, urlUsersMePrivacy, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	priv := body["privacy"].(map[string]interface{})
	assert.Equal(t, true, priv["load_gifs_automatically"])
	assert.Equal(t, true, priv["enable_klipy_proxy"])
	assert.Equal(t, false, priv["share_personalization_with_gif_provider"])
}

func TestUpdatePrivacySettingsKlipyPartialUpdate(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privklipypart")

	_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, user.ID)
	require.NoError(t, err)

	// Set all three to non-default values first
	w := ts.DoRequest(methodPatch, urlUsersMePrivacy, map[string]interface{}{
		"load_gifs_automatically":                 true,
		"enable_klipy_proxy":                      true,
		"share_personalization_with_gif_provider": false,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Now update only one field — the others must keep their previous values
	w = ts.DoRequest(methodPatch, urlUsersMePrivacy, map[string]interface{}{
		"enable_klipy_proxy": false,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	priv := body["privacy"].(map[string]interface{})
	assert.Equal(t, true, priv["load_gifs_automatically"], "unchanged field should keep its value")
	assert.Equal(t, false, priv["enable_klipy_proxy"], "explicitly updated field")
	assert.Equal(t, false, priv["share_personalization_with_gif_provider"], "unchanged field should keep its value")
}

func TestUpdatePrivacySettingsDMLevel0LegacySync(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privdm0")

	// Seed the row so ON CONFLICT UPDATE fires
	_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, user.ID)
	require.NoError(t, err)

	payload := map[string]interface{}{
		"dm_privacy_level": 0,
	}
	w := ts.DoRequest(methodPatch, urlUsersMePrivacy, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	priv := body["privacy"].(map[string]interface{})
	assert.Equal(t, float64(0), priv["dm_privacy_level"])
	// Legacy sync: level 0 => friends_only=true, server_members=false
	assert.Equal(t, true, priv["messages_friends_only"])
	assert.Equal(t, false, priv["messages_server_members"])
}

func TestUpdatePrivacySettingsDMLevel3LegacySync(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privdm3")

	// Seed the row so ON CONFLICT UPDATE fires
	_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, user.ID)
	require.NoError(t, err)

	payload := map[string]interface{}{
		"dm_privacy_level": 3,
	}
	w := ts.DoRequest(methodPatch, urlUsersMePrivacy, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	priv := body["privacy"].(map[string]interface{})
	assert.Equal(t, float64(3), priv["dm_privacy_level"])
	// Legacy sync: level 3 => friends_only=false, server_members=true
	assert.Equal(t, false, priv["messages_friends_only"])
	assert.Equal(t, true, priv["messages_server_members"])
}

// ── DM Privacy Level 1 Legacy Sync ───────────────────────────────────────────

func TestUpdatePrivacySettingsDMLevel1LegacySync(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privdm1sync")

	_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, user.ID)
	require.NoError(t, err)

	payload := map[string]interface{}{
		"dm_privacy_level": 1,
	}
	w := ts.DoRequest(methodPatch, urlUsersMePrivacy, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	priv := body["privacy"].(map[string]interface{})
	assert.Equal(t, float64(1), priv["dm_privacy_level"])
	// Legacy sync: level 1 => friends_only=true, server_members=false
	assert.Equal(t, true, priv["messages_friends_only"])
	assert.Equal(t, false, priv["messages_server_members"])
}

// ── DM Privacy Level 2 Legacy Sync ───────────────────────────────────────────

func TestUpdatePrivacySettingsDMLevel2LegacySync(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "privdm2")

	_, err := ts.DB.Exec(`INSERT INTO privacy_settings (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, user.ID)
	require.NoError(t, err)

	payload := map[string]interface{}{
		"dm_privacy_level": 2,
	}
	w := ts.DoRequest(methodPatch, urlUsersMePrivacy, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	priv := body["privacy"].(map[string]interface{})
	assert.Equal(t, float64(2), priv["dm_privacy_level"])
	// Legacy sync: level 2 => friends_only=true, server_members=true
	assert.Equal(t, true, priv["messages_friends_only"])
	assert.Equal(t, true, priv["messages_server_members"])
}

// ── Link edge cases ──────────────────────────────────────────────────────────

func TestUpdateMeLinkTooLong(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "linktoolong")

	longLink := "https://" + strings.Repeat("a", 500)
	links := json.RawMessage(fmt.Sprintf(`["%s"]`, longLink))
	payload := map[string]interface{}{
		"links": links,
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── Avatar too large ─────────────────────────────────────────────────────────

func TestUpdateMeAvatarTooLarge(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "avatarbig")

	bigData := "data:image/png;base64," + strings.Repeat("A", 1500001)
	payload := map[string]interface{}{
		"avatar_url": bigData,
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── Header image too large ───────────────────────────────────────────────────

func TestUpdateMeHeaderImageTooLarge(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "headerbig")

	bigData := "data:image/png;base64," + strings.Repeat("A", 3000001)
	payload := map[string]interface{}{
		"header_image_url": bigData,
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── ReplaceKeys invalid salt ─────────────────────────────────────────────────

func TestReplaceMyKeysInvalidSalt(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replkeybadsalt")

	_, wrappedKey, _ := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		keyWrappedPrivateKey: wrappedKey,
		keyKeyDerivationSalt: invalidBase64,
	}
	w := ts.DoRequest("PUT", urlUsersMeKeys, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── ChangePassword invalid salt ──────────────────────────────────────────────

func TestChangePasswordInvalidSaltOnly(t *testing.T) {
	ts := setupTS(t)
	accessToken := registerAndVerify(t, ts, "chgpwsaltonly@test.concord.chat", "chgpwsaltonly")

	_, wrappedKey, _ := testhelpers.E2EETestKeys()
	payload := map[string]interface{}{
		keyCurrentPassword:   testPassword,
		keyNewPassword:       "NewPassword456!",
		keyWrappedPrivateKey: wrappedKey,
		keyKeyDerivationSalt: invalidBase64,
	}
	w := ts.DoRequest("POST", urlUsersMePassword, payload, testhelpers.AuthHeaders(accessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── Unverified email blocks protected user routes ────────────────────────────

func TestUsersUnverifiedEmailBlocked(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUserUnverified(t, "unverified")

	// PATCH /users/me should be blocked (protected route)
	payload := map[string]interface{}{"display_name": "Nope"}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	// GET /users/me should still work (in pendingOK group)
	w = ts.DoRequest("GET", urlUsersMe, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}
