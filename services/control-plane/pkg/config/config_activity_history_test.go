package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadActivityHistoryConfigStrictGateAndReplicaParsing(t *testing.T) {
	for _, environment := range []string{"development", "test", "production"} {
		t.Run(environment, func(t *testing.T) {
			for _, tc := range []struct {
				name         string
				gate         *string
				replicas     *string
				wantEnabled  bool
				wantExplicit bool
				wantCount    int
				wantErr      bool
			}{
				{name: "omitted gate and count"},
				{name: "empty gate", gate: stringPointer("")},
				{name: "false gate", gate: stringPointer("false")},
				{name: "true with one replica", gate: stringPointer("true"), replicas: stringPointer("1"), wantEnabled: true, wantExplicit: true, wantCount: 1},
				{name: "malformed uppercase gate", gate: stringPointer("TRUE"), wantErr: true},
				{name: "malformed whitespace gate", gate: stringPointer(" true"), wantErr: true},
				{name: "malformed numeric gate", gate: stringPointer("1"), wantErr: true},
				{name: "true missing replica", gate: stringPointer("true"), wantErr: true},
				{name: "true empty replica", gate: stringPointer("true"), replicas: stringPointer(""), wantErr: true},
				{name: "true malformed replica", gate: stringPointer("true"), replicas: stringPointer("one"), wantErr: true},
				{name: "true zero replicas", gate: stringPointer("true"), replicas: stringPointer("0"), wantErr: true},
				{name: "true two replicas", gate: stringPointer("true"), replicas: stringPointer("2"), wantErr: true},
				{name: "false malformed replica", gate: stringPointer("false"), replicas: stringPointer("one"), wantErr: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					setOptionalEnv(t, "ACTIVITY_HISTORY_CLUSTER_ENABLED", tc.gate)
					setOptionalEnv(t, "CONTROL_PLANE_REPLICA_COUNT", tc.replicas)
					cfg := &Config{Environment: environment}

					err := loadActivityHistoryConfig(cfg)
					if tc.wantErr {
						require.Error(t, err)
						return
					}
					require.NoError(t, err)
					assert.Equal(t, tc.wantEnabled, cfg.ActivityHistoryClusterEnabled)
					assert.Equal(t, tc.wantExplicit, cfg.ControlPlaneReplicaCountExplicit)
					assert.Equal(t, tc.wantCount, cfg.ControlPlaneReplicaCount)
				})
			}
		})
	}
}

func TestLoadActivityHistoryConfigStoresOperatorDisclosureFields(t *testing.T) {
	setOptionalEnv(t, "ACTIVITY_HISTORY_CLUSTER_ENABLED", stringPointer("false"))
	setOptionalEnv(t, "CONTROL_PLANE_REPLICA_COUNT", stringPointer("3"))
	t.Setenv("ACTIVITY_HISTORY_OPERATOR_NAME", "Example Operator")
	t.Setenv("ACTIVITY_HISTORY_PRIVACY_POLICY_URL", "https://example.test/privacy")
	cfg := &Config{Environment: "development"}

	require.NoError(t, loadActivityHistoryConfig(cfg))
	assert.False(t, cfg.ActivityHistoryClusterEnabled)
	assert.True(t, cfg.ControlPlaneReplicaCountExplicit)
	assert.Equal(t, 3, cfg.ControlPlaneReplicaCount)
	assert.Equal(t, "Example Operator", cfg.ActivityHistoryOperatorName)
	assert.Equal(t, "https://example.test/privacy", cfg.ActivityHistoryPrivacyPolicyURL)
}

func TestLoadActivityHistoryConfigFailsClosedForNilConfigAndInvalidLoadInput(t *testing.T) {
	require.Error(t, loadActivityHistoryConfig(nil))
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("ACTIVITY_HISTORY_CLUSTER_ENABLED", "TRUE")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ACTIVITY_HISTORY_CLUSTER_ENABLED")
}

func TestValidateActivityHistoryRunsBeforeNonProductionEarlyReturn(t *testing.T) {
	for _, environment := range []string{"development", "test", "production"} {
		t.Run(environment, func(t *testing.T) {
			cfg := &Config{
				Environment:                      environment,
				ActivityHistoryClusterEnabled:    true,
				ControlPlaneReplicaCount:         2,
				ControlPlaneReplicaCountExplicit: true,
			}
			err := cfg.validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "activity history")
		})
	}
}

func stringPointer(value string) *string { return &value }

func setOptionalEnv(t *testing.T, key string, value *string) {
	t.Helper()
	old, existed := os.LookupEnv(key)
	require.NoError(t, os.Unsetenv(key))
	if value != nil {
		require.NoError(t, os.Setenv(key, *value))
	}
	t.Cleanup(func() {
		_ = os.Unsetenv(key)
		if existed {
			_ = os.Setenv(key, old)
		}
	})
}
