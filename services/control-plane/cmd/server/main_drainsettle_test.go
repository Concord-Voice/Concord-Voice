package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/health"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// stubDrainSettle replaces the sleep seam and records what it was asked to
// wait for, so the settle is assertable without a test ever really sleeping.
func stubDrainSettle(t *testing.T) *[]time.Duration {
	t.Helper()
	var got []time.Duration
	prev := drainSettleSleep
	drainSettleSleep = func(d time.Duration) { got = append(got, d) }
	t.Cleanup(func() { drainSettleSleep = prev })
	return &got
}

// TestSignalArmHoldsTheListenerOpenForTheDrainSettle.
//
// Latching the drain flag and making the drain OBSERVABLE are different
// things, and only the first was implemented. Measured on a real listener with
// a real SIGTERM and no shrunk constants, the latch-to-listener-close window
// was ~5us: a fresh-connection poller running flat out saw 5111 `200`s and
// ZERO `draining` responses, going straight from 200 to connection-refused.
// The window this issue exists to create was not delivered.
//
// The settle must sit BETWEEN the latch and the deferred shutdown, which is
// what makes it a window rather than a pause.
func TestSignalArmHoldsTheListenerOpenForTheDrainSettle(t *testing.T) {
	got := stubDrainSettle(t)

	// How many settles had happened when the drain latched. Recording the
	// OBSERVED count is the point: the previous version compared against a
	// counter nothing incremented, so it was always 1 and moving beginDrain()
	// after the settle still passed. CodeRabbit caught that.
	settleCountAtDrain := -1
	var settleFinishedBeforeShutdown bool
	stop := make(chan os.Signal, 1)
	stop <- syscall.SIGTERM

	err := runControlPlaneServer(
		func() error { select {} },
		stop,
		func() error {
			// The deferred shutdown -- i.e. srv.Shutdown, which closes the
			// listener. By the time it runs the settle must be complete.
			settleFinishedBeforeShutdown = len(*got) == 1
			return nil
		},
		func() { settleCountAtDrain = len(*got) },
	)
	require.NoError(t, err)

	require.Equal(t, []time.Duration{drainSettle}, *got,
		"the signal arm must wait drainSettle before returning into shutdown")
	require.True(t, settleFinishedBeforeShutdown,
		"the settle ran after srv.Shutdown had already closed the listener, "+
			"which is the same as not having one")
	require.Equal(t, 0, settleCountAtDrain,
		"beginDrain must fire BEFORE the settle. A settle that ran first would "+
			"hold the listener open while /readyz still reported ready, which is "+
			"the window pointed backwards")
	require.GreaterOrEqual(t, drainSettle, 2*time.Second,
		"the settle must outlast the poll cadence of the fastest consumer in "+
			"this repo (concord-ctl.sh wait-healthy, INTERVAL=2) or no observer "+
			"can sample the window")
}

// TestServeErrorArmDoesNotSettle: on the serve-error arm the listener is
// already dead, so a settle there is pure added restart latency with nothing
// able to observe it.
func TestServeErrorArmDoesNotSettle(t *testing.T) {
	got := stubDrainSettle(t)
	drained := false
	err := runControlPlaneServer(
		func() error { return http.ErrServerClosed },
		make(chan os.Signal, 1),
		func() error { return nil },
		func() { drained = true },
	)
	require.NoError(t, err)
	require.False(t, drained, "the serve-error arm must not latch the drain")
	require.Empty(t, *got, "and must not pay the settle")
}

// TestDrainIsObservableFromInsideShutdown pins the ORDERING, which nothing did.
//
// The existing drain test probes /readyz only after runControlPlaneServer has
// fully returned, so it cannot tell "drained first" from "drained last" --
// moving beginDrain() to after shutdown() left it green. This probes from
// INSIDE the shutdown stub, which is the only place the distinction is visible.
func TestDrainIsObservableFromInsideShutdown(t *testing.T) {
	stubDrainSettle(t)
	readiness := health.NewReadiness()
	prober := health.NewProber(
		readiness,
		[]health.Check{{Name: "postgres", Gating: true, Probe: func(context.Context) error { return nil }}},
		5*time.Second, 20*time.Second, time.Now, // production-shaped, deliberately
	)
	pctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go prober.Start(pctx)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/readyz", api.ReadyzHandler(prober))
	probe := func() int {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code
	}

	deadline := time.Now().Add(2 * time.Second)
	for probe() != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("precondition: a healthy prober must reach 200 before the drain")
		}
		time.Sleep(2 * time.Millisecond)
	}

	codeInsideShutdown := 0
	stop := make(chan os.Signal, 1)
	stop <- syscall.SIGTERM
	require.NoError(t, runControlPlaneServer(
		func() error { select {} },
		stop,
		func() error { codeInsideShutdown = probe(); return nil },
		readiness.MarkDraining,
	))

	require.Equal(t, http.StatusServiceUnavailable, codeInsideShutdown,
		"/readyz must already report draining by the time srv.Shutdown runs; "+
			"a drain latched after it is invisible for the entire shutdown")
}
