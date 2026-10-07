package servers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// The dangerous-action gates on UpdateServer and DeleteServer (#3454). On a
// server that enforces MFA on dangerous actions, both ask the actor for an
// inline factor under the gate's locks; on one that does not, a request with
// no mfa_code is answered exactly as before. See mfaenforce's dangerous.go for
// the sequence each follows.

// errMsgUpdateServerForbidden is UpdateServer's generic 403 body, unchanged.
const errMsgUpdateServerForbidden = "insufficient permissions"

// errUpdateServerForbidden is UpdateServer's ManageServer denial that no
// inline factor would lift.
var errUpdateServerForbidden = errors.New("servers: not permitted to update the server")

// manageServerDenial answers a ManageServer denial on q, the querier the
// denial came from (RS5, A-11): the enrollment refusal when an inline factor
// would lift it, otherwise errUpdateServerForbidden. Call it only after the
// denial (I7).
func manageServerDenial(ctx context.Context, q stepup.RowQuerier, serverID, userID string) error {
	if e := rbac.EnrollmentDenial(ctx, q, serverID, "", userID, rbac.PermManageServer); e != nil {
		return e
	}
	return errUpdateServerForbidden
}

// updateServerGated runs UpdateServer's write in a gate transaction (#3454 §4):
// the budget charge and the grace pre-read before it (A-9); in it, LockGateTx
// (users FOR SHARE, then servers FOR NO KEY UPDATE, the lock the UPDATE takes
// anyway), the ManageServer check under those locks, Require on every edit,
// then the UPDATE; after the commit, Settle. The UPDATE is updateQuery, built
// by the caller from buildUpdateClauses. serverID is the path parameter as
// sent, which the response echoes; serverUUID is its parsed form.
func (h *Handler) updateServerGated(
	c *gin.Context, serverID string, serverUUID uuid.UUID, userID, code, updateQuery string, args []any,
) (models.Server, error) {
	ctx := c.Request.Context()
	budget := stepup.DangerousActionBudget(h.redis)
	if e := stepup.Charge(ctx, budget, userID, code); e != nil {
		return models.Server{}, e
	}
	store := stepup.NewGraceStore(h.redis)
	var grace stepup.GraceRead
	if actor, err := uuid.Parse(userID); err == nil { // a zero read covers nothing: the actor is prompted
		grace = store.Read(ctx, stepup.GraceActorFromContext(c, actor),
			stepup.DangerousActionGraceScope(serverUUID, int64(rbac.PermManageServer)))
	}

	server := models.Server{ID: serverID}
	var outcome mfaenforce.Outcome
	err := mfaenforce.WithGateTx(ctx, h.db, mfaenforce.GateSpec{
		ServerID: serverID, ActorID: userID,
		UserLock: stepup.LockForShare, ServerLock: mfaenforce.ServerForNoKeyUpdate,
		TokenEpoch: middleware.TokenCredentialEpoch(c),
	}, func(tx *sql.Tx, g mfaenforce.Gate) error {
		if err := h.authorizeManageServerTx(ctx, tx, serverID, userID); err != nil {
			return err
		}
		var e *stepup.Error
		outcome, e = mfaenforce.Require(ctx, tx, g, mfaenforce.Confirm{
			Verifier: h.mfaVerifier, ActorID: userID, Purpose: stepup.PurposeServerUpdate, Code: code, Grace: grace,
		}, true)
		if e != nil {
			return e
		}
		// nosemgrep: go.net.sql.go-vanillasql-format-string-sqli-taint-med-conf.go-vanillasql-format-string-sqli-taint-med-conf,go.net.sql.go-vanillasql-format-string-sqli-taint.go-vanillasql-format-string-sqli-taint
		err := tx.QueryRowContext(ctx, updateQuery, args...).Scan( //nolint:gosec // updateQuery composed by buildUpdateClauses: hardcoded column names + integer argIdx via fmt.Sprintf; user values flow only through args... as parameterized $N placeholders. See matching nosemgrep on the fmt.Sprintf in UpdateServer.
			&server.Name, &server.IconURL, &server.BannerURL, &server.OwnerID, &server.AllowEmbeddedContent, &server.CreatedAt, &server.UpdatedAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return mfaenforce.ErrServerNotFound // unreachable under the gate's lock; answered as a vanished server
		}
		if err != nil {
			return fmt.Errorf("update server: %w", err)
		}
		return nil
	})
	if err != nil {
		return models.Server{}, err
	}
	mfaenforce.Settle(ctx, h.log, outcome, budget, store, grace, userID)
	return server, nil
}

// authorizeManageServerTx is UpdateServer's ManageServer check under the gate's
// locks, on tx, mirroring the pooled HasPermission check (I7). A member who
// lost the bit, or their membership, since that check gets manageServerDenial
// on tx.
func (h *Handler) authorizeManageServerTx(ctx context.Context, tx *sql.Tx, serverID, userID string) error {
	perms, err := h.resolver.ResolveServerPermissionsTx(ctx, tx, serverID, userID)
	if err != nil && !errors.Is(err, rbac.ErrNotMember) {
		return fmt.Errorf("resolve server permissions: %w", err)
	}
	if err == nil && perms.Has(rbac.PermManageServer) {
		return nil
	}
	return manageServerDenial(ctx, tx, serverID, userID)
}

// respondUpdateServerError answers a refused or failed update: the route's
// own denial first, then the gate's refusals, then the route's own 500.
func (h *Handler) respondUpdateServerError(c *gin.Context, err error) {
	if errors.Is(err, errUpdateServerForbidden) {
		c.JSON(http.StatusForbidden, gin.H{"error": errMsgUpdateServerForbidden})
		return
	}
	if h.respondGateError(c, err) {
		return
	}
	h.log.Error("Failed to update server", "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedUpdate})
}

// gateServerDeleteTx is the deletion's dangerous-action gate (#3454 §4), in
// place of the servers FOR UPDATE it replaces: LockGateTx re-takes the
// actor's users row, already held FOR NO KEY UPDATE by lockServerDeleteUsers,
// then locks the servers row FOR UPDATE for the DELETE. The owner check reads
// Gate.OwnerID under that lock, and Require runs for every deletion under
// stepup.PurposeServerDelete, with no grace: the purpose is always fresh.
//
// RS5 has only its owner arm here (§14.1). A non-owner's refusal is an
// identity check no factor lifts, and an unenrolled owner on an enforcing
// server reaches Require, whose refusal is the enrollment one.
func (h *Handler) gateServerDeleteTx(
	ctx context.Context, tx *sql.Tx, serverID, userID string, confirm serverDeleteConfirm,
) (mfaenforce.Outcome, error) {
	g, err := mfaenforce.LockGateTx(ctx, tx, serverID, userID,
		stepup.LockForNoKeyUpdate, mfaenforce.ServerForUpdate, confirm.tokenEpoch)
	if errors.Is(err, mfaenforce.ErrServerNotFound) {
		return mfaenforce.Unconfirmed, errServerDeleteNotFound
	}
	if err != nil {
		return mfaenforce.Unconfirmed, err
	}
	if g.OwnerID != userID {
		return mfaenforce.Unconfirmed, errServerDeleteNotOwner
	}
	outcome, e := mfaenforce.Require(ctx, tx, g, mfaenforce.Confirm{
		Verifier: h.mfaVerifier, ActorID: userID, Purpose: stepup.PurposeServerDelete, Code: confirm.mfaCode,
	}, true)
	if e != nil {
		return mfaenforce.Unconfirmed, e
	}
	return outcome, nil
}

// respondGateError answers err through mfaenforce.WriteError when the gate
// produced it (a lock conflict, a *stepup.Error, or a server that vanished
// under the gate, answered with this package's 404) and reports whether it
// did. Anything else is the route's own failure, answered with the route's
// own 500 body: WriteError's generic one would change it.
func (h *Handler) respondGateError(c *gin.Context, err error) bool {
	if !mfaenforce.IsGateError(err) {
		return false
	}
	mfaenforce.WriteError(c, h.log, err, writeServerNotFound)
	return true
}

func writeServerNotFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": errMsgServerNotFound})
}
