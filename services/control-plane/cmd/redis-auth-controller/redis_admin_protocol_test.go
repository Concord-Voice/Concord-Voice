package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type redisRESPFixture struct {
	listener        net.Listener
	mu              sync.Mutex
	commands        [][]string
	scanReply       string
	clientListReply string
	forcedReplies   map[string]string
	closeErr        error
	wg              sync.WaitGroup
}

func startRedisRESPFixture(t *testing.T) *redisRESPFixture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	fixture := &redisRESPFixture{listener: listener}
	fixture.wg.Add(1)
	t.Cleanup(func() {
		closeErr := listener.Close()
		require.NoError(t, closeErr)
		fixture.wg.Wait()
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		require.NoError(t, fixture.closeErr)
	})
	go fixture.accept()
	return fixture
}

func (f *redisRESPFixture) accept() {
	defer f.wg.Done()
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go f.serveConn(conn)
	}
}

func (f *redisRESPFixture) serveConn(conn net.Conn) {
	defer func() {
		if err := conn.Close(); err != nil {
			f.mu.Lock()
			f.closeErr = err
			f.mu.Unlock()
		}
		f.wg.Done()
	}()
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if len(line) < 3 || line[0] != '*' {
			return
		}
		count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil || count < 1 || count > 32 {
			return
		}
		args := make([]string, count)
		for i := range args {
			lengthLine, err := reader.ReadString('\n')
			if err != nil || len(lengthLine) < 3 || lengthLine[0] != '$' {
				return
			}
			length, err := strconv.Atoi(strings.TrimSpace(lengthLine[1:]))
			if err != nil || length < 0 || length > 4096 {
				return
			}
			value := make([]byte, length+2)
			if _, err := io.ReadFull(reader, value); err != nil {
				return
			}
			args[i] = string(value[:length])
		}
		f.mu.Lock()
		f.commands = append(f.commands, append([]string(nil), args...))
		scanReply := f.scanReply
		clientListReply := f.clientListReply
		command := strings.ToUpper(args[0])
		if len(args) > 1 && command != "SCAN" {
			command += " " + strings.ToUpper(args[1])
		}
		forcedReply, forced := f.forcedReplies[command]
		f.mu.Unlock()
		if forced {
			_, _ = fmt.Fprint(conn, forcedReply)
			continue
		}
		switch {
		case len(args) >= 2 && strings.EqualFold(args[0], "HELLO"):
			_, _ = fmt.Fprint(conn, "%2\r\n+proto\r\n:2\r\n+id\r\n:1\r\n")
		case len(args) == 3 && strings.EqualFold(args[0], "ACL") && strings.EqualFold(args[1], "GETUSER"):
			if args[2] == "missing-user" {
				_, _ = fmt.Fprint(conn, "*-1\r\n")
			} else {
				_, _ = fmt.Fprint(conn, "*0\r\n")
			}
		case len(args) >= 4 && strings.EqualFold(args[0], "CLIENT") && strings.EqualFold(args[1], "KILL"):
			_, _ = fmt.Fprint(conn, ":2\r\n")
		case len(args) >= 2 && strings.EqualFold(args[0], "CLIENT") && strings.EqualFold(args[1], "LIST"):
			_, _ = fmt.Fprint(conn, clientListReply)
		case len(args) >= 1 && strings.EqualFold(args[0], "SCAN"):
			_, _ = fmt.Fprint(conn, scanReply)
		default:
			_, _ = fmt.Fprint(conn, "+OK\r\n")
		}
	}
}

func (f *redisRESPFixture) setScanReply(reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scanReply = reply
}

func (f *redisRESPFixture) setClientListReply(reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clientListReply = reply
}

func (f *redisRESPFixture) setForcedReply(command, reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forcedReplies == nil {
		f.forcedReplies = make(map[string]string)
	}
	f.forcedReplies[command] = reply
}

func scanReply(keys []string) string {
	var reply strings.Builder
	fmt.Fprintf(&reply, "*2\r\n$1\r\n0\r\n*%d\r\n", len(keys))
	for _, key := range keys {
		fmt.Fprintf(&reply, "$%d\r\n%s\r\n", len(key), key)
	}
	return reply.String()
}

func (f *redisRESPFixture) commandSnapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.commands...)
}

func TestRedisAdminMapsACLGetUserMissingResultAndKillAffectedResponse(t *testing.T) {
	fixture := startRedisRESPFixture(t)
	client := redis.NewClient(&redis.Options{Addr: fixture.listener.Addr().String(), Protocol: 2, MaxRetries: 0})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	admin := &redisAdmin{client: client}

	present, err := admin.UserExists(context.Background(), "missing-user")
	require.NoError(t, err)
	require.False(t, present)
	present, err = admin.UserExists(context.Background(), "cv_cp_11")
	require.NoError(t, err)
	require.True(t, present)
	require.NoError(t, admin.KillUser(context.Background(), "cv_cp_11"))

	commands := fixture.commandSnapshot()
	var sawMissing, sawPresent, sawKill bool
	for _, args := range commands {
		if len(args) == 3 && strings.EqualFold(args[0], "ACL") && strings.EqualFold(args[1], "GETUSER") {
			sawMissing = sawMissing || args[2] == "missing-user"
			sawPresent = sawPresent || args[2] == "cv_cp_11"
		}
		if len(args) == 6 && strings.EqualFold(args[0], "CLIENT") && strings.EqualFold(args[1], "KILL") {
			sawKill = strings.EqualFold(args[2], "USER") && args[3] == "cv_cp_11" && strings.EqualFold(args[4], "SKIPME") && strings.EqualFold(args[5], "YES")
		}
	}
	require.True(t, sawMissing, "ACL GETUSER must be used to prove the removed user is absent")
	require.True(t, sawPresent)
	require.True(t, sawKill, "CLIENT KILL must target only the exact old username")
}

func TestRedisAdminScanRejectsExcessKeysAndOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		want string
	}{
		{name: "bounded result", keys: []string{"voice:room_owner:a"}, want: "voice:room_owner:a"},
		{name: "too many keys", keys: make([]string, 4097)},
		{name: "too many output bytes", keys: []string{strings.Repeat("a", 9000), strings.Repeat("b", 9000)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := startRedisRESPFixture(t)
			fixture.setScanReply(scanReply(tc.keys))
			client := redis.NewClient(&redis.Options{Addr: fixture.listener.Addr().String(), Protocol: 2, MaxRetries: 0})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			admin := &redisAdmin{client: client}
			output, err := admin.ExecuteOperator(context.Background(), []string{"--scan", "--pattern", "voice:room_owner:*"})
			if tc.want == "" {
				require.ErrorIs(t, err, errAdmin)
				require.Empty(t, output)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, output)
			}
		})
	}
}

func TestRedisAdminUsesScopedBootstrapAndParsesClientList(t *testing.T) {
	fixture := startRedisRESPFixture(t)
	list := "id=8 user=cv_cp_12 addr=127.0.0.1:40000\n"
	fixture.setClientListReply(fmt.Sprintf("$%d\r\n%s\r\n", len(list), list))
	admin, err := newRedisAdmin(fixture.listener.Addr().String(), nil, managedredis.AdminBootstrap{Username: "cv_admin", Password: "synthetic-bootstrap-password"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.client.Close()) })
	require.NoError(t, admin.ProbeAuth(context.Background(), "cv_admin", "synthetic-bootstrap-password"))
	require.NoError(t, admin.LoadACL(context.Background(), aclFile))
	clients, err := admin.ListClients(context.Background())
	require.NoError(t, err)
	require.Equal(t, []managedredis.ClientIdentity{{ID: "8", Username: "cv_cp_12"}}, clients)
	output, err := admin.ExecuteOperator(context.Background(), []string{"GET", "media:active_colour"})
	require.NoError(t, err)
	require.Equal(t, "OK", output)
	require.ErrorIs(t, admin.LoadACL(context.Background(), "/wrong/path"), errAdmin)
	_, err = newRedisAdmin("not-an-endpoint", nil, managedredis.AdminBootstrap{Username: "cv_admin", Password: "synthetic-bootstrap-password"})
	require.ErrorIs(t, err, errAdmin)
}

func TestRedisAdminFailsClosedOnRedisCommandErrors(t *testing.T) {
	fixture := startRedisRESPFixture(t)
	client := redis.NewClient(&redis.Options{Addr: fixture.listener.Addr().String(), Protocol: 2, MaxRetries: 0})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	admin := &redisAdmin{client: client, endpoint: fixture.listener.Addr().String()}
	for _, command := range []string{
		"ACL LOAD", "ACL GETUSER", "CLIENT LIST", "CLIENT KILL", "SCAN", "GET MEDIA:ACTIVE_COLOUR",
	} {
		fixture.setForcedReply(command, "-ERR synthetic command failure\r\n")
	}
	ctx := context.Background()
	require.ErrorIs(t, admin.LoadACL(ctx, aclFile), errAdmin)
	clients, err := admin.ListClients(ctx)
	require.ErrorIs(t, err, errAdmin)
	require.Nil(t, clients)
	present, err := admin.UserExists(ctx, "cv_cp_11")
	require.ErrorIs(t, err, errAdmin)
	require.False(t, present)
	require.ErrorIs(t, admin.KillUser(ctx, "cv_cp_11"), errAdmin)
	output, err := admin.ExecuteOperator(ctx, []string{"GET", "media:active_colour"})
	require.ErrorIs(t, err, errAdmin)
	require.Empty(t, output)
	output, err = admin.ExecuteOperator(ctx, []string{"--scan", "--pattern", "voice:room_owner:*"})
	require.ErrorIs(t, err, errAdmin)
	require.Empty(t, output)
	probeCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, admin.ProbeAuth(probeCtx, "cv_cp_11", "synthetic-password"), errAdmin)
}

func TestRedisAdminOperatorRejectsUnexpectedReplyShape(t *testing.T) {
	fixture := startRedisRESPFixture(t)
	client := redis.NewClient(&redis.Options{Addr: fixture.listener.Addr().String(), Protocol: 2, MaxRetries: 0})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	admin := &redisAdmin{client: client}
	command := "GET MEDIA:ACTIVE_COLOUR"
	fixture.setForcedReply(command, "$-1\r\n")
	output, err := admin.ExecuteOperator(context.Background(), []string{"GET", "media:active_colour"})
	require.NoError(t, err)
	require.Empty(t, output)
	fixture.setForcedReply(command, ":2\r\n")
	output, err = admin.ExecuteOperator(context.Background(), []string{"GET", "media:active_colour"})
	require.NoError(t, err)
	require.Equal(t, "2", output)
	fixture.setForcedReply(command, "*0\r\n")
	output, err = admin.ExecuteOperator(context.Background(), []string{"GET", "media:active_colour"})
	require.ErrorIs(t, err, errAdmin)
	require.Empty(t, output)
}
