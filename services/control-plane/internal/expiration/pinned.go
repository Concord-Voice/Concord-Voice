package expiration

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
)

// Pin-aware expiry: the held-stamp state machine (#3459, migration 000168).
// Read these invariants before changing any statement this file names.
//
// Terms. W is a context's window and include its raw
// expiration_include_pinned column. Every reader reports the effective value
// E = W IS NOT NULL AND include (Policy.IncludePinned). A context KEEPS pins,
// "K", when W IS NOT NULL AND NOT include (Policy.KeepsPins). A kept pin's
// would-be expiry parks in held_expires_at, which nothing that deletes reads.
//
// I-HELD. held_expires_at IS NOT NULL implies the row is pinned, its
// expires_at IS NULL, and its context is K. 000168's CHECK enforces the first
// two conjuncts on every write. This file enforces the third: every policy
// write reconciles the context's pinned rows in the same transaction, after
// the update and under the parent lock (reconcilePinnedRows), so no committed
// state has a held row under a context that stopped keeping pins.
//
// I-KEEP. In a K context a pinned row carries a non-NULL expires_at only when
// that stamp had already passed at the moment keep began to govern the row:
// the accepted cutoff C for a policy write, statement_timestamp() for a pin.
// That is how AC1 holds with no keep predicate anywhere in the sweeper's
// discovery (C-SWEEP): keep is expressed only by the stamp's absence, so a
// sweep can never stall on a kept row, and the preflight terminates.
//
// C-NORES. A stamp that has already passed is never moved into held and never
// cleared. Every move out of expires_at is predicated on expires_at > C (or >
// statement_timestamp() for a pin), never >=. A passed stamp stays, and the
// sweeper deletes it.
//
// R2 (convergence). The retroactive backfill (applyBatchSQL, applyEligibleSQL)
// and the start reconcile (reconcileApplySQL) embed ONE fragment, defined once
// below by compile-time concatenation. A batch then writes exactly the shape
// the eligibility probe stops finding. Two copies that drift make the batch
// rewrite rows the probe keeps reporting, and backfill_pending never clears
// (409 on every later policy change). A pin or unpin during a backfill
// serialises on the parent row and moves a row only toward its target shape or
// out of eligibility, so the batches stay monotone.
//
// What breaks them: a policy write that skips reconcilePinnedRows (I-HELD); a
// move predicate written with >= (C-NORES); a second copy of the apply
// fragment (R2); a keep predicate in sweeper discovery, selectExpiry or
// anyEligible (C-SWEEP); a pinned_at writer that does not embed
// ChannelPinHoldAssignments, ConversationPinHoldAssignments or
// UnpinReleaseAssignments (I-HELD, I-KEEP).
//
// Q-SHAPE. Every pinned-only statement that names held_expires_at also carries
// `pinned_at IS NOT NULL`, so it rides idx_messages_pinned or
// idx_dm_messages_pinned. The CHECK (held implies pinned) makes the predicate
// complete. Every such statement is bounded by the pin cap through that index.

// The shared apply fragment. Placeholders are fixed for every statement that
// embeds it: $2 the backfill cutoff C, $3 the window in seconds, $4 keep (the
// context is K). $1 is the scope id and appears only in the embedding
// statement. Unqualified columns name the message row.
const (
	// applyKeepsRow is true for a row the fragment holds rather than stamps.
	applyKeepsRow = `$4::boolean AND pinned_at IS NOT NULL`
	// applyWindowStamp is the would-be expiry under the new window.
	applyWindowStamp = `created_at + make_interval(secs => $3)`
	// applyTargetExpiresAt and applyTargetHeld are the row's target shape.
	applyTargetExpiresAt = `CASE WHEN ` + applyKeepsRow + ` THEN NULL ELSE ` + applyWindowStamp + ` END`
	applyTargetHeld      = `CASE WHEN ` + applyKeepsRow + ` THEN ` + applyWindowStamp + ` ELSE NULL END`
	// applyEligible selects a row the backfill still owes: written at or
	// before the cutoff, not already due (C-NORES: expires_at > C, never >=),
	// and not already in its target shape. A held row has no stamp, so it is
	// never "due" here; recomputing its held value may lengthen or shorten it,
	// and resurrects nothing, because a held row was never going to be deleted.
	applyEligible = `created_at <= $2
    AND (expires_at IS NULL OR expires_at > $2)
    AND (expires_at, held_expires_at) IS DISTINCT FROM
        (` + applyTargetExpiresAt + `, ` + applyTargetHeld + `)`
	// applyAssignments writes the target shape applyEligible compares against.
	applyAssignments = `expires_at = ` + applyTargetExpiresAt + `,
    held_expires_at = ` + applyTargetHeld
)

// Pin and unpin SET fragments. Each is appended to the SET list of the UPDATE
// that writes pinned_at, so the stamp moves in the same statement as the pin
// state and the CHECK sees a consistent row. They name no table alias: the
// parent subquery correlates on the bare channel_id / conversation_id column,
// which resolves to the UPDATE's target row because channels has no
// channel_id column and dm_conversations has no conversation_id column (a
// test pins both facts). That is what lets the same fragment serve the
// unaliased channel UPDATE and the `dm_messages dm` UPDATE.
//
// The caller must already hold the parent row (channels FOR SHARE,
// dm_conversations FOR NO KEY UPDATE): the subquery reads K from it unlocked,
// and the policy write's FOR NO KEY UPDATE conflicts with either lock, so a
// pin and a policy change serialise and neither reads the other half-done.
const (
	// channelPinHolds is true when a pin must move the row's stamp into held:
	// the stamp has not passed (C-NORES and D-C: a due stamp stays and the
	// sweeper deletes it) and the channel keeps pins. statement_timestamp() is
	// fixed for the whole statement, so both CASE arms test the same instant
	// and can never straddle the stamp (X6); two clock_timestamp() reads could,
	// and the CHECK would then fail the pin.
	channelPinHolds = `expires_at > statement_timestamp()
        AND COALESCE((SELECT expiry_parent.expiration_window_seconds IS NOT NULL
                             AND NOT expiry_parent.expiration_include_pinned
                      FROM channels expiry_parent
                      WHERE expiry_parent.id = channel_id), FALSE)`
	// conversationPinHolds is channelPinHolds for a DM conversation.
	conversationPinHolds = `expires_at > statement_timestamp()
        AND COALESCE((SELECT expiry_parent.expiration_window_seconds IS NOT NULL
                             AND NOT expiry_parent.expiration_include_pinned
                      FROM dm_conversations expiry_parent
                      WHERE expiry_parent.id = conversation_id), FALSE)`

	// ChannelPinHoldAssignments is appended to the SET list of every UPDATE of
	// messages that sets pinned_at. In a channel that keeps pins it moves a
	// stamp that has not passed into held_expires_at; otherwise it changes
	// nothing.
	ChannelPinHoldAssignments = `expires_at = CASE WHEN ` + channelPinHolds + ` THEN NULL ELSE expires_at END,
    held_expires_at = CASE WHEN ` + channelPinHolds + ` THEN expires_at ELSE held_expires_at END`

	// ConversationPinHoldAssignments is ChannelPinHoldAssignments for every
	// UPDATE of dm_messages that sets pinned_at.
	ConversationPinHoldAssignments = `expires_at = CASE WHEN ` + conversationPinHolds + ` THEN NULL ELSE expires_at END,
    held_expires_at = CASE WHEN ` + conversationPinHolds + ` THEN expires_at ELSE held_expires_at END`

	// UnpinReleaseAssignments is appended to the SET list of every UPDATE of
	// messages or dm_messages that clears pinned_at. A held stamp goes back
	// on the timer as it was; a stamp already past is due at the next sweep.
	// There is no unpin grace (D-A). Forgetting it is a loud failure: the
	// CHECK refuses an unpinned row that still has a held stamp.
	UnpinReleaseAssignments = `expires_at = COALESCE(expires_at, held_expires_at),
    held_expires_at = NULL`
)

// reconcilePinnedRows brings a context's pinned rows into step with the policy
// the caller's transaction just wrote, and returns the pinned rows that are due
// when the context stopped keeping pins (#3459 §5). It runs inside start, after
// updatePolicy, under lockPolicy's parent lock, and uses the accepted cutoff C
// that updatePolicySQL recorded (Current.UpdatedAt), never now().
//
//	clear (either mode)   drop every held stamp: a cleared context is not K,
//	                      and a kept pin was never scheduled, so even
//	                      leave_pending has nothing to keep pending for it
//	set new_only, K       move each pinned stamp that has not passed into held
//	set new_only, not K   put each held stamp back into expires_at
//	set apply             the shared apply fragment over pinned rows, no LIMIT
//
// The due collect runs only when the context kept pins before and does not
// now, and only AFTER the move, so it sees stamps the move restored as well as
// stamps a pin left on a row already due (D-C).
func reconcilePinnedRows(ctx context.Context, tx *sql.Tx, scope scope, id string, request Request, transition Transition) ([]string, error) {
	if transition.Current.UpdatedAt == nil {
		return nil, fmt.Errorf("reconcile %s pinned expiry: accepted cutoff missing", scope.name)
	}
	cutoff := *transition.Current.UpdatedAt
	keep := transition.Current.KeepsPins()

	var query string
	var args []any
	switch {
	case request.Mode == "clear":
		query, args = scope.dropHeldSQL, []any{id}
	case request.Retroactive == "apply":
		if transition.Current.WindowSeconds == nil {
			return nil, fmt.Errorf("reconcile %s pinned expiry: apply without a window", scope.name)
		}
		query, args = scope.reconcileApplySQL, []any{id, cutoff, *transition.Current.WindowSeconds, keep}
	case keep:
		query, args = scope.holdPinnedSQL, []any{id, cutoff}
	default:
		query, args = scope.releaseHeldSQL, []any{id}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return nil, fmt.Errorf("reconcile %s pinned expiry: %w", scope.name, err)
	}

	if !transition.Previous.KeepsPins() || keep {
		return nil, nil
	}
	return collectDuePinned(ctx, tx, scope, id, transition)
}

// collectDuePinned reads the pinned rows already past the accepted cutoff. It
// is bounded by the pin cap through the pinned index and, for a context with
// legacy pins beyond the cap, by one expiry batch; anything it cannot carry is
// deleted by the next sweep, which needs no help to find a stamped row.
func collectDuePinned(ctx context.Context, tx *sql.Tx, scope scope, id string, transition Transition) (due []string, returnErr error) {
	rows, err := tx.QueryContext(ctx, scope.duePinnedSQL, id, *transition.Current.UpdatedAt, purge.MaxExpiryCandidateIDs)
	if err != nil {
		return nil, fmt.Errorf("collect due %s pinned messages: %w", scope.name, err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("close due %s pinned messages: %w", scope.name, closeErr)
		}
	}()
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			return nil, fmt.Errorf("scan due %s pinned message: %w", scope.name, err)
		}
		due = append(due, messageID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due %s pinned messages: %w", scope.name, err)
	}
	return due, nil
}
