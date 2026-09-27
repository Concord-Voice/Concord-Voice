package dm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

func mappingHandler() *Handler {
	return &Handler{log: logger.NewWithWriter(io.Discard)}
}

func mappingContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	return c, w
}

func responseError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body["error"]
}

// A guard refusal is a retryable denial only while nothing was deleted. Once a
// batch has committed, the same refusal must surface as a partial purge (500),
// because the deletes cannot be undone.
func TestHandleDMPurgeRunError_RefusesOnlyBeforeAnyDelete(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		body   string
	}{
		{errDMPurgeNotParticipant, http.StatusForbidden, errMsgNotParticipant},
		{errDMPurgeScopeChanged, http.StatusForbidden, errMsgNotParticipant},
		{dmblock.ErrUnavailable, http.StatusForbidden, "dm_unavailable"},
		{dmblock.ErrMembershipChanged, http.StatusForbidden, "dm_unavailable"},
		{credepoch.ErrEpochMismatch, http.StatusUnauthorized, "Authentication required"},
		{credepoch.ErrBlocked, http.StatusUnauthorized, "Authentication required"},
		{errors.New("batch failed"), http.StatusInternalServerError, errMsgPurgeFailed},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			c, w := mappingContext()
			mappingHandler().handleDMPurgeRunError(c, uuid.NewString(), uuid.NewString(), "1h", 0, fmt.Errorf("guard: %w", tc.err))
			assert.Equal(t, tc.status, w.Code)
			assert.Equal(t, tc.body, responseError(t, w))

			c, w = mappingContext()
			mappingHandler().handleDMPurgeRunError(c, uuid.NewString(), uuid.NewString(), "1h", 3, fmt.Errorf("guard: %w", tc.err))
			assert.Equal(t, http.StatusInternalServerError, w.Code, "after a committed batch every error is a partial purge")
			assert.Equal(t, errMsgPurgeFailed, responseError(t, w))
		})
	}
}

// The lease writers fail closed: no store, a nil identity, or a store error
// each returns an error, and a missing lease is a conflict rather than a write.
func TestDMVoiceLeaseWriters_FailClosed(t *testing.T) {
	ctx := context.Background()
	conv, call, user := uuid.New(), uuid.New(), uuid.New()
	lease := VoiceCallLease{ConversationID: conv, CallID: call, CallerUserID: user, MediaAuthorized: true}

	live := miniredis.RunT(t)
	liveClient := redis.NewClient(&redis.Options{Addr: live.Addr()})
	t.Cleanup(func() { _ = liveClient.Close() })
	dead := miniredis.RunT(t)
	deadClient := redis.NewClient(&redis.Options{Addr: dead.Addr(), MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = deadClient.Close() })
	dead.Close()

	writers := map[string]func(*redis.Client, uuid.UUID) error{
		"activate": func(c *redis.Client, id uuid.UUID) error {
			return ActivateAcceptedDMVoiceCallLease(ctx, c, id, call, time.Minute)
		},
		"mark": func(c *redis.Client, id uuid.UUID) error {
			return MarkDMVoiceCallMediaAuthorized(ctx, c, id, call)
		},
		"retract": func(c *redis.Client, id uuid.UUID) error {
			return RetractDMVoiceJoinAuthorization(ctx, c, id, user, call, uuid.Nil, true)
		},
		"restore": func(c *redis.Client, id uuid.UUID) error {
			restored := lease
			restored.ConversationID = id
			return RestoreAbortedDMVoiceCallReservation(ctx, c, restored)
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, write(nil, conv), "no store")
			assert.Error(t, write(liveClient, uuid.Nil), "nil identity")
			err := write(deadClient, conv)
			require.Error(t, err, "store error")
			assert.NotErrorIs(t, err, ErrDMVoiceCallLeaseConflict, "a store error is not a conflict")
		})
	}

	for name, write := range map[string]func() error{
		"activate": func() error { return ActivateAcceptedDMVoiceCallLease(ctx, liveClient, conv, call, time.Minute) },
		"mark":     func() error { return MarkDMVoiceCallMediaAuthorized(ctx, liveClient, conv, call) },
	} {
		assert.ErrorIs(t, write(), ErrDMVoiceCallLeaseConflict, "%s without a lease", name)
	}
}
