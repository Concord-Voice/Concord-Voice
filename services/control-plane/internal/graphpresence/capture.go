package graphpresence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
)

// CaptureInTx completes the contract implementation, so the assertion lives
// here rather than beside the other methods.
var _ presencecapture.GraphPresenceCapture = (*Reconciler)(nil)

// CaptureInTx resolves the pre-mutation state INSIDE the caller's transaction,
// before the write. It must not be split into a pre-transaction phase: this
// family's write mutates the friend graph the capture reads, which is exactly
// the disjointness precondition #2445's pre-transaction phase depends on.
//
// The focal set is the principals themselves. The peripheral direction — the
// third parties whose audience shrinks because a principal left it — collapses
// to a viewer-scoped disconnect of that principal rather than N sender
// operations, because what actually changed is that ONE viewer lost access.
// That keeps users-row lock counts at <= 2 per transaction.
func (r *Reconciler) CaptureInTx(
	ctx context.Context, tx *sql.Tx, subject presencecapture.Subject,
) (presencecapture.Plan, error) {
	if tx == nil {
		return nil, errors.New("graphpresence: CaptureInTx requires a transaction")
	}

	// STEP 1. All three checks fail CLOSED regardless of posture: an empty focal
	// set and an oversized one are both derivation bugs, and an unregistered
	// family is a family whose author never stated its policy. All three run
	// before any statement, so none can poison the transaction.
	focal := focalSenders(subject)
	if len(focal) == 0 {
		// Posture- and rail-independent: the answer is the same whether or not
		// a rail is wired, which is why this check sits above the rail branch
		// rather than inside it. Without it an all-nil Subject reaches
		// presencehistory.canonicalTopologySenders and 500s with "invalid
		// topology sender batch", which names neither this function nor the
		// empty Subject that caused it. Blocking the write here is not the
		// short-circuit-to-empty-plan alternative — that one would commit the
		// mutation with the markers silently skipped.
		return nil, errors.New("graphpresence: CaptureInTx requires at least one principal")
	}
	if err := r.checkFocalBound(focal); err != nil {
		return nil, err
	}
	policy, err := presencecapture.PolicyFor(subject.Family)
	if err != nil {
		return nil, err
	}

	// STEP 2. The GATE savepoint. Every read below runs on the CALLER's
	// transaction, and a failed statement inside a PostgreSQL transaction
	// poisons it (25P02): the handler's next write then fails regardless, so
	// without this the degrade branch returned a plan into a transaction that
	// could no longer commit and BlockUser 500'd anyway — inert for its whole
	// intended failure mode (PR #2738 review, @e2ee-reviewer).
	//
	// Rolling back to it restores a usable transaction, so the block proceeds
	// and the conservative disconnect substitutes for the exact delta. Reads
	// stay inside the caller's transaction — moving them to another connection
	// would read a different snapshot and break the invariant this whole family
	// is built on.
	gateRollback, gateRelease, err := r.beginSavepoint(ctx, tx, subject, gateSavepoint)
	if err != nil {
		return nil, err
	}
	degradeAtGate := func(cause degradeCause) (presencecapture.Plan, error) {
		if rbErr := gateRollback(); rbErr != nil {
			// The transaction cannot be made usable again, so proceeding would
			// commit a write with no reconciliation. Fail closed instead.
			return nil, rbErr
		}
		return r.degradePlan(subject, cause), nil
	}

	// STEP 3. GATE (#2738 security fix). A revoking family with a counterpart
	// only narrows an audience when an ACCEPTED friendship edge exists between
	// the two principals at capture time:
	//
	//   - RemoveFriend's DELETE carries `AND status = 'accepted'`.
	//   - Block's UPDATE rewrites any status to 'blocked', but the presence
	//     audience is derived from `status = 'accepted'` rows only
	//     (internal/presence/audience.go friendsOf / friendsOfFriendsOf), so
	//     absent->blocked, pending->blocked and blocked->blocked all leave both
	//     audiences byte-identical.
	//
	// Without this gate the counterpart is a raw path parameter: any
	// authenticated caller could name a stranger and make the capture seed that
	// stranger into the viewer set (a full websocket teardown of their every
	// device), drag that stranger's entire Server Voice audience into the plan
	// (an abandon fan-out of thousands), and — now that markers exist — write a
	// durable row that suppresses that stranger's Custom Status for every
	// reconnecting viewer. Proven-no-change must not reconcile, exactly as
	// RemoveFriend's rowsAffected == 0 branch already argues.
	if policy.CanRevokeVisibility && subject.Counterpart != uuid.Nil {
		destroys, edgeErr := acceptedEdgeExists(ctx, tx, subject.Principal, subject.Counterpart)
		if edgeErr != nil {
			if subject.FailPosture == presencecapture.FailConservativeDegrade {
				return degradeAtGate(causeAudienceRead)
			}
			return nil, edgeErr
		}
		if !destroys {
			// Benign empty terminal: nothing is written that any viewer's
			// authorization depends on, so there is nothing to reconcile,
			// nobody to disconnect, and NO MARKERS.
			//
			// The gate savepoint is deliberately NOT released here. Nothing
			// after this point can roll back to it, and a RELEASE would add a
			// fail-closed error path to a terminal that writes nothing; the
			// caller's transaction is resolved either way by runInTx.
			return &Plan{subject: subject}, nil
		}
	}

	// STEP 4. Release the gate savepoint. Nothing may roll back past this point
	// to before the markers written in step 5.
	if err := gateRelease(); err != nil {
		return nil, err
	}

	plan := &Plan{subject: subject}

	// STEP 5 + 6. MARKERS, then the pre-mutation audiences. Fail CLOSED under
	// any posture, and OUTSIDE every savepoint — this call sits between the gate
	// RELEASE above and the capture savepoint below, and that placement is what
	// makes invariant TB-1 hold. Do not move it inside either savepoint. Full
	// rationale on captureTopologyMarkers; it is stated once, there.
	if err := r.captureTopologyMarkers(ctx, tx, policy, focal, plan); err != nil {
		return nil, err
	}

	// STEP 7. The CAPTURE savepoint. It is opened AFTER the markers, so the
	// step-8 degrade cannot reach them.
	captureRollback, captureRelease, err := r.beginSavepoint(ctx, tx, subject, captureSavepoint)
	if err != nil {
		return nil, err
	}
	degradeAtCapture := func(cause degradeCause) (presencecapture.Plan, error) {
		if rbErr := captureRollback(); rbErr != nil {
			return nil, rbErr
		}
		degraded := r.degradePlan(subject, cause)
		carryTopology(plan, degraded)
		return degraded, nil
	}

	// The peripheral leg — a viewer-scoped disconnect of the principals — is
	// seeded only for mutations that CAN revoke authorization. It exists to
	// clear state a principal may still hold for senders reachable only through
	// the mutated edge, and an additive mutation creates no such state.
	//
	// DisconnectRichPresenceClients closes every local DEVICE for those users
	// (internal/websocket/richpresence.go), not merely a presence subscription,
	// so seeding it unconditionally tore down both principals' sessions on
	// every friend acceptance for no reconciliation benefit (#2738 review).
	if policy.CanRevokeVisibility {
		plan.viewers = make(map[uuid.UUID]bool, len(focal))
		for _, id := range focal {
			plan.viewers[id] = true
		}
	}

	// STEP 8. The C1 legs. This is the ONLY stage that may degrade, and the
	// markers written in step 5 survive that degrade.
	for _, senderID := range focal {
		legs, legErr := r.captureActiveLegs(ctx, tx, senderID)
		if legErr != nil {
			if subject.FailPosture == presencecapture.FailConservativeDegrade {
				return degradeAtCapture(causeActiveScopeRead)
			}
			return nil, legErr
		}
		plan.active = append(plan.active, legs...)
	}
	if err := captureRelease(); err != nil {
		return nil, err
	}

	// STEP 9. The bound applies to the C1 half only. An over-large capture means
	// no exact ACTIVE delta was computed; the topology batch is exact regardless
	// of how many Server Voice viewers the sender has.
	if countCaptured(plan) > maxCapturedViewers {
		bounded := r.planForBoundExceeded(subject, policy)
		carryTopology(plan, bounded)
		return bounded, nil
	}
	return plan, nil
}

// captureTopologyMarkers is STEPS 5 and 6, extracted so CaptureInTx stays under
// the cognitive-complexity bound (SonarQube S3776). It is called from exactly
// the position the block occupied: after the gate savepoint is RELEASED and
// before the capture savepoint is OPENED. That placement is not incidental —
// it is what makes invariant TB-1 hold, so this call must never be moved inside
// either savepoint.
//
// It mutates plan in place rather than returning a value, so a caller cannot
// accidentally drop the batch on the floor by ignoring a result.
//
// STEP 5. MARKERS. Fail CLOSED under any posture, and OUTSIDE every savepoint.
//
// BeginTopologyBatch returns mid-loop with earlier senders' markers already
// written, and no savepoint is open around it, so the only thing that can undo
// them is aborting the whole transaction — which is exactly what failing closed
// here leaves the caller to do. Returning a degraded plan instead would leave
// durable markers behind that nothing resolves, and a silent rollback would
// leave a plan referencing markers that no longer exist. Both are half states;
// blocking the write is not. Note there is deliberately no FailPosture branch
// anywhere below: TB-1 forbids conditioning the batch on posture.
func (r *Reconciler) captureTopologyMarkers(
	ctx context.Context,
	tx *sql.Tx,
	policy presencecapture.FamilyPolicy,
	focal []uuid.UUID,
	plan *Plan,
) error {
	if !policy.CarriesCustomTextTopology || r.rail == nil {
		return nil
	}

	batch, beginErr := r.rail.BeginTopologyBatch(ctx, tx, focal)
	if beginErr != nil {
		return translateRailError(beginErr)
	}
	plan.topology = batch
	plan.hasTopology = true
	plan.topologySenders = append([]uuid.UUID(nil), focal...)

	// STEP 6. The pre-mutation Custom Status audiences, read BEFORE the write
	// mutates the graph they derive from. Fail CLOSED under any posture: C2 has
	// no staleness horizon.
	before, audienceErr := r.captureTopologyBefore(ctx, tx, focal)
	if audienceErr != nil {
		return audienceErr
	}
	plan.topologyBefore = before
	return nil
}

// planForBoundExceeded decides what an over-large capture resolves to.
//
// Split out so the decision is reachable without a database — asserting on a
// hand-built Plan instead would restate the expected value rather than exercise
// this branch, which is the tautological-test shape review flagged elsewhere in
// this PR.
//
// The SAME CanRevokeVisibility gate as the peripheral seed applies, and for the
// same reason. degradePlan seeds both principals for a full device teardown, so
// applying it unconditionally reintroduced exactly the defect that gate exists
// to prevent: accepting a friend request while in voice on a large server tore
// down every device of BOTH users for a mutation that revokes nothing
// (PR #2738 review, @code-reviewer).
//
// For an additive family nothing can be stale, so an over-large capture means
// only "no exact delta was computed" — the benign empty terminal, not a
// disconnect. This is a bound, not a failure.
//
// The policy is THREADED IN rather than re-resolved. Step 1 already called
// PolicyFor on this subject's family and propagated ErrFamilyUnregistered, so
// the only caller reaches here holding a resolved policy; the earlier
// subject.Family.CanRevokeVisibility() call resolved the same registry a second
// time on the same call chain and, on the branch step 1 has already ruled out,
// substituted a conservative default instead of propagating.
func (r *Reconciler) planForBoundExceeded(
	subject presencecapture.Subject, policy presencecapture.FamilyPolicy,
) *Plan {
	if !policy.CanRevokeVisibility {
		return &Plan{subject: subject}
	}
	return r.degradePlan(subject, causeBoundExceeded)
}

// focalSenders returns the users whose OWN audience changes. Every #2446 family
// yields one or two. uuid.Nil is never a user.
func focalSenders(subject presencecapture.Subject) []uuid.UUID {
	out := make([]uuid.UUID, 0, 2)
	if subject.Principal != uuid.Nil {
		out = append(out, subject.Principal)
	}
	if subject.Counterpart != uuid.Nil && subject.Counterpart != subject.Principal {
		out = append(out, subject.Counterpart)
	}
	return out
}

// voiceScopeRow is one raw live Server Voice row, read and fully drained before
// any audience resolution runs.
type voiceScopeRow struct {
	serverID    uuid.UUID
	channelID   uuid.UUID
	lifecycleAt sql.NullTime
}

// captureActiveLegs reads senderID's live Server Voice scopes through tx and
// resolves each one's pre-mutation authorized audience.
//
// The two phases must stay separate: a *sql.Tx owns exactly one connection, so
// resolving an audience while the scope cursor is still open issues a second
// query on a busy connection and wedges the transaction.
//
// The scope carries lifecycle_event_at, not joined_at, and sets LifecycleID
// equal to RoomID — the presence.CurrentServerVoiceScope contract. A scope
// missing either would be rejected by RefreshServerVoiceRecheck's validation,
// collapsing every exact reconciliation into a conservative disconnect.
func (r *Reconciler) captureActiveLegs(
	ctx context.Context, tx *sql.Tx, senderID uuid.UUID,
) ([]activeLeg, error) {
	scopes, err := activeVoiceScopes(ctx, tx, senderID)
	if err != nil {
		return nil, err
	}

	legs := make([]activeLeg, 0, len(scopes))
	for _, scope := range scopes {
		captured, capErr := presence.CaptureServerVoiceCandidatesStrict(
			ctx, tx, r.senderPresence, senderID, scope.serverID,
		)
		if capErr != nil {
			return nil, fmt.Errorf("capture server voice candidates: %w", capErr)
		}
		if len(captured) == 0 {
			// A DETERMINED suppression: this sender holds a live voice row but
			// is not currently permitted to emit, so there is no audience to
			// reconcile and skipping the leg is correct.
			//
			// It is only correct because the indeterminate case no longer
			// arrives here. CaptureServerVoiceCandidates used to return
			// (empty, nil) both when emission was suppressed AND when the
			// resolver could not determine it — a Redis transport error or a
			// failed presence_offline_fences read (a redis.Nil is a DETERMINED
			// suppression and always was) — so a transient fault during
			// a removal/block/FoF-toggle silently dropped this leg with
			// Degraded() false and no error, and the third parties who had just
			// lost authorization were never cleared (CWE-284; PR #2738 review
			// @security-reviewer, then PR #2770 review CodeRabbit). It now
			// consults RichPresenceEmissionState and returns an error for the
			// undetermined case, which reaches this function's caller and is
			// routed through the subject's declared FailPosture.
			//
			// The log stays: a skipped leg is still worth seeing, and it is now
			// an accurate statement that the sender was suppressed rather than
			// an unclassified silence.
			if r.log != nil {
				r.log.Warn("graph presence skipped an active scope with no candidates",
					"failure_class", "no_candidates")
			}
			continue
		}
		leg := activeLeg{
			senderID: senderID,
			scope: presence.Scope{
				Category:    presence.CategoryServerVoice,
				RoomID:      scope.channelID,
				LifecycleID: scope.channelID,
			},
			captured: captured,
		}
		if scope.lifecycleAt.Valid {
			leg.scope.EventAt = scope.lifecycleAt.Time
		}
		legs = append(legs, leg)
	}
	return legs, nil
}

// activeVoiceScopes drains senderID's live Server Voice rows and closes the
// cursor before returning, so the caller may query tx again.
func activeVoiceScopes(
	ctx context.Context, tx *sql.Tx, senderID uuid.UUID,
) ([]voiceScopeRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.server_id, vp.channel_id, vp.lifecycle_event_at
		FROM voice_participants vp
		JOIN channels c ON c.id = vp.channel_id
		WHERE vp.user_id = $1 AND c.type = 'voice'
	`, senderID)
	if err != nil {
		return nil, fmt.Errorf("query active server voice scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var scopes []voiceScopeRow
	for rows.Next() {
		var scope voiceScopeRow
		if err := rows.Scan(&scope.serverID, &scope.channelID, &scope.lifecycleAt); err != nil {
			return nil, fmt.Errorf("scan active server voice scope: %w", err)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active server voice scopes: %w", err)
	}
	return scopes, nil
}

func countCaptured(p *Plan) int {
	return len(p.capturedAudience())
}

// acceptedEdgeExists reports whether an accepted friendship joins the two
// principals inside tx. It is the ONLY presence-visibility edge a #2446
// friendship or block write can destroy; the query is the same predicate
// RemoveFriend's DELETE uses, so the gate and the write agree by construction.
func acceptedEdgeExists(
	ctx context.Context, tx *sql.Tx, principal, counterpart uuid.UUID,
) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM friendships
			WHERE ((requester_id = $1 AND addressee_id = $2)
			    OR (requester_id = $2 AND addressee_id = $1))
			  AND status = 'accepted'
		)
	`, principal, counterpart).Scan(&exists); err != nil {
		return false, fmt.Errorf("read accepted friendship edge: %w", err)
	}
	return exists, nil
}

// The savepoint statements are FIXED literals rather than names concatenated
// into SQL text. A savepoint name cannot be a bind parameter, so the guideline
// against building SQL by concatenation is satisfied by writing each statement
// out in full — which also removes the only place an identifier could be
// spliced in later by accident (PR #2738 review, CodeRabbit).
//
// name is the operator-facing label. Both savepoints reach beginSavepoint, and
// the two sites mean OPPOSITE things when they fail: a step-2 (gate) failure
// means no marker was ever written and the transaction is clean, while a step-7
// (capture) failure means BeginTopologyBatch has already written durable rows
// for both principals inside a transaction that is now aborting. Hard-coding
// "capture" into all three wraps made BlockUser 500 with a message an on-call
// operator could not tell those two apart from.
type savepointStatements struct {
	name     string
	open     string
	rollback string
	release  string
}

// TWO savepoints, with the durable marker write BETWEEN them. One savepoint is
// wrong in both placements it could take:
//
//   - Opened BEFORE the accepted-edge gate and still open at the C1 capture, a
//     degrade rolls the markers back silently. The plan then references markers
//     that no longer exist, so the completion has live evidence of nothing.
//   - Opened AFTER the gate, the gate read itself is unprotected. A failed gate
//     read poisons the transaction (25P02), BlockUser 500s anyway, and its
//     declared FailConservativeDegrade posture is inert again — the exact
//     defect the capture savepoint was added to fix.
var (
	gateSavepoint = savepointStatements{
		name:     "gate",
		open:     "SAVEPOINT concord_graph_presence_gate",
		rollback: "ROLLBACK TO SAVEPOINT concord_graph_presence_gate",
		release:  "RELEASE SAVEPOINT concord_graph_presence_gate",
	}
	captureSavepoint = savepointStatements{
		name:     "capture",
		open:     "SAVEPOINT concord_graph_presence_capture",
		rollback: "ROLLBACK TO SAVEPOINT concord_graph_presence_capture",
		release:  "RELEASE SAVEPOINT concord_graph_presence_capture",
	}
)

// beginSavepoint opens one savepoint for postures that need the caller's
// transaction to survive a failed capture read, and returns its rollback and
// release funcs.
//
// For FailClosedBlockWrite there is nothing to restore — a read failure blocks
// the write by design — so both funcs are no-ops and no round trip is spent.
func (r *Reconciler) beginSavepoint(
	ctx context.Context,
	tx *sql.Tx,
	subject presencecapture.Subject,
	statements savepointStatements,
) (func() error, func() error, error) {
	noop := func() error { return nil }
	if subject.FailPosture != presencecapture.FailConservativeDegrade {
		return noop, noop, nil
	}
	if _, err := tx.ExecContext(ctx, statements.open); err != nil {
		// Without a savepoint the degrade branch cannot restore the
		// transaction, so there is no safe way to proceed with the write.
		return nil, nil, fmt.Errorf("open %s savepoint: %w", statements.name, err)
	}
	rollback := func() error {
		if _, err := tx.ExecContext(ctx, statements.rollback); err != nil {
			return fmt.Errorf("restore %s savepoint: %w", statements.name, err)
		}
		return nil
	}
	release := func() error {
		if _, err := tx.ExecContext(ctx, statements.release); err != nil {
			return fmt.Errorf("release %s savepoint: %w", statements.name, err)
		}
		return nil
	}
	return rollback, release, nil
}
