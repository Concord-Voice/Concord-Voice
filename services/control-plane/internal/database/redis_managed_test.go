package database

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestNewRedisClientMalformedURLDoesNotExposeURLCredentials(t *testing.T) {
	url := "redis://user:synthetic-url-password@%zz" // pragma: allowlist secret -- malformed test URL
	_, err := NewRedisClient(url)

	require.Error(t, err)
	require.NotContains(t, err.Error(), "synthetic-url-password")
	require.NotContains(t, err.Error(), url)
}

func TestNewRedisClientClosesConnectionWhenInitialPingFails(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	closed := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			closed <- acceptErr
			return
		}
		defer func() {
			if closeErr := conn.Close(); closeErr != nil {
				t.Errorf("close fake Redis connection: %v", closeErr)
			}
		}()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		reader := bufio.NewReader(conn)
		for {
			command, readErr := readRedisTestCommand(reader)
			if readErr != nil {
				closed <- readErr
				return
			}
			if len(command) > 0 && strings.EqualFold(command[0], "PING") {
				if _, writeErr := fmt.Fprint(conn, "-ERR synthetic-password probe detail\r\n"); writeErr != nil {
					closed <- writeErr
					return
				}
				break
			}
			reply := "+OK\r\n"
			if len(command) > 0 && strings.EqualFold(command[0], "HELLO") {
				reply = "%7\r\n+server\r\n+redis\r\n+version\r\n+7.0.0\r\n+proto\r\n:2\r\n+id\r\n:1\r\n+mode\r\n+standalone\r\n+role\r\n+master\r\n+modules\r\n*0\r\n"
			}
			if _, writeErr := fmt.Fprint(conn, reply); writeErr != nil {
				closed <- writeErr
				return
			}
		}
		for {
			_, readErr := reader.ReadByte()
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed) || errors.Is(readErr, context.Canceled) {
				closed <- nil
				return
			}
			if readErr != nil {
				if errors.Is(readErr, context.DeadlineExceeded) || strings.Contains(readErr.Error(), "timeout") {
					closed <- errors.New("client connection remained open after failed initial PING")
					return
				}
				closed <- readErr
				return
			}
		}
	}()

	_, err = NewRedisClient("redis://" + listener.Addr().String() + "?protocol=2&max_retries=0")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "synthetic-password")
	select {
	case closeErr := <-closed:
		require.NoError(t, closeErr)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for failed-PING connection to close")
	}
}

func readRedisTestCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 4 || line[0] != '*' {
		return nil, errors.New("expected RESP array")
	}
	count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil || count < 1 || count > 32 {
		return nil, errors.New("invalid RESP array size")
	}
	args := make([]string, 0, count)
	for range count {
		lengthLine, readErr := reader.ReadString('\n')
		if readErr != nil {
			return nil, readErr
		}
		if len(lengthLine) < 4 || lengthLine[0] != '$' {
			return nil, errors.New("expected RESP bulk string")
		}
		length, parseErr := strconv.Atoi(strings.TrimSpace(lengthLine[1:]))
		if parseErr != nil || length < 0 || length > 1024 {
			return nil, errors.New("invalid RESP bulk string size")
		}
		payload := make([]byte, length+2)
		if _, readErr = io.ReadFull(reader, payload); readErr != nil {
			return nil, readErr
		}
		args = append(args, string(payload[:length]))
	}
	return args, nil
}

func TestNewFreshnessHookRejectsStaleOrdinaryCommand(t *testing.T) {
	mini := miniredis.RunT(t)
	now := time.Now().UTC()
	runtime := managedRedisTestRuntime(t, &now)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	client.AddHook(NewFreshnessHook(runtime))
	require.NoError(t, client.Ping(context.Background()).Err())

	// Freshness is bounded by the injected runtime clock, so the hook can be
	// tested at the exact expiry boundary without a timer or wall-clock sleep.
	now = now.Add(time.Hour)
	require.Error(t, client.Ping(context.Background()).Err())
}

func TestNewFreshnessHookRejectsStalePipeline(t *testing.T) {
	mini := miniredis.RunT(t)
	now := time.Now().UTC()
	runtime := managedRedisTestRuntime(t, &now)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	client.AddHook(NewFreshnessHook(runtime))
	require.NoError(t, client.Ping(context.Background()).Err())

	now = now.Add(time.Hour)
	pipe := client.Pipeline()
	pipe.Ping(context.Background())
	_, err := pipe.Exec(context.Background())
	require.Error(t, err)
}

func TestFreshnessHookRejectsConnectionThatStalesDuringDial(t *testing.T) {
	now := time.Now().UTC()
	runtime := managedRedisTestRuntime(t, &now)
	var peer net.Conn
	dial := NewFreshnessHook(runtime).DialHook(func(context.Context, string, string) (net.Conn, error) {
		client, other := net.Pipe()
		peer = other
		now = now.Add(time.Hour)
		return client, nil
	})
	conn, err := dial(context.Background(), "tcp", "127.0.0.1:6379")
	require.Nil(t, conn)
	require.ErrorContains(t, err, "stale")
	t.Cleanup(func() { require.NoError(t, peer.Close()) })
	_, err = peer.Write([]byte("x"))
	require.ErrorIs(t, err, io.ErrClosedPipe)

	called := false
	dial = NewFreshnessHook(runtime).DialHook(func(context.Context, string, string) (net.Conn, error) {
		called = true
		return nil, errors.New("unexpected dial")
	})
	conn, err = dial(context.Background(), "tcp", "127.0.0.1:6379")
	require.Nil(t, conn)
	require.ErrorContains(t, err, "stale")
	require.False(t, called)
}

func TestNewManagedRedisClientRejectsCredentialBearingEndpoint(t *testing.T) {
	now := time.Now().UTC()
	runtime := managedRedisTestRuntime(t, &now)
	_, err := NewManagedRedisClient(context.Background(), "redis://user:synthetic-endpoint-password@127.0.0.1:6379", nil, runtime)

	require.Error(t, err)
	require.NotContains(t, err.Error(), "synthetic-endpoint-password")
}

func TestNewRecoveryWatchClientUsesSnapshotCredentialForSingleUseWatch(t *testing.T) {
	mini := miniredis.RunT(t)
	now := time.Now().UTC()
	mini.RequireUserAuth("cv_cp_12", "synthetic-password")
	runtime := managedRedisTestRuntime(t, &now)
	snapshot, err := runtime.Snapshot(now)
	require.NoError(t, err)
	client, err := NewRecoveryWatchClient(context.Background(), mini.Addr(), nil, snapshot, runtime)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()
	key := "recovery_code:managed-watch-test"
	require.NoError(t, client.Set(ctx, key, "single-use", time.Minute).Err())
	consumed := false
	err = client.Watch(ctx, func(tx *redis.Tx) error {
		value, getErr := tx.Get(ctx, key).Result()
		if getErr != nil {
			return getErr
		}
		if value != "single-use" {
			return errors.New("unexpected recovery value")
		}
		_, execErr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			return pipe.Del(ctx, key).Err()
		})
		if execErr == nil {
			consumed = true
		}
		return execErr
	}, key)
	require.NoError(t, err)
	require.True(t, consumed)
	exists, err := client.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
}

type rotatingManagedRedisReader struct{ credential managedredis.Credential }

func (r *rotatingManagedRedisReader) ReadExact(_ context.Context, scope managedredis.SecretScope) ([]byte, string, error) {
	credential := r.credential
	raw, err := json.Marshal(map[string]any{
		"version": credential.Version, "scope": credential.Scope,
		"rotationId": credential.RotationID, "service": credential.Service,
		"generation": credential.Generation, "username": credential.Username,
		"password": credential.Password, "validFrom": credential.ValidFrom,
		"validUntil": credential.ValidUntil,
	})
	return raw, scope.Name, err
}

func TestRecoveryWatchClientRejectsSupersededSnapshot(t *testing.T) {
	mini := miniredis.RunT(t)
	now := time.Now().UTC().Truncate(time.Second)
	initial := managedRedisTestCredential(now)
	mini.RequireUserAuth(initial.Username, initial.Password)
	reader := &rotatingManagedRedisReader{credential: initial}
	runtime, err := managedredis.NewRuntime(reader, initial.Scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))
	snapshot, err := runtime.Snapshot(now)
	require.NoError(t, err)
	client, err := NewRecoveryWatchClient(context.Background(), mini.Addr(), nil, snapshot, runtime)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	ctx := context.Background()
	require.NoError(t, client.Set(ctx, "recovery-watch-snapshot", "value", time.Minute).Err())

	next := initial
	next.Generation = 13
	next.Username = "cv_cp_13"
	next.Password = "synthetic-next-password" // pragma: allowlist secret -- synthetic rotation credential
	next.RotationID = "rotation-13"
	reader.credential = next
	require.NoError(t, runtime.Refresh(ctx))
	require.ErrorContains(t, client.Get(ctx, "recovery-watch-snapshot").Err(), "stale")
	pipe := client.Pipeline()
	pipe.Get(ctx, "recovery-watch-snapshot")
	_, err = pipe.Exec(ctx)
	require.ErrorContains(t, err, "stale")
	stale, err := NewRecoveryWatchClient(ctx, mini.Addr(), nil, snapshot, runtime)
	require.Error(t, err)
	require.Nil(t, stale)
}

func TestNewRecoveryWatchClientHonorsCallerCancellation(t *testing.T) {
	mini := miniredis.RunT(t)
	now := time.Now().UTC()
	mini.RequireUserAuth("cv_cp_12", "synthetic-password")
	runtime := managedRedisTestRuntime(t, &now)
	snapshot, err := runtime.Snapshot(now)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client, err := NewRecoveryWatchClient(ctx, mini.Addr(), nil, snapshot, runtime)
	require.Error(t, err)
	require.Nil(t, client)
}

func TestManagedRedisTLSConfigValidatesAndLoadsCA(t *testing.T) {
	t.Run("disabled without CA", func(t *testing.T) {
		config, err := ManagedRedisTLSConfig("redis.example:6379", false, "")
		require.NoError(t, err)
		require.Nil(t, config)
	})
	t.Run("rejects CA when disabled", func(t *testing.T) {
		_, err := ManagedRedisTLSConfig("redis.example:6379", false, "ca.pem")
		require.Error(t, err)
	})
	t.Run("rejects malformed endpoint", func(t *testing.T) {
		_, err := ManagedRedisTLSConfig("redis.example", true, "")
		require.Error(t, err)
	})
	t.Run("rejects unavailable and invalid CA", func(t *testing.T) {
		dir := t.TempDir()
		_, err := ManagedRedisTLSConfig("redis.example:6379", true, filepath.Join(dir, "missing.pem"))
		require.Error(t, err)
		badCA := filepath.Join(dir, "bad.pem")
		require.NoError(t, os.WriteFile(badCA, []byte("not a certificate"), 0o600))
		_, err = ManagedRedisTLSConfig("redis.example:6379", true, badCA)
		require.Error(t, err)
	})
	t.Run("loads a CA and sets server name", func(t *testing.T) {
		publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "redis.example"}, IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
		der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), "ca.pem")
		require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
		config, err := ManagedRedisTLSConfig("redis.example:6379", true, path)
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS12), config.MinVersion)
		require.Equal(t, "redis.example", config.ServerName)
		require.NotNil(t, config.RootCAs)
	})
}

func TestManagedRedisAdaptersProbeAndOpenScopedCredential(t *testing.T) {
	now := time.Now().UTC()
	credential := managedRedisTestCredential(now)
	mini := miniredis.RunT(t)
	mini.RequireUserAuth(credential.Username, credential.Password)
	require.NoError(t, mini.Server().Register("ACL", func(peer *server.Peer, _ string, args []string) {
		if len(args) == 1 && strings.EqualFold(args[0], "WHOAMI") {
			peer.WriteBulk(credential.Username)
			return
		}
		peer.WriteError("ERR unsupported ACL command")
	}))
	ctx := context.Background()

	t.Run("candidate probe authenticates", func(t *testing.T) {
		require.Error(t, ProbeManagedRedisCandidate(ctx, "malformed", nil, credential))
		require.NoError(t, ProbeManagedRedisCandidate(ctx, mini.Addr(), nil, credential))
		wrong := credential
		wrong.Password = "wrong-password" // pragma: allowlist secret -- synthetic wrong credential for rejected-auth test
		require.Error(t, ProbeManagedRedisCandidate(ctx, mini.Addr(), nil, wrong))
	})
	t.Run("candidate probe rejects another applied identity", func(t *testing.T) {
		other := miniredis.RunT(t)
		other.RequireUserAuth(credential.Username, credential.Password)
		require.NoError(t, other.Server().Register("ACL", func(peer *server.Peer, _ string, args []string) {
			if len(args) == 1 && strings.EqualFold(args[0], "WHOAMI") {
				peer.WriteBulk("cv_cp_11")
				return
			}
			peer.WriteError("ERR unsupported ACL command")
		}))
		require.Error(t, ProbeManagedRedisCandidate(ctx, other.Addr(), nil, credential))
	})

	t.Run("open proves the credential and returns a usable client", func(t *testing.T) {
		reader := managedRedisTestReader{scope: credential.Scope, now: &now}
		client, runtime, err := OpenManagedRedis(mini.Addr(), nil, reader, credential.Scope)
		require.NoError(t, err)
		require.NoError(t, client.Ping(ctx).Err())
		require.NoError(t, client.Close())
		require.NoError(t, runtime.Fresh(now))
	})
	t.Run("open rejects malformed endpoint and unavailable secret", func(t *testing.T) {
		reader := managedRedisTestReader{scope: credential.Scope, now: &now}
		client, runtime, err := OpenManagedRedis("malformed", nil, reader, credential.Scope)
		require.Error(t, err)
		require.Nil(t, client)
		require.Nil(t, runtime)
		readerFailed := failingManagedRedisReader{}
		client, runtime, err = OpenManagedRedis(mini.Addr(), nil, readerFailed, credential.Scope)
		require.Error(t, err)
		require.Nil(t, client)
		require.Nil(t, runtime)
	})
}

func TestNewManagedRedisClientRequiresFreshRuntimeAndConnects(t *testing.T) {
	_, err := NewManagedRedisClient(context.Background(), "127.0.0.1:6379", nil, nil)
	require.Error(t, err)
	now := time.Now().UTC()
	_, err = NewManagedRedisClient(context.Background(), "bad-endpoint", nil, managedRedisTestRuntime(t, &now))
	require.Error(t, err)
	mini := miniredis.RunT(t)
	mini.RequireUserAuth("cv_cp_12", "synthetic-password")
	runtime := managedRedisTestRuntime(t, &now)
	client, err := NewManagedRedisClient(context.Background(), mini.Addr(), nil, runtime)
	require.NoError(t, err)
	require.NoError(t, client.Ping(context.Background()).Err())
	require.NoError(t, client.Close())
	// Expiration prevents a client from opening even when its endpoint is reachable.
	now = now.Add(time.Hour)
	_, err = NewManagedRedisClient(context.Background(), mini.Addr(), nil, runtime)
	require.Error(t, err)
}

func TestNewManagedRedisClientHonorsExhaustedStartupContext(t *testing.T) {
	mini := miniredis.RunT(t)
	mini.RequireUserAuth("cv_cp_12", "synthetic-password")
	now := time.Now().UTC()
	runtime := managedRedisTestRuntime(t, &now)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, runtime.Refresh(ctx))
	cancel() // The startup budget expires between refresh and the client PING.

	client, err := NewManagedRedisClient(ctx, mini.Addr(), nil, runtime)
	require.Nil(t, client)
	require.ErrorContains(t, err, "managed Redis initial auth failed")
}

type failingManagedRedisReader struct{}

func (failingManagedRedisReader) ReadExact(context.Context, managedredis.SecretScope) ([]byte, string, error) {
	return nil, "", errors.New("unavailable")
}

func managedRedisTestRuntime(t *testing.T, now *time.Time) *managedredis.Runtime {
	t.Helper()
	scope := managedredis.SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
	reader := managedRedisTestReader{scope: scope, now: now}
	runtime, err := managedredis.NewRuntime(reader, scope, "control-plane", func() time.Time { return *now })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))
	return runtime
}

func managedRedisTestCredential(now time.Time) managedredis.Credential {
	scope := managedredis.SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
	return managedredis.Credential{
		Version: 1, Scope: scope, RotationID: "rotation-12", Service: "control-plane", Generation: 12,
		Username: "cv_cp_12", Password: "synthetic-password", ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour),
	}
}

type managedRedisTestReader struct {
	scope managedredis.SecretScope
	now   *time.Time
}

func (r managedRedisTestReader) ReadExact(context.Context, managedredis.SecretScope) ([]byte, string, error) {
	return []byte(fmt.Sprintf(`{"version":1,"scope":{"projectId":%q,"environment":%q,"path":%q,"name":%q},"rotationId":"rotation-12","service":"control-plane","generation":12,"username":"cv_cp_12","password":"synthetic-password","validFrom":%q,"validUntil":%q}`,
		r.scope.Project, r.scope.Environment, r.scope.Path, r.scope.Name,
		(*r.now).Add(-time.Minute).Format(time.RFC3339), (*r.now).Add(time.Hour).Format(time.RFC3339))), r.scope.Name, nil
}
