package messages_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	bulkPath       = "/messages/bulk"
	channelsPrefix = "/api/v1/channels/"
)

func TestGetMessagesBulkSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bulkuser")
	serverID := ts.CreateTestServer(t, user.ID, "Bulk Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Create 5 messages
	for i := 0; i < 5; i++ {
		ts.CreateTestMessage(t, channelID, user, "Bulk message")
	}

	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Messages []models.MessageWithUser `json:"messages"`
		Count    int                      `json:"count"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, 5, resp.Count)
	assert.Len(t, resp.Messages, 5)
}

func TestGetMessagesBulkPagination(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bulkpaginate")
	serverID := ts.CreateTestServer(t, user.ID, "BulkPage Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Create 3 messages
	for i := 0; i < 3; i++ {
		ts.CreateTestMessage(t, channelID, user, "Msg")
	}

	// Fetch with limit=2
	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath+"?limit=2", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Messages []json.RawMessage `json:"messages"`
		Count    int               `json:"count"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, 2, resp.Count)
}

func TestGetMessagesBulkNotMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "bulkowner")
	outsider := ts.CreateTestUser(t, "bulkoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "BulkPrivate Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath, nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestGetMessagesBulkDefaultLimit200(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bulkdefault")
	serverID := ts.CreateTestServer(t, user.ID, "BulkDefault Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Seed 5 messages via SQL to avoid rate limit
	for i := 0; i < 5; i++ {
		_, err := ts.DB.Exec(
			`INSERT INTO messages (id, channel_id, user_id, content, key_version, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, 'bulk msg', 1, NOW(), NOW())`,
			channelID, user.ID,
		)
		require.NoError(t, err)
	}

	// Fetch without limit param — should default to 200 (returns all 5)
	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Count int `json:"count"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Equal(t, 5, resp.Count)
}

// --- No search-depth bound (founder un-gating 2026-10-15) ---

// History is never depth-limited: a free member's bulk backfill AND plain
// history both return a message older than the retired #1555 premium bound
// (180d), and neither response carries the retired search_depth_days field.
func TestGetMessagesUnboundedForFreeUser(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "nodepthfree")
	serverID := ts.CreateTestServer(t, user.ID, "NoDepth Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	_, err := ts.DB.Exec(
		`INSERT INTO messages (id, channel_id, user_id, content, key_version, created_at, updated_at)
		 VALUES (gen_random_uuid(), $1, $2, 'ancient-400d', 1, NOW() - make_interval(days => 400), NOW())`,
		channelID, user.ID,
	)
	require.NoError(t, err)

	for _, path := range []string{bulkPath, "/messages"} {
		t.Run(path, func(t *testing.T) {
			w := ts.DoRequest("GET", channelsPrefix+channelID+path, nil,
				testhelpers.AuthHeaders(user.AccessToken))
			require.Equal(t, http.StatusOK, w.Code)

			var resp struct {
				Messages        []models.MessageWithUser `json:"messages"`
				SearchDepthDays *int                     `json:"search_depth_days"`
			}
			testhelpers.ParseJSON(t, w, &resp)
			require.Len(t, resp.Messages, 1)
			assert.Equal(t, "ancient-400d", resp.Messages[0].Content)
			assert.Nil(t, resp.SearchDepthDays)
		})
	}
}

// --- Chunked search: `since` bound and lean bulk pages (#3875) ---

func bulkSince(t time.Time) string { return "?since=" + url.QueryEscape(t.UTC().Format(time.RFC3339)) }

func backdateMessage(t *testing.T, ts *testhelpers.TestServer, id string, age time.Duration) {
	t.Helper()
	_, err := ts.DB.Exec(`UPDATE messages SET created_at = NOW() - make_interval(secs => $2) WHERE id = $1`,
		id, age.Seconds())
	require.NoError(t, err)
}

func TestGetMessagesBulkSinceFiltersOlder(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bulksince")
	serverID := ts.CreateTestServer(t, user.ID, "Since Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	oldID := ts.CreateTestMessage(t, channelID, user, "old")
	recentID := ts.CreateTestMessage(t, channelID, user, "recent")
	backdateMessage(t, ts, oldID, 100*24*time.Hour)

	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath+bulkSince(time.Now().Add(-90*24*time.Hour)),
		nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Messages []models.MessageWithUser `json:"messages"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.Len(t, resp.Messages, 1)
	assert.Equal(t, recentID, resp.Messages[0].ID)
}

// The desktop client builds `since` with Date#toISOString(), which always
// carries milliseconds ("2026-10-15T12:00:00.000Z"). The shared bulkSince
// helper formats whole seconds, so this pins the value that actually crosses
// the wire: it must be accepted, and the fraction must be honoured rather
// than truncated (a truncated bound would return both rows).
func TestGetMessagesBulkSinceAcceptsMillisecondTimestamp(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bulksincems")
	serverID := ts.CreateTestServer(t, user.ID, "SinceMs Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	beforeBoundID := ts.CreateTestMessage(t, channelID, user, "before-boundary")
	afterBoundID := ts.CreateTestMessage(t, channelID, user, "after-boundary")

	boundary := time.Date(2026, time.January, 1, 0, 0, 0, 500*int(time.Millisecond), time.UTC)
	for id, at := range map[string]time.Time{
		beforeBoundID: boundary.Add(-100 * time.Millisecond),
		afterBoundID:  boundary.Add(100 * time.Millisecond),
	} {
		_, err := ts.DB.Exec(`UPDATE messages SET created_at = $2 WHERE id = $1`, id, at)
		require.NoError(t, err)
	}

	jsISOString := boundary.Format("2006-01-02T15:04:05.000Z07:00")
	require.Equal(t, "2026-01-01T00:00:00.500Z", jsISOString)

	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath+"?since="+url.QueryEscape(jsISOString),
		nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Messages []models.MessageWithUser `json:"messages"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.Len(t, resp.Messages, 1, "the .500 fraction must bound the page, not be truncated away")
	assert.Equal(t, afterBoundID, resp.Messages[0].ID)
}

func TestGetMessagesBulkSinceWithBefore(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bulksincebefore")
	serverID := ts.CreateTestServer(t, user.ID, "SinceBefore Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ancientID := ts.CreateTestMessage(t, channelID, user, "ancient")
	middleID := ts.CreateTestMessage(t, channelID, user, "middle")
	newestID := ts.CreateTestMessage(t, channelID, user, "newest")
	backdateMessage(t, ts, ancientID, 100*24*time.Hour)
	backdateMessage(t, ts, middleID, 10*24*time.Hour)

	path := channelsPrefix + channelID + bulkPath + bulkSince(time.Now().Add(-90*24*time.Hour)) + "&before=" + newestID
	w := ts.DoRequest("GET", path, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Messages []models.MessageWithUser `json:"messages"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.Len(t, resp.Messages, 1, "before and since must both apply ($3 and $4)")
	assert.Equal(t, middleID, resp.Messages[0].ID)
}

func TestGetMessagesBulkInvalidSince(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bulkbadsince")
	serverID := ts.CreateTestServer(t, user.ID, "BadSince Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// "" is `?since=` sent empty: a supplied value, not an absent one.
	for _, bad := range []string{"", "yesterday", "2026-07-17", "1752710400"} {
		t.Run(bad, func(t *testing.T) {
			w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath+"?since="+url.QueryEscape(bad), nil,
				testhelpers.AuthHeaders(user.AccessToken))
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "Invalid 'since' parameter")
		})
	}
}

func TestGetMessagesBulkNonMemberWithSinceStillForbidden(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "bulksinceowner")
	outsider := ts.CreateTestUser(t, "bulksinceoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "SincePrivate Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath+bulkSince(time.Now()), nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	// A valid value is 403 under either ordering, so the request above cannot
	// tell a parse that runs ahead of the access check from one behind it. A
	// value the parse refuses can: a non-member must not learn whether it
	// parsed before being told they have no access.
	w = ts.DoRequest("GET", channelsPrefix+channelID+bulkPath+"?since=yesterday", nil,
		testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.NotContains(t, w.Body.String(), "Invalid 'since' parameter")
}

// The control half proves the fixture produces enrichment; without it the
// bulk assertion would pass on a message that simply has none.
func TestGetMessagesBulkOmitsEnrichment(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "bulklean")
	serverID := ts.CreateTestServer(t, user.ID, "Lean Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	parentID := ts.CreateTestMessage(t, channelID, user, "parent")
	replyID := ts.CreateTestMessage(t, channelID, user, "reply")
	_, err := ts.DB.Exec(`UPDATE messages SET reply_to_id = $1 WHERE id = $2`, parentID, replyID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO message_reactions (message_id, user_id, emoji) VALUES ($1, $2, '👍')`,
		parentID, user.ID)
	require.NoError(t, err)
	fileID := insertMediaFile(t, ts, user.ID, channelID, "photo", mimeImagePNG, 100)
	insertMessageAttachment(t, ts, parentID, fileID, 0)

	control := ts.DoRequest("GET", channelsPrefix+channelID+"/messages", nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, control.Code)
	require.Contains(t, control.Body.String(), `"reactions"`)
	require.Contains(t, control.Body.String(), `"replied_to"`)
	require.Contains(t, control.Body.String(), `"attachments"`)

	bulk := ts.DoRequest("GET", channelsPrefix+channelID+bulkPath, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, bulk.Code)
	assert.NotContains(t, bulk.Body.String(), `"reactions"`)
	assert.NotContains(t, bulk.Body.String(), `"replied_to"`)
	assert.NotContains(t, bulk.Body.String(), `"attachments"`)
}

// --- Tie-safe cursor: rows that share a created_at (#3875 review) ---

// insertMessageAt seeds a message with an explicit created_at so a test can give
// several rows the same timestamp, as a bulk import or two sends inside one
// microsecond tick do. Callers pass whole microseconds: timestamptz stores no
// finer value, so the Go value and the stored one stay equal.
func insertMessageAt(t *testing.T, ts *testhelpers.TestServer, channelID string, user testhelpers.TestUser, at time.Time) string {
	t.Helper()
	return insertMessageWithID(t, ts, channelID, user, uuid.New().String(), at)
}

// insertMessageWithID is insertMessageAt with a caller-chosen id, for a test
// that must control how the id order relates to the insertion order.
func insertMessageWithID(t *testing.T, ts *testhelpers.TestServer, channelID string, user testhelpers.TestUser, id string, at time.Time) string {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO messages (id, channel_id, user_id, content, key_version, created_at, updated_at)
		 VALUES ($1, $2, $3, 'tie msg', 1, $4, $4)`,
		id, channelID, user.ID, at,
	)
	require.NoError(t, err)
	return id
}

// pageMessageIDs GETs one page and returns the message ids in response order.
func pageMessageIDs(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser, channelID, path, query string) []string {
	t.Helper()
	w := ts.DoRequest("GET", channelsPrefix+channelID+path+query, nil, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	ids := make([]string, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		ids = append(ids, m.ID)
	}
	return ids
}

// descByID sorts ids the way ORDER BY id DESC does. A uuid compares bytewise in
// PostgreSQL, and the canonical lowercase hex form orders identically as text.
func descByID(ids ...string) []string {
	out := append([]string(nil), ids...)
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// Two messages with an identical created_at: page 1 (limit 1) returns one of
// them and `before=<that id>` must return the other. With only a strict
// `created_at <` cursor the second page is empty, so the sibling is skipped for
// good; and without an id tie-breaker the LIMIT cut between the two is
// arbitrary. Both the chat-history page and the search page share the query.
func TestMessagesCursorReturnsRowSharingCreatedAt(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "tiecursor")
	channelID := ts.CreateTestChannel(t, ts.CreateTestServer(t, user.ID, "Tie Server"), "general")
	tied := time.Date(2026, time.March, 1, 12, 0, 0, 123456000, time.UTC)
	a := insertMessageAt(t, ts, channelID, user, tied)
	b := insertMessageAt(t, ts, channelID, user, tied)
	want := descByID(a, b)

	for _, path := range []string{bulkPath, "/messages"} {
		t.Run(path, func(t *testing.T) {
			first := pageMessageIDs(t, ts, user, channelID, path, "?limit=1")
			require.Equal(t, []string{want[0]}, first, "ties order by id DESC, so the cut is deterministic")

			second := pageMessageIDs(t, ts, user, channelID, path, "?limit=1&before="+first[0])
			require.Equal(t, []string{want[1]}, second, "the row sharing the cursor's created_at must not be skipped")

			// Positive control: the cursor still terminates once nothing is older.
			assert.Empty(t, pageMessageIDs(t, ts, user, channelID, path, "?limit=1&before="+second[0]))
		})
	}
}

// Walking a channel page by page, whatever the page size, visits every row
// exactly once and in (created_at DESC, id DESC) order, including when a page
// boundary lands inside a run of tied rows. Limit 1 puts every cut between two
// tied rows; limit 2 cuts inside the run and exactly at its edges.
//
// The tied ids are fixed and inserted out of order (mid, high, low), so neither
// the insertion order nor its reverse equals the id order. A query that orders by
// created_at alone returns ties in one of those two physical orders, and random
// uuids would let it pass whenever they happened to line up.
func TestMessagesCursorWalkVisitsTiedRowsExactlyOnce(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "tiewalk")
	channelID := ts.CreateTestChannel(t, ts.CreateTestServer(t, user.ID, "TieWalk Server"), "general")
	tied := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	const (
		idLow  = "00000000-0000-4000-8000-000000000001"
		idMid  = "00000000-0000-4000-8000-000000000002"
		idHigh = "00000000-0000-4000-8000-000000000003"
	)
	newer := insertMessageAt(t, ts, channelID, user, tied.Add(time.Hour))
	for _, id := range []string{idMid, idHigh, idLow} {
		insertMessageWithID(t, ts, channelID, user, id, tied)
	}
	older := insertMessageAt(t, ts, channelID, user, tied.Add(-time.Hour))
	want := []string{newer, idHigh, idMid, idLow, older}

	for _, path := range []string{bulkPath, "/messages"} {
		for _, limit := range []string{"1", "2", "3"} {
			t.Run(path+"/limit="+limit, func(t *testing.T) {
				var got []string
				query := "?limit=" + limit
				for range len(want) + 1 { // bounded: a cursor that never advances must fail, not hang
					ids := pageMessageIDs(t, ts, user, channelID, path, query)
					if len(ids) == 0 {
						break
					}
					got = append(got, ids...)
					query = "?limit=" + limit + "&before=" + ids[len(ids)-1]
				}
				assert.Equal(t, want, got)
			})
		}
	}
}

// `since` is an inclusive lower bound: a row stamped exactly at the bound is
// returned, with and without a cursor, and a tied pair sitting on the bound is
// returned whole when the page boundary falls between them.
func TestGetMessagesBulkSinceBoundIsInclusive(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sinceinclusive")
	serverID := ts.CreateTestServer(t, user.ID, "SinceInc Server")
	boundary := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	since := bulkSince(boundary)

	t.Run("row at the bound is included", func(t *testing.T) {
		channelID := ts.CreateTestChannel(t, serverID, "inclusive")
		insertMessageAt(t, ts, channelID, user, boundary.Add(-time.Second))
		exact := insertMessageAt(t, ts, channelID, user, boundary)
		newest := insertMessageAt(t, ts, channelID, user, boundary.Add(time.Second))

		assert.Equal(t, []string{newest, exact},
			pageMessageIDs(t, ts, user, channelID, bulkPath, since))
		assert.Equal(t, []string{exact},
			pageMessageIDs(t, ts, user, channelID, bulkPath, since+"&before="+newest))
	})

	t.Run("tied pair on the bound survives a page cut between them", func(t *testing.T) {
		channelID := ts.CreateTestChannel(t, serverID, "tied-on-bound")
		insertMessageAt(t, ts, channelID, user, boundary.Add(-time.Second))
		want := descByID(
			insertMessageAt(t, ts, channelID, user, boundary),
			insertMessageAt(t, ts, channelID, user, boundary),
		)

		first := pageMessageIDs(t, ts, user, channelID, bulkPath, since+"&limit=1")
		require.Equal(t, []string{want[0]}, first)
		second := pageMessageIDs(t, ts, user, channelID, bulkPath, since+"&limit=1&before="+first[0])
		require.Equal(t, []string{want[1]}, second, "since and the tie-safe cursor must both apply")
		assert.Empty(t, pageMessageIDs(t, ts, user, channelID, bulkPath, since+"&limit=1&before="+second[0]),
			"the row older than the bound stays excluded")
	})
}
