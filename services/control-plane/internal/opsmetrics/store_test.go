package opsmetrics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ MetricStore = (*PostgresStore)(nil)

func TestNewPostgresStoreValidatesDependencies(t *testing.T) {
	db := sql.OpenDB(&storeTestConnector{connection: &storeTestConnection{}})
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	store, err := NewPostgresStore(db, "cvn_aaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	assert.NotNil(t, store)

	_, err = NewPostgresStore(nil, "cvn_aaaaaaaaaaaaaaaa")
	require.ErrorContains(t, err, "database")
	_, err = NewPostgresStore(db, "api.concordvoice.chat")
	require.ErrorContains(t, err, "node id")
}

func TestPostgresStoreWriteSamplesCommitsPreparedBatch(t *testing.T) {
	connection := &storeTestConnection{}
	store, cleanup := newStoreTestPostgresStore(t, connection)
	defer cleanup()

	err := store.WriteSamples(context.Background(), []Sample{
		{Key: MetricHostCPUPercent, Value: 25, Source: SourceHost},
		{Key: MetricHostMemoryPercent, Value: 50, Source: SourceHost},
	})
	require.NoError(t, err)

	connection.mu.Lock()
	defer connection.mu.Unlock()
	assert.True(t, connection.began)
	assert.True(t, connection.committed)
	assert.False(t, connection.rolledBack)
	assert.Equal(t, 1, connection.prepareCount)
	assert.Equal(t, 2, connection.execCount)
	assert.True(t, connection.statementClosed)
	assert.Contains(t, connection.preparedQuery, "INSERT INTO ops_metric_samples")
}

func TestPostgresStoreWriteSamplesRollsBackFailedInsert(t *testing.T) {
	connection := &storeTestConnection{failExecAt: 2}
	store, cleanup := newStoreTestPostgresStore(t, connection)
	defer cleanup()

	err := store.WriteSamples(context.Background(), []Sample{
		{Key: MetricHostCPUPercent, Value: 25, Source: SourceHost},
		{Key: MetricHostMemoryPercent, Value: 50, Source: SourceHost},
	})
	require.ErrorContains(t, err, "write metric sample")

	connection.mu.Lock()
	defer connection.mu.Unlock()
	assert.True(t, connection.began)
	assert.False(t, connection.committed)
	assert.True(t, connection.rolledBack)
	assert.Equal(t, 2, connection.execCount)
	assert.True(t, connection.statementClosed)
}

func TestPostgresStoreWriteSamplesRejectsInvalidBatchBeforeTransaction(t *testing.T) {
	connection := &storeTestConnection{}
	store, cleanup := newStoreTestPostgresStore(t, connection)
	defer cleanup()

	testCases := []struct {
		name    string
		samples []Sample
	}{
		{
			name: "unknown key",
			samples: []Sample{
				{Key: "custom_metric", Value: 1, Source: SourceHost},
			},
		},
		{
			name: "duplicate key",
			samples: []Sample{
				{Key: MetricHostCPUPercent, Value: 1, Source: SourceHost},
				{Key: MetricHostCPUPercent, Value: 2, Source: SourceHost},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := store.WriteSamples(context.Background(), testCase.samples)
			require.Error(t, err)
		})
	}

	connection.mu.Lock()
	defer connection.mu.Unlock()
	assert.False(t, connection.began)
}

func TestPostgresStoreWriteSamplesEmptyBatchIsNoOp(t *testing.T) {
	connection := &storeTestConnection{}
	store, cleanup := newStoreTestPostgresStore(t, connection)
	defer cleanup()

	require.NoError(t, store.WriteSamples(context.Background(), nil))

	connection.mu.Lock()
	defer connection.mu.Unlock()
	assert.False(t, connection.began)
}

func TestRollupSQLUsesUTCHoursAndPreservesFinalizedBuckets(t *testing.T) {
	assert.Contains(t, rollupSQL, "date_trunc('hour', ts, 'UTC')")
	assert.Contains(t, rollupSQL, "ARRAY_AGG(value ORDER BY ts DESC)")
	assert.Contains(t, rollupSQL, "WHERE ts < $1")
	assert.Contains(t, rollupSQL, "ON CONFLICT (node_id, metric_key, bucket_start) DO NOTHING")
	assert.NotContains(t, rollupSQL, "DO UPDATE")
	assert.NotContains(t, strings.ToUpper(rollupSQL), "DELETE")

	cutoff := completedHour(time.Date(2026, 7, 12, 20, 37, 15, 0, time.FixedZone("EDT", -4*60*60)))
	assert.Equal(t, time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC), cutoff)
}

func TestPruneSQLIsParameterizedAndAtomic(t *testing.T) {
	assert.Contains(t, pruneRawSQL, "ts < $1")
	assert.Contains(t, pruneRollupSQL, "bucket_start < $1")
	assert.NotContains(t, pruneRawSQL, "%")
	assert.NotContains(t, pruneRollupSQL, "%")
}

func TestPostgresStorePruneUsesExactRawRetentionCutoff(t *testing.T) {
	connection := &storeTestConnection{}
	store, cleanup := newStoreTestPostgresStore(t, connection)
	defer cleanup()

	rawBefore := time.Date(2026, 7, 12, 11, 37, 15, 0, time.UTC)
	require.NoError(t, store.prune(
		context.Background(),
		rawBefore,
		time.Date(2026, 7, 4, 11, 0, 0, 0, time.UTC),
	))

	connection.mu.Lock()
	defer connection.mu.Unlock()
	require.Len(t, connection.execArgs, 2)
	assert.Equal(t, rawBefore, connection.execArgs[0][0].Value)
}

func TestPostgresStoreMaintainStopsBeforePruneWhenRollupFails(t *testing.T) {
	connection := &storeTestConnection{failExecAt: 1}
	store, cleanup := newStoreTestPostgresStore(t, connection)
	defer cleanup()

	err := store.Maintain(context.Background(), time.Date(2026, 7, 12, 20, 37, 0, 0, time.UTC))
	require.ErrorContains(t, err, "roll up operations metrics")

	connection.mu.Lock()
	defer connection.mu.Unlock()
	require.Len(t, connection.execQueries, 1)
	assert.Contains(t, connection.execQueries[0], "INSERT INTO ops_metric_rollups")
}

func TestPostgresStoreMaintainRollsUpBeforePruning(t *testing.T) {
	connection := &storeTestConnection{}
	store, cleanup := newStoreTestPostgresStore(t, connection)
	defer cleanup()

	require.NoError(t, store.Maintain(
		context.Background(),
		time.Date(2026, 7, 12, 20, 37, 0, 0, time.UTC),
	))

	connection.mu.Lock()
	defer connection.mu.Unlock()
	require.Len(t, connection.execQueries, 3)
	assert.Contains(t, connection.execQueries[0], "INSERT INTO ops_metric_rollups")
	assert.Contains(t, connection.execQueries[1], "DELETE FROM ops_metric_samples")
	assert.Contains(t, connection.execQueries[2], "DELETE FROM ops_metric_rollups")
}

func TestPostgresStoreTransactionsRollbackOnUnexpectedPanic(t *testing.T) {
	t.Run("write samples", func(t *testing.T) {
		connection := &storeTestConnection{panicPrepare: true}
		store, cleanup := newStoreTestPostgresStore(t, connection)
		defer cleanup()

		assert.Panics(t, func() {
			_ = store.WriteSamples(context.Background(), []Sample{
				{Key: MetricHostCPUPercent, Value: 25, Source: SourceHost},
			})
		})

		connection.mu.Lock()
		defer connection.mu.Unlock()
		assert.True(t, connection.rolledBack)
	})

	t.Run("rollup", func(t *testing.T) {
		connection := &storeTestConnection{panicExecAt: 1}
		store, cleanup := newStoreTestPostgresStore(t, connection)
		defer cleanup()

		assert.Panics(t, func() {
			_ = store.rollup(context.Background(), time.Now())
		})

		connection.mu.Lock()
		defer connection.mu.Unlock()
		assert.True(t, connection.rolledBack)
	})

	t.Run("prune", func(t *testing.T) {
		connection := &storeTestConnection{panicExecAt: 1}
		store, cleanup := newStoreTestPostgresStore(t, connection)
		defer cleanup()

		assert.Panics(t, func() {
			_ = store.prune(context.Background(), time.Now(), time.Now())
		})

		connection.mu.Lock()
		defer connection.mu.Unlock()
		assert.True(t, connection.rolledBack)
	})
}

func newStoreTestPostgresStore(t *testing.T, connection *storeTestConnection) (*PostgresStore, func()) {
	t.Helper()

	db := sql.OpenDB(&storeTestConnector{connection: connection})
	db.SetMaxOpenConns(1)
	store, err := NewPostgresStore(db, "cvn_aaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	store.now = func() time.Time { return time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC) }
	return store, func() { require.NoError(t, db.Close()) }
}

type storeTestConnector struct {
	connection *storeTestConnection
}

func (connector *storeTestConnector) Connect(context.Context) (driver.Conn, error) {
	return connector.connection, nil
}

func (connector *storeTestConnector) Driver() driver.Driver { return storeTestDriver{} }

type storeTestDriver struct{}

func (storeTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("store test driver requires Connector")
}

type storeTestConnection struct {
	mu              sync.Mutex
	began           bool
	committed       bool
	rolledBack      bool
	closed          bool
	statementClosed bool
	prepareCount    int
	execCount       int
	failExecAt      int
	panicExecAt     int
	panicPrepare    bool
	preparedQuery   string
	execArgs        [][]driver.NamedValue
	execQueries     []string
}

func (connection *storeTestConnection) Prepare(query string) (driver.Stmt, error) {
	return connection.PrepareContext(context.Background(), query)
}

func (connection *storeTestConnection) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.prepareCount++
	connection.preparedQuery = query
	if connection.panicPrepare {
		panic("forced prepare panic")
	}
	return &storeTestStatement{connection: connection}, nil
}

func (connection *storeTestConnection) Close() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.closed = true
	return nil
}

func (connection *storeTestConnection) Begin() (driver.Tx, error) {
	return connection.BeginTx(context.Background(), driver.TxOptions{})
}

func (connection *storeTestConnection) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.began = true
	return &storeTestTransaction{connection: connection}, nil
}

func (connection *storeTestConnection) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.execCount++
	connection.execArgs = append(connection.execArgs, append([]driver.NamedValue(nil), args...))
	connection.execQueries = append(connection.execQueries, query)
	if connection.panicExecAt == connection.execCount {
		panic("forced execution panic")
	}
	if connection.failExecAt == connection.execCount {
		return nil, errors.New("forced execution failure")
	}
	return driver.RowsAffected(1), nil
}

type storeTestStatement struct {
	connection *storeTestConnection
}

func (statement *storeTestStatement) Close() error {
	statement.connection.mu.Lock()
	defer statement.connection.mu.Unlock()
	statement.connection.statementClosed = true
	return nil
}

func (*storeTestStatement) NumInput() int { return 4 }

func (statement *storeTestStatement) Exec([]driver.Value) (driver.Result, error) {
	statement.connection.mu.Lock()
	defer statement.connection.mu.Unlock()
	statement.connection.execCount++
	if statement.connection.failExecAt == statement.connection.execCount {
		return nil, errors.New("forced insert failure")
	}
	return driver.RowsAffected(1), nil
}

func (*storeTestStatement) Query([]driver.Value) (driver.Rows, error) {
	return nil, io.EOF
}

type storeTestTransaction struct {
	connection *storeTestConnection
}

func (transaction *storeTestTransaction) Commit() error {
	transaction.connection.mu.Lock()
	defer transaction.connection.mu.Unlock()
	transaction.connection.committed = true
	return nil
}

func (transaction *storeTestTransaction) Rollback() error {
	transaction.connection.mu.Lock()
	defer transaction.connection.mu.Unlock()
	transaction.connection.rolledBack = true
	return nil
}
