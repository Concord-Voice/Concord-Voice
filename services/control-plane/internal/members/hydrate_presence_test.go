package members

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// The additive direction degrades rather than failing. Each of these branches is
// a real production state: an unwired snapshot service on a replica that has no
// presence wiring, and a malformed id that must not reach uuid.MustParse-style
// panics inside a request goroutine.
func TestHydrateJoinerPresenceDegradesQuietly(t *testing.T) {
	t.Run("no snapshot service wired", func(t *testing.T) {
		h := &Handler{log: logger.New("test")}
		require.NotPanics(t, func() {
			h.hydrateJoinerPresence(context.Background(), "11111111-1111-1111-1111-111111111111")
		}, "an unwired snapshot service is a safe degrade, not a crash")
	})

	t.Run("malformed viewer id", func(t *testing.T) {
		h := &Handler{log: logger.New("test")}
		require.NotPanics(t, func() {
			h.hydrateJoinerPresence(context.Background(), "not-a-uuid")
		}, "a malformed id is logged and skipped, never panicked on")
	})
}
