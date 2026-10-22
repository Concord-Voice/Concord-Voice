package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type managedRedisTestReader struct {
	value []byte
	name  string
	calls int
}

func (r *managedRedisTestReader) ReadExact(_ context.Context, _ managedredis.SecretScope) ([]byte, string, error) {
	r.calls++
	return append([]byte(nil), r.value...), r.name, nil
}

func managedRedisTestRuntime(t *testing.T) (*managedredis.Runtime, *managedRedisTestReader) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	scope := managedredis.SecretScope{Project: "project", Environment: "staging", Path: "/sync/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
	credential := managedredis.Credential{Version: 1, Scope: scope, RotationID: "rotation-7", Service: "control-plane", Generation: 7,
		Username: "cv_cp_7", Password: "synthetic-test-password", ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour)}
	raw, err := json.Marshal(credential) // #nosec G117 -- synthetic test credential, not a real secret
	require.NoError(t, err)
	reader := &managedRedisTestReader{value: raw, name: scope.Name}
	runtime, err := managedredis.NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	return runtime, reader
}

func TestInitializeManagedRedisWiresExactScopeAndReturnsCancellation(t *testing.T) {
	cfg := &config.Config{RedisEndpoint: "localhost:6379", RedisInfisicalURL: "https://infisical.example", RedisInfisicalClientID: "client",
		RedisInfisicalProjectID: "project", RedisInfisicalEnvironment: "staging", RedisInfisicalPath: "/sync/redis"}
	runtime, _ := managedRedisTestRuntime(t)
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	pollCalled := make(chan struct{}, 1)
	acknowledged := false
	got, gotRuntime, tlsConfig, cancel, err := initializeManagedRedisWith(cfg,
		func(baseURL, clientID, secretFile string, httpClient *http.Client) (*managedredis.InfisicalReader, error) {
			require.Equal(t, "https://infisical.example", baseURL)
			require.Equal(t, "client", clientID)
			require.Equal(t, managedredis.ClientSecretFile, secretFile)
			require.Nil(t, httpClient)
			return managedredis.NewInfisicalReader("https://infisical.example", "client", "synthetic-test-file", nil)
		},
		func(endpoint string, gotTLS *tls.Config, gotReader managedredis.ExactSecretReader, scope managedredis.SecretScope) (*redis.Client, *managedredis.Runtime, error) {
			require.Equal(t, "localhost:6379", endpoint)
			require.Nil(t, gotTLS)
			require.NotNil(t, gotReader)
			require.Equal(t, managedredis.SecretScope{Project: "project", Environment: "staging", Path: "/sync/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}, scope)
			return client, runtime, nil
		},
		func(ctx context.Context, gotRuntime *managedredis.Runtime, gotClient *redis.Client) error {
			require.Same(t, runtime, gotRuntime)
			require.Same(t, client, gotClient)
			_, hasDeadline := ctx.Deadline()
			require.True(t, hasDeadline)
			acknowledged = true
			return nil
		},
		func(_ context.Context, gotRuntime *managedredis.Runtime, gotClient *redis.Client) {
			require.True(t, acknowledged, "the first ACK must complete before polling starts")
			require.Same(t, runtime, gotRuntime)
			require.Same(t, client, gotClient)
			pollCalled <- struct{}{}
		})
	require.NoError(t, err)
	require.Same(t, client, got)
	require.Same(t, runtime, gotRuntime)
	require.Nil(t, tlsConfig)
	select {
	case <-pollCalled:
	case <-time.After(time.Second):
		t.Fatal("managed Redis poll was not started")
	}
	require.NotNil(t, cancel)
	cancel()
}

func TestInitializeManagedRedisFailsClosedAtSetupBoundaries(t *testing.T) {
	cfg := &config.Config{RedisEndpoint: "localhost:6379"}
	readerCalled, openCalled := false, false
	newReader := func(string, string, string, *http.Client) (*managedredis.InfisicalReader, error) {
		readerCalled = true
		return nil, errors.New("synthetic reader failure")
	}
	open := func(string, *tls.Config, managedredis.ExactSecretReader, managedredis.SecretScope) (*redis.Client, *managedredis.Runtime, error) {
		openCalled = true
		return nil, nil, errors.New("synthetic open failure")
	}
	poll := func(context.Context, *managedredis.Runtime, *redis.Client) {}
	ack := func(context.Context, *managedredis.Runtime, *redis.Client) error { return nil }

	cfg.RedisTLS = true
	cfg.RedisEndpoint = "localhost"
	_, _, _, _, err := initializeManagedRedisWith(cfg, newReader, open, ack, poll)
	require.Error(t, err)
	require.False(t, readerCalled)
	cfg.RedisTLS = false
	cfg.RedisEndpoint = "localhost:6379"

	_, _, _, _, err = initializeManagedRedisWith(cfg, newReader, open, ack, poll)
	require.Error(t, err)
	require.True(t, readerCalled)
	require.False(t, openCalled)

	newReader = func(string, string, string, *http.Client) (*managedredis.InfisicalReader, error) {
		return managedredis.NewInfisicalReader("https://infisical.example", "client", "synthetic-test-file", nil)
	}
	cfg.RedisInfisicalProjectID, cfg.RedisInfisicalEnvironment, cfg.RedisInfisicalPath = "project", "staging", "/sync/redis"
	_, _, _, _, err = initializeManagedRedisWith(cfg, newReader, open, ack, poll)
	require.Error(t, err)
	require.True(t, openCalled)
}

func TestInitializeManagedRedisRejectsMalformedTLSConfiguration(t *testing.T) {
	_, _, _, _, err := initializeManagedRedis(&config.Config{RedisEndpoint: "malformed", RedisTLS: true})
	require.Error(t, err)
}

func TestInitializeManagedRedisFailsStartupOnRejectedInitialAcknowledgment(t *testing.T) {
	cfg := &config.Config{RedisEndpoint: "localhost:6379", RedisInfisicalURL: "https://infisical.example", RedisInfisicalClientID: "client",
		RedisInfisicalProjectID: "project", RedisInfisicalEnvironment: "staging", RedisInfisicalPath: "/sync/redis"}
	runtime, _ := managedRedisTestRuntime(t)
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialerRetries: 1})
	pollCalled := false
	got, gotRuntime, _, cancel, err := initializeManagedRedisWith(cfg,
		func(string, string, string, *http.Client) (*managedredis.InfisicalReader, error) {
			return managedredis.NewInfisicalReader("https://infisical.example", "client", "synthetic-test-file", nil)
		},
		func(string, *tls.Config, managedredis.ExactSecretReader, managedredis.SecretScope) (*redis.Client, *managedredis.Runtime, error) {
			return client, runtime, nil
		},
		func(context.Context, *managedredis.Runtime, *redis.Client) error {
			return errors.New("synthetic controller refusal")
		},
		func(context.Context, *managedredis.Runtime, *redis.Client) { pollCalled = true })
	require.ErrorContains(t, err, "initial acknowledgment failed")
	require.Nil(t, got)
	require.Nil(t, gotRuntime)
	require.Nil(t, cancel)
	require.False(t, pollCalled, "a rejected initial ACK must prevent background polling")
}

func TestPollManagedRedisRefreshesBeforeWaitingAndAcknowledgesOnlyFreshState(t *testing.T) {
	runtime, reader := managedRedisTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acknowledged := false
	pollManagedRedisWith(ctx, runtime, nil, func(context.Context, *managedredis.Runtime, *redis.Client) error {
		acknowledged = true
		cancel()
		return nil
	}, 30*time.Second, func(kind string) {
		t.Errorf("successful poll reported %s failure", kind)
		cancel()
	})
	require.Equal(t, 1, reader.calls, "poll performs its first refresh immediately")
	require.True(t, acknowledged)

	badRuntime, badReader := managedRedisTestRuntime(t)
	badReader.name = "OTHER_SECRET"
	acknowledged = false
	reported := ""
	badCtx, badCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer badCancel()
	pollManagedRedisWith(badCtx, badRuntime, nil, func(context.Context, *managedredis.Runtime, *redis.Client) error {
		acknowledged = true
		badCancel()
		return nil
	}, 30*time.Second, func(kind string) { reported = kind; badCancel() })
	require.Equal(t, 1, badReader.calls)
	require.False(t, acknowledged, "a rejected refresh must never be acknowledged")
	require.Equal(t, "refresh", reported)
}

func TestPollManagedRedisRefreshesAfterTicker(t *testing.T) {
	runtime, reader := managedRedisTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ackCount := 0
	pollManagedRedisWith(ctx, runtime, nil, func(context.Context, *managedredis.Runtime, *redis.Client) error {
		ackCount++
		if ackCount == 2 {
			cancel()
		}
		return nil
	}, time.Millisecond, func(kind string) { t.Errorf("successful poll reported %s failure", kind) })
	require.Equal(t, 2, reader.calls)
	require.Equal(t, 2, ackCount)
}

func TestPollManagedRedisReportsRejectedAcknowledgment(t *testing.T) {
	runtime, reader := managedRedisTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	reported := ""
	pollManagedRedisWith(ctx, runtime, nil, func(context.Context, *managedredis.Runtime, *redis.Client) error {
		return errors.New("synthetic socket refusal")
	}, time.Second, func(kind string) { reported = kind; cancel() })
	require.Equal(t, 1, reader.calls)
	require.Equal(t, "acknowledgment", reported)
}

func TestPollManagedRedisWrapperStopsAfterCancellation(t *testing.T) {
	runtime, reader := managedRedisTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pollManagedRedis(ctx, runtime, nil)
	require.Equal(t, 1, reader.calls)
}

func TestAcknowledgeManagedRedisBindsAppliedGenerationToControllerSocket(t *testing.T) {
	runtime, _ := managedRedisTestRuntime(t)
	require.NoError(t, runtime.Refresh(context.Background()))
	socketDir, err := os.MkdirTemp("/tmp", "ra-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(socketDir)) })
	socketPath := filepath.Join(socketDir, "ack.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	type result struct {
		ack managedredis.AppliedAck
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			resultCh <- result{err: acceptErr}
			return
		}
		var ack managedredis.AppliedAck
		decodeErr := json.NewDecoder(conn).Decode(&ack)
		if decodeErr == nil {
			_, decodeErr = conn.Write([]byte("{\"ok\":true}\n"))
		}
		if closeErr := conn.Close(); decodeErr == nil {
			decodeErr = closeErr
		}
		resultCh <- result{ack: ack, err: decodeErr}
	}()
	err = acknowledgeManagedRedisUsing(context.Background(), runtime, func(context.Context) (string, error) { return "cv_cp_7", nil }, socketPath)
	require.NoError(t, err)
	got := <-resultCh
	require.NoError(t, got.err)
	require.Equal(t, 1, got.ack.Version)
	require.Equal(t, "rotation-7", got.ack.RotationID)
	require.Equal(t, "control-plane", got.ack.Service)
	require.NotEmpty(t, got.ack.InstanceID)
	require.Equal(t, uint64(7), got.ack.Generation)
}

func TestAcknowledgeManagedRedisRejectsWrongIdentityAndUnavailableSocket(t *testing.T) {
	runtime, _ := managedRedisTestRuntime(t)
	require.NoError(t, runtime.Refresh(context.Background()))
	whoAmI := func(context.Context) (string, error) { return "another-service", nil }
	err := acknowledgeManagedRedisUsing(context.Background(), runtime, whoAmI, filepath.Join(t.TempDir(), "missing.sock"))
	require.Error(t, err)
	whoAmI = func(context.Context) (string, error) { return "cv_cp_7", nil }
	err = acknowledgeManagedRedisUsing(context.Background(), runtime, whoAmI, filepath.Join(t.TempDir(), "missing.sock"))
	require.Error(t, err)

	coldRuntime, _ := managedRedisTestRuntime(t)
	err = acknowledgeManagedRedisUsing(context.Background(), coldRuntime, whoAmI, filepath.Join(t.TempDir(), "missing.sock"))
	require.Error(t, err)
}

func TestAcknowledgeManagedRedisRejectsCredentialThatStalesDuringWhoAmI(t *testing.T) {
	_, reader := managedRedisTestRuntime(t)
	now := time.Now().UTC().Truncate(time.Second)
	scope := managedredis.SecretScope{Project: "project", Environment: "staging", Path: "/sync/redis", Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
	runtime, err := managedredis.NewRuntime(reader, scope, "control-plane", func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, runtime.Refresh(context.Background()))
	socketDir, err := os.MkdirTemp("/tmp", "ra-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(socketDir)) })
	socketPath := filepath.Join(socketDir, "ack.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	seen := make(chan int, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			seen <- -1
			return
		}
		defer func() { _ = conn.Close() }()
		buffer := make([]byte, 1)
		n, _ := conn.Read(buffer)
		if n > 0 {
			_, _ = conn.Write([]byte("{\"ok\":true}\n"))
		}
		seen <- n
	}()
	err = acknowledgeManagedRedisUsing(context.Background(), runtime, func(context.Context) (string, error) {
		now = now.Add(121 * time.Second)
		return "cv_cp_7", nil
	}, socketPath)
	require.ErrorContains(t, err, "managed Redis primary identity not applied")
	require.Zero(t, <-seen, "an expired generation must not reach the ACK socket")
}

func TestAcknowledgeManagedRedisWrapperRejectsColdRuntime(t *testing.T) {
	runtime, _ := managedRedisTestRuntime(t)
	require.Error(t, acknowledgeManagedRedis(context.Background(), runtime, nil))
	require.NoError(t, runtime.Refresh(context.Background()))
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.Error(t, acknowledgeManagedRedis(context.Background(), runtime, client))
}

func TestAcknowledgeManagedRedisRejectsControllerNegativeReply(t *testing.T) {
	runtime, _ := managedRedisTestRuntime(t)
	require.NoError(t, runtime.Refresh(context.Background()))
	socketDir, err := os.MkdirTemp("/tmp", "ra-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(socketDir)) })
	socketPath := filepath.Join(socketDir, "ack.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	resultCh := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			resultCh <- acceptErr
			return
		}
		_, writeErr := conn.Write([]byte("{\"ok\":false}\n"))
		if closeErr := conn.Close(); writeErr == nil {
			writeErr = closeErr
		}
		resultCh <- writeErr
	}()
	err = acknowledgeManagedRedisUsing(context.Background(), runtime, func(context.Context) (string, error) { return "cv_cp_7", nil }, socketPath)
	require.Error(t, err)
	require.NoError(t, <-resultCh)
}
