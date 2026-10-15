package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	natssdk "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const voiceCommandHeadReadAttempts = 3

var errVoiceCommandOwnerInitializing = errors.New("voice command owner has not been released")
var errVoiceCommandHeadChanged = errors.New("voice command head changed during read")

// LastVoiceCommand retains the original, uncompacted-prefix contract.
func (c *Client) LastVoiceCommand(ctx context.Context, receipt voicecommand.TransportReceipt) (*voicecommand.BrokerCommand, error) {
	return c.LastVoiceCommandAfter(ctx, receipt, 0)
}

// LastVoiceCommandAfter accepts only the complete suffix authorized by SQL's
// recorded retirement floor. It never derives that floor from broker deletion.
func (c *Client) LastVoiceCommandAfter(ctx context.Context, receipt voicecommand.TransportReceipt, floor uint64) (*voicecommand.BrokerCommand, error) {
	ctx, cancel := context.WithTimeout(ctx, voiceCommandAPIBudget)
	defer cancel()
	if err := c.Verify(ctx, receipt); err != nil {
		return nil, voiceCommandPublisherError(err)
	}
	js, manager, err := c.voiceCommandJetStream(ctx)
	if err != nil {
		return nil, err
	}
	if err := voiceCommandPublicationOwnerReady(ctx, manager, receipt); err != nil {
		return nil, voiceCommandPublisherError(err)
	}
	command, readErr := readLastVoiceCommandAfter(ctx, js, receipt, floor)
	verifyErr := c.Verify(ctx, receipt)
	if err := errors.Join(voiceCommandPublisherError(readErr), voiceCommandPublisherError(verifyErr)); err != nil {
		return nil, err
	}
	return command, nil
}

func readLastVoiceCommandAfter(ctx context.Context, js natssdk.JetStreamContext, receipt voicecommand.TransportReceipt, floor uint64) (*voicecommand.BrokerCommand, error) {
	for range voiceCommandHeadReadAttempts {
		command, stable, err := readVoiceCommandHeadSnapshot(ctx, js, receipt, floor)
		if err != nil {
			return nil, err
		}
		if stable {
			return command, nil
		}
	}
	return nil, errVoiceCommandHeadChanged
}

func readVoiceCommandHeadSnapshot(ctx context.Context, js natssdk.JetStreamContext, receipt voicecommand.TransportReceipt, floor uint64) (*voicecommand.BrokerCommand, bool, error) {
	before, err := voiceCommandRetainedSuffix(ctx, js, receipt, floor)
	if err != nil {
		return nil, false, err
	}
	raw, readErr := js.GetLastMsg(VoiceCommandStream, VoiceCommandSubject, natssdk.Context(ctx))
	after, err := voiceCommandRetainedSuffix(ctx, js, receipt, floor)
	if err != nil {
		return nil, false, errors.Join(readErr, err)
	}
	if readErr != nil && !errors.Is(readErr, natssdk.ErrMsgNotFound) {
		return nil, false, fmt.Errorf("read retained voice command: %w", readErr)
	}
	if after.State.LastSeq < before.State.LastSeq {
		return nil, false, ErrVoiceCommandContinuity
	}
	if readErr == nil {
		if err := validateStoredVoiceCommand(raw, receipt); err != nil {
			return nil, false, err
		}
	}
	// An append can land between the snapshots and GET. Retry that read,
	// rather than treating an ordinary concurrent publisher as data loss.
	if before.State.LastSeq != after.State.LastSeq {
		return nil, false, nil
	}
	if unusedVoiceCommandPrefix(after, readErr) {
		return nil, true, nil
	}
	if readErr != nil || raw == nil || raw.Sequence != after.State.LastSeq {
		return nil, false, ErrVoiceCommandContinuity
	}
	return &voicecommand.BrokerCommand{Sequence: raw.Sequence, Data: append([]byte(nil), raw.Data...)}, true, nil
}

func unusedVoiceCommandPrefix(info *natssdk.StreamInfo, readErr error) bool {
	return info.State.Msgs == 0 && info.State.LastSeq == 0 && errors.Is(readErr, natssdk.ErrMsgNotFound)
}

func voiceCommandRetainedSuffix(ctx context.Context, js natssdk.JetStreamContext, receipt voicecommand.TransportReceipt, floor uint64) (*natssdk.StreamInfo, error) {
	info, err := js.StreamInfo(VoiceCommandStream, natssdk.Context(ctx))
	if err != nil {
		return nil, fmt.Errorf("read voice command retained prefix: %w", err)
	}
	if err := validateVoiceCommandStream(info, receipt.Generation, false); err != nil {
		return nil, err
	}
	if err := validateVoiceCommandSuffix(info, receipt, floor); err != nil {
		return nil, err
	}
	return info, nil
}

func validateVoiceCommandSuffix(info *natssdk.StreamInfo, receipt voicecommand.TransportReceipt, floor uint64) error {
	if info == nil || !info.Created.Equal(receipt.Identity.StreamCreatedAt) || floor > math.MaxInt64 {
		return ErrVoiceCommandContinuity
	}
	state := info.State
	if state.LastSeq > math.MaxInt64 || state.LastSeq < floor ||
		state.Msgs != state.LastSeq-floor || state.NumDeleted != 0 || len(state.Deleted) != 0 ||
		(state.Msgs > 0 && state.FirstSeq != floor+1) || (floor != 0 && state.Msgs == 0) {
		return ErrVoiceCommandContinuity
	}
	return nil
}

func validateStoredVoiceCommand(raw *natssdk.RawStreamMsg, receipt voicecommand.TransportReceipt) error {
	if raw == nil || raw.Subject != VoiceCommandSubject || raw.Sequence == 0 {
		return ErrVoiceCommandContinuity
	}
	command, err := voicecommand.DecodeVoiceCommand(raw.Data, receipt.Generation)
	if err != nil {
		return errors.Join(ErrVoiceCommandContinuity, err)
	}
	ids := raw.Header.Values(natssdk.MsgIdHdr)
	if len(ids) != 1 || ids[0] != command.CommandID {
		return ErrVoiceCommandContinuity
	}
	for key := range raw.Header {
		if key != natssdk.MsgIdHdr && strings.EqualFold(key, natssdk.MsgIdHdr) {
			return ErrVoiceCommandContinuity
		}
	}
	sequence, err := strconv.ParseUint(command.Sequence, 10, 63)
	if err != nil || sequence != raw.Sequence {
		return ErrVoiceCommandContinuity
	}
	return nil
}

// PublishVoiceCommand uses the immutable command's predecessor, including zero,
// to fence a publisher that resumes after its SQL lease and dedupe window expire.
func (c *Client) PublishVoiceCommand(ctx context.Context, receipt voicecommand.TransportReceipt, command voicecommand.VoiceCommand) (voicecommand.PublishReceipt, error) {
	return c.PublishVoiceCommandAfter(ctx, receipt, command, 0)
}

// PublishVoiceCommandAfter keeps exact predecessor CAS while accepting only
// the retained suffix authorized by the caller's committed SQL checkpoint.
func (c *Client) PublishVoiceCommandAfter(ctx context.Context, receipt voicecommand.TransportReceipt, command voicecommand.VoiceCommand, floor uint64) (voicecommand.PublishReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, voiceCommandAPIBudget)
	defer cancel()
	var result voicecommand.PublishReceipt
	frozen, data, err := freezeVoiceCommandForPublication(command, receipt)
	if err != nil {
		return result, err
	}
	sequence, err := strconv.ParseUint(frozen.Sequence, 10, 63)
	if err != nil {
		return result, voicecommand.ErrInvalidCommand
	}
	if err := c.Verify(ctx, receipt); err != nil {
		return result, voiceCommandPublisherError(err)
	}
	js, manager, err := c.voiceCommandJetStream(ctx)
	if err != nil {
		return result, err
	}
	if err := voiceCommandPublicationOwnerReady(ctx, manager, receipt); err != nil {
		return result, voiceCommandPublisherError(err)
	}
	if _, err := voiceCommandRetainedSuffix(ctx, js, receipt, floor); err != nil {
		return result, voiceCommandPublisherError(err)
	}
	ack, publishErr := js.PublishMsg(&natssdk.Msg{Subject: VoiceCommandSubject, Data: data},
		natssdk.Context(ctx), natssdk.MsgId(frozen.CommandID),
		natssdk.ExpectStream(VoiceCommandStream), natssdk.ExpectLastSequence(sequence-1),
		natssdk.ExpectLastSequencePerSubject(sequence-1), natssdk.RetryAttempts(0))
	if publishErr == nil && (ack == nil || ack.Stream != VoiceCommandStream || ack.Sequence != sequence) {
		publishErr = ErrVoiceCommandContinuity
	}
	verifyErr := c.Verify(ctx, receipt)
	var prefixErr error
	if verifyErr == nil {
		_, prefixErr = voiceCommandRetainedSuffix(ctx, js, receipt, floor)
	}
	if err := errors.Join(voiceCommandPublisherError(publishErr), voiceCommandPublisherError(verifyErr), voiceCommandPublisherError(prefixErr)); err != nil {
		return result, fmt.Errorf("publish durable voice command: %w", err)
	}
	return voicecommand.PublishReceipt{Stream: ack.Stream, Sequence: ack.Sequence, Duplicate: ack.Duplicate}, nil
}

func freezeVoiceCommandForPublication(command voicecommand.VoiceCommand, receipt voicecommand.TransportReceipt) (voicecommand.VoiceCommand, []byte, error) {
	// Copy every caller-owned pointer before any network wait or validation.
	if command.Target != nil {
		target := *command.Target
		if !utf8.ValidString(target.SocketID) {
			return voicecommand.VoiceCommand{}, nil, voicecommand.ErrInvalidCommand
		}
		command.Target = &target
	}
	if command.ChannelID != nil {
		channel := *command.ChannelID
		command.ChannelID = &channel
	}
	if command.ServerID != nil {
		server := *command.ServerID
		command.ServerID = &server
	}
	if command.ServerMuted != nil {
		muted := *command.ServerMuted
		command.ServerMuted = &muted
	}
	if command.ServerDeafened != nil {
		deafened := *command.ServerDeafened
		command.ServerDeafened = &deafened
	}
	data, err := json.Marshal(command)
	if err != nil {
		return voicecommand.VoiceCommand{}, nil, fmt.Errorf("encode durable voice command: %w", err)
	}
	frozen, err := voicecommand.DecodeVoiceCommand(data, receipt.Generation)
	if err != nil {
		return voicecommand.VoiceCommand{}, nil, err
	}
	return frozen, data, nil
}

func voiceCommandPublicationOwnerReady(ctx context.Context, manager jetstream.JetStream, receipt voicecommand.TransportReceipt) error {
	kv, err := manager.KeyValue(ctx, VoiceCommandOwnerBucket)
	if err != nil {
		return fmt.Errorf("bind voice command publication owner: %w", err)
	}
	entry, err := kv.Get(ctx, VoiceCommandOwnerKey)
	if err != nil {
		return fmt.Errorf("read voice command publication owner: %w", err)
	}
	owner, err := decodeVoiceCommandOwner(entry.Value(), receipt.Generation)
	if err != nil {
		return err
	}
	if owner.Phase == "initializing" {
		return errVoiceCommandOwnerInitializing
	}
	if entry.Revision() <= receipt.Identity.InitializingRevision || !proofMatches(owner.Proof, receipt.Identity) {
		return ErrVoiceCommandContinuity
	}
	return nil
}

func voiceCommandPublisherError(err error) error {
	if errors.Is(err, ErrVoiceCommandContinuity) || errors.Is(err, natssdk.ErrStreamNotFound) ||
		errors.Is(err, natssdk.ErrInvalidJSAck) || errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrBucketNotFound) || errors.Is(err, jetstream.ErrKeyNotFound) ||
		errors.Is(err, jetstream.ErrKeyDeleted) {
		return errors.Join(voicecommand.ErrBrokerContinuity, ErrVoiceCommandContinuity)
	}
	return err
}
