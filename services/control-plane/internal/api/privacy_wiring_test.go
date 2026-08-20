//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

// TestBuildPrivacyHandler_ConstructsHandler verifies the slim, Sentry-free
// wiring constructs a non-nil privacy handler. The construction path does
// not dereference the *sql.DB (it just embeds it in users.NewAccountService),
// so a nil db is sufficient for this unit test. Deeper assertions about
// handler behavior live in internal/privacy/handler_test.go.
func TestBuildPrivacyHandler_ConstructsHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := logger.New("test")

	h, account := buildPrivacyHandler(nil, nil, log, nil, nil, nil, nil)
	require.NotNil(t, h, "buildPrivacyHandler must return a non-nil handler")
	require.NotNil(t, account, "the account service is returned so the boot guard can check it")
	require.False(t, account.HasGraphPresenceCapture(),
		"a nil capture must leave the service reporting unwired rather than typed-nil wired")
}

// TestBuildPrivacyHandler_NilLogger pins the documented tolerance for a
// nil logger so the handler can be constructed in test contexts that don't
// exercise the failure path. Production callers must always pass a logger.
func TestBuildPrivacyHandler_NilLogger(t *testing.T) {
	gin.SetMode(gin.TestMode)

	require.NotPanics(t, func() {
		h, account := buildPrivacyHandler(nil, nil, nil, nil, nil, nil, nil)
		require.NotNil(t, h)
		require.NotNil(t, account)
	})
}

// A nil client leaves the publisher unwired, which the test above pins. This one
// pins the other direction: with a client present, buildPrivacyHandler must
// actually call SetNATS. Without it, removing or misdirecting that line would go
// unnoticed — and the publish is the ONLY mechanism that retracts an erased
// user's Custom Status anywhere (CodeRabbit, PR #2840).
func TestBuildPrivacyHandler_WiresTheErasureClearPublisher(t *testing.T) {
	gin.SetMode(gin.TestMode)

	_, account := buildPrivacyHandler(nil, nil, logger.New("test"), nil, nil, nil, &natsclient.Client{})

	require.True(t, account.HasErasureClearPublisher(),
		"buildPrivacyHandler must wire the erasure-clear publisher when a client exists")
}
