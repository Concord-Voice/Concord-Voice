package messages_test

// The channel delete's post-commit settlement must outlive the request (review
// of #3509): a client that hangs up just after its verified delete commits
// still has its soft-lock reset and its step-up budget cleared.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// cancelOnFirstDel cancels the request's context as the first DEL is issued,
// before that DEL runs. Nothing before the settlement issues a DEL, so the
// request goes away exactly after the delete committed.
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

// TestDeleteSoftLock_SettlementOutlivesTheRequest: the 16th delete confirms
// with a valid code and commits; the request is cancelled as settlement
// begins; both tiers and the budget are cleared anyway.
func TestDeleteSoftLock_SettlementOutlivesTheRequest(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	mr := miniredis.RunT(t)
	rdb := fastRedisClient(t, mr.Addr())
	s := buildSoftLockHarness(t, ts, ts.DB, rdb, mr)
	w := s.world(t, false)
	s.enroll(t, w.author.ID)
	ids := s.seed(t, w.channelID, w.author, 16)
	s.deleteN(t, w.author.ID, ids[:15])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rdb.AddHook(&cancelOnFirstDel{cancel: cancel})
	body, err := json.Marshal(map[string]string{"mfa_code": softLockValidCode})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodDelete, "/messages/"+ids[15], bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", w.author.ID)
	res := httptest.NewRecorder()
	s.router.ServeHTTP(res, req)

	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	require.Error(t, ctx.Err(), "the request was cancelled as settlement began")
	assert.False(t, s.messageExists(t, ids[15]))
	assert.Empty(t, s.counter(burstKey(w.author.ID, w.serverID)), "a hang-up does not skip the reset")
	assert.Empty(t, s.counter(dayKey(w.author.ID)))
	assert.Empty(t, s.counter(budgetKey(w.author.ID)), "or the budget clear")
}
