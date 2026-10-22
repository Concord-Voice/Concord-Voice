package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/redis/go-redis/v9"
)

const redisControlAckSocket = "/run/redis-auth/ack-control.sock"

func initializeManagedRedis(cfg *config.Config) (*redis.Client, *managedredis.Runtime, *tls.Config, func(), error) {
	return initializeManagedRedisWith(cfg, managedredis.NewInfisicalReader, database.OpenManagedRedis, acknowledgeManagedRedis, pollManagedRedis)
}

func initializeManagedRedisWith(
	cfg *config.Config,
	newReader func(string, string, string, *http.Client) (*managedredis.InfisicalReader, error),
	open func(string, *tls.Config, managedredis.ExactSecretReader, managedredis.SecretScope) (*redis.Client, *managedredis.Runtime, error),
	acknowledge func(context.Context, *managedredis.Runtime, *redis.Client) error,
	poll func(context.Context, *managedredis.Runtime, *redis.Client),
) (*redis.Client, *managedredis.Runtime, *tls.Config, func(), error) {
	tlsConfig, err := database.ManagedRedisTLSConfig(cfg.RedisEndpoint, cfg.RedisTLS, cfg.RedisTLSCAFile)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	reader, err := newReader(cfg.RedisInfisicalURL, cfg.RedisInfisicalClientID, managedredis.ClientSecretFile, nil)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	scope := managedredis.SecretScope{Project: cfg.RedisInfisicalProjectID, Environment: cfg.RedisInfisicalEnvironment,
		Path: cfg.RedisInfisicalPath, Name: "REDIS_CONTROL_PLANE_CREDENTIAL"}
	client, runtime, err := open(cfg.RedisEndpoint, tlsConfig, reader, scope)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	ackCtx, ackCancel := context.WithTimeout(context.Background(), 5*time.Second)
	ackErr := acknowledge(ackCtx, runtime, client)
	ackCancel()
	if ackErr != nil {
		if err := client.Close(); err != nil {
			return nil, nil, nil, nil, errors.New("managed Redis initial acknowledgment cleanup failed")
		}
		return nil, nil, nil, nil, errors.New("managed Redis initial acknowledgment failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go poll(ctx, runtime, client)
	return client, runtime, tlsConfig, cancel, nil
}

func pollManagedRedis(ctx context.Context, runtime *managedredis.Runtime, client *redis.Client) {
	pollManagedRedisWith(ctx, runtime, client, acknowledgeManagedRedis, 30*time.Second, func(kind string) {
		log.Printf("managed Redis %s failed", kind)
	})
}

func pollManagedRedisWith(ctx context.Context, runtime *managedredis.Runtime, client *redis.Client, acknowledge func(context.Context, *managedredis.Runtime, *redis.Client) error, interval time.Duration, reportFailure func(string)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		refreshCtx, refreshCancel := context.WithTimeout(ctx, 26*time.Second)
		err := runtime.Refresh(refreshCtx)
		refreshCancel()
		if err != nil {
			reportFailure("refresh")
		} else {
			ackCtx, ackCancel := context.WithTimeout(ctx, 5*time.Second)
			if err := acknowledge(ackCtx, runtime, client); err != nil {
				reportFailure("acknowledgment")
			}
			ackCancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func acknowledgeManagedRedis(ctx context.Context, runtime *managedredis.Runtime, client *redis.Client) (retErr error) {
	return acknowledgeManagedRedisUsing(ctx, runtime, func(ctx context.Context) (string, error) {
		return client.ACLWhoAmI(ctx).Result()
	}, redisControlAckSocket)
}

func acknowledgeManagedRedisUsing(ctx context.Context, runtime *managedredis.Runtime, whoAmI func(context.Context) (string, error), socketPath string) (retErr error) {
	credential, err := runtime.Snapshot(runtime.Now())
	if err != nil {
		return err
	}
	// This command uses the ordinary pooled client after publishing the new
	// generation. The controller also requires old-user CLIENT LIST to reach zero.
	who, err := whoAmI(ctx)
	if err != nil || who != credential.Username {
		return errors.New("managed Redis primary identity not applied")
	}
	instance, err := os.Hostname()
	if err != nil || instance == "" {
		return errors.New("managed Redis instance identity unavailable")
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return errors.New("managed Redis acknowledgment unavailable")
	}
	defer func() {
		if err := conn.Close(); err != nil {
			retErr = errors.New("managed Redis acknowledgment cleanup failed")
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return errors.New("managed Redis acknowledgment deadline unavailable")
	}
	current, err := runtime.Snapshot(runtime.Now())
	if err != nil || current != credential {
		return errors.New("managed Redis primary identity not applied")
	}
	request := managedredis.AppliedAck{Version: 1, RotationID: credential.RotationID, Service: "control-plane",
		InstanceID: instance, Generation: credential.Generation, Username: who}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return errors.New("managed Redis acknowledgment send failed")
	}
	reader := bufio.NewReaderSize(conn, 1024)
	line, err := reader.ReadBytes('\n')
	if err != nil || len(line) > 1024 {
		return errors.New("managed Redis acknowledgment reply invalid")
	}
	var response struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(line, &response); err != nil || !response.OK {
		return errors.New("managed Redis acknowledgment rejected")
	}
	return nil
}
