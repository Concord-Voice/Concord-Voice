package dm_test

// #3459 §6.4 and §8 at the DM route, observed on a real WebSocket through the
// real router (so NewRouter's pinned-expiry wiring and purge notifier are what
// run):
//
//   - the dm_expiration_event frame carries include_pinned on EVERY event, and
//     previous_include_pinned + previous_window_seconds on a "changed" event only;
//   - the policy broadcast reaches a participant BEFORE the dm_purged that the
//     immediate pinned deletion announces (§8 ordering).

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

type dmObservedFrame struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

// dmFrameTap owns every read on a subscribed socket (a gorilla connection is
// unusable once a read deadline fires, so "drain, act, drain again" on one conn
// cannot work) and records frames in arrival order. A read error other than the
// cleanup close is kept, so a dead observer fails the test instead of reading
// as a quiet window.
type dmFrameTap struct {
	mu     sync.Mutex
	frames []dmObservedFrame
	err    error
	closed bool
}

func (f *dmFrameTap) ofTypes(want ...string) []dmObservedFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []dmObservedFrame
	for _, frame := range f.frames {
		for _, w := range want {
			if frame.Type == w {
				out = append(out, frame)
			}
		}
	}
	return out
}

func (f *dmFrameTap) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = nil
}

// settle waits out a quiet window and then requires the observer to still be
// alive, so a negative or count assertion made afterwards was made by a live socket.
func (f *dmFrameTap) settle(t *testing.T, window time.Duration) {
	t.Helper()
	time.Sleep(window)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NoError(t, f.err, "the frame observer died before the window ended")
}

func tapDMFrames(t *testing.T, conn *gorillaWS.Conn) *dmFrameTap {
	t.Helper()
	tap := &dmFrameTap{}
	t.Cleanup(func() {
		tap.mu.Lock()
		tap.closed = true
		tap.mu.Unlock()
		_ = conn.Close()
	})
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				tap.mu.Lock()
				if !tap.closed {
					tap.err = err
				}
				tap.mu.Unlock()
				return
			}
			var frame dmObservedFrame
			if json.Unmarshal(raw, &frame) == nil {
				tap.mu.Lock()
				tap.frames = append(tap.frames, frame)
				tap.mu.Unlock()
			}
		}
	}()
	return tap
}

// Mutants killed:
//   - the expiration broadcast moved after the expirer in the DM UpdateExpiration
//     (dm_purged arrives first);
//   - AddPinnedEventFields dropping include_pinned, previous_include_pinned or
//     previous_window_seconds from the live frame.
func TestDMExpiration_PinnedToggleFramesCarryPinFieldsAndPrecedeThePurge(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "pin_frames_alice")
	bob := ts.CreateTestUser(t, "pin_frames_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	auth := testhelpers.AuthHeaders(alice.AccessToken)
	path := dmExpirationPath + convID + "/expiration"

	tap := tapDMFrames(t, dialDMObserver(t, ts, bob.ID, convID))

	// A new timer with the field omitted is E=false: the "set" frame still
	// announces include_pinned (false) and carries no previous_* member.
	w := ts.DoRequest(http.MethodPatch, path,
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"}, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Eventually(t, func() bool { return len(tap.ofTypes("dm_expiration_event")) == 1 }, 3*time.Second, 20*time.Millisecond)
	tap.settle(t, 500*time.Millisecond)
	setFrames := tap.ofTypes("dm_expiration_event", "dm_purged")
	require.Len(t, setFrames, 1, "creating the timer announces the policy and purges nothing")
	assert.Equal(t, "set", setFrames[0].Data["kind"])
	includePinned, present := setFrames[0].Data["include_pinned"]
	assert.True(t, present, "include_pinned rides every event so the client merge can advance")
	assert.Equal(t, false, includePinned)
	assert.NotContains(t, setFrames[0].Data, "previous_include_pinned", "previous_* is for changed events only")
	assert.NotContains(t, setFrames[0].Data, "previous_window_seconds", "previous_* is for changed events only")
	tap.reset()

	var duePin string
	require.NoError(t, ts.DB.QueryRow(`
INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at, pinned_at, pinned_by, held_expires_at)
VALUES ($1, $2, 'ciphertext', 'user', NOW() - interval '25 hours', NOW() - interval '25 hours', $2, NOW() - interval '1 hour')
RETURNING id`, convID, alice.ID).Scan(&duePin))

	w = ts.DoRequest(http.MethodPatch, path,
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only", "include_pinned": true}, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var remaining int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM dm_messages WHERE id = $1`, duePin).Scan(&remaining))
	require.Zero(t, remaining, "the due pin is deleted before the 200")

	require.Eventually(t, func() bool { return len(tap.ofTypes("dm_expiration_event", "dm_purged")) >= 2 }, 3*time.Second, 20*time.Millisecond)
	tap.settle(t, 500*time.Millisecond)
	frames := tap.ofTypes("dm_expiration_event", "dm_purged")
	require.Len(t, frames, 2, "one policy broadcast and one purge announcement, nothing else")

	// §8 ordering: the policy broadcast first, the purge announcement second.
	assert.Equal(t, "dm_expiration_event", frames[0].Type, "the policy broadcast precedes the purge event")
	assert.Equal(t, "dm_purged", frames[1].Type)
	assert.Equal(t, convID, frames[1].Data["conversation_id"])

	// §6.4: a pins-only change is a "changed" event carrying the whole pin story.
	changed := frames[0].Data
	assert.Equal(t, "changed", changed["kind"])
	assert.Equal(t, true, changed["include_pinned"])
	assert.Equal(t, false, changed["previous_include_pinned"])
	assert.EqualValues(t, 86400, changed["previous_window_seconds"],
		"the previous window is what lets a client tell a pins-only change from a window change")
	assert.EqualValues(t, 86400, changed["window_seconds"])
}

// Converse of the ordering case: a toggle with nothing due announces the policy
// and NO purge, so the ordering assertion above is not satisfiable by a purge
// that fires unconditionally.
//
// Mutant killed: the expirer announcing a purge when DuePinnedIDs is empty.
func TestDMExpiration_PinnedToggleWithNothingDueAnnouncesNoPurge(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "pin_frames_quiet_alice")
	bob := ts.CreateTestUser(t, "pin_frames_quiet_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	auth := testhelpers.AuthHeaders(alice.AccessToken)
	path := dmExpirationPath + convID + "/expiration"

	tap := tapDMFrames(t, dialDMObserver(t, ts, bob.ID, convID))
	w := ts.DoRequest(http.MethodPatch, path,
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"}, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Eventually(t, func() bool { return len(tap.ofTypes("dm_expiration_event")) == 1 }, 3*time.Second, 20*time.Millisecond)
	tap.reset()

	w = ts.DoRequest(http.MethodPatch, path,
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only", "include_pinned": true}, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Positive gate first: the policy frame arrives, so the quiet window that
	// follows is observed by a live socket that was delivering.
	require.Eventually(t, func() bool { return len(tap.ofTypes("dm_expiration_event")) == 1 }, 3*time.Second, 20*time.Millisecond)
	tap.settle(t, 500*time.Millisecond)
	frames := tap.ofTypes("dm_expiration_event", "dm_purged")
	require.Len(t, frames, 1)
	assert.Equal(t, true, frames[0].Data["include_pinned"])
}
