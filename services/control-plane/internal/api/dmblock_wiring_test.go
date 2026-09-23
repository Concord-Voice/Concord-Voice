//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDMBlockReconciliationNotifierReusesDesktopDMEventContract(t *testing.T) {
	conversation, removed, remaining := uuid.New(), uuid.New(), uuid.New()
	hub := &recordingDMBlockReconciliationHub{}
	notifier := newDMBlockReconciliationNotifier(hub)

	require.NoError(t, notifier.ParticipantRemoved(context.Background(), conversation.String(), removed))
	require.Len(t, hub.userBroadcasts, 1)
	require.Equal(t, removed, hub.userBroadcasts[0].userID)
	require.Equal(t, websocket.OutgoingMessage{
		Type: "dm_participant_removed",
		Data: map[string]interface{}{
			"conversation_id": conversation.String(),
			"user_id":         removed.String(),
		},
	}, hub.userBroadcasts[0].message)
	require.Len(t, hub.participantBroadcasts, 1)
	require.Equal(t, conversation, hub.participantBroadcasts[0].conversationID)
	require.Equal(t, &removed, hub.participantBroadcasts[0].excludeUser)
	require.Equal(t, hub.userBroadcasts[0].message, hub.participantBroadcasts[0].message)

	require.NoError(t, notifier.GroupDeleted(context.Background(), conversation.String(), []uuid.UUID{removed, remaining}))
	require.Len(t, hub.userBroadcasts, 3)
	for _, got := range hub.userBroadcasts[1:] {
		require.Equal(t, "dm_group_deleted", got.message.Type)
		require.Equal(t, map[string]interface{}{"conversation_id": conversation.String()}, got.message.Data)
	}
	require.Empty(t, hub.participantBroadcasts[1:])

	require.NoError(t, notifier.RoleChanged(context.Background(), conversation.String(), remaining))
	require.NoError(t, notifier.KeyRevocation(context.Background(), conversation.String(), 1, "user_blocked"))
	require.Len(t, hub.participantBroadcasts, 3)
	require.Equal(t, websocket.OutgoingMessage{
		Type: "dm_role_changed",
		Data: map[string]interface{}{
			"conversation_id": conversation.String(),
			"user_id":         remaining.String(),
			"role":            "admin",
		},
	}, hub.participantBroadcasts[1].message)
	require.Equal(t, websocket.OutgoingMessage{
		Type: "key_revocation",
		Data: map[string]interface{}{
			"channel_id":    conversation.String(),
			"revoked_epoch": 1,
			"new_epoch":     2,
			"reason":        "user_blocked",
		},
	}, hub.participantBroadcasts[2].message)
}

func TestDMBlockReconciliationNotifierPropagatesContextAndDeliveryFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hub := &recordingDMBlockReconciliationHub{fail: true}
	notifier := newDMBlockReconciliationNotifier(hub)

	require.Error(t, notifier.GroupDeleted(ctx, uuid.NewString(), []uuid.UUID{uuid.New()}))
	require.Len(t, hub.contexts, 1)
	require.Same(t, ctx, hub.contexts[0])
}

func TestDMBlockReconciliationNotifierAttemptsSurvivorFanoutWhenDirectDeliveryBlocks(t *testing.T) {
	conversation, removed := uuid.New(), uuid.New()
	hub := &recordingDMBlockReconciliationHub{
		directCalls:        make(chan uuid.UUID, 1),
		participantCalls:   make(chan struct{}),
		blockDirectUser:    removed,
		directBlockRelease: make(chan struct{}),
		directBlockResult:  false,
	}
	notifier := newDMBlockReconciliationNotifier(hub)
	done := make(chan error, 1)
	go func() { done <- notifier.ParticipantRemoved(context.Background(), conversation.String(), removed) }()

	select {
	case <-hub.directCalls:
	case <-time.After(time.Second):
		require.FailNow(t, "direct delivery did not start")
	}
	select {
	case <-hub.participantCalls:
	case <-time.After(time.Second):
		close(hub.directBlockRelease)
		require.FailNow(t, "survivor fanout was starved by direct delivery")
	}
	close(hub.directBlockRelease)
	require.Error(t, <-done)
}

func TestDMBlockReconciliationNotifierAttemptsAllGroupDeletionRecipientsWhenOneBlocks(t *testing.T) {
	conversation, first, second := uuid.New(), uuid.New(), uuid.New()
	hub := &recordingDMBlockReconciliationHub{
		directCalls:        make(chan uuid.UUID, 2),
		blockDirectUser:    first,
		directBlockRelease: make(chan struct{}),
		directBlockResult:  false,
	}
	notifier := newDMBlockReconciliationNotifier(hub)
	done := make(chan error, 1)
	go func() {
		done <- notifier.GroupDeleted(context.Background(), conversation.String(), []uuid.UUID{first, second})
	}()

	got := make([]uuid.UUID, 0, 2)
	for range 2 {
		select {
		case userID := <-hub.directCalls:
			got = append(got, userID)
		case <-time.After(time.Second):
			close(hub.directBlockRelease)
			require.FailNow(t, "a group-deletion recipient was starved by the first")
		}
	}
	require.ElementsMatch(t, []uuid.UUID{first, second}, got)
	close(hub.directBlockRelease)
	require.Error(t, <-done)
}

type recordedDMBlockUserBroadcast struct {
	userID  uuid.UUID
	message websocket.OutgoingMessage
}

type recordedDMBlockParticipantBroadcast struct {
	conversationID uuid.UUID
	excludeUser    *uuid.UUID
	message        websocket.OutgoingMessage
}

type recordingDMBlockReconciliationHub struct {
	mu                    sync.Mutex
	userBroadcasts        []recordedDMBlockUserBroadcast
	participantBroadcasts []recordedDMBlockParticipantBroadcast
	contexts              []context.Context
	fail                  bool
	directCalls           chan uuid.UUID
	participantCalls      chan struct{}
	participantOnce       sync.Once
	blockDirectUser       uuid.UUID
	directBlockRelease    chan struct{}
	directBlockResult     bool
}

func (h *recordingDMBlockReconciliationHub) BroadcastToUserContext(ctx context.Context, userID uuid.UUID, message websocket.OutgoingMessage) bool {
	h.mu.Lock()
	h.contexts = append(h.contexts, ctx)
	h.userBroadcasts = append(h.userBroadcasts, recordedDMBlockUserBroadcast{userID: userID, message: message})
	directCalls := h.directCalls
	blockDirectUser := h.blockDirectUser
	directBlockRelease := h.directBlockRelease
	directBlockResult := h.directBlockResult
	fail := h.fail
	h.mu.Unlock()
	if directCalls != nil {
		directCalls <- userID
	}
	if userID == blockDirectUser && directBlockRelease != nil {
		select {
		case <-directBlockRelease:
			return directBlockResult
		case <-ctx.Done():
			return false
		}
	}
	return !fail
}

func (h *recordingDMBlockReconciliationHub) BroadcastToDMParticipantsExceptContext(ctx context.Context, conversationID uuid.UUID, excludeUser *uuid.UUID, message websocket.OutgoingMessage) bool {
	h.mu.Lock()
	h.contexts = append(h.contexts, ctx)
	h.participantBroadcasts = append(h.participantBroadcasts, recordedDMBlockParticipantBroadcast{
		conversationID: conversationID,
		excludeUser:    excludeUser,
		message:        message,
	})
	participantCalls := h.participantCalls
	fail := h.fail
	h.mu.Unlock()
	if participantCalls != nil {
		h.participantOnce.Do(func() { close(participantCalls) })
	}
	return !fail
}
