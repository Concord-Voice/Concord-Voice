package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

const (
	voiceCommandOutboxConnectBudget = 2 * time.Second
	voiceCommandOutboxAttemptBudget = 10 * time.Second
	voiceCommandOutboxIdleInterval  = 250 * time.Millisecond
	voiceCommandOutboxRetryLimit    = 30 * time.Second
)

type voiceCommandOutboxBroker interface {
	voicecommand.CommandBroker
	Close() error
}

type voiceCommandOutboxWorkerDependencies struct {
	Connect func(context.Context) (voiceCommandOutboxBroker, error)
	Step    func(context.Context, voicecommand.CommandBroker) (bool, error)
	Wait    func(context.Context, time.Duration) bool
	Report  func(string)
}

// startVoiceCommandOutboxWorker owns a separate cancellable connection. Claims
// and receipts stay outside every broker wait in the dispatcher.
func startVoiceCommandOutboxWorker(ctx context.Context, db *sql.DB, url string, log *logger.Logger) func() {
	repository := voicecommand.NewRepository(db)
	report := func(class string) {
		log.Warn("Durable voice command publication delayed", "failure_class", class)
	}
	dependencies := voiceCommandOutboxWorkerDependencies{
		Connect: func(attemptCtx context.Context) (voiceCommandOutboxBroker, error) {
			client, err := natsclient.NewVoiceCommandClient(attemptCtx, url)
			if err != nil {
				return nil, err
			}
			return client, nil
		},
		Step: func(attemptCtx context.Context, broker voicecommand.CommandBroker) (bool, error) {
			dispatcher := voicecommand.Dispatcher{Repository: repository, Broker: broker, Report: report}
			return dispatcher.Step(attemptCtx)
		},
		Report: report,
	}
	return startActivityHistoryWorkers(ctx, func(workerCtx context.Context) {
		runVoiceCommandOutboxWorker(workerCtx, dependencies)
	})
}

// runVoiceCommandOutboxWorker closes its connection only after the in-flight
// step returns, so cancellation retains unfinished commands for another claim.
func runVoiceCommandOutboxWorker(ctx context.Context, dependencies voiceCommandOutboxWorkerDependencies) {
	if dependencies.Connect == nil || dependencies.Step == nil {
		reportVoiceCommandOutbox(dependencies, "voice_command_outbox_unwired")
		return
	}
	if dependencies.Wait == nil {
		dependencies.Wait = waitVoiceCommandTransport
	}
	var broker voiceCommandOutboxBroker
	defer func() { closeVoiceCommandOutboxBroker(broker, dependencies) }()
	delay := time.Second
	for ctx.Err() == nil {
		var worked bool
		var class string
		var err error
		broker, worked, class, err = voiceCommandOutboxAttempt(ctx, dependencies, broker)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			reportVoiceCommandOutbox(dependencies, class)
			closeVoiceCommandOutboxBroker(broker, dependencies)
			broker = nil
			if !dependencies.Wait(ctx, delay) {
				return
			}
			delay = min(delay*2, voiceCommandOutboxRetryLimit)
			continue
		}
		delay = time.Second
		if !worked && !dependencies.Wait(ctx, voiceCommandOutboxIdleInterval) {
			return
		}
	}
}

func voiceCommandOutboxAttempt(ctx context.Context, dependencies voiceCommandOutboxWorkerDependencies, broker voiceCommandOutboxBroker) (voiceCommandOutboxBroker, bool, string, error) {
	if broker == nil {
		connectCtx, cancel := context.WithTimeout(ctx, voiceCommandOutboxConnectBudget)
		connected, err := dependencies.Connect(connectCtx)
		connectErr := connectCtx.Err()
		cancel()
		broker = connected
		if err == nil && broker == nil {
			err = errors.New("voice command outbox factory returned no broker")
		}
		if err != nil || connectErr != nil {
			return broker, false, "voice_command_outbox_connect_failed", errors.Join(err, connectErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return broker, false, "voice_command_outbox_step_failed", err
	}
	attemptCtx, cancel := context.WithTimeout(ctx, voiceCommandOutboxAttemptBudget)
	defer cancel()
	worked, err := dependencies.Step(attemptCtx, broker)
	return broker, worked, "voice_command_outbox_step_failed", errors.Join(err, attemptCtx.Err())
}

func closeVoiceCommandOutboxBroker(broker voiceCommandOutboxBroker, dependencies voiceCommandOutboxWorkerDependencies) {
	if broker != nil {
		if err := broker.Close(); err != nil {
			reportVoiceCommandOutbox(dependencies, "voice_command_outbox_close_failed")
		}
	}
}

func reportVoiceCommandOutbox(dependencies voiceCommandOutboxWorkerDependencies, class string) {
	if dependencies.Report != nil {
		dependencies.Report(class)
	}
}
