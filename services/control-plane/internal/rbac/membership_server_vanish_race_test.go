package rbac_test

// Regression: a server deleted between the permission resolver's membership
// read and its owner read answered 500 ("Failed to check membership") instead
// of denying. It surfaced as a 2-in-8 flake of
// TestMFAEnforcement_LockOrderHasNoDeadlockWithItsCounterparts
// (internal/servers, subtest erasure_of_the_owner/concurrent), where
// PUT /servers/:id/mfa-enforcement races the owner's account erasure:
// servers.owner_id is ON DELETE CASCADE, so erasing the owner deletes the
// server, and outside a caller-supplied snapshot the resolver's reads are
// separate READ COMMITTED statements. The membership read saw the row, the
// owner read did not, and its sql.ErrNoRows was reported as a database fault.
//
// The interleaving is forced, not raced for. A driver hook on the resolver's
// own pool runs the deleting write on ANOTHER pool at the one point that
// matters: after the membership read has executed and before the owner read
// is sent. A control injects a genuine fault at that same read and requires it
// to stay a fault, so no fix can pass by reading every owner-read failure as
// "not a member" — that would turn a database outage into a quiet denial.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	// Fragments of the resolver's two reads. Every entry point below issues the
	// membership read first and the owner read next.
	memberReadFragment = `FROM server_members WHERE server_id = $1 AND user_id = $2`
	ownerReadFragment  = `FROM servers WHERE id = $1`
)

type vanishFixture struct {
	owner, member testhelpers.TestUser
	serverID      string
	channelID     string
}

func newVanishFixture(t *testing.T, ts *testhelpers.TestServer) vanishFixture {
	t.Helper()
	suffix := uuid.NewString()[:8]
	f := vanishFixture{
		owner:  ts.CreateTestUser(t, "vanisho"+suffix),
		member: ts.CreateTestUser(t, "vanishm"+suffix),
	}
	f.serverID = ts.CreateTestServer(t, f.owner.ID, "Vanishing server")
	ts.AddMemberToServer(t, f.serverID, f.member.ID, "member")
	f.channelID = ts.CreateTestChannel(t, f.serverID, "vanishing")
	return f
}

// The member holds PermViewTextChannels through @all, so "allowed" is what an
// uninterrupted request answers and a denial is attributable to the race.
const vanishProbePerm = rbac.PermViewTextChannels

// classifyVanishTx normalizes a Tx entry point's result.
func classifyVanishTx(perms rbac.Permission, err error) string {
	switch {
	case err == nil && perms.Has(vanishProbePerm):
		return stmthook.Allowed
	case errors.Is(err, rbac.ErrNotMember):
		return stmthook.Denied
	case errors.Is(err, stmthook.ErrInjected):
		return stmthook.Fault
	}
	return fmt.Sprintf("unexpected perms=%d err=%v", perms, err)
}

// inReadCommittedTx runs fn on a READ COMMITTED transaction of the hooked pool:
// each statement takes its own snapshot, as the resolver's callers' do.
func inReadCommittedTx(t testing.TB, db *sql.DB, fn func(*sql.Tx) string) string {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	// Rolling back is the cleanup here, never a no-op: fn does not commit. A
	// rollback that fails is reported; ErrTxDone would mean fn ended the
	// transaction itself.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback: %v", err)
		}
	}()
	return fn(tx)
}

// rollbackRecorder is a testing.TB that records Errorf instead of failing, so
// a case can require that a helper reports what it is given.
type rollbackRecorder struct {
	testing.TB
	errs []string
}

func (r *rollbackRecorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// Codex on #3508: inReadCommittedTx discarded its deferred rollback's error, so
// a case whose transaction failed to roll back still passed on the
// classification it had already computed. The statement ends its own session,
// so the rollback that follows runs on a connection the server has closed.
func TestInReadCommittedTx_ReportsARollbackThatFails(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()

	failed := &rollbackRecorder{TB: t}
	got := inReadCommittedTx(failed, db, func(tx *sql.Tx) string {
		_, _ = tx.Exec(`SELECT pg_terminate_backend(pg_backend_pid())`)
		return stmthook.Allowed
	})
	assert.Equal(t, stmthook.Allowed, got)
	require.Len(t, failed.errs, 1, "the failed rollback is reported")
	assert.Contains(t, failed.errs[0], "rollback")

	// Control: a rollback that succeeds reports nothing.
	clean := &rollbackRecorder{TB: t}
	inReadCommittedTx(clean, db, func(tx *sql.Tx) string {
		_, err := tx.Exec(`SELECT 1`)
		require.NoError(t, err)
		return stmthook.Allowed
	})
	assert.Empty(t, clean.errs)

	// Control: fn that commits leaves the rollback nothing to do (ErrTxDone),
	// which is not a failure.
	committed := &rollbackRecorder{TB: t}
	inReadCommittedTx(committed, db, func(tx *sql.Tx) string {
		require.NoError(t, tx.Commit())
		return stmthook.Allowed
	})
	assert.Empty(t, committed.errs)
}

type vanishEntryPoint struct {
	name string
	run  func(t *testing.T, res *rbac.Resolver, db *sql.DB, f vanishFixture) string
}

func vanishEntryPoints() []vanishEntryPoint {
	// middleware normalizes a gin middleware's answer by status AND exact body,
	// so a 403 or 500 from anywhere else is not mistaken for this one.
	middleware := func(handler func(*rbac.Resolver) gin.HandlerFunc, denyBody, faultBody string) func(*testing.T, *rbac.Resolver, *sql.DB, vanishFixture) string {
		return func(_ *testing.T, res *rbac.Resolver, _ *sql.DB, f vanishFixture) string {
			w := doMiddlewareRequest(handler(res), f.member.ID, f.serverID)
			code, body := w.Code, w.Body.String()
			switch {
			case code == http.StatusOK:
				return stmthook.Allowed
			case code == http.StatusForbidden && body == denyBody:
				return stmthook.Denied
			case code == http.StatusInternalServerError && body == faultBody:
				return stmthook.Fault
			}
			return fmt.Sprintf("unexpected %d %s", code, body)
		}
	}
	return []vanishEntryPoint{
		{
			name: "RequireMembership",
			run: middleware(rbac.RequireMembership,
				`{"error":"Not a member of this server"}`, `{"error":"Failed to check membership"}`),
		},
		{
			name: "RequirePermission",
			run: middleware(func(res *rbac.Resolver) gin.HandlerFunc { return rbac.RequirePermission(res, vanishProbePerm, "") },
				`{"error":"Insufficient permissions"}`, `{"error":"Failed to check permissions"}`),
		},
		{
			name: "ResolveServerPermissionsTx",
			run: func(t *testing.T, res *rbac.Resolver, db *sql.DB, f vanishFixture) string {
				return inReadCommittedTx(t, db, func(tx *sql.Tx) string {
					return classifyVanishTx(res.ResolveServerPermissionsTx(context.Background(), tx, f.serverID, f.member.ID))
				})
			},
		},
		{
			name: "ResolveChannelPermissionsTx",
			run: func(t *testing.T, res *rbac.Resolver, db *sql.DB, f vanishFixture) string {
				return inReadCommittedTx(t, db, func(tx *sql.Tx) string {
					return classifyVanishTx(res.ResolveChannelPermissionsTx(context.Background(), tx, f.serverID, f.member.ID, f.channelID))
				})
			},
		},
		{
			name: "GetVisibleChannelIDs",
			run: func(_ *testing.T, res *rbac.Resolver, _ *sql.DB, f vanishFixture) string {
				ids, err := res.GetVisibleChannelIDs(context.Background(), f.serverID, f.member.ID)
				switch {
				case err == nil && slices.Contains(ids, f.channelID):
					return stmthook.Allowed
				case err == nil && len(ids) == 0:
					return stmthook.Denied
				case errors.Is(err, stmthook.ErrInjected):
					return stmthook.Fault
				}
				return fmt.Sprintf("unexpected ids=%v err=%v", ids, err)
			},
		},
	}
}

// TestResolver_ServerVanishingBetweenMembershipAndOwnerRead_IsNotAMember pins
// the oracle: when the server disappears after the membership read, every
// entry point that shares those two reads denies the caller as a non-member
// (403 at the middleware) instead of reporting a fault (500), while a real
// fault at the same read is still reported as one.
func TestResolver_ServerVanishingBetweenMembershipAndOwnerRead_IsNotAMember(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)

	hook, hookedDB := stmthook.Open(t)
	res := rbac.NewResolver(hookedDB, rbac.NewPermissionCache(ts.Redis), logger.New("test"))
	sequence := []string{memberReadFragment, ownerReadFragment}

	for _, ep := range vanishEntryPoints() {
		for _, sc := range stmthook.Scenarios() {
			t.Run(ep.name+"/"+sc.Name, func(t *testing.T) {
				f := newVanishFixture(t, ts)
				hook.Arm(sequence, sc.Between(ts.DB, f.owner.ID, f.serverID), sc.Fault)

				got := ep.run(t, res, hookedDB, f)

				stmthook.RequireInterleaved(t, ts.DB, hook, sc, len(sequence), f.serverID)
				assert.Equal(t, sc.Want, got,
					"%s, with the server deleted after its membership read, must deny as a non-member rather than report a fault; "+
						"a real fault at that read must still be reported", ep.name)
			})
		}
	}
}
