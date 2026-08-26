package friends

// Recording-driver fixture for the #1240 privacy-gate regression tests.
// Adopted from the red-team pass on PR #2911 and MAINTAINED — the sibling
// redteam_*_test.go files depend on it.
//
// A recording database/sql driver. It lets a PoC drive the REAL production
// handlers (SendRequest, ClaimFriendCode, GetFriendRequestEligibility) end to
// end through gin while recording every SQL statement they issue, with no live
// Postgres. That matters here because the whole #1240 claim is about "identical
// database work" and "the same gate", so the statement LOG is the evidence.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
)

// recResponder answers one statement. Return (nil, nil) to mean "zero rows"
// (database/sql turns that into sql.ErrNoRows for QueryRow).
type recResponder func(q string, args []driver.NamedValue) (*recRows, error)

type recConn struct {
	mu      sync.Mutex
	log     []string
	respond recResponder
}

func (c *recConn) record(q string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, normalizeSQL(q))
}

func (c *recConn) statements() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.log))
	copy(out, c.log)
	return out
}

func normalizeSQL(q string) string { return strings.Join(strings.Fields(q), " ") }

func (c *recConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *recConn) Close() error                        { return nil }

func (c *recConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *recConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.record("BEGIN")
	return &recTx{conn: c}, nil
}

func (c *recConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.record(q)
	rows, err := c.respond(normalizeSQL(q), args)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = &recRows{cols: []string{"none"}}
	}
	return rows, nil
}

func (c *recConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.record(q)
	if _, err := c.respond(normalizeSQL(q), args); err != nil {
		return nil, err
	}
	return recResult{}, nil
}

type recTx struct{ conn *recConn }

func (t *recTx) Commit() error   { t.conn.record("COMMIT"); return nil }
func (t *recTx) Rollback() error { t.conn.record("ROLLBACK"); return nil }

type recResult struct{}

func (recResult) LastInsertId() (int64, error) { return 0, nil }
func (recResult) RowsAffected() (int64, error) { return 1, nil }

type recRows struct {
	cols []string
	data [][]driver.Value
	i    int
}

func (r *recRows) Columns() []string { return r.cols }
func (r *recRows) Close() error      { return nil }
func (r *recRows) Next(dest []driver.Value) error {
	if r.i >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.i])
	r.i++
	return nil
}

func row(cols []string, vals ...driver.Value) *recRows {
	return &recRows{cols: cols, data: [][]driver.Value{vals}}
}

type recDriver struct{ conn *recConn }

func (d *recDriver) Open(string) (driver.Conn, error) { return d.conn, nil }

var recDriverSeq atomic.Int64

// newRecordingDB registers a fresh driver and returns the *sql.DB plus the
// connection whose statement log is the evidence. MaxOpenConns(1) keeps the
// single shared conn from being handed out twice.
func newRecordingDB(respond recResponder) (*sql.DB, *recConn) {
	conn := &recConn{respond: respond}
	name := fmt.Sprintf("redteam-rec-%d", recDriverSeq.Add(1))
	sql.Register(name, &recDriver{conn: conn})
	db, err := sql.Open(name, "")
	if err != nil {
		panic(err)
	}
	db.SetMaxOpenConns(1)
	return db, conn
}

// mentions reports whether any recorded statement contains needle.
func mentions(stmts []string, needle string) bool {
	for _, s := range stmts {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
