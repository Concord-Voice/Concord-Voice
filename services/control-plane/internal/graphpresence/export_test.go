package graphpresence

import (
	"context"
	"database/sql"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
)

// BeginCaptureSavepointForTest exposes the savepoint helper to the external
// graphpresence_test package, which cannot reach unexported methods.
func (r *Reconciler) BeginCaptureSavepointForTest(
	ctx context.Context, tx *sql.Tx, subject presencecapture.Subject,
) (func() error, error) {
	return r.beginCaptureSavepoint(ctx, tx, subject)
}
