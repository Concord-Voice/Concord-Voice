package graphpresence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
)

// CaptureInTx completes the contract implementation, so the assertion lives
// here rather than beside the other methods.
var _ presencecapture.GraphPresenceCapture = (*Reconciler)(nil)

// CaptureInTx resolves the pre-mutation state INSIDE the caller's transaction,
// before the write. It must not be split into a pre-transaction phase: this
// family's write mutates the friend graph the capture reads, which is exactly
// the disjointness precondition #2445's pre-transaction phase depends on.
//
// The focal set is the principals themselves. The peripheral direction — the
// third parties whose audience shrinks because a principal left it — collapses
// to a viewer-scoped disconnect of that principal rather than N sender
// operations, because what actually changed is that ONE viewer lost access.
// That keeps users-row lock counts at <= 2 per transaction.
func (r *Reconciler) CaptureInTx(
	ctx context.Context, tx *sql.Tx, subject presencecapture.Subject,
) (presencecapture.Plan, error) {
	if tx == nil {
		return nil, errors.New("graphpresence: CaptureInTx requires a transaction")
	}

	focal := focalSenders(subject)
	if err := r.checkFocalBound(focal); err != nil {
		// Fails closed regardless of posture: an oversized focal set is a bug.
		return nil, err
	}

	// A SAVEPOINT is what makes FailConservativeDegrade reachable at all.
	//
	// Every read below runs on the CALLER's transaction, and a failed statement
	// inside a PostgreSQL transaction poisons it (25P02): the handler's next
	// write then fails regardless, so the degrade branch returned a plan into a
	// transaction that could no longer commit, and BlockUser 500'd anyway — the
	// exact denial of a safety affordance the posture exists to prevent. It was
	// inert for its whole intended failure mode (PR #2738 review,
	// @e2ee-reviewer), and the unit test could not catch it because it
	// fabricated a degraded plan instead of failing a real read.
	//
	// Rolling back to the savepoint restores a usable transaction, so the block
	// proceeds and the conservative disconnect substitutes for the exact delta.
	// Reads stay inside the caller's transaction — moving them to another
	// connection would read a different snapshot and break the invariant this
	// whole family is built on.
	restore, err := r.beginCaptureSavepoint(ctx, tx, subject)
	if err != nil {
		return nil, err
	}
	degrade := func(cause degradeCause) (presencecapture.Plan, error) {
		if rbErr := restore(); rbErr != nil {
			// The transaction cannot be made usable again, so proceeding would
			// commit a write with no reconciliation. Fail closed instead.
			return nil, rbErr
		}
		return r.degradePlan(subject, cause), nil
	}

	// GATE (#2738 security fix). A revoking family with a counterpart only
	// narrows an audience when an ACCEPTED friendship edge exists between the
	// two principals at capture time:
	//
	//   - RemoveFriend's DELETE carries `AND status = 'accepted'`.
	//   - Block's UPDATE rewrites any status to 'blocked', but the presence
	//     audience is derived from `status = 'accepted'` rows only
	//     (internal/presence/audience.go friendsOf / friendsOfFriendsOf), so
	//     absent->blocked, pending->blocked and blocked->blocked all leave both
	//     audiences byte-identical.
	//
	// Without this gate the counterpart is a raw path parameter: any
	// authenticated caller could name a stranger and make the capture seed that
	// stranger into the viewer set (a full websocket teardown of their every
	// device) and drag that stranger's entire Server Voice audience into the
	// plan (an abandon fan-out of thousands). Proven-no-change must not
	// reconcile, exactly as RemoveFriend's rowsAffected == 0 branch already
	// argues.
	if subject.Family.CanRevokeVisibility() && subject.Counterpart != uuid.Nil {
		destroys, edgeErr := acceptedEdgeExists(ctx, tx, subject.Principal, subject.Counterpart)
		if edgeErr != nil {
			if subject.FailPosture == presencecapture.FailConservativeDegrade {
				return degrade(causeAudienceRead)
			}
			return nil, edgeErr
		}
		if !destroys {
			// Benign empty terminal: nothing is written that any viewer's
			// authorization depends on, so there is nothing to reconcile and
			// nobody to disconnect.
			return &Plan{subject: subject}, nil
		}
	}

	// The peripheral leg — a viewer-scoped disconnect of the principals — is
	// seeded only for mutations that CAN revoke authorization. It exists to
	// clear state a principal may still hold for senders reachable only through
	// the mutated edge, and an additive mutation creates no such state.
	//
	// DisconnectRichPresenceClients closes every local DEVICE for those users
	// (internal/websocket/richpresence.go), not merely a presence subscription,
	// so seeding it unconditionally tore down both principals' sessions on
	// every friend acceptance for no reconciliation benefit (#2738 review).
	plan := &Plan{subject: subject}
	if subject.Family.CanRevokeVisibility() {
		plan.viewers = make(map[uuid.UUID]bool, len(focal))
		for _, id := range focal {
			plan.viewers[id] = true
		}
	}

	for _, senderID := range focal {
		legs, legErr := r.captureActiveLegs(ctx, tx, senderID)
		if legErr != nil {
			if subject.FailPosture == presencecapture.FailConservativeDegrade {
				return degrade(causeActiveScopeRead)
			}
			return nil, legErr
		}
		plan.active = append(plan.active, legs...)
	}

	if countCaptured(plan) > maxCapturedViewers {
		// Too large for an exact delta. Degrade to the principal superset
		// rather than attempt one; this is a bound, not a failure.
		return r.degradePlan(subject, causeBoundExceeded), nil
	}
	return plan, nil
}

// focalSenders returns the users whose OWN audience changes. Every #2446 family
// yields one or two. uuid.Nil is never a user.
func focalSenders(subject presencecapture.Subject) []uuid.UUID {
	out := make([]uuid.UUID, 0, 2)
	if subject.Principal != uuid.Nil {
		out = append(out, subject.Principal)
	}
	if subject.Counterpart != uuid.Nil && subject.Counterpart != subject.Principal {
		out = append(out, subject.Counterpart)
	}
	return out
}

// voiceScopeRow is one raw live Server Voice row, read and fully drained before
// any audience resolution runs.
type voiceScopeRow struct {
	serverID    uuid.UUID
	channelID   uuid.UUID
	lifecycleAt sql.NullTime
}

// captureActiveLegs reads senderID's live Server Voice scopes through tx and
// resolves each one's pre-mutation authorized audience.
//
// The two phases must stay separate: a *sql.Tx owns exactly one connection, so
// resolving an audience while the scope cursor is still open issues a second
// query on a busy connection and wedges the transaction.
//
// The scope carries lifecycle_event_at, not joined_at, and sets LifecycleID
// equal to RoomID — the presence.CurrentServerVoiceScope contract. A scope
// missing either would be rejected by RefreshServerVoiceRecheck's validation,
// collapsing every exact reconciliation into a conservative disconnect.
func (r *Reconciler) captureActiveLegs(
	ctx context.Context, tx *sql.Tx, senderID uuid.UUID,
) ([]activeLeg, error) {
	scopes, err := activeVoiceScopes(ctx, tx, senderID)
	if err != nil {
		return nil, err
	}

	legs := make([]activeLeg, 0, len(scopes))
	for _, scope := range scopes {
		captured, capErr := presence.CaptureServerVoiceCandidates(
			ctx, tx, r.senderPresence, senderID, scope.serverID,
		)
		if capErr != nil {
			return nil, fmt.Errorf("capture server voice candidates: %w", capErr)
		}
		if len(captured) == 0 {
			continue
		}
		leg := activeLeg{
			senderID: senderID,
			scope: presence.Scope{
				Category:    presence.CategoryServerVoice,
				RoomID:      scope.channelID,
				LifecycleID: scope.channelID,
			},
			captured: captured,
		}
		if scope.lifecycleAt.Valid {
			leg.scope.EventAt = scope.lifecycleAt.Time
		}
		legs = append(legs, leg)
	}
	return legs, nil
}

// activeVoiceScopes drains senderID's live Server Voice rows and closes the
// cursor before returning, so the caller may query tx again.
func activeVoiceScopes(
	ctx context.Context, tx *sql.Tx, senderID uuid.UUID,
) ([]voiceScopeRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.server_id, vp.channel_id, vp.lifecycle_event_at
		FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		WHERE vp.user_id = $1 AND c.type = 'voice'
	`, senderID)
	if err != nil {
		return nil, fmt.Errorf("query active server voice scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var scopes []voiceScopeRow
	for rows.Next() {
		var scope voiceScopeRow
		if err := rows.Scan(&scope.serverID, &scope.channelID, &scope.lifecycleAt); err != nil {
			return nil, fmt.Errorf("scan active server voice scope: %w", err)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active server voice scopes: %w", err)
	}
	return scopes, nil
}

func countCaptured(p *Plan) int {
	return len(p.capturedAudience())
}

// acceptedEdgeExists reports whether an accepted friendship joins the two
// principals inside tx. It is the ONLY presence-visibility edge a #2446
// friendship or block write can destroy; the query is the same predicate
// RemoveFriend's DELETE uses, so the gate and the write agree by construction.
func acceptedEdgeExists(
	ctx context.Context, tx *sql.Tx, principal, counterpart uuid.UUID,
) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM friendships
			WHERE ((requester_id = $1 AND addressee_id = $2)
			    OR (requester_id = $2 AND addressee_id = $1))
			  AND status = 'accepted'
		)
	`, principal, counterpart).Scan(&exists); err != nil {
		return false, fmt.Errorf("read accepted friendship edge: %w", err)
	}
	return exists, nil
}

// The savepoint statements are FIXED literals rather than a name concatenated
// into SQL text. A savepoint name cannot be a bind parameter, so the guideline
// against building SQL by concatenation is satisfied by writing the whole
// statement out — which also removes the only place an identifier could be
// spliced in later by accident (PR #2738 review, CodeRabbit).
const (
	savepointOpen     = "SAVEPOINT concord_graph_presence_capture"
	savepointRollback = "ROLLBACK TO SAVEPOINT concord_graph_presence_capture"
)

// beginCaptureSavepoint opens a savepoint for postures that need the caller's
// transaction to survive a failed capture read, and returns the restore func.
//
// For FailClosedBlockWrite there is nothing to restore — a read failure blocks
// the write by design — so this is a no-op and costs no round trip.
func (r *Reconciler) beginCaptureSavepoint(
	ctx context.Context, tx *sql.Tx, subject presencecapture.Subject,
) (func() error, error) {
	if subject.FailPosture != presencecapture.FailConservativeDegrade {
		return func() error { return nil }, nil
	}
	if _, err := tx.ExecContext(ctx, savepointOpen); err != nil {
		// Without a savepoint the degrade branch cannot restore the
		// transaction, so there is no safe way to proceed with the write.
		return nil, fmt.Errorf("open capture savepoint: %w", err)
	}
	return func() error {
		if _, err := tx.ExecContext(ctx, savepointRollback); err != nil {
			return fmt.Errorf("restore capture savepoint: %w", err)
		}
		return nil
	}, nil
}
