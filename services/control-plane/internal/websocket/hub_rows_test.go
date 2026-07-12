package websocket

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type scriptedRowsDriver struct {
	columns     []string
	values      [][]driver.Value
	terminalErr error
}

func (d *scriptedRowsDriver) Open(string) (driver.Conn, error) {
	return &scriptedRowsConn{driver: d}, nil
}

type scriptedRowsConn struct {
	driver *scriptedRowsDriver
}

func (c *scriptedRowsConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (c *scriptedRowsConn) Close() error {
	return nil
}

func (c *scriptedRowsConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}

func (c *scriptedRowsConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	values := make([][]driver.Value, len(c.driver.values))
	for i := range c.driver.values {
		values[i] = append([]driver.Value(nil), c.driver.values[i]...)
	}
	return &scriptedRows{
		columns:     append([]string(nil), c.driver.columns...),
		values:      values,
		terminalErr: c.driver.terminalErr,
	}, nil
}

var _ driver.QueryerContext = (*scriptedRowsConn)(nil)

type scriptedRows struct {
	columns     []string
	values      [][]driver.Value
	terminalErr error
	index       int
}

func (r *scriptedRows) Columns() []string {
	return r.columns
}

func (r *scriptedRows) Close() error {
	return nil
}

func (r *scriptedRows) Next(dest []driver.Value) error {
	if r.index < len(r.values) {
		copy(dest, r.values[r.index])
		r.index++
		return nil
	}
	if r.terminalErr != nil {
		err := r.terminalErr
		r.terminalErr = nil
		return err
	}
	return io.EOF
}

func openScriptedRowsDB(t *testing.T, columns []string, values [][]driver.Value, terminalErr error) *sql.DB {
	t.Helper()
	driverName := "websocket-scripted-" + uuid.NewString()
	sql.Register(driverName, &scriptedRowsDriver{columns: columns, values: values, terminalErr: terminalErr})
	db, err := sql.Open(driverName, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func TestQueryServerVoiceCountsReturnsScanError(t *testing.T) {
	db := openScriptedRowsDB(t, []string{"server_id", "count"}, [][]driver.Value{{"server-1", "not-an-int"}}, nil)
	hub := NewHub(db, nil)

	_, err := hub.queryServerVoiceCounts()
	require.Error(t, err)
}

func TestQueryServerVoiceCountsReturnsIterationError(t *testing.T) {
	wantErr := errors.New("row iteration failed")
	db := openScriptedRowsDB(t, []string{"server_id", "count"}, nil, wantErr)
	hub := NewHub(db, nil)

	_, err := hub.queryServerVoiceCounts()
	require.ErrorIs(t, err, wantErr)
}

func TestQueryServerMembershipsReturnsScanError(t *testing.T) {
	db := openScriptedRowsDB(t, []string{"server_id", "user_id"}, [][]driver.Value{{"not-a-uuid", uuid.NewString()}}, nil)
	hub := NewHub(db, nil)

	_, _, err := hub.queryServerMemberships([]interface{}{uuid.New()})
	require.Error(t, err)
}

func TestQueryServerMembershipsReturnsIterationError(t *testing.T) {
	wantErr := errors.New("membership iteration failed")
	db := openScriptedRowsDB(t, []string{"server_id", "user_id"}, nil, wantErr)
	hub := NewHub(db, nil)

	_, _, err := hub.queryServerMemberships([]interface{}{uuid.New()})
	require.ErrorIs(t, err, wantErr)
}

func TestDMUnreadParticipantsReturnsInvalidUUIDError(t *testing.T) {
	db := openScriptedRowsDB(t, []string{"user_id"}, [][]driver.Value{{"not-a-uuid"}}, nil)
	hub := NewHub(db, nil)

	_, err := hub.dmUnreadParticipants(uuid.New())
	require.Error(t, err)
}

func TestDMUnreadParticipantsReturnsIterationError(t *testing.T) {
	wantErr := errors.New("participant iteration failed")
	db := openScriptedRowsDB(t, []string{"user_id"}, nil, wantErr)
	hub := NewHub(db, nil)

	_, err := hub.dmUnreadParticipants(uuid.New())
	require.ErrorIs(t, err, wantErr)
}

func TestSendVoiceCountsSnapshotSuppressesPartialScanResult(t *testing.T) {
	db := openScriptedRowsDB(t, []string{"server_id", "count"}, [][]driver.Value{{"server-1", "not-an-int"}}, nil)
	hub := NewHub(db, nil)
	client := newTestClient(hub, uuid.New())

	hub.sendVoiceCountsSnapshot(context.Background(), client)

	select {
	case msg := <-client.Send:
		t.Fatalf("voice-count snapshot sent after row scan failure: %s", msg)
	default:
	}
}
