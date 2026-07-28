package websocket

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
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

// senderPresenceResolver answers the rich-presence policy's base-presence gate
// from the same Redis key the hub writes.
//
// It lives in this package rather than internal/presence for two reasons: the
// hub owns that key's lifecycle, and its failure logging must stay outside the
// activity core's AST log guards (internal/presence/activity_log_emissions_test.go).
type senderPresenceResolver struct {
	redis *redis.Client
	db    *sql.DB
	hub   *Hub
}

// NewSenderPresenceResolver builds the presence.SenderPresenceResolver consumed
// by both the activity service and the bootstrap snapshot service.
func NewSenderPresenceResolver(
	rdb *redis.Client,
	db *sql.DB,
	hub *Hub,
) presence.SenderPresenceResolver {
	return &senderPresenceResolver{redis: rdb, db: db, hub: hub}
}

// RichPresenceEmissionPermitted fails CLOSED on every uncertainty.
//
// It returns no error by contract: an error would reach failClosedGeneration and
// turn a transient Redis blip into a global Rich Presence disconnect (#2444).
//
// Note the deliberate asymmetry with resolveSnapshotVisibleStatus, which treats
// a missing key as online. That path answers "what status do I display"; this
// one answers "may this user broadcast their location", so a missing key is
// offline and MUST suppress.
func (r *senderPresenceResolver) RichPresenceEmissionPermitted(
	ctx context.Context, senderID uuid.UUID,
) bool {
	if r == nil || r.redis == nil || !r.hub.richPresenceEmissionPermitted(senderID) {
		return false
	}

	status, err := r.redis.Get(ctx, presence.StatusRedisKey(senderID)).Result()
	if errors.Is(err, redis.Nil) {
		return false // no persisted visible status is never allowed to emit
	}
	if err != nil {
		log.Printf(
			"[richpresence] presence lookup failed for user %s; suppressing activity: %v",
			sanitizeLogValue(senderID.String()), err,
		)
		return false
	}

	permitted := presence.EmissionPermittedForStatus(status)
	if !permitted && status != presence.StatusInvisible && status != presence.StatusOffline {
		// Invisible is the expected suppression; anything else reaching here is
		// a corrupt or unrecognised persisted value worth surfacing.
		log.Printf(
			"[richpresence] invalid persisted presence status for user %s: %q; suppressing activity",
			sanitizeLogValue(senderID.String()), sanitizeLogValue(status),
		)
	}
	if !permitted || r.db == nil {
		return false
	}

	var fenced bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM presence_offline_fences WHERE user_id = $1)`, senderID,
	).Scan(&fenced); err != nil {
		log.Printf(
			"[richpresence] offline fence lookup failed for user %s; suppressing activity: %v",
			sanitizeLogValue(senderID.String()), err,
		)
		return false
	}
	return !fenced
}
