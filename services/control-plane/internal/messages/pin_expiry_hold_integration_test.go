//go:build integration

package messages_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pin-aware expiry, the pin and unpin writers (#3459 §5, AC14 and I-HELD).
//
// These tests drive the real PinMessage and UnpinMessage handlers, so they
// prove what the four pinning.go statements do to the stamp, not what the
// internal/expiration fragments do in isolation (that is
// TestPinAware_PinAndUnpinFragments). The policy is written by direct SQL: the
// handlers under test only READ it, and the policy writer's own reconcile is
// covered in internal/expiration.

const pinHoldWindowSeconds = 86400

// pinHoldScope is one expiry context (a server channel or a DM conversation)
// with the real HTTP pin and unpin routes in front of it.
type pinHoldScope struct {
	name      string
	table     string
	ts        *testhelpers.TestServer
	headers   http.Header
	setPolicy func(t *testing.T, window *int, includePinned bool)
	insert    func(t *testing.T, expiresAt *time.Time) string
}

// newPinHoldScopes builds a fresh channel scope and DM scope, each with its own
// users, so the per-user pin rate limit (10 per minute) cannot couple scenarios.
func newPinHoldScopes(t *testing.T, ts *testhelpers.TestServer) []pinHoldScope {
	t.Helper()
	suffix := uuid.NewString()[:8]

	owner := ts.CreateTestUser(t, "pinholdc_"+suffix)
	serverID := ts.CreateTestServer(t, owner.ID, "Pin Hold "+suffix)
	channelID := ts.CreateTestChannel(t, serverID, "general")
	channel := pinHoldScope{
		name: "channel", table: "messages", ts: ts, headers: testhelpers.AuthHeaders(owner.AccessToken),
		setPolicy: func(t *testing.T, window *int, includePinned bool) {
			t.Helper()
			_, err := ts.DB.Exec(`UPDATE channels SET expiration_window_seconds = $1, expiration_include_pinned = $2 WHERE id = $3`,
				window, includePinned, channelID)
			require.NoError(t, err)
		},
		insert: func(t *testing.T, expiresAt *time.Time) string {
			t.Helper()
			id := ts.CreateTestMessage(t, channelID, owner, "pin hold")
			_, err := ts.DB.Exec(`UPDATE messages SET expires_at = $1 WHERE id = $2`, expiresAt, id)
			require.NoError(t, err)
			return id
		},
	}

	author := ts.CreateTestUser(t, "pinholdd_"+suffix)
	peer := ts.CreateTestUser(t, "pinholdp_"+suffix)
	conversationID := ts.CreateDMConversation(t, author.ID, peer.ID)
	conversation := pinHoldScope{
		name: "conversation", table: "dm_messages", ts: ts, headers: testhelpers.AuthHeaders(author.AccessToken),
		setPolicy: func(t *testing.T, window *int, includePinned bool) {
			t.Helper()
			_, err := ts.DB.Exec(`UPDATE dm_conversations SET expiration_window_seconds = $1, expiration_include_pinned = $2 WHERE id = $3`,
				window, includePinned, conversationID)
			require.NoError(t, err)
		},
		insert: func(t *testing.T, expiresAt *time.Time) string {
			t.Helper()
			id := insertDMMessageDirect(t, ts, conversationID, author.ID, "pin hold")
			_, err := ts.DB.Exec(`UPDATE dm_messages SET expires_at = $1 WHERE id = $2`, expiresAt, id)
			require.NoError(t, err)
			return id
		},
	}
	return []pinHoldScope{channel, conversation}
}

// pin and unpin go through the real routes. Each requires a plain success and
// that the pin state actually changed, so a 200 that took the idempotent
// already-pinned path cannot satisfy a stamp assertion by accident.
func (s pinHoldScope) pin(t *testing.T, id string) {
	t.Helper()
	w := s.ts.DoRequest(http.MethodPost, pinAPIMsg+id+pinPath, nil, s.headers)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, s.pinned(t, id), "the pin route must pin the message")
}

func (s pinHoldScope) unpin(t *testing.T, id string) {
	t.Helper()
	w := s.ts.DoRequest(http.MethodDelete, pinAPIMsg+id+pinPath, nil, s.headers)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.False(t, s.pinned(t, id), "the unpin route must unpin the message")
}

func (s pinHoldScope) pinned(t *testing.T, id string) bool {
	t.Helper()
	var pinned bool
	switch s.table {
	case "messages":
		require.NoError(t, s.ts.DB.QueryRow(`SELECT pinned_at IS NOT NULL FROM messages WHERE id = $1`, id).Scan(&pinned))
	case "dm_messages":
		require.NoError(t, s.ts.DB.QueryRow(`SELECT pinned_at IS NOT NULL FROM dm_messages WHERE id = $1`, id).Scan(&pinned))
	default:
		t.Fatalf("unsupported table %q", s.table)
	}
	return pinned
}

// requireStamps asserts a row's (expires_at, held_expires_at) exactly; nil means
// NULL. Stamps are microsecond-truncated by the caller, so Equal is exact.
func (s pinHoldScope) requireStamps(t *testing.T, id string, wantExpires, wantHeld *time.Time, msg string) {
	t.Helper()
	var expires, held *time.Time
	switch s.table {
	case "messages":
		require.NoError(t, s.ts.DB.QueryRow(`SELECT expires_at, held_expires_at FROM messages WHERE id = $1`, id).Scan(&expires, &held))
	case "dm_messages":
		require.NoError(t, s.ts.DB.QueryRow(`SELECT expires_at, held_expires_at FROM dm_messages WHERE id = $1`, id).Scan(&expires, &held))
	default:
		t.Fatalf("unsupported table %q", s.table)
	}
	requireSameInstant(t, wantExpires, expires, msg+": expires_at")
	requireSameInstant(t, wantHeld, held, msg+": held_expires_at")
	requirePinHoldIHeld(t, s.ts)
}

func requireSameInstant(t *testing.T, want, got *time.Time, msg string) {
	t.Helper()
	if want == nil {
		assert.Nil(t, got, msg)
		return
	}
	if assert.NotNil(t, got, msg) {
		assert.True(t, want.Equal(*got), "%s = %v, want %v", msg, got.UTC(), want.UTC())
	}
}

// requirePinHoldIHeld asserts I-HELD over the whole database: a held stamp
// exists only on a pinned, unstamped row of a context that keeps pins. It runs
// after every assertion on a row, so a transition that leaves the database
// outside the invariant fails even when the row under test looks right.
func requirePinHoldIHeld(t *testing.T, ts *testhelpers.TestServer) {
	t.Helper()
	var violations int
	require.NoError(t, ts.DB.QueryRow(`
SELECT
  (SELECT count(*) FROM messages m JOIN channels c ON c.id = m.channel_id
   WHERE m.held_expires_at IS NOT NULL
     AND NOT (m.pinned_at IS NOT NULL AND m.expires_at IS NULL
              AND c.expiration_window_seconds IS NOT NULL AND NOT c.expiration_include_pinned))
+ (SELECT count(*) FROM dm_messages m JOIN dm_conversations c ON c.id = m.conversation_id
   WHERE m.held_expires_at IS NOT NULL
     AND NOT (m.pinned_at IS NOT NULL AND m.expires_at IS NULL
              AND c.expiration_window_seconds IS NOT NULL AND NOT c.expiration_include_pinned))`).Scan(&violations))
	require.Zero(t, violations, "I-HELD: held_expires_at only on a pinned, unstamped row of a context that keeps pins")
}

// pinHoldStamps returns a future and a past stamp, microsecond-truncated so a
// round trip through timestamptz compares exactly.
func pinHoldStamps() (future, past time.Time) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return now.Add(2 * time.Hour), now.Add(-2 * time.Hour)
}

// TestPinHandlers_KeepPinsHoldsAFutureStampAndUnpinRestoresItExactly is the
// core transition on both pin sites: under a keep-pins policy, pinning moves a
// stamp that has not passed into held_expires_at, and unpinning puts that very
// value back on the timer.
func TestPinHandlers_KeepPinsHoldsAFutureStampAndUnpinRestoresItExactly(t *testing.T) {
	ts := setupTS(t)
	window := pinHoldWindowSeconds
	future, _ := pinHoldStamps()
	for _, s := range newPinHoldScopes(t, ts) {
		t.Run(s.name, func(t *testing.T) {
			s.setPolicy(t, &window, false)
			id := s.insert(t, &future)
			s.requireStamps(t, id, &future, nil, "before the pin the message is on the timer")

			s.pin(t, id)
			s.requireStamps(t, id, nil, &future, "pin under keep-pins parks the stamp in held")

			s.unpin(t, id)
			s.requireStamps(t, id, &future, nil, "unpin restores the exact stamp")
		})
	}
}

// TestPinHandlers_KeepPinsLeavesAnAlreadyDueStampStamped is developer decision
// D-C and C-NORES at the handler: a stamp that has already passed is never
// moved into held by a pin, and the unpin does not disturb it. The sweeper
// deletes it on its next pass.
func TestPinHandlers_KeepPinsLeavesAnAlreadyDueStampStamped(t *testing.T) {
	ts := setupTS(t)
	window := pinHoldWindowSeconds
	_, past := pinHoldStamps()
	for _, s := range newPinHoldScopes(t, ts) {
		t.Run(s.name, func(t *testing.T) {
			s.setPolicy(t, &window, false)
			id := s.insert(t, &past)

			s.pin(t, id)
			s.requireStamps(t, id, &past, nil, "a due stamp stays stamped under a keep-pins pin (D-C)")

			s.unpin(t, id)
			s.requireStamps(t, id, &past, nil, "unpin leaves a live stamp unchanged")
		})
	}
}

// TestPinHandlers_IncludeModeLeavesTheStampAlone proves the keep predicate is
// read from the parent: when pins expire with the window, a pin moves nothing.
// The no-window arm pins the other leg of K (window IS NOT NULL): an
// include=false column under a context with no window keeps nothing.
func TestPinHandlers_IncludeModeLeavesTheStampAlone(t *testing.T) {
	ts := setupTS(t)
	window := pinHoldWindowSeconds
	future, _ := pinHoldStamps()
	for _, tc := range []struct {
		name          string
		window        *int
		includePinned bool
	}{
		{"pins expire with the window", &window, true},
		{"no window", nil, false},
	} {
		for _, s := range newPinHoldScopes(t, ts) {
			t.Run(tc.name+"/"+s.name, func(t *testing.T) {
				s.setPolicy(t, tc.window, tc.includePinned)
				id := s.insert(t, &future)

				s.pin(t, id)
				s.requireStamps(t, id, &future, nil, "a context that does not keep pins moves nothing")

				s.unpin(t, id)
				s.requireStamps(t, id, &future, nil, "unpin of an unheld stamp changes nothing")
			})
		}
	}
}

// TestPinHandlers_UnpinOfAMessageTheTimerNeverCoveredStaysUnstamped pins the
// COALESCE in the release fragment: with neither a stamp nor a held stamp there
// is nothing to restore, and unpin must not invent one.
func TestPinHandlers_UnpinOfAMessageTheTimerNeverCoveredStaysUnstamped(t *testing.T) {
	ts := setupTS(t)
	window := pinHoldWindowSeconds
	for _, s := range newPinHoldScopes(t, ts) {
		t.Run(s.name, func(t *testing.T) {
			s.setPolicy(t, &window, false)
			id := s.insert(t, nil)

			s.pin(t, id)
			s.requireStamps(t, id, nil, nil, "pinning an unstamped message holds nothing")

			s.unpin(t, id)
			s.requireStamps(t, id, nil, nil, "unpinning an unstamped message stamps nothing")
		})
	}
}
