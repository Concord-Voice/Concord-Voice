//go:build integration

package websocket

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func expirationDMHub(t *testing.T) (*Hub, uuid.UUID, []uuid.UUID) {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	users := []uuid.UUID{testdb.CreateUser(t, db), testdb.CreateUser(t, db)}
	conversationID := uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, false, true, $2)`, conversationID, users[0])
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversationID, users[0], users[1])
	require.NoError(t, err)
	return NewHub(db, nil), conversationID, users
}

func TestBroadcastToDMParticipantsContext_RoutesCurrentParticipantsRegardlessOfSelectedDM(t *testing.T) {
	hub, conversationID, users := expirationDMHub(t)
	first := newTestClient(hub, users[0])
	second := newTestClient(hub, users[1])
	hub.registerClient(first)
	hub.registerClient(second)

	done := make(chan bool, 1)
	go func() {
		done <- hub.BroadcastToDMParticipantsContext(context.Background(), conversationID, OutgoingMessage{Type: "dm_purged"})
	}()
	for range users {
		select {
		case message := <-hub.userBroadcast:
			hub.handleUserBroadcast(message)
		case <-time.After(time.Second):
			t.Fatal("participant delivery did not reach the user terminal")
		}
	}
	select {
	case delivered := <-done:
		require.True(t, delivered)
	case <-time.After(time.Second):
		t.Fatal("participant delivery did not complete")
	}
	for _, client := range []*Client{first, second} {
		select {
		case data := <-client.Send:
			assert.Contains(t, string(data), `"type":"dm_purged"`)
		case <-time.After(time.Second):
			t.Fatal("current participant did not receive DM purge outside the selected conversation")
		}
	}
}

func TestBroadcastToDMParticipantsContext_CancelDuringParticipantLookup(t *testing.T) {
	hub, conversationID, _ := expirationDMHub(t)
	tx, err := hub.db.Begin()
	require.NoError(t, err)
	_, err = tx.Exec(`LOCK TABLE dm_participants IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tx.Rollback()) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- hub.BroadcastToDMParticipantsContext(ctx, conversationID, OutgoingMessage{Type: "dm_purged"})
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := hub.db.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query ILIKE '%FROM dm_participants%'
		)`).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond, "participant resolver must be blocked on the controlled dm_participants lock")
	cancel()
	select {
	case delivered := <-done:
		assert.False(t, delivered)
	case <-time.After(time.Second):
		t.Fatal("canceled participant lookup did not return")
	}
}

func TestBroadcastToDMParticipantsContext_CancelWhenUserQueueIsFull(t *testing.T) {
	hub, conversationID, _ := expirationDMHub(t)
	for i := 0; i < cap(hub.userBroadcast); i++ {
		hub.userBroadcast <- UserBroadcastMessage{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- hub.BroadcastToDMParticipantsContext(ctx, conversationID, OutgoingMessage{Type: "dm_purged"})
	}()
	select {
	case delivered := <-done:
		t.Fatalf("full user queue returned before cancellation: %t", delivered)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case delivered := <-done:
		assert.False(t, delivered)
	case <-time.After(time.Second):
		t.Fatal("canceled participant delivery blocked on a full user queue")
	}
}
