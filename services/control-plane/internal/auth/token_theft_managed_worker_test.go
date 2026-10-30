package auth

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestStartTokenTheftRecoveryWorker_ManagedFactory(t *testing.T) {
	for _, stage := range []string{"startup", "periodic"} {
		t.Run(stage, func(t *testing.T) {
			db, cleanup := dbtest.SetupTestDB(t)
			t.Cleanup(cleanup)
			mini := miniredis.RunT(t)
			now := time.Now().UTC().Truncate(time.Second)
			scope := managedredis.SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
			reader := &theftCredentialReader{managedredis.Credential{Version: 1, Scope: scope, RotationID: "rotation-12", Service: "control-plane", Generation: 12, Username: "cv_cp_12", Password: "synthetic-password", ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour)}}
			mini.RequireUserAuth(reader.credential.Username, reader.credential.Password)
			runtime, err := managedredis.NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
			require.NoError(t, err)
			require.NoError(t, runtime.Refresh(t.Context()))
			shared, err := database.NewManagedRedisClient(t.Context(), mini.Addr(), nil, runtime)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, shared.Close()) })
			held := shared.Conn()
			require.NoError(t, held.Ping(t.Context()).Err())
			t.Cleanup(func() { require.NoError(t, held.Close()) })
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
			realFactory := database.NewRecoveryWatchClientFactory(runtime, mini.Addr(), nil)
			var owned []*redis.Client
			factory := func(ctx context.Context) (*redis.Client, error) {
				client, openErr := realFactory(ctx)
				if client != nil {
					mu.Lock()
					owned = append(owned, client)
					mu.Unlock()
				}
				return client, openErr
			}
			userID := dbtest.CreateUser(t, db)
			// Keep the authenticated managed pool and owned cleanup factory under
			// test; mint through production authority on that exact shared client.
			h := NewHandler(db, shared, logger.New("test"), familyTestSecret, nil)
			rig := familyTestRig{h: h, db: db, user: userID.String()}
			var family string
			seed := func() {
				access, refresh, _, mintErr := h.IssueAccessAndRefresh(t.Context(), userID.String())
				require.NoError(t, mintErr)
				require.NotEmpty(t, refresh)
				family, _ = assertFamilyAccess(t, rig, access)
				require.NoError(t, EnqueueTokenTheftRevocation(t.Context(), db, userID.String()))
			}
			if stage == "startup" {
				seed()
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			require.NoError(t, StartTokenTheftRecoveryWorker(ctx, db, shared, logger.New("test"), 10*time.Millisecond, factory))
			if stage == "periodic" {
				mu.Lock()
				startupClients := len(owned)
				mu.Unlock()
				require.Zero(t, startupClients, "empty startup must not acquire a cleanup client")
				seed()
			}
			if stage == "startup" {
				var pending int
				require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id=$1`, userID).Scan(&pending))
				require.Zero(t, pending, "startup recovery must finish before Start returns")
			}
			// Observe the durable completion, rather than sleeping for a nominal tick.
			require.Eventually(t, func() bool {
				var pending, active int
				if queryErr := db.QueryRow(`SELECT (SELECT COUNT(*) FROM token_theft_pending_revocations WHERE user_id=$1), (SELECT COUNT(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NULL)`, userID).Scan(&pending, &active); queryErr != nil {
					return false
				}
				return pending == 0 && active == 0
			}, 5*time.Second, 10*time.Millisecond, "worker must revoke the session and retire its pending row")
			var terminal bool
			var reason, revocationScope string
			require.NoError(t, db.QueryRow(`SELECT f.terminal_at IS NOT NULL,r.reason,r.scope
				FROM login_families f JOIN login_family_revocations r
				ON r.operation_id=f.auth_operation_id AND r.user_id=f.user_id
				WHERE f.family_id=$1 AND f.user_id=$2`, family, userID).Scan(&terminal, &reason, &revocationScope))
			require.True(t, terminal, "the captured managed session must be terminal before cleanup completes")
			require.Equal(t, "theft", reason)
			require.Equal(t, "user", revocationScope)
			cancel()
			require.False(t, mini.Exists(middleware.TokenTheftBlockedKey(userID.String())))
			mu.Lock()
			clients := append([]*redis.Client(nil), owned...)
			mutationPeer, ackPeer := scriptPeer, receiptPeer
			mu.Unlock()
			require.Len(t, clients, 1, "startup and periodic recovery must forward the managed factory")
			for _, client := range clients {
				require.ErrorIs(t, client.Ping(t.Context()).Err(), redis.ErrClosed, "each cleanup scope owns and closes its snapshot client")
			}
			require.NoError(t, shared.Ping(t.Context()).Err(), "worker must retain the shared streaming pool")
			require.NotNil(t, mutationPeer)
			require.Same(t, mutationPeer, ackPeer, "worker cleanup Lua and WAITAOF must use one socket")
		})
	}
}
