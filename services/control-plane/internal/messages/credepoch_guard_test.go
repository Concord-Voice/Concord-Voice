package messages

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

func newGuardTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
	return c, w
}

// #2201 review: a GuardTx epoch-fence rejection is the middleware-identical
// generic 401; any other (store/lock) failure is a logged 500 — never a 401
// that tells the client to re-authenticate over a transient server error and
// leaves no log trail. Mirrors channels.respondKeyDistributionError.
func TestRespondGuardTxError_Mapping(t *testing.T) {
	h := &Handler{log: logger.NewWithWriter(io.Discard)}

	t.Run("epoch mismatch is the generic 401", func(t *testing.T) {
		c, w := newGuardTestContext()
		h.respondGuardTxError(c, credepoch.ErrEpochMismatch, errMsgFailedSendMessage)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), "Authentication required")
	})

	t.Run("blocked is the generic 401", func(t *testing.T) {
		c, w := newGuardTestContext()
		h.respondGuardTxError(c, credepoch.ErrBlocked, errMsgFailedSendMessage)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("a store failure is a logged 500 with the generic message", func(t *testing.T) {
		c, w := newGuardTestContext()
		h.respondGuardTxError(c, errors.New("credepoch: guard read: connection refused"), errMsgFailedUpdateMessage)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Contains(t, w.Body.String(), errMsgFailedUpdateMessage)
	})
}
