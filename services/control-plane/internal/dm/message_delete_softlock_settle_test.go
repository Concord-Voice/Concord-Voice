package dm

// Regression for the DM delete's post-commit settlement running on the
// request's context (review of #3509). A client that hangs up just after its
// verified delete commits must still have its soft-lock reset and its step-up
// budget cleared; otherwise the committed confirmation leaves both counters
// high, and the member is challenged again or locked out of the shared budget
// for a delete they already confirmed.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

// cancelOnFirstDel cancels the request's context as the first DEL is issued,
// before that DEL runs. The soft-lock charge and the budget charge issue no
// DEL, so the first one is the settlement's Reset: the request goes away
// exactly after the delete committed.
type cancelOnFirstDel struct {
	once   sync.Once
	cancel context.CancelFunc
}

func (h *cancelOnFirstDel) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *cancelOnFirstDel) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "del" {
			h.once.Do(h.cancel)
		}
		return next(ctx, cmd)
	}
}

func (h *cancelOnFirstDel) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// deleteWithContext is delete with ctx as the request's context.
func (hs *softLockHarness) deleteWithContext(ctx context.Context, t *testing.T, messageID, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.DELETE(softLockDeleteRoute, func(c *gin.Context) {
		c.Set("user_id", hs.actor)
		c.Set(middleware.JWTClaimsContextKey, jwt.MapClaims{"cred_epoch": hs.epoch})
		hs.handler.DeleteMessage(c)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete,
		"/dm/conversations/"+hs.convID+"/messages/"+messageID, strings.NewReader(body)).WithContext(ctx)
	router.ServeHTTP(w, req)
	return w
}

// TestDMDeleteSoftLock_SettlementOutlivesTheRequest: the request is cancelled
// as settlement begins, after a verified delete committed. Both post-commit
// writes must still land.
func TestDMDeleteSoftLock_SettlementOutlivesTheRequest(t *testing.T) {
	hs := newSoftLockHarness(t)
	hs.enrollMFA(t)
	hs.overThreshold(t)
	require.NoError(t, hs.mr.Set(hs.budgetKey(), "2"))
	messageID := hs.message(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hs.rdb.AddHook(&cancelOnFirstDel{cancel: cancel})

	w := hs.deleteWithContext(ctx, t, messageID, codeBody(softLockGoodCode))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, hs.messageExists(t, messageID))
	require.Error(t, ctx.Err(), "the request was cancelled as settlement began")
	assertNoKey(t, hs.mr, hs.burstKey(hs.actor), "a committed confirmation resets the soft-lock after a hang-up")
	assertNoKey(t, hs.mr, hs.budgetKey(), "and clears the budget")
}
