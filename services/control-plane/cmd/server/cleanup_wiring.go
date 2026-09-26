package main

import (
	"context"
	"sync"
)

// preflightRunner is one step of the pre-bind DM cleanup.
type preflightRunner interface {
	RunPreflight(context.Context) error
}

// runDMCleanupPreflight fixes the pre-bind order expiry -> clear reap ->
// retirement (#3462 D12). The reap runs after expiry so it sees expiry's
// deletions, and before retirement so a conversation it empties retires in the
// same pass. Typed parameters, not a variadic list, so no caller can reorder it.
func runDMCleanupPreflight(ctx context.Context, expiry, clearReap, retirement preflightRunner) error {
	for _, step := range []preflightRunner{expiry, clearReap, retirement} {
		if err := step.RunPreflight(ctx); err != nil {
			return err
		}
	}
	return nil
}

// startCleanupWorkers derives the WaitGroup count from the worker list, so the
// count cannot drift from the goroutines it waits on (#3462 D11).
func startCleanupWorkers(ctx context.Context, wg *sync.WaitGroup, workers ...func(context.Context)) {
	wg.Add(len(workers))
	for _, worker := range workers {
		go func() {
			defer wg.Done()
			worker(ctx)
		}()
	}
}
