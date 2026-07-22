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

// TestValidateProductionRejectsWildcardAllowedOrigins covers CV-CAN-014: a
// wildcard ALLOWED_ORIGINS is a CWE-942 credentialed cross-origin hijack (CORS
// reflects the Origin with Access-Control-Allow-Credentials: true, and the WS
// upgrader treats "*" as allow-all), so it must fatal-exit in production. An
// explicit allowlist is accepted, and the wildcard parity branch stays inert
// outside production.
func TestValidateProductionRejectsWildcardAllowedOrigins(t *testing.T) {
	t.Run("production + wildcard origin is fatal", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.AllowedOrigins = []string{"https://app.concordvoice.chat", "*"}
		err := cfg.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ALLOWED_ORIGINS")
	})

	t.Run("production + explicit allowlist is allowed", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.AllowedOrigins = []string{"https://app.concordvoice.chat", "app://concord"}
		assert.NoError(t, cfg.validate())
	})

	t.Run("development + wildcard origin is inert", func(t *testing.T) {
		cfg := &Config{Environment: "development", AllowedOrigins: []string{"*"}}
		assert.NoError(t, cfg.validate())
	})
}

func TestLoad_MFAEncryptionKeyVersionParsing(t *testing.T) {
	// Load reads .env from the working directory; isolate the unset case from
	// developer-local configuration.
	t.Chdir(t.TempDir())
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("ACTIVITY_HISTORY_CLUSTER_ENABLED", "false")
	t.Setenv("OPS_METRICS_ENABLED", "false")
	setOptionalEnv(t, "CONTROL_PLANE_REPLICA_COUNT", nil)

	tests := []struct {
		name    string
		value   *string
		want    int
		wantErr bool
	}{
		{name: "unset defaults to one", want: 1},
		{name: "explicit valid value", value: stringPointer("2"), want: 2},
		{name: "explicit empty value fails closed", value: stringPointer(""), wantErr: true},
		{name: "explicit malformed value fails closed", value: stringPointer("N+1"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setOptionalEnv(t, "MFA_ENCRYPTION_KEY_VERSION", tt.value)

			cfg, err := Load()
			if tt.wantErr {
				require.ErrorContains(t, err, "MFA_ENCRYPTION_KEY_VERSION")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.MFAEncryptionKeyVersion)
		})
	}
}
