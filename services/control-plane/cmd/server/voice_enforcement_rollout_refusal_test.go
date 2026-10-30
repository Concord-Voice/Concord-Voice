package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Refusal coverage for the family-rollout operator boundary: argument
// grammar, the strict receipt decoder, and observation/mutation faults.

func validRolloutReceiptMap(slots ...map[string]any) map[string]any {
	mediaSlots := make([]any, 0, len(slots))
	for _, slot := range slots {
		mediaSlots = append(mediaSlots, slot)
	}
	return map[string]any{
		"schema": "concord.login-family-activation", "version": 1, "activationId": uuid.New().String(), "protocol": 4,
		"controlPlaneArtifactSHA256": strings.Repeat("a", 64),
		"mediaSlots":                 mediaSlots,
		"legacyUsableSIDs":           0, "registryRows": 0, "authConnectionRows": 0, "authRoomBindingRows": 0,
		"dmParentRows": 0, "credentialParentRows": 0, "authParentRows": 0, "dmReconciliationRows": 0,
		"authConnectionClosureRows": 0, "authRoomClosureRows": 0, "retiredBootRows": 0,
	}
}

func rolloutSlot(name string) map[string]any {
	return map[string]any{"slot": name, "artifactSHA256": strings.Repeat("b", 64), "nodeBootId": uuid.New().String()}
}

func marshalRolloutReceipt(t *testing.T, receipt map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(receipt)
	require.NoError(t, err)
	return raw
}

func TestValidFamilyRolloutArgsGrammar(t *testing.T) {
	assert.False(t, validFamilyRolloutArgs(nil))
	assert.False(t, validFamilyRolloutArgs([]string{"bogus"}))
	assert.False(t, validFamilyRolloutArgs([]string{"activate", "--confirm-drained", "--receipt-file", ""}))
	assert.False(t, validFamilyRolloutArgs([]string{"verify-retired-boot", "--node-boot-id", "not-a-uuid"}))
	assert.False(t, validFamilyRolloutArgs([]string{"status", "--yaml"}))
	assert.True(t, validFamilyRolloutArgs([]string{"status", "--json"}))
	assert.True(t, validFamilyRolloutArgs([]string{"deactivate"}))
	assert.True(t, validFamilyRolloutArgs([]string{"verify-retired-boot", "--node-boot-id", uuid.New().String()}))
	// Placement-only slots are accepted by the grammar; the configured
	// inventory is enforced later against the loaded configuration.
	assert.True(t, validFamilyRolloutArgs(operatorRetirementArgs(uuid.New().String(), "media-plane-blue", "container")))
	assert.False(t, validFamilyRolloutArgs(operatorRetirementArgs(uuid.New().String(), "media-plane-red", "container")))
}

func TestParseVoiceBootRetirementArgsRefusals(t *testing.T) {
	boot := uuid.New().String()
	container := operatorRetirementArgs(boot, "media-plane", "container")
	native := operatorRetirementArgs(boot, "media-plane", "native")

	wrongVerb := append([]string(nil), native...)
	wrongVerb[0] = "status"
	duplicate := append([]string(nil), native...)
	duplicate[5], duplicate[6] = "--slot", "media-plane"
	unknown := append([]string(nil), native...)
	unknown[5] = "--bogus"

	cases := []struct {
		name      string
		args      []string
		placement bool
	}{
		{"wrong arity", native[:9], false},
		{"wrong verb", wrongVerb, false},
		{"duplicate flag", duplicate, false},
		{"unknown flag", unknown, false},
		{"control character", replaceRetirementArg(native, "--process-start", "start\x00"), false},
		{"oversized value", replaceRetirementArg(native, "--process-id", strings.Repeat("9", 21)), false},
		{"malformed boot", replaceRetirementArg(native, "--node-boot-id", strings.Repeat("z", 36)), false},
		{"unplaced slot under placement", container, true},
		{"placement slot without placement", replaceRetirementArg(container, "--slot", "media-plane-green"), false},
		{"native evidence under placement", replaceRetirementArg(native, "--slot", "media-plane-blue"), true},
		{"relative cwd", replaceRetirementArg(native, "--process-cwd", "workspace/media"), false},
		{"init pid", replaceRetirementArg(native, "--process-id", "1"), false},
		{"non-canonical pid", replaceRetirementArg(native, "--process-id", "02345"), false},
		{"non-numeric pid", replaceRetirementArg(native, "--process-id", "pid"), false},
		{"malformed container id", replaceRetirementArg(container, "--container-id", strings.Repeat("C", 64)), false},
		{"malformed image digest", replaceRetirementArg(container, "--image-sha256", "abc"), false},
		{"unparseable start", replaceRetirementArg(container, "--started-at", "yesterday"), false},
		{"finish before start", replaceRetirementArg(container, "--finished-at", "2026-10-26T09:00:00Z"), false},
		{"finish equals start", replaceRetirementArg(container, "--finished-at", "2026-10-26T10:00:00Z"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseVoiceBootRetirementArgs(tc.args, tc.placement)
			require.ErrorIs(t, err, errFamilyRolloutEvidence)
		})
	}

	evidence, err := parseVoiceBootRetirementArgs(replaceRetirementArg(container, "--slot", "media-plane-green"), true)
	require.NoError(t, err)
	assert.Equal(t, "media-plane-green", evidence.Slot)
	assert.Equal(t, uuid.MustParse(boot), evidence.NodeBootID)
}

func TestReadFamilyActivationReceiptFileFaults(t *testing.T) {
	_, err := readFamilyActivationReceipt(filepath.Join(t.TempDir(), "absent.json"), false)
	require.ErrorIs(t, err, os.ErrNotExist)
	// A directory opens but cannot be read.
	_, err = readFamilyActivationReceipt(t.TempDir(), false)
	require.Error(t, err)

	path := filepath.Join(t.TempDir(), "receipt.json")
	require.NoError(t, os.WriteFile(path, marshalRolloutReceipt(t, validRolloutReceiptMap(rolloutSlot("media-plane"))), 0600))
	receipt, err := readFamilyActivationReceipt(path, false)
	require.NoError(t, err)
	require.Len(t, receipt.MediaSlots, 1)
}

func TestFamilyRolloutJSONValueRefusals(t *testing.T) {
	decode := func(raw string) (any, error) {
		d := json.NewDecoder(strings.NewReader(raw))
		d.UseNumber()
		return familyRolloutJSONValue(d, 0)
	}
	for name, raw := range map[string]string{
		"empty input":                ``,
		"null":                       `null`,
		"too deep":                   strings.Repeat("[", 10) + strings.Repeat("]", 10),
		"non-string key":             `{1:2}`,
		"duplicate key":              `{"a":1,"a":2}`,
		"duplicate key inside array": `[{"a":1,"a":2}]`,
		"bad member value":           `{"a":null}`,
		"unterminated object":        `{"a":1`,
		"unterminated array":         `[1`,
		"stray closing delimiter":    `]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decode(raw)
			require.ErrorIs(t, err, errFamilyRolloutEvidence)
		})
	}
	value, err := decode(`{"a":[1,"x",{"b":true}]}`)
	require.NoError(t, err)
	object, ok := value.(map[string]any)
	require.True(t, ok)
	require.Len(t, object, 1)
}

func TestFamilyRolloutClosedObject(t *testing.T) {
	assert.False(t, familyRolloutClosedObject([]any{}, familyRolloutSlot{}))
	assert.False(t, familyRolloutClosedObject(map[string]any{}, func() {}))
	assert.False(t, familyRolloutClosedObject(map[string]any{"slot": 1, "artifactSHA256": 1}, familyRolloutSlot{}))
	assert.False(t, familyRolloutClosedObject(map[string]any{"slot": 1, "artifactSHA256": 1, "nodeBootID": 1}, familyRolloutSlot{}))
	assert.True(t, familyRolloutClosedObject(map[string]any{"slot": 1, "artifactSHA256": 1, "nodeBootId": 1}, familyRolloutSlot{}))
}

func TestDecodeFamilyActivationReceiptRefusals(t *testing.T) {
	single := func(mutate func(map[string]any)) []byte {
		receipt := validRolloutReceiptMap(rolloutSlot("media-plane"))
		mutate(receipt)
		return marshalRolloutReceipt(t, receipt)
	}
	slot := func(mutate func(map[string]any)) []byte {
		return single(func(receipt map[string]any) {
			s := rolloutSlot("media-plane")
			mutate(s)
			receipt["mediaSlots"] = []any{s}
		})
	}
	sharedBoot := rolloutSlot("media-plane-blue")
	twin := rolloutSlot("media-plane-green")
	twin["nodeBootId"] = sharedBoot["nodeBootId"]

	cases := []struct {
		name      string
		raw       []byte
		placement bool
	}{
		{"oversized", []byte(`{"pad":"` + strings.Repeat("x", 4100) + `"}`), false},
		{"invalid utf-8", []byte("{\"schema\":\"\xff\"}"), false},
		{"not json", []byte(`{`), false},
		{"array root", []byte(`[]`), false},
		{"wrong field type", single(func(r map[string]any) { r["version"] = "1" }), false},
		{"wrong schema", single(func(r map[string]any) { r["schema"] = "concord.other" }), false},
		{"wrong version", single(func(r map[string]any) { r["version"] = 2 }), false},
		{"malformed activation id", single(func(r map[string]any) { r["activationId"] = "x" }), false},
		{"malformed control-plane digest", single(func(r map[string]any) { r["controlPlaneArtifactSHA256"] = "x" }), false},
		{"slots not an array", single(func(r map[string]any) { r["mediaSlots"] = map[string]any{} }), false},
		{"placement inventory needs two slots", single(func(map[string]any) {}), true},
		{"slot with extra field", slot(func(s map[string]any) { s["extra"] = 1 }), false},
		{"unknown slot name", slot(func(s map[string]any) { s["slot"] = "media-plane-blue" }), false},
		{"malformed artifact digest", slot(func(s map[string]any) { s["artifactSHA256"] = "B" }), false},
		{"malformed boot", slot(func(s map[string]any) { s["nodeBootId"] = "boot" }), false},
		{"duplicate boot across slots", marshalRolloutReceipt(t, validRolloutReceiptMap(sharedBoot, twin)), true},
		{"duplicate slot name", marshalRolloutReceipt(t, validRolloutReceiptMap(rolloutSlot("media-plane-blue"), rolloutSlot("media-plane-blue"))), true},
		{"undrained registry", single(func(r map[string]any) { r["registryRows"] = 1 }), false},
		{"negative connection closures", single(func(r map[string]any) { r["authConnectionClosureRows"] = -1 }), false},
		{"negative room closures", single(func(r map[string]any) { r["authRoomClosureRows"] = -1 }), false},
		{"negative retired boots", single(func(r map[string]any) { r["retiredBootRows"] = -1 }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeFamilyActivationReceipt(tc.raw, tc.placement)
			require.ErrorIs(t, err, errFamilyRolloutEvidence)
		})
	}

	placed, err := decodeFamilyActivationReceipt(marshalRolloutReceipt(t, validRolloutReceiptMap(rolloutSlot("media-plane-green"), rolloutSlot("media-plane-blue"))), true)
	require.NoError(t, err)
	require.Len(t, placed.MediaSlots, 2)
	retained, err := decodeFamilyActivationReceipt(single(func(r map[string]any) { r["retiredBootRows"] = 3 }), false)
	require.NoError(t, err)
	assert.EqualValues(t, 3, retained.RetiredBootRows)
}

// restorePreparedRollout returns the singleton to its prepared state for any
// later test in this package that relies on the generic fixture.
func restorePreparedRollout(t *testing.T, db *sql.DB) {
	t.Helper()
	t.Cleanup(func() {
		_, err := dbtest.ExecReplica(db, `TRUNCATE login_family_rollout`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO login_family_rollout(id) VALUES(TRUE)`)
		require.NoError(t, err)
	})
}

func TestReadFamilyRolloutStatusRefusesCorruptRecordedReceipt(t *testing.T) {
	db := setupFamilyRolloutDB(t)
	restorePreparedRollout(t, db)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := readFamilyRolloutStatus(canceled, db)
	require.Error(t, err)

	// The recorded receipt is an object (the table CHECK) whose fields do not
	// fit the receipt struct.
	_, err = db.Exec(`UPDATE login_family_rollout SET activated_at=clock_timestamp(), receipt='{"mediaSlots":"x"}'::jsonb WHERE id`)
	require.NoError(t, err)
	_, err = readFamilyRolloutStatus(context.Background(), db)
	require.ErrorIs(t, err, errFamilyRolloutEvidence)
}

func TestReadFamilyRolloutStatusRefusesInvalidRecordedReceipt(t *testing.T) {
	db := setupFamilyRolloutDB(t)
	restorePreparedRollout(t, db)
	_, err := db.Exec(`UPDATE login_family_rollout SET activated_at=clock_timestamp(), receipt='{"schema":"concord.other"}'::jsonb WHERE id`)
	require.NoError(t, err)
	_, err = readFamilyRolloutStatus(context.Background(), db)
	require.ErrorIs(t, err, errFamilyRolloutEvidence)

	code, stdout, stderr := captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout([]string{"status"}) })
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "observe family rollout failed")
}

func TestMutateFamilyRolloutDeactivateWhilePreparedAndFaults(t *testing.T) {
	db := setupFamilyRolloutDB(t)
	_, err := db.Exec(`UPDATE voice_enforcement_rollout SET activated_at=clock_timestamp() WHERE id`)
	require.NoError(t, err)

	require.NoError(t, mutateFamilyRollout(context.Background(), db, "deactivate", familyActivationReceipt{}))
	var legacyActive bool
	require.NoError(t, db.QueryRow(`SELECT activated_at IS NOT NULL FROM voice_enforcement_rollout WHERE id`).Scan(&legacyActive))
	assert.False(t, legacyActive, "deactivate clears the legacy floor while the family floor is prepared")
	assertFamilyRolloutPrepared(t, db)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, mutateFamilyRollout(canceled, db, "deactivate", familyActivationReceipt{}))
	assertFamilyRolloutPrepared(t, db)
}

func TestRunVoiceEnforcementRolloutJSONEncodeFailure(t *testing.T) {
	setupFamilyRolloutDB(t)
	code, _, _ := captureRolloutOutput(t, func() int {
		reader, writer, err := os.Pipe()
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		require.NoError(t, writer.Close())
		previous := os.Stdout
		os.Stdout = writer
		defer func() { os.Stdout = previous }()
		return runVoiceEnforcementRollout([]string{"status", "--json"})
	})
	assert.Equal(t, 1, code)
}
