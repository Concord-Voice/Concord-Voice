package managedturn

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failDriver commits, but fails every rollback, rows close and conn close, so
// the helpers' joins can be seen; SQLite gives no way to force these.
type failDriver struct{}
type failConn struct{}
type failTx struct{}
type failStmt struct{}
type failRows struct{}

func (failDriver) Open(string) (driver.Conn, error)         { return failConn{}, nil }
func (failConn) Prepare(string) (driver.Stmt, error)        { return failStmt{}, nil }
func (failConn) Close() error                               { return errors.New("conn close failed") }
func (failConn) Begin() (driver.Tx, error)                  { return failTx{}, nil }
func (failTx) Commit() error                                { return nil }
func (failTx) Rollback() error                              { return errors.New("rollback failed") }
func (failStmt) Close() error                               { return nil }
func (failStmt) NumInput() int                              { return -1 }
func (failStmt) Exec([]driver.Value) (driver.Result, error) { return driver.ResultNoRows, nil }
func (failStmt) Query([]driver.Value) (driver.Rows, error)  { return failRows{}, nil }
func (failRows) Columns() []string                          { return []string{"x"} }
func (failRows) Close() error                               { return errors.New("rows close failed") }
func (failRows) Next([]driver.Value) error                  { return io.EOF }

func init() { sql.Register("managedturn-fail", failDriver{}) }

func TestCloseHelpers_JoinFailuresAndToleratePostCommitRollback(t *testing.T) {
	db, err := sql.Open("managedturn-fail", "")
	require.NoError(t, err)
	first := errors.New("the operation's own error")

	tx, err := db.Begin()
	require.NoError(t, err)
	got := first
	rollback(tx, &got)
	assert.ErrorIs(t, got, first, "the caller's error is kept")
	assert.ErrorContains(t, got, "managedturn: rollback: rollback failed")

	tx, err = db.Begin()
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	got = nil
	rollback(tx, &got)
	assert.NoError(t, got, "the deferred rollback after a commit is sql.ErrTxDone, not a failure")

	rows, err := db.Query("q")
	require.NoError(t, err)
	got = nil
	closeRows(rows, &got)
	assert.ErrorContains(t, got, "managedturn: close rows: rows close failed")

	assert.ErrorContains(t, closeDB(db), "managedturn: close database: conn close failed")
}

// A failed write cleans its temp file up: it may hold key material.
func TestWriteFileAtomic_FailureLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory at the target makes the final rename fail.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "target", "x"), 0o700))

	err := writeFileAtomic(dir, "target", []byte("secret-bytes"), 0o600, -1)
	require.ErrorContains(t, err, "managedturn: rename into place")
	assert.NotContains(t, err.Error(), "temp file left in")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.Contains(e.Name(), ".tmp-"), "leftover temp file %s", e.Name())
	}
}

func TestProbeError_IsProbeFailedAndKeepsItsStage(t *testing.T) {
	err := stageError(probeUDP, "allocate refused")
	assert.ErrorIs(t, err, ErrProbeFailed)
	var pe *ProbeError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "allocate refused", pe.Stage)
	assert.Equal(t, ErrProbeFailed.Error()+": udp: allocate refused", err.Error())
}
