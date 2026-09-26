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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// stubPermissionInvalidator is the smallest thing that satisfies the seam; the
// guard cares only that SetPermissionInvalidator ran, never what it stored.
type stubPermissionInvalidator struct{}

func (stubPermissionInvalidator) BumpUserPermissionGeneration(context.Context, string) error {
	return nil
}

// permissionInvalidatorGuardFatal is requirePermissionInvalidatorWired's fatal
// message. The subprocess test asserts on it, not only on the exit status: a
// child that panics or fails for another reason also exits non-zero.
const permissionInvalidatorGuardFatal = "MFA handler has no permission invalidator"

// requirePermissionInvalidatorWired's predicate must reflect WIRING (#3453).
// The handler treats a nil invalidator as a no-op, so an unwired handler fails
// OPEN: removing the last inline factor leaves the member's cached dangerous
// permissions in place until the cache TTL. Same shape, and same reason for
// asking the handler rather than the resolver, as
// TestPresenceRecheckGuardPredicate_ReflectsWiring.
//
// Kills: HasPermissionInvalidator hardcoded true (the unwired arm), and the
// router's SetPermissionInvalidator call deleted or moved below the guard
// (the source arm).
func TestPermissionInvalidatorGuardPredicate_ReflectsWiring(t *testing.T) {
	t.Run("unwired handler reports false", func(t *testing.T) {
		assert.False(t, (&mfa.Handler{}).HasPermissionInvalidator(),
			"a handler that never had SetPermissionInvalidator called must fail the guard")
	})

	t.Run("wired handler reports true", func(t *testing.T) {
		h := &mfa.Handler{}
		h.SetPermissionInvalidator(stubPermissionInvalidator{})
		assert.True(t, h.HasPermissionInvalidator())
	})

	t.Run("explicitly nil wiring still reports false", func(t *testing.T) {
		h := &mfa.Handler{}
		h.SetPermissionInvalidator(nil)
		assert.False(t, h.HasPermissionInvalidator(), "SetPermissionInvalidator(nil) is not wiring")
	})

	t.Run("router wires the resolver before the guard", func(t *testing.T) {
		source, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
		require.NoError(t, err)
		contents := string(source)
		setter := "mfaHandler.SetPermissionInvalidator(rbacResolver)"
		guard := "requirePermissionInvalidatorWired(log, mfaHandler)"
		require.Equal(t, 1, strings.Count(contents, setter), "the router must wire the resolver exactly once")
		require.Equal(t, 1, strings.Count(contents, guard), "the router must call the guard exactly once")
		assert.Less(t, strings.Index(contents, setter), strings.Index(contents, guard),
			"the wiring must precede the startup guard")
	})
}

// TestRequirePermissionInvalidatorWired_ExitsOnlyWhenUnwired runs the guard
// itself, log.Fatal included, in a child process (the pkg/config BE_CRASHER
// pattern), since os.Exit cannot be observed in-process. The wired arm is the
// control: without it, a child that exits non-zero for any reason would pass
// the unwired arms.
//
// Kills: the guard's condition hardcoded false (both unwired arms stay alive),
// hardcoded true (the wired arm dies), and the nil-handler arm dropped (the
// nil arm panics instead of printing the message).
func TestRequirePermissionInvalidatorWired_ExitsOnlyWhenUnwired(t *testing.T) {
	const childEnv = "CONCORD_PERMISSION_INVALIDATOR_GUARD_CHILD"
	switch os.Getenv(childEnv) {
	case "unwired":
		requirePermissionInvalidatorWired(logger.New("test"), &mfa.Handler{})
		return
	case "nil-handler":
		requirePermissionInvalidatorWired(logger.New("test"), nil)
		return
	case "wired":
		h := &mfa.Handler{}
		h.SetPermissionInvalidator(stubPermissionInvalidator{})
		requirePermissionInvalidatorWired(logger.New("test"), h)
		return
	}

	// os.Executable is the running test binary path, not user input.
	testBin, err := os.Executable()
	require.NoError(t, err)
	run := func(t *testing.T, arm string) (string, int) {
		t.Helper()
		// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — testBin is os.Executable() (the running test binary), not user-controlled input; self-invocation pattern for testing log.Fatal
		cmd := exec.Command(testBin, "-test.run=^TestRequirePermissionInvalidatorWired_ExitsOnlyWhenUnwired$") //nolint:gosec // G204: same rationale
		cmd.Env = append(os.Environ(), childEnv+"="+arm)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		var exitErr *exec.ExitError
		require.True(t, errors.As(err, &exitErr), "child did not run: %v", err)
		return string(out), exitErr.ExitCode()
	}

	for arm, name := range map[string]string{"unwired": "unwired handler", "nil-handler": "nil handler"} {
		t.Run(name+" exits with the guard message", func(t *testing.T) {
			out, code := run(t, arm)
			assert.Equal(t, 1, code, "output: %s", out)
			assert.Contains(t, out, permissionInvalidatorGuardFatal)
		})
	}
	t.Run("wired handler boots", func(t *testing.T) {
		out, code := run(t, "wired")
		assert.Equal(t, 0, code, "output: %s", out)
		assert.NotContains(t, out, permissionInvalidatorGuardFatal)
	})
}
