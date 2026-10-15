package voicecommand

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// StreamName is the frozen durable command stream.
const StreamName = "VOICE_COMMANDS_V1"

// ErrBrokerContinuity identifies retained evidence that cannot extend the SQL head.
var ErrBrokerContinuity = errors.New("voice command broker continuity lost")

// PublishReceipt is the broker's durable acknowledgement of one exact command.
type PublishReceipt struct {
	Stream    string
	Sequence  uint64
	Duplicate bool
}

// BrokerCommand is retained broker evidence with verified subject and message ID.
type BrokerCommand struct {
	Sequence uint64
	Data     []byte
}

// CommandBroker performs all broker work outside SQL transactions.
type CommandBroker interface {
	Verify(context.Context, TransportReceipt) error
	LastVoiceCommand(context.Context, TransportReceipt) (*BrokerCommand, error)
	PublishVoiceCommand(context.Context, TransportReceipt, VoiceCommand) (PublishReceipt, error)
}

// CommandRetirementBroker removes only an acknowledged prefix authorized by a
// durable SQL intent. Legacy brokers remain usable before the first retirement.
type CommandRetirementBroker interface {
	VoiceCommandAckFloor(context.Context, TransportReceipt) (time.Time, uint64, error)
	RetireVoiceCommandPrefix(context.Context, TransportReceipt, uint64, uint64) error
	LastVoiceCommandAfter(context.Context, TransportReceipt, uint64) (*BrokerCommand, error)
	PublishVoiceCommandAfter(context.Context, TransportReceipt, VoiceCommand, uint64) (PublishReceipt, error)
}

const (
	dispatchAttemptBudget = 10 * time.Second
	dispatchCleanupBudget = 2 * time.Second
	dispatchIdleDelay     = 250 * time.Millisecond
)

// Dispatcher reconciles one immutable SQL head against retained broker proof.
// Report admits only closed failure classes, never an error or command identity.
type Dispatcher struct {
	Repository *Repository
	Broker     CommandBroker
	Report     func(string)
}

// Step processes at most one head within a bounded attempt. Every broker call
// occurs after claim commit and before any retry/receipt transaction begins.
func (d *Dispatcher) Step(ctx context.Context) (bool, error) {
	if d == nil {
		return false, errors.New("voice command dispatcher unavailable")
	}
	if d.Repository == nil || d.Repository.DB == nil {
		d.report(failureReceiptUnavailable)
		return false, errors.New("voice command dispatcher repository unavailable")
	}
	if d.Broker == nil {
		d.report(failureBrokerUnavailable)
		return false, errors.New("voice command dispatcher broker unavailable")
	}
	attemptCtx, cancel := context.WithTimeout(ctx, dispatchAttemptBudget)
	defer cancel()
	retired, err := d.retireAcknowledged(attemptCtx)
	if err != nil || retired {
		return retired, err
	}
	token, err := uuid.NewRandom()
	if err != nil {
		d.report(failureReceiptUnavailable)
		return false, fmt.Errorf("create outbox claim token: %w", err)
	}
	claim, err := d.Repository.Claim(attemptCtx, token, claimLease)
	if err != nil {
		d.report(claimFailureClass(err))
		return false, err
	}
	if claim == nil {
		return false, nil
	}
	return d.dispatchClaim(attemptCtx, *claim)
}

func (d *Dispatcher) dispatchClaim(ctx context.Context, claim ClaimedCommand) (bool, error) {
	receipt, err := LoadReceipt(ctx, d.Repository.DB)
	if err != nil {
		return d.failClaim(ctx, claim, failureTransportUnavailable, err)
	}
	if claim.Command.StreamGeneration != receipt.Generation.String() {
		return d.failClaim(ctx, claim, failureBrokerContinuity, ErrBrokerContinuity)
	}
	if err := d.Broker.Verify(ctx, receipt); err != nil {
		return d.failClaim(ctx, claim, transportFailureClass(err), err)
	}
	last, err := d.lastCommand(ctx, receipt, claim.RetiredThrough)
	if err != nil {
		return d.failClaim(ctx, claim, brokerFailureClass(err), err)
	}
	if err := d.Broker.Verify(ctx, receipt); err != nil {
		return d.failClaim(ctx, claim, transportFailureClass(err), err)
	}
	alreadyStored, class, err := d.reconcileHead(ctx, claim, receipt, last)
	if err != nil {
		return d.failClaim(ctx, claim, class, err)
	}
	published, class, err := d.publishClaim(ctx, claim, receipt, alreadyStored)
	if err != nil {
		return d.failClaim(ctx, claim, class, err)
	}
	if err := d.Repository.MarkPublished(ctx, claim, published); err != nil {
		if benignClaimLoss(err) {
			return true, nil
		}
		class := failureReceiptUnavailable
		if errors.Is(err, ErrBrokerContinuity) {
			class = failureBrokerContinuity
		}
		return d.failClaim(ctx, claim, class, err)
	}
	return true, nil
}

func claimFailureClass(err error) string {
	if errors.Is(err, ErrInvalidCommand) {
		return failureInvalidCommand
	}
	if errors.Is(err, ErrBrokerContinuity) {
		return failureBrokerContinuity
	}
	return failureReceiptUnavailable
}

func (d *Dispatcher) publishClaim(ctx context.Context, claim ClaimedCommand, receipt TransportReceipt, alreadyStored bool) (PublishReceipt, string, error) {
	sequence, err := strconv.ParseUint(claim.Command.Sequence, 10, 64)
	if err != nil || sequence == 0 || sequence > math.MaxInt64 {
		return PublishReceipt{}, failureInvalidCommand, ErrInvalidCommand
	}
	published := PublishReceipt{Stream: StreamName, Sequence: sequence}
	if alreadyStored {
		return published, "", nil
	}
	// The broker receives its own complete copy, preserving the SQL identity
	// used for the receipt even if an adapter mutates a pointer while preparing JSON.
	command, err := validateStoredCommand(claim.Command)
	if err != nil {
		return PublishReceipt{}, failureInvalidCommand, err
	}
	published, err = d.publishCommand(ctx, receipt, command, claim.RetiredThrough)
	if err != nil {
		return PublishReceipt{}, brokerFailureClass(err), err
	}
	if published.Stream != StreamName || published.Sequence == 0 || published.Sequence > math.MaxInt64 || published.Sequence != sequence {
		return PublishReceipt{}, failureBrokerContinuity, ErrBrokerContinuity
	}
	if err := d.Broker.Verify(ctx, receipt); err != nil {
		return PublishReceipt{}, transportFailureClass(err), err
	}
	return published, "", nil
}

func (d *Dispatcher) reconcileHead(ctx context.Context, claim ClaimedCommand, receipt TransportReceipt, last *BrokerCommand) (bool, string, error) {
	sequence, err := strconv.ParseUint(claim.Command.Sequence, 10, 64)
	if err != nil || sequence == 0 || sequence > math.MaxInt64 {
		return false, failureInvalidCommand, ErrInvalidCommand
	}
	if last == nil {
		if sequence == 1 {
			return false, "", nil
		}
		return false, failureBrokerContinuity, ErrBrokerContinuity
	}
	if last.Sequence == 0 || last.Sequence > math.MaxInt64 {
		return false, failureBrokerContinuity, ErrBrokerContinuity
	}
	command, err := DecodeVoiceCommand(last.Data, receipt.Generation)
	if err != nil || command.Sequence != strconv.FormatUint(last.Sequence, 10) {
		return false, failureBrokerContinuity, errors.Join(ErrBrokerContinuity, err)
	}
	if last.Sequence == sequence && sameVoiceCommand(command, claim.Command) {
		return true, "", nil
	}
	if sequence == 1 || last.Sequence != sequence-1 {
		return false, failureBrokerContinuity, ErrBrokerContinuity
	}
	predecessor, err := d.Repository.Published(ctx, int64(sequence-1))
	if err != nil {
		class, cause := predecessorReadError(err)
		return false, class, cause
	}
	if !sameVoiceCommand(command, predecessor) {
		return false, failureBrokerContinuity, ErrBrokerContinuity
	}
	return false, "", nil
}

func predecessorReadError(err error) (string, error) {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrInvalidCommand) || errors.Is(err, ErrBrokerContinuity) {
		return failureBrokerContinuity, errors.Join(ErrBrokerContinuity, err)
	}
	return failureReceiptUnavailable, err
}

func transportFailureClass(err error) string {
	if errors.Is(err, ErrBrokerContinuity) {
		return failureBrokerContinuity
	}
	return failureTransportUnavailable
}

func brokerFailureClass(err error) string {
	if errors.Is(err, ErrBrokerContinuity) {
		return failureBrokerContinuity
	}
	return failureBrokerUnavailable
}

func (d *Dispatcher) failClaim(ctx context.Context, claim ClaimedCommand, class string, cause error) (bool, error) {
	// Cancellation leaves a finite lease even if SQL cleanup is unavailable. A
	// detached, bounded retry write can release it without extending shutdown.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dispatchCleanupBudget)
	defer cancel()
	retryErr := d.Repository.Retry(cleanupCtx, claim, class, retryDelay(claim.Attempts))
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

func retryDelay(attempts int) time.Duration {
	if attempts <= 1 {
		return time.Second
	}
	if attempts >= 6 {
		return maximumRetryDelay
	}
	return time.Second * time.Duration(1<<uint(attempts-1))
}

func benignClaimLoss(err error) bool {
	if err == ErrClaimLost {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !benignClaimLoss(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return benignClaimLoss(wrapped.Unwrap())
	}
	return false
}

func (d *Dispatcher) report(class string) {
	if d.Report != nil && validFailureClass(class) {
		d.Report(class)
	}
}

// Run drains immediately while work succeeds, waits briefly on an idle head,
// and caps database/dependency failure retries. Cancellation stops new claims.
func (d *Dispatcher) Run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		worked, err := d.Step(ctx)
		if ctx.Err() != nil {
			return
		}
		var delay time.Duration
		if err != nil {
			if failures < 6 {
				failures++
			}
			delay = retryDelay(failures)
		} else {
			failures = 0
			if worked {
				continue
			}
			delay = dispatchIdleDelay
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
