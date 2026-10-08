package api

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// permissionNotifierGuardFatal is the guard's fatal message for a missing
// notifier. The subprocess test asserts on it, not only on the exit status.
const permissionNotifierGuardFatal = "MFA handler has no permission change notifier"

// requirePermissionInvalidatorWired's notifier leg must reflect WIRING (#3456).
// An unwired notifier fails OPEN and silently: the factor change commits, the
// cache is bumped, and the user's desktop is never told to re-read.
//
// Kills: HasPermissionChangeNotifier hardcoded true (the unwired arm), nil
// counted as wiring, and the router's SetPermissionChangeNotifier call
// deleted, duplicated or moved below the guard (the source arm).
func TestPermissionNotifierGuardPredicate_ReflectsWiring(t *testing.T) {
	t.Run("unwired handler reports false", func(t *testing.T) {
		assert.False(t, (&mfa.Handler{}).HasPermissionChangeNotifier())
	})

	t.Run("wired handler reports true", func(t *testing.T) {
		h := &mfa.Handler{}
		h.SetPermissionChangeNotifier(newTestNotifierOnly())
		assert.True(t, h.HasPermissionChangeNotifier())
	})

	t.Run("explicitly nil wiring still reports false", func(t *testing.T) {
		h := &mfa.Handler{}
		h.SetPermissionChangeNotifier(nil)
		assert.False(t, h.HasPermissionChangeNotifier(), "SetPermissionChangeNotifier(nil) is not wiring")
	})

	t.Run("router wires the hub adapter before the guard", func(t *testing.T) {
		source, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
		require.NoError(t, err)
		contents := string(source)
		setter := "mfaHandler.SetPermissionChangeNotifier(newPermissionChangeNotifier(hub, log))"
		guard := "requirePermissionInvalidatorWired(log, mfaHandler)"
		require.Equal(t, 1, strings.Count(contents, setter), "the router must wire the notifier exactly once")
		require.Equal(t, 1, strings.Count(contents, guard), "the router must call the guard exactly once")
		assert.Less(t, strings.Index(contents, setter), strings.Index(contents, guard),
			"the wiring must precede the startup guard")
	})
}

func newTestNotifierOnly() mfa.PermissionChangeNotifier {
	n, _, _ := newTestNotifier(true)
	return n
}

// TestRequirePermissionInvalidatorWired_NotifierLeg runs the guard itself,
// log.Fatal included, in a child process, with the INVALIDATOR wired in every
// arm so that only the notifier decides the outcome. The both-wired arm is the
// control: without it, a child that exits non-zero for any reason would pass
// the unwired arm.
//
// Kills: the notifier leg deleted (the missing-notifier arm boots), its
// condition hardcoded false (the control arm dies) or true (the missing arm
// still exits but the control arm dies), and the message changed.
func TestRequirePermissionInvalidatorWired_NotifierLeg(t *testing.T) {
	const childEnv = "CONCORD_PERMISSION_NOTIFIER_GUARD_CHILD"
	switch os.Getenv(childEnv) {
	case "missing-notifier":
		h := &mfa.Handler{}
		h.SetPermissionInvalidator(stubPermissionInvalidator{})
		requirePermissionInvalidatorWired(logger.New("test"), h)
		return
	case "both-wired":
		h := &mfa.Handler{}
		h.SetPermissionInvalidator(stubPermissionInvalidator{})
		h.SetPermissionChangeNotifier(newTestNotifierOnly())
		requirePermissionInvalidatorWired(logger.New("test"), h)
		return
	default:
		// Not a child: this is the parent, which runs both arms below.
	}

	// os.Executable is the running test binary path, not user input.
	testBin, err := os.Executable()
	require.NoError(t, err)
	run := func(t *testing.T, arm string) (string, int) {
		t.Helper()
		// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — testBin is os.Executable() (the running test binary), not user-controlled input; self-invocation pattern for testing log.Fatal
		cmd := exec.Command(testBin, "-test.run=^TestRequirePermissionInvalidatorWired_NotifierLeg$") //nolint:gosec // G204: same rationale
		cmd.Env = append(os.Environ(), childEnv+"="+arm)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		var exitErr *exec.ExitError
		require.True(t, errors.As(err, &exitErr), "child did not run: %v", err)
		return string(out), exitErr.ExitCode()
	}

	t.Run("a wired invalidator without a notifier exits with the notifier message", func(t *testing.T) {
		out, code := run(t, "missing-notifier")
		assert.Equal(t, 1, code, "output: %s", out)
		assert.Contains(t, out, permissionNotifierGuardFatal)
		assert.NotContains(t, out, permissionInvalidatorGuardFatal, "the invalidator leg must not be what fired")
	})
	t.Run("both wired boots", func(t *testing.T) {
		out, code := run(t, "both-wired")
		assert.Equal(t, 0, code, "output: %s", out)
		assert.NotContains(t, out, permissionNotifierGuardFatal)
	})
}
