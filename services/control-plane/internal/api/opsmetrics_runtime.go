package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/opsmetrics"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
)

// OpsMetricsRuntime owns the collector goroutine started by NewRouter.
type OpsMetricsRuntime struct {
	cancel   context.CancelFunc
	done     <-chan struct{}
	receiver *opsmetrics.Receiver
}

func wireOpsMetricsRuntime(
	db *sql.DB,
	natsClient *natsclient.Client,
	hub *websocket.Hub,
	counters *opsmetrics.Counters,
	cfg config.OpsMetricsConfig,
	log *logger.Logger,
) *OpsMetricsRuntime {
	runtime, err := startOpsMetricsRuntime(db, natsClient, hub, counters, cfg, log)
	if err != nil {
		log.Error("Operations metrics runtime disabled", "reason", "startup_failed")
		return nil
	}
	return runtime
}

func startOpsMetricsRuntime(
	db *sql.DB,
	natsClient *natsclient.Client,
	hub *websocket.Hub,
	counters *opsmetrics.Counters,
	cfg config.OpsMetricsConfig,
	log *logger.Logger,
) (*OpsMetricsRuntime, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if natsClient == nil {
		return nil, errors.New("operations metrics requires an active NATS connection")
	}

	store, err := opsmetrics.NewPostgresStore(db, cfg.NodeID)
	if err != nil {
		return nil, err
	}
	receiver := opsmetrics.NewReceiver(
		natsClient,
		cfg.NodeID,
		[]byte(cfg.SharedSecret),
		counters,
		log,
		nil,
	)
	if err := receiver.Subscribe(); err != nil {
		return nil, fmt.Errorf("subscribe to operations metrics snapshots: %w", err)
	}
	if err := natsClient.Flush(); err != nil {
		_ = receiver.Unsubscribe()
		return nil, fmt.Errorf("activate operations metrics subscriptions: %w", err)
	}

	collector := opsmetrics.NewCollector(store, receiver, counters, hub, cfg.Interval, log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		collector.Run(ctx)
	}()
	return &OpsMetricsRuntime{cancel: cancel, done: done, receiver: receiver}, nil
}

// Stop cancels collection and waits for any in-flight collection to finish.
func (runtime *OpsMetricsRuntime) Stop(ctx context.Context) error {
	if runtime == nil {
		return nil
	}
	runtime.cancel()
	unsubscribeErr := runtime.receiver.Unsubscribe()
	select {
	case <-runtime.done:
		return unsubscribeErr
	case <-ctx.Done():
		return errors.Join(unsubscribeErr, fmt.Errorf("stop operations metrics collector: %w", ctx.Err()))
	}
}
