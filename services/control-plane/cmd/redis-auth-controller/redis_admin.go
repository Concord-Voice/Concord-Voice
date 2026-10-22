package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/redis/go-redis/v9"
)

var errAdmin = errors.New("managed Redis admin operation failed")

type redisAdmin struct {
	client    *redis.Client
	endpoint  string
	tlsConfig *tls.Config
}

func newRedisAdmin(endpoint string, tlsConfig *tls.Config, bootstrap managedredis.AdminBootstrap) (*redisAdmin, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" || port == "" || strings.ContainsAny(endpoint, "/@?\x00\r\n") || bootstrap.Username == "" || bootstrap.Password == "" {
		return nil, errAdmin
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return nil, errAdmin
	}
	options := &redis.Options{Addr: endpoint, Username: bootstrap.Username, Password: bootstrap.Password, ContextTimeoutEnabled: true, MaxRetries: 0}
	if tlsConfig != nil {
		options.TLSConfig = tlsConfig.Clone()
	}
	return &redisAdmin{client: redis.NewClient(options), endpoint: endpoint, tlsConfig: tlsConfig}, nil
}

func (a *redisAdmin) LoadACL(ctx context.Context, path string) error {
	if path != aclFile {
		return errAdmin
	}
	if err := a.client.Do(ctx, "ACL", "LOAD").Err(); err != nil {
		return errAdmin
	}
	return nil
}

func (a *redisAdmin) ListClients(ctx context.Context) ([]managedredis.ClientIdentity, error) {
	value, err := a.client.ClientList(ctx).Result()
	if err != nil {
		return nil, errAdmin
	}
	return parseClientList(value)
}

func parseClientList(raw string) ([]managedredis.ClientIdentity, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	clients := make([]managedredis.ClientIdentity, 0)
	seenIDs := make(map[string]struct{})
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		client, err := parseClientLine(line)
		if err != nil {
			return nil, errAdmin
		}
		if _, exists := seenIDs[client.ID]; exists {
			return nil, errAdmin
		}
		seenIDs[client.ID] = struct{}{}
		clients = append(clients, client)
	}
	return clients, nil
}

func parseClientLine(line string) (managedredis.ClientIdentity, error) {
	if line == "" {
		return managedredis.ClientIdentity{}, errAdmin
	}
	var client managedredis.ClientIdentity
	for _, field := range strings.Fields(line) {
		pair := strings.SplitN(field, "=", 2)
		if len(pair) != 2 {
			return managedredis.ClientIdentity{}, errAdmin
		}
		switch pair[0] {
		case "id":
			if client.ID != "" {
				return managedredis.ClientIdentity{}, errAdmin
			}
			client.ID = pair[1]
		case "user":
			if client.Username != "" {
				return managedredis.ClientIdentity{}, errAdmin
			}
			client.Username = pair[1]
		}
	}
	if client.ID == "" || client.Username == "" {
		return managedredis.ClientIdentity{}, errAdmin
	}
	if _, err := strconv.ParseUint(client.ID, 10, 64); err != nil {
		return managedredis.ClientIdentity{}, errAdmin
	}
	return client, nil
}

func (a *redisAdmin) ProbeAuth(ctx context.Context, username, password string) (retErr error) {
	options := &redis.Options{Addr: a.endpoint, Username: username, Password: password, ContextTimeoutEnabled: true, MaxRetries: 0}
	if a.tlsConfig != nil {
		options.TLSConfig = a.tlsConfig.Clone()
	}
	client := redis.NewClient(options)
	defer func() {
		if err := client.Close(); err != nil {
			retErr = errAdmin
		}
	}()
	if err := client.Ping(ctx).Err(); err != nil {
		return errAdmin
	}
	return nil
}

func (a *redisAdmin) UserExists(ctx context.Context, username string) (bool, error) {
	result, err := a.client.Do(ctx, "ACL", "GETUSER", username).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, errAdmin
	}
	return result != nil, nil
}

func (a *redisAdmin) KillUser(ctx context.Context, username string) error {
	if err := a.client.Do(ctx, "CLIENT", "KILL", "USER", username, "SKIPME", "YES").Err(); err != nil {
		return errAdmin
	}
	return nil
}

func (a *redisAdmin) ExecuteOperator(ctx context.Context, args []string) (string, error) {
	if len(args) == 3 {
		if args[0] == "--scan" {
			return a.scanOperator(ctx, args[len(args)-1])
		}
	}
	values := make([]any, len(args))
	for i, arg := range args {
		values[i] = arg
	}
	result, err := a.client.Do(ctx, values...).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", errAdmin
	}
	switch value := result.(type) {
	case string:
		return value, nil
	case int64:
		return fmt.Sprint(value), nil
	default:
		return "", errAdmin
	}
}

func (a *redisAdmin) scanOperator(ctx context.Context, pattern string) (string, error) {
	var cursor uint64
	var keys []string
	for rounds := 0; rounds < 128; rounds++ {
		batch, next, err := a.client.Scan(ctx, cursor, pattern, 128).Result()
		if err != nil {
			return "", errAdmin
		}
		keys = append(keys, batch...)
		if len(keys) > 4096 {
			return "", errAdmin
		}
		cursor = next
		if cursor == 0 {
			output := strings.Join(keys, "\n")
			if len(output) > 16*1024 {
				return "", errAdmin
			}
			return output, nil
		}
	}
	return "", errAdmin
}
