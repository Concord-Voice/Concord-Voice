package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
	"github.com/stretchr/testify/require"
)

type cliRedisAdmin struct {
	aclPath      string
	loadCalls    int
	loadErr      error
	users        map[string]bool
	operatorArgs []string
	operatorOut  string
	probeAuth    func(context.Context, string, string) error
}

func (a *cliRedisAdmin) LoadACL(_ context.Context, path string) error {
	a.loadCalls++
	a.aclPath = path
	if a.loadErr != nil {
		return a.loadErr
	}
	a.users = map[string]bool{"cv_cp_12": true, "cv_mp_12": true}
	return nil
}
func (a *cliRedisAdmin) ListClients(context.Context) ([]managedredis.ClientIdentity, error) {
	return nil, nil
}
func (a *cliRedisAdmin) ProbeAuth(ctx context.Context, username, password string) error {
	if a.probeAuth != nil {
		return a.probeAuth(ctx, username, password)
	}
	return nil
}
func (a *cliRedisAdmin) UserExists(_ context.Context, username string) (bool, error) {
	return a.users[username], nil
}
func (a *cliRedisAdmin) KillUser(context.Context, string) error { return nil }
func (a *cliRedisAdmin) ExecuteOperator(_ context.Context, args []string) (string, error) {
	a.operatorArgs = append([]string(nil), args...)
	return a.operatorOut, nil
}

func cliTestPolicy(now time.Time) managedredis.Policy {
	scope := managedredis.SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_AUTH_POLICY"}
	return managedredis.Policy{Version: 1, Scope: scope, RotationID: "rotation-12", Services: map[string]managedredis.PolicyService{
		"control-plane": {TargetGeneration: 12, Generations: []managedredis.PolicyGeneration{
			{Generation: 11, Username: "cv_cp_11", PasswordHash: "#0000000000000000000000000000000000000000000000000000000000000000", ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(4 * time.Minute)},
			{Generation: 12, Username: "cv_cp_12", PasswordHash: "#1111111111111111111111111111111111111111111111111111111111111111", ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour)},
		}},
		"media-plane": {TargetGeneration: 12, Generations: []managedredis.PolicyGeneration{
			{Generation: 11, Username: "cv_mp_11", PasswordHash: "#2222222222222222222222222222222222222222222222222222222222222222", ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(4 * time.Minute)},
			{Generation: 12, Username: "cv_mp_12", PasswordHash: "#3333333333333333333333333333333333333333333333333333333333333333", ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour)},
		}},
	}}
}

func newCLITestController(t *testing.T) *managedredis.Controller {
	controller, _ := newCLITestControllerWithAdmin(t, true)
	return controller
}

func bindCLISlot(t *testing.T, controller *managedredis.Controller, slot, instance string) {
	t.Helper()
	digest := sha256.Sum256([]byte(slot + ":" + instance))
	require.NoError(t, controller.BindInstance(context.Background(), slot, hex.EncodeToString(digest[:]), instance, ""))
}

func TestPrepareRequiresExplicitFirstActivationOrRecoveryMode(t *testing.T) {
	t.Setenv("REDIS_CREDENTIAL_MODE", "managed")
	require.Error(t, prepare(""), "direct one-shot invocation cannot infer first activation from missing RAM state")
}

func TestPrepareFailsClosedWhenManagedSelectorIsMissing(t *testing.T) {
	t.Setenv("REDIS_CREDENTIAL_MODE", "managed")
	t.Setenv("REDIS_ENDPOINT", "127.0.0.1:6379")
	t.Setenv("REDIS_INFISICAL_PROJECT_ID", "")
	require.ErrorIs(t, prepare("first-activation"), errServe)
}

func TestPrepareAtRejectsInvalidTransportAndLocalStateBeforeProviderAccess(t *testing.T) {
	root := t.TempDir()
	clientSecretPath := filepath.Join(root, "client-secret")
	require.NoError(t, os.WriteFile(clientSecretPath, []byte("synthetic-client-secret"), 0o600)) // pragma: allowlist secret -- synthetic provider fixture
	aclDir := filepath.Join(root, "acl")
	require.NoError(t, os.Mkdir(aclDir, 0o710))
	require.NoError(t, os.Chmod(aclDir, 0o710|os.ModeSetgid))
	for key, value := range map[string]string{
		"REDIS_CREDENTIAL_MODE": "managed", "REDIS_ENDPOINT": "127.0.0.1:1", "REDIS_TLS": "false",
		"REDIS_INFISICAL_PROJECT_ID": "project-test", "REDIS_INFISICAL_ENVIRONMENT": "test", "REDIS_INFISICAL_PATH": "/redis",
		"REDIS_INFISICAL_CLIENT_ID": "synthetic-client-id", "REDIS_INFISICAL_URL": "http://127.0.0.1:1",
		"REDIS_CONTROL_UID": "1002", "REDIS_MEDIA_UID": "1003", "REDIS_ACL_GID": "1005",
	} {
		t.Setenv(key, value)
	}
	for _, tc := range []struct {
		name       string
		envKey     string
		envValue   string
		secretPath string
		aclPath    string
	}{
		{name: "ambiguous TLS mode", envKey: "REDIS_TLS", envValue: "maybe"},
		{name: "invalid Redis endpoint", envKey: "REDIS_ENDPOINT", envValue: "not-an-endpoint"},
		{name: "missing provider credential file", secretPath: filepath.Join(root, "missing-secret")},
		{name: "unsafe ACL directory", aclPath: filepath.Join(root, "users.acl")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envKey != "" {
				t.Setenv(tc.envKey, tc.envValue)
			}
			secretPath, aclPath := clientSecretPath, filepath.Join(aclDir, "users.acl")
			if tc.secretPath != "" {
				secretPath = tc.secretPath // pragma: allowlist secret -- test-only path to a synthetic fixture.
			}
			if tc.aclPath != "" {
				aclPath = tc.aclPath
			}
			err := prepareAt("first-activation", secretPath, aclPath, filepath.Join(root, "journal"), filepath.Join(root, "marker"))
			require.ErrorIs(t, err, errServe)
			_, statErr := os.Lstat(aclPath)
			require.ErrorIs(t, statErr, os.ErrNotExist, "rejected input must not write an ACL")
		})
	}
}

type cliPolicyReader struct {
	value []byte
	name  string
}

func (r cliPolicyReader) ReadExact(_ context.Context, _ managedredis.SecretScope) ([]byte, string, error) {
	return r.value, r.name, nil
}

func TestApplyPreparationFirstActivationRequiresExactValidPolicy(t *testing.T) {
	newColdController := func(t *testing.T) (*managedredis.Controller, managedredis.Policy, string) {
		t.Helper()
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		markerDir := filepath.Join(root, "marker")
		require.NoError(t, os.Mkdir(stateDir, 0o700))
		require.NoError(t, os.Mkdir(markerDir, 0o700))
		policy := cliTestPolicy(time.Now())
		for service, entry := range policy.Services {
			entry.Generations = entry.Generations[1:]
			policy.Services[service] = entry
		}
		bootstrapScope := policy.Scope
		bootstrapScope.Name = "REDIS_ADMIN_BOOTSTRAP"
		controller, err := managedredis.NewController(managedredis.ControllerOptions{
			Admin: &cliRedisAdmin{}, ACLPath: filepath.Join(root, "users.acl"),
			JournalPath: filepath.Join(stateDir, "rotation.json"), MarkerPath: filepath.Join(markerDir, "activated"),
			Bootstrap: managedredis.AdminBootstrap{Version: 1, Scope: bootstrapScope, Username: "cv_admin", Password: "synthetic-bootstrap-password"},
			Now:       time.Now, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
		})
		require.NoError(t, err)
		return controller, policy, filepath.Join(root, "users.acl")
	}
	for _, tc := range []struct {
		name      string
		badName   bool
		badPolicy bool
	}{
		{name: "valid"},
		{name: "wrong secret name", badName: true},
		{name: "invalid policy", badPolicy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, policy, aclPath := newColdController(t)
			raw, err := json.Marshal(policy)
			require.NoError(t, err)
			if tc.badPolicy {
				raw = []byte(`{"version":1}`)
			}
			name := policy.Scope.Name
			if tc.badName {
				name = "OTHER_SECRET"
			}
			err = applyPreparation(context.Background(), "first-activation", controller, cliPolicyReader{value: raw, name: name}, policy.Scope, nil)
			if tc.badName || tc.badPolicy {
				require.ErrorIs(t, err, errServe)
				_, statErr := os.Stat(aclPath)
				require.ErrorIs(t, statErr, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			state, rotation, targets := controller.Status()
			require.Equal(t, "prepared", state)
			require.Equal(t, policy.RotationID, rotation)
			require.Equal(t, uint64(12), targets["control-plane"])
			acl, err := os.ReadFile(aclPath) // #nosec G304 -- ACL path is created under this test's temporary directory.
			require.NoError(t, err)
			require.Contains(t, string(acl), "user default off")
		})
	}
}

func TestApplyPreparationRecoveryRequiresFreshCohort(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rotationID string
		generation uint64
		wantError  bool
	}{
		{name: "fresh cohort", rotationID: "rotation-13", generation: 13},
		{name: "stale cohort", rotationID: "rotation-12", generation: 12, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, _ := newCLITestControllerWithAdmin(t, false)
			policy := cliTestPolicy(time.Now())
			policy.RotationID = tc.rotationID
			for service, entry := range policy.Services {
				entry.TargetGeneration = tc.generation
				entry.Generations = entry.Generations[1:]
				entry.Generations[0].Generation = tc.generation
				entry.Generations[0].Username = strings.Replace(entry.Generations[0].Username, "12", strconv.FormatUint(tc.generation, 10), 1)
				entry.Generations[0].ValidFrom = time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC)
				policy.Services[service] = entry
			}
			raw, err := json.Marshal(policy)
			require.NoError(t, err)
			err = applyPreparation(context.Background(), "recover", controller, cliPolicyReader{value: raw, name: policy.Scope.Name}, policy.Scope, nil)
			if tc.wantError {
				require.ErrorIs(t, err, errServe)
				return
			}
			require.NoError(t, err)
			state, rotationID, targets := controller.Status()
			require.Equal(t, "prepared", state)
			require.Equal(t, tc.rotationID, rotationID)
			require.Equal(t, tc.generation, targets["control-plane"])
		})
	}
}

func TestPrepareAtFetchesOnlyNamedSecretsAndWritesFirstActivationState(t *testing.T) {
	const policyEndpoint = "/api/v4/secrets/REDIS_AUTH_POLICY" // pragma: allowlist secret -- fixed public route name, not a credential.
	for _, wrongPolicyName := range []bool{false, true} {
		name := "valid"
		if wrongPolicyName {
			name = "wrong policy name"
		}
		t.Run(name, func(t *testing.T) {
			root := socketTestDir(t)
			aclDir := filepath.Join(root, "acl")
			stateDir := filepath.Join(root, "state")
			markerDir := filepath.Join(root, "marker")
			require.NoError(t, os.Mkdir(aclDir, 0o710))
			require.NoError(t, os.Chmod(aclDir, 0o710|os.ModeSetgid))
			require.NoError(t, os.Mkdir(stateDir, 0o700))
			require.NoError(t, os.Mkdir(markerDir, 0o700))
			clientSecretPath := filepath.Join(root, "client-secret")
			require.NoError(t, os.WriteFile(clientSecretPath, []byte("synthetic-client-secret"), 0o600)) // pragma: allowlist secret -- synthetic provider fixture
			aclPath := filepath.Join(aclDir, "users.acl")
			journalPath := filepath.Join(stateDir, "rotation.json")
			markerPath := filepath.Join(markerDir, "activated")
			policy := cliTestPolicy(time.Now().Round(0))
			policy.Scope.Environment = "test"
			policy.RotationID = "rotation-11"
			for service, entry := range policy.Services {
				entry.TargetGeneration = 11
				entry.Generations = entry.Generations[:1]
				policy.Services[service] = entry
			}
			bootstrapScope := policy.Scope
			bootstrapScope.Name = "REDIS_ADMIN_BOOTSTRAP"
			bootstrapRaw, err := json.Marshal(managedredis.AdminBootstrap{Version: 1, Scope: bootstrapScope, Username: "cv_admin", Password: "synthetic-bootstrap-password"}) // #nosec G117 -- synthetic test fixture, no real credential.
			require.NoError(t, err)
			policyRaw, err := json.Marshal(policy)
			require.NoError(t, err)
			requests := make(chan string, 3)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.URL.Path
				if r.URL.Path == "/api/v1/auth/universal-auth/login" {
					w.Header().Set("Content-Type", "application/json")
					if _, writeErr := w.Write([]byte(`{"accessToken":"synthetic-access-token","expiresIn":3600,"tokenType":"Bearer"}`)); writeErr != nil {
						t.Errorf("provider login reply: %v", writeErr)
					}
					return
				}
				if r.URL.Query().Get("projectId") != "project-test" || r.URL.Query().Get("environment") != "test" ||
					r.URL.Query().Get("secretPath") != "/redis" || r.URL.Query().Get("includeImports") != "false" ||
					r.URL.Query().Get("expandSecretReferences") != "false" {
					t.Errorf("provider GET escaped its exact scope")
				}
				name, value := "", ""
				switch r.URL.Path {
				case "/api/v4/secrets/REDIS_ADMIN_BOOTSTRAP":
					name, value = bootstrapScope.Name, string(bootstrapRaw)
				case policyEndpoint:
					name, value = policy.Scope.Name, string(policyRaw)
					if wrongPolicyName {
						name = "WRONG_NAME"
					}
				default:
					t.Errorf("unreviewed provider endpoint: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				response, marshalErr := json.Marshal(map[string]any{"secret": map[string]string{"secretKey": name, "secretValue": value}})
				if marshalErr != nil {
					t.Errorf("provider fixture: %v", marshalErr)
					return
				}
				if _, writeErr := w.Write(response); writeErr != nil {
					t.Errorf("provider secret reply: %v", writeErr)
				}
			}))
			defer provider.Close()
			for key, value := range map[string]string{
				"REDIS_CREDENTIAL_MODE": "managed", "REDIS_ENDPOINT": "127.0.0.1:1", "REDIS_TLS": "false",
				"REDIS_INFISICAL_PROJECT_ID": "project-test", "REDIS_INFISICAL_ENVIRONMENT": "test", "REDIS_INFISICAL_PATH": "/redis",
				"REDIS_INFISICAL_CLIENT_ID": "synthetic-client-id", "REDIS_INFISICAL_URL": provider.URL,
				"REDIS_CONTROL_UID": "1002", "REDIS_MEDIA_UID": "1003",
			} {
				t.Setenv(key, value)
			}
			aclInfo, err := os.Stat(aclDir)
			require.NoError(t, err)
			aclGID := int(aclInfo.Sys().(*syscall.Stat_t).Gid)
			t.Setenv("REDIS_ACL_GID", strconv.Itoa(aclGID))
			err = prepareAt("first-activation", clientSecretPath, aclPath, journalPath, markerPath)
			for _, expected := range []string{"/api/v1/auth/universal-auth/login", "/api/v4/secrets/REDIS_ADMIN_BOOTSTRAP", policyEndpoint} {
				select {
				case observed := <-requests:
					require.Equal(t, expected, observed)
				case <-time.After(time.Second):
					t.Fatalf("provider request %s absent after prepare error %v", expected, err)
				}
			}
			if wrongPolicyName {
				require.ErrorIs(t, err, errServe)
				_, statErr := os.Lstat(aclPath)
				require.ErrorIs(t, statErr, os.ErrNotExist)
				_, statErr = os.Lstat(markerPath)
				require.ErrorIs(t, statErr, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			acl, err := os.ReadFile(aclPath) // #nosec G304 -- task-owned temporary ACL fixture.
			require.NoError(t, err)
			require.Contains(t, string(acl), "user default off")
			require.Contains(t, string(acl), "user cv_cp_11")
			_, err = os.Stat(journalPath)
			require.NoError(t, err)
			_, err = os.Stat(markerPath)
			require.NoError(t, err)

			input, err := readPrepareInputs()
			require.NoError(t, err)
			bootstrap := managedredis.AdminBootstrap{Version: 1, Scope: bootstrapScope, Username: "cv_admin", Password: "synthetic-bootstrap-password"}
			probe, err := managedredis.NewController(managedredis.ControllerOptions{
				Admin: &cliRedisAdmin{}, ACLPath: aclPath, JournalPath: journalPath, MarkerPath: markerPath,
				Bootstrap: bootstrap, Now: time.Now, ExpectedUIDs: map[string]uint32{"control-plane": input.controlUID, "media-plane": input.mediaUID},
				ACLUID: os.Geteuid(), ACLGID: input.aclGID,
			})
			require.NoError(t, err)
			require.NoError(t, probe.Restore())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serveDone := make(chan error, 1)
			go func() { serveDone <- serveConfigured(ctx, input, clientSecretPath, root, markerPath) }()
			for _, want := range []string{
				"/api/v1/auth/universal-auth/login", "/api/v4/secrets/REDIS_ADMIN_BOOTSTRAP", policyEndpoint,
			} {
				select {
				case got := <-requests:
					require.Equal(t, want, got)
				case err := <-serveDone:
					t.Fatalf("controller stopped before %s: %v (queued requests: %d)", want, err, len(requests))
				case <-time.After(5 * time.Second):
					t.Fatal("controller did not fetch its scoped policy")
				}
			}
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
			}, 5*time.Second, 10*time.Millisecond, "initial preparation must keep application sockets closed until the bootstrap AUTH probe succeeds")
			cancel()
			select {
			case err := <-serveDone:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("controller did not stop after cancellation")
			}
		})
	}
}

func newCLITestControllerWithAdmin(t *testing.T, registerExpected ...bool) (*managedredis.Controller, *cliRedisAdmin) {
	t.Helper()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	require.NoError(t, os.Mkdir(stateDir, 0o700))
	markerDir := filepath.Join(root, "marker")
	require.NoError(t, os.Mkdir(markerDir, 0o700))
	bootstrapScope := managedredis.SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}
	admin := &cliRedisAdmin{}
	controller, err := managedredis.NewController(managedredis.ControllerOptions{
		Admin: admin, ACLPath: filepath.Join(root, "users.acl"), JournalPath: filepath.Join(stateDir, "rotation.json"), MarkerPath: filepath.Join(markerDir, "activated"),
		Bootstrap: managedredis.AdminBootstrap{Version: 1, Scope: bootstrapScope, Username: "cv_admin", Password: "synthetic-bootstrap-password"},
		Now:       func() time.Time { return now }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
	})
	require.NoError(t, err)
	policy := cliTestPolicy(now)
	initial := policy
	initial.RotationID = policy.RotationID
	initial.Services = make(map[string]managedredis.PolicyService, len(policy.Services))
	for service, config := range policy.Services {
		config.TargetGeneration = 11
		config.Generations = config.Generations[:1]
		initial.Services[service] = config
	}
	require.NoError(t, controller.PrepareInitial(initial))
	require.NoError(t, controller.Reconcile(context.Background(), policy))
	shouldRegister := len(registerExpected) == 0 || registerExpected[0]
	if shouldRegister {
		require.NoError(t, controller.RegisterExpected(context.Background(), "rotation-12", map[string][]string{
			"control-plane": {"cp-a"}, "media-plane": {"mp-a"},
		}, map[string]uint64{"control-plane": 12, "media-plane": 12}))
		bindCLISlot(t, controller, "control-plane", "cp-a")
		bindCLISlot(t, controller, "media-plane", "mp-a")
	}
	return controller, admin
}

func marshalAck(t *testing.T, ack managedredis.AppliedAck) []byte {
	t.Helper()
	if ack.Service == "control-plane" {
		ack.Username = "cv_cp_12"
	} else {
		ack.Username = "cv_mp_12"
	}
	raw, err := json.Marshal(ack)
	require.NoError(t, err)
	return append(raw, '\n')
}

func TestHandleAckReturnsSuccessOnlyAfterAcceptedAck(t *testing.T) {
	controller := newCLITestController(t)
	raw := marshalAck(t, managedredis.AppliedAck{Version: 1, RotationID: "rotation-12", Service: "control-plane", InstanceID: "cp-a", Generation: 12})

	response := handleAck(context.Background(), controller, "control-plane", 1002, raw)

	require.Equal(t, "{\"ok\":true}\n", string(response))
	state, rotationID, targets := controller.Status()
	require.Equal(t, "prepared", state)
	require.Equal(t, "rotation-12", rotationID)
	require.Equal(t, uint64(12), targets["control-plane"])
	mediaAck := marshalAck(t, managedredis.AppliedAck{Version: 1, RotationID: "rotation-12", Service: "media-plane", InstanceID: "mp-a", Generation: 12})
	require.Equal(t, "{\"ok\":true}\n", string(handleAck(context.Background(), controller, "media-plane", 1003, mediaAck)))
	state, _, _ = controller.Status()
	require.Equal(t, "promoted", state)
}

func TestHandleAckRejectsSameUIDSiblingSlotSpoofAndBlocksFinalize(t *testing.T) {
	controller, _ := newCLITestControllerWithAdmin(t, false)
	require.NoError(t, controller.RegisterExpected(context.Background(), "rotation-12", map[string][]string{
		"control-plane": {"cp-a"}, "media-plane": {"mp-blue", "mp-green"},
	}, map[string]uint64{"control-plane": 12, "media-plane": 12}))
	bindCLISlot(t, controller, "control-plane", "cp-a")
	bindCLISlot(t, controller, "media-plane-blue", "mp-blue")
	bindCLISlot(t, controller, "media-plane-green", "mp-green")

	controlAck := managedredis.AppliedAck{Version: 1, RotationID: "rotation-12", Service: "control-plane", InstanceID: "cp-a", Generation: 12}
	require.Equal(t, "{\"ok\":true}\n", string(handleAck(context.Background(), controller, "control-plane", 1002, marshalAck(t, controlAck))))
	blueAck := managedredis.AppliedAck{Version: 1, RotationID: "rotation-12", Service: "media-plane", InstanceID: "mp-blue", Generation: 12}
	require.Equal(t, "{\"ok\":true}\n", string(handleAck(context.Background(), controller, "media-plane-blue", 1003, marshalAck(t, blueAck))))
	forgedGreenAck := managedredis.AppliedAck{Version: 1, RotationID: "rotation-12", Service: "media-plane", InstanceID: "mp-green", Generation: 12}
	require.Equal(t, "{\"error\":\"ack-rejected\"}\n", string(handleAck(context.Background(), controller, "media-plane-blue", 1003, marshalAck(t, forgedGreenAck))),
		"blue slot cannot acknowledge green even though both peers share the media UID")
	require.Error(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}), "a forged sibling ACK must not complete the expected set")
	require.Equal(t, "{\"ok\":true}\n", string(handleAck(context.Background(), controller, "media-plane-green", 1003, marshalAck(t, forgedGreenAck))))
}

func TestHandleAckRejectsWrongServiceUIDAndUnregisteredInstance(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service string
		uid     uint32
		ack     managedredis.AppliedAck
	}{
		{name: "wrong service socket", service: "control-plane", uid: 1002, ack: managedredis.AppliedAck{Version: 1, RotationID: "rotation-12", Service: "media-plane", InstanceID: "mp-a", Generation: 12}},
		{name: "wrong peer uid", service: "control-plane", uid: 1003, ack: managedredis.AppliedAck{Version: 1, RotationID: "rotation-12", Service: "control-plane", InstanceID: "cp-a", Generation: 12}},
		{name: "unknown host", service: "control-plane", uid: 1002, ack: managedredis.AppliedAck{Version: 1, RotationID: "rotation-12", Service: "control-plane", InstanceID: "cp-unknown", Generation: 12}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := handleAck(context.Background(), newCLITestController(t), tc.service, tc.uid, marshalAck(t, tc.ack))
			require.Equal(t, "{\"error\":\"ack-rejected\"}\n", string(response))
		})
	}
}

func TestHandleAckRejectsOversizeOrSecretBearingUnknownFieldsWithoutEcho(t *testing.T) {
	controller := newCLITestController(t)
	oversized := []byte(strings.Repeat("x", 16*1024+1))
	require.Equal(t, "{\"error\":\"ack-rejected\"}\n", string(handleAck(context.Background(), controller, "control-plane", 1002, oversized)))

	secretPayload := []byte(`{"version":1,"rotationId":"rotation-12","service":"control-plane","instanceId":"cp-a","generation":12,"password":"synthetic-credential-value"}`)
	response := string(handleAck(context.Background(), controller, "control-plane", 1002, secretPayload))
	require.Equal(t, "{\"error\":\"ack-rejected\"}\n", response)
	require.NotContains(t, response, "synthetic-credential-value")
	duplicate := []byte(`{"version":1,"rotationId":"rotation-12","service":"control-plane","instanceId":"cp-a","generation":11,"generation":12}`)
	require.Equal(t, "{\"error\":\"ack-rejected\"}\n", string(handleAck(context.Background(), controller, "control-plane", 1002, duplicate)))
}

func TestHandleControlRequiresRootAndReturnsBoundedStatus(t *testing.T) {
	controller, _ := newCLITestControllerWithAdmin(t, false)
	status := []byte(`{"operation":"status"}`)

	response := handleControl(context.Background(), controller, 1000, status)
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(response))
	response = handleControl(context.Background(), controller, controllerUID, status)
	require.Contains(t, string(response), `"state":"prepared"`)
	response = handleControl(context.Background(), controller, controllerUID, []byte(`{"operation":"probe"}`))
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(response), "controller UID may read health but not invoke operator commands")

	response = handleControl(context.Background(), controller, 0, status)
	var result map[string]any
	require.NoError(t, json.Unmarshal(response, &result))
	require.Equal(t, "prepared", result["state"])
	require.Equal(t, "rotation-12", result["rotationId"])
	require.NotContains(t, string(response), "synthetic-bootstrap-password")
	require.NotContains(t, string(response), "instances", "unknown expected sets are omitted")

	controller, _ = newCLITestControllerWithAdmin(t, false)
	registered := []byte(`{"operation":"register","rotationId":"rotation-12","instances":{"control-plane":["cp-z","cp-a"],"media-plane":["mp-b","mp-a"]},"targets":{"control-plane":12,"media-plane":12}}`)
	response = handleControl(context.Background(), controller, 0, registered)
	require.Contains(t, string(response), `"instances":{"control-plane":["cp-a","cp-z"],"media-plane":["mp-a","mp-b"]}`,
		"status reports only registered host IDs in stable order")
}

func TestHandleControlAcceptsRegisterOnlyForRootAndRedactsRejections(t *testing.T) {
	controller := newCLITestController(t)
	request := []byte(`{"operation":"register","rotationId":"rotation-12","instances":{"control-plane":["cp-a","cp-b"],"media-plane":["mp-a"]},"targets":{"control-plane":12,"media-plane":12}}`)

	response := handleControl(context.Background(), controller, 0, request)
	require.Contains(t, string(response), `"state":"prepared"`)
	require.NotContains(t, string(response), "password")

	bad := []byte(`{"operation":"probe","password":"synthetic-bootstrap-password"}`)
	response = handleControl(context.Background(), controller, 0, bad)
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(response))
	require.NotContains(t, string(response), "synthetic-bootstrap-password")
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(handleControl(context.Background(), controller, 0, []byte(strings.Repeat("x", 16*1024+1)))))
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(handleControl(context.Background(), controller, 0, []byte(`{"operation":"status","operation":"probe"}`))))
}

func TestHandleControlProbeBindAndFinalizeReachTheirControllerOperations(t *testing.T) {
	ctx := context.Background()
	controller, _ := newCLITestControllerWithAdmin(t, false)
	response := handleControl(ctx, controller, 0, []byte(`{"operation":"probe"}`))
	require.Contains(t, string(response), `"state":"proved"`)

	register := []byte(`{"operation":"register","rotationId":"rotation-12","instances":{"control-plane":["cp-a"],"media-plane":["mp-a"]},"targets":{"control-plane":12,"media-plane":12}}`)
	require.Contains(t, string(handleControl(ctx, controller, 0, register)), `"state":"prepared"`)
	digest := sha256.Sum256([]byte("control-plane:cp-a"))
	bind, err := json.Marshal(map[string]string{
		"operation": "bind", "slot": "control-plane", "containerId": hex.EncodeToString(digest[:]), "instanceId": "cp-a",
	})
	require.NoError(t, err)
	response = handleControl(ctx, controller, 0, bind)
	require.Contains(t, string(response), `"containerId":"`+hex.EncodeToString(digest[:])+`"`)
	require.Contains(t, string(response), `"instanceId":"cp-a"`)
	bindCLISlot(t, controller, "media-plane", "mp-a")

	for _, ack := range []managedredis.AppliedAck{
		{Version: 1, RotationID: "rotation-12", Service: "control-plane", InstanceID: "cp-a", Generation: 12},
		{Version: 1, RotationID: "rotation-12", Service: "media-plane", InstanceID: "mp-a", Generation: 12},
	} {
		uid := uint32(1002)
		if ack.Service == "media-plane" {
			uid = 1003
			ack.Username = "cv_mp_12"
		} else {
			ack.Username = "cv_cp_12"
		}
		require.NoError(t, controller.RecordAck(ctx, ack.Service, uid, ack))
	}
	finalize := []byte(`{"operation":"finalize","rotationId":"rotation-12","oldUsernames":{"control-plane":"cv_cp_11","media-plane":"cv_mp_11"}}`)
	response = handleControl(ctx, controller, 0, finalize)
	require.Contains(t, string(response), `"state":"finalized"`)
}

func TestHandleControlReplaceReadyRejectsUnknownContainer(t *testing.T) {
	controller := newCLITestController(t)
	request := []byte(`{"operation":"replace-ready","slot":"control-plane","containerId":"unknown-container"}`)
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(handleControl(context.Background(), controller, 0, request)))
	state, _, _ := controller.Status()
	require.Equal(t, "prepared", state)
}

func TestHandleControlResumeIsRootOnlyStrictAndCompletesRevocation(t *testing.T) {
	controller, admin := newCLITestControllerWithAdmin(t)
	for _, ack := range []managedredis.AppliedAck{
		{Version: 1, RotationID: "rotation-12", Service: "control-plane", InstanceID: "cp-a", Generation: 12},
		{Version: 1, RotationID: "rotation-12", Service: "media-plane", InstanceID: "mp-a", Generation: 12},
	} {
		uid := uint32(1002)
		if ack.Service == "media-plane" {
			uid = 1003
			ack.Username = "cv_mp_12"
		} else {
			ack.Username = "cv_cp_12"
		}
		require.NoError(t, controller.RecordAck(context.Background(), ack.Service, uid, ack))
	}
	admin.loadErr = errors.New("synthetic ambiguous ACL LOAD")
	require.Error(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}))
	loadsBeforeResume := admin.loadCalls
	request := []byte(`{"operation":"resume"}`)
	require.True(t, controller.RevocationPending())
	require.Contains(t, string(handleRecoveryControl(context.Background(), controller, controllerUID, []byte(`{"operation":"status"}`))), `"state":"resume-required"`)
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(handleRecoveryControl(context.Background(), controller, 1000, []byte(`{"operation":"status"}`))))
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(handleRecoveryControl(context.Background(), controller, 0, []byte(`{"operation":"probe"}`))))
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(handleRecoveryControl(context.Background(), controller, controllerUID, request)))
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(handleControl(context.Background(), controller, 1000, request)))
	require.Equal(t, loadsBeforeResume, admin.loadCalls, "unauthorized peer cannot retry the ACL load")
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(handleControl(context.Background(), controller, 0, []byte(`{"operation":"resume","rotationId":"rotation-12"}`))))
	require.Equal(t, loadsBeforeResume, admin.loadCalls, "resume rejects extra request fields before Redis access")

	admin.loadErr = nil
	response := string(handleRecoveryControl(context.Background(), controller, 0, request))
	require.Contains(t, response, `"state":"recovery-complete"`)
	require.False(t, controller.RevocationPending())
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(handleRecoveryControl(context.Background(), controller, 0, request)))
	require.Equal(t, loadsBeforeResume+1, admin.loadCalls)
}

func TestHandleControlStatusSupportsMaximumRegisteredHostUnion(t *testing.T) {
	controller, _ := newCLITestControllerWithAdmin(t)
	cpHosts := []string{"cp-a"}
	mediaHosts := []string{"mp-a"}
	for i := 0; len(cpHosts) < 64; i++ {
		cpHosts = append(cpHosts, "cp-"+strconv.Itoa(i))
	}
	for i := 0; len(mediaHosts) < 64; i++ {
		mediaHosts = append(mediaHosts, "mp-"+strconv.Itoa(i))
	}
	request, err := json.Marshal(map[string]any{
		"operation": "register", "rotationId": "rotation-12",
		"instances": map[string][]string{"control-plane": cpHosts, "media-plane": mediaHosts},
		"targets":   map[string]uint64{"control-plane": 12, "media-plane": 12},
	})
	require.NoError(t, err)
	response := handleControl(context.Background(), controller, 0, request)
	require.Less(t, len(response), maxControlLine, "maximum expected-set status must fit the bounded protocol")
	var result struct {
		Instances map[string][]string `json:"instances"`
	}
	require.NoError(t, json.Unmarshal(response, &result))
	require.Len(t, result.Instances["control-plane"], 64)
	require.Len(t, result.Instances["media-plane"], 64)
	require.True(t, sort.StringsAreSorted(result.Instances["control-plane"]))
	require.True(t, sort.StringsAreSorted(result.Instances["media-plane"]))
}

func TestHandleControlStatusExposesPriorHostProofBeforeFreshRegistration(t *testing.T) {
	controller := newCLITestController(t)
	for _, item := range []struct {
		service, host, username string
		uid                     uint32
	}{
		{"control-plane", "cp-a", "cv_cp_12", 1002},
		{"media-plane", "mp-a", "cv_mp_12", 1003},
	} {
		require.NoError(t, controller.RecordAck(context.Background(), item.service, item.uid, managedredis.AppliedAck{
			Version: 1, RotationID: "rotation-12", Service: item.service, InstanceID: item.host, Generation: 12, Username: item.username,
		}))
	}
	require.NoError(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}))
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	next := cliTestPolicy(now)
	next.RotationID = "rotation-13"
	for _, service := range []string{"control-plane", "media-plane"} {
		value := next.Services[service]
		username := "cv_cp_13"
		if service == "media-plane" {
			username = "cv_mp_13"
		}
		value.TargetGeneration = 13
		value.Generations = []managedredis.PolicyGeneration{value.Generations[1], {
			Generation: 13, Username: username, PasswordHash: "#aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ValidFrom: now, ValidUntil: now.Add(time.Hour),
		}}
		next.Services[service] = value
	}
	require.NoError(t, controller.Reconcile(context.Background(), next))
	response := handleControl(context.Background(), controller, 0, []byte("{\"operation\":\"status\"}\n"))
	var status struct {
		Instances       map[string][]string                                       `json:"instances"`
		PriorRotationID string                                                    `json:"priorRotationId"`
		PriorExpected   map[string][]string                                       `json:"priorExpected"`
		PriorAckProofs  []struct{ Service, InstanceID, Slot, ContainerID string } `json:"priorAckProofs"`
	}
	require.NoError(t, json.Unmarshal(response, &status))
	require.Empty(t, status.Instances, "new target ACKs need a fresh registration")
	require.Equal(t, "rotation-12", status.PriorRotationID)
	require.Equal(t, map[string][]string{"control-plane": {"cp-a"}, "media-plane": {"mp-a"}}, status.PriorExpected)
	require.Len(t, status.PriorAckProofs, 2)
	for _, proof := range status.PriorAckProofs {
		require.NotEmpty(t, proof.ContainerID)
		require.Equal(t, proof.Service, proof.Slot)
	}
}

func TestHandleControlRedisIsRootOnlyAndExecutesAllowedRead(t *testing.T) {
	controller, admin := newCLITestControllerWithAdmin(t)
	admin.operatorOut = "green"
	request := []byte(`{"operation":"redis","args":["GET","media:active_colour"]}`)

	response := handleControl(context.Background(), controller, 1000, request)
	require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(response))
	require.Empty(t, admin.operatorArgs, "non-root peer must not reach Redis")

	response = handleControl(context.Background(), controller, 0, request)
	var result map[string]string
	require.NoError(t, json.Unmarshal(response, &result))
	require.Equal(t, "green", result["output"])
	require.Equal(t, []string{"GET", "media:active_colour"}, admin.operatorArgs)
}

func TestHandleControlRedisRejectsUnknownCommandKeyLuaAndArity(t *testing.T) {
	controller, admin := newCLITestControllerWithAdmin(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "admin command", args: []string{"CONFIG", "GET", "*"}},
		{name: "out of scope key", args: []string{"GET", "unrelated:secret:key"}},
		{name: "extra arity", args: []string{"GET", "media:active_colour", "extra"}},
		{name: "nonexact scan pattern", args: []string{"--scan", "--pattern", "voice:room_owner:other*"}},
		{name: "alternate lua", args: []string{"EVAL", "return redis.call('KEYS','*')", "2", "voice:room_owner:00000000-0000-4000-8000-000000000001", "voice:room_live:00000000-0000-4000-8000-000000000001", "blue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := json.Marshal(tc.args)
			require.NoError(t, err)
			line := append([]byte(`{"operation":"redis","args":`), args...)
			line = append(line, '}')
			response := handleControl(context.Background(), controller, 0, line)
			require.Equal(t, "{\"error\":\"control-rejected\"}\n", string(response))
		})
	}
	require.Empty(t, admin.operatorArgs, "rejected operations must not reach Redis")
}

func TestRefusalsAreLoggedWithoutRequestContent(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	controller := newCLITestController(t)
	ctx := context.Background()

	handleControl(ctx, controller, 0, []byte(`{"operation":"replace-ready","slot":"control-plane","containerId":"secret-looking-id"}`))
	handleControl(ctx, controller, 0, []byte(`{"operation":"injected\nline"}`))
	handleAck(ctx, controller, "media-plane", 1002, []byte(`{"version":1,"rotationId":"secret-looking-rotation"}`))
	handleRecoveryControl(ctx, controller, 1000, []byte(`{"operation":"resume"}`))

	require.Equal(t, strings.Join([]string{
		"managed Redis controller refused control request: operation=replace-ready peer_uid=0",
		"managed Redis controller refused control request: operation=other peer_uid=0",
		"managed Redis controller refused ack: operation=other peer_uid=1002",
		"managed Redis controller refused recovery request: operation=resume peer_uid=1000",
	}, "\n"), stripLogPrefixes(logs.String()))

	logs.Reset()
	require.Contains(t, string(handleControl(ctx, controller, 0, []byte(`{"operation":"status"}`))), `"state":"prepared"`)
	require.Empty(t, logs.String(), "an accepted request is not logged")
}

func stripLogPrefixes(out string) string {
	split := strings.Split(strings.TrimSpace(out), "\n")
	lines := make([]string, 0, len(split))
	for _, line := range split {
		if i := strings.Index(line, "managed Redis"); i >= 0 {
			line = line[i:]
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
