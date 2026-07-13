package opsmetrics_test

import (
	"math"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/opsmetrics"
)

func TestCatalogIsClosedAndPrivacySafe(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)(^|_)(user|email|username|display_name|content|message_id|ip|url|host_name|server_id|channel_id|room_id)($|_)`)
	seen := make(map[opsmetrics.MetricKey]bool)

	for _, def := range opsmetrics.Catalog() {
		require.False(t, seen[def.Key], "duplicate metric key %q", def.Key)
		seen[def.Key] = true
		require.NotEmpty(t, def.Source)
		require.NotEmpty(t, def.Unit)
		require.NotEmpty(t, def.Kind)
		require.NotEmpty(t, def.Rollup)
		require.LessOrEqual(t, def.Min, def.Max)
		require.False(t, forbidden.MatchString(string(def.Key)), "privacy-sensitive metric key %q", def.Key)
	}

	require.Equal(t, opsmetrics.CatalogSize(), len(seen))
	require.NotZero(t, len(seen))
}

func TestValidateSampleRejectsUnknownSourceAndOutOfBoundsValues(t *testing.T) {
	tests := []struct {
		name   string
		sample opsmetrics.Sample
	}{
		{name: "unknown key", sample: opsmetrics.Sample{Key: "not_catalogued", Value: 1, Source: opsmetrics.SourceHost}},
		{name: "wrong source", sample: opsmetrics.Sample{Key: opsmetrics.MetricHostCPUPercent, Value: 20, Source: opsmetrics.SourceMedia}},
		{name: "negative percent", sample: opsmetrics.Sample{Key: opsmetrics.MetricHostCPUPercent, Value: -1, Source: opsmetrics.SourceHost}},
		{name: "percent over max", sample: opsmetrics.Sample{Key: opsmetrics.MetricHostCPUPercent, Value: 101, Source: opsmetrics.SourceHost}},
		{name: "nan", sample: opsmetrics.Sample{Key: opsmetrics.MetricHostCPUPercent, Value: math.NaN(), Source: opsmetrics.SourceHost}},
		{name: "infinity", sample: opsmetrics.Sample{Key: opsmetrics.MetricHostCPUPercent, Value: math.Inf(1), Source: opsmetrics.SourceHost}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, opsmetrics.ValidateSample(tt.sample))
		})
	}

	require.NoError(t, opsmetrics.ValidateSample(opsmetrics.Sample{
		Key:    opsmetrics.MetricHostCPUPercent,
		Value:  42.5,
		Source: opsmetrics.SourceHost,
	}))
	require.NoError(t, opsmetrics.ValidateSample(opsmetrics.Sample{
		Key:    opsmetrics.MetricServiceMediaPlaneCPUPercent,
		Value:  25_600,
		Source: opsmetrics.SourceHost,
	}))
}

func TestMediaEgressRatesUseBitsPerSecond(t *testing.T) {
	for _, key := range []opsmetrics.MetricKey{
		opsmetrics.MetricMediaEgressCurrentBPS,
		opsmetrics.MetricMediaEgressPeakBPS,
	} {
		definition, ok := opsmetrics.Definition(key)
		require.True(t, ok)
		require.Equal(t, opsmetrics.Unit("bits_per_second"), definition.Unit)
	}
}

func TestCatalogReturnsAnIndependentCopy(t *testing.T) {
	first := opsmetrics.Catalog()
	require.NotEmpty(t, first)
	first[0].Key = "mutated"

	second := opsmetrics.Catalog()
	require.NotEqual(t, opsmetrics.MetricKey("mutated"), second[0].Key)
}
