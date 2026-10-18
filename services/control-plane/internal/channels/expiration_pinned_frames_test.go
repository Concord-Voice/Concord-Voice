package channels_test

// #3459 §6.4 and §8 at the channel route, observed where a client observes
// them: on a real WebSocket, through the real router. Two properties that a
// handler-level assertion on the notifier or on the helper alone cannot see:
//
//   - the expiration_event frame carries include_pinned on EVERY event, and
//     previous_include_pinned + previous_window_seconds on a "changed" event only;
//   - the immediate pinned deletion announces exactly one server_purged beside it.
//
// Which of those two frames arrives first is deliberately NOT asserted. §8 orders
// what the handler ISSUES (broadcast, then deletion), not what a client receives,
// and the hub has nothing that could carry the second promise. The policy frame
// is a BroadcastToChannelAuthorized: the run loop hands it to a worker goroutine
// that checks each recipient's view permission against the database
// (dispatchChannelDelivery -> checkChannelDeliveryPermissions). server_purged
// takes the other lane, BroadcastToServerContext -> serverBroadcast, which the
// run loop delivers inline. Two lanes with no shared FIFO, so whichever finishes
// first is delivered first, and under CPU load the purge wins often enough to
// fail an order assertion. A client that sees the purge first loses nothing: its
// refetch returns server truth, and the policy event, fenced by revision, lands
// moments later (the renderer's useWebSocketMessages.purge.test.ts pins every
// arrival order). The issue order itself is pinned in
// expiration_pinned_expiry_test.go, by
// TestChannelExpiration_PolicyIsAnnouncedWhileTheDueDeletionIsStillHeld.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

type observedFrame struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

// frameTap is a live observer: one goroutine owns every read on the socket (a
// gorilla connection is unusable after a read deadline fires, so "drain, act,
// drain again" on the same conn cannot work) and records each frame in arrival
// order. A read error other than the cleanup close is kept, so a dead observer
// fails the test instead of reading as "no more frames".
type frameTap struct {
	mu     sync.Mutex
	frames []observedFrame
	err    error
	closed bool
}

func (f *frameTap) record(frame observedFrame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, frame)
}

func (f *frameTap) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.err = err
	}
}

func (f *frameTap) shutdown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

// ofTypes returns the recorded frames whose type is in want, in arrival order.
func (f *frameTap) ofTypes(want ...string) []observedFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []observedFrame
	for _, frame := range f.frames {
		for _, w := range want {
			if frame.Type == w {
				out = append(out, frame)
			}
		}
	}
	return out
}

// reset forgets everything recorded so far.
func (f *frameTap) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = nil
}

// settle waits out a quiet window and then requires the observer to still be
// alive, which is what makes a negative assertion made afterwards mean
// something: a socket that died would otherwise look identical to a quiet one.
func (f *frameTap) settle(t *testing.T, window time.Duration) {
	t.Helper()
	time.Sleep(window)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NoError(t, f.err, "the frame observer died before the window ended")
}

// dialChannelFrameObserver opens a WebSocket for user subscribed to serverID and
// channelID (server_purged rides the server subscription, expiration_event the
// channel's), waits for the subscribe confirmation, and starts the tap. The
// channel "subscribed" confirmation also proves the earlier subscribe_server was
// applied: the hub drains one client's frames in order.
func dialChannelFrameObserver(t *testing.T, ts *testhelpers.TestServer, user testhelpers.TestUser, serverID, channelID string) *frameTap {
	t.Helper()
	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	conn, _, err := gorillaWS.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(wsServer.URL, "http")+"/api/v1/ws?token="+url.QueryEscape(user.AccessToken), nil)
	require.NoError(t, err)
	tap := &frameTap{}
	t.Cleanup(func() {
		tap.shutdown()
		_ = conn.Close()
	})
	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(user.ID)) > 0
	}, 2*time.Second, 10*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	require.NoError(t, conn.SetReadDeadline(deadline))
	_, _, err = conn.ReadMessage() // connection bootstrap
	require.NoError(t, err)
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "subscribe_server", "data": map[string]string{"server_id": serverID}}))
	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"type": "subscribe", "data": map[string]string{"channel_id": channelID}}))
	for {
		require.NoError(t, conn.SetReadDeadline(deadline))
		_, raw, readErr := conn.ReadMessage()
		require.NoError(t, readErr, "timed out waiting for the channel subscribe confirmation")
		var frame observedFrame
		require.NoError(t, json.Unmarshal(raw, &frame))
		if frame.Type == "subscribed" {
			break
		}
	}
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
	go func() {
		for {
			_, raw, readErr := conn.ReadMessage()
			if readErr != nil {
				tap.fail(readErr)
				return
			}
			var frame observedFrame
			if json.Unmarshal(raw, &frame) == nil {
				tap.record(frame)
			}
		}
	}()
	return tap
}

func insertDuePinnedChannelMessage(t *testing.T, ts *testhelpers.TestServer, channelID, authorID string) string {
	t.Helper()
	var id string
	require.NoError(t, ts.DB.QueryRow(`
INSERT INTO messages (channel_id, user_id, content, key_version, created_at, pinned_at, pinned_by, held_expires_at)
VALUES ($1, $2, 'ciphertext', 1, NOW() - interval '25 hours', NOW() - interval '25 hours', $2, NOW() - interval '1 hour')
RETURNING id`, channelID, authorID).Scan(&id))
	return id
}

// Mutant killed: AddPinnedEventFields dropping include_pinned,
// previous_include_pinned or previous_window_seconds from the live frame.
func TestChannelExpiration_PinnedToggleFramesCarryPinFields(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "pin_frames_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "pin-frames-server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	auth := testhelpers.AuthHeaders(owner.AccessToken)
	path := "/api/v1/channels/" + channelID + "/expiration"

	tap := dialChannelFrameObserver(t, ts, owner, serverID, channelID)

	// A new timer with the field omitted is E=false: the "set" frame still
	// announces include_pinned (false), and carries no previous_* member.
	w := ts.DoRequest(http.MethodPatch, path,
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"}, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Eventually(t, func() bool { return len(tap.ofTypes("expiration_event")) == 1 }, 3*time.Second, 20*time.Millisecond)
	tap.settle(t, 500*time.Millisecond)
	setFrames := tap.ofTypes("expiration_event", "server_purged")
	require.Len(t, setFrames, 1, "creating the timer announces the policy and purges nothing")
	require.Equal(t, "expiration_event", setFrames[0].Type)
	assert.Equal(t, "set", setFrames[0].Data["kind"])
	includePinned, present := setFrames[0].Data["include_pinned"]
	assert.True(t, present, "include_pinned rides every event so the client merge can advance")
	assert.Equal(t, false, includePinned)
	assert.NotContains(t, setFrames[0].Data, "previous_include_pinned", "previous_* is for changed events only")
	assert.NotContains(t, setFrames[0].Data, "previous_window_seconds", "previous_* is for changed events only")

	tap.reset()

	// A due pin exists, then include_pinned flips ON over the same window.
	duePin := insertDuePinnedChannelMessage(t, ts, channelID, owner.ID)
	w = ts.DoRequest(http.MethodPatch, path,
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only", "include_pinned": true}, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.False(t, channelMessageExists(t, ts, duePin), "the due pin is deleted before the 200")

	require.Eventually(t, func() bool { return len(tap.ofTypes("expiration_event", "server_purged")) >= 2 }, 3*time.Second, 20*time.Millisecond)
	tap.settle(t, 500*time.Millisecond)
	frames := tap.ofTypes("expiration_event", "server_purged")
	types := make([]string, 0, len(frames))
	dataOf := make(map[string]map[string]interface{}, len(frames))
	for _, frame := range frames {
		types = append(types, frame.Type)
		dataOf[frame.Type] = frame.Data
	}
	require.ElementsMatch(t, []string{"expiration_event", "server_purged"}, types,
		"one policy broadcast and one purge announcement, nothing else, in either order")
	assert.Equal(t, serverID, dataOf["server_purged"]["server_id"])

	// §6.4: a pins-only change is a "changed" event carrying the whole pin story.
	changed := dataOf["expiration_event"]
	assert.Equal(t, "changed", changed["kind"])
	assert.Equal(t, true, changed["include_pinned"])
	assert.Equal(t, false, changed["previous_include_pinned"])
	assert.EqualValues(t, 86400, changed["previous_window_seconds"],
		"the previous window is what lets a client tell a pins-only change from a window change")
	assert.EqualValues(t, 86400, changed["window_seconds"])
}

// Converse of the case above: a toggle with nothing due announces the policy and
// NO purge, so that case's "one purge announcement" is not satisfiable by a purge
// that fires unconditionally.
//
// Mutant killed: the expirer announcing a purge when DuePinnedIDs is empty.
func TestChannelExpiration_PinnedToggleWithNothingDueAnnouncesNoPurge(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "pin_frames_quiet_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "pin-frames-quiet-server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	auth := testhelpers.AuthHeaders(owner.AccessToken)
	path := "/api/v1/channels/" + channelID + "/expiration"

	tap := dialChannelFrameObserver(t, ts, owner, serverID, channelID)
	w := ts.DoRequest(http.MethodPatch, path,
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"}, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Eventually(t, func() bool { return len(tap.ofTypes("expiration_event")) == 1 }, 3*time.Second, 20*time.Millisecond)
	tap.reset()

	w = ts.DoRequest(http.MethodPatch, path,
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only", "include_pinned": true}, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Positive gate first: the policy frame arrives, so the quiet window that
	// follows is observed by a live socket that was delivering.
	require.Eventually(t, func() bool { return len(tap.ofTypes("expiration_event")) == 1 }, 3*time.Second, 20*time.Millisecond)
	tap.settle(t, 500*time.Millisecond)
	frames := tap.ofTypes("expiration_event", "server_purged")
	require.Len(t, frames, 1)
	assert.Equal(t, "expiration_event", frames[0].Type)
	assert.Equal(t, true, frames[0].Data["include_pinned"])
}
