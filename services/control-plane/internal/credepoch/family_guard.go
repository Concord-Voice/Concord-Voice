package credepoch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
)

const familyGuardSetting = "concord.actor_family_guard"

// ErrFamilyGuardUnavailable is a guard fault (SQL or contract), never a denial.
var ErrFamilyGuardUnavailable = errors.New("credepoch: family guard unavailable")

// GuardFamilyTx is GuardTx plus the login-family fence (#2272): the shared
// writer barrier (unless this transaction already holds it), GuardTx's users
// FOR SHARE + epoch comparison, then the actor's exact family FOR SHARE. A
// write in flight when a revocation commits either holds these locks first (the
// revocation waits for its commit) or reads the terminal family afterwards and
// is refused. Call it where GuardTx was called: inside the write's READ
// COMMITTED transaction, before its first mutation.
//
// The signed identity comes from the request context (middleware.AuthRequired)
// and must match the actorID and tokenEpoch the caller threads; absence or
// disagreement fails closed as ErrEpochMismatch, which every caller already
// maps to its stale-credential response. A second call for the same identity in
// one transaction is a no-op. Lock order and deadlock freedom:
// [internal]specs/2026-10-26-2272-logical-login-lock-contract.md
// amendment 7.
func GuardFamilyTx(ctx context.Context, tx *sql.Tx, actorID, tokenEpoch string) error {
	identity, ok := loginfamily.AccessIdentityFromContext(ctx)
	actor, err := uuid.Parse(actorID)
	if !ok || tx == nil || err != nil || identity.UserID != actor || identity.CredentialEpoch != tokenEpoch {
		return ErrEpochMismatch
	}
	marker := identity.UserID.String() + ":" + identity.SID.String()
	var held string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(current_setting($1, true), '')`, familyGuardSetting).Scan(&held); err != nil {
		return fmt.Errorf("%w: %v", ErrFamilyGuardUnavailable, err)
	}
	if held == marker {
		// The first call's users and family share locks are held until commit,
		// and every terminalizer and epoch rotation needs a conflicting users
		// lock first, so nothing the first call checked can have changed.
		return nil
	}
	if held != "" {
		return fmt.Errorf("%w: second actor in one transaction", ErrFamilyGuardUnavailable)
	}
	if err := loginfamily.EnsureWriterBarrierTx(ctx, tx); err != nil {
		return errors.Join(ErrFamilyGuardUnavailable, err)
	}
	if err := GuardTx(ctx, tx, actorID, tokenEpoch); err != nil {
		return err
	}
	if err := loginfamily.GuardLockedActorFamilyTx(ctx, tx, identity); err != nil {
		if errors.Is(err, loginfamily.ErrDenied) {
			return errors.Join(ErrEpochMismatch, err)
		}
		return errors.Join(ErrFamilyGuardUnavailable, err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, familyGuardSetting, marker); err != nil {
		return fmt.Errorf("%w: %v", ErrFamilyGuardUnavailable, err)
	}
	return nil
}
