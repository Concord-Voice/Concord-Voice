package database

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/redis/go-redis/v9"
)

// NewRedisClient creates a new Redis client
func NewRedisClient(redisURL string) (*redis.Client, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL")
	}
	opt.ContextTimeoutEnabled = true

	client := redis.NewClient(opt)

	// Test the connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		if client.Close() != nil {
			return nil, fmt.Errorf("redis connection cleanup failed")
		}
		return nil, fmt.Errorf("redis connection failed")
	}

	return client, nil
}

func managedOptions(endpoint string, tlsConfig *tls.Config) (*redis.Options, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" || port == "" || strings.ContainsAny(endpoint, "/@?\x00\r\n") {
		return nil, fmt.Errorf("invalid managed Redis endpoint")
	}
	options := &redis.Options{Addr: endpoint, ContextTimeoutEnabled: true, MaxRetries: 0}
	if tlsConfig != nil {
		options.TLSConfig = tlsConfig.Clone()
	}
	return options, nil
}

// ManagedRedisTLSConfig builds TLS settings for a validated managed endpoint.
func ManagedRedisTLSConfig(endpoint string, enabled bool, caFile string) (result *tls.Config, retErr error) {
	if !enabled {
		if caFile != "" {
			return nil, fmt.Errorf("managed Redis TLS configuration invalid")
		}
		return nil, nil
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" {
		return nil, fmt.Errorf("managed Redis TLS configuration invalid")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if caFile != "" {
		root, err := os.OpenRoot(filepath.Dir(caFile))
		if err != nil {
			return nil, fmt.Errorf("managed Redis TLS CA unavailable")
		}
		defer func() {
			if err := root.Close(); err != nil {
				result, retErr = nil, fmt.Errorf("managed Redis TLS CA cleanup failed")
			}
		}()
		file, err := root.Open(filepath.Base(caFile))
		if err != nil {
			return nil, fmt.Errorf("managed Redis TLS CA unavailable")
		}
		defer func() {
			if err := file.Close(); err != nil {
				result, retErr = nil, fmt.Errorf("managed Redis TLS CA cleanup failed")
			}
		}()
		pem, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
		if err != nil || len(pem) == 0 || len(pem) > 1024*1024 {
			return nil, fmt.Errorf("managed Redis TLS CA unavailable")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("managed Redis TLS CA invalid")
		}
		config.RootCAs = pool
	}
	return config, nil
}

// NewManagedRedisClient opens a shared client guarded by runtime freshness.
func NewManagedRedisClient(ctx context.Context, endpoint string, tlsConfig *tls.Config, runtime *managedredis.Runtime) (*redis.Client, error) {
	if runtime == nil {
		return nil, fmt.Errorf("managed Redis runtime unavailable")
	}
	options, err := managedOptions(endpoint, tlsConfig)
	if err != nil {
		return nil, err
	}
	if err := runtime.Fresh(runtime.Now()); err != nil {
		return nil, fmt.Errorf("managed Redis credential stale")
	}
	options.StreamingCredentialsProvider = runtime
	client := redis.NewClient(options)
	client.AddHook(NewFreshnessHook(runtime))
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		if client.Close() != nil {
			return nil, fmt.Errorf("managed Redis initial cleanup failed")
		}
		return nil, fmt.Errorf("managed Redis initial auth failed")
	}
	return client, nil
}

// ProbeManagedRedisCandidate proves fresh AUTH/PING and the applied identity before Runtime publishes a
// generation. It never uses an existing pooled socket or streaming callback.
func ProbeManagedRedisCandidate(ctx context.Context, endpoint string, tlsConfig *tls.Config, candidate managedredis.Credential) (retErr error) {
	options, err := managedOptions(endpoint, tlsConfig)
	if err != nil {
		return err
	}
	options.Username, options.Password = candidate.Username, candidate.Password // pragma: allowlist secret -- runtime candidate
	client := redis.NewClient(options)
	defer func() {
		if err := client.Close(); err != nil {
			retErr = fmt.Errorf("managed Redis candidate cleanup failed")
		}
	}()
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(probeCtx).Err(); err != nil {
		return fmt.Errorf("managed Redis candidate auth failed")
	}
	username, err := client.ACLWhoAmI(probeCtx).Result()
	if err != nil || username != candidate.Username {
		return fmt.Errorf("managed Redis candidate identity mismatch")
	}
	return nil
}

// OpenManagedRedis validates and proves the first scoped credential before
// constructing the shared streaming client. Callers own polling and shutdown.
func OpenManagedRedis(endpoint string, tlsConfig *tls.Config, reader managedredis.ExactSecretReader, scope managedredis.SecretScope) (*redis.Client, *managedredis.Runtime, error) {
	runtime, err := managedredis.NewRuntime(reader, scope, "control-plane", time.Now)
	if err != nil {
		return nil, nil, fmt.Errorf("managed Redis runtime invalid")
	}
	runtime.SetCandidateProbe(func(ctx context.Context, candidate managedredis.Credential) error {
		return ProbeManagedRedisCandidate(ctx, endpoint, tlsConfig, candidate)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 26*time.Second)
	defer cancel()
	if err := runtime.Refresh(ctx); err != nil {
		return nil, nil, fmt.Errorf("managed Redis initial credential unavailable")
	}
	client, err := NewManagedRedisClient(ctx, endpoint, tlsConfig, runtime)
	if err != nil {
		return nil, nil, err
	}
	return client, runtime, nil
}

// NewRecoveryWatchClient uses an immutable non-streaming credential because
// go-redis 9.23.0's sticky WATCH path does not support the streaming manager.
func NewRecoveryWatchClient(ctx context.Context, endpoint string, tlsConfig *tls.Config, snapshot managedredis.Credential, runtime *managedredis.Runtime) (*redis.Client, error) {
	if runtime == nil || snapshot.Generation == 0 || snapshot.Username == "" || snapshot.Password == "" {
		return nil, fmt.Errorf("managed Redis recovery credential unavailable")
	}
	current, err := runtime.Snapshot(runtime.Now())
	if err != nil || current != snapshot {
		return nil, fmt.Errorf("managed Redis recovery credential unavailable")
	}
	options, err := managedOptions(endpoint, tlsConfig)
	if err != nil {
		return nil, err
	}
	options.Username, options.Password = snapshot.Username, snapshot.Password // pragma: allowlist secret -- runtime snapshot
	client := redis.NewClient(options)
	client.AddHook(freshnessHook{runtime: runtime, snapshot: snapshot})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		if client.Close() != nil {
			return nil, fmt.Errorf("managed Redis recovery cleanup failed")
		}
		return nil, fmt.Errorf("managed Redis recovery auth failed")
	}
	return client, nil
}

type freshnessHook struct {
	runtime  *managedredis.Runtime
	snapshot managedredis.Credential
}

// NewFreshnessHook rejects commands and pipelines after credentials expire.
func NewFreshnessHook(runtime *managedredis.Runtime) redis.Hook {
	return freshnessHook{runtime: runtime}
}

func (h freshnessHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !h.fresh() {
			return nil, fmt.Errorf("managed Redis credential stale")
		}
		conn, err := next(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if !h.fresh() {
			if err := conn.Close(); err != nil {
				return nil, fmt.Errorf("managed Redis stale connection cleanup failed")
			}
			return nil, fmt.Errorf("managed Redis credential stale")
		}
		return conn, nil
	}
}

func (h freshnessHook) fresh() bool {
	if h.runtime == nil {
		return false
	}
	if h.snapshot.Generation == 0 {
		return h.runtime.Fresh(h.runtime.Now()) == nil
	}
	current, err := h.runtime.Snapshot(h.runtime.Now())
	return err == nil && current == h.snapshot
}

func (h freshnessHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if !h.fresh() {
			return fmt.Errorf("managed Redis credential stale")
		}
		return next(ctx, cmd)
	}
}

func (h freshnessHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if !h.fresh() {
			return fmt.Errorf("managed Redis credential stale")
		}
		return next(ctx, cmds)
	}
}
