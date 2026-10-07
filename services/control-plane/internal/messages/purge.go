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
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
)

const errMsgPurgeFailed = "Purge failed"

// The purge routes' generic 403 bodies, unchanged.
const (
	errMsgChannelPurgeForbidden = "Insufficient permissions to purge this channel"
	errMsgServerPurgeForbidden  = "Insufficient permissions to purge this server"
)

var (
	errPurgeMemberTimedOut = errors.New("purge actor is timed out")
	// errPurgeAuthorityChanged is a batch or admission guard's refusal of an
	// actor whose fresh permissions no longer yield the author filter the plan
	// was built with.
	errPurgeAuthorityChanged = errors.New("purge authority changed")
	// errPurgeEnforcementTurnedOn is the per-batch recheck's refusal (#3454
	// A-3.6): the server enforces MFA now, and the spec was admitted while it
	// did not.
	errPurgeEnforcementTurnedOn = errors.New("purge admitted before the server enforced MFA")
)

// purgeDangerousBit is the dangerous permission a D1 purge exercises: purging
// another author, or every author, needs ManageAllMessages. Both purge
// purposes map to it, so it scopes their grace (stepup.DangerousActionGraceScope)
// and is the bit RS5 asks about at a purge denial.
const purgeDangerousBit = rbac.PermManageAllMessages

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
	// admission is the HTTP route's composed admission, run as the plan's
	// Plan.Admit (#3454 A-3); nil on the ban/kick path, whose plan has no
	// Admit and keeps its pooled audit row (A-3.7).
	admission *deleteAdmission
	// provenance is how the ban/kick transaction admitted this purge's D1
	// specs (#3454 A-3.7). The HTTP route leaves it zero, which is not a
	// provenance; its admission decides (A-3.6).
	provenance PurgeProvenance
}

// specProvenance is ds's provenance for the per-batch recheck (A-3.6).
func (r serverPurgeRequest) specProvenance(ds purge.DeleteSpec) PurgeProvenance {
	return purgeSpecProvenance(ds, r.actorID, r.admission, r.provenance)
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
		h.respondChannelPurgeAuthError(c, authErr, channelID)
		return
	}
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgChannelPurgeForbidden})
		return
	}

	deletes := []purge.DeleteSpec{{
		MessagesTable:    "messages",
		ScopeColumn:      "channel_id",
		ScopeID:          channelID,
		AttachmentsTable: "message_attachments",
		Author:           author,
	}}
	epoch := middleware.TokenCredentialEpoch(c)
	guard := channelPurgeGuard{
		serverID: serverID, channelID: channelID, actorID: userID, epoch: epoch,
		author: author, target: req.TargetUserID,
	}
	softLock, ok := h.gatePurge(purgeCtx, c, selfPurge{
		serverID: serverID, userID: userID, purpose: stepup.PurposeChannelPurge,
		input: req.stepUp(), epoch: epoch,
		rangeFrom: rangeFrom, deletes: deletes, notFound: "Channel not found",
		forbidden: errMsgChannelPurgeForbidden,
		authorize: func(ctx context.Context, tx *sql.Tx, ds purge.DeleteSpec) error {
			return h.authorizeChannelPurgeBatchTx(ctx, tx, guard, ds)
		},
	})
	if !ok {
		return
	}
	deletes = softLock.fence(deletes, userID)
	guard.admission = softLock.admission

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
		Admit: h.selfPurgeAdmit(softLock.admission),
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
// batch against: the server the unlocked read found the channel in (a
// channel's server_id is never rewritten), the actor, the token's credential
// epoch, the author filter the plan was built with, and the admission whose
// outcome is the D1 spec's provenance.
type channelPurgeGuard struct {
	serverID, channelID, actorID, epoch string
	author, target                      *string
	admission                           *deleteAdmission
}

// guardChannelPurgeBatch is a channel purge's per-batch Guard. It refuses a
// batch outside the planned scope, then under the batch's transaction
// re-fences the credential epoch, rechecks the server's enforcement flag
// against the spec's provenance (#3454 A-3.6), and re-authorizes the actor
// (authorizeChannelPurgeBatchTx): users, then servers, then its children.
func (h *Handler) guardChannelPurgeBatch(ctx context.Context, tx *sql.Tx, g channelPurgeGuard, ds purge.DeleteSpec) error {
	if ds.ScopeID != g.channelID || !samePurgeAuthor(ds.Author, g.author) {
		return errors.New("unexpected channel purge batch scope")
	}
	if err := credepoch.GuardTx(ctx, tx, g.actorID, g.epoch); err != nil {
		return err
	}
	// The HTTP route carries no ban/kick provenance: a D1 spec it never
	// admitted reads as the zero value, which is unconfirmed.
	if err := recheckPurgeEnforcementTx(ctx, tx, g.serverID, purgeSpecProvenance(ds, g.actorID, g.admission, 0)); err != nil {
		return err
	}
	return h.authorizeChannelPurgeBatchTx(ctx, tx, g, ds)
}

// authorizeChannelPurgeBatchTx is the channel purge's authority check, under
// the caller's transaction: it locks the channel and the actor's membership,
// refuses a timed-out actor, and re-derives the author filter from fresh
// permissions, refusing when it no longer matches (recheckPurgeAuthorTx). The
// per-batch Guard runs it, and so does the admission over a D1 spec (I7,
// A-3.2), where the gate's users and servers locks already precede it.
func (h *Handler) authorizeChannelPurgeBatchTx(ctx context.Context, tx *sql.Tx, g channelPurgeGuard, ds purge.DeleteSpec) error {
	var lockedServerID, lockedType string
	if err := tx.QueryRowContext(ctx, `SELECT server_id, type FROM channels WHERE id = $1 FOR SHARE`, g.channelID).Scan(&lockedServerID, &lockedType); err != nil {
		return err
	}
	if err := lockPurgeActorTx(ctx, tx, lockedServerID, g.actorID); err != nil {
		return err
	}
	perms, err := h.resolver.ResolveChannelPermissionsTx(ctx, tx, lockedServerID, g.actorID, g.channelID)
	if err != nil {
		return err
	}
	return recheckPurgeAuthorTx(ctx, tx, purgeAuthority{
		serverID: lockedServerID, channelID: g.channelID, channelType: lockedType, actorID: g.actorID,
		target: g.target, planned: ds.Author,
	}, perms)
}

// lockPurgeActorTx locks the actor's membership FOR SHARE and refuses a
// timed-out actor.
func lockPurgeActorTx(ctx context.Context, tx *sql.Tx, serverID, actorID string) error {
	var timedOut bool
	if err := tx.QueryRowContext(ctx, `
		SELECT timed_out_until IS NOT NULL AND timed_out_until > clock_timestamp()
		FROM server_members WHERE server_id = $1 AND user_id = $2 FOR SHARE`, serverID, actorID,
	).Scan(&timedOut); err != nil {
		return err
	}
	if timedOut {
		return errPurgeMemberTimedOut
	}
	return nil
}

// purgeAuthority is what recheckPurgeAuthorTx re-derives one spec's author
// filter from: where, who, the requested target, and the filter planned.
type purgeAuthority struct {
	serverID, channelID, channelType, actorID string
	target, planned                           *string
}

// recheckPurgeAuthorTx refuses a spec whose author filter, re-derived from
// perms, is no longer the planned one. When the MFA mask is why, the refusal
// is RS5's mfa_enrollment_required, read on tx (#3454 A-11); otherwise it is
// errPurgeAuthorityChanged.
func recheckPurgeAuthorTx(ctx context.Context, tx *sql.Tx, a purgeAuthority, perms rbac.Permission) error {
	fresh, allowed := purgeAuthorForPermissions(perms, a.actorID, a.channelType, a.target)
	if allowed && samePurgeAuthor(fresh, a.planned) {
		return nil
	}
	if err := purgeEnrollmentDenial(ctx, tx, a.serverID, a.channelID, a.actorID, purgeViewAllowed(perms, a.channelType)); err != nil {
		return err
	}
	return errPurgeAuthorityChanged
}

// purgeEnforcementRecheckQuery is the per-batch recheck's read (#3454 A-3.6).
// FOR SHARE, not KEY SHARE: it must conflict with the toggle's UPDATE, so a
// flip either commits before the batch reads it or waits for the batch.
const purgeEnforcementRecheckQuery = `SELECT enforce_mfa_dangerous_actions FROM servers WHERE id = $1 FOR SHARE`

// recheckPurgeEnforcementTx is the per-batch enforcement recheck. It refuses
// the batch only when the server enforces now and provenance cannot vouch for
// that: a self spec (exempt) and a D1 spec whose admission confirmed the
// server rule pass; an unconfirmed spec, and the zero value, are refused. It
// never re-verifies. Run it right after the batch's credential-epoch fence and
// before any child of servers, so the order stays users → servers →
// children; on the ban/kick path, which has no epoch, it is the first
// statement.
func recheckPurgeEnforcementTx(ctx context.Context, tx *sql.Tx, serverID string, provenance PurgeProvenance) error {
	var enforcing bool
	if err := tx.QueryRowContext(ctx, purgeEnforcementRecheckQuery, serverID).Scan(&enforcing); err != nil {
		return fmt.Errorf("purge enforcement recheck: %w", err)
	}
	if enforcing && !provenance.vouchesForEnforcement() {
		return errPurgeEnforcementTurnedOn
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
//
// A guard refusal before anything was deleted is answered FIRST and never
// reaches the failure line, which carries channel_id: whether the gate or the
// soft-lock refused this actor must not be readable from a scoped log line
// (observability principle 7, C7). The server route already answered its
// refusals this way; the channel route logged first (Codex review of #3454).
// The reorder skips no invalidation, because a refusal deleted nothing.
func (h *Handler) respondChannelPurgeFailure(c *gin.Context, err error, channelID, userID, rangeLabel string, res purge.Result) bool {
	if err == nil {
		return false
	}
	if res.DeletedCount == 0 && h.writePurgeGuardRefusal(c, err) {
		return true
	}
	h.log.Error("Channel purge failed", "error", err, "channel_id", channelID)
	if res.DeletedCount > 0 {
		h.emitChannelPurged(channelID, userID, res.DeletedCount, rangeLabel)
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
	return true
}

// writePurgeGuardRefusal answers a purge that a guard refused before anything
// was deleted, when the refusal is one of the actor's own standing: a
// timed-out member, a revoked session, or RS5's enrollment refusal (a
// *stepup.Error). It reports whether it wrote a response; anything else is the
// caller's 500.
func (h *Handler) writePurgeGuardRefusal(c *gin.Context, err error) bool {
	var stepErr *stepup.Error
	switch {
	case errors.Is(err, errPurgeMemberTimedOut):
		writeMemberTimedOut(c)
	case errors.Is(err, credepoch.ErrEpochMismatch), errors.Is(err, credepoch.ErrBlocked):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
	case errors.As(err, &stepErr):
		mfaenforce.WriteError(c, h.log, stepErr, nil)
	default:
		return false
	}
	return true
}

// writeMemberTimedOut is a purge's refusal of a timed-out member.
func writeMemberTimedOut(c *gin.Context) {
	c.JSON(http.StatusForbidden, gin.H{"error": "Member is timed out", "code": "member_timed_out"})
}

// respondChannelPurgeAuthError and respondServerPurgeAuthError answer a failed
// preflight authorization: RS5's refusal (a *stepup.Error, #3454 A-11) as
// itself, anything else as a logged 500. They are two functions rather than
// one taking the route's log fields as a variadic spread: every attribute here
// sits behind a constant key, which is what keeps a request-derived id out of
// a log sink (CWE-117; [internal]codeql-false-positive-register.md,
// go/log-injection).
func (h *Handler) respondChannelPurgeAuthError(c *gin.Context, err error, channelID string) {
	if writePurgeRS5Refusal(c, h.log, err) {
		return
	}
	h.log.Error("Channel purge authorization failed", "error", err, "channel_id", channelID)
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
}

func (h *Handler) respondServerPurgeAuthError(c *gin.Context, err error, serverID string) {
	if writePurgeRS5Refusal(c, h.log, err) {
		return
	}
	h.log.Error("Server purge authorization failed", "error", err, "server_id", serverID)
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
}

// writePurgeRS5Refusal answers err when it is RS5's enrollment refusal and
// reports whether it did.
func writePurgeRS5Refusal(c *gin.Context, log mfaenforce.Logger, err error) bool {
	var stepErr *stepup.Error
	if !errors.As(err, &stepErr) {
		return false
	}
	mfaenforce.WriteError(c, log, stepErr, nil)
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

// PurgeProvenance records how a purge's D1 delete specs were admitted (#3454
// A-3.6, A-3.7), so the per-batch guard can refuse a batch whose admission
// predates the server turning MFA enforcement on. It is per spec: a self spec
// is exempt (the self-purge is #3455's), a D1 spec carries its admission's.
//
// The zero value is not a provenance. A guard must read it as
// PurgeUnconfirmed, so a request that forgot to set it is refused on an
// enforcing server rather than waved through.
type PurgeProvenance int

const (
	// PurgeExempt is not a D1 spec. The enforcement recheck never refuses it.
	PurgeExempt PurgeProvenance = iota + 1
	// PurgeConfirmed means the admitting transaction verified a factor or found a
	// valid grace on an enforcing server.
	PurgeConfirmed
	// PurgeUnconfirmed means the admitting transaction committed while the server
	// did not enforce. A batch that finds it enforcing now is refused.
	PurgeUnconfirmed
)

// vouchesForEnforcement reports whether a spec with this provenance may run on
// a server that enforces now: exempt and confirmed only. Unconfirmed, the zero
// value and anything unknown may not (fail closed).
func (p PurgeProvenance) vouchesForEnforcement() bool {
	return p == PurgeExempt || p == PurgeConfirmed
}

// purgeSpecProvenance is one spec's provenance (#3454 A-3.6): a self spec is
// exempt, whatever else is known. A D1 spec carries its admission's outcome
// when the request had one (the HTTP routes), otherwise fallback, the ban/kick
// transaction's (A-3.7), whose zero value reads as unconfirmed.
func purgeSpecProvenance(ds purge.DeleteSpec, actorID string, a *deleteAdmission, fallback PurgeProvenance) PurgeProvenance {
	switch {
	case isSelfSpec(ds, actorID):
		return PurgeExempt
	case a != nil:
		return a.d1Provenance()
	default:
		return fallback
	}
}

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
	sreq := serverPurgeRequest{
		serverID: serverID, actorID: userID, target: req.TargetUserID, reason: "manual",
		rangeFrom: rangeFrom, rangeLabel: req.Range, credentialEpoch: ptr(epoch),
	}
	softLock, ok := h.gatePurge(purgeCtx, c, selfPurge{
		serverID: serverID, userID: userID, purpose: stepup.PurposeServerPurge,
		input: req.stepUp(), epoch: epoch,
		rangeFrom: rangeFrom, deletes: deletes, notFound: "Server not found",
		forbidden: errMsgServerPurgeForbidden,
		authorize: func(ctx context.Context, tx *sql.Tx, ds purge.DeleteSpec) error {
			return h.authorizeServerPurgeBatchTx(ctx, tx, sreq, ds)
		},
	})
	if !ok {
		return
	}
	deletes = softLock.fence(deletes, userID)
	sreq.admission = softLock.admission
	deleted, status, err := h.runServerPurge(purgeCtx, sreq, deletes)
	if h.refuseSelfPurge(c, softLock) {
		return
	}
	h.settleSelfPurge(purgeCtx, softLock, err)
	h.respondServerPurge(c, serverID, userID, deleted, status, err)
}

// respondServerPurge writes PurgeServer's outcome. A skip carries RS5's
// refusal as its error when the MFA mask caused it (serverPurgeDeletes).
func (h *Handler) respondServerPurge(c *gin.Context, serverID, userID string, deleted int, status PurgeStatus, err error) {
	switch status {
	case PurgeSkippedUnauthorized:
		if err != nil {
			h.respondServerPurgeAuthError(c, err, serverID)
			return
		}
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgServerPurgeForbidden})
	case PurgeFailed:
		if deleted == 0 && h.writePurgeGuardRefusal(c, err) {
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
//
// It keeps Admit == nil and the engine's pooled audit row (#3454 A-3.7): the
// ban or removal transaction already admitted the purge, and req.provenance
// records how, for the per-batch recheck. RS5's refusal, which only an HTTP
// answer can carry, is dropped from a skip.
func (h *Handler) purgeServerCore(ctx context.Context, req serverPurgeRequest) (int, PurgeStatus, error) {
	deletes, status, err := h.authorizeServerPurge(ctx, req.serverID, req.actorID, req.target)
	if status == PurgeSkippedUnauthorized {
		return 0, status, nil
	}
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
		Admit: h.selfPurgeAdmit(req.admission),
	}
	res, err := h.purgeEngine.Run(ctx, plan)
	if err != nil {
		// A failed admission on the HTTP route is answered, and logged, by
		// PurgeServer: a soft-lock refusal must not reach a line that carries
		// server_id (C7).
		if req.admission == nil || !errors.Is(err, purge.ErrNotAdmitted) {
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
// batch's transaction it re-fences the credential epoch when the request
// carries one, rechecks the server's enforcement flag against the spec's
// provenance (#3454 A-3.6) — the first statement on the ban/kick path — and
// re-authorizes the actor in the spec's channel (authorizeServerPurgeBatchTx).
func (h *Handler) guardServerPurgeBatch(guardCtx context.Context, tx *sql.Tx, req serverPurgeRequest, ds purge.DeleteSpec) error {
	if req.credentialEpoch != nil {
		if err := credepoch.GuardTx(guardCtx, tx, req.actorID, *req.credentialEpoch); err != nil {
			return err
		}
	}
	if err := recheckPurgeEnforcementTx(guardCtx, tx, req.serverID, req.specProvenance(ds)); err != nil {
		return err
	}
	return h.authorizeServerPurgeBatchTx(guardCtx, tx, req, ds)
}

// authorizeServerPurgeBatchTx is the server purge's authority check for one
// spec, under the caller's transaction: it locks the channel and the actor's
// membership, refuses a timed-out actor, and re-derives the author filter from
// fresh permissions, refusing when it no longer matches the one the plan was
// built with. The per-batch Guard runs it, and so does the admission over a
// D1 spec (I7, A-3.2).
func (h *Handler) authorizeServerPurgeBatchTx(ctx context.Context, tx *sql.Tx, req serverPurgeRequest, ds purge.DeleteSpec) error {
	var lockedType string
	if err := tx.QueryRowContext(ctx,
		`SELECT type FROM channels WHERE id = $1 AND server_id = $2 FOR SHARE`, ds.ScopeID, req.serverID,
	).Scan(&lockedType); err != nil {
		return err
	}
	if err := lockPurgeActorTx(ctx, tx, req.serverID, req.actorID); err != nil {
		return err
	}
	perms, err := h.resolver.ResolveChannelPermissionsTx(ctx, tx, req.serverID, req.actorID, ds.ScopeID)
	if err != nil {
		return err
	}
	return recheckPurgeAuthorTx(ctx, tx, purgeAuthority{
		serverID: req.serverID, channelID: ds.ScopeID, channelType: lockedType, actorID: req.actorID,
		target: req.target, planned: ds.Author,
	}, perms)
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
//
// A refusal of every channel is PurgeSkippedUnauthorized, carrying RS5's
// refusal as its error when the MFA mask withheld ManageAllMessages (#3454
// A-11). That question is asked at server scope, because the skip answers for
// the whole server: a member whose grant exists only as one channel's ALLOW
// learns it by purging that channel.
func (h *Handler) serverPurgeDeletes(ctx context.Context, serverID, actorID string, target *string, channels []channelScope) ([]purge.DeleteSpec, PurgeStatus, error) {
	if len(channels) == 0 {
		// With no channel overrides to bypass, a server-scope authorization check
		// distinguishes a legitimate empty purge from an existence oracle.
		_, ok, err := h.resolvePurgeAuthor(ctx, serverID, actorID, "", "", target)
		var stepErr *stepup.Error
		if errors.As(err, &stepErr) {
			return nil, PurgeSkippedUnauthorized, stepErr
		}
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

	deletes, maskable := serverPurgeSpecs(permsByChannel, actorID, target, channels)
	if len(channels) > 0 && len(deletes) == 0 {
		// Deliberately unwrapped: it is RS5's *stepup.Error or nil.
		return nil, PurgeSkippedUnauthorized, purgeEnrollmentDenial(ctx, h.db, serverID, "", actorID, maskable)
	}
	return deletes, PurgeCompleted, nil
}

// serverPurgeSpecs builds one delete spec per channel the actor may purge,
// skipping every channel they are denied in. maskable reports whether any
// denial got past the view check, so the MFA mask could have caused it.
func serverPurgeSpecs(
	permsByChannel map[string]rbac.Permission, actorID string, target *string, channels []channelScope,
) (deletes []purge.DeleteSpec, maskable bool) {
	for _, channel := range channels {
		perms := permsByChannel[channel.id]
		author, ok := purgeAuthorForPermissions(perms, actorID, channel.channelType, target)
		if !ok {
			maskable = maskable || purgeViewAllowed(perms, channel.channelType)
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
	return deletes, maskable
}

// PurgeUserServerMessages purges ALL of target's messages across serverID (All Time) for the
// moderation (ban/kick) path; reason is "ban" or "kick". Thin wrapper over purgeServerCore so
// the members package can consume it through a narrow interface without the range machinery.
// provenance is how the ban or removal transaction admitted the purge (#3454 A-3.7).
func (h *Handler) PurgeUserServerMessages(
	ctx context.Context, serverID, actorID, target, reason string, provenance PurgeProvenance,
) (int, PurgeStatus, error) {
	t := target
	return h.purgeServerCore(ctx, serverPurgeRequest{
		serverID: serverID, actorID: actorID, target: &t, reason: reason, rangeLabel: "all",
		provenance: provenance,
	})
}

func ptr(value string) *string { return &value }

// resolvePurgeAuthor resolves the author filter for one channel per the RBAC matrix:
// View → required first; ManageAll → requested target (or nil = all authors);
// ManageOwn only → self, and ONLY when no other author was requested; neither → not
// authorized. Permissions are resolved from committed state to avoid using a stale
// cache entry after a membership or role change. Resolver errors fail closed and
// are reported separately from permission denials. A denial the MFA mask caused
// returns RS5's refusal as the error (#3454 A-11), read on the pool the
// denial came from.
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
	if !ok {
		return nil, false, purgeEnrollmentDenial(ctx, h.db, serverID, channelID, userID, purgeViewAllowed(perms, channelType))
	}
	return author, true, nil
}

// purgeEnrollmentDenial is RS5 at a purge denial (#3454 A-11): the
// mfa_enrollment_required refusal when the MFA mask withheld
// ManageAllMessages from a raw holder, otherwise nil, and the caller answers
// as before. maskable is false for a denial at the view check, which the mask
// never causes — it keeps every non-dangerous bit, Administrator's expansion
// included — so naming enrollment there would be false, and no statement
// runs. q is the querier the denial came from. Call it only after a denial
// (I7). The result is an untyped nil or a *stepup.Error.
func purgeEnrollmentDenial(ctx context.Context, q stepup.RowQuerier, serverID, channelID, actorID string, maskable bool) error {
	if !maskable {
		return nil
	}
	if e := rbac.EnrollmentDenial(ctx, q, serverID, channelID, actorID, purgeDangerousBit); e != nil {
		return e
	}
	return nil
}

// purgeViewAllowed is purge's visibility boundary: you cannot purge what you
// cannot see.
func purgeViewAllowed(perms rbac.Permission, channelType string) bool {
	switch channelType {
	case "":
		// A channel-less server has no text/voice scope to privilege. Requiring
		// either view bit keeps the same visibility boundary without inventing a
		// text-only scope that rejects a voice-only moderator.
		return perms.Has(rbac.PermViewTextChannels) || perms.Has(rbac.PermViewVoiceChannels)
	case "voice":
		return perms.Has(rbac.PermViewVoiceChannels)
	default:
		return perms.Has(rbac.PermViewTextChannels)
	}
}

func purgeAuthorForPermissions(perms rbac.Permission, userID, channelType string, target *string) (*string, bool) {
	if !purgeViewAllowed(perms, channelType) {
		return nil, false
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

// selfPurge is what a channel or server purge's admission needs to know about
// the authorized purge: the soft-lock's inputs, and for a D1 spec the route's
// authority check and 403 copy.
type selfPurge struct {
	serverID, userID string
	purpose          stepup.Purpose
	input            stepup.Input
	epoch            string
	rangeFrom        *time.Time
	deletes          []purge.DeleteSpec
	// notFound is the route's 404 copy for a server deleted in flight.
	notFound string
	// forbidden is the route's generic 403 copy, the answer to an admission
	// whose authority check refused a D1 spec.
	forbidden string
	// authorize is the plan's batch authority check for one spec, without its
	// credential-epoch fence (the admission's gate lock is that fence). The
	// admission runs it over every D1 spec before any confirmation (I7, #3454
	// A-3.2).
	authorize func(context.Context, *sql.Tx, purge.DeleteSpec) error
}

// gate is the actor's soft-lock gate for this purge, before any read.
func (p selfPurge) gate() softLockGate {
	return softLockGate{userID: p.userID, serverID: p.serverID, purpose: p.purpose, input: p.input, epoch: p.epoch}
}

// selfPurgeOutcome is what the purge's gates hand the purge.
type selfPurgeOutcome struct {
	// admission, when non-nil, is what the purge's admission must pass, which
	// selfPurgeAdmit runs: the composed confirmation of an over-threshold
	// self-purge, of a self-purge the unlocked read put outside the population
	// (a recheck), and of the plan's D1 specs (#3454 A-3).
	admission *deleteAdmission
	// victims, when non-nil, are the only messages of the actor's own the purge
	// may delete: the ones the soft-lock counted. It is nil when nothing was
	// counted and when a confirmation governs the whole purge.
	victims []string
}

// selfPurgeAdmit returns the purge's Plan.Admit: the composed admission
// (admitDeleteTx) when the purge has one, run in the transaction that writes
// the purge's audit row, so a refusal leaves no audit row and a factor it
// verifies is spent only by a purge that was admitted (review of #3509). nil
// otherwise, which keeps the engine's pooled audit row.
func (h *Handler) selfPurgeAdmit(a *deleteAdmission) func(context.Context, *sql.Tx) error {
	if a == nil {
		return nil
	}
	h.runBeforeSoftLockConfirmHook()
	return func(ctx context.Context, tx *sql.Tx) error {
		a.result, a.err = h.admitDeleteTx(ctx, tx, a)
		return a.err
	}
}

// refuseSelfPurge answers an admission that failed — a refusal, a lock
// conflict or a fault — and reports whether it did. The admission rolled
// back, so nothing was audited or deleted. A plan with a D1 spec answers with
// D1's plain bodies (respondD1AdmissionError, #3454 A-3.4); one without keeps
// the soft-lock's decorated refusal (respondSoftLockError).
func (h *Handler) refuseSelfPurge(c *gin.Context, o selfPurgeOutcome) bool {
	a := o.admission
	if a == nil || a.err == nil {
		return false
	}
	if len(a.d1) > 0 {
		h.respondD1AdmissionError(c, a.err, a.forbidden, a.notFound)
		return true
	}
	h.respondSoftLockError(c, a.err, a.retryAfter, a.notFound, errMsgPurgeFailed)
	return true
}

// respondD1AdmissionError answers the admission of a plan with a D1 spec
// (#3454 A-3.4): its authority check's refusals as that check's batch refusal
// with nothing deleted is answered (a timed-out member, the route's 403), then
// the gate's plain seam body through mfaenforce.WriteError — no
// delete_rate_limited and no Retry-After, because waiting lifts nothing — and
// anything else as the route's 500. An actor whose authority the check
// refused never sees mfa_required (I7).
func (h *Handler) respondD1AdmissionError(c *gin.Context, err error, forbidden, notFound string) {
	switch {
	case errors.Is(err, errPurgeAuthorityChanged):
		c.JSON(http.StatusForbidden, gin.H{"error": forbidden})
	case errors.Is(err, errPurgeMemberTimedOut):
		writeMemberTimedOut(c)
	case mfaenforce.IsGateError(err):
		mfaenforce.WriteError(c, h.log, err, func(c *gin.Context) {
			c.JSON(http.StatusNotFound, gin.H{"error": notFound})
		})
	default:
		h.log.Error("Purge admission failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
	}
}

// settleSelfPurge follows a purge's admission (settleAdmission): a verified
// factor clears the budget and grants the graces it earned once the admission
// that spent it committed; the counters are reset only after a purge that
// succeeded. A purge that was not admitted spent nothing, so nothing is
// settled, and nothing is for an admission whose outcome is unknown either:
// settling one that did not commit would clear a budget no factor was spent
// against, and grant a grace no confirmation earned.
func (h *Handler) settleSelfPurge(ctx context.Context, o selfPurgeOutcome, runErr error) {
	a := o.admission
	if a == nil || errors.Is(runErr, purge.ErrNotAdmitted) || errors.Is(runErr, purge.ErrAdmissionUnknown) {
		return
	}
	h.settleAdmission(ctx, a, runErr == nil)
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
// specs to them. Over it, a confirmation (a verified factor, or a grace the
// governing rule accepts) covers the whole purge,
// and so does a server that stopped enforcing with no rule left to apply.
//
// Outside the population, as the unlocked read sees it, nothing is counted
// or fenced, but the admission still re-reads the enforcement flag under the
// servers-row lock (a confirmRecheck admission): the per-batch recheck
// (A-3.6) exempts a self spec, so a server that turned enforcement on after
// that read would otherwise let the purge run with no confirmation and no
// count.
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

	g := p.gate()
	var counted pq.StringArray
	if err := h.db.QueryRowContext(ctx, selfPurgeSoftLockQuery,
		p.serverID, p.userID, pq.Array(own), p.rangeFrom, selfPurgeCountCap,
	).Scan(&g.enforcing, &g.ownRule, &counted); err != nil {
		h.log.Error("Self-purge soft-lock read failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgPurgeFailed})
		return selfPurgeOutcome{}, false
	}
	if !g.inPopulation() {
		return selfPurgeOutcome{admission: &deleteAdmission{gate: g, mode: confirmRecheck, notFound: p.notFound}}, true
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
	if !h.armSoftLockConfirmation(ctx, c, &g) {
		return selfPurgeOutcome{}, false
	}
	return selfPurgeOutcome{admission: &deleteAdmission{
		gate: g, mode: confirmOverThreshold, retryAfter: verdict.RetryAfter, notFound: p.notFound,
	}}, true
}

// gatePurge runs both of a channel or server purge's gates before its
// admission: the self-purge soft-lock (gateSelfPurge), then the D1 gate over
// every spec whose author is not the actor (armD1Purge, #3454 A-3). Both feed
// ONE admission, which confirms at most once. It returns ok=false once it has
// written a response.
func (h *Handler) gatePurge(ctx context.Context, c *gin.Context, p selfPurge) (selfPurgeOutcome, bool) {
	o, ok := h.gateSelfPurge(ctx, c, p)
	if !ok {
		return o, false
	}
	return h.armD1Purge(ctx, c, p, o)
}

// armD1Purge is the D1 gate's pre-transaction half for a purge with a D1
// spec — one whose author filter is every author, or someone other than the
// actor (A-3.1). It joins the soft-lock's admission, or opens one, so the
// whole purge is admitted under one lock and one confirmation decision. A
// purge with no D1 spec is returned unchanged.
//
// The budget is charged at most once per request (A-3.5): D1 charges only
// when the request carries an mfa_code and the soft-lock has not charged it
// already; a step_up_token is never charged or verified for D1, since it can
// only carry a password, which the server rule never accepts. The grace is
// read here, before BeginTx, for the purge's dangerous bit (A-9), and judged
// under the admission's locks. On a budget refusal it has written the 429 or
// 503 and returns false.
func (h *Handler) armD1Purge(ctx context.Context, c *gin.Context, p selfPurge, o selfPurgeOutcome) (selfPurgeOutcome, bool) {
	var d1 []purge.DeleteSpec
	for _, ds := range p.deletes {
		if !isSelfSpec(ds, p.userID) {
			d1 = append(d1, ds)
		}
	}
	if len(d1) == 0 {
		return o, true
	}
	a := o.admission
	if a == nil {
		a = &deleteAdmission{gate: p.gate(), notFound: p.notFound}
	}
	if !a.softLockCharged() &&
		!h.admitStepUpCharge(c, stepup.Charge(ctx, stepup.DangerousActionBudget(h.redis), p.userID, p.input.MFACode)) {
		return selfPurgeOutcome{}, false
	}
	a.d1, a.authorize, a.forbidden = d1, p.authorize, p.forbidden
	uid, userErr := uuid.Parse(p.userID)
	sid, serverErr := uuid.Parse(p.serverID)
	if userErr == nil && serverErr == nil { // a zero read covers nothing: the actor is prompted
		a.d1Grace = stepup.NewGraceStore(h.redis).Read(ctx, stepup.GraceActorFromContext(c, uid),
			stepup.DangerousActionGraceScope(sid, int64(purgeDangerousBit)))
	}
	o.admission = a
	return o, true
}
