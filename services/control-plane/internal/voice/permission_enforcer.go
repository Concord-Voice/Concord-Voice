package voice

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
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
// runs on context.Background with this explicit ceiling instead.
const recheckTimeout = 10 * time.Second

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
	nats     *natsclient.Client
	// publishMu serializes recheckOne (resolve + publish) across the
	// goroutine-per-mutation dispatches. Without it, a recheck that RESOLVED
	// before a later mutation could PUBLISH after that mutation's recheck,
	// re-raising a just-revoked bitfield (last-write-wins fail-open). With
	// resolve+publish atomic, publish order equals resolve order, and every
	// resolve reads committed DB state — so the last message the media-plane
	// applies always carries the freshest resolve. (Single-instance guarantee;
	// a multi-instance control-plane would need a cross-instance sequence.)
	publishMu sync.Mutex
}

// NewPermissionEnforcer constructs the enforcer. nats may be nil (NATS-less
// dev environments); every method degrades to a no-op in that case.
func NewPermissionEnforcer(db *sql.DB, log *logger.Logger, resolver *rbac.Resolver, nats *natsclient.Client) *PermissionEnforcer {
	return &PermissionEnforcer{db: db, log: log, resolver: resolver, nats: nats}
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
	channelIDs := e.voicePresenceChannelIDs(ctx, serverID, userID,
		"Failed to query voice presence for permission recheck")
	for _, channelID := range channelIDs {
		e.recheckOne(serverID, channelID, userID)
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
		var serverID string
		err := e.db.QueryRowContext(ctx,
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
		if err := e.db.QueryRowContext(ctx, `
			SELECT timed_out_until IS NOT NULL AND timed_out_until > NOW()
			FROM server_members WHERE server_id = $1 AND user_id = $2`,
			serverID, userID).Scan(&timedOut); err == nil && timedOut {
			e.publishMu.Lock()
			e.publishDisconnect(channelID, userID)
			e.publishMu.Unlock()
			return
		}
		e.recheckOne(serverID, channelID, userID)
	})
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
	rows, err := e.db.QueryContext(ctx,
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
	for _, userID := range userIDs {
		e.recheckOne(serverID, channelID, userID)
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
	rows, err := e.db.QueryContext(ctx, `
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
	for _, p := range pairs {
		e.recheckOne(serverID, p.channelID, p.userID)
	}
}

// dispatch runs fn on a background context so the push survives the
// originating request. No-op when the enforcer or its NATS client is absent.
func (e *PermissionEnforcer) dispatch(fn func(ctx context.Context)) {
	if e == nil || e.nats == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), recheckTimeout)
		defer cancel()
		fn(ctx)
	}()
}

// recheckOne recomputes one (channel, user) effective bitfield and publishes
// the enforcement message. The RBAC mutation handlers invalidate the
// permission cache before calling the enforcer, so this resolve computes from
// post-mutation state.
func (e *PermissionEnforcer) recheckOne(serverID, channelID, userID string) {
	e.publishMu.Lock()
	defer e.publishMu.Unlock()
	// Own per-participant deadline, started only after acquiring publishMu, so a
	// large sweep (participants serialized on the mutex) cannot drain one shared
	// budget and skip later participants. Detached from the caller's context: a
	// finished HTTP request or an exhausted enumeration deadline must not cancel
	// the per-participant resolve.
	ctx, cancel := context.WithTimeout(context.Background(), recheckTimeout)
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
		// Transient resolve failure: skip. The member keeps the join-time
		// snapshot (pre-push behavior) rather than being stripped on a DB blip.
		e.log.Error("Failed to re-resolve voice permissions; skipping push", "error", err,
			"server_id", sanitizeLogValue(serverID), "channel_id", sanitizeLogValue(channelID),
			"user_id", sanitizeLogValue(userID))
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
	channelIDs := e.voicePresenceChannelIDs(ctx, serverID, userID,
		"Failed to query voice presence for timeout disconnect")
	for _, channelID := range channelIDs {
		e.publishDisconnect(channelID, userID)
	}
}
