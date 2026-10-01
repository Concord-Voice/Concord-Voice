package mfaenforce_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
)

// TestWriteBusy pins the one lock-conflict 503 every gate caller writes: the
// copy, the lock_conflict flag a client keys on without matching copy (#3455
// X17), and Retry-After: 1. The body is spelled out so a changed constant
// fails here too.
func TestWriteBusy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	mfaenforce.WriteBusy(c)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Equal(t, "1", w.Header().Get("Retry-After"))
	require.JSONEq(t, `{"error":"The server is busy. Try again.","lock_conflict":true}`, w.Body.String())
	require.Equal(t, "The server is busy. Try again.", mfaenforce.ErrMsgBusy)
}
