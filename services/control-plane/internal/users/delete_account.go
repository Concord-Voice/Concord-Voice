// services/control-plane/internal/users/delete_account.go

package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
)

// ErrUserNotFound is returned when DeleteAccount targets a user row that no
// longer exists. Callers should map this to HTTP 404; the endpoint remains
// idempotent (retry after a successful delete is harmless) while still
// distinguishing "already gone" from "just deleted" in operational logs.
var ErrUserNotFound = errors.New("user not found")

// AccountDeleter is the narrow interface the privacy handler depends on.
// Declared here so privacy can import it without pulling the concrete
// service (and its database handle) into the handler's test surface.
type AccountDeleter interface {
	DeleteAccount(ctx context.Context, userID string) error
}

// ChannelDeletedBroadcaster notifies connected server members after account
// erasure removes an incomplete channel.
type ChannelDeletedBroadcaster func(serverID, channelID string)

// AccountService is the concrete AccountDeleter backed by the primary
// Postgres pool. Its erasure transaction starts only after sender-gated
// activity cleanup has completed.
type AccountService struct {
	db              *sql.DB
	log             *logger.Logger
	activityCleanup *Handler
	channelDeleted  ChannelDeletedBroadcaster

	// graphPresence is the #2447 erasure presence capture. nil means unwired.
	graphPresence presencecapture.GraphPresenceCapture

	// nats fans the erasure clear out to every replica (#2447). The Hub closes
	// only LOCAL clients, so without this a viewer on another replica keeps the
	// erased principal's Custom Status indefinitely -- it carries no TTL and is
	// not republished on a heartbeat. nil leaves that gap open, so the boot path
	// must wire it.
	nats *natsclient.Client
}

// NewAccountService constructs an AccountService. The logger is optional;
// a nil value is tolerated so tests that do not exercise the failure path
// can construct a service without one. Production callers must always
// pass a non-nil logger.
func NewAccountService(db *sql.DB, log *logger.Logger) *AccountService {
	return &AccountService{db: db, log: log}
}

// SetActivitySettingsCleanupHandler binds the same sender coordinator and
// suppression service used by presence-settings writers.
func (s *AccountService) SetActivitySettingsCleanupHandler(handler *Handler) {
	s.activityCleanup = handler
}

// SetChannelDeletedBroadcaster binds the post-commit incomplete-channel event.
func (s *AccountService) SetChannelDeletedBroadcaster(broadcaster ChannelDeletedBroadcaster) {
	s.channelDeleted = broadcaster
}

// DeleteAccount first resumes any durable activity-policy cleanup while holding
// the sender gate, then performs the database erasure inside one transaction:
//  1. Lock the user and delete any incomplete E2EE channels they created, so
//     the post-commit caller can notify their server members.
//  2. DELETE FROM users WHERE id = $1 — cascades through every remaining
//     user_id-FK table configured with ON DELETE CASCADE.
//  3. INSERT an audit row into account_deletions with user_id = NULL. We
//     cannot reference the just-deleted user_id at this point — the FK
//     check would fire on insert and fail — but a NULL is the intended
//     post-commit state anyway (the schema's ON DELETE SET NULL would have
//     nulled it automatically had we ordered INSERT before DELETE). The
//     audit row captures the deletion event and timestamp.
//  4. COMMIT.
//
// The DELETE-then-INSERT ordering makes the retry-after-success path behave
// correctly: a second call gets RowsAffected = 0, returns ErrUserNotFound,
// and leaves no audit side effect. Ordering INSERT-first would make the
// FK reject the retry's audit INSERT with 23503 even though semantically
// nothing needs to happen.
//
// On failure at any stage the deferred rollback restores the pre-call
// state; callers may retry safely (idempotent).
func (s *AccountService) DeleteAccount(
	ctx context.Context,
	userID string,
) error {
	if s.activityCleanup == nil {
		return s.deleteAccount(ctx, userID)
	}
	parsedUserID, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("delete account: invalid user id: %w", err)
	}
	if s.activityCleanup.presenceHistory == nil {
		return errors.New("delete account: activity cleanup coordinator unavailable")
	}
	if s.activityCleanup.activitySuppressor == nil {
		return errors.New("delete account: full activity suppression unavailable")
	}
	return s.activityCleanup.presenceHistory.WithSender(ctx, parsedUserID, func() error {
		if _, err := s.activityCleanup.resumePendingActivitySettingsCleanup(
			ctx, parsedUserID,
		); err != nil {
			return fmt.Errorf("delete account: resume activity cleanup: %w", err)
		}
		if err := s.activityCleanup.activitySuppressor.SuppressAllActivityAlreadyGated(
			ctx, parsedUserID,
		); err != nil {
			return fmt.Errorf("delete account: suppress active activity: %w", err)
		}
		return s.deleteAccount(ctx, userID)
	})
}

func (s *AccountService) deleteAccount(ctx context.Context, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete account: begin tx: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			if s.log != nil {
				s.log.Error("delete account: rollback failed", "error", rbErr)
			}
		}
	}()
	channelIDs, serverIDs, plan, err := s.deleteAccountTx(ctx, tx, userID)
	if err != nil {
		// CauseWriteFailed and friends prove no commit happened, so Abandon does
		// NOT disconnect anyone on those causes -- the deferred rollback discards
		// the plan along with the transaction.
		presencehook.Abandon(s.graphPresence, plan, presencecapture.CauseWriteFailed)
		return err
	}
	// Complete owns the commit. A bare tx.Commit() here would leave the captured
	// plan undispatched, so the surviving senders' audiences would never be
	// reconciled even though the erasure landed.
	//
	// ErrPostCommitDelivery is a DURABLE outcome: the users row is gone and only
	// presence delivery failed. Returning early on it would skip the
	// cross-replica clear and the channel-deleted fan-out below — leaving the
	// erased principal's Custom Status resident on every other replica, which is
	// the precise gap publishErasureCleared exists to close. So record it and
	// fall through (rbac review, PR #2840).
	var deliveryErr error
	if err := presencehook.Complete(ctx, s.graphPresence, tx, plan); err != nil {
		if !errors.Is(err, presencecapture.ErrPostCommitDelivery) {
			return fmt.Errorf("delete account: commit: %w", err)
		}
		deliveryErr = err
	}

	// Cross-replica Custom Status clear (#2447). AFTER the commit: publishing
	// first would tell the fleet to clear an account that may still exist.
	s.publishErasureCleared(userID)
	if s.channelDeleted != nil {
		for index, channelID := range channelIDs {
			s.channelDeleted(serverIDs[index], channelID)
		}
	}
	// Surfaced only after every post-commit obligation has run. The account IS
	// erased; this reports that presence delivery did not settle.
	if deliveryErr != nil {
		return fmt.Errorf("delete account: presence delivery: %w", deliveryErr)
	}
	return nil
}

func (s *AccountService) deleteAccountTx(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
) (pq.StringArray, pq.StringArray, presencecapture.Plan, error) {
	var lockedUserID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&lockedUserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil, ErrUserNotFound
		}
		return nil, nil, nil, fmt.Errorf("delete account: lock user: %w", err)
	}

	// Capture-before-cascade, under the user-row lock, with nothing between it
	// and the DELETE below that could change the audience. user_presence_settings
	// and presence_settings_pending_operations are both ON DELETE CASCADE, so the
	// rows defining this audience are destroyed by that DELETE.
	//
	// This reconciles the SURVIVING senders whose audiences shrink. The erased
	// principal's own state is handled separately and synchronously, by
	// SuppressAllActivityAlreadyGated before this transaction opens.
	plan, captureErr := s.captureErasureAlreadyGated(ctx, tx, lockedUserID)
	if captureErr != nil {
		return nil, nil, nil, fmt.Errorf("delete account: capture presence: %w", captureErr)
	}

	var channelIDs, serverIDs pq.StringArray
	if err := tx.QueryRowContext(ctx, `
		WITH incomplete AS (
			SELECT channel_id
			FROM channel_initial_key_distributions
			WHERE creator_id = $1
		)
		SELECT COALESCE(array_agg(c.id::text ORDER BY c.id), ARRAY[]::text[]),
		       COALESCE(array_agg(c.server_id::text ORDER BY c.id), ARRAY[]::text[])
		FROM channels c
		WHERE c.id IN (SELECT channel_id FROM incomplete)
		   OR c.linked_voice_channel_id IN (SELECT channel_id FROM incomplete)`, lockedUserID,
	).Scan(&channelIDs, &serverIDs); err != nil {
		return nil, nil, plan, fmt.Errorf("delete account: list incomplete channels: %w", err)
	}
	if len(channelIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE id = ANY($1::uuid[])`, pq.Array(channelIDs)); err != nil {
			return nil, nil, plan, fmt.Errorf("delete account: delete incomplete channels: %w", err)
		}
	}

	result, err := tx.ExecContext(ctx,
		`DELETE FROM users WHERE id = $1`,
		lockedUserID,
	)
	if err != nil {
		return nil, nil, plan, fmt.Errorf("delete account: delete user: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, nil, plan, fmt.Errorf("delete account: rows affected: %w", err)
	}
	if rows == 0 {
		return nil, nil, plan, ErrUserNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO account_deletions (user_id) VALUES (NULL)`,
	); err != nil {
		return nil, nil, plan, fmt.Errorf("delete account: insert audit: %w", err)
	}
	return channelIDs, serverIDs, plan, nil
}

// SetGraphPresenceCapture wires the #2447 erasure presence capture.
func (s *AccountService) SetGraphPresenceCapture(c presencecapture.GraphPresenceCapture) {
	s.graphPresence = c
}

// HasGraphPresenceCapture reports whether the capture was wired. The router's
// boot guard interrogates the SERVICE through this, never the constructed
// reconciler value, for the same reason the handler guards do.
func (s *AccountService) HasGraphPresenceCapture() bool { return s.graphPresence != nil }

// HasErasureClearPublisher reports whether the erasure-clear publisher is wired.
//
// The boot guard interrogates this because the publish is not a cross-replica
// nicety — it is the ONLY mechanism that retracts an erased user's already
// delivered Custom Status, including on the publishing replica. The durable C2
// rail provably cannot cover this sender: its marker path locks the sender's
// users row first, and after the erasure commits that row is gone, so delivery
// is skipped with no error at all.
//
// FAILING CLOSED HERE IS STILL RIGHT. #2854 finding A did not change that; it
// changed what a nil MEANS. Before `RetryOnFailedConnect` (pkg/nats.Connect) a
// nil could be a transient bus outage, and fataling on that took auth and
// health down with it and crash-looped self-hosted and dev deployments. It can
// no longer: an outage now yields a reconnecting client, so a nil arriving here
// is a DELETED WIRING LINE or an unparseable NATS_URL — both deterministic
// deploy defects that never fix themselves, which is exactly what a boot guard
// is for.
//
// DO NOT weaken this to a "was SetNATS called" flag to make some future
// outage-shaped crash-loop go away. That was built and rejected in #2854 stage
// A, for three reasons found by three separate reviewers: it boots on a
// misconfiguration too; a nil client silently disables the voice permission
// enforcer's dispatch (internal/voice/permission_enforcer.go), so a banned or
// permission-revoked member keeps talking in a live room; and it removes the
// only observer of `s.nats`'s assignment, after which deleting `s.nats = c`
// kills the erasure clear on every replica with every test still green. Fix the
// connection, not the guard.
func (s *AccountService) HasErasureClearPublisher() bool { return s.nats != nil }

// SetNATS wires the cross-replica erasure-clear publisher.
func (s *AccountService) SetNATS(c *natsclient.Client) { s.nats = c }

// NATSSubjectPresenceErasureCleared fans an account erasure out to every replica
// so viewers still holding the erased principal's Custom Status drop it.
//
// It is exported and lives here, beside its only publisher, because the
// subscriber is in another package: a second unexported copy of the literal
// would let the two drift and publish into a subject nothing listens on.
//
// Core NATS, so at-most-once. See publishErasureCleared for the residual.
const NATSSubjectPresenceErasureCleared = "presence.erasure.cleared"

// captureErasureAlreadyGated captures the erased principal's pre-cascade
// audience on a transaction whose sender gate the caller ALREADY holds.
//
// Deliberately NOT presencehook.WithGatedTx. DeleteAccount already runs inside
// presenceHistory.WithSender, which takes a buffered-1 channel stripe keyed on
// this user; WithGatedTx would reach rail.WithSenders, hash to the same stripe
// and block on it forever. That deadlock has no timeout and no detector, because
// half the cycle is a Go channel rather than a database lock. The AlreadyGated
// naming convention exists for exactly this hazard.
//
// The family is FamilyMemberRemove: erasure revokes shared-server visibility and
// carries the Custom Status topology, which is precisely that family's policy. A
// dedicated erasure family would say the same two things while adding a fifth
// value to a positionally dense enum that two slices are already appending to.
func (s *AccountService) captureErasureAlreadyGated(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
) (presencecapture.Plan, error) {
	if s.graphPresence == nil {
		return nil, nil
	}
	parsed, err := uuid.Parse(userID)
	if err != nil {
		// Fail closed: capturing against uuid.Nil would drop the principal from
		// the focal set silently and reconcile nothing.
		return nil, fmt.Errorf("parse erasure principal: %w", err)
	}
	return s.graphPresence.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyMemberRemove,
		FailPosture: presencecapture.FailClosedBlockWrite,
		Principal:   parsed,
	})
}

// publishErasureCleared tells every replica to drop the erased principal's
// already-delivered Custom Status.
//
// Why this exists at all: SuppressAllActivityAlreadyGated deletes only
// CategoryServerVoice and CategoryPrivateCall and never touches Custom Status,
// and the disconnect it falls back on is Hub.DisconnectAllRichPresenceClients,
// which closes every LOCAL client. The Hub holds no NATS connection and no Redis
// pub/sub, so without this a viewer on another replica keeps the erased user's
// status text indefinitely -- Custom Status carries no TTL and is not
// republished on a heartbeat.
//
// A push rather than a fence. presence_offline_fences cannot help here: its
// user_id is REFERENCES users(id) ON DELETE CASCADE, so a fence row for an
// erased user is destroyed by the very DELETE it would need to outlive. A fence
// is also the wrong shape -- it denies re-authorization on the emission path and
// never retracts what was already delivered.
//
// Best-effort by design: the error is logged, not returned. The account IS
// erased by this point, so failing the request would invite a retry against a
// user row that no longer exists.
//
// It takes NO context, deliberately. An earlier version guarded on
// ctx.Err() using the inbound REQUEST context, which meant a client that
// disconnected between the commit and this publish silently skipped the clear
// entirely (Gitar review, PR #2840). This runs AFTER the erasure has committed,
// so it is a compensating action: cancellation of the originating request must
// never be a reason to skip it. There is nothing to cancel anyway --
// Client.Publish is a non-blocking local buffer write with no context parameter.
//
// ponytail: core NATS, at-most-once. Named residual -- a replica that misses
// this publish AND holds a viewer connected continuously never re-derives. Every
// other path self-heals, because after erasure there is nothing left to
// re-derive against. internal/attestation accepts the same semantics for
// registry revocation. Upgrade path if that residual ever matters: JetStream, or
// a durable FK-free clear ledger the replicas replay on reconnect.
func (s *AccountService) publishErasureCleared(userID string) {
	if s.nats == nil {
		return
	}
	if err := s.nats.Publish(NATSSubjectPresenceErasureCleared, map[string]interface{}{
		"user_id": userID,
	}); err != nil && s.log != nil {
		s.log.Error("Cross-replica presence clear failed", "failure_class", "delivery")
	}
}
