package servers_test

// #3453 I-2, the side-effect half: flipping "Enforce MFA On Dangerous
// Actions" through the real PUT causes no voice permission recheck, no
// presence capture and no hub disconnect (invariant I4). Only the server's
// permission-generation bump and the Nightwatch success event may happen.
//
// Each effect is counted at a boundary the toggle's code does not own:
//
//   - voice rechecks: a NATS subscriber on voice.enforce.>, the subjects the
//     voice PermissionEnforcer publishes on, filtered to this fixture's voice
//     channel and participants so another package's traffic cannot count;
//   - presence captures: the hub's audience-revocation fence. Every revoking
//     capture rail (withAuthorityCapture, the graph-presence and active-plan
//     rails, account erasure) opens a bracket on it synchronously, which
//     advances its epoch, and so does every Rich Presence disconnect;
//   - hub disconnects: the hub's own per-user client count, read after a
//     sentinel DisconnectUser has drained the same FIFO queue.
//
// Residual, stated rather than hidden: an ADDITIVE graph-presence capture
// opens no bracket, and a DisconnectSession travels a queue the sentinel does
// not drain; neither is observed here.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// enforcerWorker names the goroutines voice.PermissionEnforcer runs its
// rechecks on, in a goroutine dump.
const enforcerWorker = "voice.(*PermissionEnforcer).startWorker"

// enforcerWorkerAlive reports whether any recheck goroutine is alive.
func enforcerWorkerAlive() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return bytes.Contains(buf[:n], []byte(enforcerWorker))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// waitForEnforcerIdle waits until no recheck goroutine is alive. A recheck
// dispatched inside the PUT has created its goroutine before the request
// returns (startWorker runs `go` synchronously), so once none is left every
// recheck the flips caused has run to completion. PermissionEnforcer.Close is
// NOT a drain: it cancels first, and a cancelled recheck returns silently
// without publishing.
func waitForEnforcerIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(lockProofBound)
	for enforcerWorkerAlive() {
		if time.Now().After(deadline) {
			t.Fatalf("a voice recheck was still running after %s", lockProofBound)
		}
		time.Sleep(time.Millisecond)
	}
}

// voiceEnforceBarrierSubject is published by the router's own NATS client
// after the flips. NATS delivers one connection's messages in order, so its
// arrival proves every earlier publish from that client has arrived too.
const voiceEnforceBarrierSubject = "voice.enforce.test-barrier"

// voiceEnforceSpy records voice.enforce.* messages that name this fixture's
// voice channel or one of its participants.
type voiceEnforceSpy struct {
	mu       sync.Mutex
	ids      map[string]bool
	messages []string
	barriers map[string]chan struct{}
}

func subscribeVoiceEnforcement(t *testing.T, env *mfaEnv, ids ...string) *voiceEnforceSpy {
	t.Helper()
	require.NotNil(t, env.nats, "the voice spy needs NATS")
	spy := &voiceEnforceSpy{ids: map[string]bool{}, barriers: map[string]chan struct{}{}}
	for _, id := range ids {
		spy.ids[id] = true
	}
	sub, err := env.nats.SubscribeWithSubject("voice.enforce.>", spy.record)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, env.nats.Flush(), "the subscription must be live before anything publishes")
	return spy
}

func (s *voiceEnforceSpy) record(subject string, data []byte) {
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if subject == voiceEnforceBarrierSubject {
		if token, ok := payload["token"].(string); ok {
			if ch, waiting := s.barriers[token]; waiting {
				close(ch)
				delete(s.barriers, token)
			}
		}
		return
	}
	for _, key := range []string{"channelId", "userId"} {
		if id, ok := payload[key].(string); ok && s.ids[id] {
			s.messages = append(s.messages, subject+" "+string(data))
			return
		}
	}
}

func (s *voiceEnforceSpy) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.messages
	s.messages = nil
	return out
}

// barrier publishes a unique token through the router's NATS client and waits
// for it to come back.
func (s *voiceEnforceSpy) barrier(t *testing.T, env *mfaEnv) {
	t.Helper()
	token := uuid.NewString()
	arrived := make(chan struct{})
	s.mu.Lock()
	s.barriers[token] = arrived
	s.mu.Unlock()
	require.NoError(t, env.nats.Publish(voiceEnforceBarrierSubject, map[string]string{"token": token}))
	select {
	case <-arrived:
	case <-time.After(lockProofBound):
		t.Fatalf("the NATS barrier did not come back within %s", lockProofBound)
	}
}

// dialRegisteredWS connects u and returns once the hub's Run loop has handled
// a frame from that client: a malformed subscribe_server is answered with an
// error frame, which the hub sends only after registering the client.
func dialRegisteredWS(t *testing.T, wsServer *httptest.Server, u testhelpers.TestUser) *websocket.Conn {
	t.Helper()
	headers := map[string][]string{"Authorization": {"Bearer " + u.AccessToken}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(wsServer.URL, "http")+"/api/v1/ws", headers)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	requireWSAnswers(t, conn)
	return conn
}

// requireWSAnswers round-trips one frame through the hub on conn.
func requireWSAnswers(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(lockProofBound)))
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "subscribe_server", "data": map[string]any{"server_id": "not-a-uuid"}}))
	for {
		var frame map[string]any
		require.NoError(t, conn.ReadJSON(&frame), "the socket must still answer")
		if frame["type"] == "error" {
			return
		}
	}
}

// requireWSClosed reads until the hub closes conn.
func requireWSClosed(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(lockProofBound)))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatalf("the sentinel disconnect did not land within %s", lockProofBound)
			}
			return
		}
	}
}

// Flipping the setting ON and then OFF, through the real PUT, publishes no
// voice.enforce.* message for the server's live voice participants, opens no
// presence revocation bracket and disconnects no member; it does bump the
// server's permission generation and emits exactly the two success events.
// A control first drives the real enforcer (RecheckServer) for this server,
// paused inside its participant query so its goroutine is provably alive, and
// requires the drain to see that goroutine and the spy to see both
// participants' messages: neither barrier nor spy can pass vacuously.
// Kills: a voice recheck, a presence capture or a hub disconnect wired into
// the toggle's path.
func TestMFAEnforcement_AFlipCausesNoVoiceRecheckPresenceCaptureOrDisconnect(t *testing.T) {
	sqlSpy, routerDB := openSQLStateSpyDB(t)
	env := setupMFAEnforcementEnvWithDB(t, routerDB)
	f := newMFAFixture(t, env, "mfafs", false)
	enrollMFATOTP(t, env, f.owner.ID)
	hub := env.ts.Hub

	voiceChannel := env.ts.CreateVoiceChannel(t, f.serverID, "flip-spy")
	for _, u := range []testhelpers.TestUser{f.admin, f.member} {
		insertVoiceParticipant(t, env.ts, voiceChannel, u.ID, time.Now())
	}
	voiceSpy := subscribeVoiceEnforcement(t, env, voiceChannel, f.admin.ID, f.member.ID)

	// Control: the enforcer's own server-wide recheck, held at its participant
	// query, is a live goroutine the drain sees; released, it reaches the spy
	// for both participants.
	pause := sqlSpy.armPause(t, "FROM voice_participants vp")
	env.enforcer.RecheckServer(f.serverID)
	select {
	case <-pause.reached:
	case <-time.After(lockProofBound):
		t.Fatal("control: the server recheck never queried its participants")
	}
	require.True(t, enforcerWorkerAlive(), "control: the drain must recognise a live recheck goroutine")
	pause.Release()
	waitForEnforcerIdle(t)
	voiceSpy.barrier(t, env)
	require.Len(t, voiceSpy.take(), 2, "control: a server recheck must publish for both voice participants")

	wsServer := httptest.NewServer(env.ts.Router)
	t.Cleanup(wsServer.Close)
	observers := map[string]testhelpers.TestUser{"owner": f.owner, "admin": f.admin, "member": f.member}
	conns := map[string]*websocket.Conn{}
	for name, u := range observers {
		conns[name] = dialRegisteredWS(t, wsServer, u)
		require.Equal(t, 1, hub.GetUserClientCount(uuid.MustParse(u.ID)), "precondition: %s is connected once", name)
	}
	sentinel := dialRegisteredWS(t, wsServer, f.outsider)

	epoch0, open0 := hub.PresenceAuthzEpochForTest(), hub.PresenceAuthzOpenForTest()
	require.Zero(t, open0, "precondition: no revocation is in flight")
	gen0 := serverGeneration(t, env, f.serverID, f.owner.ID)
	env.events.take()

	w := putMFA(env, f.owner, f.serverID, bodyOn())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = putMFA(env, f.owner, f.serverID, bodyOff(mfaBackupCode))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Drain every asynchronous path before judging silence: every recheck the
	// flips dispatched runs to completion, the NATS barrier delivers everything
	// they published, and the sentinel's DisconnectUser is queued behind any
	// DisconnectUser the flips queued.
	waitForEnforcerIdle(t)
	voiceSpy.barrier(t, env)
	hub.DisconnectUser(uuid.MustParse(f.outsider.ID))
	requireWSClosed(t, sentinel)

	assert.Empty(t, voiceSpy.take(), "a flip must trigger no voice permission recheck")
	assert.Equal(t, epoch0, hub.PresenceAuthzEpochForTest(), "a flip must open no presence revocation bracket")
	assert.Zero(t, hub.PresenceAuthzOpenForTest(), "a flip must leave no revocation open")
	for name, u := range observers {
		assert.Equal(t, 1, hub.GetUserClientCount(uuid.MustParse(u.ID)), "a flip must not disconnect %s", name)
		requireWSAnswers(t, conns[name])
	}

	assert.NotEqual(t, gen0, serverGeneration(t, env, f.serverID, f.owner.ID), "the flips must bump the server generation")
	events := env.events.take()
	require.Len(t, events, 2, "exactly the two success events")
	for i, reason := range []securityevent.ReasonCode{
		securityevent.ReasonServerMFAEnforcementEnabled, securityevent.ReasonServerMFAEnforcementDisabled,
	} {
		assert.Equal(t, securityevent.OutcomeSuccess, events[i].Outcome)
		assert.Equal(t, reason, events[i].ReasonCode)
	}
}
