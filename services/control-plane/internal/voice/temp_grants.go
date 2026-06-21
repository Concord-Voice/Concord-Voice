package voice

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/keyrotation"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
)

// tempGrantAllow is the EXACT permission bitmask granted to a user moved into a
// voice channel they cannot otherwise see (#487 D1 course-correction — full voice
// participation). It is VIEW + CONNECT + SPEAK only: never SEND_MESSAGES, never any
// management bit. Changing this mask is a security-relevant decision (grant integrity,
// §6.3 of the spec) — do not widen it without review.
const tempGrantAllow = rbac.PermViewVoiceChannels | rbac.PermJoinVoice | rbac.PermSpeak

// tempGrantReason is the temporary_reason value stamped on a move-granted override.
const tempGrantReason = "move_granted"

// revokeReason is the key_revocations reason used when a temporary grant is cleaned up.
const revokeReason = "temp_access_revoked"

// tempGrantManager owns the grant/revoke convergence logic for temporary SBAC
// overrides (#487 Scope C). It is shared by the voice Handler (REST move grant +
// the moderator-revoke endpoint DELETE /servers/:id/voice/:userId/temp-access,
// RevokeTempAccess) and the NATSSubscriber (voice.left / heartbeat cleanup
// triggers) so the security-critical cleanup runs through ONE code path
// (revokeTemporaryChannelAccess), regardless of which trigger fires.
type tempGrantManager struct {
	db       *sql.DB
	log      *logger.Logger
	hub      *websocket.Hub
	resolver *rbac.Resolver
	rotator  *keyrotation.Rotator
	nats     *natsclient.Client
}

// newTempGrantManager constructs the manager. The rotator is built from the same
// db/log/hub so the temp-revoke CSK rotation shares the member-removal core.
func newTempGrantManager(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver, nats *natsclient.Client) *tempGrantManager {
	return &tempGrantManager{
		db:       db,
		log:      log,
		hub:      hub,
		resolver: resolver,
		rotator:  keyrotation.NewRotator(db, log, hub),
		nats:     nats,
	}
}

// grantTemporaryChannelAccess gives a moved user just-enough access to participate
// in a channel they cannot otherwise see (#487 D1). It inserts a user-specific
// channel_permission_overrides row with allow = VIEW|CONNECT|SPEAK, deny = 0,
// is_temporary = true.
//
// GRANT INTEGRITY (security-critical): it NEVER downgrades or mutates a permanent
// override. If a non-temporary row already exists for (channel, user) the function
// is a no-op — the user already has (or is explicitly governed by) a permanent
// grant and the temp layer must not touch it. The ON CONFLICT clause is guarded by
// a WHERE is_temporary predicate so a concurrent permanent row is never overwritten.
func (m *tempGrantManager) grantTemporaryChannelAccess(ctx context.Context, serverID, channelID, userID string) error {
	// Guard: skip if a permanent (non-temporary) override already covers this user+channel.
	var isTemp bool
	err := m.db.QueryRowContext(ctx,
		`SELECT is_temporary FROM channel_permission_overrides
		 WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`,
		channelID, userID,
	).Scan(&isTemp)
	switch {
	case err == sql.ErrNoRows:
		// no existing override → safe to insert a fresh temp grant
	case err != nil:
		return fmt.Errorf("temp grant lookup: %w", err)
	case !isTemp:
		// permanent grant present → never downgrade; nothing to do
		return nil
	}

	// Insert (or refresh an existing TEMP row). The ON CONFLICT ... WHERE is_temporary
	// predicate ensures a permanent row that races in between the SELECT and the INSERT
	// is never overwritten.
	if _, err := m.db.ExecContext(ctx,
		`INSERT INTO channel_permission_overrides
		   (channel_id, target_type, target_id, allow, deny, is_temporary, temporary_reason, granted_at)
		 VALUES ($1, 'user', $2, $3, 0, true, $4, NOW())
		 ON CONFLICT (channel_id, target_type, target_id)
		   DO UPDATE SET allow = EXCLUDED.allow, deny = 0, is_temporary = true,
		                 temporary_reason = $4, granted_at = NOW()
		   WHERE channel_permission_overrides.is_temporary`,
		channelID, userID, int64(tempGrantAllow), tempGrantReason,
	); err != nil {
		return fmt.Errorf("temp grant insert: %w", err)
	}

	if err := m.resolver.InvalidateChannel(ctx, serverID, channelID); err != nil {
		return fmt.Errorf("temp grant cache invalidate: %w", err)
	}
	return nil
}

// revokeTemporaryChannelAccess is the SINGLE convergence point for temp-grant
// cleanup (#487 P1). Every trigger (graceful leave, heartbeat stale-removal,
// moderator revoke, nightly sweep) calls this.
//
// SECURITY-CRITICAL (E2EE integrity, §6.2/§6.3): it deletes ONLY is_temporary=true
// user overrides. A permanent override for the same (channel, user) is untouched,
// and in that case the entire function is a NO-OP — no channel_keys/pending purge,
// no CSK rotation, no force-disconnect, no purge broadcast. The is_temporary guard
// on the DELETE is the integrity-critical line: it must never remove a permanent
// grant. When a real temp grant IS removed, the CSK is rotated so the departed user
// cannot decrypt post-visit traffic (exactly as member-removal does).
//
// actorID is the actor attributed to the key_revocations row ("system" for
// presence/sweep triggers, the moderator's user_id for an explicit revoke).
func (m *tempGrantManager) revokeTemporaryChannelAccess(ctx context.Context, serverID, channelID, userID, actorID string) error {
	res, err := m.db.ExecContext(ctx,
		`DELETE FROM channel_permission_overrides
		 WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND is_temporary = true`,
		channelID, userID)
	if err != nil {
		return fmt.Errorf("temp revoke delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// No temporary grant → permanent grant or none at all; do nothing.
		return nil
	}

	// Purge the departed user's wrapped channel key + any pending key request so they
	// cannot decrypt traffic after access is revoked.
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM channel_keys WHERE user_id = $1 AND channel_id = $2`, userID, channelID); err != nil {
		return fmt.Errorf("temp revoke channel_keys: %w", err)
	}
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM pending_key_requests WHERE user_id = $1 AND channel_id = $2`, userID, channelID); err != nil {
		return fmt.Errorf("temp revoke pending_key_requests: %w", err)
	}

	if err := m.resolver.InvalidateChannel(ctx, serverID, channelID); err != nil {
		m.log.Error("temp revoke: cache invalidate", "error", err, "channel_id", channelID, "server_id", serverID)
	}

	// P2: rotate the channel CSK so the departed user cannot decrypt post-visit traffic.
	m.rotator.TriggerForChannel(channelID, revokeReason, actorID)

	// P3: force-disconnect the live peer (revoking VIEW/CONNECT does not eject a
	// connected peer).
	m.publishForceDisconnect(channelID, userID)

	// P4: directed WS to the affected user so the client purges the channel from its
	// sidebar/state and invalidates its cached channel key.
	if userUUID, parseErr := uuid.Parse(userID); parseErr == nil {
		m.hub.BroadcastToUser(userUUID, websocket.OutgoingMessage{
			Type: "channel_access_revoked",
			Data: map[string]interface{}{
				"channel_id": channelID,
				"server_id":  serverID,
				"reason":     revokeReason,
			},
		})
	} else {
		m.log.Error("temp revoke: invalid user UUID for directed broadcast", "error", parseErr, "user_id", userID)
	}

	return nil
}

// publishForceDisconnect publishes a voice.enforce.disconnect command so the media
// plane closes that peer's transports and removes it from the room (#487 P3).
// Mirrors the voice.enforce.mute plumbing; the payload is {channelId, userId}.
func (m *tempGrantManager) publishForceDisconnect(channelID, userID string) {
	if m.nats == nil {
		return
	}
	if err := m.nats.Publish(natsSubjectEnforceDisconnect, map[string]interface{}{
		"channelId": channelID, "userId": userID,
	}); err != nil {
		m.log.Error("Failed to publish force-disconnect", "error", err,
			"subject", natsSubjectEnforceDisconnect, "channel_id", channelID, "user_id", userID)
	}
}

// hasTemporaryGrant reports whether the user holds a temporary override on the
// channel. Cleanup triggers (voice.left, moderator revoke) use it to skip the
// revoke convergence path for users with no temp grant (the common case).
func (m *tempGrantManager) hasTemporaryGrant(ctx context.Context, channelID, userID string) (bool, error) {
	var exists bool
	err := m.db.QueryRowContext(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM channel_permission_overrides
		   WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND is_temporary = true
		 )`, channelID, userID).Scan(&exists)
	return exists, err
}

// hasTemporaryGrantPastGrace is the heartbeat-path variant of hasTemporaryGrant:
// it additionally requires the grant be older than a 60s grace window. The
// heartbeat reconcile drives this so a brand-new grant whose voice.joined event
// has not yet landed in voice_participants is NOT revoked by a heartbeat racing
// the join (finding #7). Defense-in-depth only: the heartbeat's UserIDs is the
// socket-transport-level ground truth, so a member genuinely absent from it has
// truly disconnected — the grace narrowly protects the grant→join window, never
// suppresses a real miss past 60s. granted_at IS NULL (pre-#487 rows) is treated
// as past-grace so legacy grants stay sweepable by the reconcile path.
func (m *tempGrantManager) hasTemporaryGrantPastGrace(ctx context.Context, channelID, userID string) (bool, error) {
	var exists bool
	err := m.db.QueryRowContext(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM channel_permission_overrides
		   WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND is_temporary = true
		     AND (granted_at IS NULL OR granted_at < NOW() - INTERVAL '60 seconds')
		 )`, channelID, userID).Scan(&exists)
	return exists, err
}
