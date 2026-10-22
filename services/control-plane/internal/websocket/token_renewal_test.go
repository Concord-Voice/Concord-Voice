package websocket

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	ws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// In-band control-socket token renewal (token_renewal.go). Every refusal case
// starts from the same accepting fixture and changes exactly one input, so the
// guard the case names is the only thing that can produce the refusal. Each
// case also pins which authority calls ran and the logged reason class, so two
// refusals that share ok:false still have distinct observables.

const renewalTestToken = "test-renewal-token-123-DO-NOT-LOG" //nolint:gosec // test value, not a credential

var errRenewalInjected = errors.New("injected renewal failure")

// fakeRenewalAuthority records every call and returns canned answers.
type fakeRenewalAuthority struct {
	mu sync.Mutex

	verified    accessTokenAuth
	validateErr error

	predecessor    string
	predecessorErr error

	revoked    bool
	revokedErr error

	validateCalls    int
	predecessorCalls int
	revokedCalls     int
	predecessorSIDs  []string
}

func (f *fakeRenewalAuthority) validateAccessToken(_ context.Context, _ string) (accessTokenAuth, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validateCalls++
	return f.verified, f.validateErr
}

func (f *fakeRenewalAuthority) sessionPredecessor(_ context.Context, _ uuid.UUID, sessionID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.predecessorCalls++
	f.predecessorSIDs = append(f.predecessorSIDs, sessionID)
	return f.predecessor, f.predecessorErr
}

func (f *fakeRenewalAuthority) sessionRevoked(_ context.Context, _ uuid.UUID, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokedCalls++
	return f.revoked, f.revokedErr
}

func (f *fakeRenewalAuthority) calls() (validate, predecessor, revoked int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.validateCalls, f.predecessorCalls, f.revokedCalls
}

// syncLogBuffer is a goroutine-safe log sink.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureRenewalLog(t *testing.T) *syncLogBuffer {
	t.Helper()
	buf := &syncLogBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// renewalSocketPair returns a server-side connection and the dialer's end.
func renewalSocketPair(t *testing.T) (*ws.Conn, *ws.Conn) {
	t.Helper()
	ready := make(chan *ws.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&ws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		ready <- conn
	}))
	t.Cleanup(srv.Close)
	peer, _, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	server := <-ready
	t.Cleanup(func() { _ = server.Close() })
	return server, peer
}

type peerEvent struct {
	frame string
	err   error
}

// peerReader surfaces every text frame and the terminal read error.
func peerReader(peer *ws.Conn) <-chan peerEvent {
	events := make(chan peerEvent, 32)
	go func() {
		defer close(events)
		for {
			_, data, err := peer.ReadMessage()
			if err != nil {
				events <- peerEvent{err: err}
				return
			}
			events <- peerEvent{frame: string(data)}
		}
	}()
	return events
}

// stoppedHub is a Hub whose Run is not running; done is closed at cleanup so
// any requestUnregister cannot block.
func stoppedHub(t *testing.T) *Hub {
	t.Helper()
	hub := NewHub(nil, nil)
	t.Cleanup(func() { hub.closeOnce.Do(func() { close(hub.done) }) })
	return hub
}

// runningHub is a Hub whose Run loop services rebindSession.
func runningHub(t *testing.T) *Hub {
	t.Helper()
	hub := NewHub(nil, setupHubTestRedis(t))
	return hub
}

func startHub(t *testing.T, hub *Hub) {
	t.Helper()
	go hub.Run()
	t.Cleanup(func() { hub.closeOnce.Do(func() { close(hub.done) }) })
}

type renewalFixture struct {
	client    *Client
	authority *fakeRenewalAuthority
	deadline  time.Time
	renewedTo time.Time
}

// newRenewalFixture builds a client and an authority that ACCEPTS a same-sid
// renewal. Cases mutate exactly one field.
func newRenewalFixture(hub *Hub, conn *ws.Conn) *renewalFixture {
	deadline := time.Now().Add(time.Minute)
	client := &Client{
		ID:             uuid.New(),
		UserID:         uuid.New(),
		SessionID:      uuid.NewString(),
		CredEpoch:      "epoch-1",
		TokenExpiresAt: deadline,
		Hub:            hub,
		Conn:           conn,
		Send:           make(chan []byte, 8),
	}
	renewedTo := deadline.Add(15 * time.Minute)
	authority := &fakeRenewalAuthority{
		verified: accessTokenAuth{
			UserID:    client.UserID,
			SessionID: client.SessionID,
			CredEpoch: client.CredEpoch,
			ExpiresAt: renewedTo,
		},
	}
	client.renewAuthority = authority
	return &renewalFixture{client: client, authority: authority, deadline: deadline, renewedTo: renewedTo}
}

func drainSend(t *testing.T, c *Client) string {
	t.Helper()
	select {
	case frame := <-c.Send:
		return string(frame)
	default:
		t.Fatal("no frame was enqueued on Send")
		return ""
	}
}

func TestRenewToken_AcceptsSameSessionAndExtendsDeadline(t *testing.T) {
	t.Run("handleTokenRenewal extends and acks ok:true", func(t *testing.T) {
		logs := captureRenewalLog(t)
		conn, _ := renewalSocketPair(t)
		hub := runningHub(t)
		startHub(t, hub)
		f := newRenewalFixture(hub, conn)

		f.client.handleTokenRenewal(IncomingMessage{Type: authRenewMessageType, Data: map[string]interface{}{"token": renewalTestToken}})

		assert.Equal(t, f.renewedTo, f.client.tokenDeadline())
		assert.JSONEq(t, `{"type":"auth_renew_result","data":{"ok":true}}`, drainSend(t, f.client))
		validate, predecessor, revoked := f.authority.calls()
		assert.Equal(t, 1, validate)
		assert.Zero(t, predecessor, "an idempotent same-sid resend needs no lineage lookup")
		assert.Equal(t, 1, revoked, "a same-sid resend is re-checked for revocation too")
		assert.NotContains(t, logs.String(), renewalTestToken)
	})

	t.Run("throttle window has elapsed: second renewal accepted", func(t *testing.T) {
		f := newRenewalFixture(stoppedHub(t), nil)
		f.client.lastRenewalAttempt = time.Now().Add(-tokenRenewalMinInterval - time.Second)
		require.True(t, f.client.renewToken(renewalTestToken))
		assert.Equal(t, f.renewedTo, f.client.tokenDeadline())
	})
}

func TestRenewToken_RefusalsLeaveDeadlineUnchanged(t *testing.T) {
	type expect struct {
		validate, predecessor, revoked int
		reason                         string // "" = no log line expected
	}
	cases := []struct {
		name   string
		mutate func(f *renewalFixture)
		want   expect
	}{
		{
			name:   "nil authority",
			mutate: func(f *renewalFixture) { f.client.renewAuthority = nil },
			want:   expect{},
		},
		{
			name:   "nil hub",
			mutate: func(f *renewalFixture) { f.client.Hub = nil },
			want:   expect{},
		},
		{
			name: "legacy socket with no session (lineage of a NULL-predecessor sid would otherwise match)",
			mutate: func(f *renewalFixture) {
				f.client.SessionID = ""
				f.authority.verified.SessionID = uuid.NewString()
				f.authority.predecessor = "" // a row with NULL predecessor_id
			},
			want: expect{reason: "mismatch_session"},
		},
		{
			name:   "validator error",
			mutate: func(f *renewalFixture) { f.authority.validateErr = errRenewalInjected },
			want:   expect{validate: 1, reason: "invalid"},
		},
		{
			name:   "user mismatch",
			mutate: func(f *renewalFixture) { f.authority.verified.UserID = uuid.New() },
			want:   expect{validate: 1, reason: "mismatch_user"},
		},
		{
			name:   "credential epoch mismatch",
			mutate: func(f *renewalFixture) { f.authority.verified.CredEpoch = "epoch-2" },
			want:   expect{validate: 1, reason: "mismatch_epoch"},
		},
		{
			name: "token carries no sid (lineage lookup would otherwise vouch for it)",
			mutate: func(f *renewalFixture) {
				f.authority.verified.SessionID = ""
				f.authority.predecessor = f.client.SessionID
			},
			want: expect{validate: 1, reason: "mismatch_session"},
		},
		{
			name: "rotated sid whose predecessor is a sibling session",
			mutate: func(f *renewalFixture) {
				f.authority.verified.SessionID = uuid.NewString()
				f.authority.predecessor = uuid.NewString()
			},
			want: expect{validate: 1, predecessor: 1, reason: "mismatch_session"},
		},
		{
			name: "rotated sid not found (a stale predecessor value is never trusted)",
			mutate: func(f *renewalFixture) {
				f.authority.verified.SessionID = uuid.NewString()
				f.authority.predecessor = f.client.SessionID
				f.authority.predecessorErr = errSessionNotFound
			},
			want: expect{validate: 1, predecessor: 1, reason: "mismatch_session"},
		},
		{
			name: "rotated sid lineage lookup unavailable",
			mutate: func(f *renewalFixture) {
				f.authority.verified.SessionID = uuid.NewString()
				f.authority.predecessor = f.client.SessionID
				f.authority.predecessorErr = errRenewalInjected
			},
			want: expect{validate: 1, predecessor: 1, reason: "unavailable"},
		},
		{
			name:   "exp equal to current deadline",
			mutate: func(f *renewalFixture) { f.authority.verified.ExpiresAt = f.deadline },
			want:   expect{validate: 1, reason: "not_later"},
		},
		{
			name:   "exp earlier than current deadline",
			mutate: func(f *renewalFixture) { f.authority.verified.ExpiresAt = f.deadline.Add(-time.Second) },
			want:   expect{validate: 1, reason: "not_later"},
		},
		{
			name: "deadline already passed",
			mutate: func(f *renewalFixture) {
				f.deadline = time.Now().Add(-time.Second)
				f.client.TokenExpiresAt = f.deadline
			},
			want: expect{validate: 1, reason: "not_later"},
		},
		{
			name:   "expiry stopped by teardown",
			mutate: func(f *renewalFixture) { f.client.stopTokenExpiry() },
			want:   expect{validate: 1, reason: "not_later"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureRenewalLog(t)
			conn, _ := renewalSocketPair(t)
			// A running hub: a mutant that wrongly accepts a rotated sid completes
			// its rebind and fails on the assertions below instead of hanging.
			hub := runningHub(t)
			startHub(t, hub)
			f := newRenewalFixture(hub, conn)
			tc.mutate(f)
			sessionBefore := f.client.SessionID

			f.client.handleTokenRenewal(IncomingMessage{Type: authRenewMessageType, Data: map[string]interface{}{"token": renewalTestToken}})

			assert.Equal(t, f.deadline, f.client.tokenDeadline(), "a refused renewal must not move the deadline")
			assert.Equal(t, sessionBefore, f.client.SessionID, "a refused renewal must not rebind")
			assert.JSONEq(t, `{"type":"auth_renew_result","data":{"ok":false}}`, drainSend(t, f.client))
			validate, predecessor, revoked := f.authority.calls()
			assert.Equal(t, tc.want.validate, validate, "validator calls")
			assert.Equal(t, tc.want.predecessor, predecessor, "lineage lookups")
			assert.Equal(t, tc.want.revoked, revoked, "revocation re-checks")
			if tc.want.reason != "" {
				assert.Contains(t, logs.String(), "token renewal refused for user "+f.client.UserID.String()+": "+tc.want.reason+"\n")
			} else {
				assert.NotContains(t, logs.String(), "token renewal refused")
			}
			assert.NotContains(t, logs.String(), renewalTestToken)
		})
	}

	t.Run("empty token is refused without validation", func(t *testing.T) {
		for _, data := range []map[string]interface{}{{"token": ""}, {}, {"token": 42}} {
			f := newRenewalFixture(stoppedHub(t), nil)
			f.client.handleTokenRenewal(IncomingMessage{Type: authRenewMessageType, Data: data})
			assert.Equal(t, f.deadline, f.client.tokenDeadline())
			assert.JSONEq(t, `{"type":"auth_renew_result","data":{"ok":false}}`, drainSend(t, f.client))
			validate, _, _ := f.authority.calls()
			assert.Zero(t, validate)
		}
	})
}

func TestRenewToken_ThrottlesSecondAttemptWithinWindow(t *testing.T) {
	logs := captureRenewalLog(t)
	f := newRenewalFixture(stoppedHub(t), nil)
	require.True(t, f.client.renewToken(renewalTestToken), "first renewal is accepted")
	require.Equal(t, f.renewedTo, f.client.tokenDeadline())

	// The second token is otherwise fully valid and later still.
	later := f.renewedTo.Add(15 * time.Minute)
	f.authority.mu.Lock()
	f.authority.verified.ExpiresAt = later
	f.authority.mu.Unlock()

	assert.False(t, f.client.renewToken(renewalTestToken))
	assert.Equal(t, f.renewedTo, f.client.tokenDeadline(), "a throttled renewal must not move the deadline")
	validate, _, _ := f.authority.calls()
	assert.Equal(t, 1, validate, "a throttled attempt does no validation work")
	assert.Contains(t, logs.String(), ": throttled\n")
	for range 5 {
		assert.False(t, f.client.renewToken(renewalTestToken))
	}
	assert.Equal(t, 1, strings.Count(logs.String(), ": throttled\n"), "throttling logs once per window, not once per frame")

	t.Run("a refused attempt also starts the window", func(t *testing.T) {
		g := newRenewalFixture(stoppedHub(t), nil)
		g.authority.validateErr = errRenewalInjected
		require.False(t, g.client.renewToken(renewalTestToken))
		g.authority.mu.Lock()
		g.authority.validateErr = nil
		g.authority.mu.Unlock()
		assert.False(t, g.client.renewToken(renewalTestToken), "a valid token right after a refusal is still throttled")
		assert.Equal(t, g.deadline, g.client.tokenDeadline())
	})
}

func TestRenewToken_RotatedSessionRebindsThroughHubRun(t *testing.T) {
	t.Run("rebinds SessionID and a revocation of the NEW sid evicts the socket", func(t *testing.T) {
		hub := runningHub(t)
		f := newRenewalFixture(hub, nil)
		oldSID := f.client.SessionID
		newSID := uuid.NewString()
		f.authority.verified.SessionID = newSID
		f.authority.predecessor = oldSID
		// Registered before Run starts, so no lock is needed for the seed.
		hub.clients[f.client.ID] = f.client
		startHub(t, hub)

		require.True(t, f.client.renewToken(renewalTestToken))
		assert.Equal(t, f.renewedTo, f.client.tokenDeadline())
		assert.Equal(t, newSID, f.client.SessionID, "Run must rebind the socket to its successor session")
		_, predecessor, revoked := f.authority.calls()
		assert.Equal(t, 1, predecessor)
		assert.Equal(t, []string{newSID}, f.authority.predecessorSIDs, "lineage is looked up by the TOKEN's sid")
		assert.Equal(t, 1, revoked, "the new sid is re-checked for revocation after the rebind")

		// The consumer of the rebind: revoking the NEW sid evicts the socket.
		hub.DisconnectSession(newSID)
		require.Eventually(t, func() bool {
			hub.mu.RLock()
			defer hub.mu.RUnlock()
			_, ok := hub.clients[f.client.ID]
			return !ok
		}, 2*time.Second, 10*time.Millisecond, "DisconnectSession(new sid) must evict the renewed socket")
		// forceRevoke queues the session_revoked frame before closeOutbound.
		frame, ok := <-f.client.Send
		require.True(t, ok, "the revocation frame must be queued before Send closes")
		assert.Contains(t, string(frame), sessionRevoked)
	})

	t.Run("old sid revocation alone does not evict a rebound socket", func(t *testing.T) {
		hub := runningHub(t)
		f := newRenewalFixture(hub, nil)
		oldSID := f.client.SessionID
		f.authority.verified.SessionID = uuid.NewString()
		f.authority.predecessor = oldSID
		hub.clients[f.client.ID] = f.client
		startHub(t, hub)

		require.True(t, f.client.renewToken(renewalTestToken))
		hub.DisconnectSession(oldSID)
		// Once Run has received the disconnect it handles it before its next
		// select, so a probe rebind round-trip after the drain orders after it.
		require.Eventually(t, func() bool { return len(hub.disconnectSession) == 0 }, time.Second, 5*time.Millisecond)
		probe2 := sessionRebind{client: &Client{}, sessionID: "probe", done: make(chan struct{})}
		hub.rebindSession <- probe2
		<-probe2.done
		hub.mu.RLock()
		_, still := hub.clients[f.client.ID]
		hub.mu.RUnlock()
		assert.True(t, still, "the old sid no longer owns the socket after a rebind")
	})
}

func TestRenewToken_PostRebindRevocationClosesConnection(t *testing.T) {
	cases := []struct {
		name       string
		revoked    bool
		revokedErr error
	}{
		{name: "new sid revoked", revoked: true},
		{name: "revocation re-check unavailable", revokedErr: errRenewalInjected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureRenewalLog(t)
			hub := runningHub(t)
			conn, peer := renewalSocketPair(t)
			events := peerReader(peer)
			f := newRenewalFixture(hub, conn)
			newSID := uuid.NewString()
			f.authority.verified.SessionID = newSID
			f.authority.predecessor = f.client.SessionID
			f.authority.revoked = tc.revoked
			f.authority.revokedErr = tc.revokedErr
			// Registered before Run starts; Run's unregister arm removes it.
			hub.clients[f.client.ID] = f.client
			startHub(t, hub)

			assert.False(t, f.client.renewToken(renewalTestToken))
			assert.Equal(t, newSID, f.client.SessionID, "the rebind happened before the re-check")
			assert.Equal(t, f.deadline, f.client.tokenDeadline(), "the deadline moves only after the re-check passes")
			f.client.deadlineMu.Lock()
			stopped := f.client.tokenExpiryStopped
			f.client.deadlineMu.Unlock()
			assert.True(t, stopped, "the expiry timer is stopped so it cannot re-arm")
			require.Eventually(t, func() bool {
				hub.mu.RLock()
				defer hub.mu.RUnlock()
				_, ok := hub.clients[f.client.ID]
				return !ok
			}, 2*time.Second, 10*time.Millisecond, "the socket must be unregistered")
			select {
			case ev := <-events:
				require.Error(t, ev.err, "the transport must close, not deliver a frame")
			case <-time.After(2 * time.Second):
				t.Fatal("the revoked socket's transport was not closed")
			}
			assert.Contains(t, logs.String(), ": revoked_or_unavailable\n")
			assert.NotContains(t, logs.String(), renewalTestToken)
		})
	}

	t.Run("hub shut down before the rebind is serviced", func(t *testing.T) {
		hub := NewHub(nil, nil) // Run never starts
		f := newRenewalFixture(hub, nil)
		f.authority.verified.SessionID = uuid.NewString()
		f.authority.predecessor = f.client.SessionID
		hub.closeOnce.Do(func() { close(hub.done) })
		result := make(chan bool, 1)
		go func() { result <- f.client.renewToken(renewalTestToken) }()
		select {
		case ok := <-result:
			assert.False(t, ok)
		case <-time.After(2 * time.Second):
			t.Fatal("renewal blocked on a hub that is shutting down")
		}
		_, _, revoked := f.authority.calls()
		assert.Zero(t, revoked)
	})
}

func TestExtendDeadline_Guards(t *testing.T) {
	now := time.Now()
	t.Run("later exp on a live socket is accepted", func(t *testing.T) {
		c := &Client{TokenExpiresAt: now.Add(time.Minute)}
		require.True(t, c.extendDeadline(now.Add(2*time.Minute)))
		assert.Equal(t, now.Add(2*time.Minute), c.tokenDeadline())
	})
	refusals := []struct {
		name    string
		current time.Time
		exp     time.Time
		stopped bool
	}{
		{name: "equal exp", current: now.Add(time.Minute), exp: now.Add(time.Minute)},
		{name: "earlier exp", current: now.Add(time.Minute), exp: now.Add(30 * time.Second)},
		{name: "deadline already passed", current: now.Add(-time.Millisecond), exp: now.Add(time.Hour)},
		{name: "expiry stopped", current: now.Add(time.Minute), exp: now.Add(time.Hour), stopped: true},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{TokenExpiresAt: tc.current}
			if tc.stopped {
				c.stopTokenExpiry()
			}
			assert.False(t, c.extendDeadline(tc.exp))
			assert.Equal(t, tc.current, c.tokenDeadline())
		})
	}
}

func TestOnTokenExpiryTimer(t *testing.T) {
	t.Run("re-arms after a renewal and closes at the NEW deadline", func(t *testing.T) {
		conn, peer := renewalSocketPair(t)
		events := peerReader(peer)
		hub := stoppedHub(t)
		start := time.Now()
		c := &Client{ID: uuid.New(), UserID: uuid.New(), Conn: conn, Hub: hub, TokenExpiresAt: start.Add(200 * time.Millisecond)}
		registered := make(chan struct{})
		c.deadlineMu.Lock()
		c.tokenExpiryTimer = time.AfterFunc(200*time.Millisecond, func() { c.onTokenExpiryTimer(registered) })
		c.deadlineMu.Unlock()
		require.True(t, c.extendDeadline(start.Add(900*time.Millisecond)))

		select {
		case ev := <-events:
			t.Fatalf("socket closed or wrote before the renewed deadline: %+v (after %s)", ev, time.Since(start))
		case <-time.After(600 * time.Millisecond):
		}
		select {
		case ev := <-events:
			require.Error(t, ev.err)
			assert.True(t, ws.IsCloseError(ev.err, ws.ClosePolicyViolation), "expiry close: %v", ev.err)
			assert.GreaterOrEqual(t, time.Since(start), 850*time.Millisecond, "closed before the renewed deadline")
		case <-time.After(2 * time.Second):
			t.Fatal("re-armed timer never closed the socket at the renewed deadline")
		}
	})

	t.Run("closes and unregisters when the deadline has passed", func(t *testing.T) {
		conn, peer := renewalSocketPair(t)
		events := peerReader(peer)
		hub := NewHub(nil, nil)
		t.Cleanup(func() { hub.closeOnce.Do(func() { close(hub.done) }) })
		c := &Client{ID: uuid.New(), UserID: uuid.New(), Conn: conn, Hub: hub, TokenExpiresAt: time.Now().Add(-time.Millisecond)}
		c.tokenExpiryTimer = time.NewTimer(time.Hour)
		registered := make(chan struct{})
		close(registered)
		go c.onTokenExpiryTimer(registered)
		select {
		case got := <-hub.unregister:
			assert.Same(t, c, got)
		case <-time.After(2 * time.Second):
			t.Fatal("expired registered socket was not unregistered")
		}
		ev := <-events
		assert.True(t, ws.IsCloseError(ev.err, ws.ClosePolicyViolation), "expiry close: %v", ev.err)
	})

	t.Run("closes without unregistering before registration", func(t *testing.T) {
		conn, peer := renewalSocketPair(t)
		events := peerReader(peer)
		hub := NewHub(nil, nil)
		t.Cleanup(func() { hub.closeOnce.Do(func() { close(hub.done) }) })
		c := &Client{ID: uuid.New(), UserID: uuid.New(), Conn: conn, Hub: hub, TokenExpiresAt: time.Now().Add(-time.Millisecond)}
		c.onTokenExpiryTimer(make(chan struct{}))
		ev := <-events
		assert.True(t, ws.IsCloseError(ev.err, ws.ClosePolicyViolation), "expiry close: %v", ev.err)
		select {
		case <-hub.unregister:
			t.Fatal("an unregistered socket must not be sent to unregister")
		default:
		}
	})

	t.Run("future deadline with no timer closes (nothing to re-arm)", func(t *testing.T) {
		c := &Client{TokenExpiresAt: time.Now().Add(time.Minute)}
		assert.False(t, c.rearmTokenExpiry())
	})

	t.Run("does nothing once stopped", func(t *testing.T) {
		conn, peer := renewalSocketPair(t)
		events := peerReader(peer)
		hub := NewHub(nil, nil)
		t.Cleanup(func() { hub.closeOnce.Do(func() { close(hub.done) }) })
		c := &Client{ID: uuid.New(), UserID: uuid.New(), Conn: conn, Hub: hub, TokenExpiresAt: time.Now().Add(-time.Millisecond)}
		c.tokenExpiryTimer = time.NewTimer(time.Hour)
		c.stopTokenExpiry()
		unregistered := make(chan struct{}, 1)
		go func() {
			select {
			case <-hub.unregister:
				unregistered <- struct{}{}
			case <-hub.done:
			}
		}()
		registered := make(chan struct{})
		close(registered)
		c.onTokenExpiryTimer(registered)
		// Live observer: the socket is still open, proven by a frame that arrives.
		require.NoError(t, conn.WriteMessage(ws.TextMessage, []byte(`{"type":"still_open"}`)))
		select {
		case ev := <-events:
			require.NoError(t, ev.err, "a stopped timer must not close the socket")
			assert.JSONEq(t, `{"type":"still_open"}`, ev.frame)
		case <-time.After(2 * time.Second):
			t.Fatal("observer saw nothing")
		}
		select {
		case <-unregistered:
			t.Fatal("a stopped socket's timer must not unregister")
		default:
		}
	})
}

// Run with -race: extendDeadline, the timer callback and deadline readers all
// touch TokenExpiresAt concurrently.
func TestTokenDeadline_ConcurrentRenewalTimerAndReaders(t *testing.T) {
	conn, _ := renewalSocketPair(t)
	hub := stoppedHub(t)
	c := &Client{ID: uuid.New(), UserID: uuid.New(), Conn: conn, Hub: hub, TokenExpiresAt: time.Now().Add(20 * time.Millisecond)}
	registered := make(chan struct{})
	var fires atomic.Int32
	c.deadlineMu.Lock()
	c.tokenExpiryTimer = time.AfterFunc(time.Millisecond, func() {
		fires.Add(1)
		c.onTokenExpiryTimer(registered)
	})
	c.deadlineMu.Unlock()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				c.extendDeadline(time.Now().Add(20 * time.Millisecond))
			}
		}
	}()
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = c.tokenDeadline()
					_ = c.tokenExpired()
					_ = c.connectionDeadline(pongWait)
				}
			}
		}()
	}
	require.Eventually(t, func() bool { return fires.Load() >= 3 }, 3*time.Second, time.Millisecond,
		"the timer must fire and re-arm repeatedly while renewals keep winning")
	close(stop)
	wg.Wait()
	c.stopTokenExpiry()
}

func TestReadPump_InterceptsAuthRenew(t *testing.T) {
	type scenario struct {
		name   string
		reject bool
		want   []byte
	}
	for _, sc := range []scenario{
		{name: "accepted", want: []byte(`{"type":"auth_renew_result","data":{"ok":true}}`)},
		{name: "refused", reject: true, want: []byte(`{"type":"auth_renew_result","data":{"ok":false}}`)},
	} {
		t.Run(sc.name, func(t *testing.T) {
			logs := captureRenewalLog(t)
			hub := stoppedHub(t)
			conn, peer := renewalSocketPair(t)
			events := peerReader(peer)
			f := newRenewalFixture(hub, conn)
			if sc.reject {
				f.authority.validateErr = errRenewalInjected
			}
			pumpDone := make(chan struct{})
			go func() { f.client.readPump(); close(pumpDone) }()
			go f.client.writePump()
			t.Cleanup(func() {
				// Close done first so the pump's requestUnregister cannot block on a
				// hub whose Run is not running.
				hub.closeOnce.Do(func() { close(hub.done) })
				_ = peer.Close()
				<-pumpDone
			})

			require.NoError(t, peer.WriteMessage(ws.TextMessage,
				[]byte(`{"type":"auth_renew","data":{"token":"`+renewalTestToken+`"}}`)))
			// Control frame: readPump is sequential, so once this reaches the hub the
			// renewal frame before it has been fully handled.
			require.NoError(t, peer.WriteMessage(ws.TextMessage, []byte(`{"type":"renewal_probe"}`)))

			select {
			case msg := <-hub.incoming:
				assert.Equal(t, "renewal_probe", msg.Type, "auth_renew must never reach Hub.incoming")
			case <-time.After(2 * time.Second):
				t.Fatal("control frame never reached the hub")
			}
			assert.Empty(t, hub.incoming, "nothing else may be routed to the hub")

			select {
			case ev := <-events:
				require.NoError(t, ev.err)
				assert.Equal(t, string(sc.want), ev.frame, "reply frame must be exactly the constant ack")
			case <-time.After(2 * time.Second):
				t.Fatal("no auth_renew_result reply")
			}
			if sc.reject {
				assert.Equal(t, f.deadline, f.client.tokenDeadline())
				assert.Contains(t, logs.String(), ": invalid\n", "positive control: renewal logging is captured")
			} else {
				assert.Equal(t, f.renewedTo, f.client.tokenDeadline())
			}
			assert.NotContains(t, logs.String(), renewalTestToken, "the token must never be logged")
		})
	}
}

func TestHandleTokenRenewal_AckAfterSendClosedDoesNotPanic(t *testing.T) {
	f := newRenewalFixture(stoppedHub(t), nil)
	f.authority.validateErr = errRenewalInjected
	f.client.closeOutbound()
	require.NotPanics(t, func() {
		f.client.handleTokenRenewal(IncomingMessage{Type: authRenewMessageType, Data: map[string]interface{}{"token": renewalTestToken}})
	})
}

func TestRenewToken_SameSessionRevokedFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(f *renewalFixture){
		"revoked":                func(f *renewalFixture) { f.authority.revoked = true },
		"revocation unavailable": func(f *renewalFixture) { f.authority.revokedErr = errRenewalInjected },
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureRenewalLog(t)
			conn, peer := renewalSocketPair(t)
			events := peerReader(peer)
			hub := runningHub(t)
			f := newRenewalFixture(hub, conn)
			mutate(f)
			hub.clients[f.client.ID] = f.client
			startHub(t, hub)

			assert.False(t, f.client.renewToken(renewalTestToken))
			assert.Equal(t, f.deadline, f.client.tokenDeadline())
			select {
			case ev := <-events:
				require.Error(t, ev.err, "the transport must close")
			case <-time.After(2 * time.Second):
				t.Fatal("a revoked same-sid socket was not closed")
			}
			assert.Contains(t, logs.String(), ": revoked_or_unavailable\n")
		})
	}
}

func TestRenewToken_StalledHubRunIsBoundedByTimeout(t *testing.T) {
	t.Run("rebind never taken: refused with nothing changed", func(t *testing.T) {
		logs := captureRenewalLog(t)
		f := newRenewalFixture(stoppedHub(t), nil) // Run never starts, done stays open
		oldSID := f.client.SessionID
		f.authority.verified.SessionID = uuid.NewString()
		f.authority.predecessor = oldSID

		start := time.Now()
		assert.False(t, f.client.renewToken(renewalTestToken))
		assert.Less(t, time.Since(start), tokenRenewalTimeout+time.Second, "the rebind wait is bounded by the renewal timeout")
		assert.Equal(t, f.deadline, f.client.tokenDeadline(), "no deadline extension while revocation is unproven")
		assert.Equal(t, oldSID, f.client.SessionID)
		_, _, revoked := f.authority.calls()
		assert.Zero(t, revoked)
		assert.Contains(t, logs.String(), ": unavailable\n")
	})

	t.Run("rebind taken but never applied: fails closed", func(t *testing.T) {
		logs := captureRenewalLog(t)
		hub := stoppedHub(t)
		go func() { <-hub.rebindSession }() // Run takes the rebind, then stalls
		conn, peer := renewalSocketPair(t)
		events := peerReader(peer)
		f := newRenewalFixture(hub, conn)
		f.authority.verified.SessionID = uuid.NewString()
		f.authority.predecessor = f.client.SessionID

		done := make(chan bool, 1)
		go func() { done <- f.client.renewToken(renewalTestToken) }()
		select {
		case ev := <-events:
			require.Error(t, ev.err, "the transport closes even while Run is stalled")
		case <-time.After(tokenRenewalTimeout + 2*time.Second):
			t.Fatal("the socket kept its authority while Run was stalled")
		}
		assert.Equal(t, f.deadline, f.client.tokenDeadline())
		hub.closeOnce.Do(func() { close(hub.done) }) // lets requestUnregister return
		select {
		case ok := <-done:
			assert.False(t, ok)
		case <-time.After(2 * time.Second):
			t.Fatal("renewToken did not return")
		}
		assert.Contains(t, logs.String(), ": revoked_or_unavailable\n")
	})
}

func TestRenewToken_SessionIDsAreCanonicalized(t *testing.T) {
	t.Run("an uppercase successor sid rebinds to the canonical spelling", func(t *testing.T) {
		hub := runningHub(t)
		f := newRenewalFixture(hub, nil)
		newSID := uuid.NewString()
		f.authority.verified.SessionID = strings.ToUpper(newSID)
		f.authority.predecessor = strings.ToUpper(f.client.SessionID)
		startHub(t, hub)

		require.True(t, f.client.renewToken(renewalTestToken))
		assert.Equal(t, newSID, f.client.SessionID, "DisconnectSession(canonical) must match the rebound socket")
		assert.Equal(t, []string{newSID}, f.authority.predecessorSIDs, "lineage is looked up by the canonical sid")
	})

	t.Run("an uppercase spelling of the same sid is not a rotation", func(t *testing.T) {
		f := newRenewalFixture(stoppedHub(t), nil)
		f.authority.verified.SessionID = strings.ToUpper(f.client.SessionID)
		require.True(t, f.client.renewToken(renewalTestToken))
		_, predecessor, _ := f.authority.calls()
		assert.Zero(t, predecessor)
	})

	t.Run("a non-uuid token sid is refused", func(t *testing.T) {
		logs := captureRenewalLog(t)
		f := newRenewalFixture(stoppedHub(t), nil)
		f.authority.verified.SessionID = "not-a-uuid"
		assert.False(t, f.client.renewToken(renewalTestToken))
		assert.Equal(t, f.deadline, f.client.tokenDeadline())
		assert.Contains(t, logs.String(), ": mismatch_session\n")
	})
}
