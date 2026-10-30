package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureRolloutOutput(t *testing.T, run func() int) (int, string, string) {
	t.Helper()
	// The rollout loads config outside any deploy, so it names the environment
	// the way local development does; unset now means production.
	t.Setenv("ENVIRONMENT", "development")
	oldStdout, oldStderr := os.Stdout, os.Stderr
	stdoutReader, stdoutWriter, err := os.Pipe()
	require.NoError(t, err)
	stderrReader, stderrWriter, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout, os.Stderr = stdoutWriter, stderrWriter
	defer func() {
		os.Stdout, os.Stderr = oldStdout, oldStderr
	}()

	code := run()
	require.NoError(t, stdoutWriter.Close())
	require.NoError(t, stderrWriter.Close())
	stdout, err := io.ReadAll(stdoutReader)
	require.NoError(t, err)
	stderr, err := io.ReadAll(stderrReader)
	require.NoError(t, err)
	require.NoError(t, stdoutReader.Close())
	require.NoError(t, stderrReader.Close())
	return code, string(stdout), string(stderr)
}

func TestRunVoiceEnforcementRolloutRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"unknown"},
		{"activate"},
		{"activate", "--wrong"},
		{"deactivate", "extra"},
		{"status", "extra"},
	} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			code, stdout, stderr := captureRolloutOutput(t, func() int {
				return runVoiceEnforcementRollout(args)
			})
			assert.Equal(t, 64, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, "usage: control-plane voice-enforcement-rollout")
		})
	}
}

func TestRunVoiceEnforcementRolloutReturnsConfigurationError(t *testing.T) {
	t.Setenv("MFA_ENCRYPTION_KEY_VERSION", "not-an-integer")

	code, stdout, stderr := captureRolloutOutput(t, func() int {
		return runVoiceEnforcementRollout([]string{"status"})
	})
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "load configuration")
}

func TestRunVoiceEnforcementRolloutReturnsDatabaseError(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x:x@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")

	code, stdout, stderr := captureRolloutOutput(t, func() int {
		return runVoiceEnforcementRollout([]string{"status"})
	})
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "open database")
}

func TestRunVoiceEnforcementRolloutBareActivationCannotSetFamilyFloor(t *testing.T) {
	db := setupFamilyRolloutDB(t)
	code, _, stderr := captureRolloutOutput(t, func() int {
		return runVoiceEnforcementRollout([]string{"activate", "--confirm-drained"})
	})
	require.NotEqual(t, 0, code, stderr)
	assert.NotEmpty(t, stderr)
	assertFamilyRolloutPrepared(t, db)
}

// These are trusted operator attestations. Accepting their syntax does not
// establish host containment; the lifecycle wrapper must supply that evidence.
func operatorRetirementArgs(boot, slot, variant string) []string {
	args := []string{"retire-boot", "--node-boot-id", boot, "--slot", slot}
	if variant == "container" {
		return append(args, "--container-id", strings.Repeat("c", 64), "--image-sha256", strings.Repeat("d", 64), "--started-at", "2026-10-26T10:00:00Z", "--finished-at", "2026-10-26T10:01:00Z")
	}
	return append(args, "--process-id", "2345", "--process-start", "linux-start-1234", "--process-cwd", "/workspace/media")
}

func replaceRetirementArg(args []string, flag, value string) []string {
	result := append([]string(nil), args...)
	for i := 1; i+1 < len(result); i += 2 {
		if result[i] == flag {
			result[i+1] = value
			return result
		}
	}
	return append(result, flag, value)
}

func TestRunVoiceEnforcementRolloutRetirementRejectsMalformedAttestation(t *testing.T) {
	const boot = "12abcdef-1234-4123-8123-123456789abc"
	container := operatorRetirementArgs(boot, "media-plane", "container")
	native := operatorRetirementArgs(boot, "media-plane", "native")
	cases := []struct {
		name string
		args []string
	}{
		{"bare", []string{"retire-boot", boot}},
		{"boolean-only", []string{"retire-boot", "--node-boot-id", boot, "--slot", "media-plane", "--confirm-stopped"}},
		{"missing-evidence", container[:5]},
		{"partial-container", container[:len(container)-2]},
		{"partial-native", native[:len(native)-2]},
		{"duplicate", append(append([]string(nil), container...), "--node-boot-id", boot)},
		{"unknown-option", append(append([]string(nil), container...), "--unexpected", "value")},
		{"mixed-variants", append(append([]string(nil), container...), native[5:]...)},
		{"empty-boot", replaceRetirementArg(container, "--node-boot-id", "")},
		{"uppercase-boot", replaceRetirementArg(container, "--node-boot-id", strings.ToUpper(boot))},
		{"compact-boot", replaceRetirementArg(container, "--node-boot-id", strings.ReplaceAll(boot, "-", ""))},
		{"nil-boot", replaceRetirementArg(container, "--node-boot-id", "00000000-0000-0000-0000-000000000000")},
		{"boot-whitespace", replaceRetirementArg(container, "--node-boot-id", boot+" ")},
		{"empty-slot", replaceRetirementArg(container, "--slot", "")},
		{"oversize-slot", replaceRetirementArg(container, "--slot", strings.Repeat("s", 33))},
		{"short-container", replaceRetirementArg(container, "--container-id", strings.Repeat("c", 63))},
		{"uppercase-container", replaceRetirementArg(container, "--container-id", strings.Repeat("C", 64))},
		{"nonhex-container", replaceRetirementArg(container, "--container-id", strings.Repeat("g", 64))},
		{"digest-prefix", replaceRetirementArg(container, "--image-sha256", "sha256:"+strings.Repeat("d", 64))},
		{"short-digest", replaceRetirementArg(container, "--image-sha256", strings.Repeat("d", 63))},
		{"uppercase-digest", replaceRetirementArg(container, "--image-sha256", strings.Repeat("D", 64))},
		{"bad-start", replaceRetirementArg(container, "--started-at", "yesterday")},
		{"bad-finish", replaceRetirementArg(container, "--finished-at", "tomorrow")},
		{"inverted-time", replaceRetirementArg(container, "--finished-at", "2026-10-26T09:59:00Z")},
		{"oversize-time", replaceRetirementArg(container, "--started-at", strings.Repeat("0", 65))},
		{"pid-one", replaceRetirementArg(native, "--process-id", "1")},
		{"pid-zero", replaceRetirementArg(native, "--process-id", "0")},
		{"pid-negative", replaceRetirementArg(native, "--process-id", "-2")},
		{"pid-nonnumeric", replaceRetirementArg(native, "--process-id", "2x")},
		{"pid-oversize", replaceRetirementArg(native, "--process-id", strings.Repeat("2", 21))},
		{"empty-process-start", replaceRetirementArg(native, "--process-start", "")},
		{"oversize-process-start", replaceRetirementArg(native, "--process-start", strings.Repeat("s", 129))},
		{"relative-cwd", replaceRetirementArg(native, "--process-cwd", "workspace/media")},
		{"empty-cwd", replaceRetirementArg(native, "--process-cwd", "")},
		{"oversize-cwd", replaceRetirementArg(native, "--process-cwd", "/"+strings.Repeat("x", 4096))},
	}
	for _, base := range [][]string{container, native} {
		for i := 1; i+1 < len(base); i += 2 {
			cases = append(cases, struct {
				name string
				args []string
			}{"empty-" + base[i], replaceRetirementArg(base, base[i], "")})
			missing := append([]string(nil), base[:i]...)
			missing = append(missing, base[i+2:]...)
			cases = append(cases, struct {
				name string
				args []string
			}{"missing-" + base[i], missing})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Invalid configuration proves malformed argv is rejected at the parser,
			// before the configuration or database boundaries can mask its rejection.
			t.Setenv("MFA_ENCRYPTION_KEY_VERSION", "not-an-integer")
			code, stdout, stderr := captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout(tc.args) })
			assert.Equal(t, 64, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, "usage: control-plane voice-enforcement-rollout")
		})
	}
}

func TestRunVoiceEnforcementRolloutVerifyRetiredBootRejectsMalformedArguments(t *testing.T) {
	const boot = "12abcdef-1234-4123-8123-123456789abc"
	for _, args := range [][]string{
		{"verify-retired-boot"}, {"verify-retired-boot", boot},
		{"verify-retired-boot", "--node-boot-id"},
		{"verify-retired-boot", "--node-boot-id", ""},
		{"verify-retired-boot", "--node-boot-id", strings.ToUpper(boot)},
		{"verify-retired-boot", "--node-boot-id", strings.ReplaceAll(boot, "-", "")},
		{"verify-retired-boot", "--node-boot-id", "urn:uuid:" + boot},
		{"verify-retired-boot", "--node-boot-id", "{" + boot + "}"},
		{"verify-retired-boot", "--node-boot-id", "00000000-0000-0000-0000-000000000000"},
		{"verify-retired-boot", "--node-boot-id", boot + " "},
		{"verify-retired-boot", "--node-boot-id", boot, "--node-boot-id", boot},
		{"verify-retired-boot", "--node-boot-id", boot, "--slot", "media-plane"},
		{"verify-retired-boot", "--node-boot-id", boot, "--json"},
	} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			t.Setenv("MFA_ENCRYPTION_KEY_VERSION", "not-an-integer")
			code, stdout, stderr := captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout(args) })
			assert.Equal(t, 64, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, "usage: control-plane voice-enforcement-rollout")
		})
	}
}

func TestRunVoiceEnforcementRolloutRetirementCommandsReportDatabaseObservationFailure(t *testing.T) {
	const boot = "12abcdef-1234-4123-8123-123456789abc"
	for _, args := range [][]string{operatorRetirementArgs(boot, "media-plane", "container"), operatorRetirementArgs(boot, "media-plane", "native"), {"verify-retired-boot", "--node-boot-id", boot}} {
		t.Run(strings.Join(args[:1], ""), func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://x:x@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
			t.Setenv("ROOM_PLACEMENT_ENABLED", "false")
			code, stdout, stderr := captureRolloutOutput(t, func() int { return runVoiceEnforcementRollout(args) })
			assert.Equal(t, 1, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, "open database")
		})
	}
}

func TestParseVoiceBootRetirementArgsAcceptsBoundedTrustedIdentity(t *testing.T) {
	const boot = "12abcdef-1234-4123-8123-123456789abc"
	args := operatorRetirementArgs(boot, "media-plane", "native")
	args = replaceRetirementArg(args, "--process-start", strings.Repeat("s", 128))
	args = replaceRetirementArg(args, "--process-cwd", "/"+strings.Repeat("x", 4095))
	evidence, err := parseVoiceBootRetirementArgs(args, false)
	require.NoError(t, err)
	assert.Equal(t, boot, evidence.NodeBootID.String())
	assert.Equal(t, "media-plane", evidence.Slot)
	assert.Equal(t, strings.Repeat("s", 128), evidence.Values["--process-start"])
	assert.Equal(t, "/"+strings.Repeat("x", 4095), evidence.Values["--process-cwd"])
}
