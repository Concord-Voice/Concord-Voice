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

const voiceCommandTransportAttemptBudget = 10 * time.Second
const voiceCommandTransportVerifyInterval = 30 * time.Second

type voiceCommandTransportBroker interface {
	voicecommand.TransportProvisioner
	Close() error
}

type voiceCommandTransportWorkerDependencies struct {
	Connect   func(context.Context) (voiceCommandTransportBroker, error)
	Bootstrap func(context.Context, voicecommand.TransportProvisioner) error
	Verify    func(context.Context, voicecommand.TransportProvisioner) error
	Wait      func(context.Context, time.Duration) bool
	Report    func(string)
}

// startVoiceCommandTransportWorker owns its connection independently of the
// core router's initial NATS wiring. Broker outages never block HTTP startup.
func startVoiceCommandTransportWorker(ctx context.Context, db *sql.DB, url string, log *logger.Logger) func() {
	dependencies := voiceCommandTransportWorkerDependencies{
		Connect: func(attemptCtx context.Context) (voiceCommandTransportBroker, error) {
			client, err := natsclient.NewVoiceCommandClient(attemptCtx, url)
			if err != nil {
				return nil, err
			}
			return client, nil
		},
		Bootstrap: func(attemptCtx context.Context, provisioner voicecommand.TransportProvisioner) error {
			return voicecommand.BootstrapTransport(attemptCtx, db, provisioner)
		},
		Verify: func(attemptCtx context.Context, provisioner voicecommand.TransportProvisioner) error {
			receipt, err := voicecommand.LoadReceipt(attemptCtx, db)
			if err != nil {
				return err
			}
			return provisioner.Verify(attemptCtx, receipt)
		},
		Report: func(class string) {
			log.Warn("Durable voice command transport unavailable", "failure_class", class)
		},
	}
	return startActivityHistoryWorkers(ctx, func(workerCtx context.Context) {
		runVoiceCommandTransportWorker(workerCtx, dependencies)
	})
}

// runVoiceCommandTransportWorker never retires an enforcement obligation. It
// verifies continuity and releases initialization only through the SQL receipt.
func runVoiceCommandTransportWorker(ctx context.Context, dependencies voiceCommandTransportWorkerDependencies) {
	if dependencies.Connect == nil || dependencies.Bootstrap == nil || dependencies.Verify == nil {
		reportVoiceCommandTransport(dependencies, "voice_command_transport_unwired")
		return
	}
	if dependencies.Wait == nil {
		dependencies.Wait = waitVoiceCommandTransport
	}
	var broker voiceCommandTransportBroker
	defer func() { closeVoiceCommandTransportBroker(broker, dependencies) }()
	bootstrapped := false
	delay := time.Second
	for ctx.Err() == nil {
		var err error
		broker, err = voiceCommandTransportAttempt(ctx, dependencies, broker, bootstrapped)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			reportVoiceCommandTransport(dependencies, "voice_command_transport_retry")
			closeVoiceCommandTransportBroker(broker, dependencies)
			broker, bootstrapped = nil, false
			if !dependencies.Wait(ctx, delay) {
				return
			}
			delay = min(delay*2, voiceCommandTransportVerifyInterval)
			continue
		}
		bootstrapped, delay = true, time.Second
		if !dependencies.Wait(ctx, voiceCommandTransportVerifyInterval) {
			return
		}
	}
}

func voiceCommandTransportAttempt(ctx context.Context, dependencies voiceCommandTransportWorkerDependencies, broker voiceCommandTransportBroker, bootstrapped bool) (voiceCommandTransportBroker, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, voiceCommandTransportAttemptBudget)
	defer cancel()
	var err error
	if broker == nil {
		broker, err = dependencies.Connect(attemptCtx)
		if err == nil && broker == nil {
			err = errors.New("voice command transport factory returned no broker")
		}
	}
	if err == nil {
		err = voiceCommandTransportPass(attemptCtx, dependencies, broker, bootstrapped)
	}
	return broker, errors.Join(err, attemptCtx.Err())
}

func voiceCommandTransportPass(ctx context.Context, dependencies voiceCommandTransportWorkerDependencies, broker voiceCommandTransportBroker, bootstrapped bool) error {
	if bootstrapped {
		return dependencies.Verify(ctx, broker)
	}
	return dependencies.Bootstrap(ctx, broker)
}

func closeVoiceCommandTransportBroker(broker voiceCommandTransportBroker, dependencies voiceCommandTransportWorkerDependencies) {
	if broker == nil {
		return
	}
	if err := broker.Close(); err != nil && !errors.Is(err, natsclient.ErrDrainNothingToDo) {
		reportVoiceCommandTransport(dependencies, "voice_command_transport_close_failed")
	}
}

func reportVoiceCommandTransport(dependencies voiceCommandTransportWorkerDependencies, class string) {
	if dependencies.Report != nil {
		dependencies.Report(class)
	}
}

func waitVoiceCommandTransport(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
