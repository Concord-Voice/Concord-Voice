// Package graphpresence_test is EXTERNAL on purpose: internal/testhelpers
// transitively imports internal/graphpresence once the router wires it, so an
// in-package test importing testhelpers would cycle.
package graphpresence_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/graphpresence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
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

func (d alwaysPermitted) RichPresenceEmissionState(
	ctx context.Context, senderID uuid.UUID,
) (bool, error) {
	// Always DETERMINED, so it exercises the suppression path rather than the
	// indeterminate one. undeterminedPresence below is the other half.
	return d.RichPresenceEmissionPermitted(ctx, senderID), nil
}

// undeterminedPresence is a resolver that CANNOT answer — the Redis-blip shape.
// Under the old bool-only contract this was indistinguishable from a
// suppression and silently dropped the leg.
type undeterminedPresence struct{}

func (undeterminedPresence) RichPresenceEmissionPermitted(context.Context, uuid.UUID) bool {
	return false // the bool form still absorbs it, exactly as #2444 requires
}

func (undeterminedPresence) RichPresenceEmissionState(
	context.Context, uuid.UUID,
) (bool, error) {
	return false, errors.New("presence lookup unavailable")
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

// The mixed-version rollout claim is locked by TestUnwiredHandlerBehavesAsBefore
// below. A test asserting `var c GraphPresenceCapture; assert.Nil(t, c)` used to
// stand here — that asserts the Go language's own zero-value rule, executes no
// project code, and cannot fail for any change to this bridge (PR #2738 review,
// @code-reviewer).

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
//
// The assertion is on the viewer set the RECHECK receives, not on the returned
// plan's accessors. An earlier version compared plan.HasWork()/Degraded()
// before and after the concurrent delete, which only proved those two bools are
// not lazily recomputed — they read already-populated maps, so they could not
// have changed however wrong the capture's phase placement was (PR #2738
// review, @code-reviewer).
func TestCaptureSeesPreMutationGraphUnderConcurrentDelete(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, refresher := newReconciler(t, env)
	a, b := seedFriendship(t, env)
	ctx := context.Background()

	// A Server Voice candidate set at the default TierFriends is
	// serverMembers ∩ (friends ∪ FoF) — an INTERSECTION (presence.
	// serverVoiceCandidates), so b must be BOTH a server member and a friend to
	// be captured at all. Membership alone would keep b in the set after the
	// friendship is gone and make this test pass regardless of when the capture
	// ran; friendship alone never captures b in the first place.
	//
	// Deleting the edge therefore removes b from any POST-mutation re-read
	// while leaving b in the pre-mutation capture, which is the discrimination
	// this test needs.
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
	require.True(t, plan.HasWork(),
		"precondition: a in voice with an accepted edge must capture work")

	// A second connection deletes the edge and commits while our tx is open, so
	// by the time Complete runs the friendship no longer exists anywhere.
	_, err = env.DB.Exec(
		`DELETE FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b,
	)
	require.NoError(t, err, "concurrent delete")

	require.NoError(t, r.Complete(ctx, tx, plan), "Complete")

	// b was a viewer BEFORE the delete and must still be handed to the recheck.
	// A capture that ran after the write, or that re-read anything at dispatch
	// time, would hand over a set with b already gone — and b would then keep
	// a's activity on screen with no clear ever computed for them.
	require.Eventually(t, func() bool { return refresher.recheckViewersFor()[b] },
		3*time.Second, 20*time.Millisecond,
		"the recheck must receive the PRE-mutation viewer, not a post-delete re-read")
}

// Degrade proceeds: under FailConservativeDegrade the block WRITE commits and
// the principals are disconnected, rather than the block being refused.
//
// The capture read is BROKEN here on purpose. An earlier version of this test
// ran a perfectly healthy capture, so it never entered the degrade branch and
// never asserted Degraded() — it proved only that a block commits, under a name
// promising it proved the degrade commits (PR #2738 review, @code-reviewer).
// The distinction is the whole point of the posture: #2446 names blocking the
// priority regression, so a capture failure must not deny the safety
// affordance.
func TestBlockDegradesAndStillCommits(t *testing.T) {
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
	require.NoError(t, err,
		"block capture must never return an error under degrade posture")
	require.True(t, plan.Degraded(),
		"a failed capture read under this posture must produce the conservative plan")
	assert.True(t, plan.HasWork(),
		"the block must always carry the conservative disconnect of both principals")

	// The savepoint restore does not undo the search_path redirect, which was
	// set before the capture opened its savepoint. Put it back so the block
	// write below exercises the handler's real statement rather than failing on
	// a table the test itself hid.
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path TO public, pg_catalog`)
	require.NoError(t, err, "restore search_path")

	_, err = tx.ExecContext(ctx,
		`UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`,
		a, b,
	)
	require.NoError(t, err,
		"the write must succeed after a degrade — a poisoned transaction here is "+
			"the exact inertness the capture savepoint exists to prevent")
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

	// This test owns the two INPUTS to the clear decision; it does not own the
	// decision. The subtraction lives in presence.recheckedActivityClears, which
	// is unexported and reachable only through the real ActivityService that
	// this file replaces with a double — so a locally re-derived
	// captured-minus-fresh could only ever agree with itself.
	//
	// An earlier version derived it anyway and then asserted b was absent from
	// it, two lines after a require.True(t, fresh[b]) that made that absence
	// arithmetically certain: the assertion could not fail while its own
	// precondition held (PR #2738 review, @code-reviewer). What remains are the
	// two facts graphpresence is actually responsible for, each independently
	// falsifiable.

	// (1) b reaches the recheck. Without this the subtraction never considers b
	// at all and the counterpart of a removal is silently never reconciled.
	require.Eventually(t, func() bool { return refresher.recheckViewersFor()[b] },
		3*time.Second, 20*time.Millisecond,
		"the recheck must receive b in the captured set")

	// (2) b is STILL authorized once the write has committed. This is the fact
	// that makes clearing b a false clear rather than a correct one, and it is
	// the half that breaks if the shared-server route ever stops counting.
	fresh, err := presence.ComputePresenceAudience(ctx, env.DB, a)
	require.NoError(t, err, "ComputePresenceAudience")
	assert.True(t, fresh[b],
		"the shared server must keep b authorized after the friendship is gone — "+
			"a captured viewer who drops out of the fresh audience is exactly what "+
			"recheckedActivityClears clears")
}

// Mixed-version rollout claim: with no capture wired, the normal write still
// happens and the transaction semantics are untouched.
//
// This drives the REAL handler plumbing — presencehook's three terminals — with
// a nil capture, which is what a replica that never called
// SetGraphPresenceCapture actually holds. An earlier version ran a raw
// tx.ExecContext + tx.Commit() and never referenced presencehook or
// graphpresence at all, so it asserted that PostgreSQL commits a DELETE and
// could not fail for any change to this PR's code (PR #2738 review,
// @code-reviewer). The commit assertion below matters precisely because the
// handler no longer owns the commit: Complete does, on BOTH paths.
func TestUnwiredHandlerBehavesAsBefore(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	a, b := seedFriendship(t, env)
	ctx := context.Background()

	// A nil INTERFACE, not a typed nil: this is the value the consumer field
	// holds before any wiring runs.
	var capture presencecapture.GraphPresenceCapture

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer presencehook.RollbackUnlessDone(tx, logger.New("test"))

	plan, err := presencehook.Capture(ctx, capture, tx, presencehook.Spec{
		Family:        presencecapture.FamilyFriendshipRemove,
		PrincipalID:   a.String(),
		CounterpartID: b.String(),
	})
	require.NoError(t, err,
		"an unwired capture must not fail the handler — including on the ID parse, "+
			"which an unwired path never had to perform before #2446")
	require.Nil(t, plan, "an unwired capture produces no plan")

	_, err = tx.ExecContext(ctx,
		`DELETE FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b,
	)
	require.NoError(t, err, "delete")

	// The handler never calls tx.Commit() itself on either path. If the unwired
	// branch of Complete stopped committing, the deferred rollback would discard
	// this delete and the read-back below would still find the row — the write
	// silently lost behind an HTTP 200.
	require.NoError(t, presencehook.Complete(ctx, capture, tx, plan),
		"Complete owns the commit on the unwired path too")

	var count int
	require.NoError(t, env.DB.QueryRow(
		`SELECT COUNT(*) FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b,
	).Scan(&count), "read back")
	assert.Zero(t, count, "an unwired path must still perform its normal write")

	// The third terminal. A handler's error path calls this unconditionally, so
	// an unwired replica reaches it with a nil capture and a nil plan.
	assert.NotPanics(t, func() {
		presencehook.Abandon(capture, plan, presencecapture.CauseCommitUnresolved)
	}, "the fail-closed terminal must be safe on an unwired replica")
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
// Both statements come from the bridge's own beginCaptureSavepoint. An earlier
// version hand-wrote the SAVEPOINT and ROLLBACK TO SAVEPOINT SQL and never
// called the helper, so it proved a property of PostgreSQL: the two literals
// could have been renamed, mismatched, or the whole helper deleted and this
// test would still pass (PR #2738 review, @code-reviewer).
func TestSavepointRestoresATransactionPoisonedByAFailedRead(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	ctx := context.Background()

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	restore, err := r.BeginCaptureSavepointForTest(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyBlock,
		FailPosture: presencecapture.FailConservativeDegrade,
		Principal:   uuid.New(),
		Counterpart: uuid.New(),
	})
	require.NoError(t, err, "open savepoint")

	// A failed read poisons the transaction exactly as a capture read would.
	_, err = tx.QueryContext(ctx, "SELECT 1 FROM a_table_that_does_not_exist")
	require.Error(t, err, "precondition: the read must fail")

	// Without the rollback the transaction is unusable from here.
	var probe int
	require.Error(t, tx.QueryRowContext(ctx, "SELECT 1").Scan(&probe),
		"precondition: a poisoned transaction rejects further statements (25P02)")

	require.NoError(t, restore(), "restore savepoint")

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

// Refusing a foreign plan means refusing to COMMIT it. The error is the visible
// half; the transaction still being open is the half that matters, because the
// whole reason to refuse is that committing while silently dropping the
// dispatch leaves viewers holding revoked state with no signal at all.
//
// This runs against a real transaction on purpose. The in-package version
// passed a zero-value &sql.Tx{}, which made the test brittle in both
// directions: it never observed whether tx was committed, and a reordering that
// moved the guard after the commit would panic inside database/sql rather than
// fail as a test (PR #2738 review, @code-reviewer).
func TestCompleteRejectsAForeignPlanWithoutCommitting(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	a, b := seedFriendship(t, env)
	ctx := context.Background()

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		`DELETE FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b,
	)
	require.NoError(t, err, "the write the refusal must not commit")

	err = r.Complete(ctx, tx, foreignTestPlan{})
	require.Error(t, err, "a plan this bridge did not build must be refused")
	assert.Contains(t, err.Error(), "foreign")

	// The refusal returned BEFORE touching tx, so the transaction is still open
	// and still usable.
	var probe int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT 1`).Scan(&probe),
		"the refusal must leave the caller's transaction intact to roll back")

	require.NoError(t, tx.Rollback(), "rollback")

	var count int
	require.NoError(t, env.DB.QueryRow(
		`SELECT COUNT(*) FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b,
	).Scan(&count), "read back")
	assert.Equal(t, 1, count,
		"the refused write must NOT have landed — a guard that ran after the commit "+
			"would leave the mutation applied with its dispatch silently dropped")
}

// foreignTestPlan satisfies presencecapture.Plan without being this bridge's
// concrete *Plan.
type foreignTestPlan struct{}

func (foreignTestPlan) HasWork() bool  { return true }
func (foreignTestPlan) Degraded() bool { return false }

// The silent-skip path this PR made visible. A sender holding a LIVE voice row
// whose capture resolves to zero candidates is dropped from the plan with no
// error and Degraded() false — so before the warn log there was no signal at
// all that a leg had gone missing (PR #2738 review, @security-reviewer).
//
// Zero candidates is reachable without any Redis fault: a Server Voice
// candidate set at the default TierFriends is serverMembers ∩ (friends ∪ FoF),
// so a sender alone on the server whose only friend is not a member intersects
// to empty. That is a legitimate suppression rather than the indeterminate
// case, and the whole point of the finding is that the bridge CANNOT tell them
// apart — CaptureServerVoiceCandidates returns (empty, nil) for both, including
// when RichPresenceEmissionPermitted fails closed on a Redis error. This test
// therefore locks the SIGNAL, not the classification; the sentinel that would
// separate the two changes a contract voicepresence also consumes and is left
// to the author.
func TestActiveScopeWithNoCandidatesIsLoggedNotSilentlyDropped(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	a, b := seedFriendship(t, env)
	ctx := context.Background()

	// a is the ONLY member, so the intersection with a's friends is empty.
	serverID := uuid.New()
	_, err := env.DB.Exec(
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'solo', $2)`, serverID, a,
	)
	require.NoError(t, err, "seed server")
	_, err = env.DB.Exec(
		`INSERT INTO server_members (server_id, user_id) VALUES ($1, $2)`, serverID, a,
	)
	require.NoError(t, err, "seed membership")
	joinVoiceChannel(t, env.DB, serverID, a)

	var logBuf bytes.Buffer
	r := graphpresence.New(env.DB, &testRefresher{}, env.Hub, alwaysPermitted{},
		logger.NewWithWriter(&logBuf))
	t.Cleanup(r.Close)

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	plan, err := r.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		Principal:   a,
		Counterpart: b,
	})
	require.NoError(t, err, "a skipped scope is not an error")

	assert.Contains(t, logBuf.String(), "failure_class=no_candidates",
		"a dropped leg must leave a fixed-enum trace — an operator cannot act on a skip "+
			"that emits nothing")
	assert.Contains(t, logBuf.String(), "graph presence skipped an active scope with no candidates")

	// The log is the only signal: the capture still returns cleanly and is not
	// marked degraded, which is exactly why the skip was invisible.
	assert.False(t, plan.Degraded(),
		"the skip does not degrade the plan — locking this is what makes the log load-bearing")

	// The removal's peripheral seed survives, so the plan is not empty; the
	// missing piece is the LEG, whose captured third parties are never cleared.
	assert.True(t, plan.HasWork(),
		"the peripheral clear of the principals is unaffected by the dropped leg")

	// PII discipline: the log line carries a fixed enum and nothing else — no
	// user, server, or channel identifier ([internal]rules/observability.md).
	for _, id := range []uuid.UUID{a, b, serverID} {
		assert.NotContains(t, logBuf.String(), id.String(),
			"the skip log must not carry an identifier")
	}
}

// seedLiveVoiceSender puts a alone in voice on their own server with an accepted
// edge to b — a live scope whose candidate set the resolver must be consulted
// for. Shared by the two indeterminate-resolver tests below.
func seedLiveVoiceSender(t *testing.T, env *testhelpers.TestServer) (uuid.UUID, uuid.UUID) {
	t.Helper()
	a, b := seedFriendship(t, env)
	serverID := uuid.New()
	_, err := env.DB.Exec(
		`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'solo', $2)`, serverID, a)
	require.NoError(t, err, "seed server")
	_, err = env.DB.Exec(
		`INSERT INTO server_members (server_id, user_id) VALUES ($1, $2)`, serverID, a)
	require.NoError(t, err, "seed membership")
	joinVoiceChannel(t, env.DB, serverID, a)
	return a, b
}

// CWE-284, the Major from PR #2770 review (CodeRabbit). A resolver that cannot
// DETERMINE the sender's base presence must not resolve to an empty audience:
// that made a transient Redis fault indistinguishable from a legitimate
// suppression, dropped the leg, and left a viewer who had just lost
// authorization holding the sender's activity until the presence TTL expired —
// while the caller's declared FailPosture never ran at all.
//
// Fail-closed posture: the write is BLOCKED. Nothing changed, nothing was
// disclosed, and the request is retryable.
func TestIndeterminatePresenceBlocksTheWriteUnderFailClosed(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	a, b := seedLiveVoiceSender(t, env)
	ctx := context.Background()

	r := graphpresence.New(env.DB, &testRefresher{}, env.Hub, undeterminedPresence{}, nil)
	t.Cleanup(r.Close)

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	plan, err := r.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		FailPosture: presencecapture.FailClosedBlockWrite,
		Principal:   a,
		Counterpart: b,
	})

	require.Error(t, err,
		"an undetermined base presence must reach the posture, not resolve to an "+
			"empty audience — returning nil here is the silent leg drop")
	assert.Nil(t, plan)
}

// The degrade half: BlockUser must still be able to block. The capture cannot
// produce an exact delta, so it substitutes the conservative principal clear
// rather than refusing the safety affordance.
func TestIndeterminatePresenceDegradesUnderConservativePosture(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	a, b := seedLiveVoiceSender(t, env)
	ctx := context.Background()

	d := &countingDisconnectorForTest{}
	r := graphpresence.New(env.DB, &testRefresher{}, d, undeterminedPresence{}, nil)
	t.Cleanup(r.Close)

	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	plan, err := r.CaptureInTx(ctx, tx, presencecapture.Subject{
		Family:      presencecapture.FamilyBlock,
		FailPosture: presencecapture.FailConservativeDegrade,
		Principal:   a,
		Counterpart: b,
	})

	require.NoError(t, err, "the degrade posture absorbs an undetermined presence")
	require.True(t, plan.Degraded(),
		"and it must SAY so — a clean empty plan here is the defect, because the "+
			"caller records no counter and the viewers are never cleared")
	assert.True(t, plan.HasWork(), "the conservative principal clear is carried")

	// The transaction must still be usable, or the block 500s anyway and the
	// posture is inert for the failure it exists to absorb.
	var probe int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT 1`).Scan(&probe),
		"the savepoint restore must leave the caller's transaction usable")
}

// countingDisconnectorForTest is the external-package counterpart of the
// in-package countingDisconnector; the reconciler needs a non-nil disconnector.
type countingDisconnectorForTest struct{ calls int }

func (c *countingDisconnectorForTest) DisconnectRichPresenceClients(
	context.Context, map[uuid.UUID]bool,
) error {
	c.calls++
	return nil
}

func (c *countingDisconnectorForTest) DisconnectAllRichPresenceClients(context.Context) error {
	c.calls++
	return nil
}
