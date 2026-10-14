package dm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dm"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// #3459 AC2 and AC7 on the DM route, through the real router so NewRouter's
// pinned-expiry wiring is what runs: turning include_pinned on deletes a due
// pin before the 200 with a truthful audit row, and asks for no factor even
// from an MFA-enrolled participant (the DM route is ungated, D-B).
//
// Mutant killed: the expirer call removed from the DM UpdateExpiration.
func TestDMExpiration_TogglingPinnedOnExpiresDuePinsUngated(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "pin_exp_alice")
	bob := ts.CreateTestUser(t, "pin_exp_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)
	testhelpers.EnrollInlineTOTP(t, ts.DB, alice.ID)
	auth := testhelpers.AuthHeaders(alice.AccessToken)
	path := dmExpirationPath + convID + "/expiration"

	w := ts.DoRequest(http.MethodPatch, path, map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"}, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var duePin, futurePin string
	for _, held := range []struct {
		id *string
		at time.Time
	}{{&duePin, time.Now().Add(-time.Hour)}, {&futurePin, time.Now().Add(time.Hour)}} {
		require.NoError(t, ts.DB.QueryRow(`
INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at, pinned_at, pinned_by, held_expires_at)
VALUES ($1, $2, 'ciphertext', 'user', NOW() - interval '25 hours', NOW() - interval '25 hours', $2, $3)
RETURNING id`, convID, alice.ID, held.at).Scan(held.id))
	}

	w = ts.DoRequest(http.MethodPatch, path,
		map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only", "include_pinned": true}, auth)
	require.Equal(t, http.StatusOK, w.Code, "the DM route asks for no factor: %s", w.Body.String())
	var policy map[string]any
	testhelpers.ParseJSON(t, w, &policy)
	assert.Equal(t, true, policy["include_pinned"])

	exists := func(id string) bool {
		var ok bool
		require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM dm_messages WHERE id = $1)`, id).Scan(&ok))
		return ok
	}
	assert.False(t, exists(duePin), "the due pin is gone when the PATCH answers")
	assert.True(t, exists(futurePin))
	var audits, truthful int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*), count(*) FILTER (WHERE include_pinned)
	   FROM message_purges WHERE context_id = $1 AND reason = 'expiry'`, convID).Scan(&audits, &truthful))
	assert.Equal(t, 1, audits)
	assert.Equal(t, 1, truthful)

	// The list row carries E (§6.3).
	var listed struct {
		Conversations []struct {
			ID            string `json:"id"`
			IncludePinned *bool  `json:"expiration_include_pinned"`
		} `json:"conversations"`
	}
	w = ts.DoRequest(http.MethodGet, "/api/v1/dm/conversations", nil, auth)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	testhelpers.ParseJSON(t, w, &listed)
	found := false
	for _, conv := range listed.Conversations {
		if conv.ID == convID {
			found = true
			require.NotNil(t, conv.IncludePinned, "the conversation row carries expiration_include_pinned")
			assert.True(t, *conv.IncludePinned)
		}
	}
	assert.True(t, found, "the conversation is listed")
}

type dmDeadlineRecordingRunner struct {
	mu        sync.Mutex
	deadlines []time.Time
}

func (r *dmDeadlineRecordingRunner) RunExpiryBatch(ctx context.Context, plan purge.ExpiryPlan) (purge.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	deadline, _ := ctx.Deadline()
	r.deadlines = append(r.deadlines, deadline)
	return purge.Result{DeletedCount: len(plan.CandidateIDs)}, nil
}

type dmNoopPurgeNotifier struct{}

func (dmNoopPurgeNotifier) EmitServerPurged(context.Context, string, time.Time) {}
func (dmNoopPurgeNotifier) EmitDMPurged(context.Context, string, time.Time)     {}

// The DM pin deletion after commit runs inside the PATCH's response budget,
// not on a fresh 10 seconds of its own (#3459 Codex review).
//
// Mutant killed: answerCtx replaced by c.Request.Context() in the DM
// UpdateExpiration.
func TestDMExpiration_PinDeletionRunsInsideTheResponseBudget(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	alice := ts.CreateTestUser(t, "pin_budget_alice")
	bob := ts.CreateTestUser(t, "pin_budget_bob")
	convID := ts.CreateDMConversation(t, alice.ID, bob.ID)

	log := logger.NewWithWriter(&bytes.Buffer{})
	handler := dm.NewHandler(dm.HandlerDeps{
		DB:       ts.DB,
		Log:      log,
		Hub:      ts.Hub,
		Cfg:      &config.Config{MediaPlaneURL: "http://media.test"},
		Redis:    ts.Redis,
		EntCache: entitlements.NewCache(ts.Redis, ts.DB),
	})
	runner := &dmDeadlineRecordingRunner{}
	expirer, err := expiration.NewContextExpirer(context.Background(), runner, dmNoopPurgeNotifier{}, log)
	require.NoError(t, err)
	handler.SetPinnedExpirer(expirer)
	handler.SetResponseBudgetForTest(3 * time.Second)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", alice.ID)
		c.Next()
	})
	router.PATCH("/api/v1/dm/conversations/:id/expiration", handler.UpdateExpiration)
	patch := func(body map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPatch, dmExpirationPath+convID+"/expiration", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	w := patch(map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var duePin string
	require.NoError(t, ts.DB.QueryRow(`
INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at, pinned_at, pinned_by, held_expires_at)
VALUES ($1, $2, 'ciphertext', 'user', NOW() - interval '25 hours', NOW() - interval '25 hours', $2, NOW() - interval '1 hour')
RETURNING id`, convID, alice.ID).Scan(&duePin))

	sent := time.Now()
	w = patch(map[string]any{"mode": "set", "window_seconds": 86400, "retroactive": "new_only", "include_pinned": true})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, runner.deadlines, 1, "the due pin reached the expirer")
	assert.True(t, runner.deadlines[0].Before(sent.Add(3*time.Second)),
		"the run ends inside the 3 s budget (got %s after the request)", runner.deadlines[0].Sub(sent))
}
