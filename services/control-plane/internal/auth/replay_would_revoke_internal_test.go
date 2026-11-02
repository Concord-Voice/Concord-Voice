package auth

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// A failed lineage walk logs a fixed reason and leaves the 401 to the caller.
func TestLogReplayWouldRevokeWalkFailureLogsOnly(t *testing.T) {
	h, _, userID := newRejectPathHandler(t)
	var buf bytes.Buffer
	h.log = logger.NewWithWriter(&buf)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil).WithContext(ctx)

	h.logReplayWouldRevoke(c, uuid.NewString(), userID.String())

	assert.Contains(t, buf.String(), "error_class=replay_lineage_walk")
	assert.NotContains(t, buf.String(), "would_revoke")
	assert.Equal(t, http.StatusOK, w.Code, "the helper never writes a response")
}
