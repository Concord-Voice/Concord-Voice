package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/database"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/opsmetrics"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
)

type openAdminOpsMetricsReaderFunc func(
	context.Context,
	*sql.DB,
	string,
) (opsmetrics.Reader, func(context.Context) error, error)

type ensureAdminOpsMetricsReaderDisabledFunc func(context.Context, *sql.DB) error

// deferredAdminOpsMetricsReader lets NewRouter build the closed admin route
// surface before the process-ephemeral database login exists. Main binds the
// restricted reader exactly once after every fatal-prone constructor returns
// and before the HTTP listener starts.
type deferredAdminOpsMetricsReader struct {
	mu     sync.RWMutex
	reader opsmetrics.Reader
}

func (reader *deferredAdminOpsMetricsReader) Bind(bound opsmetrics.Reader) error {
	if reader == nil || bound == nil {
		return errors.New("deferred admin operations metrics reader binding is incomplete")
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.reader != nil {
		return errors.New("deferred admin operations metrics reader is already bound")
	}
	reader.reader = bound
	return nil
}

func (reader *deferredAdminOpsMetricsReader) Latest(
	ctx context.Context,
	nodeID string,
	notBefore time.Time,
) ([]opsmetrics.Point, error) {
	bound, err := reader.boundReader()
	if err != nil {
		return nil, err
	}
	return bound.Latest(ctx, nodeID, notBefore)
}

func (reader *deferredAdminOpsMetricsReader) Series(
	ctx context.Context,
	nodeID string,
	key opsmetrics.MetricKey,
	start time.Time,
	end time.Time,
) ([]opsmetrics.Bucket, error) {
	bound, err := reader.boundReader()
	if err != nil {
		return nil, err
	}
	return bound.Series(ctx, nodeID, key, start, end)
}

func (reader *deferredAdminOpsMetricsReader) boundReader() (opsmetrics.Reader, error) {
	if reader == nil {
		return nil, errors.New("deferred admin operations metrics reader is unavailable")
	}
	reader.mu.RLock()
	defer reader.mu.RUnlock()
	if reader.reader == nil {
		return nil, errors.New("deferred admin operations metrics reader is unavailable")
	}
	return reader.reader, nil
}

type adminOpsMetricsReaderRuntime struct {
	reader  opsmetrics.Reader
	closeFn func(context.Context) error
	mu      sync.Mutex
	stopped bool
}

func (runtime *adminOpsMetricsReaderRuntime) Reader() opsmetrics.Reader {
	if runtime == nil {
		return nil
	}
	return runtime.reader
}

// Stop is idempotent after a successful cleanup. A failed cleanup remains
// retryable so the deferred fallback can use a fresh context.
func (runtime *adminOpsMetricsReaderRuntime) Stop(ctx context.Context) error {
	if runtime == nil || runtime.closeFn == nil {
		return nil
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.stopped {
		return nil
	}
	if err := runtime.closeFn(ctx); err != nil {
		return err
	}
	runtime.stopped = true
	return nil
}

func configureAdminOpsMetricsReader(
	ctx context.Context,
	cfg *config.Config,
	adminDB *sql.DB,
	openReader openAdminOpsMetricsReaderFunc,
	ensureReaderDisabled ensureAdminOpsMetricsReaderDisabledFunc,
) (*adminOpsMetricsReaderRuntime, error) {
	if cfg == nil || openReader == nil || ensureReaderDisabled == nil {
		return nil, errors.New("admin operations metrics reader configuration is incomplete")
	}
	if !cfg.AdminConsoleEnabled || !cfg.OpsMetrics.Enabled {
		if err := ensureReaderDisabled(ctx, adminDB); err != nil {
			return nil, fmt.Errorf("ensure admin operations metrics reader login is disabled: %w", err)
		}
		return &adminOpsMetricsReaderRuntime{stopped: true}, nil
	}

	reader, closeFn, err := openReader(ctx, adminDB, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open restricted admin operations metrics reader: %w", err)
	}
	if reader == nil || closeFn == nil {
		if closeFn != nil {
			err = closeFn(ctx)
		}
		return nil, errors.Join(errors.New("restricted admin operations metrics reader is incomplete"), err)
	}
	return &adminOpsMetricsReaderRuntime{reader: reader, closeFn: closeFn}, nil
}

func openAdminOpsMetricsReader(
	ctx context.Context,
	adminDB *sql.DB,
	databaseURL string,
) (opsmetrics.Reader, func(context.Context) error, error) {
	connection, err := database.OpenOpsMetricsReaderConnection(ctx, adminDB, databaseURL)
	if err != nil {
		return nil, nil, err
	}
	reader, err := opsmetrics.NewPostgresReader(connection.DB)
	if err != nil {
		return nil, nil, errors.Join(err, connection.Close(ctx))
	}
	return reader, connection.Close, nil
}
