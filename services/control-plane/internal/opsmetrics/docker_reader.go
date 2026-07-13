package opsmetrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	dockerAPIHost          = "docker"
	maxDockerResponseBytes = 1 << 20
	dockerRequestTimeout   = 5 * time.Second
)

var dockerContainerIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

type concordService struct {
	containerName string
	running       MetricKey
	healthy       MetricKey
	cpu           MetricKey
	memory        MetricKey
}

var concordServiceAllowlist = []concordService{
	{containerName: "concordvoice-control-plane", running: MetricServiceControlPlaneRunning, healthy: MetricServiceControlPlaneHealthy, cpu: MetricServiceControlPlaneCPUPercent, memory: MetricServiceControlPlaneMemoryBytes},
	{containerName: "concordvoice-media-plane", running: MetricServiceMediaPlaneRunning, healthy: MetricServiceMediaPlaneHealthy, cpu: MetricServiceMediaPlaneCPUPercent, memory: MetricServiceMediaPlaneMemoryBytes},
	{containerName: "concordvoice-postgres", running: MetricServicePostgresRunning, healthy: MetricServicePostgresHealthy, cpu: MetricServicePostgresCPUPercent, memory: MetricServicePostgresMemoryBytes},
	{containerName: "concordvoice-redis", running: MetricServiceRedisRunning, healthy: MetricServiceRedisHealthy, cpu: MetricServiceRedisCPUPercent, memory: MetricServiceRedisMemoryBytes},
	{containerName: "concordvoice-nats", running: MetricServiceNATSRunning, healthy: MetricServiceNATSHealthy, cpu: MetricServiceNATSCPUPercent, memory: MetricServiceNATSMemoryBytes},
	{containerName: "concordvoice-minio", running: MetricServiceMinIORunning, healthy: MetricServiceMinIOHealthy, cpu: MetricServiceMinIOCPUPercent, memory: MetricServiceMinIOMemoryBytes},
	{containerName: "concordvoice-coturn", running: MetricServiceCoturnRunning, healthy: MetricServiceCoturnHealthy, cpu: MetricServiceCoturnCPUPercent, memory: MetricServiceCoturnMemoryBytes},
}

type dockerContainerSummary struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

type dockerCPUUsage struct {
	TotalUsage  uint64   `json:"total_usage"`
	PercpuUsage []uint64 `json:"percpu_usage"`
}

type dockerCPUStats struct {
	CPUUsage       dockerCPUUsage `json:"cpu_usage"`
	SystemCPUUsage uint64         `json:"system_cpu_usage"`
	OnlineCPUs     uint64         `json:"online_cpus"`
}

type dockerMemoryStats struct {
	Usage uint64 `json:"usage"`
}

type dockerContainerStats struct {
	CPUStats    dockerCPUStats    `json:"cpu_stats"`
	PreCPUStats dockerCPUStats    `json:"precpu_stats"`
	MemoryStats dockerMemoryStats `json:"memory_stats"`
}

// DockerReader converts allowlisted Docker state into closed-schema scalars.
type DockerReader struct {
	client     *http.Client
	socketPath string
}

// NewDockerReader creates a GET-only Docker API client over one Unix socket.
func NewDockerReader(socketURL string) (*DockerReader, error) {
	parsed, err := url.Parse(socketURL)
	if err != nil {
		return nil, fmt.Errorf("parse Docker socket URL: %w", err)
	}
	if parsed.Scheme != "unix" || parsed.Host != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, errors.New("docker socket URL must use unix:///absolute/path with no authority, query, or fragment")
	}
	if !filepath.IsAbs(parsed.Path) || filepath.Clean(parsed.Path) != parsed.Path || parsed.Path == "/" {
		return nil, errors.New("docker socket URL must contain a clean absolute socket path")
	}

	socketPath := parsed.Path
	dialer := &net.Dialer{Timeout: dockerRequestTimeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	reader := newDockerReader(transport)
	reader.socketPath = socketPath
	return reader, nil
}

func newDockerReader(transport http.RoundTripper) *DockerReader {
	return &DockerReader{
		client: &http.Client{
			Transport: transport,
			Timeout:   dockerRequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Read returns liveness and resource metrics for the fixed Concord service set.
func (r *DockerReader) Read(ctx context.Context) (map[MetricKey]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var containers []dockerContainerSummary
	if err := r.getJSON(ctx, "/v1.44/containers/json", "all=1", &containers); err != nil {
		return nil, err
	}
	allowed, err := indexAllowlistedContainers(containers)
	if err != nil {
		return nil, err
	}

	metrics := make(map[MetricKey]float64, len(concordServiceAllowlist)*4)
	for _, service := range concordServiceAllowlist {
		if err := r.readServiceMetrics(ctx, metrics, allowed, service); err != nil {
			return nil, err
		}
	}

	for key, value := range metrics {
		if err := ValidateSample(Sample{Key: key, Value: value, Source: SourceHost}); err != nil {
			return nil, fmt.Errorf("validate Docker metric: %w", err)
		}
	}
	return metrics, nil
}

func (r *DockerReader) readServiceMetrics(ctx context.Context, metrics map[MetricKey]float64, allowed map[string]dockerContainerSummary, service concordService) error {
	metrics[service.running] = 0
	metrics[service.healthy] = 0
	metrics[service.cpu] = 0
	metrics[service.memory] = 0

	container, exists := allowed[service.containerName]
	if !exists || container.State != "running" {
		return nil
	}
	metrics[service.running] = 1
	metrics[service.healthy] = dockerHealthValue(container.Status)
	if !dockerContainerIDPattern.MatchString(container.ID) {
		return errors.New("invalid Docker container id for allowlisted service")
	}

	var stats dockerContainerStats
	statsPath := "/v1.44/containers/" + container.ID + "/stats"
	if err := r.getJSON(ctx, statsPath, "stream=false", &stats); err != nil {
		return err
	}
	cpuPercent, err := calculateContainerCPUPercent(stats)
	if err != nil {
		return err
	}
	metrics[service.cpu] = cpuPercent
	metrics[service.memory] = float64(stats.MemoryStats.Usage)
	return nil
}

func indexAllowlistedContainers(containers []dockerContainerSummary) (map[string]dockerContainerSummary, error) {
	allowedNames := make(map[string]struct{}, len(concordServiceAllowlist))
	for _, service := range concordServiceAllowlist {
		allowedNames["/"+service.containerName] = struct{}{}
	}

	result := make(map[string]dockerContainerSummary, len(concordServiceAllowlist))
	for _, container := range containers {
		for _, name := range container.Names {
			if _, allowed := allowedNames[name]; !allowed {
				continue
			}
			canonicalName := strings.TrimPrefix(name, "/")
			if existing, duplicate := result[canonicalName]; duplicate && existing.ID != container.ID {
				return nil, errors.New("duplicate allowlisted Docker container")
			}
			result[canonicalName] = container
		}
	}
	return result, nil
}

func dockerHealthValue(status string) float64 {
	normalized := strings.ToLower(status)
	if strings.Contains(normalized, "unhealthy") || strings.Contains(normalized, "health: starting") {
		return 0
	}
	return 1
}

func calculateContainerCPUPercent(stats dockerContainerStats) (float64, error) {
	currentCPU := stats.CPUStats.CPUUsage.TotalUsage
	previousCPU := stats.PreCPUStats.CPUUsage.TotalUsage
	currentSystem := stats.CPUStats.SystemCPUUsage
	previousSystem := stats.PreCPUStats.SystemCPUUsage
	if currentCPU < previousCPU || currentSystem < previousSystem {
		return 0, errors.New("docker CPU counters moved backwards")
	}
	systemDelta := currentSystem - previousSystem
	if systemDelta == 0 {
		return 0, errors.New("docker system CPU total did not advance")
	}
	onlineCPUs := stats.CPUStats.OnlineCPUs
	if onlineCPUs == 0 {
		onlineCPUs = uint64(len(stats.CPUStats.CPUUsage.PercpuUsage))
	}
	if onlineCPUs == 0 {
		return 0, errors.New("docker CPU count is missing")
	}
	percent := float64(currentCPU-previousCPU) / float64(systemDelta) * float64(onlineCPUs) * 100
	if math.IsNaN(percent) || math.IsInf(percent, 0) {
		return 0, errors.New("docker CPU percent must be finite")
	}
	return percent, nil
}

func (r *DockerReader) getJSON(ctx context.Context, path, rawQuery string, destination any) error {
	requestURL := &url.URL{Scheme: "http", Host: dockerAPIHost, Path: path, RawQuery: rawQuery}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return fmt.Errorf("create Docker request: %w", err)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("docker request failed: %w", err)
	}
	if response.Body == nil {
		return errors.New("docker response body is missing")
	}
	if response.StatusCode != http.StatusOK {
		closeErr := response.Body.Close()
		if closeErr != nil {
			return fmt.Errorf("close Docker response: %w", closeErr)
		}
		return fmt.Errorf("unexpected Docker status %d", response.StatusCode)
	}

	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxDockerResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return fmt.Errorf("read Docker response: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close Docker response: %w", closeErr)
	}
	if len(body) > maxDockerResponseBytes {
		return fmt.Errorf("docker response exceeds %d bytes", maxDockerResponseBytes)
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode Docker response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("docker response contains trailing JSON")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}
