package users_test

// Spec T4 / AC-11: the positional-alignment lock for allow_friend_requests_from.
//
// WHY THIS FILE EXISTS. #1240 inserts one column into five enumerations that
// must stay in lock-step: the GET SELECT list, the GET Scan args, the row-less
// defaults literal, the PATCH RETURNING list, and the PATCH Scan args. Four of
// the five failure modes are LOUD — a length mismatch is "sql: expected N
// destination arguments", and a string scanned into a bool or an int fails at
// Scan. Exactly one is SILENT: privacySettingsResponse holds only two string
// fields, AllowFriendRequestsFrom and UpdatedAt, and they are adjacent. Swap
// them and every type checks, every length matches, and the API serves the
// timestamp as the privacy mode and the mode as the timestamp.
//
// Two independent locks, because neither alone is sufficient:
//
//	1. A SOURCE lock (no database) on the five insertion points. It catches the
//	   swap the moment it is written, in the file where it was written, and it
//	   runs in CI even when the DB-backed suite is skipped.
//	2. A BEHAVIOURAL lock (database) asserting the round-tripped values are
//	   type-plausible for their own field. This is the one that cannot be
//	   satisfied by a coincidence of text, and it is what would catch a swap
//	   introduced anywhere other than these five lines.
//
// The source lock is deliberately narrow: it asserts ADJACENCY AND ORDER of one
// pair, not the whole 14-entry list. A test that told a reviewer to diff four
// long lists is a test that gets skimmed.

import (
	"encoding/json"
	"go/ast"
	"net/http"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

const friendRequestModeKey = "allow_friend_requests_from"

// ── Lock 1: the five insertion points, read out of the source ────────────────

// TestPrivacyColumnEnumerationsKeepFriendRequestModeBeforeUpdatedAt is the
// source half.
//
// Each pattern matches "the new column, then optional whitespace/newline, then
// updated_at" — so a rename, a reorder, or an insertion between the two breaks
// it. The occurrence COUNT is asserted, not merely presence: the SELECT and the
// RETURNING list are separate sites, and a fix applied to only one of them
// would still satisfy a bare Contains.
func TestPrivacyColumnEnumerationsKeepFriendRequestModeBeforeUpdatedAt(t *testing.T) {
	source, err := os.ReadFile(privacyHandlersFile) // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	text := string(source)

	for _, tc := range []struct {
		name    string
		pattern string
		want    int
		why     string
	}{
		{
			name:    "SQL column lists (GET SELECT + PATCH RETURNING)",
			pattern: `allow_friend_requests_from,\s*updated_at`,
			want:    2,
			why: "the GET SELECT list and the PATCH RETURNING list must BOTH place the " +
				"column immediately before updated_at. Two string columns adjacent in one " +
				"list and transposed in the other is the one swap that fails silently",
		},
		{
			name:    "Scan destination lists (GET + PATCH)",
			pattern: `&ps\.AllowFriendRequestsFrom,\s*&ps\.UpdatedAt,`,
			want:    2,
			why: "each Scan arg list must mirror its own column list. Both destinations " +
				"are *string, so a transposition here type-checks and scans cleanly",
		},
		{
			name:    "row-less defaults literal",
			pattern: `AllowFriendRequestsFrom:\s*"everyone"`,
			want:    1,
			why: "privacy_settings rows are created lazily, so the sql.ErrNoRows branch is " +
				"the majority path. Omitting the field there serves \"\" — which the client " +
				"cannot map to any mode — instead of the schema default",
		},
		{
			name:    "response struct field order",
			pattern: `AllowFriendRequestsFrom\s+string\s+` + "`" + `json:"allow_friend_requests_from"` + "`" + `\s*\n\s*UpdatedAt`,
			want:    1,
			why: "the struct declaration is the fifth site. Keeping it in the same order as " +
				"the SQL is what lets a reviewer check one pair instead of five lists",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matches := regexp.MustCompile(tc.pattern).FindAllString(text, -1)
			assert.Len(t, matches, tc.want,
				"expected %d occurrence(s) of /%s/ in %s.\n\n%s\n\n"+
					"Do NOT relax this pattern to make it pass — re-establish the ordering.",
				tc.want, tc.pattern, privacyHandlersFile, tc.why)
		})
	}
}

// TestPrivacyResponseHasExactlyTwoStringFields records the PREMISE the narrow
// lock above rests on.
//
// The argument for checking one pair rather than five whole lists is that every
// other field is a bool or an int, so every other transposition fails loudly at
// Scan. The moment a third string field is added that argument is void and the
// silent-swap surface grows — this test is what makes that a build failure
// instead of a forgotten assumption.
func TestPrivacyResponseHasExactlyTwoStringFields(t *testing.T) {
	fields := findStruct(t, privacyHandlersFile, "privacySettingsResponse")

	var stringFields []string
	for _, field := range fields.List {
		if ident, ok := field.Type.(*ast.Ident); ok && ident.Name == "string" {
			stringFields = append(stringFields, fieldNames(field))
		}
	}

	assert.ElementsMatch(t, []string{"AllowFriendRequestsFrom", "UpdatedAt"}, stringFields,
		"privacySettingsResponse must hold exactly these two string fields. A third one "+
			"widens the silent-swap surface that "+
			"TestPrivacyColumnEnumerationsKeepFriendRequestModeBeforeUpdatedAt covers for "+
			"only this pair — extend that test before adding one.")
}

// ── Lock 2: the behavioural half ─────────────────────────────────────────────

// privacyBody pulls the two adjacent string fields out of a GET/PATCH response.
// Decoded into a map rather than a struct on purpose: a struct would re-impose
// the very field ordering under test, and would silently absorb a missing key.
func privacyBody(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	var body struct {
		Privacy map[string]interface{} `json:"privacy"`
	}
	require.NoError(t, json.Unmarshal(raw, &body), "body: %s", string(raw))
	require.NotNil(t, body.Privacy, "response carried no privacy object: %s", string(raw))
	return body.Privacy
}

// assertNoStringFieldSwap is the assertion the source lock cannot make.
//
// It checks each of the two adjacent string fields against the other's shape:
// the mode must be one of three known words and must NOT parse as a timestamp;
// updated_at must parse as a timestamp and must NOT be a mode word. Either
// half alone would miss a one-way corruption.
func assertNoStringFieldSwap(t *testing.T, privacy map[string]interface{}, wantMode string) {
	t.Helper()

	rawMode, present := privacy[friendRequestModeKey]
	require.True(t, present, "the privacy object must carry %q", friendRequestModeKey)
	mode, isString := rawMode.(string)
	require.True(t, isString, "%s must be a JSON string, got %T", friendRequestModeKey, rawMode)

	assert.Equal(t, wantMode, mode)
	assert.Contains(t, []string{"everyone", "mutual_servers", "nobody"}, mode,
		"%s must hold one of the three enum values. A timestamp here means the column "+
			"list and the Scan list have transposed it with updated_at", friendRequestModeKey)
	_, asTime := time.Parse(time.RFC3339, mode)
	assert.Error(t, asTime, "%s must not be a timestamp", friendRequestModeKey)

	rawUpdated, present := privacy["updated_at"]
	require.True(t, present, "the privacy object must carry updated_at")
	updated, isString := rawUpdated.(string)
	require.True(t, isString, "updated_at must be a JSON string, got %T", rawUpdated)
	_, err := time.Parse(time.RFC3339, updated)
	assert.NoError(t, err,
		"updated_at must parse as a timestamp; %q means it received the privacy mode", updated)
	assert.NotContains(t, []string{"everyone", "mutual_servers", "nobody"}, updated)
}

// TestPatchPrivacyRoundTripsEachFriendRequestModeWithoutSwapping is the
// behavioural lock, run for all three modes.
//
// Each mode is checked twice — in the PATCH's own 200 body (the RETURNING path)
// and in a following GET (the SELECT path) — because the two read the column
// through separate enumerations and a swap can exist in one without the other.
func TestPatchPrivacyRoundTripsEachFriendRequestModeWithoutSwapping(t *testing.T) {
	for _, mode := range []string{"everyone", "mutual_servers", "nobody"} {
		t.Run(mode, func(t *testing.T) {
			ts := setupTS(t)
			user := ts.CreateTestUser(t, "frmode"+mode[:4])

			patch := ts.DoRequest(methodPatch, urlUsersMePrivacy,
				map[string]interface{}{friendRequestModeKey: mode},
				testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusOK, patch.Code, patch.Body.String())
			assertNoStringFieldSwap(t, privacyBody(t, patch.Body.Bytes()), mode)

			get := ts.DoRequest(http.MethodGet, urlUsersMePrivacy, nil,
				testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusOK, get.Code, get.Body.String())
			assertNoStringFieldSwap(t, privacyBody(t, get.Body.Bytes()), mode)
		})
	}
}

// TestGetPrivacyDefaultsFriendRequestModeToEveryoneWithNoRow covers the
// sql.ErrNoRows branch — the majority state, since rows are created lazily.
//
// It is separate from the round-trip test because it exercises a code path with
// NO SQL in it at all: a hand-written defaults literal. Omitting the field
// there serves "" and the desktop client, which maps an unknown value to
// nothing, renders no selection.
func TestGetPrivacyDefaultsFriendRequestModeToEveryoneWithNoRow(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "frnorow")

	var rows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM privacy_settings WHERE user_id = $1`, user.ID).Scan(&rows))
	require.Zero(t, rows, "precondition: no privacy_settings row")

	get := ts.DoRequest(http.MethodGet, urlUsersMePrivacy, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())

	privacy := privacyBody(t, get.Body.Bytes())
	assert.Equal(t, "everyone", privacy[friendRequestModeKey],
		"a user with no row must read as 'everyone' — the schema default")
}

// TestPatchPrivacyRejectsAnInvalidFriendRequestMode is the endpoint half of the
// unit-level validation in privacy_friend_request_clauses_internal_test.go: the
// exact §6.2 copy has to survive the trip through the router, which the unit
// test cannot show.
func TestPatchPrivacyRejectsAnInvalidFriendRequestMode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "frbadmode")

	w := ts.DoRequest(methodPatch, urlUsersMePrivacy,
		map[string]interface{}{friendRequestModeKey: "friends-of-friends"},
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t,
		"allow_friend_requests_from must be everyone, mutual_servers, or nobody",
		body["error"])

	var rows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT count(*) FROM privacy_settings WHERE user_id = $1`, user.ID).Scan(&rows))
	assert.Zero(t, rows, "a rejected PATCH must not materialize the settings row")
}

// TestPatchUnrelatedPrivacyFieldDoesNotClobberFriendRequestMode is the
// partial-update contract at the endpoint. buildPrivacyClauses builds SET
// clauses only for non-nil fields, so an omitted mode must survive untouched —
// the failure mode being a PATCH of some unrelated toggle silently reopening a
// user's friend requests.
func TestPatchUnrelatedPrivacyFieldDoesNotClobberFriendRequestMode(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "frclobber")

	require.Equal(t, http.StatusOK, ts.DoRequest(methodPatch, urlUsersMePrivacy,
		map[string]interface{}{friendRequestModeKey: "nobody"},
		testhelpers.AuthHeaders(user.AccessToken)).Code)

	require.Equal(t, http.StatusOK, ts.DoRequest(methodPatch, urlUsersMePrivacy,
		map[string]interface{}{"load_gifs_automatically": false},
		testhelpers.AuthHeaders(user.AccessToken)).Code)

	get := ts.DoRequest(http.MethodGet, urlUsersMePrivacy, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())
	assert.Equal(t, "nobody", privacyBody(t, get.Body.Bytes())[friendRequestModeKey],
		"an unrelated PATCH must not reset the friend-request gate")
}
