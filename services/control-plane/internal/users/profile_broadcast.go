package users

import (
	"context"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/google/uuid"
)

var errProfileBroadcastUnavailable = errors.New("profile audience: hub unavailable")

// profileUpdateRecipients resolves one current relationship snapshot; privacy
// settings for discovery and presence do not grant a profile-update subscription.
func (h *Handler) profileUpdateRecipients(ctx context.Context, userID string) (recipients []uuid.UUID, err error) {
	rows, err := h.db.QueryContext(ctx, `
		WITH profile_recipients AS (
			SELECT $1::uuid AS user_id
			UNION
			SELECT CASE WHEN requester_id = $1 THEN addressee_id ELSE requester_id END
			FROM friendships
			WHERE (requester_id = $1 OR addressee_id = $1) AND status = 'accepted'
			UNION
			SELECT peer.user_id
			FROM server_members subject
			JOIN server_members peer ON peer.server_id = subject.server_id
			WHERE subject.user_id = $1
			UNION
			SELECT peer.user_id
			FROM dm_participants subject
			JOIN dm_participants peer ON peer.conversation_id = subject.conversation_id
			WHERE subject.user_id = $1
		)
		SELECT audience.user_id FROM profile_recipients audience
		WHERE NOT EXISTS (
			SELECT 1 FROM friendships blocked
			WHERE blocked.status = 'blocked'
			  AND ((blocked.requester_id = $1 AND blocked.addressee_id = audience.user_id)
			    OR (blocked.addressee_id = $1 AND blocked.requester_id = audience.user_id))
		)
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("profile audience: query recipients: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			recipients, err = nil, fmt.Errorf("profile audience: close recipients: %w", closeErr)
		}
	}()
	for rows.Next() {
		var recipient uuid.UUID
		if err := rows.Scan(&recipient); err != nil {
			return nil, fmt.Errorf("profile audience: scan recipient: %w", err)
		}
		recipients = append(recipients, recipient)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("profile audience: iterate recipients: %w", err)
	}
	return recipients, nil
}

func (h *Handler) broadcastProfileUpdate(ctx context.Context, user *models.User) error {
	recipients, err := h.profileUpdateRecipients(ctx, user.ID)
	if err != nil {
		return err
	}
	message := websocket.OutgoingMessage{
		Type: "profile_updated",
		Data: map[string]interface{}{
			"user_id":          user.ID,
			"username":         user.Username,
			"display_name":     user.DisplayName,
			"avatar_url":       user.AvatarURL,
			"header_image_url": user.HeaderImageURL,
			"color_scheme":     user.ColorScheme,
		},
	}
	for _, recipient := range recipients {
		if !h.hub.BroadcastToUserContext(ctx, recipient, message) {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("profile audience: enqueue recipient: %w", err)
			}
			return errProfileBroadcastUnavailable
		}
	}
	return nil
}
