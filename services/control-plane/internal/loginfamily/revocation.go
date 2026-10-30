package loginfamily

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

func validCause(cause AuthCause) bool {
	if cause.Scope == ScopeSession {
		return cause.Reason == ReasonLogout || cause.Reason == ReasonExplicitRevoke
	}
	if cause.Scope != ScopeUser {
		return false
	}
	switch cause.Reason {
	case ReasonExplicitRevoke, ReasonDisplacement, ReasonTheft, ReasonDisablement, ReasonErasure:
		return true
	}
	return false
}

// RevokeFamiliesTx terminalizes the captured frontier under one sealed auth operation.
// Its result is uncommitted until the caller obtains definite commit success.
func RevokeFamiliesTx(ctx context.Context, tx *sql.Tx, targets Targets, cause AuthCause) (RevocationResult, error) {
	if err := checkTargets(ctx, tx, targets); err != nil {
		return RevocationResult{}, err
	}
	if !validCause(cause) {
		return RevocationResult{}, NewError(ReasonArgumentInvalid, nil)
	}
	return revokeCaptured(ctx, tx, targets, cause, nil)
}

// RevokeFamiliesForEpochTx revalidates the existing epoch owner and terminalizes its captured frontier.
// It retains earlier terminal owners and never creates a replacement epoch parent.
func RevokeFamiliesForEpochTx(ctx context.Context, tx *sql.Tx, targets Targets, owner EpochOwner) (RevocationResult, error) {
	if err := checkTargets(ctx, tx, targets); err != nil {
		return RevocationResult{}, err
	}
	if targets.epoch == nil || owner.Generation == uuid.Nil || owner.UserID != targets.user.userID || owner.SupersededCredentialEpoch != *targets.epoch || !validEpoch(owner.SupersededCredentialEpoch) || !validEpoch(owner.CredentialEpoch) || owner.CredentialEpoch == "" || owner.CredentialEpoch == owner.SupersededCredentialEpoch {
		return RevocationResult{}, NewError(ReasonOwnershipInvalid, nil)
	}
	// The locator is not authority. Lock and revalidate the actual existing
	// parent, including its replacement epoch; never recreate a completed parent.
	var generation uuid.UUID
	err := tx.QueryRowContext(ctx, `SELECT generation FROM credential_epoch_voice_ejections
 WHERE user_id=$1 AND superseded_credential_epoch=$2 AND credential_epoch=$3 AND generation=$4 FOR SHARE`, owner.UserID, owner.SupersededCredentialEpoch, owner.CredentialEpoch, owner.Generation).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return RevocationResult{}, NewError(ReasonOwnershipInvalid, nil)
	}
	if err != nil {
		return RevocationResult{}, sqlFailure(err)
	}
	return revokeCaptured(ctx, tx, targets, AuthCause{}, &owner)
}
func revokeCaptured(ctx context.Context, tx *sql.Tx, targets Targets, cause AuthCause, epoch *EpochOwner) (RevocationResult, error) {
	run := capturedRevocation{tx: tx, targets: targets, cause: cause, epoch: epoch}
	after := uuid.Nil
	for {
		page, err := capturedPage(ctx, tx, targets, after)
		if err != nil {
			return RevocationResult{}, err
		}
		for _, f := range page {
			if err = run.revokeFamily(ctx, f); err != nil {
				return RevocationResult{}, err
			}
		}
		if len(page) < targetPageSize {
			break
		}
		after = page[len(page)-1].binding.FamilyID
	}
	if run.operation != uuid.Nil {
		if err := sealRevocationOperation(ctx, tx, run.operation); err != nil {
			return RevocationResult{}, err
		}
	}
	owners, err := capturedOwners(ctx, tx, targets)
	if err != nil {
		return RevocationResult{}, err
	}
	run.result.Owners = owners
	return run.result, nil
}

// capturedRevocation is one revokeCaptured pass; operation opens on first use.
type capturedRevocation struct {
	tx        *sql.Tx
	targets   Targets
	cause     AuthCause
	epoch     *EpochOwner
	operation uuid.UUID
	result    RevocationResult
}

// revokeFamily terminalizes one live family, opening the operation on first use.
func (r *capturedRevocation) revokeFamily(ctx context.Context, f familyRecord) error {
	if f.terminal.Valid {
		return nil
	}
	if r.epoch != nil && f.binding.CredentialEpoch != r.epoch.SupersededCredentialEpoch {
		return NewError(ReasonOwnershipInvalid, nil)
	}
	if r.epoch == nil && r.operation == uuid.Nil {
		operation, err := createOperation(ctx, r.tx, r.targets.user.userID, r.cause)
		if err != nil {
			return err
		}
		r.operation = operation
	}
	count, err := terminalizeFamily(ctx, r.tx, f, r.operation, r.epoch)
	if err != nil {
		return err
	}
	r.result.NewlyTerminalFamilies++
	r.result.RevokedRefreshRows += count
	return nil
}

// sealRevocationOperation closes an operation to further families exactly once.
func sealRevocationOperation(ctx context.Context, tx *sql.Tx, operation uuid.UUID) error {
	res, err := tx.ExecContext(ctx, `UPDATE login_family_revocations SET sealed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE operation_id=$1 AND sealed_at IS NULL`, operation)
	if err != nil {
		return sqlFailure(err)
	}
	return oneChanged(res)
}
func createOperation(ctx context.Context, tx *sql.Tx, user uuid.UUID, cause AuthCause) (uuid.UUID, error) {
	id, err := newToken()
	if err != nil {
		return uuid.Nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO login_family_revocations(operation_id,user_id,scope,reason) VALUES ($1,$2,$3,$4)`, id, user, cause.Scope, cause.Reason); err != nil {
		return uuid.Nil, sqlFailure(err)
	}
	return id, nil
}
func oneChanged(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return sqlFailure(err)
	}
	if count != 1 {
		return NewError(ReasonOwnershipInvalid, nil)
	}
	return nil
}
func terminalizeFamily(ctx context.Context, tx *sql.Tx, family familyRecord, operation uuid.UUID, epoch *EpochOwner) (int64, error) {
	var authID, epochID any
	if epoch == nil {
		authID = operation
	} else {
		epochID = epoch.Generation
	}
	result, err := tx.ExecContext(ctx, `UPDATE login_families SET terminal_at=clock_timestamp(),auth_operation_id=$3,epoch_generation=$4,updated_at=clock_timestamp()
 WHERE family_id=$1 AND user_id=$2 AND terminal_at IS NULL`, family.binding.FamilyID, family.binding.UserID, authID, epochID)
	if err != nil {
		return 0, sqlFailure(err)
	}
	if err = oneChanged(result); err != nil {
		return 0, err
	}
	if epoch == nil {
		if _, err = tx.ExecContext(ctx, `INSERT INTO login_family_revocation_targets(operation_id,family_id,user_id) VALUES ($1,$2,$3)`, operation, family.binding.FamilyID, family.binding.UserID); err != nil {
			return 0, sqlFailure(err)
		}
	}
	// Preserve endpoint count semantics: expired rows still count if previously
	// unrevoked. Already-terminal families never acquire a new owner or sweep.
	result, err = tx.ExecContext(ctx, `UPDATE refresh_tokens r SET revoked_at=clock_timestamp()
 WHERE r.user_id=$1 AND r.revoked_at IS NULL AND EXISTS (SELECT 1 FROM login_family_sids s
 WHERE s.sid=r.id AND s.user_id=$1 AND s.family_id=$2)`, family.binding.UserID, family.binding.FamilyID)
	if err != nil {
		return 0, sqlFailure(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, sqlFailure(err)
	}
	return count, nil
}
func capturedOwners(ctx context.Context, tx *sql.Tx, targets Targets) (owners []DeliveryOwner, retErr error) {
	// DISTINCT and sorting happen in SQL. Go retains only the ABI-required owner
	// result, never a second copy/map proportional to the family frontier.
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT CASE WHEN f.auth_operation_id IS NOT NULL THEN 'auth' ELSE 'epoch' END AS kind,
 f.user_id,f.auth_operation_id,f.epoch_generation
 FROM pg_temp.loginfamily_targets t JOIN login_families f ON f.family_id=t.family_id
 WHERE t.capture=$1 AND f.user_id=$2 AND f.terminal_at IS NOT NULL
 ORDER BY kind,f.user_id,f.auth_operation_id,f.epoch_generation`, targets.token, targets.user.userID)
	if err != nil {
		return nil, sqlFailure(err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			owners = nil
			retErr = sqlFailure(errors.Join(retErr, closeErr))
		}
	}()
	owners = make([]DeliveryOwner, 0)
	for rows.Next() {
		var owner DeliveryOwner
		var authID, epochID uuid.NullUUID
		if err = rows.Scan(&owner.Kind, &owner.UserID, &authID, &epochID); err != nil {
			return nil, sqlFailure(err)
		}
		if (owner.Kind == OwnerAuth && (!authID.Valid || epochID.Valid)) || (owner.Kind == OwnerEpoch && (!epochID.Valid || authID.Valid)) {
			return nil, NewError(ReasonOwnershipInvalid, nil)
		}
		owner.AuthOperationID = authID.UUID
		owner.EpochGeneration = epochID.UUID
		owners = append(owners, owner)
	}
	if err = rows.Err(); err != nil {
		return nil, sqlFailure(err)
	}
	return owners, nil
}
