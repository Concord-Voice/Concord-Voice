package users_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const urlUsersMePresence = "/api/v1/users/me/presence-settings"

// ── GetPresenceSettings ──────────────────────────────────────────────────────

func TestGetPresenceSettingsDefaults(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presdefault")

	w := ts.DoRequest("GET", urlUsersMePresence, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(0), body["custom_text_tier"])
	assert.Nil(t, body["custom_text"])
	assert.Nil(t, body["custom_text_emoji"])
}

func TestGetPresenceSettingsReturnsPersistedRow(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "prespersisted")

	_, err := ts.DB.Exec(
		`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text, custom_text_emoji)
		 VALUES ($1, 2, 'Heads down', '🎧')`,
		user.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", urlUsersMePresence, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(2), body["custom_text_tier"])
	assert.Equal(t, "Heads down", body["custom_text"])
	assert.Equal(t, "🎧", body["custom_text_emoji"])
}

func TestGetPresenceSettingsUnauthorized(t *testing.T) {
	ts := setupTS(t)

	w := ts.DoRequest("GET", urlUsersMePresence, nil, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── UpdatePresenceSettings ───────────────────────────────────────────────────

func TestUpdatePresenceSettingsValidTierAndText(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presvalid")

	payload := map[string]interface{}{
		"custom_text_tier":  1,
		"custom_text":       "Out till Friday",
		"custom_text_emoji": "🌴",
	}
	w := ts.DoRequest(methodPatch, urlUsersMePresence, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(1), body["custom_text_tier"])
	assert.Equal(t, "Out till Friday", body["custom_text"])
	assert.Equal(t, "🌴", body["custom_text_emoji"])

	// Verify the row was actually persisted to the DB.
	var tier int
	var text, emoji *string
	err := ts.DB.QueryRow(
		`SELECT custom_text_tier, custom_text, custom_text_emoji
		 FROM user_presence_settings WHERE user_id = $1`,
		user.ID,
	).Scan(&tier, &text, &emoji)
	require.NoError(t, err)
	assert.Equal(t, 1, tier)
	require.NotNil(t, text)
	assert.Equal(t, "Out till Friday", *text)
	require.NotNil(t, emoji)
	assert.Equal(t, "🌴", *emoji)
}

func TestUpdatePresenceSettingsUpsertExistingRow(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presupsert")

	// First PATCH inserts; second PATCH exercises the ON CONFLICT UPDATE path.
	first := map[string]interface{}{"custom_text_tier": 1, "custom_text": "first"}
	w := ts.DoRequest(methodPatch, urlUsersMePresence, first, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	second := map[string]interface{}{"custom_text_tier": 2, "custom_text": "second"}
	w = ts.DoRequest(methodPatch, urlUsersMePresence, second, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(2), body["custom_text_tier"])
	assert.Equal(t, "second", body["custom_text"])
}

func TestUpdatePresenceSettingsInvalidTier(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "prestierbad")

	payload := map[string]interface{}{"custom_text_tier": 3}
	w := ts.DoRequest(methodPatch, urlUsersMePresence, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdatePresenceSettingsTextTooLong(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "prestextlong")

	payload := map[string]interface{}{"custom_text": strings.Repeat("a", 141)}
	w := ts.DoRequest(methodPatch, urlUsersMePresence, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdatePresenceSettingsEmojiTooLong(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presemojilong")

	payload := map[string]interface{}{"custom_text_emoji": strings.Repeat("x", 33)}
	w := ts.DoRequest(methodPatch, urlUsersMePresence, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdatePresenceSettingsEmptyTextClearsToNull(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presclear")

	// Seed a non-empty status first.
	_, err := ts.DB.Exec(
		`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text, custom_text_emoji)
		 VALUES ($1, 2, 'busy', '⛔')`,
		user.ID,
	)
	require.NoError(t, err)

	// Empty custom_text + empty emoji ⇒ clear both columns to NULL.
	payload := map[string]interface{}{"custom_text": "", "custom_text_emoji": ""}
	w := ts.DoRequest(methodPatch, urlUsersMePresence, payload, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Nil(t, body["custom_text"])
	assert.Nil(t, body["custom_text_emoji"])
	// Tier was not supplied, so it is unchanged.
	assert.Equal(t, float64(2), body["custom_text_tier"])

	// Confirm SQL NULL in the DB (not an empty string).
	var text, emoji *string
	err = ts.DB.QueryRow(
		`SELECT custom_text, custom_text_emoji FROM user_presence_settings WHERE user_id = $1`,
		user.ID,
	).Scan(&text, &emoji)
	require.NoError(t, err)
	assert.Nil(t, text)
	assert.Nil(t, emoji)
}

func TestUpdatePresenceSettingsNoFields(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presnofld")

	w := ts.DoRequest(methodPatch, urlUsersMePresence, map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdatePresenceSettingsInvalidBody(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presbadbody")

	w := ts.DoRequest(methodPatch, urlUsersMePresence, "not json", testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
