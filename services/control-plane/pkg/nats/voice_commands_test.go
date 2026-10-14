package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	natssdk "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
)

const testVoiceCommandStream = "VOICE_COMMANDS_V1"
const testVoiceCommandBucket = "VOICE_COMMAND_OWNER_V1"
const testVoiceCommandOwnerKey = "authoritative"

type testCommandOwner struct {
	Version          int               `json:"version"`
	StreamGeneration string            `json:"streamGeneration"`
	Phase            string            `json:"phase"`
	NodeBootID       *string           `json:"nodeBootId"`
	Birth            *testCommandBirth `json:"birth,omitempty"`
	Proof            *testCommandProof `json:"proof,omitempty"`
}

type testCommandBirth struct {
	StreamCreatedAt     string `json:"streamCreatedAt"`
	OwnerStoreCreatedAt string `json:"ownerStoreCreatedAt"`
}

type testCommandProof struct {
	StreamCreatedAt      string `json:"streamCreatedAt"`
	OwnerStoreCreatedAt  string `json:"ownerStoreCreatedAt"`
	InitializingRevision string `json:"initializingRevision"`
}

// Each test owns an actual server, storage directory and connection. Fixed
// production resource names therefore cannot touch another test or live media.
// A missing binary or broken broker is a failure; this suite never skips.
func voiceCommandBroker(t *testing.T) (*Client, natssdk.JetStreamContext, context.Context) {
	t.Helper()
	acquisitionCtx, cancelAcquisition := context.WithTimeout(t.Context(), 115*time.Second)
	resolver := exec.CommandContext(acquisitionCtx, "bash", "../../../../scripts/test-tools/nats-server.sh")
	// Allow the owned resolver's cleanup traps to run before forced termination.
	resolver.Cancel = func() error { return resolver.Process.Signal(syscall.SIGTERM) }
	resolver.WaitDelay = 5 * time.Second
	var resolverStderr bytes.Buffer
	resolver.Stderr = &resolverStderr
	resolved, err := resolver.Output()
	cancelAcquisition()
	require.NoError(t, err, "resolve real NATS test server: %s", strings.TrimSpace(resolverStderr.String()))
	binary := strings.TrimSpace(string(resolved))
	require.NotEmpty(t, binary, "NATS test server resolver must return an executable path")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := address.Port
	require.NoError(t, listener.Close())
	args := []string{"-js", "-sd", t.TempDir(), "-a", "127.0.0.1", "-p", strconv.Itoa(port)}
	// #nosec G204 G702 -- The executable is trusted test-harness configuration, with no production or wire input; arguments are fixture-owned and no shell is invoked.
	command := exec.Command(binary, args...)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	require.NoError(t, command.Start(), "real JetStream tests require an executable nats-server on PATH or NATS_SERVER_BINARY")
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	t.Cleanup(func() {
		if signalErr := command.Process.Signal(os.Interrupt); signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
			t.Errorf("stop isolated NATS server: %v", signalErr)
		}
		select {
		case waitErr := <-finished:
			assert.NoError(t, waitErr)
		case <-time.After(3 * time.Second):
			assert.NoError(t, command.Process.Kill())
			<-finished
			t.Error("isolated NATS server did not stop within its cleanup deadline")
		}
	})
	var connection *natssdk.Conn
	require.Eventually(t, func() bool {
		connection, err = natssdk.Connect("nats://127.0.0.1:"+strconv.Itoa(port),
			natssdk.Timeout(100*time.Millisecond), natssdk.NoReconnect())
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "isolated JetStream broker must start")
	t.Cleanup(connection.Close)
	js, err := connection.JetStream(natssdk.MaxWait(time.Second))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return &Client{conn: connection}, js, ctx
}

func readCommandOwner(t *testing.T, js natssdk.JetStreamContext) (testCommandOwner, uint64) {
	t.Helper()
	kv, err := js.KeyValue(testVoiceCommandBucket)
	require.NoError(t, err)
	entry, err := kv.Get(testVoiceCommandOwnerKey)
	require.NoError(t, err)
	var owner testCommandOwner
	require.NoError(t, json.Unmarshal(entry.Value(), &owner))
	return owner, entry.Revision()
}

func requireVoiceCommandResources(t *testing.T, js natssdk.JetStreamContext, generation uuid.UUID) {
	t.Helper()
	stream, err := js.StreamInfo(testVoiceCommandStream)
	require.NoError(t, err)
	assert.Equal(t, []string{"voice.commands.v1"}, stream.Config.Subjects)
	assert.Equal(t, natssdk.FileStorage, stream.Config.Storage)
	assert.Equal(t, natssdk.LimitsPolicy, stream.Config.Retention)
	assert.Equal(t, natssdk.DiscardNew, stream.Config.Discard)
	assert.Equal(t, 1, stream.Config.Replicas)
	assert.Zero(t, stream.Config.MaxAge)
	assert.EqualValues(t, 67108864, stream.Config.MaxBytes)
	assert.EqualValues(t, 50000, stream.Config.MaxMsgs)
	assert.EqualValues(t, 4096, stream.Config.MaxMsgSize)
	assert.Equal(t, 120*time.Second, stream.Config.Duplicates)
	assert.Contains(t, stream.Config.Description, generation.String())
	kvStream, err := js.StreamInfo("KV_" + testVoiceCommandBucket)
	require.NoError(t, err)
	assert.Equal(t, natssdk.FileStorage, kvStream.Config.Storage)
	assert.Equal(t, 1, kvStream.Config.Replicas)
	assert.EqualValues(t, 1, kvStream.Config.MaxMsgsPerSubject)
	assert.Zero(t, kvStream.Config.MaxAge, "permanent held ownership must not expire")
	owner, _ := readCommandOwner(t, js)
	assert.Equal(t, 1, owner.Version)
	assert.Equal(t, generation.String(), owner.StreamGeneration)
}

func requireBrokerIdentity(t *testing.T, js natssdk.JetStreamContext, receipt voicecommand.TransportReceipt) {
	t.Helper()
	stream, err := js.StreamInfo(testVoiceCommandStream)
	require.NoError(t, err)
	bucket, err := js.StreamInfo("KV_" + testVoiceCommandBucket)
	require.NoError(t, err)
	assert.True(t, stream.Created.Equal(receipt.Identity.StreamCreatedAt), "receipt identifies the actual command stream birth")
	assert.True(t, bucket.Created.Equal(receipt.Identity.OwnerStoreCreatedAt), "receipt identifies the actual owner store birth")
	owner, _ := readCommandOwner(t, js)
	streamCreated := stream.Created.UTC().Format(time.RFC3339Nano)
	ownerCreated := bucket.Created.UTC().Format(time.RFC3339Nano)
	if owner.Phase == "initializing" {
		require.NotNil(t, owner.Birth)
		assert.Nil(t, owner.Proof)
		assert.Equal(t, streamCreated, owner.Birth.StreamCreatedAt)
		assert.Equal(t, ownerCreated, owner.Birth.OwnerStoreCreatedAt)
	} else {
		require.NotNil(t, owner.Proof)
		assert.Nil(t, owner.Birth)
		assert.Equal(t, streamCreated, owner.Proof.StreamCreatedAt)
		assert.Equal(t, ownerCreated, owner.Proof.OwnerStoreCreatedAt)
		assert.Equal(t, strconv.FormatUint(receipt.Identity.InitializingRevision, 10), owner.Proof.InitializingRevision)
	}
}

func holdCommandOwner(t *testing.T, js natssdk.JetStreamContext, boot string) (testCommandOwner, uint64) {
	t.Helper()
	owner, revision := readCommandOwner(t, js)
	require.Equal(t, "released", owner.Phase)
	require.NotNil(t, owner.Proof, "the held transition preserves confirmed initialization proof")
	owner.Phase, owner.NodeBootID = "held", &boot
	encoded, err := json.Marshal(owner)
	require.NoError(t, err)
	kv, err := js.KeyValue(testVoiceCommandBucket)
	require.NoError(t, err)
	heldRevision, err := kv.Update(testVoiceCommandOwnerKey, encoded, revision)
	require.NoError(t, err)
	return owner, heldRevision
}

func TestValidateVoiceCommandStreamFrozenDefaults(t *testing.T) {
	generation := uuid.New()
	resources := []struct {
		name        string
		ownerBucket bool
		config      func(uuid.UUID) natssdk.StreamConfig
	}{
		{name: "command stream", config: voiceCommandStreamConfig},
		{name: "owner backing stream", ownerBucket: true, config: voiceCommandOwnerStreamConfig},
	}
	changes := []struct {
		name  string
		apply func(*natssdk.StreamConfig)
	}{
		{name: "storage compression", apply: func(config *natssdk.StreamConfig) {
			config.Compression = natssdk.S2Compression
		}},
		{name: "placement", apply: func(config *natssdk.StreamConfig) {
			config.Placement = &natssdk.Placement{Cluster: "fixture-cluster"}
		}},
		{name: "empty placement", apply: func(config *natssdk.StreamConfig) {
			config.Placement = &natssdk.Placement{}
		}},
		{name: "template owner", apply: func(config *natssdk.StreamConfig) {
			config.Template = "fixture-template"
		}},
		{name: "initial sequence", apply: func(config *natssdk.StreamConfig) {
			config.FirstSeq = 2
		}},
	}
	for _, resource := range resources {
		t.Run(resource.name, func(t *testing.T) {
			original := natssdk.StreamInfo{
				Config:  resource.config(generation),
				Created: time.Date(2026, time.October, 13, 0, 0, 0, 1, time.UTC),
			}
			require.Equal(t, natssdk.NoCompression, original.Config.Compression)
			require.Nil(t, original.Config.Placement)
			require.Empty(t, original.Config.Template)
			require.NoError(t, validateVoiceCommandStream(&original, generation, resource.ownerBucket),
				"the generated resource with frozen defaults is valid")
			for _, change := range changes {
				t.Run(change.name, func(t *testing.T) {
					modified := original
					change.apply(&modified.Config)
					require.ErrorIs(t, validateVoiceCommandStream(&modified, generation, resource.ownerBucket),
						ErrVoiceCommandContinuity, "changing a frozen default must refuse the resource")
				})
			}
		})
	}
}

func TestVoiceCommandAsyncPersistenceInitializationRealJetStream(t *testing.T) {
	for _, ownerBucket := range []bool{false, true} {
		name := testVoiceCommandStream
		if ownerBucket {
			name = "KV_" + testVoiceCommandBucket
		}
		t.Run(name, func(t *testing.T) {
			client, js, ctx := voiceCommandBroker(t)
			generation := uuid.New()
			config := voiceCommandStreamConfig(generation)
			if ownerBucket {
				_, err := js.AddStream(&config)
				require.NoError(t, err)
				config = voiceCommandOwnerStreamConfig(generation)
			}
			encoded, err := json.Marshal(config)
			require.NoError(t, err)
			var modernConfig jetstream.StreamConfig
			require.NoError(t, json.Unmarshal(encoded, &modernConfig))
			require.Equal(t, jetstream.DefaultPersistMode, modernConfig.PersistMode)
			modernConfig.PersistMode = jetstream.AsyncPersistMode
			manager, err := jetstream.New(client.conn)
			require.NoError(t, err)
			_, err = manager.CreateStream(ctx, modernConfig)
			require.NoError(t, err, "persistence mode is selected at ordinary stream creation")
			stream, err := manager.Stream(ctx, name)
			require.NoError(t, err)
			original := stream.CachedInfo()
			require.NotNil(t, original)
			require.False(t, original.Created.IsZero())
			require.Equal(t, jetstream.AsyncPersistMode, original.Config.PersistMode, "broker reports async persistence")
			_, err = client.Initialize(ctx, generation)
			assert.ErrorIs(t, err, ErrVoiceCommandContinuity, "initialization must refuse an async resource")
			stream, err = manager.Stream(ctx, name)
			require.NoError(t, err)
			assert.True(t, original.Created.Equal(stream.CachedInfo().Created))
			assert.Equal(t, jetstream.AsyncPersistMode, stream.CachedInfo().Config.PersistMode, "refusal cannot repair existing resources")
			kv, err := js.KeyValue(testVoiceCommandBucket)
			if ownerBucket {
				require.NoError(t, err)
				_, err = kv.Get(testVoiceCommandOwnerKey)
				assert.ErrorIs(t, err, natssdk.ErrKeyNotFound, "refusal must precede initializing authority creation")
			} else {
				assert.ErrorIs(t, err, natssdk.ErrBucketNotFound, "refusal must precede owner bucket creation")
			}
		})
	}
}

func TestVoiceCommandModernDefaultsRealJetStream(t *testing.T) {
	changes := []struct {
		name            string
		apply           func(*jetstream.StreamConfig)
		observed        func(jetstream.StreamConfig) any
		want            any
		brokerRejection string
	}{
		{name: "message counters", want: true, brokerRejection: "counter stream cannot use discard new",
			apply:    func(config *jetstream.StreamConfig) { config.AllowMsgCounter = true },
			observed: func(config jetstream.StreamConfig) any { return config.AllowMsgCounter }},
		{name: "message schedules", want: true, brokerRejection: "message scheduling cannot use discard new",
			apply:    func(config *jetstream.StreamConfig) { config.AllowMsgSchedules = true },
			observed: func(config jetstream.StreamConfig) any { return config.AllowMsgSchedules }},
		{name: "atomic publishing", want: true,
			apply:    func(config *jetstream.StreamConfig) { config.AllowAtomicPublish = true },
			observed: func(config jetstream.StreamConfig) any { return config.AllowAtomicPublish }},
		{name: "batch publishing", want: true,
			apply:    func(config *jetstream.StreamConfig) { config.AllowBatchPublish = true },
			observed: func(config jetstream.StreamConfig) any { return config.AllowBatchPublish }},
	}
	for _, resource := range []string{testVoiceCommandStream, "KV_" + testVoiceCommandBucket} {
		t.Run(resource, func(t *testing.T) {
			for _, change := range changes {
				t.Run(change.name, func(t *testing.T) {
					for _, phase := range []string{"initializing", "held"} {
						t.Run(phase, func(t *testing.T) {
							client, js, ctx := voiceCommandBroker(t)
							generation := uuid.New()
							identity, err := client.Initialize(ctx, generation)
							require.NoError(t, err)
							receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
							require.NoError(t, client.Verify(ctx, receipt), "healthy resource control")
							boot := uuid.New()
							if phase == "held" {
								require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
								_, _ = holdCommandOwner(t, js, boot.String())
							}
							owner, revision := readCommandOwner(t, js)
							manager, err := jetstream.New(client.conn)
							require.NoError(t, err)
							stream, err := manager.Stream(ctx, resource)
							require.NoError(t, err)
							original := stream.CachedInfo()
							require.NotNil(t, original)
							require.False(t, original.Created.IsZero())
							require.Equal(t, jetstream.DefaultPersistMode, original.Config.PersistMode)
							require.False(t, original.Config.AllowMsgCounter)
							require.False(t, original.Config.AllowMsgSchedules)
							require.False(t, original.Config.AllowAtomicPublish)
							require.False(t, original.Config.AllowBatchPublish)
							modified := original.Config
							change.apply(&modified)
							_, err = manager.UpdateStream(ctx, modified)
							if change.brokerRejection != "" {
								require.ErrorContains(t, err, change.brokerRejection)
								stream, err = manager.Stream(ctx, resource)
								require.NoError(t, err)
								assert.Equal(t, original.Config, stream.CachedInfo().Config, "broker rejection retains all frozen defaults")
								require.NoError(t, client.Verify(ctx, receipt), "rejected update leaves a healthy resource")
								retained, retainedRevision := readCommandOwner(t, js)
								assert.Equal(t, owner, retained)
								assert.Equal(t, revision, retainedRevision)
								return
							}
							require.NoError(t, err, "broker must accept the ordinary configuration update")
							stream, err = manager.Stream(ctx, resource)
							require.NoError(t, err)
							actual := stream.CachedInfo()
							require.NotNil(t, actual)
							require.Equal(t, change.want, change.observed(actual.Config), "broker must report the changed flag")
							require.True(t, original.Created.Equal(actual.Created), "configuration update preserves resource birth")
							assert.ErrorIs(t, client.Verify(ctx, receipt), ErrVoiceCommandContinuity)
							if phase == "initializing" {
								_, err = client.Initialize(ctx, generation)
								assert.ErrorIs(t, err, ErrVoiceCommandContinuity)
								assert.ErrorIs(t, client.ReleaseInitialOwner(ctx, receipt), ErrVoiceCommandContinuity)
							} else {
								assert.ErrorIs(t, client.RecoverVoiceCommandOwner(ctx, revision, boot, receipt), ErrVoiceCommandContinuity)
							}
							retained, retainedRevision := readCommandOwner(t, js)
							assert.Equal(t, owner, retained, "configuration refusal preserves owner and permanent proof")
							assert.Equal(t, revision, retainedRevision, "configuration refusal cannot mutate authority")
						})
					}
				})
			}
		})
	}
}

func TestValidateVoiceCommandStreamFeaturesRealJetStream(t *testing.T) {
	cases := []struct {
		name      string
		apply     func(*jetstream.StreamConfig)
		observed  func(jetstream.StreamConfig) any
		want      any
		wantError bool
	}{
		{name: "defaults", want: jetstream.DefaultPersistMode,
			apply:    func(*jetstream.StreamConfig) {},
			observed: func(config jetstream.StreamConfig) any { return config.PersistMode }},
		{name: "initial sequence", want: uint64(2), wantError: true,
			apply:    func(config *jetstream.StreamConfig) { config.FirstSeq = 2 },
			observed: func(config jetstream.StreamConfig) any { return config.FirstSeq }},
		{name: "message counters", want: true, wantError: true,
			apply: func(config *jetstream.StreamConfig) {
				config.Discard = jetstream.DiscardOld // Required by the broker for counter streams.
				config.AllowMsgCounter = true
			},
			observed: func(config jetstream.StreamConfig) any { return config.AllowMsgCounter }},
		{name: "message schedules", want: true, wantError: true,
			apply: func(config *jetstream.StreamConfig) {
				config.Discard = jetstream.DiscardOld // Required by the broker for scheduling streams.
				config.AllowMsgSchedules = true
			},
			observed: func(config jetstream.StreamConfig) any { return config.AllowMsgSchedules }},
	}
	for _, ownerBucket := range []bool{false, true} {
		name := testVoiceCommandStream
		if ownerBucket {
			name = "KV_" + testVoiceCommandBucket
		}
		t.Run(name, func(t *testing.T) {
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					client, js, ctx := voiceCommandBroker(t)
					config := voiceCommandStreamConfig(uuid.New())
					if ownerBucket {
						config = voiceCommandOwnerStreamConfig(uuid.New())
					}
					encoded, err := json.Marshal(config)
					require.NoError(t, err)
					var modernConfig jetstream.StreamConfig
					require.NoError(t, json.Unmarshal(encoded, &modernConfig))
					test.apply(&modernConfig)
					manager, err := jetstream.New(client.conn)
					require.NoError(t, err)
					_, err = manager.CreateStream(ctx, modernConfig)
					require.NoError(t, err, "broker must support the ordinary feature configuration")
					stream, err := manager.Stream(ctx, name)
					require.NoError(t, err)
					before := stream.CachedInfo()
					require.NotNil(t, before)
					require.Equal(t, test.want, test.observed(before.Config), "broker reports the configured feature")
					classic, err := js.StreamInfo(name)
					require.NoError(t, err)
					require.True(t, classic.Created.Equal(before.Created))
					err = validateVoiceCommandStreamFeatures(ctx, manager, classic)
					if test.wantError {
						assert.ErrorIs(t, err, ErrVoiceCommandContinuity)
					} else {
						assert.NoError(t, err, "healthy generated resource is accepted")
					}
					stream, err = manager.Stream(ctx, name)
					require.NoError(t, err)
					assert.Equal(t, before.Config, stream.CachedInfo().Config, "feature validation is read only")
					assert.True(t, before.Created.Equal(stream.CachedInfo().Created))
				})
			}
		})
	}
}

func TestValidateVoiceCommandStreamFeaturesResourceBirthRealJetStream(t *testing.T) {
	for _, resource := range []string{testVoiceCommandStream, "KV_" + testVoiceCommandBucket} {
		t.Run(resource, func(t *testing.T) {
			client, js, ctx := voiceCommandBroker(t)
			_, err := client.Initialize(ctx, uuid.New())
			require.NoError(t, err)
			manager, err := jetstream.New(client.conn)
			require.NoError(t, err)
			original, err := js.StreamInfo(resource)
			require.NoError(t, err)
			alternateZone := *original
			alternateZone.Created = original.Created.In(time.FixedZone("fixture", 3*60*60))
			require.True(t, original.Created.Equal(alternateZone.Created))
			require.NotEqual(t, original.Created.Location(), alternateZone.Created.Location())
			require.NoError(t, validateVoiceCommandStreamFeatures(ctx, manager, &alternateZone),
				"the same birth instant in another time zone is accepted")

			encoded, err := json.Marshal(original.Config)
			require.NoError(t, err)
			var config jetstream.StreamConfig
			require.NoError(t, json.Unmarshal(encoded, &config))
			require.NoError(t, manager.DeleteStream(ctx, resource))
			_, err = manager.CreateStream(ctx, config)
			require.NoError(t, err)
			stream, err := manager.Stream(ctx, resource)
			require.NoError(t, err)
			recreated := stream.CachedInfo()
			require.NotNil(t, recreated)
			require.False(t, original.Created.Equal(recreated.Created), "fixture has a genuinely new resource birth")
			require.ErrorIs(t, validateVoiceCommandStreamFeatures(ctx, manager, original), ErrVoiceCommandContinuity,
				"classic and modern reads must identify the same resource incarnation")
			fresh, err := js.StreamInfo(resource)
			require.NoError(t, err)
			require.NoError(t, validateVoiceCommandStreamFeatures(ctx, manager, fresh), "a fresh matching pair is valid")
			stream, err = manager.Stream(ctx, resource)
			require.NoError(t, err)
			assert.Equal(t, recreated.Config, stream.CachedInfo().Config, "birth validation is read only")
			assert.True(t, recreated.Created.Equal(stream.CachedInfo().Created))
		})
	}
}

func TestVoiceCommandProvisioningRealJetStream(t *testing.T) {
	t.Run("initialization binds exact resources and retains initializing owner", func(t *testing.T) {
		client, js, ctx := voiceCommandBroker(t)
		generation := uuid.New()
		identity, err := client.Initialize(ctx, generation)
		require.NoError(t, err)
		receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
		require.Positive(t, identity.InitializingRevision)
		requireVoiceCommandResources(t, js, generation)
		requireBrokerIdentity(t, js, receipt)
		owner, storedRevision := readCommandOwner(t, js)
		assert.Equal(t, identity.InitializingRevision, storedRevision)
		assert.Equal(t, "initializing", owner.Phase)
		assert.Nil(t, owner.NodeBootID)
		require.NoError(t, client.Verify(ctx, receipt))
		again, err := client.Initialize(ctx, generation)
		require.NoError(t, err)
		assert.Equal(t, identity, again, "resuming initialization retains both resource births and original revision")
		require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
		owner, _ = readCommandOwner(t, js)
		assert.Equal(t, "released", owner.Phase)
		assert.Nil(t, owner.NodeBootID)
		requireBrokerIdentity(t, js, receipt)
	})

	for _, stage := range []string{"before bucket creation", "before owner creation"} {
		t.Run("resume "+stage, func(t *testing.T) {
			client, js, ctx := voiceCommandBroker(t)
			generation := uuid.New()
			// Build a genuine interrupted creation: deleting an existing owner would
			// instead be continuity loss and is covered by the regression below.
			config := voiceCommandStreamConfig(generation)
			_, err := js.AddStream(&config)
			require.NoError(t, err)
			if stage == "before owner creation" {
				config = voiceCommandOwnerStreamConfig(generation)
				_, err = js.AddStream(&config)
				require.NoError(t, err)
			}
			identity, err := client.Initialize(ctx, generation)
			require.NoError(t, err)
			receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
			requireVoiceCommandResources(t, js, generation)
			requireBrokerIdentity(t, js, receipt)
			owner, _ := readCommandOwner(t, js)
			assert.Equal(t, "initializing", owner.Phase)
		})
	}

	t.Run("duplicate provisioners converge without replacing generation", func(t *testing.T) {
		client, js, ctx := voiceCommandBroker(t)
		generation := uuid.New()
		const callers = 6
		type initialization struct {
			identity voicecommand.TransportIdentity
			err      error
		}
		results := make(chan initialization, callers)
		start := make(chan struct{})
		for range callers {
			go func() {
				<-start
				identity, err := client.Initialize(ctx, generation)
				results <- initialization{identity: identity, err: err}
			}()
		}
		close(start)
		var first voicecommand.TransportIdentity
		for range callers {
			result := <-results
			assert.NoError(t, result.err)
			if first.InitializingRevision == 0 {
				first = result.identity
			}
			assert.Equal(t, first, result.identity)
		}
		requireVoiceCommandResources(t, js, generation)
		requireBrokerIdentity(t, js, voicecommand.TransportReceipt{Generation: generation, Identity: first})
		_, revision := readCommandOwner(t, js)
		assert.Equal(t, first.InitializingRevision, revision)
	})

	t.Run("a wrong generation cannot validate or rewrite resources", func(t *testing.T) {
		client, js, ctx := voiceCommandBroker(t)
		generation := uuid.New()
		identity, err := client.Initialize(ctx, generation)
		require.NoError(t, err)
		owner, revision := readCommandOwner(t, js)
		require.Error(t, client.Verify(ctx, voicecommand.TransportReceipt{Generation: uuid.New(), Identity: identity}))
		_, err = client.Initialize(ctx, uuid.New())
		require.Error(t, err)
		unchanged, unchangedRevision := readCommandOwner(t, js)
		assert.Equal(t, owner, unchanged)
		assert.Equal(t, revision, unchangedRevision)
		requireVoiceCommandResources(t, js, generation)
	})

	t.Run("incompatible stream configuration is rejected without repair", func(t *testing.T) {
		client, js, ctx := voiceCommandBroker(t)
		generation := uuid.New()
		identity, err := client.Initialize(ctx, generation)
		require.NoError(t, err)
		receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
		stream, err := js.StreamInfo(testVoiceCommandStream)
		require.NoError(t, err)
		stream.Config.MaxMsgs = 25000
		_, err = js.UpdateStream(&stream.Config)
		require.NoError(t, err)
		require.Error(t, client.Verify(ctx, receipt))
		_, err = client.Initialize(ctx, generation)
		require.Error(t, err)
		stream, err = js.StreamInfo(testVoiceCommandStream)
		require.NoError(t, err)
		assert.EqualValues(t, 25000, stream.Config.MaxMsgs)
	})
}

func TestVoiceCommandVerifyContinuityLossRealJetStream(t *testing.T) {
	for _, resource := range []string{"stream", "bucket", "owner key"} {
		t.Run(resource+" loss is never recreated by verification", func(t *testing.T) {
			client, js, ctx := voiceCommandBroker(t)
			generation := uuid.New()
			identity, err := client.Initialize(ctx, generation)
			require.NoError(t, err)
			receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
			require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
			boot := uuid.New().String()
			held, heldRevision := holdCommandOwner(t, js, boot)
			kv, err := js.KeyValue(testVoiceCommandBucket)
			require.NoError(t, err)
			switch resource {
			case "stream":
				require.NoError(t, js.DeleteStream(testVoiceCommandStream))
			case "bucket":
				require.NoError(t, js.DeleteKeyValue(testVoiceCommandBucket))
			case "owner key":
				require.NoError(t, kv.Delete(testVoiceCommandOwnerKey))
			}
			require.Error(t, client.Verify(ctx, receipt))
			require.Error(t, client.ReleaseInitialOwner(ctx, receipt))
			switch resource {
			case "stream":
				_, err = js.StreamInfo(testVoiceCommandStream)
				require.ErrorIs(t, err, natssdk.ErrStreamNotFound)
				retained, retainedRevision := readCommandOwner(t, js)
				assert.Equal(t, held, retained)
				assert.Equal(t, heldRevision, retainedRevision)
			case "bucket":
				_, err = js.KeyValue(testVoiceCommandBucket)
				require.ErrorIs(t, err, natssdk.ErrBucketNotFound)
			case "owner key":
				_, err = kv.Get(testVoiceCommandOwnerKey)
				require.ErrorIs(t, err, natssdk.ErrKeyNotFound)
			}
		})
	}
}

func TestVoiceCommandReleaseInitialOwnerRealJetStream(t *testing.T) {
	t.Run("release uses the exact frozen initializing revision", func(t *testing.T) {
		client, js, ctx := voiceCommandBroker(t)
		generation := uuid.New()
		identity, err := client.Initialize(ctx, generation)
		require.NoError(t, err)
		receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
		for _, revision := range []uint64{0, identity.InitializingRevision + 1} {
			invalid := receipt
			invalid.Identity.InitializingRevision = revision
			require.Error(t, client.ReleaseInitialOwner(ctx, invalid))
		}
		owner, storedRevision := readCommandOwner(t, js)
		assert.Equal(t, "initializing", owner.Phase)
		assert.Equal(t, identity.InitializingRevision, storedRevision)
		require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
		owner, releasedRevision := readCommandOwner(t, js)
		assert.Equal(t, "released", owner.Phase)
		requireBrokerIdentity(t, js, receipt)
		require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
		_, duplicateRevision := readCommandOwner(t, js)
		assert.Equal(t, releasedRevision, duplicateRevision, "duplicate bootstrap cannot rewrite a released record")
	})

	t.Run("provisioned restart preserves held owner and immutable proof", func(t *testing.T) {
		client, js, ctx := voiceCommandBroker(t)
		generation := uuid.New()
		identity, err := client.Initialize(ctx, generation)
		require.NoError(t, err)
		receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
		require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
		held, heldRevision := holdCommandOwner(t, js, uuid.New().String())
		require.NoError(t, client.Verify(ctx, receipt))
		require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
		retained, retainedRevision := readCommandOwner(t, js)
		assert.Equal(t, held, retained)
		assert.Equal(t, heldRevision, retainedRevision)
		requireBrokerIdentity(t, js, receipt)
	})
}

func TestVoiceCommandReceiptRejectsRecreatedResourcesRealJetStream(t *testing.T) {
	for _, resource := range []string{"stream", "bucket"} {
		t.Run("same configuration and generation cannot mask recreated "+resource, func(t *testing.T) {
			client, js, ctx := voiceCommandBroker(t)
			generation := uuid.New()
			identity, err := client.Initialize(ctx, generation)
			require.NoError(t, err)
			receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
			require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
			held, heldRevision := holdCommandOwner(t, js, uuid.New().String())
			if resource == "stream" {
				require.NoError(t, js.DeleteStream(testVoiceCommandStream))
				config := voiceCommandStreamConfig(generation)
				recreated, createErr := js.AddStream(&config)
				require.NoError(t, createErr)
				require.False(t, recreated.Created.Equal(identity.StreamCreatedAt), "fixture must recreate the stream with a different birth")
			} else {
				require.NoError(t, js.DeleteKeyValue(testVoiceCommandBucket))
				config := voiceCommandOwnerStreamConfig(generation)
				recreated, createErr := js.AddStream(&config)
				require.NoError(t, createErr)
				require.False(t, recreated.Created.Equal(identity.OwnerStoreCreatedAt), "fixture must recreate the bucket with a different birth")
				replacement := testCommandOwner{
					Version: 1, StreamGeneration: generation.String(), Phase: "initializing",
					Birth: &testCommandBirth{StreamCreatedAt: identity.StreamCreatedAt.UTC().Format(time.RFC3339Nano), OwnerStoreCreatedAt: recreated.Created.UTC().Format(time.RFC3339Nano)},
				}
				encoded, encodeErr := json.Marshal(replacement)
				require.NoError(t, encodeErr)
				kv, kvErr := js.KeyValue(testVoiceCommandBucket)
				require.NoError(t, kvErr)
				replacementRevision, createErr := kv.Create(testVoiceCommandOwnerKey, encoded)
				require.NoError(t, createErr)
				assert.Equal(t, identity.InitializingRevision, replacementRevision, "bucket recreation also resets revisions, so birth must prevent ABA")
			}
			before, beforeRevision := readCommandOwner(t, js)
			require.Error(t, client.Verify(ctx, receipt))
			require.Error(t, client.ReleaseInitialOwner(ctx, receipt))
			after, afterRevision := readCommandOwner(t, js)
			assert.Equal(t, before, after)
			assert.Equal(t, beforeRevision, afterRevision, "a stale receipt cannot release recreated initializing ownership")
			if resource == "stream" {
				assert.Equal(t, held, after)
				assert.Equal(t, heldRevision, afterRevision)
			} else {
				assert.Equal(t, "initializing", after.Phase)
			}
		})
	}
}

func TestVoiceCommandReceiptRejectsRecreatedKeyRealJetStream(t *testing.T) {
	client, js, ctx := voiceCommandBroker(t)
	generation := uuid.New()
	identity, err := client.Initialize(ctx, generation)
	require.NoError(t, err)
	receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
	require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
	_, _ = holdCommandOwner(t, js, uuid.New().String())
	kv, err := js.KeyValue(testVoiceCommandBucket)
	require.NoError(t, err)
	require.NoError(t, kv.Delete(testVoiceCommandOwnerKey))
	_, err = client.Initialize(ctx, generation)
	require.Error(t, err, "a retained bucket's deleted owner is continuity loss, not first creation")
	_, err = kv.Get(testVoiceCommandOwnerKey)
	require.ErrorIs(t, err, natssdk.ErrKeyNotFound)
	replacement := testCommandOwner{
		Version: 1, StreamGeneration: generation.String(), Phase: "initializing",
		Birth: &testCommandBirth{StreamCreatedAt: identity.StreamCreatedAt.UTC().Format(time.RFC3339Nano), OwnerStoreCreatedAt: identity.OwnerStoreCreatedAt.UTC().Format(time.RFC3339Nano)},
	}
	encoded, err := json.Marshal(replacement)
	require.NoError(t, err)
	replacementRevision, err := kv.Create(testVoiceCommandOwnerKey, encoded)
	require.NoError(t, err)
	require.Greater(t, replacementRevision, identity.InitializingRevision)
	before, beforeRevision := readCommandOwner(t, js)
	require.Error(t, client.Verify(ctx, receipt))
	require.Error(t, client.ReleaseInitialOwner(ctx, receipt))
	after, afterRevision := readCommandOwner(t, js)
	assert.Equal(t, "initializing", after.Phase)
	assert.Equal(t, before, after)
	assert.Equal(t, beforeRevision, afterRevision, "same bucket birth never authorizes releasing a replacement key revision")
}

func TestVoiceCommandHeldProofValidationRealJetStream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testCommandOwner)
	}{
		{name: "missing proof", mutate: func(owner *testCommandOwner) { owner.Proof = nil }},
		{name: "initializing birth mixed with held proof", mutate: func(owner *testCommandOwner) {
			owner.Birth = &testCommandBirth{StreamCreatedAt: owner.Proof.StreamCreatedAt, OwnerStoreCreatedAt: owner.Proof.OwnerStoreCreatedAt}
		}},
		{name: "empty frozen revision", mutate: func(owner *testCommandOwner) { owner.Proof.InitializingRevision = "" }},
		{name: "noncanonical frozen revision", mutate: func(owner *testCommandOwner) { owner.Proof.InitializingRevision = "01" }},
		{name: "zero frozen revision", mutate: func(owner *testCommandOwner) { owner.Proof.InitializingRevision = "0" }},
		{name: "different frozen revision", mutate: func(owner *testCommandOwner) { owner.Proof.InitializingRevision = "7" }},
		{name: "different command stream birth", mutate: func(owner *testCommandOwner) { owner.Proof.StreamCreatedAt = "2026-10-13T10:00:00.123456789Z" }},
		{name: "different owner store birth", mutate: func(owner *testCommandOwner) { owner.Proof.OwnerStoreCreatedAt = "2026-10-13T10:00:00.234567891Z" }},
		{name: "noncanonical timestamp timezone", mutate: func(owner *testCommandOwner) {
			owner.Proof.StreamCreatedAt = strings.TrimSuffix(owner.Proof.StreamCreatedAt, "Z") + "+00:00"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, js, ctx := voiceCommandBroker(t)
			generation := uuid.New()
			identity, err := client.Initialize(ctx, generation)
			require.NoError(t, err)
			receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
			require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
			owner, revision := holdCommandOwner(t, js, uuid.New().String())
			tc.mutate(&owner)
			encoded, err := json.Marshal(owner)
			require.NoError(t, err)
			kv, err := js.KeyValue(testVoiceCommandBucket)
			require.NoError(t, err)
			malformedRevision, err := kv.Update(testVoiceCommandOwnerKey, encoded, revision)
			require.NoError(t, err)
			require.Error(t, client.Verify(ctx, receipt))
			require.Error(t, client.ReleaseInitialOwner(ctx, receipt))
			retained, retainedRevision := readCommandOwner(t, js)
			assert.Equal(t, "held", retained.Phase)
			assert.Equal(t, owner, retained)
			assert.Equal(t, malformedRevision, retainedRevision, "proof rejection cannot rewrite or release ownership")
		})
	}
}

func TestVoiceCommandRecoverOwnerRealJetStream(t *testing.T) {
	t.Run("exact held predecessor releases while retaining boot and proof", func(t *testing.T) {
		client, js, ctx := voiceCommandBroker(t)
		generation := uuid.New()
		identity, err := client.Initialize(ctx, generation)
		require.NoError(t, err)
		receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
		require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
		boot := uuid.New()
		held, revision := holdCommandOwner(t, js, boot.String())
		require.NoError(t, client.RecoverVoiceCommandOwner(ctx, revision, boot, receipt))
		recovered, recoveredRevision := readCommandOwner(t, js)
		assert.Equal(t, "released", recovered.Phase)
		assert.Equal(t, held.NodeBootID, recovered.NodeBootID, "recovery retains predecessor identity")
		assert.Equal(t, held.Proof, recovered.Proof, "recovery never replaces the original provisioning proof")
		assert.Nil(t, recovered.Birth)
		assert.Greater(t, recoveredRevision, revision)
		require.NoError(t, client.Verify(ctx, receipt))
		requireBrokerIdentity(t, js, receipt)
		require.Error(t, client.RecoverVoiceCommandOwner(ctx, recoveredRevision, boot, receipt), "an already released record cannot satisfy a held-owner CAS")
		unchanged, unchangedRevision := readCommandOwner(t, js)
		assert.Equal(t, recovered, unchanged)
		assert.Equal(t, recoveredRevision, unchangedRevision)
	})

	for _, tc := range []struct {
		name      string
		duplicate bool
	}{
		{name: "missing committed receipt"},
		{name: "multiple committed receipts", duplicate: true},
	} {
		t.Run(tc.name+" refuses without mutation", func(t *testing.T) {
			client, js, ctx := voiceCommandBroker(t)
			generation := uuid.New()
			identity, err := client.Initialize(ctx, generation)
			require.NoError(t, err)
			receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
			require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
			boot := uuid.New()
			held, revision := holdCommandOwner(t, js, boot.String())
			require.NoError(t, client.Verify(ctx, receipt), "valid held fixture must verify before testing receipt count")
			var receipts []voicecommand.TransportReceipt
			if tc.duplicate {
				receipts = []voicecommand.TransportReceipt{receipt, receipt}
			}
			require.ErrorIs(t, client.RecoverVoiceCommandOwner(ctx, revision, boot, receipts...), ErrVoiceCommandContinuity,
				"operator recovery requires exactly one committed receipt")
			retained, retainedRevision := readCommandOwner(t, js)
			assert.Equal(t, held, retained, "invalid receipt count must preserve the held owner and original proof")
			assert.Equal(t, "held", retained.Phase)
			assert.Equal(t, revision, retainedRevision, "invalid receipt count must not mutate owner authority")
		})
	}

	for _, tc := range []struct {
		name                               string
		staleRevision, wrongBoot, canceled bool
		zeroRevision, emptyBoot            bool
		mutate                             func(*testCommandOwner)
	}{
		{name: "stale revision", staleRevision: true},
		{name: "different predecessor boot", wrongBoot: true},
		{name: "zero expected revision", zeroRevision: true},
		{name: "empty predecessor boot", emptyBoot: true},
		{name: "canceled request", canceled: true},
		{name: "missing provisioning proof", mutate: func(owner *testCommandOwner) { owner.Proof = nil }},
		{name: "invalid proof revision", mutate: func(owner *testCommandOwner) { owner.Proof.InitializingRevision = "0" }},
		{name: "invalid resource birth proof", mutate: func(owner *testCommandOwner) { owner.Proof.OwnerStoreCreatedAt = "invalid" }},
	} {
		t.Run(tc.name+" refuses without mutation", func(t *testing.T) {
			client, js, ctx := voiceCommandBroker(t)
			generation := uuid.New()
			identity, err := client.Initialize(ctx, generation)
			require.NoError(t, err)
			receipt := voicecommand.TransportReceipt{Generation: generation, Identity: identity}
			require.NoError(t, client.ReleaseInitialOwner(ctx, receipt))
			boot := uuid.New()
			held, revision := holdCommandOwner(t, js, boot.String())
			if tc.mutate != nil {
				tc.mutate(&held)
				encoded, encodeErr := json.Marshal(held)
				require.NoError(t, encodeErr)
				kv, kvErr := js.KeyValue(testVoiceCommandBucket)
				require.NoError(t, kvErr)
				revision, err = kv.Update(testVoiceCommandOwnerKey, encoded, revision)
				require.NoError(t, err)
			}
			expectedRevision, expectedBoot := revision, boot
			if tc.staleRevision {
				expectedRevision--
			}
			if tc.wrongBoot {
				expectedBoot = uuid.New()
			}
			if tc.zeroRevision {
				expectedRevision = 0
			}
			if tc.emptyBoot {
				expectedBoot = uuid.Nil
			}
			if tc.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			require.Error(t, client.RecoverVoiceCommandOwner(ctx, expectedRevision, expectedBoot, receipt))
			retained, retainedRevision := readCommandOwner(t, js)
			assert.Equal(t, held, retained)
			assert.Equal(t, "held", retained.Phase)
			assert.Equal(t, revision, retainedRevision, "a refused recovery cannot perform any owner write")
		})
	}
}

func TestNewVoiceCommandClientRealJetStreamLifetimeAndImmediateClose(t *testing.T) {
	fixture, _, _ := voiceCommandBroker(t)
	creation, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := NewVoiceCommandClient(creation, fixture.conn.ConnectedUrl())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	cancel()
	require.True(t, client.IsConnected(), "the creation context cannot own the successful connection's lifetime")
	_, err = client.Initialize(context.Background(), uuid.New())
	require.NoError(t, err, "a fresh request remains usable after creation cancellation")

	// Hold a real subscription callback so a drain would wait for unfinished
	// work. The dedicated client owns synchronous operations and closes directly.
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finishCallback := func() { releaseOnce.Do(func() { close(release) }) }
	defer finishCallback()
	_, err = client.conn.Subscribe("fixture.command-client-close", func(*natssdk.Msg) { close(entered); <-release })
	require.NoError(t, err)
	require.NoError(t, client.conn.FlushTimeout(time.Second))
	require.NoError(t, client.conn.Publish("fixture.command-client-close", []byte("fixture")))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("the real subscription callback never became in-flight")
	}
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case closeErr := <-closed:
		require.NoError(t, closeErr)
	case <-time.After(500 * time.Millisecond):
		finishCallback()
		<-closed
		t.Fatal("dedicated close waited for an asynchronous drain")
	}
	assert.True(t, client.conn.IsClosed())
	assert.False(t, client.IsConnected())
	require.NoError(t, client.Close(), "dedicated close is idempotent")
}

func TestNewVoiceCommandClientRejectsCanceledAndInvalidConstruction(t *testing.T) {
	t.Run("already canceled before dialing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client, err := NewVoiceCommandClient(ctx, "nats://127.0.0.1:1")
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, client)
	})
	t.Run("malformed URL", func(t *testing.T) {
		client, err := NewVoiceCommandClient(context.Background(), "://invalid")
		require.Error(t, err)
		assert.Nil(t, client)
	})
	t.Run("nil and empty close are safe", func(t *testing.T) {
		var client *VoiceCommandClient
		require.NoError(t, client.Close())
		require.NoError(t, (&VoiceCommandClient{}).Close())
		require.NoError(t, (&VoiceCommandClient{Client: &Client{}}).Close())
	})
}

func TestNewVoiceCommandClientHungInfoHandshakeClosesSocket(t *testing.T) {
	for _, cancellation := range []bool{true, false} {
		name := "API budget"
		if cancellation {
			name = "caller cancellation"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, listener.Close()) })
			type acceptance struct {
				connection net.Conn
				err        error
			}
			accepted := make(chan acceptance, 1)
			go func() {
				connection, acceptErr := listener.Accept()
				accepted <- acceptance{connection: connection, err: acceptErr}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type construction struct {
				client *VoiceCommandClient
				err    error
			}
			constructed := make(chan construction, 1)
			go func() {
				client, connectErr := NewVoiceCommandClient(ctx, "nats://"+listener.Addr().String())
				constructed <- construction{client: client, err: connectErr}
			}()
			var peer net.Conn
			select {
			case result := <-accepted:
				require.NoError(t, result.err)
				peer = result.connection
			case <-time.After(time.Second):
				t.Fatal("constructor never reached the controlled TCP server")
			}
			t.Cleanup(func() { assert.NoError(t, peer.Close()) })
			limit := 3 * time.Second
			if cancellation {
				cancel()
				limit = 500 * time.Millisecond
			}
			select {
			case result := <-constructed:
				require.Error(t, result.err)
				assert.Nil(t, result.client)
				if cancellation {
					require.ErrorIs(t, result.err, context.Canceled)
				}
			case <-time.After(limit):
				cancel()
				<-constructed
				t.Fatal("a hung INFO handshake exceeded its cancellation or API budget")
			}
			require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
			var buffer [1]byte
			count, readErr := peer.Read(buffer[:])
			assert.Zero(t, count)
			require.ErrorIs(t, readErr, io.EOF, "failed construction must close the accepted socket")
		})
	}
}
