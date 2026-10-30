package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// lockMintUserTx is the early authority prefix for the six single-user mint
// owners. Call it exactly once, before any user or child lock in that transaction.
func lockMintUserTx(ctx context.Context, tx *sql.Tx, userID string) (loginfamily.UserAuthority, error) {
	id, err := uuid.Parse(userID)
	if err != nil || id == uuid.Nil {
		return loginfamily.UserAuthority{}, loginfamily.NewError(loginfamily.ReasonAccessIdentity, err)
	}
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	if err != nil {
		return loginfamily.UserAuthority{}, err
	}
	users, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{id}, loginfamily.UserNoKeyUpdate)
	if err != nil {
		return loginfamily.UserAuthority{}, err
	}
	return users.ForUser(id)
}

// prepareFamilyAccessTx binds precisely the deadline that Sign will emit. It
// neither commits nor publishes credentials; the transaction owner does both.
func prepareFamilyAccessTx(ctx context.Context, tx *sql.Tx, family loginfamily.LiveFamily, sid string, emailVerified bool, tier string) (PreparedAccessToken, error) {
	id, err := uuid.Parse(sid)
	if err != nil {
		return PreparedAccessToken{}, loginfamily.NewError(loginfamily.ReasonArgumentInvalid, err)
	}
	binding := family.Binding()
	prepared, err := PrepareAccessToken(AccessTokenInput{UserID: binding.UserID, SID: id,
		CredentialEpoch: binding.CredentialEpoch, EmailVerified: emailVerified, Tier: tier}, time.Now())
	if err != nil {
		return PreparedAccessToken{}, err
	}
	if err := loginfamily.BindSIDTx(ctx, tx, family, id, prepared.ExpiresAt()); err != nil {
		return PreparedAccessToken{}, err
	}
	return prepared, nil
}

func guardRefreshFamilyTx(ctx context.Context, tx *sql.Tx, user loginfamily.UserAuthority, sid string) (loginfamily.LiveFamily, error) {
	id, err := uuid.Parse(sid)
	if err != nil {
		return loginfamily.LiveFamily{}, loginfamily.NewError(loginfamily.ReasonAccessIdentity, err)
	}
	return loginfamily.GuardRefreshSIDTx(ctx, tx, user, id)
}

// verifyRefreshFamilySIDTx checks the exact successor against the already
// guarded family, without acquiring a potentially different family out of order.
// Membership is immutable, and the family's user and mutation locks are held.
func verifyRefreshFamilySIDTx(ctx context.Context, tx *sql.Tx, family loginfamily.LiveFamily, sid string) error {
	id, err := uuid.Parse(sid)
	if err != nil {
		return loginfamily.NewError(loginfamily.ReasonAccessIdentity, err)
	}
	binding := family.Binding()
	var matches bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
	 SELECT 1 FROM login_family_sids WHERE sid=$1 AND user_id=$2 AND family_id=$3)`,
		id, binding.UserID, binding.FamilyID).Scan(&matches); err != nil {
		return loginfamily.NewError(loginfamily.ReasonSQLUnavailable, err)
	}
	if !matches {
		return loginfamily.NewError(loginfamily.ReasonAccessIdentity, nil)
	}
	return nil
}

// writeLoginFamilyError preserves route-specific credential and internal-error
// envelopes, while distinguishing a failed authority lookup from denial.
func writeLoginFamilyError(c *gin.Context, err error, deniedMessage, internalMessage string) {
	switch {
	case errors.Is(err, loginfamily.ErrUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Authentication temporarily unavailable"})
	case errors.Is(err, loginfamily.ErrDenied):
		c.JSON(http.StatusUnauthorized, gin.H{"error": deniedMessage})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": internalMessage})
	}
}

func (h *Handler) rollbackLoginMint(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		h.log.Error("Login mint rollback failed")
	}
}

// displaceLoginRefreshRowsTx captures the existing machine-first/IP+UA match
// before any fresh family or refresh mutation. Empty metadata is not a wildcard.
func displaceLoginRefreshRowsTx(ctx context.Context, tx *sql.Tx, user loginfamily.UserAuthority, _ string, machineID, ipAddress, userAgent string) error {
	targets, err := loginfamily.CaptureDisplacementTargetsTx(ctx, tx, user, machineID, ipAddress, userAgent)
	if err != nil {
		return err
	}
	_, err = loginfamily.RevokeFamiliesTx(ctx, tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeUser, Reason: loginfamily.ReasonDisplacement})
	return err
}
