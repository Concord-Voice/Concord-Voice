package servers

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// disconnectServerAudience runs post-commit, so it must never panic or block a
// response. Both early returns are real states: a handler constructed without a
// hub, and a server whose only member was the owner already removed by cascade.
func TestDisconnectServerAudienceEarlyReturns(t *testing.T) {
	t.Run("no hub wired", func(t *testing.T) {
		h := &Handler{log: logger.New("test")}
		require.NotPanics(t, func() {
			h.disconnectServerAudience(context.Background(), []uuid.UUID{uuid.New()})
		})
	})

	t.Run("empty audience", func(t *testing.T) {
		h := &Handler{log: logger.New("test")}
		require.NotPanics(t, func() {
			h.disconnectServerAudience(context.Background(), nil)
		}, "an empty captured audience is a no-op, not an error")
	})
}
