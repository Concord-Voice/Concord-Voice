package dm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// captureDMVoiceCommandSpecsTx freezes exact sessions in this conversation.
// Membership removal captures before deletion; snapshots capture their complete
// post-write policy. Only those persistent decisions may retain a null marker.
func captureDMVoiceCommandSpecsTx(
	ctx context.Context, tx *sql.Tx, convID, targetID string, intent voicecommand.CommandSpec, allowMarker bool,
) ([]voicecommand.CommandSpec, error) {
	conversation, err := uuid.Parse(convID)
	if err != nil {
		return nil, fmt.Errorf("parse DM voice command conversation: %w", err)
	}
	user, err := uuid.Parse(targetID)
	if err != nil {
		return nil, fmt.Errorf("parse DM voice command target: %w", err)
	}
	channelID := conversation.String()
	targets, err := voicecommand.CaptureTargetsTx(ctx, tx, "dm", user.String(), channelID, "")
	if err != nil {
		return nil, fmt.Errorf("capture DM voice command targets: %w", err)
	}
	base := voicecommand.CommandSpec{
		Scope: "dm", Kind: intent.Kind, Action: intent.Action, ChannelID: &channelID, UserID: user.String(), Reason: intent.Reason,
	}
	specs := make([]voicecommand.CommandSpec, 0, len(targets))
	for _, registered := range targets {
		spec, target := base, registered.Target
		spec.Target = &target
		specs = append(specs, spec)
	}
	if len(specs) == 0 && allowMarker {
		specs = append(specs, base)
	}
	return specs, nil
}

// enqueueDMVoiceSnapshotTx must run after every domain write and validation,
// immediately before commit: its global counter lock is the final lock taken.
func enqueueDMVoiceSnapshotTx(
	ctx context.Context, tx *sql.Tx, convID, targetID, actorID, action string, snapshot VoiceEnforcementSnapshot,
) error {
	specs, err := captureDMVoiceCommandSpecsTx(ctx, tx, convID, targetID,
		voicecommand.CommandSpec{Kind: "server_moderation", Action: action}, true)
	if err != nil {
		return err
	}
	for index := range specs {
		specs[index].AuthorizationRevision = strconv.FormatInt(snapshot.AuthorizationRevision, 10)
		specs[index].ServerMuted = &snapshot.ServerMuted
		specs[index].ServerDeafened = &snapshot.ServerDeafened
	}
	if err := enqueueDMVoiceCommandsTx(ctx, tx, specs, actorID); err != nil {
		return fmt.Errorf("enqueue DM voice snapshot: %w", err)
	}
	return nil
}

func enqueueDMVoiceCommandsTx(ctx context.Context, tx *sql.Tx, specs []voicecommand.CommandSpec, actorID string) error {
	actor, err := uuid.Parse(actorID)
	if err != nil || actor == uuid.Nil {
		return errors.New("invalid voice command actor")
	}
	_, err = voicecommand.EnqueueTx(ctx, tx, specs, actor)
	return err
}

func respondDMVoiceCommandQueueFull(c *gin.Context, err error) bool {
	if !errors.Is(err, voicecommand.ErrQueueFull) {
		return false
	}
	c.Header("Retry-After", "30")
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": "Voice command queue is full; retry shortly", "error_code": "voice_command_queue_full",
	})
	return true
}
