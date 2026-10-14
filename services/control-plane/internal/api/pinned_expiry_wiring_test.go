package api

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/channels"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	channelsPinnedExpiryGuardFatal = "channels handler has no pinned-expiry terminal"
	dmPinnedExpiryGuardFatal       = "dm handler has no pinned-expiry terminal"
)

// pinnedExpiryHandlers returns a fresh channels and DM handler pair, wired
// through wirePinnedExpiry with the given engine and hub. Neither is ever run.
func pinnedExpiryHandlers(engine *purge.Engine, hub *websocket.Hub) (*channels.Handler, *dm.Handler) {
	log := logger.New("test")
	ch := channels.NewHandler(nil, log, nil, nil, nil)
	dmH := dm.NewHandler(dm.HandlerDeps{Log: log})
	wirePinnedExpiry(context.Background(), log, engine, hub, ch, dmH)
	return ch, dmH
}

// wirePinnedExpiry wires BOTH handlers, and wires neither when a dependency is
// missing, a typed-nil engine or hub included, so the boot guard sees the gap.
// Kills: the nil-engine check dropped (a typed-nil runner would be wired and
// panic on the first toggle-on); either setter dropped; HasPinnedExpiryRunner
// hardcoded true.
func TestWirePinnedExpiry_WiresBothOrNeither(t *testing.T) {
	ch, dmH := pinnedExpiryHandlers(&purge.Engine{}, &websocket.Hub{})
	assert.True(t, ch.HasPinnedExpiryRunner())
	assert.True(t, dmH.HasPinnedExpiryRunner())

	for name, deps := range map[string]struct {
		engine *purge.Engine
		hub    *websocket.Hub
	}{
		"nil engine": {nil, &websocket.Hub{}},
		"nil hub":    {&purge.Engine{}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			ch, dmH := pinnedExpiryHandlers(deps.engine, deps.hub)
			assert.False(t, ch.HasPinnedExpiryRunner())
			assert.False(t, dmH.HasPinnedExpiryRunner())
		})
	}
	var nilChannels *channels.Handler
	var nilDM *dm.Handler
	assert.False(t, nilChannels.HasPinnedExpiryRunner())
	assert.False(t, nilDM.HasPinnedExpiryRunner())
}

// The router wires the expirer once, before the guard, after both handlers
// and the purge engine exist. Kills: the wiring or the guard call deleted, or
// the wiring moved below the guard.
func TestPinnedExpiry_RouterWiring(t *testing.T) {
	source, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	contents := string(source)
	wire := "wirePinnedExpiry(lifecycleCtx, log, purgeEngine, hub, channelsHandler, dmHandler)"
	guard := "requirePinnedExpiryWired(log, channelsHandler, dmHandler)"
	require.Equal(t, 1, strings.Count(contents, wire))
	require.Equal(t, 1, strings.Count(contents, guard), "the router must call the guard exactly once")
	assert.Less(t, strings.Index(contents, wire), strings.Index(contents, guard))
	assert.Less(t, strings.Index(contents, "purgeEngine := dependencies.PurgeEngine"), strings.Index(contents, wire))
	assert.Less(t, strings.Index(contents, "dmHandler := dm.NewHandler("), strings.Index(contents, wire))
}

// The guard, log.Fatal included, in a child process. The wired arm is the
// control. Kills: either condition dropped (its arm boots), or a guard that
// fatals when both handlers are wired.
func TestRequirePinnedExpiryWired_ExitsOnlyWhenUnwired(t *testing.T) {
	const childEnv = "CONCORD_PINNED_EXPIRY_GUARD_CHILD"
	wiredCh, wiredDM := pinnedExpiryHandlers(&purge.Engine{}, &websocket.Hub{})
	bareCh, bareDM := pinnedExpiryHandlers(nil, nil)
	switch os.Getenv(childEnv) {
	case "wired":
		requirePinnedExpiryWired(logger.New("test"), wiredCh, wiredDM)
		return
	case "no-channels":
		requirePinnedExpiryWired(logger.New("test"), bareCh, wiredDM)
		return
	case "no-dm":
		requirePinnedExpiryWired(logger.New("test"), wiredCh, bareDM)
		return
	case "nil-handlers":
		requirePinnedExpiryWired(logger.New("test"), nil, nil)
		return
	}

	// os.Executable is the running test binary path, not user input.
	testBin, err := os.Executable()
	require.NoError(t, err)
	run := func(t *testing.T, arm string) (string, int) {
		t.Helper()
		// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — testBin is os.Executable() (the running test binary), not user-controlled input; self-invocation pattern for testing log.Fatal
		cmd := exec.Command(testBin, "-test.run=^TestRequirePinnedExpiryWired_ExitsOnlyWhenUnwired$") //nolint:gosec // G204: same rationale
		cmd.Env = append(os.Environ(), childEnv+"="+arm)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		var exitErr *exec.ExitError
		require.True(t, errors.As(err, &exitErr), "child did not run: %v", err)
		return string(out), exitErr.ExitCode()
	}
	for arm, want := range map[string]string{
		"no-channels":  channelsPinnedExpiryGuardFatal,
		"no-dm":        dmPinnedExpiryGuardFatal,
		"nil-handlers": channelsPinnedExpiryGuardFatal,
	} {
		t.Run(arm+" exits with its guard message", func(t *testing.T) {
			out, code := run(t, arm)
			assert.Equal(t, 1, code, "output: %s", out)
			assert.Contains(t, out, want)
		})
	}
	t.Run("wired handlers boot", func(t *testing.T) {
		out, code := run(t, "wired")
		assert.Equal(t, 0, code, "output: %s", out)
		assert.NotContains(t, out, channelsPinnedExpiryGuardFatal)
		assert.NotContains(t, out, dmPinnedExpiryGuardFatal)
	})
}
