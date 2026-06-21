package voice_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/voice"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// natsTestURL returns the NATS URL for integration tests (dev default: no auth).
func natsTestURL() string {
	if u := os.Getenv("NATS_URL"); u != "" {
		return u
	}
	return "nats://localhost:4222"
}

// TestPublishForceDisconnect_PublishesPayload verifies the new voice.enforce.disconnect
// publisher (#487 P3) emits {channelId, userId} on the correct subject. Requires a live
// NATS server (dev env / CI); skips if NATS is unreachable.
func TestPublishForceDisconnect_PublishesPayload(t *testing.T) {
	// Control-side client passed into the subscriber (the publisher).
	pubClient, err := natsclient.Connect(natsTestURL())
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping live publish test (runs in CI)", err)
	}
	t.Cleanup(pubClient.Close)

	// Observer client subscribes to capture the published message.
	obsClient, err := natsclient.Connect(natsTestURL())
	require.NoError(t, err)
	t.Cleanup(obsClient.Close)

	ts := testhelpers.SetupTestServer(t)
	sub := voice.NewNATSSubscriber(ts.DB, logger.New("test"), ts.Hub, pubClient, nil)

	received := make(chan []byte, 1)
	natsSub, err := obsClient.Subscribe("voice.enforce.disconnect", func(data []byte) {
		received <- data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = natsSub.Unsubscribe() })

	// Flush the observer connection so the subscription interest has reached the
	// server before we publish — otherwise a fast publish can race ahead of the
	// subscription registration and the message is dropped (intermittent timeout).
	require.NoError(t, obsClient.Flush())

	const channelID = "11111111-1111-1111-1111-111111111111"
	const userID = "22222222-2222-2222-2222-222222222222"
	sub.PublishForceDisconnect(channelID, userID)

	select {
	case data := <-received:
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &payload))
		assert.Equal(t, channelID, payload["channelId"], "channelId should be in the payload")
		assert.Equal(t, userID, payload["userId"], "userId should be in the payload")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for voice.enforce.disconnect message")
	}
}

// TestPublishForceDisconnect_NilNATSNoop verifies the publisher is a safe no-op
// when the NATS client is nil (the test/default construction path), mirroring the
// publishEnforcementFlags nil guard.
func TestPublishForceDisconnect_NilNATSNoop(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	sub := voice.NewNATSSubscriber(ts.DB, logger.New("test"), ts.Hub, nil, nil)
	// Must not panic when nats is nil.
	sub.PublishForceDisconnect("chan", "user")
}
