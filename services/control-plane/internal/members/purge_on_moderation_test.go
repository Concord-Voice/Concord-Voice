package members

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/messages"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// fakePurger is a hand-written double for serverMessagePurger — the moderation handlers only
// touch the injected purger + logger, so applyPurgeOnModeration is unit-testable with no DB.
type fakePurger struct {
	called                                      bool
	gotServerID, gotActor, gotTarget, gotReason string
	gotCtxErr                                   error // ctx.Err() observed at call time
	gotDeadline                                 time.Time
	retCount                                    int
	retStatus                                   messages.PurgeStatus
	retErr                                      error
}

func (f *fakePurger) PurgeUserServerMessages(ctx context.Context, serverID, actorID, target, reason string) (int, messages.PurgeStatus, error) {
	f.called = true
	f.gotServerID, f.gotActor, f.gotTarget, f.gotReason = serverID, actorID, target, reason
	f.gotCtxErr = ctx.Err()
	f.gotDeadline, _ = ctx.Deadline()
	return f.retCount, f.retStatus, f.retErr
}

func testHandlerWithPurger(p serverMessagePurger) *Handler {
	return &Handler{log: logger.NewWithWriter(io.Discard), purger: p}
}

// testHandlerWithRedis is testHandlerWithPurger plus a live (miniredis) client, so the
// fail-closed rate-limit gate in applyPurgeOnModeration is exercised.
func testHandlerWithRedis(t *testing.T, p serverMessagePurger) (*Handler, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &Handler{log: logger.NewWithWriter(io.Discard), purger: p, redis: rdb}, mr
}

func TestApplyPurgeOnModeration_Completed(t *testing.T) {
	fp := &fakePurger{retCount: 3, retStatus: messages.PurgeCompleted}
	out := testHandlerWithPurger(fp).applyPurgeOnModeration(context.Background(), "srv", "mod", "victim", "ban")
	assert.True(t, fp.called)
	assert.Equal(t, "srv", fp.gotServerID)
	assert.Equal(t, "mod", fp.gotActor)
	assert.Equal(t, "victim", fp.gotTarget)
	assert.Equal(t, "ban", fp.gotReason)
	assert.Equal(t, purgeOutcome{Requested: true, Status: messages.PurgeCompleted, PurgedCount: 3}, out)
}

func TestApplyPurgeOnModeration_SkippedUnauthorized(t *testing.T) {
	fp := &fakePurger{retStatus: messages.PurgeSkippedUnauthorized}
	out := testHandlerWithPurger(fp).applyPurgeOnModeration(context.Background(), "srv", "mod", "victim", "kick")
	assert.Equal(t, purgeOutcome{Requested: true, Status: messages.PurgeSkippedUnauthorized}, out)
}

func TestApplyPurgeOnModeration_Failed(t *testing.T) {
	fp := &fakePurger{retCount: 3, retStatus: messages.PurgeFailed, retErr: errors.New("boom")}
	out := testHandlerWithPurger(fp).applyPurgeOnModeration(context.Background(), "srv", "mod", "victim", "ban")
	assert.Equal(t, purgeOutcome{Requested: true, Status: messages.PurgeFailed, PurgedCount: 3}, out)
}

func TestApplyPurgeOnModeration_NilPurger(t *testing.T) {
	out := testHandlerWithPurger(nil).applyPurgeOnModeration(context.Background(), "srv", "mod", "victim", "ban")
	assert.Equal(t, purgeOutcome{Requested: true, Status: messages.PurgeFailed}, out)
}

// bindOptionalBody: empty body OK, valid body binds, malformed non-empty body rejected (P1,
// #1353 review Codex) — a truncated `{"purge_messages":true,` must NOT proceed with the flag set.
func TestBindOptionalBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newCtx := func(body string) (*gin.Context, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		return c, w
	}
	t.Run("empty body proceeds with zero value", func(t *testing.T) {
		var req RemoveMemberRequest
		c, w := newCtx("")
		assert.True(t, bindOptionalBody(c, &req))
		assert.False(t, req.PurgeMessages)
		assert.NotEqual(t, http.StatusBadRequest, w.Code)
	})
	t.Run("valid body binds", func(t *testing.T) {
		var req RemoveMemberRequest
		c, _ := newCtx(`{"purge_messages":true}`)
		assert.True(t, bindOptionalBody(c, &req))
		assert.True(t, req.PurgeMessages)
	})
	t.Run("malformed body rejected with 400", func(t *testing.T) {
		var req RemoveMemberRequest
		c, w := newCtx(`{"purge_messages":true,`)
		assert.False(t, bindOptionalBody(c, &req))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

// The post-commit purge must survive a cancelled request context (client disconnected after
// the ban/kick committed) — it is detached via context.WithoutCancel. #1353 review (Gitar +
// @security-reviewer + @code-reviewer).
func TestApplyPurgeOnModeration_DetachesFromRequestContext(t *testing.T) {
	fp := &fakePurger{retStatus: messages.PurgeCompleted, retCount: 1}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate the client having already disconnected
	out := testHandlerWithPurger(fp).applyPurgeOnModeration(ctx, "srv", "mod", "victim", "ban")
	assert.True(t, fp.called, "purge still runs despite a cancelled request context")
	assert.NoError(t, fp.gotCtxErr, "purger receives a cancellation-detached context")
	assert.Equal(t, messages.PurgeCompleted, out.Status)
}

func TestApplyPurgeOnModeration_PreservesCallerDeadline(t *testing.T) {
	fp := &fakePurger{retStatus: messages.PurgeCompleted}
	deadline := time.Now().Add(time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	testHandlerWithPurger(fp).applyPurgeOnModeration(ctx, "srv", "mod", "victim", "ban")
	require.True(t, fp.called)
	require.WithinDuration(t, deadline, fp.gotDeadline, 20*time.Millisecond)
}

// Fail-CLOSED rate limit: an exhausted per-actor budget skips the purge (ban/kick already
// committed) without calling the engine. #1353 review (Gitar + @security-reviewer).
func TestApplyPurgeOnModeration_RateLimitExhausted(t *testing.T) {
	fp := &fakePurger{retStatus: messages.PurgeCompleted, retCount: 1}
	h, _ := testHandlerWithRedis(t, fp)
	// First purgeModerationRateLimit calls are allowed; the next is denied.
	for i := 0; i < purgeModerationRateLimit; i++ {
		out := h.applyPurgeOnModeration(context.Background(), "srv", "mod", "victim", "ban")
		require.Equal(t, messages.PurgeCompleted, out.Status, "call %d should be allowed", i+1)
	}
	fp.called = false
	out := h.applyPurgeOnModeration(context.Background(), "srv", "mod", "victim", "ban")
	assert.Equal(t, messages.PurgeSkippedRateLimited, out.Status, "over-budget call is skipped")
	assert.False(t, fp.called, "engine is NOT invoked when rate-limited")
}

// Fail-CLOSED on Redis backend error: a downed Redis denies the purge (skipped), never a
// silent unthrottled pass-through.
func TestApplyPurgeOnModeration_RateLimitBackendErrorFailsClosed(t *testing.T) {
	fp := &fakePurger{retStatus: messages.PurgeCompleted, retCount: 1}
	h, mr := testHandlerWithRedis(t, fp)
	mr.Close() // Redis outage
	out := h.applyPurgeOnModeration(context.Background(), "srv", "mod", "victim", "ban")
	assert.Equal(t, messages.PurgeSkippedRateLimited, out.Status, "backend error fails closed")
	assert.False(t, fp.called, "engine is NOT invoked on a Redis backend error")
}

// Regression lock for the #1353 review ordering finding (Gitar): with redis WIRED, a cancelled
// request context (client disconnected post-commit) must NOT abort the rate-limit check and
// skip the purge — the detach happens BEFORE the gate, so both survive. The nil-redis detach
// test above cannot catch this because it skips the gate entirely.
func TestApplyPurgeOnModeration_RateLimitGateSurvivesDisconnect(t *testing.T) {
	fp := &fakePurger{retStatus: messages.PurgeCompleted, retCount: 1}
	h, _ := testHandlerWithRedis(t, fp)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client already disconnected when applyPurgeOnModeration runs
	out := h.applyPurgeOnModeration(ctx, "srv", "mod", "victim", "ban")
	assert.Equal(t, messages.PurgeCompleted, out.Status, "purge runs; NOT skipped_rate_limited")
	assert.True(t, fp.called, "engine invoked despite the cancelled request context")
	assert.NoError(t, fp.gotCtxErr, "purger receives a cancellation-detached context")
}
