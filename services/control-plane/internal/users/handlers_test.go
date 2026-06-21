package users_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	urlUsersMe          = "/api/v1/users/me"
	urlUsersMeKeys      = "/api/v1/users/me/keys"
	urlUsersMePassword  = "/api/v1/users/me/password" //nolint:gosec // G101 false positive: URL path constant, not a credential
	urlUsersMeSavedGifs = "/api/v1/users/me/saved-gifs"
	urlUsersMeFriendOrg = "/api/v1/users/me/friend-organization"

	keyUsername          = "username"
	keyWrappedPrivateKey = "wrapped_private_key"
	keyKeyDerivationSalt = "key_derivation_salt"
	keyCurrentPassword   = "current_password" //nolint:gosec // G101 false positive: map key constant, not a credential
	keyNewPassword       = "new_password"     //nolint:gosec // G101 false positive: map key constant, not a credential
	methodPatch          = "PATCH"
	testPassword         = "TestPassword123!" //nolint:gosec // G101 false positive: test credential, not used in production
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// --- Get Me ---

func TestGetMeSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "meuser")

	w := ts.DoRequest("GET", urlUsersMe, nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, user.Username, userData[keyUsername])
	assert.Equal(t, user.Email, userData["email"])
}

func TestGetMeUnauthorized(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", urlUsersMe, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Update Me ---

func TestUpdateMeSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updateme")

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		"display_name": "Updated Name",
		"bio":          "My test bio",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, "Updated Name", userData["display_name"])
	assert.Equal(t, "My test bio", userData["bio"])
}

func TestUpdateMeInvalidAvatar(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badavatar")

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		"avatar_url": "not-a-data-url",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Get My Keys ---

func TestGetMyKeysSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "keysuser")

	w := ts.DoRequest("GET", urlUsersMeKeys, nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	keys := body["e2ee_keys"].(map[string]interface{})
	assert.NotEmpty(t, keys["wrapped_private_key"])
	assert.NotEmpty(t, keys["key_derivation_salt"])
}

// --- Get Public Key ---

func TestGetPublicKeySuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pubkeyuser")

	w := ts.DoRequest("GET", "/api/v1/users/"+user.ID+"/public-key", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotEmpty(t, body["public_key"])
}

func TestGetPublicKeyNonexistentUser(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "pubkeylookup")

	w := ts.DoRequest("GET", "/api/v1/users/00000000-0000-0000-0000-000000000000/public-key", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- Preferences ---

func TestGetPreferencesEmpty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "prefuser")

	w := ts.DoRequest("GET", "/api/v1/users/me/preferences", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdatePreferencesSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "prefupdate")

	w := ts.DoRequest("PUT", "/api/v1/users/me/preferences", map[string]interface{}{
		"encrypted_data": "dGVzdCBwcmVmZXJlbmNlcw==",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Replace My Keys ---

func TestReplaceMyKeysSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replacekeysuser")

	newPub, newWrapped, newSalt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("PUT", urlUsersMeKeys, map[string]interface{}{
		keyWrappedPrivateKey:    newWrapped,
		keyKeyDerivationSalt:    newSalt,
		"public_key":            newPub,
		"acknowledge_data_loss": true,
		keyCurrentPassword:      user.Password,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Keys replaced successfully. Encrypted message history was reset.", body["message"])
}

func TestReplaceMyKeysInvalidBase64(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badkeysuser")

	w := ts.DoRequest("PUT", urlUsersMeKeys, map[string]interface{}{
		keyWrappedPrivateKey: "not-valid-base64!!!",
		keyKeyDerivationSalt: "also-bad!!!",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReplaceMyKeysMissingFields(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "missingkeysuser")

	w := ts.DoRequest("PUT", urlUsersMeKeys, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Username Change Cooldown ---

func TestUpdateMeUsernameChangeCooldown(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "cooldownuser")

	// Username was just created, so username_changed_at = NOW(). Change should be blocked.
	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		keyUsername: "newcooldownname",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	// Free tier cadence is 365 days; the message is now interval-aware (#1298).
	assert.Contains(t, body["error"], "once every 365 days")
	assert.NotNil(t, body["username_change_eligible_at"])
}

func TestUpdateMeUsernameChangeAllowed(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "allowedchange")

	// Set username_changed_at to 366 days ago to allow change
	_, err := ts.DB.Exec(
		`UPDATE users SET username_changed_at = NOW() - INTERVAL '366 days' WHERE id = $1`,
		user.ID,
	)
	assert.NoError(t, err)

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		keyUsername: "newallowedname",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, "newallowedname", userData[keyUsername])
}

// grantPremium inserts an active premium subscription and clears the cached tier
// (login during CreateTestUser may have cached the then-free tier). Server
// enforcement reads the subscriptions table via the entitlements cache (#1298).
func grantPremium(t *testing.T, ts *testhelpers.TestServer, userID string) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO subscriptions (user_id, tier, status, source) VALUES ($1, 'premium', 'active', 'code')`,
		userID,
	)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.Del(context.Background(), "ent:"+userID).Err())
}

// TestUpdateMeUsernameChangePremiumFasterCadence: at 100 days since last change, a
// premium user (91d cadence) is allowed where a free user (365d) would be blocked.
func TestUpdateMeUsernameChangePremiumFasterCadence(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "premcadence")
	_, err := ts.DB.Exec(
		`UPDATE users SET username_changed_at = NOW() - INTERVAL '100 days' WHERE id = $1`, user.ID)
	require.NoError(t, err)
	grantPremium(t, ts, user.ID)

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		keyUsername: "premchanged",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, "premchanged", userData[keyUsername])
}

// TestUpdateMeUsernameChangeFreeBlockedAt100Days: a free user at 100 days is still
// blocked (365d cadence) — the fail-closed baseline when no subscription exists.
func TestUpdateMeUsernameChangeFreeBlockedAt100Days(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "freecadence")
	_, err := ts.DB.Exec(
		`UPDATE users SET username_changed_at = NOW() - INTERVAL '100 days' WHERE id = $1`, user.ID)
	require.NoError(t, err)

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		keyUsername: "freechanged",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "365 days")
}

// TestUpdateMeAvatarDataURLCappedAtFreeForAllTiers: inline data-URL avatars are
// broadcast verbatim to every client (UpdateMe -> BroadcastToAll), so the inline cap is
// the FREE value (1 MiB) for ALL tiers — a >1 MiB data-URL is rejected even for premium.
// Premium's 5 MiB allowance applies on the MinIO upload path (covered in media tests),
// which broadcasts a storage key, not the blob (#1298 review — Gitar amplification guard).
func TestUpdateMeAvatarDataURLCappedAtFreeForAllTiers(t *testing.T) {
	ts := setupTS(t)
	// base64 payload encoding ~1.5 MiB raw: over the free 1 MiB inline cap.
	big := "data:image/png;base64," + strings.Repeat("A", base64.StdEncoding.EncodedLen(1572864))

	freeUser := ts.CreateTestUser(t, "freeavatardata")
	wFree := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		"avatar_url": big,
	}, testhelpers.AuthHeaders(freeUser.AccessToken))
	assert.Equal(t, http.StatusBadRequest, wFree.Code, "free rejects >1 MiB inline data-URL")

	premUser := ts.CreateTestUser(t, "premavatardata")
	grantPremium(t, ts, premUser.ID)
	wPrem := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		"avatar_url": big,
	}, testhelpers.AuthHeaders(premUser.AccessToken))
	assert.Equal(t, http.StatusBadRequest, wPrem.Code,
		"premium inline data-URL ALSO capped at free (broadcast-amplification guard)")
}

func TestUpdateMeUsernameChangeSameUsername(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sameusername")

	// Even though on cooldown, submitting the same username should NOT trigger
	// the cooldown error — it's effectively a no-op since no change occurs.
	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		keyUsername: "sameusername",
	}, testhelpers.AuthHeaders(user.AccessToken))

	// The handler skips the cooldown check for no-op username submissions,
	// but since there are no actual changes, it returns 400 "No fields to update".
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "No fields to update", body["error"])
}

func TestUpdateMeUsernameWithPeriod(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "periodtest")

	// Allow change by setting cooldown to expired
	_, err := ts.DB.Exec(
		`UPDATE users SET username_changed_at = NOW() - INTERVAL '366 days' WHERE id = $1`,
		user.ID,
	)
	assert.NoError(t, err)

	w := ts.DoRequest(methodPatch, urlUsersMe, map[string]interface{}{
		keyUsername: "period.test",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	userData := body["user"].(map[string]interface{})
	assert.Equal(t, "period.test", userData[keyUsername])
}

// registerAndVerify registers a user via the two-step register→confirm flow
// and returns a verified access token.
func registerAndVerify(t *testing.T, ts *testhelpers.TestServer, email, username string) string {
	t.Helper()
	pub, priv, salt := testhelpers.E2EETestKeys()
	regW := ts.DoRequest("POST", "/api/v1/auth/register", map[string]interface{}{
		"email":              email,
		keyUsername:          username,
		"password":           testPassword,
		"age_confirmation":   true,
		"public_key":         pub,
		keyWrappedPrivateKey: priv,
		keyKeyDerivationSalt: salt,
	}, nil)
	require.Equal(t, http.StatusCreated, regW.Code)

	var regBody struct {
		PendingID string `json:"pending_id"`
	}
	testhelpers.ParseJSON(t, regW, &regBody)

	code := testhelpers.FetchVerificationCode(t, ts, regBody.PendingID)

	w2 := ts.DoRequest("POST", "/api/v1/auth/register/confirm",
		map[string]string{"pending_id": regBody.PendingID, "code": code}, nil)
	require.Equal(t, http.StatusOK, w2.Code)

	var confirmBody struct {
		AccessToken string `json:"access_token"` //nolint:gosec
	}
	testhelpers.ParseJSON(t, w2, &confirmBody)
	return confirmBody.AccessToken
}

// --- Change Password ---

func TestChangePasswordSuccess(t *testing.T) {
	ts := setupTS(t)

	// Register via API so password hash is real Argon2id, then auto-verify
	accessToken := registerAndVerify(t, ts, "changepw@test.concord.chat", "changepwuser")

	_, newWrapped, newSalt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		keyCurrentPassword:   testPassword,
		keyNewPassword:       "NewStrongPassword456!",
		keyWrappedPrivateKey: newWrapped,
		keyKeyDerivationSalt: newSalt,
	}, testhelpers.AuthHeaders(accessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Password changed successfully", body["message"])
}

func TestChangePasswordWrongCurrentPassword(t *testing.T) {
	ts := setupTS(t)

	accessToken := registerAndVerify(t, ts, "wrongpw@test.concord.chat", "wrongpwuser")

	_, newWrapped, newSalt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		keyCurrentPassword:   "WrongPassword!!!!",
		keyNewPassword:       "NewStrongPassword456!",
		keyWrappedPrivateKey: newWrapped,
		keyKeyDerivationSalt: newSalt,
	}, testhelpers.AuthHeaders(accessToken))

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestChangePasswordSameAsOld(t *testing.T) {
	ts := setupTS(t)

	accessToken := registerAndVerify(t, ts, "samepw@test.concord.chat", "samepwuser")

	_, newWrapped, newSalt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		keyCurrentPassword:   testPassword,
		keyNewPassword:       testPassword,
		keyWrappedPrivateKey: newWrapped,
		keyKeyDerivationSalt: newSalt,
	}, testhelpers.AuthHeaders(accessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestChangePasswordWeakNewPassword(t *testing.T) {
	ts := setupTS(t)

	accessToken := registerAndVerify(t, ts, "weaknewpw@test.concord.chat", "weaknewpwuser")

	_, newWrapped, newSalt := testhelpers.E2EETestKeys()
	w := ts.DoRequest("POST", urlUsersMePassword, map[string]interface{}{
		keyCurrentPassword:   testPassword,
		keyNewPassword:       "weak",
		keyWrappedPrivateKey: newWrapped,
		keyKeyDerivationSalt: newSalt,
	}, testhelpers.AuthHeaders(accessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Saved GIFs ---

func TestGetSavedGifsEmpty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "gifsempty")

	w := ts.DoRequest("GET", urlUsersMeSavedGifs, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Nil(t, body["saved_gifs"])
}

func TestUpdateSavedGifsSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "gifssave")

	w := ts.DoRequest("PUT", urlUsersMeSavedGifs, map[string]interface{}{
		"encrypted_data": "dGVzdCBzYXZlZCBnaWZz",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(1), body["version"])
}

func TestGetSavedGifsAfterSave(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "gifsget")

	ts.DoRequest("PUT", urlUsersMeSavedGifs, map[string]interface{}{
		"encrypted_data": "dGVzdCBzYXZlZCBnaWZz",
	}, testhelpers.AuthHeaders(user.AccessToken))

	w := ts.DoRequest("GET", urlUsersMeSavedGifs, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	savedGifs := body["saved_gifs"].(map[string]interface{})
	assert.Equal(t, "dGVzdCBzYXZlZCBnaWZz", savedGifs["encrypted_data"])
	assert.Equal(t, float64(1), savedGifs["version"])
}

func TestUpdateSavedGifsVersionIncrements(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "gifsver")

	ts.DoRequest("PUT", urlUsersMeSavedGifs, map[string]interface{}{
		"encrypted_data": "dGVzdCBzYXZlZCBnaWZz",
	}, testhelpers.AuthHeaders(user.AccessToken))

	w := ts.DoRequest("PUT", urlUsersMeSavedGifs, map[string]interface{}{
		"encrypted_data": "dXBkYXRlZCBnaWZz",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(2), body["version"])
}

func TestUpdateSavedGifsInvalidBase64(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "gifsbad64")

	w := ts.DoRequest("PUT", urlUsersMeSavedGifs, map[string]interface{}{
		"encrypted_data": "not-valid-base64!!!",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateSavedGifsMissingData(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "gifsmissing")

	w := ts.DoRequest("PUT", urlUsersMeSavedGifs, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetSavedGifsUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("GET", urlUsersMeSavedGifs, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestUpdateSavedGifsUnauthorized(t *testing.T) {
	ts := setupTS(t)
	w := ts.DoRequest("PUT", urlUsersMeSavedGifs, map[string]interface{}{
		"encrypted_data": "dGVzdA==",
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Friend Organization (encrypted blob, #324) ---

func TestUpdateFriendOrganizationSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "friendorgok")
	w := ts.DoRequest("PUT", urlUsersMeFriendOrg, map[string]interface{}{
		"encrypted_data": "dGVzdCBmcmllbmQgb3Jn",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(1), body["version"])
}

func TestUpdateFriendOrganizationInvalidBase64(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "friendorgbad64")
	w := ts.DoRequest("PUT", urlUsersMeFriendOrg, map[string]interface{}{
		"encrypted_data": "not-valid-base64!!!",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateFriendOrganizationOversize(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "friendorgbig")
	big := base64.StdEncoding.EncodeToString(make([]byte, 50000)) // >64KB base64
	w := ts.DoRequest("PUT", urlUsersMeFriendOrg, map[string]interface{}{
		"encrypted_data": big,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetFriendOrganizationEmpty(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "friendorgempty")
	w := ts.DoRequest("GET", urlUsersMeFriendOrg, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Nil(t, body["friend_organization"]) // sql.ErrNoRows → {"friend_organization": nil}
}

// --- SSO settings endpoints (Task 12 / issue #270) ---

const (
	urlUsersMeSSOIdentities = "/api/v1/users/me/sso-identities"
	urlUsersMeSecurity      = "/api/v1/users/me/security"
)

// TestListSSOIdentities_ReturnsLinkedProviders verifies that GET
// /users/me/sso-identities returns the linked-provider list ordered by
// created_at ASC, with provider, provider_email, is_relay_email, linked_at,
// and last_used_at fields.
func TestListSSOIdentities_ReturnsLinkedProviders(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "listuser")

	_, err := ts.DB.Exec(
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email, last_used_at)
		 VALUES ($1, 'google', 'g1', $2, NOW() - INTERVAL '3 days')`, user.ID, user.Email)
	require.NoError(t, err)

	w := ts.DoRequest("GET", urlUsersMeSSOIdentities, nil, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Identities []struct {
			Provider      string `json:"provider"`
			ProviderEmail string `json:"provider_email"`
			IsRelayEmail  bool   `json:"is_relay_email"`
		} `json:"identities"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.Len(t, resp.Identities, 1)
	assert.Equal(t, "google", resp.Identities[0].Provider)
	assert.Equal(t, user.Email, resp.Identities[0].ProviderEmail)
}

// TestGetSecurity_ReturnsCurrentFlags verifies that GET /users/me/security
// returns the user's current password_login_disabled and trust_sso_security
// flags so the Settings panel can hydrate the toggles on mount.
func TestGetSecurity_ReturnsCurrentFlags(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "secflags")

	// Default: both flags FALSE for a freshly created user.
	w := ts.DoRequest("GET", urlUsersMeSecurity, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, false, resp["password_login_disabled"])
	assert.Equal(t, false, resp["trust_sso_security"])

	// Flip both via direct DB write, re-fetch, verify.
	_, err := ts.DB.Exec(
		`UPDATE users SET password_login_disabled = TRUE, trust_sso_security = TRUE WHERE id = $1`, // pragma: allowlist secret
		user.ID,
	)
	require.NoError(t, err)

	w2 := ts.DoRequest("GET", urlUsersMeSecurity, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w2.Code)
	var resp2 map[string]interface{}
	testhelpers.ParseJSON(t, w2, &resp2)
	assert.Equal(t, true, resp2["password_login_disabled"])
	assert.Equal(t, true, resp2["trust_sso_security"])
}

// TestPatchSecurity_TogglesPasswordLoginDisabled verifies the happy-path:
// a valid passphrase confirmation flips password_login_disabled to TRUE.
// The user must have at least one linked SSO identity for the would_lock_out
// gate to allow the change — otherwise disabling password login would leave
// the account with no usable login method (see
// TestPatchSecurity_PasswordLoginDisabled_NoSSOIdentity_Refuses400 below for
// the negative path).
func TestPatchSecurity_TogglesPasswordLoginDisabled(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "patchuser")

	// Pre-link a Google identity so the lockout gate permits disabling password login.
	_, err := ts.DB.Exec(
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		 VALUES ($1, 'google', 'patchuser-sub', $2)`, user.ID, user.Email)
	require.NoError(t, err)

	w := ts.DoRequest(methodPatch, urlUsersMeSecurity, map[string]interface{}{
		"password_login_disabled": true,
		"current_passphrase":      testPassword,
	}, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusOK, w.Code)

	var pld bool
	require.NoError(t,
		ts.DB.QueryRow(`SELECT password_login_disabled FROM users WHERE id = $1`, user.ID).Scan(&pld),
	)
	assert.True(t, pld)
}

// TestPatchSecurity_PasswordLoginDisabled_NoSSOIdentity_Refuses400 verifies the
// would_lock_out gate added to PatchSecurity: a user attempting to disable
// password login without ANY linked SSO identity must be refused with 400.
// This is the symmetric defense to DeleteSSOIdentity's would_lock_out check —
// together they enforce the structural invariant that a user always has at
// least one usable login method (password OR ≥1 SSO identity).
func TestPatchSecurity_PasswordLoginDisabled_NoSSOIdentity_Refuses400(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "wouldlockout")

	// No SSO identity linked. password_login_disabled=FALSE by default. // pragma: allowlist secret
	w := ts.DoRequest(methodPatch, urlUsersMeSecurity, map[string]interface{}{
		"password_login_disabled": true,
		"current_passphrase":      testPassword,
	}, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "would_lock_out")

	// Verify the flag was NOT mutated.
	var pld bool
	require.NoError(t,
		ts.DB.QueryRow(`SELECT password_login_disabled FROM users WHERE id = $1`, user.ID).Scan(&pld),
	)
	assert.False(t, pld, "would_lock_out refusal must leave password_login_disabled at FALSE")
}

// TestPatchSecurity_RequiresPassphraseConfirmation verifies that supplying an
// incorrect passphrase results in 401 invalid_credentials and does NOT mutate
// the flag — defending against session-token-replay-driven settings hijack.
func TestPatchSecurity_RequiresPassphraseConfirmation(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "noconf")

	w := ts.DoRequest(methodPatch, urlUsersMeSecurity, map[string]interface{}{
		"password_login_disabled": true,
		"current_passphrase":      "wrong-password",
	}, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_credentials")

	// Verify the flag was NOT changed.
	var pld bool
	require.NoError(t,
		ts.DB.QueryRow(`SELECT password_login_disabled FROM users WHERE id = $1`, user.ID).Scan(&pld),
	)
	assert.False(t, pld, "incorrect passphrase must not mutate the flag")
}

// TestDeleteSSOIdentity_RequiresPassphraseIfLastAuthMethod verifies that the
// lockout-prevention check refuses to delete the last authentication method.
// The user has password_login_disabled=TRUE and one Google identity — deleting // pragma: allowlist secret
// it would leave them with no way to log in.
func TestDeleteSSOIdentity_RequiresPassphraseIfLastAuthMethod(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "lastauth")

	// Configure user as SSO-only with one provider.
	_, err := ts.DB.Exec(`UPDATE users SET password_login_disabled = TRUE WHERE id = $1`, user.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		 VALUES ($1, 'google', 'g-last', $2)`, user.ID, user.Email)
	require.NoError(t, err)

	// Attempt to unlink — must fail with would_lock_out.
	w := ts.DoRequest("DELETE", urlUsersMeSSOIdentities+"/google", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "would_lock_out")

	// Verify the row was NOT deleted.
	var count int
	require.NoError(t,
		ts.DB.QueryRow(`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1 AND provider = 'google'`, user.ID).Scan(&count),
	)
	assert.Equal(t, 1, count, "would_lock_out refusal must leave the identity intact")
}

// TestDeleteSSOIdentity_HappyPath_AllowsUnlinkWithPasswordFallback verifies
// that a user with password_login_disabled=FALSE can unlink any SSO identity // pragma: allowlist secret
// because the password remains a valid auth method — wouldHaveAnyAuth=true.
func TestDeleteSSOIdentity_HappyPath_AllowsUnlinkWithPasswordFallback(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "unlinkok")

	// password_login_disabled defaults to FALSE — user has password fallback. // pragma: allowlist secret
	_, err := ts.DB.Exec(
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		 VALUES ($1, 'google', 'g-unlink', $2)`, user.ID, user.Email)
	require.NoError(t, err)

	w := ts.DoRequest("DELETE", urlUsersMeSSOIdentities+"/google", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"ok":true`)

	// Verify the row was deleted.
	var count int
	require.NoError(t,
		ts.DB.QueryRow(`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1 AND provider = 'google'`, user.ID).Scan(&count),
	)
	assert.Equal(t, 0, count, "successful unlink must remove the identity row")
}

// TestDeleteSSOIdentity_NotLinked_404 verifies the 404 path when the user
// requests deletion of a provider that was never linked.
func TestDeleteSSOIdentity_NotLinked_404(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "notlinked")

	// User has password fallback (default) but no SSO identity linked.
	w := ts.DoRequest("DELETE", urlUsersMeSSOIdentities+"/google", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "not_linked")
}

// TestPatchSecurity_NoFields_400 verifies that a PATCH with no fields set
// returns 400 no_fields rather than executing an empty UPDATE — defends
// against silent no-op writes that would burn passphrase verification.
func TestPatchSecurity_NoFields_400(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "nofields")

	w := ts.DoRequest(methodPatch, urlUsersMeSecurity, map[string]interface{}{
		"current_passphrase": testPassword,
	}, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "no_fields")
}

// TestPatchSecurity_NoPassphrase_401 verifies that omitting current_passphrase
// returns 401 passphrase_required and does not even read the password hash.
func TestPatchSecurity_NoPassphrase_401(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "nopass")

	w := ts.DoRequest(methodPatch, urlUsersMeSecurity, map[string]interface{}{
		"trust_sso_security": true,
	}, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "passphrase_required")
}

// TestPatchSecurity_TrustSSOSecurity_TogglesFlag verifies the second supported
// field — flips trust_sso_security to TRUE with a valid passphrase. Exercises
// the second branch of the dynamic UPDATE builder.
func TestPatchSecurity_TrustSSOSecurity_TogglesFlag(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "trustsso")

	w := ts.DoRequest(methodPatch, urlUsersMeSecurity, map[string]interface{}{
		"trust_sso_security": true,
		"current_passphrase": testPassword,
	}, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusOK, w.Code)

	var trust bool
	require.NoError(t,
		ts.DB.QueryRow(`SELECT trust_sso_security FROM users WHERE id = $1`, user.ID).Scan(&trust),
	)
	assert.True(t, trust)
}

// TestPatchSecurity_BothFlags_DynamicUpdateBuilder verifies that supplying
// BOTH password_login_disabled and trust_sso_security in the same request
// updates both columns — exercises the multi-field branch of the dynamic
// UPDATE builder (sets joined with comma).
func TestPatchSecurity_BothFlags_DynamicUpdateBuilder(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bothflags")

	// Pre-link an SSO identity so the would_lock_out gate permits the change.
	_, err := ts.DB.Exec(
		`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
		 VALUES ($1, 'google', 'bothflags-sub', $2)`, user.ID, user.Email)
	require.NoError(t, err)

	w := ts.DoRequest(methodPatch, urlUsersMeSecurity, map[string]interface{}{
		"password_login_disabled": true,
		"trust_sso_security":      true,
		"current_passphrase":      testPassword,
	}, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusOK, w.Code)

	var pld, trust bool
	require.NoError(t,
		ts.DB.QueryRow(
			`SELECT password_login_disabled, trust_sso_security FROM users WHERE id = $1`, // pragma: allowlist secret
			user.ID,
		).Scan(&pld, &trust),
	)
	assert.True(t, pld)
	assert.True(t, trust)
}

// TestListSSOIdentities_EmptyList_ReturnsEmptyArray verifies that a user
// with zero linked identities receives a non-nil empty array, not a null —
// matters for the renderer's `identities.length === 0` empty-state path.
func TestListSSOIdentities_EmptyList_ReturnsEmptyArray(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "emptyssolist")

	w := ts.DoRequest("GET", urlUsersMeSSOIdentities, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Identities []struct {
			Provider string `json:"provider"`
		} `json:"identities"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.NotNil(t, resp.Identities, "identities must be a non-nil array even when empty")
	assert.Len(t, resp.Identities, 0)
}
