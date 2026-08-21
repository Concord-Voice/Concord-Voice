package rbac_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	_ "github.com/lib/pq" // registers the PostgreSQL driver used by the probe pool
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// Probe pool and barrier primitives (#2721)
//
// Every barrier and every observation runs on a pool of its own, never on
// ts.DB: a backend cannot observe its own pending lock while it is blocked on
// it, and borrowing from the handler's own 5-connection pool to hold a barrier
// would starve the request under test.
// ─────────────────────────────────────────────────────────────────────────────

const (
	lockRoleForShare          = `SELECT id FROM roles WHERE id = $1 FOR SHARE`
	lockRoleForShareNoWait    = `SELECT id FROM roles WHERE id = $1 FOR SHARE NOWAIT`
	lockRoleForKeyShareNoWait = `SELECT id FROM roles WHERE id = $1 FOR KEY SHARE NOWAIT`
	lockRoleForNoKeyUpdate    = `SELECT id FROM roles WHERE id = $1 FOR NO KEY UPDATE`
	currentTxIDQuery          = `SELECT txid_current()`
)

func openLockProbePool(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	db.SetMaxOpenConns(6)
	db.SetMaxIdleConns(6)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func beginProbeTx(t *testing.T, probe *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := probe.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// roleRowBarrier holds an uncommitted FOR NO KEY UPDATE on one roles row. That
// is the lock ReorderRoles' applyRolePositions takes, and it is the lock the
// guard's FOR SHARE must wait on.
type roleRowBarrier struct {
	conn *sql.Conn
	tx   *sql.Tx
	txID int64
}

func holdRoleRowBarrier(t *testing.T, probe *sql.DB, roleID string) *roleRowBarrier {
	t.Helper()
	ctx := context.Background()
	conn, err := probe.Conn(ctx)
	require.NoError(t, err)
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)

	var lockedID string
	require.NoError(t, tx.QueryRowContext(ctx, lockRoleForNoKeyUpdate, roleID).Scan(&lockedID))

	b := &roleRowBarrier{conn: conn, tx: tx}
	require.NoError(t, tx.QueryRowContext(ctx, currentTxIDQuery).Scan(&b.txID))
	t.Cleanup(func() {
		_ = b.tx.Rollback()
		_ = b.conn.Close()
	})
	return b
}

// commitAndRelease applies a final statement to the barred role and releases the
// barrier in the SAME commit, so a blocked handler wakes into a world where the
// mutation has already happened.
func (b *roleRowBarrier) commitAndRelease(t *testing.T, query string, args ...any) {
	t.Helper()
	_, err := b.tx.ExecContext(context.Background(), query, args...)
	require.NoError(t, err)
	require.NoError(t, b.tx.Commit())
}

// commitPositionRaise moves the barred role above the actor's ceiling and
// releases the lock in the SAME commit, so the blocked handler wakes into a
// world where its authorization has already been revoked.
func (b *roleRowBarrier) commitPositionRaise(t *testing.T, roleID string, position int) {
	t.Helper()
	b.commitAndRelease(t, `UPDATE roles SET position = $2 WHERE id = $1`, roleID, position)
}

// holdAdvisoryBarrier takes the per-server visibility-capture advisory key at
// SESSION scope, which parks any role mutation on that server at its very first
// statement. Returns an idempotent release.
func holdAdvisoryBarrier(t *testing.T, probe *sql.DB, key int64) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := probe.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key)
	require.NoError(t, err)

	var once sync.Once
	release := func() {
		once.Do(func() {
			_, unlockErr := conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, key)
			assert.NoError(t, unlockErr)
			assert.NoError(t, conn.Close())
		})
	}
	t.Cleanup(release)
	return release
}

// ─────────────────────────────────────────────────────────────────────────────
// AC6 — SQL-level lock-compatibility proofs
// ─────────────────────────────────────────────────────────────────────────────

type lockProofFixture struct {
	ts       *testhelpers.TestServer
	probe    *sql.DB
	serverID string
	memberID string
	roleID   string
}

func newLockProofFixture(t *testing.T) *lockProofFixture {
	t.Helper()
	ts, _, member, serverID := setupOwnerAndMember(t)
	return &lockProofFixture{
		ts:       ts,
		probe:    openLockProbePool(t),
		serverID: serverID,
		memberID: member.ID,
		roleID:   ts.CreateTestRole(t, serverID, "lockproof"+uuid.New().String()[:8], 2, 0),
	}
}

// AC6 (1). Two guards evaluating concurrently against the same role must not
// serialize each other — otherwise the guard turns every role mutation on a
// server into a queue.
func TestGuardLock_TwoConcurrentForShareReadersDoNotBlock(t *testing.T) {
	f := newLockProofFixture(t)
	ctx := context.Background()
	var id string

	first := beginProbeTx(t, f.probe)
	require.NoError(t, first.QueryRowContext(ctx, lockRoleForShare, f.roleID).Scan(&id))

	// NOWAIT converts "would block" into an immediate 55P03. That makes this a
	// deterministic compatibility proof: no goroutine, no poll, no timing.
	second := beginProbeTx(t, f.probe)
	require.NoError(t, second.QueryRowContext(ctx, lockRoleForShareNoWait, f.roleID).Scan(&id),
		"a second FOR SHARE reader must acquire the lock immediately")
}

// AC6 (2). The load-bearing half: FOR SHARE is not decorative. If it did not
// conflict with a position write, the guard could still read a stale position
// while ReorderRoles committed underneath it — the exact TOCTOU #2721 closes.
func TestGuardLock_ForShareBlocksAConcurrentRolePositionUpdate(t *testing.T) {
	f := newLockProofFixture(t)
	ctx := context.Background()

	var id string
	holder := beginProbeTx(t, f.probe)
	require.NoError(t, holder.QueryRowContext(ctx, lockRoleForShare, f.roleID).Scan(&id))
	var holderTxID int64
	require.NoError(t, holder.QueryRowContext(ctx, currentTxIDQuery).Scan(&holderTxID))

	type updateOutcome struct {
		rows int64
		err  error
	}
	done := make(chan updateOutcome, 1)
	go func() {
		res, err := f.probe.ExecContext(ctx,
			`UPDATE roles SET position = position + 1 WHERE id = $1`, f.roleID)
		if err != nil {
			done <- updateOutcome{err: err}
			return
		}
		rows, rowsErr := res.RowsAffected()
		done <- updateOutcome{rows: rows, err: rowsErr}
	}()

	// If FOR SHARE did not conflict, no waiter would ever appear and this poll
	// would exhaust its budget. Reaching the next line IS the proof.
	dbtest.WaitForRowLockWaiter(t, f.probe, holderTxID)

	require.NoError(t, holder.Rollback())
	outcome := <-done
	require.NoError(t, outcome.err)
	assert.Equal(t, int64(1), outcome.rows,
		"the position update completes the moment the FOR SHARE is released")
}

// AC6 (3). AssignRole's own write inserts into member_roles, whose FK takes
// FOR KEY SHARE on the parent role. If the guard's FOR SHARE conflicted with
// that, every AssignRole would self-deadlock against its own guard.
func TestGuardLock_ForShareDoesNotBlockAMemberRolesInsert(t *testing.T) {
	f := newLockProofFixture(t)
	ctx := context.Background()
	var id string

	holder := beginProbeTx(t, f.probe)
	require.NoError(t, holder.QueryRowContext(ctx, lockRoleForShare, f.roleID).Scan(&id))

	inserter := beginProbeTx(t, f.probe)
	require.NoError(t, inserter.QueryRowContext(ctx, lockRoleForKeyShareNoWait, f.roleID).Scan(&id),
		"FOR KEY SHARE — the mode the member_roles FK takes — must be grantable under a held FOR SHARE")

	_, err := inserter.ExecContext(ctx,
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		f.serverID, f.memberID, f.roleID)
	require.NoError(t, err)
	require.NoError(t, inserter.Commit())
}

// ─────────────────────────────────────────────────────────────────────────────
// AC4 / AC7 — per-handler race tests
//
// Shape, identical for all four row-locked handlers: a second connection holds
// a conflicting FOR NO KEY UPDATE on the target role, the handler is fired into
// a goroutine and parks at its guard's FOR SHARE, a predicate poll confirms it
// is genuinely WAITING, and only then does the barrier commit a position raise
// that revokes the actor's authority. The handler must re-read POST-mutation
// state and refuse.
//
// On main (guard outside the transaction) the guard read completed before the
// barrier existed, the handler never blocked, and the mutation committed with a
// 200. That is the before/after discriminator.
// ─────────────────────────────────────────────────────────────────────────────

const (
	raceActorCeiling  = 5
	raceRoleStartPos  = 2
	raceRoleRaisedPos = 9
)

type roleRaceFixture struct {
	ts       *testhelpers.TestServer
	probe    *sql.DB
	serverID string
	actor    testhelpers.TestUser
	target   testhelpers.TestUser
	roleID   string
}

func setupRoleRace(t *testing.T) *roleRaceFixture {
	t.Helper()
	ts, _, actor, serverID := setupOwnerAndMember(t)
	target := ts.CreateTestUser(t, "racetgt"+uuid.New().String()[:6])
	ts.AddMemberToServer(t, serverID, target.ID, "member")

	grantPermToUser(t, ts, serverID, actor.ID, raceActorCeiling,
		int64(rbac.PermManageRoles|rbac.PermManageRolesAssign))

	return &roleRaceFixture{
		ts:       ts,
		probe:    openLockProbePool(t),
		serverID: serverID,
		actor:    actor,
		target:   target,
		roleID:   ts.CreateTestRole(t, serverID, "raced"+uuid.New().String()[:8], raceRoleStartPos, 0),
	}
}

// runBlockedRequest fires the request, waits for it to park on the barrier, then
// revokes the actor's authority and releases.
func (f *roleRaceFixture) runBlockedRequest(
	t *testing.T, method, path string, body interface{},
) *httptest.ResponseRecorder {
	t.Helper()
	barrier := holdRoleRowBarrier(t, f.probe, f.roleID)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- f.ts.DoRequest(method, path, body, testhelpers.AuthHeaders(f.actor.AccessToken))
	}()

	dbtest.WaitForRowLockWaiter(t, f.probe, barrier.txID)

	// Release BEFORE the guard's 3s lock_timeout expires. A timed-out guard
	// returns 500, which would prove nothing about the hierarchy re-read.
	barrier.commitPositionRaise(t, f.roleID, raceRoleRaisedPos)

	return <-done
}

func (f *roleRaceFixture) roleExists(t *testing.T) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM roles WHERE id = $1)`, f.roleID).Scan(&exists))
	return exists
}

func (f *roleRaceFixture) assignmentExists(t *testing.T) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3)`,
		f.serverID, f.target.ID, f.roleID).Scan(&exists))
	return exists
}

func TestUpdateRole_BlockedOnGuardRowLock_ObservesPostMutationPosition(t *testing.T) {
	f := setupRoleRace(t)

	rec := f.runBlockedRequest(t, "PATCH", rolePath(f.serverID, f.roleID),
		map[string]interface{}{"name": "Tampered"})

	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "above your own position")

	var name string
	require.NoError(t, f.ts.DB.QueryRow(`SELECT name FROM roles WHERE id = $1`, f.roleID).Scan(&name))
	assert.NotEqual(t, "Tampered", name, "the denied update must not have committed")
}

func TestDeleteRole_BlockedOnGuardRowLock_ObservesPostMutationPosition(t *testing.T) {
	f := setupRoleRace(t)

	rec := f.runBlockedRequest(t, "DELETE", rolePath(f.serverID, f.roleID), nil)

	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "above your own position")
	assert.True(t, f.roleExists(t), "the denied delete must not have committed")
}

func TestAssignRole_BlockedOnGuardRowLock_ObservesPostMutationPosition(t *testing.T) {
	f := setupRoleRace(t)

	rec := f.runBlockedRequest(t, "POST", assignRolePath(f.serverID, f.target.ID),
		map[string]interface{}{"role_id": f.roleID})

	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "equal or higher position")
	assert.False(t, f.assignmentExists(t), "the denied assignment must not have committed")
}

func TestUnassignRole_BlockedOnGuardRowLock_ObservesPostMutationPosition(t *testing.T) {
	f := setupRoleRace(t)
	f.ts.AssignRoleToUser(t, f.serverID, f.target.ID, f.roleID)

	rec := f.runBlockedRequest(t, "DELETE",
		unassignRolePath(f.serverID, f.target.ID, f.roleID), nil)

	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "equal or higher position")
	assert.True(t, f.assignmentExists(t), "the denied unassign must not have removed the row")
}

// CreateRole has no target role row, so its only barrier is the advisory key it
// now joins. This is the fifth handler's AC4 proof: the actor is demoted while
// the request is parked at LockServerVisibilityCapture, and the in-transaction,
// cache-free resolve must see the demotion — the Redis entry the route
// middleware populated one statement earlier still says otherwise.
func TestCreateRole_BlockedOnAdvisoryLock_ResolvesActorPermissionsAfterDemotion(t *testing.T) {
	ts, _, actor, serverID := setupOwnerAndMember(t)
	probe := openLockProbePool(t)
	actorRoleID := grantPermToUser(t, ts, serverID, actor.ID, raceActorCeiling,
		int64(rbac.PermManageRoles|rbac.PermManageChannels))

	key, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)
	release := holdAdvisoryBarrier(t, probe, key)

	roleName := "raced" + uuid.New().String()[:8]
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- ts.DoRequest("POST", rolesPath(serverID), map[string]interface{}{
			"name":        roleName,
			"permissions": fmt.Sprintf("%d", int64(rbac.PermManageChannels)),
		}, testhelpers.AuthHeaders(actor.AccessToken))
	}()

	dbtest.WaitForAdvisoryLockWaiter(t, probe, key)

	_, err = ts.DB.Exec(`UPDATE roles SET permissions = $2 WHERE id = $1`,
		actorRoleID, int64(rbac.PermManageRoles))
	require.NoError(t, err)
	release()

	rec := <-done
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Cannot grant permissions you do not have")

	var exists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM roles WHERE server_id = $1 AND name = $2)`,
		serverID, roleName).Scan(&exists))
	assert.False(t, exists, "the denied create must not have committed")
}

// ─────────────────────────────────────────────────────────────────────────────
// AC8 / AC9 — regression locks
// ─────────────────────────────────────────────────────────────────────────────

func permCacheKey(serverID, userID string) string {
	return fmt.Sprintf("perm:%s:%s", serverID, userID)
}

// The test that would have caught the cache-staleness defect. CreateRole used to
// authorize against GetEffectivePermissions, which is cache-first with a
// 5-minute TTL, so a just-demoted actor could mint a permanent over-privileged
// role. The poisoned entry is deliberately also what carries the actor past the
// route's own cache-first RequirePermission middleware — that middleware is not
// the guard, and this test pins the difference.
func TestCreateRole_IgnoresAPoisonedPermissionCacheEntry(t *testing.T) {
	ts, _, member, serverID := setupOwnerAndMember(t)
	ctx := context.Background()

	poisoned := int64(rbac.PermManageRoles | rbac.PermManageChannels)
	require.NoError(t, ts.Redis.Set(ctx, permCacheKey(serverID, member.ID), poisoned, 5*time.Minute).Err())

	roleName := "poisoned" + uuid.New().String()[:8]
	rec := ts.DoRequest("POST", rolesPath(serverID), map[string]interface{}{
		"name":        roleName,
		"permissions": fmt.Sprintf("%d", int64(rbac.PermManageChannels)),
	}, testhelpers.AuthHeaders(member.AccessToken))

	require.Equal(t, http.StatusForbidden, rec.Code,
		"the in-transaction resolve is cache-free, so the poisoned entry must not authorize anything; body: %s",
		rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Cannot grant permissions you do not have")

	var exists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM roles WHERE server_id = $1 AND name = $2)`,
		serverID, roleName).Scan(&exists))
	assert.False(t, exists)
}

// AC8, second half. Publishing from a transaction that then rolls back would
// seed Redis with a permission state that never committed.
func TestResolveServerPermissionsTx_PublishesNoCacheEntry(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "txres"+uuid.New().String()[:6])
	member := ts.CreateTestUser(t, "txmem"+uuid.New().String()[:6])
	serverID := ts.CreateTestServer(t, owner.ID, "Tx Resolve Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	cache := rbac.NewPermissionCache(ts.Redis)
	require.NoError(t, ts.Redis.Del(ctx, permCacheKey(serverID, member.ID)).Err())

	tx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	perms, err := resolver.ResolveServerPermissionsTx(ctx, tx, serverID, member.ID)
	require.NoError(t, err)
	require.Equal(t, rbac.BasePermissions, perms)
	require.NoError(t, tx.Rollback())

	_, found := cache.Get(ctx, serverID, member.ID, "")
	assert.False(t, found,
		"an in-transaction resolve must publish nothing; a rolled-back transaction would poison Redis")
}

// AC9. CreateRole has NO owner bypass, and that omission — not the subset test —
// is what keeps bit 62 out of a server, because OwnerPermissions excludes it.
// This must already be green; it is a lock, not a new behaviour.
func TestCreateRole_OwnerStillCannotMintAnAdministratorRole(t *testing.T) {
	ts, owner, _, serverID := setupOwnerAndMember(t)

	roleName := "godmode" + uuid.New().String()[:8]
	rec := ts.DoRequest("POST", rolesPath(serverID), map[string]interface{}{
		"name":        roleName,
		"permissions": fmt.Sprintf("%d", int64(rbac.PermAdministrator)),
	}, testhelpers.AuthHeaders(owner.AccessToken))

	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Cannot grant permissions you do not have")

	var exists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM roles WHERE server_id = $1 AND name = $2)`,
		serverID, roleName).Scan(&exists))
	assert.False(t, exists)
}
