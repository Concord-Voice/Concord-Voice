package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/api"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/health"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestDrainSettleIsObservableOnARealListener is the ACCEPTANCE half of the
// drain settle, and the only test in this package that lets real time pass.
//
// TestSignalArmHoldsTheListenerOpenForTheDrainSettle proves the seam is CALLED
// with drainSettle -- but it proves that by REPLACING the seam, so it is blind
// to every change to what the seam does. Two one-line production edits leave it
// green while deleting the window entirely:
//
//	var drainSettleSleep = func(time.Duration) {}                    // window: ~1ms
//	var drainSettleSleep = func(d time.Duration) { _ = time.After(d) } // window: ~28us
//
// The second is a classic Go footgun, not a strawman. Under both, a
// fresh-connection poller observes ZERO `draining` responses -- exactly the
// state #3130 exists to fix.
//
// So this test asserts the property END TO END: after a real SIGTERM, a real
// TCP listener keeps accepting real fresh connections and answering
// 503 `draining` for at least as long as the fastest in-repo consumer's poll
// cadence (concord-ctl.sh wait-healthy, INTERVAL=2). It costs drainSettle (3s)
// of wall clock, which is the price of the only assertion that cannot be
// satisfied by a stub.
func TestDrainSettleIsObservableOnARealListener(t *testing.T) {
	// Deliberately NOT stubbed. The whole point is that real time passes.
	require.NotNil(t, drainSettleSleep)

	gin.SetMode(gin.TestMode)
	readiness := health.NewReadiness()
	prober := health.NewProber(
		readiness,
		[]health.Check{{Name: "postgres", Gating: true, Probe: func(context.Context) error { return nil }}},
		5*time.Second, 20*time.Second, time.Now, // production-shaped, deliberately
	)
	pctx, pcancel := context.WithCancel(context.Background())
	defer pcancel()
	go prober.Start(pctx)

	router := gin.New()
	router.GET("/readyz", api.ReadyzHandler(prober))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	url := "http://" + ln.Addr().String() + "/readyz"
	srv := &http.Server{Handler: router, ReadHeaderTimeout: 5 * time.Second} // G112

	// DisableKeepAlives: every sample is a fresh TCP connection, so a
	// `draining` observation proves the LISTENER is still bound rather than an
	// already-established connection being drained.
	client := &http.Client{
		Timeout:   500 * time.Millisecond,
		Transport: &http.Transport{DisableKeepAlives: true},
	}

	var drainingSeen, okSeen atomic.Int64
	stopPoll, pollDone, pollReady := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(pollDone)
		<-pollReady
		for {
			select {
			case <-stopPoll:
				return
			default:
			}
			resp, gerr := client.Get(url)
			if gerr != nil {
				time.Sleep(time.Millisecond)
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				okSeen.Add(1)
			}
			var parsed struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(body, &parsed)
			if resp.StatusCode == http.StatusServiceUnavailable && parsed.Status == "draining" {
				drainingSeen.Add(1)
			}
			time.Sleep(50 * time.Millisecond) // ~20 Hz: far cheaper than flat out, still 60 samples
		}
	}()

	var latchedAt, listenerClosedAt time.Time
	shutdown := func() error {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		serr := srv.Shutdown(sctx)
		listenerClosedAt = time.Now()
		return serr
	}

	stop := make(chan os.Signal, 1)
	serveStarted := make(chan struct{})
	go func() {
		<-serveStarted
		deadline := time.Now().Add(5 * time.Second)
		for {
			resp, gerr := client.Get(url)
			if gerr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Errorf("precondition: /readyz never answered 200 before the signal")
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		close(pollReady)
		stop <- syscall.SIGTERM
	}()

	require.NoError(t, runControlPlaneServer(
		func() error { close(serveStarted); return srv.Serve(ln) },
		stop,
		shutdown,
		func() { latchedAt = time.Now(); readiness.MarkDraining() },
	))
	close(stopPoll)
	<-pollDone

	window := listenerClosedAt.Sub(latchedAt)
	t.Logf("latch->listener-close window: %v; fresh-connection samples: 200=%d draining=%d",
		window, okSeen.Load(), drainingSeen.Load())

	require.GreaterOrEqual(t, window, 2*time.Second,
		"the REAL latch->listener-close window collapsed; drainSettleSleep no longer "+
			"sleeps, so the settle is a call with no duration")
	require.Positive(t, drainingSeen.Load(),
		"no external consumer observed a `draining` 503 on a fresh connection; "+
			"the window #3130 exists to create was not delivered")
}
