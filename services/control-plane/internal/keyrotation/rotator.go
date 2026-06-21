// Package keyrotation provides the shared per-channel CSK (channel session key)
// rotation primitive used by both member-removal (server-wide loop) and
// temporary-SBAC access revocation (#487 P2). Extracting it here keeps the
// E2EE-integrity rotation SQL + broadcast in ONE place so the members and voice
// packages do not duplicate (and drift on) the forward-secrecy logic.
//
// A rotation inserts one key_revocations row (revoked_epoch=maxEpoch,
// successor_epoch=maxEpoch+1, the supplied reason) and broadcasts a
// key_revocation WebSocket event to the remaining server members so they rotate
// to a new epoch the departed user cannot decrypt.
package keyrotation

import (
	"database/sql"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// Rotator owns the dependencies required to rotate a channel's CSK epoch and
// broadcast the rotation. It is constructed once per consuming handler and is
// safe to share (it holds no mutable state of its own).
type Rotator struct {
	db  *sql.DB
	log *logger.Logger
	hub *websocket.Hub
}

// NewRotator builds a Rotator bound to a DB, logger, and WebSocket hub.
func NewRotator(db *sql.DB, log *logger.Logger, hub *websocket.Hub) *Rotator {
	return &Rotator{db: db, log: log, hub: hub}
}

// TriggerForChannel rotates the CSK epoch for ONE channel and broadcasts
// key_revocation to the remaining members. It resolves the channel's server and
// current epoch, then delegates to RevokeChannelKeyEpoch. The broadcast omits
// removed_user_id (that field is specific to the member-removal path). Reused by
// member-removal (the server-wide loop) and by temporary-SBAC access revocation
// (#487 P2). Safe no-op (logs + returns) when the channel does not resolve.
func (r *Rotator) TriggerForChannel(channelID, reason, actorID string) {
	var serverID string
	var maxEpoch int
	err := r.db.QueryRow(
		`SELECT c.server_id, COALESCE(MAX(ck.key_version), 1)
		 FROM channels c
		 LEFT JOIN channel_keys ck ON ck.channel_id = c.id
		 WHERE c.id = $1
		 GROUP BY c.server_id`,
		channelID,
	).Scan(&serverID, &maxEpoch)
	if err != nil {
		r.log.Error("Failed to resolve channel for key revocation", "error", err, "channel_id", channelID)
		return
	}

	serverUUID, _ := uuid.Parse(serverID)
	r.RevokeChannelKeyEpoch(serverID, serverUUID, channelID, maxEpoch, reason, actorID, "")
}

// RevokeChannelKeyEpoch is the shared core: it inserts one key_revocations row
// (revoked_epoch=maxEpoch, successor_epoch=maxEpoch+1, the supplied reason) and
// broadcasts key_revocation to the server. When removedUserID is non-empty it is
// included in the payload (preserving the member-removal contract); otherwise the
// field is omitted (the temp-SBAC / generic per-channel path).
//
// actorID is the human actor attributed to the revocation. Member-removal passes a
// real user UUID. Actorless system triggers (temp-SBAC presence/heartbeat/sweep
// cleanup, #487 P2) pass "" — in that case revoked_by is inserted as SQL NULL.
// key_revocations.revoked_by is nullable (REFERENCES users(id) ON DELETE SET NULL);
// inserting a non-empty-but-non-existent actor would violate the FK, so the empty
// actor MUST become NULL, never the literal string "".
func (r *Rotator) RevokeChannelKeyEpoch(serverID string, serverUUID uuid.UUID, channelID string, maxEpoch int, reason, actorID, removedUserID string) {
	// Insert key_revocations record (ignore conflict if already revoked). The
	// nullable revoked_by is NULL for actorless system triggers (actorID == "").
	revokedBy := sql.NullString{String: actorID, Valid: actorID != ""}
	_, err := r.db.Exec(
		`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (channel_id, revoked_epoch) DO NOTHING`,
		channelID, maxEpoch, maxEpoch+1, reason, revokedBy,
	)
	if err != nil {
		r.log.Error("Failed to insert key revocation", "error", err, "channel_id", channelID)
		return
	}

	// Broadcast key_revocation to remaining server members.
	data := map[string]interface{}{
		"channel_id":    channelID,
		"server_id":     serverID,
		"revoked_epoch": maxEpoch,
		"new_epoch":     maxEpoch + 1,
		"reason":        reason,
	}
	if removedUserID != "" {
		data["removed_user_id"] = removedUserID
	}
	r.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "key_revocation",
		Data: data,
	})
}
