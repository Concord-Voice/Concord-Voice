package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	"github.com/google/uuid"
	natssdk "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mainVoiceCommandBroker(t *testing.T) (string, natssdk.JetStreamContext, context.Context) {
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
	return connection.ConnectedUrl(), js, ctx
}

func mainVoiceReceiptDatabase(t *testing.T) (*sql.DB, uuid.UUID) {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	_, err := db.Exec(`DELETE FROM voice_command_transport`)
	require.NoError(t, err)
	generation := uuid.New()
	_, err = db.Exec(`INSERT INTO voice_command_transport (id,generation) VALUES (TRUE,$1)`, generation)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := db.Exec(`TRUNCATE voice_command_admission, voice_command_retirement,
			voice_command_outbox, voice_command_sequence`)
		assert.NoError(t, cleanupErr)
		_, cleanupErr = db.Exec(`INSERT INTO voice_command_sequence (id) VALUES (TRUE)`)
		assert.NoError(t, cleanupErr)
		_, cleanupErr = db.Exec(`DELETE FROM voice_command_transport`)
		assert.NoError(t, cleanupErr)
	})
	return db, generation
}

type mainCommandOwner struct {
	Version          int               `json:"version"`
	StreamGeneration string            `json:"streamGeneration"`
	Phase            string            `json:"phase"`
	NodeBootID       *string           `json:"nodeBootId"`
	Proof            map[string]string `json:"proof,omitempty"`
	Birth            map[string]string `json:"birth,omitempty"`
}

func readMainCommandOwner(t *testing.T, js natssdk.JetStreamContext) (mainCommandOwner, uint64) {
	t.Helper()
	kv, err := js.KeyValue("VOICE_COMMAND_OWNER_V1")
	require.NoError(t, err)
	entry, err := kv.Get("authoritative")
	require.NoError(t, err)
	var owner mainCommandOwner
	require.NoError(t, json.Unmarshal(entry.Value(), &owner))
	return owner, entry.Revision()
}

func provisionMainHeldOwner(ctx context.Context, t *testing.T, db *sql.DB, url string, js natssdk.JetStreamContext) (voiceCommandOwnerRecoveryOptions, voicecommand.TransportReceipt) {
	t.Helper()
	client, err := natsclient.NewVoiceCommandClient(ctx, url)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	require.NoError(t, voicecommand.BootstrapTransport(ctx, db, client))
	receipt, err := voicecommand.LoadReceipt(ctx, db)
	require.NoError(t, err)
	owner, revision := readMainCommandOwner(t, js)
	require.Equal(t, "released", owner.Phase)
	require.NotEmpty(t, owner.Proof)
	boot := uuid.MustParse(ownerRecoveryTestBoot)
	bootString := boot.String()
	owner.Phase, owner.NodeBootID = "held", &bootString
	encoded, err := json.Marshal(owner)
	require.NoError(t, err)
	kv, err := js.KeyValue("VOICE_COMMAND_OWNER_V1")
	require.NoError(t, err)
	heldRevision, err := kv.Update("authoritative", encoded, revision)
	require.NoError(t, err)
	return voiceCommandOwnerRecoveryOptions{ExpectedRevision: heldRevision, ExpectedBootID: boot, ConfirmPredecessorStopped: true}, receipt
}
