package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain neutralizes the process-wide CONCORD_ENV=test that CI sets for the
// entire Go test run (build.yml "Run tests with coverage" step). Without this,
// the #1283 production guard would fatal every production-pass test in this
// package. Individual guard tests re-set CONCORD_ENV via t.Setenv.
func TestMain(m *testing.M) {
	if err := os.Unsetenv("CONCORD_ENV"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// TestValidateProductionRejectsConcordEnvTest covers the #1283 env-integrity
// guard: CONCORD_ENV=test is a fatal misconfiguration in production, inert
// everywhere else.
func TestValidateProductionRejectsConcordEnvTest(t *testing.T) {
	t.Run("production + CONCORD_ENV=test is fatal", func(t *testing.T) {
		t.Setenv("CONCORD_ENV", "test")
		cfg := validProductionConfig()
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CONCORD_ENV")
	})

	t.Run("production + CONCORD_ENV unset is allowed", func(t *testing.T) {
		t.Setenv("CONCORD_ENV", "")
		cfg := validProductionConfig()
		assert.NoError(t, cfg.validate())
	})

	t.Run("development + CONCORD_ENV=test is inert", func(t *testing.T) {
		t.Setenv("CONCORD_ENV", "test")
		cfg := &Config{Environment: "development"}
		assert.NoError(t, cfg.validate())
	})
}
