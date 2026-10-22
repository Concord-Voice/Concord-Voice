package managedredis

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValidateJournalIntentRequiresRevokedPriorUsernames(t *testing.T) {
	policy := policyTestValue()
	oldUsernames := map[string]string{}
	revoked := map[string]revokedIdentity{}
	for service, servicePolicy := range policy.Services {
		old := servicePolicy.Generations[0].Username
		servicePolicy.Generations = servicePolicy.Generations[1:]
		policy.Services[service] = servicePolicy
		oldUsernames[service] = old
		revoked[service] = revokedIdentity{Generation: 11}
	}
	journal := rotationJournal{
		Effective: policy, Phase: "drained", Intent: "revoke",
		OldUsernames: oldUsernames, Revoked: revoked,
	}
	require.NoError(t, validateJournalIntent(journal))

	revoked[controlPlaneService] = revokedIdentity{}
	journal.Revoked = revoked
	require.ErrorIs(t, validateJournalIntent(journal), errController)
	journal.Revoked = revoked
	journal.Effective = policyTestValue()
	require.ErrorIs(t, validateJournalIntent(journal), errController, "a revoke intent cannot retain overlapping credential generations")
}

func TestJournalKeepsCanonicalRevocationFloorBoundedBeyond128Rotations(t *testing.T) {
	controller, aclPath := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
	prepareController(t, controller)
	var journal rotationJournal
	for generation := uint64(11); generation < 140; generation++ {
		next := policyTestValue()
		next.RotationID = fmt.Sprintf("rotation-%d", generation+1)
		for service, role := range next.Services {
			prefix := "cv_cp_"
			if service == mediaPlaneService {
				prefix = "cv_mp_"
			}
			role.TargetGeneration = generation + 1
			role.Generations = []PolicyGeneration{
				{Generation: generation, Username: fmt.Sprintf("%s%d", prefix, generation), PasswordHash: role.Generations[0].PasswordHash, ValidFrom: policyTestNow.Add(-time.Hour), ValidUntil: policyTestNow.Add(time.Hour)},
				{Generation: generation + 1, Username: fmt.Sprintf("%s%d", prefix, generation+1), PasswordHash: role.Generations[1].PasswordHash, ValidFrom: policyTestNow.Add(-time.Minute), ValidUntil: policyTestNow.Add(time.Hour)},
			}
			next.Services[service] = role
		}
		controller.policy = next
		journal = controller.drainedJournalLocked(controller.finalizedPolicyLocked(), map[string]string{
			controlPlaneService: fmt.Sprintf("cv_cp_%d", generation), mediaPlaneService: fmt.Sprintf("cv_mp_%d", generation),
		})
		controller.revoked = journal.Revoked
	}
	for _, service := range []string{controlPlaneService, mediaPlaneService} {
		require.Equal(t, uint64(139), journal.Revoked[service].Generation)
	}
	require.NoError(t, controller.persistJournal(journal))
	raw, err := readJournalFixture(filepath.Join(filepath.Dir(aclPath), "state", "rotation.json"))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "usernames")
	var restored rotationJournal
	require.NoError(t, decodeJournal(raw, &restored))
	require.NoError(t, controller.validateJournal(restored))
}

func TestDecodeJournalRejectsAmbiguousOrUnboundedJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{name: "empty", raw: nil},
		{name: "oversized", raw: make([]byte, maxJournalBytes+1)},
		{name: "malformed", raw: []byte(`{"version":`)},
		{name: "duplicate key", raw: []byte(`{"version":1,"version":1}`)},
		{name: "unknown field", raw: []byte(`{"unexpected":true}`)},
		{name: "trailing document", raw: []byte(`{"version":1}{}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var journal rotationJournal
			require.ErrorIs(t, decodeJournal(tc.raw, &journal), errController)
		})
	}

	var journal rotationJournal
	require.NoError(t, decodeJournal([]byte(`{"version":1}`), &journal))
	require.Equal(t, 1, journal.Version)
}

func TestRestoreRejectsUnsafeJournalFileMetadataBeforeLoadingACL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "shared permissions", mutate: func(t *testing.T, path string) {
			require.NoError(t, os.Chmod(path, 0o644)) // #nosec G302 -- deliberate unsafe permissions in a temporary fixture.
		}},
		{name: "directory substitution", mutate: func(t *testing.T, path string) {
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.Mkdir(path, 0o700))
		}},
		{name: "symlink substitution", mutate: func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "journal-target")
			require.NoError(t, os.Rename(path, target))
			require.NoError(t, os.Symlink(target, path))
		}},
		{name: "oversized journal", mutate: func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, make([]byte, maxJournalBytes+1), 0o600))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin := &fakeRedisAdmin{users: map[string]bool{}}
			controller, aclPath := newControllerFixture(t, admin)
			prepareController(t, controller)
			journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
			tc.mutate(t, journalPath)
			admin.loadCalls = 0

			restarted, err := NewController(ControllerOptions{
				Admin: admin, ACLPath: aclPath, JournalPath: journalPath, MarkerPath: testMarkerPath(t, journalPath),
				Bootstrap: controller.bootstrap, Now: func() time.Time { return policyTestNow },
				ExpectedUIDs: map[string]uint32{controlPlaneService: 1002, mediaPlaneService: 1003},
			})
			require.NoError(t, err)
			require.ErrorIs(t, restarted.Restore(), errController)
			require.Zero(t, admin.loadCalls, "untrusted journal metadata must be rejected before ACL loading")
		})
	}
}

func TestRestoreRejectsLegacyRevokedUsernameFieldBeforeUsingACL(t *testing.T) {
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, aclPath := newControllerFixture(t, admin)
	prepareController(t, controller)
	journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
	aclBefore, err := readACLFixture(aclPath)
	require.NoError(t, err)
	raw, err := readJournalFixture(journalPath)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(raw, &document))
	revoked, ok := document["revoked"].(map[string]any)
	require.True(t, ok)
	service, ok := revoked[controlPlaneService].(map[string]any)
	require.True(t, ok)
	service["usernames"] = []string{"cv_cp_99"}
	raw, err = json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(journalPath, raw, 0o600))
	admin.loadCalls = 0

	restarted, err := NewController(ControllerOptions{
		Admin: admin, ACLPath: aclPath, JournalPath: journalPath, MarkerPath: testMarkerPath(t, journalPath),
		Bootstrap: controller.bootstrap, Now: func() time.Time { return policyTestNow },
		ExpectedUIDs: map[string]uint32{controlPlaneService: 1002, mediaPlaneService: 1003},
	})
	require.NoError(t, err)
	require.ErrorIs(t, restarted.Restore(), errController)
	require.Zero(t, admin.loadCalls, "old-schema journal must be rejected before ACL loading")
	aclAfter, err := readACLFixture(aclPath)
	require.NoError(t, err)
	require.Equal(t, aclBefore, aclAfter, "old-schema journal must not rewrite the ACL")
}

func TestSnapshotJournalCopiesPriorRecoveryFence(t *testing.T) {
	controller, _ := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
	controller.mu.Lock()
	controller.priorExpected = map[string][]string{controlPlaneService: {"cp-old"}, mediaPlaneService: {"mp-old"}}
	controller.priorAcks = []AppliedAck{{Version: 1, RotationID: "rotation-11", Service: controlPlaneService, InstanceID: "cp-old", Generation: 11, Username: "cv_cp_11"}}
	controller.priorRotationID = "rotation-11"
	controller.priorTargets = map[string]uint64{controlPlaneService: 11, mediaPlaneService: 11}
	snapshot := controller.snapshotJournalLocked()
	controller.priorExpected[controlPlaneService][0] = "mutated-host"
	controller.priorTargets[controlPlaneService] = 99
	controller.mu.Unlock()

	require.Equal(t, []string{"cp-old"}, snapshot.PriorExpected[controlPlaneService])
	require.Equal(t, uint64(11), snapshot.PriorTargets[controlPlaneService])
	require.Equal(t, "rotation-11", snapshot.PriorRotationID)
	require.Equal(t, "cp-old", snapshot.PriorAcks[0].InstanceID)
}

func TestJournalRecoveryAndPriorAckRequireCompleteFences(t *testing.T) {
	j := rotationJournal{
		Effective: initialSingleGenerationPolicy(), Phase: "prepared", Intent: "recovery", RecoveryAt: policyTestNow,
		Expected:        map[string][]string{controlPlaneService: {"cp-new"}, mediaPlaneService: {"mp-new"}},
		PriorRotationID: "rotation-11", PriorTargets: map[string]uint64{controlPlaneService: 11, mediaPlaneService: 11},
		PriorExpected: map[string][]string{controlPlaneService: {"cp-old"}, mediaPlaneService: {"mp-old"}},
		PriorAcks:     []AppliedAck{{Version: 1, RotationID: "rotation-11", Service: controlPlaneService, InstanceID: "cp-old", Generation: 11, Username: "cv_cp_11"}},
		Bindings:      map[string][]InstanceBinding{controlPlaneService: {{InstanceID: "cp-old"}}},
	}
	require.NoError(t, validateJournalIntent(j))
	require.NoError(t, validateJournalExpected(j))
	require.NoError(t, validateJournalPriorAcks(j))

	for _, tc := range []struct {
		name   string
		mutate func(*rotationJournal)
	}{
		{"missing recovery time", func(j *rotationJournal) { j.RecoveryAt = time.Time{} }},
		{"recovery with overlapping generations", func(j *rotationJournal) { j.Effective = policyTestValue() }},
		{"missing expected service", func(j *rotationJournal) { delete(j.Expected, mediaPlaneService) }},
		{"incomplete prior host set", func(j *rotationJournal) { delete(j.PriorExpected, mediaPlaneService) }},
		{"invalid prior host", func(j *rotationJournal) { j.PriorExpected[controlPlaneService] = []string{"bad host"} }},
		{"prior host list exceeds bound", func(j *rotationJournal) { j.PriorExpected[controlPlaneService] = make([]string, 65) }},
		{"invalid prior rotation", func(j *rotationJournal) { j.PriorRotationID = "bad id" }},
		{"missing prior target", func(j *rotationJournal) { delete(j.PriorTargets, controlPlaneService) }},
		{"zero prior generation", func(j *rotationJournal) { j.PriorTargets[mediaPlaneService] = 0 }},
		{"prior ack generation mismatch", func(j *rotationJournal) { j.PriorAcks[0].Generation = 12 }},
		{"prior ack host absent", func(j *rotationJournal) { j.PriorAcks[0].InstanceID = "other-host" }},
		{"prior ack missing binding", func(j *rotationJournal) { delete(j.Bindings, controlPlaneService) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := j
			mutated.Effective = j.Effective
			mutated.PriorTargets = map[string]uint64{controlPlaneService: 11, mediaPlaneService: 11}
			mutated.PriorExpected = map[string][]string{controlPlaneService: {"cp-old"}, mediaPlaneService: {"mp-old"}}
			mutated.PriorAcks = append([]AppliedAck(nil), j.PriorAcks...)
			mutated.Bindings = map[string][]InstanceBinding{controlPlaneService: {{InstanceID: "cp-old"}}}
			tc.mutate(&mutated)
			switch {
			case tc.name == "missing recovery time" || tc.name == "recovery with overlapping generations":
				require.Error(t, validateJournalIntent(mutated))
			case strings.HasPrefix(tc.name, "prior ack"):
				require.Error(t, validateJournalPriorAcks(mutated))
			default:
				require.Error(t, validateJournalExpected(mutated))
			}
		})
	}
}

func TestRestoreRejectsTamperedRotationJournalBeforeUsingACL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*rotationJournal)
	}{
		{"version", func(j *rotationJournal) { j.Version = 2 }},
		{"phase", func(j *rotationJournal) { j.Phase = "unknown" }},
		{"intent", func(j *rotationJournal) { j.Intent = "revoke" }},
		{"failure code", func(j *rotationJournal) { j.FailureCode = "unknown" }},
		{"future validation", func(j *rotationJournal) { j.ValidatedAt = policyTestNow.Add(time.Minute) }},
		{"revocation floor", func(j *rotationJournal) { j.Revoked[controlPlaneService] = revokedIdentity{Generation: 12} }},
		{"missing service revocation floor", func(j *rotationJournal) { delete(j.Revoked, mediaPlaneService) }},
		{"missing expected host", func(j *rotationJournal) { j.Expected[controlPlaneService] = nil }},
		{"duplicate expected host", func(j *rotationJournal) { j.Expected[controlPlaneService] = []string{"cp-a", "cp-a"} }},
		{"invalid expected host", func(j *rotationJournal) { j.Expected[controlPlaneService] = []string{"bad host"} }},
		{"too many expected hosts", func(j *rotationJournal) {
			j.Expected[controlPlaneService] = make([]string, 65)
		}},
		{"empty binding", func(j *rotationJournal) { j.Bindings[controlPlaneService] = nil }},
		{"unknown binding slot", func(j *rotationJournal) {
			j.Bindings["unknown"] = j.Bindings[controlPlaneService]
			delete(j.Bindings, controlPlaneService)
		}},
		{"too many binding slots", func(j *rotationJournal) {
			for _, slot := range []string{"slot-a", "slot-b", "slot-c", "slot-d", "slot-e"} {
				j.Bindings[slot] = []InstanceBinding{{ContainerID: "x", InstanceID: "host-a"}}
			}
		}},
		{"binding history exceeds bound", func(j *rotationJournal) {
			j.Bindings[controlPlaneService] = make([]InstanceBinding, 65)
		}},
		{"invalid binding hex", func(j *rotationJournal) { j.Bindings[controlPlaneService][0].ContainerID = strings.Repeat("z", 64) }},
		{"uppercase container", func(j *rotationJournal) { j.Bindings[controlPlaneService][0].ContainerID = strings.Repeat("A", 64) }},
		{"duplicate container", func(j *rotationJournal) {
			j.Bindings[mediaPlaneService][0].ContainerID = j.Bindings[controlPlaneService][0].ContainerID
		}},
		{"duplicate instance", func(j *rotationJournal) {
			j.Bindings[mediaPlaneService][0].InstanceID = j.Bindings[controlPlaneService][0].InstanceID
		}},
		{"wrong ack generation", func(j *rotationJournal) { j.Acks[0].Generation = 13 }},
		{"wrong ack version", func(j *rotationJournal) { j.Acks[0].Version = 2 }},
		{"unknown ack service", func(j *rotationJournal) { j.Acks[0].Service = "unknown" }},
		{"ack without matching binding", func(j *rotationJournal) {
			delete(j.Bindings, controlPlaneService)
		}},
		{"unregistered ack", func(j *rotationJournal) { j.Acks[0].InstanceID = "unknown-host" }},
		{"duplicate ack", func(j *rotationJournal) { j.Acks = append(j.Acks, j.Acks[0]) }},
		{"orphan prior hosts", func(j *rotationJournal) {
			j.PriorExpected = map[string][]string{controlPlaneService: {"cp-a"}, mediaPlaneService: {"mp-a"}}
		}},
		{"orphan prior ack", func(j *rotationJournal) { j.PriorAcks = append(j.PriorAcks, j.Acks[0]) }},
		{"old users outside revoke", func(j *rotationJournal) { j.OldUsernames = map[string]string{controlPlaneService: "cv_cp_11"} }},
		{"invalid effective policy", func(j *rotationJournal) {
			service := j.Effective.Services[controlPlaneService]
			service.TargetGeneration = 0
			j.Effective.Services[controlPlaneService] = service
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin := &fakeRedisAdmin{users: map[string]bool{}}
			controller, aclPath := newControllerFixture(t, admin)
			prepareController(t, controller)
			recordControllerAck(t, controller, controlPlaneService, 1002, ack(controlPlaneService, "cp-a"))
			recordControllerAck(t, controller, mediaPlaneService, 1003, ack(mediaPlaneService, "mp-a"))
			journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
			raw, err := readJournalFixture(journalPath)
			require.NoError(t, err)
			var journal rotationJournal
			require.NoError(t, json.Unmarshal(raw, &journal))
			tc.mutate(&journal)
			raw, err = json.Marshal(journal)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(journalPath, raw, 0o600))
			restarted, err := NewController(ControllerOptions{
				Admin: admin, ACLPath: aclPath, JournalPath: journalPath, MarkerPath: testMarkerPath(t, journalPath),
				Bootstrap: controller.bootstrap, Now: func() time.Time { return policyTestNow },
				ExpectedUIDs: map[string]uint32{controlPlaneService: 1002, mediaPlaneService: 1003},
			})
			require.NoError(t, err)
			require.ErrorIs(t, restarted.Restore(), errController)
			state, _, _ := restarted.Status()
			require.Equal(t, "failed", state)
		})
	}

	// A clean baseline must still restore through the same production entry point.
	admin := &fakeRedisAdmin{users: map[string]bool{}}
	controller, aclPath := newControllerFixture(t, admin)
	prepareController(t, controller)
	journalPath := filepath.Join(filepath.Dir(aclPath), "state", "rotation.json")
	restarted, err := NewController(ControllerOptions{
		Admin: admin, ACLPath: aclPath, JournalPath: journalPath, MarkerPath: testMarkerPath(t, journalPath),
		Bootstrap: controller.bootstrap, Now: func() time.Time { return policyTestNow },
		ExpectedUIDs: map[string]uint32{controlPlaneService: 1002, mediaPlaneService: 1003},
	})
	require.NoError(t, err)
	require.NoError(t, restarted.Restore())
	require.NoError(t, restarted.ProbeConfigured(context.Background()))
}
