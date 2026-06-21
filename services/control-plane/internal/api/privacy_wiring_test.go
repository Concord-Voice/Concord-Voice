//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// TestBuildPrivacyHandler_ConstructsHandler verifies the slim, Sentry-free
// wiring constructs a non-nil privacy handler. The construction path does
// not dereference the *sql.DB (it just embeds it in users.NewAccountService),
// so a nil db is sufficient for this unit test. Deeper assertions about
// handler behavior live in internal/privacy/handler_test.go.
func TestBuildPrivacyHandler_ConstructsHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := logger.New("test")

	h := buildPrivacyHandler(nil, log)
	require.NotNil(t, h, "buildPrivacyHandler must return a non-nil handler")
}

// TestBuildPrivacyHandler_NilLogger pins the documented tolerance for a
// nil logger so the handler can be constructed in test contexts that don't
// exercise the failure path. Production callers must always pass a logger.
func TestBuildPrivacyHandler_NilLogger(t *testing.T) {
	gin.SetMode(gin.TestMode)

	require.NotPanics(t, func() {
		h := buildPrivacyHandler(nil, nil)
		require.NotNil(t, h)
	})
}
