package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/opsmetrics"
)

// OpsMetricsRole selects the local storage mode. Aggregation is reserved for #1504.
type OpsMetricsRole string

const (
	// OpsMetricsRoleLocal stores this node's aggregate samples locally.
	OpsMetricsRoleLocal OpsMetricsRole = "local"
	// OpsMetricsRoleAggregator is reserved until the multi-region ingest boundary ships.
	OpsMetricsRoleAggregator OpsMetricsRole = "aggregator"
)

// OpsMetricsConfig contains the shared control-plane and ops-agent settings.
type OpsMetricsConfig struct {
	Enabled      bool
	NodeID       string
	SharedSecret string // #nosec G117 -- config field, secret is loaded from the environment
	Interval     time.Duration
	Role         OpsMetricsRole
}

// String redacts the snapshot signing secret.
func (c OpsMetricsConfig) String() string {
	return fmt.Sprintf(
		"OpsMetricsConfig{Enabled:%t NodeID:%q SharedSecret:[REDACTED %d bytes] Interval:%s Role:%q}",
		c.Enabled,
		c.NodeID,
		len(c.SharedSecret),
		c.Interval,
		c.Role,
	)
}

// LoadOpsMetricsConfig reads and validates the dormant-by-default metrics settings.
func LoadOpsMetricsConfig() (OpsMetricsConfig, error) {
	enabled := strings.EqualFold(strings.TrimSpace(os.Getenv("OPS_METRICS_ENABLED")), "true")
	if !enabled {
		return OpsMetricsConfig{
			Enabled:  false,
			Interval: 15 * time.Second,
			Role:     OpsMetricsRoleLocal,
		}, nil
	}

	rawInterval := strings.TrimSpace(os.Getenv("OPS_METRICS_INTERVAL"))
	if rawInterval == "" {
		rawInterval = "15s"
	}
	interval, err := time.ParseDuration(rawInterval)
	if err != nil {
		return OpsMetricsConfig{}, fmt.Errorf("OPS_METRICS_INTERVAL must be a duration from 5s through 5m: %w", err)
	}

	role := OpsMetricsRole(strings.ToLower(strings.TrimSpace(os.Getenv("OPS_METRICS_ROLE"))))
	if role == "" {
		role = OpsMetricsRoleLocal
	}
	cfg := OpsMetricsConfig{
		Enabled:      true,
		NodeID:       strings.TrimSpace(os.Getenv("OPS_METRICS_NODE_ID")),
		SharedSecret: strings.TrimSpace(os.Getenv("OPS_METRICS_SHARED_SECRET")),
		Interval:     interval,
		Role:         role,
	}

	var problems []string
	if err := opsmetrics.ValidateNodeID(cfg.NodeID); err != nil {
		problems = append(problems, "OPS_METRICS_NODE_ID must be an opaque assigned cvn_ token")
	}
	if len(cfg.SharedSecret) < 32 {
		problems = append(problems, "OPS_METRICS_SHARED_SECRET must be at least 32 bytes")
	}
	if cfg.Interval < 5*time.Second || cfg.Interval > 5*time.Minute {
		problems = append(problems, "OPS_METRICS_INTERVAL must be from 5s through 5m")
	}
	switch cfg.Role {
	case OpsMetricsRoleLocal:
	case OpsMetricsRoleAggregator:
		problems = append(problems, "OPS_METRICS_ROLE=aggregator is reserved until #1504")
	default:
		problems = append(problems, "OPS_METRICS_ROLE must be local")
	}
	if len(problems) > 0 {
		return OpsMetricsConfig{}, fmt.Errorf("invalid operations metrics config: %s", strings.Join(problems, "; "))
	}
	return cfg, nil
}
