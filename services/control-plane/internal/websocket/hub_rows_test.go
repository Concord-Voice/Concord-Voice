package websocket

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
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

type blockingChannelContextDriver struct {
	entered     chan struct{}
	release     chan struct{}
	hasDeadline chan bool
	once        sync.Once
}

func (d *blockingChannelContextDriver) Open(string) (driver.Conn, error) {
	return &blockingChannelContextConn{driver: d}, nil
}

type blockingChannelContextConn struct {
	driver *blockingChannelContextDriver
}

func (c *blockingChannelContextConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (c *blockingChannelContextConn) Close() error {
	return nil
}

func (c *blockingChannelContextConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}

func (c *blockingChannelContextConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	c.driver.once.Do(func() {
		close(c.driver.entered)
		_, hasDeadline := ctx.Deadline()
		c.driver.hasDeadline <- hasDeadline
	})
	select {
	case <-c.driver.release:
		return &scriptedRows{
			columns: []string{"allow_embedded_content", "server_id", "type"},
			values:  [][]driver.Value{{false, uuid.NewString(), "voice"}},
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var _ driver.QueryerContext = (*blockingChannelContextConn)(nil)

func openBlockingChannelContextDB(t *testing.T) (*sql.DB, *blockingChannelContextDriver) {
	t.Helper()
	driverName := "websocket-blocking-channel-context-" + uuid.NewString()
	driver := &blockingChannelContextDriver{
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
		hasDeadline: make(chan bool, 1),
	}
	sql.Register(driverName, driver)
	db, err := sql.Open(driverName, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db, driver
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

	_, _, err := hub.queryServerMemberships([]uuid.UUID{uuid.New()})
	require.Error(t, err)
}

func TestQueryServerMembershipsReturnsIterationError(t *testing.T) {
	wantErr := errors.New("membership iteration failed")
	db := openScriptedRowsDB(t, []string{"server_id", "user_id"}, nil, wantErr)
	hub := NewHub(db, nil)

	_, _, err := hub.queryServerMemberships([]uuid.UUID{uuid.New()})
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

func TestValidateReplyToIDReturnsErrorOnDatabaseFailure(t *testing.T) {
	wantErr := errors.New("reply lookup failed")
	db := openScriptedRowsDB(t, []string{"channel_id"}, nil, wantErr)
	hub := NewHub(db, nil)
	client := newTestClient(hub, uuid.New())
	hub.clients[client.ID] = client
	replyID := uuid.NewString()
	logs := captureHubLog(t)

	validated, ok := hub.validateReplyToID(IncomingMessage{
		ClientID: client.ID,
		Data: map[string]interface{}{
			"reply_to_id": replyID,
		},
	}, uuid.NewString())

	require.False(t, ok)
	require.Nil(t, validated)
	require.Contains(t, logs.String(), "Failed to validate reply_to_id "+replyID)
	response := readClientMsg(t, client)
	require.Equal(t, "Failed to validate reply target", response["data"].(map[string]interface{})[keyMessage])
}

func TestHandleProfileUpdateLeavesCacheUnchangedOnDatabaseFailure(t *testing.T) {
	wantErr := errors.New("profile lookup failed")
	db := openScriptedRowsDB(t, []string{"username", "display_name", "avatar_url"}, nil, wantErr)
	hub := NewHub(db, nil)
	userID := uuid.New()
	client := newTestClient(hub, userID)
	client.Username = "before-error"
	hub.clients[client.ID] = client
	hub.userClients[userID] = map[uuid.UUID]bool{client.ID: true}
	logs := captureHubLog(t)

	hub.handleProfileUpdate(IncomingMessage{ClientID: client.ID, UserID: userID})

	require.Equal(t, "before-error", client.Username)
	require.Contains(t, logs.String(), "Failed to refresh user info for "+userID.String())
}
