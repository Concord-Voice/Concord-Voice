package users_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	invalidBase64     = "!!!not-base64!!!"
	urlUsersMePrefs   = "/api/v1/users/me/preferences"
	urlUsersMePrivacy = "/api/v1/users/me/privacy"
)

func seedLiveTier1ProfileMedia(t *testing.T, ts *testhelpers.TestServer, userID, key string) {
	t.Helper()
	var slot *string
	switch {
	case strings.HasPrefix(key, "avatars/"+userID+"/") || key == "avatars/"+userID:
		value := "avatar"
		slot = &value
	case strings.HasPrefix(key, "banners/"+userID+"/") || key == "banners/"+userID:
		value := "banner"
		slot = &value
	}
	_, err := ts.DB.Exec(`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot)
		VALUES ($1, $2, 'photo', 1, 'image/png', 4, $3, $4)`, uuid.New().String(), userID, key, slot)
	require.NoError(t, err)
}

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

func TestUpdateMeInlineProfileImageTerminalizesCanonicalMedia(t *testing.T) {
	for _, tc := range []struct {
		name, field, prefix, slot string
	}{
		{"avatar", "avatar_url", "avatars/", "avatar"},
		{"banner", "header_image_url", "banners/", "banner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "inlineclear"+tc.name)
			key := tc.prefix + user.ID + "/" + uuid.NewString()
			seedLiveTier1ProfileMedia(t, ts, user.ID, key)
			canonicalURL := fmt.Sprintf("/api/v1/media/%s%s", tc.prefix, user.ID)
			column := "avatar_url"
			if tc.slot == "banner" {
				column = "header_image_url"
			}
			_, err := ts.DB.Exec(fmt.Sprintf("UPDATE users SET %s = $1 WHERE id = $2", column), canonicalURL, user.ID)
			require.NoError(t, err)

			inlineValue := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
			response := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
				tc.field: inlineValue,
			}, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusOK, response.Code)

			var storedURL *string
			require.NoError(t, ts.DB.QueryRow(fmt.Sprintf("SELECT %s FROM users WHERE id = $1", column), user.ID).Scan(&storedURL))
			require.NotNil(t, storedURL)
			assert.Equal(t, inlineValue, *storedURL, "inline replacement must preserve the accepted data URL")

			_, found, err := media.ProfileTier1StorageKey(t.Context(), ts.DB, user.ID, tc.slot)
			require.NoError(t, err)
			assert.False(t, found, "retired profile media must no longer resolve through the canonical proxy")

			// The media row is retired and represented by a durable delete obligation.
			var retiredAt sql.NullTime
			require.NoError(t, ts.DB.QueryRow(`SELECT deleted_at FROM media_files WHERE storage_key = $1`, key).Scan(&retiredAt))
			assert.True(t, retiredAt.Valid, "inline replacement must retire the old profile media")

			var obligations int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM tier1_erasure_delete_obligations WHERE storage_key = $1`, key).Scan(&obligations))
			assert.Equal(t, 1, obligations, "inline replacement must persist a durable deletion obligation")

		})
	}
}

func TestUpdateMeInlineProfileImageRefusesVendorMedia(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "inlinevendoravatar")
	key := "avatars/" + user.ID + "/" + uuid.NewString()
	seedLiveTier1ProfileMedia(t, ts, user.ID, key)
	_, err := ts.DB.Exec(`UPDATE media_files SET storage_backend = 'r2-useast' WHERE storage_key = $1`, key)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE users SET avatar_url = $1 WHERE id = $2`,
		fmt.Sprintf("/api/v1/media/avatars/%s", user.ID), user.ID)
	require.NoError(t, err)

	response := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		"avatar_url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, response.Code, "vendor-backed profile media must fail closed")

	var storedURL *string
	require.NoError(t, ts.DB.QueryRow(`SELECT avatar_url FROM users WHERE id = $1`, user.ID).Scan(&storedURL))
	require.NotNil(t, storedURL)
	assert.Equal(t, fmt.Sprintf("/api/v1/media/avatars/%s", user.ID), *storedURL, "failed replacement must retain the canonical URL")

	var retiredAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(`SELECT deleted_at FROM media_files WHERE storage_key = $1`, key).Scan(&retiredAt))
	assert.False(t, retiredAt.Valid, "failed replacement must leave vendor media live")

	var obligations int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM tier1_erasure_delete_obligations WHERE storage_key = $1`, key).Scan(&obligations))
	assert.Zero(t, obligations, "failed replacement must not record an unsafe deletion obligation")
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
	seedLiveTier1ProfileMedia(t, ts, user.ID, "avatars/"+user.ID)

	payload := map[string]interface{}{
		"avatar_url": fmt.Sprintf("/api/v1/media/avatars/%s", user.ID),
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMeValidBannerUploadURL(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bannerupload")
	seedLiveTier1ProfileMedia(t, ts, user.ID, "banners/"+user.ID)

	payload := map[string]interface{}{
		"header_image_url": fmt.Sprintf("/api/v1/media/banners/%s", user.ID),
	}
	w := ts.DoRequest(methodPatch, urlUsersMe, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateMeCanonicalMediaURLRequiresOwnedLiveTier1Media(t *testing.T) {
	for _, tc := range []struct {
		name, field, prefix string
	}{
		{"avatar", "avatar_url", "avatars/"},
		{"banner", "header_image_url", "banners/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, want := range []struct {
				name          string
				seedMedia     bool
				seedTombstone bool
				wrongOwner    bool
				status        int
			}{
				{"missing owned media", false, false, false, http.StatusBadRequest},
				{"tombstoned media", true, true, false, http.StatusBadRequest},
				{"wrong owner media", true, false, true, http.StatusBadRequest},
			} {
				t.Run(want.name, func(t *testing.T) {
					ts := setupTS(t)
					user := ts.CreateTestUser(t, "canonical"+tc.name+strings.ReplaceAll(want.name, " ", ""))
					key := tc.prefix + user.ID
					if want.seedMedia {
						uploaderID := user.ID
						if want.wrongOwner {
							other := ts.CreateTestUser(t, "otherowner"+tc.name)
							uploaderID = other.ID
							key = tc.prefix + uploaderID
						}
						_, err := ts.DB.Exec(
							`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, profile_slot)
						 VALUES ($1, $2, 'photo', 1, 'image/png', 4, $3, $4)`,
							uuid.New().String(), uploaderID, key, tc.name,
						)
						require.NoError(t, err)
					}
					if want.seedTombstone {
						_, err := ts.DB.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key) VALUES ($1)`, key)
						require.NoError(t, err)
					}

					url := fmt.Sprintf("/api/v1/media/%s%s", tc.prefix, user.ID)
					w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{tc.field: url}, testhelpers.AuthHeaders(user.AccessToken))
					assert.Equal(t, want.status, w.Code)
				})
			}
		})
	}
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

	// #2201: the reset rotated the credential epoch, so the PRE-reset access
	// token is now dead. Continue as the continuation contract intends: with
	// the fresh pair the response carries.
	var replaceBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &replaceBody)
	continuationToken, ok := replaceBody["access_token"].(string)
	require.True(t, ok, "ReplaceMyKeys response must carry a continuation access_token")
	require.NotEmpty(t, replaceBody["refresh_token"], "continuation refresh_token expected")

	// user_keys.key_version == public_keys.key_version
	var ukVersion, pkVersion int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT key_version FROM user_keys WHERE user_id = $1`, user.ID).Scan(&ukVersion))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT key_version FROM public_keys WHERE user_id = $1 ORDER BY key_version DESC LIMIT 1`, user.ID).Scan(&pkVersion))
	assert.Equal(t, ukVersion, pkVersion, "user_keys and public_keys versions must match after reset")

	// GetPublicKey returns the public key whose private counterpart the client holds.
	gw := ts.DoRequest("GET", pathPublicKey(user.ID), nil, testhelpers.AuthHeaders(continuationToken))
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
	_, err = ts.DB.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`,
		channelID, user.ID,
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
	var postMarker int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM channel_initial_key_distributions WHERE channel_id = $1`, channelID).Scan(&postMarker))
	assert.Zero(t, postMarker)
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

// ── require_auth_before_purge (#1354) ────────────────────────────────────────

// privacyRequireAuthBeforePurge reads require_auth_before_purge out of a
// GET/PATCH /users/me/privacy response body.
func privacyRequireAuthBeforePurge(t *testing.T, respBody []byte) bool {
	t.Helper()
	var body struct {
		Privacy struct {
			RequireAuthBeforePurge bool `json:"require_auth_before_purge"`
		} `json:"privacy"`
	}
	require.NoError(t, json.Unmarshal(respBody, &body))
	return body.Privacy.RequireAuthBeforePurge
}

func TestGetPrivacySettingsRequireAuthBeforePurgeDefaultsTrueWithNoRow(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgedefault")
	// Deliberately do NOT create a privacy_settings row — rows are created lazily
	// on the first PATCH, so "no row" is the majority state.

	w := ts.DoRequest("GET", urlUsersMePrivacy, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	require.True(t, privacyRequireAuthBeforePurge(t, w.Body.Bytes()),
		"a user with no privacy_settings row must default to TRUE — the DM purge handler "+
			"fail-closes to true, so a false default renders the toggle OFF while purges 403")
}

func TestPatchPrivacySettingsRequireAuthBeforePurgeRoundTrips(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgepatch")

	// Turning the fence OFF carries the step-up credential since #2765; turning
	// it ON, and every other privacy field, still needs none.
	patch := ts.DoRequest(methodPatch, urlUsersMePrivacy,
		map[string]interface{}{
			"require_auth_before_purge": false,
			keyCurrentPassword:          testhelpers.TestAuthPlaintext,
		},
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, patch.Code, patch.Body.String())
	assert.False(t, privacyRequireAuthBeforePurge(t, patch.Body.Bytes()),
		"the PATCH response must echo the value it just wrote")

	get := ts.DoRequest("GET", urlUsersMePrivacy, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())
	assert.False(t, privacyRequireAuthBeforePurge(t, get.Body.Bytes()))
}

func TestPatchUnrelatedFieldDoesNotClobberRequireAuthBeforePurge(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "purgeclobber")

	require.Equal(t, http.StatusOK, ts.DoRequest(methodPatch, urlUsersMePrivacy,
		map[string]interface{}{
			"require_auth_before_purge": false,
			keyCurrentPassword:          testhelpers.TestAuthPlaintext,
		},
		testhelpers.AuthHeaders(user.AccessToken)).Code)
	// A PATCH that does not mention the field must leave it alone.
	require.Equal(t, http.StatusOK, ts.DoRequest(methodPatch, urlUsersMePrivacy,
		map[string]interface{}{"load_gifs_automatically": true},
		testhelpers.AuthHeaders(user.AccessToken)).Code)

	get := ts.DoRequest("GET", urlUsersMePrivacy, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())
	assert.False(t, privacyRequireAuthBeforePurge(t, get.Body.Bytes()),
		"an unrelated PATCH must not reset the field")
}

// TestPrivacyToggleGovernsDMPurgeStepUp is the only test proving the
// users-package WRITE is the dm-package READ: the toggle this endpoint exposes
// is the same row internal/dm/purge.go consults before a DM purge.
func TestPrivacyToggleGovernsDMPurgeStepUp(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "purgee2eactor")
	peer := ts.CreateTestUser(t, "purgee2epeer")
	convID := ts.CreateDMConversation(t, actor.ID, peer.ID)
	purgePath := "/api/v1/dm/conversations/" + convID + "/messages"

	// Toggle ON (the row-less default): a credential-less purge must be refused.
	w := ts.DoRequest(http.MethodDelete, purgePath, map[string]interface{}{"range": "1h"},
		testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "password_required")

	// Toggle OFF through the privacy endpoint — which since #2765 demands the
	// same credential the purge itself demands...
	require.Equal(t, http.StatusOK, ts.DoRequest(methodPatch, urlUsersMePrivacy,
		map[string]interface{}{
			"require_auth_before_purge": false,
			keyCurrentPassword:          testhelpers.TestAuthPlaintext,
		},
		testhelpers.AuthHeaders(actor.AccessToken)).Code)

	// ...and the same request now passes step-up.
	w2 := ts.DoRequest(http.MethodDelete, purgePath, map[string]interface{}{"range": "1h"},
		testhelpers.AuthHeaders(actor.AccessToken))
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
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

	bigData := oversizedFreeAvatarDataURL()
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

	bigData := oversizedFreeBannerDataURL()
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

// ── #2415: server-side continuation-token contract ───────────────────────────
//
// The desktop Login ordering fix adopts the continuation pair that
// appendContinuationPair (internal/users/handlers.go) attaches to the committed
// 2xx of ChangePassword and ReplaceMyKeys. These four tests lock the server
// behaviours that adoption now depends on. They are deliberately behavioural:
// each one would go red if the corresponding server-side guarantee were
// weakened, which is exactly the signal the desktop change needs.

const (
	// The X-Machine-Id header is validated as a UUID by request middleware, so
	// the lock below has to send a well-formed one.
	continuationMachineID = "2415a11c-0000-4000-8000-00000000c0de"
	headerMachineID       = "X-Machine-Id"
)

// seedRefreshToken inserts a pre-existing live session for userID and returns
// its id. remember_me is set FALSE explicitly only so a seeded row is visibly
// distinct from the continuation row the handler mints. It does NOT make the
// remember_me assertion in TestChangePasswordContinuationRowShape
// discriminating — that assertion reads the CONTINUATION row, never these —
// and no test-side seeding can; see the note at the assertion itself.
func seedRefreshToken(t *testing.T, ts *testhelpers.TestServer, userID, label string) string {
	t.Helper()
	var id string
	require.NoError(t, ts.DB.QueryRow(
		`INSERT INTO refresh_tokens (user_id, token_hash, device_name, expires_at, remember_me, machine_id)
		 VALUES ($1, $2, $3, NOW() + INTERVAL '30 days', FALSE, NULL)
		 RETURNING id`,
		userID, "seed-hash-"+userID+"-"+label, label,
	).Scan(&id))
	return id
}

// liveRefreshTokenIDs returns userID's unrevoked refresh-token rows, oldest id
// first. Both continuation-row locks assert on it: a destructive flow's
// survivor set must be exactly the continuation row it handed back.
func liveRefreshTokenIDs(t *testing.T, ts *testhelpers.TestServer, userID string) []string {
	t.Helper()
	rows, err := ts.DB.Query(
		`SELECT id FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL ORDER BY id`, userID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var liveIDs []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		liveIDs = append(liveIDs, id)
	}
	require.NoError(t, rows.Err())
	return liveIDs
}

// (a) #2415: the continuation row ChangePassword hands back is a real,
// machine-bound, remember-me session — not a degraded stub — and it is the ONLY
// thing that survives: every session the user held before the change is
// revoked. The desktop client persists this pair as the device's session, so
// machine_id (copied from the request headers at internal/auth/handlers.go:221)
// and remember_me = true (hardcoded, :223) are both load-bearing, though only
// machine_id is lockable by a test — see the note on the remember_me assertion.
func TestChangePasswordContinuationRowShape(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "controwshape")

	priorSessions := []string{
		seedRefreshToken(t, ts, user.ID, "prior-a"),
		seedRefreshToken(t, ts, user.ID, "prior-b"),
	}

	headers := testhelpers.AuthHeaders(user.AccessToken)
	headers.Set(headerMachineID, continuationMachineID)

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		keyCurrentPassword:   user.Password,
		keyNewPassword:       "ContinuationPass456!",
		keyWrappedPrivateKey: wrappedKey,
		keyKeyDerivationSalt: salt,
	}, headers)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	sessionID, ok := body["session_id"].(string)
	require.True(t, ok, "ChangePassword 200 must carry a continuation session_id")
	require.NotEmpty(t, sessionID)
	require.NotEmpty(t, body["access_token"], "continuation access_token expected")
	require.NotEmpty(t, body["refresh_token"], "continuation refresh_token expected")

	var machineID sql.NullString
	var rememberMe bool
	var revokedAt sql.NullTime
	require.NoError(t, ts.DB.QueryRow(
		`SELECT machine_id, remember_me, revoked_at FROM refresh_tokens WHERE id = $1`, sessionID,
	).Scan(&machineID, &rememberMe, &revokedAt))

	assert.True(t, machineID.Valid, "the continuation row must copy X-Machine-Id from the request")
	assert.Equal(t, continuationMachineID, machineID.String)
	// Documented as a contract, NOT locked: remember_me is NOT NULL DEFAULT TRUE
	// (migrations/000012_add_remember_me.up.sql:2), so a continuation row minted
	// without the hardcoded true at internal/auth/handlers.go:223 still reads
	// back true here. No test-side seeding changes that — production code
	// inserts the row, and the column default is exactly what a dropped literal
	// falls through to. Kept because it states the shape the desktop client
	// adopts; the literal itself is guarded by review, not by this line.
	assert.True(t, rememberMe, "the continuation row is minted with remember_me = true")
	assert.False(t, revokedAt.Valid, "the continuation row must itself be live")

	for _, priorID := range priorSessions {
		var priorRevoked sql.NullTime
		require.NoError(t, ts.DB.QueryRow(
			`SELECT revoked_at FROM refresh_tokens WHERE id = $1`, priorID,
		).Scan(&priorRevoked))
		assert.True(t, priorRevoked.Valid,
			"every pre-change session must be revoked; %s survived", priorID)
	}

	// And the continuation row is not merely live, it is the ONLY live row. The
	// desktop client's "adopting this pair means this device owns the only
	// session" assumption has to hold for BOTH destructive flows, not just for
	// the key reset locked below.
	liveIDs := liveRefreshTokenIDs(t, ts, user.ID)
	require.Len(t, liveIDs, 1, "the password change must leave exactly one live refresh row")
	assert.Equal(t, sessionID, liveIDs[0],
		"the single surviving row must be the continuation row returned in the response body")
}

// (b) #2415, the highest-value lock: ReplaceMyKeys revokes ALL refresh tokens
// (internal/users/handlers.go:447) and the continuation row it returns is the
// single survivor. The desktop Login ordering fix rests entirely on this — if a
// second live row could survive the reset, adopting the returned pair would no
// longer be equivalent to "this device now owns the only session."
func TestReplaceMyKeysLeavesExactlyOneLiveRefreshRow(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replonelive")
	peer := ts.CreateTestUser(t, "replonelivepeer")

	for _, label := range []string{"live-a", "live-b", "live-c"} {
		seedRefreshToken(t, ts, user.ID, label)
	}
	// An already-revoked row proves the survivor count is measured by
	// revoked_at and not by row count.
	alreadyRevoked := seedRefreshToken(t, ts, user.ID, "already-revoked")
	_, err := ts.DB.Exec(`UPDATE refresh_tokens SET revoked_at = NOW() WHERE id = $1`, alreadyRevoked)
	require.NoError(t, err)
	// A different user's live session must be untouched — the sweep is
	// user-scoped, not global.
	peerSession := seedRefreshToken(t, ts, peer.ID, "peer-live")

	publicKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("PUT", urlUsersMeKeys, map[string]interface{}{
		keyWrappedPrivateKey:    wrappedKey,
		keyKeyDerivationSalt:    salt,
		"key_derivation_alg":    "argon2id",
		"public_key":            publicKey,
		"acknowledge_data_loss": true,
		keyCurrentPassword:      user.Password,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	sessionID, ok := body["session_id"].(string)
	require.True(t, ok, "ReplaceMyKeys 200 must carry a continuation session_id")
	require.NotEmpty(t, sessionID)

	liveIDs := liveRefreshTokenIDs(t, ts, user.ID)
	require.Len(t, liveIDs, 1, "the key reset must leave exactly one live refresh row")
	assert.Equal(t, sessionID, liveIDs[0],
		"the single surviving row must be the continuation row returned in the response body")

	var peerRevoked sql.NullTime
	require.NoError(t, ts.DB.QueryRow(
		`SELECT revoked_at FROM refresh_tokens WHERE id = $1`, peerSession,
	).Scan(&peerRevoked))
	assert.False(t, peerRevoked.Valid, "another user's session must survive the reset")
}

// evalFailingRedisHook fails only the Lua-script commands, letting plain
// key commands through. credepoch's Begin publishes the blocked marker with a
// plain SET while Op.Commit finalizes it with an owner-scoped CAS run as a
// script (internal/credepoch/credepoch.go:315-329), so this reproduces the
// spec §4.3 window — a Redis fault confined to the commit CAS — without
// touching production code.
type evalFailingRedisHook struct{}

func (evalFailingRedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (evalFailingRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		switch strings.ToLower(cmd.Name()) {
		case "eval", "evalsha":
			err := errors.New("simulated redis transport failure")
			cmd.SetErr(err)
			return err
		default:
			return next(ctx, cmd)
		}
	}
}

func (evalFailingRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// (c) #2415 / spec §4.3: Op.Commit retries its owner-scoped CAS exactly once
// and then gives up, so a Redis fault confined to that window leaves
// blocked:<opID> live for up to blockedTTL WHILE the handler still returns 200
// carrying a structurally valid continuation pair. The lock is the negative
// claim the desktop client must respect: the presence of access_token /
// refresh_token / session_id is a CONTRACT guarantee, not a LIVENESS guarantee
// — the very next request made with that pair can be refused by the fence.
func TestChangePasswordContinuationPairIsNotALivenessGuarantee(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "contnotlive")

	// One miniredis, two clients: the fence the handler drives has its commit
	// CAS broken; the reader observes the same keyspace intact.
	mr := miniredis.RunT(t)
	handlerRedis := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	handlerRedis.AddHook(evalFailingRedisHook{})
	readerRedis := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = handlerRedis.Close()
		_ = readerRedis.Close()
	})

	log := logger.NewWithWriter(io.Discard)
	issuer := auth.NewHandler(ts.DB, ts.Redis, log, testhelpers.TestJWTSecret, nil)
	h := users.NewHandler(ts.DB, log, nil, nil, nil,
		credepoch.New(ts.DB, handlerRedis, log), issuer)

	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	c, w := newFenceMissingContext(t, user.ID, map[string]interface{}{
		keyCurrentPassword:   user.Password,
		keyNewPassword:       "NotLivenessPass456!",
		keyWrappedPrivateKey: wrappedKey,
		keyKeyDerivationSalt: salt,
	})
	h.ChangePassword(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	accessToken, ok := body["access_token"].(string)
	require.True(t, ok, "the committed change must still carry a continuation pair")
	require.NotEmpty(t, accessToken)
	require.NotEmpty(t, body["refresh_token"])
	require.NotEmpty(t, body["session_id"])

	// The blocked marker outlived the successful mint.
	marker, err := mr.Get(credepoch.Key(user.ID))
	require.NoError(t, err, "the fence key must still exist after the failed commit CAS")
	assert.True(t, strings.HasPrefix(marker, "blocked:"),
		"a failed commit CAS leaves the blocked marker live; got a %q-shaped value", marker)

	// And it is authoritative: an independent reader over the same keyspace
	// refuses the epoch the just-minted pair carries.
	var committedEpoch string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT credential_epoch FROM users WHERE id = $1`, user.ID).Scan(&committedEpoch))
	require.NotEmpty(t, committedEpoch, "the destructive flow must have rotated the epoch")

	readerFence := credepoch.New(ts.DB, readerRedis, log)
	assert.ErrorIs(t,
		readerFence.Check(context.Background(), user.ID, committedEpoch),
		credepoch.ErrBlocked,
		"a live blocked marker must reject even the epoch this flow just committed")
}

// bodyWatchingDisconnector samples how much response body had been written at
// the moment DisconnectUser was called. Ordering is the whole point of the
// assertion: a client must not be able to observe the 200 before its sockets
// have been severed.
type bodyWatchingDisconnector struct {
	recorder      *httptest.ResponseRecorder
	bodyLenAtCall []int
}

func (d *bodyWatchingDisconnector) DisconnectUser(uuid.UUID) {
	d.bodyLenAtCall = append(d.bodyLenAtCall, d.recorder.Body.Len())
}

// (d) #2415: BOTH destructive flows enqueue the forced socket teardown BEFORE
// the response body is written — ChangePassword directly
// (internal/users/handlers.go:1576-1578) and ReplaceMyKeys through
// completeKeyReplacementClear (:492-494). The desktop client treats the 200 as
// the point at which its old sockets are already gone; if the body could be
// observed first, that assumption would be wrong.
//
// Both subtests take the committed path, so ForcedClearCompletion.Outcome is a
// durable one and RequiresDisconnect() (internal/presencehistory/forced_clear.go)
// is true — it is FALSE for ForcedClearRolledBack. A "must disconnect exactly
// once" failure below can therefore mean the flow rolled back and never
// disconnected at all, not that its ordering changed.
func TestDestructiveFlowsDisconnectBeforeResponseBody(t *testing.T) {
	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	publicKey, replaceWrapped, replaceSalt := testhelpers.E2EETestKeys()

	t.Run("ChangePassword severs sockets before writing its 200", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "disconnectorderpw")
		h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil,
			testCredFence(t, ts.DB), nil)

		c, w := newFenceMissingContext(t, user.ID, map[string]interface{}{
			keyCurrentPassword:   user.Password,
			keyNewPassword:       "DisconnectOrder456!",
			keyWrappedPrivateKey: wrappedKey,
			keyKeyDerivationSalt: salt,
		})
		observer := &bodyWatchingDisconnector{recorder: w}
		users.SetKeyResetSessionDisconnectorForTest(h, observer)

		h.ChangePassword(c)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Len(t, observer.bodyLenAtCall, 1, "the flow must disconnect exactly once")
		assert.Zero(t, observer.bodyLenAtCall[0],
			"no response body may have been written when DisconnectUser fired")
		assert.NotZero(t, w.Body.Len(), "the body is written after the disconnect, not never")
	})

	t.Run("ReplaceMyKeys severs sockets before writing its 200", func(t *testing.T) {
		ts := setupTS(t)
		user := ts.CreateTestUser(t, "disconnectorderkeys")
		h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil,
			testCredFence(t, ts.DB), nil)
		h.SetPresenceHistory(ts.PresenceHistory)

		c, w := newFenceMissingContext(t, user.ID, map[string]interface{}{
			keyCurrentPassword:      user.Password,
			keyWrappedPrivateKey:    replaceWrapped,
			keyKeyDerivationSalt:    replaceSalt,
			"key_derivation_alg":    "argon2id",
			"public_key":            publicKey,
			"acknowledge_data_loss": true,
		})
		c.Request.Method = http.MethodPut
		observer := &bodyWatchingDisconnector{recorder: w}
		users.SetKeyResetSessionDisconnectorForTest(h, observer)

		h.ReplaceMyKeys(c)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Len(t, observer.bodyLenAtCall, 1, "the flow must disconnect exactly once")
		assert.Zero(t, observer.bodyLenAtCall[0],
			"no response body may have been written when DisconnectUser fired")
		assert.NotZero(t, w.Body.Len(), "the body is written after the disconnect, not never")
	})
}
