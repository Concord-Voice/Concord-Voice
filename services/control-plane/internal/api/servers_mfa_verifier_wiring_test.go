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

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/servers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// stubServersMFAVerifier satisfies servers.MFAVerifier; the guard cares only
// that SetMFAVerifier ran.
type stubServersMFAVerifier struct{}

func (stubServersMFAVerifier) GetEnabledMethods(context.Context, string) ([]string, error) {
	return nil, nil
}

func (stubServersMFAVerifier) VerifyCodeTx(context.Context, *sql.Tx, string, string) (bool, error) {
	return false, nil
}

const (
	serversMFAVerifierGuardFatal = "servers handler has no MFA verifier"
	serversMFARedisGuardFatal    = "servers handler has no Redis client"
)

func wiredServersHandler(verifier, rdb bool) *servers.Handler {
	h := servers.NewHandler(nil, logger.New("test"), nil, nil, nil, nil)
	if verifier {
		h.SetMFAVerifier(stubServersMFAVerifier{})
	}
	if rdb {
		// Never dialled: the guard asks only whether a client was wired.
		h.SetRedis(&redis.Client{})
	}
	return h
}

// requireServersMFAVerifierWired's predicates reflect WIRING (#3453), and the
// router calls the setters before the guard.
// Kills: either Has* hardcoded true; a router setter deleted or moved below
// the guard.
func TestServersMFAVerifierGuardPredicate_ReflectsWiring(t *testing.T) {
	assert.False(t, wiredServersHandler(false, false).HasMFAVerifier())
	assert.False(t, wiredServersHandler(false, false).HasRedis())
	assert.True(t, wiredServersHandler(true, true).HasMFAVerifier())
	assert.True(t, wiredServersHandler(true, true).HasRedis())

	source, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	contents := string(source)
	guard := "requireServersMFAVerifierWired(log, serversHandler)"
	require.Equal(t, 1, strings.Count(contents, guard), "the router must call the guard exactly once")
	for _, setter := range []string{"serversHandler.SetMFAVerifier(mfaHandler)", "serversHandler.SetRedis(redis)"} {
		require.Equal(t, 1, strings.Count(contents, setter), setter)
		assert.Less(t, strings.Index(contents, setter), strings.Index(contents, guard),
			"%s must precede the startup guard", setter)
	}
}

// TestRequireServersMFAVerifierWired_ExitsOnlyWhenUnwired runs the guard,
// log.Fatal included, in a child process. The wired arm is the control.
// Kills: either condition dropped (its arm boots), the nil-handler arm dropped
// (it panics instead of printing the message), and a guard that fatals when
// wired.
func TestRequireServersMFAVerifierWired_ExitsOnlyWhenUnwired(t *testing.T) {
	const childEnv = "CONCORD_SERVERS_MFA_VERIFIER_GUARD_CHILD"
	switch os.Getenv(childEnv) {
	case "no-verifier":
		requireServersMFAVerifierWired(logger.New("test"), wiredServersHandler(false, true))
		return
	case "no-redis":
		requireServersMFAVerifierWired(logger.New("test"), wiredServersHandler(true, false))
		return
	case "nil-handler":
		requireServersMFAVerifierWired(logger.New("test"), nil)
		return
	case "wired":
		requireServersMFAVerifierWired(logger.New("test"), wiredServersHandler(true, true))
		return
	}

	// os.Executable is the running test binary path, not user input.
	testBin, err := os.Executable()
	require.NoError(t, err)
	run := func(t *testing.T, arm string) (string, int) {
		t.Helper()
		// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — testBin is os.Executable() (the running test binary), not user-controlled input; self-invocation pattern for testing log.Fatal
		cmd := exec.Command(testBin, "-test.run=^TestRequireServersMFAVerifierWired_ExitsOnlyWhenUnwired$") //nolint:gosec // G204: same rationale
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
		"no-verifier": serversMFAVerifierGuardFatal,
		"nil-handler": serversMFAVerifierGuardFatal,
		"no-redis":    serversMFARedisGuardFatal,
	} {
		t.Run(arm+" exits with its guard message", func(t *testing.T) {
			out, code := run(t, arm)
			assert.Equal(t, 1, code, "output: %s", out)
			assert.Contains(t, out, want)
		})
	}
	t.Run("wired handler boots", func(t *testing.T) {
		out, code := run(t, "wired")
		assert.Equal(t, 0, code, "output: %s", out)
		assert.NotContains(t, out, serversMFAVerifierGuardFatal)
		assert.NotContains(t, out, serversMFARedisGuardFatal)
	})
}

// The toggle's route is in the Nightwatch allowlist under its frozen template,
// so every 401/403 on it produces the privileged_route_denied fallback.
// Kills: the nightwatchRoutes entry removed (no fallback at all) or the
// fallback-events entry removed (the generic security_control fallback).
func TestNightwatchMFAEnforcementRouteIsObserved(t *testing.T) {
	route, ok := nightwatchRouteTemplate("PUT", "/api/v1/servers/:id/mfa-enforcement")
	require.True(t, ok, "the PUT must be on the Nightwatch allowlist")
	require.Equal(t, securityevent.RouteServerMFAEnforcement, route)
	require.Equal(t, securityevent.RouteTemplate("PUT /api/v1/servers/:id/mfa-enforcement"), route,
		"the template is frozen: the Nightwatch normalizer admits exactly this string")
	_, readObserved := nightwatchRouteTemplate("GET", "/api/v1/servers/:id/mfa-enforcement")
	require.False(t, readObserved, "the GET is a read and is not a privileged route")
}
