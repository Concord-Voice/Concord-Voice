package websocket

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type dmVisibilityDeliveryFixture struct {
	db             *sql.DB
	actorID        uuid.UUID
	peerID         uuid.UUID
	conversationID uuid.UUID
	messageID      uuid.UUID
	createdAt      time.Time
	hub            *Hub
	actorClient    *Client
	peerClient     *Client
}

func newDMVisibilityDeliveryFixture(t *testing.T) dmVisibilityDeliveryFixture {
	t.Helper()
	db := setupHubTestDB(t)
	fixture := dmVisibilityDeliveryFixture{
		db:             db,
		actorID:        uuid.New(),
		peerID:         uuid.New(),
		conversationID: uuid.New(),
		messageID:      uuid.New(),
		createdAt:      time.Now().UTC().Add(-time.Minute),
	}
	cleanupDMVisibilityFixture(t, db, fixture.actorID, fixture.peerID)
	t.Cleanup(func() { cleanupDMVisibilityFixture(t, db, fixture.actorID, fixture.peerID) })

	_, err := db.Exec(`
		INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES ($1, $2, $3, 'hash', TRUE, TRUE), ($4, $5, $6, 'hash', TRUE, TRUE)`,
		fixture.actorID, fixture.actorID.String()+"@test.concord.chat", "dmvisactor",
		fixture.peerID, fixture.peerID.String()+"@test.concord.chat", "dmvispeer")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, fixture.conversationID, fixture.actorID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, fixture.conversationID, fixture.actorID, fixture.peerID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_messages (id, conversation_id, user_id, content, type, created_at) VALUES ($1, $2, $3, 'old', 'user', $4)`, fixture.messageID, fixture.conversationID, fixture.peerID, fixture.createdAt)
	require.NoError(t, err)

	fixture.hub = NewHub(db, nil)
	fixture.actorClient = &Client{ID: uuid.New(), UserID: fixture.actorID, Send: make(chan []byte, 2)}
	fixture.peerClient = &Client{ID: uuid.New(), UserID: fixture.peerID, Send: make(chan []byte, 2)}
	fixture.hub.clients[fixture.actorClient.ID] = fixture.actorClient
	fixture.hub.clients[fixture.peerClient.ID] = fixture.peerClient
	fixture.hub.userClients[fixture.actorID] = map[uuid.UUID]bool{fixture.actorClient.ID: true}
	fixture.hub.userClients[fixture.peerID] = map[uuid.UUID]bool{fixture.peerClient.ID: true}
	fixture.hub.dmSubscriptions[fixture.conversationID] = map[uuid.UUID]bool{
		fixture.actorClient.ID: true,
		fixture.peerClient.ID:  true,
	}
	return fixture
}

func (f dmVisibilityDeliveryFixture) addClearRange(t *testing.T, tx *sql.Tx) {
	t.Helper()
	_, err := tx.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
		VALUES ($1, $2, '-infinity', clock_timestamp(), TRUE)`, f.actorID, f.conversationID)
	require.NoError(t, err)
}

func (f dmVisibilityDeliveryFixture) broadcastMessage() DMBroadcastMessage {
	source := NewDMMessageVisibilitySource(f.messageID)
	return DMBroadcastMessage{
		ConversationID:   f.conversationID,
		VisibilitySource: &source,
		VisibilityMode:   dmVisibilityDeliverySubscribers,
		Data:             OutgoingMessage{Type: "dm_message", Data: map[string]interface{}{"id": f.messageID.String()}},
	}
}

func (f dmVisibilityDeliveryFixture) addClient(userID uuid.UUID, subscribed bool) *Client {
	client := &Client{ID: uuid.New(), UserID: userID, Send: make(chan []byte, 2)}
	f.hub.clients[client.ID] = client
	if f.hub.userClients[userID] == nil {
		f.hub.userClients[userID] = make(map[uuid.UUID]bool)
	}
	f.hub.userClients[userID][client.ID] = true
	if subscribed {
		f.hub.dmSubscriptions[f.conversationID][client.ID] = true
	}
	return client
}

func requireDMDelivery(t *testing.T, client *Client, want bool) {
	t.Helper()
	select {
	case frame := <-client.Send:
		if !want {
			t.Fatalf("unexpected delivery to %s: %s", client.ID, frame)
		}
	default:
		if want {
			t.Fatalf("expected delivery to %s", client.ID)
		}
	}
}

// completeDMDelivery drives the hub-owned result state machine without starting
// Hub.Run. This keeps the audience tests deterministic while still exercising
// the asynchronous lookup/worker stages used by production.
func completeDMDelivery(t *testing.T, hub *Hub, message DMBroadcastMessage) {
	t.Helper()
	hub.deliverDMMessageDerived(message)
	completePendingDMDeliveries(t, hub)
}

// completePendingDMDeliveries advances all already-queued delivery stages.
// Some audience modes first resolve participant IDs and then start a delivery
// worker, so callers that trigger delivery through a higher-level API need to
// drain until the scheduler has finished the whole job.
func completePendingDMDeliveries(t *testing.T, hub *Hub) {
	t.Helper()
	require.Eventually(t, func() bool {
		select {
		case result := <-hub.dmDeliveryResults:
			hub.handleDMMessageDeliveryResult(result)
		default:
		}
		return hub.dmDeliveryOutstanding == 0
	}, 5*time.Second, time.Millisecond, "DM delivery did not complete")
	hub.dmDeliveryWg.Wait()
}

func runDMHub(t *testing.T, hub *Hub) {
	t.Helper()
	go hub.Run()
	t.Cleanup(hub.Shutdown)
}

func TestDMMessageDeliveryAudienceModes(t *testing.T) {
	tests := []struct {
		name  string
		build func(dmVisibilityDeliveryFixture) (DMBroadcastMessage, map[*Client]bool)
	}{
		{
			name: "subscribers omit an unsubscribed session",
			build: func(fixture dmVisibilityDeliveryFixture) (DMBroadcastMessage, map[*Client]bool) {
				unsubscribedPeer := fixture.addClient(fixture.peerID, false)
				return fixture.broadcastMessage(), map[*Client]bool{
					fixture.actorClient: true, fixture.peerClient: true, unsubscribedPeer: false,
				}
			},
		},
		{
			name: "all connected honors actor exclusion and an unsubscribed session",
			build: func(fixture dmVisibilityDeliveryFixture) (DMBroadcastMessage, map[*Client]bool) {
				unsubscribedPeer := fixture.addClient(fixture.peerID, false)
				message := fixture.broadcastMessage()
				message.VisibilityMode = dmVisibilityDeliveryAllConnected
				message.ExcludeUser = &fixture.actorID
				return message, map[*Client]bool{
					fixture.actorClient: false, fixture.peerClient: true, unsubscribedPeer: true,
				}
			},
		},
		{
			name: "unread skips every session for a user with a subscription",
			build: func(fixture dmVisibilityDeliveryFixture) (DMBroadcastMessage, map[*Client]bool) {
				unsubscribedPeer := fixture.addClient(fixture.peerID, false)
				message := fixture.broadcastMessage()
				message.VisibilityMode = dmVisibilityDeliveryUnreadUnsubscribed
				message.ExcludeUser = &fixture.actorID
				return message, map[*Client]bool{
					fixture.actorClient: false, fixture.peerClient: false, unsubscribedPeer: false,
				}
			},
		},
		{
			name: "mentions skip subscribed clients but retain other target sessions",
			build: func(fixture dmVisibilityDeliveryFixture) (DMBroadcastMessage, map[*Client]bool) {
				unsubscribedPeer := fixture.addClient(fixture.peerID, false)
				message := fixture.broadcastMessage()
				message.VisibilityMode = dmVisibilityDeliveryMentionTargets
				message.MentionTargets = map[uuid.UUID]bool{fixture.peerID: true}
				return message, map[*Client]bool{
					fixture.actorClient: false, fixture.peerClient: false, unsubscribedPeer: true,
				}
			},
		},
		{
			name: "origin acknowledgement reaches only its visible source client",
			build: func(fixture dmVisibilityDeliveryFixture) (DMBroadcastMessage, map[*Client]bool) {
				message := fixture.broadcastMessage()
				message.VisibilityMode = dmVisibilityDeliveryOriginClient
				message.OriginClientID = &fixture.actorClient.ID
				message.Data.Type = "dm_message_ack"
				return message, map[*Client]bool{fixture.actorClient: true, fixture.peerClient: false}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDMVisibilityDeliveryFixture(t)
			message, expected := test.build(fixture)
			completeDMDelivery(t, fixture.hub, message)
			for client, want := range expected {
				requireDMDelivery(t, client, want)
			}
		})
	}
}

func TestDMExpirationEventAllParticipantsRespectsClearVisibility(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	_, err := fixture.db.Exec(`
		UPDATE dm_messages
		SET type = 'expiration_event', expiration_event_payload = '{}'::jsonb
		WHERE id = $1`, fixture.messageID)
	require.NoError(t, err)
	runDMHub(t, fixture.hub)
	source := NewDMMessageVisibilitySource(fixture.messageID)
	event := OutgoingMessage{Type: "dm_expiration_event", Data: map[string]interface{}{
		"id": fixture.messageID.String(), "conversation_id": fixture.conversationID.String(),
	}}

	fixture.hub.BroadcastToDMMessageAllParticipants(fixture.conversationID, source, uuid.Nil, event)
	assert.Equal(t, "dm_expiration_event", readClientMsg(t, fixture.actorClient)["type"])
	assert.Equal(t, "dm_expiration_event", readClientMsg(t, fixture.peerClient)["type"])

	tx, err := fixture.db.Begin()
	require.NoError(t, err)
	fixture.addClearRange(t, tx)
	require.NoError(t, tx.Commit())

	fixture.hub.BroadcastToDMMessageAllParticipants(fixture.conversationID, source, uuid.Nil, event)
	select {
	case frame := <-fixture.actorClient.Send:
		t.Fatalf("cleared actor received expiration event: %s", frame)
	case <-time.After(100 * time.Millisecond):
	}
	assert.Equal(t, "dm_expiration_event", readClientMsg(t, fixture.peerClient)["type"])
}

func TestDMMessageDeliveryCandidatesExcludeUnrelatedConnectedUser(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	outsider := fixture.addClient(uuid.New(), false)
	message := fixture.broadcastMessage()
	message.VisibilityMode = dmVisibilityDeliveryAllConnected

	candidates := fixture.hub.snapshotDMMessageDeliveryCandidates(message, map[uuid.UUID]bool{
		fixture.actorID: true,
		fixture.peerID:  true,
	})
	candidateIDs := make(map[uuid.UUID]bool, len(candidates))
	for _, candidate := range candidates {
		candidateIDs[candidate.client.ID] = true
	}
	assert.Equal(t, map[uuid.UUID]bool{
		fixture.actorClient.ID: true,
		fixture.peerClient.ID:  true,
	}, candidateIDs)
	assert.NotContains(t, candidateIDs, outsider.ID)
}

func TestDMMessageDeliveryFullQueueReleasesParticipantLock(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	fixture.hub.redis = setupHubTestRedis(t)
	for range cap(fixture.actorClient.Send) {
		fixture.actorClient.Send <- []byte("full")
	}
	completeDMDelivery(t, fixture.hub, fixture.broadcastMessage())

	_, actorStillRegistered := fixture.hub.clients[fixture.actorClient.ID]
	assert.False(t, actorStillRegistered, "failed outbound delivery should unregister after transaction release")
	probe, err := fixture.db.Begin()
	require.NoError(t, err)
	defer func() { _ = probe.Rollback() }()
	var userID uuid.UUID
	require.NoError(t, probe.QueryRow(`SELECT user_id FROM dm_participants WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE NOWAIT`, fixture.conversationID, fixture.actorID).Scan(&userID))
	peer := readClientMsg(t, fixture.peerClient)
	assert.Equal(t, "dm_message", peer["type"])
}

func TestVisibleDMMessageRecipients_UsesEachParticipantViewer(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	tx, err := fixture.db.Begin()
	require.NoError(t, err)
	fixture.addClearRange(t, tx)
	require.NoError(t, tx.Commit())

	completeDMDelivery(t, fixture.hub, fixture.broadcastMessage())
	select {
	case frame := <-fixture.actorClient.Send:
		t.Fatalf("cleared actor received source frame: %s", frame)
	default:
	}
	peer := readClientMsg(t, fixture.peerClient)
	assert.Equal(t, "dm_message", peer["type"])
}

func TestDMMessageDeliveryOriginAckRespectsClearRange(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	tx, err := fixture.db.Begin()
	require.NoError(t, err)
	fixture.addClearRange(t, tx)
	require.NoError(t, tx.Commit())

	source := NewDMMessageVisibilitySource(fixture.messageID)
	completeDMDelivery(t, fixture.hub, DMBroadcastMessage{
		ConversationID:   fixture.conversationID,
		VisibilitySource: &source,
		VisibilityMode:   dmVisibilityDeliveryOriginClient,
		OriginClientID:   &fixture.actorClient.ID,
		Data:             OutgoingMessage{Type: "dm_message_ack", Data: map[string]interface{}{"id": fixture.messageID.String()}},
	})
	select {
	case frame := <-fixture.actorClient.Send:
		t.Fatalf("cleared actor received acknowledgement: %s", frame)
	default:
	}
}

func TestDeletedDMMessageDeliveryUsesCapturedSource(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	tx, err := fixture.db.Begin()
	require.NoError(t, err)
	fixture.addClearRange(t, tx)
	_, err = tx.Exec(`DELETE FROM dm_messages WHERE id = $1`, fixture.messageID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	source := NewDeletedDMMessageVisibilitySource(fixture.peerID, fixture.createdAt)
	completeDMDelivery(t, fixture.hub, DMBroadcastMessage{
		ConversationID:   fixture.conversationID,
		VisibilitySource: &source,
		VisibilityMode:   dmVisibilityDeliverySubscribers,
		Data:             OutgoingMessage{Type: "dm_message_deleted", Data: map[string]interface{}{"id": fixture.messageID.String()}},
	})
	select {
	case frame := <-fixture.actorClient.Send:
		t.Fatalf("cleared actor received deleted-source frame: %s", frame)
	default:
	}
	peer := readClientMsg(t, fixture.peerClient)
	assert.Equal(t, "dm_message_deleted", peer["type"])
}

func TestRespawnDMParticipantVisibility_StrictlyRequiresLaterMessage(t *testing.T) {
	db := setupHubTestDB(t)
	actorID, peerID, conversationID := uuid.New(), uuid.New(), uuid.New()
	cleanupDMVisibilityFixture(t, db, actorID, peerID)
	t.Cleanup(func() { cleanupDMVisibilityFixture(t, db, actorID, peerID) })

	_, err := db.Exec(`
		INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		VALUES ($1, $2, $3, 'hash', TRUE, TRUE), ($4, $5, $6, 'hash', TRUE, TRUE)`,
		actorID, actorID.String()+"@test.concord.chat", "dmrespawnactor",
		peerID, peerID.String()+"@test.concord.chat", "dmrespawnpeer")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, conversationID, actorID)
	require.NoError(t, err)
	hiddenAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	_, err = db.Exec(`
		INSERT INTO dm_participants (conversation_id, user_id, hidden_at)
		VALUES ($1, $2, $3), ($1, $4, $3)`, conversationID, actorID, hiddenAt, peerID)
	require.NoError(t, err)

	t.Run("later timestamp clears both hides", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		require.NoError(t, respawnDMParticipantVisibility(context.Background(), tx, conversationID, hiddenAt.Add(time.Microsecond)))
		require.NoError(t, tx.Commit())

		var hiddenCount int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM dm_participants WHERE conversation_id = $1 AND hidden_at IS NOT NULL`, conversationID).Scan(&hiddenCount))
		assert.Equal(t, 0, hiddenCount)
	})

	_, err = db.Exec(`UPDATE dm_participants SET hidden_at = $2 WHERE conversation_id = $1`, conversationID, hiddenAt)
	require.NoError(t, err)
	t.Run("equal timestamp remains hidden", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		require.NoError(t, respawnDMParticipantVisibility(context.Background(), tx, conversationID, hiddenAt))
		require.NoError(t, tx.Commit())

		var hiddenCount int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM dm_participants WHERE conversation_id = $1 AND hidden_at IS NOT NULL`, conversationID).Scan(&hiddenCount))
		assert.Equal(t, 2, hiddenCount)
	})
}

func TestDMDeliveryBarrier_ClearWinsSuppressesOldFrame(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	clearTx, err := fixture.db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = clearTx.Rollback() })
	var lockedUserID uuid.UUID
	require.NoError(t, clearTx.QueryRow(`SELECT user_id FROM dm_participants WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE`, fixture.conversationID, fixture.actorID).Scan(&lockedUserID))
	require.Equal(t, fixture.actorID, lockedUserID)
	fixture.addClearRange(t, clearTx)

	// Keep Run stopped so this test can join the worker and apply its result
	// before it asserts the actor received no stale frame.
	fixture.hub.deliverDMMessageDerived(fixture.broadcastMessage())
	requireDMVisibilityQueryWait(t, fixture.db, "%ORDER BY user_id FOR SHARE%", "delivery should wait for Clear's actor participant lock")
	require.NoError(t, clearTx.Commit())
	completePendingDMDeliveries(t, fixture.hub)
	select {
	case frame := <-fixture.actorClient.Send:
		t.Fatalf("Clear-winning delivery reached actor: %s", frame)
	default:
	}
	peer := readClientMsg(t, fixture.peerClient)
	assert.Equal(t, "dm_message", peer["type"])

	fixture.hub.handleUserBroadcast(UserBroadcastMessage{UserID: fixture.actorID, Data: OutgoingMessage{
		Type: "dm_conversation_cleared", Data: map[string]interface{}{"conversation_id": fixture.conversationID.String()},
	}})
	cleared := readClientMsg(t, fixture.actorClient)
	assert.Equal(t, "dm_conversation_cleared", cleared["type"])
}

func TestDMDeliveryBarrier_BusyOutboundHandoffDoesNotBlockClear(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	// Keep the actor subscribed so this test has one candidate whose outbound
	// sender can be made busy. The peer remains subscribed in the fixture but is
	// intentionally omitted from this delivery's candidate snapshot.
	delete(fixture.hub.dmSubscriptions[fixture.conversationID], fixture.peerClient.ID)
	runDMHub(t, fixture.hub)
	fixture.actorClient.sendMu.Lock()
	actorSendLocked := true
	t.Cleanup(func() {
		if actorSendLocked {
			fixture.actorClient.sendMu.Unlock()
		}
	})

	deliveryDone := make(chan struct{})
	go func() {
		fixture.hub.dmBroadcast <- fixture.broadcastMessage()
		close(deliveryDone)
	}()
	select {
	case <-deliveryDone:
	case <-time.After(time.Second):
		t.Fatal("hub did not accept the DM delivery")
	}

	clearStarted := make(chan struct{})
	clearResult := make(chan error, 1)
	go func() {
		close(clearStarted)
		clearTx, err := fixture.db.Begin()
		if err != nil {
			clearResult <- err
			return
		}
		defer func() { _ = clearTx.Rollback() }()
		var userID uuid.UUID
		if err = clearTx.QueryRow(`SELECT user_id FROM dm_participants WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE`, fixture.conversationID, fixture.actorID).Scan(&userID); err == nil {
			_, err = clearTx.Exec(`INSERT INTO dm_message_hidden_ranges (user_id, conversation_id, hidden_from, hidden_to, includes_own)
				VALUES ($1, $2, '-infinity', clock_timestamp(), TRUE)`, fixture.actorID, fixture.conversationID)
			if err == nil {
				err = clearTx.Commit()
			}
		}
		clearResult <- err
	}()
	<-clearStarted
	select {
	case err := <-clearResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Clear did not commit after the delivery dropped its busy outbound handoff")
	}
	select {
	case frame := <-fixture.actorClient.Send:
		t.Fatalf("busy outbound handoff must not enqueue a stale DM frame: %s", frame)
	default:
	}
	fixture.actorClient.sendMu.Unlock()
	actorSendLocked = false

	fixture.hub.userBroadcast <- UserBroadcastMessage{UserID: fixture.actorID, Data: OutgoingMessage{
		Type: "dm_conversation_cleared", Data: map[string]interface{}{"conversation_id": fixture.conversationID.String()},
	}}
	cleared := readClientMsg(t, fixture.actorClient)
	assert.Equal(t, "dm_conversation_cleared", cleared["type"])
}

func TestDMDeliveryScheduler_DoesNotBlockUnrelatedGlobalBroadcast(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	runDMHub(t, fixture.hub)
	clearTx, err := fixture.db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = clearTx.Rollback() })
	var lockedUserID uuid.UUID
	require.NoError(t, clearTx.QueryRow(`SELECT user_id FROM dm_participants WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE`, fixture.conversationID, fixture.actorID).Scan(&lockedUserID))
	fixture.addClearRange(t, clearTx)

	queued := make(chan struct{})
	go func() {
		fixture.hub.dmBroadcast <- fixture.broadcastMessage()
		close(queued)
	}()
	requireDMVisibilityQueryWait(t, fixture.db, "%ORDER BY user_id FOR SHARE%", "DM worker should be blocked by the Clear-like participant lock")

	fixture.hub.BroadcastToAll(OutgoingMessage{Type: "unrelated_global"})
	assert.Equal(t, "unrelated_global", readClientMsg(t, fixture.actorClient)["type"], "global broadcast must progress while DM worker waits")
	assert.Equal(t, "unrelated_global", readClientMsg(t, fixture.peerClient)["type"])
	require.NoError(t, clearTx.Commit())
	select {
	case <-queued:
	case <-time.After(5 * time.Second):
		t.Fatal("DM enqueue did not return")
	}
	assert.Equal(t, "dm_message", readClientMsg(t, fixture.peerClient)["type"])
	select {
	case frame := <-fixture.actorClient.Send:
		t.Fatalf("Clear-winning delivery reached actor: %s", frame)
	default:
	}
}

func TestDMDeliveryScheduler_OlderResultKeepsFreshSubscription(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	_, err := fixture.db.Exec(`DELETE FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, fixture.conversationID, fixture.peerID)
	require.NoError(t, err)

	first := fixture.broadcastMessage()
	first.Data.Type = "dm_delivery_before_rejoin"
	fixture.hub.deliverDMMessageDerived(first)
	var firstResult dmMessageDeliveryResult
	select {
	case firstResult = <-fixture.hub.dmDeliveryResults:
	case <-time.After(5 * time.Second):
		t.Fatal("older delivery did not complete")
	}

	// A group-DM member can be removed while a delivery is in flight, then be
	// restored and subscribe again before that older result reaches Run. The old
	// result must never remove this fresh, valid subscription.
	_, err = fixture.db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, fixture.conversationID, fixture.peerID)
	require.NoError(t, err)
	fixture.hub.handleSubscribeDM(IncomingMessage{
		Type:     "subscribe_dm",
		ClientID: fixture.peerClient.ID,
		Data:     map[string]interface{}{keyConversationID: fixture.conversationID.String()},
	})
	assert.Equal(t, "dm_subscribed", readClientMsg(t, fixture.peerClient)["type"])
	fixture.hub.handleDMMessageDeliveryResult(firstResult)

	second := fixture.broadcastMessage()
	second.Data.Type = "dm_delivery_after_rejoin"
	completeDMDelivery(t, fixture.hub, second)
	assert.Equal(t, "dm_delivery_after_rejoin", readClientMsg(t, fixture.peerClient)["type"])
}

func TestDMDeliveryScheduler_PreservesFIFOForQueuedConversation(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	fixture.actorClient.Send = make(chan []byte, 4)
	fixture.peerClient.Send = make(chan []byte, 4)

	clearTx, err := fixture.db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = clearTx.Rollback() })
	var lockedUserID uuid.UUID
	require.NoError(t, clearTx.QueryRow(`SELECT user_id FROM dm_participants WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE`, fixture.conversationID, fixture.actorID).Scan(&lockedUserID))

	first := fixture.broadcastMessage()
	first.Data.Type = "dm_delivery_first"
	second := fixture.broadcastMessage()
	second.Data.Type = "dm_delivery_second"
	fixture.hub.deliverDMMessageDerived(first)
	requireDMVisibilityQueryWaitCount(t, fixture.db, "%ORDER BY user_id FOR SHARE%", 1, "first delivery worker should be blocked by the participant lock")
	fixture.hub.deliverDMMessageDerived(second)

	// Workers never mutate scheduler state. With Run stopped, these checks prove
	// the second job is queued behind the first rather than starting another SQL
	// worker against the same conversation.
	require.Equal(t, 2, fixture.hub.dmDeliveryOutstanding)
	require.Len(t, fixture.hub.dmDeliveryQueues[fixture.conversationID], 2)
	requireDMVisibilityQueryWaitCount(t, fixture.db, "%ORDER BY user_id FOR SHARE%", 1, "only the FIFO head may hold a delivery worker")

	require.NoError(t, clearTx.Commit())
	completeQueuedDMDeliveries(t, fixture.hub, 2)

	assert.Equal(t, "dm_delivery_first", readClientMsg(t, fixture.actorClient)["type"])
	assert.Equal(t, "dm_delivery_second", readClientMsg(t, fixture.actorClient)["type"])
	assert.Equal(t, "dm_delivery_first", readClientMsg(t, fixture.peerClient)["type"])
	assert.Equal(t, "dm_delivery_second", readClientMsg(t, fixture.peerClient)["type"])
}

func TestDMDeliveryScheduler_GlobalCapacityIncludesQueuedAndActive(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	clearTx, err := fixture.db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = clearTx.Rollback() })
	var lockedUserID uuid.UUID
	require.NoError(t, clearTx.QueryRow(`SELECT user_id FROM dm_participants WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE`, fixture.conversationID, fixture.actorID).Scan(&lockedUserID))

	accepted := fixture.broadcastMessage()
	accepted.Data.Type = "dm_delivery_accepted"
	fixture.hub.deliverDMMessageDerived(accepted)
	requireDMVisibilityQueryWaitCount(t, fixture.db, "%ORDER BY user_id FOR SHARE%", 1, "the active delivery must hold the first global-capacity slot")
	for range dmMessageDeliveryCapacity - 1 {
		fixture.hub.deliverDMMessageDerived(accepted)
	}
	require.Equal(t, dmMessageDeliveryCapacity, fixture.hub.dmDeliveryOutstanding)
	require.Len(t, fixture.hub.dmDeliveryQueues[fixture.conversationID], dmMessageDeliveryCapacity)

	dropped := fixture.broadcastMessage()
	dropped.Data.Type = "dm_delivery_dropped"
	fixture.hub.deliverDMMessageDerived(dropped)
	assert.Equal(t, dmMessageDeliveryCapacity, fixture.hub.dmDeliveryOutstanding, "the global budget counts the active job plus queued jobs")
	assert.Len(t, fixture.hub.dmDeliveryQueues[fixture.conversationID], dmMessageDeliveryCapacity, "a job beyond the global budget must be dropped rather than queued")

	require.NoError(t, clearTx.Commit())
	select {
	case <-fixture.hub.dmDeliveryResults:
	case <-time.After(5 * time.Second):
		t.Fatal("active delivery worker did not finish after its participant lock released")
	}
	fixture.hub.dmDeliveryWg.Wait()
}

func TestDMDeliveryScheduler_ShutdownJoinsActiveWorkerAndAbandonsQueuedJob(t *testing.T) {
	fixture := newDMVisibilityDeliveryFixture(t)
	clearTx, err := fixture.db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = clearTx.Rollback() })
	var lockedUserID uuid.UUID
	require.NoError(t, clearTx.QueryRow(`SELECT user_id FROM dm_participants WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE`, fixture.conversationID, fixture.actorID).Scan(&lockedUserID))

	active := fixture.broadcastMessage()
	active.Data.Type = "dm_delivery_active_before_shutdown"
	queued := fixture.broadcastMessage()
	queued.Data.Type = "dm_delivery_abandoned_after_shutdown"
	fixture.hub.deliverDMMessageDerived(active)
	requireDMVisibilityQueryWaitCount(t, fixture.db, "%ORDER BY user_id FOR SHARE%", 1, "active delivery worker should be blocked before shutdown begins")
	fixture.hub.deliverDMMessageDerived(queued)
	require.Equal(t, 2, fixture.hub.dmDeliveryOutstanding)
	require.Len(t, fixture.hub.dmDeliveryQueues[fixture.conversationID], 2)
	requireDMVisibilityQueryWaitCount(t, fixture.db, "%ORDER BY user_id FOR SHARE%", 1, "queued work must not start a second worker before shutdown")

	go fixture.hub.Run()
	shutdownDone := make(chan struct{})
	go func() {
		fixture.hub.Shutdown()
		close(shutdownDone)
	}()
	<-fixture.hub.done
	require.NoError(t, clearTx.Commit())

	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown deadlocked while joining the active DM delivery worker")
	}

	assertDMDeliveryShutdownFrames(t, fixture.actorClient, active.Data.Type, queued.Data.Type)
	assertDMDeliveryShutdownFrames(t, fixture.peerClient, active.Data.Type, queued.Data.Type)
}

func completeQueuedDMDeliveries(t *testing.T, hub *Hub, count int) {
	t.Helper()
	for range count {
		select {
		case result := <-hub.dmDeliveryResults:
			hub.handleDMMessageDeliveryResult(result)
		case <-time.After(5 * time.Second):
			t.Fatal("queued DM delivery did not produce a result")
		}
	}
	assert.Zero(t, hub.dmDeliveryOutstanding)
	hub.dmDeliveryWg.Wait()
}

func assertDMDeliveryShutdownFrames(t *testing.T, client *Client, activeType, abandonedType string) {
	t.Helper()
	types := make([]string, 0)
	for frame := range client.Send {
		var message struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(frame, &message))
		types = append(types, message.Type)
	}
	assert.Contains(t, types, activeType, "shutdown must join the active worker before closing outbound")
	assert.NotContains(t, types, abandonedType, "shutdown must abandon queued delivery work")
}

func requireDMVisibilityQueryWait(t *testing.T, db *sql.DB, queryPattern, message string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE $1
		)`, queryPattern).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, message)
}

func requireDMVisibilityQueryWaitCount(t *testing.T, db *sql.DB, queryPattern string, want int, message string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int
		err := db.QueryRow(`SELECT count(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE $1`, queryPattern).Scan(&waiting)
		return err == nil && waiting == want
	}, 5*time.Second, 10*time.Millisecond, message)
}

func cleanupDMVisibilityFixture(t *testing.T, db *sql.DB, actorID, peerID uuid.UUID) {
	t.Helper()
	// This helper is intentionally best-effort: the test database truncation is
	// authoritative, while deleting users makes reruns safe on a reused DB.
	_, _ = db.Exec("DELETE FROM users WHERE id IN ($1, $2)", actorID, peerID)
}
