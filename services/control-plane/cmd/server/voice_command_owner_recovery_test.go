package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	"github.com/google/uuid"
	natssdk "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ownerRecoveryTestBoot = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func ownerRecoveryArguments(revision, boot string) []string {
	return []string{
		"recover", "--expected-revision", revision,
		"--expected-boot-id", boot, "--confirm-predecessor-stopped",
	}
}

func TestParseVoiceCommandOwnerRecoveryPreservesExactOptions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		revision     string
		wantRevision uint64
	}{
		{name: "first revision", revision: "1", wantRevision: 1},
		{name: "ordinary revision", revision: "17", wantRevision: 17},
		{name: "beyond JavaScript Number precision", revision: "9007199254740993", wantRevision: 9007199254740993},
		{name: "maximum KV revision", revision: "18446744073709551615", wantRevision: ^uint64(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options, err := parseVoiceCommandOwnerRecovery(ownerRecoveryArguments(tc.revision, ownerRecoveryTestBoot))
			require.NoError(t, err)
			assert.Equal(t, tc.wantRevision, options.ExpectedRevision)
			assert.Equal(t, uuid.MustParse(ownerRecoveryTestBoot), options.ExpectedBootID)
			assert.True(t, options.ConfirmPredecessorStopped)
		})
	}
	for version := 1; version <= 8; version++ {
		t.Run(fmt.Sprintf("canonical UUID version %d", version), func(t *testing.T) {
			boot := fmt.Sprintf("aaaaaaaa-aaaa-%daaa-8aaa-aaaaaaaaaaaa", version)
			options, err := parseVoiceCommandOwnerRecovery(ownerRecoveryArguments("23", boot))
			require.NoError(t, err)
			assert.Equal(t, uint64(23), options.ExpectedRevision)
			assert.Equal(t, uuid.MustParse(boot), options.ExpectedBootID)
			assert.True(t, options.ConfirmPredecessorStopped)
		})
	}
}

func TestParseVoiceCommandOwnerRecoveryRequiresPredecessorStopProof(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "missing confirmation", args: []string{"recover", "--expected-revision", "17", "--expected-boot-id", ownerRecoveryTestBoot}},
		{name: "explicit false confirmation", args: []string{"recover", "--expected-revision", "17", "--expected-boot-id", ownerRecoveryTestBoot, "--confirm-predecessor-stopped=false"}},
		{name: "false positional value", args: []string{"recover", "--expected-revision", "17", "--expected-boot-id", ownerRecoveryTestBoot, "--confirm-predecessor-stopped", "false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseVoiceCommandOwnerRecovery(tc.args)
			require.Error(t, err, "operator recovery must never infer physical stop confirmation")
		})
	}
}

func TestParseVoiceCommandOwnerRecoveryRejectsInvalidRevision(t *testing.T) {
	for _, revision := range []string{"", "0", "01", "0001", "-1", "+1", " 1", "1 ", "1.0", "1e3", "0x10", "18446744073709551616"} {
		t.Run(revision, func(t *testing.T) {
			_, err := parseVoiceCommandOwnerRecovery(ownerRecoveryArguments(revision, ownerRecoveryTestBoot))
			require.Error(t, err)
		})
	}
	t.Run("missing revision", func(t *testing.T) {
		_, err := parseVoiceCommandOwnerRecovery([]string{"recover", "--expected-boot-id", ownerRecoveryTestBoot, "--confirm-predecessor-stopped"})
		require.Error(t, err)
	})
}

func TestParseVoiceCommandOwnerRecoveryRejectsInvalidBootIdentity(t *testing.T) {
	for _, boot := range []string{
		"", "not-a-uuid", "00000000-0000-0000-0000-000000000000",
		"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "aaaaaaaa-aaaa-0aaa-8aaa-aaaaaaaaaaaa",
		"aaaaaaaa-aaaa-9aaa-8aaa-aaaaaaaaaaaa", "aaaaaaaa-aaaa-4aaa-7aaa-aaaaaaaaaaaa",
		"aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa", "{aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa}",
		"urn:uuid:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ownerRecoveryTestBoot + "\n",
	} {
		t.Run(boot, func(t *testing.T) {
			_, err := parseVoiceCommandOwnerRecovery(ownerRecoveryArguments("17", boot))
			require.Error(t, err)
		})
	}
	t.Run("missing boot identity", func(t *testing.T) {
		_, err := parseVoiceCommandOwnerRecovery([]string{"recover", "--expected-revision", "17", "--confirm-predecessor-stopped"})
		require.Error(t, err)
	})
}

func TestParseVoiceCommandOwnerRecoveryRejectsUnsupportedArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "empty invocation"},
		{name: "missing recover subcommand", args: ownerRecoveryArguments("17", ownerRecoveryTestBoot)[1:]},
		{name: "unknown subcommand with otherwise valid flags", args: append([]string{"acquire"}, ownerRecoveryArguments("17", ownerRecoveryTestBoot)[1:]...)},
		{name: "unknown flag", args: append(ownerRecoveryArguments("17", ownerRecoveryTestBoot), "--reset-generation")},
		{name: "unexpected positional argument", args: append(ownerRecoveryArguments("17", ownerRecoveryTestBoot), "unexpected")},
		{name: "unconsumed double dash argument", args: append(ownerRecoveryArguments("17", ownerRecoveryTestBoot), "--", "unexpected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseVoiceCommandOwnerRecovery(tc.args)
			require.Error(t, err)
		})
	}
}

func TestDispatchVoiceCommandOwnerRecoveryPreservesArgumentsAndExitCode(t *testing.T) {
	var received []string
	runners := controlPlaneSubcommandRunners{
		voiceCommandOwner: func(args []string) int {
			received = append([]string(nil), args...)
			return 37
		},
	}
	arguments := ownerRecoveryArguments("17", ownerRecoveryTestBoot)
	code, handled := dispatchControlPlaneSubcommandWithRunners(
		append([]string{"control-plane", "voice-command-owner"}, arguments...), runners,
	)
	assert.True(t, handled)
	assert.Equal(t, 37, code)
	assert.Equal(t, arguments, received)
	options, err := parseVoiceCommandOwnerRecovery(received)
	require.NoError(t, err)
	assert.Equal(t, uint64(17), options.ExpectedRevision)
	assert.Equal(t, uuid.MustParse(ownerRecoveryTestBoot), options.ExpectedBootID)
	assert.True(t, options.ConfirmPredecessorStopped)
}

func TestRunVoiceCommandOwnerRecoveryRefusesMissingProofBeforeConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "invalid-database-url")
	t.Setenv("NATS_URL", "invalid-nats-url")
	code := runVoiceCommandOwnerRecovery([]string{"recover", "--expected-revision", "17", "--expected-boot-id", ownerRecoveryTestBoot})
	assert.Equal(t, 64, code, "stop proof validation must precede serving configuration and broker access")
}

func TestRunVoiceCommandOwnerInspectRejectsArgumentsBeforeConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "://invalid")
	t.Setenv("NATS_URL", "://invalid")
	assert.Equal(t, 64, runVoiceCommandOwner([]string{"inspect", "--unexpected"}))
}

func TestRunVoiceCommandOwnerInspectRefusesUnavailableDatabase(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("DATABASE_URL", "://invalid")
	db, _, err := openVoiceCommandOwnerStore(context.Background())
	require.Nil(t, db)
	require.EqualError(t, err, "voice-command-owner: database unavailable")
	assert.Equal(t, 1, runVoiceCommandOwner([]string{"inspect"}))
}

func TestInspectVoiceCommandOwnerRealServices(t *testing.T) {
	db, _ := mainVoiceReceiptDatabase(t)
	url, js, ctx := mainVoiceCommandBroker(t)
	options, _ := provisionMainHeldOwner(ctx, t, db, url, js)
	before, revision := readMainCommandOwner(t, js)

	status, err := inspectVoiceCommandOwner(ctx, db, url)
	require.NoError(t, err)
	assert.Equal(t, "held", status.Phase)
	assert.Equal(t, revision, status.Revision)
	assert.Equal(t, options.ExpectedBootID.String(), status.NodeBootID)
	t.Chdir(t.TempDir())
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("DATABASE_URL", testdb.DatabaseURL())
	t.Setenv("NATS_URL", url)
	assert.Zero(t, runVoiceCommandOwner([]string{"inspect"}), "operator command must use the verified read-only path")
	t.Setenv("NATS_URL", "://invalid")
	assert.Equal(t, 1, runVoiceCommandOwner([]string{"inspect"}), "inspection must refuse an unavailable broker")
	t.Setenv("NATS_URL", url)
	after, afterRevision := readMainCommandOwner(t, js)
	assert.Equal(t, before, after)
	assert.Equal(t, revision, afterRevision, "inspection must not change ownership")

	require.NoError(t, recoverVoiceCommandOwner(ctx, db, url, options))
	status, err = inspectVoiceCommandOwner(ctx, db, url)
	require.NoError(t, err)
	assert.Equal(t, "released", status.Phase)
	assert.Greater(t, status.Revision, revision)
	assert.Equal(t, options.ExpectedBootID.String(), status.NodeBootID)

	require.NoError(t, js.DeleteStream("VOICE_COMMANDS_V1"))
	_, err = inspectVoiceCommandOwner(ctx, db, url)
	require.Error(t, err, "inspection must refuse a missing receipt-bound stream")
	_, err = db.Exec(`DELETE FROM voice_command_transport`)
	require.NoError(t, err)
	_, err = inspectVoiceCommandOwner(ctx, db, url)
	require.Error(t, err, "inspection must refuse a missing committed receipt")
}

func TestRecoverVoiceCommandOwnerExactHeldRealServices(t *testing.T) {
	db, _ := mainVoiceReceiptDatabase(t)
	url, js, ctx := mainVoiceCommandBroker(t)
	options, receipt := provisionMainHeldOwner(ctx, t, db, url, js)
	held, revision := readMainCommandOwner(t, js)
	require.NoError(t, recoverVoiceCommandOwner(ctx, db, url, options))
	recovered, recoveredRevision := readMainCommandOwner(t, js)
	assert.Equal(t, "released", recovered.Phase)
	assert.Equal(t, held.NodeBootID, recovered.NodeBootID)
	assert.Equal(t, held.Proof, recovered.Proof)
	assert.Nil(t, recovered.Birth)
	assert.Greater(t, recoveredRevision, revision)
	unchangedReceipt, err := voicecommand.LoadReceipt(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, receipt, unchangedReceipt, "operator recovery never replaces the permanent SQL proof")
}

func TestRecoverVoiceCommandOwnerRefusesUnsafeStateRealServices(t *testing.T) {
	for _, state := range []string{
		"confirmation refused", "wrong boot", "wrong revision", "canceled context", "invalid broker URL",
		"unprovisioned receipt", "missing receipt", "missing stream", "missing bucket", "missing owner key", "changed stream birth", "changed bucket birth",
	} {
		t.Run(state, func(t *testing.T) {
			db, generation := mainVoiceReceiptDatabase(t)
			url, js, ctx := mainVoiceCommandBroker(t)
			options, _ := provisionMainHeldOwner(ctx, t, db, url, js)
			kv, err := js.KeyValue("VOICE_COMMAND_OWNER_V1")
			require.NoError(t, err)
			switch state {
			case "confirmation refused":
				options.ConfirmPredecessorStopped = false
			case "wrong boot":
				options.ExpectedBootID = uuid.New()
			case "wrong revision":
				options.ExpectedRevision--
			case "canceled context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "invalid broker URL":
				url = "://invalid"
			case "unprovisioned receipt", "missing receipt":
				_, err = db.Exec(`DELETE FROM voice_command_transport`)
				require.NoError(t, err)
				if state == "unprovisioned receipt" {
					_, err = db.Exec(`INSERT INTO voice_command_transport (id,generation) VALUES (TRUE,$1)`, generation)
					require.NoError(t, err)
				}
			case "missing stream":
				require.NoError(t, js.DeleteStream("VOICE_COMMANDS_V1"))
			case "missing bucket":
				require.NoError(t, js.DeleteKeyValue("VOICE_COMMAND_OWNER_V1"))
			case "missing owner key":
				require.NoError(t, kv.Delete("authoritative"))
			case "changed stream birth":
				original, infoErr := js.StreamInfo("VOICE_COMMANDS_V1")
				require.NoError(t, infoErr)
				require.NoError(t, js.DeleteStream("VOICE_COMMANDS_V1"))
				recreated, createErr := js.AddStream(&original.Config)
				require.NoError(t, createErr)
				require.False(t, original.Created.Equal(recreated.Created))
			case "changed bucket birth":
				held, _ := readMainCommandOwner(t, js)
				original, infoErr := js.StreamInfo("KV_VOICE_COMMAND_OWNER_V1")
				require.NoError(t, infoErr)
				require.NoError(t, js.DeleteKeyValue("VOICE_COMMAND_OWNER_V1"))
				recreated, createErr := js.AddStream(&original.Config)
				require.NoError(t, createErr)
				require.False(t, original.Created.Equal(recreated.Created))
				kv, err = js.KeyValue("VOICE_COMMAND_OWNER_V1")
				require.NoError(t, err)
				encoded, encodeErr := json.Marshal(held)
				require.NoError(t, encodeErr)
				options.ExpectedRevision, err = kv.Create("authoritative", encoded)
				require.NoError(t, err)
			}
			missingOwner := state == "missing bucket" || state == "missing owner key"
			var before mainCommandOwner
			var revision uint64
			if !missingOwner {
				before, revision = readMainCommandOwner(t, js)
			}
			require.Error(t, recoverVoiceCommandOwner(ctx, db, url, options))
			switch state {
			case "missing bucket":
				_, err = js.KeyValue("VOICE_COMMAND_OWNER_V1")
				require.ErrorIs(t, err, natssdk.ErrBucketNotFound)
			case "missing owner key":
				_, err = kv.Get("authoritative")
				require.ErrorIs(t, err, natssdk.ErrKeyNotFound)
			default:
				after, afterRevision := readMainCommandOwner(t, js)
				assert.Equal(t, before, after)
				assert.Equal(t, "held", after.Phase)
				assert.Equal(t, revision, afterRevision, "refused recovery cannot modify ownership")
			}
		})
	}
}

func TestOpenVoiceCommandRecoveryDBUsesRealDatabaseAndFailsClosed(t *testing.T) {
	db, _ := mainVoiceReceiptDatabase(t)
	t.Run("valid connection", func(t *testing.T) {
		connection, err := openVoiceCommandRecoveryDB(context.Background(), testdb.DatabaseURL())
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, connection.Close()) })
		var one int
		require.NoError(t, connection.QueryRow(`SELECT 1`).Scan(&one))
		assert.Equal(t, 1, one)
	})
	t.Run("invalid DSN", func(t *testing.T) {
		connection, err := openVoiceCommandRecoveryDB(context.Background(), "://invalid")
		require.Error(t, err)
		assert.Nil(t, connection)
	})
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		connection, err := openVoiceCommandRecoveryDB(ctx, testdb.DatabaseURL())
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, connection)
	})
	t.Run("expired deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		connection, err := openVoiceCommandRecoveryDB(ctx, testdb.DatabaseURL())
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Nil(t, connection)
	})
	t.Run("unavailable database", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		address := listener.Addr().String()
		require.NoError(t, listener.Close())
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		connection, err := openVoiceCommandRecoveryDB(ctx, "postgres://epic_fixture@"+address+"/concord_epic2551_test?sslmode=disable")
		require.Error(t, err)
		assert.Nil(t, connection)
	})
	require.NoError(t, db.Ping(), "failed recovery connections never close the fixture pool")
}

func TestRunVoiceCommandOwnerRecoveryRealServices(t *testing.T) {
	for _, outcome := range []string{"success", "wrong predecessor", "configuration unavailable", "database unavailable"} {
		t.Run(outcome, func(t *testing.T) {
			db, _ := mainVoiceReceiptDatabase(t)
			url, js, ctx := mainVoiceCommandBroker(t)
			options, _ := provisionMainHeldOwner(ctx, t, db, url, js)
			before, revision := readMainCommandOwner(t, js)
			t.Chdir(t.TempDir())
			t.Setenv("ENVIRONMENT", "development")
			t.Setenv("DATABASE_URL", testdb.DatabaseURL())
			t.Setenv("NATS_URL", url)
			if outcome == "wrong predecessor" {
				options.ExpectedBootID = uuid.New()
			}
			if outcome == "configuration unavailable" {
				t.Setenv("MFA_ENCRYPTION_KEY_VERSION", "invalid")
			}
			if outcome == "database unavailable" {
				t.Setenv("DATABASE_URL", "://invalid")
			}
			code := runVoiceCommandOwnerRecovery(ownerRecoveryArguments(strconv.FormatUint(options.ExpectedRevision, 10), options.ExpectedBootID.String()))
			after, afterRevision := readMainCommandOwner(t, js)
			if outcome == "success" {
				assert.Zero(t, code)
				assert.Equal(t, "released", after.Phase)
				assert.Equal(t, before.NodeBootID, after.NodeBootID)
				assert.Equal(t, before.Proof, after.Proof)
				assert.Greater(t, afterRevision, revision)
			} else {
				assert.Equal(t, 1, code)
				assert.Equal(t, before, after)
				assert.Equal(t, revision, afterRevision)
			}
		})
	}
}
