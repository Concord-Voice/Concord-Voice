// Package presencehook holds the handler-side plumbing every #2446
// graph-destroying write shares: the deferred rollback, the endpoint-ID parse,
// and the three capture terminals.
//
// It is deliberately NOT part of internal/presencecapture, but NOT for the
// reason an earlier draft of this comment gave. That draft said these helpers
// need pkg/logger and so "would break" the leaf's zero-internal-dependency
// guarantee by risking an import cycle. pkg/logger has zero internal
// dependencies of its own, so no cycle is possible either way (PR #2738 review).
//
// The actual reason is narrower and is a preference, not a constraint: keeping
// internal/presencecapture free of behaviour makes the contract auditable in
// one read — it is types and one interface, with nothing that can act. The
// plumbing lives here so it is still written once rather than once per consumer.
// Merging the two packages would cost that auditability and nothing else.
//
// Everything here is a FREE FUNCTION taking the capture as a parameter, so a
// consumer keeps only the field, its setter, and the boot-guard accessor.
package presencehook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// RollbackUnlessDone is the deferred rollback for hooked transactions.
// sql.ErrTxDone means the terminal already committed and is not an error — it
// is in fact the normal successful path, because Complete owns the commit.
func RollbackUnlessDone(tx *sql.Tx, log *logger.Logger) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		log.Error("presence-hooked transaction rollback failed", "error", err)
	}
}

// Spec is a handler's capture request: the two enum choices the call site
// declares plus the endpoint IDs in the string form the handlers already carry.
//
// The posture is a field rather than a default so every hooked site states its
// failure behaviour explicitly at the call site.
type Spec struct {
	Family  presencecapture.Family
	Posture presencecapture.FailPosture

	// PrincipalID is the user whose graph edges the write creates or destroys.
	PrincipalID string
	// CounterpartID is the other friendship or block endpoint. Empty for a
	// family that has none, such as the friends-of-friends toggle.
	CounterpartID string
}

// Subject parses the spec's endpoint IDs. A malformed ID fails CLOSED: capturing
// against uuid.Nil would drop that endpoint from the bridge's focal set silently
// and reconcile only half the mutation — which is also why this never uses
// uuid.MustParse, whose panic would take the handler down.
//
// An earlier version of this comment claimed uuid's parse errors "describe
// only length and shape, never the rejected value". That is FALSE: google/uuid
// v1.6.0 formats the 45-byte URN branch as `invalid urn prefix: %q` on s[:9],
// echoing the first nine bytes of input (PR #2738 review, CodeRabbit). The
// wrap is still safe — those bytes are the caller's own path parameter, and
// %q escapes control characters so the value cannot forge a log line
// ([internal]rules/observability.md) — but do not repeat the stronger claim.
func (s Spec) Subject() (presencecapture.Subject, error) {
	principal, err := uuid.Parse(s.PrincipalID)
	if err != nil {
		return presencecapture.Subject{}, fmt.Errorf("parse capture principal: %w", err)
	}

	counterpart := uuid.Nil
	if s.CounterpartID != "" {
		counterpart, err = uuid.Parse(s.CounterpartID)
		if err != nil {
			return presencecapture.Subject{}, fmt.Errorf("parse capture counterpart: %w", err)
		}
	}

	return presencecapture.Subject{
		Family:      s.Family,
		FailPosture: s.Posture,
		Principal:   principal,
		Counterpart: counterpart,
	}, nil
}

// Capture resolves the pre-mutation audience inside the caller's transaction. It
// is a no-op when the capture is unwired, so a replica without the hook behaves
// exactly as it did before #2446 and degrades to the pre-existing <=90s presence
// TTL.
//
// It parses the endpoint IDs itself so an unwired handler cannot fail on an ID
// it never had to parse before.
func Capture(
	ctx context.Context,
	capture presencecapture.GraphPresenceCapture,
	tx *sql.Tx,
	spec Spec,
) (presencecapture.Plan, error) {
	if capture == nil {
		return nil, nil
	}
	subject, err := spec.Subject()
	if err != nil {
		return nil, err
	}
	return capture.CaptureInTx(ctx, tx, subject)
}

// Complete commits tx. When the capture is unwired it commits directly, so the
// caller NEVER calls tx.Commit() itself on either path — which is what lets the
// durable rail (whose terminal owns the commit) be swapped in at the
// construction site alone.
func Complete(
	ctx context.Context,
	capture presencecapture.GraphPresenceCapture,
	tx *sql.Tx,
	plan presencecapture.Plan,
) error {
	if capture == nil {
		if err := tx.Commit(); err != nil {
			// Wrapped, not bare, so the caller can still errors.Is against
			// sql.ErrTxDone while getting the operation context backend.md
			// requires on every returned error.
			return fmt.Errorf("commit unhooked graph mutation: %w", err)
		}
		return nil
	}
	return capture.Complete(ctx, tx, plan)
}

// Abandon is the fail-closed terminal for a path that will not reach Complete.
// It never touches tx.
func Abandon(
	capture presencecapture.GraphPresenceCapture,
	plan presencecapture.Plan,
	cause presencecapture.Cause,
) {
	if capture != nil {
		capture.Abandon(plan, cause)
	}
}
