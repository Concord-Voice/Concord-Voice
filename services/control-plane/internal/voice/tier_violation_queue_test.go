package voice_test

// The tier-violation door's queue group, against a live NATS server. It lives
// in package voice_test because testhelpers.SetupTestServer imports the router
// and so this package: an in-package test would be an import cycle.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voice"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

type countingViolationSink struct {
	mu    sync.Mutex
	calls int
}

func (c *countingViolationSink) Record(context.Context, uuid.UUID, entitlements.ViolationReason) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
}

func totalViolationCalls(sinks ...*countingViolationSink) int {
	total := 0
	for _, s := range sinks {
		s.mu.Lock()
		total += s.calls
		s.mu.Unlock()
	}
	return total
}

// Two replicas, one report: exactly one of them counts it. Each replica is its
// own NATS connection with its own sink, so a plain subscription would deliver
// the report to both and the total would be 2. The voice.* wildcard each
// replica also holds receives the same report and must add nothing.
func TestTierViolationReportIsCountedByExactlyOneReplica(t *testing.T) {
	natsURL := natsTestURL()
	connect := func() *natsclient.Client {
		c, err := natsclient.Connect(natsURL)
		if err != nil {
			t.Skipf("NATS unavailable (%v); skipping live queue-group test (runs in CI)", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	replicaClientA, replicaClientB, publisher := connect(), connect(), connect()

	// The UNIQUE QUEUE GROUP below is what makes the count hold: every other
	// control plane on the broker -- the router SetupTestServer builds here, and
	// every other package's test server when CI runs packages in parallel
	// against one shared broker -- joins the production group
	// control-plane.tier-violation, and would take a share of these reports. A
	// group nobody else knows the name of leaves exactly two members.
	queue := "test.tier-violation." + uuid.NewString()
	// Blanking NATS_URL additionally keeps this test's own router off the bus;
	// it is tidiness now, not the isolation.
	t.Setenv("NATS_URL", "")
	ts := testhelpers.SetupTestServer(t)
	sinkA, sinkB := &countingViolationSink{}, &countingViolationSink{}
	for _, replica := range []struct {
		client *natsclient.Client
		sink   *countingViolationSink
	}{{replicaClientA, sinkA}, {replicaClientB, sinkB}} {
		sub := voice.NewNATSSubscriber(ts.DB, logger.New("test"), ts.Hub, replica.client, ts.Redis, nil, nil)
		sub.SetTierViolationRecorder(replica.sink)
		sub.UseTierViolationQueueForTest(queue)
		require.NoError(t, sub.Subscribe())
		t.Cleanup(sub.Close)
		// Interest must reach the server before anything is published.
		require.NoError(t, replica.client.Flush())
	}

	publish := func() {
		require.NoError(t, publisher.Publish("voice.tier_violation", map[string]string{
			"userId": uuid.New().String(),
			"reason": "audio_ptime_below_tier",
		}))
		require.NoError(t, publisher.Flush())
	}

	publish()
	require.Eventually(t, func() bool { return totalViolationCalls(sinkA, sinkB) >= 1 },
		5*time.Second, 10*time.Millisecond, "the report was never counted")
	time.Sleep(300 * time.Millisecond) // a second, wrongful copy would land in this window
	assert.Equal(t, 1, totalViolationCalls(sinkA, sinkB), "one report is counted once across replicas")

	for i := 0; i < 3; i++ {
		publish()
	}
	require.Eventually(t, func() bool { return totalViolationCalls(sinkA, sinkB) >= 4 },
		5*time.Second, 10*time.Millisecond, "the later reports were not all counted")
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 4, totalViolationCalls(sinkA, sinkB), "four reports are counted four times in all")
}
