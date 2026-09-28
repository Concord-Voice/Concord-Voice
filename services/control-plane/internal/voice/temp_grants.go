package voice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
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

const tempGrantEffectTimeout = 10 * time.Second

func beginAudienceRevocationForTemporaryGrant(hub *websocket.Hub, temporary bool) func() {
	if temporary {
		return hub.BeginAudienceRevocation()
	}
	return func() {
		// No audience fence was opened for a permanent or absent grant.
	}
}

func detachedTempGrantContext(ctx context.Context) (context.Context, context.CancelFunc) {
	detached := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(time.Now().Add(tempGrantEffectTimeout)) {
		return context.WithDeadline(detached, deadline)
	}
	return context.WithTimeout(detached, tempGrantEffectTimeout)
}

// detachedTempGrantCompensationContext gives durable post-commit effects and
// fail-closed ambiguous-commit recovery a fresh bounded attempt; a completed
// request deadline must not suppress the authorization convergence work.
func detachedTempGrantCompensationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), tempGrantEffectTimeout)
}

// tempGrantManager owns the grant/revoke convergence logic for temporary SBAC
// overrides (#487 Scope C). It is shared by the voice Handler (REST move grant +
// the moderator-revoke endpoint DELETE /servers/:id/voice/:userId/temp-access,
// RevokeTempAccess) and the NATSSubscriber (voice.left / heartbeat cleanup
// triggers) so the security-critical cleanup runs through ONE code path
// (revokeTemporaryChannelAccess), regardless of which trigger fires.
type tempGrantManager struct {
	db              *sql.DB
	log             *logger.Logger
	hub             *websocket.Hub
	resolver        *rbac.Resolver
	rotator         *keyrotation.Rotator
	nats            *natsclient.Client
	presenceRecheck rbac.PresenceRecheck
	grantCommit     func(*sql.Tx) error
	grantRollback   func(*sql.Tx) error
}

// temporaryGrantAuthorityTx is supplied only by REST moderation paths. Cleanup
// paths have no moderator authority to revalidate.
type temporaryGrantAuthorityTx func(context.Context, *sql.Tx) error

type temporaryGrantAuthorization struct {
	actorID                 string
	credentialEpoch         string
	expectedSourceChannelID string
	guardCredential         bool
	authority               temporaryGrantAuthorityTx
}

var errTemporaryGrantSourceChanged = errors.New("temporary grant source participant changed")

// SetPresenceRecheck wires the #2445 Rich Presence capture. Nil leaves the
// revoke path at its pre-#2445 behavior.
func (m *tempGrantManager) SetPresenceRecheck(p rbac.PresenceRecheck) {
	m.presenceRecheck = p
}

// newTempGrantManager constructs the manager. The rotator is built from the same
// db/log/hub so the temp-revoke CSK rotation shares the member-removal core.
func newTempGrantManager(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver, nats *natsclient.Client) *tempGrantManager {
	return &tempGrantManager{
		db:       db,
		log:      log,
		hub:      hub,
		resolver: resolver,
		rotator:  keyrotation.NewContextRotator(db, log, resolver.CanDistributeChannelKeyTx, websocket.KeyRevocationContextBroadcaster(hub)),
		nats:     nats,
		grantCommit: func(tx *sql.Tx) error {
			return tx.Commit()
		},
		grantRollback: func(tx *sql.Tx) error {
			return tx.Rollback()
		},
	}
}

// grantTemporaryChannelAccess gives a moved user just-enough access to participate
// in a channel they cannot otherwise see (#487 D1). It inserts a user-specific
// channel_permission_overrides row with allow = VIEW|CONNECT|SPEAK, deny = 0,
// is_temporary = true. It shares the authority-write lock order with terminal
// voice cleanup: visibility -> lifecycle -> override -> commit -> cache invalidate.
//
// GRANT INTEGRITY (security-critical): it NEVER downgrades or mutates a permanent
// override. If a non-temporary row already exists for (channel, user) the function
// is a no-op — the user already has (or is explicitly governed by) a permanent
// grant and the temp layer must not touch it. The guarded ON CONFLICT is the
// permanent-override check under those locks; an unlocked preflight would reopen
// the terminal-cleanup race this transaction closes.
func (m *tempGrantManager) grantTemporaryChannelAccess(
	ctx context.Context, serverID, channelID, userID string,
) error {
	_, err := m.grantTemporaryChannelAccessWithCredential(ctx, serverID, channelID, userID, temporaryGrantAuthorization{})
	return err
}

// grantTemporaryChannelAccessWithCredential commits the credential- and
// authority-fenced grant before its caller sends a move signal. The boolean is
// false only when a permanent override made the locked upsert a no-op.
func (m *tempGrantManager) grantTemporaryChannelAccessWithCredential(
	ctx context.Context, serverID, channelID, userID string, authorization temporaryGrantAuthorization,
) (granted bool, returnErr error) {
	parsedUserID, err := uuid.Parse(userID)
	if err != nil || parsedUserID == uuid.Nil {
		return false, errors.New("invalid temporary grant user")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("temp grant begin: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("temp grant rollback: %w", rollbackErr))
		}
	}()
	if err := rbac.LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		return false, fmt.Errorf("temp grant visibility lock: %w", err)
	}
	if err := guardTemporaryGrantAuthorization(ctx, tx, authorization); err != nil {
		return false, err
	}
	if err := LockServerVoiceLifecycleTx(ctx, tx, parsedUserID); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("temp grant lifecycle lock: %w", ctxErr)
		}
		return false, fmt.Errorf("temp grant lifecycle lock: %w", err)
	}
	if err := verifyTemporaryGrantSource(ctx, tx, authorization.expectedSourceChannelID, userID, serverID); err != nil {
		return false, err
	}

	// Insert (or refresh an existing TEMP row). The ON CONFLICT ... WHERE is_temporary
	// predicate makes a permanent row a committed no-op and never overwrites it.
	var grantXmin string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO channel_permission_overrides
		   (channel_id, target_type, target_id, allow, deny, is_temporary, temporary_reason, granted_at)
		 VALUES ($1, 'user', $2, $3, 0, true, $4, clock_timestamp())
		 ON CONFLICT (channel_id, target_type, target_id)
		   DO UPDATE SET allow = EXCLUDED.allow, deny = 0, is_temporary = true,
		                 temporary_reason = $4, granted_at = clock_timestamp()
		   WHERE channel_permission_overrides.is_temporary
		 RETURNING xmin::text`,
		channelID, userID, int64(tempGrantAllow), tempGrantReason,
	).Scan(&grantXmin)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("temp grant permanent no-op commit: %w", err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("temp grant insert: %w", err)
	}
	if err := m.grantCommit(tx); err != nil {
		// A failed COMMIT acknowledgement is ambiguous, but a locally rejected
		// commit can still hold the visibility/lifecycle locks. Release that
		// transaction before reconciliation so the compensator never waits on
		// our own locks. A committed transaction reports ErrTxDone here.
		if rollbackErr := m.grantRollback(tx); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			safeCtx, cancel := detachedTempGrantCompensationContext(ctx)
			m.revalidateTemporaryGrantAuthority(safeCtx, serverID, channelID, userID, nil)
			cancel()
			return false, errors.Join(fmt.Errorf("temp grant commit: %w", err), fmt.Errorf("temp grant rollback before reconcile: %w", rollbackErr))
		}
		reconcileErr := m.reconcileAmbiguousTemporaryGrant(ctx, serverID, channelID, userID, parsedUserID, grantXmin)
		return false, errors.Join(fmt.Errorf("temp grant commit: %w", err), reconcileErr)
	}
	m.revalidateGrantedTemporaryAuthority(ctx, serverID, channelID)
	return true, nil
}

func guardTemporaryGrantAuthorization(
	ctx context.Context, tx *sql.Tx, authorization temporaryGrantAuthorization,
) error {
	if !authorization.guardCredential {
		return nil
	}
	if err := credepoch.GuardTx(ctx, tx, authorization.actorID, authorization.credentialEpoch); err != nil {
		return fmt.Errorf("temp grant credential guard: %w", err)
	}
	if authorization.authority != nil {
		if err := authorization.authority(ctx, tx); err != nil {
			return fmt.Errorf("temp grant authority guard: %w", err)
		}
	}
	return nil
}

func verifyTemporaryGrantSource(
	ctx context.Context, tx *sql.Tx, expectedSourceChannelID, userID, serverID string,
) error {
	if expectedSourceChannelID == "" {
		return nil
	}
	var sourceStillCurrent bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM voice_participants vp
			JOIN channels source ON source.id = vp.channel_id
			WHERE vp.channel_id = $1 AND vp.user_id = $2 AND source.server_id = $3
		)`, expectedSourceChannelID, userID, serverID,
	).Scan(&sourceStillCurrent); err != nil {
		return fmt.Errorf("temp grant source participant check: %w", err)
	}
	if !sourceStillCurrent {
		return errTemporaryGrantSourceChanged
	}
	return nil
}

// reconcileAmbiguousTemporaryGrant closes the only unsafe outcome of a failed
// grant COMMIT acknowledgement: PostgreSQL may have committed the grant even
// though the caller will not signal the move. The row-version fence (xmin)
// returned by the locked upsert is an operation fence. Cleanup may touch only
// that exact temporary move grant; a later refresh or permanent override writes
// a new tuple/xmin and remains authoritative. granted_at is audit/grace data,
// not an operation fence.
func (m *tempGrantManager) reconcileAmbiguousTemporaryGrant(
	ctx context.Context,
	serverID, channelID, userID string,
	userUUID uuid.UUID,
	grantXmin string,
) (returnErr error) {
	safeCtx, cancel := detachedTempGrantCompensationContext(ctx)
	defer cancel()

	plan, err := m.prepareTemporaryGrantCapture(safeCtx, serverID, channelID, userID)
	if err != nil {
		m.revalidateTemporaryGrantAuthority(safeCtx, serverID, channelID, userID, nil)
		return fmt.Errorf("reconcile ambiguous temp grant prepare capture: %w", err)
	}
	tx, err := m.db.BeginTx(safeCtx, nil)
	if err != nil {
		m.revalidateTemporaryGrantAuthority(safeCtx, serverID, channelID, userID, plan)
		return fmt.Errorf("reconcile ambiguous temp grant begin: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("reconcile ambiguous temp grant rollback: %w", rollbackErr))
		}
	}()
	if err := rbac.LockServerVisibilityCapture(safeCtx, tx, serverID); err != nil {
		m.revalidateTemporaryGrantAuthority(safeCtx, serverID, channelID, userID, plan)
		return fmt.Errorf("reconcile ambiguous temp grant visibility lock: %w", err)
	}
	if err := LockServerVoiceLifecycleTx(safeCtx, tx, userUUID); err != nil {
		m.revalidateTemporaryGrantAuthority(safeCtx, serverID, channelID, userID, plan)
		return fmt.Errorf("reconcile ambiguous temp grant lifecycle lock: %w", err)
	}
	if err := m.captureTemporaryGrantVisibility(safeCtx, tx, plan); err != nil {
		m.revalidateTemporaryGrantAuthority(safeCtx, serverID, channelID, userID, plan)
		return err
	}

	rotation, removed, err := m.deleteTemporaryGrantFenceAlreadyLocked(
		safeCtx, tx, channelID, userID, grantXmin,
	)
	if err != nil {
		m.revalidateTemporaryGrantAuthority(safeCtx, serverID, channelID, userID, plan)
		return err
	}
	if err := tx.Commit(); err != nil {
		if removed {
			m.completeAmbiguousTemporaryGrantRevocation(safeCtx, serverID, channelID, userID, plan)
		} else {
			m.revalidateAmbiguousTemporaryGrantSuccessor(safeCtx, serverID, channelID, plan)
		}
		return fmt.Errorf("reconcile ambiguous temp grant commit: %w", err)
	}
	if !removed {
		m.revalidateAmbiguousTemporaryGrantSuccessor(safeCtx, serverID, channelID, plan)
		return nil
	}
	m.completeTemporaryGrantRevocation(safeCtx, serverID, channelID, userID, plan, rotation)
	return nil
}

// revalidateAmbiguousTemporaryGrantSuccessor abandons a capture derived from an
// old xmin without evicting the holder. An exact-fence miss proves that this
// operation cannot revoke the current row: it may be a newer retry or a
// permanent replacement, both of which remain authoritative.
func (m *tempGrantManager) revalidateAmbiguousTemporaryGrantSuccessor(
	ctx context.Context, serverID, channelID string, plan rbac.PresenceRecheckPlan,
) {
	m.presenceAbandon(plan, "ambiguous_commit")
	m.revalidateGrantedTemporaryAuthority(ctx, serverID, channelID)
}

// revalidateGrantedTemporaryAuthority drops stale reads after a confirmed grant.
// The grant is durable, so cache delivery failure is logged but must not turn a
// successful move into an unsignaled error response.
func (m *tempGrantManager) revalidateGrantedTemporaryAuthority(ctx context.Context, serverID, channelID string) {
	safeCtx, cancel := detachedTempGrantCompensationContext(ctx)
	defer cancel()
	if err := m.resolver.InvalidateChannel(safeCtx, serverID, channelID); err != nil {
		m.log.Error("temp grant cache invalidate", "error", err, "channel_id", channelID, "server_id", serverID)
	}
	if m.hub == nil {
		return
	}
	serverUUID, serverErr := uuid.Parse(serverID)
	channelUUID, channelErr := uuid.Parse(channelID)
	if serverErr == nil && channelErr == nil {
		m.hub.RevalidateChannelSubscriptions(serverUUID, channelUUID)
	}
}

// revalidateTemporaryGrantAuthority discards stale reads and disconnects the
// target when ambiguous-grant compensation cannot establish a durable outcome.
func (m *tempGrantManager) revalidateTemporaryGrantAuthority(ctx context.Context, serverID, channelID, userID string, plan rbac.PresenceRecheckPlan) {
	m.presenceAbandon(plan, "ambiguous_commit")
	if err := m.resolver.InvalidateChannel(ctx, serverID, channelID); err != nil {
		m.log.Error("temp grant ambiguous cache invalidate", "error", err, "channel_id", channelID, "server_id", serverID)
	}
	m.publishForceDisconnect(channelID, userID)
	if m.hub == nil {
		return
	}
	serverUUID, serverErr := uuid.Parse(serverID)
	channelUUID, channelErr := uuid.Parse(channelID)
	if serverErr == nil && channelErr == nil {
		m.hub.RevalidateChannelSubscriptions(serverUUID, channelUUID)
	}
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
func (m *tempGrantManager) revokeTemporaryChannelAccess(ctx context.Context, serverID, channelID, userID, actorID string) (bool, error) {
	return m.revokeTemporaryChannelAccessWithCredential(ctx, serverID, channelID, userID, temporaryGrantAuthorization{actorID: actorID})
}

func (m *tempGrantManager) revokeTemporaryChannelAccessWithCredential(ctx context.Context, serverID, channelID, userID string, authorization temporaryGrantAuthorization) (bool, error) {
	plan, rotation, removed, err := m.deleteTemporaryGrantWithCapture(ctx, serverID, channelID, userID, authorization)
	if err != nil {
		return false, err
	}
	if !removed {
		// No temporary grant → permanent grant or none at all; do nothing.
		// Total no-op: no capture is dispatched, no viewer is disconnected.
		return false, nil
	}
	m.completeTemporaryGrantRevocation(ctx, serverID, channelID, userID, plan, rotation)
	return true, nil
}

func (m *tempGrantManager) prepareOrphanTemporaryGrantCapture(
	ctx context.Context,
	serverID, channelID, userID string,
) (bool, rbac.PresenceRecheckPlan, error) {
	preflightTemporary, err := m.hasTemporaryGrant(ctx, channelID, userID)
	if err != nil {
		return false, nil, fmt.Errorf("preflight orphan temporary grant: %w", err)
	}
	if !preflightTemporary {
		return false, nil, nil
	}
	plan, err := m.prepareTemporaryGrantCapture(ctx, serverID, channelID, userID)
	if err != nil {
		return false, nil, err
	}
	return true, plan, nil
}

// revokeOrphanedTemporaryChannelAccess closes the sweep selection-to-revoke
// window. It rechecks both the missing participant and past-grace predicates
// under the visibility and exact lifecycle locks before taking the channel lock
// through deleteTemporaryGrantAlreadyLocked.
func (m *tempGrantManager) revokeOrphanedTemporaryChannelAccess(
	ctx context.Context,
	serverID, channelID, userID string,
) (removed bool, returnErr error) {
	parsedUserID, err := uuid.Parse(userID)
	if err != nil || parsedUserID == uuid.Nil {
		return false, errors.New("invalid orphan temporary grant user")
	}
	preflightTemporary, plan, err := m.prepareOrphanTemporaryGrantCapture(ctx, serverID, channelID, userID)
	if err != nil {
		return false, err
	}
	closeAudienceFence := beginAudienceRevocationForTemporaryGrant(m.hub, preflightTemporary)
	defer closeAudienceFence()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin orphan temporary grant cleanup: %w", err)
	}
	defer func() {
		returnErr = joinRollbackErr(returnErr, tx.Rollback(), "rollback orphan temporary grant cleanup")
	}()
	if err := rbac.LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		return false, fmt.Errorf("lock orphan temporary grant visibility: %w", err)
	}
	if err := LockServerVoiceLifecycleTx(ctx, tx, parsedUserID); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("lock orphan temporary grant lifecycle: %w", ctxErr)
		}
		return false, fmt.Errorf("lock orphan temporary grant lifecycle: %w", err)
	}
	eligible, err := orphanTemporaryGrantEligibleTx(ctx, tx, channelID, userID)
	if err != nil {
		return false, err
	}
	if !eligible {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit orphan temporary grant no-op: %w", err)
		}
		return false, nil
	}
	if !preflightTemporary {
		return false, errors.New("orphan temporary grant changed before visibility capture")
	}
	if err := m.captureTemporaryGrantVisibility(ctx, tx, plan); err != nil {
		return false, err
	}
	rotation, removed, err := m.deleteTemporaryGrantAlreadyLocked(ctx, tx, channelID, userID, "")
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		if removed {
			m.completeAmbiguousTemporaryGrantRevocation(ctx, serverID, channelID, userID, plan)
		}
		return false, fmt.Errorf("commit orphan temporary grant cleanup: %w", err)
	}
	closeAudienceFence()
	if removed {
		m.completeTemporaryGrantRevocation(ctx, serverID, channelID, userID, plan, rotation)
	}
	return removed, nil
}

func orphanTemporaryGrantEligibleTx(ctx context.Context, tx *sql.Tx, channelID, userID string) (bool, error) {
	var pastGrace bool
	err := tx.QueryRowContext(ctx, `
		SELECT granted_at IS NULL OR granted_at < clock_timestamp() - INTERVAL '60 seconds'
		FROM channel_permission_overrides
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2
		  AND is_temporary = true AND temporary_reason = $3
		FOR UPDATE
	`, channelID, userID, tempGrantReason).Scan(&pastGrace)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock orphan temporary grant: %w", err)
	}
	if !pastGrace {
		return false, nil
	}
	var participantExists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM voice_participants WHERE channel_id = $1 AND user_id = $2)`,
		channelID, userID,
	).Scan(&participantExists); err != nil {
		return false, fmt.Errorf("recheck orphan temporary grant participant: %w", err)
	}
	return !participantExists, nil
}

// completeAmbiguousTemporaryGrantRevocation runs only idempotent fail-closed
// effects. A commit error has an unknown durable outcome, so it must never announce the
// locally computed rotation. A later sweep converges durable state either way.
func (m *tempGrantManager) completeAmbiguousTemporaryGrantRevocation(
	ctx context.Context, serverID, channelID, userID string, plan rbac.PresenceRecheckPlan,
) {
	m.presenceAbandon(plan, "ambiguous_commit")
	safeCtx, cancel := detachedTempGrantCompensationContext(ctx)
	defer cancel()
	if err := m.resolver.InvalidateChannel(safeCtx, serverID, channelID); err != nil {
		m.log.Error("temp revoke ambiguous cache invalidate", "error", err, "channel_id", channelID, "server_id", serverID)
	}
	if m.hub != nil {
		serverUUID, serverErr := uuid.Parse(serverID)
		channelUUID, channelErr := uuid.Parse(channelID)
		if serverErr == nil && channelErr == nil {
			m.hub.RevalidateChannelSubscriptions(serverUUID, channelUUID)
		}
	}
	m.publishForceDisconnect(channelID, userID)
}

// completeTemporaryGrantRevocation performs only post-commit effects for a
// temporary-grant deletion. The durable override/key changes are already
// committed by deleteTemporaryGrantAlreadyLocked before this runs.
func (m *tempGrantManager) completeTemporaryGrantRevocation(
	ctx context.Context,
	serverID, channelID, userID string,
	plan rbac.PresenceRecheckPlan,
	rotation *keyrotation.Rotation,
) {
	m.presenceExecute(plan)
	safeCtx, cancel := detachedTempGrantContext(ctx)
	defer cancel()

	if err := m.resolver.InvalidateChannel(safeCtx, serverID, channelID); err != nil {
		m.log.Error("temp revoke: cache invalidate", "error", err, "channel_id", channelID, "server_id", serverID)
	}
	if m.hub != nil {
		if serverUUID, serverErr := uuid.Parse(serverID); serverErr == nil {
			if channelUUID, channelErr := uuid.Parse(channelID); channelErr == nil {
				m.hub.RevalidateChannelSubscriptions(serverUUID, channelUUID)
			}
		}
	}

	// P2: the committed CSK rotation is best-effort to deliver; the durable
	// revocation record and key-material purge already committed atomically.
	if rotation != nil {
		if err := m.rotator.BroadcastContext(safeCtx, *rotation); err != nil {
			m.log.Error("temp revoke key revocation delivery failed", "failure_class", "key_revocation_delivery")
		}
	}

	// P3: force-disconnect the live peer (revoking VIEW/CONNECT does not eject a
	// connected peer).
	m.publishForceDisconnect(channelID, userID)

	// P4: directed WS to the affected user so the client purges the channel from its
	// sidebar/state and invalidates its cached channel key.
	userUUID, parseErr := uuid.Parse(userID)
	if parseErr != nil {
		m.log.Error("temp revoke: invalid user UUID for directed broadcast", "error", parseErr, "user_id", userID)
	} else if m.hub != nil {
		if !m.hub.BroadcastToUserContext(safeCtx, userUUID, websocket.OutgoingMessage{
			Type: "channel_access_revoked",
			Data: map[string]interface{}{
				"channel_id": channelID,
				"server_id":  serverID,
				"reason":     revokeReason,
			},
		}) {
			m.log.Error("temp revoke directed delivery failed", "failure_class", "channel_access_revoked_delivery")
		}
	}

}

// deleteTemporaryGrantWithCapture runs the is_temporary DELETE atomically with
// the pre-mutation Server Voice visibility capture, under the per-server
// advisory lock, so the captured set is the exact pre-write authorized audience
// (#2445). Phase 1 runs before BeginTx; phase 2 runs under the lock.
func (m *tempGrantManager) deleteTemporaryGrantWithCapture(
	ctx context.Context,
	serverID, channelID, userID string, authorization temporaryGrantAuthorization,
) (plan rbac.PresenceRecheckPlan, rotation *keyrotation.Rotation, removed bool, returnErr error) {
	parsedUserID, err := uuid.Parse(userID)
	if err != nil || parsedUserID == uuid.Nil {
		return nil, nil, false, errors.New("invalid temporary grant user")
	}
	plan, err = m.prepareTemporaryGrantCapture(ctx, serverID, channelID, userID)
	if err != nil {
		return nil, nil, false, err
	}

	defer m.hub.BeginAudienceRevocation()()

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("temp revoke begin: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("temp revoke rollback: %w", rollbackErr))
		}
	}()

	if err := rbac.LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		return nil, nil, false, fmt.Errorf("temp revoke lock: %w", err)
	}
	if authorization.guardCredential {
		if err := credepoch.GuardTx(ctx, tx, authorization.actorID, authorization.credentialEpoch); err != nil {
			return nil, nil, false, fmt.Errorf("temp revoke credential guard: %w", err)
		}
		if authorization.authority != nil {
			if err := authorization.authority(ctx, tx); err != nil {
				return nil, nil, false, fmt.Errorf("temp revoke authority guard: %w", err)
			}
		}
	}
	if err := LockServerVoiceLifecycleTx(ctx, tx, parsedUserID); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, false, fmt.Errorf("temp revoke lifecycle lock: %w", ctxErr)
		}
		return nil, nil, false, fmt.Errorf("temp revoke lifecycle lock: %w", err)
	}
	if err := m.captureTemporaryGrantVisibility(ctx, tx, plan); err != nil {
		return nil, nil, false, err
	}
	rotation, removed, err = m.deleteTemporaryGrantAlreadyLocked(
		ctx, tx, channelID, userID, authorization.actorID,
	)
	if err != nil {
		return nil, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		if removed {
			m.completeAmbiguousTemporaryGrantRevocation(ctx, serverID, channelID, userID, plan)
		}
		return nil, nil, false, fmt.Errorf("temp revoke commit: %w", err)
	}
	return plan, rotation, removed, nil
}

// prepareTemporaryGrantCapture builds the pre-mutation visibility plan outside
// a transaction. Both the standalone moderator/sweeper wrapper and terminal
// voice cleanup use this exact phase-one preparation.
func (m *tempGrantManager) prepareTemporaryGrantCapture(
	ctx context.Context,
	serverID, channelID, userID string,
) (rbac.PresenceRecheckPlan, error) {
	if m.presenceRecheck == nil {
		return nil, nil
	}
	plan, err := m.presenceRecheck.PrepareCapture(ctx, serverID, []string{channelID}, &userID)
	if err != nil {
		return nil, fmt.Errorf("temp revoke prepare capture: %w", err)
	}
	return plan, nil
}

// captureTemporaryGrantVisibility is phase two of the presence capture and
// must run after the visibility advisory lock but before a temporary override
// changes.
func (m *tempGrantManager) captureTemporaryGrantVisibility(
	ctx context.Context,
	tx *sql.Tx,
	plan rbac.PresenceRecheckPlan,
) error {
	if m.presenceRecheck == nil || plan == nil {
		return nil
	}
	if err := m.presenceRecheck.CaptureVisibility(ctx, tx, plan); err != nil {
		return fmt.Errorf("temp revoke capture: %w", err)
	}
	return nil
}

// deleteTemporaryGrantAlreadyLocked performs the durable half of every
// temporary-grant revocation. Callers must already hold the server visibility
// lock and, for terminal voice cleanup, the exact user lifecycle lock. The
// keyrotation record takes the channel lock last, preserving the global order:
// visibility -> lifecycle -> channel.
func (m *tempGrantManager) deleteTemporaryGrantAlreadyLocked(
	ctx context.Context,
	tx *sql.Tx,
	channelID, userID, actorID string,
) (*keyrotation.Rotation, bool, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM channel_permission_overrides
		 WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2
		   AND is_temporary = true AND temporary_reason = $3`,
		channelID, userID, tempGrantReason)
	if err != nil {
		return nil, false, fmt.Errorf("temp revoke delete: %w", err)
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("temp revoke rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return nil, false, nil
	}
	rotation, err := m.rotator.RecordKeyRevocationAndPurgeUserTx(
		ctx, tx, channelID, revokeReason, actorID, userID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("temp revoke record key revocation and purge: %w", err)
	}
	return rotation, true, nil
}

// deleteTemporaryGrantFenceAlreadyLocked is the ambiguous-grant recovery
// variant. It preserves the normal cleanup's durable atomicity while making a
// later refresh or permanent override a strict no-op.
func (m *tempGrantManager) deleteTemporaryGrantFenceAlreadyLocked(
	ctx context.Context,
	tx *sql.Tx,
	channelID, userID string,
	grantXmin string,
) (*keyrotation.Rotation, bool, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM channel_permission_overrides
		 WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2
		   AND is_temporary = true AND temporary_reason = $3 AND xmin::text = $4`,
		channelID, userID, tempGrantReason, grantXmin,
	)
	if err != nil {
		return nil, false, fmt.Errorf("temp grant fenced revoke delete: %w", err)
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("temp grant fenced revoke rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return nil, false, nil
	}
	rotation, err := m.rotator.RecordKeyRevocationAndPurgeUserTx(
		ctx, tx, channelID, revokeReason, "", userID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("temp grant fenced revoke record key revocation and purge: %w", err)
	}
	return rotation, true, nil
}

func (m *tempGrantManager) presenceExecute(plan rbac.PresenceRecheckPlan) {
	if m.presenceRecheck == nil || plan == nil || !plan.HasWork() {
		return
	}
	m.presenceRecheck.Execute(plan)
}

func (m *tempGrantManager) presenceAbandon(plan rbac.PresenceRecheckPlan, cause string) {
	if m.presenceRecheck == nil || plan == nil || !plan.HasWork() {
		return
	}
	m.presenceRecheck.Abandon(plan, cause)
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
		   WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2
		     AND is_temporary = true AND temporary_reason = $3
		 )`, channelID, userID, tempGrantReason).Scan(&exists)
	return exists, err
}
