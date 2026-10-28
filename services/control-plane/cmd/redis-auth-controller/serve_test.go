package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/stretchr/testify/require"
)

func socketTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cv-ack-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	group := os.Getegid()
	if group == 0 {
		group = 1
	}
	require.NoError(t, os.Chown(dir, -1, group))
	require.NoError(t, os.Chmod(dir, 0o710|os.ModeSetgid))
	for _, slot := range []string{"control-plane", "media-plane", "media-plane-blue", "media-plane-green"} {
		path := filepath.Join(dir, "acks", slot)
		require.NoError(t, os.MkdirAll(path, 0o700))
		require.NoError(t, os.Chmod(path, 0o710|os.ModeSetgid))
	}
	return dir
}

func TestRequiredControllerEnvironmentRejectsMissingAndMalformedValues(t *testing.T) {
	t.Setenv("TEST_REDIS_VALUE", "")
	_, err := requiredEnv("TEST_REDIS_VALUE")
	require.ErrorIs(t, err, errServe)
	t.Setenv("TEST_REDIS_VALUE", "bad\nvalue")
	_, err = requiredEnv("TEST_REDIS_VALUE")
	require.ErrorIs(t, err, errServe)
	t.Setenv("TEST_REDIS_VALUE", "70000")
	_, err = requiredUID("TEST_REDIS_VALUE")
	require.ErrorIs(t, err, errServe)
	_, err = requiredPeerUID("TEST_REDIS_VALUE")
	require.ErrorIs(t, err, errServe)
	t.Setenv("TEST_REDIS_VALUE", "1002")
	uid, err := requiredUID("TEST_REDIS_VALUE")
	require.NoError(t, err)
	require.Equal(t, 1002, uid)
	peerUID, err := requiredPeerUID("TEST_REDIS_VALUE")
	require.NoError(t, err)
	require.Equal(t, uint32(1002), peerUID)
}

func TestReadPrepareInputsRequiresAllScopedSettings(t *testing.T) {
	for name, value := range map[string]string{
		"REDIS_ENDPOINT": "127.0.0.1:6379", "REDIS_INFISICAL_PROJECT_ID": "project-test",
		"REDIS_INFISICAL_ENVIRONMENT": "test", "REDIS_INFISICAL_PATH": "/redis",
		"REDIS_INFISICAL_CLIENT_ID": "synthetic-client-id", "REDIS_CONTROL_UID": "1002",
		"REDIS_MEDIA_UID": "1003", "REDIS_ACL_GID": "1005",
	} {
		t.Setenv(name, value)
	}
	input, err := readPrepareInputs()
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:6379", input.endpoint)
	require.Equal(t, uint32(1003), input.mediaUID)
	t.Setenv("REDIS_INFISICAL_PROJECT_ID", "")
	_, err = readPrepareInputs()
	require.ErrorIs(t, err, errServe)
	t.Setenv("REDIS_INFISICAL_PROJECT_ID", "project-test")
	for _, name := range []string{"REDIS_CONTROL_UID", "REDIS_MEDIA_UID", "REDIS_ACL_GID"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "")
			_, err := readPrepareInputs()
			require.ErrorIs(t, err, errServe)
		})
	}
}

func TestServeFailsClosedWhenManagedSelectorIsMissing(t *testing.T) {
	t.Setenv("REDIS_CREDENTIAL_MODE", "")
	require.ErrorIs(t, serve(), errServe)
	t.Setenv("REDIS_CREDENTIAL_MODE", "managed")
	t.Setenv("REDIS_ENDPOINT", "127.0.0.1:6379")
	t.Setenv("REDIS_INFISICAL_PROJECT_ID", "")
	require.ErrorIs(t, serve(), errServe)
}

func TestServeAndPrepareRejectUnsafeProviderBeforeCredentialRead(t *testing.T) {
	for name, value := range map[string]string{
		"REDIS_CREDENTIAL_MODE": "managed", "REDIS_ENDPOINT": "127.0.0.1:6379", "REDIS_TLS": "false",
		"REDIS_INFISICAL_PROJECT_ID": "project-test", "REDIS_INFISICAL_ENVIRONMENT": "test", "REDIS_INFISICAL_PATH": "/redis",
		"REDIS_INFISICAL_CLIENT_ID": "synthetic-client-id", "REDIS_INFISICAL_URL": "http://provider.example.test",
		"REDIS_CONTROL_UID": "1002", "REDIS_MEDIA_UID": "1003", "REDIS_ACL_GID": "1005",
	} {
		t.Setenv(name, value)
	}
	require.ErrorIs(t, serve(), errServe)
	require.ErrorIs(t, prepare("first-activation"), errServe)
}

func TestOpenListenersBindsEveryServiceSocketAndClosesOnPartialFailure(t *testing.T) {
	root := socketTestDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listeners, err := openListeners(ctx, newCLITestController(t), root)
	require.NoError(t, err)
	require.Len(t, listeners, 5)
	for _, path := range []string{
		"control.sock", "acks/control-plane/ack-control.sock", "acks/media-plane/ack-media.sock",
		"acks/media-plane-blue/ack-media.sock", "acks/media-plane-green/ack-media.sock",
	} {
		info, statErr := os.Lstat(filepath.Join(root, path))
		require.NoError(t, statErr)
		require.NotZero(t, info.Mode()&os.ModeSocket)
		require.Equal(t, os.FileMode(0o660), info.Mode().Perm())
		parent, statErr := os.Stat(filepath.Dir(filepath.Join(root, path)))
		require.NoError(t, statErr)
		require.Equal(t, parent.Sys().(*syscall.Stat_t).Gid, info.Sys().(*syscall.Stat_t).Gid,
			"socket must inherit its isolated parent group")
	}
	for _, listener := range listeners {
		require.NoError(t, listener.Close())
	}

	blockedRoot := socketTestDir(t)
	blockedPath := filepath.Join(blockedRoot, "acks/media-plane/ack-media.sock")
	require.NoError(t, os.MkdirAll(filepath.Dir(blockedPath), 0o700))
	require.NoError(t, os.WriteFile(blockedPath, []byte("not a socket"), 0o600))
	listeners, err = openListeners(ctx, newCLITestController(t), blockedRoot)
	require.ErrorIs(t, err, errServe)
	require.Empty(t, listeners)
	_, err = net.DialTimeout("unix", filepath.Join(blockedRoot, "control.sock"), time.Second)
	require.Error(t, err, "earlier listeners must be closed after a later socket refuses to bind")
}

func TestReconcileOnceAppliesPolicyAndPreservesItOnProviderFailure(t *testing.T) {
	now := time.Now().Round(0)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	markerDir := filepath.Join(root, "marker")
	require.NoError(t, os.Mkdir(stateDir, 0o700))
	require.NoError(t, os.Mkdir(markerDir, 0o700))
	policy := cliTestPolicy(now)
	bootstrapScope := policy.Scope
	bootstrapScope.Name = "REDIS_ADMIN_BOOTSTRAP"
	controller, err := managedredis.NewController(managedredis.ControllerOptions{
		Admin: &cliRedisAdmin{}, ACLPath: filepath.Join(root, "users.acl"),
		JournalPath: filepath.Join(stateDir, "rotation.json"), MarkerPath: filepath.Join(markerDir, "activated"),
		Bootstrap: managedredis.AdminBootstrap{Version: 1, Scope: bootstrapScope, Username: "cv_admin", Password: "synthetic-bootstrap-password"},
		Now:       time.Now, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
	})
	require.NoError(t, err)
	initial := policy
	initial.RotationID = policy.RotationID
	initial.Services = make(map[string]managedredis.PolicyService, len(policy.Services))
	for service, entry := range policy.Services {
		entry.TargetGeneration = 11
		entry.Generations = entry.Generations[:1]
		initial.Services[service] = entry
	}
	require.NoError(t, controller.PrepareInitial(initial))
	raw, err := json.Marshal(policy)
	require.NoError(t, err)
	require.Empty(t, reconcileOnce(context.Background(), controller, cliPolicyReader{value: raw, name: policy.Scope.Name}, policy.Scope))
	state, rotationID, _ := controller.Status()
	require.Equal(t, "prepared", state)
	require.Equal(t, policy.RotationID, rotationID)
	beforeACL, err := os.ReadFile(filepath.Join(root, "users.acl")) // #nosec G304 -- ACL path is under this test's temporary directory.
	require.NoError(t, err)
	changed := cliTestPolicy(now)
	changed.RotationID = "rotation-13"
	service := changed.Services["control-plane"]
	service.Generations[1].PasswordHash = "#aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	changed.Services["control-plane"] = service
	changedRaw, err := json.Marshal(changed)
	require.NoError(t, err)
	require.Equal(t, "reconcile", reconcileOnce(context.Background(), controller, cliPolicyReader{value: changedRaw, name: policy.Scope.Name}, policy.Scope),
		"a valid provider document that changes an existing generation must be rejected without replacing the ACL")
	afterACL, err := os.ReadFile(filepath.Join(root, "users.acl")) // #nosec G304 -- ACL path is under this test's temporary directory.
	require.NoError(t, err)
	require.Equal(t, beforeACL, afterACL)

	require.Equal(t, "provider", reconcileOnce(context.Background(), controller, cliPolicyReader{value: raw, name: "OTHER_SECRET"}, policy.Scope))
	state, rotationID, _ = controller.Status()
	require.Equal(t, "failed", state)
	require.Equal(t, policy.RotationID, rotationID, "provider failure must retain the loaded ACL and policy")
	require.Equal(t, "policy", reconcileOnce(context.Background(), controller, cliPolicyReader{value: []byte(`{"version":1}`), name: policy.Scope.Name}, policy.Scope))
}

type observedPolicyReader struct {
	cliPolicyReader
	called  chan struct{}
	release chan struct{}
}

func (r *observedPolicyReader) ReadExact(ctx context.Context, scope managedredis.SecretScope) ([]byte, string, error) {
	select {
	case r.called <- struct{}{}:
	default:
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	return r.cliPolicyReader.ReadExact(ctx, scope)
}

type startupRedisAdmin struct {
	*cliRedisAdmin
	loaded    chan struct{}
	release   chan struct{}
	failFirst bool
}

func (a *startupRedisAdmin) LoadACL(ctx context.Context, path string) error {
	if a.release != nil {
		select {
		case <-a.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := a.cliRedisAdmin.LoadACL(ctx, path)
	if a.failFirst {
		a.failFirst = false
		err = errors.New("synthetic ambiguous ACL load failure")
	}
	select {
	case a.loaded <- struct{}{}:
	default:
	}
	return err
}

func TestServeRuntimeRestoresStateBeforeOpeningSocketsAndStopsCleanly(t *testing.T) {
	root := socketTestDir(t)
	aclDir := filepath.Join(root, "acl")
	stateDir := filepath.Join(root, "state")
	markerDir := filepath.Join(root, "marker")
	require.NoError(t, os.Mkdir(aclDir, 0o710))
	require.NoError(t, os.Chmod(aclDir, 0o710|os.ModeSetgid))
	aclInfo, err := os.Stat(aclDir)
	require.NoError(t, err)
	aclGID := int(aclInfo.Sys().(*syscall.Stat_t).Gid)
	require.NoError(t, os.Mkdir(stateDir, 0o700))
	require.NoError(t, os.Mkdir(markerDir, 0o700))
	markerPath := filepath.Join(markerDir, "activated")
	policy := cliTestPolicy(time.Now().Round(0))
	initial := policy
	initial.RotationID = "rotation-11"
	initial.Services = make(map[string]managedredis.PolicyService, len(policy.Services))
	for service, entry := range policy.Services {
		entry.TargetGeneration = 11
		entry.Generations = entry.Generations[:1]
		initial.Services[service] = entry
	}
	bootstrapScope := policy.Scope
	bootstrapScope.Name = "REDIS_ADMIN_BOOTSTRAP"
	bootstrap := managedredis.AdminBootstrap{Version: 1, Scope: bootstrapScope, Username: "cv_admin", Password: strings.Repeat("n", 24)}
	seedBootstrap := bootstrap
	seedBootstrap.Password = strings.Repeat("o", 24)
	uid := uint32(1002)
	input := prepareInputs{project: policy.Scope.Project, environment: policy.Scope.Environment, path: policy.Scope.Path,
		controlUID: uid, mediaUID: uid + 1, aclGID: aclGID}
	seed, err := managedredis.NewController(managedredis.ControllerOptions{
		Admin: &cliRedisAdmin{}, ACLPath: filepath.Join(aclDir, "users.acl"), JournalPath: filepath.Join(stateDir, "rotation.json"),
		MarkerPath: markerPath, Bootstrap: seedBootstrap, Now: time.Now,
		ExpectedUIDs: map[string]uint32{"control-plane": input.controlUID, "media-plane": input.mediaUID},
		ACLUID:       os.Geteuid(), ACLGID: input.aclGID,
	})
	require.NoError(t, err)
	require.NoError(t, seed.PrepareInitial(initial))
	oldHash := sha256.Sum256([]byte(seedBootstrap.Password))
	oldACL, err := os.ReadFile(filepath.Join(aclDir, "users.acl")) // #nosec G304 -- test-owned temporary ACL fixture.
	require.NoError(t, err)
	require.Contains(t, string(oldACL), "#"+hex.EncodeToString(oldHash[:]))
	raw, err := json.Marshal(initial)
	require.NoError(t, err)
	provider := &observedPolicyReader{cliPolicyReader: cliPolicyReader{value: raw, name: policy.Scope.Name}, called: make(chan struct{}, 1), release: make(chan struct{})}
	newBootstrapAccepted := make(chan struct{})
	bootstrapRejected := make(chan struct{}, 1)
	admin := &startupRedisAdmin{cliRedisAdmin: &cliRedisAdmin{probeAuth: func(_ context.Context, username, password string) error {
		if username != bootstrap.Username || !bytes.Equal([]byte(password), []byte(bootstrap.Password)) {
			return errors.New("synthetic stale bootstrap ACL")
		}
		select {
		case <-newBootstrapAccepted:
			return nil
		default:
			select {
			case bootstrapRejected <- struct{}{}:
			default:
			}
			return errors.New("synthetic Redis AUTH rejected changed bootstrap")
		}
	}}, loaded: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveRuntime(ctx, input, provider, admin, bootstrap, servePaths{root: root, markerPath: markerPath})
	}()
	select {
	case <-provider.called:
	case err := <-done:
		require.NoError(t, err, "controller must remain serving until shutdown")
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not read the startup policy")
	}
	for _, path := range []string{"control.sock", "acks/control-plane/ack-control.sock", "acks/media-plane/ack-media.sock", "acks/media-plane-blue/ack-media.sock", "acks/media-plane-green/ack-media.sock"} {
		_, statErr := os.Lstat(filepath.Join(root, path))
		require.ErrorIs(t, statErr, os.ErrNotExist, "health and ACK sockets must stay closed until provider validation")
	}
	close(provider.release)
	require.Eventually(t, func() bool {
		info, statErr := os.Lstat(filepath.Join(root, "control.sock"))
		if statErr != nil || info.Mode()&os.ModeSocket == 0 {
			return false
		}
		for _, path := range []string{"acks/control-plane/ack-control.sock", "acks/media-plane/ack-media.sock", "acks/media-plane-blue/ack-media.sock", "acks/media-plane-green/ack-media.sock"} {
			if _, statErr := os.Lstat(filepath.Join(root, path)); !errors.Is(statErr, os.ErrNotExist) {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond, "the changed bootstrap must keep application sockets closed until AUTH succeeds")
	select {
	case <-bootstrapRejected:
	case err := <-done:
		require.NoError(t, err, "controller must remain serving until shutdown")
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not attempt the fetched bootstrap AUTH")
	}
	for _, path := range []string{"acks/control-plane/ack-control.sock", "acks/media-plane/ack-media.sock", "acks/media-plane-blue/ack-media.sock", "acks/media-plane-green/ack-media.sock"} {
		_, statErr := os.Lstat(filepath.Join(root, path))
		require.ErrorIs(t, statErr, os.ErrNotExist, "rejected bootstrap AUTH must keep application sockets closed")
	}
	select {
	case <-admin.loaded:
		t.Fatal("policy reconciliation ran before the bootstrap AUTH probe succeeded")
	default:
	}
	retainedACL, err := os.ReadFile(filepath.Join(aclDir, "users.acl")) // #nosec G304 -- test-owned temporary ACL fixture.
	require.NoError(t, err)
	require.Equal(t, oldACL, retainedACL, "a changed bootstrap must not make the controller rewrite the prepared ACL before authentication")
	close(newBootstrapAccepted)
	require.Eventually(t, func() bool {
		for _, path := range []string{"control.sock", "acks/control-plane/ack-control.sock", "acks/media-plane/ack-media.sock", "acks/media-plane-blue/ack-media.sock", "acks/media-plane-green/ack-media.sock"} {
			info, statErr := os.Lstat(filepath.Join(root, path))
			if statErr != nil || info.Mode()&os.ModeSocket == 0 {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond, "successful bootstrap AUTH must admit application readiness")
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not stop after cancellation")
	}
	_, err = os.Lstat(filepath.Join(root, "control.sock"))
	require.ErrorIs(t, err, os.ErrNotExist)

	blocked := &observedPolicyReader{cliPolicyReader: cliPolicyReader{value: raw, name: policy.Scope.Name}, called: make(chan struct{}, 1), release: make(chan struct{})}
	blockedCtx, stopBlocked := context.WithCancel(context.Background())
	blockedDone := make(chan error, 1)
	go func() {
		blockedDone <- serveRuntime(blockedCtx, input, blocked, &cliRedisAdmin{}, bootstrap, servePaths{root: root, markerPath: markerPath})
	}()
	select {
	case <-blocked.called:
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not reach the provider read")
	}
	_, err = os.Lstat(filepath.Join(root, "control.sock"))
	require.ErrorIs(t, err, os.ErrNotExist)
	stopBlocked()
	select {
	case err := <-blockedDone:
		require.ErrorIs(t, err, errServe, "cancelled provider read must fail closed")
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not stop after provider cancellation")
	}

	changed, err := json.Marshal(policy)
	require.NoError(t, err)
	for name, reader := range map[string]cliPolicyReader{
		"wrong name":       {value: raw, name: "OTHER_SECRET"},
		"invalid document": {value: []byte(`{"version":1}`), name: policy.Scope.Name},
		"changed policy":   {value: changed, name: policy.Scope.Name},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, serveRuntime(context.Background(), input, reader, &cliRedisAdmin{}, bootstrap, servePaths{root: root, markerPath: markerPath}), errServe)
			_, statErr := os.Lstat(filepath.Join(root, "control.sock"))
			require.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}

	require.NoError(t, os.Chmod(aclDir, 0o700)) // #nosec G302 -- deliberate unsafe directory mode in a temporary test fixture.
	require.ErrorIs(t, serveRuntime(context.Background(), input, provider, &cliRedisAdmin{}, bootstrap, servePaths{root: root, markerPath: markerPath}), errServe)
	if runtime.GOOS == "linux" {
		require.NoError(t, os.Chmod(aclDir, 0o710)) // #nosec G302 -- deliberate missing setgid in a temporary fixture.
		require.ErrorIs(t, serveRuntime(context.Background(), input, provider, &cliRedisAdmin{}, bootstrap, servePaths{root: root, markerPath: markerPath}), errServe)
	}
	require.NoError(t, os.Chmod(aclDir, 0o710|os.ModeSetgid))
	require.NoError(t, os.Remove(filepath.Join(stateDir, "rotation.json")))
	require.ErrorIs(t, serveRuntime(context.Background(), input, cliPolicyReader{value: raw, name: policy.Scope.Name}, &cliRedisAdmin{}, bootstrap, servePaths{root: root, markerPath: markerPath}), errServe,
		"ordinary serving cannot perform a first activation without the durable journal")
	_, err = os.Lstat(filepath.Join(root, "control.sock"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestServeRuntimeRevocationRecoveryOpensOnlyControlSocket(t *testing.T) {
	root := socketTestDir(t)
	aclDir := filepath.Join(root, "acl")
	stateDir := filepath.Join(root, "state")
	markerDir := filepath.Join(root, "marker")
	require.NoError(t, os.Mkdir(aclDir, 0o710))
	require.NoError(t, os.Chmod(aclDir, 0o710|os.ModeSetgid))
	require.NoError(t, os.Mkdir(stateDir, 0o700))
	require.NoError(t, os.Mkdir(markerDir, 0o700))
	aclInfo, err := os.Stat(aclDir)
	require.NoError(t, err)
	now := time.Now().Round(0)
	policy := cliTestPolicy(now)
	initial := policy
	initial.Services = make(map[string]managedredis.PolicyService, 2)
	for service, entry := range policy.Services {
		entry.TargetGeneration = 11
		entry.Generations = entry.Generations[:1]
		initial.Services[service] = entry
	}
	bootstrapScope := policy.Scope
	bootstrapScope.Name = "REDIS_ADMIN_BOOTSTRAP"
	bootstrap := managedredis.AdminBootstrap{Version: 1, Scope: bootstrapScope, Username: "cv_admin", Password: "synthetic-bootstrap-password"}
	input := prepareInputs{project: policy.Scope.Project, environment: policy.Scope.Environment, path: policy.Scope.Path,
		controlUID: 1002, mediaUID: 1003, aclGID: int(aclInfo.Sys().(*syscall.Stat_t).Gid)}
	admin := &cliRedisAdmin{}
	seed, err := managedredis.NewController(managedredis.ControllerOptions{
		Admin: admin, ACLPath: filepath.Join(aclDir, "users.acl"), JournalPath: filepath.Join(stateDir, "rotation.json"),
		MarkerPath: filepath.Join(markerDir, "activated"), Bootstrap: bootstrap, Now: time.Now,
		ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
		ACLUID:       os.Geteuid(), ACLGID: input.aclGID,
	})
	require.NoError(t, err)
	require.NoError(t, seed.PrepareInitial(initial))
	require.NoError(t, seed.Reconcile(context.Background(), policy))
	require.NoError(t, seed.RegisterExpected(context.Background(), policy.RotationID,
		map[string][]string{"control-plane": {"cp-a"}, "media-plane": {"mp-a"}},
		map[string]uint64{"control-plane": 12, "media-plane": 12}))
	for _, item := range []struct {
		service, host string
		uid           uint32
	}{
		{"control-plane", "cp-a", 1002}, {"media-plane", "mp-a", 1003},
	} {
		bindCLISlot(t, seed, item.service, item.host)
		username := "cv_cp_12"
		if item.service == "media-plane" {
			username = "cv_mp_12"
		}
		require.NoError(t, seed.RecordAck(context.Background(), item.service, item.uid,
			managedredis.AppliedAck{Version: 1, RotationID: policy.RotationID, Service: item.service,
				InstanceID: item.host, Generation: 12, Username: username}))
	}
	admin.loadErr = errors.New("synthetic ACL load failure")
	require.Error(t, seed.Finalize(context.Background(), policy.RotationID,
		map[string]string{"control-plane": "cv_cp_11", "media-plane": "cv_mp_11"}))

	provider := &observedPolicyReader{cliPolicyReader: cliPolicyReader{name: policy.Scope.Name}, called: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveRuntime(ctx, input, provider, admin, bootstrap, servePaths{root: root, markerPath: filepath.Join(markerDir, "activated")})
	}()
	require.Eventually(t, func() bool {
		info, statErr := os.Lstat(filepath.Join(root, "control.sock"))
		return statErr == nil && info.Mode()&os.ModeSocket != 0
	}, 5*time.Second, 10*time.Millisecond)
	request := func(line string) string {
		t.Helper()
		conn, dialErr := net.DialTimeout("unix", filepath.Join(root, "control.sock"), time.Second)
		require.NoError(t, dialErr)
		require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
		_, writeErr := conn.Write([]byte(line + "\n"))
		require.NoError(t, writeErr)
		reply, readErr := bufio.NewReader(conn).ReadString('\n')
		require.NoError(t, readErr)
		require.NoError(t, conn.Close())
		return reply
	}
	if os.Geteuid() == 0 {
		require.Contains(t, request(`{"operation":"status"}`), `"state":"resume-required"`)
		require.Equal(t, "{\"error\":\"control-rejected\"}\n", request(`{"operation":"probe"}`))
	} else {
		require.Equal(t, "{\"error\":\"control-rejected\"}\n", request(`{"operation":"status"}`))
	}
	for _, path := range []string{"acks/control-plane/ack-control.sock", "acks/media-plane/ack-media.sock",
		"acks/media-plane-blue/ack-media.sock", "acks/media-plane-green/ack-media.sock"} {
		_, statErr := os.Lstat(filepath.Join(root, path))
		require.ErrorIs(t, statErr, os.ErrNotExist)
	}
	select {
	case <-provider.called:
		t.Fatal("recovery-only serving must not read or admit a stale provider policy")
	default:
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("recovery-only controller did not stop")
	}
}

func TestServeRuntimeResumesJournaledReconcileBeforeOpeningAppSockets(t *testing.T) {
	root := socketTestDir(t)
	aclDir := filepath.Join(root, "acl")
	stateDir := filepath.Join(root, "state")
	markerDir := filepath.Join(root, "marker")
	require.NoError(t, os.Mkdir(aclDir, 0o710))
	require.NoError(t, os.Chmod(aclDir, 0o710|os.ModeSetgid))
	require.NoError(t, os.Mkdir(stateDir, 0o700))
	require.NoError(t, os.Mkdir(markerDir, 0o700))
	aclInfo, err := os.Stat(aclDir)
	require.NoError(t, err)
	policy := cliTestPolicy(time.Now().Round(0))
	initial := policy
	initial.Services = make(map[string]managedredis.PolicyService, 2)
	for service, entry := range policy.Services {
		entry.TargetGeneration = 11
		entry.Generations = entry.Generations[:1]
		initial.Services[service] = entry
	}
	bootstrapScope := policy.Scope
	bootstrapScope.Name = "REDIS_ADMIN_BOOTSTRAP"
	bootstrap := managedredis.AdminBootstrap{Version: 1, Scope: bootstrapScope, Username: "cv_admin", Password: "synthetic-bootstrap-password"}
	input := prepareInputs{project: policy.Scope.Project, environment: policy.Scope.Environment, path: policy.Scope.Path,
		controlUID: 1002, mediaUID: 1003, aclGID: int(aclInfo.Sys().(*syscall.Stat_t).Gid)}
	markerPath := filepath.Join(markerDir, "activated")
	seedAdmin := &cliRedisAdmin{loadErr: errors.New("synthetic ACL load failure")}
	seed, err := managedredis.NewController(managedredis.ControllerOptions{
		Admin: seedAdmin, ACLPath: filepath.Join(aclDir, "users.acl"), JournalPath: filepath.Join(stateDir, "rotation.json"),
		MarkerPath: markerPath, Bootstrap: bootstrap, Now: time.Now,
		ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
		ACLUID:       os.Geteuid(), ACLGID: input.aclGID,
	})
	require.NoError(t, err)
	require.NoError(t, seed.PrepareInitial(initial))
	require.Error(t, seed.Reconcile(context.Background(), policy))
	require.Contains(t, string(handleRecoveryControl(context.Background(), seed, controllerUID, []byte(`{"operation":"status"}`))),
		`"state":"resume-required"`, "an interrupted reconcile must admit Redis startup")
	raw, err := json.Marshal(policy)
	require.NoError(t, err)
	provider := &observedPolicyReader{cliPolicyReader: cliPolicyReader{value: raw, name: policy.Scope.Name}, called: make(chan struct{}, 1)}
	admin := &startupRedisAdmin{cliRedisAdmin: &cliRedisAdmin{}, loaded: make(chan struct{}, 1), release: make(chan struct{}, 1), failFirst: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveRuntime(ctx, input, provider, admin, bootstrap, servePaths{root: root, markerPath: markerPath})
	}()
	require.Eventually(t, func() bool {
		info, statErr := os.Lstat(filepath.Join(root, "control.sock"))
		return statErr == nil && info.Mode()&os.ModeSocket != 0
	}, 5*time.Second, 10*time.Millisecond)
	for _, path := range []string{"acks/control-plane/ack-control.sock", "acks/media-plane/ack-media.sock"} {
		_, statErr := os.Lstat(filepath.Join(root, path))
		require.ErrorIs(t, statErr, os.ErrNotExist, "app sockets must stay closed until ACL load succeeds")
	}
	select {
	case <-provider.called:
		t.Fatal("provider validation must follow the journaled ACL reload")
	default:
	}
	admin.release <- struct{}{}
	select {
	case <-admin.loaded:
	case <-time.After(5 * time.Second):
		t.Fatal("first journaled ACL load did not complete")
	}
	for _, path := range []string{"acks/control-plane/ack-control.sock", "acks/media-plane/ack-media.sock"} {
		_, statErr := os.Lstat(filepath.Join(root, path))
		require.ErrorIs(t, statErr, os.ErrNotExist, "failed ACL load must keep app sockets closed")
	}
	select {
	case <-provider.called:
		t.Fatal("failed ACL load must not admit provider validation")
	default:
	}
	admin.release <- struct{}{}
	select {
	case <-admin.loaded:
	case <-time.After(5 * time.Second):
		t.Fatal("journaled ACL reload did not retry")
	}
	select {
	case <-provider.called:
	case <-time.After(5 * time.Second):
		t.Fatal("provider validation did not follow ACL load")
	}
	require.Eventually(t, func() bool {
		for _, path := range []string{"acks/control-plane/ack-control.sock", "acks/media-plane/ack-media.sock"} {
			if _, statErr := os.Lstat(filepath.Join(root, path)); statErr != nil {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not stop after cancellation")
	}
}

func TestServeConfiguredRejectsUnsafeBootstrapInputs(t *testing.T) {
	input := prepareInputs{endpoint: "127.0.0.1:6379", clientID: "synthetic-client-id", project: "project-test", environment: "test", path: "/redis"}
	for _, tt := range []struct {
		name   string
		tls    string
		caFile string
		url    string
	}{
		{name: "missing TLS mode"},
		{name: "CA without TLS", tls: "false", caFile: filepath.Join(t.TempDir(), "ca.pem")},
		{name: "remote cleartext provider", tls: "false", url: "http://provider.example.test"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("REDIS_TLS", tt.tls)
			t.Setenv("REDIS_TLS_CA_FILE", tt.caFile)
			t.Setenv("REDIS_INFISICAL_URL", tt.url)
			require.ErrorIs(t, serveConfigured(context.Background(), input, filepath.Join(t.TempDir(), "client-secret"), t.TempDir(), filepath.Join(t.TempDir(), "activated")), errServe)
		})
	}
}

func TestServeConfiguredRejectsMalformedBootstrapFromProvider(t *testing.T) {
	bootstrap := []byte("{")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/auth/universal-auth/login" {
			_, _ = w.Write([]byte(`{"accessToken":"synthetic-token","expiresIn":3600,"tokenType":"Bearer"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"secret": map[string]string{ // #nosec G101 -- synthetic local provider response.
			"secretKey": "REDIS_ADMIN_BOOTSTRAP", "secretValue": string(bootstrap),
		}})
	}))
	defer server.Close()
	t.Setenv("REDIS_TLS", "false")
	t.Setenv("REDIS_TLS_CA_FILE", "")
	t.Setenv("REDIS_INFISICAL_URL", server.URL)

	secretPath := filepath.Join(t.TempDir(), "client-secret")
	require.NoError(t, os.WriteFile(secretPath, []byte("synthetic-client-secret"), 0o600))
	input := prepareInputs{endpoint: "127.0.0.1:6379", clientID: "synthetic-client-id", project: "project-test", environment: "test", path: "/redis"}
	err := serveConfigured(context.Background(), input, secretPath, t.TempDir(), filepath.Join(t.TempDir(), "activated"))
	require.ErrorIs(t, err, errServe)

	scope := managedredis.SecretScope{Project: input.project, Environment: input.environment, Path: input.path, Name: "REDIS_ADMIN_BOOTSTRAP"}
	bootstrap, err = json.Marshal(managedredis.AdminBootstrap{Version: 1, Scope: scope, Username: "cv_admin", Password: "synthetic-admin-password"}) // #nosec G117 -- synthetic bootstrap fixture, no real credential.
	require.NoError(t, err)
	input.endpoint = "not-an-endpoint"
	err = serveConfigured(context.Background(), input, secretPath, t.TempDir(), filepath.Join(t.TempDir(), "activated"))
	require.ErrorIs(t, err, errServe, "valid bootstrap must still be rejected when Redis endpoint validation fails")
}

func TestListenSocketReclaimsStaleSocketAndRejectsOverlongPath(t *testing.T) {
	path := filepath.Join(socketTestDir(t), "stale.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	stale.SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())

	listener, err := listenSocket(path)
	require.NoError(t, err)
	conn, err := net.DialTimeout("unix", path, time.Second)
	require.NoError(t, err, "the stale socket path must be rebound to a live listener")
	require.NoError(t, conn.Close())
	require.NoError(t, listener.Close())

	longPath := filepath.Join(socketTestDir(t), strings.Repeat("x", 200), "socket")
	_, err = listenSocket(longPath)
	require.ErrorIs(t, err, errServe)

	regular := filepath.Join(socketTestDir(t), "existing.sock")
	require.NoError(t, os.WriteFile(regular, []byte("preserve"), 0o600))
	_, err = listenSocket(regular)
	require.ErrorIs(t, err, errServe)
	require.FileExists(t, regular)

	_, err = listenSocket(filepath.Join(regular, "nested.sock"))
	require.ErrorIs(t, err, errServe)
}

func TestCloseInvalidSocketFailsClosed(t *testing.T) {
	path := filepath.Join(socketTestDir(t), "invalid.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	require.ErrorIs(t, closeInvalidSocket(listener), errServe)
	require.NoFileExists(t, path)
	require.ErrorIs(t, closeInvalidSocket(listener), errServe, "an already closed listener still fails closed")
}

func TestServeSocketRejectsMalformedAckWithoutLeakingRequest(t *testing.T) {
	dir := socketTestDir(t)
	var err error
	path := filepath.Join(dir, "ack.sock")
	listener, err := listenSocket(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o660), info.Mode().Perm())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := newCLITestController(t)
	go serveSocket(ctx, listener, controller, "control-plane")
	conn, err := net.DialTimeout("unix", path, time.Second)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
	_, err = conn.Write([]byte(`{"version":1,"rotationId":"rotation-12","service":"control-plane","instanceId":"cp-a","generation":12,"password":"synthetic-secret"}` + "\n"))
	require.NoError(t, err)
	reply, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "{\"error\":\"ack-rejected\"}\n", reply)
	require.NotContains(t, reply, "synthetic-secret")
}

func TestServeSocketRoutesControlRequestsThroughPeerAuthorization(t *testing.T) {
	path := filepath.Join(socketTestDir(t), "control.sock")
	listener, err := listenSocket(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := newCLITestController(t)
	go serveSocket(ctx, listener, controller, "")
	request := func(line string) string {
		t.Helper()
		conn, dialErr := net.DialTimeout("unix", path, time.Second)
		require.NoError(t, dialErr)
		require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
		_, writeErr := conn.Write([]byte(line + "\n"))
		require.NoError(t, writeErr)
		reply, readErr := bufio.NewReader(conn).ReadString('\n')
		require.NoError(t, readErr)
		require.NoError(t, conn.Close())
		return reply
	}
	status := request(`{"operation":"status"}`)
	probe := request(`{"operation":"probe"}`)
	register := request(`{"operation":"register","rotationId":"rotation-12","instances":{"control-plane":["cp-a"],"media-plane":["mp-a"]},"targets":{"control-plane":12,"media-plane":12}}`)
	if os.Geteuid() == 0 {
		require.Contains(t, status, `"state"`)
		require.Contains(t, probe, `"state"`)
		require.Contains(t, register, `"state"`)
	} else {
		if os.Geteuid() == controllerUID {
			require.Contains(t, status, `"state"`, "controller UID may request health status")
		} else {
			require.Equal(t, "{\"error\":\"control-rejected\"}\n", status)
		}
		require.Equal(t, "{\"error\":\"control-rejected\"}\n", probe)
		require.Equal(t, "{\"error\":\"control-rejected\"}\n", register)
		state, _, _ := controller.Status()
		require.Equal(t, "prepared", state, "non-root peer cannot mutate controller state")
	}
}

func TestServeSocketCancelsBlockedControlOperationAtRequestDeadline(t *testing.T) {
	path := filepath.Join(socketTestDir(t), "control.sock")
	listener, err := listenSocket(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	controller, admin := newCLITestControllerWithAdmin(t)
	entered := make(chan context.Context, 1)
	finished := make(chan error, 1)
	admin.probeAuth = func(ctx context.Context, _, _ string) error {
		entered <- ctx
		<-ctx.Done()
		finished <- ctx.Err()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveSocketWithPeerUID(ctx, listener, controller, "", func(*net.UnixConn) (uint32, error) {
		return 0, nil
	})
	conn, err := net.DialTimeout("unix", path, time.Second)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	_, err = conn.Write([]byte("{\"operation\":\"probe\"}\n"))
	require.NoError(t, err)
	select {
	case requestCtx := <-entered:
		deadline, ok := requestCtx.Deadline()
		require.True(t, ok)
		require.Greater(t, time.Until(deadline), time.Duration(0))
		require.LessOrEqual(t, time.Until(deadline), 5*time.Second)
	case <-time.After(time.Second):
		t.Fatal("control operation did not receive the socket request")
	}
	select {
	case err := <-finished:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(6 * time.Second):
		t.Fatal("control operation continued past the socket deadline")
	}
}

func TestReadLostStateProofRequiresBoundedNonemptyProofOnlyInRecoveryMode(t *testing.T) {
	proof := bytes.Repeat([]byte("p"), 16*1024)
	got, err := readLostStateProof(lostStateMode, bytes.NewReader(proof))
	require.NoError(t, err)
	require.Equal(t, proof, got)

	got, err = readLostStateProof("first-activation", bytes.NewReader(nil))
	require.NoError(t, err)
	require.Nil(t, got)
	for _, input := range [][]byte{nil, bytes.Repeat([]byte("x"), 16*1024+1)} {
		_, err = readLostStateProof(lostStateMode, bytes.NewReader(input))
		require.ErrorIs(t, err, errServe)
	}
}

func TestRequestValidatesLocalCommandAndForwardsOnlySuccessfulReply(t *testing.T) {
	path := filepath.Join(socketTestDir(t), "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- func() error {
			conn, acceptErr := listener.AcceptUnix()
			if acceptErr != nil {
				return acceptErr
			}
			line, readErr := bufio.NewReader(conn).ReadBytes('\n')
			if readErr != nil {
				if closeErr := conn.Close(); closeErr != nil {
					return closeErr
				}
				return readErr
			}
			if string(line) != "{\"operation\":\"status\"}\n" {
				if closeErr := conn.Close(); closeErr != nil {
					return closeErr
				}
				return errors.New("request changed")
			}
			_, writeErr := conn.Write([]byte("{\"state\":\"ready\"}\n"))
			closeErr := conn.Close()
			if writeErr != nil {
				return writeErr
			}
			return closeErr
		}()
	}()

	var output bytes.Buffer
	err = request(bytes.NewBufferString("{\"operation\":\"status\"}\n"), &output, path)
	require.NoError(t, err)
	require.Equal(t, "{\"state\":\"ready\"}\n", output.String())
	require.NoError(t, <-serverErr)
	require.ErrorIs(t, request(bytes.NewBufferString("{\"operation\":\"status\"}\n"), &output,
		filepath.Join(socketTestDir(t), "missing.sock")), errServe, "a missing local controller socket must fail closed")

	for _, input := range []string{"not-json\n", strings.Repeat("x", maxControlLine+1)} {
		badPath := filepath.Join(socketTestDir(t), "control.sock")
		badListener, listenErr := net.ListenUnix("unix", &net.UnixAddr{Name: badPath, Net: "unix"})
		require.NoError(t, listenErr)
		require.NoError(t, badListener.SetDeadline(time.Now().Add(100*time.Millisecond)))
		accepted := make(chan bool, 1)
		go func() {
			conn, acceptErr := badListener.AcceptUnix()
			if acceptErr == nil {
				if closeErr := conn.Close(); closeErr != nil {
					accepted <- true
					return
				}
			}
			accepted <- acceptErr == nil
		}()
		err = request(bytes.NewBufferString(input), &output, badPath)
		require.ErrorIs(t, err, errServe)
		require.False(t, <-accepted, "invalid local input must be rejected before dialing the controller")
		require.NoError(t, badListener.Close())
	}
}

func TestRequestDoesNotForwardControllerDenial(t *testing.T) {
	path := filepath.Join(socketTestDir(t), "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	defer func() { require.NoError(t, listener.Close()) }()

	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		_, readErr := bufio.NewReader(conn).ReadBytes('\n')
		if readErr != nil {
			serverErr <- errors.Join(readErr, conn.Close())
			return
		}
		_, writeErr := conn.Write([]byte("{\"error\":\"synthetic-denied\"}\n"))
		serverErr <- errors.Join(writeErr, conn.Close())
	}()

	var output bytes.Buffer
	err = request(bytes.NewBufferString("{\"operation\":\"status\"}\n"), &output, path)
	require.ErrorIs(t, err, errServe)
	require.Empty(t, output.String())
	require.NoError(t, <-serverErr)
}

func TestServeSocketDropsOversizedFrame(t *testing.T) {
	path := filepath.Join(socketTestDir(t), "ack.sock")
	listener, err := listenSocket(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveSocket(ctx, listener, newCLITestController(t), "control-plane")

	conn, err := net.DialTimeout("unix", path, time.Second)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
	_, err = conn.Write(append(bytes.Repeat([]byte("x"), maxControlLine+1), '\n'))
	require.NoError(t, err)
	_, err = bufio.NewReader(conn).ReadByte()
	require.Error(t, err)
}

// expiryRotationFixture prepares rotation-12 (old 11, target 12) on a mutable
// clock. With loadErr set, the ACL LOAD that accepts the rotation fails once.
func expiryRotationFixture(t *testing.T, clock *time.Time, loadErr error) (*managedredis.Controller, *cliRedisAdmin, string, managedredis.Policy) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"state", "marker"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, dir), 0o700))
	}
	policy := cliTestPolicy(*clock)
	bootstrapScope := policy.Scope
	bootstrapScope.Name = "REDIS_ADMIN_BOOTSTRAP"
	admin := &cliRedisAdmin{}
	controller, err := managedredis.NewController(managedredis.ControllerOptions{
		Admin: admin, ACLPath: filepath.Join(root, "users.acl"),
		JournalPath: filepath.Join(root, "state", "rotation.json"), MarkerPath: filepath.Join(root, "marker", "activated"),
		Bootstrap: managedredis.AdminBootstrap{Version: 1, Scope: bootstrapScope, Username: "cv_admin", Password: "synthetic-bootstrap-password"},
		Now:       func() time.Time { return *clock }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
	})
	require.NoError(t, err)
	initial := policy
	initial.Services = make(map[string]managedredis.PolicyService, len(policy.Services))
	for service, entry := range policy.Services {
		entry.TargetGeneration = 11
		entry.Generations = entry.Generations[:1]
		initial.Services[service] = entry
	}
	require.NoError(t, controller.PrepareInitial(initial))
	admin.loadErr = loadErr
	require.Equal(t, loadErr != nil, controller.Reconcile(context.Background(), policy) != nil)
	admin.loadErr = nil
	return controller, admin, filepath.Join(root, "users.acl"), policy
}

func requireOldUsersRevoked(t *testing.T, aclPath string) {
	t.Helper()
	acl, err := os.ReadFile(aclPath) // #nosec G304 -- ACL path is under this test's temporary directory.
	require.NoError(t, err)
	require.NotContains(t, string(acl), "user cv_cp_11 ")
	require.NotContains(t, string(acl), "user cv_mp_11 ")
}

func TestReconcileOnceRevokesExpiredOldUsersDuringProviderOutage(t *testing.T) {
	for _, loadFailed := range []bool{false, true} {
		name := "loaded"
		var loadErr error
		if loadFailed {
			// A failed ACL LOAD while accepting the rotation leaves a reconcile
			// intent that only a provider read clears.
			name, loadErr = "pending reconcile", errors.New("synthetic ACL LOAD timeout")
		}
		t.Run(name, func(t *testing.T) {
			clock := time.Now().Round(0)
			controller, _, aclPath, policy := expiryRotationFixture(t, &clock, loadErr)
			require.Equal(t, loadFailed, controller.ReconciliationPending())

			clock = policy.Services["control-plane"].Generations[0].ValidUntil
			require.Equal(t, "provider", reconcileOnce(context.Background(), controller, cliPolicyReader{name: "OTHER_SECRET"}, policy.Scope))
			state, _, _ := controller.Status()
			require.Equal(t, "failed", state, "the provider failure is still reported")
			requireOldUsersRevoked(t, aclPath)
		})
	}
}

// The deploy rebuild gate reads "finalized" plus registered instances as proof
// that every host ACKed, so an expiry revocation without every ACK must not
// report it.
func TestRevokeExpiredWithoutEveryAckDoesNotReportFinalized(t *testing.T) {
	clock := time.Now().Round(0)
	controller, _, aclPath, policy := expiryRotationFixture(t, &clock, nil)
	ctx := context.Background()
	require.NoError(t, controller.RegisterExpected(ctx, policy.RotationID, map[string][]string{"control-plane": {"cp-a"}, "media-plane": {"mp-a"}},
		map[string]uint64{"control-plane": 12, "media-plane": 12}))
	bindCLISlot(t, controller, "control-plane", "cp-a")
	bindCLISlot(t, controller, "media-plane", "mp-a")
	require.NoError(t, controller.RecordAck(ctx, "control-plane", 1002, managedredis.AppliedAck{Version: 1, RotationID: policy.RotationID,
		Service: "control-plane", InstanceID: "cp-a", Generation: 12, Username: "cv_cp_12"})) // mp-a withholds its ACK

	clock = policy.Services["control-plane"].Generations[0].ValidUntil
	revoked, err := controller.RevokeExpired(ctx)
	require.NoError(t, err)
	require.True(t, revoked)
	requireOldUsersRevoked(t, aclPath)
	var status struct {
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(handleControl(ctx, controller, 0, []byte("{\"operation\":\"status\"}\n")), &status))
	require.Equal(t, "prepared", status.State)
}
