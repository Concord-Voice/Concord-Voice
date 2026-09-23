//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"context"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/google/uuid"
)

// dmBlockReconciliationBroadcaster is the post-commit subset of Hub used to
// converge desktop DM state after durable blocked-pair cleanup.
type dmBlockReconciliationBroadcaster interface {
	BroadcastToUserContext(context.Context, uuid.UUID, websocket.OutgoingMessage) bool
	BroadcastToDMParticipantsExceptContext(context.Context, uuid.UUID, *uuid.UUID, websocket.OutgoingMessage) bool
}

var _ dmBlockReconciliationBroadcaster = (*websocket.Hub)(nil)
var _ dmblock.ReconciliationNotifier = (*dmBlockReconciliationNotifier)(nil)

type dmBlockReconciliationNotifier struct {
	hub dmBlockReconciliationBroadcaster
}

func newDMBlockReconciliationNotifier(hub dmBlockReconciliationBroadcaster) *dmBlockReconciliationNotifier {
	return &dmBlockReconciliationNotifier{hub: hub}
}

// ParticipantRemoved reuses the ordinary group-member-removal contract: the
// removed user receives a direct event and current remaining participants are
// refreshed through the fenced participant rail.
func (n *dmBlockReconciliationNotifier) ParticipantRemoved(ctx context.Context, conversationID string, userID uuid.UUID) error {
	conversation, err := uuid.Parse(conversationID)
	if err != nil || n == nil || n.hub == nil {
		return fmt.Errorf("dm block participant removal notification unavailable")
	}
	message := websocket.OutgoingMessage{
		Type: "dm_participant_removed",
		Data: map[string]interface{}{
			"conversation_id": conversationID,
			"user_id":         userID.String(),
		},
	}
	directDelivered := make(chan bool, 1)
	participantsDelivered := make(chan bool, 1)
	go func() {
		directDelivered <- n.hub.BroadcastToUserContext(ctx, userID, message)
	}()
	go func() {
		participantsDelivered <- n.hub.BroadcastToDMParticipantsExceptContext(ctx, conversation, &userID, message)
	}()
	if directOK, participantsOK := <-directDelivered, <-participantsDelivered; !directOK || !participantsOK {
		return fmt.Errorf("dm block participant removal notification unavailable")
	}
	return nil
}

// GroupDeleted reuses the ordinary group-deletion contract. All recipients
// were participants in the committed deletion transaction, so direct delivery
// is the only possible post-delete audience.
func (n *dmBlockReconciliationNotifier) GroupDeleted(ctx context.Context, conversationID string, userIDs []uuid.UUID) error {
	if _, err := uuid.Parse(conversationID); err != nil || n == nil || n.hub == nil {
		return fmt.Errorf("dm block group deletion notification unavailable")
	}
	message := websocket.OutgoingMessage{
		Type: "dm_group_deleted",
		Data: map[string]interface{}{
			"conversation_id": conversationID,
		},
	}
	delivered := make(chan bool, len(userIDs))
	for _, userID := range userIDs {
		go func(userID uuid.UUID) {
			delivered <- n.hub.BroadcastToUserContext(ctx, userID, message)
		}(userID)
	}
	allDelivered := true
	for range userIDs {
		allDelivered = <-delivered && allDelivered
	}
	if !allDelivered {
		return fmt.Errorf("dm block group deletion notification unavailable")
	}
	return nil
}

// RoleChanged matches ordinary creator-transfer delivery to current
// participants after the topology transaction commits.
func (n *dmBlockReconciliationNotifier) RoleChanged(ctx context.Context, conversationID string, userID uuid.UUID) error {
	conversation, err := uuid.Parse(conversationID)
	if err != nil || n == nil || n.hub == nil {
		return fmt.Errorf("dm block role change notification unavailable")
	}
	if !n.hub.BroadcastToDMParticipantsExceptContext(ctx, conversation, nil, websocket.OutgoingMessage{
		Type: "dm_role_changed",
		Data: map[string]interface{}{
			"conversation_id": conversationID,
			"user_id":         userID.String(),
			"role":            "admin",
		},
	}) {
		return fmt.Errorf("dm block role change notification unavailable")
	}
	return nil
}

// KeyRevocation wakes the existing rotation coordinator after the topology
// transaction commits. It is deliberately a cue, not a ledger entry: the
// coordinator atomically records the revocation with successor wraps.
func (n *dmBlockReconciliationNotifier) KeyRevocation(ctx context.Context, conversationID string, revokedEpoch int, reason string) error {
	conversation, err := uuid.Parse(conversationID)
	if err != nil || n == nil || n.hub == nil || revokedEpoch <= 0 || reason == "" {
		return fmt.Errorf("dm block key revocation notification unavailable")
	}
	if !n.hub.BroadcastToDMParticipantsExceptContext(ctx, conversation, nil, websocket.OutgoingMessage{
		Type: "key_revocation",
		Data: map[string]interface{}{
			"channel_id":    conversationID,
			"revoked_epoch": revokedEpoch,
			"new_epoch":     revokedEpoch + 1,
			"reason":        reason,
		},
	}) {
		return fmt.Errorf("dm block key revocation notification unavailable")
	}
	return nil
}
