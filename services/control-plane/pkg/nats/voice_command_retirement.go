package nats

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	natssdk "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const voiceCommandDurable = "voice-enforcement-v1"

var _ voicecommand.CommandRetirementBroker = (*Client)(nil)

// VoiceCommandAckFloor reads the exact existing pull consumer without changing
// its configuration or lifecycle. Stream sequence, not delivery count, bounds
// retirement; two reads preserve one stable consumer incarnation and profile.
func (c *Client) VoiceCommandAckFloor(ctx context.Context, receipt voicecommand.TransportReceipt) (time.Time, uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, voiceCommandAPIBudget)
	defer cancel()
	js, manager, err := c.verifiedPublicationManager(ctx, receipt)
	if err != nil {
		return time.Time{}, 0, err
	}
	consumer, err := manager.Consumer(ctx, VoiceCommandStream, voiceCommandDurable)
	if err != nil {
		if errors.Is(err, jetstream.ErrNotPullConsumer) {
			err = ErrVoiceCommandContinuity
		}
		return time.Time{}, 0, voiceCommandPublisherError(err)
	}
	before := consumer.CachedInfo()
	if err := validateVoiceCommandConsumer(before); err != nil {
		return time.Time{}, 0, voiceCommandPublisherError(err)
	}
	after, err := consumer.Info(ctx)
	if err != nil {
		return time.Time{}, 0, voiceCommandPublisherError(err)
	}
	stream, err := js.StreamInfo(VoiceCommandStream, natssdk.Context(ctx))
	if err != nil {
		return time.Time{}, 0, voiceCommandPublisherError(err)
	}
	if err := validateVoiceCommandAckProof(before, after, stream); err != nil {
		return time.Time{}, 0, voiceCommandPublisherError(err)
	}
	if err := c.Verify(ctx, receipt); err != nil {
		return time.Time{}, 0, voiceCommandPublisherError(err)
	}
	return before.Created.UTC(), before.AckFloor.Stream, nil
}

func validateVoiceCommandConsumer(info *jetstream.ConsumerInfo) error {
	if info == nil || info.Created.IsZero() || info.Stream != VoiceCommandStream ||
		info.Name != voiceCommandDurable || info.Paused || info.PauseRemaining != 0 {
		return ErrVoiceCommandContinuity
	}
	c := info.Config
	if c.Durable != voiceCommandDurable || (c.Name != "" && c.Name != voiceCommandDurable) ||
		c.AckPolicy != jetstream.AckExplicitPolicy || c.DeliverPolicy != jetstream.DeliverAllPolicy ||
		c.AckWait != 30*time.Second || c.MaxAckPending != 1 || c.MaxDeliver != -1 ||
		c.FilterSubject != VoiceCommandSubject || len(c.FilterSubjects) != 0 ||
		c.DeliverSubject != "" || c.DeliverGroup != "" || c.InactiveThreshold != 0 ||
		len(c.BackOff) != 0 || c.HeadersOnly || c.FlowControl || c.IdleHeartbeat != 0 ||
		c.RateLimit != 0 || (c.Replicas != 0 && c.Replicas != 1) || c.MemoryStorage {
		return ErrVoiceCommandContinuity
	}
	return validateVoiceCommandConsumerOptions(c)
}

func validateVoiceCommandConsumerOptions(c jetstream.ConsumerConfig) error {
	if c.OptStartSeq != 0 || c.OptStartTime != nil || c.ReplayPolicy != jetstream.ReplayInstantPolicy ||
		c.MaxWaiting < 0 || c.MaxRequestBatch != 0 || c.MaxRequestExpires != 0 || c.MaxRequestMaxBytes != 0 ||
		(c.PauseUntil != nil && c.PauseUntil.After(time.Now())) ||
		c.PriorityPolicy != jetstream.PriorityPolicyNone || c.PinnedTTL != 0 || len(c.PriorityGroups) != 0 {
		return ErrVoiceCommandContinuity
	}
	return nil
}

func validateVoiceCommandAckProof(before, after *jetstream.ConsumerInfo, stream *natssdk.StreamInfo) error {
	if err := validateVoiceCommandConsumer(after); err != nil {
		return err
	}
	if before == nil || stream == nil || stream.State.LastSeq > math.MaxInt64 || !before.Created.Equal(after.Created) ||
		!reflect.DeepEqual(before.Config, after.Config) || after.AckFloor.Stream < before.AckFloor.Stream ||
		after.AckFloor.Consumer < before.AckFloor.Consumer {
		return ErrVoiceCommandContinuity
	}
	for _, info := range []*jetstream.ConsumerInfo{before, after} {
		if info.AckFloor.Stream > info.Delivered.Stream || info.Delivered.Stream > stream.State.LastSeq ||
			info.AckFloor.Consumer > info.Delivered.Consumer || info.Delivered.Consumer > math.MaxInt64 ||
			info.NumAckPending < 0 || info.NumAckPending > 1 || info.NumRedelivered < 0 || info.NumRedelivered > 1 ||
			info.NumWaiting < 0 || info.NumPending > stream.State.LastSeq {
			return ErrVoiceCommandContinuity
		}
	}
	return nil
}

// RetireVoiceCommandPrefix resumes one SQL-prepared interval. It accepts only
// its complete old or completed suffix, and never needs the historic consumer
// to exist again. The current head and every later sequence remain retained.
func (c *Client) RetireVoiceCommandPrefix(ctx context.Context, receipt voicecommand.TransportReceipt, fromFloor, through uint64) error {
	ctx, cancel := context.WithTimeout(ctx, voiceCommandAPIBudget)
	defer cancel()
	if through <= fromFloor || through >= math.MaxInt64 {
		return voiceCommandPublisherError(ErrVoiceCommandContinuity)
	}
	js, manager, err := c.verifiedPublicationManager(ctx, receipt)
	if err != nil {
		return err
	}
	floor, before, err := readVoiceCommandRetirementSuffix(ctx, js, receipt, fromFloor, through)
	if err != nil {
		return voiceCommandPublisherError(err)
	}
	var purgeErr error
	if floor == fromFloor {
		purgeErr = purgeVoiceCommandThrough(ctx, manager, through)
	}
	afterFloor, after, readErr := readVoiceCommandRetirementSuffix(ctx, js, receipt, fromFloor, through)
	verifyErr := c.Verify(ctx, receipt)
	if err := errors.Join(voiceCommandPublisherError(readErr), voiceCommandPublisherError(verifyErr)); err != nil {
		return errors.Join(voiceCommandPublisherError(purgeErr), err)
	}
	if !voiceCommandHeadPreserved(before, after) {
		return voiceCommandPublisherError(ErrVoiceCommandContinuity)
	}
	if afterFloor != through {
		if purgeErr != nil {
			return voiceCommandPublisherError(purgeErr)
		}
		return voiceCommandPublisherError(ErrVoiceCommandContinuity)
	}
	// A verified completed suffix resolves a lost purge acknowledgement. A
	// bounded duplicate purge can only remove the same already-authorized rows.
	return nil
}

func readVoiceCommandRetirementSuffix(ctx context.Context, js natssdk.JetStreamContext, receipt voicecommand.TransportReceipt, fromFloor, through uint64) (uint64, *voicecommand.BrokerCommand, error) {
	info, err := js.StreamInfo(VoiceCommandStream, natssdk.Context(ctx))
	if err != nil {
		return 0, nil, err
	}
	floor, err := voiceCommandRetirementFloor(info, receipt, fromFloor, through)
	if err != nil {
		return 0, nil, err
	}
	command, err := readLastVoiceCommandAfter(ctx, js, receipt, floor)
	return floor, command, err
}

func (c *Client) verifiedPublicationManager(ctx context.Context, receipt voicecommand.TransportReceipt) (natssdk.JetStreamContext, jetstream.JetStream, error) {
	if err := c.Verify(ctx, receipt); err != nil {
		return nil, nil, voiceCommandPublisherError(err)
	}
	js, manager, err := c.voiceCommandJetStream(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := voiceCommandPublicationOwnerReady(ctx, manager, receipt); err != nil {
		return nil, nil, voiceCommandPublisherError(err)
	}
	return js, manager, nil
}

func voiceCommandRetirementFloor(info *natssdk.StreamInfo, receipt voicecommand.TransportReceipt, fromFloor, through uint64) (uint64, error) {
	if info == nil || through >= info.State.LastSeq {
		return 0, ErrVoiceCommandContinuity
	}
	if err := validateVoiceCommandStream(info, receipt.Generation, false); err != nil {
		return 0, err
	}
	if validateVoiceCommandSuffix(info, receipt, fromFloor) == nil {
		return fromFloor, nil
	}
	if validateVoiceCommandSuffix(info, receipt, through) == nil {
		return through, nil
	}
	return 0, ErrVoiceCommandContinuity
}

func purgeVoiceCommandThrough(ctx context.Context, manager jetstream.JetStream, through uint64) error {
	stream, err := manager.Stream(ctx, VoiceCommandStream)
	if err != nil {
		return fmt.Errorf("bind voice command prefix retirement: %w", err)
	}
	// The modern API propagates ctx. The legacy PurgeStream wrapper does not
	// forward caller Context options to its internal request.
	if err := stream.Purge(ctx, jetstream.WithPurgeSequence(through+1)); err != nil {
		return fmt.Errorf("retire acknowledged voice command prefix: %w", err)
	}
	return nil
}

func voiceCommandHeadPreserved(before, after *voicecommand.BrokerCommand) bool {
	if before == nil || after == nil || before.Sequence == 0 || before.Sequence > math.MaxInt64 ||
		after.Sequence == 0 || after.Sequence > math.MaxInt64 {
		return false
	}
	if after.Sequence == before.Sequence {
		return bytes.Equal(before.Data, after.Data)
	}
	// Pending SQL retirement bars fresh claims. Only the original expired
	// predecessor-CAS publisher can append its single uncertain successor.
	return after.Sequence == before.Sequence+1
}
