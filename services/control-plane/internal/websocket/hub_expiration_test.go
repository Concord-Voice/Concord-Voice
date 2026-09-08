package websocket

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBroadcastToDMParticipantsContext_CanceledEntryReturnsFalse(t *testing.T) {
	hub := NewHub(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, hub.BroadcastToDMParticipantsContext(ctx, uuid.New(), OutgoingMessage{Type: "dm_purged"}))
}

func TestBroadcastToDMParticipantsContext_NoDatabaseFallsBackToDMQueue(t *testing.T) {
	hub := NewHub(nil, nil)
	conversationID := uuid.New()
	msg := OutgoingMessage{Type: "dm_purged"}
	require.True(t, hub.BroadcastToDMParticipantsContext(context.Background(), conversationID, msg))
	select {
	case queued := <-hub.dmBroadcast:
		assert.Equal(t, conversationID, queued.ConversationID)
		assert.Equal(t, msg, queued.Data)
	case <-time.After(time.Second):
		t.Fatal("fallback DM broadcast was not queued")
	}
}

func TestBroadcastToDMParticipantsContext_CanceledFullFallbackDoesNotBlock(t *testing.T) {
	hub := NewHub(nil, nil)
	for i := 0; i < cap(hub.dmBroadcast); i++ {
		hub.dmBroadcast <- DMBroadcastMessage{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- hub.BroadcastToDMParticipantsContext(ctx, uuid.New(), OutgoingMessage{Type: "dm_purged"})
	}()
	select {
	case got := <-done:
		t.Fatalf("full fallback queue returned before cancellation: %t", got)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case got := <-done:
		assert.False(t, got)
	case <-time.After(time.Second):
		t.Fatal("canceled broadcast blocked on a full fallback queue")
	}
}
