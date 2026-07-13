package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func clearOpsMetricsEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OPS_METRICS_ENABLED",
		"OPS_METRICS_NODE_ID",
		"OPS_METRICS_SHARED_SECRET",
		"OPS_METRICS_INTERVAL",
		"OPS_METRICS_ROLE",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadOpsMetricsConfigDisabledDefaults(t *testing.T) {
	clearOpsMetricsEnv(t)

	cfg, err := LoadOpsMetricsConfig()
	require.NoError(t, err)
	assert.False(t, cfg.Enabled)
	assert.Empty(t, cfg.NodeID)
	assert.Empty(t, cfg.SharedSecret)
	assert.Equal(t, 15*time.Second, cfg.Interval)
	assert.Equal(t, OpsMetricsRoleLocal, cfg.Role)
}

func TestLoadOpsMetricsConfigEnabled(t *testing.T) {
	clearOpsMetricsEnv(t)
	t.Setenv("OPS_METRICS_ENABLED", "true")
	t.Setenv("OPS_METRICS_NODE_ID", "cvn_aaaaaaaaaaaaaaaa")
	t.Setenv("OPS_METRICS_SHARED_SECRET", "0123456789abcdef0123456789abcdef") // pragma: allowlist secret
	t.Setenv("OPS_METRICS_INTERVAL", "30s")
	t.Setenv("OPS_METRICS_ROLE", "local")

	cfg, err := LoadOpsMetricsConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Enabled)
	assert.Equal(t, "cvn_aaaaaaaaaaaaaaaa", cfg.NodeID)
	assert.Equal(t, 30*time.Second, cfg.Interval)
	assert.Equal(t, OpsMetricsRoleLocal, cfg.Role)
	assert.NotContains(t, cfg.String(), cfg.SharedSecret)
	assert.Contains(t, cfg.String(), "[REDACTED 32 bytes]")
}

func TestLoadOpsMetricsConfigRejectsEnabledMisconfiguration(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantErrors []string
	}{
		{
			name:       "missing node and secret",
			env:        map[string]string{"OPS_METRICS_ENABLED": "true"},
			wantErrors: []string{"OPS_METRICS_NODE_ID", "OPS_METRICS_SHARED_SECRET"},
		},
		{
			name: "invalid node id",
			env: map[string]string{
				"OPS_METRICS_ENABLED":       "true",
				"OPS_METRICS_NODE_ID":       "api.concordvoice.chat",
				"OPS_METRICS_SHARED_SECRET": "0123456789abcdef0123456789abcdef", // pragma: allowlist secret
			},
			wantErrors: []string{"OPS_METRICS_NODE_ID"},
		},
		{
			name: "short secret",
			env: map[string]string{
				"OPS_METRICS_ENABLED":       "true",
				"OPS_METRICS_NODE_ID":       "cvn_aaaaaaaaaaaaaaaa",
				"OPS_METRICS_SHARED_SECRET": "short", // pragma: allowlist secret
			},
			wantErrors: []string{"OPS_METRICS_SHARED_SECRET"},
		},
		{
			name: "interval too short",
			env: map[string]string{
				"OPS_METRICS_ENABLED":       "true",
				"OPS_METRICS_NODE_ID":       "cvn_aaaaaaaaaaaaaaaa",
				"OPS_METRICS_SHARED_SECRET": "0123456789abcdef0123456789abcdef", // pragma: allowlist secret
				"OPS_METRICS_INTERVAL":      "4s",
			},
			wantErrors: []string{"OPS_METRICS_INTERVAL"},
		},
		{
			name: "interval too long",
			env: map[string]string{
				"OPS_METRICS_ENABLED":       "true",
				"OPS_METRICS_NODE_ID":       "cvn_aaaaaaaaaaaaaaaa",
				"OPS_METRICS_SHARED_SECRET": "0123456789abcdef0123456789abcdef", // pragma: allowlist secret
				"OPS_METRICS_INTERVAL":      "6m",
			},
			wantErrors: []string{"OPS_METRICS_INTERVAL"},
		},
		{
			name: "aggregator role reserved",
			env: map[string]string{
				"OPS_METRICS_ENABLED":       "true",
				"OPS_METRICS_NODE_ID":       "cvn_aaaaaaaaaaaaaaaa",
				"OPS_METRICS_SHARED_SECRET": "0123456789abcdef0123456789abcdef", // pragma: allowlist secret
				"OPS_METRICS_ROLE":          "aggregator",
			},
			wantErrors: []string{"OPS_METRICS_ROLE", "#1504"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearOpsMetricsEnv(t)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			_, err := LoadOpsMetricsConfig()
			require.Error(t, err)
			for _, message := range tt.wantErrors {
				assert.Contains(t, err.Error(), message)
			}
		})
	}
}

func TestLoadIncludesOpsMetricsConfig(t *testing.T) {
	clearOpsMetricsEnv(t)
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("OPS_METRICS_ENABLED", "true")
	t.Setenv("OPS_METRICS_NODE_ID", "cvn_aaaaaaaaaaaaaaaa")
	t.Setenv("OPS_METRICS_SHARED_SECRET", "0123456789abcdef0123456789abcdef") // pragma: allowlist secret

	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.OpsMetrics.Enabled)
}
