package api

import (
	"context"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/channels"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// wirePinnedExpiry builds the ONE pinned-expiry terminal (#3459 §8) and hands
// it to both expiration routes. The notifier comes from the same constructor
// main.go's expiry sweeper uses, so a client cannot tell which of the two
// deleted a row. A nil engine is refused here explicitly: a nil *purge.Engine
// inside the ExpiryBatchRunner interface is non-nil to the constructor's own
// check and would panic on the first toggle-on, after the policy committed.
// A construction failure leaves both handlers unwired, which
// requirePinnedExpiryWired then refuses at boot. Extracted from NewRouter,
// which sits at the go:S3776 limit.
func wirePinnedExpiry(
	lifecycleCtx context.Context, log *logger.Logger, engine *purge.Engine, hub *websocket.Hub,
	channelsH *channels.Handler, dmH *dm.Handler,
) {
	if engine == nil {
		log.Error("Pinned expiry has no purge engine")
		return
	}
	notifier, err := expiration.NewHubPurgeNotifier(hub, log)
	if err != nil {
		log.Error("Pinned expiry purge notifier unavailable", "error", err)
		return
	}
	expirer, err := expiration.NewContextExpirer(lifecycleCtx, engine, notifier, log)
	if err != nil {
		log.Error("Pinned expiry terminal unavailable", "error", err)
		return
	}
	channelsH.SetPinnedExpirer(expirer)
	dmH.SetPinnedExpirer(expirer)
}

// requirePinnedExpiryWired fatal-exits when either expiration route lacks the
// pinned-expiry terminal. Unwired, a toggle-on still commits and answers 200,
// and the due pins wait for the next sweep instead of going "right away": a
// silent degradation, which is why boot is where it surfaces. It interrogates
// the HANDLERS, never the values the router holds, as
// requireMessageDeleteGuardWired does.
func requirePinnedExpiryWired(log *logger.Logger, channelsH *channels.Handler, dmH *dm.Handler) {
	if !channelsH.HasPinnedExpiryRunner() {
		log.Fatal("channels handler has no pinned-expiry terminal: turning pinned expiry on would leave due pins for the sweeper")
	}
	if !dmH.HasPinnedExpiryRunner() {
		log.Fatal("dm handler has no pinned-expiry terminal: turning pinned expiry on would leave due pins for the sweeper")
	}
}
