package servers_test

// #3453 L-1 (lock order) and the lock_timeout proof for
// PUT /api/v1/servers/:id/mfa-enforcement, driven over HTTP through the real
// router.
//
// A deadlock is detected at the driver, not in the handlers' logs:
// sqlStateSpy sits between the router's pool and lib/pq and counts the SQLSTATE
// of every *pq.Error any statement, row fetch or commit returns. PostgreSQL
// reports 40P01 to the victim's own statement, so a deadlock is counted even
// when a handler maps it to a generic 500 or discards it. A control builds a
// real deadlock through the same pool first, so the spy's silence means
// something.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/servers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

const (
	sqlStateDeadlock    = "40P01"
	sqlStateLockTimeout = "55P03"

	// lockProofBound bounds every wait in this file. The PUT's lock_timeout is
	// 3 s and PostgreSQL's deadlock_timeout is 1 s, so a healthy wait ends
	// well inside it, and a regression fails here instead of hanging.
	lockProofBound = 10 * time.Second

	// lockOrderConcurrentRuns is how many start-barrier races each pair gets
	// on top of its deterministic interleavings.
	lockOrderConcurrentRuns = 20

	// lockWaitersSQL counts backends of this database queued on a row lock (a
	// tuple or the holder's transaction id) or an advisory lock. It is the
	// barrier that proves a request is waiting, where a sleep would guess.
	lockWaitersSQL = `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'
		  AND wait_event IN ('tuple', 'transactionid', 'advisory')`
)

// ---------------------------------------------------------------------------
// The SQLSTATE spy
// ---------------------------------------------------------------------------

// sqlStateSpy counts SQLSTATEs crossing the router's pool, and can pause one
// statement, chosen by a fragment of its SQL text, so a test can hold a
// counterpart's transaction open at a known point in its lock sequence without
// a production seam.
type sqlStateSpy struct {
	mu    sync.Mutex
	codes map[string]int
	pause atomic.Pointer[statementPause]
}

func (s *sqlStateSpy) note(err error) error {
	var pqErr *pq.Error
	if err != nil && errors.As(err, &pqErr) {
		s.mu.Lock()
		s.codes[string(pqErr.Code)]++
		s.mu.Unlock()
	}
	return err
}

func (s *sqlStateSpy) count(code string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codes[code]
}

// statementPause blocks the first statement whose text contains fragment
// until Release. Only that one statement blocks.
type statementPause struct {
	fragment    string
	reached     chan struct{}
	release     chan struct{}
	fired       atomic.Bool
	releaseOnce sync.Once
}

func (p *statementPause) Release() { p.releaseOnce.Do(func() { close(p.release) }) }

func (s *sqlStateSpy) armPause(t *testing.T, fragment string) *statementPause {
	t.Helper()
	p := &statementPause{fragment: fragment, reached: make(chan struct{}), release: make(chan struct{})}
	s.pause.Store(p)
	t.Cleanup(func() {
		s.pause.CompareAndSwap(p, nil)
		p.Release()
	})
	return p
}

func (s *sqlStateSpy) beforeStatement(query string) {
	p := s.pause.Load()
	if p == nil || !strings.Contains(query, p.fragment) || !p.fired.CompareAndSwap(false, true) {
		return
	}
	close(p.reached)
	<-p.release
}

// The driver interfaces lib/pq implements. The spy forwards every one, so
// database/sql takes exactly the paths it takes against lib/pq directly; a pq
// release that drops one fails loudly here instead of changing those paths.
type (
	pqConn interface {
		driver.Conn
		driver.ConnBeginTx
		driver.ConnPrepareContext
		driver.ExecerContext
		driver.QueryerContext
		driver.Pinger
		driver.SessionResetter
		driver.Validator
		driver.NamedValueChecker
	}
	pqStmt interface {
		driver.Stmt
		driver.StmtExecContext
		driver.StmtQueryContext
	}
	pqRows interface {
		driver.Rows
		driver.RowsNextResultSet
		driver.RowsColumnTypeScanType
		driver.RowsColumnTypeDatabaseTypeName
		driver.RowsColumnTypeLength
		driver.RowsColumnTypePrecisionScale
	}
)

type spyConnector struct {
	inner driver.Connector
	spy   *sqlStateSpy
}

func (c spyConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, c.spy.note(err)
	}
	pc, ok := conn.(pqConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlStateSpy: %T no longer implements every forwarded interface", conn)
	}
	return &spyConn{pqConn: pc, spy: c.spy}, nil
}

func (c spyConnector) Driver() driver.Driver { return c.inner.Driver() }

type spyConn struct {
	pqConn
	spy *sqlStateSpy
}

func (c *spyConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *spyConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	st, err := c.pqConn.PrepareContext(ctx, query)
	if err != nil {
		return nil, c.spy.note(err)
	}
	ps, ok := st.(pqStmt)
	if !ok {
		_ = st.Close()
		return nil, fmt.Errorf("sqlStateSpy: statement %T no longer implements every forwarded interface", st)
	}
	return &spyStmt{pqStmt: ps, spy: c.spy}, nil
}

func (c *spyConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *spyConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.pqConn.BeginTx(ctx, opts)
	if err != nil {
		return nil, c.spy.note(err)
	}
	return spyTx{tx: tx, spy: c.spy}, nil
}

func (c *spyConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.spy.beforeStatement(query)
	res, err := c.pqConn.ExecContext(ctx, query, args)
	return res, c.spy.note(err)
}

func (c *spyConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.spy.beforeStatement(query)
	rows, err := c.pqConn.QueryContext(ctx, query, args)
	if err != nil {
		return nil, c.spy.note(err)
	}
	return c.spy.wrapRows(rows)
}

type spyTx struct {
	tx  driver.Tx
	spy *sqlStateSpy
}

func (t spyTx) Commit() error   { return t.spy.note(t.tx.Commit()) }
func (t spyTx) Rollback() error { return t.spy.note(t.tx.Rollback()) }

type spyStmt struct {
	pqStmt
	spy *sqlStateSpy
}

func (s *spyStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	res, err := s.pqStmt.ExecContext(ctx, args)
	return res, s.spy.note(err)
}

func (s *spyStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := s.pqStmt.QueryContext(ctx, args)
	if err != nil {
		return nil, s.spy.note(err)
	}
	return s.spy.wrapRows(rows)
}

func (s *sqlStateSpy) wrapRows(rows driver.Rows) (driver.Rows, error) {
	pr, ok := rows.(pqRows)
	if !ok {
		_ = rows.Close()
		return nil, fmt.Errorf("sqlStateSpy: rows %T no longer implement every forwarded interface", rows)
	}
	return &spyRows{pqRows: pr, spy: s}, nil
}

// spyRows notes a row-fetch error: a SELECT … FOR UPDATE that waits on a lock
// can report 40P01 after the row description, from Next rather than Query.
type spyRows struct {
	pqRows
	spy *sqlStateSpy
}

func (r *spyRows) Next(dest []driver.Value) error {
	err := r.pqRows.Next(dest)
	if errors.Is(err, io.EOF) {
		return err
	}
	return r.spy.note(err)
}

func (r *spyRows) NextResultSet() error {
	err := r.pqRows.NextResultSet()
	if errors.Is(err, io.EOF) {
		return err
	}
	return r.spy.note(err)
}

func (r *spyRows) Close() error { return r.spy.note(r.pqRows.Close()) }

// openSQLStateSpyDB opens a pool on the test database whose every connection
// crosses a sqlStateSpy.
func openSQLStateSpyDB(t *testing.T) (*sqlStateSpy, *sql.DB) {
	t.Helper()
	connector, err := pq.NewConnector(testdb.DatabaseURL())
	require.NoError(t, err)
	spy := &sqlStateSpy{codes: map[string]int{}}
	db := sql.OpenDB(spyConnector{inner: connector, spy: spy})
	// Enough connections that no request here waits on the POOL while another
	// holds a row lock it needs: that would be a Go-side deadlock PostgreSQL
	// cannot see, and the test would hang rather than report it.
	db.SetMaxOpenConns(24)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	return spy, db
}

// ---------------------------------------------------------------------------
// Barriers
// ---------------------------------------------------------------------------

// inflight is one request running on its own goroutine. w is written before
// done is closed, so reading it after <-done is race-free.
type inflight struct {
	done chan struct{}
	w    *httptest.ResponseRecorder
}

func startRequest(do func() *httptest.ResponseRecorder) *inflight {
	r := &inflight{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.w = do()
	}()
	return r
}

func (r *inflight) await(t *testing.T, what string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case <-r.done:
		return r.w
	case <-time.After(lockProofBound):
		t.Fatalf("%s did not answer within %s", what, lockProofBound)
		return nil
	}
}

// waitForSignal waits for signal, failing if the request that should raise it
// finishes first or nothing happens within the bound.
func waitForSignal(t *testing.T, signal <-chan struct{}, r *inflight, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-r.done:
		t.Fatalf("the request finished before %s: %d %s", what, r.w.Code, r.w.Body.String())
	case <-time.After(lockProofBound):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitForLockWaitOrDone polls pg_stat_activity until some backend queues on a
// lock (true) or done closes (false). The poll interval only paces reads of an
// observed state; nothing is inferred from elapsed time.
func waitForLockWaitOrDone(t *testing.T, observer *sql.DB, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(lockProofBound)
	for {
		select {
		case <-done:
			return false
		default:
		}
		var waiting int
		require.NoError(t, observer.QueryRow(lockWaitersSQL).Scan(&waiting))
		if waiting > 0 {
			return true
		}
		if time.Now().After(deadline) {
			t.Fatalf("no lock wait and no completion within %s", lockProofBound)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// The lock_timeout proof
// ---------------------------------------------------------------------------

// With the servers row (or the actor's users row) held FOR UPDATE by another
// transaction, the PUT answers 503 with Retry-After: 1 once its 3 s
// lock_timeout fires, while the holder is still open, and logs
// failure_class=mfa_gate_lock with the lock-timeout cause. The users-row case
// proves the 55P03 is found inside the *stepup.Error LockSubjectTx returns,
// with a real timeout rather than an injected one.
// Kills: the SET LOCAL lock_timeout statement deleted (the PUT waits for the
// holder, past lockProofBound).
func TestMFAEnforcement_LockTimeoutIs503WithinTheBound(t *testing.T) {
	env := setupMFAEnforcementEnv(t)
	f := newMFAFixture(t, env, "mfalt", false)
	enrollMFATOTP(t, env, f.owner.ID)

	cases := []struct {
		name, holdSQL, holdID string
	}{
		{"servers row held FOR UPDATE", `SELECT 1 FROM servers WHERE id = $1 FOR UPDATE`, f.serverID},
		{"the actor's users row held FOR UPDATE", `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, f.owner.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			holder, err := env.ts.DB.BeginTx(ctx, nil)
			require.NoError(t, err)
			release := func() { _ = holder.Rollback() }
			t.Cleanup(release)
			_, err = holder.ExecContext(ctx, tc.holdSQL, tc.holdID)
			require.NoError(t, err)
			env.logs.Reset()

			started := time.Now()
			put := startRequest(func() *httptest.ResponseRecorder { return putMFA(env, f.owner, f.serverID, bodyOn()) })
			select {
			case <-put.done:
			case <-time.After(lockProofBound):
				release() // let the waiting PUT finish rather than outlive the test
				put.await(t, "the PUT, after the holder released")
				t.Fatalf("the PUT was still waiting on the held row after %s: its lock wait is unbounded", lockProofBound)
			}
			elapsed := time.Since(started)
			w := put.w

			// The holder is still open here, so the answer is the timeout's.
			assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			assert.Equal(t, "1", w.Header().Get("Retry-After"))
			assert.JSONEq(t, mfaErrorBody("The server is busy. Try again."), w.Body.String())
			assert.GreaterOrEqual(t, elapsed, 3*time.Second, "the 503 must come from the 3 s lock_timeout, not a fast failure")
			logged := env.logs.String()
			assert.Contains(t, logged, "failure_class=mfa_gate_lock")
			assert.Contains(t, logged, "canceling statement due to lock timeout")

			release()
			assert.False(t, readMFAFlag(t, env, f.serverID), "a timed-out PUT must not write")
		})
	}
}

// ---------------------------------------------------------------------------
// L-1: lock order
// ---------------------------------------------------------------------------

// The PUT's lock set, in order, inside one READ COMMITTED transaction:
//
//	users(actor) FOR SHARE -> servers(S) FOR NO KEY UPDATE
//	  -> user_mfa_totp(actor) row lock (an OFF that spends a backup code)
//	  -> UPDATE servers(S) (the NO KEY UPDATE it already holds)
//
// It takes no advisory lock, no other users row, and no lock on any child of
// S. So the PUT only ever waits at users(actor) holding nothing, at S holding
// users(actor) SHARE, or at user_mfa_totp(actor) holding both. A cycle
// therefore needs a counterpart that holds S or user_mfa_totp(actor) and THEN
// waits on users(actor) in a mode that conflicts with SHARE. Per pair:
//
//   - DeleteServer: [advisory lifecycle] -> users(voice candidates) NO KEY
//     UPDATE -> S FOR UPDATE -> channels FOR UPDATE -> DELETE S, cascading
//     to children. Users only before S; the cascade deletes rows that
//     REFERENCE users and never locks a users row. Its preflight transaction
//     locks S alone and commits first. No edge from S back to users.
//   - Erasure of the actor: [advisory scopes] -> users(actor, …) FOR UPDATE
//     -> … -> DELETE users(actor). Both transactions start at users(actor);
//     whichever gets it runs to commit while the other waits holding nothing.
//   - Erasure of the owner: users(owner) FOR UPDATE -> … -> DELETE
//     users(owner), whose cascade deletes S (FOR UPDATE). The PUT never locks
//     users(owner) and the erasure never locks users(actor): the only shared
//     row is S, a single-resource wait.
//   - AssignRole (withAuthorityCapture): advisory(server) -> users(owner,
//     actor) sorted, NO KEY UPDATE -> S FOR UPDATE -> roles(target) FOR SHARE
//     -> INSERT member_roles (FK KEY SHARE on server_members and roles).
//     Users before S, the PUT's own order; the PUT takes no advisory lock, so
//     that lock adds no edge.
//   - TOTPDisable by the actor: users(actor) NO KEY UPDATE -> user_mfa_totp
//     read and DELETE -> UPDATE users(actor). Both start at users(actor) in
//     conflicting modes, so they serialize before either touches
//     user_mfa_totp.
//
// Each pair runs its deterministic interleavings, each in both directions
// (ON, and OFF with a backup code), then concurrent start-barrier races:
//
//   - "PUT holds its gate locks": the PUT is paused just after LockGateTx
//     (users + S held) and the counterpart must queue behind it.
//   - "PUT holds only users": the PUT is paused between its two locks, which
//     is exactly where an inverted COUNTERPART (S, then users) deadlocks.
//   - "counterpart holds its locks": the counterpart is paused at a statement
//     it runs while holding its locks, and the PUT arrives.
//   - "counterpart holds only users" (AssignRole, the one counterpart that
//     takes users(actor) and then S): the counterpart is paused between those
//     two locks, which is exactly where an inverted PUT (S, then users)
//     deadlocks.
//
// Every outcome must be a legitimate answer for that pair, and the spy must
// count zero 40P01 across all of them.
// Kills: LockGateTx re-ordered to lock S before users(actor) ("counterpart
// holds only users" deadlocks); a counterpart re-ordered to lock S before
// users(actor) ("PUT holds only users" deadlocks).
func TestMFAEnforcement_LockOrderHasNoDeadlockWithItsCounterparts(t *testing.T) {
	spy, routerDB := openSQLStateSpyDB(t)
	env := setupMFAEnforcementEnvWithDB(t, routerDB)

	control := newLockFixture(t, env, false)
	requireSpyCountsADeadlock(t, spy, routerDB, env.ts.DB, control.owner.ID, control.admin.ID)
	baseline := spy.count(sqlStateDeadlock)

	for _, c := range lockCounterparts() {
		t.Run(c.name, func(t *testing.T) {
			schedules := []lockSchedule{schedPutHoldsGate, schedPutHoldsUsers, schedCounterpartHolds}
			if c.usersOnlyFragment != "" {
				schedules = append(schedules, schedCounterpartHoldsUsers)
			}
			for _, sched := range schedules {
				for i, on := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s/on=%t", sched, on), func(t *testing.T) {
						runLockPair(t, env, spy, c, sched, i)
					})
				}
			}
			for i := 0; i < lockOrderConcurrentRuns; i++ {
				t.Run(fmt.Sprintf("%s/%d", schedConcurrent, i), func(t *testing.T) {
					runLockPair(t, env, spy, c, schedConcurrent, i)
				})
			}
		})
	}
	assert.Equal(t, baseline, spy.count(sqlStateDeadlock), "the toggle deadlocked against a counterpart (40P01)")
	t.Logf("lock-order runs: 40P01=%d (control only), 55P03=%d",
		spy.count(sqlStateDeadlock), spy.count(sqlStateLockTimeout))
}

type lockSchedule string

const (
	schedPutHoldsGate     lockSchedule = "PUT holds its gate locks"
	schedPutHoldsUsers    lockSchedule = "PUT holds only users"
	schedCounterpartHolds lockSchedule = "counterpart holds its locks"
	// schedCounterpartHoldsUsers is "counterpart holds only users".
	schedCounterpartHoldsUsers lockSchedule = "counterpart holds only users"
	schedConcurrent            lockSchedule = "concurrent"
)

// The PUT's legitimate answers, classified.
const (
	putOK           = "ok"
	putServerGone   = "server gone"
	putNotAMember   = "not a member"
	putActorGone    = "actor gone"
	putEnrollment   = "enrollment required"
	putLockConflict = "lock conflict 503"
)

func classifyPut(w *httptest.ResponseRecorder, on bool) string {
	body := w.Body.String()
	switch {
	case w.Code == http.StatusOK && body == mfaValueBody(on):
		return putOK
	case w.Code == http.StatusNotFound:
		return putServerGone
	case w.Code == http.StatusUnauthorized:
		return putActorGone
	case w.Code == http.StatusForbidden && body == mfaErrorBody("Not a member of this server"):
		return putNotAMember
	case w.Code == http.StatusForbidden && strings.Contains(body, `"mfa_enrollment_required":true`):
		return putEnrollment
	case w.Code == http.StatusServiceUnavailable && w.Header().Get("Retry-After") == "1":
		return putLockConflict
	}
	return fmt.Sprintf("unexpected %d %s", w.Code, body)
}

// lockFixture is a fresh server per run, so no run inherits a rate-limit
// bucket, a spent code or a deleted row from another. The PUT's actor is
// always the raw-bit Administrator, so the owner and the actor are different
// users rows.
type lockFixture struct {
	owner, admin    testhelpers.TestUser
	adminTOTPSecret string
	serverID        string
	roleID          string
}

func newLockFixture(t *testing.T, env *mfaEnv, enforcing bool) lockFixture {
	t.Helper()
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	ts := env.ts
	f := lockFixture{owner: ts.CreateTestUser(t, "lo"+tag), admin: ts.CreateTestUser(t, "la"+tag)}
	f.serverID = ts.CreateTestServer(t, f.owner.ID, "Lock order "+tag)
	ts.AddMemberToServer(t, f.serverID, f.admin.ID, "member")
	adminRole := ts.CreateTestRole(t, f.serverID, "admin-"+tag, 5, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, f.serverID, f.admin.ID, adminRole)
	f.roleID = ts.CreateTestRole(t, f.serverID, "extra-"+tag, 2, 0)
	// Both enrolled: the owner so AssignRole's permission check is not masked
	// on an enforcing server, the actor so both directions can pass the gate.
	enrollMFATOTP(t, env, f.owner.ID)
	f.adminTOTPSecret = enrollMFATOTP(t, env, f.admin.ID)
	setMFAFlag(t, env, f.serverID, enforcing)
	return f
}

type lockCounterpart struct {
	name string
	// fragment is a statement the counterpart runs while holding the locks the
	// argument above names; "counterpart holds its locks" pauses it there.
	fragment string
	// usersOnlyFragment, when set, is the counterpart's servers lock, which it
	// takes AFTER locking users(actor); "counterpart holds only users" pauses
	// it there.
	usersOnlyFragment string
	do                func(t *testing.T, env *mfaEnv, f lockFixture) *httptest.ResponseRecorder
	wantCode          int
	// putOutcomes are the PUT's legitimate answers against this counterpart.
	putOutcomes []string
	// putQueuesBehind: while the counterpart is paused holding its locks, the
	// PUT must queue behind it. False only where the two share no row at that
	// point.
	putQueuesBehind bool
}

func lockCounterparts() []lockCounterpart {
	erase := func(u testhelpers.TestUser, env *mfaEnv) *httptest.ResponseRecorder {
		return env.ts.DoRequest(http.MethodPost, "/api/v1/privacy/erase-account", nil, testhelpers.AuthHeaders(u.AccessToken))
	}
	return []lockCounterpart{
		{
			name:     "DeleteServer",
			fragment: `DELETE FROM servers WHERE id = $1`,
			do: func(_ *testing.T, env *mfaEnv, f lockFixture) *httptest.ResponseRecorder {
				return env.ts.DoRequest(http.MethodDelete, "/api/v1/servers/"+f.serverID, nil,
					testhelpers.AuthHeaders(f.owner.AccessToken))
			},
			wantCode:        http.StatusOK,
			putOutcomes:     []string{putOK, putServerGone, putNotAMember},
			putQueuesBehind: true,
		},
		{
			name:     "erasure of the actor",
			fragment: `DELETE FROM users WHERE id = $1`,
			do: func(_ *testing.T, env *mfaEnv, f lockFixture) *httptest.ResponseRecorder {
				return erase(f.admin, env)
			},
			wantCode:        http.StatusNoContent,
			putOutcomes:     []string{putOK, putActorGone, putNotAMember},
			putQueuesBehind: true,
		},
		{
			name:     "erasure of the owner",
			fragment: `DELETE FROM users WHERE id = $1`,
			do: func(_ *testing.T, env *mfaEnv, f lockFixture) *httptest.ResponseRecorder {
				return erase(f.owner, env)
			},
			wantCode:        http.StatusNoContent,
			putOutcomes:     []string{putOK, putServerGone, putNotAMember},
			putQueuesBehind: false,
		},
		{
			name:              "AssignRole",
			fragment:          `INSERT INTO member_roles (server_id, user_id, role_id, assigned_by)`,
			usersOnlyFragment: `SELECT id FROM servers WHERE id = $1 FOR UPDATE`,
			do: func(_ *testing.T, env *mfaEnv, f lockFixture) *httptest.ResponseRecorder {
				return env.ts.DoRequest(http.MethodPost,
					"/api/v1/servers/"+f.serverID+"/members/"+f.admin.ID+"/roles",
					map[string]any{"role_id": f.roleID}, testhelpers.AuthHeaders(f.owner.AccessToken))
			},
			wantCode:        http.StatusOK,
			putOutcomes:     []string{putOK},
			putQueuesBehind: true,
		},
		{
			name:     "TOTPDisable by the actor",
			fragment: `DELETE FROM user_mfa_totp WHERE user_id = $1`,
			do: func(t *testing.T, env *mfaEnv, f lockFixture) *httptest.ResponseRecorder {
				code, err := totp.GenerateCode(f.adminTOTPSecret, time.Now())
				if err != nil {
					t.Errorf("generate TOTP code: %v", err)
				}
				return env.ts.DoRequest(http.MethodPost, "/api/v1/mfa/totp/disable",
					map[string]any{"password": testhelpers.TestAuthPlaintext, "code": code},
					testhelpers.AuthHeaders(f.admin.AccessToken))
			},
			wantCode:        http.StatusOK,
			putOutcomes:     []string{putOK, putEnrollment},
			putQueuesBehind: true,
		},
	}
}

// runLockPair runs one PUT against one counterpart under sched, on a fresh
// fixture. Run i flips the setting ON when i is even and OFF (with a backup
// code) when it is odd.
func runLockPair(t *testing.T, env *mfaEnv, spy *sqlStateSpy, c lockCounterpart, sched lockSchedule, i int) {
	on := i%2 == 0
	f := newLockFixture(t, env, !on)
	body := bodyOn()
	if !on {
		body = bodyOff(mfaBackupCode)
	}
	doPut := func() *httptest.ResponseRecorder { return putMFA(env, f.admin, f.serverID, body) }
	doCounterpart := func() *httptest.ResponseRecorder { return c.do(t, env, f) }
	deadlocksBefore := spy.count(sqlStateDeadlock)

	var put, cp *inflight
	switch sched {
	case schedPutHoldsGate, schedPutHoldsUsers:
		held, release := pauseToggleGate(t, sched == schedPutHoldsUsers)
		put = startRequest(doPut)
		waitForSignal(t, held, put, "the PUT to take its gate locks")
		cp = startRequest(doCounterpart)
		blocked := waitForLockWaitOrDone(t, env.ts.DB, cp.done)
		release()
		if sched == schedPutHoldsGate {
			assert.True(t, blocked, "the counterpart must queue behind the PUT's locks, or this interleaving proved nothing")
		}
	case schedCounterpartHolds, schedCounterpartHoldsUsers:
		fragment, mustQueue := c.fragment, c.putQueuesBehind
		if sched == schedCounterpartHoldsUsers {
			fragment, mustQueue = c.usersOnlyFragment, true
		}
		pause := spy.armPause(t, fragment)
		cp = startRequest(doCounterpart)
		waitForSignal(t, pause.reached, cp, "the counterpart to reach "+fragment)
		put = startRequest(doPut)
		blocked := waitForLockWaitOrDone(t, env.ts.DB, put.done)
		pause.Release()
		if mustQueue {
			assert.True(t, blocked, "the PUT must queue behind the paused counterpart, or this interleaving proved nothing")
		}
	case schedConcurrent:
		start := make(chan struct{})
		put = startRequest(func() *httptest.ResponseRecorder { <-start; return doPut() })
		cp = startRequest(func() *httptest.ResponseRecorder { <-start; return doCounterpart() })
		close(start)
	}

	putW := put.await(t, "the PUT")
	cpW := cp.await(t, c.name)
	assert.Equal(t, deadlocksBefore, spy.count(sqlStateDeadlock), "40P01 between the PUT and %s", c.name)
	assert.Contains(t, c.putOutcomes, classifyPut(putW, on), "the PUT's answer")
	assert.Equal(t, c.wantCode, cpW.Code, "%s answered %s", c.name, cpW.Body.String())
}

// pauseToggleGate replaces the PUT's gate with the real one plus a pause:
// after LockGateTx (users and servers held), or, with usersOnly, after the
// actor's users row alone, re-running the real LockGateTx on release. Taking a
// users row the transaction already holds, in the same mode, adds no wait.
func pauseToggleGate(t *testing.T, usersOnly bool) (held <-chan struct{}, release func()) {
	t.Helper()
	heldCh, releaseCh := make(chan struct{}), make(chan struct{})
	var heldOnce, releaseOnce sync.Once
	signal := func() { heldOnce.Do(func() { close(heldCh) }) }
	release = func() { releaseOnce.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	servers.SetMFAEnforcementGateForTest(t, func(ctx context.Context, tx *sql.Tx, serverID, actorID string,
		userLock stepup.Lock, serverLock mfaenforce.ServerLock, tokenEpoch string,
	) (mfaenforce.Gate, error) {
		if usersOnly {
			if _, e := stepup.LockSubjectTx(ctx, tx, actorID, userLock, tokenEpoch); e != nil {
				signal()
				return mfaenforce.Gate{}, e
			}
			signal()
			<-releaseCh
			return mfaenforce.LockGateTx(ctx, tx, serverID, actorID, userLock, serverLock, tokenEpoch)
		}
		g, err := mfaenforce.LockGateTx(ctx, tx, serverID, actorID, userLock, serverLock, tokenEpoch)
		signal()
		if err == nil {
			<-releaseCh
		}
		return g, err
	})
	return heldCh, release
}

// requireSpyCountsADeadlock builds a real two-row deadlock through the spied
// pool (each transaction holds one users row and asks for the other) and
// requires exactly one 40P01 counted: the control that gives the spy's
// silence its meaning.
func requireSpyCountsADeadlock(t *testing.T, spy *sqlStateSpy, spied, observer *sql.DB, rowA, rowB string) {
	t.Helper()
	ctx := context.Background()
	before := spy.count(sqlStateDeadlock)
	lock := func(tx *sql.Tx, id string) error {
		var got string
		return tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&got)
	}
	tx1, err := spied.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx1.Rollback() }()
	tx2, err := spied.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx2.Rollback() }()
	require.NoError(t, lock(tx1, rowA))
	require.NoError(t, lock(tx2, rowB))

	firstErr := make(chan error, 1)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		firstErr <- lock(tx1, rowB)
	}()
	require.True(t, waitForLockWaitOrDone(t, observer, firstDone), "tx1 must queue on rowB")
	second := lock(tx2, rowA)
	var first error
	select {
	case first = <-firstErr:
	case <-time.After(lockProofBound):
		t.Fatalf("the control deadlock was not resolved within %s", lockProofBound)
	}
	victims := 0
	for _, err := range []error{first, second} {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == sqlStateDeadlock {
			victims++
		}
	}
	require.Equal(t, 1, victims, "the control must deadlock exactly one side: %v / %v", first, second)
	require.Equal(t, before+1, spy.count(sqlStateDeadlock), "the spy must count the control's 40P01")
}
