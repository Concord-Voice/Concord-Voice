package media

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// The dangerous-action gate on the server icon and banner uploads (#3454 §4).
// On a server that enforces MFA on dangerous actions, an upload asks the actor
// for an inline factor under the gate's locks; on one that does not, a request
// with no mfa_code is answered exactly as before. See mfaenforce's
// dangerous.go for the sequence every gated route follows.
//
// The object store cannot roll back, and the toggle must not flip between the
// gate's read and the write it authorizes, so the gate transaction is held
// OPEN across PutObject (C3): a flip waits for the upload instead of racing
// it. The hold is bounded three ways: lock_timeout 3s on every lock wait,
// idle_in_transaction_session_timeout 10s, and a 5s context on PutObject.
//
// Media never writes the servers row, so the gate takes it FOR SHARE.

// serverImagePutTimeout bounds the PutObject made while the gate's locks are
// held (I-BOUND).
const serverImagePutTimeout = 5 * time.Second

// errMsgInsufficientPermissions is the uploads' generic ManageServer 403, unchanged.
const errMsgInsufficientPermissions = "Insufficient permissions"

var (
	// errServerImageForbidden is a ManageServer denial that no inline factor
	// would lift.
	errServerImageForbidden = errors.New("media: not permitted to manage the server")
	// errServerImageStore is a failed PutObject. The transaction rolls back, so
	// no row is written and no factor stays spent.
	errServerImageStore = errors.New("media: store server image")
	// errServerImagePerms is a failed in-transaction permission read.
	errServerImagePerms = errors.New("media: resolve server permissions")
)

// serverImageUpload is one processed server icon or banner, ready to store.
type serverImageUpload struct {
	userID, serverID, purpose string
	// code is the multipart mfa_code field, or "".
	code         string
	store        ObjectStore
	processed    *ProcessedImage
	originalSize int64
}

// isServerImagePurpose reports whether purpose is a server icon or banner.
func isServerImagePurpose(purpose string) bool {
	return purpose == purposeServerIcon || purpose == purposeServerBanner
}

// serverImagePurpose is the step-up purpose of a server image upload: one per
// route, so a WebAuthn token minted for the icon is not spendable on the
// banner.
func serverImagePurpose(purpose string) stepup.Purpose {
	if purpose == purposeServerBanner {
		return stepup.PurposeServerBannerUpload
	}
	return stepup.PurposeServerIconUpload
}

// readServerImageMFACode reads a server image upload's multipart mfa_code
// (A-6), at most 256 characters; any other purpose has none. Longer is a 400
// that has written its response. The code is read, never trusted: the gate
// decides whether it is asked for.
func readServerImageMFACode(c *gin.Context, purpose string) (string, bool) {
	if !isServerImagePurpose(purpose) {
		return "", true
	}
	// Fields.Input is the one place the cap and its 400 body are spelled. Only
	// mfa_code is set, so its other refusals (current_password, step_up_token)
	// cannot fire: this route accepts neither.
	in, e := stepup.Fields{MFACode: c.PostForm("mfa_code")}.Input()
	if e != nil {
		e.Write(c)
		return "", false
	}
	return in.MFACode, true
}

// manageServerDenial answers a ManageServer denial on q, the querier the
// denial came from (RS5, A-11): the enrollment refusal when an inline factor
// would lift it, otherwise errServerImageForbidden. Call it only after the
// denial (I7).
func manageServerDenial(ctx context.Context, q RowQuerier, serverID, userID string) error {
	if e := rbac.EnrollmentDenial(ctx, q, serverID, "", userID, rbac.PermManageServer); e != nil {
		return e
	}
	return errServerImageForbidden
}

// storeServerImage stores a processed server icon or banner behind the gate
// and answers the request. up.processed is already parsed and processed:
// nothing here runs before the transaction but the budget charge and the
// grace read.
func (h *Handler) storeServerImage(c *gin.Context, up serverImageUpload) {
	fileID, err := h.storeServerImageGated(c, up)
	if err != nil {
		h.respondServerImageError(c, err)
		return
	}
	storageKey := tier1StorageKey(up.purpose, up.userID, up.serverID, "")
	h.respondTier1Upload(c, up.purpose, up.userID, storageKey, fileID, up.originalSize, up.processed)
}

// storeServerImageGated runs the upload's gate (#3454 §4): the budget charge
// and the grace read before the transaction (A-2, A-9); in it, LockGateTx
// (users FOR SHARE, then servers FOR SHARE), the ManageServer check under
// those locks, Require, PutObject, then the media_files row; after the commit,
// Settle. It returns the new row's id.
func (h *Handler) storeServerImageGated(c *gin.Context, up serverImageUpload) (string, error) {
	ctx := c.Request.Context()
	budget := stepup.DangerousActionBudget(h.sessionRedis)
	if e := stepup.Charge(ctx, budget, up.userID, up.code); e != nil {
		return "", e
	}
	graceStore := stepup.NewGraceStore(h.sessionRedis)
	grace := readServerImageGrace(c, graceStore, up.serverID, up.userID)

	var (
		fileID  string
		outcome mfaenforce.Outcome
	)
	err := mfaenforce.WithGateTx(ctx, h.db, mfaenforce.GateSpec{
		ServerID: up.serverID, ActorID: up.userID,
		UserLock: stepup.LockForShare, ServerLock: mfaenforce.ServerForShare,
		TokenEpoch:         middleware.TokenCredentialEpoch(c),
		HoldsExternalWrite: true,
	}, func(tx *sql.Tx, g mfaenforce.Gate) error {
		var err error
		fileID, outcome, err = h.admitAndStoreServerImage(ctx, tx, g, up, grace)
		return err
	})
	if err != nil {
		return "", err
	}
	mfaenforce.Settle(ctx, h.log, outcome, budget, graceStore, grace, up.userID)
	return fileID, nil
}

// readServerImageGrace is the grace pre-read for a ManageServer-gated action
// on serverID. An id that is not a uuid reads as no grace, so the actor is
// prompted.
func readServerImageGrace(c *gin.Context, store stepup.GraceStore, serverID, userID string) stepup.GraceRead {
	server, serverErr := uuid.Parse(serverID)
	actor, actorErr := uuid.Parse(userID)
	if serverErr != nil || actorErr != nil {
		return stepup.GraceRead{}
	}
	return store.Read(c.Request.Context(), stepup.GraceActorFromContext(c, actor),
		stepup.DangerousActionGraceScope(server, int64(rbac.PermManageServer)))
}

// admitAndStoreServerImage is the gate transaction's body, in the order I7
// fixes: the route's own permission verdict, then Require, then the writes.
// A refusal leaves nothing behind: no object, no media_files row.
//
// PutObject comes before the row INSERT, as it always has, but a failure of
// the INSERT or of the commit does NOT delete the object (C4). The key is
// fixed (server-icons/<id>, server-banners/<id>), so the object just written
// replaced the live one, and a delete would leave servers.icon_url pointing at
// nothing. The new image stays; the row the failed transaction did not write
// is reconciled by the next upload's upsert.
func (h *Handler) admitAndStoreServerImage(
	ctx context.Context, tx *sql.Tx, g mfaenforce.Gate, up serverImageUpload, grace stepup.GraceRead,
) (string, mfaenforce.Outcome, error) {
	if err := h.authorizeManageServerTx(ctx, tx, up.serverID, up.userID); err != nil {
		return "", mfaenforce.Unconfirmed, err
	}
	outcome, e := mfaenforce.Require(ctx, tx, g, mfaenforce.Confirm{
		Verifier: h.mfaVerifier, ActorID: up.userID, Purpose: serverImagePurpose(up.purpose), Code: up.code, Grace: grace,
	}, true)
	if e != nil {
		return "", mfaenforce.Unconfirmed, e
	}

	storageKey := tier1StorageKey(up.purpose, up.userID, up.serverID, "")
	putCtx, cancel := context.WithTimeout(ctx, serverImagePutTimeout)
	defer cancel()
	if err := up.store.PutObject(putCtx, storageKey, bytes.NewReader(up.processed.Data), int64(len(up.processed.Data)), up.processed.ContentType); err != nil {
		return "", outcome, fmt.Errorf("%w: %w", errServerImageStore, err)
	}
	fileID, err := insertTier1Row(ctx, tx, up.userID, storageKey, up.processed)
	if err != nil {
		return "", outcome, fmt.Errorf("record server image: %w", err)
	}
	return fileID, outcome, nil
}

// authorizeManageServerTx is the upload's ManageServer check under the gate's
// locks, on tx, mirroring userCanManageServer's pooled check (I7). A member
// who lost the bit, or their membership, since that check gets
// manageServerDenial on tx.
func (h *Handler) authorizeManageServerTx(ctx context.Context, tx *sql.Tx, serverID, userID string) error {
	if h.resolver == nil {
		// Membership only, like userCanManageServer without a resolver (tests).
		var member bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2)`,
			serverID, userID).Scan(&member); err != nil {
			return fmt.Errorf("%w: %w", errServerImagePerms, err)
		}
		if member {
			return nil
		}
		return errServerImageForbidden
	}
	perms, err := h.resolver.ResolveServerPermissionsTx(ctx, tx, serverID, userID)
	if err != nil && !errors.Is(err, rbac.ErrNotMember) {
		return fmt.Errorf("%w: %w", errServerImagePerms, err)
	}
	if err == nil && perms.Has(rbac.PermManageServer) {
		return nil
	}
	return manageServerDenial(ctx, tx, serverID, userID)
}

// respondServerImageError answers a refused or failed server image upload: the
// route's own denial first, then the gate's refusals, then the route's own
// 500s. A gate error is answered by mfaenforce.WriteError (a lock conflict, a
// *stepup.Error, or a vanished server answered with the route's 403, #3508);
// anything else keeps this route's existing 500 body, which WriteError's
// generic one would change. The store and permission sentinels are checked
// after the gate's because a lock timeout inside either read is a lock
// conflict and must stay a 503.
func (h *Handler) respondServerImageError(c *gin.Context, err error) {
	if errors.Is(err, errServerImageForbidden) {
		writeInsufficientPermissions(c)
		return
	}
	if mfaenforce.IsGateError(err) {
		mfaenforce.WriteError(c, h.log, err, writeInsufficientPermissions)
		return
	}
	userID := c.GetString("user_id")
	switch {
	case errors.Is(err, errServerImageStore):
		h.log.Error("Failed to store processed image", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedStoreImage})
	case errors.Is(err, errServerImagePerms):
		h.log.Error("Failed to check server permission", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedVerifyPerms})
	default:
		h.log.Error(errMsgFailedRecordMediaMetadata, "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgFailedRecordMediaMetadata})
	}
}

func writeInsufficientPermissions(c *gin.Context) {
	c.JSON(http.StatusForbidden, gin.H{"error": errMsgInsufficientPermissions})
}
