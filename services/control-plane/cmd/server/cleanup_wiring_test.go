package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recordingPreflight struct {
	name  string
	calls *[]string
	err   error
}

func (r recordingPreflight) RunPreflight(context.Context) error {
	*r.calls = append(*r.calls, r.name)
	return r.err
}

func TestRunDMCleanupPreflightOrderIsExpiryReapRetirement(t *testing.T) {
	var calls []string
	err := runDMCleanupPreflight(context.Background(),
		recordingPreflight{"expiry", &calls, nil},
		recordingPreflight{"reap", &calls, nil},
		recordingPreflight{"retirement", &calls, nil})
	require.NoError(t, err)
	require.Equal(t, []string{"expiry", "reap", "retirement"}, calls)
}

func TestRunDMCleanupPreflightStopsAtFirstError(t *testing.T) {
	var calls []string
	boom := errors.New("boom")
	err := runDMCleanupPreflight(context.Background(),
		recordingPreflight{"expiry", &calls, nil},
		recordingPreflight{"reap", &calls, boom},
		recordingPreflight{"retirement", &calls, nil})
	require.ErrorIs(t, err, boom)
	require.Equal(t, []string{"expiry", "reap"}, calls, "retirement must not run after a failed reap")
}

func TestStartCleanupWorkersAddsOnePerWorkerAndEachFinishes(t *testing.T) {
	var wg sync.WaitGroup
	var ran atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	worker := func(ctx context.Context) {
		ran.Add(1)
		<-ctx.Done()
	}
	startCleanupWorkers(ctx, &wg, worker, worker, worker, worker, worker)
	cancel()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup workers did not all finish")
	}
	require.EqualValues(t, 5, ran.Load())
}

func TestStartCleanupWorkersWithNoWorkersDoesNotBlockWait(_ *testing.T) {
	var wg sync.WaitGroup
	startCleanupWorkers(context.Background(), &wg)
	wg.Wait() // returns immediately: Add(0)
}
