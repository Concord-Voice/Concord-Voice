package dm_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: DM history paged with a strict `created_at <` cursor and ordered by
// created_at alone, so rows sharing one created_at (a bulk import, or two sends
// inside one microsecond tick) were cut arbitrarily by LIMIT and the next page
// skipped the tied sibling for good. The channel history route got the same fix
// in the chunked message-search change: a (created_at, id) row-comparison cursor
// and a matching ORDER BY.

// dmHistoryPageSize mirrors the handler's fixed page size. The walk test asserts
// the first page is exactly this long, so a change to the handler fails loudly
// instead of leaving the tied run entirely inside one page.
const dmHistoryPageSize = 50

// Fixed ids for the small cursor test. They are inserted mid, high, low, so the
// insertion order and its reverse both differ from id order: a query ordering by
// created_at alone returns the ties in one of those two physical orders, and
// random uuids would let it pass whenever they happened to line up.
const (
	tieIDLow  = "00000000-0000-4000-8000-000000000001"
	tieIDMid  = "00000000-0000-4000-8000-000000000002"
	tieIDHigh = "00000000-0000-4000-8000-000000000003"
)

func tiedID(rank int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", rank)
}

func insertDMMessageWithID(t *testing.T, ts *testhelpers.TestServer, convID, authorID, id string, at time.Time) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO dm_messages (id, conversation_id, user_id, content, created_at, key_version)
		 VALUES ($1, $2, $3, 'tie msg', $4, 1)`,
		id, convID, authorID, at,
	)
	require.NoError(t, err)
}

// dmHistoryIDs GETs one history page and returns the message ids in response order.
func dmHistoryIDs(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser, convID, query string) []string {
	t.Helper()
	w := ts.DoRequest(http.MethodGet, pathDMConversationsPrefix+convID+"/messages"+query, nil,
		testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
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

// Three messages with an identical created_at: `before=<id>` must return every
// sibling that orders after that id, and the unpaged page must order the run by
// id DESC. With the strict cursor each `before=` page below is empty, so the
// siblings are skipped for good.
func TestDMHistoryCursorReturnsRowsSharingCreatedAt(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "dmtiecursor")
	peer := ts.CreateTestUser(t, "dmtiecursorpeer")
	convID := ts.CreateDMConversation(t, user.ID, peer.ID)
	tied := time.Date(2026, time.March, 1, 12, 0, 0, 123456000, time.UTC)
	for _, id := range []string{tieIDMid, tieIDHigh, tieIDLow} {
		insertDMMessageWithID(t, ts, convID, user.ID, id, tied)
	}

	assert.Equal(t, []string{tieIDHigh, tieIDMid, tieIDLow}, dmHistoryIDs(t, ts, user, convID, ""),
		"ties order by id DESC, whichever order the rows were inserted in")
	assert.Equal(t, []string{tieIDMid, tieIDLow}, dmHistoryIDs(t, ts, user, convID, "?before="+tieIDHigh),
		"the rows sharing the cursor's created_at must not be skipped")
	assert.Equal(t, []string{tieIDLow}, dmHistoryIDs(t, ts, user, convID, "?before="+tieIDMid))
	// Positive control: the cursor still terminates once nothing is older.
	assert.Empty(t, dmHistoryIDs(t, ts, user, convID, "?before="+tieIDLow))
}

// Walking a DM page by page visits every row exactly once, in (created_at DESC,
// id DESC) order, when the page boundary lands inside a run of tied rows. The
// page size is fixed at 50, so the run is 55 rows with one newer and one older
// row around it: page 1 is the newer row plus the 49 highest tied ids, page 2 the
// remaining 6 tied ids plus the older row.
//
// The tied ids are a fixed scramble of 1..55 (rank = (i*37+11) mod 55 + 1, a
// bijection because gcd(37,55) = 1), inserted in that scrambled order, so the
// insertion order and its reverse both differ from the id order.
func TestDMHistoryCursorWalkVisitsTiedRowsExactlyOnce(t *testing.T) {
	const tiedCount = 55
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "dmtiewalk")
	peer := ts.CreateTestUser(t, "dmtiewalkpeer")
	convID := ts.CreateDMConversation(t, user.ID, peer.ID)
	tied := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	const (
		newerID = "ffffffff-0000-4000-8000-000000000001"
		olderID = "00000000-0000-4000-8000-ffffffffffff"
	)

	insertDMMessageWithID(t, ts, convID, user.ID, newerID, tied.Add(time.Hour))
	insertion := make([]int, 0, tiedCount)
	for i := range tiedCount {
		rank := (i*37+11)%tiedCount + 1
		insertion = append(insertion, rank)
		insertDMMessageWithID(t, ts, convID, user.ID, tiedID(rank), tied)
	}
	insertDMMessageWithID(t, ts, convID, user.ID, olderID, tied.Add(-time.Hour))

	// Positive control for the scramble itself: if a future edit made the
	// insertion order monotonic, an ORDER BY without the id tie-breaker could
	// pass again by returning the rows in physical order.
	ascending, descending := true, true
	for i := 1; i < len(insertion); i++ {
		ascending = ascending && insertion[i-1] < insertion[i]
		descending = descending && insertion[i-1] > insertion[i]
	}
	require.False(t, ascending || descending, "the tied rows must not be inserted in id order or its reverse")

	want := []string{newerID}
	for rank := tiedCount; rank >= 1; rank-- {
		want = append(want, tiedID(rank))
	}
	want = append(want, olderID)

	var got []string
	query := ""
	for page := range 3 { // bounded: a cursor that never advances must fail, not hang
		ids := dmHistoryIDs(t, ts, user, convID, query)
		if page == 0 {
			require.Len(t, ids, dmHistoryPageSize, "the first page must be full so the cut lands inside the tied run")
		}
		if len(ids) == 0 {
			break
		}
		got = append(got, ids...)
		query = "?before=" + ids[len(ids)-1]
	}
	assert.ElementsMatch(t, want, got, "every row must be visited exactly once: missing rows were skipped by the cursor, unexpected ones repeated")
	assert.Equal(t, want, got, "rows must arrive in (created_at DESC, id DESC) order")
}
