package rbac

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fixture
//
// These are UNIT tests of authorizeRoleMutationTx in the sense that they call
// the guard directly rather than through HTTP — but the guard IS one SQL
// statement, so "unit" still means real PostgreSQL. Its behaviour cannot be
// exercised against a fake: the snapshot coherence between the target row, the
// actor-ceiling aggregate and the owner subquery is the property under test.
// ─────────────────────────────────────────────────────────────────────────────

type guardFixture struct {
	h        *Handler
	db       *sql.DB
	serverID string
	ownerID  string
	actorID  string
}

func newGuardFixture(t *testing.T) *guardFixture {
	t.Helper()
	db, _ := dbtest.SetupTestDB(t)

	log := logger.New("test")
	f := &guardFixture{
		h:        &Handler{db: db, log: log, resolver: NewResolver(db, nil, log)},
		db:       db,
		serverID: uuid.New().String(),
		ownerID:  dbtest.CreateUser(t, db).String(),
		actorID:  dbtest.CreateUser(t, db).String(),
	}

	guardExec(t, db, `INSERT INTO servers (id, name, owner_id) VALUES ($1, 'Role Guard Fixture', $2)`,
		f.serverID, f.ownerID)
	guardExec(t, db, `INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner')`,
		f.serverID, f.ownerID)
	guardExec(t, db, `INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'member')`,
		f.serverID, f.actorID)
	return f
}

func guardExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := db.Exec(query, args...)
	require.NoError(t, err)
}

// createRole inserts a non-default, non-managed role at an explicit position.
func (f *guardFixture) createRole(t *testing.T, name string, position int, perms Permission) string {
	t.Helper()
	roleID := uuid.New().String()
	guardExec(t, f.db,
		`INSERT INTO roles (id, server_id, name, position, permissions, is_default, is_managed)
		 VALUES ($1, $2, $3, $4, $5, FALSE, FALSE)`,
		roleID, f.serverID, name, position, int64(perms))
	return roleID
}

// grantActorRole gives the actor a role, which both sets their hierarchy ceiling
// and contributes its bits to their effective permission set.
func (f *guardFixture) grantActorRole(t *testing.T, position int, perms Permission) string {
	t.Helper()
	roleID := f.createRole(t, "actor_"+uuid.New().String()[:8], position, perms)
	guardExec(t, f.db, `INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		f.serverID, f.actorID, roleID)
	return roleID
}

// authorize runs the guard inside a real transaction and rolls it back. The
// transaction is the point: the guard's contract is that every operand comes
// from ONE snapshot on the caller's transaction.
func (f *guardFixture) authorize(
	t *testing.T, actorID, roleID string, mode conferredMode, requested int64,
) (roleGuardResult, error) {
	t.Helper()
	tx, err := f.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return f.h.authorizeRoleMutationTx(
		context.Background(), tx, f.serverID, actorID, roleID, mode, requested)
}

// ─────────────────────────────────────────────────────────────────────────────
// Guard behaviour
// ─────────────────────────────────────────────────────────────────────────────

func TestAuthorizeRoleMutationTx_UnknownRole_ReturnsRoleGone(t *testing.T) {
	f := newGuardFixture(t)
	f.grantActorRole(t, 5, PermManageRoles)

	_, err := f.authorize(t, f.actorID, uuid.New().String(), confersNothing, 0)

	require.ErrorIs(t, err, errRoleGone,
		"a target role that does not exist is a 404, not a 403 and not a 500")
}

func TestAuthorizeRoleMutationTx_TargetAtOrAboveActorCeiling_DeniesHierarchy(t *testing.T) {
	f := newGuardFixture(t)
	f.grantActorRole(t, 5, PermManageRoles)

	t.Run("target strictly above the ceiling is denied", func(t *testing.T) {
		roleID := f.createRole(t, "above", 9, 0)
		_, err := f.authorize(t, f.actorID, roleID, confersNothing, 0)
		require.ErrorIs(t, err, errHierarchyDenied)
	})

	t.Run("target exactly at the ceiling is denied", func(t *testing.T) {
		roleID := f.createRole(t, "equal", 5, 0)
		_, err := f.authorize(t, f.actorID, roleID, confersNothing, 0)
		require.ErrorIs(t, err, errHierarchyDenied,
			"the comparison is >=, so a role level with the actor is out of reach")
	})

	t.Run("target below the ceiling is allowed", func(t *testing.T) {
		roleID := f.createRole(t, "below", 4, 0)
		res, err := f.authorize(t, f.actorID, roleID, confersNothing, 0)
		require.NoError(t, err)
		assert.Equal(t, 4, res.Position, "the guard returns every value it read")
	})
}

func TestAuthorizeRoleMutationTx_ConfersRequested_RejectsBitsTheActorLacks(t *testing.T) {
	f := newGuardFixture(t)
	f.grantActorRole(t, 5, PermManageRoles)
	roleID := f.createRole(t, "target", 1, 0)

	t.Run("requested bit the actor does not hold", func(t *testing.T) {
		_, err := f.authorize(t, f.actorID, roleID, confersRequested, int64(PermManageChannels))
		require.ErrorIs(t, err, errEscalationDenied)
	})

	t.Run("requested bits that are a subset of the actor's", func(t *testing.T) {
		_, err := f.authorize(t, f.actorID, roleID, confersRequested, int64(PermManageRoles))
		require.NoError(t, err)
	})
}

// The whole point of confersTargetRole is that the compared bitfield is the one
// the guard re-read under FOR SHARE. If it ever consulted the caller-supplied
// `requested` instead, exactly one of these two subtests would flip.
func TestAuthorizeRoleMutationTx_ConfersTargetRole_ChecksTheReReadPermissions(t *testing.T) {
	f := newGuardFixture(t)
	f.grantActorRole(t, 5, PermManageRoles)

	t.Run("denies on the role's own bits even when requested is empty", func(t *testing.T) {
		roleID := f.createRole(t, "rich", 1, PermManageChannels)
		_, err := f.authorize(t, f.actorID, roleID, confersTargetRole, 0)
		require.ErrorIs(t, err, errEscalationDenied,
			"requested=0 is a subset of everything; the denial can only come from the re-read row")
	})

	t.Run("allows on the role's own bits even when requested is over-privileged", func(t *testing.T) {
		roleID := f.createRole(t, "plain", 1, PermManageRoles)
		_, err := f.authorize(t, f.actorID, roleID, confersTargetRole, int64(PermAdministrator))
		require.NoError(t, err,
			"requested carries bit 62; allowing proves the check ignored it and used r.permissions")
	})
}

func TestAuthorizeRoleMutationTx_ConfersNothing_RunsNoSubsetCheck(t *testing.T) {
	f := newGuardFixture(t)
	f.grantActorRole(t, 5, PermManageRoles)
	roleID := f.createRole(t, "privileged", 1, PermManageChannels|PermManageServer)

	res, err := f.authorize(t, f.actorID, roleID, confersNothing, 0)

	require.NoError(t, err,
		"delete and server-scope unassign confer no bits, so the role's own bits are irrelevant")
	assert.Equal(t, int64(PermManageChannels|PermManageServer), res.Permissions)
}

func TestAuthorizeRoleMutationTx_Owner_BypassesHierarchyAndSubset(t *testing.T) {
	f := newGuardFixture(t)
	// The owner holds no roles at all, so their ceiling is 0 — the hierarchy
	// check would deny every role in the server if it ran.
	roleID := f.createRole(t, "untouchable", 9, PermAdministrator)

	res, err := f.authorize(t, f.ownerID, roleID, confersRequested, int64(PermAdministrator))

	require.NoError(t, err)
	assert.True(t, res.IsOwner, "ownership is derived from servers.owner_id, never from a permission bit")
}

// shadowWithEmptyTemp replaces a real table with an empty session-local
// temporary table for the life of tx.
//
// Two guard properties fail CLOSED on a row that the schema makes
// unconstructible with data alone — roles.server_id is NOT NULL REFERENCES
// servers, and member_roles carries an FK onto server_members — so the absent
// row is synthesised instead. pg_temp is searched ahead of public, so the
// guard's unqualified table references resolve to the empty shadow while every
// other table still resolves to the real one. ON COMMIT DROP guarantees the
// shadow cannot outlive this transaction and reach another test through the
// connection pool.
func shadowWithEmptyTemp(t *testing.T, tx *sql.Tx, ddl string) {
	t.Helper()
	_, err := tx.ExecContext(context.Background(), ddl)
	require.NoError(t, err)
}

// A servers row that is not visible to the guard's snapshot leaves owner_id
// NULL. The guard must read that as "not the owner" and keep the hierarchy check
// running, never as an ambiguous state that opens the gate.
func TestAuthorizeRoleMutationTx_NullOwnerID_FailsClosed(t *testing.T) {
	f := newGuardFixture(t)
	f.grantActorRole(t, 5, PermManageRoles)
	roleID := f.createRole(t, "above_ceiling", 9, 0)

	// Control: with the real servers table the owner bypasses the hierarchy.
	_, err := f.authorize(t, f.ownerID, roleID, confersNothing, 0)
	require.NoError(t, err, "control: the owner is normally allowed above their ceiling")

	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	shadowWithEmptyTemp(t, tx,
		`CREATE TEMP TABLE servers (id UUID PRIMARY KEY, owner_id UUID) ON COMMIT DROP`)

	res, err := f.h.authorizeRoleMutationTx(
		ctx, tx, f.serverID, f.ownerID, roleID, confersNothing, 0)

	require.ErrorIs(t, err, errHierarchyDenied,
		"a NULL owner_id must fail closed: not the owner, so the hierarchy check still applies")
	assert.False(t, res.IsOwner)
}

// ─────────────────────────────────────────────────────────────────────────────
// Locking-clause regression lock (AC5)
// ─────────────────────────────────────────────────────────────────────────────

// RL-2: a guard takes AT MOST ONE row lock, always the target roles row, always
// FOR SHARE. A stronger mode, or an unqualified clause that could widen as the
// plan shape changes, re-opens the deadlock class the design argues away.
func TestRoleGuardQuery_TakesExactlyOneForShareLockOnTheTargetRole(t *testing.T) {
	upper := strings.ToUpper(roleGuardQuery)

	assert.Equal(t, 1, strings.Count(upper, "FOR SHARE"),
		"exactly one locking clause")
	assert.Contains(t, upper, "FOR SHARE OF R",
		"the clause is table-qualified onto the target roles row, not left plan-shape dependent")
	assertNoStrongerLocking(t, upper)
}

// assertNoStrongerLocking pins the clauses NEITHER guard form may ever carry.
// FOR UPDATE would conflict with the concurrent readers the guard must not
// serialize; NOWAIT / SKIP LOCKED would turn contention into a spurious denial.
func assertNoStrongerLocking(t *testing.T, upper string) {
	t.Helper()
	assert.Equal(t, 0, strings.Count(upper, "FOR UPDATE"))
	assert.Equal(t, 0, strings.Count(upper, "FOR NO KEY UPDATE"))
	assert.Equal(t, 0, strings.Count(upper, "FOR KEY SHARE"))
	assert.NotContains(t, upper, "NOWAIT")
	assert.NotContains(t, upper, "SKIP LOCKED")
}

// The pre-check runs on the POOLED connection in autocommit, where no
// `SET LOCAL lock_timeout` applies. A locking clause there could therefore block
// INDEFINITELY on a concurrent roles writer, recreating the exact denial-of-
// service the pre-check exists to prevent — an unbounded wait instead of a cheap
// 403. Nothing else in the suite covers this: collapsing the constant to
// `const roleGuardPreCheckQuery = roleGuardQuery` passes every other test.
func TestRoleGuardPreCheckQuery_CarriesNoLockingClause(t *testing.T) {
	upper := strings.ToUpper(roleGuardPreCheckQuery)

	assert.Equal(t, 0, strings.Count(upper, "FOR SHARE"),
		"a FOR SHARE on the pool has no lock_timeout above it and can wait forever")
	assertNoStrongerLocking(t, upper)
}

// The authoritative and pre-check forms must stay ONE statement plus a locking
// clause. Two hand-maintained copies would drift, and a pre-check that disagreed
// with the guard would 403 mutations the authoritative guard would have allowed.
func TestRoleGuardQueries_BothDeriveFromRoleGuardSelect(t *testing.T) {
	assert.Equal(t, roleGuardSelect, roleGuardPreCheckQuery,
		"the pre-check IS the shared SELECT, unmodified")
	require.True(t, strings.HasPrefix(roleGuardQuery, roleGuardSelect),
		"the authoritative form is the shared SELECT plus a suffix")
	assert.Contains(t, strings.ToUpper(strings.TrimPrefix(roleGuardQuery, roleGuardSelect)),
		"FOR SHARE OF R", "and that suffix is the locking clause, nothing else")
	assert.NotEqual(t, roleGuardQuery, roleGuardPreCheckQuery,
		"they must not collapse into one constant: that would put a lock on the pool")
}

// The sentinels are distinct values: mapGuardError branches on identity, and two
// sentinels that compared equal would collapse a 403 into a 404 on the wire.
func TestRoleGuardSentinelsAreDistinct(t *testing.T) {
	sentinels := []error{errRoleGone, errHierarchyDenied, errEscalationDenied, errGuardLockTimeout}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			assert.False(t, errors.Is(a, b), "sentinel %d must not match sentinel %d", i, j)
		}
	}
	assert.False(t, errors.Is(errRoleGone, sql.ErrNoRows),
		"errRoleGone deliberately does NOT wrap sql.ErrNoRows: UnassignRole distinguishes "+
			"a missing ROLE from a missing ASSIGNMENT by exactly that")
}

// ─────────────────────────────────────────────────────────────────────────────
// Error paths
// ─────────────────────────────────────────────────────────────────────────────

// A guard read that fails for any reason OTHER than "no such row" or 55P03 is a
// genuine infrastructure failure. It must surface wrapped — never silently
// collapse into errRoleGone (a 404 the caller would read as "already deleted,
// nothing to do") and never into errGuardLockTimeout.
func TestAuthorizeRoleMutationTx_GuardReadFailure_WrapsTheDatabaseError(t *testing.T) {
	f := newGuardFixture(t)
	f.grantActorRole(t, 5, PermManageRoles)
	roleID := f.createRole(t, "target", 1, 0)

	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	_, err = f.h.authorizeRoleMutationTx(ctx, tx, f.serverID, f.actorID, roleID, confersNothing, 0)

	require.Error(t, err)
	assert.ErrorIs(t, err, sql.ErrTxDone, "the underlying driver error stays reachable")
	assert.Contains(t, err.Error(), "role guard read:", "and it is wrapped with its origin")
	assert.NotErrorIs(t, err, errRoleGone, "an infrastructure failure is not a 404")
	assert.NotErrorIs(t, err, errGuardLockTimeout)
	assert.NotErrorIs(t, err, errHierarchyDenied)
}

// ErrNotMember must propagate UNWRAPPED. mapGuardError branches on it to write a
// 403 "Insufficient permissions"; wrapping it would drop that branch into the
// default 500 and drift the wire contract with no test failing anywhere else.
//
// The guard's ceiling comes from member_roles while the resolve's membership
// check reads server_members, so the divergence is synthesised the same way the
// NULL owner_id case is.
func TestAuthorizeRoleMutationTx_ActorNotAMember_PropagatesErrNotMemberUnwrapped(t *testing.T) {
	f := newGuardFixture(t)
	f.grantActorRole(t, 5, PermManageRoles)
	roleID := f.createRole(t, "target", 1, 0)

	// Control: with the real server_members table the same call is authorized.
	_, err := f.authorize(t, f.actorID, roleID, confersRequested, int64(PermManageRoles))
	require.NoError(t, err, "control: the actor is a member and the requested bits are a subset")

	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	shadowWithEmptyTemp(t, tx,
		`CREATE TEMP TABLE server_members (server_id UUID, user_id UUID) ON COMMIT DROP`)

	_, err = f.h.authorizeRoleMutationTx(
		ctx, tx, f.serverID, f.actorID, roleID, confersRequested, int64(PermManageRoles))

	require.ErrorIs(t, err, ErrNotMember)
	assert.Equal(t, ErrNotMember, err,
		"unwrapped: mapGuardError's ErrNotMember branch is what makes this a 403 and not a 500")
}

func TestApplyGuardLockTimeout(t *testing.T) {
	f := newGuardFixture(t)
	ctx := context.Background()

	t.Run("succeeds on a live transaction", func(t *testing.T) {
		tx, err := f.db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		require.NoError(t, applyGuardLockTimeout(ctx, tx))

		var setting string
		require.NoError(t, tx.QueryRowContext(ctx, `SHOW lock_timeout`).Scan(&setting))
		assert.Equal(t, "3s", setting,
			"the guard's wait on the target row lock is bounded while it holds the advisory lock")
	})

	t.Run("wraps a failure to set the timeout", func(t *testing.T) {
		tx, err := f.db.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())

		err = applyGuardLockTimeout(ctx, tx)

		require.Error(t, err)
		assert.ErrorIs(t, err, sql.ErrTxDone)
		assert.Contains(t, err.Error(), "set guard lock timeout:",
			"an unbounded guard would pin every role mutation on the server, so this must not be swallowed")
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// mapGuardError — the wire contract (AC10, inner oracle)
//
// This function is the single thing preserving the pre-#2721 statuses and bodies
// across the refactor. The ~40 integration tests in handlers_integration_test.go
// are the outer oracle; this table is the inner one, and it pins EVERY branch
// rather than only the ones the handlers happen to exercise.
// ─────────────────────────────────────────────────────────────────────────────

func TestMapGuardError_StatusAndBodyPerBranch(t *testing.T) {
	const (
		hierarchyMsg = "Cannot frobnicate a role at or above your own position"
		failureMsg   = "Failed to frobnicate role"
	)

	cases := []struct {
		name        string
		err         error
		wantHandled bool
		wantStatus  int
		wantBody    string
	}{
		{
			name:        "nil writes nothing and reports unhandled",
			err:         nil,
			wantHandled: false,
			wantStatus:  http.StatusOK, // recorder default; nothing was written
			wantBody:    "",
		},
		{
			name:        "errRoleGone is a 404",
			err:         errRoleGone,
			wantHandled: true,
			wantStatus:  http.StatusNotFound,
			wantBody:    `{"error":"Role not found"}`,
		},
		{
			name:        "a bare sql.ErrNoRows is the same 404",
			err:         sql.ErrNoRows,
			wantHandled: true,
			wantStatus:  http.StatusNotFound,
			wantBody:    `{"error":"Role not found"}`,
		},
		{
			name:        "errHierarchyDenied returns the caller's own message verbatim",
			err:         errHierarchyDenied,
			wantHandled: true,
			wantStatus:  http.StatusForbidden,
			wantBody:    `{"error":"` + hierarchyMsg + `"}`,
		},
		{
			name:        "a wrapped sentinel is still matched by errors.Is",
			err:         fmt.Errorf("write closure: %w", errHierarchyDenied),
			wantHandled: true,
			wantStatus:  http.StatusForbidden,
			wantBody:    `{"error":"` + hierarchyMsg + `"}`,
		},
		{
			name:        "a WRAPPED sql.ErrNoRows is a 500, not a 404",
			err:         fmt.Errorf("failed to fetch server owner: %w", sql.ErrNoRows),
			wantHandled: true,
			wantStatus:  http.StatusInternalServerError,
			wantBody:    `{"error":"` + failureMsg + `"}`,
		},
		{
			name:        "a bare roleFlagDenied returns its own message",
			err:         roleFlagDenied{"Cannot delete managed roles"},
			wantHandled: true,
			wantStatus:  http.StatusForbidden,
			wantBody:    `{"error":"Cannot delete managed roles"}`,
		},
		{
			name:        "a wrapped roleFlagDenied is still matched by errors.As",
			err:         fmt.Errorf("write closure: %w", roleFlagDenied{"Cannot unassign default roles"}),
			wantHandled: true,
			wantStatus:  http.StatusForbidden,
			wantBody:    `{"error":"Cannot unassign default roles"}`,
		},
		{
			// Wire-identical to the default by DESIGN, so this row pins the contract
			// rather than discriminating. The log-class test below is what fails if
			// the isGuardLockTimeout arm is removed.
			name:        "a raw 55P03 from the post-guard write is the same 500 on the wire",
			err:         &pq.Error{Code: "55P03"},
			wantHandled: true,
			wantStatus:  http.StatusInternalServerError,
			wantBody:    `{"error":"` + failureMsg + `"}`,
		},
		{
			name:        "errEscalationDenied is a 403 with the fixed escalation body",
			err:         errEscalationDenied,
			wantHandled: true,
			wantStatus:  http.StatusForbidden,
			wantBody:    `{"error":"Cannot grant permissions you do not have"}`,
		},
		{
			name:        "ErrNotMember is a 403, not a 500",
			err:         ErrNotMember,
			wantHandled: true,
			wantStatus:  http.StatusForbidden,
			wantBody:    `{"error":"Insufficient permissions"}`,
		},
		{
			name:        "errGuardLockTimeout is identical on the wire to any other failure",
			err:         errGuardLockTimeout,
			wantHandled: true,
			wantStatus:  http.StatusInternalServerError,
			wantBody:    `{"error":"` + failureMsg + `"}`,
		},
		{
			name:        "an unrelated error falls through to the 500 default",
			err:         errors.New("connection reset by peer"),
			wantHandled: true,
			wantStatus:  http.StatusInternalServerError,
			wantBody:    `{"error":"` + failureMsg + `"}`,
		},
	}

	h := &Handler{log: logger.New("test")}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)

			handled := h.mapGuardError(c, tc.err, hierarchyMsg, failureMsg)

			assert.Equal(t, tc.wantHandled, handled)
			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.wantBody, rec.Body.String())
		})
	}
}

// The guard-lock-timeout branch is deliberately indistinguishable from the
// default 500 on the wire, but it must remain distinguishable in the logs — that
// separation is the whole reason it is its own case.
func TestMapGuardError_GuardLockTimeout_LogsItsOwnFailureClass(t *testing.T) {
	var buf bytes.Buffer
	h := &Handler{log: logger.NewWithWriter(&buf)}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	require.True(t, h.mapGuardError(c, errGuardLockTimeout, "unused", "Failed to update role"))

	assert.Contains(t, buf.String(), "guard_lock_timeout",
		"operators must be able to tell a lock-starved server from a generic failure")
}

// SET LOCAL lock_timeout is TRANSACTION-scoped, so the post-guard WRITE can raise
// 55P03 on a lock the transaction does not already hold. That arrives as a raw
// *pq.Error, not as errGuardLockTimeout, and must still be classified.
func TestMapGuardError_RawLockTimeoutFromTheWritePath_KeepsTheTimeoutClass(t *testing.T) {
	var buf bytes.Buffer
	h := &Handler{log: logger.NewWithWriter(&buf)}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	require.True(t, h.mapGuardError(c, &pq.Error{Code: "55P03"}, "unused", "Failed to assign role"))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, buf.String(), "guard_lock_timeout",
		"without the isGuardLockTimeout arm this falls to the generic default and the "+
			"operator loses the failure_class they diagnose by")
}

// ─────────────────────────────────────────────────────────────────────────────
// preCheckRoleMutation — the cheap, NON-AUTHORITATIVE pre-transaction denial
// ─────────────────────────────────────────────────────────────────────────────

func TestPreCheckRoleMutation_Verdicts(t *testing.T) {
	f := newGuardFixture(t)
	f.grantActorRole(t, 5, PermManageRoles)
	ctx := context.Background()

	above := f.createRole(t, "above", 9, 0)
	below := f.createRole(t, "below", 1, 0)
	rich := f.createRole(t, "rich", 1, PermManageChannels)

	cases := []struct {
		name      string
		actorID   string
		roleID    string
		mode      conferredMode
		requested int64
		wantErr   error
	}{
		{"unknown role is denied early", f.actorID, uuid.New().String(), confersNothing, 0, errRoleGone},
		{"target above the ceiling is denied early", f.actorID, above, confersNothing, 0, errHierarchyDenied},
		{"requested bits the actor lacks are denied early",
			f.actorID, below, confersRequested, int64(PermManageChannels), errEscalationDenied},
		{"requested bits within the actor's are allowed through",
			f.actorID, below, confersRequested, int64(PermManageRoles), nil},
		{"confersTargetRole denies on the ROLE's bits, not the caller's",
			f.actorID, rich, confersTargetRole, 0, errEscalationDenied},
		{"confersNothing never runs the subset check",
			f.actorID, rich, confersNothing, 0, nil},
		// The owner short-circuit is load-bearing: ActorBasePermissions is the
		// BIT_OR over the actor's ROLES, and an owner's authority does not come
		// from roles at all. Evaluating it for an owner would manufacture a denial
		// the authoritative guard would never reach.
		{"the owner short-circuits instead of being measured by BIT_OR",
			f.ownerID, above, confersRequested, int64(PermAdministrator), nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := f.h.preCheckRoleMutation(ctx, f.serverID, tc.actorID, tc.roleID, tc.mode, tc.requested)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// The one function in this file whose ERROR PATH IS THE SECURITY DECISION.
//
// On any fault the pre-check must return nil and let the authoritative
// in-transaction guard decide, so a transient pool blip cannot 403 a legitimate
// mutation. The //nolint:nilerr on that branch says exactly this; without a test
// the annotation is the only thing standing between the current behaviour and a
// well-meaning "fix this swallowed error" edit.
func TestPreCheckRoleMutation_DatabaseFault_FailsOpenToTheAuthoritativeGuard(t *testing.T) {
	f := newGuardFixture(t)
	f.grantActorRole(t, 5, PermManageRoles)
	above := f.createRole(t, "above", 9, 0)
	ctx := context.Background()

	// Control: on a healthy pool this exact call is a hard denial.
	require.ErrorIs(t,
		f.h.preCheckRoleMutation(ctx, f.serverID, f.actorID, above, confersNothing, 0),
		errHierarchyDenied, "control: the same inputs deny when the database answers")

	// A Handler whose pool is closed. Every read faults, and nothing the pre-check
	// can observe is a denial sentinel.
	log := logger.New("test")
	broken, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, broken.Close())
	faulted := &Handler{db: broken, log: log, resolver: NewResolver(broken, nil, log)}

	assert.NoError(t,
		faulted.preCheckRoleMutation(ctx, f.serverID, f.actorID, above, confersNothing, 0),
		"a database fault must FAIL OPEN: the pre-check can only ever save work, never "+
			"grant it, and the in-transaction guard re-decides regardless")
	assert.NoError(t,
		faulted.preCheckRoleMutation(ctx, f.serverID, f.actorID, above, confersRequested,
			int64(PermAdministrator)),
		"including on the escalation half")
}

// ─────────────────────────────────────────────────────────────────────────────
// rejectRoleFlags — per-handler is_managed / is_default policy
//
// An EMPTY message means "this handler does not reject that flag". AssignRole
// passes empty for BOTH, matching its pre-#2721 behaviour: assigning a managed
// or default role is legitimate. Someone helpfully filling those blanks in would
// start refusing assignments that must succeed, and nothing else catches it.
// ─────────────────────────────────────────────────────────────────────────────

func TestRejectRoleFlags_PerHandlerPolicy(t *testing.T) {
	const (
		updateManaged   = "Cannot modify managed roles"
		deleteManaged   = "Cannot delete managed roles"
		deleteDefault   = "Cannot delete default roles"
		unassignDefault = "Cannot unassign default roles"
	)
	policies := map[string]struct{ managedMsg, defaultMsg string }{
		"UpdateRole":   {updateManaged, ""},
		"DeleteRole":   {deleteManaged, deleteDefault},
		"UnassignRole": {"", unassignDefault},
		"AssignRole":   {"", ""},
	}
	roles := map[string]roleGuardResult{
		"plain":   {},
		"managed": {IsManaged: true},
		"default": {IsDefault: true},
		"both":    {IsManaged: true, IsDefault: true},
	}
	// want[handler][role] — empty string means "no error".
	want := map[string]map[string]string{
		"UpdateRole":   {"plain": "", "managed": updateManaged, "default": "", "both": updateManaged},
		"DeleteRole":   {"plain": "", "managed": deleteManaged, "default": deleteDefault, "both": deleteManaged},
		"UnassignRole": {"plain": "", "managed": "", "default": unassignDefault, "both": unassignDefault},
		"AssignRole":   {"plain": "", "managed": "", "default": "", "both": ""},
	}

	for handler, policy := range policies {
		for role, res := range roles {
			t.Run(handler+"/"+role, func(t *testing.T) {
				err := rejectRoleFlags(res, policy.managedMsg, policy.defaultMsg)
				if want[handler][role] == "" {
					assert.NoError(t, err,
						"an empty policy message means this handler does not reject that flag")
					return
				}
				require.Error(t, err)
				assert.Equal(t, want[handler][role], err.Error(),
					"the message IS the wire body, and it differs per handler")
				var flagErr roleFlagDenied
				assert.ErrorAs(t, err, &flagErr,
					"mapGuardError matches this by type, so it must stay a roleFlagDenied")
			})
		}
	}
}
