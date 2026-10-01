package messages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
)

const errMsgPurgeFailed = "Purge failed"

var errPurgeMemberTimedOut = errors.New("purge actor is timed out")

type channelScope struct {
	id          string
	channelType string
}

// serverPurgeRequest keeps the shared HTTP and moderation purge inputs paired.
// The credential epoch remains optional because internal moderation has no bearer token.
type serverPurgeRequest struct {
	serverID        string
	actorID         string
	target          *string
	reason          string
	rangeFrom       *time.Time
	rangeLabel      string
	credentialEpoch *string
	// admit is the plan's Plan.Admit: an over-threshold self-purge's
	// confirmation on the HTTP route, nil on the ban/kick path.
	admit func(context.Context, *sql.Tx) error
}

// purgeRequest is the shared body for the channel and server purge endpoints (#1352).
// Range is recent-ward ("1h".."90d") or "all"; TargetUserID narrows to one author
// (requires ManageAllMessages — an actor with only ManageOwnMessages is forced to self).
//
// The embedded stepup.Fields confirm a self-purge past the delete-rate
// soft-lock (#3455): mfa_code, or the single-use step_up_token the mint
// endpoint returned for the password (#3509). They are the fields and caps of
// stepup.ReadOptionalStepUp, so the desktop sends one shape to every
// own-rule route, and a body that still carries current_password is a 400.
type purgeRequest struct {
	Range        string  `json:"range" binding:"required"`
	TargetUserID *string `json:"target_user_id"`
	stepup.Fields
}

// maxPurgeBodyBytes is the delete routes' ReadOptionalStepUp limit: the purge
// body carries the same step-up fields, and both at their caps fit with room
// to spare. Without it the body was read unbounded.
const maxPurgeBodyBytes = 4 << 10

// stepUp is the step-up bindPurgeRequest already validated through
// stepup.Fields.Input.
func (r purgeRequest) stepUp() stepup.Input {
	return stepup.Input{MFACode: r.MFACode, StepUpToken: r.StepUpToken}
}

// PurgeChannel handles DELETE /channels/:id/messages — bulk-delete a channel's
// messages, scoped by time range and optional author (#1352).
//
// Authorization (OWASP A01 — resolved ONCE, before any mutation):
//   - PermManageAllMessages → may purge any/all authors in the channel.
//   - PermManageOwnMessages only → forced to target_user = self.
//   - Neither → 403, zero rows deleted, no audit row.
func (h *Handler) PurgeChannel(c *gin.Context) {
	userID := c.GetString("user_id")
	channelID := c.Param("id")
	req, rangeFrom, ok := bindPurgeRequest(c, channelID, "Invalid channel ID")
	if !ok {
		return
	}
	purgeCtx, cancel := context.WithTimeout(c.Request.Context(), purge.SynchronousRunTimeout)
	defer cancel()

	var serverID, channelType string
	if err := h.db.QueryRowContext(purgeCtx,
		`SELECT server_id, type FROM channels WHERE id = $1`, channelID).Scan(&serverID, &channelType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found"})
			return
		}
		h.log.Error("Channel purge lookup failed", "error", err, "channel_id", channelID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
		return
	}

	author, ok, authErr := h.resolvePurgeAuthor(purgeCtx, serverID, userID, channelID, channelType, req.TargetUserID)
	if authErr != nil {
		h.log.Error("Channel purge authorization failed", "error", authErr, "channel_id", channelID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
		return
	}
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions to purge this channel"})
		return
	}

	deletes := []purge.DeleteSpec{{
		MessagesTable:    "messages",
		ScopeColumn:      "channel_id",
		ScopeID:          channelID,
		AttachmentsTable: "message_attachments",
		Author:           author,
	}}
	softLock, ok := h.gateSelfPurge(purgeCtx, c, selfPurge{
		serverID: serverID, userID: userID, purpose: stepup.PurposeChannelPurge,
		input: req.stepUp(), epoch: middleware.TokenCredentialEpoch(c),
		rangeFrom: rangeFrom, deletes: deletes, notFound: "Channel not found",
	})
	if !ok {
		return
	}
	deletes = softLock.fence(deletes, userID)

	guard := channelPurgeGuard{
		channelID: channelID, actorID: userID, epoch: middleware.TokenCredentialEpoch(c),
		author: author, target: req.TargetUserID,
	}
	plan := purge.Plan{
		ContextType: purge.ContextChannel,
		ContextID:   channelID,
		ServerID:    &serverID,
		ActorID:     userID,
		Target:      req.TargetUserID,
		Reason:      "manual",
		RangeFrom:   rangeFrom,
		Deletes:     deletes,
		Guard: func(ctx context.Context, tx *sql.Tx, ds purge.DeleteSpec) error {
			return h.guardChannelPurgeBatch(ctx, tx, guard, ds)
		},
		Admit: h.selfPurgeAdmit(softLock),
	}
	res, err := h.purgeEngine.Run(purgeCtx, plan)
	if h.refuseSelfPurge(c, softLock) {
		return
	}
	h.settleSelfPurge(purgeCtx, softLock, err)
	if h.respondChannelPurgeFailure(c, err, channelID, userID, req.Range, res) {
		return
	}

	h.log.Info("Channel purged", "channel_id", channelID, "actor", userID, "deleted", res.DeletedCount)
	h.emitChannelPurged(channelID, userID, res.DeletedCount, req.Range)
	// hidden_count is structurally 0 for server contexts — returned so the response
	// shape matches spec §4 { deleted_count, hidden_count } across ALL contexts.
	c.JSON(http.StatusOK, gin.H{"deleted_count": res.DeletedCount, "hidden_count": 0})
}

// channelPurgeGuard is what a channel purge's per-batch Guard checks each
// batch against: the actor, the token's credential epoch, and the author filter
// the plan was built with.
type channelPurgeGuard struct {
	channelID, actorID, epoch string
	author, target            *string
}

// guardChannelPurgeBatch is a channel purge's per-batch Guard. It refuses a
// batch outside the planned scope, then under the batch's transaction
// re-fences the credential epoch, locks the channel and the actor's
// membership, refuses a timed-out actor, and re-derives the author filter
// from fresh permissions, refusing the batch when it no longer matches.
func (h *Handler) guardChannelPurgeBatch(ctx context.Context, tx *sql.Tx, g channelPurgeGuard, ds purge.DeleteSpec) error {
	if ds.ScopeID != g.channelID || !samePurgeAuthor(ds.Author, g.author) {
		return errors.New("unexpected channel purge batch scope")
	}
	if err := credepoch.GuardTx(ctx, tx, g.actorID, g.epoch); err != nil {
		return err
	}
	var lockedServerID, lockedType string
	if err := tx.QueryRowContext(ctx, `SELECT server_id, type FROM channels WHERE id = $1 FOR SHARE`, g.channelID).Scan(&lockedServerID, &lockedType); err != nil {
		return err
	}
	var timedOut bool
	if err := tx.QueryRowContext(ctx, `
		SELECT timed_out_until IS NOT NULL AND timed_out_until > clock_timestamp()
		FROM server_members WHERE server_id = $1 AND user_id = $2 FOR SHARE`, lockedServerID, g.actorID,
	).Scan(&timedOut); err != nil {
		return err
	}
	if timedOut {
		return errPurgeMemberTimedOut
	}
	perms, err := h.resolver.ResolveChannelPermissionsTx(ctx, tx, lockedServerID, g.actorID, g.channelID)
	if err != nil {
		return err
	}
	freshAuthor, allowed := purgeAuthorForPermissions(perms, g.actorID, lockedType, g.target)
	if !allowed || !samePurgeAuthor(freshAuthor, g.author) {
		return errors.New("channel purge authority changed")
	}
	return nil
}

// samePurgeAuthor reports whether two author filters select the same rows:
// both absent (every author), or both present and naming the same user.
func samePurgeAuthor(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// respondChannelPurgeFailure emits the required invalidation for a partially
// completed purge and writes its HTTP failure response.
func (h *Handler) respondChannelPurgeFailure(c *gin.Context, err error, channelID, userID, rangeLabel string, res purge.Result) bool {
	if err == nil {
		return false
	}
	h.log.Error("Channel purge failed", "error", err, "channel_id", channelID)
	if res.DeletedCount > 0 {
		h.emitChannelPurged(channelID, userID, res.DeletedCount, rangeLabel)
	}
	if res.DeletedCount == 0 && errors.Is(err, errPurgeMemberTimedOut) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Member is timed out", "code": "member_timed_out"})
		return true
	}
	if res.DeletedCount == 0 && (errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked)) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return true
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
	return true
}

// PurgeStatus is the outcome of a server-scoped purge for one actor.
type PurgeStatus string

// Purge outcomes returned by purgeServerCore / PurgeUserServerMessages, plus the
// moderation-path-only PurgeSkippedRateLimited (set by the members handler before the
// engine runs, #1353).
const (
	PurgeCompleted           PurgeStatus = "completed"
	PurgeSkippedUnauthorized PurgeStatus = "skipped_unauthorized"
	PurgeSkippedRateLimited  PurgeStatus = "skipped_rate_limited"
	PurgeFailed              PurgeStatus = "failed"
)

// PurgeServer handles DELETE /servers/:id/messages — bulk-delete across a server's
// channels (#1352). It parses the range/target request, authorizes every channel,
// runs the self-purge soft-lock (#3455), and then the purge. It composes
// authorizeServerPurge and runServerPurge itself rather than calling
// purgeServerCore, because the soft-lock sits between the two and the ban/kick
// path through purgeServerCore must never reach it.
func (h *Handler) PurgeServer(c *gin.Context) {
	userID := c.GetString("user_id")
	serverID := c.Param("id")
	req, rangeFrom, ok := bindPurgeRequest(c, serverID, "Invalid server ID")
	if !ok {
		return
	}
	purgeCtx, cancel := context.WithTimeout(c.Request.Context(), purge.SynchronousRunTimeout)
	defer cancel()

	deletes, status, err := h.authorizeServerPurge(purgeCtx, serverID, userID, req.TargetUserID)
	if status != PurgeCompleted {
		h.respondServerPurge(c, serverID, userID, 0, status, err)
		return
	}
	epoch := middleware.TokenCredentialEpoch(c)
	softLock, ok := h.gateSelfPurge(purgeCtx, c, selfPurge{
		serverID: serverID, userID: userID, purpose: stepup.PurposeServerPurge,
		input: req.stepUp(), epoch: epoch,
		rangeFrom: rangeFrom, deletes: deletes, notFound: "Server not found",
	})
	if !ok {
		return
	}
	deletes = softLock.fence(deletes, userID)
	deleted, status, err := h.runServerPurge(purgeCtx, serverPurgeRequest{
		serverID: serverID, actorID: userID, target: req.TargetUserID, reason: "manual",
		rangeFrom: rangeFrom, rangeLabel: req.Range, credentialEpoch: ptr(epoch),
		admit: h.selfPurgeAdmit(softLock),
	}, deletes)
	if h.refuseSelfPurge(c, softLock) {
		return
	}
	h.settleSelfPurge(purgeCtx, softLock, err)
	h.respondServerPurge(c, serverID, userID, deleted, status, err)
}

// respondServerPurge writes PurgeServer's outcome.
func (h *Handler) respondServerPurge(c *gin.Context, serverID, userID string, deleted int, status PurgeStatus, err error) {
	switch status {
	case PurgeSkippedUnauthorized:
		c.JSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions to purge this server"})
	case PurgeFailed:
		if deleted == 0 && errors.Is(err, errPurgeMemberTimedOut) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Member is timed out", "code": "member_timed_out"})
			return
		}
		if deleted == 0 && (errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked)) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}
		h.log.Error("Server purge failed", "error", err, "server_id", serverID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
	default:
		h.log.Info("Server purged", "server_id", serverID, "actor", userID, "deleted", deleted)
		// hidden_count is structurally 0 for server contexts — returned so the response
		// shape matches spec §4 { deleted_count, hidden_count } across ALL contexts.
		c.JSON(http.StatusOK, gin.H{"deleted_count": deleted, "hidden_count": 0})
	}
}

// purgeServerCore is the ban/kick moderation path's authorization + purge across a
// server's channels (All Time, reason "ban"/"kick"). The HTTP PurgeServer endpoint
// runs the same two halves with the self-purge soft-lock between them; this path has
// none, because a moderation purge is never counted (#3455 D-1).
func (h *Handler) purgeServerCore(ctx context.Context, req serverPurgeRequest) (int, PurgeStatus, error) {
	deletes, status, err := h.authorizeServerPurge(ctx, req.serverID, req.actorID, req.target)
	if status != PurgeCompleted {
		return 0, status, err
	}
	return h.runServerPurge(ctx, req, deletes)
}

// authorizeServerPurge enumerates a server's channels and authorizes the actor in each,
// returning the delete specs a purge may run.
//
// SECURITY (review finding M1, #1352): authorization is re-resolved PER CHANNEL, never once
// at server scope. computeEffectivePermissions skips applyChannelOverrides when channelID is
// "", so a single server-scope check would bypass channel_permission_overrides rows that DENY
// ManageAllMessages (or view) on specific channels — and irreversibly delete messages the actor
// is explicitly denied access to. Channels where the actor lacks permission are SKIPPED; if no
// channel is purgeable the caller gets SkippedUnauthorized with no audit row (the guard runs
// before Engine.Run, which writes the audit). A nil error accompanies Completed and
// SkippedUnauthorized; a non-nil error accompanies Failed.
func (h *Handler) authorizeServerPurge(
	ctx context.Context, serverID, actorID string, target *string,
) ([]purge.DeleteSpec, PurgeStatus, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT id, type FROM channels WHERE server_id = $1`, serverID)
	if err != nil {
		h.log.Error("Server purge: enumerate channels failed", "error", err, "server_id", serverID)
		return nil, PurgeFailed, fmt.Errorf("enumerate server channels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var channels []channelScope
	for rows.Next() {
		var channel channelScope
		if err := rows.Scan(&channel.id, &channel.channelType); err != nil {
			return nil, PurgeFailed, fmt.Errorf("scan server channel: %w", err)
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, PurgeFailed, fmt.Errorf("iterate server channels: %w", err)
	}

	// Deliberately unwrapped: this is a pass-through of an error `serverPurgeDeletes`
	// already wrapped, and on a non-error refusal (PurgeSkippedUnauthorized) `err` is
	// nil — `fmt.Errorf` here would either double the context or fabricate one.
	return h.serverPurgeDeletes(ctx, serverID, actorID, target, channels)
}

// runServerPurge runs an authorized server purge and fans out its events. Its
// batch guard re-authorizes every channel under lock (#3142), and fences the
// credential epoch when the request carries one (the HTTP route does; the
// ban/kick path has no bearer token).
func (h *Handler) runServerPurge(ctx context.Context, req serverPurgeRequest, deletes []purge.DeleteSpec) (int, PurgeStatus, error) {
	plan := purge.Plan{
		ContextType: purge.ContextServer,
		ContextID:   req.serverID,
		ServerID:    &req.serverID,
		ActorID:     req.actorID,
		Target:      req.target,
		Reason:      req.reason,
		RangeFrom:   req.rangeFrom,
		Deletes:     deletes,
		Guard: func(guardCtx context.Context, tx *sql.Tx, ds purge.DeleteSpec) error {
			return h.guardServerPurgeBatch(guardCtx, tx, req, ds)
		},
		Admit: req.admit,
	}
	res, err := h.purgeEngine.Run(ctx, plan)
	if err != nil {
		// A failed admission on the soft-locked route is answered, and logged,
		// by PurgeServer: a soft-lock refusal must not reach a line that
		// carries server_id (C7).
		if req.admit == nil || !errors.Is(err, purge.ErrNotAdmitted) {
			h.log.Error("Server purge failed", "error", err, "server_id", req.serverID)
		}
		if res.DeletedCount > 0 {
			h.emitServerPurgeEvents(req.serverID, req.actorID, req.rangeLabel, deletes)
		}
		return res.DeletedCount, PurgeFailed, fmt.Errorf("run server purge: %w", err)
	}
	h.emitServerPurgeEvents(req.serverID, req.actorID, req.rangeLabel, deletes)
	return res.DeletedCount, PurgeCompleted, nil
}

// guardServerPurgeBatch is a server purge's per-batch Guard. Under the
// batch's transaction it re-fences the credential epoch, locks the channel
// and the actor's membership, refuses a timed-out actor, and re-derives the
// author filter from fresh permissions, refusing the batch when it no longer
// matches the one the plan was built with.
func (h *Handler) guardServerPurgeBatch(guardCtx context.Context, tx *sql.Tx, req serverPurgeRequest, ds purge.DeleteSpec) error {
	if req.credentialEpoch != nil {
		if err := credepoch.GuardTx(guardCtx, tx, req.actorID, *req.credentialEpoch); err != nil {
			return err
		}
	}
	var lockedType string
	if err := tx.QueryRowContext(guardCtx,
		`SELECT type FROM channels WHERE id = $1 AND server_id = $2 FOR SHARE`, ds.ScopeID, req.serverID,
	).Scan(&lockedType); err != nil {
		return err
	}
	var timedOut bool
	if err := tx.QueryRowContext(guardCtx,
		`SELECT timed_out_until IS NOT NULL AND timed_out_until > clock_timestamp()
		 FROM server_members WHERE server_id = $1 AND user_id = $2 FOR SHARE`, req.serverID, req.actorID,
	).Scan(&timedOut); err != nil {
		return err
	}
	if timedOut {
		return errPurgeMemberTimedOut
	}
	perms, err := h.resolver.ResolveChannelPermissionsTx(guardCtx, tx, req.serverID, req.actorID, ds.ScopeID)
	if err != nil {
		return err
	}
	freshAuthor, allowed := purgeAuthorForPermissions(perms, req.actorID, lockedType, req.target)
	if !allowed || !samePurgeAuthor(freshAuthor, ds.Author) {
		return errors.New("server purge authority changed")
	}
	return nil
}

// emitServerPurgeEvents fans out the invalidation events for a server purge that
// deleted rows. Emission runs only after the engine has committed its work, and no
// broadcast can fail the request: the hub calls return no error.
//
// One channel_purged per affected channel (spec §11), unchanged — it is what drops
// the mounted channel live. The engine returns only an aggregate count, so the
// per-channel event carries 0; clients treat the event as an invalidation signal
// and refetch (next-fetch is the correctness backstop). The single server_purged
// then covers every OTHER affected scope: a client subscribes to channels it has
// mounted, so the per-channel events alone leave locally cached plaintext (search
// index included) in every channel the recipient does not currently have open.
func (h *Handler) emitServerPurgeEvents(serverID, actorID, rangeLabel string, deletes []purge.DeleteSpec) {
	for _, ds := range deletes {
		h.emitChannelPurged(ds.ScopeID, actorID, 0, rangeLabel)
	}
	h.emitServerPurged(serverID, actorID, rangeLabel)
}

// serverPurgeDeletes authorizes every channel before constructing the purge plan.
func (h *Handler) serverPurgeDeletes(ctx context.Context, serverID, actorID string, target *string, channels []channelScope) ([]purge.DeleteSpec, PurgeStatus, error) {
	if len(channels) == 0 {
		// With no channel overrides to bypass, a server-scope authorization check
		// distinguishes a legitimate empty purge from an existence oracle.
		_, ok, err := h.resolvePurgeAuthor(ctx, serverID, actorID, "", "", target)
		if err != nil {
			return nil, PurgeFailed, fmt.Errorf("authorize empty server scope: %w", err)
		}
		if !ok {
			return nil, PurgeSkippedUnauthorized, nil
		}
	}

	channelIDs := make([]string, len(channels))
	for i, channel := range channels {
		channelIDs[i] = channel.id
	}
	permsByChannel, err := h.resolver.ResolveEffectivePermissionsForChannelsFresh(ctx, serverID, actorID, channelIDs)
	if err != nil {
		if errors.Is(err, rbac.ErrNotMember) {
			return nil, PurgeSkippedUnauthorized, nil
		}
		h.log.Error("Server purge authorization failed", "error", err, "server_id", serverID)
		return nil, PurgeFailed, fmt.Errorf("resolve channel permissions: %w", err)
	}

	var deletes []purge.DeleteSpec
	for _, channel := range channels {
		author, ok := purgeAuthorForPermissions(permsByChannel[channel.id], actorID, channel.channelType, target)
		if !ok {
			continue // denied in this channel — skip it, never delete here
		}
		deletes = append(deletes, purge.DeleteSpec{
			MessagesTable:    "messages",
			ScopeColumn:      "channel_id",
			ScopeID:          channel.id,
			AttachmentsTable: "message_attachments",
			Author:           author,
		})
	}
	if len(channels) > 0 && len(deletes) == 0 {
		return nil, PurgeSkippedUnauthorized, nil
	}
	return deletes, PurgeCompleted, nil
}

// PurgeUserServerMessages purges ALL of target's messages across serverID (All Time) for the
// moderation (ban/kick) path; reason is "ban" or "kick". Thin wrapper over purgeServerCore so
// the members package can consume it through a narrow interface without the range machinery.
func (h *Handler) PurgeUserServerMessages(ctx context.Context, serverID, actorID, target, reason string) (int, PurgeStatus, error) {
	t := target
	return h.purgeServerCore(ctx, serverPurgeRequest{
		serverID: serverID, actorID: actorID, target: &t, reason: reason, rangeLabel: "all",
	})
}

func ptr(value string) *string { return &value }

// resolvePurgeAuthor resolves the author filter for one channel per the RBAC matrix:
// View → required first; ManageAll → requested target (or nil = all authors);
// ManageOwn only → self, and ONLY when no other author was requested; neither → not
// authorized. Permissions are resolved from committed state to avoid using a stale
// cache entry after a membership or role change. Resolver errors fail closed and
// are reported separately from permission denials.
func (h *Handler) resolvePurgeAuthor(ctx context.Context, serverID, userID, channelID, channelType string, target *string) (*string, bool, error) {
	// You cannot purge what you cannot see. Without this, a channel override that
	// denies PermViewTextChannels but leaves ManageAllMessages intact would let an
	// actor irreversibly wipe a private channel they cannot even open. Purge needs no
	// message ID, so unlike DeleteMessage it reaches every message in the channel by
	// scope alone — the view gate is what bounds that reach.
	perms, err := h.resolver.ResolveEffectivePermissionsUncached(ctx, serverID, userID, channelID)
	if err != nil {
		if errors.Is(err, rbac.ErrNotMember) {
			return nil, false, nil
		}
		h.log.Error("Purge authz resolve failed", "error", err, "channel_id", channelID)
		return nil, false, fmt.Errorf("resolve effective permissions: %w", err)
	}
	author, ok := purgeAuthorForPermissions(perms, userID, channelType, target)
	return author, ok, nil
}

func purgeAuthorForPermissions(perms rbac.Permission, userID, channelType string, target *string) (*string, bool) {
	if channelType == "" {
		// A channel-less server has no text/voice scope to privilege. Requiring
		// either view bit keeps the same visibility boundary without inventing a
		// text-only scope that rejects a voice-only moderator.
		if !perms.Has(rbac.PermViewTextChannels) && !perms.Has(rbac.PermViewVoiceChannels) {
			return nil, false
		}
	} else {
		viewPerm := rbac.PermViewTextChannels
		if channelType == "voice" {
			viewPerm = rbac.PermViewVoiceChannels
		}
		if !perms.Has(viewPerm) {
			return nil, false
		}
	}

	if perms.Has(rbac.PermManageAllMessages) {
		return target, true
	}
	if !perms.Has(rbac.PermManageOwnMessages) {
		return nil, false
	}
	// ManageOwn only: may purge their own messages, and ONLY their own.
	//
	// An explicit target that is not self is a permission error — NOT license to purge
	// the actor's messages instead. Silently redirecting an irreversible bulk delete
	// onto a different subject and returning 200 destroys the wrong data and reports
	// success; it would also write an audit row naming the requested target while
	// deleting the actor's own messages (false Art.17 evidence against a third party).
	// A nil target means "my own messages" and is the legitimate ManageOwn request.
	if target != nil && *target != userID {
		return nil, false
	}
	self := userID
	return &self, true
}

// emitChannelPurged broadcasts the bulk-purge event to channel subscribers.
// Payload carries counts and context only — never message content (spec §11).
func (h *Handler) emitChannelPurged(channelID, actorID string, count int, rng string) {
	if h.hub == nil {
		return
	}
	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		return
	}
	h.hub.BroadcastToChannelAuthorized(channelUUID, websocket.OutgoingMessage{
		Type: "channel_purged",
		Data: map[string]interface{}{
			"channel_id":    channelID,
			"purged_by":     actorID,
			"deleted_count": count,
			"range":         rng,
		},
	})
}

// emitServerPurged broadcasts the server-scoped bulk-purge event to every client
// subscribed to the server (#1354). The subscription set is membership-verified at
// subscribe_server and evicted on removal/ban, and the desktop client subscribes to
// EVERY server it belongs to (not just the one on screen) — so this reaches each
// affected member once, the acting client included.
//
// Payload carries context only: no channel identifiers (so it discloses nothing to a
// member who cannot see a purged channel) and deliberately NO count. The per-channel
// deleted_count is structurally 0 for a server purge, and a server-level total would
// turn an invalidation signal into a report of what was destroyed (spec §11).
func (h *Handler) emitServerPurged(serverID, actorID, rng string) {
	if h.hub == nil {
		return
	}
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		return
	}
	h.hub.BroadcastToServer(serverUUID, websocket.OutgoingMessage{
		Type: "server_purged",
		Data: map[string]interface{}{
			"server_id": serverID,
			"purged_by": actorID,
			"range":     rng,
		},
	})
}

// bindPurgeRequest validates the scope ID and request body shared by the channel
// and server purges. It writes the 400 itself and reports false on failure.
func bindPurgeRequest(c *gin.Context, scopeID, invalidIDMsg string) (purgeRequest, *time.Time, bool) {
	var req purgeRequest
	if _, err := uuid.Parse(scopeID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": invalidIDMsg})
		return req, nil, false
	}
	if err := readPurgeBody(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgInvalidRequestBody})
		return purgeRequest{}, nil, false
	}
	if req.TargetUserID != nil {
		targetID, err := uuid.Parse(*req.TargetUserID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid target user ID"})
			return purgeRequest{}, nil, false
		}
		*req.TargetUserID = targetID.String()
	}
	if _, stepErr := req.Input(); stepErr != nil {
		stepErr.Write(c)
		return purgeRequest{}, nil, false
	}
	rangeFrom, err := purge.ParseRange(req.Range)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid range"})
		return req, nil, false
	}
	return req, rangeFrom, true
}

// readPurgeBody reads the whole body, at most maxPurgeBodyBytes, as exactly one
// JSON object, then applies req's binding tags. It reads the body itself, as
// stepup.ReadOptionalStepUp does, because ShouldBindJSON's decoder stops at the
// end of the first value: a second value, or kilobytes after a valid object,
// were never read, so the cap bounded nothing (review of #3509).
func readPurgeBody(c *gin.Context, req *purgeRequest) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPurgeBodyBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return err
	}
	// json.Unmarshal refuses trailing data and every non-object, and decodes an
	// empty body or null as nothing, which range's required tag then refuses.
	if err := json.Unmarshal(raw, req); err != nil {
		return err
	}
	return binding.Validator.ValidateStruct(req)
}

// selfPurgeCountCap bounds the self-purge COUNT. A count at the cap already
// exceeds both soft-lock tiers, so counting further could change no verdict.
const selfPurgeCountCap = stepup.DeleteSoftLockDayThreshold + 1

// selfPurgeSoftLockQuery reads, in the one round trip D-1 budgets for the
// COUNT, the soft-lock's population inputs — the server's enforcement flag and
// the actor's own-rule setting, a missing row reading TRUE — and the ids of a
// bounded set of the actor's messages the purge will delete; their number is
// the count. The predicate is the engine's selectBatch predicate over the
// self-authored specs' channels.
const selfPurgeSoftLockQuery = `
	SELECT s.enforce_mfa_dangerous_actions,
	       COALESCE((SELECT ps.require_auth_before_purge FROM privacy_settings ps WHERE ps.user_id = $2), TRUE),
	       (SELECT array_agg(own.id) FROM (
	            SELECT id FROM messages
	            WHERE channel_id = ANY($3::uuid[])
	              AND user_id = $2
	              AND ($4::timestamptz IS NULL OR created_at >= $4::timestamptz)
	            LIMIT $5) own)
	FROM servers s WHERE s.id = $1`

// selfPurge is what the self-purge soft-lock needs to know about an authorized
// channel or server purge.
type selfPurge struct {
	serverID, userID string
	purpose          stepup.Purpose
	input            stepup.Input
	epoch            string
	rangeFrom        *time.Time
	deletes          []purge.DeleteSpec
	// notFound is the route's 404 copy for a server deleted in flight.
	notFound string
}

// selfPurgeOutcome is what the self-purge soft-lock hands the purge.
type selfPurgeOutcome struct {
	// confirm, when non-nil, is what the purge's admission must pass, which
	// selfPurgeAdmit runs: an over-threshold purge's confirmation, or, for a
	// purge the unlocked read put outside the population, the re-read of the
	// server's enforcement flag (recheck).
	confirm *selfPurgeConfirmation
	// victims, when non-nil, are the only messages of the actor's own the purge
	// may delete: the ones the soft-lock counted. It is nil when nothing was
	// counted and when a confirmation governs the whole purge.
	victims []string
}

// selfPurgeConfirmation is a self-purge's admission check: an over-threshold
// purge's confirmation, or an outside-population purge's recheck. It runs as
// purge.Plan.Admit, in the transaction that writes the purge's audit
// row, so a refusal leaves no audit row and a factor it verifies is spent
// only by a purge that was admitted (review of #3509).
type selfPurgeConfirmation struct {
	gate       softLockGate
	retryAfter time.Duration
	notFound   string
	// recheck runs recheckSoftLockTx rather than confirmSoftLockTx: the
	// unlocked read put the actor outside the population, so the admission
	// confirms only if the server has since turned enforcement on.
	recheck bool
	// verified is set when a factor verified in the admission transaction. It
	// counts only if that transaction committed; see settleSelfPurge.
	verified bool
	// err is the confirmation's own error, which refuseSelfPurge answers.
	err error
}

// selfPurgeAdmit returns the purge's Plan.Admit: the confirmation, for an
// over-threshold self-purge, the recheck, for one outside the population, and
// nil otherwise.
func (h *Handler) selfPurgeAdmit(o selfPurgeOutcome) func(context.Context, *sql.Tx) error {
	sc := o.confirm
	if sc == nil {
		return nil
	}
	confirm := h.confirmSoftLockTx
	if sc.recheck {
		confirm = h.recheckSoftLockTx
	}
	h.runBeforeSoftLockConfirmHook()
	return func(ctx context.Context, tx *sql.Tx) error {
		sc.verified, sc.err = confirm(ctx, tx, sc.gate)
		return sc.err
	}
}

// refuseSelfPurge answers a confirmation that failed inside the purge's
// admission — a refusal, a lock conflict or a fault — and reports whether it
// did. The admission rolled back, so nothing was audited or deleted.
func (h *Handler) refuseSelfPurge(c *gin.Context, o selfPurgeOutcome) bool {
	sc := o.confirm
	if sc == nil || sc.err == nil {
		return false
	}
	h.respondSoftLockError(c, sc.err, sc.retryAfter, sc.notFound, errMsgPurgeFailed)
	return true
}

// settleSelfPurge follows a purge whose confirmation verified a factor. The
// budget is cleared once the admission that spent the factor committed; the
// counters are reset only after a purge that succeeded. A purge that was not
// admitted spent nothing, so both stay as they are, and so do they for an
// admission whose outcome is unknown: settling one that did not commit would
// clear a budget no factor was spent against.
func (h *Handler) settleSelfPurge(ctx context.Context, o selfPurgeOutcome, runErr error) {
	sc := o.confirm
	if sc == nil || !sc.verified ||
		errors.Is(runErr, purge.ErrNotAdmitted) || errors.Is(runErr, purge.ErrAdmissionUnknown) {
		return
	}
	h.clearSoftLockBudget(ctx, sc.gate)
	if runErr == nil {
		h.resetSoftLock(ctx, sc.gate)
	}
}

// fence limits the actor's own delete specs to the counted victims, so a
// message sent after the count is not deleted uncounted (review of #3509).
// Specs for other authors are never counted, so they are left as they are.
func (o selfPurgeOutcome) fence(deletes []purge.DeleteSpec, userID string) []purge.DeleteSpec {
	if o.victims == nil {
		return deletes
	}
	fenced := make([]purge.DeleteSpec, len(deletes))
	copy(fenced, deletes)
	for i := range fenced {
		if isSelfSpec(fenced[i], userID) {
			fenced[i].OnlyIDs = o.victims
		}
	}
	return fenced
}

// isSelfSpec reports whether a delete spec's resolved author is the actor.
func isSelfSpec(ds purge.DeleteSpec, userID string) bool {
	return ds.Author != nil && *ds.Author == userID
}

// gateSelfPurge is the delete-rate soft-lock for a channel or server purge
// (#3455 D-1). It counts only the delete specs whose resolved author is the
// actor — the ManageOwn-forced or explicit self target — and only for a
// population member; a purge of other authors is never counted, and the
// ban/kick path (PurgeUserServerMessages) never calls it.
//
// The count and the purge are separate reads, so under the threshold the
// outcome carries the counted messages' ids and the caller fences its own
// specs to them. Over it, a verified confirmation covers the whole purge,
// and so does a server that stopped enforcing with no rule left to apply.
//
// Outside the population, as the unlocked read sees it, nothing is counted
// or fenced, but the admission still re-reads the enforcement flag under the
// servers-row lock (recheckSoftLockTx): the per-batch guard never reads it,
// so a server that turned enforcement on after that read would otherwise let
// the purge run with no confirmation and no count.
//
// Over the threshold the governing rule confirms ONCE, in the transaction
// that admits the purge by writing its audit row (selfPurgeAdmit): the
// engine's batches are separate transactions and a TOTP code verifies once,
// so a confirmation per batch would be wrong, and one committed on its own
// would spend the factor even when the admission then failed. A refusal rolls
// the admission back, so nothing is purged and nothing is audited.
//
// It returns ok=false once it has written a response.
func (h *Handler) gateSelfPurge(ctx context.Context, c *gin.Context, p selfPurge) (selfPurgeOutcome, bool) {
	own := make([]string, 0, len(p.deletes))
	for _, ds := range p.deletes {
		if isSelfSpec(ds, p.userID) {
			own = append(own, ds.ScopeID)
		}
	}
	if len(own) == 0 {
		return selfPurgeOutcome{}, true
	}

	g := softLockGate{userID: p.userID, serverID: p.serverID, purpose: p.purpose, input: p.input, epoch: p.epoch}
	var counted pq.StringArray
	if err := h.db.QueryRowContext(ctx, selfPurgeSoftLockQuery,
		p.serverID, p.userID, pq.Array(own), p.rangeFrom, selfPurgeCountCap,
	).Scan(&g.enforcing, &g.ownRule, &counted); err != nil {
		h.log.Error("Self-purge soft-lock read failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
		return selfPurgeOutcome{}, false
	}
	if !g.inPopulation() {
		return selfPurgeOutcome{confirm: &selfPurgeConfirmation{gate: g, notFound: p.notFound, recheck: true}}, true
	}
	// Counted from here on, so the purge may delete only what was counted —
	// nothing, when the count was zero.
	victims := append([]string{}, counted...)
	if len(victims) == 0 {
		return selfPurgeOutcome{victims: victims}, true
	}

	verdict, ok := h.chargeSoftLock(ctx, c, g, int64(len(victims)))
	if !ok {
		return selfPurgeOutcome{}, false
	}
	if !verdict.Over {
		return selfPurgeOutcome{victims: victims}, true
	}
	if !h.consumeSoftLockBudget(ctx, c, g) {
		return selfPurgeOutcome{}, false
	}
	return selfPurgeOutcome{confirm: &selfPurgeConfirmation{gate: g, retryAfter: verdict.RetryAfter, notFound: p.notFound}}, true
}
