package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	natssdk "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
)

// These tests use the independently owned real JetStream fixture. A message is
// considered published only when it is retained in the frozen stream; forwarding
// options through an in-process fake would not prove the broker obeys them.
func publisherCommandBroker(t *testing.T) (*Client, natssdk.JetStreamContext, context.Context, voicecommand.TransportReceipt) {
	t.Helper()
	client, js, ctx := voiceCommandBroker(t)
	generation := uuid.MustParse("10000000-0000-4000-8000-000000000001")
	identity, err := client.Initialize(ctx, generation)
	require.NoError(t, err)
	receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
	require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
	require.NoError(t, client.Verify(ctx, receipt), "positive receipt control")
	return client, js, ctx, receipt
}

func publisherCommand(t *testing.T, receipt voicecommand.TransportReceipt, sequence uint64) voicecommand.VoiceCommand {
	t.Helper()
	commandID := fmt.Sprintf("20000000-0000-4000-8000-%012d", sequence)
	channelID := "30000000-0000-4000-8000-000000000001"
	command := voicecommand.VoiceCommand{
		Schema: voicecommand.Schema, Version: voicecommand.Version,
		StreamGeneration: receipt.Generation.String(), CommandID: commandID,
		Sequence: strconv.FormatUint(sequence, 10), Scope: "channel",
		Kind: "user_moderation", Action: "mute", ChannelID: &channelID,
		UserID: "40000000-0000-4000-8000-000000000001",
		Target: &voicecommand.ExactTarget{
			SessionGeneration: "50000000-0000-4000-8000-000000000001",
			NodeBootID:        "60000000-0000-4000-8000-000000000001",
			SocketID:          "fixture-publisher-socket", CredentialEpoch: "11111111111111111111111111111111",
		},
	}
	encoded, err := json.Marshal(command)
	require.NoError(t, err)
	decoded, err := voicecommand.DecodeVoiceCommand(encoded, receipt.Generation)
	require.NoError(t, err, "the positive envelope must reach the publisher boundary")
	require.Equal(t, command, decoded)
	return command
}

func requirePublisherPrefix(t *testing.T, js natssdk.JetStreamContext, commands ...voicecommand.VoiceCommand) {
	t.Helper()
	info, err := js.StreamInfo(testVoiceCommandStream)
	require.NoError(t, err)
	assert.EqualValues(t, len(commands), info.State.Msgs, "no duplicate or rejected publication may append")
	assert.EqualValues(t, len(commands), info.State.LastSeq)
	if len(commands) > 0 {
		assert.EqualValues(t, 1, info.State.FirstSeq)
	}
	assert.Zero(t, info.State.NumDeleted)
	for index, command := range commands {
		stored, getErr := js.GetMsg(testVoiceCommandStream, uint64(index+1))
		require.NoError(t, getErr)
		assert.Equal(t, "voice.commands.v1", stored.Subject)
		assert.EqualValues(t, index+1, stored.Sequence)
		assert.Equal(t, command.CommandID, stored.Header.Get("Nats-Msg-Id"))
		generation, parseErr := uuid.Parse(command.StreamGeneration)
		require.NoError(t, parseErr)
		decoded, decodeErr := voicecommand.DecodeVoiceCommand(stored.Data, generation)
		require.NoError(t, decodeErr)
		assert.Equal(t, command, decoded, "the exact immutable envelope reaches the durable broker")
	}
}

func requirePublisherReceipt(t *testing.T, receipt voicecommand.PublishReceipt, sequence uint64, duplicate bool) {
	t.Helper()
	assert.Equal(t, "VOICE_COMMANDS_V1", receipt.Stream)
	assert.Equal(t, sequence, receipt.Sequence)
	assert.Equal(t, duplicate, receipt.Duplicate)
}

func TestVoiceCommandPublisherRealJetStreamOrderHeadersAndExactHead(t *testing.T) {
	client, js, ctx, receipt := publisherCommandBroker(t)
	last, err := client.LastVoiceCommand(ctx, receipt)
	require.NoError(t, err)
	assert.Nil(t, last, "nil is a proved never-used empty stream")
	first, second := publisherCommand(t, receipt, 1), publisherCommand(t, receipt, 2)
	for _, command := range []voicecommand.VoiceCommand{first, second} {
		ack, publishErr := client.PublishVoiceCommand(ctx, receipt, command)
		require.NoError(t, publishErr)
		sequence, parseErr := strconv.ParseUint(command.Sequence, 10, 64)
		require.NoError(t, parseErr)
		requirePublisherReceipt(t, ack, sequence, false)
		stored, getErr := js.GetMsg(testVoiceCommandStream, sequence)
		require.NoError(t, getErr)
		assert.Equal(t, "VOICE_COMMANDS_V1", stored.Header.Get("Nats-Expected-Stream"))
		assert.Equal(t, strconv.FormatUint(sequence-1, 10), stored.Header.Get("Nats-Expected-Last-Sequence"))
		assert.Equal(t, strconv.FormatUint(sequence-1, 10), stored.Header.Get("Nats-Expected-Last-Subject-Sequence"))
		assert.Equal(t, command.CommandID, stored.Header.Get("Nats-Msg-Id"))
	}
	requirePublisherPrefix(t, js, first, second)
	last, err = client.LastVoiceCommand(ctx, receipt)
	require.NoError(t, err)
	require.NotNil(t, last)
	assert.EqualValues(t, 2, last.Sequence)
	decoded, err := voicecommand.DecodeVoiceCommand(last.Data, receipt.Generation)
	require.NoError(t, err)
	assert.Equal(t, second, decoded)
}

func TestVoiceCommandPublisherRealJetStreamRetainsCompleteModerationSnapshots(t *testing.T) {
	client, js, ctx, receipt := publisherCommandBroker(t)
	serverID := "70000000-0000-4000-8000-000000000001"
	muted, deafened, cleared := true, true, false
	channel := publisherCommand(t, receipt, 1)
	channel.Kind, channel.Action = "server_moderation", "deafen"
	channel.ServerID = &serverID
	channel.AuthorizationRevision = "9223372036854775807"
	channel.ServerMuted, channel.ServerDeafened = &muted, &deafened
	dm := publisherCommand(t, receipt, 2)
	dm.Scope, dm.Kind, dm.Action = "dm", "server_moderation", "unmute"
	dm.AuthorizationRevision = "42"
	dm.ServerMuted, dm.ServerDeafened = &cleared, &cleared
	offline := publisherCommand(t, receipt, 3)
	offline.Kind, offline.Action = "server_moderation", "undeafen"
	offline.ChannelID, offline.Target, offline.ServerID = nil, nil, &serverID
	offline.AuthorizationRevision = "43"
	offline.ServerMuted, offline.ServerDeafened = &muted, &cleared
	commands := []voicecommand.VoiceCommand{channel, dm, offline}
	for index, command := range commands {
		ack, err := client.PublishVoiceCommand(ctx, receipt, command)
		require.NoError(t, err)
		requirePublisherReceipt(t, ack, uint64(index+1), false)
	}
	// Read the actual stored envelopes: true and false snapshot fields and their
	// independent authorization revisions must survive publication, including the
	// null-target marker for saved offline server policy.
	requirePublisherPrefix(t, js, commands...)
	head, err := client.LastVoiceCommand(ctx, receipt)
	require.NoError(t, err)
	require.NotNil(t, head)
	decoded, err := voicecommand.DecodeVoiceCommand(head.Data, receipt.Generation)
	require.NoError(t, err)
	assert.Equal(t, offline, decoded)
	assert.Nil(t, decoded.ChannelID)
	assert.Nil(t, decoded.Target)
	assert.Equal(t, "43", decoded.AuthorizationRevision)
	require.NotNil(t, decoded.ServerMuted)
	require.NotNil(t, decoded.ServerDeafened)
	assert.True(t, *decoded.ServerMuted)
	assert.False(t, *decoded.ServerDeafened)
}

func TestVoiceCommandPublisherRealJetStreamDuplicateAndStalePredecessor(t *testing.T) {
	client, js, ctx, receipt := publisherCommandBroker(t)
	first, second := publisherCommand(t, receipt, 1), publisherCommand(t, receipt, 2)
	for _, command := range []voicecommand.VoiceCommand{first, second} {
		_, err := client.PublishVoiceCommand(ctx, receipt, command)
		require.NoError(t, err)
	}
	// The privileged fixture retry omits CAS solely to prove that the actual
	// broker's MsgID cache is live. Production retries retain both CAS headers.
	data, err := json.Marshal(first)
	require.NoError(t, err)
	message := natssdk.NewMsg("voice.commands.v1")
	message.Data = data
	control, err := js.PublishMsg(message, natssdk.Context(ctx), natssdk.MsgId(first.CommandID), natssdk.ExpectStream(testVoiceCommandStream))
	require.NoError(t, err)
	assert.Equal(t, "VOICE_COMMANDS_V1", control.Stream)
	assert.EqualValues(t, 1, control.Sequence)
	assert.True(t, control.Duplicate, "same-ID retry must reach the actual duplicate cache")
	requirePublisherPrefix(t, js, first, second)

	// The frozen dossier admits either the original duplicate receipt or a CAS
	// conflict. nats-server 2.15.1 can compare predecessor before its duplicate
	// lookup, so the production retry must not require the opposite check order.
	duplicate, err := client.PublishVoiceCommand(ctx, receipt, first)
	if err == nil {
		requirePublisherReceipt(t, duplicate, 1, true)
	} else {
		var conflict *natssdk.APIError
		require.ErrorAs(t, err, &conflict, "only the broker's typed predecessor conflict is an admissible refusal")
		assert.Equal(t, natssdk.JSErrCodeStreamWrongLastSequence, conflict.ErrorCode)
		assert.Empty(t, duplicate, "a CAS refusal cannot manufacture a publication receipt")
	}
	requirePublisherPrefix(t, js, first, second)
}

func TestVoiceCommandPublisherRealJetStreamUncachedStalePredecessorCannotAppend(t *testing.T) {
	client, js, ctx, receipt := publisherCommandBroker(t)
	first, second := publisherCommand(t, receipt, 1), publisherCommand(t, receipt, 2)
	for _, command := range []voicecommand.VoiceCommand{first, second} {
		_, err := client.PublishVoiceCommand(ctx, receipt, command)
		require.NoError(t, err)
	}

	// A fresh, valid MsgID gives a real cache miss without altering the immutable
	// 120-second window. This proves CAS independently of dedupe, but is not an
	// elapsed-120-second test. A resumed old request must retain predecessor zero.
	stale := first
	stale.CommandID = "20000000-0000-4000-8000-000000000099"
	refused, err := client.PublishVoiceCommand(ctx, receipt, stale)
	var conflict *natssdk.APIError
	require.ErrorAs(t, err, &conflict, "an uncached resumed request must reach the real broker's predecessor CAS")
	assert.Equal(t, natssdk.JSErrCodeStreamWrongLastSequence, conflict.ErrorCode)
	assert.Empty(t, refused)
	requirePublisherPrefix(t, js, first, second)
	// The next legitimate sequence is a positive control for the same connection.
	third := publisherCommand(t, receipt, 3)
	ack, err := client.PublishVoiceCommand(ctx, receipt, third)
	require.NoError(t, err)
	requirePublisherReceipt(t, ack, 3, false)
	requirePublisherPrefix(t, js, first, second, third)
}

func TestVoiceCommandPublisherRealJetStreamAcceptsReleasedAndHeldOwner(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(strconv.FormatBool(held), func(t *testing.T) {
			client, js, ctx, receipt := publisherCommandBroker(t)
			if held {
				_, _ = holdCommandOwner(t, js, "60000000-0000-4000-8000-000000000002")
			}
			owner, revision := readCommandOwner(t, js)
			command := publisherCommand(t, receipt, 1)
			ack, err := client.PublishVoiceCommand(ctx, receipt, command)
			require.NoError(t, err)
			requirePublisherReceipt(t, ack, 1, false)
			requirePublisherPrefix(t, js, command)
			retainedOwner, retainedRevision := readCommandOwner(t, js)
			assert.Equal(t, owner, retainedOwner)
			assert.Equal(t, revision, retainedRevision, "publication cannot acquire, release or rewrite media ownership")
		})
	}
}

func TestVoiceCommandPublisherRealJetStreamInitializingOwnerCannotPublish(t *testing.T) {
	client, js, ctx := voiceCommandBroker(t)
	generation := uuid.MustParse("10000000-0000-4000-8000-000000000001")
	identity, err := client.Initialize(ctx, generation)
	require.NoError(t, err)
	receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
	require.NoError(t, client.Verify(ctx, receipt), "initializing receipt is valid bootstrap evidence")
	head, err := client.LastVoiceCommand(ctx, receipt)
	require.Error(t, err, "publication reads require the confirmed release proof too")
	assert.Nil(t, head)
	command := publisherCommand(t, receipt, 1)
	_, err = client.PublishVoiceCommand(ctx, receipt, command)
	require.Error(t, err, "initialization must be confirmed and released before publication")
	requirePublisherPrefix(t, js)
	require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
	head, err = client.LastVoiceCommand(ctx, receipt)
	require.NoError(t, err, "released ownership is the only changed read prerequisite")
	assert.Nil(t, head, "the stream remains proved empty before the first append")
	ack, err := client.PublishVoiceCommand(ctx, receipt, command)
	require.NoError(t, err, "release is the only changed prerequisite")
	requirePublisherReceipt(t, ack, 1, false)
	requirePublisherPrefix(t, js, command)
}

func TestValidateStoredVoiceCommandRejectsMissingOrAmbiguousEvidence(t *testing.T) {
	client, js, ctx, receipt := publisherCommandBroker(t)
	command := publisherCommand(t, receipt, 1)
	_, err := client.PublishVoiceCommand(ctx, receipt, command)
	require.NoError(t, err)
	stored, err := js.GetMsg(testVoiceCommandStream, 1)
	require.NoError(t, err)
	require.NoError(t, validateStoredVoiceCommand(stored, receipt), "real retained message is the positive evidence control")
	cases := []struct {
		name    string
		missing bool
		alter   func(*natssdk.RawStreamMsg)
	}{
		{name: "missing management result", missing: true},
		{name: "wrong raw subject", alter: func(raw *natssdk.RawStreamMsg) { raw.Subject = "fixture.unexpected-subject" }},
		{name: "missing broker sequence", alter: func(raw *natssdk.RawStreamMsg) { raw.Sequence = 0 }},
		{name: "two message IDs", alter: func(raw *natssdk.RawStreamMsg) {
			raw.Header["Nats-Msg-Id"] = []string{command.CommandID, "20000000-0000-4000-8000-000000000099"}
		}},
		{name: "case alias beside canonical message ID", alter: func(raw *natssdk.RawStreamMsg) {
			raw.Header["nats-msg-id"] = []string{command.CommandID}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var input *natssdk.RawStreamMsg
			if !tc.missing {
				copyOfStored := *stored
				copyOfStored.Header = make(natssdk.Header, len(stored.Header))
				for key, values := range stored.Header {
					copyOfStored.Header[key] = append([]string(nil), values...)
				}
				input = &copyOfStored
				tc.alter(input)
			}
			require.ErrorIs(t, validateStoredVoiceCommand(input, receipt), ErrVoiceCommandContinuity)
		})
	}
	requirePublisherPrefix(t, js, command)
	head, err := client.LastVoiceCommand(ctx, receipt)
	require.NoError(t, err, "defensive validation cannot alter the real retained control")
	require.NotNil(t, head)
	assert.EqualValues(t, 1, head.Sequence)
}

func TestLastVoiceCommandRealJetStreamRejectsMalformedOrForeignHead(t *testing.T) {
	cases := []struct {
		name    string
		alter   func(voicecommand.VoiceCommand) (voicecommand.VoiceCommand, string)
		payload func([]byte) []byte
	}{
		{name: "unknown envelope field", payload: func(data []byte) []byte {
			return append(append([]byte(nil), data[:len(data)-1]...), []byte(`,"unrecognized":true}`)...)
		}},
		{name: "malformed JSON", payload: func([]byte) []byte { return []byte(`{"schema":`) }},
		{name: "wrong generation", alter: func(command voicecommand.VoiceCommand) (voicecommand.VoiceCommand, string) {
			command.StreamGeneration = "10000000-0000-4000-8000-000000000002"
			return command, command.CommandID
		}},
		{name: "future command sequence", alter: func(command voicecommand.VoiceCommand) (voicecommand.VoiceCommand, string) {
			command.Sequence = "2"
			return command, command.CommandID
		}},
		{name: "wrong MsgID", alter: func(command voicecommand.VoiceCommand) (voicecommand.VoiceCommand, string) {
			return command, "20000000-0000-4000-8000-000000000099"
		}},
		{name: "missing MsgID", alter: func(command voicecommand.VoiceCommand) (voicecommand.VoiceCommand, string) {
			return command, ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, js, ctx, receipt := publisherCommandBroker(t)
			command := publisherCommand(t, receipt, 1)
			messageID := command.CommandID
			if tc.alter != nil {
				command, messageID = tc.alter(command)
			}
			data, err := json.Marshal(command)
			require.NoError(t, err)
			if tc.payload != nil {
				data = tc.payload(data)
			}
			message := natssdk.NewMsg("voice.commands.v1")
			message.Data = data
			if messageID != "" {
				message.Header.Set("Nats-Msg-Id", messageID)
			}
			ack, err := js.PublishMsg(message, natssdk.Context(ctx))
			require.NoError(t, err, "fixture injection must be retained by the actual broker")
			require.EqualValues(t, 1, ack.Sequence)
			require.NoError(t, client.Verify(ctx, receipt), "birth, owner proof and configuration remain valid")
			last, err := client.LastVoiceCommand(ctx, receipt)
			require.ErrorIs(t, err, voicecommand.ErrBrokerContinuity)
			assert.Nil(t, last, "invalid broker content cannot be handed to reconciliation")
			stored, getErr := js.GetMsg(testVoiceCommandStream, 1)
			require.NoError(t, getErr)
			assert.Equal(t, data, stored.Data, "continuity refusal cannot repair or erase poison")
			info, infoErr := js.StreamInfo(testVoiceCommandStream)
			require.NoError(t, infoErr)
			assert.EqualValues(t, 1, info.State.Msgs)
			assert.EqualValues(t, 1, info.State.LastSeq)
		})
	}
}

func TestLastVoiceCommandRealJetStreamRejectsForeignSubjectWithFrozenConfig(t *testing.T) {
	client, js, ctx, receipt := publisherCommandBroker(t)
	info, err := js.StreamInfo(testVoiceCommandStream)
	require.NoError(t, err)
	expanded := info.Config
	expanded.Subjects = []string{"voice.commands.v1", "fixture.foreign-command"}
	_, err = js.UpdateStream(&expanded)
	require.NoError(t, err)
	command := publisherCommand(t, receipt, 1)
	data, err := json.Marshal(command)
	require.NoError(t, err)
	message := natssdk.NewMsg("fixture.foreign-command")
	message.Data = data
	message.Header.Set("Nats-Msg-Id", command.CommandID)
	ack, err := js.PublishMsg(message, natssdk.Context(ctx))
	require.NoError(t, err)
	require.EqualValues(t, 1, ack.Sequence)
	_, err = js.UpdateStream(&info.Config)
	require.NoError(t, err)
	require.NoError(t, client.Verify(ctx, receipt), "the negative read must reach retained subject validation, not config refusal")
	last, err := client.LastVoiceCommand(ctx, receipt)
	require.ErrorIs(t, err, voicecommand.ErrBrokerContinuity)
	assert.Nil(t, last, "no exact-subject message cannot mean an empty stream while a foreign message remains")
	stored, err := js.GetMsg(testVoiceCommandStream, 1)
	require.NoError(t, err)
	assert.Equal(t, "fixture.foreign-command", stored.Subject)
	assert.Equal(t, data, stored.Data)
}

func TestLastVoiceCommandRealJetStreamRejectsDeletedPrefixOrPurge(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(natssdk.JetStreamContext) error
	}{
		{name: "deleted first sequence", mutate: func(js natssdk.JetStreamContext) error { return js.DeleteMsg(testVoiceCommandStream, 1) }},
		{name: "deleted interior sequence", mutate: func(js natssdk.JetStreamContext) error { return js.DeleteMsg(testVoiceCommandStream, 2) }},
		{name: "deleted last sequence", mutate: func(js natssdk.JetStreamContext) error { return js.DeleteMsg(testVoiceCommandStream, 3) }},
		{name: "purged previously used stream", mutate: func(js natssdk.JetStreamContext) error { return js.PurgeStream(testVoiceCommandStream) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, js, ctx, receipt := publisherCommandBroker(t)
			commands := []voicecommand.VoiceCommand{publisherCommand(t, receipt, 1), publisherCommand(t, receipt, 2), publisherCommand(t, receipt, 3)}
			for _, command := range commands {
				_, err := client.PublishVoiceCommand(ctx, receipt, command)
				require.NoError(t, err)
			}
			requirePublisherPrefix(t, js, commands...)
			require.NoError(t, tc.mutate(js))
			require.NoError(t, client.Verify(ctx, receipt), "configuration and resource births survive prefix loss")
			before, err := js.StreamInfo(testVoiceCommandStream)
			require.NoError(t, err)
			require.EqualValues(t, 3, before.State.LastSeq, "deleted/purged messages retain broker sequence evidence")
			require.Less(t, before.State.Msgs, uint64(3))
			last, err := client.LastVoiceCommand(ctx, receipt)
			require.ErrorIs(t, err, voicecommand.ErrBrokerContinuity)
			assert.Nil(t, last, "neither an intact tail nor ErrMsgNotFound can bless a missing prefix")
			after, err := js.StreamInfo(testVoiceCommandStream)
			require.NoError(t, err)
			assert.Equal(t, before.State, after.State, "read refusal must not purge or reset sequence evidence")
		})
	}
}

func TestVoiceCommandPublisherRealJetStreamReadsAndPublishesVerifyReceipt(t *testing.T) {
	cases := []struct {
		name  string
		alter func(*voicecommand.TransportReceipt)
	}{
		{name: "different generation", alter: func(receipt *voicecommand.TransportReceipt) {
			receipt.Generation = uuid.MustParse("10000000-0000-4000-8000-000000000002")
		}},
		{name: "missing birth", alter: func(receipt *voicecommand.TransportReceipt) { receipt.Identity.StreamCreatedAt = time.Time{} }},
		{name: "different stream birth", alter: func(receipt *voicecommand.TransportReceipt) {
			receipt.Identity.StreamCreatedAt = receipt.Identity.StreamCreatedAt.Add(time.Nanosecond)
		}},
		{name: "different owner birth", alter: func(receipt *voicecommand.TransportReceipt) {
			receipt.Identity.OwnerStoreCreatedAt = receipt.Identity.OwnerStoreCreatedAt.Add(time.Nanosecond)
		}},
		{name: "different initializing revision", alter: func(receipt *voicecommand.TransportReceipt) { receipt.Identity.InitializingRevision++ }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, js, ctx, receipt := publisherCommandBroker(t)
			command := publisherCommand(t, receipt, 1)
			invalid := receipt
			tc.alter(&invalid)
			last, err := client.LastVoiceCommand(ctx, invalid)
			require.ErrorIs(t, err, ErrVoiceCommandContinuity)
			assert.Nil(t, last)
			_, err = client.PublishVoiceCommand(ctx, invalid, command)
			require.Error(t, err)
			requirePublisherPrefix(t, js)
			ack, err := client.PublishVoiceCommand(ctx, receipt, command)
			require.NoError(t, err, "the correct committed receipt is the positive control")
			requirePublisherReceipt(t, ack, 1, false)
		})
	}
}

func TestVoiceCommandPublisherRealJetStreamReadsAndPublishesRefuseChangedConfigOrBirth(t *testing.T) {
	for _, mode := range []string{"command config", "owner config", "command birth", "owner birth", "missing stream", "wrong stream name"} {
		t.Run(mode, func(t *testing.T) {
			client, js, ctx, receipt := publisherCommandBroker(t)
			command := publisherCommand(t, receipt, 1)
			switch mode {
			case "command config", "owner config":
				name := testVoiceCommandStream
				if mode == "owner config" {
					name = "KV_" + testVoiceCommandBucket
				}
				info, err := js.StreamInfo(name)
				require.NoError(t, err)
				config := info.Config
				if mode == "owner config" {
					config.MaxBytes = 65536
				} else {
					config.MaxBytes--
				}
				_, err = js.UpdateStream(&config)
				require.NoError(t, err)
			case "command birth", "missing stream", "wrong stream name":
				require.NoError(t, js.DeleteStream(testVoiceCommandStream))
				if mode != "missing stream" {
					config := voiceCommandStreamConfig(receipt.Generation)
					if mode == "wrong stream name" {
						config.Name = "FIXTURE_WRONG_VOICE_COMMAND_STREAM"
					}
					created, err := js.AddStream(&config)
					require.NoError(t, err)
					require.False(t, created.Created.Equal(receipt.Identity.StreamCreatedAt))
				}
			case "owner birth":
				owner, _ := readCommandOwner(t, js)
				require.NotNil(t, owner.Proof)
				require.NoError(t, js.DeleteKeyValue(testVoiceCommandBucket))
				config := voiceCommandOwnerStreamConfig(receipt.Generation)
				created, err := js.AddStream(&config)
				require.NoError(t, err)
				require.False(t, created.Created.Equal(receipt.Identity.OwnerStoreCreatedAt))
				owner.Proof.OwnerStoreCreatedAt = created.Created.UTC().Format(time.RFC3339Nano)
				encoded, err := json.Marshal(owner)
				require.NoError(t, err)
				kv, err := js.KeyValue(testVoiceCommandBucket)
				require.NoError(t, err)
				createdRevision, err := kv.Create(testVoiceCommandOwnerKey, encoded)
				require.NoError(t, err)
				require.Equal(t, receipt.Identity.InitializingRevision, createdRevision, "a recreated owner can reset the original revision")
			}
			last, err := client.LastVoiceCommand(ctx, receipt)
			require.Error(t, err)
			assert.Nil(t, last)
			_, err = client.PublishVoiceCommand(ctx, receipt, command)
			require.Error(t, err, "reads and publication must both check frozen config and original births")
			if mode == "missing stream" || mode == "wrong stream name" {
				_, err = js.StreamInfo(testVoiceCommandStream)
				require.ErrorIs(t, err, natssdk.ErrStreamNotFound, "publisher must never recreate or rename missing resources")
				if mode == "wrong stream name" {
					info, infoErr := js.StreamInfo("FIXTURE_WRONG_VOICE_COMMAND_STREAM")
					require.NoError(t, infoErr)
					assert.Zero(t, info.State.Msgs, "expected stream identity cannot adopt another stream")
				}
			} else {
				requirePublisherPrefix(t, js)
			}
		})
	}
}

func TestVoiceCommandPublisherRealJetStreamRejectsInvalidCommandBeforeAppend(t *testing.T) {
	for _, mode := range []string{"wrong generation", "zero sequence", "sequence exceeds BIGINT", "missing target", "oversized socket", "invalid UTF-8 socket"} {
		t.Run(mode, func(t *testing.T) {
			client, js, ctx, receipt := publisherCommandBroker(t)
			command := publisherCommand(t, receipt, 1)
			switch mode {
			case "wrong generation":
				command.StreamGeneration = "10000000-0000-4000-8000-000000000002"
			case "zero sequence":
				command.Sequence = "0"
			case "sequence exceeds BIGINT":
				command.Sequence = "9223372036854775808"
			case "missing target":
				command.Target = nil
			case "oversized socket":
				command.Target.SocketID = strings.Repeat("x", 129)
			case "invalid UTF-8 socket":
				command.Target.SocketID = string([]byte{0xff, 'x'})
			}
			_, err := client.PublishVoiceCommand(ctx, receipt, command)
			require.Error(t, err)
			if mode == "invalid UTF-8 socket" {
				require.ErrorIs(t, err, voicecommand.ErrInvalidCommand, "JSON replacement cannot change a frozen socket identity")
			}
			requirePublisherPrefix(t, js)
		})
	}
}

func TestVoiceCommandPublisherRealJetStreamCanceledContextDoesNotAppend(t *testing.T) {
	client, js, _, receipt := publisherCommandBroker(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	last, err := client.LastVoiceCommand(ctx, receipt)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, last)
	ack, err := client.PublishVoiceCommand(ctx, receipt, publisherCommand(t, receipt, 1))
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, ack)
	requirePublisherPrefix(t, js)
}

// The dedicated connection embeds the same production publisher implementation.
var _ voicecommand.CommandBroker = (*Client)(nil)
var _ voicecommand.CommandBroker = (*VoiceCommandClient)(nil)
