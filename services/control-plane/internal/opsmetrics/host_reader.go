package opsmetrics

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const maxHostLineBytes = 64 * 1024

type statFSFunc func(path string, stat *syscall.Statfs_t) error

type cpuTimes struct {
	total uint64
	idle  uint64
}

type memorySnapshot struct {
	totalBytes     uint64
	availableBytes uint64
	foundTotal     bool
	foundAvailable bool
}

// HostReader reads aggregate host utilization from a mounted proc filesystem.
type HostReader struct {
	mu          sync.Mutex
	procFS      fs.FS
	diskPath    string
	statFS      statFSFunc
	previousCPU *cpuTimes
}

// NewHostReader creates a host reader rooted at the mounted host proc path.
func NewHostReader(procRoot, diskPath string) *HostReader {
	return newHostReader(os.DirFS(procRoot), diskPath, syscall.Statfs)
}

func newHostReader(procFS fs.FS, diskPath string, statFS statFSFunc) *HostReader {
	return &HostReader{procFS: procFS, diskPath: diskPath, statFS: statFS}
}

// Read returns only catalogued aggregate host metrics.
func (r *HostReader) Read(ctx context.Context) (map[MetricKey]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	currentCPU, err := readCPUTimes(r.procFS)
	if err != nil {
		return nil, err
	}
	memoryPercent, err := readMemoryPercent(r.procFS)
	if err != nil {
		return nil, err
	}
	load, err := readLoad1M(r.procFS)
	if err != nil {
		return nil, err
	}
	diskPercent, err := readDiskPercent(r.diskPath, r.statFS)
	if err != nil {
		return nil, err
	}

	metrics := map[MetricKey]float64{
		MetricHostMemoryPercent: memoryPercent,
		MetricHostDiskPercent:   diskPercent,
		MetricHostLoad1M:        load,
	}
	if r.previousCPU != nil {
		cpuPercent, cpuErr := calculateCPUPercent(*r.previousCPU, currentCPU)
		if cpuErr != nil {
			return nil, cpuErr
		}
		metrics[MetricHostCPUPercent] = cpuPercent
	}

	for key, value := range metrics {
		if err := ValidateSample(Sample{Key: key, Value: value, Source: SourceHost}); err != nil {
			return nil, fmt.Errorf("validate host metric: %w", err)
		}
	}

	copyCPU := currentCPU
	r.previousCPU = &copyCPU
	return metrics, nil
}

func readCPUTimes(procFS fs.FS) (cpuTimes, error) {
	line, err := readFirstLine(procFS, "stat")
	if err != nil {
		return cpuTimes{}, fmt.Errorf("read proc stat: %w", err)
	}
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}, errors.New("parse proc stat: aggregate CPU row is missing or incomplete")
	}

	values := make([]uint64, 0, len(fields)-1)
	var total uint64
	for _, field := range fields[1:] {
		value, parseErr := strconv.ParseUint(field, 10, 64)
		if parseErr != nil {
			return cpuTimes{}, fmt.Errorf("parse proc stat CPU value: %w", parseErr)
		}
		if ^uint64(0)-total < value {
			return cpuTimes{}, errors.New("parse proc stat: CPU total overflow")
		}
		total += value
		values = append(values, value)
	}

	idle := values[3]
	if len(values) > 4 {
		if ^uint64(0)-idle < values[4] {
			return cpuTimes{}, errors.New("parse proc stat: CPU idle total overflow")
		}
		idle += values[4]
	}
	return cpuTimes{total: total, idle: idle}, nil
}

func calculateCPUPercent(previous, current cpuTimes) (float64, error) {
	if current.total < previous.total || current.idle < previous.idle {
		return 0, errors.New("cpu counters moved backwards")
	}
	totalDelta := current.total - previous.total
	if totalDelta == 0 {
		return 0, errors.New("cpu total did not advance")
	}
	idleDelta := current.idle - previous.idle
	if idleDelta > totalDelta {
		return 0, errors.New("cpu idle delta exceeds total delta")
	}
	return float64(totalDelta-idleDelta) / float64(totalDelta) * 100, nil
}

func readMemoryPercent(procFS fs.FS) (float64, error) {
	file, err := procFS.Open("meminfo")
	if err != nil {
		return 0, fmt.Errorf("read proc meminfo: %w", err)
	}
	percent, readErr := scanMemoryPercent(file)
	closeErr := file.Close()
	if readErr != nil {
		return 0, readErr
	}
	if closeErr != nil {
		return 0, fmt.Errorf("close proc meminfo: %w", closeErr)
	}
	return percent, nil
}

func scanMemoryPercent(file fs.File) (float64, error) {
	var snapshot memorySnapshot
	scanner := boundedScanner(file)
	for scanner.Scan() {
		name, value, relevant, err := parseMemoryLine(scanner.Text())
		if err != nil {
			return 0, err
		}
		if !relevant {
			continue
		}
		snapshot.record(name, value)
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan proc meminfo: %w", err)
	}
	return snapshot.percent()
}

func parseMemoryLine(line string) (string, uint64, bool, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", 0, false, nil
	}
	name := strings.TrimSuffix(fields[0], ":")
	if name != "MemTotal" && name != "MemAvailable" {
		return "", 0, false, nil
	}
	if len(fields) != 3 || fields[2] != "kB" {
		return "", 0, false, fmt.Errorf("parse proc meminfo %s: expected kB value", name)
	}
	kilobytes, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return "", 0, false, fmt.Errorf("parse proc meminfo %s: %w", name, err)
	}
	if kilobytes > ^uint64(0)/1024 {
		return "", 0, false, fmt.Errorf("parse proc meminfo %s: memory value overflow", name)
	}
	return name, kilobytes * 1024, true, nil
}

func (snapshot *memorySnapshot) record(name string, value uint64) {
	if name == "MemTotal" {
		snapshot.totalBytes = value
		snapshot.foundTotal = true
		return
	}
	snapshot.availableBytes = value
	snapshot.foundAvailable = true
}

func (snapshot memorySnapshot) percent() (float64, error) {
	if !snapshot.foundTotal || snapshot.totalBytes == 0 {
		return 0, errors.New("parse proc meminfo: MemTotal is missing or zero")
	}
	if !snapshot.foundAvailable {
		return 0, errors.New("parse proc meminfo: MemAvailable is missing")
	}
	if snapshot.availableBytes > snapshot.totalBytes {
		return 0, errors.New("parse proc meminfo: MemAvailable exceeds MemTotal")
	}
	return float64(snapshot.totalBytes-snapshot.availableBytes) / float64(snapshot.totalBytes) * 100, nil
}

func readLoad1M(procFS fs.FS) (float64, error) {
	line, err := readFirstLine(procFS, "loadavg")
	if err != nil {
		return 0, fmt.Errorf("read proc loadavg: %w", err)
	}
	fields := strings.Fields(line)
	if len(fields) < 1 {
		return 0, errors.New("parse load average: value is missing")
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse load average: %w", err)
	}
	if math.IsNaN(load) || math.IsInf(load, 0) {
		return 0, errors.New("load average must be finite")
	}
	return load, nil
}

func readDiskPercent(path string, statFS statFSFunc) (float64, error) {
	if statFS == nil {
		return 0, errors.New("read filesystem usage: statfs function is required")
	}
	var stat syscall.Statfs_t
	if err := statFS(path, &stat); err != nil {
		return 0, fmt.Errorf("read filesystem usage: %w", err)
	}
	if stat.Blocks == 0 {
		return 0, errors.New("filesystem has zero blocks")
	}
	if stat.Bavail > stat.Blocks {
		return 0, errors.New("available filesystem blocks exceed total")
	}
	return float64(stat.Blocks-stat.Bavail) / float64(stat.Blocks) * 100, nil
}

func readFirstLine(filesystem fs.FS, name string) (string, error) {
	file, err := filesystem.Open(name)
	if err != nil {
		return "", err
	}

	scanner := boundedScanner(file)
	var line string
	var scanErr error
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			scanErr = fmt.Errorf("scan proc %s: %w", name, err)
		} else {
			scanErr = fmt.Errorf("proc %s is empty", name)
		}
	} else {
		line = scanner.Text()
	}
	closeErr := file.Close()
	if scanErr != nil {
		return "", scanErr
	}
	if closeErr != nil {
		return "", fmt.Errorf("close proc %s: %w", name, closeErr)
	}
	return line, nil
}

func boundedScanner(file fs.File) *bufio.Scanner {
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), maxHostLineBytes)
	return scanner
}
