package nats

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	natssdk "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRetirementDurable = "voice-enforcement-v1"

// This profile is independently copied from the media consumer contract. The
// fixture must not obtain its expected configuration from the publisher.
func retirementConsumerConfig() jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Name: testRetirementDurable, Durable: testRetirementDurable,
		AckPolicy: jetstream.AckExplicitPolicy, DeliverPolicy: jetstream.DeliverAllPolicy,
		AckWait: 30 * time.Second, MaxAckPending: 1, MaxDeliver: -1,
		FilterSubject: "voice.commands.v1",
	}
}

func retirementConsumer(ctx context.Context, t *testing.T, client *Client, config jetstream.ConsumerConfig) (jetstream.JetStream, jetstream.Consumer) {
	t.Helper()
	manager, err := jetstream.New(client.conn)
	require.NoError(t, err)
	consumer, err := manager.CreateConsumer(ctx, testVoiceCommandStream, config)
	require.NoError(t, err, "the negative profile must still be accepted by the real broker")
	return manager, consumer
}

func retirementPublishFive(ctx context.Context, t *testing.T, client *Client, receipt voicecommand.TransportReceipt) []voicecommand.VoiceCommand {
	t.Helper()
	commands := make([]voicecommand.VoiceCommand, 5)
	for i := range commands {
		commands[i] = publisherCommand(t, receipt, uint64(i+1))
		ack, err := client.PublishVoiceCommand(ctx, receipt, commands[i])
		require.NoError(t, err)
		requirePublisherReceipt(t, ack, uint64(i+1), false)
	}
	return commands
}

func retirementAckThrough(ctx context.Context, t *testing.T, consumer jetstream.Consumer, first, through uint64) {
	t.Helper()
	for sequence := first; sequence <= through; sequence++ {
		message, err := consumer.Next(jetstream.FetchMaxWait(time.Second))
		require.NoError(t, err)
		metadata, err := message.Metadata()
		require.NoError(t, err)
		require.Equal(t, sequence, metadata.Sequence.Stream)
		require.NoError(t, message.DoubleAck(ctx), "retirement requires broker-confirmed acknowledgement")
	}
}

func requireRetirementSuffix(t *testing.T, js natssdk.JetStreamContext, receipt voicecommand.TransportReceipt, floor uint64, commands ...voicecommand.VoiceCommand) {
	t.Helper()
	info, err := js.StreamInfo(testVoiceCommandStream)
	require.NoError(t, err)
	require.NotEmpty(t, commands, "a retirement always preserves a published predecessor")
	assert.EqualValues(t, len(commands), info.State.Msgs)
	assert.Equal(t, floor+1, info.State.FirstSeq)
	assert.Equal(t, floor+uint64(len(commands)), info.State.LastSeq)
	assert.Zero(t, info.State.NumDeleted)
	assert.Empty(t, info.State.Deleted)
	for i, command := range commands {
		sequence := floor + uint64(i) + 1
		stored, getErr := js.GetMsg(testVoiceCommandStream, sequence)
		require.NoError(t, getErr)
		assert.Equal(t, sequence, stored.Sequence)
		assert.Equal(t, "voice.commands.v1", stored.Subject)
		assert.Equal(t, []string{command.CommandID}, stored.Header.Values(natssdk.MsgIdHdr))
		decoded, decodeErr := voicecommand.DecodeVoiceCommand(stored.Data, receipt.Generation)
		require.NoError(t, decodeErr)
		assert.Equal(t, command, decoded, "actual broker storage retains each exact obligation")
	}
}

func TestVoiceCommandRetirementRealJetStreamAcknowledgedPrefixPreservesHeadAndCAS(t *testing.T) {
	client, js, ctx, receipt := publisherCommandBroker(t)
	commands := retirementPublishFive(ctx, t, client, receipt)
	_, consumer := retirementConsumer(ctx, t, client, retirementConsumerConfig())
	retirementAckThrough(ctx, t, consumer, 1, 3)
	// Delivery alone must not authorize deletion. Leave sequence four unacked.
	unacked, err := consumer.Next(jetstream.FetchMaxWait(time.Second))
	require.NoError(t, err)
	metadata, err := unacked.Metadata()
	require.NoError(t, err)
	require.EqualValues(t, 4, metadata.Sequence.Stream)
	consumerBefore, err := consumer.Info(ctx)
	require.NoError(t, err)
	born, floor, err := client.VoiceCommandAckFloor(ctx, receipt)
	require.NoError(t, err)
	assert.True(t, born.Equal(consumerBefore.Created))
	require.EqualValues(t, 3, floor, "the floor is the stream ACK cursor, never the delivery cursor")
	streamBefore, err := js.StreamInfo(testVoiceCommandStream)
	require.NoError(t, err)
	require.NoError(t, client.RetireVoiceCommandPrefix(ctx, receipt, 0, floor))
	requireRetirementSuffix(t, js, receipt, 3, commands[3:]...)
	for sequence := uint64(1); sequence <= 3; sequence++ {
		_, getErr := js.GetMsg(testVoiceCommandStream, sequence)
		require.ErrorIs(t, getErr, natssdk.ErrMsgNotFound, "the acknowledged prefix really leaves broker storage")
	}
	head, err := client.LastVoiceCommandAfter(ctx, receipt, floor)
	require.NoError(t, err)
	require.NotNil(t, head)
	assert.EqualValues(t, 5, head.Sequence)
	decoded, err := voicecommand.DecodeVoiceCommand(head.Data, receipt.Generation)
	require.NoError(t, err)
	assert.Equal(t, commands[4], decoded)

	sixth := publisherCommand(t, receipt, 6)
	ack, err := client.PublishVoiceCommandAfter(ctx, receipt, sixth, floor)
	require.NoError(t, err)
	requirePublisherReceipt(t, ack, 6, false)
	requireRetirementSuffix(t, js, receipt, 3, commands[3], commands[4], sixth)
	stored, err := js.GetMsg(testVoiceCommandStream, 6)
	require.NoError(t, err)
	assert.Equal(t, "5", stored.Header.Get("Nats-Expected-Last-Sequence"))
	assert.Equal(t, "5", stored.Header.Get("Nats-Expected-Last-Subject-Sequence"))
	assert.Equal(t, testVoiceCommandStream, stored.Header.Get("Nats-Expected-Stream"))

	// A cache-miss stale request exercises the real broker's sequence fence.
	stale := commands[3]
	stale.CommandID = "20000000-0000-4000-8000-000000000099"
	refused, err := client.PublishVoiceCommandAfter(ctx, receipt, stale, floor)
	var conflict *natssdk.APIError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, natssdk.JSErrCodeStreamWrongLastSequence, conflict.ErrorCode)
	assert.Empty(t, refused)
	require.NoError(t, client.RetireVoiceCommandPrefix(ctx, receipt, 0, floor), "repeating the exact prepared interval is idempotent after later appends")
	requireRetirementSuffix(t, js, receipt, 3, commands[3], commands[4], sixth)
	streamAfter, err := js.StreamInfo(testVoiceCommandStream)
	require.NoError(t, err)
	assert.True(t, streamBefore.Created.Equal(streamAfter.Created))
	assert.Equal(t, streamBefore.Config, streamAfter.Config)
	consumerAfter, err := consumer.Info(ctx)
	require.NoError(t, err)
	assert.True(t, consumerBefore.Created.Equal(consumerAfter.Created))
	assert.Equal(t, consumerBefore.Config, consumerAfter.Config)
	requireVoiceCommandResources(t, js, receipt.Generation)
	requireBrokerIdentity(t, js, receipt)

	legacyHead, err := client.LastVoiceCommand(ctx, receipt)
	require.ErrorIs(t, err, voicecommand.ErrBrokerContinuity)
	assert.Nil(t, legacyHead)
	legacyAck, err := client.PublishVoiceCommand(ctx, receipt, publisherCommand(t, receipt, 7))
	require.ErrorIs(t, err, voicecommand.ErrBrokerContinuity)
	assert.Empty(t, legacyAck)
	requireRetirementSuffix(t, js, receipt, 3, commands[3], commands[4], sixth)
}

func TestVoiceCommandRetirementRealJetStreamConsumerRecreationUsesStreamACKSequence(t *testing.T) {
	client, js, ctx, receipt := publisherCommandBroker(t)
	commands := retirementPublishFive(ctx, t, client, receipt)
	manager, consumer := retirementConsumer(ctx, t, client, retirementConsumerConfig())
	retirementAckThrough(ctx, t, consumer, 1, 4)
	oldBorn, floor, err := client.VoiceCommandAckFloor(ctx, receipt)
	require.NoError(t, err)
	require.EqualValues(t, 4, floor)
	require.NoError(t, client.RetireVoiceCommandPrefix(ctx, receipt, 0, floor))
	require.NoError(t, manager.DeleteConsumer(ctx, testVoiceCommandStream, testRetirementDurable))
	missingBorn, missingFloor, err := client.VoiceCommandAckFloor(ctx, receipt)
	require.ErrorIs(t, err, jetstream.ErrConsumerNotFound)
	assert.True(t, missingBorn.IsZero())
	assert.Zero(t, missingFloor)
	require.NoError(t, client.RetireVoiceCommandPrefix(ctx, receipt, 0, floor), "completion of historic SQL proof does not require its consumer to reappear")
	requireRetirementSuffix(t, js, receipt, 4, commands[4])
	_, recreated := retirementConsumer(ctx, t, client, retirementConsumerConfig())
	retirementAckThrough(ctx, t, recreated, 5, 5)
	info, err := recreated.Info(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, info.AckFloor.Consumer)
	require.EqualValues(t, 5, info.AckFloor.Stream)
	newBorn, streamFloor, err := client.VoiceCommandAckFloor(ctx, receipt)
	require.NoError(t, err)
	assert.False(t, newBorn.Equal(oldBorn), "a recreated consumer supplies a fresh incarnation proof")
	assert.True(t, newBorn.Equal(info.Created))
	require.EqualValues(t, 5, streamFloor, "a new consumer's first delivery still acknowledges original global sequence five")
	sixth := publisherCommand(t, receipt, 6)
	ack, err := client.PublishVoiceCommandAfter(ctx, receipt, sixth, 4)
	require.NoError(t, err)
	requirePublisherReceipt(t, ack, 6, false)
	require.NoError(t, client.RetireVoiceCommandPrefix(ctx, receipt, 4, streamFloor))
	requireRetirementSuffix(t, js, receipt, 5, sixth)
	requireBrokerIdentity(t, js, receipt)
}

func TestVoiceCommandRetirementRealJetStreamMissingConsumerDoesNotCreateOne(t *testing.T) {
	client, js, ctx, receipt := publisherCommandBroker(t)
	born, floor, err := client.VoiceCommandAckFloor(ctx, receipt)
	require.ErrorIs(t, err, jetstream.ErrConsumerNotFound)
	assert.True(t, born.IsZero())
	assert.Zero(t, floor)
	info, err := js.StreamInfo(testVoiceCommandStream)
	require.NoError(t, err)
	assert.Zero(t, info.State.Consumers)
	// Initial consumer absence prevents retirement, but does not prevent queuing
	// a command in durable storage while the broker has capacity.
	first := publisherCommand(t, receipt, 1)
	ack, err := client.PublishVoiceCommandAfter(ctx, receipt, first, 0)
	require.NoError(t, err)
	requirePublisherReceipt(t, ack, 1, false)
	requirePublisherPrefix(t, js, first)
	_, consumer := retirementConsumer(ctx, t, client, retirementConsumerConfig())
	born, floor, err = client.VoiceCommandAckFloor(ctx, receipt)
	require.NoError(t, err)
	assert.False(t, born.IsZero())
	assert.Zero(t, floor)
	retirementAckThrough(ctx, t, consumer, 1, 1)
}

func TestVoiceCommandRetirementRealJetStreamRejectsForeignConsumerProfiles(t *testing.T) {
	cases := []struct {
		name   string
		change func(*jetstream.ConsumerConfig)
	}{
		{"parallel acknowledgements", func(c *jetstream.ConsumerConfig) { c.MaxAckPending = 2 }},
		{"different acknowledgement wait", func(c *jetstream.ConsumerConfig) { c.AckWait = time.Second }},
		{"bounded delivery attempts", func(c *jetstream.ConsumerConfig) { c.MaxDeliver = 3 }},
		{"latest only delivery", func(c *jetstream.ConsumerConfig) { c.DeliverPolicy = jetstream.DeliverLastPolicy }},
		{"unfiltered consumer", func(c *jetstream.ConsumerConfig) { c.FilterSubject = "" }},
		{"original replay timing", func(c *jetstream.ConsumerConfig) { c.ReplayPolicy = jetstream.ReplayOriginalPolicy }},
		{"header only delivery", func(c *jetstream.ConsumerConfig) { c.HeadersOnly = true }},
		{"expiring consumer", func(c *jetstream.ConsumerConfig) { c.InactiveThreshold = time.Hour }},
		{"memory consumer", func(c *jetstream.ConsumerConfig) { c.MemoryStorage = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, js, ctx, receipt := publisherCommandBroker(t)
			config := retirementConsumerConfig()
			tc.change(&config)
			_, consumer := retirementConsumer(ctx, t, client, config)
			before, err := consumer.Info(ctx)
			require.NoError(t, err)
			born, floor, err := client.VoiceCommandAckFloor(ctx, receipt)
			require.ErrorIs(t, err, voicecommand.ErrBrokerContinuity)
			assert.True(t, born.IsZero())
			assert.Zero(t, floor)
			after, err := consumer.Info(ctx)
			require.NoError(t, err)
			assert.Equal(t, before.Config, after.Config, "refusal does not repair a foreign consumer")
			assert.True(t, before.Created.Equal(after.Created))
			requirePublisherPrefix(t, js)
		})
	}
}

func TestVoiceCommandRetirementRealJetStreamRejectsPushConsumer(t *testing.T) {
	client, js, ctx, receipt := publisherCommandBroker(t)
	_, err := js.AddConsumer(testVoiceCommandStream, &natssdk.ConsumerConfig{
		Name: testRetirementDurable, Durable: testRetirementDurable,
		AckPolicy: natssdk.AckExplicitPolicy, DeliverPolicy: natssdk.DeliverAllPolicy,
		AckWait: 30 * time.Second, MaxAckPending: 1, MaxDeliver: -1,
		FilterSubject: "voice.commands.v1", DeliverSubject: "fixture.retirement.delivery",
	}, natssdk.Context(ctx))
	require.NoError(t, err)
	born, floor, err := client.VoiceCommandAckFloor(ctx, receipt)
	require.ErrorIs(t, err, voicecommand.ErrBrokerContinuity)
	assert.True(t, born.IsZero())
	assert.Zero(t, floor)
	requirePublisherPrefix(t, js)
}

func TestVoiceCommandRetirementRealJetStreamRefusesUnrecordedLoss(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *Client, natssdk.JetStreamContext, context.Context)
	}{
		{"unrecorded earlier prefix", func(t *testing.T, client *Client, _ natssdk.JetStreamContext, ctx context.Context) {
			manager, err := jetstream.New(client.conn)
			require.NoError(t, err)
			stream, err := manager.Stream(ctx, testVoiceCommandStream)
			require.NoError(t, err)
			require.NoError(t, stream.Purge(ctx, jetstream.WithPurgeSequence(2)))
		}},
		{"deleted internal command", func(t *testing.T, _ *Client, js natssdk.JetStreamContext, _ context.Context) {
			require.NoError(t, js.DeleteMsg(testVoiceCommandStream, 3))
		}},
		{"deleted retained head", func(t *testing.T, _ *Client, js natssdk.JetStreamContext, _ context.Context) {
			require.NoError(t, js.DeleteMsg(testVoiceCommandStream, 5))
		}},
		{"previously used empty stream", func(t *testing.T, _ *Client, js natssdk.JetStreamContext, _ context.Context) {
			require.NoError(t, js.PurgeStream(testVoiceCommandStream))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, js, ctx, receipt := publisherCommandBroker(t)
			retirementPublishFive(ctx, t, client, receipt)
			tc.change(t, client, js, ctx)
			before, err := js.StreamInfo(testVoiceCommandStream)
			require.NoError(t, err)
			require.NoError(t, client.Verify(ctx, receipt), "births and profile remain valid independently of retained history")
			require.ErrorIs(t, client.RetireVoiceCommandPrefix(ctx, receipt, 0, 2), voicecommand.ErrBrokerContinuity)
			head, err := client.LastVoiceCommandAfter(ctx, receipt, 2)
			require.ErrorIs(t, err, voicecommand.ErrBrokerContinuity)
			assert.Nil(t, head)
			ack, err := client.PublishVoiceCommandAfter(ctx, receipt, publisherCommand(t, receipt, 6), 2)
			require.ErrorIs(t, err, voicecommand.ErrBrokerContinuity)
			assert.Empty(t, ack)
			after, err := js.StreamInfo(testVoiceCommandStream)
			require.NoError(t, err)
			assert.Equal(t, before.State, after.State, "continuity refusal preserves all remaining evidence")
		})
	}
}

func TestVoiceCommandRetirementRealJetStreamInvalidBoundariesNeverRemoveHead(t *testing.T) {
	client, js, ctx, receipt := publisherCommandBroker(t)
	commands := retirementPublishFive(ctx, t, client, receipt)
	for _, boundary := range [][2]uint64{{0, 0}, {2, 2}, {3, 2}, {0, 5}, {0, 6}, {0, math.MaxInt64}, {math.MaxUint64, math.MaxUint64}} {
		t.Run(strconv.FormatUint(boundary[0], 10)+" through "+strconv.FormatUint(boundary[1], 10), func(t *testing.T) {
			require.ErrorIs(t, client.RetireVoiceCommandPrefix(ctx, receipt, boundary[0], boundary[1]), voicecommand.ErrBrokerContinuity)
			requirePublisherPrefix(t, js, commands...)
		})
	}
	head, err := client.LastVoiceCommandAfter(ctx, receipt, math.MaxUint64)
	require.ErrorIs(t, err, voicecommand.ErrBrokerContinuity)
	assert.Nil(t, head)
	requirePublisherPrefix(t, js, commands...)
}

func TestVoiceCommandRetirementRealJetStreamCancellationAndReceiptMismatchPreserveStorage(t *testing.T) {
	client, js, ctx, receipt := publisherCommandBroker(t)
	commands := retirementPublishFive(ctx, t, client, receipt)
	retirementConsumer(ctx, t, client, retirementConsumerConfig())
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	born, floor, err := client.VoiceCommandAckFloor(canceled, receipt)
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, born.IsZero())
	assert.Zero(t, floor)
	require.ErrorIs(t, client.RetireVoiceCommandPrefix(canceled, receipt, 0, 2), context.Canceled)
	head, err := client.LastVoiceCommandAfter(canceled, receipt, 0)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, head)
	ack, err := client.PublishVoiceCommandAfter(canceled, receipt, publisherCommand(t, receipt, 6), 0)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, ack)
	forged := receipt
	forged.Identity.StreamCreatedAt = forged.Identity.StreamCreatedAt.Add(time.Nanosecond)
	born, floor, err = client.VoiceCommandAckFloor(ctx, forged)
	require.ErrorIs(t, err, voicecommand.ErrBrokerContinuity)
	assert.True(t, born.IsZero())
	assert.Zero(t, floor)
	require.ErrorIs(t, client.RetireVoiceCommandPrefix(ctx, forged, 0, 2), voicecommand.ErrBrokerContinuity)
	head, err = client.LastVoiceCommandAfter(ctx, forged, 0)
	require.ErrorIs(t, err, voicecommand.ErrBrokerContinuity)
	assert.Nil(t, head)
	requirePublisherPrefix(t, js, commands...)
	requireBrokerIdentity(t, js, receipt)
}

func TestVoiceCommandRetirementDisconnectedClientsReturnWithoutBrokerWork(t *testing.T) {
	client, _, ctx, receipt := publisherCommandBroker(t)
	command := publisherCommand(t, receipt, 1)
	client.conn.Close()
	for _, disconnected := range []*Client{nil, {}, client} {
		born, floor, err := disconnected.VoiceCommandAckFloor(ctx, receipt)
		require.ErrorIs(t, err, natssdk.ErrInvalidConnection)
		assert.True(t, born.IsZero())
		assert.Zero(t, floor)
		require.ErrorIs(t, disconnected.RetireVoiceCommandPrefix(ctx, receipt, 0, 1), natssdk.ErrInvalidConnection)
		head, err := disconnected.LastVoiceCommandAfter(ctx, receipt, 0)
		require.ErrorIs(t, err, natssdk.ErrInvalidConnection)
		assert.Nil(t, head)
		ack, err := disconnected.PublishVoiceCommandAfter(ctx, receipt, command, 0)
		require.ErrorIs(t, err, natssdk.ErrInvalidConnection)
		assert.Empty(t, ack)
	}
}

// Incarnation changes and impossible accounting snapshots cannot be induced
// reliably between two real API reads. These controls pin their fail-closed
// interpretation without adding a timing race to the real-broker cases.
func retirementProofSnapshot() *jetstream.ConsumerInfo {
	return &jetstream.ConsumerInfo{
		Stream: testVoiceCommandStream, Name: testRetirementDurable,
		Created:       time.Date(2026, 10, 14, 12, 0, 0, 123456789, time.UTC),
		Config:        retirementConsumerConfig(),
		Delivered:     jetstream.SequenceInfo{Consumer: 4, Stream: 4},
		AckFloor:      jetstream.SequenceInfo{Consumer: 3, Stream: 3},
		NumAckPending: 1, NumPending: 6,
	}
}

func TestVoiceCommandRetirementProofRejectsUncertainOrImpossibleSnapshots(t *testing.T) {
	stream := &natssdk.StreamInfo{State: natssdk.StreamState{LastSeq: 10}}
	require.NoError(t, validateVoiceCommandAckProof(retirementProofSnapshot(), retirementProofSnapshot(), stream))
	cases := []struct {
		name   string
		change func(*jetstream.ConsumerInfo, *jetstream.ConsumerInfo, **natssdk.StreamInfo)
	}{
		{"consumer replaced", func(_, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) {
			after.Created = after.Created.Add(time.Nanosecond)
		}},
		{"config changed during proof", func(_, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) { after.Config.Description = "changed" }},
		{"stream ACK moved backwards", func(_, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) { after.AckFloor.Stream = 2 }},
		{"consumer ACK moved backwards", func(_, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) { after.AckFloor.Consumer = 2 }},
		{"ACK beyond delivery", func(before, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) {
			before.AckFloor.Stream, after.AckFloor.Stream = 6, 6
		}},
		{"delivery beyond stream", func(before, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) {
			before.Delivered.Stream, after.Delivered.Stream = 11, 11
		}},
		{"consumer ACK beyond delivery", func(before, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) {
			before.AckFloor.Consumer, after.AckFloor.Consumer = 6, 6
		}},
		{"consumer delivery overflow", func(before, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) {
			before.Delivered.Consumer, after.Delivered.Consumer = math.MaxUint64, math.MaxUint64
		}},
		{"negative pending ACKs", func(before, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) {
			before.NumAckPending, after.NumAckPending = -1, -1
		}},
		{"too many pending ACKs", func(before, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) {
			before.NumAckPending, after.NumAckPending = 2, 2
		}},
		{"negative redelivery count", func(before, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) {
			before.NumRedelivered, after.NumRedelivered = -1, -1
		}},
		{"too many redeliveries", func(before, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) {
			before.NumRedelivered, after.NumRedelivered = 2, 2
		}},
		{"negative waiting count", func(before, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) {
			before.NumWaiting, after.NumWaiting = -1, -1
		}},
		{"pending beyond stream", func(before, after *jetstream.ConsumerInfo, _ **natssdk.StreamInfo) {
			before.NumPending, after.NumPending = 11, 11
		}},
		{"missing stream", func(_, _ *jetstream.ConsumerInfo, stream **natssdk.StreamInfo) { *stream = nil }},
		{"stream sequence overflow", func(_, _ *jetstream.ConsumerInfo, stream **natssdk.StreamInfo) {
			(*stream).State.LastSeq = math.MaxUint64
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, after := retirementProofSnapshot(), retirementProofSnapshot()
			stream := &natssdk.StreamInfo{State: natssdk.StreamState{LastSeq: 10}}
			tc.change(before, after, &stream)
			require.ErrorIs(t, validateVoiceCommandAckProof(before, after, stream), ErrVoiceCommandContinuity)
		})
	}
	before, after := retirementProofSnapshot(), retirementProofSnapshot()
	after.AckFloor = after.Delivered
	require.NoError(t, validateVoiceCommandAckProof(before, after, stream), "ordinary ACK progress within one incarnation is valid")
}

func TestVoiceCommandRetirementConsumerProofRejectsNonFrozenFields(t *testing.T) {
	require.NoError(t, validateVoiceCommandConsumer(retirementProofSnapshot()))
	require.ErrorIs(t, validateVoiceCommandConsumer(nil), ErrVoiceCommandContinuity)
	cases := []struct {
		name   string
		change func(*jetstream.ConsumerInfo)
	}{
		{"missing birth", func(i *jetstream.ConsumerInfo) { i.Created = time.Time{} }},
		{"other stream", func(i *jetstream.ConsumerInfo) { i.Stream = "OTHER" }},
		{"other consumer", func(i *jetstream.ConsumerInfo) { i.Name = "other" }},
		{"paused", func(i *jetstream.ConsumerInfo) { i.Paused = true }},
		{"pause remaining", func(i *jetstream.ConsumerInfo) { i.PauseRemaining = time.Second }},
		{"different durable", func(i *jetstream.ConsumerInfo) { i.Config.Durable = "other" }},
		{"different explicit name", func(i *jetstream.ConsumerInfo) { i.Config.Name = "other" }},
		{"no ACKs", func(i *jetstream.ConsumerInfo) { i.Config.AckPolicy = jetstream.AckNonePolicy }},
		{"multiple filters", func(i *jetstream.ConsumerInfo) { i.Config.FilterSubjects = []string{"voice.commands.v1"} }},
		{"push subject", func(i *jetstream.ConsumerInfo) { i.Config.DeliverSubject = "delivery" }},
		{"push group", func(i *jetstream.ConsumerInfo) { i.Config.DeliverGroup = "group" }},
		{"delivery backoff", func(i *jetstream.ConsumerInfo) { i.Config.BackOff = []time.Duration{time.Second} }},
		{"flow control", func(i *jetstream.ConsumerInfo) { i.Config.FlowControl = true }},
		{"heartbeats", func(i *jetstream.ConsumerInfo) { i.Config.IdleHeartbeat = time.Second }},
		{"rate limit", func(i *jetstream.ConsumerInfo) { i.Config.RateLimit = 1 }},
		{"multiple replicas", func(i *jetstream.ConsumerInfo) { i.Config.Replicas = 2 }},
		{"starting sequence", func(i *jetstream.ConsumerInfo) { i.Config.OptStartSeq = 1 }},
		{"starting timestamp", func(i *jetstream.ConsumerInfo) { birth := i.Created; i.Config.OptStartTime = &birth }},
		{"negative waiting limit", func(i *jetstream.ConsumerInfo) { i.Config.MaxWaiting = -1 }},
		{"batch limit", func(i *jetstream.ConsumerInfo) { i.Config.MaxRequestBatch = 1 }},
		{"request expiry", func(i *jetstream.ConsumerInfo) { i.Config.MaxRequestExpires = time.Second }},
		{"request byte limit", func(i *jetstream.ConsumerInfo) { i.Config.MaxRequestMaxBytes = 1 }},
		{"future pause", func(i *jetstream.ConsumerInfo) { future := time.Now().Add(time.Hour); i.Config.PauseUntil = &future }},
		{"priority policy", func(i *jetstream.ConsumerInfo) { i.Config.PriorityPolicy = jetstream.PriorityPolicyPinned }},
		{"priority TTL", func(i *jetstream.ConsumerInfo) { i.Config.PinnedTTL = time.Second }},
		{"priority groups", func(i *jetstream.ConsumerInfo) { i.Config.PriorityGroups = []string{"group"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := retirementProofSnapshot()
			tc.change(info)
			require.ErrorIs(t, validateVoiceCommandConsumer(info), ErrVoiceCommandContinuity)
		})
	}
	info := retirementProofSnapshot()
	info.Config.Name = ""
	info.Config.MaxWaiting = 512
	info.Config.Replicas = 1
	past := info.Created
	info.Config.PauseUntil = &past
	require.NoError(t, validateVoiceCommandConsumer(info), "normal server defaults and an expired pause do not invent a profile mismatch")
}

func TestVoiceCommandRetirementHeadPreservationRejectsMissingChangedOrJumpedEvidence(t *testing.T) {
	before := &voicecommand.BrokerCommand{Sequence: 5, Data: []byte("exact-head")}
	assert.True(t, voiceCommandHeadPreserved(before, &voicecommand.BrokerCommand{Sequence: 5, Data: []byte("exact-head")}))
	assert.True(t, voiceCommandHeadPreserved(before, &voicecommand.BrokerCommand{Sequence: 6, Data: []byte("next-head")}))
	assert.False(t, voiceCommandHeadPreserved(nil, before))
	assert.False(t, voiceCommandHeadPreserved(before, nil))
	assert.False(t, voiceCommandHeadPreserved(before, &voicecommand.BrokerCommand{Sequence: 4, Data: []byte("exact-head")}))
	assert.False(t, voiceCommandHeadPreserved(before, &voicecommand.BrokerCommand{Sequence: 5, Data: []byte("changed-head")}))
	assert.False(t, voiceCommandHeadPreserved(before, &voicecommand.BrokerCommand{Sequence: 7, Data: []byte("jumped-head")}))
	assert.False(t, voiceCommandHeadPreserved(&voicecommand.BrokerCommand{Sequence: 0, Data: []byte("empty")}, &voicecommand.BrokerCommand{Sequence: 1, Data: []byte("next")}))
	assert.False(t, voiceCommandHeadPreserved(before, &voicecommand.BrokerCommand{Sequence: math.MaxUint64, Data: []byte("overflow")}))
	assert.False(t, voiceCommandHeadPreserved(&voicecommand.BrokerCommand{Sequence: math.MaxInt64, Data: []byte("limit")}, &voicecommand.BrokerCommand{Sequence: math.MaxInt64 + 1, Data: []byte("overflow")}))
}
