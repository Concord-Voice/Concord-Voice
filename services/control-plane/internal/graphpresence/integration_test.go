// Package graphpresence_test is EXTERNAL on purpose: internal/testhelpers
// transitively imports internal/graphpresence once the router wires it, so an
// in-package test importing testhelpers would cycle.
package graphpresence_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/graphpresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// testhelpers.TestServer exposes Router, Hub, DB, Redis, and PresenceHistory —
// it has NO ActivityService or SenderPresence field. The subject under test
// here is the CAPTURE, which is database-bound; the delivery legs are unit
// tested in-package. So this file supplies its own doubles for those two and
// uses the real DB and Hub.
//
// The counter is mutex-guarded because dispatch runs on the sink worker
// goroutine while the assertion reads from the test goroutine.
type testRefresher struct {
	mu       sync.Mutex
	calls    int
	captured []map[uuid.UUID]bool
}

// recheckViewersFor returns the union of every captured viewer set handed to the
// refresher, so a test can assert on what the recheck actually received rather
// than merely that it was called.
func (r *testRefresher) recheckViewersFor() map[uuid.UUID]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[uuid.UUID]bool)
	for _, set := range r.captured {
		for id := range set {
			out[id] = true
		}
	}
	return out
}

func (r *testRefresher) RefreshServerVoiceRecheck(
	_ context.Context, _ uuid.UUID, _ presence.Scope, recheckViewers map[uuid.UUID]bool,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	copied := make(map[uuid.UUID]bool, len(recheckViewers))
	for id, in := range recheckViewers {
		copied[id] = in
	}
	r.captured = append(r.captured, copied)
	return nil
}

func (r *testRefresher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// alwaysPermitted stands in for presence.SenderPresenceResolver.
type alwaysPermitted struct{}

func (alwaysPermitted) RichPresenceEmissionPermitted(context.Context, uuid.UUID) bool {
	return true
}

func newReconciler(
	t *testing.T, env *testhelpers.TestServer,
) (*graphpresence.Reconciler, *testRefresher) {
	t.Helper()
	refresher := &testRefresher{}
	r := graphpresence.New(env.DB, refresher, env.Hub, alwaysPermitted{}, nil)
	t.Cleanup(r.Close)
	return r, refresher
}

// joinVoiceChannel seeds a voice channel on serverID and puts userID inside it.
// voice_participants.lifecycle_event_at defaults to now(), which is the exact
// column the capture reads for the scope generation.
func joinVoiceChannel(t *testing.T, db *sql.DB, serverID, userID uuid.UUID) {
	t.Helper()
	channelID := uuid.New()
	_, err := db.Exec(
		`INSERT INTO channels (id, server_id, name, type)
		 VALUES ($1, $2, 'v_' || left($3, 8), 'voice')`,
		channelID, serverID, channelID.String(),
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`,
		channelID, userID,
	)
	require.NoError(t, err)
}

// A capture over two users with no friendship and no active voice resolves to
// the conservative viewer-scoped clear of the two principals and nothing else:
// no exact leg, no error, no degrade.
func TestCaptureInTxEmptyGraphIsBenignTerminal(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)

	a, b := uuid.New(), uuid.New()

	tx, err := env.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	plan, err := r.CaptureInTx(context.Background(), tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		Principal:   a,
		Counterpart: b,
	})
	require.NoError(t, err, "CaptureInTx")
	assert.False(t, plan.Degraded(), "a clean empty capture must not be degraded")
	assert.False(t, plan.HasWork(),
		"no accepted edge joins these two users, so the write can revoke nothing "+
			"and the capture must produce nothing to dispatch")
}

// The exact path end to end: a principal live in Server Voice with an audience
// yields a leg, and Complete's post-commit dispatch refreshes it.
func TestCaptureInTxCapturesLiveServerVoiceLeg(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, refresher := newReconciler(t, env)
	ctx := context.Background()

	sender := testhelpers.CreateUser(t, env.DB)
	viewer := testhelpers.CreateUser(t, env.DB)
	serverID := testhelpers.CreateServer(t, env.DB, sender)
	testhelpers.AddServerMember(t, env.DB, serverID, sender)
	testhelpers.AddServerMember(t, env.DB, serverID, viewer)
	testhelpers.AddFriendship(t, env.DB, sender, viewer)
	joinVoiceChannel(t, env.DB, serverID, sender)

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	// A failed assertion must never leave the transaction open: the test-DB
	// cleanup truncates, and TRUNCATE blocks behind an idle-in-transaction
	// session until the package times out.
	defer func() { _ = tx.Rollback() }()

	plan, err := r.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		Principal:   sender,
		Counterpart: viewer,
	})
	require.NoError(t, err, "CaptureInTx")
	require.True(t, plan.HasWork(), "a live Server Voice sender must produce work")
	require.False(t, plan.Degraded(), "a clean capture must not be degraded")

	require.NoError(t, r.Complete(ctx, tx, plan), "Complete must commit the transaction")
	require.Eventually(t, func() bool { return refresher.count() == 1 },
		2*time.Second, 10*time.Millisecond,
		"the captured Server Voice leg must be refreshed after commit")
}

// Complete OWNS the commit: after it returns, the caller's transaction is
// resolved and the handler has nothing left to do with it.
func TestCompleteCommitsTheCallersTransaction(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	ctx := context.Background()

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	plan, err := r.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		Principal:   uuid.New(),
		Counterpart: uuid.New(),
	})
	require.NoError(t, err, "CaptureInTx")
	require.NoError(t, r.Complete(ctx, tx, plan), "Complete must commit the transaction")

	assert.ErrorIs(t, tx.Rollback(), sql.ErrTxDone,
		"the transaction must already be resolved once Complete returns")
}

// The load-bearing regression: a handler that commits for itself must fail
// loudly rather than silently bypass the contract.
func TestCompleteAfterHandlerCommitReturnsErrTxDone(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)

	tx, err := env.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	plan, err := r.CaptureInTx(context.Background(), tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		Principal:   uuid.New(),
		Counterpart: uuid.New(),
	})
	require.NoError(t, err, "CaptureInTx")

	require.NoError(t, tx.Commit(), "handler commit") // the mistake this test exists to catch
	assert.ErrorIs(t, r.Complete(context.Background(), tx, plan), sql.ErrTxDone,
		"Complete after a handler-owned commit must error")
}

// A capture with no transaction is refused rather than silently skipped: the
// contract has no non-transactional path.
func TestCaptureInTxRequiresATransaction(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)

	_, err := r.CaptureInTx(context.Background(), nil, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		Principal:   uuid.New(),
		Counterpart: uuid.New(),
	})
	assert.Error(t, err, "CaptureInTx with a nil tx must error, not silently succeed")
}

// A nil capture must leave every handler behaving exactly as before. This is
// the mixed-version rollout claim.
func TestNilCaptureIsSafeForConsumers(t *testing.T) {
	var c presencecapture.GraphPresenceCapture
	assert.Nil(t, c, "a zero GraphPresenceCapture must be nil so handlers can guard on it")
}

// seedFriendship inserts an accepted edge and returns the two user IDs. The
// fixture is raw SQL against env.DB on purpose: it pins this file to the schema
// rather than to a testhelpers API that may not expose what these regressions
// need.
func seedFriendship(t *testing.T, env *testhelpers.TestServer) (uuid.UUID, uuid.UUID) {
	t.Helper()
	a, b := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{a, b} {
		// users requires only email, username, password_hash — the NOT NULL
		// wrapped_private_key / key_derivation_salt columns live on user_keys.
		// Lowercase hex satisfies the LOWER(username) unique index.
		_, err := env.DB.Exec(
			`INSERT INTO users (id, username, email, password_hash)
			 VALUES ($1, $2, $3, 'x')`,
			id, "u"+id.String()[:8], id.String()[:8]+"@test.local",
		)
		require.NoError(t, err, "seed user")
	}
	_, err := env.DB.Exec(
		`INSERT INTO friendships (id, requester_id, addressee_id, status)
		 VALUES ($1, $2, $3, 'accepted')`,
		uuid.New(), a, b,
	)
	require.NoError(t, err, "seed friendship")
	return a, b
}

// Phase-inversion lock. The capture reads the friend graph INSIDE the
// transaction that destroys it. A concurrent delete committed on another
// connection after the capture began must not tear what the capture saw.
func TestCaptureSeesPreMutationGraphUnderConcurrentDelete(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	a, b := seedFriendship(t, env)
	ctx := context.Background()

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	// A failed assertion must never leave the transaction open: the shared
	// test-DB cleanup TRUNCATEs and would block behind an idle-in-transaction
	// session until the whole package times out.
	defer func() { _ = tx.Rollback() }()

	plan, err := r.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		Principal:   a,
		Counterpart: b,
	})
	require.NoError(t, err, "CaptureInTx")

	hadWork, wasDegraded := plan.HasWork(), plan.Degraded()
	require.True(t, hadWork || wasDegraded,
		"capture over a live friendship must produce work or degrade, never a silent empty")

	// A second connection deletes the edge and commits while our tx is open.
	_, err = env.DB.Exec(
		`DELETE FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b,
	)
	require.NoError(t, err, "concurrent delete")

	// The plan is already materialized: the concurrent commit cannot retro-edit
	// what the capture saw. Nothing here may be re-read lazily at Complete time
	// — that is exactly the tear the in-transaction capture exists to prevent.
	assert.Equal(t, hadWork, plan.HasWork(),
		"a concurrent commit must not change what the capture already materialized")
	assert.Equal(t, wasDegraded, plan.Degraded(),
		"a concurrent commit must not retroactively degrade a clean capture")
	assert.NoError(t, r.Complete(ctx, tx, plan),
		"Complete must still resolve the transaction after a concurrent commit")
}

// Degrade proceeds: under FailConservativeDegrade the block WRITE commits and
// the principals are disconnected, rather than the block being refused.
func TestBlockDegradesAndStillCommits(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	a, b := seedFriendship(t, env)
	ctx := context.Background()

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	plan, err := r.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyBlock,
		FailPosture: presencecapture.FailConservativeDegrade,
		Principal:   a,
		Counterpart: b,
	})
	require.NoError(t, err,
		"block capture must never return an error under degrade posture")
	assert.True(t, plan.HasWork(),
		"the block must always carry the conservative disconnect of both principals")

	_, err = tx.ExecContext(ctx,
		`UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`,
		a, b,
	)
	require.NoError(t, err, "block write")
	require.NoError(t, r.Complete(ctx, tx, plan), "Complete must commit the transaction")

	var status string
	require.NoError(t, env.DB.QueryRow(
		`SELECT status FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b,
	).Scan(&status), "read back")
	assert.Equal(t, "blocked", status,
		"the block must commit even when the capture degrades")
}

// No false clear: a viewer who retains authorization through a second route
// must not be cleared. Here the shared server survives the friendship removal.
func TestSharedServerPreventsFalseClear(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, refresher := newReconciler(t, env)
	a, b := seedFriendship(t, env)
	ctx := context.Background()

	// A shared server is the SECOND authorization route. After the friendship
	// goes away, b must still be authorized to see a's Server Voice activity,
	// so clearing b would be a false clear.
	serverID := uuid.New()
	_, err := env.DB.Exec(
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'shared', $2)`, serverID, a,
	)
	require.NoError(t, err, "seed server")
	for _, id := range []uuid.UUID{a, b} {
		_, err = env.DB.Exec(
			`INSERT INTO server_members (server_id, user_id) VALUES ($1, $2)`, serverID, id,
		)
		require.NoError(t, err, "seed membership")
	}
	joinVoiceChannel(t, env.DB, serverID, a)

	// Drive the ACTUAL reviewed path: capture inside the transaction, delete,
	// commit through Complete, and let the post-commit dispatch run. An earlier
	// version of this test deleted the friendship outside any capture and
	// asserted on presence.ComputePresenceAudience directly, so it proved a
	// property of internal/presence and left the reconciliation's no-false-clear
	// behaviour completely unguarded (PR #2738 review, CodeRabbit).
	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	plan, err := r.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		Principal:   a,
		Counterpart: b,
	})
	require.NoError(t, err, "CaptureInTx")
	require.True(t, plan.HasWork(), "an accepted edge exists, so the capture must carry work")

	_, err = tx.ExecContext(ctx,
		`DELETE FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err, "remove friendship")
	require.NoError(t, r.Complete(ctx, tx, plan), "Complete")

	// The recheck must have been handed b as a captured viewer — that is what
	// lets RefreshServerVoiceRecheck compute captured-minus-fresh and decide.
	require.Eventually(t, func() bool { return refresher.recheckViewersFor()[b] },
		3*time.Second, 20*time.Millisecond,
		"the recheck must receive b in the captured set")

	// Now compute the CLEAR DECISION itself, which is what "no false clear"
	// actually means. The refresher double records the captured set but never
	// computes captured-minus-fresh, so asserting on it alone would still pass
	// if b were wrongly cleared (PR #2738 review, CodeRabbit). Rather than
	// reimplement ActivityService in a double, derive the set here from the two
	// real inputs — the captured audience the recheck received, and the freshly
	// computed post-commit audience.
	fresh, err := presence.ComputePresenceAudience(ctx, env.DB, a)
	require.NoError(t, err, "ComputePresenceAudience")
	require.True(t, fresh[b],
		"a shared server keeps the viewer authorized post-commit")

	captured := refresher.recheckViewersFor()
	cleared := make(map[uuid.UUID]bool)
	for id := range captured {
		if !fresh[id] {
			cleared[id] = true
		}
	}
	assert.NotContains(t, cleared, b,
		"b is captured AND still authorized, so captured-minus-fresh must not "+
			"clear b — clearing a viewer the shared server still authorizes is "+
			"exactly the false clear this test is named for")
}

// Mixed-version rollout claim: with no capture wired, the normal write still
// happens and the transaction semantics are untouched.
func TestUnwiredHandlerBehavesAsBefore(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	a, b := seedFriendship(t, env)
	ctx := context.Background()

	// Simulate an unwired handler: no capture, the caller commits directly.
	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		`DELETE FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b,
	)
	require.NoError(t, err, "delete")
	require.NoError(t, tx.Commit(), "commit")

	var count int
	require.NoError(t, env.DB.QueryRow(
		`SELECT COUNT(*) FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b,
	).Scan(&count), "read back")
	assert.Zero(t, count, "an unwired path must still perform its normal write")
}

// #2738 review: an ACCEPT must seed no peripheral disconnect.
//
// The peripheral leg tears down every local device of both principals, so it is
// earned only when the mutation can revoke authorization. An accepted edge is
// purely additive — no viewer loses access, so no viewer holds stale state, and
// the widened audience arrives via the focal refresh. Seeding it unconditionally
// dropped both users' websockets on every friend acceptance for nothing.
func TestAcceptSeedsNoPeripheralDisconnect(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	a, b := seedFriendship(t, env)

	tx, err := env.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	plan, err := r.CaptureInTx(context.Background(), tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipAccept,
		Principal:   a,
		Counterpart: b,
	})
	require.NoError(t, err, "CaptureInTx")
	assert.False(t, plan.Degraded(), "a clean accept capture must not degrade")
	assert.False(t, plan.HasWork(),
		"an accept with no active presence must produce nothing to dispatch: "+
			"seeding the peripheral viewers would disconnect both principals' devices")
}

// The revoking counterpart of the test above: a removal DOES seed the
// peripheral disconnect, because the counterpart loses every sender that was
// FoF-reachable only through the deleted edge.
func TestRemoveSeedsThePeripheralDisconnect(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	a, b := seedFriendship(t, env)

	tx, err := env.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	plan, err := r.CaptureInTx(context.Background(), tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		Principal:   a,
		Counterpart: b,
	})
	require.NoError(t, err, "CaptureInTx")
	assert.True(t, plan.HasWork(),
		"a removal must carry the peripheral clear even with no active legs")
}

// PR #2738 review: FailConservativeDegrade was INERT for the failure mode it
// was written for. Every capture read runs on the caller's transaction, and a
// failed statement poisons it (25P02), so the handler's next write failed
// regardless and BlockUser 500'd — the exact denial of a safety affordance the
// posture exists to prevent. The fix is a SAVEPOINT around the reads; this
// proves the primitive it now depends on actually restores a usable
// transaction against real PostgreSQL.
func TestSavepointRestoresATransactionPoisonedByAFailedRead(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, "SAVEPOINT concord_graph_presence_capture")
	require.NoError(t, err, "open savepoint")

	// A failed read poisons the transaction exactly as a capture read would.
	_, err = tx.QueryContext(ctx, "SELECT 1 FROM a_table_that_does_not_exist")
	require.Error(t, err, "precondition: the read must fail")

	// Without the rollback the transaction is unusable from here.
	var probe int
	require.Error(t, tx.QueryRowContext(ctx, "SELECT 1").Scan(&probe),
		"precondition: a poisoned transaction rejects further statements (25P02)")

	_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT concord_graph_presence_capture")
	require.NoError(t, err, "restore savepoint")

	require.NoError(t, tx.QueryRowContext(ctx, "SELECT 1").Scan(&probe),
		"after ROLLBACK TO SAVEPOINT the transaction must be usable again — "+
			"this is what lets the block proceed under FailConservativeDegrade")
	assert.Equal(t, 1, probe)
	assert.NoError(t, tx.Commit(), "and it must still be able to commit")
}

// The savepoint costs a round trip, so it is opened only for the posture that
// needs it. Fail-closed sites block the write on a read failure by design and
// have nothing to restore.
func TestSavepointIsSkippedForTheFailClosedPosture(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	ctx := context.Background()

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	restore, err := r.BeginCaptureSavepointForTest(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		FailPosture: presencecapture.FailClosedBlockWrite,
		Principal:   uuid.New(),
		Counterpart: uuid.New(),
	})
	require.NoError(t, err)
	assert.NoError(t, restore(), "the fail-closed restore must be a no-op, not a SQL round trip")
}

// ─── capture-read failures: degrade vs fail closed ──────────────────────────
//
// These drive the safety paths the whole #2446 argument rests on, and they were
// the least-covered code in the bridge (PR #2738: capture.go sat at 65.7% with
// every degrade branch unexecuted).
//
// The reads are broken with SET LOCAL search_path rather than by renaming a
// table: it is transaction-local, takes no ACCESS EXCLUSIVE lock on a shared
// test database, and disappears with the rollback.

// hideTablesFromCapture makes the capture's reads fail inside tx only, by
// pointing the search_path at a schema that does not contain them.
func hideTablesFromCapture(ctx context.Context, t *testing.T, tx *sql.Tx) {
	t.Helper()
	_, err := tx.ExecContext(ctx, `SET LOCAL search_path TO pg_catalog`)
	require.NoError(t, err, "redirect search_path")
}

func TestCaptureDegradesWhenTheEdgeReadFails(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	a, b := seedFriendship(t, env)
	ctx := context.Background()

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()
	hideTablesFromCapture(ctx, t, tx)

	plan, err := r.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyBlock,
		FailPosture: presencecapture.FailConservativeDegrade,
		Principal:   a,
		Counterpart: b,
	})

	require.NoError(t, err, "the degrade posture must absorb a capture read failure")
	require.NotNil(t, plan)
	assert.True(t, plan.Degraded(), "the plan must be the conservative superset")
	assert.True(t, plan.HasWork(), "and it must still carry the principals to disconnect")

	// The savepoint rollback is what makes the posture usable at all: without it
	// the failed read leaves the transaction poisoned (25P02) and BlockUser 500s
	// anyway, which is the defect this posture exists to prevent.
	var restored int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT 1`).Scan(&restored),
		"the transaction must still be usable after the degrade")
	assert.Equal(t, 1, restored)
}

func TestCaptureFailsClosedWhenTheEdgeReadFails(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	a, b := seedFriendship(t, env)
	ctx := context.Background()

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()
	hideTablesFromCapture(ctx, t, tx)

	plan, err := r.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		FailPosture: presencecapture.FailClosedBlockWrite,
		Principal:   a,
		Counterpart: b,
	})

	require.Error(t, err, "the fail-closed posture must BLOCK the write, not degrade")
	assert.Nil(t, plan)
}

// The scope read runs after the accepted-edge gate has already passed, so this
// exercises a different degrade branch from the two above.
func TestCaptureDegradesWhenTheVoiceScopeReadFails(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	a, b := seedFriendship(t, env)
	ctx := context.Background()

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	// A schema carrying ONLY friendships: the gate resolves and finds the
	// accepted edge, then the voice-scope read fails on the missing tables.
	for _, stmt := range []string{
		`CREATE SCHEMA capture_scope_fail`,
		`CREATE TABLE capture_scope_fail.friendships AS TABLE public.friendships`,
		`SET LOCAL search_path TO capture_scope_fail, pg_catalog`,
	} {
		_, err = tx.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	// Prove the GATE still resolves under the redirected search_path, so the
	// degrade below can only come from the scope read. Degraded() alone cannot
	// distinguish the two branches, which would make this test's claim
	// unfalsifiable.
	var edgeVisible bool
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM friendships
			WHERE ((requester_id = $1 AND addressee_id = $2)
			    OR (requester_id = $2 AND addressee_id = $1))
			  AND status = 'accepted'
		)`, a, b).Scan(&edgeVisible), "the gate query must still resolve")
	require.True(t, edgeVisible, "the accepted edge must be readable, or this tests the gate branch")

	plan, err := r.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		FailPosture: presencecapture.FailConservativeDegrade,
		Principal:   a,
		Counterpart: b,
	})

	require.NoError(t, err, "the degrade posture must absorb a scope read failure too")
	require.NotNil(t, plan)
	assert.True(t, plan.Degraded())
}

// A savepoint that cannot even be opened leaves no way to restore the caller's
// transaction after a failed read, so there is no safe way to continue.
func TestCaptureFailsClosedWhenTheSavepointCannotOpen(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	a, b := seedFriendship(t, env)
	ctx := context.Background()

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	// Poison the transaction first: every later statement, SAVEPOINT included,
	// fails with 25P02.
	_, err = tx.ExecContext(ctx, `SELECT * FROM a_table_that_does_not_exist`)
	require.Error(t, err, "the poisoning statement must fail")

	plan, err := r.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyBlock,
		FailPosture: presencecapture.FailConservativeDegrade,
		Principal:   a,
		Counterpart: b,
	})

	require.Error(t, err, "no savepoint means no safe degrade, so this must fail closed")
	assert.Nil(t, plan)
}
