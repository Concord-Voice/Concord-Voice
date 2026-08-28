package opsmetrics_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
)

// contractsPath is the admin console's closed metric contract. It is the CLIENT
// half of the same closed catalog this package owns, and nothing else couples
// the two: the SPA parses /current and /counters against its own fixed key sets
// and hard array bounds, so a key added here and not there makes both endpoints
// fail closed in the browser while the server keeps answering 200.
//
// That is not hypothetical. #2975 added presence_audience_suppressed_total to
// the Go catalog alone; the console then rendered every value as "Unavailable"
// under a "live telemetry is unavailable" banner — the SPA's NETWORK-failure
// copy — because ContractError reaches usePolling as ApiError(0). Server-side
// telemetry was healthy throughout, so no backend signal existed to contradict
// the banner, and the drift survived a provider migration before anyone traced
// it. This test is the missing coupling.
const contractsPath = "../../../../client/admin/src/contracts.ts"

// clientMetricKeys extracts one `as const` string-literal array from the SPA
// contract. A regex over TypeScript is coarse, but the alternative is a Node
// toolchain dependency inside a Go test, and these lists are fixed literal
// arrays by design — the closed catalog they mirror forbids computed members.
func clientMetricKeys(t *testing.T, source, name string) []string {
	t.Helper()

	block := regexp.MustCompile(`(?s)\b` + regexp.QuoteMeta(name) + `\s*=\s*\[(.*?)\]\s*as const`)
	match := block.FindStringSubmatch(source)
	require.Len(t, match, 2, "client contract must declare a literal %s array", name)

	literal := regexp.MustCompile(`"([a-z][a-z0-9_]*)"`)
	found := literal.FindAllStringSubmatch(match[1], -1)
	keys := make([]string, 0, len(found))
	for _, entry := range found {
		keys = append(keys, entry[1])
	}
	require.NotEmpty(t, keys, "client contract %s must not be empty", name)
	return keys
}

// TestClientContractMatchesCatalog fails when the Go catalog and the admin
// console's contract drift apart in EITHER direction. A missing client key
// breaks the console; a surplus client key is a dangling reference that renders
// as "Unknown metric".
func TestClientContractMatchesCatalog(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(contractsPath))
	require.NoError(t, err, "admin contract must be readable from the repository tree")
	source := string(raw)

	// METRIC_KEYS is a spread of the per-surface lists, so the concrete keys
	// live in those lists rather than in METRIC_KEYS itself.
	clientKeys := make(map[string]bool)
	for _, name := range []string{
		"HOST_METRIC_KEYS",
		"SERVICE_METRIC_KEYS",
		"CONTROL_METRIC_KEYS",
		"ACCOUNT_ACTIVITY_METRIC_KEYS",
		"MEDIA_ACTIVITY_METRIC_KEYS",
		"MEDIA_EGRESS_METRIC_KEYS",
		"PARTICIPANT_HOUR_METRIC_KEYS",
	} {
		for _, key := range clientMetricKeys(t, source, name) {
			require.False(t, clientKeys[key], "duplicate client metric key %q", key)
			clientKeys[key] = true
		}
	}

	catalogKeys := make(map[string]bool, opsmetrics.CatalogSize())
	catalogCounters := make(map[string]bool)
	for _, def := range opsmetrics.Catalog() {
		catalogKeys[string(def.Key)] = true
		if def.Kind == opsmetrics.KindCounter {
			catalogCounters[string(def.Key)] = true
		}
	}

	for key := range catalogKeys {
		require.True(t, clientKeys[key],
			"catalog key %q is missing from the admin contract; /current and /counters will fail closed in the console", key)
	}
	for key := range clientKeys {
		require.True(t, catalogKeys[key],
			"admin contract key %q is not in the catalog; the console will render it as an unknown metric", key)
	}

	clientCounters := make(map[string]bool)
	for _, key := range clientMetricKeys(t, source, "COUNTER_METRIC_KEYS") {
		clientCounters[key] = true
	}
	for key := range catalogCounters {
		require.True(t, clientCounters[key],
			"counter %q is missing from the admin contract's COUNTER_METRIC_KEYS; /counters will fail closed", key)
	}
	for key := range clientCounters {
		require.True(t, catalogCounters[key],
			"admin contract counter %q is not a catalogued counter", key)
	}
}
