package managedredis

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeRedisAdmin struct {
	loadErr            error
	applyLoadThenError bool
	loadCalls          int
	loadedACL          string
	clients            []ClientIdentity
	listClientsHook    func()
	listClientsErr     error
	userExistsErr      error
	users              map[string]bool
	killed             []string
	killErr            error
	keepClientsOnKill  bool
	probeErr           error
	loadHook           func(string)
	operatorArgs       []string
	operatorOutput     string
	operatorErr        error
}

func (f *fakeRedisAdmin) LoadACL(_ context.Context, path string) error {
	f.loadCalls++
	contents, err := readACLFixture(path)
	if err != nil {
		return err
	}
	if f.loadErr != nil && !f.applyLoadThenError {
		return f.loadErr
	}
	f.loadedACL = string(contents)
	f.users = make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(f.loadedACL))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "user" {
			f.users[fields[1]] = !strings.Contains(scanner.Text(), " off")
		}
	}
	if f.loadHook != nil {
		f.loadHook(f.loadedACL)
	}
	if f.loadErr != nil {
		return f.loadErr
	}
	return nil
}

func readACLFixture(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	file, err := root.Open("users.acl")
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	contents, readErr := io.ReadAll(file)
	fileErr := file.Close()
	rootErr := root.Close()
	if err := errors.Join(readErr, fileErr, rootErr); err != nil {
		return nil, err
	}
	return contents, nil
}

func readJournalFixture(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	file, err := root.Open("rotation.json")
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	contents, readErr := io.ReadAll(file)
	fileErr := file.Close()
	rootErr := root.Close()
	if err := errors.Join(readErr, fileErr, rootErr); err != nil {
		return nil, err
	}
	return contents, nil
}

func writeJournalFixture(path string, contents []byte) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	file, err := root.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return errors.Join(err, root.Close())
	}
	_, writeErr := file.Write(contents)
	fileErr := file.Close()
	rootErr := root.Close()
	return errors.Join(writeErr, fileErr, rootErr)
}

func testMarkerPath(t *testing.T, journalPath string) string {
	t.Helper()
	root := filepath.Dir(filepath.Dir(journalPath))
	markerDir := filepath.Join(root, "marker")
	require.NoError(t, os.MkdirAll(markerDir, 0o700))
	return filepath.Join(markerDir, "activated")
}

func readTestFile(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	contents, readErr := io.ReadAll(file)
	fileErr := file.Close()
	rootErr := root.Close()
	if err := errors.Join(readErr, fileErr, rootErr); err != nil {
		return nil, err
	}
	return contents, nil
}

func (f *fakeRedisAdmin) ListClients(context.Context) ([]ClientIdentity, error) {
	if f.listClientsHook != nil {
		f.listClientsHook()
	}
	if f.listClientsErr != nil {
		return nil, f.listClientsErr
	}
	return append([]ClientIdentity(nil), f.clients...), nil
}

func (f *fakeRedisAdmin) ProbeAuth(context.Context, string, string) error { return f.probeErr }

func (f *fakeRedisAdmin) ExecuteOperator(_ context.Context, args []string) (string, error) {
	f.operatorArgs = append([]string(nil), args...)
	return f.operatorOutput, f.operatorErr
}

func (f *fakeRedisAdmin) UserExists(_ context.Context, username string) (bool, error) {
	if f.userExistsErr != nil {
		return false, f.userExistsErr
	}
	return f.users[username], nil
}

func (f *fakeRedisAdmin) KillUser(_ context.Context, username string) error {
	f.killed = append(f.killed, username)
	if f.killErr != nil {
		return f.killErr
	}
	if !f.keepClientsOnKill {
		kept := f.clients[:0]
		for _, client := range f.clients {
			if client.Username != username {
				kept = append(kept, client)
			}
		}
		f.clients = kept
	}
	return nil
}

func newControllerFixture(t *testing.T, admin *fakeRedisAdmin) (*Controller, string) {
	return newControllerFixtureWithNow(t, admin, func() time.Time { return policyTestNow })
}

func TestNewControllerRejectsReservedBootstrapUsernames(t *testing.T) {
	for _, username := range []string{"cv_cp_999", "cv_mp_999"} {
		t.Run(username, func(t *testing.T) {
			root := t.TempDir()
			_, err := NewController(ControllerOptions{
				Admin:       &fakeRedisAdmin{users: map[string]bool{}},
				ACLPath:     filepath.Join(root, "users.acl"),
				JournalPath: filepath.Join(root, "state", "rotation.json"),
				MarkerPath:  filepath.Join(root, "marker", "activated"),
				Bootstrap: AdminBootstrap{
					Version:  1,
					Scope:    SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"},
					Username: username, Password: "synthetic-admin-password",
				},
				Now:          func() time.Time { return policyTestNow },
				ExpectedUIDs: map[string]uint32{controlPlaneService: 1002, mediaPlaneService: 1003},
			})
			require.ErrorIs(t, err, errController)
		})
	}
}

func newControllerFixtureWithNow(t *testing.T, admin *fakeRedisAdmin, now func() time.Time) (*Controller, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "users.acl")
	stateDir := filepath.Join(root, "state")
	require.NoError(t, os.Mkdir(stateDir, 0o700))
	bootstrapScope := SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}
	controller, err := NewController(ControllerOptions{
		Admin: admin, ACLPath: path, JournalPath: filepath.Join(stateDir, "rotation.json"),
		MarkerPath:   testMarkerPath(t, filepath.Join(stateDir, "rotation.json")),
		Bootstrap:    AdminBootstrap{Version: 1, Scope: bootstrapScope, Username: "cv_admin", Password: "synthetic-admin-password"},
		Now:          now,
		ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
	})
	require.NoError(t, err)
	return controller, path
}

func registerControllerInstances(t *testing.T, controller *Controller) {
	t.Helper()
	err := controller.RegisterExpected(context.Background(), "rotation-12", map[string][]string{
		"control-plane": {"cp-a", "cp-b"}, "media-plane": {"mp-a"},
	}, map[string]uint64{"control-plane": 12, "media-plane": 12})
	require.NoError(t, err)
}

func TestControllerBindingsAreSlotScopedDurableAndReplacedOnlyAfterAck(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, aclPath := newControllerFixture(t, admin)
	prepareController(t, controller)
	require.NoError(t, controller.RegisterExpected(context.Background(), "rotation-12", map[string][]string{
		"control-plane": {"cp-a", "cp-b"}, "media-plane": {"mp-a", "mp-blue", "mp-green"},
	}, map[string]uint64{"control-plane": 12, "media-plane": 12}))
	containerID := func(c string) string { return strings.Repeat(c, 64) }
	for _, binding := range []struct{ slot, id, host string }{
		{"control-plane", containerID("a"), "cp-a"},
		{"media-plane", containerID("b"), "mp-a"},
		{"media-plane-blue", containerID("c"), "mp-blue"},
		{"media-plane-green", containerID("d"), "mp-green"},
	} {
		require.NoError(t, controller.BindInstance(context.Background(), binding.slot, binding.id, binding.host, ""))
		if binding.slot == "control-plane" {
			require.NoError(t, controller.BindInstance(context.Background(), binding.slot, binding.id, binding.host, ""),
				"an exact retry is idempotent")
			require.Error(t, controller.BindInstance(context.Background(), "media-plane", binding.id, "mp-duplicate", ""),
				"a container cannot be bound to two slots")
			require.Error(t, controller.BindInstance(context.Background(), "media-plane", containerID("9"), binding.host, ""),
				"an instance cannot be bound to two slots")
			require.Len(t, controller.BindingSnapshot(), 1, "rejected bindings cannot alter the journal")
		}
	}
	require.Error(t, controller.RecordAck(context.Background(), "media-plane-blue", 1003, ack("media-plane", "mp-green")),
		"a blue socket cannot acknowledge the green slot despite sharing the media UID")
	oldID := containerID("a")
	require.Error(t, controller.ReplaceReady("control-plane", oldID), "slot cannot be recreated before its host ACKs")
	require.Error(t, controller.BindInstance(context.Background(), "control-plane", containerID("e"), "cp-b", oldID),
		"rebinding before an accepted old ACK must fail")
	require.NoError(t, controller.RecordAck(context.Background(), "control-plane", 1002, ack("control-plane", "cp-a")))
	require.NoError(t, controller.ReplaceReady("control-plane", oldID))
	require.Error(t, controller.BindInstance(context.Background(), "control-plane", containerID("e"), "cp-b", containerID("f")),
		"replacement must identify the exact previous container")
	require.NoError(t, controller.BindInstance(context.Background(), "control-plane", containerID("e"), "cp-b", oldID))
	require.NoError(t, controller.RecordAck(context.Background(), "control-plane", 1002, ack("control-plane", "cp-b")))
	require.NoError(t, controller.RecordAck(context.Background(), "media-plane", 1003, ack("media-plane", "mp-a")))

	bindings := controller.BindingSnapshot()
	require.Len(t, bindings, 4, "each runtime slot has one current binding")
	require.Equal(t, InstanceBinding{ContainerID: containerID("e"), InstanceID: "cp-b"}, bindings["control-plane"])

	journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
	restarted, err := NewController(ControllerOptions{
		Admin: admin, ACLPath: aclPath, JournalPath: journalPath,
		MarkerPath: testMarkerPath(t, journalPath),
		Bootstrap:  AdminBootstrap{Version: 1, Scope: SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}, Username: "cv_admin", Password: "synthetic-admin-password"},
		Now:        func() time.Time { return policyTestNow }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
	})
	require.NoError(t, err)
	require.NoError(t, restarted.Restore())
	require.Equal(t, bindings, restarted.BindingSnapshot(), "slot container identity survives controller restart")
	journalRaw, err := readJournalFixture(journalPath)
	require.NoError(t, err)
	var journal rotationJournal
	require.NoError(t, decodeJournal(journalRaw, &journal))
	require.Equal(t, []string{"cp-a", "cp-b"}, journal.Expected["control-plane"], "expected host history remains append-only")
	require.Len(t, journal.Acks, 3, "accepted ACK history remains append-only across slot replacement")

	legacyUnbound := map[string]any{}
	require.NoError(t, json.Unmarshal(journalRaw, &legacyUnbound))
	delete(legacyUnbound, "bindings")
	legacyRaw, err := json.Marshal(legacyUnbound)
	require.NoError(t, err)
	require.NoError(t, writeJournalFixture(journalPath, legacyRaw))
	unbound, err := NewController(ControllerOptions{
		Admin: admin, ACLPath: aclPath, JournalPath: journalPath,
		MarkerPath: testMarkerPath(t, journalPath),
		Bootstrap:  AdminBootstrap{Version: 1, Scope: SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}, Username: "cv_admin", Password: "synthetic-admin-password"},
		Now:        func() time.Time { return policyTestNow }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
	})
	require.NoError(t, err)
	require.Error(t, unbound.Restore(), "accepted legacy ACKs without durable slot bindings must fail closed")
}

func TestCompactBindingHistoryKeepsCurrentAndAckProofs(t *testing.T) {
	history := make([]InstanceBinding, 65)
	for index := range history {
		history[index] = InstanceBinding{
			ContainerID: fmt.Sprintf("%064x", index+1),
			InstanceID:  fmt.Sprintf("cp-%d", index+1),
		}
	}
	j := rotationJournal{
		Bindings:  map[string][]InstanceBinding{controlPlaneService: history},
		Acks:      []AppliedAck{{Service: controlPlaneService, InstanceID: history[63].InstanceID}},
		PriorAcks: []AppliedAck{{Service: controlPlaneService, InstanceID: history[0].InstanceID}},
	}
	j.Bindings[controlPlaneService] = compactBindingHistory(j, controlPlaneService)
	require.Equal(t, []InstanceBinding{history[0], history[63], history[64]}, j.Bindings[controlPlaneService])
	require.NoError(t, validateJournalBindings(j))
	require.True(t, journalAckBound(j, j.Acks[0]))
	require.True(t, journalAckBound(j, j.PriorAcks[0]))

	j.Acks = nil
	for _, binding := range history[:64] {
		j.Acks = append(j.Acks, AppliedAck{Service: controlPlaneService, InstanceID: binding.InstanceID})
	}
	j.Bindings[controlPlaneService] = history
	require.Len(t, compactBindingHistory(j, controlPlaneService), 65,
		"a fully referenced history must still fail closed at the journal bound")
}

func TestLivePriorBindingProofKeepsOnlyAckedSlotHeads(t *testing.T) {
	history := make([]InstanceBinding, 64)
	journal := rotationJournal{Bindings: make(map[string][]InstanceBinding)}
	for index := range history {
		history[index] = InstanceBinding{ContainerID: fmt.Sprintf("%064x", index+1), InstanceID: fmt.Sprintf("cp-%d", index+1)}
		journal.Acks = append(journal.Acks, AppliedAck{Service: controlPlaneService, InstanceID: history[index].InstanceID})
	}
	journal.Bindings[controlPlaneService] = history
	for index, slot := range []string{"media-plane-blue", "media-plane-green"} {
		binding := InstanceBinding{ContainerID: fmt.Sprintf("%064x", index+100), InstanceID: slot}
		journal.Bindings[slot] = []InstanceBinding{binding}
		journal.Acks = append(journal.Acks, AppliedAck{Service: mediaPlaneService, InstanceID: slot})
	}
	expected, acks, bindings, err := livePriorBindingProof(journal)
	require.NoError(t, err)
	require.Equal(t, []string{history[63].InstanceID}, expected[controlPlaneService])
	require.Equal(t, []string{"media-plane-blue", "media-plane-green"}, expected[mediaPlaneService])
	require.Len(t, acks, 3)
	require.Equal(t, []InstanceBinding{history[63]}, bindings[controlPlaneService])
	require.Len(t, bindings, 3)

	journal.Acks = append(journal.Acks[:63], journal.Acks[64:]...)
	_, _, _, err = livePriorBindingProof(journal)
	require.Error(t, err, "an unacknowledged slot head must block a new rotation")
}

func ack(service, instance string) AppliedAck {
	username := "cv_cp_12"
	if service == mediaPlaneService {
		username = "cv_mp_12"
	}
	return AppliedAck{Version: 1, RotationID: "rotation-12", Service: service, InstanceID: instance, Generation: 12, Username: username}
}

func recordControllerAck(t *testing.T, controller *Controller, slot string, uid uint32, value AppliedAck) {
	t.Helper()
	bindings := controller.BindingSnapshot()
	replaced := ""
	if current, exists := bindings[slot]; exists && current.InstanceID != value.InstanceID {
		replaced = current.ContainerID
		require.NoError(t, controller.ReplaceReady(slot, replaced))
	}
	digest := sha256.Sum256([]byte(slot + ":" + value.InstanceID))
	require.NoError(t, controller.BindInstance(context.Background(), slot, hex.EncodeToString(digest[:]), value.InstanceID, replaced))
	require.NoError(t, controller.RecordAck(context.Background(), slot, uid, value))
}

func bindControllerInstance(t *testing.T, controller *Controller, slot, instance string) {
	t.Helper()
	digest := sha256.Sum256([]byte(slot + ":" + instance))
	require.NoError(t, controller.BindInstance(context.Background(), slot, hex.EncodeToString(digest[:]), instance, ""))
}

func initialSingleGenerationPolicy() Policy {
	policy := policyTestValue()
	policy.RotationID = "rotation-11"
	for service, value := range policy.Services {
		value.TargetGeneration = 11
		value.Generations = value.Generations[:1]
		policy.Services[service] = value
	}
	return policy
}

func initialSameRotationPolicy() Policy {
	policy := initialSingleGenerationPolicy()
	policy.RotationID = policyTestValue().RotationID
	return policy
}

func ackInitialPolicy(t *testing.T, controller *Controller) {
	t.Helper()
	require.NoError(t, controller.RegisterExpected(context.Background(), "rotation-11", map[string][]string{
		controlPlaneService: {"cp-initial"}, mediaPlaneService: {"mp-initial"},
	}, map[string]uint64{controlPlaneService: 11, mediaPlaneService: 11}))
	recordControllerAck(t, controller, controlPlaneService, 1002, AppliedAck{
		Version: 1, RotationID: "rotation-11", Service: controlPlaneService,
		InstanceID: "cp-initial", Generation: 11, Username: "cv_cp_11",
	})
	recordControllerAck(t, controller, mediaPlaneService, 1003, AppliedAck{
		Version: 1, RotationID: "rotation-11", Service: mediaPlaneService,
		InstanceID: "mp-initial", Generation: 11, Username: "cv_mp_11",
	})
}

func ackAll(t *testing.T, controller *Controller) {
	t.Helper()
	for _, item := range []struct {
		uid uint32
		ack AppliedAck
	}{{1002, ack("control-plane", "cp-a")}, {1002, ack("control-plane", "cp-b")}, {1003, ack("media-plane", "mp-a")}} {
		recordControllerAck(t, controller, item.ack.Service, item.uid, item.ack)
	}
}

func prepareController(t *testing.T, controller *Controller) {
	t.Helper()
	require.NoError(t, controller.PrepareInitial(initialSameRotationPolicy()))
	require.NoError(t, controller.Reconcile(context.Background(), policyTestValue()))
	registerControllerInstances(t, controller)
}

func TestControllerPrepareInitialDisablesDefaultBeforeManagedUsers(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)

	require.NoError(t, controller.PrepareInitial(initialSingleGenerationPolicy()))

	acl, err := readACLFixture(path)
	require.NoError(t, err)
	require.Contains(t, string(acl), "user default off")
	require.Contains(t, string(acl), "user cv_cp_11")
	require.Contains(t, string(acl), "user cv_mp_11")
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o440), info.Mode().Perm(), "controller-owned ACL must be readable by Redis's group")
	aclDir, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	require.EqualValues(t, os.Geteuid(), info.Sys().(*syscall.Stat_t).Uid)
	require.Equal(t, aclDir.Sys().(*syscall.Stat_t).Gid, info.Sys().(*syscall.Stat_t).Gid,
		"ACL leaf must inherit the Redis-reader group from its parent")
	journalPath := filepath.Join(filepath.Dir(path), "state", "rotation.json")
	journalInfo, err := os.Stat(journalPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), journalInfo.Mode().Perm(), "rotation journal must be private to the controller")
	require.NotContains(t, string(acl), "+@all")
	require.NotContains(t, string(acl), "~*")
	require.Zero(t, admin.loadCalls, "initial ACL is written before Redis starts")
}

func TestControllerReconcilePreservesRegisteredAckOnRefresh(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, _ := newControllerFixture(t, admin)
	policy := policyTestValue()
	require.NoError(t, controller.PrepareInitial(initialSameRotationPolicy()))
	require.NoError(t, controller.Reconcile(context.Background(), policy))
	registerControllerInstances(t, controller)
	recordControllerAck(t, controller, "control-plane", 1002, ack("control-plane", "cp-a"))

	require.NoError(t, controller.Reconcile(context.Background(), policy))
	require.Equal(t, 1, admin.loadCalls, "same-policy refresh must not reload")
	state, rotationID, targets := controller.Status()
	require.Equal(t, "prepared", state, "one early ACK must survive initial ACL activation")
	require.Equal(t, "rotation-12", rotationID)
	require.Equal(t, uint64(12), targets["control-plane"])

	require.NoError(t, controller.Reconcile(context.Background(), policy))
	require.Equal(t, 1, admin.loadCalls, "same-policy refresh must not reload or reset progress")
	state, _, _ = controller.Status()
	require.Equal(t, "prepared", state)
	require.Error(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}), "only cp-a acknowledged, so the other expected instances must still block finalization")
	recordControllerAck(t, controller, "control-plane", 1002, ack("control-plane", "cp-b"))
	recordControllerAck(t, controller, "media-plane", 1003, ack("media-plane", "mp-a"))
	state, _, _ = controller.Status()
	require.Equal(t, "promoted", state)
	require.NoError(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}))
}

func TestControllerRejectsLateFinalAckWithoutFreshOverlap(t *testing.T) {
	now := policyTestNow
	controller, aclPath := newControllerFixtureWithNow(t, &fakeRedisAdmin{users: map[string]bool{}}, func() time.Time { return now })
	prepareController(t, controller)
	recordControllerAck(t, controller, controlPlaneService, 1002, ack(controlPlaneService, "cp-a"))
	recordControllerAck(t, controller, controlPlaneService, 1002, ack(controlPlaneService, "cp-b"))
	bindControllerInstance(t, controller, mediaPlaneService, "mp-a")
	now = now.Add(time.Minute + time.Second)

	require.ErrorIs(t, controller.RecordAck(context.Background(), mediaPlaneService, 1003, ack(mediaPlaneService, "mp-a")), errController)
	state, _, _ := controller.Status()
	require.Equal(t, "prepared", state)
	raw, err := readJournalFixture(filepath.Join(filepath.Dir(aclPath), "state", "rotation.json"))
	require.NoError(t, err)
	var journal rotationJournal
	require.NoError(t, json.Unmarshal(raw, &journal))
	require.Len(t, journal.Acks, 2, "late final ACK must not be persisted")
}

func TestControllerKeepsTargetValidThroughAckAndFinalization(t *testing.T) {
	for _, finalize := range []bool{false, true} {
		name := "final ACK"
		if finalize {
			name = "finalization"
		}
		for _, shortService := range []string{controlPlaneService, mediaPlaneService} {
			t.Run(name+"/"+shortService, func(t *testing.T) {
				now := policyTestNow
				controller, _ := newControllerFixtureWithNow(t, &fakeRedisAdmin{users: map[string]bool{}}, func() time.Time { return now })
				require.NoError(t, controller.PrepareInitial(initialSameRotationPolicy()))
				policy := policyTestValue()
				value := policy.Services[shortService]
				value.Generations[1].ValidUntil = now.Add(MinimumOverlap + time.Second)
				policy.Services[shortService] = value
				require.NoError(t, controller.Reconcile(context.Background(), policy))
				require.NoError(t, controller.RegisterExpected(context.Background(), policy.RotationID,
					map[string][]string{controlPlaneService: {"cp-a"}, mediaPlaneService: {"mp-a"}},
					map[string]uint64{controlPlaneService: 12, mediaPlaneService: 12}))
				recordControllerAck(t, controller, controlPlaneService, 1002, ack(controlPlaneService, "cp-a"))
				if finalize {
					recordControllerAck(t, controller, mediaPlaneService, 1003, ack(mediaPlaneService, "mp-a"))
				} else {
					bindControllerInstance(t, controller, mediaPlaneService, "mp-a")
				}
				now = now.Add(2 * time.Second)
				if finalize {
					require.ErrorIs(t, controller.Finalize(context.Background(), policy.RotationID,
						map[string]string{controlPlaneService: "cv_cp_11", mediaPlaneService: "cv_mp_11"}), errController)
				} else {
					require.ErrorIs(t, controller.RecordAck(context.Background(), mediaPlaneService, 1003,
						ack(mediaPlaneService, "mp-a")), errController)
				}
			})
		}
	}
}

func TestReconcileGenerationsCompatibleComparesValidityInstants(t *testing.T) {
	prior := policyTestValue().Services["control-plane"]
	candidate := prior
	candidate.Generations = append([]PolicyGeneration(nil), prior.Generations...)
	candidate.Generations[0].ValidFrom = prior.Generations[0].ValidFrom.In(time.FixedZone("different offset", 2*60*60))
	candidate.Generations[0].ValidUntil = prior.Generations[0].ValidUntil.In(time.FixedZone("different offset", 2*60*60))
	require.True(t, reconcileGenerationsCompatible(prior, candidate))

	candidate.Generations[0].ValidUntil = candidate.Generations[0].ValidUntil.Add(time.Second)
	require.False(t, reconcileGenerationsCompatible(prior, candidate))
}

func TestControllerReconcileAcceptsEquivalentPolicyTimestampSpellings(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, _ := newControllerFixture(t, admin)
	policy := policyTestValue()
	require.NoError(t, controller.PrepareInitial(initialSameRotationPolicy()))
	require.NoError(t, controller.Reconcile(context.Background(), policy))
	registerControllerInstances(t, controller)

	republished := policyTestValue()
	offset := time.FixedZone("same instant", 2*60*60)
	for name, service := range republished.Services {
		for i := range service.Generations {
			service.Generations[i].ValidFrom = service.Generations[i].ValidFrom.In(offset)
			service.Generations[i].ValidUntil = service.Generations[i].ValidUntil.In(offset)
		}
		service.Generations[0], service.Generations[1] = service.Generations[1], service.Generations[0]
		republished.Services[name] = service
	}
	require.NoError(t, controller.Reconcile(context.Background(), republished))
	require.Equal(t, 1, admin.loadCalls, "equivalent instants must not reload the ACL")
	state, _, _ := controller.Status()
	require.Equal(t, "prepared", state)

	changed := republished.Services[controlPlaneService]
	changed.Generations[0].ValidUntil = changed.Generations[0].ValidUntil.Add(time.Nanosecond)
	republished.Services[controlPlaneService] = changed
	require.Error(t, controller.Reconcile(context.Background(), republished), "a changed validity instant must still be rejected")
}

func TestControllerReconcileRequiresFreshPolicyOverlapWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
		want error
	}{
		{name: "exactly 180 seconds remain", at: policyTestNow.Add(time.Minute)},
		{name: "less than 180 seconds remain", at: policyTestNow.Add(121 * time.Second), want: errController},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin := &fakeRedisAdmin{users: map[string]bool{}}
			now := tc.at
			controller, _ := newControllerFixtureWithNow(t, admin, func() time.Time { return now })
			require.NoError(t, controller.PrepareInitial(initialSameRotationPolicy()))

			err := controller.Reconcile(context.Background(), policyTestValue())
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestControllerRejectsUnknownInstanceWrongUIDAndConflictingAck(t *testing.T) {
	controller, _ := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
	prepareController(t, controller)

	bindControllerInstance(t, controller, "control-plane", "cp-a")
	require.Error(t, controller.RecordAck(context.Background(), "control-plane", 1002, ack("control-plane", "not-registered")))
	require.Error(t, controller.RecordAck(context.Background(), "control-plane", 1003, ack("control-plane", "cp-a")))
	require.Error(t, controller.RecordAck(context.Background(), "control-plane", 0, ack("control-plane", "cp-a")))
	require.NoError(t, controller.RecordAck(context.Background(), "control-plane", 1002, ack("control-plane", "cp-a")))
	conflict := ack("control-plane", "cp-a")
	conflict.Generation = 11
	require.Error(t, controller.RecordAck(context.Background(), "control-plane", 1002, conflict))
}

func TestControllerRejectsAckForWrongPolicyUsername(t *testing.T) {
	controller, _ := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
	prepareController(t, controller)
	bindControllerInstance(t, controller, "control-plane", "cp-a")
	var wrong AppliedAck
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"rotationId":"rotation-12","service":"control-plane","instanceId":"cp-a","generation":12,"username":"cv_cp_11"}`), &wrong))
	require.Error(t, controller.RecordAck(context.Background(), "control-plane", 1002, wrong), "an ACK for a different applied Redis username must not count")
}

func TestControllerFinalizeRejectsExpiredValidationDeadline(t *testing.T) {
	now := policyTestNow
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, _ := newControllerFixtureWithNow(t, admin, func() time.Time { return now })
	prepareController(t, controller)
	ackAll(t, controller)
	now = policyTestNow.Add(5 * time.Minute)
	require.Error(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}), "a stale policy validation must never authorize revocation")
	require.Empty(t, admin.killed)
}

func TestControllerFinalizeRejectsDeadlineCrossedDuringClientDrain(t *testing.T) {
	now := policyTestNow
	admin := &fakeRedisAdmin{users: map[string]bool{}, clients: []ClientIdentity{{Username: "cv_cp_11"}}}
	controller, _ := newControllerFixtureWithNow(t, admin, func() time.Time { return now })
	prepareController(t, controller)
	ackAll(t, controller)
	admin.listClientsHook = func() { now = policyTestNow.Add(5 * time.Minute) }
	require.Error(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}), "a slow client listing must not authorize revocation after the deadline")
	require.Empty(t, admin.killed)
}

func TestControllerReconcileRequiresActiveGenerationInNextRotation(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	require.NoError(t, controller.PrepareInitial(initialSingleGenerationPolicy()))
	ackInitialPolicy(t, controller)
	before, err := readACLFixture(path)
	require.NoError(t, err)
	next := nextRecoveryPolicy()
	require.Error(t, controller.Reconcile(context.Background(), next), "new rotation must preserve the active Redis username until every client switches")
	after, err := readACLFixture(path)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

func TestControllerReconcileRequiresHigherTargetInNextRotation(t *testing.T) {
	controller, _ := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
	require.NoError(t, controller.PrepareInitial(initialSingleGenerationPolicy()))
	ackInitialPolicy(t, controller)
	next := initialSingleGenerationPolicy()
	next.RotationID = "rotation-12"
	require.Error(t, controller.Reconcile(context.Background(), next), "a new rotation must advance both service targets")
}

func TestControllerReconcileSameRotationRequiresBothTargetsToAdvance(t *testing.T) {
	for _, unchangedService := range []string{controlPlaneService, mediaPlaneService} {
		t.Run(unchangedService, func(t *testing.T) {
			admin := &fakeRedisAdmin{users: map[string]bool{}}
			controller, path := newControllerFixture(t, admin)
			require.NoError(t, controller.PrepareInitial(initialSameRotationPolicy()))
			before, err := readACLFixture(path)
			require.NoError(t, err)

			candidate := policyTestValue()
			service := candidate.Services[unchangedService]
			service.TargetGeneration = 11
			service.Generations = []PolicyGeneration{{
				Generation: 10, Username: canonicalUsername(unchangedService, 10), PasswordHash: "#aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				ValidFrom: policyTestNow.Add(-2 * time.Hour), ValidUntil: policyTestNow.Add(10 * time.Minute),
			}, service.Generations[0]}
			candidate.Services[unchangedService] = service
			require.NoError(t, controller.validatePolicy(candidate), "the rejected provider policy must otherwise be valid")
			require.Error(t, controller.Reconcile(context.Background(), candidate))
			after, err := readACLFixture(path)
			require.NoError(t, err)
			require.Equal(t, before, after, "rejected rotation must not load or write a rollback ACL")
			require.Zero(t, admin.loadCalls)
		})
	}
}

func TestControllerReconcileRejectsUnprovedMultiStepRotation(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	require.NoError(t, controller.PrepareInitial(initialSingleGenerationPolicy()))
	before, err := readACLFixture(path)
	require.NoError(t, err)

	second := policyTestValue()
	third := nextRecoveryPolicy()
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		value := third.Services[service]
		value.Generations = append([]PolicyGeneration{second.Services[service].Generations[1]}, value.Generations...)
		third.Services[service] = value
	}
	require.Error(t, controller.Reconcile(context.Background(), second), "rotation N+1 needs accepted N ACK proof before ACL LOAD")
	require.Error(t, controller.Reconcile(context.Background(), third), "rotation N+2 cannot leap past unproved N")
	after, err := readACLFixture(path)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "rejected rotations must retain the N ACL")
	require.Zero(t, admin.loadCalls)
}

func TestControllerReconcileRejectsNextRotationBeforeOldGenerationDrain(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	prepareController(t, controller)
	ackAll(t, controller)
	state, _, _ := controller.Status()
	require.Equal(t, "promoted", state, "the new generation is ACKed but the old ACL is not finalized")
	before, err := readACLFixture(path)
	require.NoError(t, err)
	loadCalls := admin.loadCalls

	next := nextRecoveryPolicy()
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		value := next.Services[service]
		value.Generations = append([]PolicyGeneration{policyTestValue().Services[service].Generations[1]}, value.Generations...)
		next.Services[service] = value
	}
	require.Error(t, controller.Reconcile(context.Background(), next), "ACKs alone cannot authorize dropping the old generation before Finalize drains it")
	after, err := readACLFixture(path)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	require.Equal(t, loadCalls, admin.loadCalls)
}

func TestControllerReconcilePreservesPriorAckProofForReplacement(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	prepareController(t, controller)
	ackAll(t, controller)
	require.NoError(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}))
	prior := controller.BindingSnapshot()["control-plane"]
	next := nextRecoveryPolicy()
	for _, service := range []string{"control-plane", "media-plane"} {
		current := controller.policy.Services[service].Generations[0]
		value := next.Services[service]
		value.Generations = append([]PolicyGeneration{current}, value.Generations...)
		next.Services[service] = value
	}
	require.NoError(t, controller.Reconcile(context.Background(), next))
	require.NoError(t, controller.ReplaceReady("control-plane", prior.ContainerID), "a bound current container retains its prior ACK proof across rotation")
	journalRaw, err := readJournalFixture(filepath.Join(filepath.Dir(path), "state", "rotation.json"))
	require.NoError(t, err)
	var journal rotationJournal
	require.NoError(t, decodeJournal(journalRaw, &journal))
	require.Empty(t, journal.Acks, "new rotation must wait for new ACKs")
	require.Equal(t, []string{"cp-b"}, journal.PriorExpected[controlPlaneService], "retired hosts are not live predecessor proof")
	require.Equal(t, []string{"mp-a"}, journal.PriorExpected[mediaPlaneService])
	require.Len(t, journal.PriorAcks, 2, "live predecessor ACK proof must be durable")
	require.Len(t, journal.Bindings[controlPlaneService], 1, "retired binding history is compacted at rotation")
	restored, err := NewController(ControllerOptions{
		Admin: admin, ACLPath: path, JournalPath: filepath.Join(filepath.Dir(path), "state", "rotation.json"),
		MarkerPath: controller.markerPath, Bootstrap: controller.bootstrap, Now: controller.now,
		ExpectedUIDs: map[string]uint32{controlPlaneService: 1002, mediaPlaneService: 1003},
	})
	require.NoError(t, err)
	require.NoError(t, restored.Restore())
	require.NoError(t, restored.ReplaceReady("control-plane", prior.ContainerID), "restart must retain prior ACK and binding proof")
	third := next
	third.RotationID = "rotation-14"
	third.Services = make(map[string]PolicyService, 2)
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		value := next.Services[service]
		username := "cv_cp_14"
		if service == mediaPlaneService {
			username = "cv_mp_14"
		}
		value.TargetGeneration = 14
		value.Generations = []PolicyGeneration{value.Generations[1], {
			Generation: 14, Username: username, PasswordHash: "#bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			ValidFrom: policyTestNow, ValidUntil: policyTestNow.Add(time.Hour),
		}}
		third.Services[service] = value
	}
	require.Error(t, restored.Reconcile(context.Background(), third), "a newer rotation cannot displace unregistered prior host proof")
	require.NoError(t, restored.RegisterExpected(context.Background(), "rotation-13", map[string][]string{
		controlPlaneService: {"cp-b"}, mediaPlaneService: {"mp-a"},
	}, map[string]uint64{controlPlaneService: 13, mediaPlaneService: 13}))
	require.NoError(t, restored.RecordAck(context.Background(), controlPlaneService, 1002, AppliedAck{
		Version: 1, RotationID: "rotation-13", Service: controlPlaneService,
		InstanceID: "cp-b", Generation: 13, Username: "cv_cp_13",
	}))
	journalRaw, err = readJournalFixture(filepath.Join(filepath.Dir(path), "state", "rotation.json"))
	require.NoError(t, err)
	require.NoError(t, decodeJournal(journalRaw, &journal))
	require.Len(t, journal.PriorAcks, 1, "current-generation ACK retires the corresponding prior proof")
	require.Equal(t, mediaPlaneService, journal.PriorAcks[0].Service)
}

func TestControllerFinalizeRequiresEveryExpectedAckAndZeroOldConnections(t *testing.T) {
	now := policyTestNow
	admin := &fakeRedisAdmin{users: map[string]bool{"cv_cp_11": true, "cv_mp_11": true}}
	controller, _ := newControllerFixtureWithNow(t, admin, func() time.Time { return now })
	prepareController(t, controller)
	recordControllerAck(t, controller, "control-plane", 1002, ack("control-plane", "cp-a"))

	err := controller.Finalize(context.Background(), "rotation-12", map[string]string{"control-plane": "cv_cp_11", "media-plane": "cv_mp_11"})
	require.Error(t, err)
	require.Empty(t, admin.killed, "incomplete acknowledgments must not revoke credentials")

	ackAll(t, controller)
	admin.clients = []ClientIdentity{{ID: "redis-client-1", Username: "cv_cp_11"}}
	err = controller.Finalize(context.Background(), "rotation-12", map[string]string{"control-plane": "cv_cp_11", "media-plane": "cv_mp_11"})
	require.Error(t, err, "old client must not be killed until the 30 second drain interval")
	require.Empty(t, admin.killed)
	now = now.Add(30 * time.Second)
	err = controller.Finalize(context.Background(), "rotation-12", map[string]string{"control-plane": "cv_cp_11", "media-plane": "cv_mp_11"})
	require.NoError(t, err)
	require.Equal(t, []string{"cv_cp_11"}, admin.killed)
	require.Empty(t, admin.clients)
}

func TestControllerFinalizeWithZeroOldClientsDoesNotWaitForDrain(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, _ := newControllerFixture(t, admin)
	prepareController(t, controller)
	ackAll(t, controller)
	require.NoError(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}))
	require.Empty(t, admin.killed)
}

func TestControllerReportFailurePreservesProgressAndSamePolicyRecovers(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	prepareController(t, controller)
	ackAll(t, controller)
	before, err := readACLFixture(path)
	require.NoError(t, err)

	controller.ReportFailure("reconcile")
	state, rotationID, targets := controller.Status()
	require.Equal(t, "failed", state)
	require.Equal(t, "rotation-12", rotationID)
	require.Equal(t, uint64(12), targets["control-plane"])

	require.NoError(t, controller.Reconcile(context.Background(), policyTestValue()))
	state, _, _ = controller.Status()
	require.Equal(t, "promoted", state)
	after, err := readACLFixture(path)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "failure recovery retains the active policy ACL")
	require.NoError(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}))
}

func TestControllerExpectedInstanceSetIsBounded(t *testing.T) {
	controller, _ := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
	prepareController(t, controller)
	instances := make([]string, 65)
	for i := range instances {
		instances[i] = "cp-" + strconv.Itoa(i)
	}
	err := controller.RegisterExpected(context.Background(), "rotation-12", map[string][]string{
		"control-plane": instances, "media-plane": {"mp-a"},
	}, map[string]uint64{"control-plane": 12, "media-plane": 12})
	require.Error(t, err, "the externally reported resume snapshot must remain bounded")
	_, _, _, known := controller.StatusSnapshot()
	require.Equal(t, []string{"cp-a", "cp-b"}, known["control-plane"], "rejected expansion must preserve the current set")
}

func TestControllerRegisteringUnacknowledgedInstanceRevertsPromotedState(t *testing.T) {
	controller, _ := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
	prepareController(t, controller)
	ackAll(t, controller)
	state, _, _ := controller.Status()
	require.Equal(t, "promoted", state)

	require.NoError(t, controller.RegisterExpected(context.Background(), "rotation-12", map[string][]string{
		"control-plane": {"cp-a", "cp-b", "cp-c"}, "media-plane": {"mp-a"},
	}, map[string]uint64{"control-plane": 12, "media-plane": 12}))
	state, _, _ = controller.Status()
	require.Equal(t, "prepared", state, "new expected host has not applied the target generation")
	require.Error(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}))
}

func TestControllerFinalizedRegistrationAcceptsOnlyOldAndNewHostnameUnion(t *testing.T) {
	controller, _ := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
	prepareController(t, controller)
	ackAll(t, controller)
	require.NoError(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}))

	require.Error(t, controller.RegisterExpected(context.Background(), "rotation-12", map[string][]string{
		"control-plane": {"cp-a"}, "media-plane": {"mp-a"},
	}, map[string]uint64{"control-plane": 12, "media-plane": 12}), "a stopped rebuild may not shrink the persisted host set")
	require.NoError(t, controller.RegisterExpected(context.Background(), "rotation-12", map[string][]string{
		"control-plane": {"cp-a", "cp-b", "cp-c"}, "media-plane": {"mp-a"},
	}, map[string]uint64{"control-plane": 12, "media-plane": 12}))
	state, _, _ := controller.Status()
	require.Equal(t, "prepared", state, "the new host is part of the durable expected union")
	recordControllerAck(t, controller, "control-plane", 1002, ack("control-plane", "cp-c"))
	state, _, _ = controller.Status()
	require.Equal(t, "finalized", state, "the old CP and media ACKs must survive, and a finalized single-generation policy stays finalized")
}

func TestControllerReconcileLoadFailureRetainsCandidateAndMarksFailed(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	prepareController(t, controller)
	ackAll(t, controller)
	require.NoError(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		controlPlaneService: "cv_cp_11", mediaPlaneService: "cv_mp_11",
	}))
	admin.loadErr = errors.New("synthetic ACL LOAD failure")
	next := policyTestValue()
	next.RotationID = "rotation-13"
	service := next.Services["control-plane"]
	service.TargetGeneration = 13
	service.Generations = []PolicyGeneration{service.Generations[1], {Generation: 13, Username: "cv_cp_13", PasswordHash: "#4444444444444444444444444444444444444444444444444444444444444444", ValidFrom: policyTestNow, ValidUntil: policyTestNow.Add(time.Hour)}}
	next.Services["control-plane"] = service
	service = next.Services["media-plane"]
	service.TargetGeneration = 13
	service.Generations = []PolicyGeneration{service.Generations[1], {Generation: 13, Username: "cv_mp_13", PasswordHash: "#5555555555555555555555555555555555555555555555555555555555555555", ValidFrom: policyTestNow, ValidUntil: policyTestNow.Add(time.Hour)}}
	next.Services["media-plane"] = service

	err := controller.Reconcile(context.Background(), next)
	require.Error(t, err)
	after, readErr := readACLFixture(path)
	require.NoError(t, readErr)
	require.Contains(t, string(after), "user cv_cp_13 ", "keep the candidate ACL after an ambiguous load")
	require.Contains(t, string(after), "user cv_mp_13 ")
	require.NotContains(t, admin.loadedACL, "cv_cp_13")
	state, _, _ := controller.Status()
	require.Equal(t, "failed", state)
}

func TestControllerRestartsJournaledReconcileBeforeServing(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, aclPath := newControllerFixture(t, admin)
	require.NoError(t, controller.PrepareInitial(initialSameRotationPolicy()))
	admin.loadErr = errors.New("synthetic ACL load failure")
	policy := policyTestValue()
	require.Error(t, controller.Reconcile(context.Background(), policy))

	journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
	restarted, err := NewController(ControllerOptions{
		Admin: admin, ACLPath: aclPath, JournalPath: journalPath,
		MarkerPath: testMarkerPath(t, journalPath), Bootstrap: controller.bootstrap,
		Now:          func() time.Time { return policyTestNow },
		ExpectedUIDs: map[string]uint32{controlPlaneService: 1002, mediaPlaneService: 1003},
	})
	require.NoError(t, err)
	require.NoError(t, restarted.Restore())
	require.True(t, restarted.ReconciliationPending())
	require.Error(t, restarted.ValidateRestoredPolicy(policy))
	require.Error(t, restarted.ResumeReconcile(context.Background()))
	require.True(t, restarted.ReconciliationPending(), "failed ACL load must retain the durable intent")
	admin.loadErr = nil
	require.NoError(t, restarted.ResumeReconcile(context.Background()))
	require.False(t, restarted.ReconciliationPending())
	require.NoError(t, restarted.ValidateRestoredPolicy(policy))
	require.True(t, admin.users["cv_cp_11"], "existing user must remain active during overlap")
	require.True(t, admin.users["cv_cp_12"], "accepted target must be loaded")
	raw, err := readJournalFixture(journalPath)
	require.NoError(t, err)
	var journal rotationJournal
	require.NoError(t, decodeJournal(raw, &journal))
	require.Empty(t, journal.Intent)
}

func TestControllerReconcileRejectsGenerationReplay(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	prepareController(t, controller)
	before, err := readACLFixture(path)
	require.NoError(t, err)

	replayed := policyTestValue()
	replayed.RotationID = "rotation-13"
	for _, serviceName := range []string{"control-plane", "media-plane"} {
		service := replayed.Services[serviceName]
		service.TargetGeneration = 11
		service.Generations = service.Generations[:1]
		replayed.Services[serviceName] = service
	}

	err = controller.Reconcile(context.Background(), replayed)
	require.Error(t, err)
	after, readErr := readACLFixture(path)
	require.NoError(t, readErr)
	require.Equal(t, string(before), string(after))
}

func TestControllerReconcileRejectsEqualGenerationValueChange(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	prepareController(t, controller)
	before, err := readACLFixture(path)
	require.NoError(t, err)

	changed := policyTestValue()
	changed.RotationID = "rotation-13"
	for _, serviceName := range []string{"control-plane", "media-plane"} {
		service := changed.Services[serviceName]
		service.Generations[1].PasswordHash = "#aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		changed.Services[serviceName] = service
	}

	err = controller.Reconcile(context.Background(), changed)
	require.Error(t, err)
	after, readErr := readACLFixture(path)
	require.NoError(t, readErr)
	require.Equal(t, string(before), string(after))
}

func TestControllerFinalizeKillFailureKeepsOldACL(t *testing.T) {
	now := policyTestNow
	admin := &fakeRedisAdmin{users: map[string]bool{}, killErr: errors.New("synthetic CLIENT KILL failure")}
	controller, path := newControllerFixtureWithNow(t, admin, func() time.Time { return now })
	prepareController(t, controller)
	ackAll(t, controller)
	admin.clients = []ClientIdentity{{ID: "redis-client-1", Username: "cv_cp_11"}}
	before, err := readACLFixture(path)
	require.NoError(t, err)
	now = now.Add(30 * time.Second)

	err = controller.Finalize(context.Background(), "rotation-12", map[string]string{"control-plane": "cv_cp_11", "media-plane": "cv_mp_11"})
	require.Error(t, err)
	require.Equal(t, []string{"cv_cp_11"}, admin.killed, "this reaches and verifies the kill failure")
	after, readErr := readACLFixture(path)
	require.NoError(t, readErr)
	require.Equal(t, string(before), string(after))
	require.Contains(t, admin.loadedACL, "cv_cp_11")
}

func TestControllerFinalizeRechecksClientsAfterACLLoad(t *testing.T) {
	t.Run("kills socket opened during final ACL load before reporting success", func(t *testing.T) {
		admin := &fakeRedisAdmin{users: map[string]bool{}}
		controller, _ := newControllerFixture(t, admin)
		prepareController(t, controller)
		ackAll(t, controller)
		admin.loadHook = func(acl string) {
			if !strings.Contains(acl, "user cv_cp_11 ") {
				admin.clients = []ClientIdentity{{ID: "raced-client", Username: "cv_cp_11"}}
			}
		}

		err := controller.Finalize(context.Background(), "rotation-12", map[string]string{"control-plane": "cv_cp_11", "media-plane": "cv_mp_11"})
		require.NoError(t, err)
		require.Equal(t, []string{"cv_cp_11"}, admin.killed)
		require.Empty(t, admin.clients, "success requires a post-load zero-client observation")
	})

	t.Run("kill failure reports failed with revoked ACL retained", func(t *testing.T) {
		admin := &fakeRedisAdmin{users: map[string]bool{}, killErr: errors.New("synthetic race kill failure")}
		controller, path := newControllerFixture(t, admin)
		prepareController(t, controller)
		ackAll(t, controller)
		admin.loadHook = func(acl string) {
			if !strings.Contains(acl, "user cv_cp_11 ") {
				admin.clients = []ClientIdentity{{ID: "raced-client", Username: "cv_cp_11"}}
			}
		}

		err := controller.Finalize(context.Background(), "rotation-12", map[string]string{"control-plane": "cv_cp_11", "media-plane": "cv_mp_11"})
		require.Error(t, err)
		state, _, _ := controller.Status()
		require.Equal(t, "failed", state)
		activeACL, readErr := readACLFixture(path)
		require.NoError(t, readErr)
		require.NotContains(t, string(activeACL), "user cv_cp_11 ", "new ACL must remain installed after revocation")
	})
}

func TestControllerFinalizeLoadFailureRetainsRevocationFloorAndMarksFailed(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	prepareController(t, controller)
	ackAll(t, controller)
	admin.loadErr = errors.New("synthetic ACL LOAD failure")

	err := controller.Finalize(context.Background(), "rotation-12", map[string]string{"control-plane": "cv_cp_11", "media-plane": "cv_mp_11"})
	require.Error(t, err)
	activeACL, readErr := readACLFixture(path)
	require.NoError(t, readErr)
	require.NotContains(t, string(activeACL), "user cv_cp_11 ", "keep the revocation floor even if ACL LOAD outcome is unknown")
	require.NotContains(t, string(activeACL), "user cv_mp_11 ")
	state, _, _ := controller.Status()
	require.Equal(t, "failed", state)
	// Redis may or may not have applied the ACL before returning an error.
	require.True(t, admin.users["cv_cp_11"])
	require.True(t, admin.users["cv_mp_11"])
}

func TestControllerFinalizeAmbiguousAppliedLoadKeepsRevocationFloor(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	prepareController(t, controller)
	ackAll(t, controller)
	admin.applyLoadThenError = true
	admin.loadErr = errors.New("context deadline exceeded")

	err := controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	})
	require.Error(t, err)
	activeACL, readErr := readACLFixture(path)
	require.NoError(t, readErr)
	require.NotContains(t, string(activeACL), "user cv_cp_11 ", "do not restore an old user after ACL LOAD may have applied")
	require.NotContains(t, string(activeACL), "user cv_mp_11 ")
	require.False(t, admin.users["cv_cp_11"])
	require.False(t, admin.users["cv_mp_11"])
	state, _, _ := controller.Status()
	require.Equal(t, "failed", state, "an ambiguous ACL LOAD result must require reconciliation")
}

func TestControllerResumeFinalizeRequiresSuccessfulReloadAndCompletesPersistedIntent(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	prepareController(t, controller)
	ackAll(t, controller)
	admin.applyLoadThenError = true
	admin.loadErr = errors.New("context deadline exceeded")
	require.Error(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}))
	state, _, _ := controller.Status()
	require.Equal(t, "failed", state)

	root := filepath.Dir(path)
	stateDir := filepath.Join(root, "state")
	bootstrapScope := SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}
	restarted, err := NewController(ControllerOptions{
		Admin: admin, ACLPath: path, JournalPath: filepath.Join(stateDir, "rotation.json"),
		MarkerPath: testMarkerPath(t, filepath.Join(stateDir, "rotation.json")),
		Bootstrap:  AdminBootstrap{Version: 1, Scope: bootstrapScope, Username: "cv_admin", Password: "synthetic-admin-password"},
		Now:        func() time.Time { return policyTestNow }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
	})
	require.NoError(t, err)
	require.NoError(t, restarted.Restore())
	admin.loadErr = errors.New("synthetic ACL reload failure")
	require.Error(t, restarted.ResumeFinalize(context.Background()))
	state, _, _ = restarted.Status()
	require.Equal(t, "failed", state, "failed reload must retain the persisted deny intent for another retry")
	admin.loadErr = nil
	admin.userExistsErr = errors.New("synthetic ACL lookup failure")
	require.Error(t, restarted.ResumeFinalize(context.Background()))
	admin.userExistsErr = nil
	admin.loadHook = func(string) { admin.users["cv_cp_11"] = true }
	require.Error(t, restarted.ResumeFinalize(context.Background()), "an old user still enabled after ACL LOAD must keep finalization pending")
	admin.loadHook = nil
	admin.loadHook = func(string) { admin.users["cv_cp_12"] = false }
	require.Error(t, restarted.ResumeFinalize(context.Background()), "a missing replacement user must keep finalization pending")
	admin.loadHook = nil
	admin.listClientsErr = errors.New("synthetic client listing failure")
	require.Error(t, restarted.ResumeFinalize(context.Background()))
	admin.listClientsErr = nil
	admin.clients = []ClientIdentity{{Username: "cv_cp_11"}}
	admin.killErr = errors.New("synthetic client revocation failure")
	require.Error(t, restarted.ResumeFinalize(context.Background()))
	admin.killErr = nil
	admin.keepClientsOnKill = true
	require.Error(t, restarted.ResumeFinalize(context.Background()), "a client that survives explicit revocation must keep finalization pending")
	admin.keepClientsOnKill = false
	admin.clients = nil
	originalJournalPath := restarted.journalPath
	restarted.journalPath = filepath.Join(root, "missing", "rotation.json")
	require.Error(t, restarted.ResumeFinalize(context.Background()), "failed durable completion must not report finalization success")
	restarted.journalPath = originalJournalPath
	require.NoError(t, restarted.ResumeFinalize(context.Background()))
	state, _, _ = restarted.Status()
	require.Equal(t, "finalized", state)
	require.Error(t, restarted.ValidateRestoredPolicy(policyTestValue()),
		"a normal restart must refuse the provider's stale overlap after recovery")
	require.NoError(t, restarted.ValidateRestoredPolicy(restarted.policy))
	acl, err := readACLFixture(path)
	require.NoError(t, err)
	require.NotContains(t, string(acl), "user cv_cp_11 ")
	require.NotContains(t, string(acl), "user cv_mp_11 ")
}

func TestControllerResumeFinalizeRejectsWithoutPersistedRevocationIntent(t *testing.T) {
	controller, _ := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
	require.Error(t, controller.ResumeFinalize(context.Background()))
}

func TestControllerWriteACLFailurePreservesExistingTarget(t *testing.T) {
	controller, aclPath := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
	require.NoError(t, os.Mkdir(aclPath, 0o700))

	require.Error(t, controller.writeACL([]byte("new ACL\n")))
	info, err := os.Stat(aclPath)
	require.NoError(t, err)
	require.True(t, info.IsDir(), "failed atomic replacement must preserve the existing target")
	entries, err := os.ReadDir(filepath.Dir(aclPath))
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), ".users-acl-"), "failed writes must clean up their temporary file")
	}
}

func TestControllerJournalRestorePreservesRevocationAndAckFence(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	now := policyTestNow
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	require.NoError(t, os.Mkdir(stateDir, 0o700))
	aclPath := filepath.Join(root, "users.acl")
	journalPath := filepath.Join(stateDir, "rotation.json")
	bootstrapScope := SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}
	newController := func() *Controller {
		controller, err := NewController(ControllerOptions{
			Admin: admin, ACLPath: aclPath, JournalPath: journalPath,
			MarkerPath: testMarkerPath(t, journalPath),
			Bootstrap:  AdminBootstrap{Version: 1, Scope: bootstrapScope, Username: "cv_admin", Password: "synthetic-admin-password"},
			Now:        func() time.Time { return now }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
		})
		require.NoError(t, err)
		return controller
	}

	initial := policyTestValue()
	initial.RotationID = "rotation-12"
	for service, config := range initial.Services {
		config.TargetGeneration = 11
		config.Generations = config.Generations[:1]
		initial.Services[service] = config
	}
	controller := newController()
	require.NoError(t, controller.PrepareInitial(initial))
	overlap := policyTestValue()
	require.NoError(t, controller.Reconcile(context.Background(), overlap))
	now = policyTestNow.Add(30 * time.Second)
	require.NoError(t, controller.Reconcile(context.Background(), overlap), "refreshing an unchanged policy does not restart its overlap window")
	require.NoError(t, controller.RegisterExpected(context.Background(), "rotation-12", map[string][]string{
		"control-plane": {"cp-a", "cp-b"}, "media-plane": {"mp-a"},
	}, map[string]uint64{"control-plane": 12, "media-plane": 12}))
	recordControllerAck(t, controller, "control-plane", 1002, ack("control-plane", "cp-a"))

	now = policyTestNow.Add(59 * time.Second)
	restarted := newController()
	require.NoError(t, restarted.Restore())
	require.Error(t, restarted.RegisterExpected(context.Background(), "rotation-12", map[string][]string{
		"control-plane": {"cp-a"}, "media-plane": {"mp-a"},
	}, map[string]uint64{"control-plane": 12, "media-plane": 12}), "restart must not forget an unacknowledged expected host")
	require.Error(t, restarted.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}), "the persisted partial ACK set must still block revocation")
	recordControllerAck(t, restarted, "control-plane", 1002, ack("control-plane", "cp-b"))
	recordControllerAck(t, restarted, "media-plane", 1003, ack("media-plane", "mp-a"))
	require.NoError(t, restarted.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}))

	afterFinalize := newController()
	require.NoError(t, afterFinalize.Restore())
	require.ErrorIs(t, afterFinalize.Reconcile(context.Background(), overlap), ErrPolicyStale, "stale provider overlap must not restore a revoked username")
	activeACL, err := readACLFixture(aclPath)
	require.NoError(t, err)
	require.NotContains(t, string(activeACL), "user cv_cp_11 ")
	require.NotContains(t, string(activeACL), "user cv_mp_11 ")
}

func TestControllerRestoreAcceptsExpiredOverlapFromIntactACLWithoutLosingAckFence(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	now := policyTestNow
	controller, aclPath := newControllerFixtureWithNow(t, admin, func() time.Time { return now })
	prepareController(t, controller)
	recordControllerAck(t, controller, "control-plane", 1002, ack("control-plane", "cp-a"))

	now = policyTestNow.Add(5 * time.Minute)
	journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
	options := ControllerOptions{
		Admin: admin, ACLPath: aclPath, JournalPath: journalPath,
		MarkerPath:   testMarkerPath(t, journalPath),
		Bootstrap:    AdminBootstrap{Version: 1, Scope: SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}, Username: "cv_admin", Password: "synthetic-admin-password"},
		Now:          func() time.Time { return now },
		ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
	}
	restarted, err := NewController(options)
	require.NoError(t, err)
	journalBeforeRestore, err := readJournalFixture(journalPath)
	require.NoError(t, err)
	require.NoError(t, restarted.Restore(), "expired generations in an accepted journal remain structurally valid while its ACL is intact")
	journalAfterRestore, err := readJournalFixture(journalPath)
	require.NoError(t, err)
	require.Equal(t, string(journalBeforeRestore), string(journalAfterRestore), "restore does not rewrite the journal or reset its validation time")

	require.Error(t, restarted.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}), "the persisted expected host set and partial ACK must still block finalization")
	recordControllerAck(t, restarted, "control-plane", 1002, ack("control-plane", "cp-b"))
	bindControllerInstance(t, restarted, mediaPlaneService, "mp-a")
	require.ErrorIs(t, restarted.RecordAck(context.Background(), mediaPlaneService, 1003, ack(mediaPlaneService, "mp-a")), errController,
		"restored late ACK must not promote an expired overlap")
	require.Error(t, restarted.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}), "restored ACKs cannot extend the original five-minute validation deadline")

	retained, err := readACLFixture(aclPath)
	require.NoError(t, err)
	require.Contains(t, string(retained), "user cv_cp_11 ")
	require.Contains(t, string(retained), "user cv_mp_11 ")
	beforeStaleRefresh := string(retained)
	require.Error(t, restarted.Reconcile(context.Background(), policyTestValue()), "an expired provider overlap cannot be loaded again")
	afterStaleRefresh, err := readACLFixture(aclPath)
	require.NoError(t, err)
	require.Equal(t, beforeStaleRefresh, string(afterStaleRefresh), "rejected stale policy must not rewrite the ACL")
}

func TestControllerRestoreDoesNotRerenderExpiredOverlapWhenACLIsMissing(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	now := policyTestNow
	controller, aclPath := newControllerFixtureWithNow(t, admin, func() time.Time { return now })
	prepareController(t, controller)
	require.NoError(t, os.Remove(aclPath))
	now = policyTestNow.Add(5 * time.Minute)
	restarted, err := NewController(ControllerOptions{
		Admin: admin, ACLPath: aclPath, JournalPath: filepath.Join(filepath.Dir(aclPath), "state", "rotation.json"),
		MarkerPath:   testMarkerPath(t, filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")),
		Bootstrap:    AdminBootstrap{Version: 1, Scope: SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}, Username: "cv_admin", Password: "synthetic-admin-password"},
		Now:          func() time.Time { return now },
		ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
	})
	require.NoError(t, err)
	require.Error(t, restarted.Restore(), "a missing ACL cannot be reconstructed with an expired generation")
	_, err = os.Lstat(aclPath)
	require.ErrorIs(t, err, os.ErrNotExist, "failed restore must leave the missing ACL untouched")
}

func TestControllerRestoreFailsClosedForMissingOrCorruptJournal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "existing ACL without journal", mutate: func(t *testing.T, journal string) {
			require.NoError(t, os.Remove(journal))
		}},
		{name: "duplicate journal keys", mutate: func(t *testing.T, journal string) {
			require.NoError(t, os.WriteFile(journal, []byte(`{"version":1,"version":1}`), 0o600))
			require.NoError(t, os.Chmod(journal, 0o600))
		}},
		{name: "noncanonical journal mode", mutate: func(t *testing.T, journal string) {
			require.NoError(t, os.Chmod(journal, 0o400))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin := &fakeRedisAdmin{users: map[string]bool{}}
			controller, aclPath := newControllerFixture(t, admin)
			require.NoError(t, controller.PrepareInitial(initialSingleGenerationPolicy()))
			journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
			tc.mutate(t, journalPath)
			// Reuse the existing protected paths to model a container restart.
			options := ControllerOptions{
				Admin: admin, ACLPath: aclPath, JournalPath: journalPath,
				MarkerPath: testMarkerPath(t, journalPath),
				Bootstrap:  AdminBootstrap{Version: 1, Scope: SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}, Username: "cv_admin", Password: "synthetic-admin-password"},
				Now:        func() time.Time { return policyTestNow }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
			}
			restarted, err := NewController(options)
			require.NoError(t, err)
			require.Error(t, restarted.Restore(), "existing ACL without a valid journal must never infer empty rotation state")
		})
	}
}

func nextRecoveryPolicy() Policy {
	policy := policyTestValue()
	policy.RotationID = "rotation-13"
	for _, service := range []string{"control-plane", "media-plane"} {
		config := policy.Services[service]
		prefix, digit := "cv_cp_", "13"
		if service == "media-plane" {
			prefix = "cv_mp_"
		}
		config.TargetGeneration = 13
		config.Generations = []PolicyGeneration{{
			Generation: 13, Username: prefix + digit,
			PasswordHash: "#aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ValidFrom:    policyTestNow.Add(-time.Minute), ValidUntil: policyTestNow.Add(time.Hour),
		}}
		policy.Services[service] = config
	}
	return policy
}

func TestControllerPrepareRecoveryAdvancesRevocationFloor(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	prepareController(t, controller)
	ackAll(t, controller)
	require.NoError(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}))

	next := nextRecoveryPolicy()
	require.NoError(t, controller.PrepareRecovery(next))
	state, rotation, targets := controller.Status()
	require.Equal(t, "prepared", state)
	require.Equal(t, "rotation-13", rotation)
	require.Equal(t, uint64(13), targets["control-plane"])
	acl, err := readACLFixture(path)
	require.NoError(t, err)
	require.NotContains(t, string(acl), "user cv_cp_12 ", "stopped recovery must revoke the formerly active generation")
	require.Contains(t, string(acl), "user cv_cp_13 ")
	require.Error(t, controller.Reconcile(context.Background(), policyTestValue()), "stale provider policy cannot restore a revoked generation")
}

func TestControllerPrepareRecoveryCompletesPersistedExactIntent(t *testing.T) {
	newInterruptedRecovery := func(t *testing.T) (*Controller, *fakeRedisAdmin, string, Policy) {
		t.Helper()
		admin := &fakeRedisAdmin{users: map[string]bool{}}
		controller, aclPath := newControllerFixture(t, admin)
		prepareController(t, controller)
		ackAll(t, controller)
		require.NoError(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
			controlPlaneService: "cv_cp_11", mediaPlaneService: "cv_mp_11",
		}))
		next := nextRecoveryPolicy()
		controller.aclUID = os.Geteuid() + 1
		require.Error(t, controller.PrepareRecovery(next), "synthetic ACL ownership mismatch interrupts after recovery intent persistence")
		controller.aclUID = 0
		return controller, admin, aclPath, next
	}

	t.Run("same process retries only the exact policy", func(t *testing.T) {
		controller, _, aclPath, next := newInterruptedRecovery(t)
		changed := next
		changed.RotationID = "rotation-other"
		require.Error(t, controller.PrepareRecovery(changed))
		require.NoError(t, controller.PrepareRecovery(next))
		acl, err := readACLFixture(aclPath)
		require.NoError(t, err)
		require.Contains(t, string(acl), "user cv_cp_13 ")
		require.NotContains(t, string(acl), "user cv_cp_12 ")
	})

	t.Run("restart completes the persisted exact intent", func(t *testing.T) {
		controller, admin, aclPath, next := newInterruptedRecovery(t)
		journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
		restarted, err := NewController(ControllerOptions{
			Admin: admin, ACLPath: aclPath, JournalPath: journalPath, MarkerPath: testMarkerPath(t, journalPath),
			Bootstrap: controller.bootstrap, Now: func() time.Time { return policyTestNow },
			ExpectedUIDs: map[string]uint32{controlPlaneService: 1002, mediaPlaneService: 1003},
		})
		require.NoError(t, err)
		require.NoError(t, restarted.Restore())
		require.NoError(t, restarted.ValidateRestoredPolicy(next))
		journalRaw, err := readJournalFixture(journalPath)
		require.NoError(t, err)
		var journal rotationJournal
		require.NoError(t, decodeJournal(journalRaw, &journal))
		require.Empty(t, journal.Intent)
		require.Empty(t, journal.FailureCode)
	})
}

func TestControllerLostStateRecoveryCompletesPersistedExactIntent(t *testing.T) {
	newInterruptedRecovery := func(t *testing.T) (*Controller, *fakeRedisAdmin, string, Policy) {
		t.Helper()
		admin := &fakeRedisAdmin{users: map[string]bool{}}
		controller, aclPath := newControllerFixture(t, admin)
		policy := nextRecoveryPolicy()
		proof, err := json.Marshal(map[string]any{
			"version": 1, "scope": policy.Scope, "rotationId": policy.RotationID,
			"inventoryComplete": true, "reviewedBy": "operator@example.invalid",
			"reviewedAt": policyTestNow.Format(time.RFC3339),
			"services": map[string]any{
				controlPlaneService: map[string]any{"maxGeneration": 12},
				mediaPlaneService:   map[string]any{"maxGeneration": 12},
			},
		})
		require.NoError(t, err)
		controller.aclUID = os.Geteuid() + 1
		require.Error(t, controller.PrepareLostStateRecovery(policy, proof), "synthetic ACL ownership mismatch interrupts after recovery intent persistence")
		controller.aclUID = 0
		return controller, admin, aclPath, policy
	}

	t.Run("same process retries only the exact policy", func(t *testing.T) {
		controller, _, aclPath, policy := newInterruptedRecovery(t)
		changed := policy
		changed.RotationID = "rotation-other"
		require.Error(t, controller.PrepareLostStateRecovery(changed, nil))
		require.NoError(t, controller.PrepareLostStateRecovery(policy, nil), "persisted proof permits exact-policy completion without rereading it")
		acl, err := readACLFixture(aclPath)
		require.NoError(t, err)
		require.Contains(t, string(acl), "user cv_cp_13 ")
	})

	t.Run("fresh recovery process completes the persisted exact intent", func(t *testing.T) {
		controller, admin, aclPath, policy := newInterruptedRecovery(t)
		journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
		restarted, err := NewController(ControllerOptions{
			Admin: admin, ACLPath: aclPath, JournalPath: journalPath, MarkerPath: testMarkerPath(t, journalPath),
			Bootstrap: controller.bootstrap, Now: func() time.Time { return policyTestNow },
			ExpectedUIDs: map[string]uint32{controlPlaneService: 1002, mediaPlaneService: 1003},
		})
		require.NoError(t, err)
		require.NoError(t, restarted.PrepareLostStateRecovery(policy, nil))
		journalRaw, err := readJournalFixture(journalPath)
		require.NoError(t, err)
		var journal rotationJournal
		require.NoError(t, decodeJournal(journalRaw, &journal))
		require.Empty(t, journal.Intent)
		require.Empty(t, journal.FailureCode)
	})
}

func TestControllerLostStateRecoveryRequiresFreshIdentityEvidence(t *testing.T) {
	t.Run("missing evidence refuses without creating either RAM file", func(t *testing.T) {
		admin := &fakeRedisAdmin{users: map[string]bool{}}
		controller, aclPath := newControllerFixture(t, admin)
		policy := nextRecoveryPolicy()
		require.Error(t, controller.PrepareLostStateRecovery(policy, nil))
		_, err := os.Lstat(aclPath)
		require.ErrorIs(t, err, os.ErrNotExist)
		_, err = os.Lstat(filepath.Join(filepath.Dir(aclPath), "state", "rotation.json"))
		require.ErrorIs(t, err, os.ErrNotExist)
		require.Zero(t, admin.loadCalls)
	})

	t.Run("existing ACL or journal is never overwritten", func(t *testing.T) {
		for _, existing := range []string{"ACL", "journal"} {
			t.Run(existing, func(t *testing.T) {
				admin := &fakeRedisAdmin{users: map[string]bool{}}
				controller, aclPath := newControllerFixture(t, admin)
				journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
				path := aclPath
				if existing == "journal" {
					path = journalPath
				}
				before := []byte("preserve-existing-" + existing)
				require.NoError(t, os.WriteFile(path, before, 0o600))
				require.Error(t, controller.PrepareLostStateRecovery(nextRecoveryPolicy(), []byte("{}")))
				after, err := readTestFile(t, path)
				require.NoError(t, err)
				require.Equal(t, before, after)
				require.Zero(t, admin.loadCalls)
			})
		}
	})

	t.Run("candidate generation at or below prior floor refuses before creating state", func(t *testing.T) {
		admin := &fakeRedisAdmin{users: map[string]bool{}}
		controller, aclPath := newControllerFixture(t, admin)
		policy := nextRecoveryPolicy()
		proof, err := json.Marshal(map[string]any{
			"version": 1, "scope": policy.Scope, "rotationId": policy.RotationID,
			"inventoryComplete": true, "reviewedBy": "operator@example.invalid",
			"reviewedAt": policyTestNow.Format(time.RFC3339),
			"services": map[string]any{
				"control-plane": map[string]any{"maxGeneration": 13},
				"media-plane":   map[string]any{"maxGeneration": 13},
			},
		})
		require.NoError(t, err)
		require.Error(t, controller.PrepareLostStateRecovery(policy, proof))
		_, err = os.Lstat(aclPath)
		require.ErrorIs(t, err, os.ErrNotExist)
		_, err = os.Lstat(filepath.Join(filepath.Dir(aclPath), "state", "rotation.json"))
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("legacy usernames field refuses before creating state", func(t *testing.T) {
		admin := &fakeRedisAdmin{users: map[string]bool{}}
		controller, aclPath := newControllerFixture(t, admin)
		policy := nextRecoveryPolicy()
		proof, err := json.Marshal(map[string]any{
			"version": 1, "scope": policy.Scope, "rotationId": policy.RotationID,
			"inventoryComplete": true, "reviewedBy": "operator@example.invalid",
			"reviewedAt": policyTestNow.Format(time.RFC3339),
			"services": map[string]any{
				"control-plane": map[string]any{"maxGeneration": 12, "usernames": []string{"cv_cp_12"}},
				"media-plane":   map[string]any{"maxGeneration": 12, "usernames": []string{"cv_mp_12"}},
			},
		})
		require.NoError(t, err)
		require.Error(t, controller.PrepareLostStateRecovery(policy, proof))
		_, err = os.Lstat(aclPath)
		require.ErrorIs(t, err, os.ErrNotExist)
		_, err = os.Lstat(filepath.Join(filepath.Dir(aclPath), "state", "rotation.json"))
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("reviewed fresh identities establish revocation floor before ACL", func(t *testing.T) {
		admin := &fakeRedisAdmin{users: map[string]bool{}}
		controller, aclPath := newControllerFixture(t, admin)
		policy := nextRecoveryPolicy()
		proof, err := json.Marshal(map[string]any{
			"version": 1, "scope": policy.Scope, "rotationId": policy.RotationID,
			"inventoryComplete": true, "reviewedBy": "operator@example.invalid",
			"reviewedAt": policyTestNow.Format(time.RFC3339),
			"services": map[string]any{
				"control-plane": map[string]any{"maxGeneration": 12},
				"media-plane":   map[string]any{"maxGeneration": 12},
			},
		})
		require.NoError(t, err)
		require.NoError(t, controller.PrepareLostStateRecovery(policy, proof))

		journalRaw, err := os.ReadFile(filepath.Join(filepath.Dir(aclPath), "state", "rotation.json"))
		require.NoError(t, err)
		var journal rotationJournal
		require.NoError(t, decodeJournal(journalRaw, &journal))
		require.Equal(t, uint64(12), journal.Revoked["control-plane"].Generation)
		require.Equal(t, uint64(12), journal.Revoked["media-plane"].Generation)
		acl, err := readACLFixture(aclPath)
		require.NoError(t, err)
		require.NotContains(t, string(acl), "user cv_cp_12 ")
		require.NotContains(t, string(acl), "user cv_mp_12 ")
		require.Contains(t, string(acl), "user cv_cp_13 ")
		require.Contains(t, string(acl), "user cv_mp_13 ")
	})
}

func TestControllerActivationMarkerPreventsImplicitReinitializationAfterRAMLoss(t *testing.T) {
	t.Run("marker creation failure precedes journal and ACL effects", func(t *testing.T) {
		admin := &fakeRedisAdmin{users: map[string]bool{}}
		controller, aclPath := newControllerFixture(t, admin)
		markerPath := filepath.Join(filepath.Dir(aclPath), "marker", "activated")
		target := filepath.Join(t.TempDir(), "marker-target")
		require.NoError(t, os.WriteFile(target, []byte("sentinel"), 0o600))
		require.NoError(t, os.Symlink(target, markerPath))
		require.Error(t, controller.PrepareInitial(nextRecoveryPolicy()))
		_, err := os.Lstat(filepath.Join(filepath.Dir(aclPath), "state", "rotation.json"))
		require.ErrorIs(t, err, os.ErrNotExist)
		_, err = os.Lstat(aclPath)
		require.ErrorIs(t, err, os.ErrNotExist)
		require.Zero(t, admin.loadCalls)
	})

	t.Run("crash after initializing marker can retry exact first activation", func(t *testing.T) {
		admin := &fakeRedisAdmin{users: map[string]bool{}}
		controller, aclPath := newControllerFixture(t, admin)
		initial := policyTestValue()
		initial.RotationID = "rotation-11"
		for service, config := range initial.Services {
			config.TargetGeneration = 11
			config.Generations = config.Generations[:1]
			initial.Services[service] = config
		}
		journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
		stateDir := filepath.Dir(journalPath)
		require.NoError(t, os.Remove(stateDir))
		require.NoError(t, os.Mkdir(stateDir, 0o750))
		require.Error(t, controller.PrepareInitial(initial), "journal failure occurs after durable activation marker creation")
		markerPath := filepath.Join(filepath.Dir(aclPath), "marker", "activated")
		_, err := os.Stat(markerPath)
		require.NoError(t, err)
		_, err = os.Lstat(journalPath)
		require.ErrorIs(t, err, os.ErrNotExist)
		_, err = os.Lstat(aclPath)
		require.ErrorIs(t, err, os.ErrNotExist)
		require.NoError(t, os.Remove(stateDir))
		require.NoError(t, os.Mkdir(stateDir, 0o700))
		restarted, err := NewController(ControllerOptions{
			Admin: admin, ACLPath: aclPath, JournalPath: journalPath,
			MarkerPath: testMarkerPath(t, journalPath),
			Bootstrap:  AdminBootstrap{Version: 1, Scope: SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}, Username: "cv_admin", Password: "synthetic-admin-password"},
			Now:        func() time.Time { return policyTestNow }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
		})
		require.NoError(t, err)
		require.Error(t, restarted.Restore(), "ordinary serving rejects an incomplete first activation")
		require.NoError(t, restarted.PrepareInitial(initial), "the exact stopped first-activation retry completes")
		require.NoError(t, restarted.requireActivationMarker())
		_, err = os.Lstat(journalPath)
		require.NoError(t, err)
		_, err = os.Lstat(aclPath)
		require.NoError(t, err)
	})

	t.Run("marker survives RAM loss and requires reviewed recovery", func(t *testing.T) {
		admin := &fakeRedisAdmin{users: map[string]bool{}}
		controller, aclPath := newControllerFixture(t, admin)
		initial := policyTestValue()
		initial.RotationID = "rotation-11"
		for service, config := range initial.Services {
			config.TargetGeneration = 11
			config.Generations = config.Generations[:1]
			initial.Services[service] = config
		}
		require.NoError(t, controller.PrepareInitial(initial))
		markerPath := filepath.Join(filepath.Dir(aclPath), "marker", "activated")
		info, err := os.Lstat(markerPath)
		require.NoError(t, err)
		require.True(t, info.Mode().IsRegular())
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		require.NoError(t, os.Remove(aclPath))
		require.NoError(t, os.Remove(filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")))
		newController := func() *Controller {
			restarted, err := NewController(ControllerOptions{
				Admin: admin, ACLPath: aclPath,
				JournalPath: filepath.Join(filepath.Dir(aclPath), "state", "rotation.json"),
				MarkerPath:  markerPath,
				Bootstrap:   AdminBootstrap{Version: 1, Scope: SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}, Username: "cv_admin", Password: "synthetic-admin-password"},
				Now:         func() time.Time { return policyTestNow }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
			})
			require.NoError(t, err)
			return restarted
		}
		require.Error(t, newController().PrepareInitial(initial), "a surviving activation marker prevents repeating first activation after restart")
		_, err = os.Lstat(aclPath)
		require.ErrorIs(t, err, os.ErrNotExist)
		_, err = os.Lstat(filepath.Join(filepath.Dir(aclPath), "state", "rotation.json"))
		require.ErrorIs(t, err, os.ErrNotExist)
		policy := nextRecoveryPolicy()
		proof, err := json.Marshal(map[string]any{
			"version": 1, "scope": policy.Scope, "rotationId": policy.RotationID,
			"inventoryComplete": true, "reviewedBy": "operator@example.invalid",
			"reviewedAt": policyTestNow.Format(time.RFC3339),
			"services": map[string]any{
				"control-plane": map[string]any{"maxGeneration": 12},
				"media-plane":   map[string]any{"maxGeneration": 12},
			},
		})
		require.NoError(t, err)
		require.NoError(t, newController().PrepareLostStateRecovery(policy, proof))
		_, err = os.Stat(markerPath)
		require.NoError(t, err, "reviewed recovery retains the durable activation marker")
	})
}

func TestControllerFirstActivationRetriesOnlyExactPartialState(t *testing.T) {
	for _, phase := range []string{"marker", "journal", "acl"} {
		t.Run(phase, func(t *testing.T) {
			admin := &fakeRedisAdmin{users: map[string]bool{}}
			controller, aclPath := newControllerFixture(t, admin)
			policy := initialSingleGenerationPolicy()
			require.NoError(t, controller.createMarker(initializingMarkerContents))
			journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
			if phase != "marker" {
				j := controller.snapshotJournalLocked()
				j.Effective, j.Phase, j.ValidatedAt = policy, "prepared", policyTestNow
				require.NoError(t, controller.persistJournal(j))
				controller.applyJournalLocked(j)
				if phase == "acl" {
					require.NoError(t, controller.writePolicy(policy))
				}
			}
			restarted, err := NewController(ControllerOptions{
				Admin: admin, ACLPath: aclPath, JournalPath: journalPath,
				MarkerPath: testMarkerPath(t, journalPath),
				Bootstrap: AdminBootstrap{Version: 1, Scope: SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"},
					Username: "cv_admin", Password: "synthetic-admin-password"},
				Now: func() time.Time { return policyTestNow }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
			})
			require.NoError(t, err)
			require.Error(t, restarted.Restore(), "normal startup must reject an initializing marker")
			if phase != "marker" {
				changed := policy
				changed.RotationID = "rotation-other"
				require.Error(t, restarted.PrepareInitial(changed), "a partial journal is bound to the exact provider policy")
			}
			require.NoError(t, restarted.PrepareInitial(policy))
			require.NoError(t, restarted.requireActivationMarker())
			require.NoError(t, restarted.Restore())
		})
	}
}

func TestControllerPrepareRecoveryRejectsInvalidPolicyWithoutLosingExpectedHosts(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, path := newControllerFixture(t, admin)
	prepareController(t, controller)
	ackAll(t, controller)
	require.NoError(t, controller.Finalize(context.Background(), "rotation-12", map[string]string{
		"control-plane": "cv_cp_11", "media-plane": "cv_mp_11",
	}))
	journalPath := filepath.Join(filepath.Dir(path), "state", "rotation.json")
	beforeJournal, err := readJournalFixture(journalPath)
	require.NoError(t, err)
	beforeACL, err := readACLFixture(path)
	require.NoError(t, err)

	invalid := nextRecoveryPolicy()
	config := invalid.Services["control-plane"]
	config.Generations[0].Username = "cv_cp_12"
	invalid.Services["control-plane"] = config
	require.Error(t, controller.PrepareRecovery(invalid))
	afterJournal, err := readJournalFixture(journalPath)
	require.NoError(t, err)
	afterACL, err := readACLFixture(path)
	require.NoError(t, err)
	require.Equal(t, string(beforeJournal), string(afterJournal))
	require.Equal(t, string(beforeACL), string(afterACL))

	// Reconstruct with the existing journal and ACL paths.
	restarted, err := NewController(ControllerOptions{
		Admin: admin, ACLPath: path, JournalPath: journalPath,
		MarkerPath: testMarkerPath(t, journalPath),
		Bootstrap:  AdminBootstrap{Version: 1, Scope: SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}, Username: "cv_admin", Password: "synthetic-admin-password"},
		Now:        func() time.Time { return policyTestNow }, ExpectedUIDs: map[string]uint32{"control-plane": 1002, "media-plane": 1003},
	})
	require.NoError(t, err)
	require.NoError(t, restarted.Restore())
	require.Error(t, restarted.RegisterExpected(context.Background(), "rotation-12", map[string][]string{
		"control-plane": {"cp-a"}, "media-plane": {"mp-a"},
	}, map[string]uint64{"control-plane": 12, "media-plane": 12}), "an invalid recovery attempt must preserve the prior expected host set")
}

func TestControllerAdminProbeFailsClosed(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}, probeErr: errors.New("synthetic auth rejection")}
	controller, _ := newControllerFixture(t, admin)

	err := controller.Probe(context.Background(), "cv_admin", "wrong-synthetic-password")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "wrong-synthetic-password")
	require.True(t, strings.Contains(err.Error(), "probe") || strings.Contains(err.Error(), "Redis"), "error should retain a bounded operation context")
}
