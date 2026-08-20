package graphpresence

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
)

// BeginCaptureSavepointForTest exposes the C1 capture savepoint to the external
// graphpresence_test package, which cannot reach unexported methods. It returns
// the rollback func only, which is the shape the existing tests assert on.
func (r *Reconciler) BeginCaptureSavepointForTest(
	ctx context.Context, tx *sql.Tx, subject presencecapture.Subject,
) (func() error, error) {
	rollback, _, err := r.beginSavepoint(ctx, tx, subject, captureSavepoint)
	return rollback, err
}

// BeginGateSavepointForTest exposes the accepted-edge gate savepoint, which is
// the half a single-savepoint implementation leaves unprotected.
func (r *Reconciler) BeginGateSavepointForTest(
	ctx context.Context, tx *sql.Tx, subject presencecapture.Subject,
) (func() error, func() error, error) {
	return r.beginSavepoint(ctx, tx, subject, gateSavepoint)
}

// CaptureTopologyBeforeForTest exposes the pre-mutation Custom Status audience
// resolution to the external graphpresence_test package. That package owns the
// tests that run it against real PostgreSQL, which is the only place the #1234
// recipient exceptions and the sender-exclusion can actually be observed —
// the in-package tests script the settings read and never reach the audience
// queries on the success path.
func (r *Reconciler) CaptureTopologyBeforeForTest(
	ctx context.Context, tx *sql.Tx, senders []uuid.UUID,
) (map[uuid.UUID]map[uuid.UUID]bool, error) {
	return r.captureTopologyBefore(ctx, tx, senders)
}
