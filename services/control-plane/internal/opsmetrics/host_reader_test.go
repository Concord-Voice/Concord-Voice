package opsmetrics

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func validHostFS() fstest.MapFS {
	return fstest.MapFS{
		"stat":    {Data: []byte("cpu 100 0 50 50 0 0 0 0 0 0\n")},
		"meminfo": {Data: []byte("MemTotal: 1000 kB\nMemAvailable: 250 kB\n")},
		"loadavg": {Data: []byte("1.25 0.75 0.50 1/100 123\n")},
	}
}

func fixedStatFS(blocks, available uint64) statFSFunc {
	return func(_ string, stat *syscall.Statfs_t) error {
		stat.Blocks = blocks
		stat.Bavail = available
		return nil
	}
}

func TestHostReaderReturnsCataloguedHostMetrics(t *testing.T) {
	reader := newHostReader(validHostFS(), "/", fixedStatFS(100, 25))
	reader.previousCPU = &cpuTimes{total: 100, idle: 40}

	metrics, err := reader.Read(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[MetricKey]float64{
		MetricHostCPUPercent:    90,
		MetricHostMemoryPercent: 75,
		MetricHostDiskPercent:   75,
		MetricHostLoad1M:        1.25,
	}, metrics)
	for key, value := range metrics {
		require.NoError(t, ValidateSample(Sample{Key: key, Value: value, Source: SourceHost}))
	}
}

func TestHostReaderFirstSampleEstablishesCPUBaseline(t *testing.T) {
	reader := newHostReader(validHostFS(), "/", fixedStatFS(100, 25))

	metrics, err := reader.Read(context.Background())
	require.NoError(t, err)
	require.NotContains(t, metrics, MetricHostCPUPercent)
	require.Equal(t, 75.0, metrics[MetricHostMemoryPercent])
	require.NotNil(t, reader.previousCPU)
}

func TestNewHostReaderReadsConfiguredProcRoot(t *testing.T) {
	procRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(procRoot, "stat"), []byte("cpu 100 0 50 50\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(procRoot, "meminfo"), []byte("MemTotal: 1000 kB\nMemAvailable: 250 kB\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(procRoot, "loadavg"), []byte("1.25 0.75 0.50 1/100 123\n"), 0o600))

	metrics, err := NewHostReader(procRoot, procRoot).Read(context.Background())
	require.NoError(t, err)
	require.Equal(t, 75.0, metrics[MetricHostMemoryPercent])
}

func TestHostReaderRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name     string
		files    fstest.MapFS
		previous *cpuTimes
		statFS   statFSFunc
		wantErr  string
	}{
		{
			name: "missing proc stat",
			files: fstest.MapFS{
				"meminfo": {Data: []byte("MemTotal: 1000 kB\nMemAvailable: 250 kB\n")},
				"loadavg": {Data: []byte("1.25 0.75 0.50 1/100 123\n")},
			},
			statFS:  fixedStatFS(100, 25),
			wantErr: "read proc stat",
		},
		{
			name:    "malformed cpu total",
			files:   hostFSWith("stat", "cpu nope 0 0 0\n"),
			statFS:  fixedStatFS(100, 25),
			wantErr: "parse proc stat",
		},
		{
			name:    "cpu total overflow",
			files:   hostFSWith("stat", "cpu 18446744073709551615 1 0 0\n"),
			statFS:  fixedStatFS(100, 25),
			wantErr: "CPU total overflow",
		},
		{
			name:     "zero cpu delta",
			files:    validHostFS(),
			previous: &cpuTimes{total: 200, idle: 50},
			statFS:   fixedStatFS(100, 25),
			wantErr:  "cpu total did not advance",
		},
		{
			name:     "cpu counters moved backwards",
			files:    validHostFS(),
			previous: &cpuTimes{total: 300, idle: 75},
			statFS:   fixedStatFS(100, 25),
			wantErr:  "cpu counters moved backwards",
		},
		{
			name:    "missing memory total",
			files:   hostFSWith("meminfo", "MemAvailable: 250 kB\n"),
			statFS:  fixedStatFS(100, 25),
			wantErr: "MemTotal",
		},
		{
			name:    "memory unit overflow",
			files:   hostFSWith("meminfo", "MemTotal: 18014398509481984 kB\nMemAvailable: 1 kB\n"),
			statFS:  fixedStatFS(100, 25),
			wantErr: "memory value overflow",
		},
		{
			name:    "available memory exceeds total",
			files:   hostFSWith("meminfo", "MemTotal: 100 kB\nMemAvailable: 101 kB\n"),
			statFS:  fixedStatFS(100, 25),
			wantErr: "MemAvailable exceeds MemTotal",
		},
		{
			name:    "malformed load average",
			files:   hostFSWith("loadavg", "not-a-number 0.75 0.50 1/100 123\n"),
			statFS:  fixedStatFS(100, 25),
			wantErr: "parse load average",
		},
		{
			name:    "non-finite load average",
			files:   hostFSWith("loadavg", "NaN 0.75 0.50 1/100 123\n"),
			statFS:  fixedStatFS(100, 25),
			wantErr: "load average must be finite",
		},
		{
			name:    "statfs failure",
			files:   validHostFS(),
			statFS:  func(string, *syscall.Statfs_t) error { return errors.New("denied") },
			wantErr: "read filesystem usage",
		},
		{
			name:    "zero filesystem blocks",
			files:   validHostFS(),
			statFS:  fixedStatFS(0, 0),
			wantErr: "filesystem has zero blocks",
		},
		{
			name:    "filesystem available exceeds total",
			files:   validHostFS(),
			statFS:  fixedStatFS(100, 101),
			wantErr: "available filesystem blocks exceed total",
		},
		{
			name:    "scanner input exceeds bound",
			files:   hostFSWith("stat", "cpu "+strings.Repeat("1 ", maxHostLineBytes)+"\n"),
			statFS:  fixedStatFS(100, 25),
			wantErr: "scan proc stat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := newHostReader(tt.files, "/", tt.statFS)
			reader.previousCPU = tt.previous
			_, err := reader.Read(context.Background())
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestHostReaderHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := newHostReader(validHostFS(), "/", fixedStatFS(100, 25))

	_, err := reader.Read(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestHostReaderChecksProcFileCloseErrors(t *testing.T) {
	for _, name := range []string{"stat", "meminfo", "loadavg"} {
		t.Run(name, func(t *testing.T) {
			reader := newHostReader(closeErrorFS{FS: validHostFS(), failName: name}, "/", fixedStatFS(100, 25))

			_, err := reader.Read(context.Background())
			require.ErrorContains(t, err, "close proc "+name)
		})
	}
}

func TestHostReaderUsesConfiguredFilesystemPath(t *testing.T) {
	var gotPath string
	reader := newHostReader(validHostFS(), "/host-disk", func(path string, stat *syscall.Statfs_t) error {
		gotPath = path
		stat.Blocks = 1
		stat.Bavail = 0
		return nil
	})

	_, err := reader.Read(context.Background())
	require.NoError(t, err)
	require.Equal(t, "/host-disk", gotPath)
}

func hostFSWith(name, contents string) fstest.MapFS {
	files := validHostFS()
	files[name] = &fstest.MapFile{Data: []byte(contents)}
	return files
}

func TestMemoryPercentAvoidsIntegerTruncation(t *testing.T) {
	files := hostFSWith("meminfo", "MemTotal: 3 kB\nMemAvailable: 2 kB\n")
	reader := newHostReader(files, "/", fixedStatFS(100, 25))

	metrics, err := reader.Read(context.Background())
	require.NoError(t, err)
	require.InDelta(t, 100.0/3.0, metrics[MetricHostMemoryPercent], 1e-9)
	require.False(t, math.IsNaN(metrics[MetricHostMemoryPercent]))
}

type closeErrorFS struct {
	fs.FS
	failName string
}

func (f closeErrorFS) Open(name string) (fs.File, error) {
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return &closeErrorFile{File: file, fail: name == f.failName}, nil
}

type closeErrorFile struct {
	fs.File
	fail bool
}

func (f *closeErrorFile) Close() error {
	if err := f.File.Close(); err != nil {
		return err
	}
	if f.fail {
		return errors.New("close failed")
	}
	return nil
}
