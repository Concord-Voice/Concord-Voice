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
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Rotation is a committed channel-key outcome ready for broadcast.
type Rotation struct {
	ChannelID         string
	ServerID          string
	DeletedChannelIDs []string
	RevokedEpoch      int
	SuccessorEpoch    int
	Reason            string
	RemovedUserID     string
}

// Broadcaster emits a committed channel-key outcome after its transaction has
// committed. The websocket package supplies the production adapter.
type Broadcaster func(Rotation)

// InitialDistributorChecker verifies that a fenced creator can still distribute
// a channel key within the revocation transaction.
type InitialDistributorChecker func(context.Context, *sql.Tx, string, string, string) (bool, error)

// Rotator owns the dependencies required to rotate a channel's CSK epoch and
// broadcast the rotation. It is constructed once per consuming handler and is
// safe to share (it holds no mutable state of its own).
type Rotator struct {
	db            *sql.DB
	log           *logger.Logger
	canDistribute InitialDistributorChecker
	broadcastFn   Broadcaster
}

// NewRotator builds a Rotator bound to a DB, logger, and committed-event broadcaster.
func NewRotator(db *sql.DB, log *logger.Logger, canDistribute InitialDistributorChecker, broadcastFn Broadcaster) *Rotator {
	return &Rotator{db: db, log: log, canDistribute: canDistribute, broadcastFn: broadcastFn}
}

// RecordKeyRevocationTx serializes an epoch transition with the channel and its
// initial-distribution marker. A mandatory rotation advances an active marker
// immediately so the old epoch cannot remain usable. If its sole distributor
// was removed or compromised, the incomplete channel is deleted fail-closed.
func RecordKeyRevocationTx(ctx context.Context, tx *sql.Tx, canDistribute InitialDistributorChecker, channelID, reason, actorID, removedUserID string) (*Rotation, error) {
	var serverID string
	if err := tx.QueryRowContext(ctx,
		`SELECT server_id FROM channels WHERE id = $1 FOR UPDATE`, channelID,
	).Scan(&serverID); err != nil {
		return nil, fmt.Errorf("lock channel for key revocation: %w", err)
	}
	rotation := Rotation{
		ChannelID:     channelID,
		ServerID:      serverID,
		Reason:        reason,
		RemovedUserID: removedUserID,
	}

	var creatorID sql.NullString
	var markerEpoch int
	err := tx.QueryRowContext(ctx,
		`SELECT creator_id, key_version
		 FROM channel_initial_key_distributions
		 WHERE channel_id = $1 FOR UPDATE`, channelID,
	).Scan(&creatorID, &markerEpoch)
	if err == nil {
		return recordInitialKeyRevocationTx(ctx, tx, canDistribute, rotation, actorID, creatorID, markerEpoch)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read initial key distribution: %w", err)
	}

	var maxEpoch int
	if err := tx.QueryRowContext(ctx,
		`SELECT GREATEST(
			COALESCE(MAX(key_version), 1),
			COALESCE((SELECT MAX(successor_epoch) FROM key_revocations WHERE channel_id = $1), 1)
		) FROM channel_keys WHERE channel_id = $1`, channelID,
	).Scan(&maxEpoch); err != nil {
		return nil, fmt.Errorf("read current key epoch: %w", err)
	}
	rotation.RevokedEpoch = maxEpoch
	rotation.SuccessorEpoch = maxEpoch + 1
	return insertKeyRevocationTx(ctx, tx, rotation, actorID)
}

func recordInitialKeyRevocationTx(ctx context.Context, tx *sql.Tx, canDistribute InitialDistributorChecker, rotation Rotation, actorID string, creatorID sql.NullString, markerEpoch int) (*Rotation, error) {
	creatorCanDistribute := creatorID.Valid && creatorID.String != rotation.RemovedUserID
	if creatorCanDistribute {
		var err error
		creatorCanDistribute, err = initialDistributorCanRotateTx(ctx, tx, canDistribute, rotation.ServerID, rotation.ChannelID, creatorID.String)
		if err != nil {
			return nil, err
		}
	}
	if !creatorCanDistribute {
		deletedChannelIDs, err := deleteIncompleteChannelTx(ctx, tx, rotation.ChannelID)
		if err != nil {
			return nil, err
		}
		return &Rotation{
			ChannelID:         rotation.ChannelID,
			ServerID:          rotation.ServerID,
			DeletedChannelIDs: deletedChannelIDs,
		}, nil
	}

	rotation.RevokedEpoch = markerEpoch
	rotation.SuccessorEpoch = markerEpoch + 1
	recorded, err := insertKeyRevocationTx(ctx, tx, rotation, actorID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE channel_initial_key_distributions
		 SET key_version = $2
		 WHERE channel_id = $1`, rotation.ChannelID, recorded.SuccessorEpoch,
	); err != nil {
		return nil, fmt.Errorf("advance initial key distribution: %w", err)
	}
	return recorded, nil
}

func initialDistributorCanRotateTx(ctx context.Context, tx *sql.Tx, canDistribute InitialDistributorChecker, serverID, channelID, creatorID string) (bool, error) {
	if canDistribute == nil {
		return false, errors.New("check initial key distributor: missing eligibility checker")
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT initial_key_distributor_check`); err != nil {
		return false, fmt.Errorf("save initial key distributor check: %w", err)
	}
	creatorCanDistribute, err := canDistribute(ctx, tx, serverID, channelID, creatorID)
	if err != nil {
		if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT initial_key_distributor_check`); rollbackErr != nil {
			return false, fmt.Errorf("recover initial key distributor check: %w", rollbackErr)
		}
		creatorCanDistribute = false
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT initial_key_distributor_check`); err != nil {
		return false, fmt.Errorf("release initial key distributor check: %w", err)
	}
	return creatorCanDistribute, nil
}

func deleteIncompleteChannelTx(ctx context.Context, tx *sql.Tx, channelID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM channels WHERE id = $1 OR linked_voice_channel_id = $1`, channelID,
	)
	if err != nil {
		return nil, fmt.Errorf("list incomplete channels to delete: %w", err)
	}
	deletedChannelIDs := make([]string, 0, 2)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan incomplete channel to delete: %w", errors.Join(err, rows.Close()))
		}
		deletedChannelIDs = append(deletedChannelIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list incomplete channels to delete: %w", errors.Join(err, rows.Close()))
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close incomplete channels to delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE id = $1`, channelID); err != nil {
		return nil, fmt.Errorf("fail closed incomplete channel: %w", err)
	}
	return deletedChannelIDs, nil
}

// CompleteInitialKeyDistributionTx deletes the completed marker. Mandatory
// rotations have already advanced its epoch and broadcast after their commit.
func CompleteInitialKeyDistributionTx(ctx context.Context, tx *sql.Tx, channelID string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channel_initial_key_distributions WHERE channel_id = $1`, channelID,
	); err != nil {
		return fmt.Errorf("finish initial key distribution: %w", err)
	}
	return nil
}

func insertKeyRevocationTx(ctx context.Context, tx *sql.Tx, rotation Rotation, actorID string) (*Rotation, error) {
	revokedBy := sql.NullString{String: actorID, Valid: actorID != ""}
	result, err := tx.ExecContext(ctx,
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_claimed
		 ) VALUES ($1, $2, $3, $4, $5, FALSE)
		 ON CONFLICT (channel_id, revoked_epoch) DO NOTHING`,
		rotation.ChannelID, rotation.RevokedEpoch, rotation.SuccessorEpoch, rotation.Reason, revokedBy,
	)
	if err != nil {
		return nil, fmt.Errorf("insert key revocation: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("check key revocation insert: %w", err)
	}
	if inserted == 0 {
		var successorEpoch int
		var existingReason string
		if err := tx.QueryRowContext(ctx,
			`SELECT successor_epoch, reason FROM key_revocations
			 WHERE channel_id = $1 AND revoked_epoch = $2`, rotation.ChannelID, rotation.RevokedEpoch,
		).Scan(&successorEpoch, &existingReason); err != nil {
			return nil, fmt.Errorf("read existing key revocation: %w", err)
		}
		rotation.SuccessorEpoch = successorEpoch
		rotation.Reason = existingReason
	}
	return &rotation, nil
}

// TriggerForChannel rotates the CSK epoch for ONE channel and broadcasts
// key_revocation to the remaining members. It resolves the channel's server and
// current epoch, then delegates to RevokeChannelKeyEpoch. The broadcast omits
// removed_user_id (that field is specific to the member-removal path). Reused by
// member-removal (the server-wide loop) and by temporary-SBAC access revocation
// (#487 P2). Safe no-op (logs + returns) when the channel does not resolve.
func (r *Rotator) TriggerForChannel(channelID, reason, actorID string) {
	r.revokeChannelKey(context.Background(), channelID, reason, actorID, "")
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
func (r *Rotator) RevokeChannelKeyEpoch(channelID, reason, actorID, removedUserID string) {
	r.revokeChannelKey(context.Background(), channelID, reason, actorID, removedUserID)
}

// StartManualRotation records and broadcasts a manual rotation after fencing
// the actor's credential epoch within the revocation transaction.
func (r *Rotator) StartManualRotation(
	ctx context.Context,
	channelID, actorID, tokenEpoch string,
	admit func(context.Context) error,
) (*Rotation, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin key revocation: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			r.log.Error("Failed to rollback key revocation", "error", rbErr)
		}
	}()
	if err := credepoch.GuardTx(ctx, tx, actorID, tokenEpoch); err != nil {
		return nil, fmt.Errorf("guard manual key rotation credential epoch: %w", err)
	}
	if admit != nil {
		if err := admit(ctx); err != nil {
			return nil, fmt.Errorf("admit manual key rotation: %w", err)
		}
	}
	rotation, err := RecordKeyRevocationTx(ctx, tx, r.canDistribute, channelID, "manual_rotation", actorID, "")
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit key revocation: %w", err)
	}
	if rotation != nil {
		r.broadcast(*rotation)
	}
	return rotation, nil
}

func (r *Rotator) revokeChannelKey(ctx context.Context, channelID, reason, actorID, removedUserID string) {
	rotation, err := r.recordRotation(ctx, channelID, reason, actorID, removedUserID)
	if err != nil {
		r.log.Error("Failed to record key revocation", "error", err)
		return
	}
	if rotation == nil {
		return
	}
	r.broadcast(*rotation)
}

func (r *Rotator) recordRotation(ctx context.Context, channelID, reason, actorID, removedUserID string) (*Rotation, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin key revocation: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			r.log.Error("Failed to rollback key revocation", "error", rbErr)
		}
	}()
	rotation, err := RecordKeyRevocationTx(ctx, tx, r.canDistribute, channelID, reason, actorID, removedUserID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit key revocation: %w", err)
	}
	return rotation, nil
}

func (r *Rotator) broadcast(rotation Rotation) {
	if r.broadcastFn != nil {
		r.broadcastFn(rotation)
	}
}
