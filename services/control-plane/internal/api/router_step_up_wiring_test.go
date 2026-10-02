package api_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRouterStepUpRequirementsWiring pins the two pieces of the MFA picker's
// server read that live only in router.go. Without the reader, every login,
// refresh and SSO challenge silently omits default_method — auth.Handler's
// nil reader is the documented "off" state, so no test elsewhere would fail.
// Without the route, the picker reads 404 and treats the server as too old.
func TestRouterStepUpRequirementsWiring(t *testing.T) {
	sourceBytes, err := os.ReadFile("router.go") //nolint:gosec // G304: fixed test-only source path
	require.NoError(t, err)
	source := string(sourceBytes)

	checker := strings.Index(source, "authHandler.SetMFAChecker(mfaHandler)")
	reader := strings.Index(source, "authHandler.SetDefaultMethodReader(stepup.DefaultMethodReader(db))")
	require.NotEqual(t, -1, checker)
	require.NotEqual(t, -1, reader, "the challenges' default_method reader must be wired")

	route := strings.Index(source, `mfaRoutes.GET("/step-up",`)
	require.NotEqual(t, -1, route, "GET /mfa/step-up must stay registered on mfaRoutes")
	registration := source[route:]
	handler := strings.Index(registration, "stepup.RequirementsHandler(db, log),")
	require.NotEqual(t, -1, handler, "the route must serve stepup.RequirementsHandler")
	require.Contains(t, registration[:handler], "middleware.RateLimitByUser(redis, 20, 1*time.Minute)",
		"the 20/min budget is a code constant on this route's own bucket")
}
