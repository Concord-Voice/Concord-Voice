package voice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// natsSubjectEnforcePermissions carries a recomputed effective-permission
// bitfield for a voice-connected member whose RBAC state changed mid-session
// (CV-CAN-007 review P1). Payload: {channelId, userId, permissions} where
// permissions is the decimal-string bitfield — the same wire format the
// join-authorize response uses, parsed by the media-plane's strict fail-closed
// parser. The media-plane replaces the participant's join-time snapshot and
// closes producers whose required publish bit was revoked.
const natsSubjectEnforcePermissions = "voice.enforce.permissions"

// recheckTimeout bounds a single enforcement operation: the participant
// enumeration query in dispatch, and each per-participant resolve+publish in
// recheckOne. recheckOne derives its OWN deadline (started only after it
// acquires publishMu) rather than sharing one budget across the whole sweep, so
// a large live audience cannot exhaust a single timeout and leave later
// participants on their stale join-time snapshot (resolve would otherwise fail
// with context deadline exceeded and publish nothing). The push must outlive the
// originating HTTP request (whose context dies at response time), so enforcement
// runs on the enforcer's process-lifecycle context with this explicit ceiling.
const recheckTimeout = 10 * time.Second

type voiceEnforcementPublisher interface {
	Publish(subject string, data interface{}) error
}

// PermissionEnforcer pushes recomputed effective voice permissions to the
// media-plane when an RBAC mutation changes them for a member who is currently
// in a voice room. It closes the CV-CAN-007 review-P1 gap: the media-plane
// produce() gate keys on a permission snapshot captured at join, so without
// this push a mid-session Speak/Video/ScreenShare revocation would not bind
// until the member left and rejoined.
//
// Enforcement is best-effort push layered over the join-time gate: a transient
// resolve or publish failure logs and skips (degrading to the pre-push,
// join-snapshot behavior — it never strips permissions on a DB blip), while a
// definitive rbac.ErrNotMember publishes voice.enforce.disconnect so a member
// removed from the server cannot linger in the room. Grants propagate too: the
// media-plane snapshot is replaced, not intersected, so a newly granted member
// can publish without rejoining.
type PermissionEnforcer struct {
	db       *sql.DB
	log      *logger.Logger
	resolver *rbac.Resolver
	nats     voiceEnforcementPublisher
	// This is the enforcer-owned process lifecycle, not a request context. Close
	// cancels it and joins every worker; passing request contexts would cancel
	// fire-and-forget enforcement as soon as the originating handler returns.
	ctx         context.Context // NOSONAR -- intentional service lifecycle ownership
	cancel      context.CancelFunc
	workers     sync.WaitGroup
	lifecycleMu sync.Mutex
	closed      bool
	closeHooks  []func()
	// publishMu serializes recheckOne (resolve + publish) across the
	// goroutine-per-mutation dispatches. Without it, a recheck that RESOLVED
	// before a later mutation could PUBLISH after that mutation's recheck,
	// re-raising a just-revoked bitfield (last-write-wins fail-open). With
	// resolve+publish atomic, publish order equals resolve order, and every
	// resolve reads committed DB state — so the last message the media-plane
	// applies always carries the freshest resolve. (Single-instance guarantee;
	// a multi-instance control-plane would need a cross-instance sequence.)
	publishMu sync.Mutex
	// participantRecheckPending coalesces authoritative heartbeat sweeps by
	// channel. One global keyed-FIFO drain processes the latest exact set for
	// each pending channel; replacing a pending key keeps its place, while a
	// channel requeued during its own run joins the tail behind existing work.
	// This matches publishMu's global serialization without creating one detached
	// goroutine per participant or heartbeat.
	participantRecheckMu      sync.Mutex
	participantRecheckPending map[string]participantRecheckBatch
	participantRecheckOrder   []string
	participantRecheckRunning bool
}

type participantRecheckBatch struct {
	serverID       string
	channelID      string
	participantIDs []uuid.UUID
}

// NewPermissionEnforcer constructs the enforcer. nats may be nil (NATS-less
// dev environments); every method degrades to a no-op in that case.
func NewPermissionEnforcer(db *sql.DB, log *logger.Logger, resolver *rbac.Resolver, nats *natsclient.Client) *PermissionEnforcer {
	var publisher voiceEnforcementPublisher
	if nats != nil {
		publisher = nats
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PermissionEnforcer{
		db: db, log: log, resolver: resolver, nats: publisher,
		ctx: ctx, cancel: cancel,
	}
}

// Close cancels and joins all asynchronous permission rechecks. Call it before
// closing the database or NATS client used by the enforcer.
func (e *PermissionEnforcer) Close() {
	if e == nil {
		return
	}
	e.lifecycleMu.Lock()
	var closeHooks []func()
	if !e.closed {
		e.closed = true
		if e.cancel != nil {
			e.cancel()
		}
		closeHooks = append(closeHooks, e.closeHooks...)
		e.closeHooks = nil
	}
	e.lifecycleMu.Unlock()
	e.workers.Wait()
	for _, closeHook := range closeHooks {
		closeHook()
	}
}

// AddCloseHook binds another background component to the enforcer's existing
// server-shutdown lifecycle. Hooks added after shutdown run immediately.
func (e *PermissionEnforcer) AddCloseHook(closeHook func()) {
	if e == nil || closeHook == nil {
		return
	}
	e.lifecycleMu.Lock()
	if e.closed {
		e.lifecycleMu.Unlock()
		closeHook()
		return
	}
	e.closeHooks = append(e.closeHooks, closeHook)
	e.lifecycleMu.Unlock()
}

func (e *PermissionEnforcer) startWorker(fn func(context.Context)) {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	if e.closed || e.ctx == nil {
		return
	}
	e.workers.Add(1)
	go func(ctx context.Context) {
		defer e.workers.Done()
		fn(ctx)
	}(e.ctx)
}

// RecheckUser re-resolves and pushes permissions for one member's current
// voice channel(s) in the server. Fire-and-forget: returns immediately.
// Scope match for PermissionCache.Invalidate (role assign/unassign).
func (e *PermissionEnforcer) RecheckUser(serverID, userID string) {
	e.dispatch(func(ctx context.Context) { e.recheckUserSync(ctx, serverID, userID) })
}

// recheckUserSync is the synchronous body of RecheckUser, kept separate from the
// async dispatch wrapper.
func (e *PermissionEnforcer) recheckUserSync(ctx context.Context, serverID, userID string) {
	queryCtx, cancel := context.WithTimeout(ctx, recheckTimeout)
	channelIDs := e.voicePresenceChannelIDs(queryCtx, serverID, userID,
		"Failed to query voice presence for permission recheck")
	cancel()
	for _, channelID := range channelIDs {
		e.recheckOne(ctx, serverID, channelID, userID)
	}
}

// voicePresenceChannelIDs returns the channel IDs within serverID where
// userID is currently a voice participant. Any query/scan/iteration error is
// logged (query failures with the caller-supplied context message) and yields
// nil — callers treat nil as nothing-to-do, preserving the pre-extraction
// fail-safe posture where a mid-scan error performs NO per-channel actions.
func (e *PermissionEnforcer) voicePresenceChannelIDs(ctx context.Context, serverID, userID, queryErrMsg string) []string {
	rows, err := e.db.QueryContext(ctx, `
		SELECT vp.channel_id FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		WHERE c.server_id = $1 AND vp.user_id = $2`, serverID, userID)
	if err != nil {
		e.log.Error(queryErrMsg, "error", err,
			"server_id", sanitizeLogValue(serverID), "user_id", sanitizeLogValue(userID))
		return nil
	}
	defer rows.Close() //nolint:errcheck
	var channelIDs []string
	for rows.Next() {
		var channelID string
		if err := rows.Scan(&channelID); err != nil {
			e.log.Error("Failed to scan voice presence row", "error", err)
			return nil
		}
		channelIDs = append(channelIDs, channelID)
	}
	if err := rows.Err(); err != nil {
		e.log.Error("Failed to iterate voice presence rows", "error", err)
		return nil
	}
	return channelIDs
}

// RecheckParticipant re-resolves and pushes permissions for one member in one
// voice channel, deriving the server from the channel row. Called by the
// voice.joined NATS bridge after it records presence, closing the join-race
// window: a member whose join-authorize resolved BEFORE a mutation but whose
// voice_participants row landed AFTER the mutation's sweep would otherwise
// hold a stale snapshot that no recheck covers. A DM voice join is a
// structural no-op (its conversation id has no channels row).
func (e *PermissionEnforcer) RecheckParticipant(channelID, userID string) {
	e.dispatch(func(ctx context.Context) {
		queryCtx, cancel := context.WithTimeout(ctx, recheckTimeout)
		defer cancel()
		var serverID string
		err := e.db.QueryRowContext(queryCtx,
			`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID)
		if errors.Is(err, sql.ErrNoRows) {
			return // DM voice (no channels row) — no server permission model
		}
		if err != nil {
			e.log.Error("Failed to resolve channel server for permission recheck", "error", err,
				"channel_id", sanitizeLogValue(channelID))
			return
		}
		// Timeout backstop (join-vs-timeout race): AuthorizeJoin rejects
		// timed-out members, but a join authorized just BEFORE TimeoutMember
		// committed lands its voice.joined after the timeout's disconnect sweep
		// queried presence — and a timeout changes neither membership nor the
		// bitfield, so the fresh re-resolve below would re-push valid
		// permissions and leave the member in the room for the timeout's
		// duration. Kick/ban need no equivalent: their joins hit ErrNotMember
		// below and disconnect. Fail-safe: a query error skips the backstop
		// (never evict on a DB blip); a missing membership row falls through to
		// recheckOne, whose ErrNotMember branch disconnects.
		var timedOut bool
		if err := e.db.QueryRowContext(queryCtx, `
			SELECT timed_out_until IS NOT NULL AND timed_out_until > NOW()
			FROM server_members WHERE server_id = $1 AND user_id = $2`,
			serverID, userID).Scan(&timedOut); err == nil && timedOut {
			e.publishMu.Lock()
			e.publishDisconnect(channelID, userID)
			e.publishMu.Unlock()
			return
		}
		cancel()
		e.recheckOne(ctx, serverID, channelID, userID)
	})
}

// RecheckParticipants re-resolves one authoritative heartbeat participant set
// without launching a goroutine per user. Calls for the same channel coalesce
// to the latest exact set while one global worker drains channels serially.
// The caller supplies the already-resolved server/channel pair, avoiding 1,000
// duplicate channel lookups for a full room.
func (e *PermissionEnforcer) RecheckParticipants(
	serverID, channelID string,
	participantIDs []uuid.UUID,
) {
	if e == nil || e.db == nil || e.nats == nil || serverID == "" || channelID == "" ||
		len(participantIDs) == 0 || len(participantIDs) > maxServerVoiceParticipantIDs {
		return
	}
	ordered := append([]uuid.UUID(nil), participantIDs...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].String() < ordered[right].String()
	})
	for index, participantID := range ordered {
		if participantID == uuid.Nil ||
			(index > 0 && participantID == ordered[index-1]) {
			return
		}
	}
	batch := participantRecheckBatch{
		serverID: serverID, channelID: channelID, participantIDs: ordered,
	}
	e.lifecycleMu.Lock()
	if e.closed || e.ctx == nil {
		e.lifecycleMu.Unlock()
		return
	}
	e.participantRecheckMu.Lock()
	if e.participantRecheckPending == nil {
		e.participantRecheckPending = make(map[string]participantRecheckBatch)
	}
	if _, pending := e.participantRecheckPending[channelID]; !pending {
		e.participantRecheckOrder = append(e.participantRecheckOrder, channelID)
	}
	e.participantRecheckPending[channelID] = batch
	if e.participantRecheckRunning {
		e.participantRecheckMu.Unlock()
		e.lifecycleMu.Unlock()
		return
	}
	e.participantRecheckRunning = true
	e.workers.Add(1)
	ctx := e.ctx
	e.participantRecheckMu.Unlock()
	e.lifecycleMu.Unlock()
	go func() {
		defer e.workers.Done()
		e.drainParticipantRechecks(ctx)
	}()
}

func (e *PermissionEnforcer) drainParticipantRechecks(ctx context.Context) {
	for {
		e.participantRecheckMu.Lock()
		if ctx.Err() != nil {
			e.participantRecheckPending = nil
			e.participantRecheckOrder = nil
			e.participantRecheckRunning = false
			e.participantRecheckMu.Unlock()
			return
		}
		if len(e.participantRecheckOrder) == 0 {
			e.participantRecheckRunning = false
			e.participantRecheckOrder = nil
			e.participantRecheckMu.Unlock()
			return
		}
		channelID := e.participantRecheckOrder[0]
		e.participantRecheckOrder[0] = ""
		e.participantRecheckOrder = e.participantRecheckOrder[1:]
		batch := e.participantRecheckPending[channelID]
		delete(e.participantRecheckPending, channelID)
		e.participantRecheckMu.Unlock()
		e.recheckParticipantBatch(ctx, batch)
	}
}

func (e *PermissionEnforcer) recheckParticipantBatch(ctx context.Context, batch participantRecheckBatch) {
	batchCtx, cancel := context.WithTimeout(ctx, recheckTimeout)
	timedOut, err := e.timedOutParticipantIDs(batchCtx, batch.serverID, batch.participantIDs)
	cancel()
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			e.log.Error("Failed to query heartbeat participant timeouts", "error", err)
		}
		return
	}
	for _, participantID := range batch.participantIDs {
		if ctx.Err() != nil {
			return
		}
		if timedOut[participantID] {
			e.recheckInitiallyTimedOutParticipant(ctx, batch, participantID)
		}
	}
	for _, participantID := range batch.participantIDs {
		if ctx.Err() != nil {
			return
		}
		if timedOut[participantID] {
			continue
		}
		e.recheckOne(ctx, batch.serverID, batch.channelID, participantID.String())
	}
}

func (e *PermissionEnforcer) recheckInitiallyTimedOutParticipant(
	ctx context.Context,
	batch participantRecheckBatch,
	participantID uuid.UUID,
) {
	e.publishMu.Lock()
	defer e.publishMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()
	userID := participantID.String()
	var stillTimedOut bool
	err := e.db.QueryRowContext(ctx, `
		SELECT timed_out_until IS NOT NULL AND timed_out_until > NOW()
		FROM server_members
		WHERE server_id = $1 AND user_id = $2
	`, batch.serverID, userID).Scan(&stillTimedOut)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		if !errors.Is(err, context.Canceled) {
			e.log.Error("Failed to revalidate heartbeat participant timeout", "error", err)
		}
		return
	}
	if stillTimedOut {
		e.publishDisconnect(batch.channelID, userID)
		return
	}
	e.recheckOneLocked(ctx, batch.serverID, batch.channelID, userID)
}

func (e *PermissionEnforcer) timedOutParticipantIDs(
	ctx context.Context,
	serverID string,
	participantIDs []uuid.UUID,
) (map[uuid.UUID]bool, error) {
	rawParticipantIDs := make([]string, 0, len(participantIDs))
	for _, participantID := range participantIDs {
		rawParticipantIDs = append(rawParticipantIDs, participantID.String())
	}
	rows, err := e.db.QueryContext(ctx, `
		SELECT user_id
		FROM server_members
		WHERE server_id = $1
		  AND user_id = ANY($2::uuid[])
		  AND timed_out_until IS NOT NULL
		  AND timed_out_until > NOW()
	`, serverID, pq.Array(rawParticipantIDs))
	if err != nil {
		return nil, fmt.Errorf("query heartbeat participant timeouts: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	timedOut := make(map[uuid.UUID]bool)
	for rows.Next() {
		var participantID uuid.UUID
		if err := rows.Scan(&participantID); err != nil {
			return nil, fmt.Errorf("scan heartbeat participant timeout: %w", err)
		}
		timedOut[participantID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate heartbeat participant timeouts: %w", err)
	}
	return timedOut, nil
}

// RecheckChannel re-resolves and pushes permissions for every member currently
// in the given voice channel. Scope match for PermissionCache.InvalidateChannel
// (channel override upsert/delete, permission-sync rebuild).
func (e *PermissionEnforcer) RecheckChannel(serverID, channelID string) {
	e.dispatch(func(ctx context.Context) { e.recheckChannelSync(ctx, serverID, channelID) })
}

// recheckChannelSync is the synchronous body of RecheckChannel, kept separate from
// the async dispatch wrapper.
func (e *PermissionEnforcer) recheckChannelSync(ctx context.Context, serverID, channelID string) {
	queryCtx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()
	rows, err := e.db.QueryContext(queryCtx,
		`SELECT user_id FROM voice_participants WHERE channel_id = $1`, channelID)
	if err != nil {
		e.log.Error("Failed to query voice participants for permission recheck", "error", err,
			"channel_id", sanitizeLogValue(channelID))
		return
	}
	defer rows.Close() //nolint:errcheck
	var userIDs []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			e.log.Error("Failed to scan voice participant row", "error", err)
			return
		}
		userIDs = append(userIDs, userID)
	}
	if err := rows.Err(); err != nil {
		e.log.Error("Failed to iterate voice participant rows", "error", err)
		return
	}
	cancel()
	for _, userID := range userIDs {
		e.recheckOne(ctx, serverID, channelID, userID)
	}
}

// RecheckServer re-resolves and pushes permissions for every member currently
// in any voice channel of the server. Scope match for
// PermissionCache.InvalidateServer (role permission edit/delete, category
// override cascades). Cost is bounded by live voice participation, and the
// triggering mutations are rare, moderator-authenticated actions.
func (e *PermissionEnforcer) RecheckServer(serverID string) {
	e.dispatch(func(ctx context.Context) { e.recheckServerSync(ctx, serverID) })
}

// recheckServerSync is the synchronous body of RecheckServer, kept separate from
// the async dispatch wrapper.
func (e *PermissionEnforcer) recheckServerSync(ctx context.Context, serverID string) {
	queryCtx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()
	rows, err := e.db.QueryContext(queryCtx, `
		SELECT vp.channel_id, vp.user_id FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		WHERE c.server_id = $1`, serverID)
	if err != nil {
		e.log.Error("Failed to query server voice participants for permission recheck", "error", err,
			"server_id", sanitizeLogValue(serverID))
		return
	}
	defer rows.Close() //nolint:errcheck
	type pair struct{ channelID, userID string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.channelID, &p.userID); err != nil {
			e.log.Error("Failed to scan server voice participant row", "error", err)
			return
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		e.log.Error("Failed to iterate server voice participant rows", "error", err)
		return
	}
	cancel()
	for _, p := range pairs {
		e.recheckOne(ctx, serverID, p.channelID, p.userID)
	}
}

// dispatch runs fn on the enforcer lifecycle context so the push survives the
// originating request but is canceled and joined by Close.
func (e *PermissionEnforcer) dispatch(fn func(ctx context.Context)) {
	if e == nil || e.nats == nil {
		return
	}
	e.startWorker(func(ctx context.Context) {
		fn(ctx)
	})
}

// recheckOne recomputes one (channel, user) effective bitfield and publishes
// the enforcement message. The RBAC mutation handlers invalidate the
// permission cache before calling the enforcer, so this resolve computes from
// post-mutation state.
func (e *PermissionEnforcer) recheckOne(ctx context.Context, serverID, channelID, userID string) {
	e.publishMu.Lock()
	defer e.publishMu.Unlock()
	e.recheckOneLocked(ctx, serverID, channelID, userID)
}

// recheckOneLocked requires publishMu. Keeping fresh resolve and publish in one
// critical section guarantees that the last applied bitfield is the freshest.
func (e *PermissionEnforcer) recheckOneLocked(ctx context.Context, serverID, channelID, userID string) {
	// Own per-participant deadline, started only after acquiring publishMu, so a
	// large sweep (participants serialized on the mutex) cannot drain one shared
	// budget and skip later participants. The parent is the enforcer lifecycle,
	// not the originating request or the batch-enumeration deadline.
	ctx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()
	// Fresh (cache-READ-bypassing) resolve: an in-flight pre-mutation compute
	// can repopulate the cache after the mutation invalidated it; publishing
	// that stale entry would be sticky fail-open on the media-plane snapshot.
	perms, err := e.resolver.ResolveEffectivePermissionsFresh(ctx, serverID, userID, channelID)
	if errors.Is(err, rbac.ErrNotMember) {
		// Definitive: no longer a server member — evict from the room (mirrors
		// tempGrantManager.publishForceDisconnect, ADR-0023).
		e.publishDisconnect(channelID, userID)
		return
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		// A mutation-triggered recheck cannot safely retain the join-time
		// snapshot when fresh authorization is unavailable. Fail closed for this
		// known live participant; a later authorized join can reconnect normally.
		e.log.Error("Failed to re-resolve voice permissions; disconnecting participant", "error", err,
			"server_id", sanitizeLogValue(serverID), "channel_id", sanitizeLogValue(channelID),
			"user_id", sanitizeLogValue(userID))
		e.publishDisconnect(channelID, userID)
		return
	}
	if err := e.nats.Publish(natsSubjectEnforcePermissions, map[string]interface{}{
		"channelId":   channelID,
		"userId":      userID,
		"permissions": strconv.FormatInt(int64(perms), 10),
	}); err != nil {
		e.log.Error("Failed to publish permission recheck", "error", err,
			"subject", natsSubjectEnforcePermissions,
			"channel_id", sanitizeLogValue(channelID), "user_id", sanitizeLogValue(userID))
	}
}

// publishDisconnect publishes a voice.enforce.disconnect for one (channel, user)
// so the media plane closes that peer's transports and removes it from the room.
// Shared by recheckOne's membership-loss branch and DisconnectUser so the
// force-disconnect publish primitive lives in one place.
func (e *PermissionEnforcer) publishDisconnect(channelID, userID string) {
	if err := e.nats.Publish(natsSubjectEnforceDisconnect, map[string]interface{}{
		"channelId": channelID, "userId": userID,
	}); err != nil {
		e.log.Error("Failed to publish force-disconnect", "error", err,
			"channel_id", sanitizeLogValue(channelID), "user_id", sanitizeLogValue(userID))
	}
}

// DisconnectUser force-disconnects one member from every voice channel they
// currently occupy in the server. Unlike RecheckUser (which re-resolves the
// effective bitfield and pushes it), this unconditionally publishes
// voice.enforce.disconnect: it is the member-timeout path (CV-CAN-007 review
// P1). AuthorizeJoin bars a timed-out member from voice via
// server_members.timed_out_until, a check independent of the permission
// bitfield, so a permission recheck would re-resolve their unchanged bits and
// never evict them — leaving a timed-out member live in the room. Fire-and-
// forget: returns immediately, and a NATS-less enforcer is a no-op.
func (e *PermissionEnforcer) DisconnectUser(serverID, userID string) {
	e.dispatch(func(ctx context.Context) { e.disconnectUserSync(ctx, serverID, userID) })
}

// disconnectUserSync is the synchronous body of DisconnectUser, kept separate from
// the async dispatch wrapper.
func (e *PermissionEnforcer) disconnectUserSync(ctx context.Context, serverID, userID string) {
	queryCtx, cancel := context.WithTimeout(ctx, recheckTimeout)
	channelIDs := e.voicePresenceChannelIDs(queryCtx, serverID, userID,
		"Failed to query voice presence for timeout disconnect")
	cancel()
	for _, channelID := range channelIDs {
		e.publishDisconnect(channelID, userID)
	}
}
