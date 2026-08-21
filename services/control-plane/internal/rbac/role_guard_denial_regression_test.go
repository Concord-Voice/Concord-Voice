package rbac_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// INVERTED EXPLOIT PROOFS — #2721 red-team pass (AC15).
//
// A red-team pass aimed at the FIX rather than at the original TOCTOU found four
// primitives that moving the guard inside the write transaction had introduced.
// All four are closed by 8aea1db67. The tests below are those proofs-of-concept
// INVERTED: each asserts the DEFENCE holds. Precedent and house style:
// internal/friends/presence_disconnect_regression_test.go — "inverted exploit
// proofs: if they fail, the class is back".
//
// The deliberately-omitted fifth PoC is the concurrent-ReorderRoles deadlock
// probe. It reproduces identically on the pre-#2721 tree with none of this work
// present: it is a pre-existing lock-order cycle in applyRolePositions, which
// locks N roles rows in client-supplied order under no advisory lock. That
// belongs to #2851; a flaky 40P01 test here would be misattributed noise.
//
// Barrier primitives, the probe pool and the path helpers are reused from
// role_guard_integration_test.go and testhelpers/testdb rather than re-cut.
// ─────────────────────────────────────────────────────────────────────────────

const (
	// denialActorCeiling is the position of the role granting the actor
	// PermManageRoles / PermManageRolesAssign, hence their hierarchy ceiling.
	denialActorCeiling = 1
	// denialTargetAbove sits above that ceiling, so EVERY mutation the actor
	// aims at it is a certain errHierarchyDenied. That certainty is the point:
	// the server has already decided on the 403 before any expensive work could
	// possibly be worth doing.
	denialTargetAbove = 9
	// denialTargetBelow is the legitimate control the OWNER edits successfully.
	denialTargetBelow = 0

	// presenceCaptureFanOutBound mirrors voicepresence's unexported
	// presenceCaptureMaxChannels (executor.go: `len(channelIDs) > 64`). Crossing
	// it makes PrepareCapture fail closed with ErrPresenceCaptureLimited.
	presenceCaptureFanOutBound = 64

	// denialPollBudget bounds the "no advisory waiter ever appeared" poll. It is
	// an iteration budget, never a duration: every iteration is a real pg_locks
	// round-trip, so the loop is self-throttling and carries no timing.
	denialPollBudget = 60000
)

type denialFixture struct {
	ts       *testhelpers.TestServer
	probe    *sql.DB
	serverID string
	owner    testhelpers.TestUser
	actor    testhelpers.TestUser
	// aboveRole sits above the actor's ceiling: every mutation aimed at it is a
	// guaranteed hierarchy 403.
	aboveRole string
	// belowRole is the unrelated role the owner legitimately edits.
	belowRole string
	advKey    int64
}

func newDenialFixture(t *testing.T) *denialFixture {
	t.Helper()
	return newDenialServerOn(t, testhelpers.SetupTestServer(t), nil)
}

// newDenialServerOn builds an independent server (its own owner, its own actor,
// its own advisory key) on an existing TestServer, so a test needing two servers
// does not pay for two routers. Pass a nil probe to open one.
func newDenialServerOn(t *testing.T, ts *testhelpers.TestServer, probe *sql.DB) *denialFixture {
	t.Helper()
	if probe == nil {
		probe = openLockProbePool(t)
	}
	owner := ts.CreateTestUser(t, "dnown"+uuid.New().String()[:6])
	actor := ts.CreateTestUser(t, "dnact"+uuid.New().String()[:6])
	serverID := ts.CreateTestServer(t, owner.ID, "Denial Regression Server")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")

	grantPermToUser(t, ts, serverID, actor.ID, denialActorCeiling,
		int64(rbac.PermManageRoles|rbac.PermManageRolesAssign))

	key, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)

	return &denialFixture{
		ts:        ts,
		probe:     probe,
		serverID:  serverID,
		owner:     owner,
		actor:     actor,
		aboveRole: ts.CreateTestRole(t, serverID, "dnabove"+uuid.New().String()[:8], denialTargetAbove, 0),
		belowRole: ts.CreateTestRole(t, serverID, "dnbelow"+uuid.New().String()[:8], denialTargetBelow, 0),
		advKey:    key,
	}
}

func (f *denialFixture) actorRequest(method, path string, body interface{}) *httptest.ResponseRecorder {
	return f.ts.DoRequest(method, path, body, testhelpers.AuthHeaders(f.actor.AccessToken))
}

// seedOccupiedVoiceChannels gives the server n voice channels each holding a
// participant, which is what PrepareCapture counts against its fan-out bound.
func (f *denialFixture) seedOccupiedVoiceChannels(t *testing.T, n int) {
	t.Helper()
	occupant := f.ts.CreateTestUser(t, "dnvoc"+uuid.New().String()[:6])
	f.ts.AddMemberToServer(t, f.serverID, occupant.ID, "member")
	for i := 0; i < n; i++ {
		channelID := f.ts.CreateVoiceChannel(t, f.serverID, "dnvc"+uuid.New().String()[:12])
		_, err := f.ts.DB.Exec(
			`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`,
			channelID, occupant.ID)
		require.NoError(t, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// R1 — a hierarchy-denied request must NOT acquire the per-server advisory lock.
//
// If this fails the class is back: an actor authorized for NOTHING on the target
// role can force the server-wide role-mutation lock to be taken and can be
// parked on it for an unbounded time. `SET LOCAL lock_timeout = '3s'` does not
// bound that wait — it is the first statement INSIDE the write closure, i.e.
// strictly after the advisory acquisition.
// ─────────────────────────────────────────────────────────────────────────────

func TestRoleMutationDenial_NeverWaitsOnTheServerAdvisoryLock(t *testing.T) {
	f := newDenialFixture(t)

	// Held for the whole test. A request that needed the advisory lock could not
	// possibly complete before the release in t.Cleanup.
	holdAdvisoryBarrier(t, f.probe, f.advKey)

	cases := []struct {
		name     string
		method   string
		path     string
		body     interface{}
		wantBody string
	}{
		{"UpdateRole", "PATCH", rolePath(f.serverID, f.aboveRole),
			map[string]interface{}{"name": "denied"}, "above your own position"},
		{"DeleteRole", "DELETE", rolePath(f.serverID, f.aboveRole),
			nil, "above your own position"},
		{"AssignRole", "POST", assignRolePath(f.serverID, f.actor.ID),
			map[string]interface{}{"role_id": f.aboveRole}, "equal or higher position"},
		{"UnassignRole", "DELETE", unassignRolePath(f.serverID, f.actor.ID, f.aboveRole),
			nil, "equal or higher position"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.denyWithoutAdvisoryWait(t, tc.method, tc.path, tc.body)
			require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.wantBody)
		})
	}
}

// denyWithoutAdvisoryWait fires a guaranteed-403 request while the advisory lock
// is held elsewhere and fails the instant the handler shows up in pg_locks as a
// waiter on that key. Completion while the lock is held IS the proof that the
// denial was decided before the transaction opened.
func (f *denialFixture) denyWithoutAdvisoryWait(
	t *testing.T, method, path string, body interface{},
) *httptest.ResponseRecorder {
	t.Helper()
	var rec *httptest.ResponseRecorder
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		rec = f.actorRequest(method, path, body)
	}()

	for attempt := 0; attempt < denialPollBudget; attempt++ {
		select {
		case <-finished:
			return rec
		default:
		}
		require.False(t, dbtest.AdvisoryLockWaiterExists(t, f.probe, f.advKey),
			"R1 REGRESSION — the class is back: a request that was always going to be a "+
				"hierarchy 403 is parked on the per-server role-mutation advisory lock. The "+
				"cheap pre-transaction denial no longer runs ahead of BeginTx.")
	}
	t.Fatal("R1: the denied request never completed while the advisory lock was held")
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// R2 — a denied request must NOT stall an unrelated legitimate mutation.
//
// If this fails the class is back: one guaranteed-403 request from an actor who
// may not touch the target role converts a single concurrent roles-row writer
// into a ~3s server-wide outage of every role mutation, the owner's included,
// because the denial waits on the row lock WHILE holding the advisory lock.
//
// Asserted on outcome, never on wall-clock: the denial is a 403 (pre-fix it was
// the 500 guard_lock_timeout) and no guard_lock_timeout is logged at all.
// ─────────────────────────────────────────────────────────────────────────────

func TestRoleMutationDenial_DoesNotStallTheOwnersLegitimateMutation(t *testing.T) {
	f := newDenialFixture(t)
	logs := f.ts.CaptureLogs(t)

	// A conflicting FOR NO KEY UPDATE on the denied request's target — exactly
	// the lock applyRolePositions takes, and it takes no advisory lock, so it
	// can legitimately overlap a guarded mutation.
	holdRoleRowBarrier(t, f.probe, f.aboveRole)

	deniedDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		deniedDone <- f.actorRequest("PATCH", rolePath(f.serverID, f.aboveRole),
			map[string]interface{}{"name": "denied"})
	}()
	ownerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		ownerDone <- f.ts.DoRequest("PATCH", rolePath(f.serverID, f.belowRole),
			map[string]interface{}{"name": "legit"}, testhelpers.AuthHeaders(f.owner.AccessToken))
	}()

	deniedRec := <-deniedDone
	ownerRec := <-ownerDone
	// The barrier is still outstanding here — it is released only by t.Cleanup.
	// Both requests therefore completed while a conflicting roles-row writer held
	// the target row.

	require.Equal(t, http.StatusForbidden, deniedRec.Code,
		"R2 REGRESSION — the class is back: the denial entered the guard's row-lock wait "+
			"instead of being refused before the transaction; body: %s", deniedRec.Body.String())
	assert.Contains(t, deniedRec.Body.String(), "above your own position")

	require.Equal(t, http.StatusOK, ownerRec.Code,
		"the owner's unrelated role edit must be unaffected; body: %s", ownerRec.Body.String())
	assert.NotContains(t, logs.String(), "guard_lock_timeout",
		"R2 REGRESSION — an unauthorized actor's request reached the guard lock timeout, "+
			"which means it held the advisory lock for its full 3s budget")
}

// ─────────────────────────────────────────────────────────────────────────────
// R3 — the denial status must NOT vary with voice occupancy.
//
// The most important of the four. PrepareCapture runs before the transaction and
// fails closed above presenceCaptureMaxChannels, so with the guard inside the
// closure the SAME unauthorized request returned 403 below the bound and 500
// above it — a binary oracle on aggregate active-voice-channel count, readable
// by an actor authorized for none of it. authority_tx.go names that exact signal
// as the class it exists to contain.
//
// If this fails the class is back and the disclosure is live.
// ─────────────────────────────────────────────────────────────────────────────

func TestRoleMutationDenial_StatusDoesNotDiscloseVoiceOccupancy(t *testing.T) {
	quiet := newDenialFixture(t)
	quiet.seedOccupiedVoiceChannels(t, 1)

	// A second, independent server on the same router, over the fan-out bound.
	busy := newDenialServerOn(t, quiet.ts, quiet.probe)
	busy.seedOccupiedVoiceChannels(t, presenceCaptureFanOutBound+1)
	logs := quiet.ts.CaptureLogs(t)

	// SELF-VALIDATION, so "both are 403" can never be vacuously true. An
	// AUTHORIZED mutation still sees the bound: the owner's edit succeeds on the
	// quiet server and is refused on the busy one with the capture-limit failure
	// class. That pins PrepareCapture as live and the bound as genuinely crossed,
	// which is what makes the equal denial statuses below load-bearing — pre-fix
	// the denied request observed this same 500.
	quietOwner := quiet.ts.DoRequest("PATCH", rolePath(quiet.serverID, quiet.belowRole),
		map[string]interface{}{"name": "owner-low"}, testhelpers.AuthHeaders(quiet.owner.AccessToken))
	require.Equal(t, http.StatusOK, quietOwner.Code, "body: %s", quietOwner.Body.String())
	busyOwner := busy.ts.DoRequest("PATCH", rolePath(busy.serverID, busy.belowRole),
		map[string]interface{}{"name": "owner-high"}, testhelpers.AuthHeaders(busy.owner.AccessToken))
	require.Equal(t, http.StatusInternalServerError, busyOwner.Code,
		"the fan-out bound must actually be crossed for this test to mean anything; body: %s",
		busyOwner.Body.String())
	require.Contains(t, logs.String(), "capture_channel_limit",
		"and it must be the capture bound that refused it, not some other failure")

	quietRec := quiet.actorRequest("PATCH", rolePath(quiet.serverID, quiet.aboveRole),
		map[string]interface{}{"name": "probe-low"})
	busyRec := busy.actorRequest("PATCH", rolePath(busy.serverID, busy.aboveRole),
		map[string]interface{}{"name": "probe-high"})

	require.Equal(t, http.StatusForbidden, quietRec.Code,
		"control: below the bound the denial is a clean 403; body: %s", quietRec.Body.String())
	require.Equal(t, http.StatusForbidden, busyRec.Code,
		"R3 REGRESSION — the class is back: the identical denial changed status once the "+
			"server crossed %d occupied voice channels, disclosing aggregate voice occupancy "+
			"to an actor authorized for none of it; body: %s",
		presenceCaptureFanOutBound, busyRec.Body.String())
	assert.Equal(t, quietRec.Body.String(), busyRec.Body.String(),
		"and the bodies must be byte-identical, not merely the same status")
}

// ─────────────────────────────────────────────────────────────────────────────
// R4 — is_managed / is_default are enforced by the IN-TRANSACTION guard, not
// only by the cheap pre-check.
//
// The guard always scanned both flags into roleGuardResult and every call site
// discarded the result, leaving the POSITION verdict re-derived under FOR SHARE
// while the FLAG verdict silently stayed authoritative in the pre-transaction
// read — the exact straddle #2721 exists to close, one new is_managed writer
// away from being reachable.
//
// If this fails the class is back: a role that was is_managed = TRUE at guard
// time is deleted anyway.
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteRole_ReEnforcesIsManagedInsideTheGuardTransaction(t *testing.T) {
	f := newDenialFixture(t)
	victim := f.ts.CreateTestRole(t, f.serverID, "dnvictim"+uuid.New().String()[:8], denialTargetBelow, 0)
	barrier := holdRoleRowBarrier(t, f.probe, victim)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- f.actorRequest("DELETE", rolePath(f.serverID, victim), nil)
	}()
	dbtest.WaitForRowLockWaiter(t, f.probe, barrier.txID)

	// The handler already saw is_managed = FALSE in its cheap pre-check. Flip the
	// flag and release in one commit, so it wakes into a world where the role it
	// pre-checked is now system-managed.
	barrier.commitAndRelease(t, `UPDATE roles SET is_managed = TRUE WHERE id = $1`, victim)

	rec := <-done
	require.Equal(t, http.StatusForbidden, rec.Code,
		"R4 REGRESSION — the class is back: the in-transaction guard read is_managed and "+
			"ignored it; body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Cannot delete managed roles")

	var exists bool
	require.NoError(t, f.ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM roles WHERE id = $1)`, victim).Scan(&exists))
	assert.True(t, exists, "R4 REGRESSION: the now-managed role was deleted anyway")
}
