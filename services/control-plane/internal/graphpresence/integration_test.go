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
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
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
// The statements come from the bridge's own beginSavepoint, reached here
// through the BeginCaptureSavepointForTest export shim -- which is where the
// retired beginCaptureSavepoint name legitimately survives. It issues three:
// open, rollback and release. An earlier
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

// BeginAudienceRevocation satisfies Disconnector. This fake exists to observe
// DISCONNECTS, and the #2992 bracket is orthogonal to every assertion it
// carries, so the closer is inert. The fence's own behaviour is covered by
// topology_test.go's fenceStub, which records into the ordering trace.
func (c *countingDisconnectorForTest) BeginAudienceRevocation() func() { return func() {} }

// ─── the pre-mutation Custom Status audience (C2) ───────────────────────────

// The bridge's HAPPY path resolves a REAL audience for an active sender, and
// the in-package tests cannot reach it: they script the settings read and stop
// at the first audience query. This is the half that proves the audience is the
// authorized one — tier-resolved, then cut by the #1234 recipient exceptions as
// the FINAL filter — and that the sender is never inserted into their own
// audience, which presencehistory.cloneTopologyRecipients rejects outright.
func TestCaptureTopologyBeforeReadsTheCallerTransactionAndAppliesTheExceptions(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	sender, friend := seedFriendship(t, env)
	ctx := context.Background()

	_, err := env.DB.Exec(`
		INSERT INTO user_presence_settings (user_id, master_enabled, custom_text_tier, custom_text)
		VALUES ($1, TRUE, 1, 'in a meeting')
		ON CONFLICT (user_id) DO UPDATE
		SET master_enabled = TRUE, custom_text_tier = 1, custom_text = 'in a meeting'
	`, sender)
	require.NoError(t, err, "publish a Friends-tier custom status")

	before := captureTopologyBefore(ctx, t, env.DB, r, sender)
	require.Contains(t, before, sender)
	assert.True(t, before[sender][friend],
		"an accepted friend is inside the Friends-tier custom-text audience")
	assert.NotContains(t, before[sender], sender,
		"cloneTopologyRecipients rejects recipientID == senderID, so the settings "+
			"rail's self-insert must NOT be copied here")

	// #1234: the exception is materialized, and it is the LAST cut. It is
	// written INSIDE the capture's own transaction and never committed, so this
	// also proves the resolution runs on the CALLER's *sql.Tx rather than on
	// r.db — a resolution through the pool would not see this row at all and
	// would hand back an unfiltered audience.
	tx, err := env.DB.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO presence_override_preferences (user_id, category, encrypted_data)
		VALUES ($1, 'custom_text', 'opaque-client-blob')
	`, sender)
	require.NoError(t, err, "seed override preference document")
	_, err = tx.ExecContext(ctx, `
		INSERT INTO user_presence_overrides (sender_id, category, target_user_id)
		VALUES ($1, 'custom_text', $2)
	`, sender, friend)
	require.NoError(t, err, "seed recipient exception")

	excluded, err := r.CaptureTopologyBeforeForTest(ctx, tx, []uuid.UUID{sender})
	require.NoError(t, err, "captureTopologyBefore")
	assert.False(t, excluded[sender][friend],
		"an excepted recipient must be out of the PRE-mutation audience too, or the "+
			"delivery would clear a status they were never authorized to hold")
}

// An inactive sender carries NO audience at all. prepareTopologyPlan rejects a
// non-empty audience on an inactive operation with "inactive topology operation
// has audience", so nil is the only value that prepares.
func TestCaptureTopologyBeforeLeavesAnInactiveSenderNil(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	r, _ := newReconciler(t, env)
	sender, friend := seedFriendship(t, env)
	ctx := context.Background()

	// The friend is a real, authorized viewer — what makes the sender inactive
	// is the master toggle, not an empty graph.
	_, err := env.DB.Exec(`
		INSERT INTO user_presence_settings (user_id, master_enabled, custom_text_tier, custom_text)
		VALUES ($1, FALSE, 1, 'in a meeting')
		ON CONFLICT (user_id) DO UPDATE
		SET master_enabled = FALSE, custom_text_tier = 1, custom_text = 'in a meeting'
	`, sender)
	require.NoError(t, err, "seed a master-disabled sender")

	before := captureTopologyBefore(ctx, t, env.DB, r, sender)
	require.Contains(t, before, sender, "every focal sender gets an entry")
	assert.Nil(t, before[sender])
	assert.False(t, before[sender][friend])
}

// captureTopologyBefore runs the bridge's audience resolution in its own
// transaction and discards it, exactly as the capture reads it.
func captureTopologyBefore(
	ctx context.Context, t *testing.T, db *sql.DB,
	r *graphpresence.Reconciler, senders ...uuid.UUID,
) map[uuid.UUID]map[uuid.UUID]bool {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err, "begin")
	defer func() { _ = tx.Rollback() }()

	before, err := r.CaptureTopologyBeforeForTest(ctx, tx, senders)
	require.NoError(t, err, "captureTopologyBefore")
	return before
}

// ─── the durable C2 terminal, end to end ────────────────────────────────────

// The SUCCESS path of completeTopology, which nothing else covers. Every other
// test that reaches it lands on the REJECTION arm — the in-package terminal
// tests hand completeTopologyBatch a bare presencehistory.TopologyBatch{} and
// skip PrepareTopologyBatch entirely, and the stub rail's empty batch cannot be
// prepared at all — so the one statement none of them execute is the nil
// return, and the §3.5 checkpoint it stands for went unproven.
//
// It has to live HERE. TopologyBatch's operations and plans are unexported, so
// a batch that can actually be prepared comes from one place only: a real
// presencehistory.Service.BeginTopologyBatch over a real transaction.
//
// What it guards is predicate DRIFT. readTopologyActivity re-derives
// prepareTopologyPlan's `BeforeMasterEnabled && BeforeTier > 0 &&
// Before.Text != ""` from the settings row, and the two must agree byte for
// byte. Let the bridge's copy become the more permissive of the pair — drop a
// conjunct, stop reading master_enabled, trim where the batch does not — and it
// hands a non-empty audience to an operation the batch calls inactive:
// PrepareTopologyBatch returns "inactive topology operation has audience" and
// EVERY friendship, block and FoF write touching that sender 500s, while a
// suite that only ever exercised the rejection arm stays green.
func TestCompleteTopologyPreparesAndCommitsThroughTheRealRail(t *testing.T) {
	t.Run("an active sender carries its Before audience into a committed batch", func(t *testing.T) {
		env := testhelpers.SetupTestServer(t)
		r, _ := newReconciler(t, env)
		r.SetTopologyRail(env.PresenceHistory)
		sender, friend := seedFriendship(t, env)
		ctx := context.Background()

		seedCustomTextSettings(t, env.DB, sender, true)

		// NON-VACUITY. Without this the case passes on an empty Before set,
		// which satisfies prepareTopologyPlan's predicate trivially and would
		// prove nothing about the audience the terminal actually prepares.
		before := captureTopologyBefore(ctx, t, env.DB, r, sender)
		require.True(t, before[sender][friend],
			"the active sender must really carry a non-empty Friends-tier audience")

		subject := presencecapture.Subject{
			Family:      presencecapture.FamilyFriendshipRemove,
			Principal:   sender,
			Counterpart: friend,
		}
		err := r.WithGatedTx(ctx, subject, func(tx *sql.Tx) error {
			plan, captureErr := r.CaptureInTx(ctx, tx, subject)
			if captureErr != nil {
				return captureErr
			}
			// RemoveFriend's own predicate, so the write destroys exactly the
			// edge the capture's gate read.
			if _, execErr := tx.ExecContext(ctx, `
				DELETE FROM friendships
				WHERE ((requester_id = $1 AND addressee_id = $2)
				    OR (requester_id = $2 AND addressee_id = $1))
				  AND status = 'accepted'
			`, sender, friend); execErr != nil {
				return execErr
			}
			return r.Complete(ctx, tx, plan)
		})
		require.NoError(t, err,
			"the prepared batch must reach the rail and come back committed and delivered")

		var surviving int
		require.NoError(t, env.DB.QueryRow(`
			SELECT COUNT(*) FROM friendships
			WHERE (requester_id = $1 AND addressee_id = $2)
			   OR (requester_id = $2 AND addressee_id = $1)
		`, sender, friend).Scan(&surviving))
		assert.Zero(t, surviving,
			"the rail owns the COMMIT, so a nil return must mean the write is durable")
	})

	t.Run("a master-disabled sender prepares with no audience at all", func(t *testing.T) {
		env := testhelpers.SetupTestServer(t)
		r, _ := newReconciler(t, env)
		r.SetTopologyRail(env.PresenceHistory)
		sender, friend := seedFriendship(t, env)
		ctx := context.Background()

		// The friend is a real, authorized viewer: what makes this sender
		// inactive is the master toggle alone. That is what makes the case a
		// drift detector — a bridge predicate that stopped consulting
		// master_enabled would resolve the friend into the Before set, and the
		// batch would reject the operation that audience belongs to.
		seedCustomTextSettings(t, env.DB, sender, false)

		before := captureTopologyBefore(ctx, t, env.DB, r, sender)
		require.Nil(t, before[sender],
			"nil is the only value prepareTopologyPlan accepts for an inactive operation")

		subject := presencecapture.Subject{
			Family:      presencecapture.FamilyBlock,
			FailPosture: presencecapture.FailConservativeDegrade,
			Principal:   sender,
			Counterpart: friend,
		}
		err := r.WithGatedTx(ctx, subject, func(tx *sql.Tx) error {
			plan, captureErr := r.CaptureInTx(ctx, tx, subject)
			if captureErr != nil {
				return captureErr
			}
			if _, execErr := tx.ExecContext(ctx, `
				UPDATE friendships SET status = 'blocked'
				WHERE (requester_id = $1 AND addressee_id = $2)
				   OR (requester_id = $2 AND addressee_id = $1)
			`, sender, friend); execErr != nil {
				return execErr
			}
			return r.Complete(ctx, tx, plan)
		})
		require.NoError(t, err, "an inactive operation with a nil audience still prepares")

		var blocked int
		require.NoError(t, env.DB.QueryRow(`
			SELECT COUNT(*) FROM friendships
			WHERE status = 'blocked'
			  AND ((requester_id = $1 AND addressee_id = $2)
			    OR (requester_id = $2 AND addressee_id = $1))
		`, sender, friend).Scan(&blocked))
		assert.Equal(t, 1, blocked, "and the block is durable")
	})

	// The other drift direction, and it does NOT surface as an error: a bridge
	// STRICTER than the batch yields an active operation with an empty audience,
	// which prepareTopologyPlan accepts. Nothing 500s — the sender's Custom
	// Status simply stops being cleared from viewers who just lost it. So this
	// case pins the predicates against each other directly. Whitespace-only text
	// is the discriminator: normalizeCustomTextState treats it as SET (it only
	// collapses the empty string), so the bridge must too.
	t.Run("whitespace-only text is active on both sides of the predicate", func(t *testing.T) {
		env := testhelpers.SetupTestServer(t)
		r, _ := newReconciler(t, env)
		r.SetTopologyRail(env.PresenceHistory)
		sender, friend := seedFriendship(t, env)
		ctx := context.Background()

		_, err := env.DB.Exec(`
			INSERT INTO user_presence_settings (user_id, master_enabled, custom_text_tier, custom_text)
			VALUES ($1, TRUE, 1, '   ')
		`, sender)
		require.NoError(t, err, "publish a whitespace-only custom status")

		before := captureTopologyBefore(ctx, t, env.DB, r, sender)
		require.True(t, before[sender][friend],
			"a TRIM on either side of the predicate splits the two derivations apart")

		subject := presencecapture.Subject{
			Family:      presencecapture.FamilyFriendshipRemove,
			Principal:   sender,
			Counterpart: friend,
		}
		require.NoError(t, r.WithGatedTx(ctx, subject, func(tx *sql.Tx) error {
			plan, captureErr := r.CaptureInTx(ctx, tx, subject)
			if captureErr != nil {
				return captureErr
			}
			if _, execErr := tx.ExecContext(ctx, `
				DELETE FROM friendships
				WHERE ((requester_id = $1 AND addressee_id = $2)
				    OR (requester_id = $2 AND addressee_id = $1))
				  AND status = 'accepted'
			`, sender, friend); execErr != nil {
				return execErr
			}
			return r.Complete(ctx, tx, plan)
		}), "both sides call the operation active, so the batch prepares and commits")
	})
}

// seedCustomTextSettings publishes a Friends-tier custom status for userID with
// the master toggle as given. The tier and the text stay fixed, so the toggle is
// the only thing separating the active case from the inactive one.
func seedCustomTextSettings(t *testing.T, db *sql.DB, userID uuid.UUID, masterEnabled bool) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (user_id, master_enabled, custom_text_tier, custom_text)
		VALUES ($1, $2, 1, 'in a meeting')
		ON CONFLICT (user_id) DO UPDATE
		SET master_enabled = $2, custom_text_tier = 1, custom_text = 'in a meeting'
	`, userID, masterEnabled)
	require.NoError(t, err, "seed custom text settings")
}

// ─── Task 11: the durable Custom Status leg, end to end ─────────────────────

// newTopologyRail builds a PRIVATE presencehistory.Service over the test
// database.
//
// It does not use env.PresenceHistory, and it cannot: internal/api/router.go
// already calls BindDelivery(hub) on the service testhelpers hands to
// NewRouter, and BindDelivery refuses a second bind ("presence history delivery
// already bound"). Every durable effect asserted below is a row in env.DB, so
// only the process-local sender gates and the delivery adapter are private to
// the test — and no test here runs a concurrent settings write that would need
// to share a gate.
func newTopologyRail(t *testing.T, env *testhelpers.TestServer) *presencehistory.Service {
	t.Helper()
	return presencehistory.NewService(
		env.DB,
		presencehistory.BuildDisclosure(presencehistory.DisclosureOptions{InstanceType: "saas"}),
		true,
	)
}

// recordingDeliverer stands in for *websocket.Hub on the topology rail. It
// acknowledges with the plan's own operation ID, which is what
// acknowledgeTopologyPlan requires, and records every plan so a test can assert
// on the exact recipients rather than merely that delivery happened.
//
// It is mutex-guarded: delivery runs on the completing goroutine while the
// assertion reads from the test goroutine.
type recordingDeliverer struct {
	mu    sync.Mutex
	plans []presencehistory.DeliveryPlan
	fail  error
}

func (d *recordingDeliverer) DeliverCustomText(
	_ context.Context, plan presencehistory.DeliveryPlan,
) (presencehistory.DeliveryAck, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.plans = append(d.plans, plan)
	if d.fail != nil {
		return presencehistory.DeliveryAck{}, d.fail
	}
	return presencehistory.DeliveryAck{OperationID: plan.OperationID}, nil
}

// setFailure swaps the delivery outcome under the same mutex DeliverCustomText
// takes. Assigning the field directly races the rail's own goroutines, which is
// what -race reports on the tests that fail delivery and then repair it.
func (d *recordingDeliverer) setFailure(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fail = err
}

func (d *recordingDeliverer) recorded() []presencehistory.DeliveryPlan {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]presencehistory.DeliveryPlan(nil), d.plans...)
}

// railedReconciler builds a reconciler with the durable leg wired to a private
// rail bound to deliverer, and returns both.
func railedReconciler(
	t *testing.T, env *testhelpers.TestServer, deliverer *recordingDeliverer,
) (*graphpresence.Reconciler, *presencehistory.Service) {
	t.Helper()
	rail := newTopologyRail(t, env)
	require.NoError(t, rail.BindDelivery(deliverer), "bind the private rail's delivery")
	r, _ := newReconciler(t, env)
	r.SetTopologyRail(rail)
	// require, not assert: every marker assertion below passes VACUOUSLY on an
	// unwired rail, because captureTopologyMarkers returns before
	// BeginTopologyBatch when r.rail is nil.
	require.True(t, r.HasTopologyRail(), "precondition: the durable leg must be wired")
	return r, rail
}

// pendingOperationCount reports how many durable topology markers exist for a
// sender. Zero means none is outstanding; one means a write is.
func pendingOperationCount(t *testing.T, db *sql.DB, senderID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`,
		senderID,
	).Scan(&count))
	return count
}

// settingsOperationID reads the sender's settings-row marker, which
// beginTopologyOperation stamps and the acknowledgement clears.
func settingsOperationID(t *testing.T, db *sql.DB, senderID uuid.UUID) uuid.NullUUID {
	t.Helper()
	var marker uuid.NullUUID
	require.NoError(t, db.QueryRow(
		`SELECT presence_settings_operation_id FROM user_presence_settings WHERE user_id = $1`,
		senderID,
	).Scan(&marker))
	return marker
}

// expireMarkerGrace pulls the marker's clock into the past so discoverPending
// selects it on the next ReconcilePending call. The alternative is sleeping out
// presencehistory.pendingOperationGrace, which is 30 seconds.
//
// created_at moves with reconcile_after because migration 000087 constrains
// them: presence_settings_pending_operations_reconcile_check is
// `reconcile_after > created_at`, so backdating only the second column raises
// 23514 rather than expiring the grace.
func expireMarkerGrace(t *testing.T, db *sql.DB, senderID uuid.UUID) {
	t.Helper()
	result, err := db.Exec(`
		UPDATE presence_settings_pending_operations
		SET created_at = clock_timestamp() - INTERVAL '2 minutes',
		    reconcile_after = clock_timestamp() - INTERVAL '1 minute'
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err, "expire marker grace")
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, affected,
		"there must be a marker to expire, or the reconciliation below is vacuous")
}

// customTextSnapshotJoinable runs a COPY of the question
// websocket.Hub.readCustomTextSnapshotCandidate asks on reconnect
// (internal/websocket/customtext.go): a sender holding ANY pending row is cut
// by the NOT EXISTS clause in the settings read, before the tier and override
// checks run at all.
//
// A COPY is the whole caveat, and the name used to hide it. As
// customTextSnapshotAuthorized it read like proof that a reconnecting viewer is
// denied; it is not. Deleting the NOT EXISTS clause from the production reader
// leaves every assertion here GREEN -- measured, not supposed -- because this
// query still carries its own copy. What the production reader does is owned by
// TestSendCustomTextSnapshot_FinalStateQuerySuppressesNewPendingSender in
// internal/websocket, which drives it directly and does go red.
//
// Kept because the join it exercises is still worth pinning: the marker row and
// the settings row must remain joinable in this shape, and that is a schema
// fact this suite can own. Renamed so the next reader is not misled by it.
func customTextSnapshotJoinable(t *testing.T, db *sql.DB, senderID uuid.UUID) bool {
	t.Helper()
	var visible bool
	require.NoError(t, db.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM user_presence_settings AS settings
			WHERE settings.user_id = $1
			  AND settings.master_enabled
			  AND settings.custom_text_tier IN (1, 2)
			  AND COALESCE(settings.custom_text, '') <> ''
			  AND NOT EXISTS (
			      SELECT 1
			      FROM presence_settings_pending_operations AS pending
			      WHERE pending.user_id = settings.user_id
			  )
		)
	`, senderID).Scan(&visible))
	return visible
}

// addCustomTextException materializes a #1234 recipient exception, preference
// document first, in the order the production write path uses.
func addCustomTextException(t *testing.T, db *sql.DB, senderID, targetID uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO presence_override_preferences (user_id, category, encrypted_data)
		VALUES ($1, 'custom_text', 'opaque-client-blob')
		ON CONFLICT DO NOTHING
	`, senderID)
	require.NoError(t, err, "seed override preference document")
	_, err = db.Exec(`
		INSERT INTO user_presence_overrides (sender_id, category, target_user_id)
		VALUES ($1, 'custom_text', $2)
	`, senderID, targetID)
	require.NoError(t, err, "seed recipient exception")
}

// addFriend creates a new user with an accepted edge to an existing one.
// seedFriendship above only ever produces a PAIR; the recipient-exception case
// needs a second friend of the same sender.
func addFriend(t *testing.T, db *sql.DB, existing uuid.UUID) uuid.UUID {
	t.Helper()
	friend := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, existing, friend)
	return friend
}

// createPendingRequester creates a user holding a PENDING request to sender.
func createPendingRequester(t *testing.T, db *sql.DB, sender uuid.UUID) uuid.UUID {
	t.Helper()
	requester := testhelpers.CreateUser(t, db)
	_, err := db.Exec(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'pending')`,
		requester, sender,
	)
	require.NoError(t, err, "seed pending request")
	return requester
}

// removeFriendThroughTheBridge drives the exact WithGatedTx -> CaptureInTx ->
// write -> Complete sequence internal/friends' RemoveFriend uses, with
// RemoveFriend's own predicate, and RETURNS the terminal error rather than
// asserting on it so a caller can assert either direction.
func removeFriendThroughTheBridge(
	ctx context.Context, t *testing.T, r *graphpresence.Reconciler, sender, friend uuid.UUID,
) error {
	t.Helper()
	subject := presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipRemove,
		Principal:   sender,
		Counterpart: friend,
	}
	return r.WithGatedTx(ctx, subject, func(tx *sql.Tx) error {
		plan, captureErr := r.CaptureInTx(ctx, tx, subject)
		if captureErr != nil {
			return captureErr
		}
		if _, execErr := tx.ExecContext(ctx, `
			DELETE FROM friendships
			WHERE ((requester_id = $1 AND addressee_id = $2)
			    OR (requester_id = $2 AND addressee_id = $1))
			  AND status = 'accepted'
		`, sender, friend); execErr != nil {
			return execErr
		}
		return r.Complete(ctx, tx, plan)
	})
}

// acceptFriendThroughTheBridge drives the additive family the same way, over a
// pending request rather than an accepted edge.
func acceptFriendThroughTheBridge(
	ctx context.Context, t *testing.T, r *graphpresence.Reconciler, sender, requester uuid.UUID,
) error {
	t.Helper()
	subject := presencecapture.Subject{
		Family:      presencecapture.FamilyFriendshipAccept,
		Principal:   sender,
		Counterpart: requester,
	}
	return r.WithGatedTx(ctx, subject, func(tx *sql.Tx) error {
		plan, captureErr := r.CaptureInTx(ctx, tx, subject)
		if captureErr != nil {
			return captureErr
		}
		if _, execErr := tx.ExecContext(ctx, `
			UPDATE friendships SET status = 'accepted'
			WHERE requester_id = $1 AND addressee_id = $2 AND status = 'pending'
		`, requester, sender); execErr != nil {
			return execErr
		}
		return r.Complete(ctx, tx, plan)
	})
}

// anyPlanUpdates reports whether any recorded plan named recipientID as an
// UPDATE recipient.
func anyPlanUpdates(plans []presencehistory.DeliveryPlan, recipientID uuid.UUID) bool {
	for _, plan := range plans {
		if plan.UpdateRecipients[recipientID] {
			return true
		}
	}
	return false
}

// planFor returns the recorded delivery plan for one sender. A #2446 batch
// carries ONE operation per focal sender — two for every friendship family —
// so a test that asserts on delivered[0] is asserting on whichever of the two
// canonicalTopologySenders happened to sort first, which is a coin flip on
// generated UUIDs.
func planFor(
	t *testing.T, plans []presencehistory.DeliveryPlan, senderID uuid.UUID,
) presencehistory.DeliveryPlan {
	t.Helper()
	for _, plan := range plans {
		if plan.SenderID == senderID {
			return plan
		}
	}
	require.FailNow(t, "no delivery plan was recorded for the sender")
	return presencehistory.DeliveryPlan{}
}

// guardedDisconnector counts rich-presence teardowns under a mutex. The
// existing countingDisconnectorForTest is not mutex-guarded, and dispatch runs
// on the sink worker goroutine, so reading its counter from the test goroutine
// is a data race the moment the count is non-zero — which turns the very
// failure the assertion exists to catch into a -race report instead of a test
// failure.
type guardedDisconnector struct {
	mu    sync.Mutex
	calls int
}

func (d *guardedDisconnector) DisconnectRichPresenceClients(
	context.Context, map[uuid.UUID]bool,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return nil
}

func (d *guardedDisconnector) DisconnectAllRichPresenceClients(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return nil
}

// BeginAudienceRevocation satisfies Disconnector. This fake exists to observe
// DISCONNECTS, and the #2992 bracket is orthogonal to every assertion it
// carries, so the closer is inert. The fence's own behaviour is covered by
// topology_test.go's fenceStub, which records into the ordering trace.
func (d *guardedDisconnector) BeginAudienceRevocation() func() { return func() {} }

func (d *guardedDisconnector) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// AC-4 — invariant TB-1: a C1 degrade leaves the topology markers INTACT, and
// the C2 batch that rides beside them is fully valid and exactly delivered.
//
// The degrade is real, not simulated: undeterminedPresence makes
// CaptureServerVoiceCandidatesStrict fail for a sender who holds a live voice
// row, which is the ONE stage CaptureInTx allows to degrade (step 8). The gate
// and the marker write in steps 3-5 have already run by then, and the capture
// savepoint is opened AFTER them, so ROLLBACK TO SAVEPOINT cannot reach the
// markers.
//
// Drop carryTopology's body and this goes red twice over: Complete takes the
// bare-commit path instead of completeTopology, so nothing is ever delivered
// AND both markers sit until their grace expires, suppressing the sender's
// Custom Status for every reconnecting viewer in the meantime.
func TestC1DegradeLeavesTheTopologyMarkersIntact(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	deliverer := &recordingDeliverer{}
	rail := newTopologyRail(t, env)
	require.NoError(t, rail.BindDelivery(deliverer))

	sender, viewer := seedLiveVoiceSender(t, env)
	seedCustomTextSettings(t, env.DB, sender, true)

	r := graphpresence.New(env.DB, &testRefresher{}, env.Hub, undeterminedPresence{}, nil)
	t.Cleanup(r.Close)
	r.SetTopologyRail(rail)
	require.True(t, r.HasTopologyRail(), "precondition: the durable leg must be wired")

	subject := presencecapture.Subject{
		Family:      presencecapture.FamilyBlock,
		FailPosture: presencecapture.FailConservativeDegrade,
		Principal:   sender,
		Counterpart: viewer,
	}
	err := r.WithGatedTx(ctx, subject, func(tx *sql.Tx) error {
		plan, captureErr := r.CaptureInTx(ctx, tx, subject)
		require.NoError(t, captureErr, "the degrade posture absorbs the C1 read failure")
		require.True(t, plan.Degraded(),
			"the C1 leg must really have degraded, or this test asserts nothing about TB-1")

		// The marker is durable INSIDE the transaction, after the degrade's
		// ROLLBACK TO SAVEPOINT has already run.
		var marker uuid.NullUUID
		require.NoError(t, tx.QueryRow(`
			SELECT presence_settings_operation_id FROM user_presence_settings WHERE user_id = $1
		`, sender).Scan(&marker))
		require.True(t, marker.Valid, "TB-1: the topology marker must survive a C1 degrade")

		if _, execErr := tx.ExecContext(ctx, `
			UPDATE friendships SET status = 'blocked'
			WHERE (requester_id = $1 AND addressee_id = $2)
			   OR (requester_id = $2 AND addressee_id = $1)
		`, sender, viewer); execErr != nil {
			return execErr
		}
		return r.Complete(ctx, tx, plan)
	})
	require.NoError(t, err, "a degraded C1 leg must not block the C2 batch or the write")

	// A degraded C1 leg AND a fully valid C2 batch in one Plan: the batch was
	// prepared in DeliveryExactDelta mode and delivered exactly.
	delivered := deliverer.recorded()
	senderPlan := planFor(t, delivered, sender)
	assert.Equal(t, presencehistory.DeliveryExactDelta, senderPlan.Mode,
		"TB-1 forbids conditioning the batch on FailPosture, so it stays exact")
	assert.Contains(t, senderPlan.ClearRecipients, viewer,
		"the blocked viewer loses the accepted edge, so their Custom Status is cleared")

	// Acknowledged delivery deletes the PENDING row, so nothing is left
	// suppressing the sender on reconnect. It deliberately does NOT clear
	// user_presence_settings.presence_settings_operation_id: that column is the
	// same-version evidence a later operation classifies against, and the
	// reconnect predicate reads only presence_settings_pending_operations.
	assert.Zero(t, pendingOperationCount(t, env.DB, sender))
	assert.Zero(t, pendingOperationCount(t, env.DB, viewer))
	assert.True(t, customTextSnapshotJoinable(t, env.DB, sender),
		"the marker's lifetime ended with the ack, so the sender is visible again")
}

// AC-5 — the accepted-edge gate writes NO marker.
//
// This is the durable half of the #2738 fix. Without the gate, naming a
// stranger writes a row that suppresses that stranger's Custom Status for every
// reconnecting viewer for the whole grace window, from an attacker-supplied
// path parameter. Delete capture.go's `if !destroys` early return and both
// assertions go red.
func TestNoMarkerWhenTheAcceptedEdgeGateSaysNo(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	r, _ := railedReconciler(t, env, &recordingDeliverer{})

	blocker := testhelpers.CreateUser(t, env.DB)
	stranger := testhelpers.CreateUser(t, env.DB) // NO friendship edge, ever
	// An ACTIVE Custom Status on the stranger, or the suppression the marker
	// would cause has nothing to suppress and the assertion is vacuous.
	seedCustomTextSettings(t, env.DB, stranger, true)
	require.True(t, customTextSnapshotJoinable(t, env.DB, stranger),
		"precondition: the stranger's Custom Status must be visible before the block")

	subject := presencecapture.Subject{
		Family:      presencecapture.FamilyBlock,
		FailPosture: presencecapture.FailConservativeDegrade,
		Principal:   blocker,
		Counterpart: stranger,
	}
	require.NoError(t, r.WithGatedTx(ctx, subject, func(tx *sql.Tx) error {
		plan, captureErr := r.CaptureInTx(ctx, tx, subject)
		require.NoError(t, captureErr)
		assert.False(t, plan.HasWork(), "proven-no-change must not reconcile")
		return r.Complete(ctx, tx, plan)
	}))

	assert.Zero(t, pendingOperationCount(t, env.DB, stranger),
		"blocking a stranger must not write a durable marker naming them")
	assert.Zero(t, pendingOperationCount(t, env.DB, blocker))
	assert.True(t, customTextSnapshotJoinable(t, env.DB, stranger),
		"and the stranger's Custom Status must stay visible on reconnect")
}

// AC-6 — the #1234 recipient exceptions are the FINAL filter, on the delivered
// audience as well as the resolved one.
//
// The control read before the exception is what makes this non-vacuous: it
// proves the excepted friend WOULD otherwise be in the audience, so their later
// absence is the exception doing work rather than an empty graph. Drop the
// overrides cut from presence.ComputeCustomTextAudienceForTier and the second
// captureTopologyBefore plus the delivered-plan assertion all go red.
func TestRecipientExceptionsFilterTheDeliveredAudience(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	deliverer := &recordingDeliverer{}
	r, _ := railedReconciler(t, env, deliverer)

	sender := testhelpers.CreateUser(t, env.DB)
	seedCustomTextSettings(t, env.DB, sender, true)
	excepted := addFriend(t, env.DB, sender)
	ordinary := addFriend(t, env.DB, sender)

	// CONTROL. Both friends are inside the Friends-tier audience before any
	// exception exists.
	control := captureTopologyBefore(ctx, t, env.DB, r, sender)
	require.True(t, control[sender][excepted],
		"precondition: the friend about to be excepted must start inside the audience")
	require.True(t, control[sender][ordinary])

	addCustomTextException(t, env.DB, sender, excepted)

	filtered := captureTopologyBefore(ctx, t, env.DB, r, sender)
	require.False(t, filtered[sender][excepted],
		"#1234: the exception is the final cut on the pre-mutation audience")
	require.True(t, filtered[sender][ordinary])

	require.NoError(t, removeFriendThroughTheBridge(ctx, t, r, sender, ordinary))

	senderPlan := planFor(t, deliverer.recorded(), sender)
	assert.Contains(t, senderPlan.ClearRecipients, ordinary,
		"the removed friend loses authorization, so their copy is cleared")
	assert.NotContains(t, senderPlan.ClearRecipients, excepted,
		"an excepted recipient never held the status, so clearing them would "+
			"disclose that a status exists at all")
	assert.NotContains(t, senderPlan.UpdateRecipients, excepted,
		"and they must never be sent one either")
}

// AC-8 — an additive family produces zero clears, a non-empty update, and NO
// peripheral disconnect.
//
// The last part is what plan.viewers staying nil buys: seeding it for an accept
// tears down every device of BOTH users on every friend acceptance, for a
// mutation that revokes nothing. Delete capture.go's `if policy.
// CanRevokeVisibility` guard around the viewer seed and the disconnect
// assertion goes red; make prepareTopologyPlan emit before-set clears for an
// additive delta and the clears assertion goes red.
func TestAdditiveFamilyProducesUpdatesAndNoClears(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	deliverer := &recordingDeliverer{}
	rail := newTopologyRail(t, env)
	require.NoError(t, rail.BindDelivery(deliverer))

	disconnector := &guardedDisconnector{}
	r := graphpresence.New(env.DB, &testRefresher{}, disconnector, alwaysPermitted{}, nil)
	t.Cleanup(r.Close)
	r.SetTopologyRail(rail)
	require.True(t, r.HasTopologyRail(), "precondition: the durable leg must be wired")

	sender := testhelpers.CreateUser(t, env.DB)
	seedCustomTextSettings(t, env.DB, sender, true)
	joiner := createPendingRequester(t, env.DB, sender)

	// NON-VACUITY: a pending row confers no Custom Status authorization, so the
	// joiner starts OUTSIDE the audience and the accept is what puts them in.
	before := captureTopologyBefore(ctx, t, env.DB, r, sender)
	require.False(t, before[sender][joiner],
		"precondition: a pending request must not already authorize the joiner")

	require.NoError(t, acceptFriendThroughTheBridge(ctx, t, r, sender, joiner))

	delivered := deliverer.recorded()
	require.NotEmpty(t, delivered, "the additive family still carries a topology batch")
	for _, plan := range delivered {
		assert.Empty(t, plan.ClearRecipients,
			"an accepted edge is purely additive: nobody loses authorization")
	}
	assert.True(t, anyPlanUpdates(delivered, joiner),
		"the widened audience must reach the new friend")

	// Polled, not sampled. Complete's enqueue hands the plan to the ASYNC
	// dispatch sink, so an immediate count() reads it at t=0 -- before the
	// worker could have run. That is not a stylistic point: with the
	// CanRevokeVisibility guard removed, an immediate assertion still passes.
	require.Never(t, func() bool { return disconnector.count() > 0 },
		2*time.Second, 10*time.Millisecond,
		"plan.viewers stays nil for an additive family -- seeding it would tear "+
			"down every device of BOTH users on every friend acceptance")
}

// strandMarker removes an accepted edge through the bridge with delivery
// failing, so the mutation COMMITS and its markers stay outstanding. It returns
// the sender and the friend it removed.
//
// require.ErrorIs on the terminal is load-bearing rather than tidy: if the
// removal ever stopped reaching the post-commit terminal, the marker
// assertions below would all read a zero count and PASS, reporting that no
// marker is outstanding when the truth is that none was ever written.
func strandMarker(
	ctx context.Context, t *testing.T, env *testhelpers.TestServer, r *graphpresence.Reconciler,
) (sender, friend uuid.UUID) {
	t.Helper()
	sender = testhelpers.CreateUser(t, env.DB)
	seedCustomTextSettings(t, env.DB, sender, true)
	friend = addFriend(t, env.DB, sender)
	require.True(t, customTextSnapshotJoinable(t, env.DB, sender),
		"precondition: the sender's Custom Status must be visible before the write")

	err := removeFriendThroughTheBridge(ctx, t, r, sender, friend)
	require.ErrorIs(t, err, presencecapture.ErrPostCommitDelivery,
		"the mutation must be DURABLE and only its delivery failed -- that is the "+
			"only state in which a marker is legitimately left outstanding")
	return sender, friend
}

// AC-7 — a marker's lifetime is BOUNDED, and while it is outstanding the
// sender's reconnect snapshot is suppressed.
//
// The suppression is why lifetime is an acceptance criterion rather than an
// implementation detail: readCustomTextSnapshotCandidate's NOT EXISTS cuts the
// settings row for a sender holding ANY pending row, before the tier and
// override checks run, so an unbounded marker would silently hide that user's
// Custom Status from every reconnecting viewer forever.
//
// SCOPE, and it is narrower than an earlier version of this comment claimed.
// This test owns the LIFETIME half only: that a stranded marker is discovered
// once its grace elapses and that resolving it removes the row. Drop
// reconcileTopologyMarker's completeTopologyPlan call and that half goes red.
//
// It does NOT own the suppression half. The earlier comment said dropping the
// NOT EXISTS clause from internal/websocket/customtext.go would turn this red.
// That was measured and is FALSE: customTextSnapshotJoinable (above) hand-writes
// its own copy of the production predicate, so the assertion proved only that
// Postgres evaluates the TEST's SQL. Production could lose the clause entirely
// -- the exact disclosure this criterion exists to prevent -- and this stayed
// green. The suppression claim is owned by
// TestSendCustomTextSnapshot_FinalStateQuerySuppressesNewPendingSender in
// internal/websocket, which drives the real reader and does go red under that
// mutation. The probe is kept below as a schema check, relabelled, and no
// longer carries an assertion it cannot support.
func TestOutstandingMarkerSuppressesTheReconnectSnapshot(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	deliverer := &recordingDeliverer{fail: errors.New("hub unreachable")}
	r, rail := railedReconciler(t, env, deliverer)

	sender, _ := strandMarker(ctx, t, env, r)
	require.Equal(t, 1, pendingOperationCount(t, env.DB, sender),
		"a failed post-commit delivery must leave the marker for reconciliation")

	// Schema probe, NOT a proof of the production call site -- see the scope note
	// above. It confirms the marker row and the settings row are joinable in the
	// shape the production predicate relies on; it cannot tell you production
	// still applies that predicate.
	assert.False(t, customTextSnapshotJoinable(t, env.DB, sender),
		"while the marker stands, the suppression join must select nothing")

	// The lifetime is bounded by presencehistory.pendingOperationGrace: once it
	// elapses the marker becomes ELIGIBLE and reconciliation owns it.
	expireMarkerGrace(t, env.DB, sender)
	deliverer.setFailure(nil)

	stats, err := rail.ReconcilePending(ctx, 10)
	require.NoError(t, err)
	assert.Positive(t, stats.DiscoveredCount, "the expired marker must be discovered")
	assert.Zero(t, pendingOperationCount(t, env.DB, sender),
		"and resolving it must remove it -- an unbounded marker is an unbounded outage")
	assert.True(t, customTextSnapshotJoinable(t, env.DB, sender),
		"and once it is resolved the same join selects the row again")
}

// AC-12 — the erasure cascade destroys the marker.
//
// presence_settings_pending_operations.user_id is ON DELETE CASCADE (migration
// 000087), so the rail is STRUCTURALLY incapable of serving an erased
// principal: an account deletion cannot leave a durable row naming that user
// behind for reconciliation to pick up and deliver against.
//
// Change that foreign key to ON DELETE NO ACTION and the delete itself fails,
// which is the same red.
func TestErasureCascadeDestroysTheMarker(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	deliverer := &recordingDeliverer{fail: errors.New("hub unreachable")}
	r, _ := railedReconciler(t, env, deliverer)

	sender, _ := strandMarker(ctx, t, env, r)
	require.Equal(t, 1, pendingOperationCount(t, env.DB, sender))

	_, err := env.DB.Exec(`DELETE FROM users WHERE id = $1`, sender)
	require.NoError(t, err, "erasing the principal must not be blocked by its own marker")
	assert.Zero(t, pendingOperationCount(t, env.DB, sender),
		"the marker must go with the account, not outlive it")
}

// AC-13 — mixed-version convergence. A marker written by a NEW replica is
// resolved with no graphpresence producer wired at all: the resolver is
// presencehistory's own ticker path, which an OLD replica runs unchanged.
//
// Two independent facts are asserted, because either alone is satisfiable by a
// wrong implementation:
//
//   - The resolution is a CONSERVATIVE RESET, not an exact delta. An old
//     replica holds no captured audience, so an exact delta it could compute
//     would be computed from the POST-mutation graph and would clear nobody.
//   - It resolves THAT operation rather than minting a new one. A
//     reconciliation that superseded the marker with a fresh operation would
//     also empty the pending table, so the count alone cannot tell the two
//     apart; the settings marker is the evidence that distinguishes them.
func TestMarkerConvergesWithoutTheGraphPresenceProducer(t *testing.T) {
	env := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	deliverer := &recordingDeliverer{fail: errors.New("hub unreachable")}
	r, rail := railedReconciler(t, env, deliverer)

	sender, _ := strandMarker(ctx, t, env, r)
	r.Close() // the producer is gone, as on a replica that never wired one

	require.Equal(t, 1, pendingOperationCount(t, env.DB, sender))
	stamped := settingsOperationID(t, env.DB, sender)
	require.True(t, stamped.Valid, "BeginTopologyBatch must have stamped the settings row")

	expireMarkerGrace(t, env.DB, sender)
	deliverer.setFailure(nil)
	_, err := rail.ReconcilePending(ctx, 10)
	require.NoError(t, err)

	delivered := deliverer.recorded()
	require.NotEmpty(t, delivered)
	assert.Equal(t, presencehistory.DeliveryConservativeReset, delivered[len(delivered)-1].Mode,
		"a replica with no captured audience can only converge conservatively")
	assert.Zero(t, pendingOperationCount(t, env.DB, sender))
	assert.Equal(t, stamped, settingsOperationID(t, env.DB, sender),
		"reconciliation must RESOLVE the marker it found, never supersede it with a "+
			"fresh operation -- superseding is what ForcedSecurityClear is for")
}
