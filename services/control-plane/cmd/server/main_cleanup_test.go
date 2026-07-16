package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type resultWithRowsAffected struct {
	rows int64
	err  error
}

var _ sql.Result = resultWithRowsAffected{}

func (r resultWithRowsAffected) LastInsertId() (int64, error) {
	return 0, nil
}

func (r resultWithRowsAffected) RowsAffected() (int64, error) {
	return r.rows, r.err
}

func TestCheckedRowsAffectedReturnsCount(t *testing.T) {
	got, err := checkedRowsAffected(resultWithRowsAffected{rows: 2}, "update member role")
	require.NoError(t, err)
	assert.Equal(t, int64(2), got)
}

func TestCheckedRowsAffectedWrapsDriverError(t *testing.T) {
	wantErr := errors.New("driver rows-affected failure")
	_, err := checkedRowsAffected(resultWithRowsAffected{err: wantErr}, "update member role")
	require.ErrorIs(t, err, wantErr)
	assert.ErrorContains(t, err, "update member role")
}

func TestJoinCleanupErrorPreservesBothErrors(t *testing.T) {
	primaryErr := errors.New("transaction operation failed")
	cleanupErr := errors.New("rollback failed")

	err := joinCleanupError("rollback transfer transaction", primaryErr, cleanupErr)

	require.ErrorIs(t, err, primaryErr)
	require.ErrorIs(t, err, cleanupErr)
	assert.ErrorContains(t, err, "rollback transfer transaction")
}

func TestJoinCleanupErrorReturnsPrimaryWhenCleanupSucceeds(t *testing.T) {
	primaryErr := errors.New("transaction operation failed")
	assert.ErrorIs(t, joinCleanupError("rollback transfer transaction", primaryErr, nil), primaryErr)
}

type stubPresenceStore struct {
	keys      []string
	deleteErr error
}

func (s *stubPresenceStore) Scan(context.Context, uint64, string, int64) *redis.ScanCmd {
	return redis.NewScanCmdResult(s.keys, 0, nil)
}

func (s *stubPresenceStore) Del(context.Context, ...string) *redis.IntCmd {
	return redis.NewIntResult(1, s.deleteErr)
}

func TestCleanupStalePresenceCountsSuccessfulDeletes(t *testing.T) {
	store := &stubPresenceStore{keys: []string{"presence:" + uuid.NewString()}}
	var logs bytes.Buffer

	cleanupStalePresence(context.Background(), store, websocket.NewHub(nil, nil), logger.NewWithWriter(&logs))

	assert.Contains(t, logs.String(), "Cleanup: removed stale presence keys")
	assert.Contains(t, logs.String(), "count=1")
}

func TestCleanupStalePresenceDoesNotCountFailedDeletes(t *testing.T) {
	store := &stubPresenceStore{
		keys:      []string{"presence:" + uuid.NewString()},
		deleteErr: errors.New("redis DEL failed"),
	}
	var logs bytes.Buffer

	cleanupStalePresence(context.Background(), store, websocket.NewHub(nil, nil), logger.NewWithWriter(&logs))

	assert.Contains(t, logs.String(), "Cleanup: failed to delete stale presence key")
	assert.NotContains(t, logs.String(), "Cleanup: removed stale presence keys")
}
