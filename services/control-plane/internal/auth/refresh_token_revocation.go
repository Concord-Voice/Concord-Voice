package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/google/uuid"
)

// ErrNoActiveRefreshSession reports that the requested preserved family cannot be verified.
var ErrNoActiveRefreshSession = errors.New("no active refresh session")

// RevokeAllRefreshTokensOptions controls a serialized captured-family revocation.
type RevokeAllRefreshTokensOptions struct {
	ExcludeTokenHash        string
	ExcludeSessionID        string
	ExcludeAccessExpiresAt  time.Time
	ExpectedCredentialEpoch string
	EnforceCredentialEpoch  bool
}

// RevokeAllRefreshTokens commits a fixed family frontier. Counts describe changed
// refresh rows, including expired rows; Owners retain original delivery ownership.
func RevokeAllRefreshTokens(ctx context.Context, db *sql.DB, userID string, options RevokeAllRefreshTokensOptions) (result loginfamily.RevocationResult, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, loginfamily.NewError(loginfamily.ReasonSQLUnavailable, err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, loginfamily.NewError(loginfamily.ReasonSQLUnavailable, rollbackErr))
			result = loginfamily.RevocationResult{}
		}
	}()
	user, err := lockMintUserTx(ctx, tx, userID)
	if err != nil {
		return result, err
	}
	if err = matchRevocationEpochTx(ctx, tx, userID, options); err != nil {
		return result, err
	}
	preserve, err := preservedFamilyIdentityTx(ctx, tx, userID, options)
	if err != nil {
		return result, err
	}
	targets, err := loginfamily.CaptureUserTargetsTx(ctx, tx, user, preserve)
	if err != nil {
		var classified *loginfamily.Error
		if errors.As(err, &classified) && classified.Reason() == loginfamily.ReasonPreserveUnknown {
			return result, ErrNoActiveRefreshSession
		}
		return result, err
	}
	result, err = loginfamily.RevokeFamiliesTx(ctx, tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeUser, Reason: loginfamily.ReasonExplicitRevoke})
	if err != nil {
		return loginfamily.RevocationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		// The revocation may be durable: keep only the owner locators, so the
		// caller can fence whatever committed without reporting success.
		return loginfamily.RevocationResult{Owners: result.Owners}, loginfamily.NewError(loginfamily.ReasonCommitUnknown, err)
	}
	return result, nil
}

func matchRevocationEpochTx(ctx context.Context, tx *sql.Tx, userID string, options RevokeAllRefreshTokensOptions) error {
	if !options.EnforceCredentialEpoch {
		return nil
	}
	var epoch sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT credential_epoch FROM users WHERE id=$1`, userID).Scan(&epoch); err != nil {
		return loginfamily.NewError(loginfamily.ReasonSQLUnavailable, err)
	}
	if err := credepoch.MatchEpoch(epoch, options.ExpectedCredentialEpoch); err != nil {
		return err
	}
	// Session revocation does not rotate the epoch, so the epoch alone cannot
	// tell that the requesting login was revoked while this request waited.
	// Revalidate the middleware-verified family under the user lock just taken.
	identity, ok := loginfamily.AccessIdentityFromContext(ctx)
	if !ok || identity.UserID.String() != userID || identity.CredentialEpoch != options.ExpectedCredentialEpoch {
		return credepoch.ErrEpochMismatch
	}
	return loginfamily.GuardLockedActorFamilyTx(ctx, tx, identity)
}

// A selected bearer SID never falls through to an ambient cookie. Immutable SID
// membership survives refresh rotation and refresh-row retention cleanup.
func preservedFamilyIdentityTx(ctx context.Context, tx *sql.Tx, userID string, options RevokeAllRefreshTokensOptions) (*loginfamily.AccessIdentity, error) {
	if options.ExcludeSessionID == "" && options.ExcludeTokenHash == "" {
		return nil, nil
	}
	identity := &loginfamily.AccessIdentity{}
	var err error
	identity.UserID, err = uuid.Parse(userID)
	if err != nil {
		return nil, ErrNoActiveRefreshSession
	}
	query := `SELECT s.sid,s.access_expires_at,f.credential_epoch FROM login_family_sids s
 JOIN login_families f ON f.family_id=s.family_id AND f.user_id=s.user_id
 WHERE s.user_id=$1 AND s.sid=$2`
	target := options.ExcludeSessionID
	if target != "" {
		if _, err := uuid.Parse(target); err != nil {
			return nil, ErrNoActiveRefreshSession
		}
	} else {
		query = `SELECT s.sid,s.access_expires_at,f.credential_epoch FROM refresh_tokens r
  JOIN login_family_sids s ON s.sid=r.id AND s.user_id=r.user_id
  JOIN login_families f ON f.family_id=s.family_id AND f.user_id=s.user_id
  WHERE r.user_id=$1 AND r.token_hash=$2 AND r.revoked_at IS NULL AND r.expires_at>clock_timestamp()`
		target = options.ExcludeTokenHash
	}
	if err := tx.QueryRowContext(ctx, query, userID, target).Scan(&identity.SID, &identity.AccessExpiresAt, &identity.CredentialEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoActiveRefreshSession
		}
		return nil, loginfamily.NewError(loginfamily.ReasonSQLUnavailable, err)
	}
	if options.ExcludeSessionID != "" && !options.ExcludeAccessExpiresAt.IsZero() {
		identity.AccessExpiresAt = options.ExcludeAccessExpiresAt
	}
	if options.EnforceCredentialEpoch {
		identity.CredentialEpoch = options.ExpectedCredentialEpoch
	}
	return identity, nil
}

// RevokeRefreshSession terminalizes an owned SID's entire logical login, even
// when the selected refresh row has rotated, expired, or already been purged.
func RevokeRefreshSession(ctx context.Context, db *sql.DB, userID, sessionID, expectedEpoch string) (result loginfamily.RevocationResult, err error) {
	sid, parseErr := uuid.Parse(sessionID)
	if parseErr != nil || sid == uuid.Nil {
		return result, loginfamily.NewError(loginfamily.ReasonTargetUnknown, nil)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, loginfamily.NewError(loginfamily.ReasonSQLUnavailable, err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, loginfamily.NewError(loginfamily.ReasonSQLUnavailable, rollbackErr))
			result = loginfamily.RevocationResult{}
		}
	}()
	user, err := lockMintUserTx(ctx, tx, userID)
	if err != nil {
		return result, err
	}
	if err = matchRevocationEpochTx(ctx, tx, userID, RevokeAllRefreshTokensOptions{ExpectedCredentialEpoch: expectedEpoch, EnforceCredentialEpoch: true}); err != nil {
		return result, err
	}
	targets, err := loginfamily.ResolveTargetSIDTx(ctx, tx, user, sid)
	if err != nil {
		return result, err
	}
	result, err = loginfamily.RevokeFamiliesTx(ctx, tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeSession, Reason: loginfamily.ReasonExplicitRevoke})
	if err != nil {
		return loginfamily.RevocationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		// The revocation may be durable: keep only the owner locators, so the
		// caller can fence whatever committed without reporting success.
		return loginfamily.RevocationResult{Owners: result.Owners}, loginfamily.NewError(loginfamily.ReasonCommitUnknown, err)
	}
	return result, nil
}
