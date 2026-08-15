package main

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

type serverOpsMetricsReader struct{}

func (serverOpsMetricsReader) Latest(context.Context, string, time.Time) ([]opsmetrics.Point, error) {
	return []opsmetrics.Point{}, nil
}

func (serverOpsMetricsReader) Series(context.Context, string, opsmetrics.MetricKey, time.Time, time.Time) ([]opsmetrics.Bucket, error) {
	return []opsmetrics.Bucket{}, nil
}

func TestDeferredAdminOpsMetricsReaderBindsExactlyOnce(t *testing.T) {
	reader := &deferredAdminOpsMetricsReader{}
	if _, err := reader.Latest(context.Background(), "cvn_aaaaaaaaaaaaaaaa", time.Time{}); err == nil {
		t.Fatal("unbound deferred reader returned no error")
	}
	if err := reader.Bind(serverOpsMetricsReader{}); err != nil {
		t.Fatalf("bind deferred reader: %v", err)
	}
	if _, err := reader.Latest(context.Background(), "cvn_aaaaaaaaaaaaaaaa", time.Time{}); err != nil {
		t.Fatalf("read through bound deferred reader: %v", err)
	}
	if err := reader.Bind(serverOpsMetricsReader{}); err == nil {
		t.Fatal("second deferred reader bind returned no error")
	}
}

func TestConfigureAdminOpsMetricsReaderActivatesOnlyWhenBothFeaturesEnabled(t *testing.T) {
	wantReader := serverOpsMetricsReader{}
	openCalls := 0
	disableCalls := 0
	closeCalls := 0
	cfg := &config.Config{
		AdminConsoleEnabled: true,
		DatabaseURL:         "postgres://redacted.example/concord",
		OpsMetrics: config.OpsMetricsConfig{
			Enabled: true,
		},
	}

	runtime, err := configureAdminOpsMetricsReader(
		context.Background(),
		cfg,
		nil,
		func(_ context.Context, _ *sql.DB, databaseURL string) (opsmetrics.Reader, func(context.Context) error, error) {
			openCalls++
			if databaseURL != cfg.DatabaseURL {
				t.Fatalf("database URL = %q, want configured URL", databaseURL)
			}
			return wantReader, func(context.Context) error {
				closeCalls++
				return nil
			}, nil
		},
		func(context.Context, *sql.DB) error {
			disableCalls++
			return nil
		},
	)
	if err != nil {
		t.Fatalf("configureAdminOpsMetricsReader returned error: %v", err)
	}
	if runtime.Reader() != wantReader {
		t.Fatalf("reader = %#v, want injected restricted reader", runtime.Reader())
	}
	if openCalls != 1 || disableCalls != 0 {
		t.Fatalf("open calls = %d, disable calls = %d; want 1, 0", openCalls, disableCalls)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("stop reader runtime: %v", err)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("repeat stop reader runtime: %v", err)
	}
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
}

func TestAdminOpsMetricsReaderRuntimeRetriesFailedCleanup(t *testing.T) {
	wantErr := errors.New("disable login failed")
	closeCalls := 0
	runtime := &adminOpsMetricsReaderRuntime{
		reader: serverOpsMetricsReader{},
		closeFn: func(context.Context) error {
			closeCalls++
			if closeCalls == 1 {
				return wantErr
			}
			return nil
		},
	}

	if err := runtime.Stop(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("first stop error = %v, want %v", err, wantErr)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("retry stop reader runtime: %v", err)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("idempotent stop reader runtime: %v", err)
	}
	if closeCalls != 2 {
		t.Fatalf("close calls = %d, want one failure plus one successful retry", closeCalls)
	}
}

func TestAdminOpsMetricsReaderRuntimeNilAndUnavailableAreSafe(t *testing.T) {
	var nilRuntime *adminOpsMetricsReaderRuntime
	if nilRuntime.Reader() != nil {
		t.Fatal("nil runtime returned a reader")
	}
	if err := nilRuntime.Stop(context.Background()); err != nil {
		t.Fatalf("nil runtime stop: %v", err)
	}

	unavailable := &adminOpsMetricsReaderRuntime{}
	if err := unavailable.Stop(context.Background()); err != nil {
		t.Fatalf("unavailable runtime stop: %v", err)
	}
}

func TestConfigureAdminOpsMetricsReaderDisablesLoginWhenEitherFeatureIsOff(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		adminEnabled bool
		opsEnabled   bool
	}{
		{name: "admin disabled", adminEnabled: false, opsEnabled: true},
		{name: "metrics disabled", adminEnabled: true, opsEnabled: false},
		{name: "both disabled", adminEnabled: false, opsEnabled: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			openCalls := 0
			disableCalls := 0
			cfg := &config.Config{
				AdminConsoleEnabled: testCase.adminEnabled,
				OpsMetrics: config.OpsMetricsConfig{
					Enabled: testCase.opsEnabled,
				},
			}

			runtime, err := configureAdminOpsMetricsReader(
				context.Background(),
				cfg,
				nil,
				func(context.Context, *sql.DB, string) (opsmetrics.Reader, func(context.Context) error, error) {
					openCalls++
					return serverOpsMetricsReader{}, func(context.Context) error { return nil }, nil
				},
				func(context.Context, *sql.DB) error {
					disableCalls++
					return nil
				},
			)
			if err != nil {
				t.Fatalf("configureAdminOpsMetricsReader returned error: %v", err)
			}
			if runtime.Reader() != nil {
				t.Fatalf("reader = %#v, want unavailable nil reader", runtime.Reader())
			}
			if openCalls != 0 || disableCalls != 1 {
				t.Fatalf("open calls = %d, disable calls = %d; want 0, 1", openCalls, disableCalls)
			}
		})
	}
}

func TestConfigureAdminOpsMetricsReaderFailsClosed(t *testing.T) {
	wantOpenErr := errors.New("restricted open failed")
	cfg := &config.Config{
		AdminConsoleEnabled: true,
		OpsMetrics:          config.OpsMetricsConfig{Enabled: true},
	}

	runtime, err := configureAdminOpsMetricsReader(
		context.Background(),
		cfg,
		nil,
		func(context.Context, *sql.DB, string) (opsmetrics.Reader, func(context.Context) error, error) {
			return nil, nil, wantOpenErr
		},
		func(context.Context, *sql.DB) error {
			t.Fatal("disable must not replace an active-mode open failure")
			return nil
		},
	)
	if runtime != nil {
		t.Fatalf("runtime = %#v, want nil", runtime)
	}
	if !errors.Is(err, wantOpenErr) {
		t.Fatalf("error = %v, want wrapped %v", err, wantOpenErr)
	}
}

func TestConfigureAdminOpsMetricsReaderPropagatesDisableFailure(t *testing.T) {
	wantErr := errors.New("cannot revoke login")
	runtime, err := configureAdminOpsMetricsReader(
		context.Background(),
		&config.Config{},
		nil,
		func(context.Context, *sql.DB, string) (opsmetrics.Reader, func(context.Context) error, error) {
			t.Fatal("disabled configuration must not open a reader")
			return nil, nil, nil
		},
		func(context.Context, *sql.DB) error { return wantErr },
	)
	if runtime != nil {
		t.Fatalf("runtime = %#v, want nil", runtime)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapped %v", err, wantErr)
	}
}

func TestConfigureAdminOpsMetricsReaderRejectsIncompleteOpenResult(t *testing.T) {
	cfg := &config.Config{
		AdminConsoleEnabled: true,
		OpsMetrics:          config.OpsMetricsConfig{Enabled: true},
	}
	closeCalls := 0
	runtime, err := configureAdminOpsMetricsReader(
		context.Background(),
		cfg,
		nil,
		func(context.Context, *sql.DB, string) (opsmetrics.Reader, func(context.Context) error, error) {
			return nil, func(context.Context) error {
				closeCalls++
				return nil
			}, nil
		},
		func(context.Context, *sql.DB) error { return nil },
	)
	if runtime != nil || err == nil {
		t.Fatalf("runtime = %#v, error = %v; want nil runtime and error", runtime, err)
	}
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
}

func TestOpenAdminOpsMetricsReaderIntegration(t *testing.T) {
	databaseURL := os.Getenv("OPS_METRICS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OPS_METRICS_TEST_DATABASE_URL is unset")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	if !strings.Contains(strings.TrimPrefix(parsed.Path, "/"), "opsmetrics") {
		t.Fatalf("test database %q is not isolated for operations metrics", parsed.Path)
	}

	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping test database: %v", err)
	}

	if _, err := db.Exec(serverOpsMetricsMigration(t, "000086_ops_metrics.down.sql")); err != nil {
		t.Fatalf("reset operations metrics schema: %v", err)
	}
	if _, err := db.Exec(serverOpsMetricsMigration(t, "000086_ops_metrics.up.sql")); err != nil {
		t.Fatalf("create operations metrics schema: %v", err)
	}
	for _, statement := range []struct {
		name  string
		query string
	}{
		{name: "users", query: `CREATE TABLE IF NOT EXISTS users (test_id INTEGER)`},
		{name: "messages", query: `CREATE TABLE IF NOT EXISTS messages (test_id INTEGER)`},
		{name: "user_keys", query: `CREATE TABLE IF NOT EXISTS user_keys (test_id INTEGER)`},
		{name: "admin_users", query: `CREATE TABLE IF NOT EXISTS admin_users (test_id INTEGER)`},
	} {
		if _, err := db.Exec(statement.query); err != nil {
			t.Fatalf("create restricted-reader test table %s: %v", statement.name, err)
		}
	}
	if _, err := db.Exec(serverOpsMetricsMigration(t, "000088_ops_metrics_reader.up.sql")); err != nil {
		t.Fatalf("create operations metrics reader role: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := database.DisableOpsMetricsReaderLogin(context.Background(), db); cleanupErr != nil {
			t.Errorf("disable operations metrics reader login: %v", cleanupErr)
		}
		if _, cleanupErr := db.Exec(serverOpsMetricsMigration(t, "000088_ops_metrics_reader.down.sql")); cleanupErr != nil {
			t.Errorf("drop operations metrics reader role: %v", cleanupErr)
		}
		if _, cleanupErr := db.Exec(serverOpsMetricsMigration(t, "000086_ops_metrics.down.sql")); cleanupErr != nil {
			t.Errorf("drop operations metrics schema: %v", cleanupErr)
		}
	})

	reader, closeFn, err := openAdminOpsMetricsReader(context.Background(), db, databaseURL)
	if err != nil {
		t.Fatalf("open restricted reader: %v", err)
	}
	if reader == nil || closeFn == nil {
		t.Fatal("open restricted reader returned incomplete runtime")
	}
	points, err := reader.Latest(context.Background(), "cvn_aaaaaaaaaaaaaaaa", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("read through restricted reader: %v", err)
	}
	if len(points) != 0 {
		t.Fatalf("points = %#v, want empty metrics database", points)
	}
	if err := closeFn(context.Background()); err != nil {
		t.Fatalf("close restricted reader: %v", err)
	}
}

func serverOpsMetricsMigration(t *testing.T, name string) string {
	t.Helper()
	_, filename, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("resolve server test path")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "migrations", name)
	contents, err := os.ReadFile(path) // #nosec G304 -- fixed test-owned migration name.
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	return string(contents)
}

func TestShutdownControlPlaneWaitsForHTTPDrain(t *testing.T) {
	httpStarted := make(chan struct{})
	releaseHTTP := make(chan struct{})
	events := make(chan string, 7)
	result := make(chan error, 1)

	go func() {
		result <- shutdownControlPlane(
			func() { events <- "cancel" },
			func() error {
				close(httpStarted)
				<-releaseHTTP
				events <- "http"
				return nil
			},
			func() { events <- "activity" },
			func() { events <- "presence" },
			func() { events <- "hub" },
			func() error { events <- "metrics"; return nil },
			func() error { events <- "admin_reader"; return nil },
			func() { events <- "nats" },
		)
	}()

	<-httpStarted
	if event := <-events; event != "cancel" {
		t.Fatalf("first shutdown event = %s, want cancel", event)
	}
	select {
	case event := <-events:
		t.Fatalf("dependency closed before HTTP drain completed: %s", event)
	default:
	}

	close(releaseHTTP)
	if err := <-result; err != nil {
		t.Fatalf("shutdownControlPlane returned error: %v", err)
	}

	got := []string{<-events, <-events, <-events, <-events, <-events, <-events, <-events}
	// presence BEFORE hub: both presence dispatch workers fail-closed-abandon
	// through the hub during their drain, so a hub closed first would silently
	// discard exactly the disconnects that drain exists to deliver (#2738).
	want := []string{"http", "activity", "presence", "hub", "metrics", "admin_reader", "nats"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
}

func TestShutdownControlPlaneCleansUpAfterHTTPError(t *testing.T) {
	wantErr := errors.New("HTTP drain failed")
	metricsErr := errors.New("metrics shutdown failed")
	wantReaderErr := errors.New("admin metrics reader cleanup failed")
	events := make([]string, 0, 8)

	gotErr := shutdownControlPlane(
		func() { events = append(events, "cancel") },
		func() error {
			events = append(events, "http")
			return wantErr
		},
		func() { events = append(events, "activity") },
		func() { events = append(events, "presence") },
		func() { events = append(events, "hub") },
		func() error { events = append(events, "metrics"); return metricsErr },
		func() error { events = append(events, "admin_reader"); return wantReaderErr },
		func() { events = append(events, "nats") },
	)

	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("shutdownControlPlane error = %v, want %v", gotErr, wantErr)
	}
	if !errors.Is(gotErr, metricsErr) {
		t.Fatalf("shutdownControlPlane error = %v, want joined error %v", gotErr, metricsErr)
	}
	if !errors.Is(gotErr, wantReaderErr) {
		t.Fatalf("shutdownControlPlane error = %v, want joined error %v", gotErr, wantReaderErr)
	}
	// The presence drain must still run when the HTTP drain errored — a failed
	// drain is exactly when stale presence is most likely to be left behind.
	wantEvents := []string{
		"cancel", "http", "activity", "presence", "hub", "metrics", "admin_reader", "nats",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("shutdown order = %v, want %v", events, wantEvents)
	}
}

func TestRunControlPlaneServerCleansReaderAfterListenerFailure(t *testing.T) {
	wantServeErr := errors.New("listener failed")
	wantCleanupErr := errors.New("reader cleanup failed")
	stop := make(chan os.Signal)
	closeCalls := 0
	runtime := &adminOpsMetricsReaderRuntime{
		reader: serverOpsMetricsReader{},
		closeFn: func(context.Context) error {
			closeCalls++
			return wantCleanupErr
		},
	}

	err := runControlPlaneServer(
		func() error { return wantServeErr },
		stop,
		func() error { return runtime.Stop(context.Background()) },
	)

	if !errors.Is(err, wantServeErr) {
		t.Fatalf("runControlPlaneServer error = %v, want listener error %v", err, wantServeErr)
	}
	if !errors.Is(err, wantCleanupErr) {
		t.Fatalf("runControlPlaneServer error = %v, want cleanup error %v", err, wantCleanupErr)
	}
	if closeCalls != 1 {
		t.Fatalf("reader cleanup calls = %d, want 1", closeCalls)
	}
}
