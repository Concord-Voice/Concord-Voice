package api

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/channels"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/members"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// gateWiring says which of the four handlers have their verifier and Redis.
// A nil Redis client stands for "not wired"; a non-nil one is never dialled.
type gateWiring struct {
	chV, chR, memV, memR, rbV, rbR, medV, medR, medRes bool
	noMedia                                            bool
}

func allGatesWired() gateWiring {
	return gateWiring{true, true, true, true, true, true, true, true, true, false}
}

func client(wired bool) *redis.Client {
	if wired {
		return &redis.Client{}
	}
	return nil
}

func gateHandlers(w gateWiring) (*channels.Handler, *members.Handler, *rbac.Handler, *media.Handler) {
	log := logger.New("test")
	ch := channels.NewHandler(nil, log, nil, nil, client(w.chR))
	mem := members.NewHandler(nil, log, client(w.memR), nil, nil, nil)
	rb := rbac.NewHandler(nil, log, client(w.rbR), nil, nil, nil, nil)
	if w.chV {
		ch.SetMFAVerifier(stubServersMFAVerifier{})
	}
	if w.memV {
		mem.SetMFAVerifier(stubServersMFAVerifier{})
	}
	if w.rbV {
		rb.SetMFAVerifier(stubServersMFAVerifier{})
	}
	if w.noMedia {
		return ch, mem, rb, nil
	}
	var resolver *rbac.Resolver
	if w.medRes {
		resolver = rbac.NewResolver(nil, nil, log)
	}
	med := media.NewHandler(nil, nil, log, &config.Config{}, resolver, nil)
	med.SetSessionRedis(client(w.medR))
	if w.medV {
		med.SetMFAVerifier(stubServersMFAVerifier{})
	}
	return ch, mem, rb, med
}

// Each missing dependency is named, and a deployment without object storage
// (no media handler) has no media gate to wire.
// Kills: any one condition dropped or hardcoded; media checked when nil.
func TestDangerousActionGateGap_NamesEachGap(t *testing.T) {
	assert.Empty(t, dangerousActionGateGap(gateHandlers(allGatesWired())))
	noMedia := allGatesWired()
	noMedia.noMedia, noMedia.medV, noMedia.medR, noMedia.medRes = true, false, false, false
	assert.Empty(t, dangerousActionGateGap(gateHandlers(noMedia)), "no media handler, no media gate")

	for name, tc := range map[string]struct {
		unwire func(*gateWiring)
		want   string
	}{
		"channels verifier": {func(w *gateWiring) { w.chV = false }, "channels handler has no MFA verifier"},
		"channels redis":    {func(w *gateWiring) { w.chR = false }, "channels handler has no Redis client"},
		"members verifier":  {func(w *gateWiring) { w.memV = false }, "members handler has no MFA verifier"},
		"members redis":     {func(w *gateWiring) { w.memR = false }, "members handler has no Redis client"},
		"rbac verifier":     {func(w *gateWiring) { w.rbV = false }, "rbac handler has no MFA verifier"},
		"rbac redis":        {func(w *gateWiring) { w.rbR = false }, "rbac handler has no Redis client"},
		"media verifier":    {func(w *gateWiring) { w.medV = false }, "media handler has no MFA verifier"},
		"media redis":       {func(w *gateWiring) { w.medR = false }, "media handler has no Redis client"},
		"media resolver":    {func(w *gateWiring) { w.medRes = false }, "media handler has no permission resolver"},
	} {
		t.Run(name, func(t *testing.T) {
			w := allGatesWired()
			tc.unwire(&w)
			assert.Contains(t, dangerousActionGateGap(gateHandlers(w)), tc.want)
		})
	}
	assert.Contains(t, dangerousActionGateGap(nil, nil, nil, nil), "channels handler has no MFA verifier",
		"a nil handler is a gap, not a panic")
}

// The router wires every setter before the guard, calls the guard once, and
// derives the capability from the guard's own predicate.
// Kills: a setter deleted or moved below the guard; the guard call deleted;
// the capability hardcoded or left unset.
func TestDangerousActionGates_RouterWiring(t *testing.T) {
	source, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	require.NoError(t, err)
	contents := string(source)
	guard := "requireDangerousActionGatesWired(log, channelsHandler, membersHandler, rbacHandler, mediaHandler)"
	require.Equal(t, 1, strings.Count(contents, guard), "the router must call the guard exactly once")
	for _, setter := range []string{
		"channelsHandler.SetMFAVerifier(mfaHandler)",
		"membersHandler.SetMFAVerifier(mfaHandler)",
		"rbacHandler.SetMFAVerifier(mfaHandler)",
		"wireMediaHandler(mediaHandler, redis, mfaHandler,",
	} {
		require.Equal(t, 1, strings.Count(contents, setter), setter)
		assert.Less(t, strings.Index(contents, setter), strings.Index(contents, guard),
			"%s must precede the startup guard", setter)
	}
	capability := "SetMFAEnforcedDangerousActions(\n\t\tdangerousActionGateGap(channelsHandler, membersHandler, rbacHandler, mediaHandler) == \"\")"
	require.Equal(t, 1, strings.Count(contents, capability), "the capability must read the guard's predicate")
	assert.Less(t, strings.Index(contents, guard), strings.Index(contents, capability))
}

// The guard, log.Fatal included, in a child process. The wired arm is the
// control. Kills: a guard that never exits, or exits when wired.
func TestRequireDangerousActionGatesWired_ExitsOnlyWhenUnwired(t *testing.T) {
	const childEnv = "CONCORD_DANGEROUS_ACTION_GATES_GUARD_CHILD"
	switch os.Getenv(childEnv) {
	case "wired":
		ch, mem, rb, med := gateHandlers(allGatesWired())
		requireDangerousActionGatesWired(logger.New("test"), ch, mem, rb, med)
		return
	case "unwired":
		w := allGatesWired()
		w.rbV = false
		ch, mem, rb, med := gateHandlers(w)
		requireDangerousActionGatesWired(logger.New("test"), ch, mem, rb, med)
		return
	}

	// os.Executable is the running test binary path, not user input.
	testBin, err := os.Executable()
	require.NoError(t, err)
	run := func(arm string) (string, int) {
		// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — testBin is os.Executable() (the running test binary), not user-controlled input; self-invocation pattern for testing log.Fatal
		cmd := exec.Command(testBin, "-test.run=^TestRequireDangerousActionGatesWired_ExitsOnlyWhenUnwired$") //nolint:gosec // G204: same rationale
		cmd.Env = append(os.Environ(), childEnv+"="+arm)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		var exitErr *exec.ExitError
		require.True(t, errors.As(err, &exitErr), "child did not run: %v", err)
		return string(out), exitErr.ExitCode()
	}

	out, code := run("unwired")
	assert.Equal(t, 1, code, "output: %s", out)
	assert.Contains(t, out, "rbac handler has no MFA verifier")
	out, code = run("wired")
	assert.Equal(t, 0, code, "a wired router must boot: %s", out)
}
