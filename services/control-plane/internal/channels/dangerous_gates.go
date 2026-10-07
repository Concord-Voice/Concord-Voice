package channels

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// The dangerous-action gates on DeleteChannel and on shortening a channel's
// message-expiration policy (#3454 A-7, A-8). On a server that enforces MFA on
// dangerous actions, each asks the actor for an inline factor under the gate's
// locks; on one that does not, a request carrying no mfa_code is answered
// exactly as before. Each runs, in order:
//
//  1. chargeChannelGate, before BeginTx: the budget charge and the grace read,
//     the only Redis calls the gate makes (A-2, A-9);
//  2. in the route's existing transaction, LockGateTx right after its
//     credepoch.GuardTx, then the route's own channel lock and ManageChannels
//     check (I7), then requireChannelGate, then the write;
//  3. after COMMIT, settleChannelGate.
//
// Both routes require ManageChannels, so they share one grace scope per
// server: a confirmed deletion covers a shortening there, and the reverse.

// channelGateConfirm is what a channel gate confirms with: the request's
// mfa_code and the grace read before BeginTx. The zero value carries neither,
// so a gate that fires on it prompts.
type channelGateConfirm struct {
	code  string
	grace stepup.GraceRead
}

// chargeChannelGate is a channel gate's pre-transaction half: it charges the
// dangerous-action budget when the request carries a code, whatever the
// server's setting (C6), then reads the ManageChannels grace when mayFire. A
// request whose gate cannot fire skips that read, so it costs no Redis call
// unless it sent a code. It writes the charge's refusal and returns false. A
// grace that cannot be read is no grace, so the actor is prompted.
func (h *Handler) chargeChannelGate(c *gin.Context, serverID, userID, code string, mayFire bool) (channelGateConfirm, bool) {
	ctx := c.Request.Context()
	if e := stepup.Charge(ctx, stepup.DangerousActionBudget(h.redis), userID, code); e != nil {
		mfaenforce.WriteError(c, h.log, e, nil)
		return channelGateConfirm{}, false
	}
	confirm := channelGateConfirm{code: code}
	if !mayFire {
		return confirm, true
	}
	server, serverErr := uuid.Parse(serverID)
	actor, actorErr := uuid.Parse(userID)
	if serverErr == nil && actorErr == nil { // a zero read covers nothing: the actor is prompted
		confirm.grace = stepup.NewGraceStore(h.redis).Read(ctx, stepup.GraceActorFromContext(c, actor),
			stepup.DangerousActionGraceScope(server, int64(rbac.PermManageChannels)))
	}
	return confirm, true
}

// requireChannelGate is the gate's confirmation, on the route's transaction
// after LockGateTx and the route's own ManageChannels check (I7), and before
// the write. fires is the route's predicate. The owner reaches it too (I-ID).
func (h *Handler) requireChannelGate(
	ctx context.Context, tx *sql.Tx, g mfaenforce.Gate, userID string, purpose stepup.Purpose,
	in channelGateConfirm, fires bool,
) (mfaenforce.Outcome, error) {
	outcome, e := mfaenforce.Require(ctx, tx, g, mfaenforce.Confirm{
		Verifier: h.mfaVerifier,
		ActorID:  userID,
		Purpose:  purpose,
		Code:     in.code,
		Grace:    in.grace,
	}, fires)
	if e != nil {
		return outcome, e
	}
	// Not `return outcome, e`: a nil *stepup.Error in an error is non-nil.
	return outcome, nil
}

// settleChannelGate runs once the route's transaction has COMMITTED: on a
// verified outcome it clears the budget and grants the grace. Any other
// outcome settles nothing, so a grace-covered action never extends its grace.
func (h *Handler) settleChannelGate(ctx context.Context, outcome mfaenforce.Outcome, in channelGateConfirm, userID string) {
	mfaenforce.Settle(ctx, h.log, outcome, stepup.DangerousActionBudget(h.redis), stepup.NewGraceStore(h.redis),
		in.grace, userID)
}

// manageChannelsDenial answers a ManageChannels denial on q, the querier the
// denial came from, at the denial's own scope (RS5, A-11): the enrollment
// refusal when an inline factor would lift it, otherwise
// errManageChannelsDenied. channelID is empty for DeleteChannel, whose checks
// resolve ManageChannels at server scope, and the channel for the expiration
// route, whose check resolves it at channel scope; a channel ALLOW that the
// denied check never consulted cannot be lifted by a factor, so it must not
// earn the enrollment refusal. Call it only after the denial (I7).
func manageChannelsDenial(ctx context.Context, q stepup.RowQuerier, serverID, channelID, userID string) error {
	if e := rbac.EnrollmentDenial(ctx, q, serverID, channelID, userID, rbac.PermManageChannels); e != nil {
		return e
	}
	return errManageChannelsDenied
}

// channelExpirationRequest is the channel expiration PATCH body: the shared
// expiration request plus mfa_code, which confirms a shortening on a server
// that enforces MFA on dangerous actions (#3454 A-6). It is a plain field, not
// stepup.Fields, because this is not an own-rule route: it neither refuses
// current_password nor accepts step_up_token. It lives here rather than on
// expiration.Request because the DM expiration PATCH decodes that type and has
// no gate, so a field there would be a code that route silently ignores.
type channelExpirationRequest struct {
	expiration.Request
	MFACode string `json:"mfa_code" binding:"max=256"`
}

// UnmarshalJSON takes mfa_code out of the document and hands the rest to
// expiration.Request's closed-schema decoder, so every other field is
// accepted or refused exactly as before.
func (r *channelExpirationRequest) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields["mfa_code"]; ok {
		if err := json.Unmarshal(raw, &r.MFACode); err != nil {
			return err
		}
		delete(fields, "mfa_code")
		rest, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		data = rest
	}
	return r.Request.UnmarshalJSON(data)
}

// shortensRetention is the expiration gate's predicate (#3454 §4, A-12): a
// finite new window that shortens how long some message is kept. That holds
// when no window was in force, when the new one is shorter, or when the set
// applies the window retroactively: "apply" back-fills expires_at onto every
// message an earlier "new_only" policy left unstamped, so even the same or a
// longer window turns kept history into already-expired rows. prior is the
// channel's window read under the transaction's channel lock; NULL means
// messages never expire. Clearing the policy (a nil window) and lengthening
// it for new messages only are ungated.
func shortensRetention(prior sql.NullInt64, window *int, retroactive string) bool {
	if window == nil {
		return false
	}
	return !prior.Valid || int64(*window) < prior.Int64 || retroactive == retroactiveApply
}

// retroactiveApply is the expiration set mode that stamps existing messages.
const retroactiveApply = "apply"

// respondChannelExpirationError answers a refusal or failure inside the
// expiration transaction: the route's own ManageChannels denial, then the
// gate's refusals (a vanished server is answered as the channel it took with
// it), then the route's own 500. It is never the ambiguous-commit 503, which
// UpdateExpiration answers with the candidate policy and no error key.
func (h *Handler) respondChannelExpirationError(c *gin.Context, err error) {
	if errors.Is(err, errManageChannelsDenied) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
		return
	}
	if mfaenforce.IsGateError(err) {
		mfaenforce.WriteError(c, h.log, err, writeChannelNotFound)
		return
	}
	h.log.Error("Failed to authorize channel expiration", "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdateChannel})
}

func writeChannelNotFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": errMsgChannelNotFound})
}

// writeDeleteChannelServerGone is DeleteChannel's answer for a server that
// vanished mid-request: the authority capture answers it with
// rbac.ErrNotMember, a 403 (#3508), and the gate's ErrServerNotFound, which
// the capture's servers FOR UPDATE makes unreachable, gets the same.
func writeDeleteChannelServerGone(c *gin.Context) {
	c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPerms})
}
