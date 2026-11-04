package accountguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

// ErrSourceRefused means a source outside the four a runner write may carry was asked
// for. It fails closed: the caller must not write under a different or empty source.
var ErrSourceRefused = errors.New("accountguard: source refused")

// setSourceSQL records the auth-change source for the audit triggers. The third argument
// is is_local = true, so the setting ends with the transaction and a pooled connection
// cannot carry one request's attribution into the next.
const setSourceSQL = `SELECT set_config('concord.auth_change_source', $1, true)`

// sourceTx attributes the writes the transaction makes from here on to s. It takes a
// *sql.Tx and never a pool or a connection because the setting is transaction-local:
// set_config(..., true) in autocommit ends with its own implicit transaction and does not
// reach the next statement, so only a statement inside the caller's transaction can carry
// the attribution to the writes that follow it. When that transaction ends, the session
// that made the setting reads an empty string, not NULL, so every reader must treat "never
// set" and "reset" as one state:
//
//	NULLIF(current_setting('concord.auth_change_source', true), '')
//
// Only SourceUserSession, SourceLockdown, SourceBreakGlass and SourceRecoveryKey are
// admitted. SourceOperator and SourceUnattributed are refused on purpose: an operator
// write and an unattributed write are never runner writes, so a runner that asks for
// either has a defect and must stop rather than write under a source that misdescribes
// it. The check runs before any statement, so a refusal touches neither the transaction
// nor a value set earlier in it.
func sourceTx(ctx context.Context, tx *sql.Tx, s decide.Source) error {
	switch s {
	case decide.SourceUserSession, decide.SourceLockdown, decide.SourceBreakGlass, decide.SourceRecoveryKey:
	default:
		return fmt.Errorf("set source: %w: %q", ErrSourceRefused, string(s))
	}
	if _, err := tx.ExecContext(ctx, setSourceSQL, string(s)); err != nil {
		return fmt.Errorf("set source: %w", err)
	}
	return nil
}
