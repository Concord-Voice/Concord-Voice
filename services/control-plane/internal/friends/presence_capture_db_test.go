// Package friends_test drives the hooked handlers against a real database.
//
// These tests are EXTERNAL on purpose: internal/testhelpers builds the router
// and therefore imports internal/friends, so an in-package test importing it
// would cycle. The capture is wired through the exported setter instead of the
// unexported field.
package friends_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/friends"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// probingCapture is a GraphPresenceCapture double that runs a caller-supplied
// probe against the handler's own transaction at capture time. That is how these
// tests prove the capture strictly precedes the mutation write: the probe reads
// the pre-mutation row through the very transaction that is about to destroy it.
type probingCapture struct {
	mu sync.Mutex

	// probe runs inside CaptureInTx, on the handler's transaction.
	probe func(*sql.Tx) (int, error)
	// captureErr, when set, makes stage-1 capture fail.
	captureErr error
	// plan, when set, is handed back as the capture result.
	plan presencecapture.Plan

	subjects  []presencecapture.Subject
	probed    []int
	completes int
	abandons  []string
}

func (c *probingCapture) CaptureInTx(
	_ context.Context, tx *sql.Tx, subject presencecapture.Subject,
) (presencecapture.Plan, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.subjects = append(c.subjects, subject)
	if c.probe != nil {
		got, err := c.probe(tx)
		if err != nil {
			return nil, err
		}
		c.probed = append(c.probed, got)
	}
	if c.captureErr != nil {
		return nil, c.captureErr
	}
	return c.plan, nil
}

// Complete owns the COMMIT, exactly as the real reconciler does.
func (c *probingCapture) Complete(_ context.Context, tx *sql.Tx, _ presencecapture.Plan) error {
	c.mu.Lock()
	c.completes++
	c.mu.Unlock()
	return tx.Commit()
}

func (c *probingCapture) Abandon(_ presencecapture.Plan, cause presencecapture.Cause) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.abandons = append(c.abandons, string(cause))
}

// snapshot copies before unlocking. Returning the internal slice headers
// handed the caller aliases that a later append from a dispatch goroutine
// could write through, so the read was racy the moment it left the lock
// (PR #2738 review, CodeRabbit).
func (c *probingCapture) snapshot() ([]presencecapture.Subject, []int, int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]presencecapture.Subject(nil), c.subjects...),
		append([]int(nil), c.probed...),
		c.completes,
		append([]string(nil), c.abandons...)
}

// hookedHandler builds a friends handler wired to capture and mounts it on a
// bare engine that authenticates as actingUserID. It deliberately does NOT reuse
// ts.Router: that router owns its own handler, which the capture cannot reach.
func hookedHandler(
	t *testing.T, ts *testhelpers.TestServer, capture presencecapture.GraphPresenceCapture,
	actingUserID string, register func(*gin.Engine, *friends.Handler),
) *gin.Engine {
	t.Helper()

	h := friends.NewHandler(ts.DB, logger.New("test"), nil)
	h.SetGraphPresenceCapture(capture)
	require.True(t, h.HasGraphPresenceCapture())

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("user_id", actingUserID)
		c.Next()
	})
	register(engine, h)
	return engine
}

func acceptedFriendshipCount(tx *sql.Tx, userID, targetID string) (int, error) {
	var n int
	err := tx.QueryRow(`
		SELECT COUNT(*) FROM friendships
		WHERE ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))
		  AND status = 'accepted'
	`, userID, targetID).Scan(&n)
	return n, err
}

func countFriendships(t *testing.T, ts *testhelpers.TestServer, userID, targetID, status string) int {
	t.Helper()
	var n int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM friendships
		WHERE ((requester_id = $1 AND addressee_id = $2) OR (requester_id = $2 AND addressee_id = $1))
		  AND status = $3
	`, userID, targetID, status).Scan(&n))
	return n
}

// The load-bearing ordering claim: the capture reads the friendship the DELETE
// is about to destroy, through the same transaction, BEFORE the delete runs.
func TestRemoveFriendCapturesBeforeTheDelete(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "remover")
	user2 := ts.CreateTestUser(t, "removed")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	capture := &probingCapture{probe: func(tx *sql.Tx) (int, error) {
		return acceptedFriendshipCount(tx, user1.ID, user2.ID)
	}}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.DELETE("/friends/:user_id", h.RemoveFriend)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/friends/"+user2.ID, nil))
	require.Equal(t, http.StatusOK, w.Code)

	subjects, probed, completes, abandons := capture.snapshot()
	require.Len(t, subjects, 1)
	assert.Equal(t, presencecapture.FamilyFriendshipRemove, subjects[0].Family)
	assert.Equal(t, presencecapture.FailClosedBlockWrite, subjects[0].FailPosture,
		"removal must block the write on capture failure — only block degrades")
	assert.Equal(t, user1.ID, subjects[0].Principal.String())
	assert.Equal(t, user2.ID, subjects[0].Counterpart.String())

	require.Equal(t, []int{1}, probed,
		"the capture must still see the friendship: the write destroys what it reads")
	assert.Equal(t, 1, completes, "Complete owns the commit")
	assert.Empty(t, abandons)
	assert.Equal(t, 0, countFriendships(t, ts, user1.ID, user2.ID, statusAccepted))
}

// Fail-closed: a stage-1 capture failure rolls the transaction back, so the
// friendship survives and nothing was disclosed.
func TestRemoveFriendCaptureFailureBlocksTheWrite(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "remover_fc")
	user2 := ts.CreateTestUser(t, "removed_fc")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	capture := &probingCapture{captureErr: errors.New("capture boom")}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.DELETE("/friends/:user_id", h.RemoveFriend)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/friends/"+user2.ID, nil))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	_, _, completes, _ := capture.snapshot()
	assert.Equal(t, 0, completes)
	assert.Equal(t, 1, countFriendships(t, ts, user1.ID, user2.ID, statusAccepted),
		"a fail-closed capture must leave the friend graph untouched")
}

// degradedPlan stands in for the conservative superset the bridge substitutes
// when a stage-1 read fails under FailConservativeDegrade.
type degradedPlan struct{}

func (degradedPlan) HasWork() bool  { return true }
func (degradedPlan) Degraded() bool { return true }

// The block site captures the prior friendship state through its own
// transaction, before the status write replaces it. A 2xx here also proves
// executeBlockTx no longer commits: a handler-owned commit would leave Complete
// with sql.ErrTxDone and a 500.
func TestBlockUserCapturesBeforeTheFriendshipWrite(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker_cap")
	user2 := ts.CreateTestUser(t, "blocked_cap")
	ts.CreateFriendship(t, user1.ID, user2.ID, statusAccepted)

	capture := &probingCapture{probe: func(tx *sql.Tx) (int, error) {
		return acceptedFriendshipCount(tx, user1.ID, user2.ID)
	}}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.POST("/friends/:user_id/block", h.BlockUser)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/"+user2.ID+"/block", nil))
	require.Equal(t, http.StatusOK, w.Code)

	subjects, probed, completes, abandons := capture.snapshot()
	require.Len(t, subjects, 1)
	assert.Equal(t, presencecapture.FamilyBlock, subjects[0].Family)
	assert.Equal(t, presencecapture.FailConservativeDegrade, subjects[0].FailPosture)
	require.Equal(t, []int{1}, probed, "the capture must still see the pre-block friendship")
	assert.Equal(t, 1, completes)
	assert.Empty(t, abandons)
	assert.Equal(t, 1, countFriendships(t, ts, user1.ID, user2.ID, statusBlocked))
}

// No prior friendship row exists, so the write is an INSERT. The capture is
// still unconditional — the delta simply comes out empty. No status branch.
func TestBlockUserCapturesWithNoPriorFriendship(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker_new")
	user2 := ts.CreateTestUser(t, "blocked_new")

	capture := &probingCapture{}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.POST("/friends/:user_id/block", h.BlockUser)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/"+user2.ID+"/block", nil))
	require.Equal(t, http.StatusOK, w.Code)

	subjects, _, completes, _ := capture.snapshot()
	require.Len(t, subjects, 1, "block captures unconditionally, whatever the prior status")
	assert.Equal(t, 1, completes)
	assert.Equal(t, 1, countFriendships(t, ts, user1.ID, user2.ID, statusBlocked))
}

// Degraded() is a counter, never a business-logic branch: the block still
// returns its normal 2xx and the write still lands.
func TestBlockUserSucceedsWithADegradedPlan(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker_deg")
	user2 := ts.CreateTestUser(t, "blocked_deg")

	capture := &probingCapture{plan: degradedPlan{}}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.POST("/friends/:user_id/block", h.BlockUser)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/"+user2.ID+"/block", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, countFriendships(t, ts, user1.ID, user2.ID, statusBlocked))
}

// A stage-2 failure — the error the bridge returns rather than a degraded plan —
// still blocks the write regardless of the declared posture.
func TestBlockUserCaptureErrorBlocksTheWrite(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "blocker_err")
	user2 := ts.CreateTestUser(t, "blocked_err")

	capture := &probingCapture{captureErr: errors.New("capture boom")}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.POST("/friends/:user_id/block", h.BlockUser)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/"+user2.ID+"/block", nil))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	_, _, completes, _ := capture.snapshot()
	assert.Equal(t, 0, completes)
	assert.Equal(t, 0, countFriendships(t, ts, user1.ID, user2.ID, statusBlocked))
}

// SECURITY REGRESSION (#2738 review). A no-op delete must NOT abandon.
//
// This test previously asserted the opposite — that a no-op removal abandons —
// and in doing so locked in a live vulnerability. `Abandon` disconnects the
// captured audience, whose viewer set always contains BOTH principals, and
// `Hub.DisconnectRichPresenceClients` closes every local DEVICE for those
// users. So any authenticated caller could `DELETE /friends/<stranger>` and
// force a full websocket teardown of that stranger's sessions, repeatably,
// having mutated nothing — a cheap DoS amplification vector against arbitrary
// users.
//
// `rowsAffected == 0` is positive proof that nothing was written, so there is
// no stale audience and nobody to disconnect. Contrast the driver-error branch,
// which DOES abandon because the write's outcome is genuinely unknown, and
// unknown state must fail closed. Proven no-change must not.
func TestRemoveFriendDoesNotDisconnectWhenNoFriendshipExists(t *testing.T) {
	ts := setupTS(t)
	user1 := ts.CreateTestUser(t, "remover_none")
	user2 := ts.CreateTestUser(t, "removed_none")

	capture := &probingCapture{}
	engine := hookedHandler(t, ts, capture, user1.ID, func(e *gin.Engine, h *friends.Handler) {
		e.DELETE("/friends/:user_id", h.RemoveFriend)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/friends/"+user2.ID, nil))

	assert.Equal(t, http.StatusNotFound, w.Code)
	_, _, completes, abandons := capture.snapshot()
	assert.Equal(t, 0, completes, "nothing was written, so nothing may commit")
	assert.Empty(t, abandons,
		"a non-mutating RemoveFriend must disconnect nobody: Abandon would tear down "+
			"every device of both principals, letting any caller DoS a stranger")
}
