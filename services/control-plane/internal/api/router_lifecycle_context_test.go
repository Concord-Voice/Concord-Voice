package api

import (
	"context"
	"testing"
)

func TestNormalizeLifecycleContext(t *testing.T) {
	t.Run("uses a usable background context when omitted", func(t *testing.T) {
		ctx := normalizeLifecycleContext(nil)
		if ctx == nil {
			t.Fatal("nil lifecycle context was not normalized")
		}
		select {
		case <-ctx.Done():
			t.Fatal("fallback lifecycle context is already canceled")
		default:
		}
	})

	t.Run("preserves the caller context", func(t *testing.T) {
		callerCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if got := normalizeLifecycleContext(callerCtx); got != callerCtx {
			t.Fatal("lifecycle context was replaced")
		}
	})
}
