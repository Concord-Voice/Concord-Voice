// Package stmthook forces a statement-level interleaving without a production
// seam. Open returns a pool on the test database whose every query first passes
// through a Hook; an armed Hook fires once, immediately before a chosen
// statement is sent, and there either commits a write on ANOTHER pool or fails
// that statement with an injected fault.
//
// It exists for the "server vanishes mid-request" regressions: a server
// deleted — by DeleteServer, or by its owner's erasure through
// servers.owner_id's ON DELETE CASCADE — after a request was admitted and
// before one of its later servers reads. Scenarios returns the four cases every
// such repro runs (two controls, two deletions), and RequireInterleaved proves
// a subtest exercised the race rather than something beside it.
package stmthook

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// ErrInjected is the fault a Scenario injects in place of the hooked statement.
var ErrInjected = errors.New("stmthook: injected read fault")

// Hook fires once, at the query matching the LAST fragment of its sequence,
// after every earlier fragment has been seen in order. When armed with an arg,
// that last query must also carry an argument equal to it, which tells apart
// two statements with the same text (for example the same membership EXISTS
// asked about the actor and then about the target).
type Hook struct {
	mu         sync.Mutex
	armed      bool
	sequence   []string
	arg        string
	seen       int
	between    func() error
	fault      error
	betweenErr error
	// observe, when set, is handed the context the hooked statement runs
	// under as it fires, so a test can tell a query that honours the request's
	// context from one that does not. A callback rather than a stored context:
	// the hook keeps nothing request-scoped past the statement.
	observe func(context.Context)
	// commitFault, when set, is what the next transaction's Commit reports,
	// after committing it when commitCommits is true and after rolling it
	// back otherwise. See ArmCommit.
	commitFault   error
	commitCommits bool
}

// Arm resets the hook. between, when non-nil, runs at the hooked statement and
// completes before it is sent; fault, when non-nil, is returned in its place.
func (h *Hook) Arm(sequence []string, between func() error, fault error) {
	h.ArmArg(sequence, "", between, fault)
}

// ArmArg is Arm whose last fragment only matches a query carrying an argument
// equal to arg; an empty arg matches any.
func (h *Hook) ArmArg(sequence []string, arg string, between func() error, fault error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.armed, h.sequence, h.arg, h.seen = true, sequence, arg, 0
	h.between, h.fault, h.betweenErr, h.observe = between, fault, nil, nil
}

func (h *Hook) beforeQuery(ctx context.Context, query string, args []driver.NamedValue) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.armed || h.seen == len(h.sequence) || !strings.Contains(query, h.sequence[h.seen]) {
		return nil
	}
	if h.seen == len(h.sequence)-1 && h.arg != "" && !hasArg(args, h.arg) {
		return nil
	}
	h.seen++
	if h.seen < len(h.sequence) {
		return nil
	}
	if h.observe != nil {
		h.observe(ctx)
	}
	if h.between != nil {
		h.betweenErr = h.between()
	}
	return h.fault
}

func hasArg(args []driver.NamedValue, want string) bool {
	for _, a := range args {
		if s, ok := a.Value.(string); ok && s == want {
			return true
		}
	}
	return false
}

// Observe sets the callback the armed hook hands its statement's context to
// when it fires. Call it after Arm, which clears it.
func (h *Hook) Observe(fn func(context.Context)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.observe = fn
}

// Report returns how many fragments of the sequence were seen (the hook fired
// iff seen == len(sequence)) and the interleaved write's error.
func (h *Hook) Report() (seen int, betweenErr error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen, h.betweenErr
}

// hookableConn is the lib/pq connection surface database/sql consults. The
// wrapper forwards all of it, so database/sql takes exactly the paths it takes
// against lib/pq directly, and overrides QueryContext and ExecContext alone: a
// write that reports only rows affected, such as ReorderRoles' UPDATE, reaches
// the driver through ExecContext.
type hookableConn interface {
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

type hookConnector struct {
	inner driver.Connector
	hook  *Hook
}

func (c hookConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	hc, ok := conn.(hookableConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("stmthook: %T no longer implements every forwarded interface", conn)
	}
	return &hookConn{hookableConn: hc, hook: c.hook}, nil
}

func (c hookConnector) Driver() driver.Driver { return c.inner.Driver() }

type hookConn struct {
	hookableConn
	hook *Hook
}

func (c *hookConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.hook.beforeQuery(ctx, query, args); err != nil {
		return nil, err
	}
	return c.hookableConn.QueryContext(ctx, query, args)
}

func (c *hookConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.hook.beforeQuery(ctx, query, args); err != nil {
		return nil, err
	}
	return c.hookableConn.ExecContext(ctx, query, args)
}

// Open opens a pool on the test database whose every query first passes
// through the returned hook.
func Open(t *testing.T) (*Hook, *sql.DB) {
	t.Helper()
	hook := &Hook{}
	connector, err := pq.NewConnector(testdb.DatabaseURL())
	require.NoError(t, err)
	db := sql.OpenDB(hookConnector{inner: connector, hook: hook})
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	return hook, db
}

// The normalized outcomes a repro classifies each answer into.
const (
	Allowed = "allowed"
	Denied  = "denied"
	Fault   = "reported a fault"
)

// Scenario is one case of a server-vanish repro.
type Scenario struct {
	Name string
	// DeleteSQL, when set, is the DELETE the hook commits before the hooked
	// statement: of the owner's users row when DeleteOwner, else of the servers
	// row. The server must be gone afterwards; a control must leave it.
	DeleteSQL   string
	DeleteOwner bool
	Fault       error
	Want        string
}

// Scenarios returns the two controls and the two deletions.
func Scenarios() []Scenario {
	return []Scenario{
		{
			// Positive control: the hook fires at its statement and changes
			// nothing, so the harness itself cannot be what denies.
			Name: "control: nothing interleaved",
			Want: Allowed,
		},
		{
			// A genuine fault at the hooked statement must stay a fault. This is
			// the guard against widening: a denial is not an outage, and an
			// outage must never be answered as one.
			Name:  "control: a real fault at the hooked statement",
			Fault: ErrInjected,
			Want:  Fault,
		},
		{
			Name:      "server deleted before the hooked statement",
			DeleteSQL: `DELETE FROM servers WHERE id = $1`,
			Want:      Denied,
		},
		{
			// The owner's erasure cascades to the server.
			Name:        "owner erased before the hooked statement",
			DeleteSQL:   `DELETE FROM users WHERE id = $1`,
			DeleteOwner: true,
			Want:        Denied,
		},
	}
}

// FKScenarios is Scenarios plus two controls for a fix that reads a 23503 on
// one named foreign key (serverFK, from the inserted row to servers): a 23503
// on ANY OTHER constraint, and any other error class on THAT constraint, must
// both stay a fault. They fail a fix that checks only the code, or only the
// name. The deletions exercise the real constraint, which pins its name.
func FKScenarios(serverFK string) []Scenario {
	return append(Scenarios(),
		Scenario{
			Name:  "control: a foreign-key violation on another constraint",
			Fault: &pq.Error{Code: "23503", Constraint: "some_other_fkey"},
			Want:  Fault,
		},
		Scenario{
			Name:  "control: another error class on the server foreign key",
			Fault: &pq.Error{Code: "23514", Constraint: serverFK},
			Want:  Fault,
		},
	)
}

// Between returns the write sc commits at the hooked statement, on db (which
// must not be the hooked pool), or nil for a control. It requires the DELETE to
// remove exactly one row, and a lock_timeout turns a DELETE that queues behind
// the hooked transaction — which is waiting on this very call — into an error
// instead of a hang.
func (sc Scenario) Between(db *sql.DB, ownerID, serverID string) func() error {
	if sc.DeleteSQL == "" {
		return nil
	}
	id := serverID
	if sc.DeleteOwner {
		id = ownerID
	}
	return func() (err error) {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		// A no-op once Commit has run; before that it is the real cleanup, so a
		// rollback that fails too is reported beside the error that caused it.
		defer func() {
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
			}
		}()
		if _, err := tx.Exec(`SET LOCAL lock_timeout = '5s'`); err != nil {
			return err
		}
		res, err := tx.Exec(sc.DeleteSQL, id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("%s affected %d rows (err %v), want 1", sc.DeleteSQL, n, err)
		}
		return tx.Commit()
	}
}

// TerminateIdleBackend returns a Between that ends the backend sitting idle in
// a transaction whose last statement contains lastFragment, adding the number
// it ended to *ended. A failed-discard regression arms it so the hooked
// transaction's rollback runs on a dead connection, then requires *ended to be
// exactly 1: that count is what proves the hooked backend, and only it, was
// the one ended.
func TerminateIdleBackend(db *sql.DB, lastFragment string, ended *int) func() error {
	return func() (err error) {
		rows, err := db.Query(`
			SELECT pg_terminate_backend(pid, 5000)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND state = 'idle in transaction'
			  AND position($1 in query) > 0`, lastFragment)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, rows.Close()) }()
		for rows.Next() {
			var ok bool
			if err := rows.Scan(&ok); err != nil {
				return err
			}
			if ok {
				*ended++
			}
		}
		return rows.Err()
	}
}

// RequireInterleaved proves the subtest exercised the race: the hook fired at
// the last fragment of a sequence of length want, the interleaved write
// succeeded, and the server is gone iff the scenario deleted it. db is the
// unhooked fixture pool.
func RequireInterleaved(t *testing.T, db *sql.DB, hook *Hook, sc Scenario, want int, serverID string) {
	t.Helper()
	seen, betweenErr := hook.Report()
	require.Equal(t, want, seen, "the hook saw %d of %d statements; the window was never opened", seen, want)
	require.NoError(t, betweenErr, "the interleaved write failed, so the fixture is not the race")
	var serverRows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM servers WHERE id = $1`, serverID).Scan(&serverRows))
	if sc.DeleteSQL != "" {
		require.Zero(t, serverRows, "the server must be gone by the hooked statement, or this was not the race")
	} else {
		require.Equal(t, 1, serverRows, "a control must leave the server in place")
	}
}

// ArmCommit makes the next transaction committed through this pool report
// fault from Commit. When committed is true the transaction commits first, as
// one whose COMMIT reached the server and whose acknowledgement was then lost
// would; otherwise it is rolled back, as a COMMIT the server refused would be.
// It fires once and is independent of Arm.
func (h *Hook) ArmCommit(committed bool, fault error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commitFault, h.commitCommits = fault, committed
}

// takeCommitFault disarms and returns ArmCommit's fault, if any.
func (h *Hook) takeCommitFault() (committed bool, fault error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	committed, fault = h.commitCommits, h.commitFault
	h.commitFault, h.commitCommits = nil, false
	return committed, fault
}

func (c *hookConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.hookableConn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return hookTx{Tx: tx, hook: c.hook}, nil
}

// hookTx is a transaction whose Commit ArmCommit can fault.
type hookTx struct {
	driver.Tx
	hook *Hook
}

func (t hookTx) Commit() error {
	committed, fault := t.hook.takeCommitFault()
	if fault == nil {
		return t.Tx.Commit()
	}
	end := t.Rollback
	if committed {
		end = t.Tx.Commit
	}
	if err := end(); err != nil {
		return errors.Join(fault, err)
	}
	return fault
}
