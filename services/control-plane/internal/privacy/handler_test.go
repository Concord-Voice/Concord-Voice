package privacy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/privacy"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/users"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// stubAccountDeleter implements users.AccountDeleter for unit tests.
// Configurable to return any error or nil. Records the userID it was
// called with so tests can assert the handler passed the right value.
type stubAccountDeleter struct {
	err            error
	called         bool
	calledWithUser string
}

func (s *stubAccountDeleter) DeleteAccount(_ context.Context, userID string) error {
	s.called = true
	s.calledWithUser = userID
	return s.err
}

// newTestContext returns a Gin context wrapping a recorded response, with
// the given JSON body and "user_id" set in context (matching auth middleware).
func newTestContext(t *testing.T, body string, userID string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/privacy/erase-account", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	if userID != "" {
		c.Set("user_id", userID)
	}
	return c, w
}

// TestNewHandler_PanicsOnNilAccount asserts the constructor's documented
// fail-fast behavior. Mis-wiring a nil dependency must surface at startup,
// not as an opaque nil-pointer dereference at first request.
func TestNewHandler_PanicsOnNilAccount(t *testing.T) {
	require.Panics(t, func() {
		privacy.NewHandler(nil, logger.New("test"))
	})
}

// TestNewHandler_ToleratesNilLogger pins the documented test-only tolerance
// for a nil logger. The log field is only dereferenced behind a nil-check
// on the error path, so construction must not panic.
func TestNewHandler_ToleratesNilLogger(t *testing.T) {
	stub := &stubAccountDeleter{}
	require.NotPanics(t, func() {
		_ = privacy.NewHandler(stub, nil)
	})
}

func TestEraseAccount_Returns401WhenUserIDMissing(t *testing.T) {
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, logger.New("test"))

	c, _ := newTestContext(t, `{}`, "")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusUnauthorized, c.Writer.Status())
	assert.False(t, stub.called, "DeleteAccount must not run when user_id is missing")
}

func TestEraseAccount_Returns400OnInvalidJSON(t *testing.T) {
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, logger.New("test"))

	c, _ := newTestContext(t, `{not-json`, "test-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusBadRequest, c.Writer.Status())
	assert.False(t, stub.called, "DeleteAccount must not run on invalid body")
}

func TestEraseAccount_Returns204OnEmptyBody(t *testing.T) {
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, logger.New("test"))

	c, _ := newTestContext(t, ``, "test-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
	assert.True(t, stub.called)
	assert.Equal(t, "test-user-uuid", stub.calledWithUser)
}

func TestEraseAccount_Returns204OnEmptyJSONBody(t *testing.T) {
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, logger.New("test"))

	c, _ := newTestContext(t, `{}`, "test-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status())
	assert.True(t, stub.called)
}

func TestEraseAccount_Returns404OnUserNotFound(t *testing.T) {
	stub := &stubAccountDeleter{err: users.ErrUserNotFound}
	h := privacy.NewHandler(stub, logger.New("test"))

	c, _ := newTestContext(t, `{}`, "missing-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusNotFound, c.Writer.Status())
}

func TestEraseAccount_Returns500OnUnknownError(t *testing.T) {
	stub := &stubAccountDeleter{err: errors.New("db unavailable")}
	h := privacy.NewHandler(stub, logger.New("test"))

	c, _ := newTestContext(t, `{}`, "test-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusInternalServerError, c.Writer.Status())
}

// TestEraseAccount_IgnoresUnknownClientIdField is the regression guard for
// the #758 transition window: the desktop client (#757 not yet shipped) may
// continue to POST {"clientId":"..."} bodies after the server-side handler
// is Sentry-free. Gin's ShouldBindJSON does not reject unknown fields by
// default, so the handler must accept the extra field and proceed to the
// account-deletion path.
func TestEraseAccount_IgnoresUnknownClientIdField(t *testing.T) {
	const sentinelClientID = "deadbeefcafef00d1122334455667788" //nolint:gosec // pragma: allowlist secret — test sentinel
	stub := &stubAccountDeleter{}
	h := privacy.NewHandler(stub, logger.New("test"))

	body := `{"clientId":"` + sentinelClientID + `"}`
	c, _ := newTestContext(t, body, "test-user-uuid")
	h.EraseAccount(c)

	assert.Equal(t, http.StatusNoContent, c.Writer.Status(),
		"unknown clientId field must be silently ignored — POST returns 204")
	assert.True(t, stub.called,
		"DeleteAccount must still be invoked when the body has an extra field")
}
