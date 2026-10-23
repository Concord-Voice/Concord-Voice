package websocket

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type serverVoiceRevocationChecker struct {
	mu      sync.Mutex
	allowed bool
	calls   int
	started chan struct{}
	release chan struct{}
}

type scopedPurgeChecker struct {
	allowedUser uuid.UUID
	batchCalls  *int
}

func (c scopedPurgeChecker) FilterVisibleUserIDsForChannelFresh(_ context.Context, _, _ string, candidates []string) ([]string, error) {
	if c.batchCalls != nil {
		*c.batchCalls++
	}
	for _, candidate := range candidates {
		if candidate == c.allowedUser.String() {
			return []string{candidate}, nil
		}
	}
	return []string{}, nil
}

type retryingPurgeChecker struct {
	mu    sync.Mutex
	calls int
}

type boundedPurgeChecker struct {
	mu      sync.Mutex
	active  int
	peak    int
	started chan struct{}
	release chan struct{}
}

func (c *boundedPurgeChecker) HasChannelPermission(context.Context, string, string, string, int64) (bool, error) {
	return false, errors.New("uncached permission path required")
}

func (c *boundedPurgeChecker) HasChannelPermissionsUncached(ctx context.Context, _, _, _ string, _ ...int64) (bool, error) {
	c.mu.Lock()
	c.active++
	if c.active > c.peak {
		c.peak = c.active
	}
	c.mu.Unlock()
	c.started <- struct{}{}
	select {
	case <-c.release:
	case <-ctx.Done():
	}
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	return ctx.Err() == nil, ctx.Err()
}

func TestServerChannelPurgeBoundsConcurrentPermissionWalks(t *testing.T) {
	serverID := uuid.New()
	hub := NewHub(openScriptedRowsDB(t,
		[]string{"allow_embedded_content", "server_id", "type"},
		[][]driver.Value{{false, serverID.String(), "text"}}, nil,
	), nil)
	viewer := newTestClient(hub, uuid.New())
	hub.clients[viewer.ID] = viewer
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{viewer.ID: true}
	checker := &boundedPurgeChecker{started: make(chan struct{}, 9), release: make(chan struct{})}
	hub.SetChannelPermissionChecker(checker)
	go hub.Run()
	t.Cleanup(hub.Shutdown)
	events := make([]ServerPurgeChannelEvent, 9)
	for i := range events {
		events[i] = ServerPurgeChannelEvent{ChannelID: uuid.New(), Message: OutgoingMessage{Type: "channel_purged"}}
	}
	hub.BroadcastServerPurgeViewAuthorizedRecovering(serverID, events)
	for range cap(hub.purgeDeliverySlots) {
		select {
		case <-checker.started:
		case <-time.After(2 * time.Second):
			t.Fatal("purge authorization did not fill bounded workers")
		}
	}
	select {
	case <-checker.started:
		t.Fatal("purge authorization exceeded the shared concurrency bound")
	case <-time.After(100 * time.Millisecond):
	}
	checker.mu.Lock()
	require.Equal(t, cap(hub.purgeDeliverySlots), checker.peak)
	checker.mu.Unlock()
	close(checker.release)
	for range events {
		select {
		case <-viewer.Send:
		case <-time.After(2 * time.Second):
			t.Fatal("bounded purge workers did not finish every channel")
		}
	}
}

func (c *retryingPurgeChecker) HasChannelPermission(context.Context, string, string, string, int64) (bool, error) {
	return false, errors.New("uncached permission path required")
}

func (c *retryingPurgeChecker) HasChannelPermissionsUncached(context.Context, string, string, string, ...int64) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls <= serverVoiceAuthorizationRetryMaxAttempts {
		return false, errors.New("permission database unavailable")
	}
	return true, nil
}

func TestServerChannelPurgeRecoversAfterAuthorizationDependencyFailure(t *testing.T) {
	serverID, channelID := uuid.New(), uuid.New()
	hub := NewHub(openScriptedRowsDB(t,
		[]string{"allow_embedded_content", "server_id", "type"},
		[][]driver.Value{{false, serverID.String(), "text"}}, nil,
	), nil)
	viewer := newTestClient(hub, uuid.New())
	hub.clients[viewer.ID] = viewer
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{viewer.ID: true}
	checker := &retryingPurgeChecker{}
	hub.SetChannelPermissionChecker(checker)
	go hub.Run()
	t.Cleanup(hub.Shutdown)

	hub.BroadcastToServerChannelViewAuthorizedRecovering(serverID, channelID,
		OutgoingMessage{Type: "channel_purged"})
	select {
	case <-viewer.Send:
	case <-time.After(8 * time.Second):
		t.Fatal("committed purge was not retried after authorization recovered")
	}
	checker.mu.Lock()
	defer checker.mu.Unlock()
	require.GreaterOrEqual(t, checker.calls, serverVoiceAuthorizationRetryMaxAttempts+1)
}

func (c scopedPurgeChecker) HasChannelPermission(_ context.Context, _, userID, _ string, bit int64) (bool, error) {
	return userID == c.allowedUser.String() && bit == permViewTextChannels, nil
}

func (c scopedPurgeChecker) HasChannelPermissionsUncached(ctx context.Context, serverID, userID, channelID string, bits ...int64) (bool, error) {
	if len(bits) != 1 {
		return false, nil
	}
	return c.HasChannelPermission(ctx, serverID, userID, channelID, bits[0])
}

func TestServerChannelViewAuthorizedDeliversTextPurgeOnlyToViewers(t *testing.T) {
	serverID, channelID := uuid.New(), uuid.New()
	hub := NewHub(openScriptedRowsDB(t,
		[]string{"allow_embedded_content", "server_id", "type"},
		[][]driver.Value{{false, serverID.String(), "text"}}, nil,
	), nil)
	viewer := newTestClient(hub, uuid.New())
	channelOnly := newTestClient(hub, viewer.UserID)
	hidden := newTestClient(hub, uuid.New())
	for _, client := range []*Client{viewer, channelOnly, hidden} {
		hub.clients[client.ID] = client
	}
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{viewer.ID: true, hidden.ID: true}
	hub.channelSubscriptions[channelID] = map[uuid.UUID]bool{viewer.ID: true, channelOnly.ID: true, hidden.ID: true}
	batchCalls := 0
	hub.SetChannelPermissionChecker(scopedPurgeChecker{allowedUser: viewer.UserID, batchCalls: &batchCalls})
	t.Cleanup(func() { close(hub.done) })
	require.True(t, hub.BroadcastToServerChannelViewAuthorizedContext(context.Background(), serverID, channelID,
		OutgoingMessage{Type: "channel_purged"}))

	select {
	case msg := <-hub.serverVoiceBroadcast:
		require.True(t, hub.handleServerBroadcast(msg))
	case <-time.After(time.Second):
		t.Fatal("scoped purge event was not queued")
	}
	select {
	case result := <-hub.channelDeliveryResults:
		hub.handleChannelDeliveryResult(result)
	case <-time.After(time.Second):
		t.Fatal("scoped purge authorization did not complete")
	}
	select {
	case <-viewer.Send:
	case <-time.After(time.Second):
		t.Fatal("server subscriber missed the purge")
	}
	select {
	case <-channelOnly.Send:
	case <-time.After(time.Second):
		t.Fatal("channel-only subscriber missed the purge")
	}
	select {
	case <-viewer.Send:
		t.Fatal("recipient subscribed to both audiences received a duplicate purge")
	default:
	}
	select {
	case frame := <-hidden.Send:
		t.Fatalf("hidden channel event leaked to denied recipient: %s", frame)
	default:
	}
	require.Equal(t, 1, batchCalls, "all recipients must share one fresh audience query")
}

type blockingServerVoiceStateDriver struct {
	entered  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	once     sync.Once
	cancel   sync.Once
}

func (d *blockingServerVoiceStateDriver) Open(string) (driver.Conn, error) {
	return &blockingServerVoiceStateConn{driver: d}, nil
}

type blockingServerVoiceStateConn struct {
	driver *blockingServerVoiceStateDriver
}

type ordinaryServerVoiceErasureDriver struct {
	serverID   uuid.UUID
	mu         sync.Mutex
	stateCalls int
}

func (d *ordinaryServerVoiceErasureDriver) Open(string) (driver.Conn, error) {
	return &ordinaryServerVoiceErasureConn{driver: d}, nil
}

type ordinaryServerVoiceErasureConn struct {
	driver *ordinaryServerVoiceErasureDriver
}

func (c *ordinaryServerVoiceErasureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (c *ordinaryServerVoiceErasureConn) Close() error { return nil }

func (c *ordinaryServerVoiceErasureConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}

func (c *ordinaryServerVoiceErasureConn) QueryContext(
	_ context.Context, query string, _ []driver.NamedValue,
) (driver.Rows, error) {
	if strings.Contains(query, "FROM users") {
		c.driver.mu.Lock()
		c.driver.stateCalls++
		exists := c.driver.stateCalls == 1
		c.driver.mu.Unlock()
		return &scriptedRows{
			columns: []string{"participant_exists", "successor_exists"},
			values:  [][]driver.Value{{exists, false}},
		}, nil
	}
	return &scriptedRows{
		columns: []string{"allow_embedded_content", "server_id", "type"},
		values:  [][]driver.Value{{false, c.driver.serverID.String(), "voice"}},
	}, nil
}

var _ driver.QueryerContext = (*ordinaryServerVoiceErasureConn)(nil)

func openOrdinaryServerVoiceErasureDB(t *testing.T, serverID uuid.UUID) (*sql.DB, *ordinaryServerVoiceErasureDriver) {
	t.Helper()
	driverName := "websocket-ordinary-server-voice-erasure-" + uuid.NewString()
	driver := &ordinaryServerVoiceErasureDriver{serverID: serverID}
	sql.Register(driverName, driver)
	db, err := sql.Open(driverName, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db, driver
}

func (c *blockingServerVoiceStateConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (c *blockingServerVoiceStateConn) Close() error { return nil }

func (c *blockingServerVoiceStateConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}

func (c *blockingServerVoiceStateConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	c.driver.once.Do(func() {
		close(c.driver.entered)
	})
	select {
	case <-c.driver.release:
	case <-ctx.Done():
		c.driver.cancel.Do(func() { close(c.driver.canceled) })
		return nil, ctx.Err()
	}
	return &scriptedRows{
		columns: []string{"participant_exists", "successor_exists"},
		values:  [][]driver.Value{{true, false}},
	}, nil
}

var _ driver.QueryerContext = (*blockingServerVoiceStateConn)(nil)

func openBlockingServerVoiceStateDB(t *testing.T) (*sql.DB, *blockingServerVoiceStateDriver) {
	t.Helper()
	driverName := "websocket-blocking-server-voice-state-" + uuid.NewString()
	driver := &blockingServerVoiceStateDriver{
		entered:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
	}
	sql.Register(driverName, driver)
	db, err := sql.Open(driverName, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db, driver
}

func (c *serverVoiceRevocationChecker) HasChannelPermission(
	context.Context, string, string, string, int64,
) (bool, error) {
	c.mu.Lock()
	allowed := c.allowed
	call := c.calls
	c.calls++
	c.mu.Unlock()
	if call == 0 {
		close(c.started)
		<-c.release
	}
	return allowed, nil
}

func (c *serverVoiceRevocationChecker) HasChannelPermissionsUncached(
	ctx context.Context, serverID, userID, channelID string, permBits ...int64,
) (bool, error) {
	for _, permBit := range permBits {
		allowed, err := c.HasChannelPermission(ctx, serverID, userID, channelID, permBit)
		if err != nil || !allowed {
			return allowed, err
		}
	}
	return true, nil
}

func TestServerVoiceDeliveryRechecksAuthorizationAfterPermissionRevocation(t *testing.T) {
	serverID := uuid.New()
	channelID := uuid.New()
	hub := NewHub(openScriptedRowsDB(t,
		[]string{"allow_embedded_content", "server_id", "type"},
		[][]driver.Value{{false, serverID.String(), "voice"}},
		nil,
	), nil)
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}
	checker := &serverVoiceRevocationChecker{
		allowed: true,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	hub.SetChannelPermissionChecker(checker)
	t.Cleanup(func() { close(hub.done) })

	require.True(t, hub.handleServerBroadcast(ServerBroadcastMessage{
		ServerID:             serverID,
		ChannelID:            channelID,
		RequireVoiceViewAuth: true,
		Data:                 OutgoingMessage{Type: "voice_state_update"},
	}))
	select {
	case <-checker.started:
	case <-time.After(time.Second):
		t.Fatal("authorization did not start")
	}

	closeRevocation := hub.BeginAudienceRevocation()
	require.NotZero(t, hub.PresenceAuthzOpenForTest(), "revocation fence must be raised")
	checker.mu.Lock()
	checker.allowed = false
	checker.mu.Unlock()
	closeRevocation()
	close(checker.release)

	var result channelDeliveryResult
	select {
	case result = <-hub.channelDeliveryResults:
	case <-time.After(time.Second):
		t.Fatal("authorization result was not published")
	}
	hub.handleChannelDeliveryResult(result)

	select {
	case frame := <-client.Send:
		t.Fatalf("revoked recipient received stale Server Voice frame: %s", frame)
	default:
	}
}

func TestServerVoiceDeliveryRetriesAnAuthorizedOrdinaryFrameAfterAudienceRevocation(t *testing.T) {
	serverID := uuid.New()
	channelID := uuid.New()
	hub := NewHub(openScriptedRowsDB(t,
		[]string{"allow_embedded_content", "server_id", "type"},
		[][]driver.Value{{false, serverID.String(), "voice"}},
		nil,
	), nil)
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}
	checker := &serverVoiceRevocationChecker{
		allowed: true,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	hub.SetChannelPermissionChecker(checker)
	release := sync.OnceFunc(func() { close(checker.release) })
	t.Cleanup(release)
	t.Cleanup(func() { close(hub.done) })
	require.True(t, hub.BroadcastToServerChannelAuthorizedContext(context.Background(), serverID, channelID, OutgoingMessage{
		Type: "voice_state_update",
	}))
	var message ServerBroadcastMessage
	select {
	case message = <-hub.serverVoiceBroadcast:
	case <-time.After(time.Second):
		t.Fatal("ordinary Server Voice frame was not admitted to the hub")
	}
	require.True(t, hub.handleServerBroadcast(message))
	select {
	case <-checker.started:
	case <-time.After(time.Second):
		t.Fatal("authorization did not start")
	}

	closeRevocation := hub.BeginAudienceRevocation()
	closeRevocation()
	release()

	var result channelDeliveryResult
	select {
	case result = <-hub.channelDeliveryResults:
	case <-time.After(time.Second):
		t.Fatal("first authorization result was not published")
	}
	hub.handleChannelDeliveryResult(result)
	select {
	case result = <-hub.channelDeliveryResults:
	case <-time.After(time.Second):
		t.Fatal("retried authorization result was not published")
	}
	hub.handleChannelDeliveryResult(result)

	select {
	case <-client.Send:
	case <-time.After(2 * time.Second):
		t.Fatal("authorized ordinary Server Voice frame was not rechecked and delivered")
	}
	checker.mu.Lock()
	calls := checker.calls
	checker.mu.Unlock()
	require.Equal(t, 2, calls, "the invalidated ordinary frame must be authorized twice")
}

func TestServerVoiceDeliverySuppressesErasedParticipantAfterOrdinaryRetry(t *testing.T) {
	serverID := uuid.New()
	channelID := uuid.New()
	participantID := uuid.New()
	db, state := openOrdinaryServerVoiceErasureDB(t, serverID)
	hub := NewHub(db, nil)
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}
	checker := &serverVoiceRevocationChecker{
		allowed: true,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	hub.SetChannelPermissionChecker(checker)
	release := sync.OnceFunc(func() { close(checker.release) })
	t.Cleanup(release)
	t.Cleanup(func() { close(hub.done) })

	require.True(t, hub.BroadcastToServerVoiceParticipantContext(
		context.Background(), serverID, channelID, participantID,
		OutgoingMessage{Type: "voice_state_update"},
	))
	var message ServerBroadcastMessage
	select {
	case message = <-hub.serverVoiceBroadcast:
	case <-time.After(time.Second):
		t.Fatal("ordinary participant frame was not admitted to the hub")
	}
	require.True(t, hub.handleServerBroadcast(message))
	select {
	case <-checker.started:
	case <-time.After(time.Second):
		t.Fatal("authorization did not start")
	}

	hub.InvalidateServerVoiceParticipantDelivery(participantID)
	release()
	result := <-hub.channelDeliveryResults
	hub.handleChannelDeliveryResult(result)

	require.Eventually(t, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.stateCalls == 2
	}, time.Second, 10*time.Millisecond)
	checker.mu.Lock()
	calls := checker.calls
	checker.mu.Unlock()
	require.Equal(t, 1, calls, "an erased participant must be suppressed before retry authorization")
	select {
	case frame := <-client.Send:
		t.Fatalf("erased participant reached a retried ordinary frame: %s", frame)
	default:
	}
}

func TestServerVoiceTerminalSuppressionDoesNotRecoverUnprobedDependencies(t *testing.T) {
	tests := []struct {
		name      string
		exists    bool
		successor bool
		want      ServerVoiceTerminalDeliveryOutcome
	}{
		{
			name: "erased participant",
			want: ServerVoiceTerminalDeliverySuppressedErased,
		},
		{
			name:      "successor participant",
			exists:    true,
			successor: true,
			want:      ServerVoiceTerminalDeliverySuppressedSuccessor,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serverID := uuid.New()
			channelID := uuid.New()
			hub := NewHub(openScriptedRowsDB(t,
				[]string{"participant_exists", "successor_exists"},
				[][]driver.Value{{test.exists, test.successor}},
				nil,
			), nil)
			client := newTestClient(hub, uuid.New())
			hub.clients[client.ID] = client
			hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}
			recorder := &securityEventRecorder{}
			hub.SetSecurityEvents(recorder)
			hub.SetChannelPermissionChecker(staticChannelPermissionChecker{allowed: true})
			t.Cleanup(func() { close(hub.done) })

			hub.completeSecurityEventProbeFailure(hub.beginSecurityEventProbe(securityEventSourcePermissionAuthority))
			hub.completeSecurityEventProbeFailure(hub.beginSecurityEventProbe(securityEventSourceChannelDelivery))
			requireSecurityEventDrain(t, hub)
			require.Equal(t, []securityevent.Event{{
				EventType:  securityevent.EventDependency,
				Outcome:    securityevent.OutcomeDegraded,
				Severity:   securityevent.SeverityHigh,
				ReasonCode: securityevent.ReasonDependencyUnavailable,
			}}, recorder.events)

			receipt := make(chan ServerVoiceTerminalDeliveryOutcome, 1)
			require.True(t, hub.handleServerBroadcast(ServerBroadcastMessage{
				ServerID:                 serverID,
				ChannelID:                channelID,
				RequireVoiceViewAuth:     true,
				serverVoiceParticipantID: uuid.New(),
				serverVoiceRetryOnAuthorizationInvalidation: true,
				serverVoiceTerminalReceipt:                  receipt,
				Data:                                        OutgoingMessage{Type: "voice_state_update"},
			}))
			select {
			case outcome := <-receipt:
				require.Equal(t, test.want, outcome)
			case <-time.After(time.Second):
				t.Fatal("terminal suppression receipt was not delivered")
			}
			requireSecurityEventDrain(t, hub)
			require.Equal(t, []securityevent.Event{{
				EventType:  securityevent.EventDependency,
				Outcome:    securityevent.OutcomeDegraded,
				Severity:   securityevent.SeverityHigh,
				ReasonCode: securityevent.ReasonDependencyUnavailable,
			}}, recorder.events)
		})
	}
}

func TestServerVoiceDeliveryAdmissionPrecedesConcurrentRevocation(t *testing.T) {
	serverID := uuid.New()
	channelID := uuid.New()
	hub := NewHub(nil, nil)
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}

	revocationEntered := make(chan struct{})
	revocationOpened := make(chan func(), 1)
	openedBeforeAdmission := make(chan func(), 1)
	hub.audienceRevocationBeforeOpen = func() {
		close(revocationEntered)
	}
	hub.serverVoiceDeliveryBeforeEnqueue = func() {
		go func() {
			revocationOpened <- hub.BeginAudienceRevocation()
		}()
		select {
		case <-revocationEntered:
		case <-time.After(time.Second):
			t.Fatal("revocation did not reach its admission boundary")
		}
		select {
		case closer := <-revocationOpened:
			openedBeforeAdmission <- closer
		case <-time.After(50 * time.Millisecond):
		}
	}

	result := channelDeliveryResult{
		kind:       channelDeliveryServerBroadcast,
		serverID:   serverID,
		channelID:  channelID,
		authzState: hub.presenceAuthzState.Load(),
		data:       []byte(`{"type":"voice_state_update"}`),
		decisions: []channelDeliveryDecision{{
			clientID: client.ID,
			userID:   client.UserID,
			allowed:  true,
		}},
	}

	deliveryDone := make(chan struct{})
	go func() {
		hub.applyServerBroadcastDeliveryResult(result)
		close(deliveryDone)
	}()
	select {
	case <-deliveryDone:
	case <-time.After(time.Second):
		t.Fatal("Server Voice delivery did not finish")
	}
	select {
	case closer := <-openedBeforeAdmission:
		closer()
		t.Fatal("revocation opened before the authorized frame was admitted")
	default:
	}

	closer := <-revocationOpened
	defer closer()
	select {
	case frame := <-client.Send:
		require.Equal(t, result.data, frame)
	default:
		t.Fatal("authorized frame was not admitted before the revocation opened")
	}
}

func TestServerVoiceTerminalDeliverySnapshotsEpochBeforeParticipantState(t *testing.T) {
	serverID := uuid.New()
	channelID := uuid.New()
	participantID := uuid.New()
	db, stateDriver := openBlockingServerVoiceStateDB(t)
	hub := NewHub(db, nil)
	t.Cleanup(func() { close(hub.done) })
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}
	checker := &serverVoiceRevocationChecker{
		allowed: true,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	close(checker.release)
	hub.SetChannelPermissionChecker(checker)

	receipt := make(chan ServerVoiceTerminalDeliveryOutcome, 1)
	require.True(t, hub.handleServerBroadcast(ServerBroadcastMessage{
		ServerID:                 serverID,
		ChannelID:                channelID,
		RequireVoiceViewAuth:     true,
		serverVoiceParticipantID: participantID,
		serverVoiceRetryOnAuthorizationInvalidation: true,
		serverVoiceTerminalReceipt:                  receipt,
		Data:                                        OutgoingMessage{Type: "voice_state_update"},
	}))
	select {
	case <-stateDriver.entered:
	case <-time.After(time.Second):
		t.Fatal("terminal delivery did not query participant state")
	}

	hub.InvalidateServerVoiceChannelDelivery(serverID, channelID)
	close(stateDriver.release)

	var result channelDeliveryResult
	select {
	case result = <-hub.channelDeliveryResults:
	case <-time.After(time.Second):
		t.Fatal("terminal delivery did not publish authorization result")
	}
	require.Zero(t, result.authzEpoch, "participant-state query must not capture a newer lifecycle epoch")
	hub.handleChannelDeliveryResult(result)
	select {
	case frame := <-client.Send:
		t.Fatalf("stale terminal delivery crossed the successor epoch: %s", frame)
	default:
	}
}

func TestServerVoiceTerminalDeliveryParticipantStateQueryCancelsOnShutdown(t *testing.T) {
	serverID, channelID, participantID := uuid.New(), uuid.New(), uuid.New()
	db, stateDriver := openBlockingServerVoiceStateDB(t)
	hub := NewHub(db, nil)
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}
	hub.SetChannelPermissionChecker(staticChannelPermissionChecker{allowed: true})
	go hub.Run()

	require.True(t, hub.handleServerBroadcast(ServerBroadcastMessage{
		ServerID:                 serverID,
		ChannelID:                channelID,
		RequireVoiceViewAuth:     true,
		serverVoiceParticipantID: participantID,
		serverVoiceRetryOnAuthorizationInvalidation: true,
		Data: OutgoingMessage{Type: "voice_state_update"},
	}))
	select {
	case <-stateDriver.entered:
	case <-time.After(time.Second):
		t.Fatal("terminal delivery did not query participant state")
	}

	shutdownDone := make(chan struct{})
	go func() {
		hub.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-stateDriver.canceled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the terminal participant-state query")
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join the canceled terminal delivery worker")
	}
}

func TestServerVoiceCanceledQueueCannotAdmitBufferedTerminalAfterSuccessor(t *testing.T) {
	db, stateDriver := openBlockingServerVoiceStateDB(t)
	close(stateDriver.release)
	hub := NewHub(db, nil)
	serverID, channelID, participantID := uuid.New(), uuid.New(), uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}
	hub.SetChannelPermissionChecker(staticChannelPermissionChecker{allowed: true})
	receipt := make(chan ServerVoiceTerminalDeliveryOutcome, 1)

	require.True(t, hub.dispatchChannelDelivery(channelDeliveryRequest{
		kind:                             channelDeliveryServerBroadcast,
		serverID:                         serverID,
		channelID:                        channelID,
		viewPerm:                         permViewVoiceChannels,
		data:                             []byte(`{"type":"voice_state_update","status":"left"}`),
		recipients:                       []channelDeliveryRecipient{{clientID: client.ID, userID: client.UserID}},
		serverVoiceParticipantID:         participantID,
		retryOnAuthorizationInvalidation: true,
		serverVoiceTerminalReceipt:       receipt,
	}))

	var stale channelDeliveryResult
	select {
	case stale = <-hub.channelDeliveryResults:
	case <-time.After(time.Second):
		t.Fatal("terminal worker did not publish its authorized result")
	}

	hub.stopServerVoiceDeliveryWorkers()
	_, err := hub.ApplyServerVoiceChannelMutation(serverID, channelID, func() (bool, error) {
		return true, nil
	})
	require.NoError(t, err)

	hub.handleChannelDeliveryResult(stale)
	select {
	case frame := <-client.Send:
		t.Fatalf("canceled stale terminal crossed the successor fence: %s", frame)
	default:
	}
	select {
	case outcome := <-receipt:
		t.Fatalf("canceled worker unexpectedly settled terminal receipt: %v", outcome)
	default:
	}
}

func TestServerVoiceStoppedQueueDoesNotAcknowledgeBufferedTerminal(t *testing.T) {
	db, stateDriver := openBlockingServerVoiceStateDB(t)
	close(stateDriver.release)
	hub := NewHub(db, nil)
	hub.channelDeliveryResults = make(chan channelDeliveryResult)
	serverID, channelID, participantID := uuid.New(), uuid.New(), uuid.New()
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}
	hub.SetChannelPermissionChecker(staticChannelPermissionChecker{allowed: true})
	defer hub.stopServerVoiceDeliveryWorkers()
	receipt := make(chan ServerVoiceTerminalDeliveryOutcome, 1)

	require.True(t, hub.dispatchChannelDelivery(channelDeliveryRequest{
		kind:                             channelDeliveryServerBroadcast,
		serverID:                         serverID,
		channelID:                        channelID,
		viewPerm:                         permViewVoiceChannels,
		data:                             []byte(`{"type":"voice_state_update","status":"left"}`),
		recipients:                       []channelDeliveryRecipient{{clientID: client.ID, userID: client.UserID}},
		serverVoiceParticipantID:         participantID,
		retryOnAuthorizationInvalidation: true,
		serverVoiceTerminalReceipt:       receipt,
	}))

	var stale channelDeliveryResult
	select {
	case stale = <-hub.channelDeliveryResults:
	case <-time.After(time.Second):
		t.Fatal("terminal worker did not publish its authorized result")
	}
	hub.serverVoiceDeliveryMu.Lock()
	hub.serverVoiceDeliveryStopped = true
	hub.serverVoiceDeliveryMu.Unlock()
	hub.handleChannelDeliveryResult(stale)

	select {
	case frame := <-client.Send:
		t.Fatalf("stopped terminal delivery was admitted: %s", frame)
	default:
	}
	select {
	case outcome := <-receipt:
		t.Fatalf("stopped terminal delivery was incorrectly settled: %v", outcome)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestServerVoiceChannelMutationGateDoesNotSerializeSameChannel(t *testing.T) {
	hub := NewHub(nil, nil)
	serverID, channelID := uuid.New(), uuid.New()
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{}, 2)
	errs := make(chan error, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		_, err := hub.ApplyServerVoiceChannelMutation(serverID, channelID, func() (bool, error) {
			close(firstEntered)
			<-release
			return false, nil
		})
		errs <- err
	}()
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first mutation did not begin")
	}

	go func() {
		defer func() { done <- struct{}{} }()
		_, err := hub.ApplyServerVoiceChannelMutation(serverID, channelID, func() (bool, error) {
			close(secondEntered)
			<-release
			return false, nil
		})
		errs <- err
	}()
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		close(release)
		<-done
		<-done
		t.Fatal("same-channel mutation was serialized")
	}
	close(release)
	<-done
	<-done
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
}

func TestServerVoiceChannelMutationGateIsolatesCollidingChannels(t *testing.T) {
	hub := NewHub(nil, nil)
	first := serverVoiceDeliveryKey{serverID: uuid.New(), channelID: uuid.New()}
	second := first
	second.channelID[0] ^= serverVoiceDeliveryGateCount
	require.NotEqual(t, first, second)
	require.Equal(t, serverVoiceDeliveryGateStripeIndex(first), serverVoiceDeliveryGateStripeIndex(second))

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	errResult := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := hub.ApplyServerVoiceChannelMutation(
			second.serverID, second.channelID, func() (bool, error) {
				close(entered)
				<-release
				return false, nil
			},
		)
		errResult <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("colliding mutation did not begin")
	}

	cleared := make(chan struct{})
	go func() {
		_, ok := hub.waitForServerVoiceChannelMutationClear(first, make(chan struct{}))
		if ok {
			close(cleared)
		}
	}()
	select {
	case <-cleared:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("colliding channel mutation blocked terminal admission")
	}
	close(release)
	<-done
	require.NoError(t, <-errResult)
}
