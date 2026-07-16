package users_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const urlUsersMePresence = "/api/v1/users/me/presence-settings"

var presenceSettingsJSONFields = []string{
	"master_enabled",
	"server_voice_tier",
	"server_voice_show_details",
	"private_call_tier",
	"private_call_show_details",
	"custom_text_tier",
	"custom_text",
	"custom_text_emoji",
}

func requirePresenceSettingsBody(t *testing.T, w *httptest.ResponseRecorder, expected map[string]interface{}) {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code)
	raw := w.Body.String()

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	require.Len(t, body, len(presenceSettingsJSONFields))
	for field, value := range expected {
		assert.Equal(t, value, body[field], field)
	}

	previous := -1
	for _, field := range presenceSettingsJSONFields {
		index := strings.Index(raw, `"`+field+`":`)
		require.Greater(t, index, previous, "response field order: %s", field)
		previous = index
	}
}

func rawPresenceSettingsPatch(
	t *testing.T,
	ts *testhelpers.TestServer,
	accessToken string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, urlUsersMePresence, strings.NewReader(body))
	req.Header = testhelpers.AuthHeaders(accessToken)
	w := httptest.NewRecorder()
	ts.Router.ServeHTTP(w, req)
	return w
}

// ── GetPresenceSettings ──────────────────────────────────────────────────────

func TestGetPresenceSettingsDefaults(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presdefault")

	w := ts.DoRequest("GET", urlUsersMePresence, nil, testhelpers.AuthHeaders(user.AccessToken))
	requirePresenceSettingsBody(t, w, map[string]interface{}{
		"master_enabled":            true,
		"server_voice_tier":         float64(1),
		"server_voice_show_details": true,
		"private_call_tier":         float64(0),
		"private_call_show_details": false,
		"custom_text_tier":          float64(0),
		"custom_text":               nil,
		"custom_text_emoji":         nil,
	})

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM user_presence_settings WHERE user_id = $1`, user.ID,
	).Scan(&count))
	assert.Zero(t, count, "GET defaults must remain virtual")
}

func TestGetPresenceSettingsReturnsPersistedRow(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "prespersisted")

	_, err := ts.DB.Exec(
		`INSERT INTO user_presence_settings (
			user_id, master_enabled, server_voice_tier, server_voice_show_details,
			private_call_tier, private_call_show_details,
			custom_text_tier, custom_text, custom_text_emoji
		) VALUES ($1, FALSE, 2, FALSE, 1, TRUE, 2, 'Heads down', '🎧')`,
		user.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("GET", urlUsersMePresence, nil, testhelpers.AuthHeaders(user.AccessToken))
	requirePresenceSettingsBody(t, w, map[string]interface{}{
		"master_enabled":            false,
		"server_voice_tier":         float64(2),
		"server_voice_show_details": false,
		"private_call_tier":         float64(1),
		"private_call_show_details": true,
		"custom_text_tier":          float64(2),
		"custom_text":               "Heads down",
		"custom_text_emoji":         "🎧",
	})
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

func TestUpdatePresenceSettingsPersistsExplicitFalseAndZero(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presexplicit")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings (
			user_id, master_enabled, server_voice_tier, server_voice_show_details,
			private_call_tier, private_call_show_details, custom_text_tier, custom_text
		) VALUES ($1, TRUE, 2, TRUE, 2, TRUE, 0, 'keep')
	`, user.ID)
	require.NoError(t, err)

	w := ts.DoRequest(methodPatch, urlUsersMePresence, map[string]interface{}{
		"master_enabled":            false,
		"server_voice_tier":         0,
		"server_voice_show_details": false,
		"private_call_tier":         0,
		"private_call_show_details": false,
	}, testhelpers.AuthHeaders(user.AccessToken))
	requirePresenceSettingsBody(t, w, map[string]interface{}{
		"master_enabled":            false,
		"server_voice_tier":         float64(0),
		"server_voice_show_details": false,
		"private_call_tier":         float64(0),
		"private_call_show_details": false,
		"custom_text_tier":          float64(0),
		"custom_text":               "keep",
		"custom_text_emoji":         nil,
	})

	var master, serverDetails, privateDetails bool
	var serverTier, privateTier int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT master_enabled, server_voice_tier, server_voice_show_details,
		       private_call_tier, private_call_show_details
		FROM user_presence_settings WHERE user_id = $1
	`, user.ID).Scan(&master, &serverTier, &serverDetails, &privateTier, &privateDetails))
	assert.False(t, master)
	assert.Zero(t, serverTier)
	assert.False(t, serverDetails)
	assert.Zero(t, privateTier)
	assert.False(t, privateDetails)
}

func TestUpdatePresenceSettingsBindsCategoryFieldsIndependently(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presbinds")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings (
			user_id, master_enabled, server_voice_tier, server_voice_show_details,
			private_call_tier, private_call_show_details
		) VALUES ($1, TRUE, 2, FALSE, 0, TRUE)
	`, user.ID)
	require.NoError(t, err)

	expected := map[string]interface{}{
		"master_enabled":            true,
		"server_voice_tier":         float64(2),
		"server_voice_show_details": false,
		"private_call_tier":         float64(0),
		"private_call_show_details": true,
		"custom_text_tier":          float64(0),
		"custom_text":               nil,
		"custom_text_emoji":         nil,
	}
	steps := []struct {
		name  string
		field string
		value interface{}
		want  interface{}
	}{
		{name: "server tier", field: "server_voice_tier", value: 1, want: float64(1)},
		{name: "private tier", field: "private_call_tier", value: 2, want: float64(2)},
		{name: "server details", field: "server_voice_show_details", value: true, want: true},
		{name: "private details", field: "private_call_show_details", value: false, want: false},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			w := ts.DoRequest(methodPatch, urlUsersMePresence, map[string]interface{}{
				step.field: step.value,
			}, testhelpers.AuthHeaders(user.AccessToken))
			expected[step.field] = step.want
			requirePresenceSettingsBody(t, w, expected)
		})
	}

	var serverTier, privateTier int
	var serverDetails, privateDetails bool
	require.NoError(t, ts.DB.QueryRow(`
		SELECT server_voice_tier, server_voice_show_details,
		       private_call_tier, private_call_show_details
		FROM user_presence_settings WHERE user_id = $1
	`, user.ID).Scan(&serverTier, &serverDetails, &privateTier, &privateDetails))
	assert.Equal(t, 1, serverTier)
	assert.True(t, serverDetails)
	assert.Equal(t, 2, privateTier)
	assert.False(t, privateDetails)
}

func TestUpdatePresenceSettingsLegacyCustomTextOnly(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "preslegacy")

	w := ts.DoRequest(methodPatch, urlUsersMePresence, map[string]interface{}{
		"custom_text": "legacy client",
	}, testhelpers.AuthHeaders(user.AccessToken))
	requirePresenceSettingsBody(t, w, map[string]interface{}{
		"master_enabled":            true,
		"server_voice_tier":         float64(1),
		"server_voice_show_details": true,
		"private_call_tier":         float64(0),
		"private_call_show_details": false,
		"custom_text_tier":          float64(0),
		"custom_text":               "legacy client",
		"custom_text_emoji":         nil,
	})
}

func TestUpdatePresenceSettingsPreservesOmittedFields(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "presomitted")
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings (
			user_id, master_enabled, server_voice_tier, server_voice_show_details,
			private_call_tier, private_call_show_details,
			custom_text_tier, custom_text, custom_text_emoji
		) VALUES ($1, FALSE, 2, FALSE, 1, TRUE, 0, 'before', '💤')
	`, user.ID)
	require.NoError(t, err)

	w := ts.DoRequest(methodPatch, urlUsersMePresence, map[string]interface{}{
		"custom_text": "after",
	}, testhelpers.AuthHeaders(user.AccessToken))
	requirePresenceSettingsBody(t, w, map[string]interface{}{
		"master_enabled":            false,
		"server_voice_tier":         float64(2),
		"server_voice_show_details": false,
		"private_call_tier":         float64(1),
		"private_call_show_details": true,
		"custom_text_tier":          float64(0),
		"custom_text":               "after",
		"custom_text_emoji":         "💤",
	})
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

func TestUpdatePresenceSettingsRejectsNonExactBodiesBeforeMutation(t *testing.T) {
	ts := setupTS(t)

	type invalidCase struct {
		name         string
		body         string
		genericError bool
	}
	cases := make([]invalidCase, 0, 24+len(presenceSettingsJSONFields))
	cases = append(cases, []invalidCase{
		{name: "empty body", body: "", genericError: true},
		{name: "array", body: `[]`, genericError: true},
		{name: "empty object", body: `{}`, genericError: true},
		{name: "malformed", body: `{"master_enabled":`, genericError: true},
		{name: "mixed null and valid", body: `{"master_enabled":null,"custom_text":"valid"}`, genericError: true},
		{name: "duplicate key", body: `{"master_enabled":true,"master_enabled":false}`, genericError: true},
		{name: "trailing object", body: `{"master_enabled":true}{}`, genericError: true},
		{name: "unknown only", body: `{"unknown":true}`, genericError: true},
		{name: "known and unknown", body: `{"master_enabled":true,"unknown":true}`, genericError: true},
		{name: "master wrong type", body: `{"master_enabled":"false"}`, genericError: true},
		{name: "server tier wrong type", body: `{"server_voice_tier":"1"}`, genericError: true},
		{name: "server details wrong type", body: `{"server_voice_show_details":"false"}`, genericError: true},
		{name: "private tier wrong type", body: `{"private_call_tier":"1"}`, genericError: true},
		{name: "private details wrong type", body: `{"private_call_show_details":"false"}`, genericError: true},
		{name: "custom tier wrong type", body: `{"custom_text_tier":"1"}`, genericError: true},
		{name: "custom text wrong type", body: `{"custom_text":true}`, genericError: true},
		{name: "custom emoji wrong type", body: `{"custom_text_emoji":true}`, genericError: true},
		{name: "server tier below range", body: `{"server_voice_tier":-1}`},
		{name: "server tier above range", body: `{"server_voice_tier":3}`},
		{name: "private tier below range", body: `{"private_call_tier":-1}`},
		{name: "private tier above range", body: `{"private_call_tier":3}`},
		{name: "custom tier below range", body: `{"custom_text_tier":-1}`},
		{name: "custom tier above range", body: `{"custom_text_tier":3}`},
		{
			name:         "oversized",
			body:         `{"master_enabled":true}` + strings.Repeat(" ", 16*1024),
			genericError: true,
		},
	}...)
	for _, field := range presenceSettingsJSONFields {
		cases = append(cases, invalidCase{
			name:         "null " + field,
			body:         `{"` + field + `":null}`,
			genericError: true,
		})
	}

	users := make([]testhelpers.TestUser, 4)
	for index := range users {
		users[index] = ts.CreateTestUser(t, "presinvalid"+string(rune('a'+index)))
		_, err := ts.DB.Exec(`
			INSERT INTO user_presence_settings (
				user_id, master_enabled, server_voice_tier, server_voice_show_details,
				private_call_tier, private_call_show_details,
				custom_text_tier, custom_text, custom_text_emoji
			) VALUES ($1, FALSE, 2, FALSE, 1, TRUE, 0, 'unchanged', '🔒')
		`, users[index].ID)
		require.NoError(t, err)
	}

	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user := users[index%len(users)]
			w := rawPresenceSettingsPatch(t, ts, user.AccessToken, tc.body)
			require.Equal(t, http.StatusBadRequest, w.Code)
			if tc.genericError {
				var body map[string]interface{}
				testhelpers.ParseJSON(t, w, &body)
				assert.Equal(t, "Invalid request body", body["error"])
			}

			var unchanged bool
			require.NoError(t, ts.DB.QueryRow(`
				SELECT master_enabled = FALSE
				   AND server_voice_tier = 2
				   AND server_voice_show_details = FALSE
				   AND private_call_tier = 1
				   AND private_call_show_details = TRUE
				   AND custom_text_tier = 0
				   AND custom_text = 'unchanged'
				   AND custom_text_emoji = '🔒'
				FROM user_presence_settings WHERE user_id = $1
			`, user.ID).Scan(&unchanged))
			assert.True(t, unchanged, "invalid request mutated persisted settings")
		})
	}

	missing := ts.CreateTestUser(t, "presinvalidmissing")
	w := rawPresenceSettingsPatch(t, ts, missing.AccessToken, `{"master_enabled":null}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM user_presence_settings WHERE user_id = $1`, missing.ID,
	).Scan(&count))
	assert.Zero(t, count, "invalid PATCH must not insert settings")
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
