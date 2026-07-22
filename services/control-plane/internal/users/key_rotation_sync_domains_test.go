package users_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// No-broadcast invariant (spec #2200 §5, revised at review): password-change
// rotations emit NO *_updated events — the rotation path collects no
// broadcasts at all (rotateAllPasswordSyncDomains returns versions only), so
// there is nothing to observe on success or rollback. Old-key sessions
// converge via the forced disconnect + re-login rehydration instead.

const (
	syncDomainNewPassword = "NewPassword456!"                      // pragma: allowlist secret
	prefsNewCiphertext    = "bmV3LXByZWZzLWNpcGhlcnRleHQ="         // pragma: allowlist secret
	gifsNewCiphertext     = "bmV3LWdpZnMtY2lwaGVydGV4dA=="         // pragma: allowlist secret
	friendNewCiphertext   = "bmV3LWZyaWVuZC1vcmctY2lwaGVydGV4dA==" // pragma: allowlist secret
	prefsOldCiphertext    = "b2xkLXByZWZzLWNpcGhlcnRleHQ="         // pragma: allowlist secret
	gifsOldCiphertext     = "b2xkLWdpZnMtY2lwaGVydGV4dA=="         // pragma: allowlist secret
	friendOldCiphertext   = "b2xkLWZyaWVuZC1vcmctY2lwaGVydGV4dA==" // pragma: allowlist secret
	presenceCiphertextNew = "bmV3LXByZXNlbmNlLWNpcGhlcnRleHQ="     // pragma: allowlist secret
)

var syncDomainTables = []struct {
	domain string
	table  string
}{
	{"preferences", "user_preferences"},
	{"saved_gifs", "saved_gifs"},
	{"friend_organization", "friend_organization"},
}

// Fixed literal SQL per table — no Sprintf-built SQL, so scanners and grep
// audits need no special case for these helpers (CodeRabbit, backend.md).
var syncDomainSeedSQL = map[string]string{
	"user_preferences":    `INSERT INTO user_preferences (user_id, encrypted_data, version) VALUES ($1, $2, $3)`,
	"saved_gifs":          `INSERT INTO saved_gifs (user_id, encrypted_data, version) VALUES ($1, $2, $3)`,
	"friend_organization": `INSERT INTO friend_organization (user_id, encrypted_data, version) VALUES ($1, $2, $3)`,
}

var syncDomainReadSQL = map[string]string{
	"user_preferences":    `SELECT encrypted_data, version FROM user_preferences WHERE user_id = $1`,
	"saved_gifs":          `SELECT encrypted_data, version FROM saved_gifs WHERE user_id = $1`,
	"friend_organization": `SELECT encrypted_data, version FROM friend_organization WHERE user_id = $1`,
}

func seedSyncDomainRow(t *testing.T, ts *testhelpers.TestServer, table, userID, ciphertext string, version int) {
	t.Helper()
	query, ok := syncDomainSeedSQL[table]
	require.True(t, ok, "unknown sync domain table: %s", table)
	_, err := ts.DB.Exec(query, userID, ciphertext, version)
	require.NoError(t, err)
}

func readSyncDomainRow(t *testing.T, ts *testhelpers.TestServer, table, userID string) (string, int, bool) {
	t.Helper()
	query, ok := syncDomainReadSQL[table]
	if !ok {
		t.Fatalf("unknown sync domain table: %s", table)
	}
	var ciphertext string
	var version int
	if err := ts.DB.QueryRow(query, userID).Scan(&ciphertext, &version); err != nil {
		return "", 0, false
	}
	return ciphertext, version, true
}

func changePasswordBody(user testhelpers.TestUser, syncDomains map[string]interface{}) map[string]interface{} {
	_, wrappedKey, salt := testhelpers.E2EETestKeys()
	body := map[string]interface{}{
		keyCurrentPassword:   user.Password,
		keyNewPassword:       syncDomainNewPassword,
		keyWrappedPrivateKey: wrappedKey,
		keyKeyDerivationSalt: salt,
		"key_derivation_alg": "argon2id",
	}
	if syncDomains != nil {
		body["sync_domains"] = syncDomains
	}
	return body
}

func assertPasswordUnchanged(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser) {
	t.Helper()
	var passwordHash string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT password_hash FROM users WHERE id = $1`, user.ID,
	).Scan(&passwordHash))
	matchesOld, err := auth.VerifyPassword(user.Password, passwordHash)
	require.NoError(t, err)
	assert.True(t, matchesOld, "password must be unchanged after rollback")
}

func TestChangePasswordSyncDomainsRotatesAllPopulatedDomains(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "syncdomainsrotateall")
	seedSyncDomainRow(t, ts, "user_preferences", user.ID, prefsOldCiphertext, 3)
	seedSyncDomainRow(t, ts, "saved_gifs", user.ID, gifsOldCiphertext, 1)
	seedSyncDomainRow(t, ts, "friend_organization", user.ID, friendOldCiphertext, 7)

	w := ts.DoRequest("POST", urlUsersMePassword, changePasswordBody(user, map[string]interface{}{
		"preferences":         map[string]interface{}{"encrypted_data": prefsNewCiphertext, "expected_version": 3},
		"saved_gifs":          map[string]interface{}{"encrypted_data": gifsNewCiphertext, "expected_version": 1},
		"friend_organization": map[string]interface{}{"encrypted_data": friendNewCiphertext, "expected_version": 7},
	}), testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		SyncDomainVersions map[string]int `json:"sync_domain_versions"`
	}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, map[string]int{
		"preferences": 4, "saved_gifs": 2, "friend_organization": 8,
	}, body.SyncDomainVersions)

	for _, expected := range []struct {
		table, ciphertext string
		version           int
	}{
		{"user_preferences", prefsNewCiphertext, 4},
		{"saved_gifs", gifsNewCiphertext, 2},
		{"friend_organization", friendNewCiphertext, 8},
	} {
		ciphertext, version, found := readSyncDomainRow(t, ts, expected.table, user.ID)
		require.True(t, found, expected.table)
		assert.Equal(t, expected.ciphertext, ciphertext, expected.table)
		assert.Equal(t, expected.version, version, expected.table)
	}

	var passwordHash string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT password_hash FROM users WHERE id = $1`, user.ID,
	).Scan(&passwordHash))
	matchesNew, err := auth.VerifyPassword(syncDomainNewPassword, passwordHash)
	require.NoError(t, err)
	assert.True(t, matchesNew)

	var unrevoked int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user.ID,
	).Scan(&unrevoked))
	assert.Zero(t, unrevoked, "all refresh tokens must be revoked atomically")
}

func TestChangePasswordSyncDomainsAbsentAllVerifiesWithoutCreatingRows(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "syncdomainsabsentall")

	w := ts.DoRequest("POST", urlUsersMePassword, changePasswordBody(user, map[string]interface{}{
		"preferences":         map[string]interface{}{"expected_version": 0},
		"saved_gifs":          map[string]interface{}{"expected_version": 0},
		"friend_organization": map[string]interface{}{"expected_version": 0},
	}), testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		SyncDomainVersions map[string]int `json:"sync_domain_versions"`
	}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, map[string]int{
		"preferences": 0, "saved_gifs": 0, "friend_organization": 0,
	}, body.SyncDomainVersions)

	for _, d := range syncDomainTables {
		_, _, found := readSyncDomainRow(t, ts, d.table, user.ID)
		assert.False(t, found, "%s: absence assertion must not materialize a row", d.table)
	}
}

func TestChangePasswordSyncDomainsMixedRotateAbsentPreserve(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "syncdomainsmixed")
	seedSyncDomainRow(t, ts, "user_preferences", user.ID, prefsOldCiphertext, 2)
	// friend_organization row exists but is submitted data-less: verify + preserve.
	seedSyncDomainRow(t, ts, "friend_organization", user.ID, friendOldCiphertext, 5)

	w := ts.DoRequest("POST", urlUsersMePassword, changePasswordBody(user, map[string]interface{}{
		"preferences":         map[string]interface{}{"encrypted_data": prefsNewCiphertext, "expected_version": 2},
		"saved_gifs":          map[string]interface{}{"expected_version": 0},
		"friend_organization": map[string]interface{}{"expected_version": 5},
	}), testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		SyncDomainVersions map[string]int `json:"sync_domain_versions"`
	}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, map[string]int{
		"preferences": 3, "saved_gifs": 0, "friend_organization": 5,
	}, body.SyncDomainVersions)

	ciphertext, version, found := readSyncDomainRow(t, ts, "friend_organization", user.ID)
	require.True(t, found)
	assert.Equal(t, friendOldCiphertext, ciphertext, "preserved row's ciphertext must be byte-unchanged")
	assert.Equal(t, 5, version, "preserved row's version must be unchanged")
	_, _, gifsFound := readSyncDomainRow(t, ts, "saved_gifs", user.ID)
	assert.False(t, gifsFound)
}

func TestChangePasswordSyncDomainConflictRollsBackEverything(t *testing.T) {
	for _, conflicting := range syncDomainTables {
		t.Run(conflicting.domain, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "syncconflict"+conflicting.domain)
			seedSyncDomainRow(t, ts, "user_preferences", user.ID, prefsOldCiphertext, 3)
			seedSyncDomainRow(t, ts, "saved_gifs", user.ID, gifsOldCiphertext, 1)
			seedSyncDomainRow(t, ts, "friend_organization", user.ID, friendOldCiphertext, 7)
			seedPresenceOverrideStateForKeyRotation(t, ts, user.ID, ts.CreateTestUser(t, "synctarget"+conflicting.domain).ID)

			var originalWrappedKey, originalSalt []byte
			require.NoError(t, ts.DB.QueryRow(
				`SELECT wrapped_private_key, key_derivation_salt FROM user_keys WHERE user_id = $1`,
				user.ID,
			).Scan(&originalWrappedKey, &originalSalt))

			correct := map[string]int{"preferences": 3, "saved_gifs": 1, "friend_organization": 7}
			domains := map[string]interface{}{}
			ciphertexts := map[string]string{
				"preferences": prefsNewCiphertext, "saved_gifs": gifsNewCiphertext, "friend_organization": friendNewCiphertext,
			}
			for name, version := range correct {
				expected := version
				if name == conflicting.domain {
					expected = version + 41 // stale
				}
				domains[name] = map[string]interface{}{
					"encrypted_data": ciphertexts[name], "expected_version": expected,
				}
			}
			body := changePasswordBody(user, domains)
			body["presence_override"] = map[string]interface{}{
				"encrypted_data": presenceCiphertextNew, "expected_version": 4,
			}

			w := ts.DoRequest("POST", urlUsersMePassword, body, testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusConflict, w.Code, w.Body.String())

			var conflictBody map[string]interface{}
			testhelpers.ParseJSON(t, w, &conflictBody)
			assert.Equal(t, "sync_domain_version_conflict", conflictBody["code"])
			assert.Equal(t, conflicting.domain, conflictBody["domain"])
			assert.Equal(t, float64(correct[conflicting.domain]), conflictBody["current_version"])

			assertPasswordUnchanged(t, ts, user)
			var persistedWrappedKey, persistedSalt []byte
			require.NoError(t, ts.DB.QueryRow(
				`SELECT wrapped_private_key, key_derivation_salt FROM user_keys WHERE user_id = $1`,
				user.ID,
			).Scan(&persistedWrappedKey, &persistedSalt))
			assert.Equal(t, originalWrappedKey, persistedWrappedKey)
			assert.Equal(t, originalSalt, persistedSalt)

			var revoked int
			require.NoError(t, ts.DB.QueryRow(
				`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NOT NULL`, user.ID,
			).Scan(&revoked))
			assert.Zero(t, revoked, "conflict must not revoke refresh tokens")

			for _, d := range syncDomainTables {
				expectedCipher := map[string]string{
					"user_preferences": prefsOldCiphertext, "saved_gifs": gifsOldCiphertext, "friend_organization": friendOldCiphertext,
				}[d.table]
				ciphertext, version, found := readSyncDomainRow(t, ts, d.table, user.ID)
				require.True(t, found, d.table)
				assert.Equal(t, expectedCipher, ciphertext, d.table)
				assert.Equal(t, correct[d.domain], version, d.table)
			}

			var presenceCipher string
			var presenceVersion int
			require.NoError(t, ts.DB.QueryRow(
				`SELECT encrypted_data, version FROM presence_override_preferences
				 WHERE user_id = $1 AND category = 'custom_text'`, user.ID,
			).Scan(&presenceCipher, &presenceVersion))
			assert.Equal(t, "old-preference-key-ciphertext", presenceCipher)
			assert.Equal(t, 4, presenceVersion)
		})
	}
}

func TestChangePasswordSyncDomainAbsentAssertedButPresentConflicts(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "syncabsentbutpresent")
	seedSyncDomainRow(t, ts, "saved_gifs", user.ID, gifsOldCiphertext, 6)

	w := ts.DoRequest("POST", urlUsersMePassword, changePasswordBody(user, map[string]interface{}{
		"preferences":         map[string]interface{}{"expected_version": 0},
		"saved_gifs":          map[string]interface{}{"expected_version": 0},
		"friend_organization": map[string]interface{}{"expected_version": 0},
	}), testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())

	var conflictBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &conflictBody)
	assert.Equal(t, "sync_domain_version_conflict", conflictBody["code"])
	assert.Equal(t, "saved_gifs", conflictBody["domain"])
	assert.Equal(t, float64(6), conflictBody["current_version"])
	assertPasswordUnchanged(t, ts, user)
}

func TestChangePasswordLegacyOmissionLeavesDomainsUntouched(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "synclegacyomission")
	seedSyncDomainRow(t, ts, "user_preferences", user.ID, prefsOldCiphertext, 9)

	w := ts.DoRequest("POST", urlUsersMePassword, changePasswordBody(user, nil),
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var raw map[string]interface{}
	testhelpers.ParseJSON(t, w, &raw)
	_, present := raw["sync_domain_versions"]
	assert.False(t, present, "legacy response must not carry sync_domain_versions")

	ciphertext, version, found := readSyncDomainRow(t, ts, "user_preferences", user.ID)
	require.True(t, found)
	assert.Equal(t, prefsOldCiphertext, ciphertext)
	assert.Equal(t, 9, version)
}

func TestChangePasswordSyncDomainsValidationRejects(t *testing.T) {
	cases := []struct {
		name    string
		domains map[string]interface{}
	}{
		{"version zero with data", map[string]interface{}{
			"preferences":         map[string]interface{}{"encrypted_data": prefsNewCiphertext, "expected_version": 0},
			"saved_gifs":          map[string]interface{}{"expected_version": 0},
			"friend_organization": map[string]interface{}{"expected_version": 0},
		}},
		{"missing sub-field", map[string]interface{}{
			"preferences":         map[string]interface{}{"expected_version": 0},
			"friend_organization": map[string]interface{}{"expected_version": 0},
		}},
		{"non-base64 data", map[string]interface{}{
			"preferences":         map[string]interface{}{"encrypted_data": "not-base64!!!", "expected_version": 1},
			"saved_gifs":          map[string]interface{}{"expected_version": 0},
			"friend_organization": map[string]interface{}{"expected_version": 0},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "syncvalidation")
			w := ts.DoRequest("POST", urlUsersMePassword, changePasswordBody(user, tc.domains),
				testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assertPasswordUnchanged(t, ts, user)
		})
	}
}

// The absent-assert phantom-row race (#2200 review, Codex P1): a first-time
// PUT INSERT could land between the password transaction's assert-absent read
// (which locks no row) and its commit. upsertE2EEBlob therefore locks the
// users row FOR NO KEY UPDATE — the same lock the password transaction holds
// from step-up through commit — so every blob write waits out an in-flight
// rotation. This test proves the PUT genuinely blocks on that lock.
func TestUpsertE2EEBlobWaitsForPasswordTransaction(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "syncupsertserialize")

	blocker, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	_, err = blocker.Exec(`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, user.ID)
	require.NoError(t, err)

	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- ts.DoRequest("PUT", "/api/v1/users/me/preferences", map[string]interface{}{
			"encrypted_data": prefsNewCiphertext,
		}, testhelpers.AuthHeaders(user.AccessToken))
	}()

	require.Eventually(t, func() bool {
		var waiting int
		queryErr := ts.DB.QueryRow(`
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE%'
		`).Scan(&waiting)
		return queryErr == nil && waiting > 0
	}, 10*time.Second, 25*time.Millisecond,
		"blob upsert never reached the serializing users-row lock")

	require.NoError(t, blocker.Commit())

	select {
	case w := <-response:
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		ciphertext, version, found := readSyncDomainRow(t, ts, "user_preferences", user.ID)
		require.True(t, found)
		assert.Equal(t, prefsNewCiphertext, ciphertext)
		assert.Equal(t, 1, version)
	case <-time.After(10 * time.Second):
		t.Fatal("blob upsert did not complete after the blocker committed")
	}
}

// A concurrent standalone PUT and a password change serialize on the domain
// row lock; the CAS catches the version movement so no torn state can commit.
// Deterministic variant of the race: a blocker transaction holds the
// user_preferences row, advances its version the way the PUT upsert would,
// and commits only after the password change is provably waiting on the lock.
func TestChangePasswordSyncDomainCASCatchesConcurrentPut(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "syncconcurrentput")
	seedSyncDomainRow(t, ts, "user_preferences", user.ID, prefsOldCiphertext, 2)

	blocker, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	_, err = blocker.Exec(`SELECT version FROM user_preferences WHERE user_id = $1 FOR UPDATE`, user.ID)
	require.NoError(t, err)

	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- ts.DoRequest("POST", urlUsersMePassword, changePasswordBody(user, map[string]interface{}{
			"preferences":         map[string]interface{}{"encrypted_data": prefsNewCiphertext, "expected_version": 2},
			"saved_gifs":          map[string]interface{}{"expected_version": 0},
			"friend_organization": map[string]interface{}{"expected_version": 0},
		}), testhelpers.AuthHeaders(user.AccessToken))
	}()

	require.Eventually(t, func() bool {
		var waiting int
		queryErr := ts.DB.QueryRow(`
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%FROM user_preferences WHERE user_id = $1 FOR UPDATE%'
		`).Scan(&waiting)
		return queryErr == nil && waiting > 0
	}, 10*time.Second, 25*time.Millisecond,
		"password change never reached the serialized domain-row lock")

	_, err = blocker.Exec(
		`UPDATE user_preferences SET encrypted_data = $1, version = version + 1, updated_at = NOW()
		 WHERE user_id = $2`,
		gifsNewCiphertext, user.ID,
	)
	require.NoError(t, err)
	require.NoError(t, blocker.Commit())

	select {
	case w := <-response:
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		var conflictBody map[string]interface{}
		testhelpers.ParseJSON(t, w, &conflictBody)
		assert.Equal(t, "sync_domain_version_conflict", conflictBody["code"])
		assert.Equal(t, "preferences", conflictBody["domain"])
		assert.Equal(t, float64(3), conflictBody["current_version"])
	case <-time.After(10 * time.Second):
		t.Fatal("password change did not complete after the blocker committed")
	}

	assertPasswordUnchanged(t, ts, user)
	ciphertext, version, found := readSyncDomainRow(t, ts, "user_preferences", user.ID)
	require.True(t, found)
	assert.Equal(t, gifsNewCiphertext, ciphertext, "the concurrent writer's value must win intact")
	assert.Equal(t, 3, version)
}
