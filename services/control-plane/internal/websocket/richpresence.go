package websocket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/google/uuid"
)

var (
	// ErrRichPresenceDeliveryPlan identifies an invalid prepared delivery delta.
	ErrRichPresenceDeliveryPlan = errors.New("invalid rich-presence delivery plan")
	// ErrRichPresenceDeliveryMarshal identifies frame construction failures.
	ErrRichPresenceDeliveryMarshal = errors.New("rich-presence delivery marshal failed")
)

type richPresenceFrameMarshaler func(
	uuid.UUID,
	presence.Category,
	bool,
	json.RawMessage,
	int64,
) ([]byte, error)

// DeliverRichPresence synchronously enqueues an authorized Rich Presence
// delta to every connected device, clearing revoked viewers before updates.
func (h *Hub) DeliverRichPresence(ctx context.Context, plan presence.DeliveryPlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateRichPresenceDeliveryPlan(plan); err != nil {
		return err
	}

	var clearData []byte
	var err error
	if privacyCriticalRecipientCount(plan.ClearRecipients) > 0 {
		clearData, err = h.marshalRichPresenceDeliveryFrame(
			plan.SenderID,
			plan.Category,
			false,
			nil,
			0,
		)
		if err != nil {
			return err
		}
	}

	var updateData []byte
	if privacyCriticalRecipientCount(plan.UpdateRecipients) > 0 {
		updateData, err = h.marshalRichPresenceDeliveryFrame(
			plan.SenderID,
			plan.Category,
			plan.Minimized,
			plan.Payload,
			plan.UpdatedAt,
		)
		if err != nil {
			return err
		}
	}

	disconnected := make(map[*Client]bool)
	if len(clearData) > 0 {
		if err := h.deliverPrivacyCriticalToUsersWhere(
			ctx,
			plan.ClearRecipients,
			clearData,
			disconnected,
			activityRichPresenceClient,
		); err != nil {
			return err
		}
	}
	if len(updateData) > 0 {
		if err := h.deliverPrivacyCriticalToUsersWhere(
			ctx,
			plan.UpdateRecipients,
			updateData,
			disconnected,
			activityRichPresenceClient,
		); err != nil {
			return err
		}
	}
	return nil
}

func activityRichPresenceClient(client *Client) bool {
	return client != nil && client.activityRichPresenceCapable
}

// DisconnectRichPresenceClients closes every local device for the included
// users so reconnect can rebuild an authorized snapshot.
func (h *Hub) DisconnectRichPresenceClients(
	ctx context.Context,
	recipients map[uuid.UUID]bool,
) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var disconnectErr error
	for userID, included := range recipients {
		if !included {
			continue
		}
		for clientID := range h.userClients[userID] {
			client, connected := h.clients[clientID]
			if !connected {
				continue
			}
			if err := h.disconnectPrivacyCriticalClient(client); err != nil {
				disconnectErr = errors.Join(disconnectErr, err)
			}
		}
	}
	return errors.Join(disconnectErr, ctx.Err())
}

// DisconnectAllRichPresenceClients closes every local client when the affected
// audience cannot be determined safely.
func (h *Hub) DisconnectAllRichPresenceClients(ctx context.Context) error {
	return h.disconnectAllPrivacyCriticalClients(ctx)
}

func validateRichPresenceDeliveryPlan(plan presence.DeliveryPlan) error {
	if plan.SenderID == uuid.Nil {
		return fmt.Errorf("%w: sender is required", ErrRichPresenceDeliveryPlan)
	}
	switch plan.Category {
	case presence.CategoryServerVoice, presence.CategoryPrivateCall:
	default:
		return fmt.Errorf("%w: unsupported category", ErrRichPresenceDeliveryPlan)
	}
	if privacyCriticalRecipientCount(plan.UpdateRecipients) == 0 {
		return nil
	}
	payload := bytes.TrimSpace(plan.Payload)
	if plan.UpdatedAt <= 0 || plan.UpdatedAt > presence.MaxActivityUnixSeconds ||
		len(payload) < 2 || payload[0] != '{' ||
		payload[len(payload)-1] != '}' || !json.Valid(payload) {
		return fmt.Errorf("%w: update payload and timestamp are required", ErrRichPresenceDeliveryPlan)
	}
	return nil
}

func (h *Hub) marshalRichPresenceDeliveryFrame(
	senderID uuid.UUID,
	category presence.Category,
	minimized bool,
	payload json.RawMessage,
	updatedAt int64,
) ([]byte, error) {
	marshal := h.richPresenceFrameMarshaler
	if marshal == nil {
		marshal = marshalRichPresenceFrame
	}
	data, err := marshal(senderID, category, minimized, payload, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRichPresenceDeliveryMarshal, err)
	}
	return data, nil
}

func marshalRichPresenceFrame(
	senderID uuid.UUID,
	category presence.Category,
	minimized bool,
	payload json.RawMessage,
	updatedAt int64,
) ([]byte, error) {
	if payload == nil {
		return json.Marshal(OutgoingMessage{
			Type: "rich_presence_clear",
			Data: map[string]interface{}{
				keyUserID:  senderID.String(),
				"category": category,
			},
		})
	}
	return json.Marshal(OutgoingMessage{
		Type: "rich_presence_update",
		Data: map[string]interface{}{
			keyUserID:    senderID.String(),
			"category":   category,
			"minimized":  minimized,
			"payload":    payload,
			keyUpdatedAt: updatedAt,
		},
	})
}
