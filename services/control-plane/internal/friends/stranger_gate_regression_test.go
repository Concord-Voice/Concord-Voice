// Package friends_test — stage C stranger-gate regressions (#2854 finding C).
//
// DIFFERENT CLASS from presence_disconnect_regression_test.go, which owns the
// #2738 DISCONNECT class. This file owns STRIPE CONTENTION: presencehook's
// WithGatedTx acquires a 1-of-64 process-local sender gate BEFORE opening its
// transaction, so a no-op check living inside the closure cannot run until an
// uninvolved user's gate is already held.
//
// The observable is capture.gated() — what WithGatedTx was asked to gate. The
// handler doubles take no REAL process-local gate (see presence_capture_db_test.go),
// and that is deliberate: the gate ORDER is proven in graphpresence's own tests.
// Here the question is only whether the gated path was ENTERED at all.
package friends_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/friends"
)

// TestRemoveFriendOnAStrangerTakesNoGate is the headline stage C regression.
//
// The actor holds NO permission and NO relationship to the victim. That is the
// property that makes this a security finding rather than a performance nit:
// any authenticated user can drive it against any user ID, repeatably, and the
// transaction provably deletes nothing.
func TestRemoveFriendOnAStrangerTakesNoGate(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "stranger-gate-actor")
	victim := ts.CreateTestUser(t, "stranger-gate-victim")
	// Deliberately NO friendship of any status: not accepted, not pending, not
	// blocked. The two accounts have never interacted.

	capture := &probingCapture{db: ts.DB}
	engine := hookedHandler(t, ts, capture, actor.ID, func(e *gin.Engine, h *friends.Handler) {
		e.DELETE("/friends/:user_id", h.RemoveFriend)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/friends/"+victim.ID, nil))

	// PAIRED with the terminal assertion on purpose. "no gate was taken" passes
	// vacuously for ANY early return — a 400 on a malformed ID, a 500 from an
	// unrelated fault — so on its own it would certify a broken handler.
	require.Equal(t, http.StatusNotFound, w.Code,
		"the request must traverse the whole handler, not bail out earlier")
	require.Empty(t, capture.gated(),
		"AC (#2854): a stranger's presence stripe must never be acquired for a removal that deletes nothing")

	subjects, _, completes, abandons := capture.snapshot()
	require.Empty(t, subjects, "and no capture may run either")
	require.Zero(t, completes)
	require.Empty(t, abandons,
		"nothing was captured, so nothing may be abandoned — an abandon here would be the #2738 defect")
}

// TestRemoveFriendOnARealFriendStillGates is the non-regression half. It fails
// if the probe's branch is inverted, i.e. if the fix over-skips and starts
// short-circuiting real removals.
func TestRemoveFriendOnARealFriendStillGates(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "real-remove-actor")
	friend := ts.CreateTestUser(t, "real-remove-friend")
	ts.CreateFriendship(t, actor.ID, friend.ID, statusAccepted)

	capture := &probingCapture{db: ts.DB}
	engine := hookedHandler(t, ts, capture, actor.ID, func(e *gin.Engine, h *friends.Handler) {
		e.DELETE("/friends/:user_id", h.RemoveFriend)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/friends/"+friend.ID, nil))

	require.Equal(t, http.StatusOK, w.Code)

	gated := capture.gated()
	require.Len(t, gated, 1, "a real removal must still enter the gated path")
	require.ElementsMatch(t,
		[]string{actor.ID, friend.ID},
		[]string{gated[0].Principal.String(), gated[0].Counterpart.String()},
		"focalSenders gates BOTH endpoints; a real removal must still gate both")

	require.Zero(t, countFriendships(t, ts, actor.ID, friend.ID, statusAccepted),
		"and the removal must actually have happened")
}

// TestBlockingAStrangerTakesNoGateButStillBlocks.
//
// BlockUser is the second unbounded stranger-gate vector: POST /friends/<any>/block
// needs no relationship and no permission. Unlike RemoveFriend it CANNOT
// short-circuit, because the block is a real write and a safety affordance —
// refusing it would itself be the regression. So the fix nils the capture
// instead, which skips the gate while the write still lands.
//
// Skipping the capture costs nothing here: CaptureInTx gates on an accepted
// edge, so with no such edge the plan it would have produced is empty anyway.
func TestBlockingAStrangerTakesNoGateButStillBlocks(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "block-gate-actor")
	stranger := ts.CreateTestUser(t, "block-gate-stranger")

	capture := &probingCapture{db: ts.DB}
	engine := hookedHandler(t, ts, capture, actor.ID, func(e *gin.Engine, h *friends.Handler) {
		e.POST("/friends/:user_id/block", h.BlockUser)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/"+stranger.ID+"/block", nil))

	require.Equal(t, http.StatusOK, w.Code,
		"blocking a stranger must still succeed — this is a safety affordance, not a no-op")
	require.Empty(t, capture.gated(),
		"AC (#2854): no gate may be held on a stranger for a capture that is structurally empty")

	subjects, _, _, _ := capture.snapshot()
	require.Empty(t, subjects, "and no capture may run")

	require.Equal(t, 1, countFriendships(t, ts, actor.ID, stranger.ID, "blocked"),
		"the block MUST be applied — skipping the gate must not skip the write")
}

// TestBlockingARealFriendStillGatesAndCaptures is the non-regression half: it
// fails if the probe's branch is inverted and real blocks stop reconciling.
func TestBlockingARealFriendStillGatesAndCaptures(t *testing.T) {
	ts := setupTS(t)
	actor := ts.CreateTestUser(t, "block-real-actor")
	friend := ts.CreateTestUser(t, "block-real-friend")
	ts.CreateFriendship(t, actor.ID, friend.ID, statusAccepted)

	capture := &probingCapture{db: ts.DB}
	engine := hookedHandler(t, ts, capture, actor.ID, func(e *gin.Engine, h *friends.Handler) {
		e.POST("/friends/:user_id/block", h.BlockUser)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/friends/"+friend.ID+"/block", nil))

	require.Equal(t, http.StatusOK, w.Code)

	gated := capture.gated()
	require.Len(t, gated, 1, "blocking a real friend revokes a real audience and must still gate")
	require.ElementsMatch(t,
		[]string{actor.ID, friend.ID},
		[]string{gated[0].Principal.String(), gated[0].Counterpart.String()})

	subjects, _, _, _ := capture.snapshot()
	require.Len(t, subjects, 1, "and it must still capture")

	require.Equal(t, 1, countFriendships(t, ts, actor.ID, friend.ID, "blocked"))
}
