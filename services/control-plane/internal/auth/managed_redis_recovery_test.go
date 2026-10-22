package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type failingCloseConn struct {
	net.Conn
	closed *atomic.Int32
}

func TestConsumeRecoveryCodePassesCallerContextToWatchClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	expected := errors.New("synthetic watch client failure")
	h := &Handler{recoveryWatchClient: func(got context.Context) (*redis.Client, error) {
		require.Equal(t, ctx, got)
		return nil, expected
	}}

	_, err := h.consumeRecoveryCode(ctx, "unused", "unused")
	require.ErrorIs(t, err, expected)
}

func (c failingCloseConn) Close() error {
	c.closed.Add(1)
	return errors.Join(c.Conn.Close(), errors.New("synthetic close failure"))
}

func TestConsumeRecoveryCodeKeepsCommittedRedemptionOnClientCloseFailure(t *testing.T) {
	ctx := context.Background()
	mini := miniredis.RunT(t)
	seed := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, seed.Close()) })
	key := recoveryRedisKey("managed-close@test.local")
	raw, err := json.Marshal(recoveryRecord{UserID: "test-user", CodeHash: hashRecoveryCode("123456")})
	require.NoError(t, err)
	require.NoError(t, seed.Set(ctx, key, raw, time.Minute).Err())
	var closes atomic.Int32
	var cleanupLog bytes.Buffer
	h := &Handler{
		redis: seed, log: logger.NewWithWriter(&cleanupLog),
		recoveryWatchClient: func(context.Context) (*redis.Client, error) {
			return redis.NewClient(&redis.Options{Addr: mini.Addr(), Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return failingCloseConn{Conn: conn, closed: &closes}, nil
			}}), nil
		},
	}

	record, err := h.consumeRecoveryCode(ctx, key, "123456")
	require.NoError(t, err, "client cleanup cannot invalidate a committed one-use redemption")
	require.Equal(t, "test-user", record.UserID)
	require.Positive(t, closes.Load(), "the attempt transport must be closed")
	require.Contains(t, cleanupLog.String(), "Recovery Redis attempt client cleanup failed", "the close failure must be observed")
	remaining, err := seed.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, remaining, "the committed redemption must remain consumed")
}

func TestConsumeRecoveryCodeUsesFreshClosedClientAfterWatchConflict(t *testing.T) {
	ctx := context.Background()
	mini := miniredis.RunT(t)
	seed := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, seed.Close()) })
	key := recoveryRedisKey("managed-retry@test.local")
	original, err := json.Marshal(recoveryRecord{UserID: "test-user", CodeHash: hashRecoveryCode("123456")})
	require.NoError(t, err)
	require.NoError(t, seed.Set(ctx, key, original, time.Minute).Err())
	replacement, err := json.Marshal(recoveryRecord{UserID: "test-user", CodeHash: hashRecoveryCode("654321")})
	require.NoError(t, err)

	var mu sync.Mutex
	var clients []*redis.Client
	factory := func(context.Context) (*redis.Client, error) {
		client := redis.NewClient(&redis.Options{Addr: mini.Addr(), MaxRetries: -1})
		mu.Lock()
		clients = append(clients, client)
		index := len(clients)
		mu.Unlock()
		if index == 1 {
			var once sync.Once
			var mutationErr error
			client.AddHook(recoveryCommitHook{beforeCommit: func() error {
				once.Do(func() { mutationErr = seed.Set(ctx, key, replacement, time.Minute).Err() })
				return mutationErr
			}})
		}
		return client, nil
	}
	h := &Handler{redis: seed, recoveryWatchClient: factory}

	got, err := h.consumeRecoveryCode(ctx, key, "654321")

	require.NoError(t, err)
	require.Equal(t, "test-user", got.UserID)
	require.Equal(t, 2, len(clients), "a WATCH conflict must retry with a newly constructed snapshot client")
	for i, client := range clients {
		_, pingErr := client.Ping(ctx).Result()
		require.Error(t, pingErr, "attempt client %d must be closed before consumeRecoveryCode returns", i)
	}
	remaining, err := seed.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, remaining, "successful single-use redemption deletes the recovery record")
}

func TestConsumeRecoveryCodeClosesSnapshotClientWhenCanceledDuringWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mini := miniredis.RunT(t)
	seed := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, seed.Close()) })
	key := recoveryRedisKey("managed-cancel@test.local")
	raw, err := json.Marshal(recoveryRecord{UserID: "test-user", CodeHash: hashRecoveryCode("123456")})
	require.NoError(t, err)
	require.NoError(t, seed.Set(context.Background(), key, raw, time.Minute).Err())

	enteredMulti := make(chan struct{})
	client := redis.NewClient(&redis.Options{Addr: mini.Addr(), MaxRetries: -1})
	client.AddHook(&waitForRecoveryCancelHook{entered: enteredMulti})
	h := &Handler{redis: seed, recoveryWatchClient: func(context.Context) (*redis.Client, error) { return client, nil }}
	done := make(chan error, 1)
	go func() {
		_, consumeErr := h.consumeRecoveryCode(ctx, key, "123456")
		done <- consumeErr
	}()

	select {
	case <-enteredMulti:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("recovery redemption did not reach its WATCH transaction")
	}
	select {
	case consumeErr := <-done:
		require.Error(t, consumeErr)
	case <-time.After(2 * time.Second):
		t.Fatal("canceled recovery redemption did not return")
	}
	_, pingErr := client.Ping(context.Background()).Result()
	require.Error(t, pingErr, "the per-attempt client must close on cancellation")
	remaining, err := seed.Get(context.Background(), key).Bytes()
	require.NoError(t, err)
	require.Equal(t, raw, remaining, "cancellation must not consume or mutate the recovery record")
}

type waitForRecoveryCancelHook struct {
	entered chan struct{}
	once    sync.Once
}

func (h *waitForRecoveryCancelHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *waitForRecoveryCancelHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}
func (h *waitForRecoveryCancelHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if strings.EqualFold(cmd.Name(), "multi") {
				h.once.Do(func() { close(h.entered) })
				<-ctx.Done()
				return ctx.Err()
			}
		}
		return next(ctx, cmds)
	}
}
