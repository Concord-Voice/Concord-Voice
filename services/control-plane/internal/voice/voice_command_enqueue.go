package voice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// captureServerVoiceCommandSpecsTx freezes every registered session in this
// server, including sessions whose voice-presence event has not arrived yet.
// Authority and credential checks must already hold in the caller's transaction.
func captureServerVoiceCommandSpecsTx(
	ctx context.Context, tx *sql.Tx, serverID, targetID, kind, action, reason string,
) ([]voicecommand.CommandSpec, error) {
	server, err := uuid.Parse(serverID)
	if err != nil {
		return nil, fmt.Errorf("parse server voice command server: %w", err)
	}
	targetUser, err := uuid.Parse(targetID)
	if err != nil {
		return nil, fmt.Errorf("parse server voice command target: %w", err)
	}
	targets, err := voicecommand.CaptureTargetsTx(ctx, tx, "channel", targetUser.String(), "", server.String())
	if err != nil {
		return nil, fmt.Errorf("capture server voice command targets: %w", err)
	}
	specs := make([]voicecommand.CommandSpec, 0, len(targets))
	for _, registered := range targets {
		channelID, target := registered.ChannelID, registered.Target
		specs = append(specs, voicecommand.CommandSpec{
			Scope: "channel", Kind: kind, Action: action,
			ChannelID: &channelID, UserID: targetUser.String(), Target: &target, Reason: reason,
		})
	}
	return specs, nil
}

// enqueueServerVoiceSnapshotTx is the final domain transaction step: enqueue
// retains the global sequence lock until commit, so no later domain lock is safe.
func enqueueServerVoiceSnapshotTx(
	ctx context.Context, tx *sql.Tx, serverID, targetID, actorID, action string, snapshot serverEnforcementSnapshot,
) error {
	server, err := uuid.Parse(serverID)
	if err != nil {
		return fmt.Errorf("parse server voice snapshot server: %w", err)
	}
	user, err := uuid.Parse(targetID)
	if err != nil {
		return fmt.Errorf("parse server voice snapshot target: %w", err)
	}
	specs, err := captureServerVoiceCommandSpecsTx(ctx, tx, serverID, targetID, "server_moderation", action, "")
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		specs = append(specs, voicecommand.CommandSpec{
			Scope: "channel", Kind: "server_moderation", Action: action, UserID: user.String(),
		})
	} else {
		// Preserve established-before-pending channel order. Sessions registered
		// after discovery keep the capture helper's deterministic order at the end.
		priority := make(map[string]int, len(snapshot.channelIDs))
		for index, channelID := range snapshot.channelIDs {
			priority[channelID] = index
		}
		channelPriority := func(channelID string) int {
			if index, ok := priority[channelID]; ok {
				return index
			}
			return len(snapshot.channelIDs)
		}
		sort.SliceStable(specs, func(i, j int) bool {
			return channelPriority(*specs[i].ChannelID) < channelPriority(*specs[j].ChannelID)
		})
	}
	canonicalServerID := server.String()
	for index := range specs {
		specs[index].ServerID = &canonicalServerID
		specs[index].AuthorizationRevision = strconv.FormatInt(snapshot.authorizationID, 10)
		specs[index].ServerMuted = &snapshot.serverMuted
		specs[index].ServerDeafened = &snapshot.serverDeafened
	}
	if err := enqueueServerVoiceCommandsTx(ctx, tx, specs, actorID); err != nil {
		return fmt.Errorf("enqueue server voice snapshot: %w", err)
	}
	return nil
}

func enqueueServerVoiceCommandsTx(ctx context.Context, tx *sql.Tx, specs []voicecommand.CommandSpec, actorID string) error {
	actor, err := uuid.Parse(actorID)
	if err != nil || actor == uuid.Nil {
		return errors.New("invalid voice command actor")
	}
	_, err = voicecommand.EnqueueTx(ctx, tx, specs, actor)
	return err
}

func respondVoiceCommandQueueFull(c *gin.Context, err error) bool {
	if !errors.Is(err, voicecommand.ErrQueueFull) {
		return false
	}
	c.Header("Retry-After", "30")
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": "Voice command queue is full; retry shortly", "error_code": "voice_command_queue_full",
	})
	return true
}
