package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type gatedWriteConn struct {
	net.Conn
	writeStarted chan struct{}
	writeRelease chan struct{}
}

func (conn *gatedWriteConn) Write(data []byte) (int, error) {
	if conn.writeRelease != nil {
		select {
		case conn.writeStarted <- struct{}{}:
		default:
		}
		<-conn.writeRelease
	}
	return conn.Conn.Write(data)
}

func startBlockedRichPresenceWrite(t *testing.T) (*Client, *gatedWriteConn) {
	t.Helper()
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upgrader := websocket.Upgrader{
			CheckOrigin: func(request *http.Request) bool {
				return request.Header.Get("Origin") == ""
			},
		}
		conn, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		<-serverDone
	}))
	t.Cleanup(func() {
		close(serverDone)
		server.Close()
	})

	var gatedConn *gatedWriteConn
	dialer := websocket.Dialer{NetDial: func(network, address string) (net.Conn, error) {
		rawConn, err := net.Dial(network, address)
		if err != nil {
			return nil, err
		}
		gatedConn = &gatedWriteConn{Conn: rawConn}
		return gatedConn, nil
	}}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	gatedConn.writeStarted = make(chan struct{}, 1)
	gatedConn.writeRelease = make(chan struct{})

	return &Client{
		Conn: conn, Send: make(chan []byte, 1),
		privacyClearThenCloseWake: make(chan struct{}, 1),
	}, gatedConn
}

func startGatedRichPresenceWritePump(t *testing.T) (*Client, *websocket.Conn, func()) {
	t.Helper()
	ready := make(chan *Client, 1)
	start := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{
			CheckOrigin: func(request *http.Request) bool {
				return request.Header.Get("Origin") == ""
			},
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := &Client{
			ID: uuid.New(), UserID: uuid.New(), Conn: conn, Send: make(chan []byte, 8),
			privacyClearThenCloseWake: make(chan struct{}, 1),
		}
		ready <- client
		<-start
		client.writePump()
	}))
	t.Cleanup(server.Close)

	peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })

	return <-ready, peer, func() { close(start) }
}

// The settings tier-down clear must reach the browser before its reconnect
// fallback closes the socket. The pump starts only after scheduling, making a
// direct Conn.Close deterministic: it closes the peer before the queued clear
// can be written.
func TestDeliverRichPresenceClearsThenDisconnectWritesClearBeforeClosing(t *testing.T) {
	hub := NewHub(nil, nil)
	client, peer, start := startGatedRichPresenceWritePump(t)
	client.UserID = uuid.New()
	client.Hub = hub
	client.activityRichPresenceCapable = true
	hub.clients[client.ID] = client
	hub.userClients[client.UserID] = map[uuid.UUID]bool{client.ID: true}
	client.Send <- []byte(`{"type":"stale_pre_tier_down_projection"}`)
	senderID := uuid.New()

	require.NoError(t, hub.DeliverRichPresenceClearsThenDisconnect(
		context.Background(),
		[]presence.DeliveryPlan{{
			SenderID: senderID, Category: presence.CategoryServerVoice,
			ClearRecipients: map[uuid.UUID]bool{client.UserID: true},
		}},
		map[uuid.UUID]bool{client.UserID: true},
	))
	start()

	require.NoError(t, peer.SetReadDeadline(time.Now().Add(2*time.Second)))
	messageType, payload, err := peer.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, messageType)
	var message map[string]any
	require.NoError(t, json.Unmarshal(payload, &message))
	assertRichPresenceClear(t, message, senderID, presence.CategoryServerVoice)

	require.NoError(t, peer.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, _, err = peer.ReadMessage()
	require.Error(t, err, "the clear is followed by the reconnect close")
}

func TestWritePumpRevocationLinearizesBeforeDequeuedFrameWrite(t *testing.T) {
	client, peer, start := startGatedRichPresenceWritePump(t)
	senderID := uuid.New()
	clearFrame, err := marshalRichPresenceFrame(
		senderID, presence.CategoryServerVoice, false, nil, 0,
	)
	require.NoError(t, err)

	client.sendMu.Lock()
	locked := true
	defer func() {
		if locked {
			client.sendMu.Unlock()
		}
	}()
	client.Send <- []byte(`{"type":"stale_pre_revocation_projection"}`)
	start()
	require.Eventually(t, func() bool { return len(client.Send) == 0 }, time.Second, time.Millisecond)

	firstFrame := make(chan []byte, 1)
	go func() {
		_, payload, _ := peer.ReadMessage()
		firstFrame <- payload
	}()

	// Simulate the scheduler after it acquires sendMu. The writer already
	// dequeued the stale frame, so its recheck must observe this terminal state.
	client.privacyClearThenCloseMu.Lock()
	client.privacyClearThenClose = [][]byte{clearFrame}
	client.privacyClearThenCloseScheduled = true
	client.sendClosed = true
	client.privacyClearThenCloseMu.Unlock()
	client.sendMu.Unlock()
	locked = false

	select {
	case payload := <-firstFrame:
		var message map[string]any
		require.NoError(t, json.Unmarshal(payload, &message))
		assertRichPresenceClear(t, message, senderID, presence.CategoryServerVoice)
	case <-time.After(2 * time.Second):
		require.Fail(t, "privacy clear was not written")
	}
}

func TestDeliverRichPresenceClearsThenDisconnectClosesBlockedWriter(t *testing.T) {
	client, gatedConn := startBlockedRichPresenceWrite(t)
	hub := NewHub(nil, nil)
	client.ID = uuid.New()
	client.UserID = uuid.New()
	client.Hub = hub
	client.activityRichPresenceCapable = true
	hub.clients[client.ID] = client
	hub.userClients[client.UserID] = map[uuid.UUID]bool{client.ID: true}
	writeDone := make(chan bool, 1)
	go func() {
		writeDone <- client.writePumpMessage([]byte(`{"type":"stale_pre_revocation_projection"}`), true)
	}()
	select {
	case <-gatedConn.writeStarted:
	case <-time.After(time.Second):
		require.Fail(t, "ordinary write did not reach the test gate")
	}

	released := false
	defer func() {
		if !released {
			close(gatedConn.writeRelease)
		}
	}()
	producerDone := make(chan bool, 1)
	go func() { producerDone <- client.enqueueOutbound([]byte(`{"type":"queued_after_blocked_write"}`)) }()
	select {
	case enqueued := <-producerDone:
		require.True(t, enqueued, "Hub producers can enqueue while a socket write is blocked")
	case <-time.After(time.Second):
		require.Fail(t, "Hub producer waited for a blocked socket write")
	}
	delivered := make(chan error, 1)
	go func() {
		delivered <- hub.DeliverRichPresenceClearsThenDisconnect(
			context.Background(),
			[]presence.DeliveryPlan{{
				SenderID: uuid.New(), Category: presence.CategoryServerVoice,
				ClearRecipients: map[uuid.UUID]bool{client.UserID: true},
			}},
			map[uuid.UUID]bool{client.UserID: true},
		)
	}()
	select {
	case err := <-delivered:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.Fail(t, "privacy delivery waited for a blocked ordinary write")
	}
	require.Error(t, gatedConn.SetReadDeadline(time.Now().Add(time.Second)),
		"an in-flight stale write requires an immediate transport close")

	// The scheduler refuses the busy writer; the delivery fallback closed it
	// before the gate opened, so the dequeued stale frame cannot cross.
	close(gatedConn.writeRelease)
	released = true
	select {
	case stopped := <-writeDone:
		require.True(t, stopped)
	case <-time.After(time.Second):
		require.Fail(t, "ordinary write did not complete after release")
	}
}

func TestDeliverRichPresenceClearsThenDisconnectClosesBlockedKeepalive(t *testing.T) {
	client, gatedConn := startBlockedRichPresenceWrite(t)
	hub := NewHub(nil, nil)
	client.ID = uuid.New()
	client.UserID = uuid.New()
	client.Hub = hub
	client.activityRichPresenceCapable = true
	hub.clients[client.ID] = client
	hub.userClients[client.UserID] = map[uuid.UUID]bool{client.ID: true}
	tickDone := make(chan bool, 1)
	go func() { tickDone <- client.writePumpTick(true) }()
	select {
	case <-gatedConn.writeStarted:
	case <-time.After(time.Second):
		require.Fail(t, "keepalive write did not reach the test gate")
	}

	released := false
	defer func() {
		if !released {
			close(gatedConn.writeRelease)
		}
	}()
	delivered := make(chan error, 1)
	go func() {
		delivered <- hub.DeliverRichPresenceClearsThenDisconnect(
			context.Background(),
			[]presence.DeliveryPlan{{
				SenderID: uuid.New(), Category: presence.CategoryServerVoice,
				ClearRecipients: map[uuid.UUID]bool{client.UserID: true},
			}},
			map[uuid.UUID]bool{client.UserID: true},
		)
	}()
	select {
	case err := <-delivered:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.Fail(t, "privacy delivery waited for a blocked keepalive write")
	}

	close(gatedConn.writeRelease)
	released = true
	select {
	case stopped := <-tickDone:
		require.True(t, stopped)
	case <-time.After(time.Second):
		require.Fail(t, "keepalive write did not complete after release")
	}
}

func TestDisconnectPrivacyCriticalClientRemainsPromptDuringBlockedClear(t *testing.T) {
	client, gatedConn := startBlockedRichPresenceWrite(t)
	hub := NewHub(nil, nil)
	client.ID = uuid.New()
	hub.clients[client.ID] = client
	require.True(t, client.schedulePrivacyClearsThenClose([][]byte{[]byte(`{"type":"rich_presence_clear"}`)}))

	clearDone := make(chan bool, 1)
	go func() { clearDone <- client.writePrivacyClearsThenClose() }()
	select {
	case <-gatedConn.writeStarted:
	case <-time.After(time.Second):
		require.Fail(t, "clear write did not reach the test gate")
	}
	released := false
	defer func() {
		if !released {
			close(gatedConn.writeRelease)
		}
	}()

	disconnected := make(chan error, 1)
	go func() { disconnected <- hub.DisconnectAllRichPresenceClients(context.Background()) }()
	select {
	case err := <-disconnected:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.Fail(t, "disconnect waited for a blocked clear write")
	}
	require.Error(t, gatedConn.SetReadDeadline(time.Now().Add(time.Second)),
		"a later direct revocation must close the transport before the first clear unblocks")

	close(gatedConn.writeRelease)
	released = true
	select {
	case stopped := <-clearDone:
		require.True(t, stopped)
	case <-time.After(time.Second):
		require.Fail(t, "clear write did not complete after release")
	}
}

func TestRepeatedPrivacyClearScheduleClosesBlockedFirstClear(t *testing.T) {
	client, gatedConn := startBlockedRichPresenceWrite(t)
	require.True(t, client.schedulePrivacyClearsThenClose([][]byte{[]byte(`{"sender_id":"first"}`)}))

	clearDone := make(chan bool, 1)
	go func() { clearDone <- client.writePrivacyClearsThenClose() }()
	select {
	case <-gatedConn.writeStarted:
	case <-time.After(time.Second):
		require.Fail(t, "first privacy clear did not reach the test gate")
	}

	require.True(t, client.schedulePrivacyClearsThenClose([][]byte{[]byte(`{"sender_id":"second"}`)}))
	require.Error(t, gatedConn.SetReadDeadline(time.Now().Add(time.Second)),
		"a repeated schedule must close before the blocked first clear can leave the second projection visible")

	close(gatedConn.writeRelease)
	select {
	case stopped := <-clearDone:
		require.True(t, stopped)
	case <-time.After(time.Second):
		require.Fail(t, "blocked clear did not finish after release")
	}
}

func TestForceRevokeClosesPendingPrivacyClearConnection(t *testing.T) {
	client, gatedConn := startBlockedRichPresenceWrite(t)
	require.True(t, client.schedulePrivacyClearsThenClose([][]byte{[]byte(`{"type":"rich_presence_clear"}`)}))

	client.forceRevoke([]byte(`{"type":"session_revoked"}`))

	require.Error(t, gatedConn.SetReadDeadline(time.Now().Add(time.Second)),
		"forced revocation must close a connection awaiting privacy clears")
}

func TestForceRevokeClosesBlockedScheduledPrivacyClearPromptly(t *testing.T) {
	client, gatedConn := startBlockedRichPresenceWrite(t)
	require.True(t, client.schedulePrivacyClearsThenClose([][]byte{[]byte(`{"type":"rich_presence_clear"}`)}))

	clearDone := make(chan bool, 1)
	go func() { clearDone <- client.writePumpMessage(nil, true) }()
	select {
	case <-gatedConn.writeStarted:
	case <-time.After(time.Second):
		require.Fail(t, "scheduled clear did not reach the test gate")
	}

	released := false
	defer func() {
		if !released {
			close(gatedConn.writeRelease)
		}
	}()
	revoked := make(chan struct{})
	go func() {
		client.forceRevoke([]byte(`{"type":"session_revoked"}`))
		close(revoked)
	}()
	select {
	case <-revoked:
	case <-time.After(time.Second):
		require.Fail(t, "forced revocation waited for a blocked scheduled clear")
	}
	require.Error(t, gatedConn.SetReadDeadline(time.Now().Add(time.Second)),
		"forced revocation must close the transport before the clear write is released")

	close(gatedConn.writeRelease)
	released = true
	select {
	case stopped := <-clearDone:
		require.True(t, stopped)
	case <-time.After(time.Second):
		require.Fail(t, "blocked clear did not finish after release")
	}
}

func TestDeliverRichPresence_ClearsBeforeUpdatesOnEveryConnectedDevice(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	senderID := uuid.New()
	clearOnlyID := uuid.New()
	updateOnlyID := uuid.New()
	bothID := uuid.New()
	unrelatedID := uuid.New()

	clearDevices := []*Client{
		activityRichPresenceDeliveryClient(hub, clearOnlyID, 2),
		activityRichPresenceDeliveryClient(hub, clearOnlyID, 2),
	}
	updateDevices := []*Client{
		activityRichPresenceDeliveryClient(hub, updateOnlyID, 2),
		activityRichPresenceDeliveryClient(hub, updateOnlyID, 2),
	}
	bothDevices := []*Client{
		activityRichPresenceDeliveryClient(hub, bothID, 2),
		activityRichPresenceDeliveryClient(hub, bothID, 2),
	}
	unrelated := customTextDeliveryClient(hub, unrelatedID, 1)

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:         senderID,
		Category:         presence.CategoryServerVoice,
		ClearRecipients:  map[uuid.UUID]bool{clearOnlyID: true, bothID: true, uuid.New(): false},
		UpdateRecipients: map[uuid.UUID]bool{updateOnlyID: true, bothID: true, uuid.New(): false},
		Minimized:        true,
		Payload:          json.RawMessage(`{"channel_id":"11111111-1111-1111-1111-111111111111","server_id":"22222222-2222-2222-2222-222222222222"}`),
		UpdatedAt:        1784088000,
	})

	require.NoError(t, err)
	for _, client := range clearDevices {
		assertRichPresenceClear(t, readClientMsg(t, client), senderID, presence.CategoryServerVoice)
	}
	for _, client := range updateDevices {
		assertServerVoiceUpdate(t, readClientMsg(t, client), senderID)
	}
	for _, client := range bothDevices {
		assertRichPresenceClear(t, readClientMsg(t, client), senderID, presence.CategoryServerVoice)
		assertServerVoiceUpdate(t, readClientMsg(t, client), senderID)
	}
	assertNoMessage(t, unrelated)
}

func TestDeliverRichPresence_RequiresUpgradeActivityCapabilityForLiveFrames(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	senderID := uuid.New()
	recipientID := uuid.New()
	legacy := customTextDeliveryClient(hub, recipientID, 2)
	capable := customTextDeliveryClient(hub, recipientID, 2)
	capable.activityRichPresenceCapable = true

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:         senderID,
		Category:         presence.CategoryServerVoice,
		ClearRecipients:  map[uuid.UUID]bool{recipientID: true},
		UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
		Minimized:        true,
		Payload:          json.RawMessage(`{"channel_id":"11111111-1111-1111-1111-111111111111","server_id":"22222222-2222-2222-2222-222222222222"}`),
		UpdatedAt:        1784088000,
	})

	require.NoError(t, err)
	assertNoMessage(t, legacy)
	assertRichPresenceClear(t, readClientMsg(t, capable), senderID, presence.CategoryServerVoice)
	assertServerVoiceUpdate(t, readClientMsg(t, capable), senderID)
}

func TestDeliverRichPresence_CapableBootstrapOrdersSnapshotBeforeLiveClear(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := customTextDeliveryClient(hub, recipientID, 2)
	recipient.activityRichPresenceCapable = true
	recipient.beginBootstrap()

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:        uuid.New(),
		Category:        presence.CategoryPrivateCall,
		ClearRecipients: map[uuid.UUID]bool{recipientID: true},
	})
	require.NoError(t, err)
	require.True(t, hub.completeClientBootstrap(
		recipient,
		[]byte(`{"type":"presence_snapshot","data":{"users":[]}}`),
	))

	assert.Equal(t, "presence_snapshot", readClientMsg(t, recipient)["type"])
	assertRichPresenceClear(t, readClientMsg(t, recipient), uuid.Nil, presence.CategoryPrivateCall)
}

func TestDeliverRichPresence_MarshalsEveryFrameBeforeSendingAny(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := customTextDeliveryClient(hub, recipientID, 2)
	marshalErr := errors.New("marshal sentinel")
	calls := 0
	hub.richPresenceFrameMarshaler = func(
		uuid.UUID,
		presence.Category,
		bool,
		json.RawMessage,
		int64,
	) ([]byte, error) {
		calls++
		if calls == 2 {
			return nil, marshalErr
		}
		return []byte(`{"type":"rich_presence_clear"}`), nil
	}

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:         uuid.New(),
		Category:         presence.CategoryPrivateCall,
		ClearRecipients:  map[uuid.UUID]bool{recipientID: true},
		UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
		Payload:          json.RawMessage(`{"call_type":"group","participant_count":3}`),
		UpdatedAt:        1784088000,
	})

	require.ErrorIs(t, err, marshalErr)
	assert.Equal(t, 2, calls)
	assert.Empty(t, recipient.Send)
}

func TestDeliverRichPresence_CancellationImmediatelyBeforeEnqueueSendsNothing(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := activityRichPresenceDeliveryClient(hub, recipientID, 1)
	ctx, cancel := context.WithCancel(context.Background())
	hub.customTextDeliveryBeforeEnqueue = cancel

	err := hub.DeliverRichPresence(ctx, presence.DeliveryPlan{
		SenderID:         uuid.New(),
		Category:         presence.CategoryPrivateCall,
		UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
		Minimized:        true,
		Payload:          json.RawMessage(`{"call_type":"dm","participant_count":2}`),
		UpdatedAt:        1784088000,
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, recipient.Send)
}

func TestDeliverRichPresence_FullQueueDisconnectsWithoutDroppingQueuedFrame(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := activityRichPresenceDeliveryClient(hub, recipientID, 1)
	queued := []byte(`{"type":"already_queued"}`)
	recipient.Send <- queued
	disconnected := false
	hub.customTextClientDisconnect = func(client *Client) error {
		assert.Same(t, recipient, client)
		disconnected = true
		return nil
	}

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:        uuid.New(),
		Category:        presence.CategoryServerVoice,
		ClearRecipients: map[uuid.UUID]bool{recipientID: true},
	})

	require.NoError(t, err)
	assert.True(t, disconnected)
	assert.Equal(t, queued, <-recipient.Send)
}

func TestDeliverRichPresence_DisconnectedClearRecipientIsNotRetargetedByUpdate(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	recipientID := uuid.New()
	recipient := activityRichPresenceDeliveryClient(hub, recipientID, 1)
	disconnectCalls := 0
	hub.customTextClientDisconnect = func(*Client) error {
		disconnectCalls++
		if disconnectCalls > 1 {
			return errors.New("client disconnected twice")
		}
		return nil
	}

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:         uuid.New(),
		Category:         presence.CategoryServerVoice,
		ClearRecipients:  map[uuid.UUID]bool{recipientID: true},
		UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
		Payload:          json.RawMessage(`{"channel_id":"11111111-1111-1111-1111-111111111111","server_id":"22222222-2222-2222-2222-222222222222"}`),
		UpdatedAt:        1784088000,
	})

	require.NoError(t, err)
	assert.Equal(t, 1, disconnectCalls)
	assertRichPresenceClear(t, readClientMsg(t, recipient), uuid.Nil, "")
}

func TestDisconnectRichPresenceClients_DisconnectsEveryTargetDeviceOnly(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	targetID := uuid.New()
	unrelatedID := uuid.New()
	targets := []*Client{
		customTextDeliveryClient(hub, targetID, 1),
		customTextDeliveryClient(hub, targetID, 1),
	}
	unrelated := customTextDeliveryClient(hub, unrelatedID, 1)
	disconnected := make(map[uuid.UUID]bool)
	hub.customTextClientDisconnect = func(client *Client) error {
		disconnected[client.ID] = true
		return nil
	}

	err := hub.DisconnectRichPresenceClients(
		context.Background(),
		map[uuid.UUID]bool{targetID: true, unrelatedID: false},
	)

	require.NoError(t, err)
	for _, client := range targets {
		assert.True(t, disconnected[client.ID])
	}
	assert.False(t, disconnected[unrelated.ID])
}

func TestDisconnectRichPresenceClients_AttemptsEveryTargetAfterCloseError(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	targetID := uuid.New()
	targets := []*Client{
		customTextDeliveryClient(hub, targetID, 1),
		customTextDeliveryClient(hub, targetID, 1),
		customTextDeliveryClient(hub, targetID, 1),
	}
	closeErr := errors.New("close sentinel")
	attempted := make(map[uuid.UUID]bool)
	hub.customTextClientDisconnect = func(client *Client) error {
		attempted[client.ID] = true
		if len(attempted) == 1 {
			return closeErr
		}
		return nil
	}

	err := hub.DisconnectRichPresenceClients(
		context.Background(),
		map[uuid.UUID]bool{targetID: true},
	)

	require.ErrorIs(t, err, closeErr)
	for _, client := range targets {
		assert.True(t, attempted[client.ID])
	}
}

func TestDisconnectRichPresenceClients_AttemptsEveryTargetAfterCancellation(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	targetID := uuid.New()
	targets := []*Client{
		customTextDeliveryClient(hub, targetID, 1),
		customTextDeliveryClient(hub, targetID, 1),
		customTextDeliveryClient(hub, targetID, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempted := make(map[uuid.UUID]bool)
	hub.customTextClientDisconnect = func(client *Client) error {
		attempted[client.ID] = true
		return nil
	}

	err := hub.DisconnectRichPresenceClients(
		ctx,
		map[uuid.UUID]bool{targetID: true},
	)

	require.ErrorIs(t, err, context.Canceled)
	for _, client := range targets {
		assert.True(t, attempted[client.ID])
	}
}

func TestDisconnectAllRichPresenceClients_DisconnectsEveryLocalClient(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	clients := []*Client{
		customTextDeliveryClient(hub, uuid.New(), 1),
		customTextDeliveryClient(hub, uuid.New(), 1),
		customTextDeliveryClient(hub, uuid.New(), 1),
	}
	disconnected := make(map[uuid.UUID]bool)
	hub.customTextClientDisconnect = func(client *Client) error {
		disconnected[client.ID] = true
		return nil
	}

	require.NoError(t, hub.DisconnectAllRichPresenceClients(context.Background()))
	for _, client := range clients {
		assert.True(t, disconnected[client.ID])
	}
}

func TestDisconnectAllRichPresenceClients_AttemptsEveryClientAfterCloseError(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	clients := []*Client{
		customTextDeliveryClient(hub, uuid.New(), 1),
		customTextDeliveryClient(hub, uuid.New(), 1),
		customTextDeliveryClient(hub, uuid.New(), 1),
	}
	closeErr := errors.New("close sentinel")
	attempted := make(map[uuid.UUID]bool)
	hub.customTextClientDisconnect = func(client *Client) error {
		attempted[client.ID] = true
		if len(attempted) == 1 {
			return closeErr
		}
		return nil
	}

	err := hub.DisconnectAllRichPresenceClients(context.Background())

	require.ErrorIs(t, err, closeErr)
	for _, client := range clients {
		assert.True(t, attempted[client.ID])
	}
}

func TestDisconnectAllRichPresenceClients_AttemptsEveryClientAfterCancellation(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	clients := []*Client{
		customTextDeliveryClient(hub, uuid.New(), 1),
		customTextDeliveryClient(hub, uuid.New(), 1),
		customTextDeliveryClient(hub, uuid.New(), 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempted := make(map[uuid.UUID]bool)
	hub.customTextClientDisconnect = func(client *Client) error {
		attempted[client.ID] = true
		return nil
	}

	err := hub.DisconnectAllRichPresenceClients(ctx)

	require.ErrorIs(t, err, context.Canceled)
	for _, client := range clients {
		assert.True(t, attempted[client.ID])
	}
}

func TestDeliverRichPresence_PrivateCallFrameUsesExactWireShape(t *testing.T) {
	hub := newCustomTextDeliveryHub()
	senderID := uuid.New()
	recipientID := uuid.New()
	recipient := activityRichPresenceDeliveryClient(hub, recipientID, 1)

	err := hub.DeliverRichPresence(context.Background(), presence.DeliveryPlan{
		SenderID:         senderID,
		Category:         presence.CategoryPrivateCall,
		UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
		Payload:          json.RawMessage(`{"call_type":"dm","participant_count":2}`),
		UpdatedAt:        1784088001,
	})

	require.NoError(t, err)
	message := readClientMsg(t, recipient)
	assert.Equal(t, "rich_presence_update", message["type"])
	data, ok := message["data"].(map[string]interface{})
	require.True(t, ok)
	assert.ElementsMatch(
		t,
		[]string{"user_id", "category", "minimized", "payload", "updated_at"},
		interfaceMapKeys(data),
	)
	assert.Equal(t, senderID.String(), data["user_id"])
	assert.Equal(t, string(presence.CategoryPrivateCall), data["category"])
	assert.Equal(t, false, data["minimized"])
	assert.Equal(t, float64(1784088001), data["updated_at"])
	payload, ok := data["payload"].(map[string]interface{})
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"call_type", "participant_count"}, interfaceMapKeys(payload))
	assert.Equal(t, "dm", payload["call_type"])
	assert.Equal(t, float64(2), payload["participant_count"])
}

func TestDeliverRichPresence_RejectsInvalidPlansBeforeSending(t *testing.T) {
	recipientID := uuid.New()
	tests := []struct {
		name string
		plan presence.DeliveryPlan
	}{
		{
			name: "missing sender",
			plan: presence.DeliveryPlan{
				Category:        presence.CategoryServerVoice,
				ClearRecipients: map[uuid.UUID]bool{recipientID: true},
			},
		},
		{
			name: "unsupported category",
			plan: presence.DeliveryPlan{
				SenderID:        uuid.New(),
				Category:        presence.Category("custom_text"),
				ClearRecipients: map[uuid.UUID]bool{recipientID: true},
			},
		},
		{
			name: "update missing payload",
			plan: presence.DeliveryPlan{
				SenderID:         uuid.New(),
				Category:         presence.CategoryPrivateCall,
				UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
				UpdatedAt:        1784088000,
			},
		},
		{
			name: "update missing timestamp",
			plan: presence.DeliveryPlan{
				SenderID:         uuid.New(),
				Category:         presence.CategoryPrivateCall,
				UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
				Payload:          json.RawMessage(`{"call_type":"dm","participant_count":2}`),
			},
		},
		{
			name: "update timestamp exceeds exact JSON ceiling",
			plan: presence.DeliveryPlan{
				SenderID:         uuid.New(),
				Category:         presence.CategoryPrivateCall,
				UpdateRecipients: map[uuid.UUID]bool{recipientID: true},
				Payload:          json.RawMessage(`{"call_type":"dm","participant_count":2}`),
				UpdatedAt:        presence.MaxActivityUnixSeconds + 1,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hub := newCustomTextDeliveryHub()
			recipient := customTextDeliveryClient(hub, recipientID, 1)
			err := hub.DeliverRichPresence(context.Background(), test.plan)
			require.ErrorIs(t, err, ErrRichPresenceDeliveryPlan)
			assert.Empty(t, recipient.Send)
		})
	}
}

func assertRichPresenceClear(
	t *testing.T,
	message map[string]interface{},
	senderID uuid.UUID,
	category presence.Category,
) {
	t.Helper()
	assert.Equal(t, "rich_presence_clear", message["type"])
	data, ok := message["data"].(map[string]interface{})
	require.True(t, ok)
	if senderID != uuid.Nil {
		assert.Equal(t, senderID.String(), data["user_id"])
	}
	if category != "" {
		assert.Equal(t, string(category), data["category"])
	}
	assert.ElementsMatch(t, []string{"user_id", "category"}, interfaceMapKeys(data))
}

func assertServerVoiceUpdate(t *testing.T, message map[string]interface{}, senderID uuid.UUID) {
	t.Helper()
	assert.Equal(t, "rich_presence_update", message["type"])
	data, ok := message["data"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, senderID.String(), data["user_id"])
	assert.Equal(t, string(presence.CategoryServerVoice), data["category"])
	assert.Equal(t, true, data["minimized"])
	assert.Equal(t, float64(1784088000), data["updated_at"])
	assert.ElementsMatch(
		t,
		[]string{"user_id", "category", "minimized", "payload", "updated_at"},
		interfaceMapKeys(data),
	)
	payload, ok := data["payload"].(map[string]interface{})
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"channel_id", "server_id"}, interfaceMapKeys(payload))
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", payload["channel_id"])
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", payload["server_id"])
}

func interfaceMapKeys(values map[string]interface{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func activityRichPresenceDeliveryClient(hub *Hub, userID uuid.UUID, capacity int) *Client {
	client := customTextDeliveryClient(hub, userID, capacity)
	client.activityRichPresenceCapable = true
	return client
}
