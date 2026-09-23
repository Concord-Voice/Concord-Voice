package main

import (
	"io"
	"net/url"
	"os"
	"strings"
	"testing"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureRolloutOutput(t *testing.T, run func() int) (int, string, string) {
	t.Helper()
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

func TestRunVoiceEnforcementRolloutActivatesDeactivatesAndReportsStatus(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	parsedURL, err := url.Parse(databaseURL)
	if databaseURL == "" || err != nil || !strings.HasSuffix(parsedURL.Path, "_test") {
		t.Skip("DATABASE_URL must point to an explicitly named *_test database")
	}
	db, _ := dbtest.SetupTestDB(t)

	code, stdout, stderr := captureRolloutOutput(t, func() int {
		return runVoiceEnforcementRollout([]string{"status"})
	})
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "voice-enforcement-rollout: inactive")
	assert.Contains(t, stdout, "registryRows=0")

	code, stdout, stderr = captureRolloutOutput(t, func() int {
		return runVoiceEnforcementRollout([]string{"activate", "--confirm-drained"})
	})
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "voice-enforcement-rollout: active since")

	var activated bool
	require.NoError(t, db.QueryRow(`
		SELECT activated_at IS NOT NULL
		FROM voice_enforcement_rollout
		WHERE id = TRUE`).Scan(&activated))
	assert.True(t, activated)

	code, stdout, stderr = captureRolloutOutput(t, func() int {
		return runVoiceEnforcementRollout([]string{"deactivate"})
	})
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "voice-enforcement-rollout: inactive")

	require.NoError(t, db.QueryRow(`
		SELECT activated_at IS NOT NULL
		FROM voice_enforcement_rollout
		WHERE id = TRUE`).Scan(&activated))
	assert.False(t, activated)
}
