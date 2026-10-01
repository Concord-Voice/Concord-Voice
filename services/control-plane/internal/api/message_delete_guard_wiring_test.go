package api

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/messages"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// stubMessageDeleteMFAVerifier satisfies both stepup.MFATxCodeVerifier
// (messages.Handler.SetMFAVerifier) and the fuller stepup.MFAVerifier
// (dm.HandlerDeps.MFAVerifier); the guard cares only that a non-nil value
// reached the handler.
type stubMessageDeleteMFAVerifier struct{}

func (stubMessageDeleteMFAVerifier) IsEnabled(context.Context, string) bool { return false }

func (stubMessageDeleteMFAVerifier) GetEnabledMethods(context.Context, string) ([]string, error) {
	return nil, nil
}

func (stubMessageDeleteMFAVerifier) VerifyCode(context.Context, string, stepup.Purpose, string) (bool, error) {
	return false, nil
}

func (stubMessageDeleteMFAVerifier) VerifyCodeTx(context.Context, *sql.Tx, string, stepup.Purpose, string) (bool, error) {
	return false, nil
}

const (
	messagesMFAVerifierGuardFatal = "messages handler has no MFA verifier"
	messagesRedisGuardFatal       = "messages handler has no Redis client"
	dmMFAVerifierGuardFatal       = "dm handler has no MFA verifier"
	dmRedisGuardFatal             = "dm handler has no Redis client"
)

func wiredMessagesHandlerForDeleteGuard(verifier, rdb bool) *messages.Handler {
	h := messages.NewHandler(nil, logger.New("test"), nil, nil, nil, nil)
	if verifier {
		h.SetMFAVerifier(stubMessageDeleteMFAVerifier{})
	}
	if rdb {
		// Never dialled: the guard asks only whether a client was wired.
		h.SetRedis(&redis.Client{})
	}
	return h
}

func wiredDMHandlerForDeleteGuard(verifier, rdb bool) *dm.Handler {
	deps := dm.HandlerDeps{Log: logger.New("test")}
	if verifier {
		deps.MFAVerifier = stubMessageDeleteMFAVerifier{}
	}
	if rdb {
		// Never dialled: the guard asks only whether a client was wired.
		deps.Redis = &redis.Client{}
	}
	return dm.NewHandler(deps)
}

// requireMessageDeleteGuardWired's predicates reflect WIRING (#3455), and the
// router calls the setters (messagesHandler's) or passes HandlerDeps
// (dmHandler's) before the guard.
// Kills: either Has* hardcoded true; a router setter/HandlerDeps field
// deleted or moved below the guard.
func TestMessageDeleteGuardPredicate_ReflectsWiring(t *testing.T) {
	assert.False(t, wiredMessagesHandlerForDeleteGuard(false, false).HasMFAVerifier())
	assert.False(t, wiredMessagesHandlerForDeleteGuard(false, false).HasRedis())
	assert.True(t, wiredMessagesHandlerForDeleteGuard(true, true).HasMFAVerifier())
	assert.True(t, wiredMessagesHandlerForDeleteGuard(true, true).HasRedis())

	assert.False(t, wiredDMHandlerForDeleteGuard(false, false).HasMFAVerifier())
	assert.False(t, wiredDMHandlerForDeleteGuard(false, false).HasRedis())
	assert.True(t, wiredDMHandlerForDeleteGuard(true, true).HasMFAVerifier())
	assert.True(t, wiredDMHandlerForDeleteGuard(true, true).HasRedis())

	source, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	contents := string(source)
	guard := "requireMessageDeleteGuardWired(log, messagesHandler, dmHandler)"
	require.Equal(t, 1, strings.Count(contents, guard), "the router must call the guard exactly once")
	for _, setter := range []string{"messagesHandler.SetMFAVerifier(mfaHandler)", "messagesHandler.SetRedis(redis)"} {
		require.Equal(t, 1, strings.Count(contents, setter), setter)
		assert.Less(t, strings.Index(contents, setter), strings.Index(contents, guard),
			"%s must precede the startup guard", setter)
	}
}

// TestRequireMessageDeleteGuardWired_ExitsOnlyWhenUnwired runs the guard,
// log.Fatal included, in a child process. The wired arm is the control.
// Kills: any of the four conditions dropped (its arm boots), the nil-handler
// arms dropped (they panic instead of printing the message), and a guard that
// fatals when both handlers are fully wired.
func TestRequireMessageDeleteGuardWired_ExitsOnlyWhenUnwired(t *testing.T) {
	const childEnv = "CONCORD_MESSAGE_DELETE_GUARD_CHILD"
	wiredMessages := func() *messages.Handler { return wiredMessagesHandlerForDeleteGuard(true, true) }
	wiredDM := func() *dm.Handler { return wiredDMHandlerForDeleteGuard(true, true) }

	switch os.Getenv(childEnv) {
	case "no-messages-verifier":
		requireMessageDeleteGuardWired(logger.New("test"), wiredMessagesHandlerForDeleteGuard(false, true), wiredDM())
		return
	case "no-messages-redis":
		requireMessageDeleteGuardWired(logger.New("test"), wiredMessagesHandlerForDeleteGuard(true, false), wiredDM())
		return
	case "no-dm-verifier":
		requireMessageDeleteGuardWired(logger.New("test"), wiredMessages(), wiredDMHandlerForDeleteGuard(false, true))
		return
	case "no-dm-redis":
		requireMessageDeleteGuardWired(logger.New("test"), wiredMessages(), wiredDMHandlerForDeleteGuard(true, false))
		return
	case "nil-messages-handler":
		requireMessageDeleteGuardWired(logger.New("test"), nil, wiredDM())
		return
	case "nil-dm-handler":
		requireMessageDeleteGuardWired(logger.New("test"), wiredMessages(), nil)
		return
	case "wired":
		requireMessageDeleteGuardWired(logger.New("test"), wiredMessages(), wiredDM())
		return
	}

	// os.Executable is the running test binary path, not user input.
	testBin, err := os.Executable()
	require.NoError(t, err)
	run := func(t *testing.T, arm string) (string, int) {
		t.Helper()
		// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — testBin is os.Executable() (the running test binary), not user-controlled input; self-invocation pattern for testing log.Fatal
		cmd := exec.Command(testBin, "-test.run=^TestRequireMessageDeleteGuardWired_ExitsOnlyWhenUnwired$") //nolint:gosec // G204: same rationale
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
		"no-messages-verifier": messagesMFAVerifierGuardFatal,
		"nil-messages-handler": messagesMFAVerifierGuardFatal,
		"no-messages-redis":    messagesRedisGuardFatal,
		"no-dm-verifier":       dmMFAVerifierGuardFatal,
		"nil-dm-handler":       dmMFAVerifierGuardFatal,
		"no-dm-redis":          dmRedisGuardFatal,
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
		assert.NotContains(t, out, messagesMFAVerifierGuardFatal)
		assert.NotContains(t, out, messagesRedisGuardFatal)
		assert.NotContains(t, out, dmMFAVerifierGuardFatal)
		assert.NotContains(t, out, dmRedisGuardFatal)
	})
}
