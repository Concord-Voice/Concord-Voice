package rbac

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// errAmbiguousAuthorityCommit marks the only failure class where the authority
// write may have committed. Callers must not report success or execute the
// captured presence plan, but must run their bounded fail-closed recovery.
var errAmbiguousAuthorityCommit = errors.New("ambiguous authority commit")

const authorityLifecyclePrincipalPrefix = "lifecycle-principal:"

// authorityLifecyclePrincipal marks a user whose Server Voice lifecycle
// advisory lock must precede the ordinary user/FK lock set. It is deliberately
// private to the coordinator package; callers cannot accidentally reorder
// lifecycle locking through an arbitrary callback.
func authorityLifecyclePrincipal(userID string) string {
	return authorityLifecyclePrincipalPrefix + userID
}

// IsAmbiguousAuthorityCommit identifies the acknowledgement-lost failure
// class. Callers must apply only idempotent fail-closed post-commit effects.
func IsAmbiguousAuthorityCommit(err error) bool {
	return errors.Is(err, errAmbiguousAuthorityCommit)
}

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

// ServerVoiceLifecycleAdvisoryKey derives the Server Voice participant
// lifecycle key. It is defined here because RBAC permanent overrides must
// serialize with temporary voice-grant lifecycle mutations without importing
// internal/voice.
func ServerVoiceLifecycleAdvisoryKey(userID uuid.UUID) (int64, error) {
	if userID == uuid.Nil {
		return 0, errors.New("invalid voice lifecycle lock sender")
	}
	digest := sha256.Sum256([]byte("server_voice\x00" + userID.String()))
	return int64(binary.BigEndian.Uint64(digest[:8])), nil //nolint:gosec // bit-preserving conversion into the signed advisory key space
}

// LockServerVoiceLifecycleTx takes the exact Server Voice lifecycle lock after
// the server visibility lock and before the channel/key lock.
func LockServerVoiceLifecycleTx(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error {
	if tx == nil {
		return errors.New("server voice lifecycle transaction unavailable")
	}
	lockKey, err := ServerVoiceLifecycleAdvisoryKey(userID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("lock voice lifecycle mutation: %w", err)
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
	principalIDs ...string,
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

	defer h.hub.BeginAudienceRevocation()()

	// READ COMMITTED is pinned so the first post-lock read observes the prior
	// lock holder's committed authority state. Under REPEATABLE READ, the
	// advisory-lock statement would establish a stale snapshot before waiting.
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
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
	var lifecyclePrincipalIDs []string
	principalIDs, lifecyclePrincipalIDs = splitAuthorityLifecyclePrincipals(principalIDs)
	if err := lockAuthorityLifecyclePrincipalsTx(ctx, tx, lifecyclePrincipalIDs); err != nil {
		return nil, err
	}
	// Users precede domain parents. Authority writes can insert revoked_by and
	// assigned_by FKs, while CaptureVisibility itself deliberately takes no user
	// lock; lock every supplied actor/FK principal in one canonical query first.
	if err := LockAuthorityPrincipalsTx(ctx, tx, principalIDs); err != nil {
		return nil, err
	}
	// This parent fence serializes CreateChannel's server FK lock. It must be
	// taken before the capture's domain reads, after all user/FK principals.
	var lockedServerID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&lockedServerID); err != nil {
		return nil, fmt.Errorf("lock authority server parent: %w", err)
	}
	// PHASE 2 - under the advisory and parent fences, before the write.
	if err := h.capturePresenceVisibility(ctx, tx, plan); err != nil {
		return nil, err
	}
	if err := write(ctx, tx); err != nil {
		return nil, err
	}
	commit := h.authorityCommit
	if commit == nil {
		commit = func(commitTx *sql.Tx) error { return commitTx.Commit() }
	}
	if err := commit(tx); err != nil {
		h.presenceAbandon(plan, "ambiguous_commit")
		return nil, fmt.Errorf("%w: %v", errAmbiguousAuthorityCommit, err)
	}
	return plan, nil
}

func splitAuthorityLifecyclePrincipals(principalIDs []string) ([]string, []string) {
	users := make([]string, 0, len(principalIDs))
	lifecycle := make([]string, 0, 1)
	for _, principalID := range principalIDs {
		if strings.HasPrefix(principalID, authorityLifecyclePrincipalPrefix) {
			lifecycle = append(lifecycle, strings.TrimPrefix(principalID, authorityLifecyclePrincipalPrefix))
			continue
		}
		users = append(users, principalID)
	}
	return users, lifecycle
}

func lockAuthorityLifecyclePrincipalsTx(ctx context.Context, tx *sql.Tx, principalIDs []string) error {
	seen := make(map[string]struct{}, len(principalIDs))
	ids := make([]string, 0, len(principalIDs))
	for _, id := range principalIDs {
		if id == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("invalid authority lifecycle principal: %w", err)
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		principalID, err := uuid.Parse(id)
		if err != nil {
			return fmt.Errorf("parse authority lifecycle principal: %w", err)
		}
		if err := LockServerVoiceLifecycleTx(ctx, tx, principalID); err != nil {
			return fmt.Errorf("lock authority lifecycle principal: %w", err)
		}
	}
	return nil
}

// LockAuthorityPrincipalsTx locks every actor or user referenced by an
// authority write before its server/channel parents. The sorted exact set
// prevents a pair of multi-user mutations from taking opposite user locks.
func LockAuthorityPrincipalsTx(ctx context.Context, tx *sql.Tx, principalIDs []string) (returnErr error) {
	seen := make(map[string]struct{}, len(principalIDs))
	ids := make([]string, 0, len(principalIDs))
	for _, id := range principalIDs {
		if id == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("invalid authority principal: %w", err)
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM users WHERE id = ANY($1::uuid[])
		ORDER BY id FOR NO KEY UPDATE`, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("lock authority principals: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close authority principals: %w", closeErr))
		}
	}()
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate authority principals: %w", err)
	}
	if count != len(ids) {
		return errors.New("authority principal no longer exists")
	}
	return nil
}

// RunChannelAuthorityMutation gives channel topology writers the same
// visibility-capture transaction used by RBAC override writers. The supplied
// write must lock and revalidate its exact preflight child set before changing
// it; a set mismatch must abort rather than applying a stale capture.
func (h *Handler) RunChannelAuthorityMutation(
	ctx context.Context,
	serverID string,
	voiceChannelIDs []string,
	write func(context.Context, *sql.Tx) error,
	principalIDs ...string,
) (PresenceRecheckPlan, error) {
	return h.withAuthorityCapture(ctx, serverID, voiceChannelIDs, nil, write, principalIDs...)
}

// ErrChannelAuthorityChannelLimit refuses a server-wide authority mutation
// whose exact affected channel set cannot be locked and reconciled in one
// bounded transaction.
var ErrChannelAuthorityChannelLimit = errors.New("channel authority channel limit exceeded")

const maxChannelAuthorityChannels = 500

// ChannelAuthorityMutation is the confirmed transaction result used by every
// VIEW-capable writer. The IDs are the exact rows locked under the server
// visibility lock; callers must use them for both completion and ambiguity
// recovery rather than re-querying a topology that may have changed.
type ChannelAuthorityMutation struct {
	Plan            PresenceRecheckPlan
	ChannelIDs      []string
	Rotations       []keyrotation.Rotation
	DeniedByChannel map[string][]string
	Noop            bool
}

type channelAuthorityTarget struct {
	all   []string
	voice []string
}

func sameAuthorityChannelTarget(left, right channelAuthorityTarget) bool {
	return sameChannelIDs(left.all, right.all) && sameChannelIDs(left.voice, right.voice)
}

// serverChannelAuthorityTarget lists a bounded, canonical server channel set
// for role mutations. It is deliberately read before capture only; the locked
// transaction re-reads and compares the same set before any DML.
func (h *Handler) serverChannelAuthorityTarget(ctx context.Context, serverID string) (channelAuthorityTarget, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT id, type = 'voice' FROM channels
		WHERE server_id = $1
		ORDER BY id
		LIMIT $2`, serverID, maxChannelAuthorityChannels+1)
	if err != nil {
		return channelAuthorityTarget{}, fmt.Errorf("list server authority channels: %w", err)
	}
	target := channelAuthorityTarget{all: []string{}, voice: []string{}}
	for rows.Next() {
		var (
			channelID string
			isVoice   bool
		)
		if err := rows.Scan(&channelID, &isVoice); err != nil {
			return channelAuthorityTarget{}, fmt.Errorf("scan server authority channel: %w", errors.Join(err, rows.Close()))
		}
		target.all = append(target.all, channelID)
		if isVoice {
			target.voice = append(target.voice, channelID)
		}
	}
	if err := rows.Err(); err != nil {
		return channelAuthorityTarget{}, fmt.Errorf("iterate server authority channels: %w", errors.Join(err, rows.Close()))
	}
	if err := rows.Close(); err != nil {
		return channelAuthorityTarget{}, fmt.Errorf("close server authority channels: %w", err)
	}
	if len(target.all) > maxChannelAuthorityChannels {
		return channelAuthorityTarget{}, ErrChannelAuthorityChannelLimit
	}
	return target, nil
}

func serverChannelAuthorityTargetTx(ctx context.Context, tx *sql.Tx, serverID string) (channelAuthorityTarget, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, type = 'voice' FROM channels
		WHERE server_id = $1
		ORDER BY id
		LIMIT $2
		FOR UPDATE`, serverID, maxChannelAuthorityChannels+1)
	if err != nil {
		return channelAuthorityTarget{}, fmt.Errorf("lock server authority channels: %w", err)
	}
	target := channelAuthorityTarget{all: []string{}, voice: []string{}}
	for rows.Next() {
		var (
			channelID string
			isVoice   bool
		)
		if err := rows.Scan(&channelID, &isVoice); err != nil {
			return channelAuthorityTarget{}, fmt.Errorf("scan locked server authority channel: %w", errors.Join(err, rows.Close()))
		}
		target.all = append(target.all, channelID)
		if isVoice {
			target.voice = append(target.voice, channelID)
		}
	}
	if err := rows.Err(); err != nil {
		return channelAuthorityTarget{}, fmt.Errorf("iterate locked server authority channels: %w", errors.Join(err, rows.Close()))
	}
	if err := rows.Close(); err != nil {
		return channelAuthorityTarget{}, fmt.Errorf("close locked server authority channels: %w", err)
	}
	if len(target.all) > maxChannelAuthorityChannels {
		return channelAuthorityTarget{}, ErrChannelAuthorityChannelLimit
	}
	return target, nil
}

// LockServerAuthorityChannelsTx locks the complete bounded server channel set
// in canonical order. Multi-consumer revocation writers call it after taking
// the server visibility lock and before their membership/ownership DML, so
// message writers holding channels FOR SHARE serialize with the epoch fence.
func (h *Handler) LockServerAuthorityChannelsTx(ctx context.Context, tx *sql.Tx, serverID string) ([]string, error) {
	target, err := serverChannelAuthorityTargetTx(ctx, tx, serverID)
	if err != nil {
		return nil, err
	}
	return target.all, nil
}

var errChannelAuthoritySetChanged = errors.New("channel authority set changed")
var errChannelAuthorityMutationNoop = errors.New("channel authority mutation noop")

// withServerChannelKeyAuthorityMutation is the bounded coordinator for role
// writes that can change effective VIEW on every channel in a server. It holds
// the visibility lock through channel locking, raw durable-recipient capture,
// caller DML, fresh VIEW filtering, purge, and the one successor epoch per
// channel. The callback performs only the role/member write after the exact
// channel set is locked and before reconciliation.
func (h *Handler) withServerChannelKeyAuthorityMutation(
	ctx context.Context,
	serverID, actorID, tokenEpoch string,
	onlyCandidateUserID *string,
	write func(context.Context, *sql.Tx) error,
) (ChannelAuthorityMutation, error) {
	for attempt := 0; attempt < 2; attempt++ {
		preflight, err := h.serverChannelAuthorityTarget(ctx, serverID)
		if err != nil {
			return ChannelAuthorityMutation{}, err
		}
		result := ChannelAuthorityMutation{ChannelIDs: preflight.all}
		principalIDs := []string{actorID}
		if onlyCandidateUserID != nil {
			principalIDs = append(principalIDs, *onlyCandidateUserID)
		}
		plan, err := h.withAuthorityCapture(ctx, serverID, preflight.voice, nil,
			func(ctx context.Context, tx *sql.Tx) error {
				if err := credepoch.GuardTx(ctx, tx, actorID, tokenEpoch); err != nil {
					return err
				}
				var writeErr error
				result, writeErr = h.runServerChannelKeyAuthorityWrite(
					ctx, tx, serverID, actorID, onlyCandidateUserID, preflight, write,
				)
				return writeErr
			}, principalIDs...,
		)
		result.Plan = plan
		if !errors.Is(err, errChannelAuthoritySetChanged) {
			return result, err
		}
	}
	return ChannelAuthorityMutation{}, errChannelAuthoritySetChanged
}

func (h *Handler) runServerChannelKeyAuthorityWrite(
	ctx context.Context,
	tx *sql.Tx,
	serverID, actorID string,
	onlyCandidateUserID *string,
	preflight channelAuthorityTarget,
	write func(context.Context, *sql.Tx) error,
) (ChannelAuthorityMutation, error) {
	result := ChannelAuthorityMutation{ChannelIDs: preflight.all}
	locked, err := serverChannelAuthorityTargetTx(ctx, tx, serverID)
	if err != nil {
		return result, err
	}
	if !sameAuthorityChannelTarget(preflight, locked) {
		return result, errChannelAuthoritySetChanged
	}
	result.ChannelIDs = locked.all
	candidates, err := captureAuthorityCandidates(ctx, tx, locked.all, onlyCandidateUserID)
	if err != nil {
		return result, err
	}
	if err := write(ctx, tx); err != nil {
		if errors.Is(err, errChannelAuthorityMutationNoop) {
			result.Noop = true
			return result, nil
		}
		return result, err
	}
	result.Rotations, result.DeniedByChannel, err = h.RevokeDeniedChannelKeyCandidatesTx(
		ctx, tx, serverID, actorID, locked.all, candidates,
	)
	return result, err
}

func captureAuthorityCandidates(ctx context.Context, tx *sql.Tx, channelIDs []string, onlyUserID *string) (ChannelKeyCandidates, error) {
	if onlyUserID == nil {
		return CaptureChannelKeyCandidatesTx(ctx, tx, channelIDs, maxChannelAuthorityChannels)
	}
	candidates := make(ChannelKeyCandidates, len(channelIDs))
	if len(channelIDs) == 0 {
		return candidates, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT channel_id FROM channel_keys
		WHERE user_id = $1 AND channel_id = ANY($2::uuid[])
		UNION
		SELECT channel_id FROM pending_key_requests
		WHERE user_id = $1 AND channel_id = ANY($2::uuid[])`,
		*onlyUserID, pq.Array(channelIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("list targeted durable channel key candidates: %w", err)
	}
	for rows.Next() {
		var channelID string
		if err := rows.Scan(&channelID); err != nil {
			return nil, fmt.Errorf("scan targeted durable channel key candidate: %w", errors.Join(err, rows.Close()))
		}
		candidates[channelID] = []string{*onlyUserID}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate targeted durable channel key candidates: %w", errors.Join(err, rows.Close()))
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close targeted durable channel key candidates: %w", err)
	}
	return candidates, nil
}

// FencePostStateLossTx is the tx-local half of the shared VIEW-revocation
// coordinator. Callers hold the server visibility lock and every channel row
// in channelIDs in canonical order, reject any lifecycle-owned temporary row,
// then pass their authority DML as write. The helper captures the bounded
// durable cleanup scope before DML, filters it against the fresh post-write
// VIEW state, purges losses, and always writes one successor epoch per channel.
func (h *Handler) FencePostStateLossTx(
	ctx context.Context,
	tx *sql.Tx,
	serverID, actorID string,
	channelIDs []string,
	write func() error,
) ([]keyrotation.Rotation, map[string][]string, error) {
	candidates, err := CaptureChannelKeyCandidatesTx(ctx, tx, channelIDs, maxChannelAuthorityChannels)
	if err != nil {
		return nil, nil, err
	}
	if err := write(); err != nil {
		return nil, nil, err
	}
	return h.RevokeDeniedChannelKeyCandidatesTx(ctx, tx, serverID, actorID, channelIDs, candidates)
}

// FenceKnownLossTx is the tx-local coordinator for a caller that already
// knows a subject lost channel VIEW (for example member removal). It does not
// enumerate server members: it purges only the named bounded subject and still
// advances one epoch per channel to fence retained local CSKs.
func (h *Handler) FenceKnownLossTx(
	ctx context.Context,
	tx *sql.Tx,
	serverID, actorID, deniedUserID string,
	channelIDs []string,
) ([]keyrotation.Rotation, map[string][]string, error) {
	candidates := make(ChannelKeyCandidates, len(channelIDs))
	for _, channelID := range channelIDs {
		candidates[channelID] = []string{deniedUserID}
	}
	return h.RevokeDeniedChannelKeyCandidatesTx(ctx, tx, serverID, actorID, channelIDs, candidates)
}

// ChannelKeyCandidates is the bounded durable-recipient set captured after
// the caller locks every affected channel row. It is cleanup scope, not the
// confidentiality boundary: unconditional epoch rotation fences every old CSK
// even if a legacy pending-enrollment writer races this snapshot, and fresh
// VIEW checks fence every successor-key distribution.
type ChannelKeyCandidates map[string][]string

// CaptureChannelKeyCandidatesTx probes each affected channel's indexed durable
// rows in canonical order. The physical-row budget bounds opportunistic
// cleanup only; rotation remains unconditional for every affected channel,
// so an oversized legacy key history cannot turn into a write-refusal DoS.
func CaptureChannelKeyCandidatesTx(
	ctx context.Context,
	tx *sql.Tx,
	channelIDs []string,
	maxCandidates int,
) (ChannelKeyCandidates, error) {
	candidates := make(ChannelKeyCandidates, len(channelIDs))
	if len(channelIDs) == 0 {
		return candidates, nil
	}
	orderedChannelIDs := append([]string(nil), channelIDs...)
	sort.Strings(orderedChannelIDs)
	remaining := maxCandidates
	queries := []string{
		`SELECT user_id FROM channel_keys WHERE channel_id = $1 LIMIT $2`,
		`SELECT user_id FROM pending_key_requests WHERE channel_id = $1 LIMIT $2`,
	}
	for _, channelID := range orderedChannelIDs {
		rawUserIDs := make([]string, 0)
		for _, query := range queries {
			if remaining == 0 {
				break
			}
			userIDs, physicalRows, err := captureChannelKeyCandidateRows(ctx, tx, query, channelID, remaining)
			if err != nil {
				return nil, err
			}
			rawUserIDs = append(rawUserIDs, userIDs...)
			remaining -= physicalRows
		}
		candidates[channelID] = dedupeAuthorityCandidateIDs(rawUserIDs)
	}
	return candidates, nil
}

func captureChannelKeyCandidateRows(ctx context.Context, tx *sql.Tx, query, channelID string, remaining int) ([]string, int, error) {
	rows, err := tx.QueryContext(ctx, query, channelID, remaining)
	if err != nil {
		return nil, 0, fmt.Errorf("list durable channel key candidates: %w", err)
	}
	userIDs := make([]string, 0)
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, 0, fmt.Errorf("scan durable channel key candidate: %w", errors.Join(err, rows.Close()))
		}
		userIDs = append(userIDs, userID)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate durable channel key candidates: %w", errors.Join(err, rows.Close()))
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("close durable channel key candidates: %w", err)
	}
	return userIDs, len(userIDs), nil
}

func dedupeAuthorityCandidateIDs(userIDs []string) []string {
	seen := make(map[string]struct{}, len(userIDs))
	unique := make([]string, 0, len(userIDs))
	for _, userID := range userIDs {
		if _, duplicate := seen[userID]; duplicate {
			continue
		}
		seen[userID] = struct{}{}
		unique = append(unique, userID)
	}
	return unique
}

// RevokeDeniedChannelKeyCandidatesTx rotates every changed synchronized
// channel exactly once, and purges only the bounded durable recipients that
// fail a fresh post-mutation VIEW check.
func (h *Handler) RevokeDeniedChannelKeyCandidatesTx(
	ctx context.Context,
	tx *sql.Tx,
	serverID, actorID string,
	channelIDs []string,
	candidates ChannelKeyCandidates,
) ([]keyrotation.Rotation, map[string][]string, error) {
	rotations := make([]keyrotation.Rotation, 0, len(channelIDs))
	deniedByChannel := make(map[string][]string)
	for _, channelID := range channelIDs {
		candidateUserIDs := candidates[channelID]
		visibleUserIDs, err := h.resolver.FilterVisibleUserIDsForChannelTx(
			ctx, tx, serverID, channelID, candidateUserIDs,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("filter durable channel key candidates: %w", err)
		}
		visible := make(map[string]struct{}, len(visibleUserIDs))
		for _, userID := range visibleUserIDs {
			visible[userID] = struct{}{}
		}
		deniedUserIDs := make([]string, 0, len(candidateUserIDs))
		for _, userID := range candidateUserIDs {
			if _, canView := visible[userID]; !canView {
				deniedUserIDs = append(deniedUserIDs, userID)
			}
		}
		rotation, err := keyrotation.RecordKeyRevocationAndPurgeUsersTx(
			ctx, tx, h.resolver.CanDistributeChannelKeyTx, channelID,
			"permission_revocation", actorID, deniedUserIDs,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("revoke topology channel key candidates: %w", err)
		}
		if rotation != nil {
			rotations = append(rotations, *rotation)
		}
		recordDeniedChannelKeyCandidates(deniedByChannel, channelID, deniedUserIDs)
	}
	return rotations, deniedByChannel, nil
}

func recordDeniedChannelKeyCandidates(deniedByChannel map[string][]string, channelID string, deniedUserIDs []string) {
	if len(deniedUserIDs) > 0 {
		deniedByChannel[channelID] = deniedUserIDs
	}
}

// CompleteChannelAuthorityMutationWithRotations publishes only confirmed
// topology commits, after cache, subscription, voice, and presence effects.
func (h *Handler) CompleteChannelAuthorityMutationWithRotations(
	ctx context.Context,
	serverID string,
	channelIDs []string,
	plan PresenceRecheckPlan,
	rotations []keyrotation.Rotation,
	deniedByChannel map[string][]string,
) {
	safeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	h.invalidateSyncedChannelCaches(safeCtx, serverID, channelIDs)
	h.presenceExecute(plan)
	if len(rotations) == 0 {
		return
	}
	rotator := keyrotation.NewContextRotator(
		h.db, h.log, h.resolver.CanDistributeChannelKeyTx,
		websocket.KeyRevocationContextBroadcaster(h.hub),
	)
	for _, rotation := range rotations {
		if err := rotator.BroadcastContext(safeCtx, rotation); err != nil {
			h.log.Error("topology key revocation delivery failed", "failure_class", "key_revocation_delivery")
		}
	}
	if h.hub == nil {
		return
	}
	for channelID, userIDs := range deniedByChannel {
		for _, userID := range userIDs {
			userUUID, err := uuid.Parse(userID)
			if err != nil {
				continue
			}
			if !h.hub.BroadcastToUserContext(safeCtx, userUUID, websocket.OutgoingMessage{
				Type: "channel_access_revoked",
				Data: map[string]interface{}{
					"channel_id": channelID,
					"server_id":  serverID,
					"reason":     "permission_revocation",
				},
			}) {
				h.log.Error("topology channel access revocation delivery failed", "failure_class", "channel_access_revoked_delivery")
			}
		}
	}
}
