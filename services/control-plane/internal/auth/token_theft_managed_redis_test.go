package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type theftCredentialReader struct{ credential managedredis.Credential }

func (r *theftCredentialReader) ReadExact(context.Context, managedredis.SecretScope) ([]byte, string, error) {
	raw, err := json.Marshal(map[string]any{
		"version": r.credential.Version, "scope": r.credential.Scope,
		"rotationId": r.credential.RotationID, "service": r.credential.Service,
		"generation": r.credential.Generation, "username": r.credential.Username,
		"password": r.credential.Password, "validFrom": r.credential.ValidFrom,
		"validUntil": r.credential.ValidUntil,
	})
	return raw, r.credential.Scope.Name, err
}

func TestTokenTheftRecovery_ManagedColdConnection(t *testing.T) {
	for _, action := range []string{"queue", "cleanup", "retry"} {
		for _, mode := range []string{"managed", "snapshot_control", "expired_snapshot", "superseded_snapshot", "expired_factory"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				mini := miniredis.RunT(t)
				now := time.Now().UTC().Truncate(time.Second)
				scope := managedredis.SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
				reader := &theftCredentialReader{managedredis.Credential{Version: 1, Scope: scope, RotationID: "rotation-12", Service: "control-plane", Generation: 12, Username: "cv_cp_12", Password: "synthetic-password", ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour)}}
				mini.RequireUserAuth(reader.credential.Username, reader.credential.Password)
				runtime, err := managedredis.NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
				require.NoError(t, err)
				require.NoError(t, runtime.Refresh(ctx))
				client, err := database.NewManagedRedisClient(ctx, mini.Addr(), nil, runtime)
				require.NoError(t, err)
				if mode != "managed" && mode != "expired_factory" {
					require.NoError(t, client.Close())
					snapshot, snapshotErr := runtime.Snapshot(now)
					require.NoError(t, snapshotErr)
					client, err = database.NewRecoveryWatchClient(ctx, mini.Addr(), nil, snapshot, runtime)
					require.NoError(t, err)
				}
				t.Cleanup(func() { require.NoError(t, client.Close()) })
				userID := "test-theft-user"
				blockedKey := middleware.TokenTheftBlockedKey(userID)
				mini.HSet(tokenTheftFallbackHashKey, userID, "test-operation")
				require.NoError(t, mini.Set(blockedKey, "test-operation"))
				// Reserve the constructor's warm socket so the production operation must
				// initialize another one, the path hidden by ordinary sequential tests.
				held := client.Conn()
				require.NoError(t, held.Ping(ctx).Err())
				t.Cleanup(func() { require.NoError(t, held.Close()) })
				h := &Handler{redis: client}
				var factory []func(context.Context) (*redis.Client, error)
				var ownedClients []*redis.Client
				if mode == "managed" || mode == "expired_factory" {
					h.SetManagedRedisRecovery(runtime, mini.Addr(), nil)
					realFactory := h.recoveryWatchClient
					h.recoveryWatchClient = func(ctx context.Context) (*redis.Client, error) {
						owned, openErr := realFactory(ctx)
						if owned != nil {
							ownedClients = append(ownedClients, owned)
						}
						return owned, openErr
					}
					factory = append(factory, h.recoveryWatchClient)
				}
				var mu sync.Mutex
				var scriptPeer, receiptPeer *server.Peer
				mini.Server().SetPreHook(func(peer *server.Peer, command string, _ ...string) bool {
					mu.Lock()
					defer mu.Unlock()
					switch strings.ToLower(command) {
					case "eval":
						scriptPeer = peer
					case "waitaof":
						receiptPeer = peer
						peer.WriteLen(2)
						peer.WriteInt(1)
						peer.WriteInt(0)
						return true
					}
					return false
				})
				switch mode {
				case "expired_snapshot", "expired_factory":
					now = now.Add(managedredis.MaxCredentialAge)
				case "superseded_snapshot":
					reader.credential.Generation++
					reader.credential.Username = "cv_cp_13"
					reader.credential.RotationID = "rotation-13"
					require.NoError(t, runtime.Refresh(ctx))
				}
				rejected := mode == "expired_snapshot" || mode == "superseded_snapshot" || mode == "expired_factory"
				// A closed handle reliably exercises the fallback without contacting SQL.
				db, err := sql.Open("postgres", "")
				require.NoError(t, err)
				require.NoError(t, db.Close())
				h.db = db
				require.NotPanics(t, func() {
					switch action {
					case "queue":
						queued, queueErr := queueTokenTheftRevocation(ctx, db, client, userID, factory...)
						if rejected {
							require.Error(t, queueErr)
						} else {
							require.NoError(t, queueErr)
						}
						require.False(t, queued)
					case "retry":
						done, retryErr := h.retryTokenTheftRecoveryOnce(ctx, userID, true)
						require.Equal(t, !rejected, done)
						if rejected {
							require.Error(t, retryErr)
						} else {
							require.NoError(t, retryErr)
						}
					case "cleanup":
						clearErr := clearTokenTheftRecoveryState(ctx, client, userID, sql.NullString{String: "test-operation", Valid: true}, sql.NullString{}, factory...)
						if mode == "expired_factory" {
							require.ErrorContains(t, clearErr, "credential unavailable")
						} else if rejected {
							require.ErrorContains(t, clearErr, "stale")
						} else {
							require.NoError(t, clearErr)
						}
					}
				}, "managed Redis cold sockets must not abort theft recovery")
				for _, owned := range ownedClients {
					require.ErrorIs(t, owned.Ping(ctx).Err(), redis.ErrClosed, "snapshot scope must close its owned client")
				}
				if mode == "managed" {
					require.Len(t, ownedClients, 1)
					require.NoError(t, client.Ping(ctx).Err(), "shared streaming client remains usable")
				}
				mu.Lock()
				defer mu.Unlock()
				if rejected {
					require.Nil(t, scriptPeer, "stale snapshot must not execute the mutation")
					require.Nil(t, receiptPeer)
					require.Equal(t, "test-operation", mini.HGet(tokenTheftFallbackHashKey, userID))
					require.True(t, mini.Exists(blockedKey))
					return
				}
				require.NotNil(t, scriptPeer, "the production Lua mutation reached Redis")
				require.Same(t, scriptPeer, receiptPeer, "WAITAOF must acknowledge the socket that executed Lua")
				if action == "queue" || action == "retry" {
					operation := mini.HGet(tokenTheftFallbackHashKey, userID)
					require.NotEqual(t, "test-operation", operation)
					blocked, getErr := mini.Get(blockedKey)
					require.NoError(t, getErr)
					require.Equal(t, operation, blocked)
				} else {
					require.False(t, mini.Exists(blockedKey))
					require.Empty(t, mini.HGet(tokenTheftFallbackHashKey, userID))
				}
			})
		}
	}
}

func TestTokenTheftRecovery_RejectsUnavailableFactory(t *testing.T) {
	for _, factoryErr := range []error{nil, errors.New("synthetic factory unavailable")} {
		t.Run(map[bool]string{true: "factory_error", false: "nil_client"}[factoryErr != nil], func(t *testing.T) {
			mini := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			userID := "test-theft-user"
			blockedKey := middleware.TokenTheftBlockedKey(userID)
			mini.HSet(tokenTheftFallbackHashKey, userID, "test-operation")
			require.NoError(t, mini.Set(blockedKey, "test-operation"))
			factory := func(context.Context) (*redis.Client, error) { return nil, factoryErr }
			err := clearTokenTheftRecoveryState(context.Background(), client, userID, sql.NullString{String: "test-operation", Valid: true}, sql.NullString{}, factory)
			require.Error(t, err)
			if factoryErr != nil {
				require.ErrorIs(t, err, factoryErr)
			}
			require.Equal(t, "test-operation", mini.HGet(tokenTheftFallbackHashKey, userID))
			require.True(t, mini.Exists(blockedKey))
			require.NoError(t, client.Ping(context.Background()).Err())
		})
	}
}

func TestTokenTheftRecovery_RejectsCredentialChangeBeforeReceipt(t *testing.T) {
	for _, change := range []string{"unchanged_control", "expired", "superseded", "slow_receipt"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			mini := miniredis.RunT(t)
			initial := time.Now().UTC().Truncate(time.Second)
			var clock atomic.Int64
			clock.Store(initial.Unix())
			now := func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
			scope := managedredis.SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
			reader := &theftCredentialReader{managedredis.Credential{Version: 1, Scope: scope, RotationID: "rotation-12", Service: "control-plane", Generation: 12, Username: "cv_cp_12", Password: "synthetic-password", ValidFrom: initial.Add(-time.Minute), ValidUntil: initial.Add(time.Hour)}}
			mini.RequireUserAuth(reader.credential.Username, reader.credential.Password)
			runtime, err := managedredis.NewRuntime(reader, scope, "control-plane", now)
			require.NoError(t, err)
			require.NoError(t, runtime.Refresh(ctx))
			shared := redis.NewClient(&redis.Options{Addr: mini.Addr()})
			t.Cleanup(func() { require.NoError(t, shared.Close()) })
			factory := database.NewRecoveryWatchClientFactory(runtime, mini.Addr(), nil)
			var scriptReached, receiptReached atomic.Bool
			changed := make(chan error, 1)
			mini.Server().SetPreHook(func(peer *server.Peer, command string, _ ...string) bool {
				switch strings.ToLower(command) {
				case "eval":
					scriptReached.Store(true)
					var rotateErr error
					// Change authority only after this operation has passed the script's
					// freshness check, so the receipt must perform its own check.
					switch change {
					case "expired":
						clock.Store(initial.Add(managedredis.MaxCredentialAge).Unix())
					case "superseded":
						reader.credential.Generation++
						reader.credential.Username = "cv_cp_13"
						reader.credential.RotationID = "rotation-13"
						rotateErr = runtime.Refresh(ctx)
					}
					changed <- rotateErr
				case "waitaof":
					receiptReached.Store(true)
					if change == "slow_receipt" {
						return true
					}
					peer.WriteLen(2)
					peer.WriteInt(1)
					peer.WriteInt(0)
					return true
				}
				return false
			})
			userID, operationID := "test-theft-user", "test-operation"
			operationCtx := ctx
			if change == "slow_receipt" {
				var cancel context.CancelFunc
				operationCtx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			started := time.Now()
			err = persistTokenTheftRecoveryState(operationCtx, shared, userID, operationID, factory)
			elapsed := time.Since(started)
			require.True(t, scriptReached.Load(), "the mutation must run before authority changes")
			require.NoError(t, <-changed, "the fixture must publish the superseding credential")
			require.Equal(t, operationID, mini.HGet(tokenTheftFallbackHashKey, userID))
			blocked, getErr := mini.Get(middleware.TokenTheftBlockedKey(userID))
			require.NoError(t, getErr)
			require.Equal(t, operationID, blocked, "an unacknowledged operation retains the deny marker")
			switch change {
			case "unchanged_control":
				require.NoError(t, err)
				require.True(t, receiptReached.Load(), "fresh authority must allow the real receipt command")
			case "slow_receipt":
				require.Error(t, err)
				var timeout net.Error
				require.True(t, errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()), "slow receipt must fail with a timeout: %v", err)
				require.True(t, receiptReached.Load(), "the fixture must stall at the receipt, after Lua succeeded")
				require.Less(t, elapsed, time.Second, "caller deadline must bound warm-socket reads")
			default:
				require.ErrorContains(t, err, "stale", "receipt must fail closed when its credential changes")
				require.False(t, receiptReached.Load(), "stale snapshot must be rejected before WAITAOF")
			}
		})
	}
}
