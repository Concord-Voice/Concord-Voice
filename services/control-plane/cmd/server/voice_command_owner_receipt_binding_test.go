package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voicecommand"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
	natssdk "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression for #2624: operator recovery remains bound to the committed SQL
// receipt across verification and the held-owner CAS.
func TestRecoverVoiceCommandOwnerPreservesCommittedReceiptAcrossVerification(t *testing.T) {
	for _, tc := range []struct {
		name          string
		replaceBucket bool
		replaceStream bool
	}{
		{name: "unchanged births release exact held owner"},
		{name: "replacement command stream with original proof refuses continuity loss", replaceStream: true},
		{name: "replacement owner bucket refuses receipt adoption", replaceBucket: true},
		{name: "replacement command stream and owner bucket refuse receipt adoption", replaceBucket: true, replaceStream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := mainVoiceReceiptDatabase(t)
			url, js, ctx := mainVoiceCommandBroker(t)
			options, receipt := provisionMainHeldOwner(ctx, t, db, url, js)
			verifier, err := natsclient.NewVoiceCommandClient(ctx, url)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, verifier.Close()) })
			require.NoError(t, verifier.Verify(ctx, receipt), "the real SQL receipt must verify before recovery starts")
			before, revision := readMainCommandOwner(t, js)
			require.Equal(t, options.ExpectedRevision, revision)
			require.Equal(t, options.ExpectedBootID.String(), *before.NodeBootID)

			gatedURL, verified, resume := ownerRecoveryReceiptGate(ctx, t, url)
			recovered := make(chan error, 1)
			go func() { recovered <- recoverVoiceCommandOwner(ctx, db, gatedURL, options) }()
			select {
			case <-verified:
				// The real CLI has finished its first owner read and issued its next
				// broker request: its SQL-backed Verify therefore returned success.
			case err := <-recovered:
				t.Fatalf("recovery never reached the verified-receipt interval: %v", err)
			case <-ctx.Done():
				t.Fatal("recovery never reached the verified-receipt interval")
			}
			if tc.replaceBucket {
				before = replaceRecoveryResources(t, js, receipt, options, before, tc.replaceStream)
				_, revision = readMainCommandOwner(t, js)
				require.Equal(t, options.ExpectedRevision, revision, "replacement must preserve the exact expected held revision")
				require.Equal(t, options.ExpectedBootID.String(), *before.NodeBootID)
			} else if tc.replaceStream {
				_ = replaceRecoveryCommandStream(t, js, receipt)
			}
			close(resume)
			var recoveryErr error
			select {
			case recoveryErr = <-recovered:
			case <-ctx.Done():
				t.Fatal("recovery did not finish after the broker request was released")
			}
			after, afterRevision := readMainCommandOwner(t, js)
			if tc.replaceBucket || tc.replaceStream {
				assert.ErrorIs(t, recoveryErr, natsclient.ErrVoiceCommandContinuity,
					"operator recovery must reject replacement resource births or broker proof that differ from the committed SQL TransportReceipt")
				assert.Equal(t, before, after, "receipt mismatch must preserve the held owner and its proof")
				assert.Equal(t, "held", after.Phase, "receipt mismatch must never release ownership")
				assert.Equal(t, revision, afterRevision, "receipt mismatch must not perform the owner CAS")
			} else {
				require.NoError(t, recoveryErr)
				assert.Equal(t, "released", after.Phase)
				assert.Greater(t, afterRevision, revision, "the control must observe the real successful owner CAS")
				assert.Equal(t, before.NodeBootID, after.NodeBootID)
				assert.Equal(t, before.Proof, after.Proof)
			}
			committed, err := voicecommand.LoadReceipt(ctx, db)
			require.NoError(t, err)
			assert.Equal(t, receipt, committed, "recovery must preserve the committed SQL receipt")
		})
	}
}

func replaceRecoveryResources(t *testing.T, js natssdk.JetStreamContext, receipt voicecommand.TransportReceipt, options voiceCommandOwnerRecoveryOptions, held mainCommandOwner, replaceStream bool) mainCommandOwner {
	t.Helper()
	if replaceStream {
		held.Proof["streamCreatedAt"] = replaceRecoveryCommandStream(t, js, receipt).UTC().Format(time.RFC3339Nano)
	}
	original, err := js.StreamInfo("KV_" + natsclient.VoiceCommandOwnerBucket)
	require.NoError(t, err)
	require.NoError(t, js.DeleteKeyValue(natsclient.VoiceCommandOwnerBucket))
	replacement, err := js.AddStream(&original.Config)
	require.NoError(t, err)
	require.False(t, receipt.Identity.OwnerStoreCreatedAt.Equal(replacement.Created), "the owner bucket must have a genuinely new broker birth")
	held.Proof["ownerStoreCreatedAt"] = replacement.Created.UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(held)
	require.NoError(t, err)
	kv, err := js.KeyValue(natsclient.VoiceCommandOwnerBucket)
	require.NoError(t, err)
	revision, err := kv.Create(natsclient.VoiceCommandOwnerKey, encoded)
	require.NoError(t, err)
	require.LessOrEqual(t, options.ExpectedRevision, uint64(5), "fixture recovery revision must be small and bounded")
	for revision < options.ExpectedRevision {
		revision, err = kv.Update(natsclient.VoiceCommandOwnerKey, encoded, revision)
		require.NoError(t, err)
	}
	require.Equal(t, options.ExpectedRevision, revision)
	return held
}

func replaceRecoveryCommandStream(t *testing.T, js natssdk.JetStreamContext, receipt voicecommand.TransportReceipt) time.Time {
	t.Helper()
	original, err := js.StreamInfo(natsclient.VoiceCommandStream)
	require.NoError(t, err)
	require.NoError(t, js.DeleteStream(natsclient.VoiceCommandStream))
	replacement, err := js.AddStream(&original.Config)
	require.NoError(t, err)
	require.False(t, receipt.Identity.StreamCreatedAt.Equal(replacement.Created), "the command stream must have a genuinely new broker birth")
	return replacement.Created
}

// The proxy forwards actual NATS protocol traffic without changing responses.
// It gates one request after the initial owner proof read, so resource replacement
// is ordered between the CLI's successful Verify and its recovery operation.
func ownerRecoveryReceiptGate(ctx context.Context, t *testing.T, brokerURL string) (string, <-chan struct{}, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	verified, resume := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { assert.NoError(t, listener.Close()) })
	t.Cleanup(func() {
		cancel()
		if stop() {
			assert.NoError(t, listener.Close())
		}
		select {
		case proxyErr := <-finished:
			if !errors.Is(proxyErr, net.ErrClosed) && !errors.Is(proxyErr, context.Canceled) {
				assert.NoError(t, proxyErr)
			}
		case <-time.After(3 * time.Second):
			t.Error("receipt gate proxy did not stop")
		}
	})
	go func() {
		client, acceptErr := listener.Accept()
		if acceptErr != nil {
			finished <- acceptErr
			return
		}
		defer func() { closeRecoveryGateConnection(t, client) }()
		broker, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(brokerURL, "nats://"))
		if dialErr != nil {
			finished <- dialErr
			return
		}
		defer func() { closeRecoveryGateConnection(t, broker) }()
		stopConnections := context.AfterFunc(ctx, func() {
			closeRecoveryGateConnection(t, client)
			closeRecoveryGateConnection(t, broker)
		})
		defer stopConnections()
		forwarded := make(chan error, 1)
		go func() { _, copyErr := io.Copy(client, broker); forwarded <- copyErr }()
		forwardErr := forwardRecoveryRequests(ctx, client, broker, verified, resume)
		closeRecoveryGateConnection(t, broker)
		copyErr := <-forwarded
		if errors.Is(copyErr, net.ErrClosed) {
			copyErr = nil
		}
		finished <- errors.Join(forwardErr, copyErr)
	}()
	return "nats://" + listener.Addr().String(), verified, resume
}

func forwardRecoveryRequests(ctx context.Context, client, broker net.Conn, verified, resume chan struct{}) error {
	reader := bufio.NewReader(client)
	readOwner, gated := false, false
	for {
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		fields := strings.Fields(line)
		var payload []byte
		if len(fields) > 0 && (fields[0] == "PUB" || fields[0] == "HPUB") {
			if readOwner && !gated {
				gated = true
				close(verified)
				select {
				case <-resume:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if len(fields) < 3 {
				return errors.New("invalid NATS fixture publish frame")
			}
			if strings.HasPrefix(fields[1], "$JS.API.DIRECT.GET.KV_"+natsclient.VoiceCommandOwnerBucket) {
				readOwner = true
			}
			size, parseErr := strconv.Atoi(fields[len(fields)-1])
			if parseErr != nil || size < 0 || size > 1<<20 {
				return fmt.Errorf("invalid NATS fixture publish size: %w", parseErr)
			}
			payload = make([]byte, size+2)
			if _, err := io.ReadFull(reader, payload); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(broker, line); err != nil {
			return err
		}
		if len(payload) > 0 {
			if _, err := broker.Write(payload); err != nil {
				return err
			}
		}
	}
}

func closeRecoveryGateConnection(t *testing.T, connection net.Conn) {
	t.Helper()
	if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Errorf("close receipt gate connection: %v", err)
	}
}
