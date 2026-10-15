package voicecommand

import (
	"context"
	"errors"
	"math"
	"strconv"
)

// A prepared interval blocks new claims under the shared SQL cursor lock. Its
// bounded broker operation is safe to resume after either acknowledgement is
// lost; no SQL lock remains held while any broker method runs.
func (d *Dispatcher) retireAcknowledged(ctx context.Context) (bool, error) {
	state, err := d.Repository.ReadRetirement(ctx)
	if err != nil {
		d.report(claimFailureClass(err))
		return false, err
	}
	broker, capable := d.Broker.(CommandRetirementBroker)
	if !capable {
		if state.RetiredThrough != 0 || state.Pending != nil {
			d.report(failureBrokerContinuity)
			return false, ErrBrokerContinuity
		}
		return false, nil
	}
	if state.Pending != nil {
		return d.resumeRetirement(ctx, broker, *state.Pending)
	}
	if state.Published <= 1 {
		return false, nil
	}
	receipt, err := LoadReceipt(ctx, d.Repository.DB)
	if err != nil {
		d.report(failureTransportUnavailable)
		return false, err
	}
	intent, err := d.prepareAcknowledged(ctx, broker, receipt, state)
	if err != nil || intent == nil {
		return false, err
	}
	return d.resumeRetirement(ctx, broker, *intent)
}

func (d *Dispatcher) retirementPredecessor(ctx context.Context, receipt TransportReceipt, published uint64, last *BrokerCommand) (bool, error) {
	if last == nil || published > math.MaxInt64 || last.Sequence < published || last.Sequence > published+1 {
		d.report(failureBrokerContinuity)
		return false, ErrBrokerContinuity
	}
	// An appended command with an uncertain SQL receipt must use the existing
	// exact-head reconciliation before it can supply a retirement predecessor.
	if last.Sequence == published+1 {
		return false, nil
	}
	command, err := DecodeVoiceCommand(last.Data, receipt.Generation)
	if err != nil || command.Sequence != strconv.FormatUint(last.Sequence, 10) {
		d.report(failureBrokerContinuity)
		return false, errors.Join(ErrBrokerContinuity, err)
	}
	predecessor, err := d.Repository.Published(ctx, int64(published))
	if err != nil {
		class, cause := predecessorReadError(err)
		d.report(class)
		return false, cause
	}
	if !sameVoiceCommand(command, predecessor) {
		d.report(failureBrokerContinuity)
		return false, ErrBrokerContinuity
	}
	return true, nil
}

func (d *Dispatcher) prepareAcknowledged(ctx context.Context, broker CommandRetirementBroker, receipt TransportReceipt, state RetirementState) (*RetirementIntent, error) {
	created, ack, err := broker.VoiceCommandAckFloor(ctx, receipt)
	if err != nil {
		d.report(brokerFailureClass(err))
		if errors.Is(err, ErrBrokerContinuity) {
			return nil, err
		}
		// An absent consumer or uncertain acknowledgement permits no deletion,
		// but ordinary durable publication can continue while capacity remains.
		return nil, nil
	}
	if created.IsZero() || ack > math.MaxInt64 {
		d.report(failureBrokerContinuity)
		return nil, ErrBrokerContinuity
	}
	if ack <= state.RetiredThrough {
		return nil, nil
	}
	ready, err := d.acknowledgedPredecessor(ctx, broker, receipt, state, ack)
	if err != nil || !ready {
		return nil, err
	}
	intent, err := d.Repository.PrepareRetirement(ctx, receipt, created, ack)
	if err != nil {
		d.report(claimFailureClass(err))
	}
	return intent, err
}

func (d *Dispatcher) acknowledgedPredecessor(ctx context.Context, broker CommandRetirementBroker, receipt TransportReceipt, state RetirementState, ack uint64) (bool, error) {
	// Read the suffix after acknowledgement proof so ordinary concurrent
	// appends cannot make a valid acknowledgement appear ahead of its snapshot.
	last, err := broker.LastVoiceCommandAfter(ctx, receipt, state.RetiredThrough)
	if err != nil {
		d.report(brokerFailureClass(err))
		return false, err
	}
	if last == nil || ack > last.Sequence {
		d.report(failureBrokerContinuity)
		return false, ErrBrokerContinuity
	}
	return d.retirementPredecessor(ctx, receipt, state.Published, last)
}

func (d *Dispatcher) resumeRetirement(ctx context.Context, broker CommandRetirementBroker, intent RetirementIntent) (bool, error) {
	if !intent.Due {
		return false, nil
	}
	if err := broker.RetireVoiceCommandPrefix(ctx, intent.Receipt, intent.From, intent.Through); err != nil {
		return d.failRetirement(ctx, intent, brokerFailureClass(err), err)
	}
	if err := d.Repository.CompleteRetirement(ctx, intent); err != nil {
		if benignClaimLoss(err) {
			return true, nil
		}
		return d.failRetirement(ctx, intent, claimFailureClass(err), err)
	}
	return true, nil
}

func (d *Dispatcher) failRetirement(ctx context.Context, intent RetirementIntent, class string, cause error) (bool, error) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dispatchCleanupBudget)
	defer cancel()
	retryErr := d.Repository.RetryRetirement(cleanupCtx, intent, class, retryDelay(intent.Attempts+1))
	if benignClaimLoss(retryErr) {
		return true, nil
	}
	d.report(class)
	if retryErr != nil {
		d.report(failureReceiptUnavailable)
		return true, errors.Join(cause, retryErr)
	}
	return true, cause
}

func (d *Dispatcher) lastCommand(ctx context.Context, receipt TransportReceipt, floor uint64) (*BrokerCommand, error) {
	if broker, ok := d.Broker.(CommandRetirementBroker); ok {
		return broker.LastVoiceCommandAfter(ctx, receipt, floor)
	}
	if floor != 0 {
		return nil, ErrBrokerContinuity
	}
	return d.Broker.LastVoiceCommand(ctx, receipt)
}

func (d *Dispatcher) publishCommand(ctx context.Context, receipt TransportReceipt, command VoiceCommand, floor uint64) (PublishReceipt, error) {
	if broker, ok := d.Broker.(CommandRetirementBroker); ok {
		return broker.PublishVoiceCommandAfter(ctx, receipt, command, floor)
	}
	if floor != 0 {
		return PublishReceipt{}, ErrBrokerContinuity
	}
	return d.Broker.PublishVoiceCommand(ctx, receipt, command)
}
