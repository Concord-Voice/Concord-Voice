package graphpresence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
)

// reconcileRetryAfter is the delay handed to a caller whose sender holds an
// ELIGIBLE marker — one whose grace has already elapsed, so reconciliation owns
// it rather than this request. It matches RunPendingReconciler's tick interval
// (presencehistory.Service.reconcileInterval, 5s at NewService), which is the
// shortest interval after which the marker can have been resolved. It must be
// strictly positive: a zero Retry-After sends the client straight back into the
// same marker.
const reconcileRetryAfter = 5 * time.Second

// TopologyRail is the durable Custom Status (C2) rail, implemented by
// *presencehistory.Service.
//
// It is declared HERE, at the consumer, so this package depends on the three
// methods it uses rather than on the whole service — the same shape
// ActivityRefresher and Disconnector already use in reconciler.go. That the
// real service still satisfies it is locked by a compile-time assertion in
// topology_test.go.
type TopologyRail interface {
	// WithSenders holds every process-local sender stripe named by senderIDs
	// for one publication boundary. It must be entered BEFORE BeginTx.
	WithSenders(ctx context.Context, senderIDs []uuid.UUID, work func() error) error

	// BeginTopologyBatch writes one same-version durable marker per distinct
	// sender in deterministic UUID order, inside the caller's transaction.
	BeginTopologyBatch(
		ctx context.Context, tx *sql.Tx, senderIDs []uuid.UUID,
	) (presencehistory.TopologyBatch, error)

	// CompleteTopologyBatchWithOutcome commits the caller's transaction, then
	// performs bounded delivery, and reports whether the commit happened.
	CompleteTopologyBatchWithOutcome(
		ctx context.Context, tx *sql.Tx, batch presencehistory.TopologyBatch,
	) presencehistory.TopologyCompletion
}

// SetTopologyRail wires the durable Custom Status leg. A reconciler it has not
// been called on runs the PR-1 in-memory-only behaviour, which is why
// internal/api/router.go calls it at the one site that builds the production
// reconciler and then refuses to boot on an unwired rail.
func (r *Reconciler) SetTopologyRail(rail TopologyRail) {
	if r == nil {
		return
	}
	r.rail = rail
}

// HasTopologyRail reports whether the durable leg is wired.
//
// internal/api/router.go's requireGraphPresenceCaptureWired consults it and
// fatal-exits when it answers false. That is the whole reason it exists: New
// never returns nil, so a nil check on the constructed value is a tautology
// that still boots with the wiring line deleted, while this reports whether
// SetTopologyRail actually ran.
func (r *Reconciler) HasTopologyRail() bool {
	return r != nil && r.rail != nil
}

// WithGatedTx acquires the subject's process-local sender gates, then opens the
// transaction, then runs work under both.
//
// The order is the whole point. BeginTopologyBatch takes users ->
// user_presence_settings -> presence_settings_pending_operations row locks
// (presencehistory.beginTopologyOperation -> lockAudienceOperationPrior ->
// lockUser), and internal/users/presence_settings.go's UpdatePresenceSettings
// takes the SAME process-local gates first — via
// presencehistory.WithReadySenderModeBeforeReconcile, which is Service.WithSender
// over the same senderGates array WithSenders uses — and only then opens its
// transaction and takes the same users row. Acquiring the gates after BeginTx
// would let this path hold the row lock while waiting for a gate the settings
// path holds while waiting for the row — a cycle no database deadlock detector
// can break, because half of it is a Go channel.
//
// It does NOT commit: work's Complete owns the commit on both paths.
func (r *Reconciler) WithGatedTx(
	ctx context.Context,
	subject presencecapture.Subject,
	work func(tx *sql.Tx) error,
) error {
	if r == nil {
		return errors.New("graphpresence: WithGatedTx requires a reconciler")
	}
	if work == nil {
		return errors.New("graphpresence: WithGatedTx requires work")
	}
	focal := focalSenders(subject)
	if err := r.checkFocalBound(focal); err != nil {
		// Fails closed regardless of posture, before any lock or transaction:
		// an oversized focal set is a bug, and taking gates for it would
		// saturate the stripe array.
		return err
	}
	if r.rail == nil || len(focal) == 0 {
		// Unwired replica, or a subject with no focal sender. Both are the
		// pre-PR-2 behaviour: a plain transaction with no gate held.
		return r.runInTx(ctx, work)
	}
	return r.rail.WithSenders(ctx, focal, func() error {
		return r.runInTx(ctx, work)
	})
}

// runInTx opens the transaction and guarantees it is discarded unless work's
// terminal already committed it. sql.ErrTxDone is the NORMAL successful path
// here, because Complete owns the commit.
func (r *Reconciler) runInTx(ctx context.Context, work func(tx *sql.Tx) error) (err error) {
	if r.db == nil {
		return errors.New("graphpresence: WithGatedTx requires a database")
	}
	tx, beginErr := r.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return fmt.Errorf("begin gated graph mutation: %w", beginErr)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
			return
		}
		if r.log != nil {
			// Fixed enum only — never the underlying error, never any user ID.
			r.log.Error("gated graph mutation rollback failed",
				"failure_class", "gated_rollback")
		}
		// Fail closed. A discard that neither succeeded nor found the
		// transaction already resolved leaves the mutation's fate unknown, so
		// returning work's nil here would report success for a write nobody can
		// prove landed.
		err = errors.Join(err, fmt.Errorf("discard gated graph mutation: %w", rollbackErr))
	}()
	return work(tx)
}

// translateRailError converts the durable rail's pending terminals into the
// leaf sentinel, so handlers classify on internal/presencecapture and take NO
// internal/presencehistory import. Everything else passes through untouched.
//
// presencehistory.ServiceError carries RetryAfter as a FIELD, not a method, so
// no structural interface at the leaf can reach it — the translation has to
// happen here, at the one package that imports both.
func translateRailError(err error) error {
	if err == nil {
		return nil
	}
	var serviceErr *presencehistory.ServiceError
	if errors.As(err, &serviceErr) && serviceErr.Code == "presence_operation_pending" {
		return &presencecapture.PendingError{After: serviceErr.RetryAfter}
	}
	if errors.Is(err, presencehistory.ErrPendingOperationEligible) {
		// The marker's grace already elapsed, so reconciliation owns it. This
		// request must not supersede it: superseding an eligible marker is what
		// ForcedSecurityClear is for, and a friendship mutation is not one.
		return &presencecapture.PendingError{After: reconcileRetryAfter}
	}
	return err
}

// carryTopology copies the durable C2 evidence from a captured plan onto a
// degraded or bounded one. It is INVARIANT TB-1 in code: once
// BeginTopologyBatch has been called the batch is never dropped and never
// conditioned on FailPosture, so a C1 degrade yields a degraded active leg PLUS
// a fully valid C2 batch in one Plan.
//
// Without it a degraded plan would carry live markers with no exact-delta
// audience. presencehistory's prepared plans are DeliveryExactDelta, so the
// batch would either fail preparation or acknowledge having delivered nothing —
// and the markers would sit until the grace window expired, suppressing that
// sender's Custom Status for every reconnecting viewer.
func carryTopology(source, target *Plan) {
	if source == nil || target == nil || !source.hasTopology {
		return
	}
	target.topology = source.topology
	target.hasTopology = true
	target.topologySenders = source.topologySenders
	target.topologyBefore = source.topologyBefore
}

// readTopologyActivity re-derives BeginTopologyBatch's activity predicate from
// the settings row it just locked FOR UPDATE, on the same transaction.
//
// It exists so the batch stays OPAQUE to this package. The predicate lives in
// AudienceOperation.Before* inside TopologyBatch's unexported fields, and
// exposing those would put the sender's Custom Status text and emoji across the
// bridge boundary. Re-reading the locked row costs one round trip per focal
// sender (at most two) and returns the same values, because
// lockAudienceOperationPrior holds the row FOR UPDATE and the graph write has
// not run yet.
//
// custom_text is projected to a BOOLEAN in SQL. The text itself is never
// selected, so it cannot reach a variable, a log line, or an error string here.
//
// The predicate MUST stay identical to presencehistory.prepareTopologyPlan's
// `operation.BeforeMasterEnabled && operation.BeforeTier > 0 &&
// operation.Before.Text != ""`. Before.Text is normalizeCustomTextState over
// COALESCE of the same column, so the third projection below asks the same
// question, byte for byte with the SQL:
//
//	COALESCE(custom_text, '') <> ''
//
// If they drift, PrepareTopologyBatch fails closed with "inactive topology
// operation has audience" — escalate rather than widening the batch.
//
// That fragment is an INDENTED CODE BLOCK, not prose. gofmt's doc comment
// printer rewrites a bare pair of ASCII single quotes in prose into a U+201D
// right double quotation mark, which is how this very claim came to carry two
// smart quotes — breaking a grep for the SQL and a paste into psql alike.
// Code blocks are printed verbatim, so the equivalence claim survives a
// reformat.
func readTopologyActivity(
	ctx context.Context, tx *sql.Tx, senderID uuid.UUID,
) (bool, int, error) {
	var (
		masterEnabled bool
		tier          int
		hasText       bool
	)
	err := tx.QueryRowContext(ctx, `
		SELECT master_enabled,
		       custom_text_tier,
		       COALESCE(custom_text, '') <> ''
		FROM user_presence_settings
		WHERE user_id = $1
	`, senderID).Scan(&masterEnabled, &tier, &hasText)
	if errors.Is(err, sql.ErrNoRows) {
		// Unreachable through BeginTopologyBatch, whose
		// lockAudienceOperationPrior INSERTs the row ON CONFLICT DO NOTHING
		// before locking it. Treated as inactive rather than as an error so a
		// future caller that skips that guarantee still fails safe: an inactive
		// sender gets a nil audience, which is the only value
		// prepareTopologyPlan accepts for one.
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("read topology activity predicate: %w", err)
	}
	return masterEnabled && tier > 0 && hasText, tier, nil
}

// captureTopologyBefore resolves each sender's PRE-mutation authorized Custom
// Status audience.
//
// It fails CLOSED under every posture. C2 has no level arm: Custom Status is not
// republished on a heartbeat and carries no TTL, so a viewer who never
// reconnects holds the text indefinitely and there is no staleness horizon to
// fall back on (durability amendment §6.7.1). Degrading here would commit a
// friendship write whose Custom Status revocation nothing will ever deliver.
//
// The audience goes through the exported ComputeCustomTextAudienceForTier on
// the CALLER's transaction, never r.db, so it reads the same pre-write snapshot
// the rest of the capture does and the #1234 recipient exceptions run as the
// FINAL filter. An overrides-read failure returns an error rather than an
// unfiltered tier audience.
//
// The sender is NOT inserted into its own audience. internal/users'
// preparePresenceSettingsPlan does exactly that at :630 and :640 for the
// settings rail, and copying it here would be rejected:
// presencehistory.cloneTopologyRecipients returns "invalid topology audience
// recipient" for recipientID == senderID.
func (r *Reconciler) captureTopologyBefore(
	ctx context.Context, tx *sql.Tx, senders []uuid.UUID,
) (map[uuid.UUID]map[uuid.UUID]bool, error) {
	before := make(map[uuid.UUID]map[uuid.UUID]bool, len(senders))
	for _, senderID := range senders {
		active, tier, err := readTopologyActivity(ctx, tx, senderID)
		if err != nil {
			return nil, err
		}
		if !active {
			// prepareTopologyPlan rejects a non-empty audience on an inactive
			// operation, so nil is the only correct value here.
			before[senderID] = nil
			continue
		}
		audience, err := presence.ComputeCustomTextAudienceForTier(ctx, tx, senderID, tier)
		if err != nil {
			return nil, fmt.Errorf("compute topology before audience: %w", err)
		}
		before[senderID] = audience
	}
	return before, nil
}

// buildTopologyAudiences pairs each sender's Before and After sets into the
// coverage PrepareTopologyBatch requires: exactly one TopologyAudience per
// operation, one operation per distinct focal sender.
//
// The pairing is BY KEY, never positional. BeginTopologyBatch canonicalizes the
// senders it is handed — canonicalTopologySenders dedupes them and sorts by
// uuid.String() — while topologySenders keeps the bridge's derivation order
// (principal, then counterpart). The same SET in a different order.
// PrepareTopologyBatch indexes audiences by TopologyAudience.SenderID rather
// than by position (indexTopologyAudiences), so one entry per sender covers the
// batch exactly; zipping the two slices would hand two senders each other's
// audience.
//
// A nil map on either side is the inactive case and stays nil.
// cloneTopologyRecipients ranges over it and yields an empty set, which is the
// only value prepareTopologyPlan accepts for an inactive operation.
func buildTopologyAudiences(
	senders []uuid.UUID,
	before, after map[uuid.UUID]map[uuid.UUID]bool,
) []presencehistory.TopologyAudience {
	audiences := make([]presencehistory.TopologyAudience, 0, len(senders))
	for _, senderID := range senders {
		audiences = append(audiences, presencehistory.TopologyAudience{
			SenderID: senderID,
			Before:   before[senderID],
			After:    after[senderID],
		})
	}
	return audiences
}

// topologyFailureClass is a FIXED enum, mirroring degradeCause in plan.go. It is
// never a wrapped database error and never a query-derived string, so the log
// line it produces cannot become a data leak
// ([internal]rules/observability.md), and backend.md's #2446 rule that this
// vocabulary be a named type holds here for the same reason it holds for
// presencecapture.Cause: an untyped parameter invites a caller to interpolate a
// driver error into failure_class.
type topologyFailureClass uint8

const (
	// topologyAudienceRead is the post-write After audience failing to resolve.
	// Pre-commit: the write is blocked.
	topologyAudienceRead topologyFailureClass = iota
	// topologyAudienceCoverage is PrepareTopologyBatch rejecting the audiences.
	// Pre-commit, and the §3.5 escalation signal — it means this bridge's
	// re-derived activity predicate disagrees with the batch's own.
	topologyAudienceCoverage
	// topologyPostCommitDelivery is a DURABLE mutation whose delivery failed.
	topologyPostCommitDelivery
	// topologyCommitUnproven is a commit that neither succeeded nor provably
	// failed, which fails closed.
	topologyCommitUnproven
)

func (c topologyFailureClass) String() string {
	switch c {
	case topologyAudienceCoverage:
		return "topology_audience_coverage"
	case topologyPostCommitDelivery:
		return "topology_post_commit_delivery"
	case topologyCommitUnproven:
		return "topology_commit_unproven"
	default:
		return "topology_audience_read"
	}
}

// logTopologyFailure emits the fixed enum and nothing else. There is no
// recipient count and no sender ID: the durable leg's audiences are Custom
// Status recipients, and a per-sender count is a coarse read on how many people
// can see one user's status.
func (r *Reconciler) logTopologyFailure(class topologyFailureClass) {
	if r == nil || r.log == nil {
		return
	}
	r.log.Error("durable custom status leg failed", "failure_class", class.String())
}

// classifyTopologyCompletion turns the rail's outcome into the leaf sentinel a
// handler classifies on.
//
// Committed == true with an error means the mutation IS durable and only
// delivery failed: 503, not 500, and the response body must still describe the
// mutation as having happened. Committed == false is NOT proof that nothing
// landed — presencehistory.TopologyCompletion documents it as covering a proven
// rollback and an unresolved commit alike — so the caller fails closed on it.
func classifyTopologyCompletion(completion presencehistory.TopologyCompletion) error {
	switch {
	case completion.Committed && completion.Err == nil:
		return nil
	case completion.Committed:
		return fmt.Errorf("%w: %w", presencecapture.ErrPostCommitDelivery, completion.Err)
	case completion.Err != nil:
		return fmt.Errorf("complete topology audience batch: %w", completion.Err)
	default:
		// Unreachable through *presencehistory.Service: its validation terminal
		// joins a non-nil error, and commitTopologyBatch reports Committed
		// false only after CommitTx failed, which is also an error. This arm is
		// here because the alternative — falling through to the nil above — is
		// the one shape that would report SUCCESS for a commit nobody proved,
		// and TopologyCompletion's own contract tells the caller to treat
		// Committed == false as not proven.
		return errors.New("complete topology audience batch: commit unproven")
	}
}

// topologyAudiences computes each sender's POST-write authorized Custom Status
// audience and pairs it with the pre-mutation one the capture already holds.
//
// It runs post-write, pre-commit, on the CALLER's transaction. Post-commit
// would race every concurrent graph write; another transaction would read
// another snapshot.
//
// The After side goes through the exported ComputeCustomTextAudience, so the
// #1234 recipient exceptions are the FINAL filter on it exactly as
// captureTopologyBefore's ComputeCustomTextAudienceForTier makes them the final
// filter on the Before side — computeCustomTextBaseAudienceForTier stays
// unexported and neither side can reach the unfiltered tier audience. An
// overrides-read failure returns an error and the write fails CLOSED.
//
// The two entry points differ only in where the tier comes from, and they agree
// here: ComputeCustomTextAudience re-reads master_enabled and custom_text_tier
// from the row BeginTopologyBatch already holds FOR UPDATE
// (lockAudienceOperationPrior), and the graph write does not touch that row.
func (r *Reconciler) topologyAudiences(
	ctx context.Context, tx *sql.Tx, plan *Plan,
) ([]presencehistory.TopologyAudience, error) {
	after := make(map[uuid.UUID]map[uuid.UUID]bool, len(plan.topologySenders))
	for _, senderID := range plan.topologySenders {
		if plan.topologyBefore[senderID] == nil {
			// The operation is inactive: master off, tier 0, or empty text.
			// prepareTopologyPlan rejects ANY audience on such an operation, so
			// the After side must stay nil too — a friendship write cannot make
			// an inactive Custom Status deliverable. An ACTIVE sender with an
			// empty audience is a different value: captureTopologyBefore stores
			// the non-nil empty map the audience computation returns.
			after[senderID] = nil
			continue
		}
		audience, err := presence.ComputeCustomTextAudience(ctx, tx, senderID)
		if err != nil {
			return nil, fmt.Errorf("compute topology after audience: %w", err)
		}
		after[senderID] = audience
	}
	return buildTopologyAudiences(plan.topologySenders, plan.topologyBefore, after), nil
}

// completeTopology runs the durable C2 terminal: After audiences, then
// PrepareTopologyBatch, then the rail's commit-and-deliver.
//
// CompleteTopologyBatchWithOutcome OWNS the transaction from the moment it is
// called: it commits on success and rolls back on a validation failure, so
// nothing here may touch tx afterwards. It also owns every terminal on this
// path, because the terminal is chosen on the completion's OWN commit evidence
// rather than on the shape of an error.
//
// The two failures before that call are PRE-COMMIT, and they abandon NOBODY.
// The handler's deferred rollback discards the write and the markers with it,
// so no viewer's authorization changed and there is nothing stale to clear —
// the same reasoning presencecapture.CauseProvesNoCommit encodes, and the
// reason #2738 exists.
func (r *Reconciler) completeTopology(ctx context.Context, tx *sql.Tx, plan *Plan) error {
	audiences, err := r.topologyAudiences(ctx, tx, plan)
	if err != nil {
		r.logTopologyFailure(topologyAudienceRead)
		return err
	}
	prepared, err := presencehistory.PrepareTopologyBatch(plan.topology, audiences)
	if err != nil {
		// The §3.5 escalation trigger: "inactive topology operation has
		// audience" here means the bridge's re-derived activity predicate
		// disagrees with the batch's own. Do NOT widen the batch to fix it.
		r.logTopologyFailure(topologyAudienceCoverage)
		return fmt.Errorf("prepare topology audience batch: %w", err)
	}

	return r.completeTopologyBatch(ctx, tx, plan, prepared)
}

// completeTopologyBatch hands the prepared batch to the rail and takes the
// terminal the rail's own commit evidence names.
//
// It branches on TopologyCompletion.Committed, NOT on the shape of the error
// classifyTopologyCompletion returns. The sentinel is for the handler; deciding
// here whether a mutation is durable by re-reading our own wrap would put the
// fail-closed branch behind an errors.Is against a value this function produced
// one line earlier.
func (r *Reconciler) completeTopologyBatch(
	ctx context.Context, tx *sql.Tx, plan *Plan, prepared presencehistory.TopologyBatch,
) error {
	completion := r.rail.CompleteTopologyBatchWithOutcome(ctx, tx, prepared)
	err := classifyTopologyCompletion(completion)
	if err == nil {
		r.enqueue(plan)
		return nil
	}
	if completion.Committed {
		// The mutation IS durable. The C1 leg must still run: skipping it would
		// leave viewers who just lost Server Voice authorization holding it
		// until the presence TTL.
		r.logTopologyFailure(topologyPostCommitDelivery)
		r.enqueue(plan)
		return err
	}
	// Not committed, or not proven. Unknown state fails closed.
	r.logTopologyFailure(topologyCommitUnproven)
	r.Abandon(plan, presencecapture.CauseCommitUnresolved)
	return err
}
