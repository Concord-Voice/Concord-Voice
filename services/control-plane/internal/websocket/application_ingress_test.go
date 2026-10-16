package websocket

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	gorillaws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func startReadPump(t *testing.T, incomingCapacity int) (*Client, *gorillaws.Conn, <-chan struct{}) {
	t.Helper()
	hub := NewHub(nil, nil)
	hub.incoming = make(chan IncomingMessage, incomingCapacity)
	ready := make(chan *Client, 1)
	stopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := gorillaws.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := newTestClient(hub, uuid.New())
		client.Conn = conn
		ready <- client
		client.readPump()
		close(stopped)
	}))
	t.Cleanup(server.Close)
	peer, _, err := gorillaws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, peer.Close())
		select {
		case <-hub.done:
		default:
			close(hub.done)
		}
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("read pump did not stop")
		}
	})
	return <-ready, peer, stopped
}

func TestReadPumpPacesEveryApplicationFrame(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		name := "all event types and both data opcodes"
		if malformed {
			name = "malformed JSON also consumes budget"
		}
		t.Run(name, func(t *testing.T) {
			client, peer, _ := startReadPump(t, 256)
			types := []string{
				"subscribe", "unsubscribe", "subscribe_server", "unsubscribe_server",
				"message", "typing", "profile_update", "heartbeat", "set_status",
				"server_update", "subscribe_dm", "unsubscribe_dm", "connection_ready_probe",
				"dm_message", "dm_typing", "unknown",
			}
			started := time.Now()
			const burst = applicationFrameBurst
			const extra = 10
			require.Less(t, burst, cap(client.Hub.incoming), "one socket must not fill the shared inbound queue immediately")
			for index := 0; index <= burst+extra; index++ {
				payload, err := json.Marshal(map[string]interface{}{
					"type": types[index%len(types)], "data": map[string]interface{}{"sequence": index},
				})
				require.NoError(t, err)
				if malformed && index < burst+extra {
					payload = []byte("{")
				}
				opcode := gorillaws.TextMessage
				if index%2 != 0 {
					opcode = gorillaws.BinaryMessage
				}
				require.NoError(t, peer.WriteMessage(opcode, payload))
			}
			first := 0
			if malformed {
				first = burst + extra
			}
			for index := first; index <= burst+extra; index++ {
				select {
				case msg := <-client.Hub.incoming:
					require.Equal(t, float64(index), msg.Data["sequence"], "subscription replay must stay ordered without drops")
					require.Equal(t, client.UserID, msg.UserID)
					require.Equal(t, client.ID, msg.ClientID)
				case <-time.After(3 * time.Second):
					t.Fatal("application frame was lost")
				}
			}
			// A half-budget margin tolerates scheduling overhead while detecting unmetered ingress.
			require.GreaterOrEqual(t, time.Since(started), 250*time.Millisecond)
			require.True(t, client.rateLastFill.IsZero(), "application pacing must leave the chat quota independent")
		})
	}
}

func TestReadPumpStopsWhenHubQueueFull(t *testing.T) {
	client, peer, stopped := startReadPump(t, 0)
	require.NoError(t, peer.WriteMessage(gorillaws.TextMessage, []byte(`{"type":"heartbeat"}`)))
	close(client.Hub.done)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("hub shutdown stranded a reader on the incoming or unregister queue")
	}
}

func TestReadPumpDataFramesKeepActiveConnectionAlive(t *testing.T) {
	client, peer, stopped := startReadPump(t, 64)
	require.NoError(t, peer.WriteMessage(gorillaws.TextMessage, []byte(`{"type":"heartbeat"}`)))
	select {
	case <-client.Hub.incoming:
	case <-time.After(time.Second):
		t.Fatal("read pump did not start")
	}
	// Shorten the 60-second failure window; frames exceed Gorilla's read buffer so a later read observes it.
	require.NoError(t, client.Conn.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	payload := []byte(`{"type":"heartbeat","data":{"padding":"` + strings.Repeat("x", 400) + `"}}`)
	for i := 0; i < applicationFrameBurst+25; i++ {
		require.NoError(t, peer.WriteMessage(gorillaws.TextMessage, payload))
	}
	for i := 0; i < applicationFrameBurst+25; i++ {
		select {
		case <-client.Hub.incoming:
		case <-stopped:
			t.Fatal("active connection closed while paced data frames waited ahead of a pong")
		case <-time.After(3 * time.Second):
			t.Fatal("paced data frame was not delivered")
		}
	}
}

func TestRequiredOutboundFullQueueDoesNotBlockWriter(t *testing.T) {
	client, peer, _ := startReadPump(t, 1)
	client.Send = make(chan []byte, 1)
	client.Send <- []byte("queued")
	require.True(t, client.claimOutboundWrite())
	done := make(chan bool, 1)
	go func() { done <- client.enqueueOutboundBlocking([]byte("confirmation")) }()
	select {
	case accepted := <-done:
		require.False(t, accepted)
	case <-time.After(time.Second):
		<-client.Send
		<-done
		client.releaseOutboundWrite()
		t.Fatal("a full confirmation queue blocks the hub and the writer's completion")
	}
	client.releaseOutboundWrite()
	require.Equal(t, []byte("queued"), <-client.Send)
	_, open := <-client.Send
	require.False(t, open, "a lost required confirmation must terminate the connection")
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err := peer.ReadMessage()
	require.Error(t, err, "a lost required confirmation must promptly close the socket")
}

func TestRequiredOutboundAvailableQueuePreservesConfirmation(t *testing.T) {
	client := &Client{Send: make(chan []byte, 1)}
	require.True(t, client.enqueueOutboundBlocking([]byte("confirmation")))
	require.Equal(t, []byte("confirmation"), <-client.Send)
	require.False(t, client.sendClosed)
}

func TestTypingFullQueueDoesNotBlockHub(t *testing.T) {
	for _, dm := range []bool{false, true} {
		name := "channel"
		if dm {
			name = "DM"
		}
		t.Run(name, func(t *testing.T) {
			hub := newMinimalHub()
			client := newTestClient(hub, uuid.New())
			hub.clients[client.ID] = client
			resourceID := uuid.New()
			msg := IncomingMessage{UserID: client.UserID, ClientID: client.ID}
			var invoke, drain func()
			if dm {
				hub.dmSubscriptions[resourceID] = map[uuid.UUID]bool{client.ID: true}
				hub.dmBroadcast = make(chan DMBroadcastMessage, 1)
				hub.dmBroadcast <- DMBroadcastMessage{}
				msg.Data = map[string]interface{}{keyConversationID: resourceID.String(), keyIsTyping: true}
				invoke = func() { hub.handleDMTyping(msg) }
				drain = func() { <-hub.dmBroadcast }
			} else {
				client.Channels[resourceID] = true
				hub.channelSubscriptions[resourceID] = map[uuid.UUID]bool{client.ID: true}
				hub.broadcast = make(chan BroadcastMessage, 1)
				hub.broadcast <- BroadcastMessage{}
				msg.Data = map[string]interface{}{keyChannelID: resourceID.String(), keyIsTyping: true}
				invoke = func() { hub.handleTyping(msg) }
				drain = func() { <-hub.broadcast }
			}
			done := make(chan struct{})
			go func() { invoke(); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				drain()
				<-done
				t.Fatal("typing blocked the sole broadcast queue consumer")
			}
		})
	}
}

func TestHandleServerUpdateBoundsDatabaseWait(t *testing.T) {
	db, driver := openBlockingChannelContextDB(t)
	hub := NewHub(db, nil)
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	serverID := uuid.New()
	hub.serverSubscriptions[serverID] = map[uuid.UUID]bool{client.ID: true}
	done := make(chan struct{})
	go func() {
		hub.handleServerUpdate(IncomingMessage{
			UserID: client.UserID, ClientID: client.ID,
			Data: map[string]interface{}{keyServerID: serverID.String()},
		})
		close(done)
	}()
	select {
	case <-driver.entered:
	case <-time.After(time.Second):
		t.Fatal("server update did not reach the database")
	}
	hasDeadline := <-driver.hasDeadline
	require.True(t, hasDeadline, "client-triggered server metadata reads must have a deadline")
	select {
	case <-done:
	case <-time.After(channelAuthCtxTimeout + time.Second):
		close(driver.release)
		<-done
		t.Fatal("server metadata read exceeded its three-second deadline")
	}
}

func TestHandleServerUpdateRejectsUnsubscribedBeforeDatabase(t *testing.T) {
	db, driver := openBlockingChannelContextDB(t)
	hub := NewHub(db, nil)
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	serverID := uuid.New()
	done := make(chan struct{})
	go func() {
		hub.handleServerUpdate(IncomingMessage{
			UserID: client.UserID, ClientID: client.ID,
			Data: map[string]interface{}{keyServerID: serverID.String()},
		})
		close(done)
	}()
	select {
	case <-done:
	case <-driver.entered:
		close(driver.release)
		<-done
		t.Fatal("unsubscribed server_update reached the database")
	case <-time.After(time.Second):
		close(driver.release)
		t.Fatal("server_update did not finish")
	}
}
