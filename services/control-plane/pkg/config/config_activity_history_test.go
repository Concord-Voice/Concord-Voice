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
	// The subject here is the operator disclosure fields; the replica count is
	// incidental and must be 1 — anything else is rejected outright (#2178).
	setOptionalEnv(t, "CONTROL_PLANE_REPLICA_COUNT", stringPointer("1"))
	t.Setenv("ACTIVITY_HISTORY_OPERATOR_NAME", "Example Operator")
	t.Setenv("ACTIVITY_HISTORY_PRIVACY_POLICY_URL", "https://example.test/privacy")
	cfg := &Config{Environment: "development"}

	require.NoError(t, loadActivityHistoryConfig(cfg))
	assert.False(t, cfg.ActivityHistoryClusterEnabled)
	assert.True(t, cfg.ControlPlaneReplicaCountExplicit)
	assert.Equal(t, 1, cfg.ControlPlaneReplicaCount)
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
			// The replica count is left implicit so this trips ONLY the
			// activity history invariant: an explicit count other than 1
			// would trip the unconditional replica guard (#2178), which
			// validate() runs first, masking the error under test.
			cfg := &Config{
				Environment:                   environment,
				ActivityHistoryClusterEnabled: true,
			}
			err := cfg.validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "activity history")
		})
	}
}

// TestControlPlaneReplicaCountGuardAndActivityHistoryPrecedence locks the
// unconditional single-replica guard (#2178) and its precedence over the
// Activity History invariant, which it coexists with rather than replaces.
//
// The final case is the ordering regression lock: with BOTH constraints
// violated, the operator must see the replica error. It drives the real Load()
// path — asserting against validate() in isolation would not catch a
// regression that moves or drops the load-time call, because
// loadActivityHistoryConfig fails during Load() long before validate() runs.
func TestControlPlaneReplicaCountGuardAndActivityHistoryPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		gate         *string
		replicas     *string
		viaLoad      bool
		wantContains []string
		wantExcludes []string
	}{
		{name: "implicit count with gate off"},
		{
			name:         "implicit count with gate on reports activity history",
			gate:         stringPointer("true"),
			wantContains: []string{"activity history"},
		},
		{name: "one replica with gate off", gate: stringPointer("false"), replicas: stringPointer("1")},
		{name: "one replica with gate on", gate: stringPointer("true"), replicas: stringPointer("1")},
		{
			name:         "two replicas with gate off",
			gate:         stringPointer("false"),
			replicas:     stringPointer("2"),
			wantContains: []string{"CONTROL_PLANE_REPLICA_COUNT=2", "#2757", "Redis"},
		},
		{
			name:         "zero replicas with gate off",
			gate:         stringPointer("false"),
			replicas:     stringPointer("0"),
			wantContains: []string{"CONTROL_PLANE_REPLICA_COUNT=0", "#2757", "Redis"},
		},
		{
			name:         "negative replicas with gate off",
			gate:         stringPointer("false"),
			replicas:     stringPointer("-1"),
			wantContains: []string{"CONTROL_PLANE_REPLICA_COUNT=-1", "#2757", "Redis"},
		},
		{
			name:         "four replicas with gate on reports the replica error not activity history",
			gate:         stringPointer("true"),
			replicas:     stringPointer("4"),
			viaLoad:      true,
			wantContains: []string{"CONTROL_PLANE_REPLICA_COUNT=4", "#2757", "Redis"},
			wantExcludes: []string{"activity history"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setOptionalEnv(t, "ACTIVITY_HISTORY_CLUSTER_ENABLED", tc.gate)
			setOptionalEnv(t, "CONTROL_PLANE_REPLICA_COUNT", tc.replicas)

			var err error
			if tc.viaLoad {
				t.Setenv("ENVIRONMENT", "development")
				_, err = Load()
			} else {
				err = loadActivityHistoryConfig(&Config{Environment: "development"})
			}

			if len(tc.wantContains) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range tc.wantContains {
				assert.Contains(t, err.Error(), want)
			}
			for _, unwanted := range tc.wantExcludes {
				assert.NotContains(t, err.Error(), unwanted)
			}
		})
	}
}

// TestValidateRejectsMultiReplicaBeforeActivityHistory covers the
// defense-in-depth call site: validate() must apply the replica guard even
// when the load-time call never ran (a Config assembled in-process).
func TestValidateRejectsMultiReplicaBeforeActivityHistory(t *testing.T) {
	cfg := &Config{
		Environment:                      "development",
		ActivityHistoryClusterEnabled:    true,
		ControlPlaneReplicaCount:         2,
		ControlPlaneReplicaCountExplicit: true,
	}

	err := cfg.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CONTROL_PLANE_REPLICA_COUNT=2")
	assert.Contains(t, err.Error(), "#2757")
	assert.NotContains(t, err.Error(), "activity history")
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
