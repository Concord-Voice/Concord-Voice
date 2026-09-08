package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/opsmetrics"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

type accountActivityFlusher interface {
	FlushQualifications(context.Context, time.Time) error
}

// OpsMetricsRuntime owns the collector goroutine and its account activity flush.
type OpsMetricsRuntime struct {
	cancel   context.CancelFunc
	done     <-chan struct{}
	receiver *opsmetrics.Receiver
	accounts accountActivityFlusher
	now      func() time.Time
}

func newOpsMetricsReceiverWithSecurityEvents(subscriber opsmetrics.Subscriber, nodeID string, secret []byte, counters *opsmetrics.Counters, log *logger.Logger, events securityevent.Emitter) *opsmetrics.Receiver {
	receiver := opsmetrics.NewReceiver(subscriber, nodeID, secret, counters, log, nil)
	receiver.SetSecurityEvents(events)
	return receiver
}

func wireOpsMetricsRuntime(
	db *sql.DB,
	natsClient *natsclient.Client,
	hub *websocket.Hub,
	counters *opsmetrics.Counters,
	cfg config.OpsMetricsConfig,
	log *logger.Logger,
) *OpsMetricsRuntime {
	return wireOpsMetricsRuntimeWithSecurityEvents(db, natsClient, hub, counters, cfg, log, securityevent.Discard)
}

func wireOpsMetricsRuntimeWithSecurityEvents(db *sql.DB, natsClient *natsclient.Client, hub *websocket.Hub, counters *opsmetrics.Counters, cfg config.OpsMetricsConfig, log *logger.Logger, events securityevent.Emitter) *OpsMetricsRuntime {
	runtime, err := startOpsMetricsRuntimeWithSecurityEvents(db, natsClient, hub, counters, cfg, log, events)
	if err != nil {
		log.Error("Operations metrics runtime disabled", "reason", "startup_failed")
		return nil
	}
	return runtime
}

func startOpsMetricsRuntimeWithSecurityEvents(db *sql.DB, natsClient *natsclient.Client, hub *websocket.Hub, counters *opsmetrics.Counters, cfg config.OpsMetricsConfig, log *logger.Logger, events securityevent.Emitter) (*OpsMetricsRuntime, error) {
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
	receiver := newOpsMetricsReceiverWithSecurityEvents(
		natsClient,
		cfg.NodeID,
		[]byte(cfg.SharedSecret),
		counters,
		log,
		events,
	)
	if err := receiver.Subscribe(); err != nil {
		return nil, fmt.Errorf("subscribe to operations metrics snapshots: %w", err)
	}
	if err := natsClient.Flush(); err != nil {
		receiver.MarkDependencyDegraded()
		activationErr := fmt.Errorf("activate operations metrics subscriptions: %w", err)
		if unsubscribeErr := receiver.Unsubscribe(); unsubscribeErr != nil {
			return nil, errors.Join(activationErr, fmt.Errorf("unsubscribe operations metrics subscriptions: %w", unsubscribeErr))
		}
		return nil, activationErr
	}

	tracker := opsmetrics.NewActivityTracker()
	hub.SetActivityObserver(tracker)
	accounts := opsmetrics.NewAccountProvider(db, tracker)
	collector := opsmetrics.NewCollector(store, receiver, counters, hub, cfg.Interval, log, accounts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		collector.Run(ctx)
	}()
	return &OpsMetricsRuntime{
		cancel:   cancel,
		done:     done,
		receiver: receiver,
		accounts: accounts,
		now:      time.Now,
	}, nil
}

// Stop cancels collection, waits for in-flight work, and flushes activity that
// crossed the qualification threshold before graceful Hub shutdown completed.
func (runtime *OpsMetricsRuntime) Stop(ctx context.Context) error {
	if runtime == nil {
		return nil
	}
	runtime.cancel()
	unsubscribeErr := runtime.receiver.Unsubscribe()
	select {
	case <-runtime.done:
		var flushErr error
		if runtime.accounts != nil {
			now := time.Now
			if runtime.now != nil {
				now = runtime.now
			}
			flushErr = runtime.accounts.FlushQualifications(ctx, now().UTC())
		}
		return errors.Join(unsubscribeErr, flushErr)
	case <-ctx.Done():
		return errors.Join(unsubscribeErr, fmt.Errorf("stop operations metrics collector: %w", ctx.Err()))
	}
}
