// Package opsmetrics defines Concord's aggregate-only operations metric boundary.
package opsmetrics

import (
	"fmt"
	"math"
	"sort"
)

// MetricKey is a closed, dimension-free operations metric identifier.
type MetricKey string

// Source identifies the fixed producer that owns a metric.
type Source string

// Unit describes the scalar value represented by a metric.
type Unit string

// Kind distinguishes point-in-time gauges from monotonic counters.
type Kind string

// RollupMode selects how a metric is represented in downsampled buckets.
type RollupMode string

//nolint:revive // The typed source/unit/kind constants are self-describing as a closed schema.
const (
	// SourceHost and the other Source constants are the only accepted producers.
	SourceHost    Source = "host"
	SourceControl Source = "control"
	SourceMedia   Source = "media"

	UnitPercent       Unit = "percent"
	UnitCount         Unit = "count"
	UnitBytes         Unit = "bytes"
	UnitBitsPerSecond Unit = "bits_per_second"
	UnitLoad          Unit = "load"
	UnitHours         Unit = "hours"

	KindGauge   Kind = "gauge"
	KindCounter Kind = "counter"

	RollupAverage RollupMode = "average"
	RollupLast    RollupMode = "last"
)

//nolint:revive // Metric names are intentionally explicit and share one closed-schema comment.
const (
	// MetricHostCPUPercent and the other metric constants form the complete v1 schema.
	MetricHostCPUPercent    MetricKey = "host_cpu_percent"
	MetricHostMemoryPercent MetricKey = "host_memory_percent"
	MetricHostDiskPercent   MetricKey = "host_disk_percent"
	MetricHostLoad1M        MetricKey = "host_load_1m"

	MetricServiceControlPlaneRunning     MetricKey = "service_control_plane_running"
	MetricServiceControlPlaneHealthy     MetricKey = "service_control_plane_healthy"
	MetricServiceControlPlaneCPUPercent  MetricKey = "service_control_plane_cpu_percent"
	MetricServiceControlPlaneMemoryBytes MetricKey = "service_control_plane_memory_bytes"
	MetricServiceMediaPlaneRunning       MetricKey = "service_media_plane_running"
	MetricServiceMediaPlaneHealthy       MetricKey = "service_media_plane_healthy"
	MetricServiceMediaPlaneCPUPercent    MetricKey = "service_media_plane_cpu_percent"
	MetricServiceMediaPlaneMemoryBytes   MetricKey = "service_media_plane_memory_bytes"
	MetricServicePostgresRunning         MetricKey = "service_postgres_running"
	MetricServicePostgresHealthy         MetricKey = "service_postgres_healthy"
	MetricServicePostgresCPUPercent      MetricKey = "service_postgres_cpu_percent"
	MetricServicePostgresMemoryBytes     MetricKey = "service_postgres_memory_bytes"
	MetricServiceRedisRunning            MetricKey = "service_redis_running"
	MetricServiceRedisHealthy            MetricKey = "service_redis_healthy"
	MetricServiceRedisCPUPercent         MetricKey = "service_redis_cpu_percent"
	MetricServiceRedisMemoryBytes        MetricKey = "service_redis_memory_bytes"
	MetricServiceNATSRunning             MetricKey = "service_nats_running"
	MetricServiceNATSHealthy             MetricKey = "service_nats_healthy"
	MetricServiceNATSCPUPercent          MetricKey = "service_nats_cpu_percent"
	MetricServiceNATSMemoryBytes         MetricKey = "service_nats_memory_bytes"
	MetricServiceMinIORunning            MetricKey = "service_minio_running"
	MetricServiceMinIOHealthy            MetricKey = "service_minio_healthy"
	MetricServiceMinIOCPUPercent         MetricKey = "service_minio_cpu_percent"
	MetricServiceMinIOMemoryBytes        MetricKey = "service_minio_memory_bytes"
	MetricServiceCoturnRunning           MetricKey = "service_coturn_running"
	MetricServiceCoturnHealthy           MetricKey = "service_coturn_healthy"
	MetricServiceCoturnCPUPercent        MetricKey = "service_coturn_cpu_percent"
	MetricServiceCoturnMemoryBytes       MetricKey = "service_coturn_memory_bytes"

	MetricHTTPRequestsTotal           MetricKey = "http_requests_total"
	MetricHTTPClientErrorsTotal       MetricKey = "http_client_errors_total"
	MetricHTTPServerErrorsTotal       MetricKey = "http_server_errors_total"
	MetricWebSocketConnections        MetricKey = "websocket_connections_current"
	MetricChannelMessagesTotal        MetricKey = "channel_messages_total"
	MetricDMMessagesTotal             MetricKey = "dm_messages_total"
	MetricSnapshotRejectionsTotal     MetricKey = "ops_snapshot_rejections_total"
	MetricRegisteredUsersCurrent      MetricKey = "registered_users_current"
	MetricPendingRegistrationsCurrent MetricKey = "pending_registrations_current"
	MetricUsersOnlineCurrent          MetricKey = "users_online_current"
	MetricActiveSessionsCurrent       MetricKey = "active_sessions_current"
	MetricActiveUsers24H              MetricKey = "active_users_24h"
	MetricActiveUsers7D               MetricKey = "active_users_7d"
	MetricActiveUsers15D              MetricKey = "active_users_15d"
	MetricActiveUsers30D              MetricKey = "active_users_30d"
	MetricMediaUploadsTotal           MetricKey = "media_uploads_total"

	MetricMediaRoomsCurrent                   MetricKey = "media_rooms_current"
	MetricMediaParticipantsAudioCurrent       MetricKey = "media_participants_audio_current"
	MetricMediaParticipantsWebcamCurrent      MetricKey = "media_participants_webcam_current"
	MetricMediaParticipantsScreenshareCurrent MetricKey = "media_participants_screenshare_current"
	MetricMediaCameraPublishersCurrent        MetricKey = "media_camera_publishers_current"
	MetricMediaScreenPublishersCurrent        MetricKey = "media_screen_publishers_current"
	MetricMediaPeakVideoPublishersPerRoom     MetricKey = "media_peak_video_publishers_per_room"
	MetricMediaEgressCurrentBPS               MetricKey = "media_egress_current_bps"
	MetricMediaEgressPeakBPS                  MetricKey = "media_egress_peak_bps"
	MetricMediaEgressCumulativeBytes          MetricKey = "media_egress_cumulative_bytes"
	MetricMediaParticipantHoursAudio          MetricKey = "media_participant_hours_audio"
	MetricMediaParticipantHoursWebcam         MetricKey = "media_participant_hours_webcam"
	MetricMediaParticipantHoursScreenshare    MetricKey = "media_participant_hours_screenshare"
)

// MetricDefinition declares ownership, units, bounds, and rollup behavior.
type MetricDefinition struct {
	Key    MetricKey
	Source Source
	Unit   Unit
	Kind   Kind
	Rollup RollupMode
	Min    float64
	Max    float64
}

// Sample is one validated scalar value emitted by a fixed source.
type Sample struct {
	Key    MetricKey
	Value  float64
	Source Source
}

const (
	maxCount             = 1e15
	maxBytes             = 1e18
	maxRate              = 1e15
	maxHours             = 1e12
	maxServiceCPUPercent = 1e6
)

func metric(key MetricKey, source Source, unit Unit, kind Kind, rollup RollupMode, minValue, maxValue float64) MetricDefinition {
	return MetricDefinition{Key: key, Source: source, Unit: unit, Kind: kind, Rollup: rollup, Min: minValue, Max: maxValue}
}

func serviceMetrics(running, healthy, cpu, memory MetricKey) []MetricDefinition {
	return []MetricDefinition{
		metric(running, SourceHost, UnitCount, KindGauge, RollupLast, 0, 1),
		metric(healthy, SourceHost, UnitCount, KindGauge, RollupLast, 0, 1),
		metric(cpu, SourceHost, UnitPercent, KindGauge, RollupAverage, 0, maxServiceCPUPercent),
		metric(memory, SourceHost, UnitBytes, KindGauge, RollupAverage, 0, maxBytes),
	}
}

var catalog = func() map[MetricKey]MetricDefinition {
	definitions := make([]MetricDefinition, 0, 61)
	definitions = append(definitions,
		metric(MetricHostCPUPercent, SourceHost, UnitPercent, KindGauge, RollupAverage, 0, 100),
		metric(MetricHostMemoryPercent, SourceHost, UnitPercent, KindGauge, RollupAverage, 0, 100),
		metric(MetricHostDiskPercent, SourceHost, UnitPercent, KindGauge, RollupAverage, 0, 100),
		metric(MetricHostLoad1M, SourceHost, UnitLoad, KindGauge, RollupAverage, 0, 1e6),
		metric(MetricHTTPRequestsTotal, SourceControl, UnitCount, KindCounter, RollupLast, 0, maxCount),
		metric(MetricHTTPClientErrorsTotal, SourceControl, UnitCount, KindCounter, RollupLast, 0, maxCount),
		metric(MetricHTTPServerErrorsTotal, SourceControl, UnitCount, KindCounter, RollupLast, 0, maxCount),
		metric(MetricWebSocketConnections, SourceControl, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricChannelMessagesTotal, SourceControl, UnitCount, KindCounter, RollupLast, 0, maxCount),
		metric(MetricDMMessagesTotal, SourceControl, UnitCount, KindCounter, RollupLast, 0, maxCount),
		metric(MetricSnapshotRejectionsTotal, SourceControl, UnitCount, KindCounter, RollupLast, 0, maxCount),
		metric(MetricRegisteredUsersCurrent, SourceControl, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricPendingRegistrationsCurrent, SourceControl, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricUsersOnlineCurrent, SourceControl, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricActiveSessionsCurrent, SourceControl, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricActiveUsers24H, SourceControl, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricActiveUsers7D, SourceControl, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricActiveUsers15D, SourceControl, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricActiveUsers30D, SourceControl, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricMediaUploadsTotal, SourceControl, UnitCount, KindCounter, RollupLast, 0, maxCount),
		metric(MetricMediaRoomsCurrent, SourceMedia, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricMediaParticipantsAudioCurrent, SourceMedia, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricMediaParticipantsWebcamCurrent, SourceMedia, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricMediaParticipantsScreenshareCurrent, SourceMedia, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricMediaCameraPublishersCurrent, SourceMedia, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricMediaScreenPublishersCurrent, SourceMedia, UnitCount, KindGauge, RollupAverage, 0, maxCount),
		metric(MetricMediaPeakVideoPublishersPerRoom, SourceMedia, UnitCount, KindGauge, RollupLast, 0, maxCount),
		metric(MetricMediaEgressCurrentBPS, SourceMedia, UnitBitsPerSecond, KindGauge, RollupAverage, 0, maxRate),
		metric(MetricMediaEgressPeakBPS, SourceMedia, UnitBitsPerSecond, KindGauge, RollupLast, 0, maxRate),
		metric(MetricMediaEgressCumulativeBytes, SourceMedia, UnitBytes, KindCounter, RollupLast, 0, maxBytes),
		metric(MetricMediaParticipantHoursAudio, SourceMedia, UnitHours, KindCounter, RollupLast, 0, maxHours),
		metric(MetricMediaParticipantHoursWebcam, SourceMedia, UnitHours, KindCounter, RollupLast, 0, maxHours),
		metric(MetricMediaParticipantHoursScreenshare, SourceMedia, UnitHours, KindCounter, RollupLast, 0, maxHours),
	)

	definitions = append(definitions,
		serviceMetrics(MetricServiceControlPlaneRunning, MetricServiceControlPlaneHealthy, MetricServiceControlPlaneCPUPercent, MetricServiceControlPlaneMemoryBytes)...)
	definitions = append(definitions,
		serviceMetrics(MetricServiceMediaPlaneRunning, MetricServiceMediaPlaneHealthy, MetricServiceMediaPlaneCPUPercent, MetricServiceMediaPlaneMemoryBytes)...)
	definitions = append(definitions,
		serviceMetrics(MetricServicePostgresRunning, MetricServicePostgresHealthy, MetricServicePostgresCPUPercent, MetricServicePostgresMemoryBytes)...)
	definitions = append(definitions,
		serviceMetrics(MetricServiceRedisRunning, MetricServiceRedisHealthy, MetricServiceRedisCPUPercent, MetricServiceRedisMemoryBytes)...)
	definitions = append(definitions,
		serviceMetrics(MetricServiceNATSRunning, MetricServiceNATSHealthy, MetricServiceNATSCPUPercent, MetricServiceNATSMemoryBytes)...)
	definitions = append(definitions,
		serviceMetrics(MetricServiceMinIORunning, MetricServiceMinIOHealthy, MetricServiceMinIOCPUPercent, MetricServiceMinIOMemoryBytes)...)
	definitions = append(definitions,
		serviceMetrics(MetricServiceCoturnRunning, MetricServiceCoturnHealthy, MetricServiceCoturnCPUPercent, MetricServiceCoturnMemoryBytes)...)

	result := make(map[MetricKey]MetricDefinition, len(definitions))
	for _, definition := range definitions {
		if _, exists := result[definition.Key]; exists {
			panic("duplicate operations metric key: " + string(definition.Key))
		}
		result[definition.Key] = definition
	}
	return result
}()

// Definition returns the immutable definition for a catalogued key.
func Definition(key MetricKey) (MetricDefinition, bool) {
	definition, ok := catalog[key]
	return definition, ok
}

// Catalog returns a stable, independent copy of every metric definition.
func Catalog() []MetricDefinition {
	definitions := make([]MetricDefinition, 0, len(catalog))
	for _, definition := range catalog {
		definitions = append(definitions, definition)
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Key < definitions[j].Key })
	return definitions
}

// CatalogSize returns the number of accepted v1 metric keys.
func CatalogSize() int { return len(catalog) }

// ValidateSample enforces catalog ownership, finiteness, and numeric bounds.
func ValidateSample(sample Sample) error {
	definition, ok := Definition(sample.Key)
	if !ok {
		return fmt.Errorf("unknown metric key %q", sample.Key)
	}
	if sample.Source != definition.Source {
		return fmt.Errorf("metric %q does not belong to source %q", sample.Key, sample.Source)
	}
	if math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) {
		return fmt.Errorf("metric %q must be finite", sample.Key)
	}
	if sample.Value < definition.Min || sample.Value > definition.Max {
		return fmt.Errorf("metric %q is outside allowed range", sample.Key)
	}
	return nil
}
