//go:build managedredis_integration

package managedredis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// This opt-in test requires Docker and runs only a task-owned, disposable Redis.
// It validates Redis ACL semantics; it does not prove production UID or mount custody.
func TestRedisACLIntegration(t *testing.T) {
	if os.Getenv("MANAGED_REDIS_INTEGRATION") != "1" {
		t.Skip("set MANAGED_REDIS_INTEGRATION=1 to run the real Redis ACL canary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("Docker CLI unavailable")
	}
	name := fmt.Sprintf("cv-redis-acl-%d", time.Now().UnixNano())
	work := t.TempDir()
	data := filepath.Join(work, "data")
	aclDir := filepath.Join(work, "acl")
	require.NoError(t, os.MkdirAll(data, 0o700))
	require.NoError(t, os.MkdirAll(aclDir, 0o700))
	aclPath := filepath.Join(aclDir, "users.acl")
	passwords := map[string]string{
		"cv_cp_11": "cp-old-fixture", "cv_cp_12": "cp-new-fixture",
		"cv_mp_11": "mp-old-fixture", "cv_mp_12": "mp-new-fixture",
	}
	policy := integrationPolicy(passwords)
	initial := policy
	initial.RotationID = policy.RotationID
	initial.Services = make(map[string]PolicyService, len(policy.Services))
	for service, value := range policy.Services {
		value.TargetGeneration = 11
		value.Generations = append([]PolicyGeneration(nil), policy.Services[service].Generations[:1]...)
		initial.Services[service] = value
	}
	now := policyTestNow
	admin := &integrationAdminFixture{password: "admin-fixture"}
	stateDir := filepath.Join(work, "state")
	require.NoError(t, os.Mkdir(stateDir, 0o700))
	controller, err := NewController(ControllerOptions{
		Admin: admin, ACLPath: aclPath, JournalPath: filepath.Join(stateDir, "rotation.json"),
		MarkerPath: testMarkerPath(t, filepath.Join(stateDir, "rotation.json")),
		Bootstrap:  AdminBootstrap{Version: 1, Scope: SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}, Username: "cv_admin", Password: admin.password},
		Now:        func() time.Time { return now }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
	})
	require.NoError(t, err)
	require.NoError(t, controller.PrepareInitial(initial))
	contents, err := os.ReadFile(aclPath)
	require.NoError(t, err)
	manifest := checkedACLManifest
	require.NotContains(t, string(contents), "+@all")
	require.NotContains(t, string(contents), "~*")

	// The fixture runs as root because the test-created host bind mount's UID mapping
	// is not equivalent to the production tmpfs ownership contract.
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "--name", name,
		"--label", "concord.task=epic2944-3930-acl-integration",
		"--cpus", "0.5", "--memory", "128m", "-p", "127.0.0.1::6379",
		"-v", aclDir+":/acl:ro", "-v", data+":/data",
		"--entrypoint", "redis-server", "redis:7-alpine", "--aclfile", "/acl/users.acl", "--dir", "/data", "--appendonly", "yes")
	out, err := run.CombinedOutput()
	require.NoErrorf(t, err, "start fixture: %s", out)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	portOut, err := exec.Command("docker", "port", name, "6379/tcp").Output()
	require.NoError(t, err)
	endpoint := strings.TrimSpace(string(portOut))
	_, port, err := net.SplitHostPort(strings.TrimPrefix(endpoint, "127.0.0.1:"))
	if err != nil { // Docker commonly prints 127.0.0.1:<port>.
		_, port, err = net.SplitHostPort(endpoint)
	}
	require.NoError(t, err)
	addr := net.JoinHostPort("127.0.0.1", port)
	adminClient := integrationRedisClient(t, addr, "cv_admin", admin.password)
	t.Cleanup(func() { _ = adminClient.Close() })
	admin.client = adminClient
	require.Eventually(t, func() bool { return adminClient.Ping(ctx).Err() == nil }, 20*time.Second, 100*time.Millisecond)
	versionOut, err := exec.CommandContext(ctx, "docker", "exec", name, "redis-server", "--version").Output()
	require.NoError(t, err)
	aclDigest := sha256.Sum256(contents)
	t.Logf("Redis fixture image=redis:7-alpine version=%s ACL_SHA256=%s command_counts={control-plane:%d,media-plane:%d} key_pattern_counts={control-plane:%d,media-plane:%d} container_user=root", strings.TrimSpace(string(versionOut)), hex.EncodeToString(aclDigest[:]), len(manifest.Services["control-plane"].Commands), len(manifest.Services["media-plane"].Commands), len(manifest.Services["control-plane"].KeyPatterns), len(manifest.Services["media-plane"].KeyPatterns))
	if err := controller.Reconcile(ctx, policy); err != nil {
		t.Fatalf("reconcile: %v; ACL load: %v", err, admin.loadACLErr)
	}

	// Every manifest permission is exercised by ACL DRYRUN with its declared key
	// family, including representative command arities and both read/write paths.
	checker := diagnosticClient(adminClient)
	t.Cleanup(func() { _ = checker.Close() })
	for service, selector := range manifest.Services {
		user := policy.Services[service].Generations[1].Username
		client := integrationRedisClient(t, addr, user, passwords[user])
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(ctx).Err())
		for _, key := range selector.KeyPatterns {
			key = strings.TrimSuffix(key, "*") + "acl-fixture"
			_, err := checker.Do(ctx, "ACL", "DRYRUN", user, "GET", key).Result()
			require.NoErrorf(t, err, "%s GET %s", service, key)
		}
		for _, command := range selector.Commands {
			args := integrationDryRunArgs(command, selector.KeyPatterns[0])
			_, err := checker.Do(ctx, append([]any{"ACL", "DRYRUN", user}, args...)...).Result()
			require.NoErrorf(t, err, "%s command %s", service, command)
		}
		for _, command := range []string{"CONFIG", "ACL", "CLIENT"} {
			_, err := client.Do(ctx, command, "GET", "*", "NO_SUCH_SUBCOMMAND").Result()
			require.Error(t, err, service+" must not administer Redis")
		}
	}
	for _, keyPattern := range manifest.BluegreenOperator.KeyPatterns {
		key := strings.TrimSuffix(keyPattern, "*") + "acl-fixture"
		_, err := checker.Do(ctx, "ACL", "DRYRUN", "cv_admin", "GET", key).Result()
		require.NoErrorf(t, err, "operator GET %s", key)
	}
	for _, command := range manifest.BluegreenOperator.Commands {
		args := integrationOperatorDryRunArgs(command)
		_, err := checker.Do(ctx, append([]any{"ACL", "DRYRUN", "cv_admin"}, args...)...).Result()
		require.NoErrorf(t, err, "operator command %s", command)
	}
	cp := integrationRedisClient(t, addr, "cv_cp_12", passwords["cv_cp_12"])
	mp := integrationRedisClient(t, addr, "cv_mp_12", passwords["cv_mp_12"])
	t.Cleanup(func() { _ = cp.Close(); _ = mp.Close() })
	for _, key := range []string{"mfa_upgrade_bypass:acl-fixture", "session_revokes:acl-fixture", "attach_sess:acl-fixture"} {
		require.NoError(t, cp.Set(ctx, key, "preserve", 0).Err(), key)
		require.Equal(t, "preserve", cp.Get(ctx, key).Val())
	}
	require.Error(t, mp.Set(ctx, "voice:room_owner:acl-fixture", "x", 0).Err())
	require.Error(t, mp.Set(ctx, "media:colour_draining:blue", "x", 0).Err())
	require.NoError(t, adminClient.Set(ctx, "voice:room_owner:acl-fixture", "owner-fixture", 0).Err())
	require.NoError(t, mp.Get(ctx, "voice:room_owner:acl-fixture").Err())
	require.NoError(t, mp.MGet(ctx, "media:colour_draining:blue", "voice:room_owner:acl-fixture").Err())
	require.NoError(t, cp.Eval(ctx, `return redis.call('GET', KEYS[1])`, []string{"session_revokes:acl-fixture"}).Err())

	// A real overlapping ACL load preserves the keyspace and drops old AUTH only
	// after exact acknowledgments and an observed zero-client drain.
	old := integrationRedisClient(t, addr, "cv_cp_11", passwords["cv_cp_11"])
	t.Cleanup(func() { _ = old.Close() })
	require.NoError(t, old.Ping(ctx).Err())
	require.NoError(t, controller.RegisterExpected(ctx, "rotation-12", map[string][]string{"control-plane": {"cp-fixture"}, "media-plane": {"mp-fixture"}}, map[string]uint64{"control-plane": 12, "media-plane": 12}))
	require.NoError(t, controller.BindInstance(ctx, "control-plane", fmt.Sprintf("%064x", 1), "cp-fixture", ""))
	require.NoError(t, controller.BindInstance(ctx, "media-plane", fmt.Sprintf("%064x", 2), "mp-fixture", ""))
	require.NoError(t, controller.RecordAck(ctx, "control-plane", 1002, AppliedAck{Version: 1, RotationID: "rotation-12", Service: "control-plane", InstanceID: "cp-fixture", Generation: 12, Username: "cv_cp_12"}))
	require.NoError(t, controller.RecordAck(ctx, "media-plane", 1003, AppliedAck{Version: 1, RotationID: "rotation-12", Service: "media-plane", InstanceID: "mp-fixture", Generation: 12, Username: "cv_mp_12"}))
	// Close the known old socket so the controller observes zero old connections.
	require.NoError(t, old.Close())
	now = now.Add(31 * time.Second)
	aclBeforeFinalize, err := os.Stat(aclPath)
	require.NoError(t, err)
	require.NoError(t, controller.Finalize(ctx, "rotation-12", map[string]string{"control-plane": "cv_cp_11", "media-plane": "cv_mp_11"}))
	aclAfterFinalize, err := os.Stat(aclPath)
	require.NoError(t, err)
	require.False(t, os.SameFile(aclBeforeFinalize, aclAfterFinalize), "finalization must atomically replace the ACL file")
	remainingClients, err := admin.ListClients(ctx)
	require.NoError(t, err)
	for _, connected := range remainingClients {
		require.NotContains(t, []string{"cv_cp_11", "cv_mp_11"}, connected.Username)
	}
	failedOld := integrationRedisClient(t, addr, "cv_cp_11", passwords["cv_cp_11"])
	require.Error(t, failedOld.Ping(ctx).Err())
	_ = failedOld.Close()
	require.NoError(t, cp.Set(ctx, "session_revokes:acl-fixture", "after-finalize", 0).Err())

	// Restart from the committed ACL file and AOF; both new auth and data survive.
	require.NoError(t, cp.Set(ctx, "session_revokes:acl-fixture", "restart-proof", 0).Err())
	require.NoError(t, exec.CommandContext(ctx, "docker", "restart", name).Run())
	portOut, err = exec.CommandContext(ctx, "docker", "port", name, "6379/tcp").Output()
	require.NoError(t, err)
	addr = net.JoinHostPort("127.0.0.1", dockerPortNumber(t, string(portOut)))
	_ = cp.Close()
	_ = mp.Close()
	cp = integrationRedisClient(t, addr, "cv_cp_12", passwords["cv_cp_12"])
	mp = integrationRedisClient(t, addr, "cv_mp_12", passwords["cv_mp_12"])
	admin.client = integrationRedisClient(t, addr, "cv_admin", admin.password)
	if !eventually(20*time.Second, 100*time.Millisecond, func() bool { return cp.Ping(ctx).Err() == nil }) {
		logs, _ := exec.CommandContext(ctx, "docker", "logs", name).CombinedOutput()
		t.Fatalf("Redis did not recover after restart: %s", logs)
	}
	require.Equal(t, "restart-proof", cp.Get(ctx, "session_revokes:acl-fixture").Val())
	require.NoError(t, controller.ProbeConfigured(ctx))
	roomID := "01234567-89ab-cdef-0123-456789abcdef"
	ownerKey := "voice:room_owner:" + roomID
	liveKey := "voice:room_live:" + roomID
	drainKey := "media:colour_draining:blue"
	require.NoError(t, admin.client.Set(ctx, ownerKey, "blue", 0).Err())
	createScript := readMediaLuaScript(t, "CREATE_ROOM_LIVE_WITH_FENCE_SCRIPT")
	refreshScript := readMediaLuaScript(t, "REFRESH_ROOM_LIVE_WITH_FENCE_SCRIPT")
	removeScript := readMediaLuaScript(t, "REMOVE_OWNED_ROOM_LIVE_SCRIPT")
	createKeys := []string{ownerKey, liveKey, drainKey}
	createArgs := []interface{}{"blue", "fixture-instance", "90"}
	flat := integrationRedisClient(t, addr, "cv_mp_flat", passwords["cv_mp_12"])
	t.Cleanup(func() { _ = flat.Close() })
	_, err = flat.Eval(ctx, createScript, createKeys, createArgs...).Result()
	require.Error(t, err, "an unpaired flat EVAL grant must fail the mixed owner/live/drain key vector")
	require.Contains(t, strings.ToUpper(err.Error()), "NOPERM")
	created, err := mp.Eval(ctx, createScript, createKeys, createArgs...).Int()
	require.NoError(t, err)
	require.Equal(t, 1, created)
	refreshed, err := mp.Eval(ctx, refreshScript, []string{ownerKey, liveKey}, createArgs...).Int()
	require.NoError(t, err)
	require.Equal(t, 1, refreshed)
	removed, err := mp.Eval(ctx, removeScript, []string{liveKey}, "fixture-instance").Int()
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	for _, protectedKey := range []string{ownerKey, drainKey} {
		_, err := mp.Eval(ctx, `return redis.call('SET', KEYS[1], ARGV[1])`, []string{protectedKey}, "forbidden").Result()
		require.Error(t, err, "the EVAL selector must not grant nested SET to %s", protectedKey)
		require.Contains(t, strings.ToUpper(err.Error()), "NO PERMISSIONS TO ACCESS A KEY")
	}
	require.Equal(t, "blue", admin.client.Get(ctx, ownerKey).Val(), "blocked Lua must leave owner state unchanged")
	require.ErrorIs(t, admin.client.Get(ctx, drainKey).Err(), redis.Nil, "blocked Lua must leave drain state absent")
}

type integrationAdminFixture struct {
	client     *redis.Client
	password   string
	loadACLErr error
}

func (a *integrationAdminFixture) LoadACL(ctx context.Context, path string) error {
	if err := appendACLDiagnosticUserOnDisk(path); err != nil {
		a.loadACLErr = err
		return err
	}
	// Docker Desktop may expose a host-side atomic rename to Redis a moment late.
	for attempt := 0; attempt < 20; attempt++ {
		a.loadACLErr = a.client.Do(ctx, "ACL", "LOAD").Err()
		if a.loadACLErr == nil || !strings.Contains(a.loadACLErr.Error(), "opening file '/acl/users.acl': No such file or directory") {
			return a.loadACLErr
		}
		time.Sleep(100 * time.Millisecond)
	}
	return a.loadACLErr
}
func (a *integrationAdminFixture) ListClients(ctx context.Context) ([]ClientIdentity, error) {
	rows, err := a.client.Do(ctx, "CLIENT", "LIST", "TYPE", "normal").Text()
	if err != nil {
		return nil, err
	}
	var clients []ClientIdentity
	for _, row := range strings.Split(rows, "\n") {
		fields := strings.Fields(row)
		values := map[string]string{}
		for _, field := range fields {
			pair := strings.SplitN(field, "=", 2)
			if len(pair) == 2 {
				values[pair[0]] = pair[1]
			}
		}
		if values["user"] != "" {
			clients = append(clients, ClientIdentity{ID: values["id"], Username: values["user"]})
		}
	}
	return clients, nil
}
func (a *integrationAdminFixture) ProbeAuth(ctx context.Context, username, password string) error {
	opts := *a.client.Options()
	opts.Username = username
	opts.Password = password // pragma: allowlist secret -- disposable ACL fixture
	client := redis.NewClient(&opts)
	defer client.Close()
	return client.Ping(ctx).Err()
}
func (a *integrationAdminFixture) UserExists(ctx context.Context, username string) (bool, error) {
	value, err := a.client.Do(ctx, "ACL", "GETUSER", username).Result()
	if err != nil {
		if err == redis.Nil || strings.Contains(err.Error(), "null") {
			return false, nil
		}
		return false, err
	}
	return value != nil, nil
}
func (a *integrationAdminFixture) KillUser(ctx context.Context, username string) error {
	return a.client.Do(ctx, "CLIENT", "KILL", "USER", username).Err()
}
func (a *integrationAdminFixture) ExecuteOperator(context.Context, []string) (string, error) {
	return "", fmt.Errorf("unused")
}

func integrationPolicy(passwords map[string]string) Policy {
	policy := policyTestValue()
	for service, generations := range map[string][]string{"control-plane": {"cv_cp_11", "cv_cp_12"}, "media-plane": {"cv_mp_11", "cv_mp_12"}} {
		value := policy.Services[service]
		for i, user := range generations {
			hash := sha256.Sum256([]byte(passwords[user]))
			value.Generations[i].PasswordHash = "#" + hex.EncodeToString(hash[:])
		}
		policy.Services[service] = value
	}
	return policy
}

func appendACLDiagnosticUserOnDisk(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(contents)
	addChecker := !strings.Contains(text, "user acl_checker ")
	addFlat := !strings.Contains(text, "user cv_mp_flat ")
	if !addChecker && !addFlat {
		return nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if addChecker {
		text += "user acl_checker on resetpass >acl-checker-fixture resetkeys resetchannels -@all +acl|dryrun\n"
	}
	if addFlat {
		found := false
		for _, line := range strings.Split(text, "\n") {
			if !strings.HasPrefix(line, "user cv_mp_12 ") {
				continue
			}
			if selectorAt := strings.Index(line, " (+eval "); selectorAt >= 0 {
				line = line[:selectorAt]
			}
			text += strings.Replace(line, "user cv_mp_12 ", "user cv_mp_flat ", 1) + "\n"
			found = true
			break
		}
		if !found {
			return fmt.Errorf("cannot clone candidate media user for flat-ACL falsification")
		}
	}
	if err := os.WriteFile(path, []byte(text), 0o400); err != nil {
		return err
	}
	return nil
}

func diagnosticClient(admin *redis.Client) *redis.Client {
	opts := *admin.Options()
	opts.Username = "acl_checker"
	opts.Password = "acl-checker-fixture" // pragma: allowlist secret -- disposable ACL fixture
	return redis.NewClient(&opts)
}

func integrationRedisClient(t *testing.T, addr, username, password string) *redis.Client {
	t.Helper()
	opts := &redis.Options{Addr: addr}
	// Each test uses a fresh Docker Redis, so DB 0 is isolated and avoids granting
	// SELECT to the ACL fixture users.
	opts.DB = 0
	opts.Username = username
	opts.Password = password // pragma: allowlist secret -- disposable ACL fixture
	opts.ReadTimeout = time.Second
	opts.DialTimeout = time.Second
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func eventually(timeout, interval time.Duration, check func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(interval)
	}
	return check()
}

func dockerPortNumber(t *testing.T, output string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimSpace(output))
	require.NoError(t, err)
	return port
}

func integrationDryRunArgs(command, key string) []any {
	key = strings.TrimSuffix(key, "*") + "acl-fixture"
	switch command {
	case "ACL|WHOAMI":
		return []any{"ACL", "WHOAMI"}
	case "DECRBY", "INCRBY":
		return []any{command, "stepup:softlock:acl-fixture", "1"}
	case "DEL", "UNLINK":
		return []any{command, key}
	case "EVAL", "EVALSHA":
		return []any{command, "return 1", "0"}
	case "EXEC":
		return []any{command}
	case "EXISTS":
		return []any{command, key}
	case "EXPIRE":
		return []any{command, "auth_failures:ip:acl-fixture", "30"}
	case "GET", "GETDEL", "HGETALL", "HLEN", "PTTL", "TTL", "TYPE":
		return []any{command, key}
	case "HGET":
		return []any{command, "attach_sess:acl-fixture", "status"}
	case "HSET":
		return []any{command, "attach_sess:acl-fixture", "status", "x"}
	case "INCR":
		return []any{command, "login_attempts:acl-fixture"}
	case "MGET":
		return []any{command, key, key}
	case "MULTI", "PING", "UNWATCH":
		return []any{command}
	case "PEXPIRE":
		return []any{command, "voice:room_live:acl-fixture", "30000"}
	case "RPUSH":
		return []any{command, "attach_sess:acl-fixture", "x"}
	case "SADD", "SREM":
		return []any{command, "attestation:acl-fixture", "x"}
	case "SCARD", "SMEMBERS":
		return []any{command, "attestation:acl-fixture"}
	case "SCAN":
		return []any{command, "0", "MATCH", key}
	case "SET":
		return []any{command, key, "x"}
	case "SISMEMBER":
		return []any{command, "attestation:acl-fixture", "x"}
	case "WATCH":
		return []any{command, "recovery_code:acl-fixture"}
	case "ZADD":
		return []any{command, "session_revokes:acl-fixture", "1", "x"}
	case "ZCARD":
		return []any{command, "session_revokes:acl-fixture"}
	case "ZREMRANGEBYSCORE":
		return []any{command, "session_revokes:acl-fixture", "-inf", "0"}
	default:
		return []any{command}
	}
}

func integrationOperatorDryRunArgs(command string) []any {
	key := "voice:room_owner:01234567-89ab-cdef-0123-456789abcdef"
	switch command {
	case "DEL", "EXISTS", "GET":
		return []any{command, "media:active_colour"}
	case "EVAL":
		return []any{"EVAL", cleanupRoomScript, "2", key, "voice:room_live:01234567-89ab-cdef-0123-456789abcdef", "generation", "blue"}
	case "SADD", "SISMEMBER", "SREM":
		return []any{command, "media:colours:registered", "blue"}
	case "SCAN":
		return []any{"SCAN", "0", "MATCH", "voice:room_owner:*"}
	case "SET":
		return []any{"SET", "media:active_colour", "blue"}
	default:
		return []any{command}
	}
}

func readMediaLuaScript(t *testing.T, name string) string {
	t.Helper()
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "media-plane", "src", "lib", "redis.ts"))
	require.NoError(t, err)
	pattern := regexp.MustCompile("(?s)const " + regexp.QuoteMeta(name) + "\\s*=\\s*(?:`([^`]*)`|\"([^\"]*)\")")
	matches := pattern.FindSubmatch(source)
	require.Len(t, matches, 3, "source must declare %s", name)
	if len(matches[1]) != 0 {
		return string(matches[1])
	}
	return string(matches[2])
}
