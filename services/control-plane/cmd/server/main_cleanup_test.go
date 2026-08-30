package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

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
