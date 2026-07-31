package rbac

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ServerVisibilityCaptureAdvisoryKey derives the per-server PostgreSQL advisory
// key that totally orders concurrent RBAC/SBAC visibility mutations on one
// server (#2445).
//
// The domain string is load-bearing: it keeps this key disjoint from the
// voice-lifecycle and settings-cleanup advisory key spaces, so no new lock edge
// exists against those transaction families. The lock is always the
// transaction's FIRST statement and the only advisory key these transactions
// take, so no deadlock cycle is constructible.
//
// A per-CHANNEL key was considered and rejected: it cannot order a
// server-scoped role edit against a channel-scoped override edit, which is the
// exact race the determinism acceptance criterion names.
func ServerVisibilityCaptureAdvisoryKey(serverID string) (int64, error) {
	parsed, err := uuid.Parse(serverID)
	if err != nil {
		return 0, fmt.Errorf("invalid visibility capture lock server: %w", err)
	}
	if parsed == uuid.Nil {
		return 0, errors.New("invalid visibility capture lock server")
	}
	digest := sha256.Sum256([]byte("rbac_visibility_capture\x00" + parsed.String()))
	// PostgreSQL advisory locks accept signed int64 keys; preserve all digest bits.
	return int64(binary.BigEndian.Uint64(digest[:8])), nil //nolint:gosec // bit-preserving conversion into the signed advisory key space
}

// LockServerVisibilityCapture takes the per-server advisory transaction lock.
// It MUST be the transaction's first statement: capture and the authority write
// then commit atomically under it, so the captured set is the exact pre-write
// authorized audience.
//
// Exported because internal/voice's temporary-SBAC revoke takes the same lock
// on the same domain; a second derivation would silently diverge.
func LockServerVisibilityCapture(ctx context.Context, tx *sql.Tx, serverID string) error {
	if tx == nil {
		return errors.New("visibility capture transaction unavailable")
	}
	lockKey, err := ServerVisibilityCaptureAdvisoryKey(serverID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("lock server visibility capture: %w", err)
	}
	return nil
}

// withAuthorityCapture runs one RBAC authority write atomically with its
// pre-mutation Rich Presence visibility capture. The ordering is EXACT:
//
//	PrepareCapture (pre-tx, outside the lock)
//	  -> BeginTx -> pg_advisory_xact_lock(server)
//	  -> CaptureVisibility -> authority write -> Commit
//	  -> Execute / Abandon
//
// Phase 1 runs BEFORE BeginTx on purpose. pg_advisory_xact_lock is held from
// the moment it executes until COMMIT, so anything between the lock and the
// write runs under the lock regardless of which connection it uses. Keeping
// the O(#senders) candidate resolution in phase 1 is what makes advisory-lock
// hold time O(#affected channels). Do NOT move PrepareCapture inside the
// transaction — the ordering regression tests lock this.
//
// channelIDs nil means "every active voice channel in the server". onlyUserID
// non-nil bounds the phase-2 visibility-filter input to that one affected user.
//
// This is precisely the structure epic #2555 / issue #2635 later wraps: #2635
// adds the per-(channel,user) enforcement-head advance and the SHARED outbox
// INSERT inside this same transaction and converts the post-commit dispatch
// into an outbox row. Do NOT introduce a second outbox, table, stream,
// dispatcher, or consumer family ahead of it.
//
// Failure classification (spec section 8):
//   - PrepareCapture failure -> return BEFORE BeginTx; no transaction is ever
//     opened and the advisory lock is never taken (500, class 1).
//   - BeginTx / advisory lock / CaptureVisibility / write failure -> rollback,
//     error returned, nothing changed, nothing disclosed, retryable (500,
//     class 2).
//   - Commit() error is AMBIGUOUS (it may have committed): the plan is
//     abandoned fail-closed before the error is returned (class 4).
//
// On success the returned plan is dispatched by the caller AFTER its existing
// cache invalidation and recheckVoice* calls, so call-site ordering stays:
// withAuthorityCapture -> cache invalidate -> recheckVoice* -> presenceExecute
// -> revalidate*Subscribers.
func (h *Handler) withAuthorityCapture(
	ctx context.Context,
	serverID string,
	channelIDs []string,
	onlyUserID *string,
	write func(context.Context, *sql.Tx) error,
) (PresenceRecheckPlan, error) {
	// PHASE 1 - pre-transaction, outside the advisory lock.
	plan, err := h.preparePresenceCapture(ctx, serverID, channelIDs, onlyUserID)
	if err != nil {
		// The capture fan-out bound is a DETERMINISTIC, configuration-reachable
		// failure, not a transient one: a server whose active voice channel count
		// exceeds presenceCaptureMaxChannels disables UpdateRole, DeleteRole,
		// AssignRole and UnassignRole for as long as that holds — including the
		// two revocations an operator most needs during an incident. Its caller
		// returns the same generic 500 as any other capture failure, which is
		// correct for the CLIENT (design §8 disclosure invariant: an error body
		// must not reveal whether a channel had active senders) but leaves an
		// operator with nothing to diagnose. Classify it here so the two are
		// distinguishable in logs while staying identical on the wire.
		//
		// Deliberately NOT promoted to a distinct status code. The four handlers
		// that can reach the bound resolve their channel set from channels WITH
		// active senders, so a distinct response would disclose aggregate voice
		// occupancy — the exact class of signal this issue exists to contain.
		// Raising or removing the bound is a design decision for #2635, not a
		// response-shape change here.
		if errors.Is(err, ErrPresenceCaptureLimited) {
			h.log.Error("Authority write refused: presence capture fan-out bound exceeded",
				"failure_class", "capture_channel_limit", "error", err)
		}
		return nil, err
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin authority transaction: %w", err)
	}
	defer func() {
		// discard: Rollback is a no-op after a successful Commit and there is no
		// recovery available on the failure paths, which already return an error.
		_ = tx.Rollback()
	}()

	if err := LockServerVisibilityCapture(ctx, tx, serverID); err != nil {
		return nil, err
	}
	// PHASE 2 - under the lock, before the write. One query per channel.
	if err := h.capturePresenceVisibility(ctx, tx, plan); err != nil {
		return nil, err
	}
	if err := write(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		h.presenceAbandon(plan, "ambiguous_commit")
		return nil, fmt.Errorf("commit authority transaction: %w", err)
	}
	return plan, nil
}
